# Relay ops endpoints: metrics + log export

**Date:** 2026-07-26
**Status:** Approved design, pre-implementation

## Problem

Scaling and orchestrating `piper-relay` (autoscaling decisions, health dashboards,
alerting) needs machine-readable runtime metrics. Debugging a deployed relay needs
its recent logs without SSH access. Both must be reachable by our infrastructure
only — when the relay's public listeners (:443/:80/:7000) sit on a public VPC,
these operational endpoints must not be exposed alongside them.

## Decision summary

A separate **ops listener** inside `piper-relay` — plain HTTP, structurally
disconnected from the SNI dispatcher, so no public hostname can ever route to it.
Network placement is the fence: it binds loopback by default; production binds a
private VPC IP and the security group keeps the public out. No auth layer.

Metrics are Prometheus exposition format served by
`github.com/prometheus/client_golang` (pure Go, `CGO_ENABLED=0`-safe). Logs are a
bounded in-memory ring buffer snapshot.

## Configuration

| Env var | Default | Meaning |
| --- | --- | --- |
| `PIPER_RELAY_OPS_ADDR` | `127.0.0.1:9090` | Bind address for the ops listener |
| `PIPER_RELAY_METRICS` | unset (off) | `1` enables `GET /metrics` |
| `PIPER_RELAY_LOGS` | unset (off) | `1` enables `GET /logs` and the log tee |

Both endpoints are off by default. The ops listener starts only if at least one
toggle is set; with neither, nothing binds. A disabled endpoint on a running
listener returns `404`. Startup logs the ops bind address alongside the other
listeners so a misconfigured public bind is visible.

Failure to bind the ops listener at startup is **fatal**, like the control API —
a silently-missing metrics endpoint would defeat its orchestration purpose.

## Endpoints

### `GET /metrics`

Prometheus exposition via `promhttp` on a **private registry** (not the library's
global default), keeping tests hermetic and avoiding duplicate-registration
panics. Namespace `piper_relay_`.

**Topology gauges** — `GaugeFunc`s reading live router state at scrape time (no
inc/dec bookkeeping to drift):

- `piper_relay_agents_connected` — registered tunnel sessions
- `piper_relay_hostnames_routed` — app hostnames in the router
- `piper_relay_custom_domains_routed` — custom domains in the router

The `Router` grows one `Counts()` method; nothing else about it changes.

**Traffic counters** — incremented in the accept/route paths:

- `piper_relay_conns_accepted_total{listener="tls"|"http"|"tunnel"}`
- `piper_relay_conns_routed_total{listener="tls"|"http"}`
- `piper_relay_conns_unrouted_total{listener="tls"|"http"}` — unknown SNI/Host
- `piper_relay_active_streams` — gauge of currently-proxied connections
  (inc on splice start, dec on end)

**Free collectors:** client_golang's Go runtime (goroutines, GC, memory) and
process (CPU, fds) collectors — the generic scaling signals.

Out of scope for v1 (explicitly deferred): bytes-throughput counters (would wrap
the `pump` hot path) and webhook delivery stats.

### `GET /logs`

Last 1000 log lines, oldest-first, `text/plain`. When `PIPER_RELAY_LOGS=1`,
`main` tees the stdlib logger — `log.SetOutput(io.MultiWriter(os.Stderr, ring))`
— into a fixed-size, mutex-guarded ring buffer. stderr/journald behavior is
unchanged. When off, no ring buffer is allocated and no tee is installed: zero
cost, zero exposure.

No follow/streaming; consumers poll. No log-level changes — the relay has no
leveled logging and adding it is out of scope.

## Architecture

New code lives in `internal/relay` (an `ops.go` or similar): a `Metrics` struct
owning the private registry and instruments, plus the ring buffer and the ops
`http.Handler`. `main` constructs it and passes it into `relay.Serve` as one
added parameter, matching how `Store`/`Router` already flow. A nil `*Metrics` is
a no-op everywhere it is threaded, so existing tests need no edits.

Existing surfaces — control API, public TLS/HTTP, tunnel listener — are
untouched.

## Security posture

- Isolation is structural (separate listener, never SNI-routed) plus network
  placement (bind address + firewall). No bearer token, by decision.
- Logs can contain tenant hostnames; acceptable because the endpoint is
  toggle-gated, off by default, and infra-only by placement.

## Testing

TDD, per repo discipline:

- **Ring buffer:** wraps at capacity, ordered oldest-first, safe under
  concurrent writes.
- **Ops handler:** `/metrics` scrape contains expected gauge values against fake
  router state; `/logs` returns teed lines when enabled; toggle matrix
  (neither/either/both) — disabled endpoints 404, neither ⇒ no listener.
- **Traffic counters:** drive fake conns through `handlePublic`/`acceptHTTP`
  (existing harness in `server_test.go`) and assert counter deltas via the
  registry.
- `make verify` proves the cross-compile survives the new dependency.
