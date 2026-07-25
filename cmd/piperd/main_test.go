package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/piperbox/piper/internal/store"
	"github.com/piperbox/piper/internal/tunnel"
	"github.com/piperbox/piper/internal/version"
)

type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) add(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

type recServer struct{ rec *recorder }

func (s *recServer) Shutdown(context.Context) error { s.rec.add("api-shutdown"); return nil }
func (s *recServer) Close() error                   { s.rec.add("api-close"); return nil }

type recWebhook struct {
	rec     *recorder
	drained bool
}

func (w *recWebhook) stop(context.Context)      { w.rec.add("webhook-stop") }
func (w *recWebhook) close()                    { w.rec.add("webhook-close") }
func (w *recWebhook) wait(context.Context) bool { w.rec.add("webhook-wait"); return w.drained }
func (w *recWebhook) cancel()                   { w.rec.add("webhook-cancel") }

type recManager struct{ rec *recorder }

func (m *recManager) Stop() { m.rec.add("caddy") }

type recStore struct{ rec *recorder }

func (s *recStore) FailBuildingDeployments() (int64, error) {
	s.rec.add("store-fail-building")
	return 0, nil
}
func (s *recStore) Close() error { s.rec.add("store"); return nil }

func TestShutdownDrainsBeforeInfrastructureTeardown(t *testing.T) {
	rec := &recorder{}
	shutdownWithTimeouts(
		&recServer{rec}, &recWebhook{rec: rec, drained: true},
		&recManager{rec}, &recStore{rec}, time.Second, 2*time.Second,
	)
	got := rec.snapshot()
	if len(got) != 7 {
		t.Fatalf("events = %v, want 7 events", got)
	}
	first := map[string]bool{got[0]: true, got[1]: true}
	if !first["api-shutdown"] || !first["webhook-stop"] {
		t.Fatalf("first events = %v, want API and webhook stop", got[:2])
	}
	wantTail := []string{"webhook-wait", "webhook-cancel", "caddy", "store-fail-building", "store"}
	if !reflect.DeepEqual(got[2:], wantTail) {
		t.Fatalf("tail = %v, want %v", got[2:], wantTail)
	}
}

type blockingWebhook struct {
	rec   *recorder
	waits int
}

func (w *blockingWebhook) stop(context.Context) { w.rec.add("webhook-stop") }
func (w *blockingWebhook) close()               { w.rec.add("webhook-close") }
func (w *blockingWebhook) cancel()              { w.rec.add("webhook-cancel") }

func (w *blockingWebhook) wait(ctx context.Context) bool {
	w.waits++
	w.rec.add("webhook-wait")
	if w.waits == 1 {
		<-ctx.Done()
		return false
	}
	return true
}

func TestShutdownCancelsAtDrainDeadlineAndStillTearsDown(t *testing.T) {
	rec := &recorder{}
	started := time.Now()
	shutdownWithTimeouts(
		&recServer{rec}, &blockingWebhook{rec: rec},
		&recManager{rec}, &recStore{rec}, 20*time.Millisecond, 100*time.Millisecond,
	)
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("shutdown took %v, want below 500ms", elapsed)
	}
	got := rec.snapshot()
	for _, want := range []string{"api-close", "webhook-close", "webhook-cancel", "caddy", "store"} {
		found := false
		for _, event := range got {
			found = found || event == want
		}
		if !found {
			t.Errorf("events = %v, missing %q", got, want)
		}
	}
}

