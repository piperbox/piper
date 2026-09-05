# Relay graceful drain (#523)

Third child of the scale-out epic (#524). The
[edge ownership design](2026-09-04-relay-edge-ownership-design.md) shipped
`piper-edge` and single-owner tunnels; #522 moved login state into Postgres.
This child makes a relay leave the pool in an order that lets in-flight work
finish and keeps a dying relay from being handed new work.

The scope is narrower than the issue's original text. The
[prior-art study](2026-09-05-tunnel-placement-prior-art.md) showed that
zero-drop deploys come from redundancy, not from a reconnect message: #530
gives each agent two sessions on two relays, after which a relay going away
is never an agent's only link. That child owns the agent's second dial loop
and removes the stale-session close on `piper_owners`. Building an agent
hand-off here would be rewritten there, so this design has no agent change
and no relay-to-agent message. What it builds is the part #530 needs
underneath it: a `draining` state the edge respects, and an exit order that
waits for open streams.

## What SIGTERM does today

`cmd/piper-relay` cancels the pool context first. The heartbeat goroutine
deletes the `relay_instances` row, which cascades every `agent_owners` row
this relay holds, so all of its agents go dark at the edge instantly. Then
in-flight webhook deliveries park (bounded by `shutdownGrace`, 35 s), and
the process exits, severing every tunnel session and every public splice
running through it. Agents redial after a 1 s backoff and the edge places
them on a survivor; each hostname is unrouted for roughly 1 to 3 s.

Meanwhile the row was live until the cancel, so an edge could place a brand
new agent on the relay in the seconds before it died.

## Goals

- A draining relay takes no new tunnels and no new `api.<apex>` connections.
- Hostnames a draining relay owns keep routing to it until its sessions end,
  so nothing it is still serving is cut by routing.
- Every open stream on the relay finishes, bounded by a deadline, before its
  session is closed.
- The pool row goes away last, after nothing depends on it.
- The whole exit fits an orchestrator stop timeout the runbook names.

## Non-goals

- **Zero-drop for single-session agents.** Until #530, an agent whose only
  session is on the draining relay still redials and is dark for the 1 to
  3 s that takes. This design shortens nothing there; it stops cutting work
  that is in flight.
- **A relay-to-agent "reconnect now" message.** #530 makes it unnecessary:
  the agent already holds a second session, and redials the lost slot from
  a 1 s backoff on its own.
- **Edge drain and readiness.** On the Hetzner single-host layout the edge
  holds the public ports itself and a replacement cannot bind until it
  exits, so an edge drain buys nothing there without the SO_REUSEPORT
  handoff parked as follow-up 3 of the edge design. A `/readyz` that flips
  on SIGTERM, plus keeping splices alive for a grace, is filed as its own
  issue for the fronted (Kubernetes/ECS) case.
- **Long-lived connections.** A websocket or SSE stream that outlives the
  deadline is cut at the deadline. Inherent; documented.

## Design

### 1. The pool row says `draining`

`relay_instances` gains one column:

```sql
draining    BOOLEAN NOT NULL DEFAULT false
```

`InstanceRow` gains `Draining bool`. `UpsertInstance` writes it (in both the
insert and the `ON CONFLICT` update), `LiveInstances` and `OwnerOf` read it.
`Instance` gains an `atomic.Bool`; `row()` copies it into the payload so
every subsequent heartbeat carries the flag.

Marking is immediate: the drain path sets the flag and performs one upsert
itself rather than waiting up to `heartbeatInterval` (5 s) for the next
beat. `UpsertInstance` already NOTIFYs `piper_instances`, so edges see the
flip at once.

**Schema change.** Per the pre-1.x policy the `CREATE TABLE` is edited in
place; there is no migration. Both `relay_instances` and `agent_owners` are
ephemeral (heartbeats re-create the first; registration and
`reassertOwnership` re-create the second), so the release notes say
`DROP TABLE agent_owners, relay_instances;` before starting the new
relays. `agent_owners` must go too: dropping only `relay_instances` with
`CASCADE` would drop the foreign key and `CREATE TABLE IF NOT EXISTS` would
never put it back.

### 2. The edge stops placing work on a draining relay

`pickTunnel` (:7000 placement) and `pickAPI` (`api.<apex>` round-robin)
skip rows with `Draining` set. `ownerOf` (:443/:80 by owner) does not: the
draining relay is still the only process holding those agents' sessions,
and routing away from it would cut them. If every live relay is draining,
`pickTunnel` and `pickAPI` report no candidate and the connection is counted
unrouted, which is the truth.

When #530 lands and an agent has several owners, the edge's choice among
owners should skip draining ones too. That is one condition in the picker
and is why the flag lives in the row rather than in relay memory.

### 3. The relay refuses new tunnels without closing the listener

While draining, `acceptTunnels` closes each accepted connection before the
handshake, logging one line. The :7000 listener stays open on purpose: an
edge that has not yet seen the NOTIFY may still dial, and a refused dial
makes the edge evict the relay and delete its row, cascading its owner rows
and blacking out every agent it still serves. A closed connection costs the
misplaced agent one failed handshake and a 1 s retry, by which time the
edge has the flag.

The relay's API listener keeps serving until exit. The edge no longer sends
it new `api.<apex>` connections, but the relay-to-relay control hop dials
the owner of an agent directly, and while the relay still owns agents those
requests must be answered.

### 4. Sessions close when idle, then the row goes

A new function in `internal/relay`:

```go
// Drain marks inst draining, stops acceptTunnels taking new sessions, and
// closes each held session the moment it has no open streams. It returns
// when the router is empty or ctx is done; in the second case every
// remaining session is closed and their count is returned as forced.
func Drain(ctx context.Context, st *Store, inst *Instance, router *Router) (forced int)
```

Behaviour:

1. Set `inst.draining`; upsert the row once, now.
2. Loop on a short tick (100 ms). On each tick, for every session in
   `router.Sessions()`, if `sess.NumStreams() == 0` call `sess.Close()`.
   The existing `serveTunnel` loop sees `CloseChan` fire, unregisters the
   session and releases its owner row exactly as it does for an
   agent-initiated close.
3. Return when `router.Counts()` reports zero agents.
4. On `ctx.Done()`, close every remaining session and return their count.

An idle session closes immediately. Agents with nothing in flight redial at
once, which is as short as their blip can be before #530. A session with
work in flight lives until that work ends or the deadline fires. The window
that is lost is any connection the relay has accepted but not yet turned
into a stream (still inside the SNI/Host peek), plus the tick between the
zero read and the `Close`; #530's owner skip removes it.

Two small exports make this possible:

- `tunnel.Session.NumStreams() int`, yamux's count of open streams on the
  link. It counts streams either side opened, so a relay-side opener that
  forgot to close its stream would pin the wait. Every opener today defers
  its close; that stays a rule.
- `Router.Sessions() []*tunnel.Session`, a snapshot of the sessions held.

`cmd/piper-relay` orders the exit:

```go
s := <-sig
log.Printf("piper-relay: %s — draining (deadline %s)", s, drainTimeout)
dctx, done := context.WithTimeout(context.Background(), drainTimeout)
forced := relay.Drain(dctx, st, inst, router)
done()
log.Printf("piper-relay: drained; %d session(s) closed at the deadline", forced)
cancel()                      // leave the pool: the row goes now
<-poolDone (bounded 5 s, as today)
delivery.Shutdown (bounded shutdownGrace, as today)
os.Exit(0)
```

The pool context stays live through the drain because the heartbeat must
keep carrying `draining=true` and the row must exist for owned hostnames to
route. Nothing else in `RunInstance` changes: the stale-session close on
`piper_owners` is untouched here and removed by #530.

### 5. Timing

- `drainTimeout = 20 * time.Second`, a constant next to `shutdownGrace` in
  `cmd/piper-relay/main.go`. No environment knob: an orchestrator's stop
  timeout is set to fit the relay, not the reverse.
- Worst case exit is drain (20 s) + pool leave (5 s) + delivery park (35 s)
  = 60 s. The compose relay service gets `stop_grace_period: 60s`; compose's
  default of 10 s would SIGKILL mid-drain. The runbook names 60 s for ECS
  `stopTimeout` and Kubernetes `terminationGracePeriodSeconds`.
- `docker compose up -d` recreates every relay replica in parallel, so both
  drain at once and there is no survivor for anything. Rolling recreation
  needs `COMPOSE_PARALLEL_LIMIT=1` (or an equivalent one-at-a-time step);
  the runbook records this with a note that it is to be confirmed on the
  Hetzner host at the next upgrade.

### 6. Logging

- On signal: the signal name and the deadline.
- Per refused tunnel while draining: remote address, one line.
- On drain end: how many sessions were force-closed at the deadline (zero
  on a clean drain).

## Testing

All relay tests run against Postgres via the existing `openTestStore`.

- **Store.** `UpsertInstance` round-trips `Draining` on insert and on
  update; `LiveInstances` and `OwnerOf` return it.
- **Edge state.** `pickTunnel` and `pickAPI` skip a draining row; with every
  row draining both report no candidate; `ownerOf` still returns a draining
  owner.
- **Relay drain.** With an in-process `acceptTunnels` on a loopback
  listener and a real `tunnel.Dial` session:
  - `Drain` flips the row to draining before it returns and before any
    session closes (observed via `LiveInstances` from a second goroutine, or
    by holding a stream open so `Drain` cannot finish).
  - A new tunnel dial during drain is closed before the handshake
    completes; `tunnel.Dial` returns an error.
  - A session with one open stream survives while the stream is open, and
    `Drain` returns once the stream is closed, with `forced == 0`.
  - A session with a stream that never closes is closed when ctx expires,
    and `Drain` returns `forced == 1`.
  - An idle session is closed without waiting for the deadline.
- **Tunnel.** `Session.NumStreams` reads 0, then 1 after `OpenKind`, then 0
  after the stream closes.
- **Compose.** A contract test in `deploy/compose` asserts
  `relay/docker-compose.yml` contains `stop_grace_period: 60s` (the existing
  `TestComposeContract` covers the agent file; this is a sibling assertion
  over the relay file).
- `cmd/piper-relay` gets no new test: the ordering it adds is the call
  sequence above, and `Drain` carries the behaviour.

## Paper trail

- #523 body rewritten to this scope with a pointer to the prior-art study
  and to #530 for the zero-drop half.
- A new issue for the edge readiness flip and splice grace (the deferred
  edge half), `[relay]`, P3, size/S, blocked on a fronted deployment.
- Runbook: `stop_grace_period`, the orchestrator stop-timeout number, the
  serialized-recreation note, and the schema-change drop for this release.
  The line "restarting a relay drops its tunnels" stays until #530 removes
  the drop.
- `PROGRESS.md`: one line under the scale-out epic.

## Deploy order

Drop the two ephemeral tables; that step is mandatory and is the only thing
that matters for this release. Both `piper-relay` and `piper-edge` apply
the same `schema.sql`, so after the drop whichever binary opens the store
first re-creates the tables with the new `draining` column — roll order is
irrelevant. Skip the drop and every new relay's heartbeat fails, leaving no
relay placeable no matter which binary was rolled first.
