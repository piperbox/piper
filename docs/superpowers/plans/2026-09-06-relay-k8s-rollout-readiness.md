# Kubernetes rollout readiness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make a Kubernetes rolling update of `piper-relay` and `piper-edge` observable and safe: readiness/liveness probes on both, an edge drain on SIGTERM, a non-root relay image, no vestigial data dir, a race-free first start, and a runbook that names every knob.

**Architecture:** A small `Readiness` state (starting → ready → draining) lives in `internal/relay/ops.go` and is served as `/readyz` + `/livez` by the existing ops handler. The relay flips it from `Instance.heartbeat` and `MarkDraining`; the edge flips it in `serve` and drains live connections on ctx cancel the way `Drain` does for the relay. Everything else is a one-file change.

**Tech Stack:** Go, `net/http`, `sync/atomic`, pgx against Postgres (tests provision one via `relaytest.DSN`: Docker or `PIPER_TEST_POSTGRES_URL`), distroless images built by goreleaser.

Spec: [`docs/superpowers/specs/2026-09-06-relay-k8s-rollout-readiness-design.md`](../specs/2026-09-06-relay-k8s-rollout-readiness-design.md). Epic #557.

## Global Constraints

- `CGO_ENABLED=0` everywhere; no cgo.
- Module path `github.com/piperbox/piper`.
- Pre-1.0: no compat shim for the removed `PIPER_RELAY_DATA_DIR`.
- One commit per issue, conventional-commit style, trailer `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`, body `Part of #N`.
- `/livez` is always 200 with body `ok`. `/readyz` is 503 with body `starting` or `draining`, 200 with body `ready`.
- Ops listener binds when `PIPER_RELAY_OPS_ADDR` / `PIPER_EDGE_OPS_ADDR` is set explicitly **or** metrics/logs is on. Default address stays `127.0.0.1:9090`.
- Edge drain deadline `edgeDrainTimeout = 20 * time.Second` (package var). Relay drain numbers unchanged.
- `Dockerfile.edge` stays root. Only `Dockerfile.relay` moves to `:nonroot`.
- Postgres tests skip cleanly when no Postgres can be provisioned; never make them required.
- `make verify` must pass before the PR.

---

### Task 1: `Readiness` state and `/readyz` + `/livez` on the ops handler (#552, shared with #534)

**Files:**
- Modify: `internal/relay/ops.go` (add `Readiness`; extend `NewOpsHandler`)
- Modify: `internal/relay/ops_test.go:16-22` (`scrapeMetrics` passes the new argument)
- Modify: `cmd/piper-relay/main.go:301` and `cmd/piper-edge/main.go:88` (callers of `NewOpsHandler` — pass `nil` for now; Tasks 2 and 3 wire real values)

**Interfaces:**
- Produces:
  ```go
  type Readiness struct{ state atomic.Int32 }
  func (r *Readiness) SetReady()        // starting → ready; no-op once draining
  func (r *Readiness) SetDraining()     // any → draining, final
  func (r *Readiness) Ready() bool
  func (r *Readiness) String() string   // "starting" | "ready" | "draining"
  func NewOpsHandler(m *Metrics, ring *LogRing, r *Readiness) http.Handler
  ```
  A nil `r` registers no probe routes (keeps `scrapeMetrics` and any other caller that only wants metrics unchanged).

- [ ] **Step 1: Write the failing tests** — append to `internal/relay/ops_test.go`:

```go
func probe(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec.Code, strings.TrimSpace(rec.Body.String())
}

func TestReadinessTransitions(t *testing.T) {
	var r Readiness
	if r.Ready() || r.String() != "starting" {
		t.Fatalf("fresh: ready=%v state=%q", r.Ready(), r.String())
	}
	r.SetReady()
	if !r.Ready() || r.String() != "ready" {
		t.Fatalf("after SetReady: ready=%v state=%q", r.Ready(), r.String())
	}
	r.SetDraining()
	if r.Ready() || r.String() != "draining" {
		t.Fatalf("after SetDraining: ready=%v state=%q", r.Ready(), r.String())
	}
	r.SetReady() // draining is final
	if r.Ready() {
		t.Fatal("SetReady reopened a draining instance")
	}
}

func TestOpsHandlerProbes(t *testing.T) {
	var r Readiness
	h := NewOpsHandler(nil, nil, &r) // probes alone, no metrics, no logs

	if code, body := probe(t, h, "/livez"); code != 200 || body != "ok" {
		t.Fatalf("/livez = %d %q", code, body)
	}
	if code, body := probe(t, h, "/readyz"); code != 503 || body != "starting" {
		t.Fatalf("/readyz before ready = %d %q", code, body)
	}
	r.SetReady()
	if code, body := probe(t, h, "/readyz"); code != 200 || body != "ready" {
		t.Fatalf("/readyz ready = %d %q", code, body)
	}
	r.SetDraining()
	if code, body := probe(t, h, "/readyz"); code != 503 || body != "draining" {
		t.Fatalf("/readyz draining = %d %q", code, body)
	}
	if code, _ := probe(t, h, "/metrics"); code != 404 {
		t.Fatalf("/metrics with nil Metrics = %d, want 404", code)
	}
}

func TestOpsHandlerWithoutReadinessHasNoProbes(t *testing.T) {
	h := NewOpsHandler(NewMetrics(NewRouter()), nil, nil)
	if code, _ := probe(t, h, "/readyz"); code != 404 {
		t.Fatalf("/readyz with nil Readiness = %d, want 404", code)
	}
}
```

