package relay

import (
	"bytes"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/piperbox/piper/internal/tunnel"
)

func TestAcceptTunnelsRebindsCustomDomainOnReconnect(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)

	acc, err := st.UpsertAccount("sub-1", "alice")
	if err != nil {
		t.Fatal(err)
	}
	en, err := st.EnrollForAccount(acc.ID, "")
	if err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	router := NewRouter()
	go acceptTunnels(ln, st, router, nil, nil, nil, testInstance(t, st))

	customDomain := "app.example.com"

	connect := func() *tunnel.Session {
		t.Helper()
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		sess, err := tunnel.Dial(conn, en.Token, en.BaseDomain)
		if err != nil {
			t.Fatal(err)
		}
		return sess
	}

	controlDomain := func(sess *tunnel.Session, op, domain string) {
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
		if resp.Error != "" {
			t.Fatalf("%s %q error: %s", op, domain, resp.Error)
		}
	}

	waitFor := func(desc string, fn func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if fn() {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("timeout waiting for %s", desc)
	}

	sess1 := connect()
	controlDomain(sess1, "add-domain", customDomain)
	controlDomain(sess1, "domain-active", customDomain)
	waitFor("custom domain on first session", func() bool {
		s, ok := router.Lookup(customDomain)
		return ok && s.BaseDomain == en.BaseDomain
	})
	first, _ := router.Lookup(customDomain)

	// The rebind must not wait on the old session being swept: serveTunnel
	// re-derives custom domains for every connect, so the new session's
	// registration overwrites the entry whatever state the old one is in.
	//
	// Deliberately NOT asserted here: that the entry disappears in between.
	// Unregistration is driven by the relay observing the peer's close, and TCP
	// does not promise that promptly — a socket closed with unread data in
	// flight can produce no FIN at all, only an RST once the kernel reaps the
	// orphan (macOS net.inet.tcp.msl, 15s). That is what made this test flaky
	// on a loaded box (#368); it was pinning transport timing, not relay
	// behaviour. The invariant that actually matters — the stale session's late
	// Unregister must not evict its successor — is proven directly and
	// deterministically by TestUnregisterKeepsSuccessorEntries.
	sess1.Close()

	sess2 := connect()
	waitFor("custom domain rebind after reconnect", func() bool {
		s, ok := router.Lookup(customDomain)
		return ok && s != first && s.BaseDomain == en.BaseDomain
	})

	sess2.Close()
}

// syncLogBuffer is a concurrency-safe log sink: the accept goroutine can still
// be inside log.Printf writing to it while the test polls for the line, and a
// bare bytes.Buffer is not safe for that concurrent use.
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

// A rejected handshake must leave a trace on the relay. serveTunnel used to
// close the connection silently, so a box with a stale enrollment was invisible
// on both ends at once: the agent logged "connected" (#400) and the relay
// logged nothing at all, making the ops /logs endpoint useless for diagnosing
// exactly the case it most needed to explain.
func TestServeTunnelLogsRejectedHandshake(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)

	var logged syncLogBuffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&logged)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go acceptTunnels(ln, st, NewRouter(), nil, nil, nil, testInstance(t, st))

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := tunnel.Dial(conn, "stale-token", "092942b4-alice.public.getpiper.co"); err == nil {
		t.Fatal("Dial succeeded with a token the relay never issued")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(logged.String(), "092942b4-alice.public.getpiper.co") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("rejected handshake left no log naming the base domain; log was %q", logged.String())
}

// A draining relay must not take a new agent: the listener stays open (a
// refused dial would make the edge evict us and cascade our owner rows) but
// every connection is closed before the handshake, and the log says why.
func TestAcceptTunnelsRefusesWhileDraining(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)

	var logged syncLogBuffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&logged)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	inst := testInstance(t, st)
	router := NewRouter()
	go acceptTunnels(ln, st, router, nil, nil, nil, inst)
	inst.MarkDraining()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := tunnel.Dial(conn, en.Token, en.BaseDomain); err == nil {
		t.Fatal("a valid enrollment got a session from a draining relay")
	}
	waitForLog(t, &logged, "refused: relay is draining")
	if agents, _, _ := router.Counts(); agents != 0 {
		t.Fatalf("router holds %d agents after a refused dial", agents)
	}

	// The listener itself must stay open (#523): a draining relay refuses the
	// session, not the socket, because a closed listener makes piper-edge
	// evict this relay and cascade-delete its owner rows. Prove that with a
	// second dial against the very same ln: if the drain branch had instead
	// called ln.Close(), this dial would fail at the TCP level (connection
	// refused, nothing listening) instead of being refused by the drain
	// check.
	conn2, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("listener no longer accepting connections while draining: %v", err)
	}
	defer conn2.Close()
	if _, err := tunnel.Dial(conn2, en.Token, en.BaseDomain); err == nil {
		t.Fatal("a valid enrollment got a session from a draining relay (second dial)")
	}
	waitCond(t, 2*time.Second, "second drain refusal logged", func() bool {
		return strings.Count(logged.String(), "refused: relay is draining") >= 2
	})
	if agents, _, _ := router.Counts(); agents != 0 {
		t.Fatalf("router holds %d agents after a second refused dial", agents)
	}
}
