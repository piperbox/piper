package relay

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/piperbox/piper/internal/tunnel"
)

// drainFixture is a relay accept loop on loopback with one enrolled agent:
// the shape Drain sees in production, minus the edge.
type drainFixture struct {
	st     *Store
	inst   *Instance
	router *Router
	en     Enrollment
	addr   string
}

func newDrainFixture(t *testing.T) *drainFixture {
	t.Helper()
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f := &drainFixture{st: st, inst: testInstance(t, st), router: NewRouter(), en: en, addr: ln.Addr().String()}
	go acceptTunnels(ln, st, f.router, nil, nil, nil, f.inst)
	return f
}

// connect dials one agent session and waits until the relay holds it.
func (f *drainFixture) connect(t *testing.T) *tunnel.Session {
	t.Helper()
	conn, err := net.Dial("tcp", f.addr)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := tunnel.Dial(conn, f.en.Token, f.en.BaseDomain)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	waitCond(t, 3*time.Second, "relay registered the session", func() bool {
		_, ok := f.router.Holds(f.en.BaseDomain)
		return ok
	})
	return sess
}

// openStream has the agent open a control stream and send nothing: the
// relay's handleControl sits in ReadMsg, so the stream stays open until the
// agent closes it — a stand-in for any in-flight splice.
func (f *drainFixture) openStream(t *testing.T, agent *tunnel.Session) net.Conn {
	t.Helper()
	relaySess, ok := f.router.Holds(f.en.BaseDomain)
	if !ok {
		t.Fatal("relay does not hold the session")
	}
	stream, err := agent.OpenKind(tunnel.KindControl)
	if err != nil {
		t.Fatal(err)
	}
	waitCond(t, 3*time.Second, "relay sees the open stream", func() bool { return relaySess.NumStreams() == 1 })
	return stream
}

func waitClosed(t *testing.T, sess *tunnel.Session, what string) {
	t.Helper()
	select {
	case <-sess.CloseChan():
	case <-time.After(3 * time.Second):
		t.Fatalf("%s never closed", what)
	}
}

// An idle session goes at once — the agent redials as early as it can —
// and the pool row says draining before Drain returns.
func TestDrainClosesIdleSessionsAndMarksTheRow(t *testing.T) {
	f := newDrainFixture(t)
	agent := f.connect(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	forced := Drain(ctx, f.st, f.inst, f.router)
	if forced != 0 {
		t.Fatalf("forced = %d, want 0 for an idle session", forced)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("Drain took %s on an idle session; it should not wait", time.Since(start))
	}
	waitClosed(t, agent, "agent side of an idle session")
	if agents, _, _ := f.router.Counts(); agents != 0 {
		t.Fatalf("router still holds %d agents", agents)
	}
	if !f.inst.Draining() {
		t.Fatal("instance not marked draining")
	}
	rows, err := f.st.LiveInstances()
	if err != nil || len(rows) != 1 || !rows[0].Draining {
		t.Fatalf("pool row = %+v err=%v, want one live row with draining=true", rows, err)
	}
}

// A session with a stream in flight lives until the stream ends; Drain
// returns only then, and reports nothing forced.
func TestDrainWaitsForOpenStreamsThenCloses(t *testing.T) {
	f := newDrainFixture(t)
	agent := f.connect(t)
	stream := f.openStream(t, agent)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- Drain(ctx, f.st, f.inst, f.router) }()

	select {
	case forced := <-done:
		t.Fatalf("Drain returned (forced=%d) while a stream was open", forced)
	case <-time.After(5 * drainTick):
	}
	if _, held := f.router.Holds(f.en.BaseDomain); !held {
		t.Fatal("session dropped while its stream was open")
	}

	stream.Close()
	select {
	case forced := <-done:
		if forced != 0 {
			t.Fatalf("forced = %d, want 0: the stream closed before the deadline", forced)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Drain did not return after the last stream closed")
	}
	waitClosed(t, agent, "agent side after its stream ended")
}

// A stream that never ends is cut at the deadline, and Drain says so.
func TestDrainForcesStuckSessionsAtTheDeadline(t *testing.T) {
	f := newDrainFixture(t)
	agent := f.connect(t)
	stream := f.openStream(t, agent)
	defer stream.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	forced := Drain(ctx, f.st, f.inst, f.router)
	if forced != 1 {
		t.Fatalf("forced = %d, want 1", forced)
	}
	waitClosed(t, agent, "agent side after the forced close")
	waitCond(t, 3*time.Second, "router empty after the forced close", func() bool {
		agents, _, _ := f.router.Counts()
		return agents == 0
	})
}

// Drain with nothing held returns at once.
func TestDrainWithNoSessionsReturnsImmediately(t *testing.T) {
	f := newDrainFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if forced := Drain(ctx, f.st, f.inst, f.router); forced != 0 {
		t.Fatalf("forced = %d, want 0", forced)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("Drain took %s with nothing to drain", time.Since(start))
	}
}
