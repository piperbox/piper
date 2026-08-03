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

// diskVersion names what one attempt observed on disk. When both files stat,
// their (mtime, size) stamps identify the exact bytes the attempt ran against,
// so two different broken versions are distinguishable even when they fail
// identically. A failed stat observes no version at all: there are no stamps to
// name, and the zero value (stamped false) stands for "the files could not be
// read", not for any particular content.
type diskVersion struct {
	stamped bool // both files stat'ed, so cs/ks are meaningful
	cs, ks  fileStamp
}

// notedFailure is the failure already written to the log, kept so a repeat of
// the *same* incident stays quiet: the version it was observed on plus the
// message. Keying on the version is what keeps two bad on-disk versions with
// the same error text from collapsing into one line; the message is a second
// discriminator, and carries the whole identity for an unstamped observation
// (it names the file that failed to stat and the stat error).
type notedFailure struct {
	ver diskVersion
	msg string
}

// wildcardReloader serves the relay's wildcard pair and re-reads it from disk
// when it changes. getCertificate runs on every TLS handshake, concurrently, so
// the hot path is two stats under an RLock and the parse happens only when a
// stamp moved. It never returns an error: a failed reload keeps the last pair
// that parsed, so a half-written renewal can't take the listener down.
type wildcardReloader struct {
	certFile, keyFile string

	mu     sync.RWMutex
	cert   *tls.Certificate // never nil: the constructor fails if the first load does
	cs, ks fileStamp        // the stamps of the version last attempted (not necessarily the one that parsed)
	noted  *notedFailure    // failure already logged, to keep a broken pair from flooding the log; nil once the files are healthy again
}

func newWildcardReloader(certFile, keyFile string) (*wildcardReloader, error) {
	r := &wildcardReloader{certFile: certFile, keyFile: keyFile}
	// Uncontended — nobody else has the reloader yet; held to satisfy
	// loadLocked's contract rather than to exclude anyone.
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, _, err := r.loadLocked(); err != nil {
		return nil, err
	}
	return r, nil
}

// getCertificate is the tls.Config callback: serve the newest pair on disk.
func (r *wildcardReloader) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	cs, ks, failedPath, err := r.stamps()
	if err == nil {
		r.mu.RLock()
		cert, unchanged := r.cert, cs == r.cs && ks == r.ks
		stale := r.staleFailureLocked(cs, ks)
		r.mu.RUnlock()
		if unchanged {
			// Serving the cached pair for the version on disk is the only
			// evidence of health this path produces, and a failure logged
			// before must be re-armed on it or a second identical outage is
			// swallowed. Deciding that under the read lock keeps the hot path
			// off the write lock — including for as long as a broken version
			// sits on disk, when the noted failure is about these very bytes
			// and there is nothing to re-arm.
			if stale {
				r.rearm(cs, ks)
			}
			return cert, nil
		}
		return r.reload(), nil
	}
	// One of the two files can't be stat'ed — it was removed, or a rotation is
	// mid-rename. Nothing to reload from, so keep serving what we have. Name the
	// file that actually failed, not the cert path in every case.
	r.mu.Lock()
	defer r.mu.Unlock()
	r.noteErrLocked(diskVersion{}, "relay: wildcard cert %s: %v (serving the last good certificate)", failedPath, err)
	return r.cert, nil
}

// staleFailureLocked reports whether a failure has been logged that seeing
// (cs, ks) on disk contradicts. A failure noted on *these* stamps is about the
// bytes that are there now — a version that failed to parse leaves its own
// stamps in cs/ks while cert stays the older pair, so every later handshake
// takes the fast path over bytes that never loaded, and that is not health.
// Anything else — a stat that failed, or a version since replaced — is stale.
// Caller holds mu.
func (r *wildcardReloader) staleFailureLocked(cs, ks fileStamp) bool {
	if r.noted == nil {
		return false
	}
	v := r.noted.ver
	return !(v.stamped && v.cs == cs && v.ks == ks)
}

// rearm clears the logged-failure state once a handshake has served the version
// that is on disk: the files are healthy, so the next failure — including an
// exact repeat of the one already logged — gets reported again.
//
// The caller's read of staleFailureLocked is only a hint: both it and the stamps
// it was taken on are re-checked here under the write lock, so a rotation that
// landed in between — whose stamps the caller never saw — can't have its failure
// disarmed by an observation that predates it.
func (r *wildcardReloader) rearm(cs, ks fileStamp) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cs == r.cs && ks == r.ks && r.staleFailureLocked(cs, ks) {
		r.noted = nil
	}
}

// reload re-parses the pair if it really did change. The stamps are taken again
// under the write lock rather than trusted from the caller, so of N handshakes
// racing one rotation exactly one parses and the rest see their own stat already
// matching the cached pair.
func (r *wildcardReloader) reload() *tls.Certificate {
	r.mu.Lock()
	defer r.mu.Unlock()
	cert, ver, err := r.loadLocked()
	if err != nil {
		// Half-written PEM, a cert whose key hasn't landed yet, a file that
		// vanished between the two stats: keep the previous certificate. The
		// stamps of the bad version are recorded by loadLocked, so this costs
		// one parse and one log line per version on disk, not per handshake —
		// and the next write moves them again and triggers a retry.
		r.noteErrLocked(ver, "relay: wildcard cert reload from %s/%s: %v (serving the last good certificate)", r.certFile, r.keyFile, err)
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
//
// It also returns the version it observed, so a caller that logs the failure
// can key its suppression to those bytes rather than to the error text.
func (r *wildcardReloader) loadLocked() (*tls.Certificate, diskVersion, error) {
	cs, ks, _, err := r.stamps()
	if err != nil {
		return nil, diskVersion{}, err
	}
	ver := diskVersion{stamped: true, cs: cs, ks: ks}
	if cs == r.cs && ks == r.ks && r.cert != nil {
		return r.cert, ver, nil // another handshake already reloaded this version
	}
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	r.cs, r.ks = cs, ks
	if err != nil {
		return nil, ver, err
	}
	r.cert, r.noted = &cert, nil
	return r.cert, ver, nil
}

func (r *wildcardReloader) stamps() (cs, ks fileStamp, failedPath string, err error) {
	if cs, err = stampOf(r.certFile); err != nil {
		return fileStamp{}, fileStamp{}, r.certFile, err
	}
	ks, err = stampOf(r.keyFile)
	if err != nil {
		return fileStamp{}, fileStamp{}, r.keyFile, err
	}
	return cs, ks, "", nil
}

// noteErrLocked logs a failure once per observed on-disk version. Every
// handshake re-examines the files, so logging unconditionally would turn one
// bad rotation into a line per connection; keying on the version rather than on
// the message is what still gives two differently-broken versions a line each
// when they fail with the same text. Serving the version on disk clears the
// state — a load that parses, or a fast path that re-arms — so an outage that
// comes back after a healthy period is reported again. Caller holds mu for
// writing.
func (r *wildcardReloader) noteErrLocked(ver diskVersion, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if r.noted != nil && r.noted.ver == ver && r.noted.msg == msg {
		return
	}
	r.noted = &notedFailure{ver: ver, msg: msg}
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