func TestShutdownSkipsAbsentDependencies(t *testing.T) {
	rec := &recorder{}
	shutdownWithTimeouts(&recServer{rec}, nil, nil, &recStore{rec}, time.Second, 2*time.Second)
	got := rec.snapshot()
	want := []string{"api-shutdown", "store-fail-building", "store"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestShutdownFailsBuildingRowsBeforeStoreClose(t *testing.T) {
	rec := &recorder{}
	shutdownWithTimeouts(&recServer{rec}, nil, nil, &recStore{rec}, time.Second, 2*time.Second)
	got := rec.snapshot()
	// In-flight "building" rows must be finalized while the store is still open,
	// so a graceful shutdown never strands a permanent "building" row (#158).
	want := []string{"api-shutdown", "store-fail-building", "store"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestNewDNSProviderRejectsUnsupportedProvider(t *testing.T) {
	provider, err := newDNSProvider("route53")
	if provider != nil {
		t.Fatalf("provider = %T, want nil", provider)
	}
	if err == nil || !strings.Contains(err.Error(), `unsupported DNS provider "route53"`) {
		t.Fatalf("error = %v", err)
	}
}

func TestNewDNSProviderSelectsCloudflare(t *testing.T) {
	t.Setenv("CF_DNS_API_TOKEN", "test-token")
	for _, name := range []string{"", "cloudflare"} {
		t.Run(name, func(t *testing.T) {
			provider, err := newDNSProvider(name)
			if err != nil {
				t.Fatalf("newDNSProvider(%q): %v", name, err)
			}
			if provider == nil {
				t.Fatalf("newDNSProvider(%q) returned nil", name)
			}
		})
	}
}

type fakeProvisionStore struct {
	tokens  []store.Token
	created []string
	deleted []string
}

func (f *fakeProvisionStore) ListTokens() ([]store.Token, error) { return f.tokens, nil }
func (f *fakeProvisionStore) CreateToken(label, scope string) (string, error) {
	f.created = append(f.created, label+"/"+scope)
	return "tok-" + label, nil
}
func (f *fakeProvisionStore) DeleteToken(label string) error {
	f.deleted = append(f.deleted, label)
	return nil
}

func TestProvisionRelayControlFirstConnect(t *testing.T) {
	f := &fakeProvisionStore{}
	var pushed string
	provisionRelayControl(&sync.Mutex{}, f, func(tok string) error { pushed = tok; return nil }, "base.example.com")
	if len(f.created) != 1 || f.created[0] != "relay:base.example.com/admin" {
		t.Fatalf("created = %v", f.created)
	}
	if pushed != "tok-relay:base.example.com" {
		t.Fatalf("pushed = %q", pushed)
	}
	if len(f.deleted) != 0 {
		t.Fatalf("unexpected delete: %v", f.deleted)
	}
}

func TestProvisionRelayControlAlreadyProvisioned(t *testing.T) {
	f := &fakeProvisionStore{tokens: []store.Token{{Label: "relay:base.example.com"}}}
	provisionRelayControl(&sync.Mutex{}, f, func(string) error { t.Fatal("must not push"); return nil }, "base.example.com")
	if len(f.created) != 0 {
		t.Fatalf("re-minted: %v", f.created)
	}
}

func TestProvisionRelayControlRevokedMeansNo(t *testing.T) {
	// A revoked row is the owner's unilateral cutoff: never re-mint for this
	// enrollment (spec: re-provisioning requires a new claim → new base domain).
	rt := time.Now()
	f := &fakeProvisionStore{tokens: []store.Token{{Label: "relay:base.example.com", RevokedAt: &rt}}}
	provisionRelayControl(&sync.Mutex{}, f, func(string) error { t.Fatal("must not push"); return nil }, "base.example.com")
	if len(f.created) != 0 {
		t.Fatalf("re-minted after owner revoke: %v", f.created)
	}
}

func TestProvisionRelayControlPushFailureUnwinds(t *testing.T) {
	f := &fakeProvisionStore{}
	provisionRelayControl(&sync.Mutex{}, f, func(string) error { return errors.New("session died") }, "base.example.com")
	// The mint must be unwound so the marker doesn't block the next attempt.
	if len(f.deleted) != 1 || f.deleted[0] != "relay:base.example.com" {
		t.Fatalf("deleted = %v, want the just-minted label", f.deleted)
	}
}

// Both control-API servers (local tokenless + authenticated) must go through
// the same graceful drain; apiServers folds them into the one apiShutdowner
// slot shutdown() already has. #221.
func TestAPIServersShutdownCoversAll(t *testing.T) {
	rec := &recorder{}
	s := apiServers{&recServer{rec}, &recServer{rec}}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got := rec.snapshot()
	want := []string{"api-shutdown", "api-shutdown", "api-close", "api-close"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

// The authenticated listener is the relay tunnel's control-API entry point:
// it must sit on loopback at an ephemeral port and enforce the bearer, while
// the local listener (cfg.APIAddr) serves tokenless. #221.
func TestStartAuthAPIRequiresToken(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "piper.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()
	tok, err := st.CreateToken("test", "admin")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	addr, srv, err := startAuthAPI(st, handler)
	if err != nil {
		t.Fatalf("startAuthAPI: %v", err)
	}
	defer srv.Close()

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("addr %q: %v", addr, err)
	}
	if host != "127.0.0.1" {
		t.Errorf("host = %q, want 127.0.0.1", host)
	}
	if port == "0" || port == "" {
		t.Errorf("port = %q, want a bound ephemeral port", port)
	}

	get := func(bearer string) int {
		req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/v1/apps", nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if code := get(""); code != http.StatusUnauthorized {
		t.Errorf("no bearer = %d, want 401", code)
	}
	if code := get("nope"); code != http.StatusUnauthorized {
		t.Errorf("bad bearer = %d, want 401", code)
	}
	if code := get(tok); code != http.StatusOK {
		t.Errorf("valid bearer = %d, want 200", code)
	}
}

// The local listener is tokenless only while it binds loopback. A non-loopback
// bind (the documented PIPER_API_ADDR=0.0.0.0:8088 LAN flow) keeps requiring
// the bearer — otherwise that flow would expose an unauthenticated control API
// to the whole LAN. #221.
func TestNewLocalHandlerAuthFollowsBindAddress(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "piper.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	cases := []struct {
		addr     string
		wantCode int
	}{
		{"127.0.0.1:8088", http.StatusOK},
		{"localhost:8088", http.StatusOK},
		{"[::1]:8088", http.StatusOK},
		{"0.0.0.0:8088", http.StatusUnauthorized},
		{":8088", http.StatusUnauthorized},
		{"192.168.1.50:8088", http.StatusUnauthorized},
	}
	for _, c := range cases {
		t.Run(c.addr, func(t *testing.T) {
			h := newLocalHandler(st, ok, c.addr)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/apps", nil))
			if rec.Code != c.wantCode {
				t.Fatalf("tokenless request on bind %q = %d, want %d", c.addr, rec.Code, c.wantCode)
			}
		})
	}
}

// Tunnel control streams must land on the authenticated listener, never the
// tokenless local one — otherwise the relay path silently loses its bearer
// gate. Pins the dial wiring for both relay modes. #221.
func TestDialLocalControlGoesToAuthListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	accepted := make(chan struct{}, 2)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
			accepted <- struct{}{}
		}
	}()

	dial := newDialLocal(ln.Addr().String(), "", "127.0.0.1:443")
	conn, err := dial(tunnel.KindControlAPI, nil)
	if err != nil {
		t.Fatalf("dial control: %v", err)
	}
	conn.Close()
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("control stream did not reach the auth listener")
	}
}

