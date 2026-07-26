# Relay Ops Endpoints (Metrics + Log Export) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give `piper-relay` an infra-only ops listener serving Prometheus metrics (`GET /metrics`) and a ring-buffered log snapshot (`GET /logs`), each independently toggled by env vars.

**Architecture:** A new plain-HTTP listener (`PIPER_RELAY_OPS_ADDR`, default `127.0.0.1:9090`) structurally disconnected from the SNI dispatcher — isolation is bind address + firewall, no auth. A `relay.Metrics` struct owns a private Prometheus registry (topology `GaugeFunc`s reading live `Router` state, traffic counters incremented in the accept/route paths, Go+process collectors); a `relay.LogRing` tees the stdlib logger. Both are nil when their toggle is off; every `*Metrics` method is nil-safe so the data path never branches on config.

**Tech Stack:** Go 1.26, `github.com/prometheus/client_golang` v1.23.2 (already in `go.sum` as an indirect dep — this promotes it to direct), stdlib `net/http` + `log`.

**Spec:** `docs/superpowers/specs/2026-07-26-relay-ops-endpoints-design.md`

## Global Constraints

- `CGO_ENABLED=0` must hold: `make verify` (gofmt → vet → test → linux/arm64 cross-compile) must pass at every commit.
- Module path is `github.com/piperbox/piper`; all new code lives in `internal/relay` (package `relay`) and `cmd/piper-relay`.
- Metric names verbatim: `piper_relay_agents_connected`, `piper_relay_hostnames_routed`, `piper_relay_custom_domains_routed`, `piper_relay_conns_accepted_total{listener}`, `piper_relay_conns_routed_total{listener}`, `piper_relay_conns_unrouted_total{listener}`, `piper_relay_active_streams`. Listener label values: `tls`, `http`, `tunnel`.
- Env vars verbatim: `PIPER_RELAY_OPS_ADDR` (default `127.0.0.1:9090`), `PIPER_RELAY_METRICS` (`1` = on), `PIPER_RELAY_LOGS` (`1` = on). Both endpoints off by default; no listener when both are off; a disabled endpoint on a running listener returns 404.
- Pre-1.0 policy: change `relay.Serve`'s signature in place — no compat shim, no variadic trick.
- Work on branch `faruk/relay-ops-endpoints` (already exists, holds the spec commit). One commit per task, conventional-commit style, each ending with:
  `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`
- Run tests with `go test ./internal/relay/ -run <Name> -v` per step; full `make verify` before each commit.

---

### Task 1: LogRing — bounded log line ring buffer

**Files:**
- Create: `internal/relay/logring.go`
- Test: `internal/relay/logring_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `type LogRing struct` with `func NewLogRing(capacity int) *LogRing`, `func (r *LogRing) Write(p []byte) (int, error)` (an `io.Writer` for `log.SetOutput` tee; splits writes on newlines), `func (r *LogRing) Lines() []string` (oldest-first copy). Task 4 serves `Lines()` at `/logs`; Task 6 installs the tee.

- [ ] **Step 1: Write the failing tests**

```go
// internal/relay/logring_test.go
package relay

import (
	"fmt"
	"sync"
	"testing"
)

