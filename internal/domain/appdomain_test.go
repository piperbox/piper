package domain

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/piperbox/piper/internal/store"
)

type fakeRouter struct {
	mu     sync.Mutex
	routed []string // "app:domain"
	fail   error
}

func (f *fakeRouter) RouteAppDomain(app, domain string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.routed = append(f.routed, app+":"+domain)
	return nil
}
func (f *fakeRouter) routes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.routed...)
}

// newAppManagerOn builds a Manager over an existing store/dataDir — the
// restart-resume tests construct a second Manager on the first one's state.
func newAppManagerOn(t *testing.T, st *store.Store, dataDir string, iss Issuer) (*Manager, *fakeProxy, *fakeNotifier, *fakeRouter) {
	t.Helper()
	proxy := &fakeProxy{}
	relay := &fakeNotifier{}
	router := &fakeRouter{}
	m := New(Options{
		Store:     st,
		Issuer:    func(provider, token string) (Issuer, error) { return iss, nil },
		AppIssuer: func() (Issuer, error) { return iss, nil },
		Proxy:     proxy, Router: router, DataDir: dataDir,
		RelayHost: "relay.example.net", HTTPSListen: ":8443",
	})
	// Join the lifecycle goroutines before the temp dir's RemoveAll: cleanups
	// run LIFO and t.TempDir registered its own cleanup before this helper was
	// called, so this Close runs first (#279).
	t.Cleanup(m.Close)
	m.SetRelay(relay)
	m.retryDelay = func(int) time.Duration { return time.Millisecond }
	m.dnsWait = time.Millisecond
	// By default every name resolves to the relay's address: DNS points.
	m.resolve = func(_ context.Context, _ string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("203.0.113.7")}, nil
	}
	return m, proxy, relay, router
}

