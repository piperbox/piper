package relay

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/piperbox/piper/internal/tunnel"
)

// startTestRelay opens a store with one enrolled account-bound agent, starts
// Serve on ephemeral ports with the given tlsCfg, and dials an agent tunnel back.
// It returns the agent session, the relay's TLS address, its plain-HTTP (:80
// stand-in) address, the agent base domain, the store, and the router.
func startTestRelay(t *testing.T, tlsCfg *tls.Config, ctrl http.Handler) (*tunnel.Session, string, string, string, *Store, *Router) {
	t.Helper()
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	acc, _ := st.UpsertAccount("sub-1", "alice")
	en, _ := st.EnrollForAccount(acc.ID, "")

	tlsLn, _ := net.Listen("tcp", "127.0.0.1:0")
	httpLn, _ := net.Listen("tcp", "127.0.0.1:0")
	tunLn, _ := net.Listen("tcp", "127.0.0.1:0")
	router := NewRouter()

	var ctrlQ *connQueue
	if ctrl != nil && tlsCfg != nil {
		ctrlQ = newConnQueue()
		srv := &http.Server{Handler: ctrl, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
		go func() { _ = srv.Serve(ctrlQ) }()
		t.Cleanup(func() { ctrlQ.Close() })
	}
	ctrlHost := "api." + st.apexOrDefault()

	go func() {
		for {
			c, err := tunLn.Accept()
			if err != nil {
				return
			}
			sess, err := tunnel.Serve(c, func(tok, base string) error {
				ag, err := st.Authenticate(tok)
				if err != nil {
					return err
				}
				if ag.BaseDomain != base {
					return ErrBadToken
				}
				return nil
			})
			if err != nil {
				c.Close()
				continue
			}
			router.Register(sess)
			go serveControl(sess, st, router, nil)
		}
	}()
	go func() {
		for {
			c, err := tlsLn.Accept()
			if err != nil {
				return
			}
			go handlePublic(c, router, tlsCfg, ctrlHost, ctrlQ, nil)
		}
	}()
	go acceptHTTP(httpLn, router, nil)
	t.Cleanup(func() { tlsLn.Close(); httpLn.Close(); tunLn.Close() })

	conn, err := net.Dial("tcp", tunLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := tunnel.Dial(conn, en.Token, en.BaseDomain)
	if err != nil {
		t.Fatal(err)
	}
	// The relay's accept goroutine above registers the session into router only
	// after tunnel.Serve completes its handshake read and auth (Serve then
	// Register), which lags Dial's return. Callers that use the returned router
	// immediately need that registration to have already landed.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := router.Lookup(en.BaseDomain); ok {
			break
		}
		time.Sleep(time.Millisecond)
	}
	return sess, tlsLn.Addr().String(), httpLn.Addr().String(), en.BaseDomain, st, router
}

