# Relay Graceful Drain Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** On SIGTERM a `piper-relay` marks itself draining so edges place nothing new on it, refuses new tunnels, closes each agent session once it has no open streams (bounded by a 20 s deadline), and only then deletes its pool row and exits.

**Architecture:** One new `draining` column on `relay_instances`, carried by every heartbeat and read by the edge's placement rules. A new `relay.Drain(ctx, st, inst, router)` owns the wait loop; `cmd/piper-relay` calls it before cancelling the pool context. Two small exports (`tunnel.Session.NumStreams`, `Router.Sessions`) make the wait observable. No agent change. Spec: [`docs/superpowers/specs/2026-09-05-relay-graceful-drain-design.md`](../specs/2026-09-05-relay-graceful-drain-design.md).

**Tech Stack:** Go, Postgres (`internal/relay` tests via `openTestStore`, which needs Docker or `PIPER_TEST_POSTGRES_URL`; a skip is not a pass), hashicorp/yamux v0.1.2.

## Global Constraints

- Module `github.com/piperbox/piper`; `CGO_ENABLED=0` everywhere; no cgo.
- Pre-1.x: `schema.sql` is the complete current shape, edited in place; no migrations, no legacy readers.
- Layering: `tunnel` knows nothing of `relay`; `relay` never imports `cmd`.
- Test-first: every step below writes the failing test, runs it red, then implements.
- Relay tests use `openTestStore(t)` (Postgres). Run them with `go test ./internal/relay -run <Name> -v` and read the output: `--- SKIP` means Postgres is missing, not that the test passed.
- One commit per task, conventional-commit style, ending with the trailer `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`. Reference `#523` in each message.
- `make verify` (gofmt → vet → test → cross) must exit 0 before the branch is pushed. Judge it by exit status, not by grepping output.
- Branch: `ozykhan/relay-drain`, off `main`. PRs #528 and #529 are open and touch `internal/relay/edge_state.go` and `edge.go`; see the note in Task 3.
- Match the surrounding package's style. Do not touch code a task does not name.

---

### Task 1: `relay_instances.draining` column and store round-trip

**Files:**
- Modify: `internal/relay/schema.sql:148-157`
- Modify: `internal/relay/ownership.go:28-36` (InstanceRow), `:52-62` (UpsertInstance), `:80-86` (instanceCols/scanInstance), `:143-157` (OwnerOf)
- Test: `internal/relay/ownership_test.go`

**Interfaces:**
- Consumes: `stampInstance(t, st, id, addr, started) *Instance`, `enrollTestAgent(t, st) Enrollment`, `openTestStore(t)` from the test package; `Instance.row(sessions int) InstanceRow`.
- Produces: `InstanceRow.Draining bool`, written by `UpsertInstance`, read by `LiveInstances` and `OwnerOf`.

- [ ] **Step 1: Write the failing test**

Append to `internal/relay/ownership_test.go`:

```go
// A draining relay tells the edge so through its pool row: the flag has to
// survive the insert, the heartbeat's ON CONFLICT update, and the owner
// lookup the edge uses for :443/:80.
func TestUpsertInstanceRoundTripsDraining(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	inst := stampInstance(t, st, "a", "127.0.0.1:1", time.Now())
	if err := st.SetOwner(en.BaseDomain, "a"); err != nil {
		t.Fatal(err)
	}
	rows, err := st.LiveInstances()
	if err != nil || len(rows) != 1 || rows[0].Draining {
		t.Fatalf("fresh row: %+v err=%v, want one live row that is not draining", rows, err)
	}
	row := inst.row(0)
	row.Draining = true
	if err := st.UpsertInstance(row); err != nil {
		t.Fatal(err)
	}
	rows, _ = st.LiveInstances()
	if len(rows) != 1 || !rows[0].Draining {
		t.Fatalf("after a draining upsert: %+v, want Draining=true", rows)
	}
	owner, ok, err := st.OwnerOf(en.BaseDomain)
	if err != nil || !ok || !owner.Draining {
		t.Fatalf("OwnerOf = %+v ok=%v err=%v, want the draining owner", owner, ok, err)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/relay -run TestUpsertInstanceRoundTripsDraining -v`
Expected: compile error `row.Draining undefined` (or `rows[0].Draining undefined`).

- [ ] **Step 3: Add the column and the field**

