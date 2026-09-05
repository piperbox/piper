package relay

import (
	"net"
	"testing"
	"time"

	"github.com/piperbox/piper/internal/tunnel"
)

func TestRouterSuffixMatch(t *testing.T) {
	r := NewRouter()
	sess := &tunnel.Session{BaseDomain: "alice.example.com"}
	r.Register(sess)

	if got, ok := r.Lookup("blog.alice.example.com"); !ok || got != sess {
		t.Fatalf("subdomain lookup failed: %v %v", got, ok)
	}
	if got, ok := r.Lookup("alice.example.com"); !ok || got != sess {
		t.Fatalf("apex lookup failed: %v %v", got, ok)
	}
	if _, ok := r.Lookup("evil.example.com"); ok {
		t.Fatal("unrelated host should not match")
	}
	r.Unregister(sess)
	if _, ok := r.Lookup("blog.alice.example.com"); ok {
		t.Fatal("lookup after unregister should fail")
	}
}

func TestRouterByHost(t *testing.T) {
	r := NewRouter()
	s1 := &tunnel.Session{BaseDomain: "aaaa-alice.public.getpiper.co"}
	s2 := &tunnel.Session{BaseDomain: "bbbb-bob.public.getpiper.co"}
	r.Register(s1)
	r.Register(s2)
	r.RegisterHost("blog-alice.public.getpiper.co", s1)
	r.RegisterHost("api-bob.public.getpiper.co", s2)

	if got, ok := r.LookupHost("blog-alice.public.getpiper.co"); !ok || got != s1 {
		t.Fatalf("LookupHost blog = %v,%v", got, ok)
	}
	// Terminated lookup is exact — no suffix matching.
	if _, ok := r.LookupHost("x.blog-alice.public.getpiper.co"); ok {
		t.Fatal("LookupHost must not suffix-match")
	}
	// Session teardown drops its terminated hostnames.
	r.Unregister(s1)
	if _, ok := r.LookupHost("blog-alice.public.getpiper.co"); ok {
		t.Fatal("host should be gone after Unregister(s1)")
	}
	if _, ok := r.LookupHost("api-bob.public.getpiper.co"); !ok {
		t.Fatal("s2 host must survive s1 teardown")
	}
}