func newAppTestManager(t *testing.T, iss Issuer) (*Manager, *store.Store, *fakeProxy, *fakeNotifier, *fakeRouter, string) {
	t.Helper()
	dataDir := t.TempDir()
	st, err := store.Open(filepath.Join(dataDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m, proxy, relay, router := newAppManagerOn(t, st, dataDir, iss)
	return m, st, proxy, relay, router, dataDir
}

// waitAppStatus polls the store until domain reaches status (waitCeiling cap).
func waitAppStatus(t *testing.T, st *store.Store, domain, status string) store.AppDomain {
	t.Helper()
	deadline := time.Now().Add(waitCeiling)
	var row store.AppDomain
	var err error
	for time.Now().Before(deadline) {
		row, err = st.GetAppDomain(domain)
		if err == nil && row.Status == status {
			return row
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s never reached %q (last: %+v, err %v)", domain, status, row, err)
	return row
}

// orderCheckIssuer records, per Obtain, whether the relay had already seen the
// domain's add-domain claim — the ALPN-inversion ordering assertion.
type orderCheckIssuer struct {
	fakeIssuer
	relay *fakeNotifier

	omu        sync.Mutex
	addBefore  []bool
	gotDomains [][]string
}

func (o *orderCheckIssuer) Obtain(domains []string) ([]byte, []byte, error) {
	seen := false
	for _, p := range o.relay.pushes() {
		if p == "add:"+domains[0] {
			seen = true
		}
	}
	o.omu.Lock()
	o.addBefore = append(o.addBefore, seen)
	o.gotDomains = append(o.gotDomains, append([]string(nil), domains...))
	o.omu.Unlock()
	return o.fakeIssuer.Obtain(domains)
}

func TestAddAppDomainIssuesAndActivates(t *testing.T) {
	iss := &orderCheckIssuer{}
	m, st, proxy, relay, router, dataDir := newAppTestManager(t, iss)
	iss.relay = relay
	if _, err := st.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}

	row, err := m.AddAppDomain("blog", "Shop.EXAMPLE.com")
	if err != nil {
		t.Fatalf("AddAppDomain: %v", err)
	}
	if row.Domain != "shop.example.com" || row.App != "blog" {
		t.Fatalf("AddAppDomain returned %+v", row)
	}

	got := waitAppStatus(t, st, "shop.example.com", StatusActive)
	if got.CertNotAfter.IsZero() || got.Error != "" {
		t.Fatalf("active row = %+v", got)
	}

	// Relay claim before issuance, confirm after.
	iss.omu.Lock()
	addBefore, gotDomains := iss.addBefore, iss.gotDomains
	iss.omu.Unlock()
	if len(addBefore) == 0 || !addBefore[0] {
		t.Fatalf("Obtain ran before the relay add-domain claim (addBefore=%v)", addBefore)
	}
	if len(gotDomains[0]) != 1 || gotDomains[0][0] != "shop.example.com" {
		t.Fatalf("Obtain domains = %v, want exact host only", gotDomains[0])
	}
	pushes := relay.pushes()
	if pushes[len(pushes)-1] != "confirm:shop.example.com" {
		t.Fatalf("relay ops = %v, want trailing confirm", pushes)
	}

	// Local activation: HTTPS armed, cert synced, backfill route armed.
	proxy.mu.Lock()
	ensured := append([]string(nil), proxy.ensured...)
	certs := len(proxy.certs)
	proxy.mu.Unlock()
	if len(ensured) == 0 || ensured[0] != ":8443" || certs != 1 {
		t.Fatalf("proxy: ensured=%v certs=%d", ensured, certs)
	}
	if r := router.routes(); len(r) != 1 || r[0] != "blog:shop.example.com" {
		t.Fatalf("router backfill = %v", r)
	}
	for _, f := range []string{"cert.pem", "key.pem"} {
		fi, err := os.Stat(filepath.Join(dataDir, "appdomains", "shop.example.com", f))
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s perms = %v, want 0600", f, fi.Mode().Perm())
		}
	}
}

func TestAddAppDomainValidation(t *testing.T) {
	m, st, _, _, _, _ := newAppTestManager(t, &fakeIssuer{})
	if _, err := st.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddAppDomain("blog", "not a domain"); !errors.Is(err, ErrInvalidDomain) {
		t.Fatalf("bad domain: %v", err)
	}
	if _, err := m.AddAppDomain("ghost", "ok.example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown app: %v", err)
	}
	if _, err := m.AddAppDomain("blog", "shop.example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddAppDomain("blog", "shop.example.com"); !errors.Is(err, store.ErrDomainExists) {
		t.Fatalf("duplicate: %v", err)
	}
	// The box-wide domain must not double as a per-app domain: the relay
	// no-ops an add for the agent's own active row, so only this local check
	// stops two lifecycles from managing the same name.
	if err := st.SetDomainConfig("wild.example.com", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddAppDomain("blog", "wild.example.com"); !errors.Is(err, ErrBoxWideDomain) {
		t.Fatalf("box-wide collision: %v", err)
	}
}

// While the user's DNS does not point at the relay the domain stays pending —
// and no ACME order is burned. The claim is re-added on each poll so the
// relay's pending TTL keeps being refreshed.
func TestAppDomainWaitsForDNS(t *testing.T) {
	iss := newBlockingIssuer()
	m, st, _, relay, _, _ := newAppTestManager(t, iss)
	t.Cleanup(iss.releaseAll) // runs before m.Close (LIFO): unblocks a parked Obtain
	if _, err := st.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	var pointed atomicBool
	m.resolve = func(_ context.Context, host string) ([]net.IP, error) {
		if host == "shop.example.com" && !pointed.get() {
			return []net.IP{net.ParseIP("198.51.100.9")}, nil // elsewhere
		}
		return []net.IP{net.ParseIP("203.0.113.7")}, nil
	}
	if _, err := m.AddAppDomain("blog", "shop.example.com"); err != nil {
		t.Fatal(err)
	}

	// The claim is re-added on each DNS poll and the loop is sequential, so
	// the second recorded add proves one whole poll cycle completed with DNS
	// unset: the pending status and its DNS hint are already persisted, and
	// no cycle could have reached Obtain — the blocking issuer would park and
	// signal entered instead of passing silently because the work had not
	// started yet.
	waitFor(t, "pending claim re-added while DNS unset", func() bool {
		adds := 0
		for _, p := range relay.pushes() {
			if p == "add:shop.example.com" {
				adds++
			}
		}
		return adds >= 2
	})
	row, err := st.GetAppDomain("shop.example.com")
	if err != nil || row.Status != StatusPending {
		t.Fatalf("row while DNS unset = %+v (err %v), want pending", row, err)
	}
	if row.Error == "" {
		t.Fatal("pending row carries no DNS hint")
	}
	select {
	case <-iss.entered:
		t.Fatal("obtained a cert while DNS unset")
	default:
	}

	pointed.set(true)
	iss.release <- struct{}{} // the one Obtain behind the DNS gate
	waitAppStatus(t, st, "shop.example.com", StatusActive)
}

type atomicBool struct {
	mu sync.Mutex
	v  bool
}

func (b *atomicBool) get() bool  { b.mu.Lock(); defer b.mu.Unlock(); return b.v }
func (b *atomicBool) set(v bool) { b.mu.Lock(); b.v = v; b.mu.Unlock() }

// failFor fails Obtain for one domain forever; others succeed.
type failForIssuer struct {
	fakeIssuer
	bad string
}

func (f *failForIssuer) Obtain(domains []string) ([]byte, []byte, error) {
	if domains[0] == f.bad {
		return nil, nil, errors.New("acme: validation failed")
	}
	return f.fakeIssuer.Obtain(domains)
}

// N domains across M apps run independently: one domain failing forever never
// blocks the others from activating, and its row records the failure.
func TestAppDomainFailureDoesNotBlockOthers(t *testing.T) {
	iss := &failForIssuer{bad: "bad.example.com"}
	m, st, proxy, _, router, _ := newAppTestManager(t, iss)
	for _, a := range []string{"blog", "api"} {
		if _, err := st.CreateApp(a, 8080); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []struct{ app, dom string }{
		{"blog", "bad.example.com"},
		{"blog", "one.example.com"},
		{"api", "two.example.com"},
	} {
		if _, err := m.AddAppDomain(d.app, d.dom); err != nil {
			t.Fatal(err)
		}
	}

	waitAppStatus(t, st, "one.example.com", StatusActive)
	waitAppStatus(t, st, "two.example.com", StatusActive)
	bad := waitAppStatus(t, st, "bad.example.com", StatusFailed)
	if !strings.Contains(bad.Error, "acme: validation failed") {
		t.Fatalf("failed row error = %q", bad.Error)
	}
	if got := proxy.certCount(); got != 2 {
		t.Fatalf("loaded certs = %d, want the 2 healthy domains", got)
	}
	got := router.routes()
	want := map[string]bool{"blog:one.example.com": true, "api:two.example.com": true}
	for _, r := range got {
		delete(want, r)
	}
	if len(want) != 0 {
		t.Fatalf("router backfills = %v, missing %v", got, want)
	}
}

func TestRemoveAppDomainTearsDown(t *testing.T) {
	m, st, proxy, relay, _, dataDir := newAppTestManager(t, &fakeIssuer{})
	if _, err := st.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"one.example.com", "two.example.com"} {
		if _, err := m.AddAppDomain("blog", d); err != nil {
			t.Fatal(err)
		}
		waitAppStatus(t, st, d, StatusActive)
	}

	if err := m.RemoveAppDomain("one.example.com"); err != nil {
		t.Fatalf("RemoveAppDomain: %v", err)
	}
	if got := relay.last(); got != "remove:one.example.com" {
		t.Fatalf("relay last op = %q", got)
	}
	proxy.mu.Lock()
	removed := append([]string(nil), proxy.removed...)
	certs := len(proxy.certs)
	proxy.mu.Unlock()
	found := false
	for _, h := range removed {
		found = found || h == "one.example.com"
	}
	if !found {
		t.Fatalf("removed routes = %v, want one.example.com", removed)
	}
	// The surviving domain's cert stays loaded; the removed one is gone.
	if certs != 1 {
		t.Fatalf("loaded certs after remove = %d, want 1", certs)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "appdomains", "one.example.com")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cert dir survives remove: %v", err)
	}
	if _, err := st.GetAppDomain("one.example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("row survives remove: %v", err)
	}
	if _, err := st.GetAppDomain("two.example.com"); err != nil {
		t.Fatalf("sibling row damaged by remove: %v", err)
	}
	// Removing an absent domain is a no-op.
	if err := m.RemoveAppDomain("one.example.com"); err != nil {
		t.Fatalf("second remove: %v", err)
	}
}

// An active domain's relay removal must succeed before local teardown — the
// row survives a failed removal so the user can retry. A never-confirmed
// (pending) claim expires on the relay by TTL, so its removal is best-effort.
func TestRemoveAppDomainRelayFailure(t *testing.T) {
	m, st, _, relay, _, _ := newAppTestManager(t, &fakeIssuer{})
	if _, err := st.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddAppDomain("blog", "shop.example.com"); err != nil {
		t.Fatal(err)
	}
	waitAppStatus(t, st, "shop.example.com", StatusActive)

	relay.mu.Lock()
	relay.failRem = errors.New("tunnel down")
	relay.mu.Unlock()
	if err := m.RemoveAppDomain("shop.example.com"); err == nil {
		t.Fatal("remove of active domain with relay down: want error")
	}
	if _, err := st.GetAppDomain("shop.example.com"); err != nil {
		t.Fatalf("row must survive failed remove: %v", err)
	}
	relay.mu.Lock()
	relay.failRem = nil
	relay.mu.Unlock()
	if err := m.RemoveAppDomain("shop.example.com"); err != nil {
		t.Fatalf("retried remove: %v", err)
	}

	// Pending claim: removal proceeds even when the relay rejects it.
	if err := st.AddAppDomain("pend.example.com", "blog"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateAppDomainStatus("pend.example.com", StatusPending, "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	relay.mu.Lock()
	relay.failRem = errors.New("tunnel down")
	relay.mu.Unlock()
	if err := m.RemoveAppDomain("pend.example.com"); err != nil {
		t.Fatalf("remove of pending domain with relay down: %v", err)
	}
	if _, err := st.GetAppDomain("pend.example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("pending row survives remove: %v", err)
	}
}

// Restart with an active row and a valid disk cert: re-arm locally without
// re-issuing, and re-assert the relay claim in the background.
func TestResumeAppDomainActiveReloadsWithoutReissuing(t *testing.T) {
	iss := &fakeIssuer{}
	m1, st, _, _, _, dataDir := newAppTestManager(t, iss)
	if _, err := st.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := m1.AddAppDomain("blog", "shop.example.com"); err != nil {
		t.Fatal(err)
	}
	waitAppStatus(t, st, "shop.example.com", StatusActive)
	obtains := iss.obtainCalls()

	// "Restart": a fresh manager over the same store and disk.
	m2, proxy2, relay2, router2 := newAppManagerOn(t, st, dataDir, iss)
	m2.ResumeAppDomains()

	if got := iss.obtainCalls(); got != obtains {
		t.Fatalf("resume re-issued (%d→%d obtains), want disk reload", obtains, got)
	}
	if got := proxy2.certCount(); got != 1 {
		t.Fatalf("resume loaded %d certs, want 1", got)
	}
	if r := router2.routes(); len(r) != 1 || r[0] != "blog:shop.example.com" {
		t.Fatalf("resume backfill = %v", r)
	}
	// The relay claim is re-asserted (add + confirm) in the background.
	deadline := time.Now().Add(waitCeiling)
	for {
		p := relay2.pushes()
		if len(p) >= 2 && p[len(p)-1] == "confirm:shop.example.com" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("relay claim never re-asserted: ops = %v", p)
		}
		time.Sleep(5 * time.Millisecond)
	}
	row, _ := st.GetAppDomain("shop.example.com")
	if row.Status != StatusActive {
		t.Fatalf("resume flapped status to %q", row.Status)
	}
}

// gatedAddNotifier holds the FIRST AddCustomDomain call open until the test
// releases it, exposing the window between reassertLoop's checks and its push.
type gatedAddNotifier struct {
	fakeNotifier
	entered chan struct{} // one value when the gated Add enters
	release chan struct{}
	once    sync.Once
}

func (g *gatedAddNotifier) AddCustomDomain(d string) error {
	gated := false
	g.once.Do(func() { gated = true })
	if gated {
		g.entered <- struct{}{}
		<-g.release
	}
	return g.fakeNotifier.AddCustomDomain(d)
}

// A RemoveAppDomain racing a resumed domain's relay re-assert must win: the
// re-assert push must never land after the removal, or it would mint a fresh
// durable relay claim for a domain the box no longer tracks — unremovable
// (row gone, resume never sees it) and eating the per-agent quota forever.
func TestReassertLoopCannotResurrectRemovedDomain(t *testing.T) {
	iss := &fakeIssuer{}
	m1, st, _, _, _, dataDir := newAppTestManager(t, iss)
	if _, err := st.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := m1.AddAppDomain("blog", "shop.example.com"); err != nil {
		t.Fatal(err)
	}
	waitAppStatus(t, st, "shop.example.com", StatusActive)

	// "Restart" with a relay whose first add-domain hangs mid-push.
	m2, _, _, _ := newAppManagerOn(t, st, dataDir, iss)
	gated := &gatedAddNotifier{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}, 1),
	}
	m2.SetRelay(gated)
	m2.ResumeAppDomains()
	<-gated.entered // the re-assert is inside its add-domain push
	genBefore := m2.currentGenFor("shop.example.com")

	remDone := make(chan error, 1)
	go func() { remDone <- m2.RemoveAppDomain("shop.example.com") }()
	// RemoveAppDomain's first step is the generation bump; once it lands,
	// Remove is parked on the domain's run lock — held by the gated push — so
	// the interleaving under test (a removal racing an in-flight re-assert)
	// is in place by construction, not by giving the goroutine time to run.
	waitFor(t, "RemoveAppDomain superseded the re-assert generation", func() bool {
		return m2.currentGenFor("shop.example.com") != genBefore
	})
	gated.release <- struct{}{}
	if err := <-remDone; err != nil {
		t.Fatalf("RemoveAppDomain: %v", err)
	}
	// Join the lifecycle goroutines: after Close returns no stale re-assert
	// can still be in flight, so a resurrection push after the removal would
	// already be in the recorded ops — not still on its way.
	m2.Close()

	ops := gated.pushes()
	removed := false
	for _, p := range ops {
		if p == "remove:shop.example.com" {
			removed = true
		} else if removed && strings.HasSuffix(p, ":shop.example.com") {
			t.Fatalf("re-assert resurrected the removed claim: ops = %v", ops)
		}
	}
	if !removed {
		t.Fatalf("removal never reached the relay: ops = %v", ops)
	}
	if _, err := st.GetAppDomain("shop.example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("row survives remove: %v", err)
	}
}