In `internal/relay/schema.sql`, change the `relay_instances` table to:

```sql
CREATE TABLE IF NOT EXISTS relay_instances (
    id          TEXT PRIMARY KEY,
    started_at  TIMESTAMPTZ NOT NULL,
    last_seen   TIMESTAMPTZ NOT NULL,
    sessions    INTEGER NOT NULL DEFAULT 0,
    tls_addr    TEXT NOT NULL,
    http_addr   TEXT NOT NULL,
    tunnel_addr TEXT NOT NULL,
    api_addr    TEXT NOT NULL,
    -- Set for the rest of the process's life once it receives SIGTERM (#523):
    -- edges place no new tunnels or api.<apex> connections here, but keep
    -- routing the hostnames it owns until their sessions close.
    draining    BOOLEAN NOT NULL DEFAULT false
);
```

In `internal/relay/ownership.go`, add the field to `InstanceRow`:

```go
type InstanceRow struct {
	ID         string
	StartedAt  time.Time
	Sessions   int
	TLSAddr    string
	HTTPAddr   string
	TunnelAddr string
	APIAddr    string
	Draining   bool
}
```

Change `UpsertInstance`'s statement to:

```go
	if _, err := s.db.Exec(
		`INSERT INTO relay_instances(id, started_at, last_seen, sessions, tls_addr, http_addr, tunnel_addr, api_addr, draining)
		 VALUES($1, $2, now(), $3, $4, $5, $6, $7, $8)
		 ON CONFLICT(id) DO UPDATE SET last_seen = now(), sessions = excluded.sessions, draining = excluded.draining`,
		r.ID, r.StartedAt, r.Sessions, r.TLSAddr, r.HTTPAddr, r.TunnelAddr, r.APIAddr, r.Draining); err != nil {
		return err
	}
```

Change `instanceCols` and `scanInstance` to:

```go
const instanceCols = `id, started_at, sessions, tls_addr, http_addr, tunnel_addr, api_addr, draining`

func scanInstance(sc interface{ Scan(...any) error }) (InstanceRow, error) {
	var r InstanceRow
	err := sc.Scan(&r.ID, &r.StartedAt, &r.Sessions, &r.TLSAddr, &r.HTTPAddr, &r.TunnelAddr, &r.APIAddr, &r.Draining)
	return r, err
}
```

In `OwnerOf`, change the select list to include the column:

```go
		`SELECT i.id, i.started_at, i.sessions, i.tls_addr, i.http_addr, i.tunnel_addr, i.api_addr, i.draining
		   FROM agent_owners o
```

- [ ] **Step 4: Run the test and the package**

Run: `go test ./internal/relay -run 'TestUpsertInstanceRoundTripsDraining|TestLiveInstances|TestOwnerOf|TestHeartbeat' -v`
Expected: all PASS (no SKIP).

- [ ] **Step 5: Commit**

```bash
git add internal/relay/schema.sql internal/relay/ownership.go internal/relay/ownership_test.go
git commit -m "feat(relay): relay_instances.draining column round-trips through the store

Part of #523.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 2: `Instance` carries the drain flag on every heartbeat

**Files:**
- Modify: `internal/relay/instance.go:19-27` (struct), `:78-82` (row)
- Test: `internal/relay/instance_test.go`

**Interfaces:**
- Consumes: `InstanceRow.Draining` (Task 1); `waitCond(t, timeout, desc, pred)` and the 20 ms `heartbeatInterval` set in `TestMain` (`watchdog_test.go`).
- Produces: `(*Instance).MarkDraining()`, `(*Instance).Draining() bool`; `row()` sets `Draining`.

- [ ] **Step 1: Write the failing test**

Append to `internal/relay/instance_test.go`:

```go
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
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/relay -run TestMarkDrainingFlagsEveryHeartbeat -v`
Expected: compile error `inst.Draining undefined`.

- [ ] **Step 3: Implement**

In `internal/relay/instance.go`, add `"sync/atomic"` to the imports and change the struct and `row`:

