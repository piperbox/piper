package certs

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/go-acme/lego/v4/challenge/tlsalpn01"
)

// alpnHandshakeTimeout bounds one validator connection's handshake so a
// stalled peer can't pin a goroutine.
const alpnHandshakeTimeout = 10 * time.Second

// ALPNSolver answers TLS-ALPN-01 challenges. It is both a lego
// challenge.Provider (Present stores the RFC 8737 challenge cert per domain,
// CleanUp drops it) and a loopback TLS listener that completes the acme-tls/1
// handshake with the cert matching the ClientHello's SNI. The passthrough
// path splices acme-tls/1 connections here (see cmd/piperd newDialLocal);
// no HTTP is ever spoken and Caddy is not involved.
type ALPNSolver struct {
	ln net.Listener

	mu    sync.Mutex
	certs map[string]*tls.Certificate
}

// NewALPNSolver starts the solver's TLS listener on listenAddr
// ("127.0.0.1:0" for an ephemeral port; the pebble test pins the port its
// validator dials).
func NewALPNSolver(listenAddr string) (*ALPNSolver, error) {
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("alpn solver: %w", err)
	}
	s := &ALPNSolver{certs: map[string]*tls.Certificate{}}
	s.ln = tls.NewListener(ln, &tls.Config{
		NextProtos:     []string{tlsalpn01.ACMETLS1Protocol},
		GetCertificate: s.getCertificate,
	})
	go s.serve()
	return s, nil
}

// serve completes handshakes; the validator inspects the presented cert and
// closes. Exits when Close shuts the listener. A failed handshake — unknown
// SNI, no acme-tls/1 offered, a stalled peer hitting the deadline — is logged,
// not just dropped: with per-domain renewal loops running, repeated validator
// failures are otherwise invisible until someone reads the Obtain error (#242).
// The happy path stays quiet: a completed handshake logs nothing.
func (s *ALPNSolver) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(alpnHandshakeTimeout))
			if err := c.(*tls.Conn).Handshake(); err != nil {
				log.Printf("alpn solver: handshake from %s: %v", c.RemoteAddr(), err)
			}
		}(conn)
	}
}

func (s *ALPNSolver) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	offered := false
	for _, p := range hello.SupportedProtos {
		if p == tlsalpn01.ACMETLS1Protocol {
			offered = true
			break
		}
	}
	if !offered {
		return nil, fmt.Errorf("alpn solver: client did not offer %s", tlsalpn01.ACMETLS1Protocol)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if crt, ok := s.certs[hello.ServerName]; ok {
		return crt, nil
	}
	return nil, fmt.Errorf("alpn solver: no pending challenge for %q", hello.ServerName)
}

// Present implements challenge.Provider: build and arm the challenge cert.
func (s *ALPNSolver) Present(domain, token, keyAuth string) error {
	crt, err := tlsalpn01.ChallengeCert(domain, keyAuth)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.certs[domain] = crt
	s.mu.Unlock()
	return nil
}

// CleanUp implements challenge.Provider: disarm the domain's challenge cert.
func (s *ALPNSolver) CleanUp(domain, token, keyAuth string) error {
	s.mu.Lock()
	delete(s.certs, domain)
	s.mu.Unlock()
	return nil
}

// Addr is where the listener landed — the passthrough peek's splice target.
func (s *ALPNSolver) Addr() string { return s.ln.Addr().String() }

// Close stops the listener.
func (s *ALPNSolver) Close() error { return s.ln.Close() }