// Restart with pending/failed rows resumes their lifecycle; an active row with
// a damaged disk cert degrades to re-issuance.
func TestResumeAppDomainsResumesUnfinishedAndDamaged(t *testing.T) {
	iss := &fakeIssuer{}
	dataDir := t.TempDir()
	st, err := store.Open(filepath.Join(dataDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	seed := []struct{ dom, status string }{
		{"pend.example.com", StatusPending},
		{"fail.example.com", StatusFailed},
		{"damaged.example.com", StatusActive}, // no disk cert behind it
	}
	for _, s := range seed {
		if err := st.AddAppDomain(s.dom, "blog"); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateAppDomainStatus(s.dom, s.status, "", time.Time{}); err != nil {
			t.Fatal(err)
		}
	}

	m, _, _, _ := newAppManagerOn(t, st, dataDir, iss)
	m.ResumeAppDomains()

	for _, s := range seed {
		waitAppStatus(t, st, s.dom, StatusActive)
	}
	if iss.obtainCalls() != 3 {
		t.Fatalf("obtains = %d, want one per resumed domain", iss.obtainCalls())
	}
}

func TestRenewAppDomains(t *testing.T) {
	iss := &fakeIssuer{}
	m, st, proxy, _, _, _ := newAppTestManager(t, iss)
	if _, err := st.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"one.example.com", "two.example.com"} {
		if _, err := m.AddAppDomain("blog", d); err != nil {
			t.Fatal(err)
		}
		waitAppStatus(t, st, d, StatusActive)
	}
	one, _ := st.GetAppDomain("one.example.com")
	calls := iss.obtainCalls()
	proxy.mu.Lock()
	replaced := proxy.replaced
	proxy.mu.Unlock()

	// Not due: nothing renews.
	m.renewCheckApps(time.Now())
	if iss.obtainCalls() != calls {
		t.Fatal("renewed certs that are not due")
	}

	// Inside the window: both renew, certs re-sync, expiries advance.
	m.renewCheckApps(one.CertNotAfter.Add(-20 * 24 * time.Hour))
	if got := iss.obtainCalls(); got != calls+2 {
		t.Fatalf("obtains = %d, want %d (both renewed)", got, calls+2)
	}
	proxy.mu.Lock()
	nowReplaced, certCount := proxy.replaced, len(proxy.certs)
	proxy.mu.Unlock()
	if nowReplaced != replaced+2 || certCount != 2 {
		t.Fatalf("cert sync after renewal: replaced %d→%d, set %d", replaced, nowReplaced, certCount)
	}
	oneAfter, _ := st.GetAppDomain("one.example.com")
	if !oneAfter.CertNotAfter.After(one.CertNotAfter) {
		t.Fatal("cert_not_after not advanced by renewal")
	}
}

// A renewal failure keeps the domain active on its old cert, records the
// error, and does not stop the other domains' renewals.
func TestRenewAppDomainFailureKeepsServingAndOthersRenew(t *testing.T) {
	iss := &failForIssuer{}
	m, st, _, _, _, _ := newAppTestManager(t, iss)
	if _, err := st.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"bad.example.com", "good.example.com"} {
		if _, err := m.AddAppDomain("blog", d); err != nil {
			t.Fatal(err)
		}
		waitAppStatus(t, st, d, StatusActive)
	}
	good, _ := st.GetAppDomain("good.example.com")
	iss.bad = "bad.example.com" // fail only bad.example.com's renewals

	m.renewCheckApps(good.CertNotAfter.Add(-20 * 24 * time.Hour))

	bad, _ := st.GetAppDomain("bad.example.com")
	if bad.Status != StatusActive || !strings.HasPrefix(bad.Error, "renew: ") {
		t.Fatalf("failed renewal row = %+v, want active + renew error", bad)
	}
	goodAfter, _ := st.GetAppDomain("good.example.com")
	if !goodAfter.CertNotAfter.After(good.CertNotAfter) {
		t.Fatal("healthy domain's renewal was blocked by the failing one")
	}
}

