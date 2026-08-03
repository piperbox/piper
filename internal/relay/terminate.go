package relay

import (
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"

	"github.com/piperbox/piper/internal/tunnel"
)

// LoadWildcardConfig loads certFile/keyFile into a *tls.Config the relay uses to
// terminate shared-domain app TLS. Both paths empty ⇒ (nil, nil): the relay runs
// passthrough-only and never arms the terminate branch.
//
// The pair is served through GetCertificate, not a static Certificates slice, so
// a renewal written over those paths — certbot's deploy hook, a cert-manager
// Secret remount — is picked up by the next handshake without restarting the
// relay and dropping live tunnels (#484). A pair that doesn't load *at startup*
// is still a hard error: the relay refuses to come up with no cert at all.
func LoadWildcardConfig(certFile, keyFile string) (*tls.Config, error) {
	if certFile == "" && keyFile == "" {
		return nil, nil
	}
	r, err := newWildcardReloader(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{GetCertificate: r.getCertificate}, nil
}

// fileStamp is the identity a reload decision is made on. A rotation rewrites
// the file, so its mtime — and nearly always its size — changes; two stats that
// agree mean the bytes on disk are the ones already parsed. mtime is kept as
// Unix nanoseconds because time.Time carries a location pointer and a monotonic
// reading that make == unreliable. The pathological miss is a replacement that
// preserves mtime to the nanosecond *and* size, which no renewal tool does.
type fileStamp struct {
	modNano int64
	size    int64
}

func stampOf(path string) (fileStamp, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}, err
	}
	return fileStamp{modNano: fi.ModTime().UnixNano(), size: fi.Size()}, nil
}

// wildcardReloader serves the relay's wildcard pair and re-reads it from disk
// when it changes. getCertificate runs on every TLS handshake, concurrently, so
// the hot path is two stats under an RLock and the parse happens only when a
// stamp moved. It never returns an error: a failed reload keeps the last pair
// that parsed, so a half-written renewal can't take the listener down.
type wildcardReloader struct {
	certFile, keyFile string

	mu      sync.RWMutex
	cert    *tls.Certificate // never nil: the constructor fails if the first load does
	cs, ks  fileStamp        // the stamps cert was parsed from
	lastErr string           // last failure logged, to keep a broken pair from flooding the log
}

func newWildcardReloader(certFile, keyFile string) (*wildcardReloader, error) {
	r := &wildcardReloader{certFile: certFile, keyFile: keyFile}
	// Uncontended — nobody else has the reloader yet; held to satisfy
	// loadLocked's contract rather than to exclude anyone.
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.loadLocked(); err != nil {
		return nil, err
	}
	return r, nil
}

// getCertificate is the tls.Config callback: serve the newest pair on disk.
func (r *wildcardReloader) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	cs, ks, err := r.stamps()
	if err == nil {
		r.mu.RLock()
		cert, unchanged := r.cert, cs == r.cs && ks == r.ks
		r.mu.RUnlock()
		if unchanged {
			return cert, nil
		}
		return r.reload(), nil
	}
	// One of the two files can't be stat'ed — it was removed, or a rotation is
	// mid-rename. Nothing to reload from, so keep serving what we have.
	r.mu.Lock()
	defer r.mu.Unlock()
	r.noteErrLocked("relay: wildcard cert %s: %v (serving the last good certificate)", r.certFile, err)
	return r.cert, nil
}

// reload re-parses the pair if it really did change. The stamps are taken again
// under the write lock rather than trusted from the caller, so of N handshakes
// racing one rotation exactly one parses and the rest see their own stat already
// matching the cached pair.
func (r *wildcardReloader) reload() *tls.Certificate {
	r.mu.Lock()
	defer r.mu.Unlock()
	cert, err := r.loadLocked()
	if err != nil {
		// Half-written PEM, a cert whose key hasn't landed yet, a file that
		// vanished between the two stats: keep the previous certificate. The
		// stamps of the bad version are recorded by loadLocked, so this costs
		// one parse and one log line per version on disk, not per handshake —
		// and the next write moves them again and triggers a retry.
		r.noteErrLocked("relay: wildcard cert reload from %s: %v (serving the last good certificate)", r.certFile, err)
		return r.cert
	}
	return cert
}

// loadLocked parses the pair and, on success, installs it with the stamps it was
// parsed from. Caller holds mu for writing.
//
// The stamps are taken *before* the read: if the files are rewritten between the
// stat and the parse we then hold new bytes under the older stamps, and the next
// handshake sees a mismatch and re-reads — one wasted parse. Stamping afterwards
// would file the new bytes under the stamps of a version we never read and miss
// the rotation entirely.
func (r *wildcardReloader) loadLocked() (*tls.Certificate, error) {
	cs, ks, err := r.stamps()
	if err != nil {
		return nil, err
	}
	if cs == r.cs && ks == r.ks && r.cert != nil {
		return r.cert, nil // another handshake already reloaded this version
	}
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	r.cs, r.ks = cs, ks
	if err != nil {
		return nil, err
	}
	r.cert, r.lastErr = &cert, ""
	return r.cert, nil
}

func (r *wildcardReloader) stamps() (cs, ks fileStamp, err error) {
	if cs, err = stampOf(r.certFile); err != nil {
		return fileStamp{}, fileStamp{}, err
	}
	ks, err = stampOf(r.keyFile)
	if err != nil {
		return fileStamp{}, fileStamp{}, err
	}
	return cs, ks, nil
}

// noteErrLocked logs a failure once per distinct message. Every handshake
// re-examines the files, so logging unconditionally would turn one bad rotation
// into a line per connection. A successful load clears it, so a failure that
// comes back after a good period is reported again. Caller holds mu for writing.
func (r *wildcardReloader) noteErrLocked(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if msg == r.lastErr {
		return
	}
	r.lastErr = msg
	log.Print(msg)
}

// prefixConn is a net.Conn whose Read first drains a byte prefix (the ClientHello
// bytes readSNI already consumed) before reading the underlying conn. Writes and
// everything else pass straight through — so a tls.Server built on it can replay
// the recorded ClientHello and then complete a real handshake with the client.
type prefixConn struct {
	net.Conn
	prefix []byte
}

func (p *prefixConn) Read(b []byte) (int, error) {
	if len(p.prefix) > 0 {
		n := copy(b, p.prefix)
		p.prefix = p.prefix[n:]
		return n, nil
	}
	return p.Conn.Read(b)
}

// terminate completes a TLS handshake with the wildcard cert (replaying the
// consumed ClientHello via prefixConn), then pipes decrypted plaintext to a
// KindHTTP stream on the app's session. The relay sees plaintext HTTP but never
// parses it — it is a byte pump into the box's :80.
func terminate(conn net.Conn, buffered []byte, sess *tunnel.Session, tlsCfg *tls.Config, m *Metrics) {
	tlsConn := tls.Server(&prefixConn{Conn: conn, prefix: buffered}, tlsCfg)
	if err := tlsConn.Handshake(); err != nil {
		return
	}
	stream, err := sess.OpenKind(tunnel.KindHTTP)
	if err != nil {
		return
	}
	m.StreamStart()
	defer m.StreamEnd()
	defer stream.Close()
	done := make(chan struct{}, 2)
	go func() { io.Copy(stream, tlsConn); done <- struct{}{} }()
	go func() { io.Copy(tlsConn, stream); done <- struct{}{} }()
	<-done
}
