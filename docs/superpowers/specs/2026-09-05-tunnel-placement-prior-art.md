# Research: tunnel placement in Cloudflare Tunnel and its peers

Prior-art study for the scale-out epic (#524). The
[edge ownership design](2026-09-04-relay-edge-ownership-design.md) shipped
`piper-edge` and single-owner tunnels; #522 (login state) and #523 (graceful
drain) are in flight. Running N relays on a real orchestrator (Kubernetes,
ECS) is still the sore spot, and Piper's shape is close enough to Cloudflare
Tunnel that its choices are worth reading before designing further. This
document records what is publicly knowable about how Cloudflare places and
routes tunnels, what three comparable systems do, and which of those choices
carry over. It proposes two follow-up children for the epic and names what
not to copy.

Everything here comes from public docs, blog posts, the open-source
`cloudflared` client, and live DNS (2026-09-05). Cloudflare's edge is closed;
where a claim is inferred from the client's behaviour rather than stated,
it says so.

## Cloudflare Tunnel

### Placement is client-driven

`cloudflared` does not get told where to connect. It resolves one SRV record
and spreads its own connections:

| Fact | Value | Where it is visible |
|---|---|---|
| Discovery | `_v2-origintunneld._tcp.argotunnel.com` → `region1.v2.argotunnel.com` (priority 1), `region2.v2.argotunnel.com` (priority 2), port 7844 | live `dig`; `edgediscovery/allregions/discovery.go` |
| Addresses per region | 10 each, on separate prefixes (198.41.192.0/24, 198.41.200.0/24) | live `dig` |
| Connections per connector | 4 (`--ha-connections`, hidden, default 4) | `cmd/cloudflared/tunnel/cmd.go` |
| Spread rule | "Prefer to use addresses evenly across both regions"; random first pick when equal; each connection index gets a distinct address | `edgediscovery/allregions/regions.go` |
| Replicas per tunnel | up to 25 connectors, 100 connections | tunnel-availability doc |

A "region" is not a geography. Both prefixes are anycast, so each is an
independent failure domain, and the docs promise only that the four
connections land on "four different servers spread across at least two
distinct data centers".

### Every connection registers itself

The control stream on each connection calls
`registerConnection(auth, tunnelId, connIndex, options)` and gets back
`ConnectionDetails{uuid, locationName, tunnelIsRemotelyManaged}`, where
`locationName` is the airport code of the colo the connection landed in
(`tunnelrpc/proto/tunnelrpc.capnp`, `connection/control.go`).

Two behaviours matter for placement:

- **Duplicate rejection.** The edge refuses a second connection of the same
  tunnel on the same server. The client maps that error to "get a different
  address" (`supervisor/tunnel.go`, `DupConnRegisterTunnelError`). Spreading
  is enforced by rejection, not negotiated.
- **Rotate, then fall back.** Registration failures rotate the edge IP for
  that connection index; after `max-edge-addr-retries` the client drops from
  QUIC to HTTP/2.

Connection index 0 also pushes the local ingress config after registering;
the other three carry only traffic.

### Routing is a registry lookup plus one inter-node hop

Cloudflare's 2020 post says it plainly: "cloudflared connects to an Argo
Tunnel service running in Cloudflare's control plane. That service registers
your Tunnel and its connections." DNS never points at a colo; it points at
`<uuid>.cfargotunnel.com`, and the registry maps the UUID to live
connections.