```go
// Instance is this relay process's identity in the pool: a random id per
// start plus the addresses an edge dials to reach each of its listeners.
type Instance struct {
	ID         string
	StartedAt  time.Time
	TLSAddr    string
	HTTPAddr   string
	TunnelAddr string
	APIAddr    string
	// draining is set once, on SIGTERM, and never cleared: from then on the
	// heartbeat row says so and acceptTunnels refuses new sessions (#523).
	draining atomic.Bool
}

// MarkDraining flips this instance into its final state: every heartbeat
// from now on carries draining=true and acceptTunnels refuses new sessions.
func (i *Instance) MarkDraining() { i.draining.Store(true) }

// Draining reports whether MarkDraining has been called.
func (i *Instance) Draining() bool { return i.draining.Load() }

// row is the heartbeat payload: the identity plus the current session count
// and the drain flag.
func (i *Instance) row(sessions int) InstanceRow {
	return InstanceRow{ID: i.ID, StartedAt: i.StartedAt, Sessions: sessions,
		TLSAddr: i.TLSAddr, HTTPAddr: i.HTTPAddr, TunnelAddr: i.TunnelAddr, APIAddr: i.APIAddr,
		Draining: i.draining.Load()}
}
```

`atomic.Bool` makes `Instance` non-copyable under `go vet`'s copylocks check. Every existing use is by pointer (`NewInstance` returns `*Instance`; `stampInstance` builds `&Instance{...}`); confirm with `go vet ./internal/relay/ ./cmd/piper-relay/` in the next step.

- [ ] **Step 4: Run the test, vet, and the heartbeat tests**

Run: `go vet ./internal/relay/ ./cmd/piper-relay/ && go test ./internal/relay -run 'TestMarkDrainingFlagsEveryHeartbeat|TestHeartbeat|TestNewInstance' -v`
Expected: vet clean, all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/relay/instance.go internal/relay/instance_test.go
git commit -m "feat(relay): Instance.MarkDraining rides every heartbeat

Part of #523.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 3: Edge placement skips draining relays

**Files:**
- Modify: `internal/relay/edge_state.go:137-150` (pickLocked)
- Test: `internal/relay/edge_state_test.go`

**Interfaces:**
- Consumes: `InstanceRow.Draining` (Task 1); `instRow(id, started, sessions)` test helper; `newEdgeState()`, `setInstances`, `setOwners`, `pickTunnel(exclude)`, `pickAPI()`, `ownerOf(agent)`.
- Produces: `pickTunnel` and `pickAPI` never return a draining row; `ownerOf` is unchanged.

> **Rebase note.** On `main` today `pickAPI` is `return s.pickLocked(earlier, nil)`, so the filter in `pickLocked` covers both pickers. PR #529 rewrites `pickAPI` as a round-robin that builds its own `live` slice. When this branch is rebased over #529, add `|| r.Draining` to that slice's filter too (`for _, r := range s.instances { if r.Draining { continue } ... }`) and keep this task's test, which then guards both paths.

- [ ] **Step 1: Write the failing test**

Append to `internal/relay/edge_state_test.go`:

```go
// A draining relay must get nothing new — no tunnel placement however idle
// it looks (its session count is falling), no api.<apex> connection — but it
// still owns the agents it holds, so :443/:80 keep routing to it until each
// session closes.
func TestPlacementSkipsDrainingInstances(t *testing.T) {
	t0 := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	s := newEdgeState()
	a := instRow("a", t0, 0)
	a.Draining = true
	s.setInstances([]InstanceRow{a, instRow("b", t0.Add(time.Minute), 7)})
	s.setOwners(map[string]string{"x.example": "a"})

	if got, ok := s.pickTunnel(nil); !ok || got.ID != "b" {
		t.Fatalf("pickTunnel = %+v ok=%v, want b (a is draining, however idle and early)", got, ok)
	}
	if got, ok := s.pickAPI(); !ok || got.ID != "b" {
		t.Fatalf("pickAPI = %+v ok=%v, want b (a is draining)", got, ok)
	}
	if got, ok := s.ownerOf("x.example"); !ok || got.ID != "a" {
		t.Fatalf("ownerOf x = %+v ok=%v, want the draining owner a", got, ok)
	}

	b := instRow("b", t0.Add(time.Minute), 7)
	b.Draining = true
	s.setInstances([]InstanceRow{a, b})
	if got, ok := s.pickTunnel(nil); ok {
		t.Fatalf("pickTunnel = %+v on a pool that is all draining, want no candidate", got)
	}
	if got, ok := s.pickAPI(); ok {
		t.Fatalf("pickAPI = %+v on a pool that is all draining, want no candidate", got)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/relay -run TestPlacementSkipsDrainingInstances -v`