Also change `scrapeMetrics` to call `NewOpsHandler(m, nil, nil)`.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/relay -run 'TestReadiness|TestOpsHandler' 2>&1 | head -20`
Expected: compile error — `undefined: Readiness`, wrong argument count.

- [ ] **Step 3: Implement** in `internal/relay/ops.go`. Add `"sync/atomic"` to imports and, above `NewOpsHandler`:

```go
// Readiness is the probe state an orchestrator reads: starting until the
// process can take new work, ready while it should, draining once it has
// been told to stop and forever after (#552, #534). It says nothing about
// Postgres: a relay whose database is down still splices every tunnel it
// holds, and the edge's 15 s instance TTL — not a restart — is what retires
// a relay that stopped heartbeating.
type Readiness struct{ state atomic.Int32 }

const (
	readyStarting int32 = iota
	readyReady
	readyDraining
)

// SetReady moves starting → ready. A draining instance stays draining.
func (r *Readiness) SetReady() { r.state.CompareAndSwap(readyStarting, readyReady) }

// SetDraining is final: from here /readyz is 503 until the process exits.
func (r *Readiness) SetDraining() { r.state.Store(readyDraining) }

// Ready reports whether /readyz answers 200.
func (r *Readiness) Ready() bool { return r.state.Load() == readyReady }

func (r *Readiness) String() string {
	switch r.state.Load() {
	case readyReady:
		return "ready"
	case readyDraining:
		return "draining"
	}
	return "starting"
}
```

Change `NewOpsHandler`:

```go
// NewOpsHandler serves the infra-only ops surface: /metrics when m is
// non-nil, /logs when ring is non-nil, and the kubelet-shaped probes /livez
// and /readyz when r is non-nil. A nil argument means that endpoint is
// toggled off and 404s — the caller decides exposure purely by what it
// constructs, so there is no config to consult here.
func NewOpsHandler(m *Metrics, ring *LogRing, r *Readiness) http.Handler {
	mux := http.NewServeMux()
	if m != nil {
		mux.Handle("GET /metrics", promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{}))
	}
	if ring != nil {
		mux.HandleFunc("GET /logs", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			for _, line := range ring.Lines() {
				fmt.Fprintln(w, line)
			}
		})
	}
	if r != nil {
		mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprintln(w, "ok")
		})
		mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			if !r.Ready() {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
			fmt.Fprintln(w, r.String())
		})
	}
	return mux
}
```

Update the two `cmd` callers to `relay.NewOpsHandler(metrics, ring, nil)` so the tree compiles (Tasks 2 and 3 replace the nil).

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/relay -run 'TestReadiness|TestOpsHandler|TestMetrics' && go build ./...`
Expected: PASS, build clean.

- [ ] **Step 5: Commit**

