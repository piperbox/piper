package relay

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/piperbox/piper/internal/tunnel"
)

func TestNewInstanceAdvertisesHostWithListenerPorts(t *testing.T) {
	inst, err := NewInstance("10.0.0.7", ":443", "0.0.0.0:80", "127.0.0.1:7000", ":8080")
	if err != nil {
		t.Fatal(err)
	}
	if inst.ID == "" || inst.StartedAt.IsZero() {
		t.Fatalf("identity not minted: %+v", inst)
	}
	want := map[string]string{"tls": "10.0.0.7:443", "http": "10.0.0.7:80", "tunnel": "10.0.0.7:7000", "api": "10.0.0.7:8080"}
	got := map[string]string{"tls": inst.TLSAddr, "http": inst.HTTPAddr, "tunnel": inst.TunnelAddr, "api": inst.APIAddr}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s addr = %q, want %q", k, got[k], w)
		}
	}
	other, _ := NewInstance("10.0.0.7", ":443", ":80", ":7000", ":8080")
	if other.ID == inst.ID {
		t.Fatal("two instances share an id")
	}
}

func TestNewInstanceRejectsAddrWithoutPort(t *testing.T) {
	if _, err := NewInstance("10.0.0.7", "443", ":80", ":7000", ":8080"); err == nil {
		t.Fatal("bare port accepted")
	}
}

func TestDefaultAdvertiseHostIsNonLoopbackIPv4(t *testing.T) {
	host, err := defaultAdvertiseHost()
	if err != nil {
		t.Skip("no non-loopback IPv4 on this machine:", err)
	}
	if host == "" || host == "127.0.0.1" {
		t.Fatalf("advertise host = %q", host)
	}
}

func TestHeartbeatPublishesSessionsAndLeavesOnStop(t *testing.T) {
	st := openTestStore(t)
	router := NewRouter()
	relaySess, _ := pipeSession(t, "box.public.getpiper.co")
	router.Register(relaySess)
	inst, err := NewInstance("127.0.0.1", ":443", ":80", ":7000", ":8080")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { inst.heartbeat(ctx, st, router); close(done) }()

	waitCond(t, 3*time.Second, "heartbeat row with one session", func() bool {
		rows, _ := st.LiveInstances()
		return len(rows) == 1 && rows[0].ID == inst.ID && rows[0].Sessions == 1 && rows[0].TLSAddr == "127.0.0.1:443"
	})
	cancel()
	<-done
	if rows, _ := st.LiveInstances(); len(rows) != 0 {
		t.Fatalf("row survived a clean stop: %+v", rows)
	}
}