func TestLogRingOrderedOldestFirst(t *testing.T) {
	r := NewLogRing(4)
	r.Write([]byte("one\n"))
	r.Write([]byte("two\nthree\n")) // one Write may carry several lines
	got := r.Lines()
	want := []string{"one", "two", "three"}
	if len(got) != len(want) {
		t.Fatalf("Lines() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Lines()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLogRingWrapsAtCapacity(t *testing.T) {
	r := NewLogRing(3)
	for i := 1; i <= 5; i++ {
		fmt.Fprintf(r, "line-%d\n", i)
	}
	got := r.Lines()
	want := []string{"line-3", "line-4", "line-5"}
	if len(got) != len(want) {
		t.Fatalf("Lines() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Lines()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLogRingConcurrentWrites(t *testing.T) {
	r := NewLogRing(64)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				fmt.Fprintf(r, "g%d-%d\n", g, i)
			}
		}(g)
	}
	wg.Wait()
	if got := len(r.Lines()); got != 64 {
		t.Fatalf("len(Lines()) = %d, want full ring of 64", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/relay/ -run TestLogRing -v`
Expected: FAIL — `undefined: NewLogRing`

- [ ] **Step 3: Write the implementation**

```go
// internal/relay/logring.go
package relay

import (
	"strings"
	"sync"
)

// LogRing is a fixed-capacity ring buffer of log lines. It implements
// io.Writer so main can tee the stdlib logger into it (log lines can then be
// served over HTTP); Lines returns the buffered lines oldest-first. A full
// ring overwrites its oldest line, so memory stays bounded for the process
// lifetime.
type LogRing struct {
	mu    sync.Mutex
	lines []string
	next  int
	full  bool
}

func NewLogRing(capacity int) *LogRing {
	return &LogRing{lines: make([]string, capacity)}
}

// Write records p's newline-separated lines. One log.Printf call arrives as
// one Write ending in "\n", but nothing guarantees a single line per call, so
// split rather than assume.
func (r *LogRing) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		r.lines[r.next] = line
		r.next = (r.next + 1) % len(r.lines)
		if r.next == 0 {
			r.full = true
		}
	}
	return len(p), nil
}

// Lines returns a copy of the buffered lines, oldest first.
func (r *LogRing) Lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full {
		return append([]string(nil), r.lines[:r.next]...)
	}
	out := make([]string, 0, len(r.lines))
	out = append(out, r.lines[r.next:]...)
	out = append(out, r.lines[:r.next]...)
	return out
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/relay/ -run TestLogRing -v`
Expected: PASS (all three)

- [ ] **Step 5: Verify and commit**

```bash
make verify
git add internal/relay/logring.go internal/relay/logring_test.go
git commit -m "feat: LogRing, a bounded ring buffer for relay log export

Part of the relay ops endpoints
(docs/superpowers/specs/2026-07-26-relay-ops-endpoints-design.md).

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 2: Router.Counts — topology totals for the gauges

**Files:**
- Modify: `internal/relay/router.go` (append method at end of file)
- Test: `internal/relay/router_test.go` (append test)

**Interfaces:**
- Consumes: existing `Router` internals (`byBase`, `byHost`, `custom` maps; `RegisterCustom` writes a domain into *both* `byBase` and `custom`).
- Produces: `func (r *Router) Counts() (agents, hosts, custom int)` — agents = base-domain entries that are not custom-domain aliases; hosts = relay-terminated hostnames; custom = custom domains. Task 3's `GaugeFunc`s call this at scrape time.

- [ ] **Step 1: Write the failing test**

```go
// append to internal/relay/router_test.go
func TestRouterCounts(t *testing.T) {
	r := NewRouter()
	s1 := &tunnel.Session{BaseDomain: "alice.example.com"}
	s2 := &tunnel.Session{BaseDomain: "bob.example.com"}
	r.Register(s1)
	r.Register(s2)
	r.RegisterHost("blog-alice.public.getpiper.co", s1)
	// RegisterCustom writes byBase AND custom — Counts must not double-count
	// the custom domain as an agent.
	r.RegisterCustom("byo.example.org", s1)

	a, h, c := r.Counts()
	if a != 2 || h != 1 || c != 1 {
		t.Fatalf("Counts() = %d,%d,%d; want 2,1,1", a, h, c)
	}
	r.Unregister(s1)
	a, h, c = r.Counts()
	if a != 1 || h != 0 || c != 0 {
		t.Fatalf("after Unregister: Counts() = %d,%d,%d; want 1,0,0", a, h, c)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/relay/ -run TestRouterCounts -v`
Expected: FAIL — `r.Counts undefined`

- [ ] **Step 3: Write the implementation**

```go
// append to internal/relay/router.go
// Counts reports the router's live registration totals for the metrics
// gauges: agent sessions, relay-terminated hostnames, and custom domains.
// RegisterCustom stores a domain in both byBase and custom, so an agent is a
// byBase entry with no custom twin.
func (r *Router) Counts() (agents, hosts, custom int) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for k := range r.byBase {
		if _, ok := r.custom[k]; !ok {
			agents++
		}
	}
	return agents, len(r.byHost), len(r.custom)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/relay/ -run TestRouterCounts -v`
Expected: PASS

- [ ] **Step 5: Verify and commit**

```bash
make verify
git add internal/relay/router.go internal/relay/router_test.go
git commit -m "feat: Router.Counts for relay topology gauges

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 3: Metrics — private registry, gauges, nil-safe counters

**Files:**
- Modify: `go.mod` / `go.sum` (promote `github.com/prometheus/client_golang` to a direct require — it is already pinned at v1.23.2 indirectly via Caddy)
- Create: `internal/relay/ops.go`
- Test: `internal/relay/ops_test.go`

**Interfaces:**
- Consumes: `Router.Counts()` from Task 2.
- Produces:
  - `type Metrics struct` with `func NewMetrics(router *Router) *Metrics`
  - Nil-safe instrument methods (all no-ops on a nil receiver): `func (m *Metrics) ConnAccepted(listener string)`, `ConnRouted(listener string)`, `ConnUnrouted(listener string)`, `StreamStart()`, `StreamEnd()`
  - Unexported field `reg *prometheus.Registry` (Task 4's handler scrapes it from inside the package)
  - Test helper `scrapeMetrics(t, m)` used again in Task 5.

- [ ] **Step 1: Add the dependency**

```bash
go get github.com/prometheus/client_golang@v1.23.2
go mod tidy
```

Expected: `go.mod`'s main require block now lists `github.com/prometheus/client_golang v1.23.2`; no version changes elsewhere.

- [ ] **Step 2: Write the failing tests**

```go
// internal/relay/ops_test.go
package relay

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/piperbox/piper/internal/tunnel"
)

// scrapeMetrics returns the text exposition of m's registry via the ops
// handler (wired in Task 4; until then this fails to compile — see Step 3's
// note for the interim direct-promhttp version).
func scrapeMetrics(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	NewOpsHandler(m, nil).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

func TestMetricsTopologyGauges(t *testing.T) {
	router := NewRouter()
	m := NewMetrics(router)
	router.Register(&tunnel.Session{BaseDomain: "alice.example.com"})
	s := &tunnel.Session{BaseDomain: "bob.example.com"}
	router.Register(s)
	router.RegisterHost("app-bob.public.getpiper.co", s)
	router.RegisterCustom("byo.example.org", s)

	body := scrapeMetrics(t, m)
	for _, want := range []string{
		"piper_relay_agents_connected 2",
		"piper_relay_hostnames_routed 1",
		"piper_relay_custom_domains_routed 1",
		"go_goroutines", // free Go collector present
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape missing %q\n%s", want, body)
		}
	}
}

func TestMetricsCounters(t *testing.T) {
	m := NewMetrics(NewRouter())
	m.ConnAccepted("tls")
	m.ConnAccepted("tls")
	m.ConnRouted("http")
	m.ConnUnrouted("tls")
	m.StreamStart()

	body := scrapeMetrics(t, m)
	for _, want := range []string{
		`piper_relay_conns_accepted_total{listener="tls"} 2`,
		`piper_relay_conns_routed_total{listener="http"} 1`,
		`piper_relay_conns_unrouted_total{listener="tls"} 1`,
		"piper_relay_active_streams 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape missing %q\n%s", want, body)
		}
	}
	m.StreamEnd()
	if body := scrapeMetrics(t, m); !strings.Contains(body, "piper_relay_active_streams 0") {
		t.Errorf("active_streams should return to 0\n%s", body)
	}
}

