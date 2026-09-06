# Kubernetes rollout readiness (#557)

The scale-out epic (#524) left `piper-relay` and `piper-edge` in a shape
that runs on Kubernetes — the runbook's own "Kubernetes" paragraph describes
it — but not one an orchestrator can roll safely. A readiness audit on
2026-09-06 filed the gaps as #552–#556 and re-opened the deferred edge half
of graceful drain, #534. They are small and share one test harness, one
runbook section and one review, so they ship as one branch with one commit
per issue. This spec fixes the decisions; the plan carries the steps.

## Goals

- A rolling update of the relay Deployment is zero-drop *because* the
  orchestrator can see when a replacement is in the pool, not by luck of
  timing.
- A rolling update of the edge Deployment keeps in-flight splices alive for
  a bounded grace and stops taking new connections first.
- The images and binary start on a `restricted`-profile cluster with
  `readOnlyRootFilesystem` without workarounds, as far as each binary's port
  requirements allow.
- Concurrent first start against an empty database does not crash-loop.
- The runbook names every manifest knob the above depends on.

## Non-goals

- In-tree manifests or a Helm chart.
- A shared secret on the internal `:8080` control hop.
- `PIPER_RELAY_ZONE` from node labels (the runbook's workaround stands).
- Running `piper-edge` as non-root in the image (see #554 below).

## Readiness and liveness on the ops listener (#552, #534)

Both binaries already have an infra-only ops listener (`NewOpsHandler`,
`internal/relay/ops.go`) that serves `/metrics` and `/logs` behind two
toggles. It gains two unauthenticated routes:

- `GET /livez` — always 200. It proves the process answers and nothing
  else.
- `GET /readyz` — 200 while the process should receive new work, 503
  otherwise, with a one-word body naming the state (`starting`, `ready`,
  `draining`).

For the relay, ready begins at the first successful `UpsertInstance` in
`Instance.heartbeat` — the moment an edge can see the row — and ends at
`MarkDraining`. For the edge, ready begins when the listeners are bound and
ends on SIGTERM.

Neither probe depends on Postgres being reachable. A relay whose database is
down still splices every tunnel it holds; restarting it would drop those
sessions and the replacement could not register. The edge evicts a relay
whose heartbeat has stopped through the 15 s `instanceTTL` on its own, and
that is the right layer for it. A DB outage is an alert, not a restart.

**Bind rule.** Today the ops listener binds only when `PIPER_RELAY_METRICS`
or `PIPER_RELAY_LOGS` is `1`, and the default address is `127.0.0.1:9090`.
The e2e harness runs an edge and two relays on one host with the default,
so binding unconditionally would clash. The listener now binds when it is
*configured*: the address is set explicitly, or either toggle is on. A
Kubernetes manifest must set `PIPER_RELAY_OPS_ADDR=:9090` (likewise
`PIPER_EDGE_OPS_ADDR`) for the kubelet to reach it, which is what enables
the probes with no extra flag. `/readyz` and `/livez` are always registered
on a bound listener; `/metrics` and `/logs` keep their toggles.

Implementation: a small `Readiness` type in `internal/relay/ops.go` with
`SetReady()`, `SetDraining()` and `Ready() bool` on an atomic, passed to
`NewOpsHandler`; `Instance` holds one and flips it in `heartbeat` (first
success) and `MarkDraining`; the edge holds one and flips it in `serve` and
on ctx cancel. `NewOpsHandler(nil, nil, r)` serves only the probes.

## Edge drain on SIGTERM (#534)

`ServeEdge` today returns on ctx cancel and the deferred listener close
severs everything. The new order, in `edge.serve`:

1. On ctx cancel: `readiness.SetDraining()` — `/readyz` is 503 from here.
2. Close the three listeners. New connections are refused; the Service has
   already stopped sending them if the runbook's `preStop` sleep is in place.
3. Wait up to `edgeDrainTimeout` (20 s, a package var so tests tick fast)
   for the live connection count to reach zero. Every `handle` goroutine
   increments a `sync.WaitGroup`-style counter on entry and decrements on
   return; `forward` already blocks until either direction of the splice
   ends.
4. Exit. Whatever is still open is cut by process exit.

Tunnel forwards (`:7000`) never end on their own, so an edge carrying any
tunnel always waits the full 20 s and cuts them at the deadline. That is
intended: during the grace, HTTP through those tunnels still works, and the
relays behind are untouched, so the agent's other session (#530) serves
throughout. Agents redial and land on the surviving edge pod through the
Service. The runbook documents `terminationGracePeriodSeconds: 30` for the
edge and `stop_grace_period: 30s` for the compose edge.

`ServeEdge` returns `nil` after a completed drain, not `context.Canceled`,
and `cmd/piper-edge` logs how many connections were cut.

## Vestigial data dir (#553)

`cmd/piper-relay/main.go` drops the `PIPER_RELAY_DATA_DIR` read and mkdir;
`Dockerfile.relay` drops the `ENV` and `VOLUME` lines. The runbook's compose
examples mount certs at `/etc/piper-relay/certs` instead of the former data
dir. No compatibility for the removed env var (pre-1.0 policy).

## Non-root image, relay only (#554)

> **Amended after review, 2026-09-06.** Not shipped. The compose layout
> bind-mounts certbot's `/etc/letsencrypt` (0700 root directories, 0600
> root private key) and a 0600 root GitHub App key; uid 65532 cannot read
> either, and both `LoadWildcardConfig` and `readAppKey` are fatal, so the
> Hetzner relays would crash-loop on the next version bump. Both images
> stay root. Non-root on Kubernetes is a `runAsUser: 65532` + `fsGroup` +
> sysctl recipe in the runbook, which works because Secret mounts honour
> `fsGroup`. #554 stays open for both halves, blocked on a host-side
> recipe for the compose mounts.

`Dockerfile.relay` moves to `gcr.io/distroless/static-debian12:nonroot`
(uid 65532). Behind an edge the relay has no privileged-port need, and the
GitHub App key check looks only at world bits, so a `0400` Secret mount
still passes. `Dockerfile.edge` stays root: the Hetzner edge is
host-networked, Docker refuses net sysctls under host networking, and a
non-root process with no file capabilities cannot bind `:443` there.
Flipping it would break the next Hetzner upgrade. #554 stays open for the
edge with that reason recorded; the runbook gives Kubernetes users the
`runAsUser` plus `net.ipv4.ip_unprivileged_port_start=0` recipe, which is
where non-root for the edge actually works.

## Schema apply under an advisory lock (#555)

`relay.Open` runs the schema inside one transaction that first takes
`pg_advisory_xact_lock(<fixed key>)`. Concurrent starters serialize; the
losers see every table present when their turn comes. The key is a package
constant. Test: drop the schema on `relaytest.DSN`, call `Open` from
several goroutines at once, assert all succeed and the tables exist.

## Runbook (#556)

The "Kubernetes" paragraph in `docs/runbooks/relay-deploy.md` becomes a
subsection covering: the topology already described; `PIPER_RELAY_OPS_ADDR`
/ `PIPER_EDGE_OPS_ADDR` at `:9090`; `readinessProbe`/`livenessProbe`
stanzas; `maxSurge: 1, maxUnavailable: 0` for the relay Deployment and why;
`terminationGracePeriodSeconds` 60 (relay) and 30 (edge) with a `preStop`
sleep on the edge; Secret `defaultMode: 0400` for the App key; the
non-root recipe for the edge; no transaction-mode PgBouncer; the one-shot
crash on concurrent first start no longer applies. "What still drops" loses
the edge-restart sentence.

## Testing

- `ops_test.go`: `/livez` 200; `/readyz` 503 → 200 → 503 across the
  `Readiness` transitions; a handler with no metrics and no ring still serves
  the probes.
- `instance_test.go`: readiness flips after the first heartbeat upsert and
  back on `MarkDraining`.
- `edge_test.go`: on cancel, a held splice survives until it ends within the
  grace; a forward still open at the (shortened) deadline is cut and counted;
  the listener refuses a new dial after cancel.
- `store_test.go`: concurrent `Open` on a dropped schema.
- `cmd/piper-relay/main_test.go`: the ops bind rule (address set ⇒ bind).
- e2e: unchanged; `make verify` green.

## Tracking

One PR, `Closes #552 #553 #555 #556 #534`, `Part of #554 #557`. #557 is
closed by hand once #554's edge remainder is either done or re-scoped.