A visitor lands on the nearest colo by anycast. If that colo holds a
connection for the tunnel it uses it ("cloudflared prefers to serve requests
using connections in the same data center", load-balancing doc). Otherwise
the request is forwarded over Cloudflare's backbone to the colo with the
geographically closest connection; on failure it "retries with other
replicas" and makes no promise about which. There is no traffic steering
among replicas at all; if you want round-robin or hashing you run separate
tunnels behind a Cloudflare Load Balancer, which then still cannot tell the
replicas of one tunnel apart.

Inside a colo, Unimog keeps a long-lived flow on one server through a
consistent-hash forwarding table with a second "previous owner" slot, so a
server can be drained without breaking its established connections. Every
server in a colo runs the tunnel service; there is no dedicated tunnel tier.

The store behind the registry, and whether the forwarding colo looks it up
or is pushed to, is not public.

### Zero-drop edge deploys come from redundancy, not drain

Cloudflare restarts edge servers at will. A connector survives because it
holds four sessions in two failure domains and the edge simply stops routing
to a dead connection. The only drain protocol in the client is
connector-initiated: on SIGTERM it calls `GracefulShutdown(gracePeriod)`
(default 30 s), stops taking new requests, and waits for in-flight ones
(`connection/control.go`, `waitForUnregister`). Nothing in the client
handles "the edge asked me to move".

## Three peers

| System | Who picks placement | Sessions per agent | Ownership state | Cross-node path |
|---|---|---|---|---|
| **Teleport proxy peering** (RFD 69) | agent, from the proxy list in the backend | `agent_connection_count`, default 1 | `ProxyID` + monotonic `Nonce`/`NonceID` on the agent's heartbeat record | non-owning proxy dials the owner over gRPC `DialNode` at its advertised `peer_listen_addr` |
| **Tailscale DERP** | client, by latency ("home" node) | 1 per region | none in a store; presence pushed as `PeerPresent`/`PeerGone` frames over mesh connections | nodes in a region are fully meshed, "forwarded only one hop", "no routing between regions" |
| **Kubernetes konnectivity** (apiserver-network-proxy) | none: agent connects to every server | one per server; server sends a `serverCount` header from leases and the agent redials through the LB until it holds one connection per unique server ID | none; each server knows its own agents | none; server picks a random local agent per request |

Teleport is the instructive one. Its previous mode (`agent_mesh`, every
agent to every proxy, konnectivity's shape) was replaced because of
"ephemeral port exhaustion between a NAT gateway and load balancer" and
"thundering herd when adding, removing, or restarting a proxy". The
replacement is Piper's design with the peer hop inside the proxies instead
of in a separate edge, and its RFD notes the trade: one connection per agent
"is more likely to lead to unavailability to a subset of agents during
network partitions and cluster upgrades", mitigated by raising
`agent_connection_count`.

## What carries over to Piper

Piper already has Cloudflare's essentials:

| Cloudflare | Piper today |
|---|---|
| registry of tunnel → live connections in the control plane | `agent_owners` in Postgres |
| registration on connect, removal on disconnect, server-side | `SetOwner` on register, conditional `ClearOwner`, heartbeat re-assertion |
| nearest colo looks up the owner and forwards | `piper-edge` looks up the owner and splices |
| presence pushed to routers | LISTEN/NOTIFY on `piper_owners` |
| dedicated tunnel port (7844) | :7000 |
| client rotates on registration failure | agent redials from 1 s backoff |

The differences are all about redundancy. Cloudflare holds four sessions per
connector; Piper holds one. That single fact is why Cloudflare needs no
edge-initiated drain and why relay rollouts on Kubernetes/ECS hurt for Piper:
each relay termination is the agent's only link going away.

### Proposed follow-up 4: two sessions per agent on two relays (#530)

> Designed in
> [`2026-09-05-two-sessions-per-agent-design.md`](2026-09-05-two-sessions-per-agent-design.md),
> which departs from the sketch below in one place: the edge peeks the agent
> base from a routing preface frame instead of relying on rejection alone,
> because deterministic placement never converges under rejection.

Let each agent keep two tunnel sessions, placed on different relays. A
rolling restart then drops one of two, the edge routes to whichever owner
row survives, and the agent redials the lost one in the background. #523
shrinks to "stop accepting on :7000, close the listener, exit within the
orchestrator's stop timeout"; the agent-side "reconnect now" message becomes
optional.

What changes:

- `agent_owners` keyed on `(agent_name, instance_id)`; `SetOwner` inserts,
  `ClearOwner` deletes its own row as today. `OwnerOf` returns a set.
- Edge :443/:80 choose among live owners (fewest sessions, then earliest
  `started_at`, same rule as :7000). Edge :7000 placement excludes relays
  that already own the dialling agent, which needs the agent name before
  placement: the agent sends it in the first bytes (the tunnel hello already
  carries the token; the edge would peek the base the same way it peeks SNI)
  or the relay rejects a duplicate and the agent redials, cloudflared-style.
  The rejection path is simpler and keeps the edge blind to tokens; pick it
  unless the double dial proves noisy.
- Relay: `router.Register` for an agent it already holds returns a duplicate
  error instead of replacing, so a redial after a network blip lands on
  another relay. The control hop and webhook drain pick any local session;
  both already work per relay.
- Agent: a second dial loop, and a `PIPER_AGENT_TUNNEL_SESSIONS` knob
  defaulting to 2 so a single-relay loopback e2e keeps working with 1.

Cost: 2× yamux sessions per relay, and ownership moves from "the truth is
whoever registered last" to "the truth is every live row". The stale-session
close on `piper_owners` (a relay closing its session when another instance
takes ownership) must go, since two owners is now normal.

Schedule it after #522 and #523, not inside them: #523 is still needed for
the single-session case and for the edge's own readiness flip, and #522 is
independent.

### Proposed follow-up 5: failure domains in placement (#531)

Once an agent has two sessions, make the edge prefer a relay in a different
zone for the second one. One nullable `zone` column on `relay_instances`,
filled from `PIPER_RELAY_ZONE` (Kubernetes: the downward API cannot expose
the node zone directly, so a node label copied by an init step or the
topology-aware scheduler's hint; ECS: `AWS_AVAILABILITY_ZONE` is available
to tasks), and a tie-break in the :7000 placement rule. Nothing else. This
is Cloudflare's two-prefix split at Piper's scale.

## What not to copy

- **Inter-relay forwarding.** Cloudflare, Teleport, and DERP all forward
  between nodes because their nodes are the public surface. Piper has one
  public address, and the edge already costs the same single hop
  (edge → relay) that colo → colo costs Cloudflare. Adding a relay mesh would
  buy nothing and add the operational surface the edge design rejected.
- **Client-side discovery (SRV).** The edge is Piper's discovery. Handing
  agents a relay list would put the placement rule in every agent version and
  reopen the "which address do I advertise" problem on orchestrators.
- **Steering among an agent's sessions.** Cloudflare offers none, and the
  fewest-sessions rule is enough for two.

## Orchestrator notes

The current spec maps onto ECS and Kubernetes without changes:

- **ECS, awsvpc mode.** Each relay task gets an ENI IP that the advertise
  default (first non-loopback IPv4) picks up. The edge runs as a service
  behind an NLB with PROXY protocol v2 on all three listeners and
  `PIPER_EDGE_PROXY_PROTOCOL=1`. The task `stopTimeout` (default 30 s, max
  120 s) is the budget #523 must fit.
- **Kubernetes.** As written in the edge design: edge Deployment behind a
  TCP Service or NLB, relays with `PIPER_RELAY_ADVERTISE_HOST` from
  `status.podIP`, NetworkPolicy admitting only edges and relays.
  `terminationGracePeriodSeconds` is the drain budget.

With follow-up 4, `maxUnavailable: 1` on the relay rollout (or ECS
`minimumHealthyPercent`) is enough to make relay deploys zero-drop: no agent
loses both sessions while the replaced relay is out, provided placement put
them on different relays.

## Sources

- Cloudflare, [Tunnel availability and failover](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/configure-tunnels/tunnel-availability/)
- Cloudflare, [Argo Tunnels that live forever](https://blog.cloudflare.com/argo-tunnels-that-live-forever/) (2020)
- Cloudflare, [Argo Tunnel: A Private Link to the Public Internet](https://blog.cloudflare.com/argo-tunnel/) (2018)
- Cloudflare, [Getting Cloudflare Tunnels to connect to the Cloudflare Network with QUIC](https://blog.cloudflare.com/getting-cloudflare-tunnels-to-connect-to-the-cloudflare-network-with-quic/) (2021)
- Cloudflare, [Load balancing tunnel endpoints](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/routing-to-tunnel/lb/)
- Cloudflare, [Tunnel run parameters](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/configure-tunnels/run-parameters/)
- Cloudflare, [Unimog: Cloudflare's edge load balancer](https://blog.cloudflare.com/unimog-cloudflares-edge-load-balancer/) (2020)
- [cloudflare/cloudflared](https://github.com/cloudflare/cloudflared): `tunnelrpc/proto/tunnelrpc.capnp`, `edgediscovery/allregions/{discovery,regions}.go`, `supervisor/{supervisor,tunnel}.go`, `connection/control.go`, `cmd/cloudflared/tunnel/cmd.go`
- Teleport, [RFD 69: Proxy Peering](https://github.com/gravitational/teleport/blob/master/rfd/0069-proxy-peering.md)
- Tailscale, [`tailscale.com/derp` package docs](https://pkg.go.dev/tailscale.com/derp) and [DERP servers](https://tailscale.com/kb/1232/derp-servers)
- [kubernetes-sigs/apiserver-network-proxy](https://github.com/kubernetes-sigs/apiserver-network-proxy): `pkg/agent/clientset.go`, `pkg/server/backend_manager.go`
