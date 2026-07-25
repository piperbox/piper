package relay

import (
	"testing"
	"time"
)

func TestParkEventCoalescesByRef(t *testing.T) {
	st := openTestStore(t)
	_, agent := enrolledAgent(t, st, "1001", "alice")

	if err := st.ParkEvent(agent, "blog", "main", "push", []byte(`{"after":"old"}`)); err != nil {
		t.Fatal(err)
	}
	if err := st.ParkEvent(agent, "blog", "main", "push", []byte(`{"after":"new"}`)); err != nil {
		t.Fatal(err)
	}

	got, err := st.DrainEvents(agent)
	if err != nil {
		t.Fatalf("DrainEvents: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d parked events, want 1 (coalesced)", len(got))
	}
	if string(got[0].Payload) != `{"after":"new"}` {
		t.Fatalf("payload = %s, want the newer one", got[0].Payload)
	}
	if got[0].App != "blog" || got[0].Ref != "main" || got[0].Event != "push" {
		t.Fatalf("event = %+v", got[0])
	}
}

func TestParkEventKeepsDistinctRefs(t *testing.T) {
	st := openTestStore(t)
	_, agent := enrolledAgent(t, st, "1001", "alice")

	if err := st.ParkEvent(agent, "blog", "main", "push", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := st.ParkEvent(agent, "blog", "pr-7", "pull_request", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	got, err := st.DrainEvents(agent)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
}

func TestDrainEventsEmptiesTheSlot(t *testing.T) {
	st := openTestStore(t)
	_, agent := enrolledAgent(t, st, "1001", "alice")
	if err := st.ParkEvent(agent, "blog", "main", "push", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DrainEvents(agent); err != nil {
		t.Fatal(err)
	}
	got, err := st.DrainEvents(agent)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("drain was not destructive: %+v", got)
	}
}

func TestParkEventCapsPerAgent(t *testing.T) {
	st := openTestStore(t)
	_, agent := enrolledAgent(t, st, "1001", "alice")

	for i := 0; i < maxPendingPerAgent+10; i++ {
		ref := "pr-" + itoa(i)
		if err := st.ParkEvent(agent, "blog", ref, "pull_request", []byte(`{}`)); err != nil {
			t.Fatalf("ParkEvent %d: %v", i, err)
		}
	}
	got, err := st.DrainEvents(agent)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != maxPendingPerAgent {
		t.Fatalf("parked %d events, want exactly %d", len(got), maxPendingPerAgent)
	}
	// Eviction must drop the OLDEST rows: survivors are pr-10..pr-59, in
	// ascending created_at order (DrainEvents orders by created_at).
	for i, ev := range got {
		want := "pr-" + itoa(i+10)
		if ev.Ref != want {
			t.Fatalf("survivor %d: ref = %s, want %s", i, ev.Ref, want)
		}
	}
}

// TestReparkEventDoesNotClobberNewerEvent pins the coalescing invariant a
// re-park must not break: a replay carrying a stale original created_at must
// lose the slot to a genuinely newer event already parked there.
func TestReparkEventDoesNotClobberNewerEvent(t *testing.T) {
	st := openTestStore(t)
	_, agent := enrolledAgent(t, st, "1001", "alice")

	if err := st.ParkEvent(agent, "blog", "main", "push", []byte(`{"after":"new"}`)); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().UTC().Add(-time.Hour).Format(pendingTimeLayout)
	if err := st.ReparkEvent(agent, "blog", "main", "push", []byte(`{"after":"old"}`), stale, 2); err != nil {
		t.Fatal(err)
	}

	got, err := st.DrainEvents(agent)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	if string(got[0].Payload) != `{"after":"new"}` {
		t.Fatalf("payload = %s, want the newer event to survive the stale re-park", got[0].Payload)
	}
}

// TestReparkEventEnforcesTheCap pins the concurrent-refill window ReparkEvent
// must not leave open: fill the table to the cap, "drain" it (simulating
// DrainFor), let fresh ParkEvent calls refill it to the cap under distinct
// refs while the drained batch is still in flight, then re-park the drained
// batch under yet more distinct refs. Without eviction on the re-park path,
// the table would end up far over the cap.
func TestReparkEventEnforcesTheCap(t *testing.T) {
	st := openTestStore(t)
	_, agent := enrolledAgent(t, st, "1001", "alice")

	drained := make([]PendingEvent, 0, maxPendingPerAgent)
	for i := 0; i < maxPendingPerAgent; i++ {
		ref := "drained-" + itoa(i)
		createdAt := time.Now().UTC().Format(pendingTimeLayout)
		if err := st.ParkEvent(agent, "blog", ref, "pull_request", []byte(`{}`)); err != nil {
			t.Fatalf("ParkEvent %d: %v", i, err)
		}
		drained = append(drained, PendingEvent{App: "blog", Ref: ref, Event: "pull_request", Payload: []byte(`{}`), CreatedAt: createdAt})
	}
	if _, err := st.DrainEvents(agent); err != nil { // empties the table, as DrainFor would before replaying
		t.Fatal(err)
	}

	for i := 0; i < maxPendingPerAgent; i++ {
		ref := "fresh-" + itoa(i)
		if err := st.ParkEvent(agent, "blog", ref, "push", []byte(`{}`)); err != nil {
			t.Fatalf("ParkEvent fresh %d: %v", i, err)
		}
	}

	for i, ev := range drained {
		if err := st.ReparkEvent(agent, ev.App, ev.Ref, ev.Event, ev.Payload, ev.CreatedAt, ev.Attempts+1); err != nil {
			t.Fatalf("ReparkEvent %d: %v", i, err)
		}
	}

	got, err := st.DrainEvents(agent)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != maxPendingPerAgent {
		t.Fatalf("parked %d events after re-park, want exactly %d (cap enforced)", len(got), maxPendingPerAgent)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