func TestMetricsNilReceiverIsNoOp(t *testing.T) {
	var m *Metrics // every instrument method must be safe on nil
	m.ConnAccepted("tls")
	m.ConnRouted("http")
	m.ConnUnrouted("http")
	m.StreamStart()
	m.StreamEnd()
}
```

Note: until Task 4 adds `NewOpsHandler`, make `scrapeMetrics` use promhttp directly so this task stands alone — Task 4 then rewrites it to the form above:

```go
func scrapeMetrics(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{}).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}
```

(import `"github.com/prometheus/client_golang/prometheus/promhttp"` in the test file for this interim version).

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/relay/ -run TestMetrics -v`
Expected: FAIL — `undefined: NewMetrics`

- [ ] **Step 4: Write the implementation**

```go
// internal/relay/ops.go
package relay

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Metrics owns the relay's Prometheus instruments on a private registry — the
// library's global default registry would leak state between tests and panic
// on duplicate registration. Topology is exposed as GaugeFuncs reading live
// Router state at scrape time (no inc/dec bookkeeping to drift); traffic
// counters are incremented by the accept/route paths through the nil-safe
// methods below, so a relay running without metrics passes nil and the data
// path never branches on config.
type Metrics struct {
	reg           *prometheus.Registry
	connsAccepted *prometheus.CounterVec
	connsRouted   *prometheus.CounterVec
	connsUnrouted *prometheus.CounterVec
	activeStreams prometheus.Gauge
}

func NewMetrics(router *Router) *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "piper_relay_agents_connected",
		Help: "Agent tunnel sessions currently registered.",
	}, func() float64 { a, _, _ := router.Counts(); return float64(a) }))
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "piper_relay_hostnames_routed",
		Help: "Relay-terminated app hostnames currently routed.",
	}, func() float64 { _, h, _ := router.Counts(); return float64(h) }))
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "piper_relay_custom_domains_routed",
		Help: "BYO custom domains currently routed.",
	}, func() float64 { _, _, c := router.Counts(); return float64(c) }))

	m := &Metrics{
		reg: reg,
		connsAccepted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "piper_relay_conns_accepted_total",
			Help: "Connections accepted, by public listener.",
		}, []string{"listener"}),
		connsRouted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "piper_relay_conns_routed_total",
			Help: "Connections whose SNI/Host matched a registered session.",
		}, []string{"listener"}),
		connsUnrouted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "piper_relay_conns_unrouted_total",
			Help: "Connections that completed a head read but matched no session.",
		}, []string{"listener"}),
		activeStreams: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "piper_relay_active_streams",
			Help: "Connections currently being spliced down an agent tunnel.",
		}),
	}
	reg.MustRegister(m.connsAccepted, m.connsRouted, m.connsUnrouted, m.activeStreams)
	return m
}

// The instrument methods are nil-safe: a relay with metrics disabled threads a
// nil *Metrics through the data path, and every call below no-ops.

func (m *Metrics) ConnAccepted(listener string) {
	if m == nil {
		return
	}
	m.connsAccepted.WithLabelValues(listener).Inc()
}

func (m *Metrics) ConnRouted(listener string) {
	if m == nil {
		return
	}
	m.connsRouted.WithLabelValues(listener).Inc()
}

func (m *Metrics) ConnUnrouted(listener string) {
	if m == nil {
		return
	}
	m.connsUnrouted.WithLabelValues(listener).Inc()
}

func (m *Metrics) StreamStart() {
	if m == nil {
		return
	}
	m.activeStreams.Inc()
}

func (m *Metrics) StreamEnd() {
	if m == nil {
		return
	}
	m.activeStreams.Dec()
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/relay/ -run TestMetrics -v`
Expected: PASS (all three)

