package relay

import (
	"crypto/tls"
	"io"
	"net"

	"github.com/piperbox/piper/internal/tunnel"
)

// LoadWildcardConfig loads certFile/keyFile into a *tls.Config the relay uses to
// terminate shared-domain app TLS. Both paths empty ⇒ (nil, nil): the relay runs
// passthrough-only and never arms the terminate branch.
func LoadWildcardConfig(certFile, keyFile string) (*tls.Config, error) {
	if certFile == "" && keyFile == "" {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
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