Expected: FAIL with `pickTunnel = {ID:a ...}, want b`.

- [ ] **Step 3: Implement**

In `internal/relay/edge_state.go`, change `pickLocked`:

```go
// pickLocked is the shared placement scan. A draining instance is never a
// candidate (#523): it is on its way out and refuses new tunnels anyway.
// ownerOf does not go through here on purpose — a draining relay still
// holds its agents' sessions and must keep receiving their traffic.
func (s *edgeState) pickLocked(less func(a, b InstanceRow) bool, exclude map[string]bool) (InstanceRow, bool) {
	var best InstanceRow
	found := false
	for _, r := range s.instances {
		if exclude[r.ID] || r.Draining {
			continue
		}
		if !found || less(r, best) {
			best, found = r, true
		}
	}
	return best, found
}
```

- [ ] **Step 4: Run the edge tests**

Run: `go test ./internal/relay -run 'TestPlacementSkipsDrainingInstances|TestPick|TestOwnerOf|TestEdge' -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/relay/edge_state.go internal/relay/edge_state_test.go
git commit -m "feat(relay): piper-edge places nothing on a draining relay

Part of #523.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 4: `tunnel.Session.NumStreams` and `Router.Sessions`

**Files:**
- Modify: `internal/tunnel/tunnel.go:50-53`
- Modify: `internal/relay/router.go:136-149` (after Bases)
- Test: `internal/tunnel/tunnel_test.go`, `internal/relay/router_test.go`

**Interfaces:**
- Consumes: `yamux.Session.NumStreams() int`; `pipeSession(t, base) (relaySide, agentSide *tunnel.Session)` from `proxy_test.go`.
- Produces: `(*tunnel.Session).NumStreams() int`; `(*Router).Sessions() []*tunnel.Session`.

- [ ] **Step 1: Write the failing tunnel test**

Append to `internal/tunnel/tunnel_test.go`:

```go
// NumStreams is what a draining relay waits on: it must see a stream the peer
// opened, and fall back to zero once both ends have closed it.
func TestNumStreamsFollowsOpenAndClose(t *testing.T) {
	c1, c2 := net.Pipe()
	t.Cleanup(func() { c1.Close(); c2.Close() })
	srvSess := make(chan *Session, 1)
	go func() {
		s, err := Serve(c2, func(_, _ string) error { return nil })
		if err != nil {
			t.Errorf("Serve: %v", err)
			return
		}
		srvSess <- s
	}()
	cli, err := Dial(c1, "tok", "base.example.com")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	srv := <-srvSess
	if n := srv.NumStreams(); n != 0 {
		t.Fatalf("fresh session reports %d streams, want 0", n)
	}

	stream, err := cli.OpenKind(KindControl)
	if err != nil {
		t.Fatal(err)
	}
	_, accepted, err := srv.AcceptKind()
	if err != nil {
		t.Fatal(err)
	}
	if n := srv.NumStreams(); n != 1 {
		t.Fatalf("after one open: %d streams, want 1", n)
	}

	stream.Close()
	accepted.Close()
	deadline := time.Now().Add(2 * time.Second)
	for srv.NumStreams() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("stream count stuck at %d after both ends closed", srv.NumStreams())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/tunnel -run TestNumStreamsFollowsOpenAndClose -v`
Expected: compile error `srv.NumStreams undefined`.

- [ ] **Step 3: Implement NumStreams**

In `internal/tunnel/tunnel.go`, after the `Close` one-liner (line 53), add:

```go
// NumStreams is the number of streams open on the link, whichever end
// opened them. A draining relay closes a session only when this reads zero,
// so every relay-side opener must close its stream when done or the wait
// runs to its deadline.
func (s *Session) NumStreams() int { return s.mux.NumStreams() }
```

- [ ] **Step 4: Run the tunnel tests**

Run: `go test ./internal/tunnel -v`
Expected: all PASS.

- [ ] **Step 5: Write the failing router test**

Append to `internal/relay/router_test.go` (add `"github.com/piperbox/piper/internal/tunnel"` to its imports if not already present):

```go
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
```

- [ ] **Step 6: Run it to verify it fails**

Run: `go test ./internal/relay -run TestSessionsListsAgentsNotCustomDomains -v`
Expected: compile error `r.Sessions undefined`.

- [ ] **Step 7: Implement Sessions**

In `internal/relay/router.go`, after `Bases`, add:

```go
// Sessions snapshots the agent sessions this router holds. Custom domains
// share byBase and are skipped, as in Bases, so each agent appears once.
func (r *Router) Sessions() []*tunnel.Session {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*tunnel.Session
	for base, s := range r.byBase {
		if _, custom := r.custom[base]; !custom {
			out = append(out, s)
		}
	}
	return out
}
```

- [ ] **Step 8: Run the router tests**

Run: `go test ./internal/relay -run 'TestSessionsListsAgentsNotCustomDomains|TestRouter|TestUnregister|TestBases' -v`
Expected: all PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/tunnel/tunnel.go internal/tunnel/tunnel_test.go internal/relay/router.go internal/relay/router_test.go
git commit -m "feat(tunnel,relay): Session.NumStreams and Router.Sessions for the drain wait

Part of #523.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 5: `acceptTunnels` refuses new sessions while draining

**Files:**
- Modify: `internal/relay/server.go:129-138` (acceptTunnels)
- Test: `internal/relay/accepttunnels_test.go`

**Interfaces:**
- Consumes: `Instance.Draining()` (Task 2); `syncLogBuffer` (same file), `waitForLog(t, *syncLogBuffer, want)` from `proxyproto_test.go`, `testInstance(t, st)`, `enrollTestAgent(t, st)`.
- Produces: a draining relay closes each new :7000 connection before the handshake, with the log line `tunnel from <addr> refused: relay is draining`.

- [ ] **Step 1: Write the failing test**

Append to `internal/relay/accepttunnels_test.go`:

```go
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
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/relay -run TestAcceptTunnelsRefusesWhileDraining -v`
Expected: FAIL with `a valid enrollment got a session from a draining relay`.

- [ ] **Step 3: Implement**

In `internal/relay/server.go`, change `acceptTunnels`:

```go
func acceptTunnels(ln net.Listener, st *Store, router *Router, ghApp *GitHubApp, delivery *TunnelDelivery, m *Metrics, inst *Instance) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		m.ConnAccepted("tunnel")
		// Draining (#523): the listener stays open so a stale edge's dial
		// still succeeds — a refused dial would evict us and cascade our
		// owner rows — but nothing gets a session. The agent retries in 1 s,
		// by which time the edge has seen draining=true and places it
		// elsewhere.
		if inst.Draining() {
			log.Printf("tunnel from %s refused: relay is draining", conn.RemoteAddr())
			conn.Close()
			continue
		}
		go serveTunnel(conn, st, router, st.AgentDisabled, ghApp, delivery, inst)
	}
}
```

- [ ] **Step 4: Run the accept tests**

Run: `go test ./internal/relay -run 'TestAcceptTunnels|TestServeTunnel' -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/relay/server.go internal/relay/accepttunnels_test.go
git commit -m "feat(relay): a draining relay refuses new tunnels without closing :7000