- [ ] **Step 6: Verify and commit**

```bash
make verify
git add go.mod go.sum internal/relay/ops.go internal/relay/ops_test.go
git commit -m "feat: relay Metrics on a private Prometheus registry

Promotes prometheus/client_golang (already indirect via Caddy) to a
direct dependency. make cross proves the CGO_ENABLED=0 arm64 build
survives it.

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 4: NewOpsHandler — toggle-gated /metrics and /logs

**Files:**
- Modify: `internal/relay/ops.go` (append handler)
- Test: `internal/relay/ops_test.go` (append tests; rewrite `scrapeMetrics` to use `NewOpsHandler` as shown in Task 3 Step 2)

**Interfaces:**
- Consumes: `Metrics.reg` (Task 3), `LogRing.Lines()` (Task 1).
- Produces: `func NewOpsHandler(m *Metrics, ring *LogRing) http.Handler` — nil `m` ⇒ `/metrics` 404s; nil `ring` ⇒ `/logs` 404s. Task 6 mounts this on the ops listener.

- [ ] **Step 1: Write the failing tests**

```go
// append to internal/relay/ops_test.go
// (add imports: "net/http")

func TestOpsHandlerLogs(t *testing.T) {
	ring := NewLogRing(8)
	ring.Write([]byte("first\nsecond\n"))
	rec := httptest.NewRecorder()
	NewOpsHandler(nil, ring).ServeHTTP(rec, httptest.NewRequest("GET", "/logs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /logs = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q, want text/plain", ct)
	}
	if got := rec.Body.String(); got != "first\nsecond\n" {
		t.Fatalf("body = %q, want %q", got, "first\nsecond\n")
	}
}