// newSessionPair returns a live server/client tunnel session pair over an
// in-memory pipe, so a test can close the server side and exercise the router's
// refuse-closed-session guard against a genuinely torn-down session.
func newSessionPair(t *testing.T) (server, client *tunnel.Session) {
	t.Helper()
	c1, c2 := net.Pipe()
	type result struct {
		sess *tunnel.Session
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		s, err := tunnel.Serve(c1, func(string, string) error { return nil })
		ch <- result{s, err}
	}()
	client, err := tunnel.Dial(c2, "tok", "closed.example.com")
	if err != nil {
		t.Fatal(err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatal(r.err)
	}
	return r.sess, client
}

// TestRegisterRefusesClosedSession pins the stale-entry fix: a register that
// arrives for an already-closed session must no-op under the router lock, so no
// permanent byBase/byHost/custom entry can outlive the session's Unregister.
func TestRegisterRefusesClosedSession(t *testing.T) {
	r := NewRouter()
	server, client := newSessionPair(t)
	defer client.Close()

	server.Close()
	deadline := time.Now().Add(2 * time.Second)
	for !server.Closed() {
		if time.Now().After(deadline) {
			t.Fatal("server session did not close")
		}
		time.Sleep(5 * time.Millisecond)
	}

	r.Register(server)
	if _, ok := r.Lookup(server.BaseDomain); ok {
		t.Fatal("Register must refuse a closed session")
	}
	r.RegisterHost("host.example.com", server)
	if _, ok := r.LookupHost("host.example.com"); ok {
		t.Fatal("RegisterHost must refuse a closed session")
	}
	r.RegisterCustom("custom.example.com", server)
	if _, ok := r.Lookup("custom.example.com"); ok {
		t.Fatal("RegisterCustom must refuse a closed session")
	}
	if _, ok := r.LookupCustom("custom.example.com"); ok {
		t.Fatal("LookupCustom must not see a refused registration")
	}
}

// LookupCustom must match ONLY custom domains (exact or subdomain) — never an
// agent base domain or a terminated shared hostname. It is what keeps the :80
// Host routing (#228) from resurrecting shared-domain HTTP.
func TestRouterLookupCustomOnlyMatchesCustomDomains(t *testing.T) {
	r := NewRouter()
	sess := &tunnel.Session{BaseDomain: "alice.example.com"}
	r.Register(sess)
	r.RegisterHost("blog-alice.public.getpiper.co", sess)
	r.RegisterCustom("shop.dev", sess)

	if s, ok := r.LookupCustom("shop.dev"); !ok || s != sess {
		t.Fatal("exact custom domain should match")
	}
	if s, ok := r.LookupCustom("www.shop.dev"); !ok || s != sess {
		t.Fatal("subdomain of custom domain should match")
	}
	if _, ok := r.LookupCustom("alice.example.com"); ok {
		t.Fatal("agent base domain must not match LookupCustom")
	}
	if _, ok := r.LookupCustom("blog.alice.example.com"); ok {
		t.Fatal("subdomain of base domain must not match LookupCustom")
	}
	if _, ok := r.LookupCustom("blog-alice.public.getpiper.co"); ok {
		t.Fatal("terminated shared hostname must not match LookupCustom")
	}

	r.UnregisterCustom("shop.dev")
	if _, ok := r.LookupCustom("shop.dev"); ok {
		t.Fatal("custom domain should be gone after UnregisterCustom")
	}

	// Unregister(sess) sweeps custom entries out of LookupCustom too.
	r.RegisterCustom("shop.dev", sess)
	r.Unregister(sess)
	if _, ok := r.LookupCustom("shop.dev"); ok {
		t.Fatal("custom domain should be swept by Unregister")
	}
}

func TestRouterCustomDomain(t *testing.T) {
	r := NewRouter()
	sess := &tunnel.Session{BaseDomain: "alice.example.com"}
	r.Register(sess)
	r.RegisterCustom("shop.dev", sess)

	if s, ok := r.Lookup("blog.shop.dev"); !ok || s != sess {
		t.Fatal("subdomain of custom domain should route to the session")
	}
	if s, ok := r.Lookup("shop.dev"); !ok || s != sess {
		t.Fatal("custom apex should route to the session")
	}

	r.UnregisterCustom("shop.dev")
	if _, ok := r.Lookup("blog.shop.dev"); ok {
		t.Fatal("custom domain should be gone after UnregisterCustom")
	}

	// Unregister(sess) sweeps custom entries too.
	r.RegisterCustom("shop.dev", sess)
	r.Unregister(sess)
	if _, ok := r.Lookup("blog.shop.dev"); ok {
		t.Fatal("custom domain should be swept by Unregister")
	}
	if _, ok := r.Lookup("x.alice.example.com"); ok {
		t.Fatal("base domain should be swept by Unregister")
	}
}

// TestUnregisterKeepsSuccessorEntries pins the invariant that makes a late
// unregister harmless: when an agent reconnects before the relay has noticed
// the old session dying, the new session overwrites every entry, and the old
// session's Unregister — which may land seconds later — must sweep only its
// own. Unregister matches on session identity, so this holds, and it is why
// the reconnect path never has to wait for the dead session to be reaped
// (#368; the delay there was TCP declining to report the peer's close for a
// full macOS MSL).
func TestUnregisterKeepsSuccessorEntries(t *testing.T) {
	r := NewRouter()
	base := "alice.example.com"
	old := &tunnel.Session{BaseDomain: base}
	fresh := &tunnel.Session{BaseDomain: base}

	for _, s := range []*tunnel.Session{old, fresh} {
		r.Register(s)
		r.RegisterHost("blog-alice.public.getpiper.co", s)
		r.RegisterCustom("shop.dev", s)
	}

	r.Unregister(old)

	if s, ok := r.Lookup(base); !ok || s != fresh {
		t.Fatalf("base domain = %v (%p), want the reconnected session %p", ok, s, fresh)
	}
	if s, ok := r.LookupHost("blog-alice.public.getpiper.co"); !ok || s != fresh {
		t.Fatalf("terminated hostname = %v (%p), want the reconnected session %p", ok, s, fresh)
	}
	if s, ok := r.LookupCustom("shop.dev"); !ok || s != fresh {
		t.Fatalf("custom domain = %v (%p), want the reconnected session %p", ok, s, fresh)
	}
}

func TestRouterCounts(t *testing.T) {
	r := NewRouter()
	s1 := &tunnel.Session{BaseDomain: "alice.example.com"}
	s2 := &tunnel.Session{BaseDomain: "bob.example.com"}
	r.Register(s1)
	r.Register(s2)
	r.RegisterHost("blog-alice.public.getpiper.co", s1)
	// RegisterCustom writes byBase AND custom — Counts must not double-count
	// the custom domain as an agent.
	r.RegisterCustom("byo.example.org", s1)

	a, h, c := r.Counts()
	if a != 2 || h != 1 || c != 1 {
		t.Fatalf("Counts() = %d,%d,%d; want 2,1,1", a, h, c)
	}
	r.Unregister(s1)
	a, h, c = r.Counts()
	if a != 1 || h != 0 || c != 0 {
		t.Fatalf("after Unregister: Counts() = %d,%d,%d; want 1,0,0", a, h, c)
	}
}

func TestHoldsIsExactAndBasesListsAgentsOnly(t *testing.T) {
	r := NewRouter()
	relaySess, _ := pipeSession(t, "box.public.getpiper.co")
	r.Register(relaySess)
	r.RegisterCustom("shop.example.com", relaySess)

	if _, ok := r.Holds("box.public.getpiper.co"); !ok {
		t.Fatal("Holds missed the registered base")
	}
	if _, ok := r.Holds("app.box.public.getpiper.co"); ok {
		t.Fatal("Holds walked a suffix; it must be exact")
	}
	if got := r.Bases(); len(got) != 1 || got[0] != "box.public.getpiper.co" {
		t.Fatalf("Bases() = %v, want the one agent base", got)
	}
}

// Sessions is Drain's view of what is still connected: one entry per agent,
// and a custom domain (which shares byBase) must not make an agent appear
// twice.
func TestSessionsListsAgentsNotCustomDomains(t *testing.T) {
	r := NewRouter()
	a, _ := pipeSession(t, "a.public.getpiper.co")
	b, _ := pipeSession(t, "b.public.getpiper.co")
	r.Register(a)
	r.Register(b)
	r.RegisterCustom("shop.example.com", a)

	got := r.Sessions()
	if len(got) != 2 {
		t.Fatalf("Sessions() has %d entries, want 2 (custom domain excluded)", len(got))
	}
	seen := map[*tunnel.Session]bool{}
	for _, s := range got {
		seen[s] = true
	}
	if !seen[a] || !seen[b] {
		t.Fatalf("Sessions() = %v, want both a and b", got)
	}
	if got := NewRouter().Sessions(); len(got) != 0 {
		t.Fatalf("empty router lists %d sessions", len(got))
	}
}