Part of #523.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 6: `relay.Drain`

**Files:**
- Create: `internal/relay/drain.go`
- Test: `internal/relay/drain_test.go`

**Interfaces:**
- Consumes: `Instance.MarkDraining()`/`row()` (Task 2), `Router.Sessions()`/`Counts()`/`Holds()` (Task 4), `tunnel.Session.NumStreams()`/`Close()`/`CloseChan()` (Task 4), `Store.UpsertInstance`, `acceptTunnels` (Task 5), `waitCond`, `testInstance`, `enrollTestAgent`.
- Produces: `func Drain(ctx context.Context, st *Store, inst *Instance, router *Router) (forced int)`; package var `drainTick`.

- [ ] **Step 1: Write the failing tests**

Create `internal/relay/drain_test.go`:

```go
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
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/relay -run TestDrain -v`
Expected: compile error `undefined: Drain` (and `undefined: drainTick`).

- [ ] **Step 3: Implement**

Create `internal/relay/drain.go`:

```go
package relay

import (
	"context"
	"log"
	"time"
)

// drainTick is how often Drain re-reads each held session's stream count.
// A package var (cf. heartbeatInterval) so tests can reason in ticks;
// production leaves it at 100ms.
var drainTick = 100 * time.Millisecond

// Drain is the first half of a relay's exit (#523). It marks inst draining —
// which makes acceptTunnels refuse new sessions and, through the pool row,
// makes edges place nothing new here — then closes each held session the
// moment it has no open streams, so in-flight splices finish. Owned
// hostnames keep routing here meanwhile: the pool row stays until the caller
// cancels the pool context, after Drain returns. It returns when the router
// holds no agents or ctx is done; in the second case every remaining session
// is closed and the number that still had streams open is returned.
func Drain(ctx context.Context, st *Store, inst *Instance, router *Router) (forced int) {
	inst.MarkDraining()
	agents, _, _ := router.Counts()
	if err := st.UpsertInstance(inst.row(agents)); err != nil {
		log.Printf("relay: mark draining: %v", err)
	}
	tick := time.NewTicker(drainTick)
	defer tick.Stop()
	for {
		sessions := router.Sessions()
		if len(sessions) == 0 {
			return 0
		}
		for _, sess := range sessions {
			if sess.NumStreams() == 0 {
				sess.Close()
			}
		}
		select {
		case <-ctx.Done():
			for _, sess := range router.Sessions() {
				if sess.NumStreams() > 0 {
					forced++
				}
				sess.Close()
			}
			return forced
		case <-tick.C:
		}
	}
}
```