func TestOpsHandlerDisabledEndpoints404(t *testing.T) {
	cases := []struct {
		name string
		h    http.Handler
		path string
		want int
	}{
		{"metrics off", NewOpsHandler(nil, NewLogRing(8)), "/metrics", http.StatusNotFound},
		{"logs off", NewOpsHandler(NewMetrics(NewRouter()), nil), "/logs", http.StatusNotFound},
		{"metrics on", NewOpsHandler(NewMetrics(NewRouter()), nil), "/metrics", http.StatusOK},
		{"logs on", NewOpsHandler(nil, NewLogRing(8)), "/logs", http.StatusOK},
		{"unknown path", NewOpsHandler(NewMetrics(NewRouter()), NewLogRing(8)), "/nope", http.StatusNotFound},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		c.h.ServeHTTP(rec, httptest.NewRequest("GET", c.path, nil))
		if rec.Code != c.want {
			t.Errorf("%s: GET %s = %d, want %d", c.name, c.path, rec.Code, c.want)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/relay/ -run TestOpsHandler -v`
Expected: FAIL — `undefined: NewOpsHandler`

- [ ] **Step 3: Write the implementation**

```go
// append to internal/relay/ops.go
// (add imports: "fmt", "net/http", "github.com/prometheus/client_golang/prometheus/promhttp")

// NewOpsHandler serves the relay's infra-only ops surface: /metrics when m is
// non-nil, /logs when ring is non-nil. A nil argument means that endpoint is
// toggled off and 404s — the caller decides exposure purely by what it
// constructs, so there is no config to consult here.
func NewOpsHandler(m *Metrics, ring *LogRing) http.Handler {
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
	return mux
}
```

Also rewrite `scrapeMetrics` in `ops_test.go` to the `NewOpsHandler(m, nil)` form (Task 3 Step 2 shows it) and drop the test file's `promhttp` import.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/relay/ -run 'TestOpsHandler|TestMetrics' -v`
Expected: PASS (handler tests and the Task 3 tests still green through the rewritten helper)

- [ ] **Step 5: Verify and commit**

```bash
make verify
git add internal/relay/ops.go internal/relay/ops_test.go
git commit -m "feat: ops handler serving toggle-gated /metrics and /logs

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 5: Thread Metrics through the accept/route/splice paths

**Files:**
- Modify: `internal/relay/server.go` (`Serve`, `acceptTunnels`, `handlePublic`, `pump` — `serveTunnel`/`serveControl` are untouched), `internal/relay/http80.go` (`acceptHTTP`, `handleHTTP`), `internal/relay/terminate.go` (`terminate`)
- Modify (mechanical, add `nil` argument): `internal/relay/server_test.go:76,79,445`, `internal/relay/watchdog_test.go:84,132,220`, `internal/relay/accepttunnels_test.go:36`, `cmd/piper-relay/main.go:282` (the `relay.Serve` call — pass `nil` for now; Task 6 wires the real value)
- Test: `internal/relay/ops_test.go` (append traffic-counter tests)

**Interfaces:**
- Consumes: `Metrics` nil-safe methods (Task 3), `newSessionPair(t)` helper from `router_test.go` (returns live `server, client *tunnel.Session` over an in-memory pipe).
- Produces: new signatures every later task and test must use:
  - `func Serve(tlsAddr, httpAddr, tunnelAddr string, st *Store, tlsCfg *tls.Config, router *Router, ctrl http.Handler, ghApp *GitHubApp, delivery *TunnelDelivery, m *Metrics) error`
  - `func acceptTunnels(ln net.Listener, st *Store, router *Router, ghApp *GitHubApp, delivery *TunnelDelivery, m *Metrics)`
  - `func acceptHTTP(ln net.Listener, router *Router, m *Metrics)`
  - `func handleHTTP(conn net.Conn, router *Router, m *Metrics)`
  - `func handlePublic(conn net.Conn, router *Router, tlsCfg *tls.Config, ctrlHost string, ctrlQ *connQueue, m *Metrics)`
  - `func pump(conn net.Conn, buffered []byte, sess *tunnel.Session, kind byte, m *Metrics)`
  - `func terminate(conn net.Conn, buffered []byte, sess *tunnel.Session, tlsCfg *tls.Config, m *Metrics)`

Counting rules (from the spec): `ConnAccepted` fires once per accepted conn in each accept loop (`tls` in `Serve`'s inline loop, `http` in `acceptHTTP`, `tunnel` in `acceptTunnels`). `ConnRouted`/`ConnUnrouted` fire after a successful head read: control-plane conns (`sni == ctrlHost`) count as neither; a failed `readSNI`/`readHost` counts as neither. `StreamStart`/`StreamEnd` bracket the splice in both `pump` and `terminate`, after `OpenKind` succeeds.

- [ ] **Step 1: Write the failing tests**

```go
// append to internal/relay/ops_test.go
// (add imports: "crypto/tls", "io", "net", "time")

// waitForScrape polls the scrape until it contains want or the deadline
// passes — counters increment on goroutines the test doesn't join.
func waitForScrape(t *testing.T, m *Metrics, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(scrapeMetrics(t, m), want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("scrape never contained %q\n%s", want, scrapeMetrics(t, m))
}

func TestCountersUnroutedTLS(t *testing.T) {
	router := NewRouter()
	m := NewMetrics(router)
	c1, c2 := net.Pipe()
	defer c1.Close()
	go handlePublic(c2, router, nil, "api.public.getpiper.co", nil, m)
	// A real ClientHello for an unknown SNI: drive a TLS client handshake at
	// the pipe; readSNI parses the hello, the router matches nothing.
	go tls.Client(c1, &tls.Config{ServerName: "unknown.example.com", InsecureSkipVerify: true}).Handshake()
	waitForScrape(t, m, `piper_relay_conns_unrouted_total{listener="tls"} 1`)
}

func TestCountersUnroutedHTTP(t *testing.T) {
	router := NewRouter()
	m := NewMetrics(router)
	c1, c2 := net.Pipe()
	defer c1.Close()
	go handleHTTP(c2, router, m)
	go c1.Write([]byte("GET / HTTP/1.1\r\nHost: nope.example.com\r\n\r\n"))
	waitForScrape(t, m, `piper_relay_conns_unrouted_total{listener="http"} 1`)
}

func TestCountersRoutedHTTPAndActiveStreams(t *testing.T) {
	router := NewRouter()
	m := NewMetrics(router)
	server, client := newSessionPair(t)
	router.RegisterCustom("byo.example.org", server)
	// The agent side accepts the spliced stream and drains it, like a box's
	// Caddy would.
	go func() {
		_, stream, err := client.AcceptKind()
		if err != nil {
			return
		}
		io.Copy(io.Discard, stream)
	}()
	c1, c2 := net.Pipe()
	go handleHTTP(c2, router, m)
	go c1.Write([]byte("GET / HTTP/1.1\r\nHost: byo.example.org\r\n\r\n"))
	waitForScrape(t, m, `piper_relay_conns_routed_total{listener="http"} 1`)
	waitForScrape(t, m, "piper_relay_active_streams 1")
	c1.Close() // splice ends…
	waitForScrape(t, m, "piper_relay_active_streams 0") // …and the gauge returns to 0
}

func TestCountersAcceptedHTTP(t *testing.T) {
	router := NewRouter()
	m := NewMetrics(router)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go acceptHTTP(ln, router, m)
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	waitForScrape(t, m, `piper_relay_conns_accepted_total{listener="http"} 1`)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/relay/ -run TestCounters -v`
Expected: FAIL to compile — `handlePublic`/`handleHTTP`/`acceptHTTP` don't take a `*Metrics` yet.

- [ ] **Step 3: Change the signatures and add the instrument calls**

In `internal/relay/server.go`:

```go
// Serve gains m; pass it to every path. (Doc comment gains one line: “m,
// when non-nil, receives traffic counters; nil disables instrumentation.”)
func Serve(tlsAddr, httpAddr, tunnelAddr string, st *Store, tlsCfg *tls.Config, router *Router, ctrl http.Handler, ghApp *GitHubApp, delivery *TunnelDelivery, m *Metrics) error {
	// … unchanged until the goroutine spawns:
	go acceptTunnels(tunLn, st, router, ghApp, delivery, m)
	// …
	go acceptHTTP(httpLn, router, m)
	// … in the tls accept loop:
	for {
		conn, err := tlsLn.Accept()
		if err != nil {
			return err
		}
		m.ConnAccepted("tls")
		go handlePublic(conn, router, tlsCfg, ctrlHost, ctrlQ, m)
	}
}

func acceptTunnels(ln net.Listener, st *Store, router *Router, ghApp *GitHubApp, delivery *TunnelDelivery, m *Metrics) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		m.ConnAccepted("tunnel")
		go serveTunnel(conn, st, router, st.AgentDisabled, ghApp, delivery)
	}
}

func handlePublic(conn net.Conn, router *Router, tlsCfg *tls.Config, ctrlHost string, ctrlQ *connQueue, m *Metrics) {
	sni, buffered, err := readSNI(conn)
	if err != nil {
		conn.Close()
		return
	}
	// Control plane: … (unchanged — counts as neither routed nor unrouted)
	if ctrlQ != nil && sni == ctrlHost {
		ctrlQ.push(tls.Server(&prefixConn{Conn: conn, prefix: buffered}, tlsCfg))
		return
	}
	defer conn.Close()
	if sess, ok := router.LookupHost(sni); ok {
		m.ConnRouted("tls")
		if tlsCfg == nil {
			return // terminated hostname but no wildcard cert configured
		}
		terminate(conn, buffered, sess, tlsCfg, m)
		return
	}
	if sess, ok := router.Lookup(sni); ok {
		m.ConnRouted("tls")
		pump(conn, buffered, sess, tunnel.KindPassthrough, m)
		return
	}
	m.ConnUnrouted("tls")
}

func pump(conn net.Conn, buffered []byte, sess *tunnel.Session, kind byte, m *Metrics) {
	stream, err := sess.OpenKind(kind)
	if err != nil {
		return
	}
	m.StreamStart()
	defer m.StreamEnd()
	defer stream.Close()
	// … rest unchanged
}
```

In `internal/relay/http80.go`:

```go
func acceptHTTP(ln net.Listener, router *Router, m *Metrics) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		m.ConnAccepted("http")
		go handleHTTP(conn, router, m)
	}
}

func handleHTTP(conn net.Conn, router *Router, m *Metrics) {
	defer conn.Close()
	host, buffered, err := readHost(conn)
	if err != nil {
		return
	}
	if sess, ok := router.LookupCustom(host); ok {
		m.ConnRouted("http")
		pump(conn, buffered, sess, tunnel.KindHTTP, m)
		return
	}
	m.ConnUnrouted("http")
}
```

In `internal/relay/terminate.go`, `terminate` gains `m *Metrics` and brackets its splice:

```go
func terminate(conn net.Conn, buffered []byte, sess *tunnel.Session, tlsCfg *tls.Config, m *Metrics) {
	tlsConn := tls.Server(&prefixConn{Conn: conn, prefix: buffered}, tlsCfg)
	if err := tlsConn.Handshake(); err != nil {
		return
	}
	stream, err := sess.OpenKind(tunnel.KindHTTP)
	if err != nil {
		return
	}
	m.StreamStart()
	defer m.StreamEnd()
	defer stream.Close()
	// … rest unchanged
}
```

Then fix every call site to pass `nil` (mechanical): `server_test.go:76` (`handlePublic(c, router, tlsCfg, ctrlHost, ctrlQ, nil)`), `server_test.go:79` (`acceptHTTP(httpLn, router, nil)`), `server_test.go:445`, `watchdog_test.go:84,132,220`, `accepttunnels_test.go:36` (all `acceptTunnels(…, nil, nil, nil)`), and the `relay.Serve(…)` call at the bottom of `cmd/piper-relay/main.go` (append `nil` — Task 6 replaces it).

- [ ] **Step 4: Run the full relay package tests**

Run: `go test ./internal/relay/ ./cmd/piper-relay/ -count=1`
Expected: PASS — new counter tests green, all pre-existing tests still green with nil metrics.

- [ ] **Step 5: Verify and commit**

```bash
make verify
git add internal/relay/ cmd/piper-relay/main.go
git commit -m "feat: thread Metrics through relay accept/route/splice paths

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 6: Wire the ops listener in cmd/piper-relay

**Files:**
- Modify: `cmd/piper-relay/main.go`

**Interfaces:**
- Consumes: `relay.NewMetrics(router)`, `relay.NewLogRing(1000)`, `relay.NewOpsHandler(m, ring)`, the `m *Metrics` parameter of `relay.Serve` (Tasks 3–5), plus the file's existing `env` helper.
- Produces: the running behavior — env vars `PIPER_RELAY_OPS_ADDR` / `PIPER_RELAY_METRICS` / `PIPER_RELAY_LOGS` as specced. No new exported symbols.

- [ ] **Step 1: Add the wiring**

In `main()`, immediately after `router := relay.NewRouter()` (before `apiHandler := …`):

```go
	// Infra-only ops surface (metrics + log export). Isolation is the bind
	// address — loopback by default, a private VPC IP in production — never
	// the SNI dispatcher, so no public hostname can route here. Each endpoint
	// is off unless its toggle is set; with neither, nothing binds at all.
	opsAddr := env("PIPER_RELAY_OPS_ADDR", "127.0.0.1:9090")
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
	if metricsOn || logsOn {
		opsHandler := relay.NewOpsHandler(metrics, ring)
		go func() {
			log.Printf("piper-relay: ops endpoint %s (metrics=%v logs=%v)", opsAddr, metricsOn, logsOn)
			// Fatal like the control API: a silently-missing metrics endpoint
			// would defeat its orchestration purpose.
			if err := http.ListenAndServe(opsAddr, opsHandler); err != nil {
				log.Fatalf("ops endpoint: %v", err)
			}
		}()
	}
```

Add `"io"` to the import block (the file already imports `log`, `net/http`, `os`). Replace the `nil` passed to `relay.Serve` in Task 5 with `metrics`:

```go
	log.Fatal(relay.Serve(tlsAddr, httpAddr, tunnelAddr, st, tlsCfg, router, ctrl, ghApp, delivery, metrics))
```

- [ ] **Step 2: Smoke-test the binary by hand**

```bash
go build -o /tmp/piper-relay-smoke ./cmd/piper-relay
PIPER_RELAY_METRICS=1 PIPER_RELAY_LOGS=1 PIPER_RELAY_TLS_ADDR=127.0.0.1:0 PIPER_RELAY_HTTP_ADDR=127.0.0.1:0 PIPER_RELAY_TUNNEL_ADDR=127.0.0.1:0 PIPER_RELAY_API_ADDR=127.0.0.1:0 PIPER_RELAY_DATA_DIR=$(mktemp -d) /tmp/piper-relay-smoke &
sleep 1
curl -s http://127.0.0.1:9090/metrics | head -5   # expect piper_relay_* and go_* lines
curl -s http://127.0.0.1:9090/logs                # expect the startup log lines
kill %1
```

Expected: `/metrics` serves exposition text including `piper_relay_agents_connected 0`; `/logs` shows the startup lines (e.g. `piper-relay: ops endpoint 127.0.0.1:9090 …`).

- [ ] **Step 3: Verify and commit**

```bash
make verify
git add cmd/piper-relay/main.go
git commit -m "feat: relay ops listener — PIPER_RELAY_OPS_ADDR/METRICS/LOGS

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 7: Progress trail and PR

**Files:**
- Modify: `PROGRESS.md` (one line in the relay section)

- [ ] **Step 1: File the tracking issue**

```bash
gh issue create \
  --title "[relay] infra-only ops endpoints: Prometheus /metrics and /logs export" \
  --label relay,enhancement,P2,size/M \
  --body "Add an ops listener to piper-relay for scaling/orchestration: PIPER_RELAY_OPS_ADDR (default 127.0.0.1:9090) serving GET /metrics (Prometheus, toggle PIPER_RELAY_METRICS=1) and GET /logs (ring-buffered log snapshot, toggle PIPER_RELAY_LOGS=1). Isolation by bind address + firewall, never the SNI dispatcher. Spec: docs/superpowers/specs/2026-07-26-relay-ops-endpoints-design.md"
```

Note the issue number `#N`; use it below.

- [ ] **Step 2: Update PROGRESS.md**

Add one line to the relay section, matching the file's existing terse `[#N]`-linked style (read the neighboring lines and copy their form), e.g.:

```
- Ops endpoints: infra-only /metrics (Prometheus) + /logs (ring buffer), env-toggled [#N]
```

- [ ] **Step 3: Commit, push, open the PR**

```bash
git add PROGRESS.md
git commit -m "chore: PROGRESS entry for relay ops endpoints

Part of #N

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
make verify
git push -u origin faruk/relay-ops-endpoints
gh pr create --base main \
  --title "[relay] infra-only ops endpoints: /metrics + /logs" \
  --body "Adds an ops listener (PIPER_RELAY_OPS_ADDR, default 127.0.0.1:9090) serving Prometheus metrics behind PIPER_RELAY_METRICS=1 and a 1000-line log-ring snapshot behind PIPER_RELAY_LOGS=1. Both off by default; with neither toggle, nothing binds. Isolation is bind address + firewall — the listener is never reachable through the SNI dispatcher. Spec: docs/superpowers/specs/2026-07-26-relay-ops-endpoints-design.md.

Closes #N

🤖 Generated with [Claude Code](https://claude.com/claude-code)"
```

Expected: CI's `verify` gate green; squash-merge when approved.
