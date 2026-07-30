package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"strings"
	"testing"
)

// New must refuse ambiguous or empty challenge configuration: exactly one of
// DNSProvider (wildcard, box-wide BYO) or ALPNSolver (exact-host, per-app
// BYO) — and it must refuse before any ACME network I/O, which is what lets
// this test run offline.
func TestNewRequiresExactlyOneChallengeMode(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	cases := []struct {
		name string
		cfg  Config
	}{
		{"neither", Config{Email: "e@example.com", AccountKey: key}},
		{"both", Config{Email: "e@example.com", AccountKey: key, DNSProvider: fakeDNS{}, ALPNSolver: fakeDNS{}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := New(c.cfg); err == nil {
				t.Fatal("New() error = nil, want exactly-one-challenge-mode error")
			}
		})
	}
}

// TestNewRejectsTypedNilProviders pins #242: a nil pointer boxed into the
// challenge.Provider interface compares non-nil with ==, so the old check
// counted it as "set" and let New proceed toward ACME network I/O. The test
// must therefore assert the *specific* exactly-one-challenge-mode error: with
// the old check these configs either errored much later (a failed account
// registration against the CA) or, on a networked box, would actually have
// registered — a bare "err != nil" assertion would pass for the wrong reason.
// fakeDNS has value receivers, so *fakeDNS satisfies challenge.Provider.
func TestNewRejectsTypedNilProviders(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	var nilDNS *fakeDNS     // typed nil: interface is non-nil, the value is
	var nilALPN *ALPNSolver // same, for the solver side
	cases := []struct {
		name string
		cfg  Config
	}{
		{"typed-nil dns", Config{Email: "e@example.com", AccountKey: key, DNSProvider: nilDNS}},
		{"typed-nil alpn", Config{Email: "e@example.com", AccountKey: key, ALPNSolver: nilALPN}},
		{"typed-nil both", Config{Email: "e@example.com", AccountKey: key, DNSProvider: nilDNS, ALPNSolver: nilALPN}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := New(c.cfg)
			if err == nil || !strings.Contains(err.Error(), "exactly one of DNSProvider or ALPNSolver") {
				t.Fatalf("New() error = %v, want the exactly-one-challenge-mode rejection", err)
			}
		})
	}
}