Closing a session makes `serveTunnel`'s `CloseChan` case fire, which unregisters it and releases its owner row exactly as an agent-initiated close does; the next tick sees a shorter list.

- [ ] **Step 4: Run the drain tests, twice**

Run: `go test ./internal/relay -run TestDrain -count=2 -v`
Expected: all PASS both times (the second run guards against a timing fluke).

- [ ] **Step 5: Commit**

```bash
git add internal/relay/drain.go internal/relay/drain_test.go
git commit -m "feat(relay): Drain closes sessions when idle, bounded by a deadline

Part of #523.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 7: `cmd/piper-relay` drains before leaving the pool

**Files:**
- Modify: `cmd/piper-relay/main.go:24-27` (constants), `:311-337` (signal handler)

**Interfaces:**
- Consumes: `relay.Drain` (Task 6); locals `st`, `inst`, `router`, `cancel`, `poolDone`, `delivery`, `sig` already in `main`.
- Produces: the exit order SIGTERM → drain → leave pool → park deliveries → exit.

This task has no unit test: the behaviour is `Drain`'s (Task 6) and the ordering is a straight call sequence. Verification is build + vet.

- [ ] **Step 1: Add the constant**

In `cmd/piper-relay/main.go`, after `shutdownGrace`, add:

```go
// drainTimeout bounds how long a SIGTERM waits for open streams on held
// sessions to finish before the remaining sessions are cut (#523). With the
// 5 s pool leave and shutdownGrace after it, the worst-case exit is 60 s;
// the compose file's stop_grace_period and the runbook's orchestrator stop
// timeouts are sized to that.
const drainTimeout = 20 * time.Second
```

- [ ] **Step 2: Reorder the signal handler**

Replace the comment and goroutine starting at `// Pool membership: heartbeat, ownership watch, NOTIFY-woken drains.` through `os.Exit(0)` with:

```go
	// Pool membership: heartbeat, ownership watch, NOTIFY-woken drains. On
	// SIGTERM/SIGINT drain first (#523): the row flips to draining so edges
	// place nothing new here while still routing what we own, new tunnels
	// are refused, and each session closes once its streams end. Only then
	// leave the pool (the row delete is what tells edges to stop routing
	// here) and let in-flight webhook deliveries park (#295).
	ctx, cancel := context.WithCancel(context.Background())
	poolDone := make(chan struct{})
	go func() {
		relay.RunInstance(ctx, st, inst, router, delivery)
		close(poolDone)
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-sig
		log.Printf("piper-relay: %s — draining (deadline %s)", s, drainTimeout)
		dctx, dcancel := context.WithTimeout(context.Background(), drainTimeout)
		forced := relay.Drain(dctx, st, inst, router)
		dcancel()
		log.Printf("piper-relay: drained; %d session(s) cut at the deadline — leaving the pool", forced)
		cancel()
		select {
		case <-poolDone:
		case <-time.After(5 * time.Second):
			log.Print("piper-relay: pool row not released in time; edges will expire it")
		}
		if delivery != nil {
			ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
			defer cancel()
			delivery.Shutdown(ctx)
		}
		os.Exit(0)
	}()
```

