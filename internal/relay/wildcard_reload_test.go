package relay

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// pemPad is the byte size every cert/key file in these tests is padded to.
// pem.Decode skips whatever precedes the first "-----BEGIN" line, so padding a
// pair with leading newlines lets a test hold a file's (mtime, size) identity
// fixed while swapping the certificate inside it — the exact input the reloader
// must decide *not* to re-read. Comfortably larger than a P-256 pair (~800B).
const pemPad = 4096

// newWildcardPEM generates a fresh self-signed wildcard pair for apex. Every
// call makes a new key, so any two pairs differ in their certificate bytes.
func newWildcardPEM(t *testing.T, apex string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "*." + apex},
		DNSNames:     []string{"*." + apex},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
}

// writeFilePadded writes data to path preceded by enough newlines to make the
// file exactly pemPad bytes, then stamps it with mod. Tests pass an explicit
// mtime rather than trusting the filesystem clock: a rotation must be seen even
// where mtime granularity is coarse, and the no-reload test needs two different
// files to share one mtime exactly.
func writeFilePadded(t *testing.T, path string, data []byte, mod time.Time) {
	t.Helper()
	if len(data) > pemPad {
		t.Fatalf("%s: %d bytes exceeds pad of %d", path, len(data), pemPad)
	}
	padded := append(bytes.Repeat([]byte("\n"), pemPad-len(data)), data...)
	if err := os.WriteFile(path, padded, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

// writeWildcardPair generates a wildcard pair for apex, writes it to
// certFile/keyFile stamped with mod, and returns the certificate DER so callers
// can assert which pair a handshake served.
func writeWildcardPair(t *testing.T, certFile, keyFile, apex string, mod time.Time) (der []byte) {
	t.Helper()
	certPEM, keyPEM := newWildcardPEM(t, apex)
	writeFilePadded(t, certFile, certPEM, mod)
	writeFilePadded(t, keyFile, keyPEM, mod)
	blk, _ := pem.Decode(certPEM)
	if blk == nil {
		t.Fatal("generated cert is not PEM")
	}
	return blk.Bytes
}

// armedWildcard writes a pair into a temp dir and returns the config the relay
// would serve with, the file paths, and the served certificate's DER.
func armedWildcard(t *testing.T, apex string) (cfg *tls.Config, certFile, keyFile string, der []byte) {
	t.Helper()
	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	der = writeWildcardPair(t, certFile, keyFile, apex, time.Now().Add(-time.Hour))
	cfg, err := LoadWildcardConfig(certFile, keyFile)
	if err != nil {
		t.Fatalf("LoadWildcardConfig: %v", err)
	}
	if cfg == nil {
		t.Fatal("LoadWildcardConfig returned a nil config for a real pair")
	}
	if cfg.GetCertificate == nil {
		t.Fatal("tls.Config.GetCertificate is nil: the pair is loaded once at startup and never re-read from disk (#484)")
	}
	return cfg, certFile, keyFile, der
}

// served calls GetCertificate the way a handshake does and returns the pointer
// and its leaf DER.
func served(t *testing.T, cfg *tls.Config, sni string) (*tls.Certificate, []byte) {
	t.Helper()
	crt, err := cfg.GetCertificate(&tls.ClientHelloInfo{ServerName: sni})
	if err != nil {
		t.Fatalf("GetCertificate(%q): %v", sni, err)
	}
	if crt == nil || len(crt.Certificate) == 0 {
		t.Fatalf("GetCertificate(%q) returned no certificate", sni)
	}
	return crt, crt.Certificate[0]
}

// TestWildcardConfigPicksUpRotatedPair is the issue itself: a renewed pair
// landing on disk must be served without restarting the relay.
func TestWildcardConfigPicksUpRotatedPair(t *testing.T) {
	cfg, certFile, keyFile, derA := armedWildcard(t, "public.getpiper.co")
	if _, got := served(t, cfg, "app.public.getpiper.co"); !bytes.Equal(got, derA) {
		t.Fatal("first handshake did not serve the cert on disk")
	}

	derB := writeWildcardPair(t, certFile, keyFile, "public.getpiper.co", time.Now())
	_, got := served(t, cfg, "app.public.getpiper.co")
	if bytes.Equal(got, derA) {
		t.Fatal("still serving the pre-rotation certificate after the files were replaced")
	}
	if !bytes.Equal(got, derB) {
		t.Fatal("served certificate is neither the old nor the new pair")
	}
}

// TestWildcardConfigServesRotatedPairOverHandshake proves the rotation reaches
// the wire, not just the callback: a real TLS client sees the new leaf.
func TestWildcardConfigServesRotatedPairOverHandshake(t *testing.T) {
	cfg, certFile, keyFile, derA := armedWildcard(t, "public.getpiper.co")

	handshake := func() []byte {
		t.Helper()
		c, s := net.Pipe()
		defer c.Close()
		defer s.Close()
		srv := tls.Server(s, cfg)
		go func() { _ = srv.Handshake() }()
		cli := tls.Client(c, &tls.Config{InsecureSkipVerify: true, ServerName: "app.public.getpiper.co"})
		if err := cli.Handshake(); err != nil {
			t.Fatalf("client handshake: %v", err)
		}
		peers := cli.ConnectionState().PeerCertificates
		if len(peers) == 0 {
			t.Fatal("server presented no certificate")
		}
		return peers[0].Raw
	}

	if got := handshake(); !bytes.Equal(got, derA) {
		t.Fatal("handshake did not present the cert on disk")
	}
	derB := writeWildcardPair(t, certFile, keyFile, "public.getpiper.co", time.Now())
	if got := handshake(); !bytes.Equal(got, derB) {
		t.Fatal("handshake after rotation did not present the renewed certificate")
	}
}

// TestWildcardConfigDoesNotReparseUnchangedFiles pins the cost side of the
// contract: the reload decision is made from a stat, so an untouched pair is
// never parsed twice however many connections arrive.
//
// The second half is the decisive check. It swaps in a *different valid pair*
// while restoring the original (mtime, size) — an implementation that parsed
// per handshake would serve the new certificate, one that gates on the stat
// keeps serving the old one. The first half (pointer identity) shows no reparse
// was stored on a genuinely untouched pair.
func TestWildcardConfigDoesNotReparseUnchangedFiles(t *testing.T) {
	cfg, certFile, keyFile, derA := armedWildcard(t, "public.getpiper.co")
	first, _ := served(t, cfg, "app.public.getpiper.co")
	for i := 0; i < 5; i++ {
		again, _ := served(t, cfg, "app.public.getpiper.co")
		if again != first {
			t.Fatalf("call %d re-parsed an untouched pair (new *tls.Certificate)", i+2)
		}
	}

	fi, err := os.Stat(certFile)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	writeWildcardPair(t, certFile, keyFile, "public.getpiper.co", fi.ModTime())
	if _, got := served(t, cfg, "app.public.getpiper.co"); !bytes.Equal(got, derA) {
		t.Fatal("re-read a pair whose mtime and size were unchanged — the stat gate is not being consulted")
	}
}

// TestWildcardConfigKeepsLastGoodCertOnFailedReload: a renewal caught
// mid-write, or a truncated file, must not fail handshakes. The listener keeps
// serving the last certificate that parsed.
func TestWildcardConfigKeepsLastGoodCertOnFailedReload(t *testing.T) {
	cfg, certFile, keyFile, derA := armedWildcard(t, "public.getpiper.co")
	served(t, cfg, "app.public.getpiper.co")

	for _, tc := range []struct {
		name  string
		write func()
	}{
		{"truncated", func() { writeFilePadded(t, certFile, nil, time.Now()) }},
		{"garbage", func() { writeFilePadded(t, certFile, []byte("-----BEGIN CERTIFICATE-----\nnope\n"), time.Now()) }},
		{"cert of another pair", func() {
			other, _ := newWildcardPEM(t, "public.getpiper.co")
			writeFilePadded(t, certFile, other, time.Now())
		}},
		{"cert file removed", func() {
			if err := os.Remove(certFile); err != nil {
				t.Fatalf("remove: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.write()
			for i := 0; i < 3; i++ {
				if _, got := served(t, cfg, "app.public.getpiper.co"); !bytes.Equal(got, derA) {
					t.Fatal("a failed reload changed what is served; want the last good certificate")
				}
			}
		})
	}

	// And a good pair written after the failures is still picked up.
	derB := writeWildcardPair(t, certFile, keyFile, "public.getpiper.co", time.Now())
	if _, got := served(t, cfg, "app.public.getpiper.co"); !bytes.Equal(got, derB) {
		t.Fatal("did not recover: a valid pair written after a failed reload was not picked up")
	}
}

// TestWildcardConfigLogsMissingKeyFile pins the failure log to the file that
// actually failed. Removing only the key must not make the log name the cert.
func TestWildcardConfigLogsMissingKeyFile(t *testing.T) {
	cfg, _, keyFile, _ := armedWildcard(t, "public.getpiper.co")

	origOut := log.Writer()
	origFlags := log.Flags()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(origOut)
		log.SetFlags(origFlags)
	}()

	if err := os.Remove(keyFile); err != nil {
		t.Fatalf("remove key file: %v", err)
	}
	served(t, cfg, "app.public.getpiper.co")

	got := buf.Bytes()
	if !bytes.Contains(got, []byte("relay: wildcard cert "+keyFile+":")) {
		t.Fatalf("log message does not name the missing key file in the prefix\nlog: %s", got)
	}
	if !bytes.Contains(got, []byte("(serving the last good certificate)")) {
		t.Fatalf("log message missing expected suffix\nlog: %s", got)
	}
}

// captureLog redirects the standard logger into a buffer for the duration of
// the test, the way the rest of the package does it.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })
	return &buf
}

// countLines returns the logged lines containing sub.
func countLines(buf *bytes.Buffer, sub string) []string {
	var hits []string
	for _, line := range bytes.Split(buf.Bytes(), []byte("\n")) {
		if len(line) > 0 && bytes.Contains(line, []byte(sub)) {
			hits = append(hits, string(line))
		}
	}
	return hits
}

// TestWildcardConfigLogsOutageAgainAfterRecovery: an outage that clears and
// comes back is two incidents and must be two log lines. The recovery here is
// the hostile one — the key returns with its original bytes, size and mtime, so
// nothing is reloaded and the *only* evidence of health is the fast path
// serving the cached pair. Suppression that is never re-armed there swallows
// the second outage entirely.
func TestWildcardConfigLogsOutageAgainAfterRecovery(t *testing.T) {
	cfg, _, keyFile, _ := armedWildcard(t, "public.getpiper.co")
	served(t, cfg, "app.public.getpiper.co")

	keyBytes, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatalf("read key file: %v", err)
	}
	fi, err := os.Stat(keyFile)
	if err != nil {
		t.Fatalf("stat key file: %v", err)
	}
	mod := fi.ModTime()

	buf := captureLog(t)

	removeKey := func() {
		t.Helper()
		if err := os.Remove(keyFile); err != nil {
			t.Fatalf("remove key file: %v", err)
		}
	}
	restoreKey := func() {
		t.Helper()
		if err := os.WriteFile(keyFile, keyBytes, 0o600); err != nil {
			t.Fatalf("restore key file: %v", err)
		}
		if err := os.Chtimes(keyFile, mod, mod); err != nil {
			t.Fatalf("chtimes key file: %v", err)
		}
		again, err := os.Stat(keyFile)
		if err != nil {
			t.Fatalf("stat restored key file: %v", err)
		}
		if again.Size() != fi.Size() || !again.ModTime().Equal(mod) {
			t.Fatalf("restored key file has stamps (%d, %v), want (%d, %v): the test no longer reproduces recovery without a reload",
				again.Size(), again.ModTime(), fi.Size(), mod)
		}
	}

	removeKey()
	served(t, cfg, "app.public.getpiper.co")
	if n := len(countLines(buf, keyFile)); n != 1 {
		t.Fatalf("first outage logged %d lines naming the key file, want 1\nlog: %s", n, buf.Bytes())
	}

	restoreKey()
	served(t, cfg, "app.public.getpiper.co")

	removeKey()
	served(t, cfg, "app.public.getpiper.co")

	if got := countLines(buf, keyFile); len(got) != 2 {
		t.Fatalf("two separate outages logged %d lines, want 2 — the failure state was never re-armed after the files became healthy\nlog: %s",
			len(got), buf.Bytes())
	}
}

// TestWildcardConfigLogsEachBadVersionWithTheSameError is the other half: the
// suppression must be keyed to the version observed on disk, not to the text of
// the message. Two distinct corrupt certificates that fail with the identical
// error each get a line — while repeated handshakes over one of them still get
// only the one.
func TestWildcardConfigLogsEachBadVersionWithTheSameError(t *testing.T) {
	cfg, certFile, keyFile, derA := armedWildcard(t, "public.getpiper.co")
	served(t, cfg, "app.public.getpiper.co")

	buf := captureLog(t)

	// Both bodies are non-PEM, so both fail with the same parse error, and
	// writeFilePadded pads both to pemPad — only the mtime tells them apart.
	corrupt := func(body string, mod time.Time) {
		t.Helper()
		writeFilePadded(t, certFile, []byte(body), mod)
		for i := 0; i < 3; i++ {
			if _, got := served(t, cfg, "app.public.getpiper.co"); !bytes.Equal(got, derA) {
				t.Fatal("a corrupt cert file changed what is served; want the last good certificate")
			}
		}
	}

	corrupt("-----BEGIN CERTIFICATE-----\nnope\n", time.Now().Add(-time.Minute))
	corrupt("-----BEGIN CERTIFICATE-----\nstill not a certificate\n", time.Now())

	got := countLines(buf, "relay: wildcard cert reload from "+certFile+"/"+keyFile+":")
	if len(got) != 2 {
		t.Fatalf("two distinct corrupt versions logged %d lines, want 2 (one per version, and no more than one per version)\nlog: %s",
			len(got), buf.Bytes())
	}
	if got[0] != got[1] {
		t.Fatalf("the two versions did not fail with the same message, so this test no longer covers message-keyed suppression:\n%s\n%s", got[0], got[1])
	}
}

// TestWildcardConfigConcurrentGetCertificate hammers the callback from many
// goroutines across several rotations — run under -race. Every call must return
// a certificate that actually exists on disk (some pair we wrote), never an
// error and never nil, and the final call must see the last pair written.
func TestWildcardConfigConcurrentGetCertificate(t *testing.T) {
	cfg, certFile, keyFile, derA := armedWildcard(t, "public.getpiper.co")

	var mu sync.Mutex
	known := [][]byte{derA}

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < 16; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				crt, err := cfg.GetCertificate(&tls.ClientHelloInfo{ServerName: "app.public.getpiper.co"})
				if err != nil || crt == nil || len(crt.Certificate) == 0 {
					t.Errorf("GetCertificate during rotation = %v, %v", crt, err)
					return
				}
				mu.Lock()
				ok := false
				for _, der := range known {
					if bytes.Equal(crt.Certificate[0], der) {
						ok = true
						break
					}
				}
				mu.Unlock()
				if !ok {
					t.Error("served a certificate that was never written to disk")
					return
				}
			}
		}()
	}

	var last []byte
	for i := 0; i < 8; i++ {
		mod := time.Now().Add(time.Duration(i) * time.Second)
		certPEM, keyPEM := newWildcardPEM(t, "public.getpiper.co")
		blk, _ := pem.Decode(certPEM)
		mu.Lock()
		known = append(known, blk.Bytes)
		mu.Unlock()
		writeFilePadded(t, certFile, certPEM, mod)
		writeFilePadded(t, keyFile, keyPEM, mod)
		last = blk.Bytes
	}
	close(stop)
	readers.Wait()

	if _, got := served(t, cfg, "app.public.getpiper.co"); !bytes.Equal(got, last) {
		t.Fatal("after the rotations settled, the last pair on disk is not the one served")
	}
}