// The box-wide wildcard cert and per-app certs share Caddy's load_pem set:
// activating and renewing one side must never drop the other's entry.
func TestBoxWideAndAppCertsCoexist(t *testing.T) {
	iss := &fakeIssuer{}
	m, st, proxy, _, _, _ := newAppTestManager(t, iss)
	if _, err := st.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Set("wild.example.com", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, st, StatusActive)
	if _, err := m.AddAppDomain("blog", "shop.other.net"); err != nil {
		t.Fatal(err)
	}
	waitAppStatus(t, st, "shop.other.net", StatusActive)
	if got := proxy.certCount(); got != 2 {
		t.Fatalf("loaded certs = %d, want wildcard + exact-host", got)
	}

	// Box-wide renewal must re-sync both, not clobber the app cert.
	dc, _ := st.GetDomainConfig()
	m.renewCheck(dc.CertNotAfter.Add(-20 * 24 * time.Hour))
	if got := proxy.certCount(); got != 2 {
		t.Fatalf("loaded certs after box-wide renewal = %d, want 2", got)
	}

	// App renewal likewise keeps the wildcard loaded.
	ad, _ := st.GetAppDomain("shop.other.net")
	m.renewCheckApps(ad.CertNotAfter.Add(-20 * 24 * time.Hour))
	if got := proxy.certCount(); got != 2 {
		t.Fatalf("loaded certs after app renewal = %d, want 2", got)
	}
}

