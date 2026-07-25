package relay

import (
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/piperbox/piper/internal/tunnel"
)

func TestAcceptTunnelsRebindsCustomDomainOnReconnect(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.Configure("public.getpiper.co", 3, 10, 5)

	acc, err := st.UpsertAccount("sub-1", "alice")
	if err != nil {
		t.Fatal(err)
	}
	en, err := st.EnrollForAccount(acc.ID)
	if err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	router := NewRouter()
	go acceptTunnels(ln, st, router, nil, nil)

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