// A passthrough stream whose ClientHello offers acme-tls/1 is a TLS-ALPN-01
// validation: it must be spliced to the in-process solver — with the peeked
// hello bytes replayed so the handshake isn't corrupted — instead of Caddy's
// :443 (#226).
func TestDialLocalPassthroughACMEGoesToSolver(t *testing.T) {
	solver, err := net.Listen("tcp", "127.0.0.1:0") // stands in for the ALPN solver
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer solver.Close()
	gotBytes := make(chan []byte, 1)
	go func() {
		c, err := solver.Accept()
		if err != nil {
			return
		}
		buf := make([]byte, 4096)
		n, _ := c.Read(buf)
		gotBytes <- buf[:n]
		c.Close()
	}()

	client, server := net.Pipe()
	defer client.Close()
	go func() {
		_ = tls.Client(client, &tls.Config{
			ServerName:         "myshop.example.com",
			NextProtos:         []string{"acme-tls/1"},
			InsecureSkipVerify: true,
		}).Handshake()
	}()

	dial := newDialLocal("127.0.0.1:1", solver.Addr().String(), "127.0.0.1:443")
	conn, err := dial(tunnel.KindPassthrough, server)
	if err != nil {
		t.Fatalf("dial passthrough: %v", err)
	}
	defer conn.Close()

	select {
	case replayed := <-gotBytes:
		// 0x16 = TLS handshake record: the consumed ClientHello was replayed.
		if len(replayed) == 0 || replayed[0] != 0x16 {
			t.Fatalf("solver received %d bytes (want a replayed TLS ClientHello)", len(replayed))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("acme-tls/1 passthrough never reached the solver")
	}
}

// A passthrough stream whose ClientHello does NOT offer acme-tls/1 (ordinary
// HTTPS traffic) must reach Caddy (caddyAddr) with the peeked ClientHello
// replayed, and must never be routed to the ALPN solver stand-in (#226).
func TestDialLocalPassthroughNonACMEGoesToCaddy(t *testing.T) {
	caddy, err := net.Listen("tcp", "127.0.0.1:0") // stands in for Caddy's :443
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer caddy.Close()
	gotBytes := make(chan []byte, 1)
	go func() {
		c, err := caddy.Accept()
		if err != nil {
			return
		}
		buf := make([]byte, 4096)
		n, _ := c.Read(buf)
		gotBytes <- buf[:n]
		c.Close()
	}()

	solver, err := net.Listen("tcp", "127.0.0.1:0") // must NOT be reached
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer solver.Close()
	solverHit := make(chan struct{}, 1)
	go func() {
		c, err := solver.Accept()
		if err != nil {
			return
		}
		c.Close()
		solverHit <- struct{}{}
	}()

	client, server := net.Pipe()
	defer client.Close()
	go func() {
		_ = tls.Client(client, &tls.Config{
			ServerName:         "myshop.example.com",
			NextProtos:         []string{"h2", "http/1.1"},
			InsecureSkipVerify: true,
		}).Handshake()
	}()

	dial := newDialLocal("127.0.0.1:1", solver.Addr().String(), caddy.Addr().String())
	conn, err := dial(tunnel.KindPassthrough, server)
	if err != nil {
		t.Fatalf("dial passthrough: %v", err)
	}
	defer conn.Close()

	select {
	case replayed := <-gotBytes:
		// 0x16 = TLS handshake record: the consumed ClientHello was replayed.
		if len(replayed) == 0 || replayed[0] != 0x16 {
			t.Fatalf("caddy received %d bytes (want a replayed TLS ClientHello)", len(replayed))
		}
	case <-solverHit:
		t.Fatal("non-acme passthrough reached the solver stand-in, want Caddy")
	case <-time.After(2 * time.Second):
		t.Fatal("non-acme passthrough never reached caddy")
	}
}

// piperd/piper-relay must have a version surface so the release ldflags stamp
// lands and the binary can report its build. #61.
func TestVersionRequested(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}} {
		if !versionRequested(args) {
			t.Errorf("versionRequested(%v) = false, want true", args)
		}
	}
	for _, args := range [][]string{nil, {"token"}, {"serve"}} {
		if versionRequested(args) {
			t.Errorf("versionRequested(%v) = true, want false", args)
		}
	}
	if version.String() == "" {
		t.Error("version.String() is empty")
	}
}

