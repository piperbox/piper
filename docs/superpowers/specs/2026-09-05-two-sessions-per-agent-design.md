# Two tunnel sessions per agent on two relays (#530)

Fourth child of the scale-out epic (#524). The
[edge ownership design](2026-09-04-relay-edge-ownership-design.md) shipped
`piper-edge` and single-owner tunnels, #522 moved login state into Postgres,
and #523 gave a relay a graceful drain. This child gives each agent two
tunnel sessions on two different relays, so no relay termination is ever an
agent's only link going away.

The [prior-art study](2026-09-05-tunnel-placement-prior-art.md) is the
rationale: Cloudflare Tunnel gets zero-drop edge deploys from redundancy,
not from a drain protocol. Each connector holds several sessions in two
failure domains and the edge just stops routing to a dead one. Piper does the
same at its own scale: two sessions, the edge routes to whichever owner row
survives, and the agent redials the lost one in the background.

## What happens today

Each agent holds one yamux session. `agent_owners` is keyed on the agent
alone, so ownership is "whoever registered last": a relay that sees another
live instance take ownership of an agent it holds closes its own session as
stale. When a relay terminates, every agent on it is dark at the edge until
its redial lands elsewhere, roughly 1 to 3 s per hostname. #523 narrowed
that window; it cannot close it.

## Goals

- Every agent holds two sessions, on two different relays whenever the pool
  has two live non-draining relays.
- A relay going away drops one of an agent's two sessions. Public traffic
  for that agent keeps flowing through the other, including the connection
  that discovers the dead relay.
- The agent regains its second session on its own, without a relay-to-agent
  message.
- Both of an agent's relays route every hostname and custom domain the agent
  has, regardless of which session carried the control op that created it.
- The edge stays a blind L4 hop: it reads a public routing key on every
  listener and never parses a credential.

## Non-goals

- A single-relay deployment. A supported relay deployment is `piper-edge` in
  front of at least two `piper-relay` processes sharing one Postgres. One
  relay still works, on one session, and the agent's second slot says so in
  its log; nothing adapts to it. The e2e harness is that unsupported shape
  today and moves to the supported one under #537.
- More than two sessions, or a knob for the count. Two is enough for a
  rolling restart with `maxUnavailable: 1`, and there is no
  `PIPER_AGENT_TUNNEL_SESSIONS`.
- Zone-aware placement of the second session. That is #531, a tie-break on
  top of this design's placement rule.
- Compatibility with agents that speak the current one-frame handshake. The
  wire changes in place per the pre-1.x policy; agents upgrade with the
  relay release.
- Steering among an agent's two sessions. The edge picks by the same rule it
  already uses for placement.

## Design

### 1. Handshake: a routing preface, then the credential

`tunnel.Dial` writes two frames in one flush. Frame one is the preface,
`{"base_domain": ...}`. Frame two is the credential, `{"token": ...}`.
`tunnel.Serve` reads both, calls `auth(token, base)` with the base from the
preface, and everything from the ack onward is unchanged.

The preface is the only carrier of the base domain. The credential frame
never repeats it, so the two frames cannot disagree. The preface is
unauthenticated peer input until `auth` passes; the relay's existing check
that the token's enrolled base equals the claimed base is what makes it
trustworthy afterwards, exactly as it is for today's single frame.

The split is what lets the edge peek the routing key without touching the
credential (section 2). It matches how the edge already treats its other two
listeners: SNI on :443 and the Host header on :80 are public routing keys
and the payload behind them flows through unread.

The handshake ack gains one differentiated case. `tunnel.ErrDuplicateSession`
is a sentinel the relay returns from `auth` when it already holds a session
for the base. `Serve` maps that one error to its own ack reason and `Dial`
maps the reason back to the sentinel, so the agent can `errors.Is` it. It is
safe to differentiate because the relay only checks for a duplicate after
the token has been validated: the peer has proven which agent it is. Every
other rejection keeps the undifferentiated reason.

### 2. Edge: placement excludes current owners; routing picks among them

**:7000.** `handleTunnel` gains a `readPreface` peek, the sibling of
`readSNI` and `readHost`: a bounded read of exactly frame one through the
connection, with no buffering of its own so nothing past it is consumed,
decode the base, and replay the frame's bytes into `forward` as `buffered`.
The credential frame is never read by the edge; it flows through the splice
with the rest of the stream.

Placement becomes `pickTunnel(base, exclude)`: fewest sessions, then
earliest started, among live non-draining relays that do not already own
`base`. `exclude` still names relays a failed dial has just ruled out, and
the one-retry loop stays as it is.

Exclusion is soft. If it empties the candidate set, the pick runs again over
every live non-draining relay, and the relay that receives the dial rejects
it after auth (section 3). A hard exclusion would close the connection
without a relay ever answering, and an unauthenticated peer could then tell
"this agent is fully connected" apart from "not connected" just by claiming
its base. With the fallback, every claimed base gets the same observable
behaviour: a relay's rejection ack.

**:443 and :80.** `ownerOf(agent)` returns one row chosen among the agent's
live owners: non-draining first, then fewest sessions, then earliest
started. A draining relay is still a candidate, because it still holds the
session and is still serving; it only loses ties, so a rolling restart
shifts new connections to the survivor the moment the row says draining.

`handleTLS` and `handleHTTP` take the :7000 shape: on a failed dial, evict
that relay and retry once on the next owner. The comment that :443 never
retries because "the owner is unique" goes with its reason. This is what
makes the connection that discovers a dead relay still succeed.

**Edge state.** `owners` becomes `map[string][]string`, agent base to live
owner ids. `setOwner(agent, ids)` replaces the slice, `evict(id)` removes
that id from every slice, and `ownerOf` applies the rule above over the ids
that still map to a live instance. `onNotify` for `piper_owners` reloads the
whole list for the named agent.

### 3. Store: ownership is every live row

**Schema.** `agent_owners` becomes `PRIMARY KEY (agent_name, instance_id)`,
with the same two cascades. The `CREATE TABLE` is edited in place. The
release carries the same mandatory drop as the edge release did:
`DROP TABLE agent_owners, relay_instances;` together, since dropping only
one loses the foreign key and `CREATE TABLE IF NOT EXISTS` never puts it
back.

**Writes.** `SetOwner(base, instance)` inserts with `ON CONFLICT DO NOTHING`
and notifies `piper_owners` only when a row was actually inserted.
Idempotent and silent when nothing changed, which is what lets the heartbeat
call it unconditionally (section 4). An unknown agent still returns
`ErrBadToken`, detected separately from the zero-row "already there" case.
`ClearOwner(base, instance)` is unchanged: it deletes only its own row.

**Reads.** `OwnerOf(base)` returns `[]InstanceRow`, the live owners in the
total order `LiveInstances` uses. `Owners()` returns `map[string][]string`.
The `liveWhere` predicate stays the read-side liveness rule, so a dead
relay's rows vanish from both without anyone deleting them.

`HostnamesFor(base)` is new: the relay-terminated hostnames the agent holds,
for section 4's route derivation.

### 4. Relay: reject duplicates, derive routes from Postgres

**Duplicate rejection.** The check runs after the token is validated and
before the ack is written. Both happen inside `tunnel.Serve`, so it lives in
the relay's auth callback: `serveTunnel`'s wrapper calls `tunnelAuth` as
today, then returns `tunnel.ErrDuplicateSession` if `router.Holds(base)`.
`router.Register` also returns that error when the base is already held,
covering two handshakes that both passed the check concurrently, and
`serveTunnel` closes the session on it. A duplicate arriving at all means
the edge's exclusion had nothing left to offer or a race, so the relay logs
it with the base named.

**Ownership.** `RunInstance` drops its `piper_owners` branch and with it the
"reconnected elsewhere, closing stale session" behaviour: two owners is the
normal state, and `reassertOwnership` no longer has anyone to defer to. It
becomes "call `SetOwner` for every held base each beat". Section 3 made that
one idempotent insert per base with no NOTIFY in steady state, and a relay
whose row an edge cascaded away gets every owner row back on the next beat.

`clearOwner` keeps its `Holds` guard. On one instance an unregister, a fresh
register of the same agent, and the late clear can still interleave, and
both sessions map to the same `(agent, instance)` row; only "do we still
hold the base" tells the newer session's row from the old one's.

**Routes from Postgres.** Relay-terminated hostnames and custom domains live
in each relay's router and get there through control ops the agent sends
over one session. With two sessions on two relays, a `register` sent over
the session to relay A never reaches relay B, and the edge routes :443 for
that hostname to either. So truth moves to the store, where it already is:

- New router methods `SetHosts(sess, hosts)` and `SetCustom(sess, domains)`
  make a session's entries exactly the given set: add the missing, remove
  the extras registered to that session, leave other sessions' entries
  alone.
- `syncRoutes(st, router, base, sess)` calls `HostnamesFor` and
  `CustomDomains` and applies both. It replaces the custom-domain loop in
  `serveTunnel` at register time.
- `RunInstance` LISTENs on `piper_hostnames` too. Every write to
  `hostnames` and `custom_domains` already fires it for the edge's name
  cache. On a payload the relay runs `syncRoutes` for every base it holds;
  the payload is a hostname, not a base, and the held set is small. The
  listener's reconnect resync does the same, so a NOTIFY missed while
  disconnected is caught up.
- Control op handlers keep their direct local router updates, so the relay
  that served the op routes the new name before the NOTIFY comes back to
  it.

**Untouched.** The control hop's `liveOwner` picks the first live owner that
is not itself from the list `OwnerOf` returns. Webhook delivery is
unchanged: `drainEvents` reads, locks and deletes in one transaction, so two
owners draining the same agent split the parked events without duplicates,
and a live webhook that lands on a non-owner parks and wakes both. `Drain`
(#523) is unchanged and now usually closes one of two sessions.

### 5. Agent: two slots

`TunnelClient` holds two session slots behind its existing mutex; the count
is a constant. `Run` starts one dial loop per slot, each identical to
today's loop with its own backoff and healthy-threshold logic, and returns
when both have. Slot 2 sends its first dial only after slot 1's first
successful handshake, or on shutdown: without that gate both boot dials
reach the edge before the first owner row exists, land on the same relay,
and the second is rejected on every start.

`current()` returns any live session, lowest slot first, so every control
op works unchanged. `Status` reports connected if any slot is live, retrying
if running with none, and the most recent error from either slot.
`OnConnect` fires per slot connect; provisioning is already mutexed and
idempotent, and sync-apps may ride either session because the relay now
derives routes from Postgres. `ObservedIP` is set by whichever slot
connected last.

On `tunnel.ErrDuplicateSession` a slot logs once, on the transition into
that state, that the pool has no second relay for this agent and it will
keep retrying, and backs off straight to a one-minute cap instead of the
1 s to 30 s ladder. A successful connect resets both the log gate and the
backoff. The cap is the time to regain redundancy after a relay comes back,
so it is short; the dial is cheap and the log is once per state, so there is
nothing to be quiet about.

`piperd` needs no change.

### 6. Failure walk-throughs

**Rolling restart, `maxUnavailable: 1`.** Relay B receives SIGTERM and marks
draining. The edge stops placing on it and prefers A for the agents both
hold. B closes each session once it has no open streams; each close drops
B's owner row, and the agent's slot redials. The edge excludes A, finds
nothing, falls back to A, A rejects the duplicate, and the slot waits a
minute. B's replacement heartbeats, the next retry places there, and the
agent is back on two relays. No hostname was ever unrouted.

**Relay dies without warning.** B's heartbeat stops. Within the 15 s TTL its
rows read as dead and the edge's poll purges them; before that, the first
:443 or :80 connection the edge tries to send to B fails to dial, evicts B,
and is retried on A. The agent's slot on B dies at once on a reset or within
the yamux keepalive if the network blackholes, then follows the redial path
above. Until B or a replacement returns the box runs on one session, the
same exposure it has today all the time.

**Half-open session.** The agent's link to A dies but A has not noticed. A's
owner row stays, the edge excludes A, falls back to A, and A rejects the
redial as a duplicate because it still holds the old session. The slot
retries every minute; A's yamux keepalive clears the stale session within
about 30 s and the next retry lands. A delay, not an outage: the other
session served throughout.

**One relay in the pool.** Slot 1 connects. Slot 2's dial lands on the same
relay and is rejected; the slot logs once that the pool has no second relay
and retries every minute. The relay logs each duplicate with the base named.
The deployment is mis-sized and both sides say so.

## Testing

Failing test first at every layer, one plan task per row.

| Layer | Tests |
| --- | --- |
| `internal/tunnel` | two-frame round trip; the duplicate sentinel survives the ack in both directions; every other rejection is still the undifferentiated reason |
| store | composite key holds two rows for one agent; `SetOwner` inserts once, notifies once, is a silent no-op after; `OwnerOf`/`Owners` return sets and skip dead instances; `HostnamesFor` |
| `edgeState` | exclusion picks the non-owner; exclusion that empties the set falls back to the full pool; `ownerOf` prefers non-draining, then fewest sessions, then earliest; `evict` removes an id from every set |
| edge cluster (`edge_test.go`) | an agent ends with two sessions on two distinct relays; stopping one relay leaves the agent's hostname served through the other, including the connection that discovers it; the second session re-places once a relay is back; a hostname registered over one session routes through both relays |
| router | `Register` of a held base returns the duplicate error and leaves the existing session; `SetHosts`/`SetCustom` reconcile one session's entries without touching another's |
| instance | `reassertOwnership` re-inserts every held base each beat; a `piper_hostnames` NOTIFY re-derives held agents' routes; `TestRunInstanceClosesSessionOwnedElsewhere` is deleted |
| `internal/agent` | two slots connect, slot 2 only after slot 1; `current` returns the surviving slot when one dies; `Status` is connected with one live slot; a duplicate rejection logs once and backs off to the cap; a later success resets both |

The relay e2e stays as it is: slot 2 rejects quietly, slot 1 carries the
test. #537 moves it to the supported topology.

## Paper trail

- Design: this document. Prior art:
  [tunnel placement study](2026-09-05-tunnel-placement-prior-art.md),
  follow-up 4.
- Issue: #530, child of epic #524. Follow-ups: #531 (zone tie-break), #537
  (e2e on edge plus two relays).
- Runbook: the scale-out section says two sessions on two relays and names
  at least two relays as a requirement; the release note carries the
  `agent_owners` drop.
- `PROGRESS.md`: one line under the epic.

## Deploy order

Relays and edge first, then agents. The schema drop goes with the relay
release, and both binaries apply the same `schema.sql`, so whichever opens
the store first re-creates the table. Agents on the old one-frame handshake
cannot connect to a new relay, so the Mac and the Pi upgrade the same day.
Schema changed, so the release is a minor bump.
