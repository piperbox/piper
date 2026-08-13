package domain

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/piperbox/piper/internal/agent"
	"github.com/piperbox/piper/internal/caddy"
	"github.com/piperbox/piper/internal/store"
)

// selfSignedPEM issues a throwaway wildcard cert so the fake issuer's output
// parses (activation reads NotAfter and hostname coverage from it).
func selfSignedPEM(t *testing.T, notAfter time.Time, domains ...string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: domains[0]},
		DNSNames:     domains,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	kb, _ := x509.MarshalECPrivateKey(key)
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	return certPEM, keyPEM
}

type fakeIssuer struct {
	mu       sync.Mutex
	calls    int
	failures int // fail the first N Obtain calls
	notAfter time.Time
}

func (f *fakeIssuer) Obtain(domains []string) ([]byte, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls <= f.failures {
		return nil, nil, errors.New("acme: boom")
	}
	na := f.notAfter
	if na.IsZero() {
		// Add 1 hour per call to simulate time passing between Obtain calls
		na = time.Now().Add(90 * 24 * time.Hour).Add(time.Duration(f.calls) * time.Hour)
	}
	c, k := selfSignedPEMForObtain(na, domains)
	return c, k, nil
}

// selfSignedPEMForObtain is selfSignedPEM without *testing.T (Obtain has none).
func selfSignedPEMForObtain(notAfter time.Time, domains []string) ([]byte, []byte) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: domains[0]},
		DNSNames:     domains,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	kb, _ := x509.MarshalECPrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	return certPEM, keyPEM
}

func (f *fakeIssuer) obtainCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type fakeProxy struct {
	mu       sync.Mutex
	ensured  []string
	replaced int
	certs    []caddy.CertPair // the last full set synced
	routes   map[string]int
	removed  []string
}

func (f *fakeProxy) EnsureHTTPS(listen string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensured = append(f.ensured, listen)
	return nil
}
func (f *fakeProxy) ReplaceCerts(pairs []caddy.CertPair) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replaced++
	f.certs = append([]caddy.CertPair(nil), pairs...)
	return nil
}
func (f *fakeProxy) certCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.certs)
}
func (f *fakeProxy) UpsertRouteTLS(host string, port int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.routes == nil {
		f.routes = map[string]int{}
	}
	f.routes[host] = port
	return nil
}
func (f *fakeProxy) RemoveRoute(host string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, host)
	return nil
}
func (f *fakeProxy) route(host string) (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.routes[host]
	return p, ok
}

// fakeNotifier records the relay claim ops as "add:<d>" / "confirm:<d>" /
// "remove:<d>". fail rejects every op; failAdd/failConfirm/failRemove reject
// one op kind.
type fakeNotifier struct {
	mu                            sync.Mutex
	got                           []string
	fail                          error
	failAdd, failConfirm, failRem error
}

func (f *fakeNotifier) op(kind, d string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	kindFail := map[string]error{"add": f.failAdd, "confirm": f.failConfirm, "remove": f.failRem}[kind]
	if kindFail != nil {
		return kindFail
	}
	f.got = append(f.got, kind+":"+d)
	return nil
}
func (f *fakeNotifier) AddCustomDomain(d string) error     { return f.op("add", d) }
func (f *fakeNotifier) ConfirmCustomDomain(d string) error { return f.op("confirm", d) }
func (f *fakeNotifier) RemoveCustomDomain(d string) error  { return f.op("remove", d) }
func (f *fakeNotifier) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.got) == 0 {
		return ""
	}
	return f.got[len(f.got)-1]
}
func (f *fakeNotifier) pushes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

// blockingIssuer holds each Obtain call open until the test releases it,
// exposing the minutes-long ACME window the C1 races live in.
type blockingIssuer struct {
	fakeIssuer
	entered     chan struct{} // receives one value when an Obtain enters
	release     chan struct{} // each Obtain proceeds after one receive
	releaseOnce sync.Once
}

func newBlockingIssuer() *blockingIssuer {
	return &blockingIssuer{
		entered: make(chan struct{}, 8),
		release: make(chan struct{}, 8),
	}
}

// releaseAll unblocks every parked and future Obtain, idempotently. Tests
// register it with t.Cleanup after the manager helper has registered
// t.Cleanup(m.Close); cleanups run LIFO, so releaseAll fires first and a
// failed barrier assertion cannot leave Close waiting forever on a parked
// Obtain. The Once keeps it safe when a test already released on the happy
// path.
func (b *blockingIssuer) releaseAll() { b.releaseOnce.Do(func() { close(b.release) }) }

func (b *blockingIssuer) Obtain(domains []string) ([]byte, []byte, error) {
	b.entered <- struct{}{}
	<-b.release
	return b.fakeIssuer.Obtain(domains)
}

func newTestManager(t *testing.T, iss *fakeIssuer) (*Manager, *store.Store, *fakeProxy, *fakeNotifier, string) {
	return newTestManagerWith(t, iss)
}