// AppDomainStatuses assembles the wire state (#231) straight from the store
// rows: per-domain status/error/expiry plus the guided-setup CNAME record and
// the exact-host dns_ok check against the relay.
func TestAppDomainStatuses(t *testing.T) {
	m, st, _, _, _, _ := newAppTestManager(t, &fakeIssuer{})
	for _, a := range []string{"blog", "api"} {
		if _, err := st.CreateApp(a, 8080); err != nil {
			t.Fatal(err)
		}
	}

	// No domains yet: empty, not nil (the handler serves it as JSON []).
	sts, err := m.AppDomainStatuses("blog")
	if err != nil || sts == nil || len(sts) != 0 {
		t.Fatalf("empty statuses = %v (err %v), want []", sts, err)
	}

	// Rows written directly: status assembly is read-only over the store.
	notAfter := time.Now().Add(60 * 24 * time.Hour).UTC().Truncate(time.Second)
	for _, d := range []struct{ dom, app string }{
		{"a.example.com", "blog"}, {"b.example.com", "blog"}, {"other.example.com", "api"},
	} {
		if err := st.AddAppDomain(d.dom, d.app); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.UpdateAppDomainStatus("a.example.com", StatusActive, "", notAfter); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateAppDomainStatus("b.example.com", StatusFailed, "acme: boom", time.Time{}); err != nil {
		t.Fatal(err)
	}

	sts, err = m.AppDomainStatuses("blog")
	if err != nil {
		t.Fatalf("AppDomainStatuses: %v", err)
	}
	if len(sts) != 2 || sts[0].Domain != "a.example.com" || sts[1].Domain != "b.example.com" {
		t.Fatalf("statuses = %+v, want blog's two domains ordered", sts)
	}
	a := sts[0]
	if a.App != "blog" || a.Status != StatusActive || a.Error != "" {
		t.Fatalf("a = %+v", a)
	}
	if a.CertNotAfter == nil || !a.CertNotAfter.Equal(notAfter) {
		t.Fatalf("a.CertNotAfter = %v, want %v", a.CertNotAfter, notAfter)
	}
	wantRec := DNSRecord{Type: "CNAME", Name: "a.example.com", Value: "relay.example.net"}
	if len(a.DNSRecords) != 1 || a.DNSRecords[0] != wantRec {
		t.Fatalf("a.DNSRecords = %+v, want [%+v]", a.DNSRecords, wantRec)
	}
	if !a.DNSOK { // the test resolver points every name at the relay
		t.Fatal("a.DNSOK = false, want true")
	}
	b := sts[1]
	if b.Status != StatusFailed || b.Error != "acme: boom" || b.CertNotAfter != nil {
		t.Fatalf("b = %+v", b)
	}
}

func TestAppDomainStatusSingle(t *testing.T) {
	m, st, _, _, _, _ := newAppTestManager(t, &fakeIssuer{})
	if _, err := st.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AppDomainStatus("ghost.example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing domain err = %v, want ErrNotFound", err)
	}

	if err := st.AddAppDomain("shop.example.com", "blog"); err != nil {
		t.Fatal(err)
	}
	// DNS pointing elsewhere: dns_ok false, never an error.
	m.resolve = func(_ context.Context, host string) ([]net.IP, error) {
		if host == "shop.example.com" {
			return []net.IP{net.ParseIP("198.51.100.9")}, nil
		}
		return []net.IP{net.ParseIP("203.0.113.7")}, nil
	}
	got, err := m.AppDomainStatus("shop.example.com")
	if err != nil {
		t.Fatalf("AppDomainStatus: %v", err)
	}
	if got.Domain != "shop.example.com" || got.App != "blog" || got.DNSOK {
		t.Fatalf("status = %+v, want shop.example.com/blog with dns_ok false", got)
	}
}
