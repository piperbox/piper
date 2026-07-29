package certs

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-acme/lego/v4/challenge/tlsalpn01"
)

// dialSolver completes an acme-tls/1 handshake against the solver with the
// given SNI — what an ACME validator does — and returns the connection state.
func dialSolver(t *testing.T, addr, sni string) (tls.ConnectionState, error) {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, &tls.Config{
		ServerName:         sni,
		NextProtos:         []string{tlsalpn01.ACMETLS1Protocol},
		InsecureSkipVerify: true,
	})
	if err != nil {
		return tls.ConnectionState{}, err
	}
	defer conn.Close()
	return conn.ConnectionState(), nil
}

// assertACMEDigest checks the RFC 8737 acmeIdentifier extension (OID
// 1.3.6.1.5.5.7.1.31) carries the SHA-256 digest of keyAuth — the thing the
// ACME validator actually verifies.
func assertACMEDigest(t *testing.T, leaf *x509.Certificate, keyAuth string) {
	t.Helper()
	want := sha256.Sum256([]byte(keyAuth))
	oid := asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 1, 31}
	for _, ext := range leaf.Extensions {
		if !ext.Id.Equal(oid) {
			continue
		}
		var digest []byte
		if _, err := asn1.Unmarshal(ext.Value, &digest); err != nil {
			t.Fatalf("unmarshal acmeIdentifier extension: %v", err)
		}
		if !bytes.Equal(digest, want[:]) {
			t.Fatal("acmeIdentifier digest does not match keyAuth")
		}
		return
	}
	t.Fatal("presented cert has no acmeIdentifier extension")
}

func TestALPNSolverAnswersChallenge(t *testing.T) {
	s, err := NewALPNSolver("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewALPNSolver: %v", err)
	}
	defer s.Close()
	const domain, keyAuth = "myshop.example.com", "token.account-thumbprint"
	if err := s.Present(domain, "token", keyAuth); err != nil {
		t.Fatalf("Present: %v", err)
	}

	cs, err := dialSolver(t, s.Addr(), domain)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if cs.NegotiatedProtocol != tlsalpn01.ACMETLS1Protocol {
		t.Fatalf("negotiated %q, want %q", cs.NegotiatedProtocol, tlsalpn01.ACMETLS1Protocol)
	}
	leaf := cs.PeerCertificates[0]
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != domain {
		t.Fatalf("cert DNSNames = %v, want [%s]", leaf.DNSNames, domain)
	}
	assertACMEDigest(t, leaf, keyAuth)
}

func TestALPNSolverConcurrentDomains(t *testing.T) {
	s, err := NewALPNSolver("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewALPNSolver: %v", err)
	}
	defer s.Close()
	for _, d := range []string{"a.example.com", "b.example.com"} {
		if err := s.Present(d, "token", "auth-"+d); err != nil {
			t.Fatalf("Present(%s): %v", d, err)
		}
	}
	for _, d := range []string{"a.example.com", "b.example.com"} {
		cs, err := dialSolver(t, s.Addr(), d)
		if err != nil {
			t.Fatalf("handshake for %s: %v", d, err)
		}
		if got := cs.PeerCertificates[0].DNSNames; len(got) != 1 || got[0] != d {
			t.Fatalf("SNI %s got cert for %v", d, got)
		}
	}
}

// TestALPNSolverRejectsClientsWithoutACMEProto ensures the solver only
// completes handshakes for clients that actually negotiate acme-tls/1. Go's
// server-side ALPN completes a handshake for a client offering no ALPN
// protocols at all (it only rejects a non-overlapping non-empty list), so
// without this check any TLS client hitting the solver's SNI would get the
// challenge cert served to it — not just the ACME validator.
func TestALPNSolverRejectsClientsWithoutACMEProto(t *testing.T) {
	s, err := NewALPNSolver("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewALPNSolver: %v", err)
	}
	defer s.Close()
	const domain, keyAuth = "noproto.example.com", "token.account-thumbprint"
	if err := s.Present(domain, "token", keyAuth); err != nil {
		t.Fatalf("Present: %v", err)
	}

	conn, err := tls.Dial("tcp", s.Addr(), &tls.Config{
		ServerName:         domain,
		NextProtos:         nil,
		InsecureSkipVerify: true,
	})
	if err == nil {
		conn.Close()
		t.Fatal("handshake succeeded for a client offering no ALPN protocols, want failure")
	}
}

func TestALPNSolverCleanUp(t *testing.T) {
	s, err := NewALPNSolver("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewALPNSolver: %v", err)
	}
	defer s.Close()
	const domain, keyAuth = "gone.example.com", "token.thumb"
	if err := s.Present(domain, "token", keyAuth); err != nil {
		t.Fatalf("Present: %v", err)
	}
	if err := s.CleanUp(domain, "token", keyAuth); err != nil {
		t.Fatalf("CleanUp: %v", err)
	}
	if _, err := dialSolver(t, s.Addr(), domain); err == nil {
		t.Fatal("handshake succeeded after CleanUp, want failure")
	}
}

// syncLogBuffer is a concurrency-safe log sink: the solver's handshake
// goroutine can still be inside log.Printf writing to it while the test polls
// for the line, and a bare bytes.Buffer is not safe for that concurrent use.
type syncLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestALPNSolverLogsHandshakeFailures pins the operator signal for failed
// validations (#242): an unknown-SNI miss must leave a log line. With
// per-domain renewal loops running, a validator that keeps failing would
// otherwise be silent until someone went looking for the Obtain error. The
// happy path stays quiet — nothing is asserted for successful handshakes
// because nothing is logged.
func TestALPNSolverLogsHandshakeFailures(t *testing.T) {
	var logged syncLogBuffer
	prevOut := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(prevOut) })

	s, err := NewALPNSolver("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewALPNSolver: %v", err)
	}
	defer s.Close()

	if _, err := dialSolver(t, s.Addr(), "unknown.example.com"); err == nil {
		t.Fatal("handshake succeeded for an SNI with no pending challenge, want failure")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(logged.String(), `no pending challenge for "unknown.example.com"`) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("failed handshake not logged; log = %q", logged.String())
}