// concurrentProvisionStore is a thread-safe relayTokenStore whose ListTokens
// reflects prior CreateToken calls, so it exposes the list-then-mint TOCTOU.
type concurrentProvisionStore struct {
	mu      sync.Mutex
	tokens  []store.Token
	creates int
}

func (s *concurrentProvisionStore) ListTokens() ([]store.Token, error) {
	s.mu.Lock()
	snap := append([]store.Token(nil), s.tokens...)
	s.mu.Unlock()
	// Widen the read→mint window so that, without the provisioning mutex, every
	// concurrent caller observes the same (empty) list before any mint commits —
	// making the TOCTOU deterministic rather than scheduler-dependent. With the
	// mutex, callers are serialized and only the first ever sees it empty.
	time.Sleep(20 * time.Millisecond)
	return snap, nil
}

func (s *concurrentProvisionStore) CreateToken(label, scope string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creates++
	s.tokens = append(s.tokens, store.Token{Label: label})
	return "tok-" + label, nil
}

func (s *concurrentProvisionStore) DeleteToken(label string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.tokens[:0]
	for _, tk := range s.tokens {
		if tk.Label != label {
			out = append(out, tk)
		}
	}
	s.tokens = out
	return nil
}

// TestProvisionRelayControlConcurrentSingleMint pins the item-5 fix: when the
// first session flaps and multiple OnConnect callbacks run provisionRelayControl
// at once, the shared mutex serializes the list-then-mint so exactly one
// relay:<base> admin token is minted. Without the lock every goroutine reads an
// empty token list first and each mints a duplicate.
func TestProvisionRelayControlConcurrentSingleMint(t *testing.T) {
	s := &concurrentProvisionStore{}
	var mu sync.Mutex
	const n = 20
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			provisionRelayControl(&mu, s, func(string) error { return nil }, "base.example.com")
		}()
	}
	close(start)
	wg.Wait()
	if s.creates != 1 {
		t.Fatalf("minted %d relay tokens under concurrent OnConnect, want exactly 1", s.creates)
	}
}

