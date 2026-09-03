package relay

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/piperbox/piper/internal/tunnel"
)

func TestDeliverySignsWithAgentSecretAndDropsGitHubs(t *testing.T) {
	sess, _, _, base, st, router := startTestRelay(t, nil, nil)

	secret, err := st.AgentWebhookSecret(base)
	if err != nil {
		t.Fatalf("AgentWebhookSecret: %v", err)
	}
	if secret == "" {
		t.Fatal("enrollment minted no webhook secret")
	}

	// Stand in for the box: accept the KindHTTP stream and answer 202.
	type got struct {
		host, sig, ghSig, event string
		body                    []byte
	}
	ch := make(chan got, 1)
	go func() {
		kind, conn, err := sess.AcceptKind()
		if err != nil || kind != tunnel.KindHTTP {
			return
		}
		defer conn.Close()
		req, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			return
		}
		body, _ := io.ReadAll(req.Body)
		ch <- got{
			host:  req.Host,
			sig:   req.Header.Get("X-Hub-Signature-256"),
			ghSig: req.Header.Get("X-Hub-Signature"),
			event: req.Header.Get("X-GitHub-Event"),
			body:  body,
		}
		_, _ = io.WriteString(conn, "HTTP/1.1 202 Accepted\r\nContent-Length: 0\r\n\r\n")
	}()

	d := NewTunnelDelivery(st, router)
	payload := []byte(`{"ref":"refs/heads/main"}`)
	b := Binding{AgentName: base, App: "blog", Repo: "alice/blog", Branch: "main"}
	if err := d.Deliver(context.Background(), b, "push", payload); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	select {
	case g := <-ch:
		if g.host != "hooks."+base {
			t.Fatalf("Host = %q, want hooks.%s", g.host, base)
		}
		if g.event != "push" {
			t.Fatalf("event = %q", g.event)
		}
		if string(g.body) != string(payload) {
			t.Fatalf("body = %q", g.body)
		}
		m := hmac.New(sha256.New, []byte(secret))
		m.Write(payload)
		want := "sha256=" + hex.EncodeToString(m.Sum(nil))
		if g.sig != want {
			t.Fatalf("signature = %q, want %q", g.sig, want)
		}
		if g.ghSig != "" {
			t.Fatal("GitHub's original signature was forwarded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no request arrived on the tunnel")
	}
}

func TestDeliveryOfflineAgent(t *testing.T) {
	st := openTestStore(t)
	_, base := enrolledAgent(t, st, "1001", "alice")
	d := NewTunnelDelivery(st, NewRouter())

	err := d.Deliver(context.Background(), Binding{AgentName: base, App: "blog"}, "push", []byte(`{}`))
	if !errors.Is(err, ErrAgentOffline) {
		t.Fatalf("err = %v, want ErrAgentOffline", err)
	}
}

