package relay

import (
	"errors"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/piperbox/piper/internal/relay/relaytest"
	"github.com/piperbox/piper/internal/tunnel"
)

// TestMain shrinks the per-session disabled-watchdog interval for the whole
// package so eviction tests deadline-poll on a short tick instead of waiting the
// production 5s. The write happens once, before any test (and thus any watchdog
// goroutine) runs, so it does not race the goroutines that read it.
func TestMain(m *testing.M) {
	disabledPollInterval = 20 * time.Millisecond
	os.Exit(relaytest.Main(m))
}

// waitCond polls pred until it returns true or timeout elapses.
func waitCond(t *testing.T, timeout time.Duration, desc string, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v: %s", timeout, desc)
}

// serveTunnels is acceptTunnels with an injectable disabled-checker, letting a
// test drive the watchdog with a store read that fails on demand.
func serveTunnels(ln net.Listener, st *Store, router *Router, disabled func(string) (bool, error)) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go serveTunnel(conn, st, router, disabled, nil, nil)
	}
}

func dialAgent(t *testing.T, addr, token, base string) *tunnel.Session {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := tunnel.Dial(conn, token, base)
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	return sess
}

// TestDisabledWatchdogEvictsLiveSession drives the operator kill-switch through
// the real accept path: a live session is registered, then Store.DisableAccount
// flips the flag and the per-session watchdog must both stop routing the base
// and tear the client session down.
func TestDisabledWatchdogEvictsLiveSession(t *testing.T) {
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
	go acceptTunnels(ln, st, router, nil, nil, nil)

	sess := dialAgent(t, ln.Addr().String(), en.Token, en.BaseDomain)
	defer sess.Close()

	waitCond(t, 2*time.Second, "session registered", func() bool {
		_, ok := router.Lookup(en.BaseDomain)
		return ok
	})

	if err := st.DisableAccount(acc.Username, "user"); err != nil {
		t.Fatal(err)
	}

	waitCond(t, 2*time.Second, "base stops routing", func() bool {
		_, ok := router.Lookup(en.BaseDomain)
		return !ok
	})
	waitCond(t, 2*time.Second, "client session torn down", func() bool {
		return sess.Closed()
	})
}

// TestDeletedAgentWatchdogEvictsLiveSession drives the other affirmative kill
// read: a live session whose agent row is deleted out from under it (a future
// account-deletion path) must be evicted, not retried forever. AgentDisabled
// returns ErrUnknownAccount for the now-missing base, and the watchdog treats
// that as a permanent signal — the same eviction the operator kill-switch gets,
// distinct from a transient store error which keeps the session up. Mirrors
// TestDisabledWatchdogEvictsLiveSession, deleting the row straight in the store.
func TestDeletedAgentWatchdogEvictsLiveSession(t *testing.T) {
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
	go acceptTunnels(ln, st, router, nil, nil, nil)

	sess := dialAgent(t, ln.Addr().String(), en.Token, en.BaseDomain)
	defer sess.Close()

	waitCond(t, 2*time.Second, "session registered", func() bool {
		_, ok := router.Lookup(en.BaseDomain)
		return ok
	})

	// Delete the agent row mid-session: AgentDisabled now reads ErrUnknownAccount.
	if _, err := st.db.Exec(`DELETE FROM agents WHERE base_domain=$1`, en.BaseDomain); err != nil {
		t.Fatal(err)
	}

	waitCond(t, 2*time.Second, "base stops routing", func() bool {
		_, ok := router.Lookup(en.BaseDomain)
		return !ok
	})
	waitCond(t, 2*time.Second, "client session torn down", func() bool {
		return sess.Closed()
	})
}