// repushStore is the store slice repushRelayApps needs.
type repushStore struct {
	apps        []store.App
	previews    []store.Deployment
	err         error
	previewsErr error
}

func (s *repushStore) ListApps() ([]store.App, error) { return s.apps, s.err }

func (s *repushStore) RunningPreviews() ([]store.Deployment, error) {
	return s.previews, s.previewsErr
}

// repushAnnouncer records what the re-push announced to the relay.
type repushAnnouncer struct {
	binds       []string
	registered  []string
	registerErr error
}

func (a *repushAnnouncer) BindRepo(app, repo, branch string) error {
	a.binds = append(a.binds, app+"="+repo+"@"+branch)
	return nil
}

// Register records app@pr so a preview re-registration is distinguishable
// from the production one for the same app.
func (a *repushAnnouncer) Register(app string, pr int) (string, error) {
	a.registered = append(a.registered, fmt.Sprintf("%s@%d", app, pr))
	return app + ".relay.example", a.registerErr
}

// TestRepushRelayAppsReregistersHostnames pins #369: the relay's router keeps
// relay-assigned hostnames in memory only, and re-derives just custom domains
// when a session registers — so after a relay restart or tunnel flap every
// <hash>-<user> URL drops TLS until the app is redeployed. The box re-announces
// its own hostnames on every (re)connect to close that window.
func TestRepushRelayAppsReregistersHostnames(t *testing.T) {
	st := &repushStore{apps: []store.App{
		{Name: "test1", Hostname: "855d1432-ozykhan.relay.example"},
		{Name: "test2"}, // never deployed: no hostname to restore
	}}
	a := &repushAnnouncer{}

	repushRelayApps(st, a, true)

	if want := []string{"test1@0"}; !reflect.DeepEqual(a.registered, want) {
		t.Fatalf("re-registered %v, want %v", a.registered, want)
	}
}