```bash
git add internal/relay/ops.go internal/relay/ops_test.go cmd/piper-relay/main.go cmd/piper-edge/main.go
git commit -m "feat(relay): Readiness state with /readyz and /livez on the ops handler

Part of #552, #534

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 2: Relay flips readiness from the heartbeat; ops listener bind rule (#552)

**Files:**
- Modify: `internal/relay/instance.go` (`Instance.Ready` field; flip in `heartbeat` and `MarkDraining`)
- Modify: `internal/relay/instance_test.go` (new test)
- Modify: `cmd/piper-relay/main.go:288-305` (bind rule; pass `inst.Ready`)
- Modify: `cmd/piper-relay/main_test.go` (bind-rule test)

**Interfaces:**
- Consumes: `Readiness` from Task 1.
- Produces: `Instance.Ready *Readiness` (non-nil from `NewInstance`); `func opsWanted(addrSet, metrics, logs bool) bool` in `cmd/piper-relay`.

- [ ] **Step 1: Write the failing tests.** Append to `internal/relay/instance_test.go`:

```go
// /readyz follows the pool row: 503 until the first heartbeat upsert lands
// (an edge cannot see the relay before that), 200 until MarkDraining, then
// 503 for good (#552).
func TestHeartbeatFlipsReadiness(t *testing.T) {
	st := openTestStore(t)
	router := NewRouter()
	inst, err := NewInstance("127.0.0.1", ":443", ":80", ":7000", ":8080")
	if err != nil {
		t.Fatal(err)
	}
	if inst.Ready == nil || inst.Ready.Ready() {
		t.Fatal("fresh instance is ready before any heartbeat")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { inst.heartbeat(ctx, st, router); close(done) }()
	waitCond(t, 3*time.Second, "ready after first heartbeat", inst.Ready.Ready)
	inst.MarkDraining()
	if inst.Ready.Ready() {
		t.Fatal("still ready after MarkDraining")
	}
	cancel()
	<-done
}
```

Append to `cmd/piper-relay/main_test.go`:

```go
func TestOpsWanted(t *testing.T) {
	cases := []struct {
		addrSet, metrics, logs, want bool
	}{
		{false, false, false, false},
		{true, false, false, true},
		{false, true, false, true},
		{false, false, true, true},
		{true, true, true, true},
	}
	for _, c := range cases {
		if got := opsWanted(c.addrSet, c.metrics, c.logs); got != c.want {
			t.Errorf("opsWanted(%v,%v,%v) = %v, want %v", c.addrSet, c.metrics, c.logs, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/relay -run TestHeartbeatFlipsReadiness 2>&1 | head -5; go test ./cmd/piper-relay -run TestOpsWanted 2>&1 | head -5`
Expected: `inst.Ready undefined`; `undefined: opsWanted`.

- [ ] **Step 3: Implement.** In `internal/relay/instance.go`:

Add to the `Instance` struct, after `Zone`:
```go
	// Ready is what /readyz reports: set on the first successful heartbeat
	// upsert, cleared for good by MarkDraining (#552).
	Ready *Readiness
```
In `NewInstance`, construct with `inst := &Instance{ID: ..., StartedAt: ..., Ready: &Readiness{}}`.

`MarkDraining` becomes:
```go
func (i *Instance) MarkDraining() {
	i.draining.Store(true)
	i.Ready.SetDraining()
}
```

In `heartbeat`'s `beat` closure, flip on success:
```go
		if err := st.UpsertInstance(i.row(agents)); err != nil {
			log.Printf("relay: heartbeat: %v", err)
		} else {
			i.Ready.SetReady()
		}
```

In `cmd/piper-relay/main.go`, add near `atoiOr`:
```go
// opsWanted decides whether the ops listener binds at all: when its address
// was set explicitly, or when either endpoint toggle is on. Default-address
// binding would clash where several relays share a host (the e2e harness),
// so probes come for free only once an operator has pointed the listener
// somewhere — which a Kubernetes manifest must do for the kubelet anyway.
func opsWanted(addrSet, metrics, logs bool) bool { return addrSet || metrics || logs }
```
Replace the ops block (`opsAddr := ...` through the `if metricsOn || logsOn {` goroutine) with:
```go
	opsAddr, opsAddrSet := os.LookupEnv("PIPER_RELAY_OPS_ADDR")
	if opsAddr == "" {
		opsAddr, opsAddrSet = "127.0.0.1:9090", false
	}
	metricsOn := os.Getenv("PIPER_RELAY_METRICS") == "1"
	logsOn := os.Getenv("PIPER_RELAY_LOGS") == "1"
	var metrics *relay.Metrics
	var ring *relay.LogRing
	if metricsOn {
		metrics = relay.NewMetrics(router)
	}
	if logsOn {
		ring = relay.NewLogRing(1000)
		log.SetOutput(io.MultiWriter(os.Stderr, ring))
	}
	if opsWanted(opsAddrSet, metricsOn, logsOn) {
		opsHandler := relay.NewOpsHandler(metrics, ring, inst.Ready)
		go func() {
			log.Printf("piper-relay: ops endpoint %s (metrics=%v logs=%v probes=/readyz,/livez)", opsAddr, metricsOn, logsOn)
			srv := &http.Server{Addr: opsAddr, Handler: opsHandler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
			if err := srv.ListenAndServe(); err != nil {
				log.Fatalf("ops endpoint: %v", err)
			}
		}()
	}
```
`inst` is created earlier in `main` (`newInstanceFromEnv`), so it is in scope; the ops block must stay after it.

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/relay -run 'TestHeartbeat|TestMarkDraining' && go test ./cmd/piper-relay`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/relay/instance.go internal/relay/instance_test.go cmd/piper-relay/main.go cmd/piper-relay/main_test.go
git commit -m "feat(relay): /readyz follows the pool row; ops listener binds when configured

Part of #552

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 3: Edge readiness and drain on SIGTERM (#534)

**Files:**
- Modify: `internal/relay/edge.go` (`ready`, `live` counter, `edgeDrainTimeout`, drain in `serve`)
- Modify: `internal/relay/edge_test.go` (two tests; `startEdge` passes `&Readiness{}`)
- Modify: `cmd/piper-edge/main.go` (bind rule, pass readiness, simplify the return check)
- Modify: `deploy/compose/relay/docker-compose.yml` (edge `stop_grace_period`)

**Interfaces:**
- Consumes: `Readiness` (Task 1); `drainTick` (existing, `internal/relay/drain.go`).
- Produces:
  ```go
  func ServeEdge(ctx context.Context, cfg EdgeConfig, st *Store, m *Metrics, r *Readiness) error // nil after a completed drain
  func newEdge(cfg EdgeConfig, st *Store, m *Metrics, r *Readiness) *edge
  var edgeDrainTimeout = 20 * time.Second
  // edge fields: ready *Readiness; live atomic.Int64
  ```

- [ ] **Step 1: Write the failing tests.** Append to `internal/relay/edge_test.go`. Before writing, check the real helper names with `grep -n '^func ' internal/relay/edge_test.go` — the enrollment helper and the `edgeRelay` struct's instance field are referenced below as `enrollTestAgent` and `r.inst`; use whatever the file actually defines.

```go
// serveEdgeForDrain runs the edge like startEdge but hands back the cancel
// and the serve result channel, which the drain tests need.
func serveEdgeForDrain(t *testing.T, st *Store) (EdgeConfig, *edge, context.CancelFunc, <-chan error) {
	t.Helper()
	cfg := EdgeConfig{Apex: "public.getpiper.co", TLSAddr: freeTCPAddr(t), HTTPAddr: freeTCPAddr(t), TunnelAddr: freeTCPAddr(t)}
	e := newEdge(cfg, st, NewEdgeMetrics(), &Readiness{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errc := make(chan error, 1)
	go func() { errc <- e.serve(ctx) }()
	waitCond(t, 5*time.Second, "edge ready", e.ready.Ready)
	return cfg, e, cancel, errc
}

// On cancel the edge flips /readyz, refuses new dials, and waits for the
// connections it is carrying — an idle :80 conn here — until they end (#534).
func TestEdgeDrainWaitsForLiveConnections(t *testing.T) {
	old := edgeDrainTimeout
	edgeDrainTimeout = 5 * time.Second
	t.Cleanup(func() { edgeDrainTimeout = old })

	st := openTestStore(t)
	cfg, e, cancel, errc := serveEdgeForDrain(t, st)

	held, err := net.Dial("tcp", cfg.HTTPAddr)
	if err != nil {
		t.Fatal(err)
	}
	waitCond(t, 2*time.Second, "edge sees the held conn", func() bool { return e.live.Load() == 1 })

	cancel()
	waitCond(t, 2*time.Second, "readiness flipped", func() bool { return !e.ready.Ready() })
	if c, err := net.DialTimeout("tcp", cfg.TLSAddr, 300*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("new dial accepted after cancel")
	}
	select {
	case err := <-errc:
		t.Fatalf("serve returned %v while a connection was still open", err)
	case <-time.After(300 * time.Millisecond):
	}
	held.Close()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("serve returned %v after a clean drain, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serve did not return once the last connection closed")
	}
}

// A :7000 forward never ends on its own; the drain cuts it at the deadline
// and serve still returns nil (#534).
func TestEdgeDrainCutsForwardsAtTheDeadline(t *testing.T) {
	old := edgeDrainTimeout
	edgeDrainTimeout = 500 * time.Millisecond
	t.Cleanup(func() { edgeDrainTimeout = old })

	st := openTestStore(t)
	cfg, e, cancel, errc := serveEdgeForDrain(t, st)
	r := startRelayBehindEdge(t, st, nil)
	en := enrollTestAgent(t, st, "box.public.getpiper.co")
	sess, _ := dialAgentThroughEdge(t, cfg, en)
	waitEdgeSessions(t, e, r.inst.ID, 1)

	start := time.Now()
	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("serve returned %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serve did not return at the drain deadline")
	}
	if took := time.Since(start); took < edgeDrainTimeout {
		t.Fatalf("serve returned after %s, before the %s deadline", took, edgeDrainTimeout)
	}
	// serve returning is the process-exit point in cmd/piper-edge; in-process
	// the forward is still spliced, so close the agent side and make sure
	// the handler goroutine ends (live drops to 0) rather than leaking.
	sess.Close()
	waitCond(t, 3*time.Second, "handler goroutines gone", func() bool { return e.live.Load() == 0 })
}
```

Update `startEdge` to call `newEdge(cfg, st, NewEdgeMetrics(), &Readiness{})`.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/relay -run TestEdgeDrain 2>&1 | head -10`
Expected: compile errors — `newEdge` arity, `e.ready`, `e.live`, `edgeDrainTimeout` undefined.

- [ ] **Step 3: Implement** in `internal/relay/edge.go`.

Package vars, after `edgePollInterval`:
```go
// edgeDrainTimeout bounds how long a cancelled edge keeps carrying the
// connections it already accepted before returning and letting process exit
// cut them (#534). Splices end on their own; :7000 forwards never do and
// always take the whole grace, which is intended — HTTP through them still
// works meanwhile and the relays behind are untouched. The runbook's edge
// stop timeout is sized to this plus a margin.
var edgeDrainTimeout = 20 * time.Second
```

`edge` struct: add
```go
	ready *Readiness
	live  atomic.Int64 // connections currently inside a handle*
```

`ServeEdge` and `newEdge`:
```go
func ServeEdge(ctx context.Context, cfg EdgeConfig, st *Store, m *Metrics, r *Readiness) error {
	return newEdge(cfg, st, m, r).serve(ctx)
}

func newEdge(cfg EdgeConfig, st *Store, m *Metrics, r *Readiness) *edge {
	return &edge{cfg: cfg, st: st, state: newEdgeState(), m: m, apiHost: "api." + cfg.Apex, ready: r}
}
```

Accept loop body: count the connection before handing it off:
```go
				e.m.ConnAccepted(name)
				e.live.Add(1)
				go func() {
					defer e.live.Add(-1)
					handle(conn)
				}()
```

After the listener loop, replace the `defer` + `select` tail with:
```go
	e.ready.SetReady()
	select {
	case err := <-errc:
		for _, ln := range lns {
			ln.Close()
		}
		return err
	case <-ctx.Done():
	}
	// Drain (#534): stop being a target, stop accepting, then let what we
	// carry finish — bounded, because :7000 forwards never end by themselves.
	e.ready.SetDraining()
	for _, ln := range lns {
		ln.Close()
	}
	deadline := time.NewTimer(edgeDrainTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(drainTick)
	defer tick.Stop()
	for e.live.Load() > 0 {
		select {
		case <-deadline.C:
			log.Printf("edge: drain deadline %s reached; cutting %d connection(s)", edgeDrainTimeout, e.live.Load())
			return nil
		case <-tick.C:
		}
	}
	log.Print("edge: drained; no connections left")
	return nil
}
```
The old `defer` closing listeners is gone — both exits above close them explicitly. Process exit in `cmd/piper-edge` is what cuts the remaining connections.

In `cmd/piper-edge/main.go`, mirror Task 2's bind rule and wire the readiness (replace from `opsAddr := ...` to the end of `main`):
```go
	opsAddr, opsAddrSet := os.LookupEnv("PIPER_EDGE_OPS_ADDR")
	if opsAddr == "" {
		opsAddr, opsAddrSet = "127.0.0.1:9090", false
	}
	metricsOn := os.Getenv("PIPER_EDGE_METRICS") == "1"
	logsOn := os.Getenv("PIPER_EDGE_LOGS") == "1"
	var metrics *relay.Metrics
	var ring *relay.LogRing
	if metricsOn {
		metrics = relay.NewEdgeMetrics()
	}
	if logsOn {
		ring = relay.NewLogRing(1000)
		log.SetOutput(io.MultiWriter(os.Stderr, ring))
	}
	ready := &relay.Readiness{}
	if opsAddrSet || metricsOn || logsOn {
		opsHandler := relay.NewOpsHandler(metrics, ring, ready)
		go func() {
			log.Printf("piper-edge: ops endpoint %s (metrics=%v logs=%v probes=/readyz,/livez)", opsAddr, metricsOn, logsOn)
			srv := &http.Server{Addr: opsAddr, Handler: opsHandler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
			if err := srv.ListenAndServe(); err != nil {
				log.Fatalf("ops endpoint: %v", err)
			}
		}()
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	log.Printf("piper-edge: TLS %s, HTTP %s, tunnel %s, apex %s", cfg.TLSAddr, cfg.HTTPAddr, cfg.TunnelAddr, cfg.Apex)
	if err := relay.ServeEdge(ctx, cfg, st, metrics, ready); err != nil {
		log.Fatal(err)
	}
	log.Print("piper-edge: stopped")
```
`errors` stays imported (`configFromEnv` uses it).

`deploy/compose/relay/docker-compose.yml`, under the `edge` service after `restart: unless-stopped`:
```yaml
    # SIGTERM drains (#534): up to 20s for the connections it carries.
    stop_grace_period: 30s
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/relay -run 'TestEdge' -count=1 && go test ./cmd/piper-edge && go test ./deploy/...`
Expected: PASS (the edge suite needs Postgres; it skips otherwise — make sure Docker is up so it actually runs).

- [ ] **Step 5: Commit**

```bash
git add internal/relay/edge.go internal/relay/edge_test.go cmd/piper-edge/main.go deploy/compose/relay/docker-compose.yml
git commit -m "feat(edge): drain on SIGTERM — flip /readyz, stop accepting, carry live connections up to 20s

Part of #534

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 4: Drop the vestigial data dir (#553)

**Files:**
- Modify: `cmd/piper-relay/main.go:163-166`
- Modify: `Dockerfile.relay:11-12`
- Modify: `docs/runbooks/relay-deploy.md` (two compose snippets' cert mount; the "State" bullet under "Run as a container")
- Possibly: anything `grep -rn 'PIPER_RELAY_DATA_DIR\|/var/lib/piper-relay' --exclude-dir=.git .` finds under `packaging/`

No new test: the change is a deletion whose only observable is that the binary no longer touches the filesystem at boot; `go build` and the existing `cmd/piper-relay` tests cover the compile.

- [ ] **Step 1: Remove the mkdir** — delete these four lines from `main`:
```go
	dataDir := env("PIPER_RELAY_DATA_DIR", "./relay-data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		log.Fatalf("data dir: %v", err)
	}
```

- [ ] **Step 2: Remove the image lines** — delete from `Dockerfile.relay`:
```
ENV PIPER_RELAY_DATA_DIR=/var/lib/piper-relay
VOLUME /var/lib/piper-relay
```

- [ ] **Step 3: Runbook and packaging** — in both compose snippets, change the relay volume to `- ./certs:/etc/piper-relay/certs:ro`, and reword the "State" bullet to:
```
- **State** lives in Postgres; the relay writes nothing to disk. On K8s/ECS
  point `PIPER_RELAY_DB_URL` at a managed instance and mount only the cert
  pair (and the GitHub App key) read-only; `readOnlyRootFilesystem: true`
  is fine.
```
For each grep hit under `packaging/`: if it is the systemd unit's `StateDirectory=` or an env example naming the variable, remove it; a cert path may stay as long as it is described as a cert location, not a data directory.

- [ ] **Step 4: Verify**

Run: `go build ./... && go test ./cmd/piper-relay && grep -rn 'PIPER_RELAY_DATA_DIR' --exclude-dir=.git . ; echo "grep exit $?"`
Expected: build OK, tests PASS, `grep exit 1` (no matches).

- [ ] **Step 5: Commit**

```bash
git add -A cmd/piper-relay/main.go Dockerfile.relay docs/runbooks/relay-deploy.md packaging
git commit -m "chore(relay): drop the vestigial PIPER_RELAY_DATA_DIR mkdir and image VOLUME

Part of #553

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 5: Non-root relay image (#554, relay half)

**Files:**
- Modify: `Dockerfile.relay:8`

- [ ] **Step 1: Change the base** — replace the `FROM` line with:
```
# :nonroot (uid 65532): behind an edge the relay binds no privileged port,
# and readAppKey only checks world bits, so a 0400 Secret mount still loads.
# Dockerfile.edge stays root — it owns :443/:80/:7000 and on a host-networked
# compose edge a non-root process cannot bind them (#554).
FROM gcr.io/distroless/static-debian12:nonroot
```

- [ ] **Step 2: Confirm the tag exists** (goreleaser's snapshot build is the only full check and is heavy):

Run: `docker manifest inspect gcr.io/distroless/static-debian12:nonroot >/dev/null && echo ok`
Expected: `ok`. If Docker is unavailable, say so in the PR body.

- [ ] **Step 3: Commit**

```bash
git add Dockerfile.relay
git commit -m "chore(relay): run the piper-relay image as non-root

Part of #554

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 6: Schema apply under an advisory lock (#555)

**Files:**
- Modify: `internal/relay/store.go:78-95`
- Modify: `internal/relay/store_test.go` (new test)

- [ ] **Step 1: Write the failing test.** Append to `internal/relay/store_test.go`:

```go
// N relays starting against an empty database apply the schema at once.
// Postgres's CREATE ... IF NOT EXISTS is not atomic across sessions, so
// without the advisory lock in Open one of them dies on a catalog
// duplicate-key error (#555).
func TestOpenConcurrentOnEmptyDatabase(t *testing.T) {
	dsn := relaytest.DSN(t)
	const n = 8
	errs := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			<-start
			st, err := Open(dsn)
			if err == nil {
				st.Close()
			}
			errs <- err
		}()
	}
	close(start)
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent Open: %v", err)
		}
	}
}
```

- [ ] **Step 2: Run it** a few times. The race is probabilistic; passing before the fix is acceptable — the test is the regression guard:

Run: `go test ./internal/relay -run TestOpenConcurrentOnEmptyDatabase -count=5`
Expected: at least one `apply schema: ERROR: duplicate key value violates unique constraint "pg_type_typname_nsp_index"`, or PASS if the race did not surface.

- [ ] **Step 3: Implement.** In `store.go`, add above `Open`:
```go
// schemaLockKey serializes concurrent schema applies (#555). Any constant
// works; it only has to be the same in every relay and edge on the database.
const schemaLockKey int64 = 0x70697065725f7265 // "piper_re"
```
and replace the schema `Exec` block with:
```go
	// Serialize concurrent starters: CREATE ... IF NOT EXISTS races across
	// sessions on an empty database and the loser dies on a catalog
	// duplicate key. The transaction-scoped advisory lock makes them queue;
	// each sees the tables the winner created.
	tx, err := db.Begin()
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock($1)`, schemaLockKey); err != nil {
		tx.Rollback()
		db.Close()
		return nil, fmt.Errorf("apply schema: lock: %w", err)
	}
	// No arguments ⇒ pgx uses the simple protocol, which accepts the
	// multi-statement schema in one round trip.
	if _, err := tx.Exec(schema); err != nil {
		tx.Rollback()
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := tx.Commit(); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
```
`schema.sql` must contain nothing that refuses to run inside a transaction: `grep -n 'CONCURRENTLY\|VACUUM' internal/relay/schema.sql` must print nothing.

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/relay -run 'TestOpen|TestStore' -count=3`
Expected: PASS every time.

- [ ] **Step 5: Commit**

```bash
git add internal/relay/store.go internal/relay/store_test.go
git commit -m "fix(relay): serialize the startup schema apply with an advisory lock

Part of #555

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 7: Runbook Kubernetes subsection and PROGRESS (#556)

**Files:**
- Modify: `docs/runbooks/relay-deploy.md` — the `**Kubernetes.**` paragraph (~line 477) becomes a `### Kubernetes` subsection; the "Ops surface" bullet (~line 296) mentions the probes and the bind rule; the stop-timeout sentence (~line 169) gains the edge's 30 s; "What still drops" (~line 496) loses the edge-restart sentence.
- Modify: `PROGRESS.md` — one line under Plan 2.

- [ ] **Step 1: Replace the Kubernetes paragraph** with this subsection (keep the ECS task-metadata sentence from the old paragraph inside the Zone bullet):

````markdown
### Kubernetes

`piper-edge` is a Deployment behind a TCP Service that holds the public IP
(or a cloud NLB speaking PROXY protocol, with `PIPER_EDGE_PROXY_PROTOCOL=1`;
otherwise `externalTrafficPolicy: Local`). `piper-relay` is a Deployment
with `PIPER_RELAY_PROXY_PROTOCOL=1` and a NetworkPolicy admitting only edge
pods and other relays (the `:8080` control hop). The knobs each needs:

```yaml
# relay Deployment — the parts that are not just env from this runbook
strategy:
  rollingUpdate: { maxSurge: 1, maxUnavailable: 0 }   # replacement in the pool before the old one drains
template:
  spec:
    terminationGracePeriodSeconds: 60                  # drain 20s + leave 5s + webhooks 35s
    securityContext: { runAsNonRoot: true, fsGroup: 65532 }
    containers:
      - name: relay
        image: ghcr.io/piperbox/piper-relay:<version>  # runs as uid 65532
        env:
          - { name: PIPER_RELAY_ADVERTISE_HOST, valueFrom: { fieldRef: { fieldPath: status.podIP } } }
          - { name: PIPER_RELAY_OPS_ADDR, value: ":9090" }  # kubelet + Prometheus; default is loopback
          - { name: PIPER_RELAY_PROXY_PROTOCOL, value: "1" }
        readinessProbe: { httpGet: { path: /readyz, port: 9090 }, periodSeconds: 2 }
        livenessProbe:  { httpGet: { path: /livez,  port: 9090 }, periodSeconds: 10 }
        securityContext: { readOnlyRootFilesystem: true }
        volumeMounts:
          - { name: app-key, mountPath: /etc/piper-relay, readOnly: true }
    volumes:
      - name: app-key
        secret: { secretName: piper-relay-github-app, defaultMode: 0400 }  # the 0644 default is refused as world-readable
```

- **`/readyz`** is 503 until the relay's first heartbeat row lands (an edge
  cannot route to it before that) and from SIGTERM onward; **`/livez`** is
  200 whenever the process answers. Neither depends on Postgres: a relay
  with its database down still serves every tunnel it holds, and the edge's
  15 s instance TTL is what retires one that stopped heartbeating. Both
  probes live on the ops listener, which binds when `PIPER_RELAY_OPS_ADDR`
  is set or metrics/logs are on.
- **Edge:** `terminationGracePeriodSeconds: 30` and a
  `lifecycle.preStop.exec.command: ["sleep", "5"]` so the Service has
  removed the endpoint before the edge stops accepting; on SIGTERM it flips
  `/readyz`, refuses new connections and carries existing ones for up to
  20 s (#534). Its `PIPER_EDGE_OPS_ADDR=:9090` serves the same probes. The
  edge image runs as root because it binds `:443/:80/:7000`; to run it
  non-root add `securityContext.sysctls: [{name: net.ipv4.ip_unprivileged_port_start, value: "0"}]`
  (a safe sysctl since 1.22) with `runAsUser: 65532`.
- **Zone:** `PIPER_RELAY_ZONE` should carry the node's
  `topology.kubernetes.io/zone`; the downward API cannot read node labels,
  so run one relay Deployment per zone with the value hard-coded and a node
  selector, or have an init step copy the label in. On ECS the task metadata
  endpoint (`${ECS_CONTAINER_METADATA_URI_V4}/task`, field
  `AvailabilityZone`) reports the zone; an entrypoint exports it before
  `exec`ing `piper-relay`.
- **Postgres:** a direct connection or a session-mode pooler. Both binaries
  hold a `LISTEN` connection, which a transaction-mode PgBouncer silently
  breaks. Concurrent first start against an empty database is safe (#555).
- **`api.<apex>`** may be terminated by an L7 ingress with a cert-manager
  certificate and routed to the relays' `:8080` Service — that port is plain
  HTTP written to be fronted with TLS — with DNS pointing `api.<apex>` at the
  ingress and the wildcard at the edge. The ingress must never take the
  wildcard: per-hostname routing to the owning pod is the dynamic map the
  edge exists to hold, and box-held certificates need L4 passthrough anyway.
````

- [ ] **Step 2: Touch the three other spots.**
  - Ops surface bullet becomes: ``Optional ops listener on `127.0.0.1:9090` (`PIPER_RELAY_OPS_ADDR`; binds when set or when either toggle is on); `PIPER_RELAY_METRICS=1` / `PIPER_RELAY_LOGS=1` enable metrics/log endpoints; `/readyz` and `/livez` are always served on it``.
  - Stop-timeout paragraph: append ``The edge's own drain is 20 s; give it 30 s (compose `stop_grace_period: 30s`, Kubernetes `terminationGracePeriodSeconds: 30`).``
  - "What still drops": replace the sentence ``Restarting the edge on a single host drops every tunnel through it (on Kubernetes the Service holds the port across a rolling restart).`` with ``Restarting the edge on a single host still drops every tunnel through it — a replacement cannot bind the ports until the old one exits; behind a Service the edge drains (#534) and only `:7000` forwards are cut, at the 20 s deadline.``

- [ ] **Step 3: PROGRESS.md** — add under the Plan 2 relay list, after the `slot 1 boot race` line:
```
- ✅ Kubernetes rollout readiness — `/readyz`+`/livez` on both ops listeners, edge drain on SIGTERM, non-root relay image, advisory-locked schema apply, runbook Kubernetes subsection — [#557](https://github.com/piperbox/piper/issues/557)
```
and update the `_Last updated:` line's date to 2026-09-06 with a lead clause naming #557.

- [ ] **Step 4: Verify**

Run: `grep -c 'Restarting the edge on a single host still' docs/runbooks/relay-deploy.md; grep -c '### Kubernetes' docs/runbooks/relay-deploy.md`
Expected: `1` and `1`.

- [ ] **Step 5: Commit**

```bash
git add docs/runbooks/relay-deploy.md PROGRESS.md
git commit -m "docs(relay): Kubernetes subsection — probes, rollout strategy, stop timeouts, Secret mode, PgBouncer

Part of #556

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 8: Verify and open the PR

- [ ] **Step 1: Full gate**

Run: `make verify`
Expected: exit 0. Judge by exit status, not by grepping output — it halts at the first failing gate.

- [ ] **Step 2: Push and open the PR** into `main`. Write the body to a file first (a nested heredoc inside `gh pr create` does not survive the shell):

```bash
git push -u origin HEAD
gh pr create --base main --title "[relay] Kubernetes rollout readiness (#557)" --body-file /path/to/pr-body.md
```

PR body:

```markdown
Groups the small issues from the 2026-09-06 Kubernetes-readiness audit into one branch, one commit per issue. Spec: `docs/superpowers/specs/2026-09-06-relay-k8s-rollout-readiness-design.md`.

- `/readyz` + `/livez` on both ops listeners; the relay's readiness follows its pool row, the edge's follows its listeners. Neither depends on Postgres. The ops listener binds when its address is set or a toggle is on.
- Edge drain on SIGTERM: flip readiness, stop accepting, carry live connections up to 20 s, cut the rest.
- Vestigial `PIPER_RELAY_DATA_DIR` mkdir and image `VOLUME` removed.
- `piper-relay` image runs as uid 65532; the edge image stays root (a host-networked compose edge cannot bind :443 otherwise) — #554 stays open for that half.
- Startup schema apply under `pg_advisory_xact_lock`.
- Runbook: Kubernetes subsection with the manifest knobs.

Closes #552
Closes #553
Closes #555
Closes #556
Closes #534
Part of #554
Part of #557

🤖 Generated with [Claude Code](https://claude.com/claude-code)
```

- [ ] **Step 3: Comment on #554** with the edge remainder:

```bash
gh issue comment 554 --body "Relay half is in the #557 PR: the piper-relay image is now distroless :nonroot (uid 65532). The edge image stays root on purpose — the Hetzner compose edge is host-networked, Docker refuses net sysctls under host networking, and a non-root process without file caps cannot bind :443 there. Kubernetes users get the runAsUser + ip_unprivileged_port_start recipe in the runbook. Leaving this open for the edge image; it flips once the single-host layout no longer needs a root bind."
```