// serveHandshake runs one tunnel handshake and returns the server-side Serve
// result: nil means auth authorized the connection, non-nil means it was
// rejected at the handshake (before any yamux session or watchdog exists). It
// dials a client in the background, mirroring TestServeRejectsBadAuth.
func serveHandshake(t *testing.T, auth tunnel.Auth, token, base string) error {
	t.Helper()
	c, s := net.Pipe()
	defer c.Close()
	defer s.Close()
	go func() {
		if sess, err := tunnel.Dial(c, token, base); err == nil {
			sess.Close()
		}
	}()
	sess, err := tunnel.Serve(s, auth)
	if sess != nil {
		sess.Close()
	}
	return err
}

// TestPostDisableRedialRejected proves a disabled account is turned away at the
// tunnel handshake — the auth-layer rejection ozykhan asked for, distinct from
// the watchdog rescue. It drives the relay's real handshake authorizer
// (tunnelAuth, the exact callback acceptTunnels hands tunnel.Serve) and asserts
// Serve rejects only after the account is disabled. Because no session is ever
// established here, the watchdog cannot mask a broken Store.Authenticate check.
func TestPostDisableRedialRejected(t *testing.T) {
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

	auth := tunnelAuth(st)

	// Baseline: a healthy agent completes the handshake.
	if err := serveHandshake(t, auth, en.Token, en.BaseDomain); err != nil {
		t.Fatalf("healthy agent rejected at handshake: %v", err)
	}

	if err := st.DisableAccount(acc.Username, "user"); err != nil {
		t.Fatal(err)
	}

	// After disable the handshake itself must fail — a later eviction is not
	// enough; the reconnect must never authenticate.
	if err := serveHandshake(t, auth, en.Token, en.BaseDomain); err == nil {
		t.Fatal("disabled account must be rejected at the tunnel handshake, not merely evicted later")
	}

	// Belt-and-braces through the full relay path: the re-dial never routes.
	// Since #400 the agent learns this at Dial — the relay acks its verdict
	// before yamux starts — so the rejection surfaces as a Dial error rather
	// than a session that comes up and is torn down a moment later.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	router := NewRouter()
	go acceptTunnels(ln, st, router, nil, nil, nil)

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sess, err := tunnel.Dial(conn, en.Token, en.BaseDomain)
	if err == nil {
		sess.Close()
		t.Fatal("disabled account must be rejected at the handshake, not handed a session")
	}
	if _, ok := router.Lookup(en.BaseDomain); ok {
		t.Fatal("disabled account must not register a base")
	}
}

// TestWatchdogTransientReadErrorKeepsSession pins the binding rule that a
// transient store read must not evict a healthy session: while the disabled
// check errors the session stays up, and only once the read recovers and
// reports disabled=true does the next tick evict.
func TestWatchdogTransientReadErrorKeepsSession(t *testing.T) {
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

	var failing atomic.Bool
	failing.Store(true)
	disabled := func(base string) (bool, error) {
		if failing.Load() {
			return false, errors.New("store unavailable")
		}
		return true, nil // recovered read: the account is disabled
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	router := NewRouter()
	go serveTunnels(ln, st, router, disabled)

	sess := dialAgent(t, ln.Addr().String(), en.Token, en.BaseDomain)
	defer sess.Close()

	waitCond(t, 2*time.Second, "session registered", func() bool {
		_, ok := router.Lookup(en.BaseDomain)
		return ok
	})

	// Across many ticks of erroring reads the session must survive.
	stableUntil := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(stableUntil) {
		if _, ok := router.Lookup(en.BaseDomain); !ok {
			t.Fatal("erroring read evicted a healthy session")
		}
		if sess.Closed() {
			t.Fatal("erroring read closed a healthy session")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Recover the read as disabled=true; the next tick must evict.
	failing.Store(false)
	waitCond(t, 2*time.Second, "base stops routing after recovery", func() bool {
		_, ok := router.Lookup(en.BaseDomain)
		return !ok
	})
	waitCond(t, 2*time.Second, "client session torn down after recovery", func() bool {
		return sess.Closed()
	})
}