func TestDrainForReplaysOnlyTheNewestPerRef(t *testing.T) {
	sess, _, _, base, st, router := startTestRelay(t, nil, nil)

	if err := st.ParkEvent(base, "blog", "main", "push", []byte(`{"after":"old"}`)); err != nil {
		t.Fatal(err)
	}
	if err := st.ParkEvent(base, "blog", "main", "push", []byte(`{"after":"new"}`)); err != nil {
		t.Fatal(err)
	}

	bodies := make(chan string, 4)
	go func() {
		for {
			kind, conn, err := sess.AcceptKind()
			if err != nil {
				return
			}
			if kind != tunnel.KindHTTP {
				conn.Close()
				continue
			}
			req, err := http.ReadRequest(bufio.NewReader(conn))
			if err != nil {
				conn.Close()
				return
			}
			body, _ := io.ReadAll(req.Body)
			bodies <- string(body)
			_, _ = io.WriteString(conn, "HTTP/1.1 202 Accepted\r\nContent-Length: 0\r\n\r\n")
			conn.Close()
		}
	}()

	NewTunnelDelivery(st, router).DrainFor(context.Background(), base)

	select {
	case got := <-bodies:
		if got != `{"after":"new"}` {
			t.Fatalf("replayed %s, want the newer commit", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no replay arrived")
	}
	select {
	case extra := <-bodies:
		t.Fatalf("a second replay arrived: %s", extra)
	case <-time.After(300 * time.Millisecond):
	}

	left, err := st.DrainEvents(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("%d events still parked after drain", len(left))
	}
}

// TestDrainForBailsWhileOffline pins the bail at the top of DrainFor: it must
// never reach the store while the agent has no live session. DrainEvents is
// destructive — delete then re-insert — so a drain-and-re-park round trip
// necessarily changes the parked row's rowid; an unchanged rowid after
// DrainFor is therefore proof the bail fired and the store was never
// touched. A decoy row parked for a second agent AFTER base's, and never
// touched by DrainFor(base), pins the comparison: SQLite assigns a deleted
// row's replacement the table's current max rowid plus one, so a decoy with
// a higher rowid than base's original guarantees any drain-and-re-park
// round trip lands base's row on a strictly larger rowid than before (parking
// the decoy first would let the reinsert innocuously reclaim base's exact
// original number and mask the very mutation this test exists to catch).
func TestDrainForBailsWhileOffline(t *testing.T) {
	st := openTestStore(t)
	_, base := enrolledAgent(t, st, "1001", "alice")
	_, other := enrolledAgent(t, st, "1002", "bob")
	router := NewRouter() // no session registered: base is offline

	if err := st.ParkEvent(base, "blog", "main", "push", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := st.ParkEvent(other, "blog", "main", "push", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	before := pendingRowID(t, st, base, "blog", "main")

	NewTunnelDelivery(st, router).DrainFor(context.Background(), base)

	after := pendingRowID(t, st, base, "blog", "main")
	if before != after {
		t.Fatalf("rowid changed %d -> %d: DrainFor touched the store while offline", before, after)
	}
}

func pendingRowID(t *testing.T, st *Store, agentName, app, ref string) int64 {
	t.Helper()
	var id int64
	if err := st.db.QueryRow(
		`SELECT id FROM pending_events WHERE agent_name=$1 AND app=$2 AND ref=$3`,
		agentName, app, ref).Scan(&id); err != nil {
		t.Fatalf("query rowid: %v", err)
	}
	return id
}

// TestDrainForReparksFailedReplay proves a replay that fails is re-parked,
// not dropped: GitHub already got its 202 for the original delivery, so a
// silently lost event here would never be retried by anyone.
func TestDrainForReparksFailedReplay(t *testing.T) {
	sess, _, _, base, st, router := startTestRelay(t, nil, nil)

	if err := st.ParkEvent(base, "blog", "main", "push", []byte(`{"after":"x"}`)); err != nil {
		t.Fatal(err)
	}

	go func() {
		kind, conn, err := sess.AcceptKind()
		if err != nil || kind != tunnel.KindHTTP {
			return
		}
		defer conn.Close()
		req, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, req.Body)
		_, _ = io.WriteString(conn, "HTTP/1.1 500 Internal Server Error\r\nContent-Length: 0\r\n\r\n")
	}()

	NewTunnelDelivery(st, router).DrainFor(context.Background(), base)

	got, err := st.DrainEvents(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events after failed replay, want 1 (re-parked, not dropped)", len(got))
	}
	if string(got[0].Payload) != `{"after":"x"}` {
		t.Fatalf("payload = %s, want the original", got[0].Payload)
	}
	// The re-park carries the failure forward so the sweep can back off (#294).
	if got[0].Attempts != 2 {
		t.Fatalf("attempts = %d, want 2 (parked once, replay failed once)", got[0].Attempts)
	}
}

// boxReturning answers count tunnel requests with status, so a delivery can be
// made to fail deterministically. It reports how many it served.
func boxReturning(t *testing.T, sess *tunnel.Session, status string, count int) func() int {
	t.Helper()
	served := make(chan struct{}, count+8)
	go func() {
		for i := 0; i < count; i++ {
			kind, conn, err := sess.AcceptKind()
			if err != nil || kind != tunnel.KindHTTP {
				return
			}
			req, err := http.ReadRequest(bufio.NewReader(conn))
			if err != nil {
				conn.Close()
				return
			}
			_, _ = io.Copy(io.Discard, req.Body)
			_, _ = io.WriteString(conn, "HTTP/1.1 "+status+"\r\nContent-Length: 0\r\n\r\n")
			conn.Close()
			served <- struct{}{}
		}
	}()
	return func() int { return len(served) }
}

// The failure #294 describes: the box is up and its tunnel is registered, but
// it is rejecting deliveries. The event parks, the box never disconnects (so no
// reconnect drain), and no further webhook arrives for it (so no ingress
// re-drain). Nothing retried it — the box sat on an undelivered tip commit
// forever. The sweep is what closes that hole.
func TestSweepRetriesParkedEventForAConnectedBox(t *testing.T) {
	sess, _, _, base, st, router := startTestRelay(t, nil, nil)
	served := boxReturning(t, sess, "202 Accepted", 1)

	if err := st.ParkEvent(base, "blog", "main", "push", []byte(`{"after":"x"}`)); err != nil {
		t.Fatal(err)
	}
	// A fresh park is not due yet; make it due, as the backoff would in time.
	if err := st.setNextTryForTest(base, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	d := NewTunnelDelivery(st, router)
	d.sweep()
	waitFor(t, "sweep to deliver the parked event", func() bool { return served() == 1 })

	d.Shutdown(context.Background())
	left, err := st.DrainEvents(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("%d events still parked after a successful sweep, want 0", len(left))
	}
}

// The sweep must not consume a slot that is still backing off: draining it
// would deliver early, and re-parking it would reset nothing but churn.
func TestSweepSkipsEventsStillBackingOff(t *testing.T) {
	sess, _, _, base, st, router := startTestRelay(t, nil, nil)
	served := boxReturning(t, sess, "202 Accepted", 1)

	// A fresh park is due one backoff period out, so the sweep must pass.
	if err := st.ParkEvent(base, "blog", "main", "push", []byte(`{"after":"x"}`)); err != nil {
		t.Fatal(err)
	}

	d := NewTunnelDelivery(st, router)
	d.sweep()
	d.Shutdown(context.Background())

	if served() != 0 {
		t.Fatalf("sweep delivered %d events that were still backing off, want 0", served())
	}
	left, err := st.DrainEvents(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 {
		t.Fatalf("%d events parked, want the backing-off one left alone", len(left))
	}
}

// A box that is permanently rejecting deliveries must back off rather than be
// retried at sweep frequency forever: each attempt costs a stream and up to
// deliveryTimeout.
func TestRepeatedFailuresBackOff(t *testing.T) {
	if got, want := retryDelay(1), retryBackoffBase; got != want {
		t.Errorf("retryDelay(1) = %v, want %v", got, want)
	}
	if got, want := retryDelay(3), 4*retryBackoffBase; got != want {
		t.Errorf("retryDelay(3) = %v, want %v", got, want)
	}
	if got := retryDelay(50); got != retryBackoffMax {
		t.Errorf("retryDelay(50) = %v, want the cap %v", got, retryBackoffMax)
	}

	sess, _, _, base, st, router := startTestRelay(t, nil, nil)
	boxReturning(t, sess, "500 Internal Server Error", 4)
	if err := st.ParkEvent(base, "blog", "main", "push", []byte(`{"after":"x"}`)); err != nil {
		t.Fatal(err)
	}

	d := NewTunnelDelivery(st, router)
	// Force each round due, so only the recorded attempt count grows.
	for i := 0; i < 3; i++ {
		if err := st.setNextTryForTest(base, time.Now().Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		d.drain(context.Background(), base, true)
	}
	d.Shutdown(context.Background())

	got, err := st.DrainEvents(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events, want the still-undelivered one", len(got))
	}
	if got[0].Attempts != 4 {
		t.Fatalf("attempts = %d, want 4 (one park + three failed replays)", got[0].Attempts)
	}
}

// A box that never comes back would otherwise hold its capped slots forever,
// and a replay of a long-dead push is not something anyone wants deployed.
func TestExpiredPendingEventsArePurgedAndNeverReplayed(t *testing.T) {
	st := openTestStore(t)
	_, base := enrolledAgent(t, st, "1001", "alice")

	if err := st.ParkEvent(base, "blog", "main", "push", []byte(`{"after":"old"}`)); err != nil {
		t.Fatal(err)
	}
	if err := st.ParkEvent(base, "blog", "dev", "push", []byte(`{"after":"new"}`)); err != nil {
		t.Fatal(err)
	}
	// Age the "main" slot past the TTL.
	if err := st.setCreatedAtForTest(base, "main", time.Now().Add(-pendingEventTTL-time.Hour)); err != nil {
		t.Fatal(err)
	}

	// A drain must not hand back the stale one even before a purge runs.
	got, err := st.DrainEvents(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Ref != "dev" {
		t.Fatalf("drain returned %+v, want only the unexpired dev event", got)
	}

	// And the purge reclaims the slots of a box that never drains at all.
	if err := st.ParkEvent(base, "blog", "main", "push", []byte(`{"after":"old"}`)); err != nil {
		t.Fatal(err)
	}
	if err := st.setCreatedAtForTest(base, "main", time.Now().Add(-pendingEventTTL-time.Hour)); err != nil {
		t.Fatal(err)
	}
	n, err := st.PurgeExpiredPending()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("purged %d rows, want 1", n)
	}
}

// Deliveries must not outnumber the pool: one webhook fans out to every
// binding for the repo, and a burst of pushes multiplies that (#295).
func TestDispatchBoundsConcurrency(t *testing.T) {
	st := openTestStore(t)
	d := NewTunnelDelivery(st, NewRouter())

	var mu sync.Mutex
	inFlight, peak := 0, 0
	release := make(chan struct{})
	for i := 0; i < maxConcurrentDeliveries*3; i++ {
		d.Dispatch(func(context.Context) {
			mu.Lock()
			inFlight++
			if inFlight > peak {
				peak = inFlight
			}
			mu.Unlock()
			<-release
			mu.Lock()
			inFlight--
			mu.Unlock()
		})
	}
	// Let the pool fill, then let everything through.
	waitFor(t, "the pool to saturate", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return peak >= maxConcurrentDeliveries
	})
	close(release)
	d.Shutdown(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if peak > maxConcurrentDeliveries {
		t.Fatalf("peak concurrency = %d, want at most %d", peak, maxConcurrentDeliveries)
	}
}

// A relay restart mid-burst used to drop every in-flight delivery: GitHub had
// already been sent its 202, so nothing would ever retry them. Shutdown must
// wait for each dispatched unit to finish parking instead (#295).
func TestShutdownWaitsForInFlightDeliveriesToPark(t *testing.T) {
	st := openTestStore(t)
	_, base := enrolledAgent(t, st, "1001", "alice")
	d := NewTunnelDelivery(st, NewRouter())

	started := make(chan struct{})
	d.Dispatch(func(ctx context.Context) {
		close(started)
		<-ctx.Done() // still running when Shutdown is called
		if err := st.ParkEvent(base, "blog", "main", "push", []byte(`{"after":"x"}`)); err != nil {
			t.Errorf("park during shutdown: %v", err)
		}
	})
	<-started

	d.Shutdown(context.Background())

	// Shutdown returned, so the park must already be durable.
	got, err := st.DrainEvents(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("%d events parked after shutdown, want 1 — the in-flight one was dropped", len(got))
	}
}

// Work handed to a shut-down pool must still park rather than vanish.
func TestDispatchAfterShutdownStillRuns(t *testing.T) {
	st := openTestStore(t)
	d := NewTunnelDelivery(st, NewRouter())
	d.Shutdown(context.Background())

	ran := false
	d.Dispatch(func(ctx context.Context) {
		ran = true
		if ctx.Err() == nil {
			t.Error("a post-shutdown dispatch should get a cancelled context")
		}
	})
	if !ran {
		t.Fatal("dispatch after shutdown dropped the work instead of running it")
	}
}

// setNextTryForTest makes every parked event for agentName due at t, standing
// in for the passage of a real backoff period.
func (s *Store) setNextTryForTest(agentName string, at time.Time) error {
	_, err := s.db.Exec(`UPDATE pending_events SET next_try_at=$1 WHERE agent_name=$2`,
		at.UTC().Format(pendingTimeLayout), agentName)
	return err
}

// setCreatedAtForTest ages one parked slot, standing in for the passage of the
// TTL.
func (s *Store) setCreatedAtForTest(agentName, ref string, at time.Time) error {
	_, err := s.db.Exec(`UPDATE pending_events SET created_at=$1 WHERE agent_name=$2 AND ref=$3`,
		at.UTC().Format(pendingTimeLayout), agentName, ref)
	return err
}

// waitFor polls cond until it holds or ~3s passes.
func waitFor(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", desc)
}