func newTestManagerWith(t *testing.T, iss Issuer) (*Manager, *store.Store, *fakeProxy, *fakeNotifier, string) {
	t.Helper()
	dataDir := t.TempDir()
	st, err := store.Open(filepath.Join(dataDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	proxy := &fakeProxy{}
	relay := &fakeNotifier{}
	m := New(Options{
		Store:  st,
		Issuer: func(provider, token string) (Issuer, error) { return iss, nil },
		Proxy:  proxy, DataDir: dataDir,
		RelayHost: "relay.example.net", HTTPSListen: ":8443",
	})
	// Join the lifecycle goroutines before the temp dir's RemoveAll: cleanups
	// run LIFO and t.TempDir registered its own cleanup above, so this Close
	// runs first (#279).
	t.Cleanup(m.Close)
	m.SetRelay(relay)
	m.retryDelay = func(int) time.Duration { return time.Millisecond }
	return m, st, proxy, relay, dataDir
}

// waitCeiling bounds the poll helpers below. They return the instant their
// condition holds, so a generous ceiling costs nothing on a fast machine and
// buys robustness on a loaded runner (#421).
const waitCeiling = 15 * time.Second

// waitFor polls cond until it holds (waitCeiling cap), failing with what.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitCeiling)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s: condition never held", what)
}

// attemptCountingNotifier counts Add/Confirm attempts, including failed ones —
// fakeNotifier only records successes, but a retry loop's liveness shows in
// its failed attempts: a second recorded attempt proves the loop is
// sequential and completed the whole previous cycle.
type attemptCountingNotifier struct {
	fakeNotifier
	adds, confirms atomic.Int64
}

func (c *attemptCountingNotifier) AddCustomDomain(d string) error {
	c.adds.Add(1)
	return c.fakeNotifier.AddCustomDomain(d)
}
func (c *attemptCountingNotifier) ConfirmCustomDomain(d string) error {
	c.confirms.Add(1)
	return c.fakeNotifier.ConfirmCustomDomain(d)
}
func (c *attemptCountingNotifier) addAttempts() int64     { return c.adds.Load() }
func (c *attemptCountingNotifier) confirmAttempts() int64 { return c.confirms.Load() }

// waitStatus polls the store until the domain config reaches status
// (waitCeiling cap).
func waitStatus(t *testing.T, st *store.Store, status string) store.DomainConfig {
	t.Helper()
	deadline := time.Now().Add(waitCeiling)
	var dc store.DomainConfig
	var err error
	for time.Now().Before(deadline) {
		dc, err = st.GetDomainConfig()
		if err == nil && dc.Status == status {
			return dc
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("status never reached %q (last: %+v, err %v)", status, dc, err)
	return dc
}

// waitError polls the store until the config carries a non-empty error — the
// first failed issuance attempt has recorded its outcome (waitCeiling cap).
// The status alone can't mark the moment: a fresh Set already reads "issuing".
func waitError(t *testing.T, st *store.Store) store.DomainConfig {
	t.Helper()
	deadline := time.Now().Add(waitCeiling)
	var dc store.DomainConfig
	var err error
	for time.Now().Before(deadline) {
		dc, err = st.GetDomainConfig()
		if err == nil && dc.Error != "" {
			return dc
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no failure recorded (last: %+v, err %v)", dc, err)
	return dc
}

func TestSetIssuesAndActivates(t *testing.T) {
	m, st, proxy, relay, dataDir := newTestManager(t, &fakeIssuer{})
	if _, err := st.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateDeployment("blog", "img", "ctr", 40001, "running", ""); err != nil {
		t.Fatal(err)
	}

	status, err := m.Set("Example.COM", "cloudflare", "cf-token")
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if status.Domain != "example.com" || status.Status != StatusIssuing {
		t.Fatalf("Set returned %+v", status)
	}

	dc := waitStatus(t, st, StatusActive)
	if dc.CertNotAfter.IsZero() {
		t.Fatal("active config has zero cert_not_after")
	}
	// Claim first, confirm after activation — the epic's per-domain ordering.
	if got := relay.pushes(); len(got) != 2 || got[0] != "add:example.com" || got[1] != "confirm:example.com" {
		t.Fatalf("relay ops = %v, want [add:example.com confirm:example.com]", got)
	}
	if p, ok := proxy.route("blog.example.com"); !ok || p != 40001 {
		t.Fatalf("blog route = %d,%v; want 40001 on blog.example.com", p, ok)
	}
	proxy.mu.Lock()
	ensured, replaced := proxy.ensured, proxy.replaced
	proxy.mu.Unlock()
	if len(ensured) == 0 || ensured[0] != ":8443" || replaced == 0 {
		t.Fatalf("proxy calls: ensured=%v replaced=%d", ensured, replaced)
	}
	for _, f := range []string{"cert.pem", "key.pem"} {
		fi, err := os.Stat(filepath.Join(dataDir, "domain", f))
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s perms = %v, want 0600", f, fi.Mode().Perm())
		}
	}
}

// Set documents that it returns the freshly-kicked "issuing" state, but it
// re-read the store after spawning the issue loop, so a loop that reached
// active first won the read and Set returned "active" instead (#354 — a CI
// flake, because a real ACME Obtain takes seconds and the fake one is
// instant). The genBumpHook seam fires between the snapshot and the spawn,
// so activating the config there is exactly the interleaving that used to
// lose: with the snapshot taken up front the return value is unaffected.
func TestSetReturnsIssuingSnapshot(t *testing.T) {
	m, st, _, _, _ := newTestManager(t, &fakeIssuer{})
	m.genBumpHook = func() {
		if err := st.UpdateDomainStatus("example.com", StatusActive, "", time.Now().Add(24*time.Hour)); err != nil {
			t.Errorf("activate during bump: %v", err)
		}
	}

	status, err := m.Set("example.com", "cloudflare", "tok")
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if status.Status != StatusIssuing {
		t.Fatalf("Set returned status %q, want %q", status.Status, StatusIssuing)
	}
	if status.Domain != "example.com" {
		t.Fatalf("Set returned domain %q, want example.com", status.Domain)
	}
}

func TestSetValidation(t *testing.T) {
	m, _, _, _, _ := newTestManager(t, &fakeIssuer{})
	if _, err := m.Set("not a domain", "cloudflare", "tok"); !errors.Is(err, ErrInvalidDomain) {
		t.Fatalf("bad domain: %v", err)
	}
	if _, err := m.Set("ok.example.com", "route53", "tok"); !errors.Is(err, ErrUnsupportedProvider) {
		t.Fatalf("bad provider: %v", err)
	}
	if _, err := m.Set("ok.example.com", "cloudflare", ""); !errors.Is(err, ErrTokenRequired) {
		t.Fatalf("empty token: %v", err)
	}
}

func TestIssueFailureRecordsFailedThenRetriesToActive(t *testing.T) {
	iss := &fakeIssuer{failures: 2}
	m, st, _, _, _ := newTestManager(t, iss)
	if _, err := m.Set("example.com", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	dc := waitStatus(t, st, StatusActive)
	if dc.Error != "" {
		t.Fatalf("active config still carries error %q", dc.Error)
	}
	if iss.obtainCalls() < 3 {
		t.Fatalf("obtain calls = %d, want ≥3 (2 failures + success)", iss.obtainCalls())
	}
}

func TestRelayRejectionSurfacesAsFailed(t *testing.T) {
	iss := newBlockingIssuer()
	m, st, _, _, _ := newTestManagerWith(t, iss)
	t.Cleanup(iss.releaseAll) // runs before m.Close (LIFO): unblocks a parked Obtain
	attempts := &attemptCountingNotifier{}
	attempts.failAdd = errors.New("domain already in use")
	m.SetRelay(attempts)
	if _, err := m.Set("example.com", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	dc := waitStatus(t, st, StatusFailed)
	if dc.Error != "domain already in use" {
		t.Fatalf("error = %q", dc.Error)
	}
	// The claim precedes issuance, so a contested name never burns an ACME
	// order at all. The second add attempt proves the sequential retry loop
	// completed a whole cycle (failed attempts are not recorded by the
	// embedded fake, only counted here); while the relay rejects, no cycle
	// can reach Obtain — the blocking issuer would park and signal entered
	// instead of letting a slow runner pass before any retry ran.
	waitFor(t, "relay add retried while rejected", func() bool {
		return attempts.addAttempts() >= 2
	})
	select {
	case <-iss.entered:
		t.Fatal("obtained a cert while the relay rejects the claim")
	default:
	}
	// Unblock the relay; the loop must converge to active through exactly one
	// gated Obtain.
	attempts.mu.Lock()
	attempts.failAdd = nil
	attempts.mu.Unlock()
	select {
	case <-iss.entered:
	case <-time.After(waitCeiling):
		t.Fatal("issuance never reached Obtain after the relay unblocked")
	}
	iss.release <- struct{}{}
	waitStatus(t, st, StatusActive)
}

// A relay confirm failure after the cert was obtained must retry on the disk
// cert instead of re-obtaining (rate limits).
func TestRelayConfirmFailureReusesDiskCert(t *testing.T) {
	iss := newBlockingIssuer()
	m, st, _, _, _ := newTestManagerWith(t, iss)
	t.Cleanup(iss.releaseAll) // runs before m.Close (LIFO): unblocks a parked Obtain
	attempts := &attemptCountingNotifier{}
	attempts.failConfirm = errors.New("tunnel down")
	m.SetRelay(attempts)
	if _, err := m.Set("example.com", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-iss.entered: // the one genuine Obtain, behind the relay claim
	case <-time.After(waitCeiling):
		t.Fatal("issuance never reached Obtain")
	}
	iss.release <- struct{}{}
	waitStatus(t, st, StatusFailed)
	if before := iss.obtainCalls(); before != 1 {
		t.Fatalf("obtain calls = %d before the confirm failure, want exactly 1", before)
	}
	// Confirm-only retries must reuse the disk cert, not re-obtain. The second
	// confirm attempt proves the sequential retry loop completed a whole
	// retry cycle; a re-Obtain in any cycle would park at the blocking issuer
	// and signal entered instead of going unnoticed on a slow runner.
	waitFor(t, "relay confirm retried while failing", func() bool {
		return attempts.confirmAttempts() >= 2
	})
	select {
	case <-iss.entered:
		t.Fatal("re-obtained during confirm-only retries")
	default:
	}
	if got := iss.obtainCalls(); got != 1 {
		t.Fatalf("obtain calls = %d during confirm-only retries, want 1 (disk reuse)", got)
	}
	attempts.mu.Lock()
	attempts.failConfirm = nil
	attempts.mu.Unlock()
	waitStatus(t, st, StatusActive)
	if got := iss.obtainCalls(); got != 1 {
		t.Fatalf("activation after confirm unblocked re-obtained (%d calls)", got)
	}
}

func TestSetEnvManaged(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := New(Options{Store: st, Proxy: &fakeProxy{}, EnvDomain: "env.example.com",
		Issuer: func(string, string) (Issuer, error) { return nil, errors.New("unused") }})
	if _, err := m.Set("x.dev", "cloudflare", "tok"); !errors.Is(err, ErrEnvManaged) {
		t.Fatalf("Set on env-managed: %v", err)
	}
	if err := m.Remove(); !errors.Is(err, ErrEnvManaged) {
		t.Fatalf("Remove on env-managed: %v", err)
	}
}

func TestStatusDNSOK(t *testing.T) {
	m, st, _, _, _ := newTestManager(t, &fakeIssuer{})
	// A controllable clock so dns_ok's cache (#114) can be expired deterministically.
	clock := time.Now()
	m.now = func() time.Time { return clock }
	// Probe and relay host resolve to the same address → dns_ok.
	m.resolve = func(_ context.Context, host string) ([]net.IP, error) {
		switch host {
		case "piper-probe.example.com", "relay.example.net":
			return []net.IP{net.ParseIP("203.0.113.7")}, nil
		}
		return nil, errors.New("nxdomain")
	}
	if _, err := m.Set("example.com", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, st, StatusActive)

	s, err := m.Status()
	if err != nil {
		t.Fatal(err)
	}
	if !s.DNSOK {
		t.Fatal("want dns_ok=true when probe matches relay")
	}
	if s.DNSTokenSet != true || s.Domain != "example.com" || s.Source != "api" {
		t.Fatalf("status = %+v", s)
	}
	want := []DNSRecord{
		{Type: "CNAME", Name: "*.example.com", Value: "relay.example.net"},
		{Type: "CNAME", Name: "example.com", Value: "relay.example.net"},
	}
	if len(s.DNSRecords) != 2 || s.DNSRecords[0] != want[0] || s.DNSRecords[1] != want[1] {
		t.Fatalf("dns_records = %+v", s.DNSRecords)
	}

	// Probe resolving elsewhere → not ok, once the cache TTL elapses.
	m.resolve = func(_ context.Context, host string) ([]net.IP, error) {
		if host == "piper-probe.example.com" {
			return []net.IP{net.ParseIP("198.51.100.9")}, nil
		}
		return []net.IP{net.ParseIP("203.0.113.7")}, nil
	}
	clock = clock.Add(dnsOKTTL + time.Second)
	s, _ = m.Status()
	if s.DNSOK {
		t.Fatal("want dns_ok=false on mismatch")
	}
}

func TestStatusUnconfigured(t *testing.T) {
	m, _, _, _, _ := newTestManager(t, &fakeIssuer{})
	s, err := m.Status()
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != "" || s.Domain != "" || s.Source != "api" {
		t.Fatalf("unconfigured status = %+v", s)
	}
}

func TestRemoveTearsDown(t *testing.T) {
	m, st, proxy, relay, dataDir := newTestManager(t, &fakeIssuer{})
	if _, err := st.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateDeployment("blog", "img", "ctr", 40001, "running", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Set("example.com", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, st, StatusActive)

	if err := m.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := relay.last(); got != "remove:example.com" {
		t.Fatalf("relay last op = %q, want remove:example.com", got)
	}
	proxy.mu.Lock()
	removed := append([]string(nil), proxy.removed...)
	proxy.mu.Unlock()
	found := false
	for _, h := range removed {
		found = found || h == "blog.example.com"
	}
	if !found {
		t.Fatalf("removed routes = %v, want blog.example.com", removed)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "domain")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cert dir survives Remove: %v", err)
	}
	if _, err := st.GetDomainConfig(); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("config survives Remove: %v", err)
	}
	// Removing again is a no-op.
	if err := m.Remove(); err != nil {
		t.Fatalf("second Remove: %v", err)
	}
}

func TestResumeActiveReloadsWithoutReissuing(t *testing.T) {
	iss := &fakeIssuer{}
	m, st, proxy, _, _ := newTestManager(t, iss)
	if _, err := st.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateDeployment("blog", "img", "ctr", 40001, "running", ""); err != nil {
		t.Fatal(err)
	}
	// Simulate the pre-restart state: active row + valid disk cert.
	certPEM, keyPEM := selfSignedPEM(t, time.Now().Add(60*24*time.Hour), "*.example.com", "example.com")
	if err := m.writeCert(certPEM, keyPEM); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDomainConfig("example.com", "cloudflare", "tok", "relay"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateDomainStatus("example.com", StatusActive, "", time.Now().Add(60*24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	m.Resume()

	if iss.obtainCalls() != 0 {
		t.Fatalf("Resume re-issued (%d obtains), want disk reload", iss.obtainCalls())
	}
	proxy.mu.Lock()
	replaced := proxy.replaced
	proxy.mu.Unlock()
	if replaced == 0 {
		t.Fatal("Resume did not reload the cert into Caddy")
	}
	if p, ok := proxy.route("blog.example.com"); !ok || p != 40001 {
		t.Fatalf("Resume route = %d,%v", p, ok)
	}
}

func TestResumeDamagedCertReissues(t *testing.T) {
	iss := &fakeIssuer{}
	m, st, _, _, _ := newTestManager(t, iss)
	if err := st.SetDomainConfig("example.com", "cloudflare", "tok", "relay"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateDomainStatus("example.com", StatusActive, "", time.Now().Add(60*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := m.writeCert([]byte("garbage"), []byte("garbage")); err != nil {
		t.Fatal(err)
	}

	m.Resume()

	waitStatus(t, st, StatusActive)
	if iss.obtainCalls() == 0 {
		t.Fatal("damaged disk cert must degrade to re-issuance")
	}
}

func TestRenewCheckReissuesNearExpiry(t *testing.T) {
	iss := &fakeIssuer{}
	m, st, proxy, _, _ := newTestManager(t, iss)
	if _, err := m.Set("example.com", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	dcBefore := waitStatus(t, st, StatusActive)
	callsAfterIssue := iss.obtainCalls()
	proxy.mu.Lock()
	replacedAfterIssue := proxy.replaced
	proxy.mu.Unlock()

	// Not due yet: 90-day cert, check "now".
	m.renewCheck(time.Now())
	if iss.obtainCalls() != callsAfterIssue {
		t.Fatal("renewed a cert that is not due")
	}

	// Due: pretend it's 20 days before expiry (inside the 30-day window).
	m.renewCheck(dcBefore.CertNotAfter.Add(-20 * 24 * time.Hour))
	if iss.obtainCalls() != callsAfterIssue+1 {
		t.Fatalf("obtains = %d, want one renewal", iss.obtainCalls())
	}
	proxy.mu.Lock()
	replaced := proxy.replaced
	proxy.mu.Unlock()
	if replaced != replacedAfterIssue+1 {
		t.Fatal("renewal did not swap the cert in Caddy")
	}
	dcAfter, _ := st.GetDomainConfig()
	if !dcAfter.CertNotAfter.After(dcBefore.CertNotAfter) {
		t.Fatal("cert_not_after not advanced by renewal")
	}
}

func TestRenewFailureKeepsServing(t *testing.T) {
	iss := &fakeIssuer{}
	m, st, _, _, _ := newTestManager(t, iss)
	if _, err := m.Set("example.com", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	dc := waitStatus(t, st, StatusActive)

	iss.mu.Lock()
	iss.failures = iss.calls + 100 // all future obtains fail
	iss.mu.Unlock()
	m.renewCheck(dc.CertNotAfter.Add(-20 * 24 * time.Hour))

	dcAfter, _ := st.GetDomainConfig()
	if dcAfter.Status != StatusActive {
		t.Fatalf("status = %q, want active (old cert still serves)", dcAfter.Status)
	}
	if dcAfter.Error == "" {
		t.Fatal("renewal failure not recorded in error")
	}
}

// TestRenewStatusWriteFailureIsLogged closes the store underneath a renewal —
// the issuer factory runs between the run's stale-config re-read and its
// status write — so the "old cert keeps serving" status update fails. The
// sweep must keep going (renewCheck returns; a later sweep still runs) and
// the failed write must be reported on the log, not dropped (#422).
func TestRenewStatusWriteFailureIsLogged(t *testing.T) {
	var logBuf bytes.Buffer
	prevOut := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(prevOut) })

	dataDir := t.TempDir()
	st, err := store.Open(filepath.Join(dataDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := New(Options{
		Store: st, Proxy: &fakeProxy{}, DataDir: dataDir,
		RelayHost: "relay.example.net", HTTPSListen: ":8443",
		Issuer: func(string, string) (Issuer, error) {
			st.Close() // every later store call fails with "database is closed"
			return nil, errors.New("acme: boom")
		},
	})
	t.Cleanup(m.Close)

	notAfter := time.Now().Add(25 * 24 * time.Hour) // inside the renewal window
	if err := st.SetDomainConfig("example.com", "cloudflare", "tok", "relay"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateDomainStatus("example.com", StatusActive, "", notAfter); err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM := selfSignedPEM(t, notAfter, "*.example.com", "example.com")
	if err := m.writeCert(certPEM, keyPEM); err != nil {
		t.Fatal(err)
	}

	// The renewal fails and its status write errors on the closed store; the
	// sweep still returns, and a later sweep runs without panic or deadlock.
	m.renewCheck(time.Now())
	m.renewCheck(time.Now())

	if got := logBuf.String(); !strings.Contains(got, "domain: status write for example.com: sql: database is closed") {
		t.Fatalf("failed status write not reported; log = %q", got)
	}
}

func TestRunEnvIssuesAndRenews(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	proxy := &fakeProxy{}
	iss := &fakeIssuer{}
	m := New(Options{Store: st, Proxy: proxy, EnvDomain: "env.example.com",
		Issuer: func(string, string) (Issuer, error) { return iss, nil }})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.RunEnv(ctx, iss); err != nil {
		t.Fatalf("RunEnv: %v", err)
	}
	if iss.obtainCalls() != 1 {
		t.Fatalf("obtains = %d, want 1 initial issuance", iss.obtainCalls())
	}
	proxy.mu.Lock()
	replaced := proxy.replaced
	proxy.mu.Unlock()
	if replaced != 1 {
		t.Fatal("initial env cert not loaded")
	}
}

// C1(a): a Set-replace racing an in-flight issuance must never leave the relay
// pointing at the replaced domain, and the new domain must actually issue.
func TestSetReplaceDuringInFlightIssuance(t *testing.T) {
	bi := newBlockingIssuer()
	m, st, _, relay, _ := newTestManagerWith(t, bi)
	t.Cleanup(bi.releaseAll) // runs before m.Close (LIFO): unblocks a parked Obtain

	if _, err := m.Set("old.dev", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	<-bi.entered // old.dev Obtain is in flight

	setDone := make(chan error, 1)
	go func() {
		_, err := m.Set("new.dev", "cloudflare", "tok")
		setDone <- err
	}()
	bi.release <- struct{}{} // let the old Obtain finish
	if err := <-setDone; err != nil {
		t.Fatalf("replace Set: %v", err)
	}
	// The new domain must reach its own Obtain (a stale "active" stamp from
	// the old run must not short-circuit it).
	select {
	case <-bi.entered:
		bi.release <- struct{}{}
	case <-time.After(2 * time.Second):
		t.Fatal("new.dev issuance never started; stale run short-circuited it")
	}

	dc := waitStatus(t, st, StatusActive)
	if dc.Domain != "new.dev" {
		t.Fatalf("active domain = %q, want new.dev", dc.Domain)
	}
	if got := relay.last(); got != "confirm:new.dev" {
		t.Fatalf("relay last op = %q, want confirm:new.dev", got)
	}
	// After the replace's teardown removal, the old domain must never be
	// re-claimed or re-confirmed by the stale run.
	pushes := relay.pushes()
	removed := false
	for _, p := range pushes {
		if p == "remove:old.dev" {
			removed = true
		} else if removed && strings.HasSuffix(p, ":old.dev") {
			t.Fatalf("stale run re-pushed old.dev after teardown: ops = %v", pushes)
		}
	}
	if !removed {
		t.Fatalf("replace never removed old.dev from the relay: ops = %v", pushes)
	}
}

// C1(b): a Remove racing an in-flight issuance must win — the relay mapping
// stays cleared and the cert dir stays deleted.
func TestRemoveDuringInFlightIssuance(t *testing.T) {
	bi := newBlockingIssuer()
	m, st, _, relay, dataDir := newTestManagerWith(t, bi)
	t.Cleanup(bi.releaseAll) // runs before m.Close (LIFO): unblocks a parked Obtain

	if _, err := m.Set("gone.dev", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	<-bi.entered // gone.dev Obtain is in flight

	remDone := make(chan error, 1)
	go func() { remDone <- m.Remove() }()
	bi.release <- struct{}{} // let the in-flight Obtain finish
	if err := <-remDone; err != nil {
		t.Fatalf("Remove: %v", err)
	}
	// Give a stale issuance run time to misbehave before asserting.
	time.Sleep(50 * time.Millisecond)
	if _, err := st.GetDomainConfig(); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("config survives Remove: %v", err)
	}
	if got := relay.last(); got != "remove:gone.dev" {
		t.Fatalf("relay last op = %q after Remove, want remove:gone.dev", got)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "domain")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cert dir resurrected after Remove: %v", err)
	}
}

// C1(c): a Remove racing an in-flight renewal must not resurrect the deleted
// cert dir via the renewal's writeCert.
func TestRemoveDuringRenewalKeepsCertDirDeleted(t *testing.T) {
	bi := newBlockingIssuer()
	m, st, _, _, dataDir := newTestManagerWith(t, bi)
	t.Cleanup(bi.releaseAll) // runs before m.Close (LIFO): unblocks a parked Obtain

	if _, err := m.Set("shop.dev", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	<-bi.entered
	bi.release <- struct{}{}
	dc := waitStatus(t, st, StatusActive)

	go m.renewCheck(dc.CertNotAfter.Add(-20 * 24 * time.Hour))
	<-bi.entered // renewal Obtain is in flight

	remDone := make(chan error, 1)
	go func() { remDone <- m.Remove() }()
	// Let Remove run as far as it can while the renewal Obtain is still in
	// flight, then release the Obtain.
	time.Sleep(100 * time.Millisecond)
	bi.release <- struct{}{}
	if err := <-remDone; err != nil {
		t.Fatalf("Remove: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dataDir, "domain")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cert dir resurrected by renewal after Remove: %v", err)
	}
	if _, err := st.GetDomainConfig(); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("config survives Remove: %v", err)
	}
}

// I3: Remove must fail (keeping the config) when the relay clear fails —
// deleting the row first would leave the relay splicing the domain forever.
func TestRemoveFailsWhenRelayClearFails(t *testing.T) {
	m, st, _, relay, _ := newTestManager(t, &fakeIssuer{})
	if _, err := m.Set("shop.dev", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, st, StatusActive)

	relay.mu.Lock()
	relay.fail = errors.New("tunnel down")
	relay.mu.Unlock()
	if err := m.Remove(); err == nil {
		t.Fatal("Remove with relay down: want error, got nil")
	}
	dc, err := st.GetDomainConfig()
	if err != nil || dc.Domain != "shop.dev" {
		t.Fatalf("config must survive a failed Remove: %+v, %v", dc, err)
	}

	relay.mu.Lock()
	relay.fail = nil
	relay.mu.Unlock()
	if err := m.Remove(); err != nil {
		t.Fatalf("retry Remove: %v", err)
	}
	if _, err := st.GetDomainConfig(); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("config survives retried Remove: %v", err)
	}
	if got := relay.last(); got != "remove:shop.dev" {
		t.Fatalf("relay last op = %q, want remove:shop.dev", got)
	}
}

// A same-domain re-PUT (or any generation bump) must supersede the running
// issue loop so a second loop doesn't keep retrying alongside it. #112.
func TestIssueLoopExitsWhenSuperseded(t *testing.T) {
	iss := &fakeIssuer{failures: 1 << 30} // every Obtain fails → loop retries forever
	m, st, _, _, _ := newTestManager(t, iss)
	m.retryDelay = func(int) time.Duration { return time.Millisecond }
	if _, err := m.Set("example.com", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, st, StatusFailed) // the loop is actively retrying

	// Bump the generation as a fresh Set/Resume would, but start no replacement
	// loop: the running loop must observe the bump and stop.
	m.nextGen()

	time.Sleep(60 * time.Millisecond)
	a := iss.obtainCalls()
	time.Sleep(60 * time.Millisecond)
	if b := iss.obtainCalls(); b != a {
		t.Fatalf("superseded loop kept issuing: %d -> %d obtains", a, b)
	}
}

// A relay push that fails with ErrNotConnected (the tunnel isn't up yet) is a
// wait, not an issuance failure: the row must stay "issuing" so a restart
// never reports "failed" solely because the relay wasn't connected. #166.
func TestNotConnectedKeepsIssuing(t *testing.T) {
	m, st, _, relay, _ := newTestManager(t, &fakeIssuer{})
	relay.failAdd = agent.ErrNotConnected
	if _, err := m.Set("example.com", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	dc := waitError(t, st)
	if dc.Status != StatusIssuing {
		t.Fatalf("status = %q, want %q for a not-connected relay (error %q)", dc.Status, StatusIssuing, dc.Error)
	}
	if !strings.Contains(dc.Error, "waiting for relay connection") {
		t.Fatalf("error = %q, want it to name the relay wait", dc.Error)
	}
}

// The not-connected carve-out is errors.Is-based, not string-based: any other
// error — even one carrying ErrNotConnected's text — still records "failed"
// with its message intact.
func TestGenericRelayErrorStillFails(t *testing.T) {
	m, st, _, relay, _ := newTestManager(t, &fakeIssuer{})
	relay.failAdd = errors.New("relay tunnel not connected") // same text, not the sentinel
	if _, err := m.Set("example.com", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	dc := waitError(t, st)
	if dc.Status != StatusFailed {
		t.Fatalf("status = %q, want %q (error %q)", dc.Status, StatusFailed, dc.Error)
	}
	if dc.Error != "relay tunnel not connected" {
		t.Fatalf("error = %q, want the original message preserved", dc.Error)
	}
}

// A tunnel (re)connect re-kicks a waiting config immediately: the connect
// event, not the retry timer, drives the retry. The hour-long retryDelay seam
// proves only a fresh loop from the kick could activate inside the poll
// window — and the generation bump supersedes the sleeping one. #166.
func TestOnRelayConnectKicksIssuance(t *testing.T) {
	iss := &fakeIssuer{}
	m, st, _, relay, _ := newTestManager(t, iss)
	m.retryDelay = func(int) time.Duration { return time.Hour } // the sleeping loop's timer can never fire in-test
	relay.failAdd = agent.ErrNotConnected
	if _, err := m.Set("example.com", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	dc := waitError(t, st) // first attempt hit the not-connected wait and is now asleep
	if dc.Status != StatusIssuing {
		t.Fatalf("pre-kick status = %q, want %q", dc.Status, StatusIssuing)
	}
	gen := m.currentGen()

	relay.mu.Lock()
	relay.failAdd = nil
	relay.mu.Unlock()
	m.OnRelayConnect()

	waitStatus(t, st, StatusActive)
	if got := m.currentGen(); got != gen+1 {
		t.Fatalf("gen = %d, want %d: the kick must supersede the sleeping loop", got, gen+1)
	}
	if got := iss.obtainCalls(); got != 1 {
		t.Fatalf("obtains = %d, want exactly 1 (the kicked loop's)", got)
	}
}

// An active config is arm-only on reconnect — the relay re-derives the mapping
// at session registration — so the kick must not start a new issuance run.
func TestOnRelayConnectSkipsActive(t *testing.T) {
	iss := &fakeIssuer{}
	m, st, _, relay, _ := newTestManager(t, iss)
	if _, err := m.Set("example.com", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, st, StatusActive)
	obtains, pushes, gen := iss.obtainCalls(), len(relay.pushes()), m.currentGen()

	m.OnRelayConnect()

	// Give a misbehaving kick time to run before asserting nothing happened.
	time.Sleep(60 * time.Millisecond)
	if got := iss.obtainCalls(); got != obtains {
		t.Fatalf("obtains grew %d -> %d: active config re-issued on connect", obtains, got)
	}
	if got := len(relay.pushes()); got != pushes {
		t.Fatalf("relay pushes grew %d -> %d: active config re-pushed on connect", pushes, got)
	}
	if got := m.currentGen(); got != gen {
		t.Fatalf("gen bumped %d -> %d: kick started a loop for an active config", gen, got)
	}
}

// A reconnect kick racing a Set-replace must not strand the replacement
// domain: the kick reads the config and bumps the generation without
// coordinating with Set, so if it reads old domain A, a concurrent Set can
// store B and start B's generation, and the kick's bump can then land on top —
// B's loop exits as superseded, the kick's loop exits on the domain mismatch,
// and B is left in "issuing" until the next reconnect. The genBumpHook seam
// parks the kick between its (stale) read and its bump so the replace lands
// deterministically in between; the assertion is the observable end state —
// once the tunnel comes up, a live current-generation loop must drive B to
// active, which no surviving loop can do after the buggy interleaving.
func TestOnRelayConnectDoesNotStrandReplacement(t *testing.T) {
	iss := &fakeIssuer{}
	m, st, _, relay, _ := newTestManager(t, iss)
	// Every relay claim waits on the tunnel, so no issuance can complete on
	// its own; loops park in the not-connected wait and retry.
	relay.failAdd = agent.ErrNotConnected
	if err := st.SetDomainConfig("a.dev", "cloudflare", "tok", "relay"); err != nil {
		t.Fatal(err)
	}

	// Park the first generation bump after arming (the kick's) until the test
	// releases it; later bumps (the replace's) pass straight through.
	entered := make(chan struct{})
	release := make(chan struct{})
	var hooked int32
	m.genBumpHook = func() {
		if atomic.CompareAndSwapInt32(&hooked, 0, 1) {
			close(entered)
			<-release
		}
	}

	go m.OnRelayConnect()
	<-entered // the kick has read a.dev and is parked just before its bump

	var setErr error
	setDone := make(chan struct{})
	go func() {
		_, setErr = m.Set("b.dev", "cloudflare", "tok")
		close(setDone)
	}()
	// Without the fix the replace completes while the kick is parked; with it
	// Set queues behind the kick's issueMu critical section until release.
	select {
	case <-setDone:
	case <-time.After(2 * time.Second):
	}
	close(release)
	<-setDone
	if setErr != nil {
		t.Fatalf("replace Set: %v", setErr)
	}

	// The tunnel comes up: a live loop for b.dev must now drive it to active.
	// The buggy interleaving superseded B's loop and the kick's loop exited on
	// the domain mismatch, so nothing ever retries B — it strands in issuing.
	relay.mu.Lock()
	relay.failAdd = nil
	relay.mu.Unlock()
	dc := waitStatus(t, st, StatusActive)
	if dc.Domain != "b.dev" {
		t.Fatalf("active domain = %q, want b.dev", dc.Domain)
	}
}

// A boot-time Resume racing a Set-replace must not strand the replacement
// domain: like the reconnect kick, Resume reads the config and bumps the
// generation without coordinating with Set, so if it reads old domain A, a
// concurrent Set can store B and start B's generation, and Resume's bump can
// then land on top — B's loop exits as superseded, Resume's loop exits on the
// domain mismatch, and B is left in "issuing" until something re-kicks it. The
// genBumpHook seam parks Resume between its (stale) read and its bump so the
// replace lands deterministically in between; the assertion is the observable
// end state — once the tunnel comes up, a live current-generation loop must
// drive B to active, which no surviving loop can do after the buggy
// interleaving. #275.
func TestResumeDoesNotStrandReplacement(t *testing.T) {
	iss := &fakeIssuer{}
	m, st, _, relay, _ := newTestManager(t, iss)
	// Every relay claim waits on the tunnel, so no issuance can complete on
	// its own; loops park in the not-connected wait and retry.
	relay.failAdd = agent.ErrNotConnected
	if err := st.SetDomainConfig("a.dev", "cloudflare", "tok", "relay"); err != nil {
		t.Fatal(err)
	}

	// Park the first generation bump after arming (Resume's) until the test
	// releases it; later bumps (the replace's) pass straight through.
	entered := make(chan struct{})
	release := make(chan struct{})
	var hooked int32
	m.genBumpHook = func() {
		if atomic.CompareAndSwapInt32(&hooked, 0, 1) {
			close(entered)
			<-release
		}
	}

	go m.Resume()
	<-entered // Resume has read a.dev and is parked just before its bump

	var setErr error
	setDone := make(chan struct{})
	go func() {
		_, setErr = m.Set("b.dev", "cloudflare", "tok")
		close(setDone)
	}()
	// Without the fix the replace completes while Resume is parked; with it
	// Set queues behind Resume's issueMu critical section until release.
	select {
	case <-setDone:
	case <-time.After(2 * time.Second):
	}
	close(release)
	<-setDone
	if setErr != nil {
		t.Fatalf("replace Set: %v", setErr)
	}

	// The tunnel comes up: a live loop for b.dev must now drive it to active.
	// The buggy interleaving superseded B's loop and Resume's loop exited on
	// the domain mismatch, so nothing ever retries B — it strands in issuing.
	relay.mu.Lock()
	relay.failAdd = nil
	relay.mu.Unlock()
	dc := waitStatus(t, st, StatusActive)
	if dc.Domain != "b.dev" {
		t.Fatalf("active domain = %q, want b.dev", dc.Domain)
	}
}

// Repeated Status polls within the TTL must not re-run the live DNS lookups;
// after the TTL they must. #114.
func TestStatusDNSOKCachedWithinTTL(t *testing.T) {
	m, st, _, _, _ := newTestManager(t, &fakeIssuer{})
	clock := time.Now()
	m.now = func() time.Time { return clock }
	var resolves int64
	m.resolve = func(_ context.Context, _ string) ([]net.IP, error) {
		atomic.AddInt64(&resolves, 1)
		return []net.IP{net.ParseIP("203.0.113.7")}, nil // probe == relay → ok
	}
	if _, err := m.Set("example.com", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, st, StatusActive)

	if _, err := m.Status(); err != nil {
		t.Fatal(err)
	}
	base := atomic.LoadInt64(&resolves)
	for i := 0; i < 5; i++ {
		if _, err := m.Status(); err != nil {
			t.Fatal(err)
		}
	}
	if got := atomic.LoadInt64(&resolves); got != base {
		t.Fatalf("cached dns_ok still hit the resolver: %d extra lookups", got-base)
	}

	clock = clock.Add(dnsOKTTL + time.Second)
	if _, err := m.Status(); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&resolves); got <= base {
		t.Fatal("cache never expired: no re-lookup after TTL")
	}
}

// Env-managed Status reflects the real issuance outcome (pending → active, or
// failed) rather than a constant "active". #116.
func TestRunEnvStatusReflectsState(t *testing.T) {
	newEnvManager := func() (*Manager, *store.Store) {
		st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		m := New(Options{Store: st, Proxy: &fakeProxy{}, EnvDomain: "env.example.com",
			Issuer: func(string, string) (Issuer, error) { return nil, errors.New("unused") }})
		return m, st
	}

	// Before RunEnv completes: pending "issuing", no cert expiry.
	m, _ := newEnvManager()
	s, err := m.Status()
	if err != nil {
		t.Fatal(err)
	}
	if s.Source != "env" || s.Status != StatusIssuing || s.CertNotAfter != nil {
		t.Fatalf("pre-RunEnv status = %+v, want env/issuing/no-cert", s)
	}

	// Successful RunEnv → active with the cert's expiry surfaced.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.RunEnv(ctx, &fakeIssuer{notAfter: time.Now().Add(90 * 24 * time.Hour)}); err != nil {
		t.Fatalf("RunEnv: %v", err)
	}
	if s, _ = m.Status(); s.Status != StatusActive || s.CertNotAfter == nil {
		t.Fatalf("post-RunEnv status = %+v, want active + cert_not_after", s)
	}

	// A failing RunEnv → failed with the error surfaced.
	m2, _ := newEnvManager()
	if err := m2.RunEnv(ctx, &fakeIssuer{failures: 1}); err == nil {
		t.Fatal("want RunEnv error from a failing issuer")
	}
	if s, _ = m2.Status(); s.Status != StatusFailed || s.Error == "" {
		t.Fatalf("failed RunEnv status = %+v, want failed + error", s)
	}
}

func TestDefaultRetryDelayShortFirstRetry(t *testing.T) {
	cases := map[int]time.Duration{
		0:   5 * time.Second, // short first retry so a resume flap clears fast (#113)
		1:   time.Minute,
		2:   2 * time.Minute,
		100: 64 * time.Minute, // capped
	}
	for attempt, want := range cases {
		if got := defaultRetryDelay(attempt); got != want {
			t.Errorf("defaultRetryDelay(%d) = %v, want %v", attempt, got, want)
		}
	}
}