// TestHeartbeatReassertsOwnershipAfterTheInstanceRowIsDeleted: an edge that
// found this relay undialable for one dial deletes its instance row, and the
// cascade takes every agent_owners row with it. Ownership is written at
// register, so without a re-assert the agents this relay still holds stay
// dark at :443/:80 until each one reconnects. The heartbeat has to put the
// rows back.
func TestHeartbeatReassertsOwnershipAfterTheInstanceRowIsDeleted(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	inst, err := NewInstance("127.0.0.1", ":443", ":80", ":7000", ":8080")
	if err != nil {
		t.Fatal(err)
	}
	router := NewRouter()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go inst.heartbeat(ctx, st, router)

	dialTestTunnel(t, st, router, inst, en)
	waitCond(t, 3*time.Second, "owner row written", func() bool {
		r, ok, _ := st.OwnerOf(en.BaseDomain)
		return ok && r.ID == inst.ID
	})

	if err := st.DeleteInstance(inst.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.OwnerOf(en.BaseDomain); ok {
		t.Fatal("cascade did not take the owner row")
	}
	waitCond(t, 3*time.Second, "ownership re-asserted", func() bool {
		r, ok, _ := st.OwnerOf(en.BaseDomain)
		return ok && r.ID == inst.ID
	})
}

// TestHeartbeatDoesNotStealOwnershipFromAnotherLiveInstance is the other half
// of the re-assert: a base owned by a different live relay means the agent
// moved there. RunInstance closes our stale session; the beat must not race it
// by claiming the row back.
func TestHeartbeatDoesNotStealOwnershipFromAnotherLiveInstance(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	inst, err := NewInstance("127.0.0.1", ":443", ":80", ":7000", ":8080")
	if err != nil {
		t.Fatal(err)
	}
	router := NewRouter()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go inst.heartbeat(ctx, st, router)

	dialTestTunnel(t, st, router, inst, en)
	waitCond(t, 3*time.Second, "owner row written", func() bool {
		r, ok, _ := st.OwnerOf(en.BaseDomain)
		return ok && r.ID == inst.ID
	})

	// The agent reconnected on another live relay while we still hold the
	// session (no RunInstance here to close it).
	other := testInstance(t, st)
	if err := st.SetOwner(en.BaseDomain, other.ID); err != nil {
		t.Fatal(err)
	}

	lastSeen := func() time.Time {
		var ts time.Time
		if err := st.db.QueryRow(`SELECT last_seen FROM relay_instances WHERE id=$1`, inst.ID).Scan(&ts); err != nil {
			t.Fatal(err)
		}
		return ts
	}
	// Three beats is proof the loop ran with the foreign owner in place.
	for i := 0; i < 3; i++ {
		was := lastSeen()
		waitCond(t, 3*time.Second, "heartbeat tick", func() bool { return lastSeen().After(was) })
	}
	if r, ok, _ := st.OwnerOf(en.BaseDomain); !ok || r.ID != other.ID {
		t.Fatalf("owner after three beats = %+v ok=%v, want %s", r, ok, other.ID)
	}
}

func TestRunInstanceClosesSessionOwnedElsewhere(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	mine, err := NewInstance("127.0.0.1", ":443", ":80", ":7000", ":8080")
	if err != nil {
		t.Fatal(err)
	}
	other := testInstance(t, st)
	router := NewRouter()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go RunInstance(ctx, st, mine, router, nil)
	waitCond(t, 3*time.Second, "instance live", func() bool {
		rows, _ := st.LiveInstances()
		for _, r := range rows {
			if r.ID == mine.ID {
				return true
			}
		}
		return false
	})

	dialTestTunnel(t, st, router, mine, en)
	waitCond(t, 3*time.Second, "owned by mine", func() bool {
		r, ok, _ := st.OwnerOf(en.BaseDomain)
		return ok && r.ID == mine.ID
	})
	// The agent reconnected on another live relay: our copy must go.
	if err := st.SetOwner(en.BaseDomain, other.ID); err != nil {
		t.Fatal(err)
	}
	waitCond(t, 3*time.Second, "stale session closed", func() bool {
		_, ok := router.Holds(en.BaseDomain)
		return !ok
	})
	if r, ok, _ := st.OwnerOf(en.BaseDomain); !ok || r.ID != other.ID {
		t.Fatalf("owner after stale close = %+v ok=%v, want %s", r, ok, other.ID)
	}
}

func TestRunInstanceDrainsParkedEventsOnNotify(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	inst, err := NewInstance("127.0.0.1", ":443", ":80", ":7000", ":8080")
	if err != nil {
		t.Fatal(err)
	}
	router := NewRouter()
	delivery := NewTunnelDelivery(st, router)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go RunInstance(ctx, st, inst, router, delivery)

	sess := dialTestTunnel(t, st, router, inst, en)
	bodies := make(chan string, 4)
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
			req, err := http.ReadRequest(bufio.NewReader(stream))
			if err != nil {
				stream.Close()
				return
			}
			body, _ := io.ReadAll(req.Body)
			bodies <- string(body)
			_, _ = io.WriteString(stream, "HTTP/1.1 202 Accepted\r\nContent-Length: 0\r\n\r\n")
			stream.Close()
		}
	}()

	// Parked by "another relay" (any store on the same database).
	if err := st.ParkEvent(en.BaseDomain, "blog", "main", "push", []byte(`{"after":"x"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-bodies:
		if got != `{"after":"x"}` {
			t.Fatalf("drained %s", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("owner never drained the parked event after NOTIFY")
	}
}

// Once marked, every heartbeat carries draining=true: the flag is how the
// edge learns to stop placing work here, so it must not depend on Drain's
// one explicit upsert alone.
func TestMarkDrainingFlagsEveryHeartbeat(t *testing.T) {
	st := openTestStore(t)
	router := NewRouter()
	inst, err := NewInstance("127.0.0.1", ":443", ":80", ":7000", ":8080")
	if err != nil {
		t.Fatal(err)
	}
	if inst.Draining() {
		t.Fatal("fresh instance reports draining")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { inst.heartbeat(ctx, st, router); close(done) }()
	waitCond(t, 3*time.Second, "first heartbeat, not draining", func() bool {
		rows, _ := st.LiveInstances()
		return len(rows) == 1 && !rows[0].Draining
	})
	inst.MarkDraining()
	if !inst.Draining() {
		t.Fatal("MarkDraining did not stick")
	}
	waitCond(t, 3*time.Second, "heartbeat carrying draining=true", func() bool {
		rows, _ := st.LiveInstances()
		return len(rows) == 1 && rows[0].Draining
	})
	cancel()
	<-done
}
