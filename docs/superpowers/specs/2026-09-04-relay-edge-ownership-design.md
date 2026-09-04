# Design: relay scale-out — `piper-edge` + tunnel ownership

Lets `piper-relay` run as N processes behind one public address. Today the
relay is exactly one replica ([Postgres design](2026-08-04-relay-postgres-design.md)
moved the store; this removes the next blocker). The problem is tunnels: each
agent's yamux session is terminated by exactly one relay process, and every
public connection, control call, and webhook for that agent has to reach that
process. Nothing else in the relay is process-bound in a way that matters
today.

This is the first child of the scale-out epic. It ships a working tiered
deployment. Two follow-ups are scoped out below (login-flow state, graceful
drain).

## Decision summary

- **Route to the owner; never forward between relays.** A new binary,
  `piper-edge`, is the only public entrypoint. It learns which relay holds
  each agent from Postgres and sends each connection straight there. Relay
  processes never talk to each other for data traffic.
- **The relay stays what it is.** Same binary, same listeners, same `Serve`.
  It gains an instance heartbeat and an ownership write on tunnel
  register/unregister. Nothing else in `piper-relay` changes shape.
- **The edge is a binary, not a role.** It shares parsing and splicing code
  with the relay but no configuration and no lifecycle: no certs, no GitHub
  App, no verifier, no API handlers. A separate `cmd/piper-edge` keeps
  "the edge never opens the store's write paths" structural rather than a
  discipline, gives Kubernetes its own image and rollout cadence, and keeps a
  relay env file from ever being handed to an edge. The code lives in
  `internal/relay` (`edge.go`) so the unexported SNI reader, Host peeker,
  splice, and PROXY helpers stay unexported; both binaries link that package.
- **Postgres is the only discovery mechanism.** No proxy config, no backend
  lists, no static addresses. A relay that starts is in the pool; one that
  stops heartbeating is out. LISTEN/NOTIFY keeps edges current in
  milliseconds; polling is the fallback.
- **Two small patches make "any relay" sufficient for control traffic.** The
  control proxy hops to the owner over internal HTTP when the agent is not
  local. Webhook delivery parks in Postgres and NOTIFY wakes the owner. Both
  reuse code that exists.
- **No new module.** SNI parsing, Host peeking, the byte splice, PROXY v2
  accept (`pires/go-proxyproto`, which also writes headers), pgx, and yamux
  are all already direct dependencies.