Everything after this goroutine in `main` (the control API server, the wildcard config, `relay.Serve`) is unchanged.

- [ ] **Step 3: Build and vet**

Run: `go build ./... && go vet ./cmd/piper-relay/ && go test ./cmd/piper-relay/`
Expected: clean build, vet clean, existing tests PASS.

- [ ] **Step 4: Commit**

```bash
git add cmd/piper-relay/main.go
git commit -m "feat(relay): SIGTERM drains sessions before leaving the pool

Part of #523.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 8: Compose `stop_grace_period` with a contract test

**Files:**
- Modify: `deploy/compose/relay/docker-compose.yml:45-47`
- Test: `deploy/compose/compose_test.go`

**Interfaces:**
- Consumes: `repositoryFile(t, parts...)` test helper in the same file.
- Produces: the relay service carries `stop_grace_period: 60s`.

- [ ] **Step 1: Write the failing test**

Append to `deploy/compose/compose_test.go`:

```go
// The relay drains on SIGTERM (#523): up to 20 s for open streams, 5 s to
// leave the pool, 35 s for webhook deliveries to park. Compose's default
// 10 s stop grace would SIGKILL it mid-drain.
func TestRelayComposeGivesTheDrainItsBudget(t *testing.T) {
	compose := repositoryFile(t, "deploy", "compose", "relay", "docker-compose.yml")
	if !strings.Contains(compose, "stop_grace_period: 60s") {
		t.Error("relay/docker-compose.yml missing \"stop_grace_period: 60s\" on the relay service")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./deploy/compose -run TestRelayComposeGivesTheDrainItsBudget -v`
Expected: FAIL with `missing "stop_grace_period: 60s"`.

- [ ] **Step 3: Add the directive**

In `deploy/compose/relay/docker-compose.yml`, change the head of the `relay` service to:

```yaml
  relay:
    image: ghcr.io/piperbox/piper-relay:${PIPER_RELAY_VERSION:?set PIPER_RELAY_VERSION in .env (e.g. 0.20.0)}
    restart: unless-stopped
    # SIGTERM drains (#523): up to 20s for open streams, 5s to leave the pool,
    # 35s for webhook deliveries to park. Compose's default 10s would SIGKILL
    # the relay mid-drain.
    stop_grace_period: 60s
    depends_on:
```

- [ ] **Step 4: Run the compose tests**

Run: `go test ./deploy/compose -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add deploy/compose/relay/docker-compose.yml deploy/compose/compose_test.go
git commit -m "chore(compose): relay stop_grace_period fits the SIGTERM drain

Part of #523.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 9: Runbook and PROGRESS.md

**Files:**
- Modify: `docs/runbooks/relay-deploy.md` (three places, quoted below)
- Modify: `PROGRESS.md:5` (the `_Last updated:` line) and after `:55`

No test; the compose contract test (Task 8) covers the file the runbook points at. Verify by re-reading the diff.

- [ ] **Step 1: Same-schema upgrade paragraph**

In `docs/runbooks/relay-deploy.md`, under `## Upgrade, same schema`, replace the paragraph beginning `Tunnels drop and agents reconnect on their own` with:

```markdown
On SIGTERM the relay drains before it exits (#523): its pool row flips to
`draining` so an edge places nothing new on it, new tunnels are refused, and
each agent session is closed once it has no open streams — up to 20 s — then
the row is deleted and in-flight webhook deliveries park (up to 35 s more).
Give it a 60 s stop timeout: systemd's default `TimeoutStopSec` (90 s)
already covers it; on compose that is `stop_grace_period: 60s`, on ECS
`stopTimeout: 60`, on Kubernetes `terminationGracePeriodSeconds: 60`.
Tunnels still drop when their session closes and agents reconnect on their
own (piperd's tunnel client retries in the background) until #530 gives
each agent a second session. Verify: `journalctl -u piper-relay -f` shows
agents re-registering within a minute, and `piper box ls` from an enrolled
account shows `connected`.
```

- [ ] **Step 2: Schema-change section**

Under `## Upgrade across a schema change`, after the paragraph beginning `Path (b) below resets **all** relay state`, add:

```markdown
`relay_instances` and `agent_owners` are ephemeral — heartbeats re-create
the first, registration and the heartbeat's ownership re-assert the second —
so a release that changes either is dropped with
`DROP TABLE agent_owners, relay_instances;`, both together. Dropping only
`relay_instances` with `CASCADE` would remove the foreign key from
`agent_owners`, and `CREATE TABLE IF NOT EXISTS` would never put it back.
Roll relays before the edge: the edge selects columns of these tables that
only a new relay creates.
```

- [ ] **Step 3: Scale-out "What still drops" and compose day-to-day**

Under `## Scale out`, replace the paragraph beginning `**What still drops.**` with:

```markdown
**What still drops.** A relay restart closes each of its tunnels once the
tunnel has no streams in flight (#523), so requests finish but the agent's
hostnames are dark for the 1–3 s it takes to redial onto a survivor; #530
(two sessions per agent) closes that gap. Restarting the edge on a single
host drops every tunnel through it (on Kubernetes the Service holds the port
across a rolling restart). Both relays drain at once if they are recreated
together: `docker compose up -d` recreates replicas in parallel, so a
rolling relay upgrade needs `COMPOSE_PARALLEL_LIMIT=1` in the environment
(to be confirmed on the Hetzner host at the next upgrade).
```

Under `## Single host with compose`, in the **Day-to-day afterwards** list, replace the `Upgrade:` bullet with:

```markdown
- Upgrade: set `PIPER_RELAY_VERSION` and `PIPER_EDGE_VERSION` in `.env`, then
  `sudo docker compose pull && sudo COMPOSE_PARALLEL_LIMIT=1 docker compose up -d --scale relay=2`
  so the relays are recreated one at a time and each drains (#523) while the
  other serves. Restarting a relay still drops its tunnels once their
  streams end (agents redial onto the survivor) until #530; restarting the
  edge drops every tunnel until it is back. For a schema-change release,
  drop the named tables first with
  `sudo docker compose exec postgres psql -U piper_relay piper_relay`, and
  roll relays before the edge.
```

- [ ] **Step 4: PROGRESS.md**

In `PROGRESS.md`, after line 55 (the `piper-edge` + tunnel ownership line), add:

```markdown
- ✅ graceful relay drain — SIGTERM flips the pool row to `draining` (edge places nothing new there, keeps routing what it owns), refuses `:7000`, closes each session once its streams end (20 s bound), then leaves the pool; zero-drop arrives with [#530](https://github.com/piperbox/piper/issues/530) — [#523](https://github.com/piperbox/piper/issues/523) (child of epic [#524](https://github.com/piperbox/piper/issues/524))
```

Change the start of line 5 from `_Last updated: 2026-09-04 — relay scale-out child 1 (epic [#524](https://github.com/piperbox/piper/issues/524)) landed: `piper-edge` in front of N relays.` to `_Last updated: 2026-09-05 — relay scale-out child 3 (epic [#524](https://github.com/piperbox/piper/issues/524)) landed: graceful drain on SIGTERM. Earlier: child 1: `piper-edge` in front of N relays.` and leave the rest of the line as it is.

- [ ] **Step 5: Re-read and commit**

Run: `git diff --stat && git diff docs/runbooks/relay-deploy.md PROGRESS.md | head -120`
Check every quoted paragraph landed where named and nothing else in either file changed.

```bash
git add docs/runbooks/relay-deploy.md PROGRESS.md
git commit -m "docs: relay graceful drain landed (#523)

Part of #523.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

## After the last task (controller, not a subagent)

1. `make verify` — exit status 0.
2. Push `ozykhan/relay-drain`, open the PR into `main` with `Closes #523`, `Part of #524`, the deploy order (drop `agent_owners, relay_instances`; relays before edge), and the follow-up pointer to #530 for the edge's owner-choice skip.
3. Rewrite the #523 body to the shrunk scope (pointer to the prior-art study and to #530).
4. File the deferred edge issue via `/file-issue`: `[relay] piper-edge readiness flip and splice grace on SIGTERM`, `enhancement`, `P3`, `size/S`, `relay`, blocked on a fronted deployment (SO_REUSEPORT handoff or LB/Kubernetes Service).
5. Update the `relay-scale-out-state` memory.
