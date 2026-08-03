package domain

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/piperbox/piper/internal/store"
)

// newDNSTargetManager builds a Manager with an explicit base domain and dial
// host. baseDomain empty exercises the no-base-domain fallback.
func newDNSTargetManager(t *testing.T, baseDomain, relayHost string) (*Manager, *store.Store) {
	t.Helper()
	dataDir := t.TempDir()
	st, err := store.Open(filepath.Join(dataDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := New(Options{
		Store:     st,
		Issuer:    func(provider, token string) (Issuer, error) { return &fakeIssuer{}, nil },
		AppIssuer: func() (Issuer, error) { return &fakeIssuer{}, nil },
		Proxy:     &fakeProxy{}, Router: &fakeRouter{}, DataDir: dataDir,
		BaseDomain: baseDomain, RelayHost: relayHost, HTTPSListen: ":8443",
	})
	// Close before t.TempDir's RemoveAll (cleanups run LIFO), as in the other
	// Manager helpers here (#279).
	t.Cleanup(m.Close)
	m.SetRelay(&fakeNotifier{})
	m.retryDelay = func(int) time.Duration { return time.Millisecond }
	m.dnsWait = time.Millisecond
	return m, st
}

// resolveTable resolves each name to exactly the address the table gives it, so
// a points-at comparison can only succeed against the name it really probed;
// anything unlisted is NXDOMAIN.
func resolveTable(table map[string]string) func(context.Context, string) ([]net.IP, error) {
	return func(_ context.Context, host string) ([]net.IP, error) {
		ip, ok := table[host]
		if !ok {
			return nil, fmt.Errorf("no such host: %s", host)
		}
		return []net.IP{net.ParseIP(ip)}, nil
	}
}

func mustCreateAppDomain(t *testing.T, st *store.Store, app, domain string) {
	t.Helper()
	if _, err := st.CreateApp(app, 8080); err != nil {
		t.Fatal(err)
	}
	if err := st.AddAppDomain(domain, app); err != nil {
		t.Fatal(err)
	}
}

// PIPER_RELAY_ADDR answers "where does this box dial the tunnel"; the DNS
// record answers "where should the public point this domain". On a box dialling
// the relay over loopback the two diverge, and following the dial address makes
// the guided setup instruct `CNAME 127.0.0.1` — not even a legal CNAME target
// (#434). The record follows the base domain, which does resolve to the relay.
func TestAppDomainRecordTargetsBaseDomainNotDialHost(t *testing.T) {
	const base = "ab12-alice.public.getpiper.co"
	m, st := newDNSTargetManager(t, base, "127.0.0.1") // PIPER_RELAY_ADDR=127.0.0.1:7000
	m.resolve = resolveTable(map[string]string{
		"shop.example.com": "203.0.113.7",
		base:               "203.0.113.7",
		"127.0.0.1":        "127.0.0.1",
	})
	mustCreateAppDomain(t, st, "blog", "shop.example.com")

	got, err := m.AppDomainStatus("shop.example.com")
	if err != nil {
		t.Fatalf("AppDomainStatus: %v", err)
	}
	if len(got.DNSRecords) != 1 {
		t.Fatalf("DNSRecords = %+v, want exactly one", got.DNSRecords)
	}
	rec := got.DNSRecords[0]
	if rec.Value == "127.0.0.1" {
		t.Errorf("CNAME target = %q: the dial address, which is not a legal CNAME target", rec.Value)
	}
	if rec.Type != "CNAME" || rec.Name != "shop.example.com" || rec.Value != base {
		t.Errorf("record = %+v, want CNAME shop.example.com -> %s", rec, base)
	}
	// Same value drives the gate: the app domain points at the base domain's
	// address, so dns_ok must be true even though it points nowhere near the
	// loopback dial address.
	if !got.DNSOK {
		t.Error("dns_ok = false, want true: the domain resolves to the base domain's address")
	}
}

// The box-wide (#102) records carry the same target as the per-app ones.
func TestBoxWideRecordsTargetBaseDomain(t *testing.T) {
	const base = "ab12-alice.public.getpiper.co"
	m, _ := newDNSTargetManager(t, base, "127.0.0.1")

	recs := m.dnsRecords("example.com")
	if len(recs) != 2 {
		t.Fatalf("dnsRecords = %+v, want two", recs)
	}
	for _, r := range recs {
		if r.Value == "127.0.0.1" {
			t.Errorf("record %+v targets the dial address", r)
		}
		if r.Value != base {
			t.Errorf("record %+v value = %q, want %q", r, r.Value, base)
		}
	}
}

// The issuance gate is the other consumer of the target: pointed at the dial
// address it could never flip, so the cert never issued (#434). Its wait-DNS
// hint names the base domain, and only a domain pointed there clears it.
func TestAppDomainIssueGateComparesAgainstBaseDomain(t *testing.T) {
	const base = "ab12-alice.public.getpiper.co"
	m, st := newDNSTargetManager(t, base, "127.0.0.1")
	mustCreateAppDomain(t, st, "blog", "shop.example.com")
	snap := store.AppDomain{Domain: "shop.example.com", App: "blog"}

	// Pointing at the loopback dial address is NOT pointing at the relay.
	m.resolve = resolveTable(map[string]string{
		"shop.example.com": "127.0.0.1",
		base:               "203.0.113.7",
		"127.0.0.1":        "127.0.0.1",
	})
	err := m.appIssueOnce(snap)
	if !errors.Is(err, errWaitDNS) {
		t.Fatalf("err = %v, want errWaitDNS", err)
	}
	if !strings.Contains(err.Error(), base) {
		t.Errorf("wait-DNS hint %q does not name the base domain %q", err, base)
	}
	if strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("wait-DNS hint %q tells the user to point DNS at the dial address", err)
	}

	// Pointing at the base domain's address clears the gate.
	m.resolve = resolveTable(map[string]string{
		"shop.example.com": "203.0.113.7",
		base:               "203.0.113.7",
		"127.0.0.1":        "127.0.0.1",
	})
	if err := m.appIssueOnce(snap); err != nil {
		t.Fatalf("appIssueOnce with DNS pointed at the base domain: %v", err)
	}
}

// A box with no base domain configured keeps the pre-#434 behaviour: the dial
// host is the target, for both the record and the gate.
func TestDNSTargetFallsBackToRelayHostWithoutBaseDomain(t *testing.T) {
	m, st := newDNSTargetManager(t, "", "relay.example.net")
	m.resolve = resolveTable(map[string]string{
		"shop.example.com":  "203.0.113.7",
		"relay.example.net": "203.0.113.7",
	})
	mustCreateAppDomain(t, st, "blog", "shop.example.com")

	got, err := m.AppDomainStatus("shop.example.com")
	if err != nil {
		t.Fatalf("AppDomainStatus: %v", err)
	}
	if len(got.DNSRecords) != 1 || got.DNSRecords[0].Value != "relay.example.net" {
		t.Errorf("DNSRecords = %+v, want CNAME -> relay.example.net", got.DNSRecords)
	}
	if !got.DNSOK {
		t.Error("dns_ok = false, want true: the domain resolves to the relay host's address")
	}
	for _, r := range m.dnsRecords("example.com") {
		if r.Value != "relay.example.net" {
			t.Errorf("box-wide record %+v value = %q, want relay.example.net", r, r.Value)
		}
	}

	// The gate falls back too: its hint names the relay host.
	m.resolve = resolveTable(map[string]string{
		"shop.example.com":  "198.51.100.9",
		"relay.example.net": "203.0.113.7",
	})
	err = m.appIssueOnce(store.AppDomain{Domain: "shop.example.com", App: "blog"})
	if !errors.Is(err, errWaitDNS) {
		t.Fatalf("err = %v, want errWaitDNS", err)
	}
	if !strings.Contains(err.Error(), "relay.example.net") {
		t.Errorf("wait-DNS hint %q does not name the relay host", err)
	}
}