Rejected: peer forwarding between relays (one internal hop for (N-1)/N of
public traffic, and a relay-to-relay mesh to operate); dynamic config pushed
into HAProxy/Traefik/Envoy/Gateway API (a proxy-specific integration per
platform, and control calls still need ownership); consistent hashing at the
proxy (an agent owns many hostnames that hash to different replicas); DNS
steering (TTL lag on failover); splitting the relay into tunnel and API tiers
(everything it bought is either graceful drain or a one-line "start a relay
with :7000 off" later).

## Non-goals

- **Login-flow state across replicas.** Device-flow polls, web-login `state`,
  CLI browser-login handles, and the login rate limiter stay per-process.
  Until the follow-up lands the edge pins `api.<apex>` to one relay (below),
  so login keeps working with N relays.
- **Graceful drain.** Restarting a relay still drops its tunnels; agents
  redial from a 1 s backoff and land on a survivor. Its own follow-up, agent
  side included.
- **Zero-drop edge restarts on a single host.** An edge restart drops the
  tunnels that pass through it (same redial as above). On Kubernetes the
  Service holds the port across a rolling restart, so it is a single-host
  concern only. SO_REUSEPORT handoff is a possible later opt-in, not built.
- **Authentication on internal listeners.** The relay's PROXY v2 listeners
  and :8080 trust their network (compose network or a NetworkPolicy). A shared
  secret is a hardening follow-up.
- **Edge-side TLS termination or HTTP routing.** The edge never holds a cert.
- Postgres HA, and sizing the edge's cache — measure first.

## Data model

Two tables in `schema.sql`, complete current shape as always (pre-1.x, no
migration).

```sql
CREATE TABLE IF NOT EXISTS relay_instances (
    id          TEXT PRIMARY KEY,          -- random per process start
    started_at  TIMESTAMPTZ NOT NULL,
    last_seen   TIMESTAMPTZ NOT NULL,
    sessions    INTEGER NOT NULL DEFAULT 0,
    tls_addr    TEXT NOT NULL,             -- host:port the edge dials for :443 traffic
    http_addr   TEXT NOT NULL,             -- … for :80 traffic
    tunnel_addr TEXT NOT NULL,             -- … for :7000 traffic
    api_addr    TEXT NOT NULL              -- … for api.<apex> and the control hop
);
CREATE TABLE IF NOT EXISTS agent_owners (
    agent_name  TEXT PRIMARY KEY REFERENCES agents(name),
    instance_id TEXT NOT NULL REFERENCES relay_instances(id) ON DELETE CASCADE,
    since       TIMESTAMPTZ NOT NULL
);
```

- An instance is **live** when `last_seen` is within 15 s (heartbeat every
  5 s). Liveness is a read-side predicate, so a crashed relay disappears from
  routing without anyone deleting anything. Whoever reads a dead row deletes
  it; the cascade takes its ownership rows with it. No reaper.
- Agent name → owner is the only new mapping. Hostname → agent already exists:
  exact `hostnames.hostname` (relay-terminated shared-domain names), exact
  `custom_domains.domain` via `agent_base` (BYO), and suffix match on
  `agents.base_domain` (passthrough), in that order — the same precedence as
  `Router.LookupHost`/`LookupCustom`/`Lookup` today.
- **Ownership writes are conditional.** Register does an upsert (a new owner
  overwrites, since an agent that reconnected elsewhere is the truth).
  Unregister deletes `WHERE instance_id = <me>` so a relay whose half-open
  session dies late never removes the new owner's row — and skips the delete
  altogether while its own router still holds the base, because a reconnect
  onto the *same* relay leaves the row naming us either way and only the
  router tells the newer session's row from the dying one's.
- **Ownership is re-asserted, not written once.** Every heartbeat claims each
  base the router holds that no live instance owns. A relay whose row was
  deleted — by an edge that found it undialable for one dial, cascading its
  ownership away — is otherwise back in the pool with every agent it holds
  dark until each reconnects. A base a *different* live instance owns is left
  alone: the agent moved, and the stale session is closed rather than stolen
  back.

Four NOTIFY channels, fired inside the store methods that change the rows,
payload = the key that changed:

| Channel | Fired by | Consumers |
|---|---|---|
| `piper_instances` | instance upsert/delete | edges (backend pool) |
| `piper_owners` | `SetOwner`/`ClearOwner` | edges (owner map); the relay that lost ownership closes its stale session |
| `piper_hostnames` | hostname register/deregister/reconcile, custom domain add/confirm/remove | edges (name cache invalidation) |
| `piper_events` | `ParkEvent` | the owning relay drains for that agent |

Listeners use one dedicated pgx connection per process (`pgx.Connect` on the
same DSN; the store's `database/sql` pool is unsuitable for LISTEN). A
dropped listener connection reconnects with backoff and does a full resync
on return, so a missed NOTIFY costs at most one reconnect, never correctness.

## Relay changes (`piper-relay`)

Confined to four places.

1. **Instance heartbeat.** `main` creates the instance (random id, advertised
   addresses) and runs a 5 s upsert loop carrying `router.Counts()` agents as
   `sessions`. On clean shutdown it deletes its row. Advertised host defaults
   to the first non-loopback IPv4 (the container/pod IP, which is what an edge
   can dial); `PIPER_RELAY_ADVERTISE_HOST` overrides. Ports are the relay's
   own configured listener ports.
2. **Ownership.** `serveTunnel` calls `st.SetOwner(agent, instanceID)` right
   after `router.Register(sess)` and `st.ClearOwner(agent, instanceID)` where
   it calls `router.Unregister` — unless `router.Holds(agent)` still names a
   session, in which case a newer one already owns the base. The heartbeat
   re-asserts ownership for held bases nobody live owns. A relay also listens on `piper_owners`: a
   payload naming an agent it holds, now owned by another live instance,
   closes that session (the agent has reconnected elsewhere; keeping the
   stale one risks a late webhook drain or control answer from the wrong
   process).
3. **Control hop.** `NewControlProxy` today resolves the target with
   `router.Lookup` and dials a `KindControlAPI` stream. When the router
   misses, it now asks `st.OwnerOf(base)`; if that names a live instance
   other than itself, the request is forwarded **unchanged** (original path
   `/agents/<base>/…`, original `Authorization`) to `http://<api_addr>` with
   a second, minimal `httputil.ReverseProxy`. The owner authenticates and
   does its own box-token rewrite exactly as today. No loop is possible: the
   hop is taken only when the router misses and the owner is a different
   live instance; the owner's own miss (a race) answers 404 as now. `GET
   /agents` liveness and the per-agent liveness field switch from
   `router.Lookup` to "local session, or a live owner row".
4. **Webhook delivery.** `Deliver` keeps its local fast path. On a router
   miss it parks (as today on `ErrAgentOffline`) and `ParkEvent` fires
   `piper_events`. Every relay listens; the one whose router holds the agent
   calls `DrainFor`. The existing sweeper is already safe on N replicas: its
   `drain` skips agents without a local session, so replicas never race on
   the same agent. The installation-reconcile throttle stays per-process
   (harmless: N× a coarse throttle).

Everything else — SNI dispatch, termination, `:80` pump, custom domains, org
and GitHub handlers — is untouched. `PIPER_RELAY_PROXY_PROTOCOL=1` on a relay
behind an edge is required and documented, not implied, so the existing
"never trust PROXY headers from the internet" guard keeps its meaning.

## The edge (`piper-edge`)

`cmd/piper-edge/main.go` is a thin main (env parsing, store open, ops
listener, `relay.ServeEdge`). It starts the three public listeners plus the
ops listener. Its only store access is reads plus deleting dead instance
rows. Holds no cert, speaks no HTTP beyond peeking a `Host` line.

### Tables in memory

- **instances**: live rows from `relay_instances`, refreshed by
  `piper_instances` and a 15 s poll (belt and braces; the poll also evicts
  stale rows the NOTIFY stream would not mention).
- **owners**: `agent_name → instance_id`, refreshed by `piper_owners`, full
  load at start and on listener reconnect.
- **names**: `hostname → agent_name`, looked up on demand with the three
  queries above, cached 30 s including negatives, entries evicted by
  `piper_hostnames`. A negatively cached name becomes routable the moment
  its row lands, because the register path notifies.

### Per-connection routing

| Listener | Key | Target |
|---|---|---|
| :443, SNI = `api.<apex>` | none | the live instance with the earliest `started_at` (pin, see non-goals); becomes round-robin when login-flow state lands |
| :443, other SNI | names → owners → instances | that instance's `tls_addr`; close if unowned (as an unrouted SNI closes today) |
| :80 | `Host` → names (custom domains only, as `LookupCustom`) | `http_addr` of the owner; close otherwise |
| :7000 | none | live instance with the fewest `sessions` (ties: earliest `started_at`) |

Forwarding is the existing `pump` with a different far end: dial the target,
write a PROXY v2 header carrying the client's real address (the edge's own
listeners accept PROXY v2 behind `PIPER_EDGE_PROXY_PROTOCOL=1`, so a cloud
balancer in front can preserve it), replay the bytes already consumed while
peeking, then splice both ways. Metrics reuse `ConnAccepted`/`ConnRouted`/
`ConnUnrouted` with the listener label, plus one new counter for backend
dial failures labelled by listener.

### Failure handling

- **Dial refused / timed out (2 s).** Mark the instance dead in memory,
  delete its row (cascading its ownership), and: on :7000 retry the next
  candidate once; on :443/:80 close (the owner is unique; the agent will
  reconnect and a new owner row will arrive). One retry, never a loop.
- **Owner row names a dead instance.** Same as above, discovered on dial.
- **Postgres unreachable.** Keep serving from the in-memory tables; the
  names cache stops expiring (serve stale rather than nothing); log once on
  loss and once on recovery. New agents cannot enroll during the outage
  anyway, because the relay needs the store to authenticate them.
- **Listener connection lost.** Reconnect with backoff; full resync on
  return.

## Configuration

`piper-relay` gains one variable:

| Env | Meaning |
|---|---|
| `PIPER_RELAY_ADVERTISE_HOST` | host edges dial; default first non-loopback IPv4 |
| `PIPER_RELAY_PROXY_PROTOCOL=1` | existing; required on a relay behind an edge |

`piper-edge` has its own prefix, so a relay env file cannot be handed to it:

| Env | Meaning |
|---|---|
| `PIPER_EDGE_DB_URL` | required, same database as the relays |
| `PIPER_EDGE_APEX` | required; recognises `api.<apex>` |
| `PIPER_EDGE_TLS_ADDR` / `HTTP_ADDR` / `TUNNEL_ADDR` | public listeners; default `:443`, `:80`, `:7000` |
| `PIPER_EDGE_PROXY_PROTOCOL=1` | only when a PROXY-speaking balancer sits in front |
| `PIPER_EDGE_OPS_ADDR`, `PIPER_EDGE_METRICS`, `PIPER_EDGE_LOGS` | ops listener, same semantics as the relay's |

Nothing else.

## Packaging

`piper-edge` joins the release matrix beside `piper-relay`: a goreleaser
build entry (`CGO_ENABLED=0`, same platforms) and a second `dockers_v2`
image, `ghcr.io/piperbox/piper-edge`, built from the same minimal base. Like
the relay it is container-only — never apt or brew. `make build` and `make
cross` cover it because both build `./...`. The binary list in `CLAUDE.md`
gains the entry (and loses the stale "relay not built yet").

## Deployment shapes

**Single host (Hetzner compose).** One `piper-edge` service on the host
network owning the public :443/:80/:7000; `piper-relay` scaled to two or
more on the bridge network, each on its own container IP,
`PIPER_RELAY_PROXY_PROTOCOL=1`; Postgres as today. The edge is the only
thing with published ports. Adding a relay is `docker compose up -d --scale
relay=3`; nothing is configured.

**Kubernetes.** `piper-edge` Deployment behind a TCP Service that gets the
public IP (or a cloud NLB with PROXY protocol, `externalTrafficPolicy: Local`
otherwise). `piper-relay` Deployment with `PIPER_RELAY_ADVERTISE_HOST` from the
downward API `status.podIP` and a NetworkPolicy admitting only edge pods and
other relays (for the control hop). An L7 ingress terminates `api.<apex>`
with a cert-manager cert and routes to the relays' :8080 Service, which is
already plain HTTP written to be fronted with TLS; DNS points `api.<apex>` at
the ingress and the wildcard at the edge. The edge's own `api.<apex>` rule is
then never exercised. Prometheus scrapes each pod's ops listener.

The L7 ingress must not take the wildcard: routing per hostname to the owning
pod is the dynamic-map problem this design avoids, and passthrough for
box-held certs needs L4 anyway.

## Testing

- **Store.** Owner upsert/conditional clear, instance liveness predicate and
  cascade, and that each mutating method fires its channel (a test LISTENer
  on `relaytest.DSN`).
- **Edge resolver, in isolation.** Table-driven over an in-memory snapshot:
  precedence of the three name lookups, `api.<apex>` pinning to the earliest
  instance, least-sessions placement with tie-break, dead-instance eviction
  on dial failure, single retry on :7000 and none on :443. No sockets.
- **Two relays behind one edge, in-process.** Extends `startTestRelay`: two
  relays on ephemeral ports sharing one test database, one edge in front,
  two fake agents dialled through the edge. Asserts: each agent's tunnel
  landed on a different relay (least-sessions), a TLS passthrough connection
  for each agent reaches the right relay with the client address intact
  (PROXY v2), a control call for an agent lands on the non-owner relay and is
  answered through the hop, a parked webhook is drained by the owner after
  NOTIFY, and killing one relay's session makes its agent reconnect and the
  owner row move. This is the test that earns the design its keep.
- **e2e.** The existing loopback relay e2e runs unchanged (no edge in front);
  no new Docker dependency.

## Documentation and tracking

- `docs/runbooks/relay-deploy.md`: replace the "still exactly one replica"
  bullets with a **Scale out** section — the edge + relays shape, the compose
  example, the Kubernetes notes, the client-IP chain, the login pin, and what
  still drops on restart until graceful drain lands.
- `PROGRESS.md`: one line under Plan 2 with the issue number.
- GitHub: epic `[relay] scale out behind a reverse proxy` with three
  children — this spec, login-flow state in Postgres, graceful drain. The
  "per-process background work" concern from the Postgres design is closed
  in the epic body: the sweeper already drains local sessions only.

## Follow-ups (not this child)

1. **Login-flow state in Postgres** — flows and rate buckets with TTL; then
   the edge's `api.<apex>` rule becomes round-robin.
2. **Graceful drain** — on SIGTERM a relay stops accepting :7000, tells each
   agent to reconnect now, waits for sessions to leave; an edge flips
   readiness off and keeps splices alive until they end. Needs a small agent
   change. Turns N replicas into zero-drop deploys.
3. Possible later: SO_REUSEPORT handoff for single-host edge upgrades; a
   shared secret on internal listeners; a "relay with :7000 off" flag if API
   handling ever needs separate scaling.