// TestRepushRelayAppsReregistersPreviewHostnames is the preview half of #369,
// carried over as #376: previews get relay-assigned hostnames from the same
// register op, and they were as invisible to the reconnect re-push as they were
// to ResumeRoutes. Without this a live PR-preview URL goes dark on a relay
// restart and stays dark until the PR pushes again.
func TestRepushRelayAppsReregistersPreviewHostnames(t *testing.T) {
	st := &repushStore{
		apps: []store.App{{Name: "test1", Hostname: "855d1432-ozykhan.relay.example"}},
		previews: []store.Deployment{
			{App: "test1", PR: 7, Status: "running"},
			{App: "test1", PR: 9, Status: "running"},
		},
	}
	a := &repushAnnouncer{}

	repushRelayApps(st, a, true)

	want := []string{"test1@0", "test1@7", "test1@9"}
	if !reflect.DeepEqual(a.registered, want) {
		t.Fatalf("re-registered %v, want %v", a.registered, want)
	}
}

// A preview's hostname is claimed by the deploy, so a box with no relay-assigned
// hostnames must not register previews either — same guard as the app loop.
func TestRepushRelayAppsSkipsPreviewsWhenNotTerminated(t *testing.T) {
	st := &repushStore{
		apps:     []store.App{{Name: "test1", Hostname: "test1.piper.localhost"}},
		previews: []store.Deployment{{App: "test1", PR: 7, Status: "running"}},
	}
	a := &repushAnnouncer{}

	repushRelayApps(st, a, false)

	if len(a.registered) != 0 {
		t.Fatalf("re-registered %v on a non-terminated box, want none", a.registered)
	}
}

// A failed preview lookup must not cost the box its production hostnames.
func TestRepushRelayAppsPreviewErrorKeepsAppHostnames(t *testing.T) {
	st := &repushStore{
		apps:        []store.App{{Name: "test1", Hostname: "855d1432-ozykhan.relay.example"}},
		previewsErr: errors.New("disk"),
	}
	a := &repushAnnouncer{}

	repushRelayApps(st, a, true)

	if want := []string{"test1@0"}; !reflect.DeepEqual(a.registered, want) {
		t.Fatalf("re-registered %v, want %v", a.registered, want)
	}
}

// TestRepushRelayAppsSkipsHostnamesWhenNotTerminated keeps a BYO/LAN box out of
// it: with no relay-terminated hostnames, there is nothing to re-register, and
// registering would claim relay hostnames the box never asked for.
func TestRepushRelayAppsSkipsHostnamesWhenNotTerminated(t *testing.T) {
	st := &repushStore{apps: []store.App{
		{Name: "test1", Hostname: "test1.piper.localhost"},
	}}
	a := &repushAnnouncer{}

	repushRelayApps(st, a, false)

	if len(a.registered) != 0 {
		t.Fatalf("re-registered %v on a non-terminated box, want none", a.registered)
	}
}

// TestRepushRelayAppsStillBindsRepos guards the pre-existing re-push the
// hostname restore was folded into.
func TestRepushRelayAppsStillBindsRepos(t *testing.T) {
	st := &repushStore{apps: []store.App{
		{Name: "test1", Repo: "ozykhan/test1", Branch: "main", Hostname: "h.relay.example"},
		{Name: "test2"}, // no repo: nothing to bind
	}}
	a := &repushAnnouncer{}

	repushRelayApps(st, a, true)

	if want := []string{"test1=ozykhan/test1@main"}; !reflect.DeepEqual(a.binds, want) {
		t.Fatalf("re-bound %v, want %v", a.binds, want)
	}
}

// TestRepushRelayAppsRegisterErrorDoesNotStopOthers keeps one failing app from
// stranding the rest of the box's URLs.
func TestRepushRelayAppsRegisterErrorDoesNotStopOthers(t *testing.T) {
	st := &repushStore{apps: []store.App{
		{Name: "test1", Hostname: "a.relay.example"},
		{Name: "test2", Hostname: "b.relay.example"},
	}}
	a := &repushAnnouncer{registerErr: errors.New("relay: quota exceeded")}

	repushRelayApps(st, a, true)

	if want := []string{"test1@0", "test2@0"}; !reflect.DeepEqual(a.registered, want) {
		t.Fatalf("re-registered %v, want %v", a.registered, want)
	}
}
