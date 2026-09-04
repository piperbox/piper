package relay

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

type notification struct{ channel, payload string }

// startListener runs listen on st's DSN for channels and returns the
// notification stream plus a counter of resyncs (one per connect).
func startListener(t *testing.T, st *Store, channels ...string) (<-chan notification, *atomic.Int32) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	got := make(chan notification, 64)
	var resyncs atomic.Int32
	ready := make(chan struct{}, 8)
	go listen(ctx, st.dsn, channels,
		func() { resyncs.Add(1); ready <- struct{}{} },
		func(channel, payload string) { got <- notification{channel, payload} })
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("listener never connected")
	}
	return got, &resyncs
}

func expectNotification(t *testing.T, got <-chan notification, channel, payload string) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case n := <-got:
			if n.channel == channel && n.payload == payload {
				return
			}
			// Other channels' traffic (e.g. a heartbeat) is fine to skip past.
		case <-deadline:
			t.Fatalf("no NOTIFY on %s with payload %q", channel, payload)
		}
	}
}

func TestEveryMutatorNotifiesItsChannel(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	base := en.BaseDomain
	got, _ := startListener(t, st, chanInstances, chanOwners, chanHostnames, chanEvents)

	stampInstance(t, st, "a", "127.0.0.1:1", time.Now())
	expectNotification(t, got, chanInstances, "a")

	if err := st.SetOwner(base, "a"); err != nil {
		t.Fatal(err)
	}
	expectNotification(t, got, chanOwners, base)
	if err := st.ClearOwner(base, "a"); err != nil {
		t.Fatal(err)
	}
	expectNotification(t, got, chanOwners, base)

	host, err := st.RegisterHostname(base, "blog", 0)
	if err != nil {
		t.Fatal(err)
	}
	expectNotification(t, got, chanHostnames, host)
	if err := st.DeregisterHostname(base, host); err != nil {
		t.Fatal(err)
	}
	expectNotification(t, got, chanHostnames, host)

	if err := st.AddCustomDomain(base, "shop.example.com"); err != nil {
		t.Fatal(err)
	}
	expectNotification(t, got, chanHostnames, "shop.example.com")
	if err := st.ConfirmCustomDomain(base, "shop.example.com"); err != nil {
		t.Fatal(err)
	}
	expectNotification(t, got, chanHostnames, "shop.example.com")
	if _, err := st.removeCustomDomainOwned(base, "shop.example.com"); err != nil {
		t.Fatal(err)
	}
	expectNotification(t, got, chanHostnames, "shop.example.com")

	if err := st.ParkEvent(base, "blog", "main", "push", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	expectNotification(t, got, chanEvents, base)

	if err := st.DeleteInstance("a"); err != nil {
		t.Fatal(err)
	}
	expectNotification(t, got, chanInstances, "a")
}

func TestReconcileNotifiesPrunedHostnames(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	host, err := st.RegisterHostname(en.BaseDomain, "old", 0)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := startListener(t, st, chanHostnames)
	if _, pruned, err := st.ReconcileHostnames(en.BaseDomain, nil); err != nil || len(pruned) != 1 {
		t.Fatalf("reconcile: pruned=%v err=%v", pruned, err)
	}
	expectNotification(t, got, chanHostnames, host)
}

func TestListenReconnectsAndResyncs(t *testing.T) {
	st := openTestStore(t)
	got, resyncs := startListener(t, st, "piper_test_reconnect")

	// Kill the listener's backend from another connection; the last query
	// it ran is its final LISTEN, which is how pg_stat_activity finds it.
	if _, err := st.db.Exec(
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE query = 'LISTEN piper_test_reconnect'`); err != nil {
		t.Fatal(err)
	}
	waitCond(t, 10*time.Second, "listener resynced after reconnect", func() bool { return resyncs.Load() >= 2 })

	if err := notify(st.db, "piper_test_reconnect", "after"); err != nil {
		t.Fatal(err)
	}
	expectNotification(t, got, "piper_test_reconnect", "after")
}
