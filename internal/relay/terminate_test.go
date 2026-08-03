package relay

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeWildcard(t *testing.T, apex string) (certFile, keyFile string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "*." + apex},
		DNSNames:     []string{"*." + apex},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	cf, _ := os.Create(certFile)
	pem.Encode(cf, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	cf.Close()
	kb, _ := x509.MarshalECPrivateKey(key)
	kf, _ := os.Create(keyFile)
	pem.Encode(kf, &pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	kf.Close()
	return certFile, keyFile
}

func TestLoadWildcardConfig(t *testing.T) {
	if cfg, err := LoadWildcardConfig("", ""); err != nil || cfg != nil {
		t.Fatalf("empty paths = %v,%v want nil,nil", cfg, err)
	}
	cert, key := writeWildcard(t, "public.getpiper.co")
	cfg, err := LoadWildcardConfig(cert, key)
	// The pair is served through GetCertificate rather than a static
	// Certificates slice so a renewal on disk is picked up without a restart
	// (#484); the rotation behaviour itself is pinned in wildcard_reload_test.go.
	if err != nil || cfg == nil || cfg.GetCertificate == nil {
		t.Fatalf("LoadWildcardConfig = %v,%v", cfg, err)
	}
	if _, err := cfg.GetCertificate(&tls.ClientHelloInfo{ServerName: "app.public.getpiper.co"}); err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
}

func TestPrefixConnReplaysThenReads(t *testing.T) {
	inner := &fakeConn{readBuf: []byte("world")}
	pc := &prefixConn{Conn: inner, prefix: []byte("hello ")}
	got := make([]byte, 11)
	n, _ := readFull(pc, got)
	if string(got[:n]) != "hello world" {
		t.Fatalf("prefixConn read %q", got[:n])
	}
}

// fakeConn is a minimal net.Conn whose Read drains readBuf then EOFs.
type fakeConn struct {
	readBuf []byte
}

func (c *fakeConn) Read(p []byte) (int, error) {
	if len(c.readBuf) == 0 {
		return 0, os.ErrDeadlineExceeded // any non-nil EOF-ish
	}
	n := copy(p, c.readBuf)
	c.readBuf = c.readBuf[n:]
	return n, nil
}
func (c *fakeConn) Write(p []byte) (int, error)        { return len(p), nil }
func (c *fakeConn) Close() error                       { return nil }
func (c *fakeConn) LocalAddr() net.Addr                { return nil }
func (c *fakeConn) RemoteAddr() net.Addr               { return nil }
func (c *fakeConn) SetDeadline(t time.Time) error      { return nil }
func (c *fakeConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *fakeConn) SetWriteDeadline(t time.Time) error { return nil }

func readFull(r interface{ Read([]byte) (int, error) }, p []byte) (int, error) {
	total := 0
	for total < len(p) {
		n, err := r.Read(p[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