func TestControlRegisterThenTerminate(t *testing.T) {
	cert, key := writeWildcard(t, "public.getpiper.co")
	tlsCfg, err := LoadWildcardConfig(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	sess, tlsAddr, _, _, _, _ := startTestRelay(t, tlsCfg, nil)

	// Agent side: register a hostname over a control stream.
	cs, err := sess.OpenKind(tunnel.KindControl)
	if err != nil {
		t.Fatal(err)
	}
	if err := tunnel.WriteMsg(cs, tunnel.ControlRequest{Op: "register", App: "blog"}); err != nil {
		t.Fatal(err)
	}
	var resp tunnel.ControlResponse
	if err := tunnel.ReadMsg(cs, &resp); err != nil {
		t.Fatal(err)
	}
	cs.Close()
	if resp.Error != "" || resp.Hostname == "" {
		t.Fatalf("register resp = %+v", resp)
	}
	hostname := resp.Hostname

	// Agent side: accept the relay's KindHTTP stream and answer HTTP/1.1.
	go func() {
		for {
			kind, stream, err := sess.AcceptKind()
			if err != nil {
				return
			}
			if kind != tunnel.KindHTTP {
				stream.Close()
				continue
			}
			go func() {
				defer stream.Close()
				br := bufio.NewReader(stream)
				if _, err := http.ReadRequest(br); err != nil {
					return
				}
				io.WriteString(stream, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nhi")
			}()
		}
	}()

	// Visitor side: TLS to the relay with SNI = the assigned hostname, GET /.
	deadline := time.Now().Add(5 * time.Second)
	var body string
	for time.Now().Before(deadline) {
		d := &tls.Dialer{Config: &tls.Config{ServerName: hostname, InsecureSkipVerify: true}}
		c, err := d.Dial("tcp", tlsAddr)
		if err == nil {
			fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", hostname)
			b, _ := io.ReadAll(c)
			c.Close()
			if len(b) > 0 {
				body = string(b)
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if body == "" || !contains(body, "hi") {
		t.Fatalf("terminated response = %q", body)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestControlProvisionStoresToken(t *testing.T) {
	sess, _, _, base, st, _ := startTestRelay(t, nil, nil)

	cs, err := sess.OpenKind(tunnel.KindControl)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if err := tunnel.WriteMsg(cs, tunnel.ControlRequest{Op: "provision", Token: "box-tok"}); err != nil {
		t.Fatal(err)
	}
	var resp tunnel.ControlResponse
	if err := tunnel.ReadMsg(cs, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != "" {
		t.Fatalf("provision error: %s", resp.Error)
	}
	if got, err := st.ControlToken(base); err != nil || got != "box-tok" {
		t.Fatalf("ControlToken = %q, %v (want box-tok)", got, err)
	}
}

func TestControlProvisionRejectsEmptyToken(t *testing.T) {
	sess, _, _, base, st, _ := startTestRelay(t, nil, nil)

	// Seed a working token first, so we can confirm an empty provision
	// doesn't clear it.
	cs, err := sess.OpenKind(tunnel.KindControl)
	if err != nil {
		t.Fatal(err)
	}
	if err := tunnel.WriteMsg(cs, tunnel.ControlRequest{Op: "provision", Token: "box-tok"}); err != nil {
		t.Fatal(err)
	}
	var resp tunnel.ControlResponse
	if err := tunnel.ReadMsg(cs, &resp); err != nil {
		t.Fatal(err)
	}
	cs.Close()
	if resp.Error != "" {
		t.Fatalf("seed provision error: %s", resp.Error)
	}

	cs2, err := sess.OpenKind(tunnel.KindControl)
	if err != nil {
		t.Fatal(err)
	}
	defer cs2.Close()
	if err := tunnel.WriteMsg(cs2, tunnel.ControlRequest{Op: "provision"}); err != nil {
		t.Fatal(err)
	}
	var resp2 tunnel.ControlResponse
	if err := tunnel.ReadMsg(cs2, &resp2); err != nil {
		t.Fatal(err)
	}
	if resp2.Error == "" {
		t.Fatalf("provision with empty token: want error, got %+v", resp2)
	}
	if got, err := st.ControlToken(base); err != nil || got != "box-tok" {
		t.Fatalf("ControlToken = %q, %v (want unchanged box-tok)", got, err)
	}
}

func TestSetDomainControlOpRemoved(t *testing.T) {
	sess, _, _, base, st, _ := startTestRelay(t, nil, nil)

	control := func(op, domain string) tunnel.ControlResponse {
		t.Helper()
		cs, err := sess.OpenKind(tunnel.KindControl)
		if err != nil {
			t.Fatal(err)
		}
		defer cs.Close()
		if err := tunnel.WriteMsg(cs, tunnel.ControlRequest{Op: op, Domain: domain}); err != nil {
			t.Fatal(err)
		}
		var resp tunnel.ControlResponse
		if err := tunnel.ReadMsg(cs, &resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}
	for _, domain := range []string{"blog.dev", "shop.dev"} {
		if resp := control("add-domain", domain); resp.Error != "" {
			t.Fatalf("add-domain %q error: %s", domain, resp.Error)
		}
		if resp := control("domain-active", domain); resp.Error != "" {
			t.Fatalf("domain-active %q error: %s", domain, resp.Error)
		}
	}

	resp := control("set-domain", "replace.dev")
	if resp.Error != "unknown op" {
		t.Fatalf("set-domain error = %q, want unknown op", resp.Error)
	}
	if got, _ := st.CustomDomains(base); len(got) != 2 || got[0] != "blog.dev" || got[1] != "shop.dev" {
		t.Fatalf("stored custom domains = %v, want [blog.dev shop.dev]", got)
	}
}

func TestControlPlaneSNIDispatch(t *testing.T) {
	cert, key := writeWildcard(t, "public.getpiper.co")
	tlsCfg, err := LoadWildcardConfig(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	ctrl := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ctrl-ok "+r.URL.Path)
	})
	_, tlsAddr, _, _, _, _ := startTestRelay(t, tlsCfg, ctrl)

	d := &tls.Dialer{Config: &tls.Config{ServerName: "api.public.getpiper.co", InsecureSkipVerify: true}}
	c, err := d.Dial("tcp", tlsAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "GET /ping HTTP/1.1\r\nHost: api.public.getpiper.co\r\nConnection: close\r\n\r\n")
	b, _ := io.ReadAll(c)
	if !contains(string(b), "ctrl-ok /ping") {
		t.Fatalf("control dispatch response = %q", b)
	}
}

// controlOp sends one control request over sess and returns the response,
// failing the test on transport errors or an unexpected error-ness.
func controlOp(t *testing.T, sess *tunnel.Session, req tunnel.ControlRequest, wantErr bool) tunnel.ControlResponse {
	t.Helper()
	cs, err := sess.OpenKind(tunnel.KindControl)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if err := tunnel.WriteMsg(cs, req); err != nil {
		t.Fatal(err)
	}
	var resp tunnel.ControlResponse
	if err := tunnel.ReadMsg(cs, &resp); err != nil {
		t.Fatal(err)
	}
	if wantErr && resp.Error == "" {
		t.Fatalf("%s %q accepted, want rejection", req.Op, req.Domain)
	}
	if !wantErr && resp.Error != "" {
		t.Fatalf("%s %q: %s", req.Op, req.Domain, resp.Error)
	}
	return resp
}

// A pending claim must route immediately: that is what lets the TLS-ALPN-01
// challenge reach the box before any cert exists (#227).
func TestAddDomainRoutesWhilePending(t *testing.T) {
	sess, tlsAddr, _, _, st, _ := startTestRelay(t, nil, nil)

	got := make(chan byte, 1)
	go func() {
		for {
			kind, stream, err := sess.AcceptKind()
			if err != nil {
				return
			}
			if kind != tunnel.KindPassthrough {
				stream.Close()
				continue
			}
			buf := make([]byte, 1)
			if _, err := io.ReadFull(stream, buf); err == nil {
				got <- buf[0]
			}
			stream.Close()
			return
		}
	}()

	controlOp(t, sess, tunnel.ControlRequest{Op: "add-domain", Domain: "shop.dev"}, false)
	if domains, _ := st.CustomDomains(sess.BaseDomain); len(domains) != 1 || domains[0] != "shop.dev" {
		t.Fatalf("stored domains = %v", domains)
	}

	conn, err := net.Dial("tcp", tlsAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	tc := tls.Client(conn, &tls.Config{ServerName: "shop.dev", InsecureSkipVerify: true})
	go tc.Handshake() // never completes — only the ClientHello needs to travel
	select {
	case b := <-got:
		if b != 0x16 {
			t.Fatalf("first passthrough byte = %#x, want TLS record type 0x16", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no passthrough stream reached the agent for a pending domain")
	}
}

func TestDomainLifecycleControlOps(t *testing.T) {
	sess, _, _, base, st, _ := startTestRelay(t, nil, nil)

	controlOp(t, sess, tunnel.ControlRequest{Op: "add-domain", Domain: "shop.dev"}, false)
	controlOp(t, sess, tunnel.ControlRequest{Op: "domain-active", Domain: "shop.dev"}, false)
	var status string
	if err := st.db.QueryRow(
		`SELECT status FROM custom_domains WHERE domain='shop.dev'`).Scan(&status); err != nil || status != "active" {
		t.Fatalf("status = %q, %v, want active", status, err)
	}
	// Confirming a domain you don't hold is rejected.
	controlOp(t, sess, tunnel.ControlRequest{Op: "domain-active", Domain: "other.dev"}, true)
	// Malformed and relay-namespace domains are rejected on add.
	controlOp(t, sess, tunnel.ControlRequest{Op: "add-domain", Domain: "Bad_Domain"}, true)
	controlOp(t, sess, tunnel.ControlRequest{Op: "add-domain", Domain: base}, true)

	controlOp(t, sess, tunnel.ControlRequest{Op: "remove-domain", Domain: "shop.dev"}, false)
	if got, _ := st.CustomDomains(base); len(got) != 0 {
		t.Fatalf("domains after remove = %v", got)
	}
}

// Reconnect re-derives live domains (active + unexpired pending) and drops
// expired pending squats; a rival claim over an expired squat overwrites the
// router mapping in place.
func TestReconnectRederivesCustomDomains(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	now := time.Now()
	st.nowFunc = func() time.Time { return now }
	tokA, err := st.Enroll("alice", "alice.example.com")
	if err != nil {
		t.Fatal(err)
	}
	tokB, err := st.Enroll("bob", "bob.example.com")
	if err != nil {
		t.Fatal(err)
	}

	// Seed: one active, one fresh pending, one expired pending.
	if err := st.AddCustomDomain("alice.example.com", "active.dev"); err != nil {
		t.Fatal(err)
	}
	if err := st.ConfirmCustomDomain("alice.example.com", "active.dev"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddCustomDomain("alice.example.com", "squat.dev"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(pendingTTL + time.Second) // squat.dev expires
	if err := st.AddCustomDomain("alice.example.com", "fresh.dev"); err != nil {
		t.Fatal(err)
	}

	router := NewRouter()
	tunLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tunLn.Close()
	go acceptTunnels(tunLn, st, router, nil, nil, nil, testInstance(t, st))

	dial := func(tok, base string) *tunnel.Session {
		t.Helper()
		conn, err := net.Dial("tcp", tunLn.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		sess, err := tunnel.Dial(conn, tok, base)
		if err != nil {
			t.Fatal(err)
		}
		return sess
	}
	sessA := dial(tokA, "alice.example.com")
	defer sessA.Close()

	waitRouted := func(domain string, want *tunnel.Session) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			// want is the client-side Session from tunnel.Dial; router.Lookup
			// returns the server-side Session from tunnel.Serve inside
			// acceptTunnels — always a distinct object, so compare by the
			// identity that matters here: which agent owns the route.
			if s, ok := router.Lookup(domain); ok && s.BaseDomain == want.BaseDomain {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("%s not routed to the expected session", domain)
	}
	waitRouted("active.dev", sessA)
	waitRouted("fresh.dev", sessA)
	if _, ok := router.Lookup("squat.dev"); ok {
		t.Fatal("expired pending domain routed after reconnect")
	}

	// Rival claim over the expired squat: bob's registration overwrites in place.
	sessB := dial(tokB, "bob.example.com")
	defer sessB.Close()
	controlOp(t, sessB, tunnel.ControlRequest{Op: "add-domain", Domain: "squat.dev"}, false)
	waitRouted("squat.dev", sessB)

	// Cross-tenant remove-domain: alice never held squat.dev (bob does), so
	// her remove must be a no-op — idempotent success from her perspective,
	// but it must not unroute bob's live domain (#227 cross-tenant DoS).
	controlOp(t, sessA, tunnel.ControlRequest{Op: "remove-domain", Domain: "squat.dev"}, false)
	waitRouted("squat.dev", sessB)
	if got, _ := st.CustomDomains("bob.example.com"); len(got) != 1 || got[0] != "squat.dev" {
		t.Fatalf("bob's domains after alice's remove = %v, want [squat.dev]", got)
	}
}

func TestBindRepoControlOp(t *testing.T) {
	sess, _, _, base, st, _ := startTestRelay(t, nil, nil)

	control := func(req tunnel.ControlRequest) tunnel.ControlResponse {
		t.Helper()
		cs, err := sess.OpenKind(tunnel.KindControl)
		if err != nil {
			t.Fatal(err)
		}
		defer cs.Close()
		if err := tunnel.WriteMsg(cs, req); err != nil {
			t.Fatal(err)
		}
		var resp tunnel.ControlResponse
		if err := tunnel.ReadMsg(cs, &resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}

	if resp := control(tunnel.ControlRequest{
		Op: "bind-repo", App: "blog", Repo: "alice/blog", Branch: "main",
	}); resp.Error != "" {
		t.Fatalf("bind-repo error: %s", resp.Error)
	}
	ok, err := st.AgentBoundToRepo(base, "alice/blog")
	if err != nil || !ok {
		t.Fatalf("binding not stored: ok=%v err=%v", ok, err)
	}

	if resp := control(tunnel.ControlRequest{Op: "bind-repo", App: "blog"}); resp.Error == "" {
		t.Fatal("bind-repo without repo/branch was accepted")
	}

	if resp := control(tunnel.ControlRequest{Op: "unbind-repo", App: "blog"}); resp.Error != "" {
		t.Fatalf("unbind-repo error: %s", resp.Error)
	}
	if ok, _ := st.AgentBoundToRepo(base, "alice/blog"); ok {
		t.Fatal("binding survived unbind-repo")
	}
}

func TestGHTokenControlOpRejectsUnboundRepo(t *testing.T) {
	sess, _, _, _, _, _ := startTestRelay(t, nil, nil)
	cs, err := sess.OpenKind(tunnel.KindControl)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if err := tunnel.WriteMsg(cs, tunnel.ControlRequest{Op: "gh-token", Repo: "someone/else"}); err != nil {
		t.Fatal(err)
	}
	var resp tunnel.ControlResponse
	if err := tunnel.ReadMsg(cs, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error == "" || resp.Token != "" {
		t.Fatalf("unbound repo minted a token: %+v", resp)
	}
}

// #418: a box announces every slot it holds on connect; the relay prunes rows
// for the ones it dropped and routes the survivors, so the box no longer has to
// re-register each app one at a time.
func TestControlSyncAppsPrunesAndRoutes(t *testing.T) {
	cert, key := writeWildcard(t, "public.getpiper.co")
	tlsCfg, err := LoadWildcardConfig(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	sess, _, _, base, st, router := startTestRelay(t, tlsCfg, nil)

	// Two apps registered; the box will report only one back.
	kept, err := st.RegisterHostname(base, "blog", 0)
	if err != nil {
		t.Fatal(err)
	}
	dropped, err := st.RegisterHostname(base, "shop", 0)
	if err != nil {
		t.Fatal(err)
	}
	// Route it first, so the assertion below tests that sync unroutes a pruned
	// host rather than passing because it was never mapped.
	router.RegisterHost(dropped, sess)

	cs, err := sess.OpenKind(tunnel.KindControl)
	if err != nil {
		t.Fatal(err)
	}
	if err := tunnel.WriteMsg(cs, tunnel.ControlRequest{
		Op:   "sync-apps",
		Apps: []tunnel.AppRef{{App: "blog"}},
	}); err != nil {
		t.Fatal(err)
	}
	var resp tunnel.ControlResponse
	if err := tunnel.ReadMsg(cs, &resp); err != nil {
		t.Fatal(err)
	}
	cs.Close()
	if resp.Error != "" {
		t.Fatalf("sync-apps resp = %+v", resp)
	}
	// The response must name the survivors: the box persists these, and #405
	// means the hostname it stored at deploy time can be stale.
	if len(resp.Apps) != 1 || resp.Apps[0].App != "blog" || resp.Apps[0].Hostname != kept {
		t.Fatalf("resp.Apps = %+v, want [{blog 0 %s}]", resp.Apps, kept)
	}

	if _, ok := router.LookupHost(kept); !ok {
		t.Errorf("kept hostname %q is not routed", kept)
	}
	if _, ok := router.LookupHost(dropped); ok {
		t.Errorf("dropped hostname %q is still routed", dropped)
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM hostnames`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("hostname rows = %d, want 1", n)
	}
}

// dialTestTunnel runs serveTunnel for inst on one end of a pipe and dials
// the agent side, returning the agent's session once the relay holds it.
func dialTestTunnel(t *testing.T, st *Store, router *Router, inst *Instance, en Enrollment) *tunnel.Session {
	t.Helper()
	cc, sc := net.Pipe()
	t.Cleanup(func() { cc.Close(); sc.Close() })
	go serveTunnel(sc, st, router, st.AgentDisabled, nil, nil, inst)
	sess, err := tunnel.Dial(cc, en.Token, en.BaseDomain)
	if err != nil {
		t.Fatal(err)
	}
	waitCond(t, 3*time.Second, "session registered", func() bool {
		_, ok := router.Lookup(en.BaseDomain)
		return ok
	})
	return sess
}

func TestServeTunnelRecordsAndClearsOwner(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	inst := testInstance(t, st)
	router := NewRouter()

	sess := dialTestTunnel(t, st, router, inst, en)
	waitCond(t, 3*time.Second, "owner row written", func() bool {
		r, ok, _ := st.OwnerOf(en.BaseDomain)
		return ok && r.ID == inst.ID
	})
	sess.Close()
	waitCond(t, 3*time.Second, "owner row cleared", func() bool {
		_, ok, _ := st.OwnerOf(en.BaseDomain)
		return !ok
	})
}

func TestServeTunnelNeverClearsAnotherRelaysOwnership(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	mine := testInstance(t, st)
	other := testInstance(t, st)
	router := NewRouter()

	sess := dialTestTunnel(t, st, router, mine, en)
	waitCond(t, 3*time.Second, "owner row written", func() bool {
		r, ok, _ := st.OwnerOf(en.BaseDomain)
		return ok && r.ID == mine.ID
	})
	// The agent reconnected elsewhere while our half-open session lingers.
	if err := st.SetOwner(en.BaseDomain, other.ID); err != nil {
		t.Fatal(err)
	}
	sess.Close()
	waitCond(t, 3*time.Second, "stale session unregistered", func() bool {
		_, ok := router.Lookup(en.BaseDomain)
		return !ok
	})
	if r, ok, _ := st.OwnerOf(en.BaseDomain); !ok || r.ID != other.ID {
		t.Fatalf("owner after stale unregister = %+v ok=%v, want %s", r, ok, other.ID)
	}
}

// tunnelLogSpy is a race-safe log sink. serveTunnel logs "agent gone" from its
// own goroutine *after* it has released ownership, so the line is the sync
// point that proves a teardown finished — without it a test asserting the
// owner row survived could pass before the teardown ever ran.
type tunnelLogSpy struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *tunnelLogSpy) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *tunnelLogSpy) saw(sub string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Contains(s.buf.String(), sub)
}

// TestServeTunnelKeepsOwnerWhenItStillHoldsANewerSession: the agent's TCP goes
// half-open and it redials, and the edge places it back on *this* relay. The
// new session re-records the same owner row; when the old session's keepalive
// finally fires, its teardown must not delete it. WHERE instance_id = me does
// not help here — both sessions are this instance — so the guard has to be the
// router: we still hold the base, so the row is not ours to clear.
func TestServeTunnelKeepsOwnerWhenItStillHoldsANewerSession(t *testing.T) {
	spy := &tunnelLogSpy{}
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(spy)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })

	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	inst := testInstance(t, st)
	router := NewRouter()

	first := dialTestTunnel(t, st, router, inst, en)
	stale, _ := router.Holds(en.BaseDomain)
	waitCond(t, 3*time.Second, "owner row written", func() bool {
		r, ok, _ := st.OwnerOf(en.BaseDomain)
		return ok && r.ID == inst.ID
	})

	// The redial lands on the same relay.
	cc, sc := net.Pipe()
	t.Cleanup(func() { cc.Close(); sc.Close() })
	go serveTunnel(sc, st, router, st.AgentDisabled, nil, nil, inst)
	second, err := tunnel.Dial(cc, en.Token, en.BaseDomain)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.Close() })
	waitCond(t, 3*time.Second, "newer session registered", func() bool {
		s, ok := router.Holds(en.BaseDomain)
		return ok && s != stale
	})

	first.Close()
	waitCond(t, 3*time.Second, "stale session torn down", func() bool {
		return spy.saw("agent gone: " + en.BaseDomain)
	})

	if r, ok, _ := st.OwnerOf(en.BaseDomain); !ok || r.ID != inst.ID {
		t.Fatalf("owner after stale teardown = %+v ok=%v, want %s", r, ok, inst.ID)
	}
	if s, ok := router.Holds(en.BaseDomain); !ok || s == stale {
		t.Fatal("newer session lost its registration")
	}
}
