# One-command login — Plan 2: piperd socket enrollment + drain-then-exec apply

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** piperd itself claims the box and applies the enrollment: a local unix-socket surface accepts `{relay_api, account_credential}`, piperd mints a durable box-id, calls the relay's (now idempotent) enroll, **validates the returned token with a real tunnel handshake, persists `relay.json` into its own data dir, then drains and re-execs itself** — no sudo, no visible restart, on every install tier.

**Architecture:** A new `internal/enrollapi` leaf package holds the wire types shared with the CLI (Plan 3). `cmd/piperd/enroll.go` implements the socket surface as an `enrollServer` with injected seams (relay call, validate, apply) so every guard is unit-tested with fakes. Apply reuses `main()`'s existing shutdown machinery: an `applyExec` channel joins the tail `select`, and after the tested drain (`FailBuildingDeployments` and friends) the process `syscall.Exec`s its own image — same PID, so systemd/launchd observe nothing and the fresh boot runs today's relay wiring unchanged. The socket lives where only piperd can create it (systemd `RuntimeDirectory`, the root `/var/run/piper`, or the 0700 data dir), which is the whole authenticity story.

**Tech Stack:** Go 1.26, stdlib `net`/`net/http`/`syscall`, `internal/tunnel` (yamux handshake), `internal/relayclient`.

This is **Plan 2 of 3** for [`docs/superpowers/specs/2026-07-31-one-command-login-design.md`](../specs/2026-07-31-one-command-login-design.md). It **requires Plan 1** (`relayclient.Enroll(ctx, cred, boxID, org)` and the relay-side upsert) to be merged. Plan 3 wires the CLI.

## Global Constraints

- **No cgo**; `make cross` (arm64) must stay green. **Module path** `github.com/piperbox/piper`.
- **TDD**; `make verify` before every push, judged by exit status.
- **Layering:** the enroll surface lives in `cmd/piperd` (it orchestrates config + relayclient + tunnel + store) and must NOT be registered in `internal/api` — `api.New`'s mux is served, bearer-wrapped, to the relay tunnel, and a route there would let a hostile relay re-point the box. `internal/enrollapi` is a types-only leaf package (imports nothing but stdlib).
- **Terminated-only:** this surface always persists `Terminated: true`. BYO/non-terminated stays the operator env flow.
- **Validate before persist:** nothing touches `relay.json` until a real `tunnel.Dial` handshake with the new token has succeeded.
- **Never exit 0 on a failed apply:** `Restart=on-failure` (and brew `keep_alive`) only relaunch on non-zero/unclean exits.
- **Env is an operator pin:** `PIPER_RELAY_ADDR` in the environment ⇒ the enroll endpoint answers 409 `env-managed`. `config.Load`'s env-over-file precedence is untouched.
- **Unit contract tests** (`packaging/systemd/piperd_test.go`, `packaging/deb/deb_test.go`) must be updated in the same commit as any unit change; the two units may differ only in `ExecStart`.
- **Before Task 1:** file the tracking issue with the `file-issue` skill — title `[agent] daemon-owned enrollment: unix socket surface + drain-then-exec apply`, labels `agent`, `enhancement`, `P1`, `size/L` — and reference it as `Part of #<that issue>` in every commit.
- Commits are conventional-commit style ending with:
  `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`

## File Structure

- Create `internal/enrollapi/enrollapi.go` — wire types + route paths (shared with Plan 3's CLI).
- Modify `internal/config/config.go` — socket-path candidate vars/helper (CLI probe order; piperd serves, CLI dials).
- Modify `internal/agent/tunnelclient.go` (+ its test file) — `Status()` with last-error recording.
- Modify `internal/store/store.go` (+ test) — `CountBuildingDeployments`.
- Create `cmd/piperd/enroll.go` — `enrollServer`, `loadOrMintBoxID`, `validateEnrollment`, `enrollSocketPath`, `listenEnrollSocket`.
- Create `cmd/piperd/enroll_test.go` — guard/order/persistence tests with fakes.
- Create `cmd/piperd/reexec.go` + `cmd/piperd/reexec_test.go` — boot-identity gate + exec/exit seams.
- Modify `cmd/piperd/main.go` — capture boot identity; construct + serve the socket; `applyExec` in the tail select; drain then `execSelf()`.
- Modify `cmd/piperd/main_test.go` — contract test: enroll routes 404 on the control-API handler.
- Modify `packaging/deb/piperd.service`, `packaging/systemd/piperd.service`, `packaging/systemd/piperd_test.go`, `packaging/systemd/piperd.env.example`.

---

### Task 1: `internal/enrollapi` types + socket candidates in `internal/config`

**Files:**
- Create: `internal/enrollapi/enrollapi.go`
- Modify: `internal/config/config.go` (after the `SystemEnvFile` block, ~line 111)
- Create: `internal/enrollapi/enrollapi_test.go`
- Modify: `internal/config/config_test.go`

**Interfaces:**
- Consumes: nothing (leaf package).
- Produces (Plan 3 consumes all of this):
  - `enrollapi.PathStatus = "/v1/relay/status"`, `enrollapi.PathEnroll = "/v1/relay/enroll"`
  - `enrollapi.EnrollRequest{RelayAPI, AccountCredential, Org string; Replace bool}`
  - `enrollapi.EnrollResponse{BaseDomain, RelayAddr string}`
  - `enrollapi.ErrorResponse{Error, Detail, BaseDomain string}` with `Error` ∈ `env-managed | already-enrolled | busy | bad-credential | quota | validate-failed | enroll-failed | bad-request`
  - `enrollapi.Status{Enrolled, EnvManaged bool; RelayAddr, BaseDomain, Tunnel, LastTunnelError string}` with `Tunnel` ∈ `connected | retrying | off`
  - `config.EnrollSocketCandidates(dataDir string) []string` and the vars `config.SystemRuntimeSocket`, `config.DarwinRootSocket`

- [ ] **Step 1: Write the failing tests**

Create `internal/enrollapi/enrollapi_test.go`:

```go
package enrollapi

import (
	"encoding/json"
	"strings"
	"testing"
)

// The status shape is a fixed, secrets-free contract: it must marshal exactly
// these keys and nothing else (never a RelayFile), so a token can never leak
// through it by accident.
func TestStatusMarshalsFixedShape(t *testing.T) {
	b, err := json.Marshal(Status{Enrolled: true, EnvManaged: false,
		RelayAddr: "relay:7000", BaseDomain: "ab12-erin.public.getpiper.co",
		Tunnel: "retrying", LastTunnelError: "dial tcp: refused"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"enrolled":true`, `"relay_addr"`, `"base_domain"`, `"tunnel":"retrying"`, `"last_tunnel_error"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("marshal missing %s in %s", want, b)
		}
	}
	for _, banned := range []string{"token", "secret", "credential"} {
		if strings.Contains(string(b), banned) {
			t.Errorf("status shape must never carry %q: %s", banned, b)
		}
	}
}
```

Add to `internal/config/config_test.go`:

```go
func TestEnrollSocketCandidatesOrder(t *testing.T) {
	got := EnrollSocketCandidates("/tmp/dd")
	want := []string{SystemRuntimeSocket, DarwinRootSocket, filepath.Join("/tmp/dd", "piperd.sock")}
	if len(got) != len(want) {
		t.Fatalf("candidates = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidates[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/enrollapi/ ./internal/config/ -run 'TestStatusMarshalsFixedShape|TestEnrollSocketCandidatesOrder' -v`
Expected: FAIL — package `enrollapi` does not exist; `EnrollSocketCandidates` undefined.

- [ ] **Step 3: Implement**

Create `internal/enrollapi/enrollapi.go`:

```go
// Package enrollapi defines the wire contract of piperd's local enrollment
// socket: the surface `piper login` uses to hand a claimed enrollment to the
// daemon (one-command login design). It is a types-only leaf shared by
// cmd/piperd (server) and internal/client (CLI); the routes are deliberately
// NOT part of internal/api — they must never be reachable through the relay
// tunnel or the TCP control listeners.
package enrollapi

// Route paths on the enrollment socket. GET /v1/version also answers there,
// mirroring the control API's shape, so the CLI can positively identify piperd
// before sending anything.
const (
	PathStatus = "/v1/relay/status"
	PathEnroll = "/v1/relay/enroll"
)

// EnrollRequest asks piperd to claim this box on a relay. The account
// credential is used for the single relay call and never persisted on the box.
type EnrollRequest struct {
	RelayAPI          string `json:"relay_api"`
	AccountCredential string `json:"account_credential"`
	Org               string `json:"org,omitempty"`
	Replace           bool   `json:"replace,omitempty"`
}

// EnrollResponse reports a persisted (validated) enrollment.
type EnrollResponse struct {
	BaseDomain string `json:"base_domain"`
	RelayAddr  string `json:"relay_addr"`
}

// ErrorResponse is the JSON body of every non-2xx enrollment answer. Error is
// a machine code: env-managed, already-enrolled, busy, bad-credential, quota,
// validate-failed, enroll-failed, bad-request. BaseDomain rides along on
// already-enrolled so the CLI can report the current identity.
type ErrorResponse struct {
	Error      string `json:"error"`
	Detail     string `json:"detail,omitempty"`
	BaseDomain string `json:"base_domain,omitempty"`
}

// Status is the fixed, secrets-free view of the box's relay state. It is built
// field-by-field — never a marshal of config.RelayFile — so the enrollment
// token and webhook secret cannot leak through it. Tunnel is one of
// "connected", "retrying" (relay mode up, not currently connected), "off"
// (no relay mode).
type Status struct {
	Enrolled        bool   `json:"enrolled"`
	EnvManaged      bool   `json:"env_managed"`
	RelayAddr       string `json:"relay_addr,omitempty"`
	BaseDomain      string `json:"base_domain,omitempty"`
	Tunnel          string `json:"tunnel"`
	LastTunnelError string `json:"last_tunnel_error,omitempty"`
}
```

Add to `internal/config/config.go`, directly after `SystemEnvFile`:

```go
// SystemRuntimeSocket is piperd's enrollment socket under the shipped systemd
// unit's RuntimeDirectory=piper; DarwinRootSocket is its equivalent for a root
// (`sudo brew services`) macOS install. Vars so tests can point them at
// scratch paths.
var SystemRuntimeSocket = "/run/piper/piperd.sock"
var DarwinRootSocket = "/var/run/piper/piperd.sock"

// EnrollSocketCandidates lists, in probe order, where a local piperd may be
// serving its enrollment socket: the systemd runtime dir, the darwin root
// path, then the per-user data dir. Probing a path that does not exist on the
// current platform is harmless — connect simply fails.
func EnrollSocketCandidates(dataDir string) []string {
	return []string{SystemRuntimeSocket, DarwinRootSocket, filepath.Join(dataDir, "piperd.sock")}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/enrollapi/ ./internal/config/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/enrollapi/ internal/config/config.go internal/config/config_test.go
git commit -m "feat(agent): enrollapi wire types + enrollment-socket path candidates"
```

---

### Task 2: `TunnelClient.Status` — last-error recording

**Files:**
- Modify: `internal/agent/tunnelclient.go`
- Test: `internal/agent/tunnelclient_test.go` (add to the existing file)

**Interfaces:**
- Consumes: the existing `TunnelClient{mu, sess, OnConnect}` and `Run` loop.
- Produces: `func (c *TunnelClient) Status() (state, lastErr string)` — `"connected"` while a session is live; `"retrying"` (+ the last dial/handshake error) while `Run` is looping without one; `"off"` before `Run` starts. Today a rejected token only `log.Printf`s inside the retry loop; this is what makes it visible to the status surface.

- [ ] **Step 1: Write the failing test**

Add to `internal/agent/tunnelclient_test.go`:

```go
func TestStatusReportsRetryingWithLastError(t *testing.T) {
	c := &TunnelClient{}
	if state, _ := c.Status(); state != "off" {
		t.Fatalf("state before Run = %q, want off", state)
	}

	// Dial a port that refuses immediately; cancel once an error is recorded.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // now nothing listens there

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx, addr, "tok", "base.example", nil) }()

	deadline := time.After(5 * time.Second)
	for {
		state, lastErr := c.Status()
		if state == "retrying" && lastErr != "" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("never saw retrying+error; state=%q err=%q", state, lastErr)
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done
	if state, _ := c.Status(); state != "off" {
		t.Fatalf("state after Run returns = %q, want off", state)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/agent/ -run TestStatusReportsRetryingWithLastError -v`
Expected: compile FAIL — `c.Status undefined`.

- [ ] **Step 3: Implement**

In `internal/agent/tunnelclient.go`: extend the struct and add the recording.

```go
type TunnelClient struct {
	mu      sync.Mutex
	sess    *tunnel.Session
	running bool
	lastErr string

	// OnConnect, if set before Run, is invoked in its own goroutine each time a
	// relay session is established — piperd uses it to provision the relay's
	// control bearer (see the control-stream routing design).
	OnConnect func()
}

// Status reports the tunnel's state for the enrollment socket's status
// surface: "connected" with a live session, "retrying" while Run is looping
// without one (lastErr is the most recent dial/handshake failure — the only
// place a rejected enrollment token becomes visible outside the log), "off"
// when Run is not running.
func (c *TunnelClient) Status() (state, lastErr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.sess != nil:
		return "connected", ""
	case c.running:
		return "retrying", c.lastErr
	default:
		return "off", c.lastErr
	}
}

func (c *TunnelClient) setErr(err error) {
	c.mu.Lock()
	c.lastErr = err.Error()
	c.mu.Unlock()
}
```

In `Run`, mark the running window and record both failure sites; clear the error on a successful connect:

- at the top of `Run`:

```go
	c.mu.Lock()
	c.running = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.running = false
		c.mu.Unlock()
	}()
```

- after the dial error check (`log.Printf("tunnel: dial relay: ...")`): add `c.setErr(err)` on the line before the log.
- after the handshake error check (`log.Printf("tunnel: handshake: ...")`): add `c.setErr(err)` on the line before the log.
- in `setSession`, clear on connect:

```go
func (c *TunnelClient) setSession(s *tunnel.Session) {
	c.mu.Lock()
	c.sess = s
	if s != nil {
		c.lastErr = ""
	}
	c.mu.Unlock()
}
```

- [ ] **Step 4: Run the package tests**

Run: `go test ./internal/agent/ -v`
Expected: PASS (new test plus the existing Run/serveStreams suite).

- [ ] **Step 5: Commit**

```bash
git add internal/agent/tunnelclient.go internal/agent/tunnelclient_test.go
git commit -m "feat(agent): TunnelClient.Status exposes connect state and last tunnel error"
```

---

### Task 3: `store.CountBuildingDeployments`

**Files:**
- Modify: `internal/store/store.go` (next to `FailBuildingDeployments`)
- Test: `internal/store/store_test.go` (or wherever `FailBuildingDeployments`' test lives — `grep -rn "FailBuildingDeployments" internal/store/` and put the new test beside it, reusing its arrange helpers)

**Interfaces:**
- Consumes: the store's `deployments` table and the fixed status string `"building"` (CLAUDE.md hard constraint).
- Produces: `func (s *Store) CountBuildingDeployments() (int64, error)` — the enroll handler's "busy" guard (a restart must never kill a Docker build mid-flight).

- [ ] **Step 1: Write the failing test**

Locate the existing test for `FailBuildingDeployments` (`grep -rn "FailBuildingDeployments" internal/store/*_test.go`). Add a sibling test in the same file, reusing exactly the arrange helpers that test uses to create an app + a `"building"` deployment row, then assert:

```go
func TestCountBuildingDeployments(t *testing.T) {
	// arrange: same store-open + app/deployment setup as the
	// FailBuildingDeployments test in this file, leaving one row "building".

	n, err := st.CountBuildingDeployments()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("building = %d, want 1", n)
	}
	if _, err := st.FailBuildingDeployments(); err != nil {
		t.Fatal(err)
	}
	n, err = st.CountBuildingDeployments()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("building after fail-sweep = %d, want 0", n)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/store/ -run TestCountBuildingDeployments -v`
Expected: compile FAIL — `CountBuildingDeployments undefined`.

- [ ] **Step 3: Implement**

In `internal/store/store.go`, next to `FailBuildingDeployments` (match its receiver and db-field naming exactly):

```go
// CountBuildingDeployments reports how many deployments are mid-build. The
// enrollment apply refuses to restart piperd while one is in flight (409
// busy), so a Docker build is never killed halfway (see #158 for why a
// surviving "building" row is poison).
func (s *Store) CountBuildingDeployments() (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM deployments WHERE status = 'building'`).Scan(&n)
	return n, err
}
```

- [ ] **Step 4: Run the store tests** — `go test ./internal/store/ -v` — expected PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/
git commit -m "feat(store): CountBuildingDeployments for the enrollment busy guard"
```

---

### Task 4: box-id + `validateEnrollment`

**Files:**
- Create: `cmd/piperd/enroll.go`
- Create: `cmd/piperd/enroll_test.go`

**Interfaces:**
- Consumes: `internal/tunnel.Dial(conn, token, baseDomain)` (returns `(*tunnel.Session, error)`; caller closes `conn` on Dial failure, `sess.Close()` on success).
- Produces:
  - `func loadOrMintBoxID(dataDir string) (string, error)` — 32-hex durable identity at `<dataDir>/box-id`, minted once, created **before** the first relay call so a crashed enroll retries under the same identity.
  - `func validateEnrollment(ctx context.Context, relayAddr, token, baseDomain string) error` — real handshake, nothing persisted by it.

- [ ] **Step 1: Write the failing tests**

Create `cmd/piperd/enroll_test.go`:

```go
package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadOrMintBoxIDIsDurable(t *testing.T) {
	dir := t.TempDir()
	first, err := loadOrMintBoxID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 32 {
		t.Fatalf("box id = %q, want 32 hex chars", first)
	}
	second, err := loadOrMintBoxID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("box id not durable: %q -> %q", first, second)
	}
	fi, err := os.Stat(filepath.Join(dir, "box-id"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("box-id mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestValidateEnrollmentFailsOnUnreachableRelay(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := validateEnrollment(ctx, addr, "tok", "base.example"); err == nil {
		t.Fatal("validate against a dead relay must fail")
	}
}

func TestValidateEnrollmentFailsOnSilentRelay(t *testing.T) {
	// A listener that accepts and says nothing: tunnel.Dial must give up on
	// the ack wait, not report success.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
		}
	}()
	ctx := context.Background()
	err = validateEnrollment(ctx, ln.Addr().String(), "tok", "base.example")
	if err == nil {
		t.Fatal("validate against a silent relay must fail")
	}
	if !strings.Contains(err.Error(), "handshake") {
		t.Fatalf("err = %v, want a handshake failure", err)
	}
}
```

(The success path is exercised end-to-end in Plan 3's e2e; `tunnel.Dial`'s ack wait is bounded by its own `ackReadTimeout` of 10s, so the silent-relay test completes within the suite's budget.)

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./cmd/piperd/ -run 'TestLoadOrMintBoxID|TestValidateEnrollment' -v`
Expected: compile FAIL — both functions undefined.

- [ ] **Step 3: Implement**

Create `cmd/piperd/enroll.go`:

```go
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/piperbox/piper/internal/tunnel"
)

// loadOrMintBoxID returns this box's durable identity, minting and persisting
// it on first use. It exists BEFORE the first relay call so a crashed enroll
// retries under the same identity — the relay upserts its agent row on
// (account, box_id), which is what makes `piper login` idempotent. It never
// leaves the machine except inside the enroll request.
func loadOrMintBoxID(dataDir string) (string, error) {
	path := filepath.Join(dataDir, "box-id")
	if b, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(b)); id != "" {
			return id, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	id := hex.EncodeToString(raw)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		return "", err
	}
	return id, nil
}

// enrollDialTimeout bounds the validation dial, mirroring the tunnel client's
// own relayDialTimeout.
const enrollDialTimeout = 10 * time.Second

// validateEnrollment proves an enrollment works before anything is persisted:
// it dials the relay and completes a real tunnel handshake with the token,
// then closes the throwaway session. A bad endpoint or rejected token fails
// here — so a poisoned enrollment can never reach relay.json and crash-loop
// the next boot (validate-before-persist, one-command login design). The
// validation session is closed immediately; a transient second session for an
// already-connected box (the --re-enroll path) is accepted.
func validateEnrollment(ctx context.Context, relayAddr, token, baseDomain string) error {
	conn, err := (&net.Dialer{Timeout: enrollDialTimeout}).DialContext(ctx, "tcp", relayAddr)
	if err != nil {
		return fmt.Errorf("dial relay: %w", err)
	}
	sess, err := tunnel.Dial(conn, token, baseDomain)
	if err != nil {
		conn.Close() // tunnel.Dial does not close conn on failure
		return fmt.Errorf("relay handshake: %w", err)
	}
	return sess.Close()
}
```

- [ ] **Step 4: Run the tests** — `go test ./cmd/piperd/ -run 'TestLoadOrMintBoxID|TestValidateEnrollment' -v` — expected PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/piperd/enroll.go cmd/piperd/enroll_test.go
git commit -m "feat(agent): durable box-id + validate-before-persist relay handshake"
```

---

### Task 5: `enrollServer` — guards, ordering, persistence

**Files:**
- Modify: `cmd/piperd/enroll.go`
- Modify: `cmd/piperd/enroll_test.go`

**Interfaces:**
- Consumes: Task 1's `enrollapi` types, Task 4's `loadOrMintBoxID`, `config.LoadRelayFile`/`SaveRelayFile`, `relayclient.Enrollment`/`ErrBadCredential`/`ErrQuotaExceeded`.
- Produces: `type enrollServer struct{...}` with seam fields (below) and `func (s *enrollServer) mux() *http.ServeMux` serving `GET /v1/version`, `GET /v1/relay/status`, `POST /v1/relay/enroll`. **Guard order (spec):** bad-request → env-managed → already-enrolled (unless `Replace`) → busy (building or another enroll in flight) → relay enroll → validate → persist → respond → async apply.

- [ ] **Step 1: Write the failing tests**

Add to `cmd/piperd/enroll_test.go`:

```go
// newTestEnrollServer returns a server whose seams are all benign fakes plus
// recorders for what got called.
type enrollRec struct {
	enrolled  []string // "api|cred|boxID|org" per relayEnroll call
	validated []string // "addr|token|base" per validate call
	applied   int
}

func newTestEnrollServer(t *testing.T, dataDir string) (*enrollServer, *enrollRec) {
	t.Helper()
	rec := &enrollRec{}
	s := &enrollServer{
		dataDir:    dataDir,
		version:    "test",
		envManaged: func() bool { return false },
		relayStatus: func() (string, string) { return "", "" },
		tunnelStatus: func() (string, string) { return "off", "" },
		relayEnroll: func(ctx context.Context, api, cred, boxID, org string) (relayclient.Enrollment, error) {
			rec.enrolled = append(rec.enrolled, api+"|"+cred+"|"+boxID+"|"+org)
			return relayclient.Enrollment{
				EnrollmentToken: "enr-1", BaseDomain: "ab12-erin.public.getpiper.co",
				TunnelEndpoint: "relay.getpiper.co:7000", WebhookSecret: "whsec-1", GitHubApp: true,
			}, nil
		},
		validate: func(ctx context.Context, addr, token, base string) error {
			rec.validated = append(rec.validated, addr+"|"+token+"|"+base)
			return nil
		},
		countBuilding: func() (int64, error) { return 0, nil },
		apply:         func() { rec.applied++ },
	}
	return s, rec
}

func postEnroll(t *testing.T, h http.Handler, req enrollapi.EnrollRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(req)
	r := httptest.NewRequest(http.MethodPost, enrollapi.PathEnroll, bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e enrollapi.ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&e); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, rec.Body.String())
	}
	return e.Error
}

func TestEnrollHappyPathPersistsThenApplies(t *testing.T) {
	dir := t.TempDir()
	s, rec := newTestEnrollServer(t, dir)
	res := postEnroll(t, s.mux(), enrollapi.EnrollRequest{RelayAPI: "https://api.relay", AccountCredential: "cred-xyz"})
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", res.Code, res.Body.String())
	}
	var out enrollapi.EnrollResponse
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.BaseDomain != "ab12-erin.public.getpiper.co" {
		t.Fatalf("resp = %+v", out)
	}
	if res.Header().Get("Content-Length") == "" {
		t.Fatal("response must carry Content-Length (flushed before the apply)")
	}
	rf, found, err := config.LoadRelayFile(dir)
	if err != nil || !found {
		t.Fatalf("relay.json: found=%v err=%v", found, err)
	}
	want := config.RelayFile{RelayAddr: "relay.getpiper.co:7000", RelayToken: "enr-1",
		BaseDomain: "ab12-erin.public.getpiper.co", Terminated: true,
		WebhookSecret: "whsec-1", GitHubBrokered: true}
	if rf != want {
		t.Fatalf("relay.json = %+v, want %+v", rf, want)
	}
	// The relay was called with the durable box id; validate saw the enrollment.
	boxID, _ := loadOrMintBoxID(dir)
	if len(rec.enrolled) != 1 || rec.enrolled[0] != "https://api.relay|cred-xyz|"+boxID+"|" {
		t.Fatalf("relayEnroll calls = %v", rec.enrolled)
	}
	if len(rec.validated) != 1 || rec.validated[0] != "relay.getpiper.co:7000|enr-1|ab12-erin.public.getpiper.co" {
		t.Fatalf("validate calls = %v", rec.validated)
	}
	waitFor(t, func() bool { return rec.applied == 1 }) // apply is async
}

func TestEnrollValidateFailurePersistsNothing(t *testing.T) {
	dir := t.TempDir()
	s, _ := newTestEnrollServer(t, dir)
	s.validate = func(ctx context.Context, addr, token, base string) error {
		return errors.New("relay handshake: rejected")
	}
	res := postEnroll(t, s.mux(), enrollapi.EnrollRequest{RelayAPI: "https://api.relay", AccountCredential: "cred-xyz"})
	if res.Code != http.StatusBadGateway || errCode(t, res) != "validate-failed" {
		t.Fatalf("status = %d code = %s", res.Code, res.Body.String())
	}
	if _, found, _ := config.LoadRelayFile(dir); found {
		t.Fatal("relay.json written despite failed validation")
	}
}

func TestEnrollGuardsInSpecOrder(t *testing.T) {
	dir := t.TempDir()

	// bad request
	s, rec := newTestEnrollServer(t, dir)
	res := postEnroll(t, s.mux(), enrollapi.EnrollRequest{})
	if res.Code != http.StatusBadRequest || errCode(t, res) != "bad-request" {
		t.Fatalf("empty request: %d %s", res.Code, res.Body.String())
	}

	// env-managed wins before anything else
	s.envManaged = func() bool { return true }
	res = postEnroll(t, s.mux(), enrollapi.EnrollRequest{RelayAPI: "a", AccountCredential: "c"})
	if res.Code != http.StatusConflict || errCode(t, res) != "env-managed" {
		t.Fatalf("env-managed: %d %s", res.Code, res.Body.String())
	}
	s.envManaged = func() bool { return false }

	// already-enrolled unless Replace
	if err := config.SaveRelayFile(dir, config.RelayFile{RelayAddr: "r:1", RelayToken: "old",
		BaseDomain: "old-base.public.getpiper.co", Terminated: true}); err != nil {
		t.Fatal(err)
	}
	res = postEnroll(t, s.mux(), enrollapi.EnrollRequest{RelayAPI: "a", AccountCredential: "c"})
	if res.Code != http.StatusConflict || errCode(t, res) != "already-enrolled" {
		t.Fatalf("already-enrolled: %d %s", res.Code, res.Body.String())
	}
	var e enrollapi.ErrorResponse
	res2 := postEnroll(t, s.mux(), enrollapi.EnrollRequest{RelayAPI: "a", AccountCredential: "c"})
	_ = json.NewDecoder(res2.Body).Decode(&e)
	if e.BaseDomain != "old-base.public.getpiper.co" {
		t.Fatalf("already-enrolled must report the current base domain, got %+v", e)
	}
	res = postEnroll(t, s.mux(), enrollapi.EnrollRequest{RelayAPI: "a", AccountCredential: "c", Replace: true})
	if res.Code != http.StatusOK {
		t.Fatalf("replace: %d %s", res.Code, res.Body.String())
	}
	if rec.enrolled == nil {
		t.Fatal("replace did not reach the relay")
	}
}

func TestEnrollBusyWhileBuilding(t *testing.T) {
	dir := t.TempDir()
	s, rec := newTestEnrollServer(t, dir)
	s.countBuilding = func() (int64, error) { return 1, nil }
	res := postEnroll(t, s.mux(), enrollapi.EnrollRequest{RelayAPI: "a", AccountCredential: "c"})
	if res.Code != http.StatusConflict || errCode(t, res) != "busy" {
		t.Fatalf("building guard: %d %s", res.Code, res.Body.String())
	}
	if len(rec.enrolled) != 0 {
		t.Fatal("busy guard must fire before the relay call")
	}
}

func TestEnrollSerializesConcurrentRequests(t *testing.T) {
	dir := t.TempDir()
	s, _ := newTestEnrollServer(t, dir)
	inFirst := make(chan struct{})
	release := make(chan struct{})
	s.relayEnroll = func(ctx context.Context, api, cred, boxID, org string) (relayclient.Enrollment, error) {
		close(inFirst)
		<-release
		return relayclient.Enrollment{EnrollmentToken: "enr-1", BaseDomain: "b.example",
			TunnelEndpoint: "r:1"}, nil
	}
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		firstDone <- postEnroll(t, s.mux(), enrollapi.EnrollRequest{RelayAPI: "a", AccountCredential: "c"})
	}()
	<-inFirst
	second := postEnroll(t, s.mux(), enrollapi.EnrollRequest{RelayAPI: "a", AccountCredential: "c"})
	if second.Code != http.StatusConflict || errCode(t, second) != "busy" {
		t.Fatalf("concurrent enroll: %d %s", second.Code, second.Body.String())
	}
	close(release)
	if res := <-firstDone; res.Code != http.StatusOK {
		t.Fatalf("first enroll: %d %s", res.Code, res.Body.String())
	}
}

func TestEnrollMapsRelayErrors(t *testing.T) {
	dir := t.TempDir()
	s, _ := newTestEnrollServer(t, dir)
	s.relayEnroll = func(ctx context.Context, api, cred, boxID, org string) (relayclient.Enrollment, error) {
		return relayclient.Enrollment{}, relayclient.ErrBadCredential
	}
	res := postEnroll(t, s.mux(), enrollapi.EnrollRequest{RelayAPI: "a", AccountCredential: "c"})
	if res.Code != http.StatusUnauthorized || errCode(t, res) != "bad-credential" {
		t.Fatalf("bad credential: %d %s", res.Code, res.Body.String())
	}
	s.relayEnroll = func(ctx context.Context, api, cred, boxID, org string) (relayclient.Enrollment, error) {
		return relayclient.Enrollment{}, relayclient.ErrQuotaExceeded
	}
	res = postEnroll(t, s.mux(), enrollapi.EnrollRequest{RelayAPI: "a", AccountCredential: "c"})
	if res.Code != http.StatusTooManyRequests || errCode(t, res) != "quota" {
		t.Fatalf("quota: %d %s", res.Code, res.Body.String())
	}
}

func TestStatusNeverLeaksSecrets(t *testing.T) {
	dir := t.TempDir()
	s, _ := newTestEnrollServer(t, dir)
	if err := config.SaveRelayFile(dir, config.RelayFile{RelayAddr: "r:1", RelayToken: "SECRET-TOKEN",
		BaseDomain: "b.example", Terminated: true, WebhookSecret: "SECRET-WHSEC"}); err != nil {
		t.Fatal(err)
	}
	s.relayStatus = func() (string, string) { return "r:1", "b.example" }
	r := httptest.NewRequest(http.MethodGet, enrollapi.PathStatus, nil)
	rec := httptest.NewRecorder()
	s.mux().ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	body := rec.Body.String()
	for _, banned := range []string{"SECRET-TOKEN", "SECRET-WHSEC"} {
		if strings.Contains(body, banned) {
			t.Fatalf("status leaked %q: %s", banned, body)
		}
	}
	var st enrollapi.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if !st.Enrolled || st.BaseDomain != "b.example" || st.Tunnel != "off" {
		t.Fatalf("status = %+v", st)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatal("condition never became true")
		case <-time.After(5 * time.Millisecond):
		}
	}
}
```

Add the imports the file now needs (`bytes`, `encoding/json`, `net/http`, `net/http/httptest`, `github.com/piperbox/piper/internal/config`, `github.com/piperbox/piper/internal/enrollapi`, `github.com/piperbox/piper/internal/relayclient`).

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./cmd/piperd/ -run 'TestEnroll|TestStatusNeverLeaks' -v`
Expected: compile FAIL — `enrollServer` undefined.

- [ ] **Step 3: Implement the server**

Add to `cmd/piperd/enroll.go`:

```go
// enrollServer is the local enrollment surface: version + status + enroll,
// served ONLY on the piperd-owned unix socket — never on the TCP control API
// and never on the relay-facing authenticated listener (a route there would
// let a hostile relay re-point the box; see the one-command login design).
// Every side effect goes through a seam so the guards unit-test with fakes.
type enrollServer struct {
	dataDir string
	version string

	envManaged    func() bool
	relayStatus   func() (relayAddr, baseDomain string)
	tunnelStatus  func() (state, lastErr string)
	relayEnroll   func(ctx context.Context, api, cred, boxID, org string) (relayclient.Enrollment, error)
	validate      func(ctx context.Context, relayAddr, token, baseDomain string) error
	countBuilding func() (int64, error)
	apply         func() // async: hands off to main's drain-then-exec

	mu sync.Mutex // serializes enrolls; a held lock answers 409 busy
}

func (s *enrollServer) mux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("GET /v1/version", func(w http.ResponseWriter, r *http.Request) {
		writeEnrollJSON(w, http.StatusOK, map[string]string{"version": s.version})
	})
	m.HandleFunc("GET "+enrollapi.PathStatus, s.status)
	m.HandleFunc("POST "+enrollapi.PathEnroll, s.enroll)
	return m
}

// status is a fixed, secrets-free shape built field-by-field — never a marshal
// of config.RelayFile.
func (s *enrollServer) status(w http.ResponseWriter, r *http.Request) {
	_, found, _ := config.LoadRelayFile(s.dataDir)
	env := s.envManaged()
	st := enrollapi.Status{Enrolled: found || env, EnvManaged: env}
	if st.Enrolled {
		st.RelayAddr, st.BaseDomain = s.relayStatus()
	}
	st.Tunnel, st.LastTunnelError = s.tunnelStatus()
	writeEnrollJSON(w, http.StatusOK, st)
}

func (s *enrollServer) enroll(w http.ResponseWriter, r *http.Request) {
	var req enrollapi.EnrollRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil ||
		req.RelayAPI == "" || req.AccountCredential == "" {
		writeEnrollError(w, http.StatusBadRequest, "bad-request",
			"relay_api and account_credential are required", "")
		return
	}
	if s.envManaged() {
		writeEnrollError(w, http.StatusConflict, "env-managed",
			"enrollment is pinned by PIPER_RELAY_* environment (see "+config.SystemEnvFile()+")", "")
		return
	}
	if !s.mu.TryLock() {
		writeEnrollError(w, http.StatusConflict, "busy", "another enrollment is in flight", "")
		return
	}
	defer s.mu.Unlock()
	if rf, found, err := config.LoadRelayFile(s.dataDir); err != nil {
		writeEnrollError(w, http.StatusInternalServerError, "enroll-failed", err.Error(), "")
		return
	} else if found && !req.Replace {
		writeEnrollError(w, http.StatusConflict, "already-enrolled", "", rf.BaseDomain)
		return
	}
	if n, err := s.countBuilding(); err != nil {
		writeEnrollError(w, http.StatusInternalServerError, "enroll-failed", err.Error(), "")
		return
	} else if n > 0 {
		writeEnrollError(w, http.StatusConflict, "busy",
			"a deployment is building; retry when it finishes", "")
		return
	}
	boxID, err := loadOrMintBoxID(s.dataDir)
	if err != nil {
		writeEnrollError(w, http.StatusInternalServerError, "enroll-failed", err.Error(), "")
		return
	}
	en, err := s.relayEnroll(r.Context(), req.RelayAPI, req.AccountCredential, boxID, req.Org)
	switch {
	case errors.Is(err, relayclient.ErrBadCredential):
		writeEnrollError(w, http.StatusUnauthorized, "bad-credential",
			"relay rejected the account credential", "")
		return
	case errors.Is(err, relayclient.ErrQuotaExceeded):
		writeEnrollError(w, http.StatusTooManyRequests, "quota",
			"account agent quota exceeded", "")
		return
	case err != nil:
		writeEnrollError(w, http.StatusBadGateway, "enroll-failed", err.Error(), "")
		return
	}
	if err := s.validate(r.Context(), en.TunnelEndpoint, en.EnrollmentToken, en.BaseDomain); err != nil {
		// Nothing persisted: a poisoned enrollment must never reach relay.json.
		writeEnrollError(w, http.StatusBadGateway, "validate-failed", err.Error(), "")
		return
	}
	if err := config.SaveRelayFile(s.dataDir, config.RelayFile{
		RelayAddr: en.TunnelEndpoint, RelayToken: en.EnrollmentToken,
		BaseDomain: en.BaseDomain, Terminated: true,
		WebhookSecret: en.WebhookSecret, GitHubBrokered: en.GitHubApp,
	}); err != nil {
		writeEnrollError(w, http.StatusInternalServerError, "enroll-failed", err.Error(), "")
		return
	}
	// Deliver the response completely before the apply tears this listener
	// down: explicit Content-Length + Flush, then hand off asynchronously (the
	// drain waits for this handler to return, so apply cannot run inline).
	body, _ := json.Marshal(enrollapi.EnrollResponse{BaseDomain: en.BaseDomain, RelayAddr: en.TunnelEndpoint})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	log.Printf("enrolled as %s via %s (box %s); applying", en.BaseDomain, en.TunnelEndpoint, boxID)
	go s.apply()
}

func writeEnrollJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeEnrollError(w http.ResponseWriter, code int, errCode, detail, baseDomain string) {
	writeEnrollJSON(w, code, enrollapi.ErrorResponse{Error: errCode, Detail: detail, BaseDomain: baseDomain})
}
```

Add the new imports to `enroll.go` (`encoding/json`, `log`, `net/http`, `strconv`, `sync`, `github.com/piperbox/piper/internal/config`, `github.com/piperbox/piper/internal/enrollapi`, `github.com/piperbox/piper/internal/relayclient`).

- [ ] **Step 4: Run the tests** — `go test ./cmd/piperd/ -v` — expected PASS (the whole package).

- [ ] **Step 5: Commit**

```bash
git add cmd/piperd/enroll.go cmd/piperd/enroll_test.go
git commit -m "feat(agent): enrollServer — guarded, validated, daemon-persisted claims"
```

---

### Task 6: re-exec with a boot-identity gate

**Files:**
- Create: `cmd/piperd/reexec.go`
- Create: `cmd/piperd/reexec_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: `captureBootExe()` (called first thing in `main`), `execSelf()` (called by `main` after the drain), seams `execFn` (defaults `syscall.Exec`) and `exitFn` (defaults `os.Exit`).

- [ ] **Step 1: Write the failing tests**

Create `cmd/piperd/reexec_test.go`:

```go
package main

import (
	"testing"
)

func stubExec(t *testing.T) (execs *[][]string, exits *[]int) {
	t.Helper()
	var e [][]string
	var x []int
	oldExec, oldExit := execFn, exitFn
	execFn = func(argv0 string, argv []string, envv []string) error {
		e = append(e, append([]string{argv0}, argv...))
		return nil
	}
	exitFn = func(code int) { x = append(x, code) }
	t.Cleanup(func() { execFn, exitFn = oldExec, oldExit })
	return &e, &x
}

func TestExecSelfRunsWhenBinaryUnchanged(t *testing.T) {
	execs, exits := stubExec(t)
	captureBootExe()
	execSelf()
	if len(*exits) != 0 {
		t.Fatalf("exited %v instead of exec", *exits)
	}
	if len(*execs) != 1 {
		t.Fatalf("execs = %v", *execs)
	}
}

func TestExecSelfRefusesWhenBinaryChanged(t *testing.T) {
	execs, exits := stubExec(t)
	captureBootExe()
	old := bootExe
	bootExe.ino = old.ino + 1 // simulate a replaced binary
	t.Cleanup(func() { bootExe = old })
	execSelf()
	if len(*execs) != 0 {
		t.Fatal("exec'd a binary that changed since boot")
	}
	if len(*exits) != 1 || (*exits)[0] != 1 {
		t.Fatalf("exits = %v, want [1] (supervised restart)", *exits)
	}
}

func TestExecSelfExitsOneWhenIdentityUnknown(t *testing.T) {
	_, exits := stubExec(t)
	old := bootExe
	bootExe.ok = false
	t.Cleanup(func() { bootExe = old })
	execSelf()
	if len(*exits) != 1 || (*exits)[0] != 1 {
		t.Fatalf("exits = %v, want [1]", *exits)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./cmd/piperd/ -run TestExecSelf -v`
Expected: compile FAIL — `execFn`, `captureBootExe`, `execSelf` undefined.

- [ ] **Step 3: Implement**

Create `cmd/piperd/reexec.go`:

```go
package main

import (
	"errors"
	"log"
	"os"
	"runtime"
	"syscall"
)

// bootExe pins the identity (device+inode) of the binary this process booted
// from. execSelf refuses to re-exec when the on-disk executable no longer
// matches: on a root install whose program path a non-root user can write
// (sudo-brew macOS), an unchecked re-exec would run a replaced binary as root
// from a loopback-triggerable endpoint. A mismatch falls back to exit(1) so
// the supervisor relaunches the (new) binary under its own hardening.
var bootExe struct {
	dev, ino uint64
	ok       bool
}

func captureBootExe() {
	path, err := os.Executable()
	if err != nil {
		return
	}
	if dev, ino, err := statDevIno(path); err == nil {
		bootExe.dev, bootExe.ino, bootExe.ok = dev, ino, true
	}
}

func statDevIno(path string) (uint64, uint64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, errors.New("no stat_t")
	}
	return uint64(st.Dev), uint64(st.Ino), nil
}

// Seams so tests observe the exec/exit decision without leaving the process.
var execFn = syscall.Exec
var exitFn = os.Exit

// execSelf replaces this process with a fresh boot of the same image — same
// PID, so systemd/launchd observe nothing, and the new boot re-runs
// config.Load and all the relay wiring. Callers must have fully drained
// first. On any doubt it exits 1 instead: Restart=on-failure and brew
// keep_alive relaunch piperd supervised, re-reading EnvironmentFile and
// re-applying sandboxing. Never exit 0 here — on systemd that leaves the box
// down for good.
func execSelf() {
	path, err := os.Executable()
	if err != nil {
		log.Printf("re-exec: cannot resolve executable: %v; exiting for a supervised restart", err)
		exitFn(1)
		return
	}
	if dev, ino, err := statDevIno(path); err != nil || !bootExe.ok || dev != bootExe.dev || ino != bootExe.ino {
		log.Printf("re-exec: binary changed since boot; exiting for a supervised restart")
		exitFn(1)
		return
	}
	if runtime.GOOS == "linux" {
		path = "/proc/self/exe" // immune to on-disk replacement races
	}
	log.Printf("applying enrollment: re-exec %s", path)
	if err := execFn(path, os.Args, os.Environ()); err != nil {
		log.Printf("re-exec failed: %v; exiting for a supervised restart", err)
		exitFn(1)
	}
}
```

- [ ] **Step 4: Run the tests** — `go test ./cmd/piperd/ -run TestExecSelf -v` — expected PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/piperd/reexec.go cmd/piperd/reexec_test.go
git commit -m "feat(agent): drain-safe re-exec with a boot-identity gate"
```

---

### Task 7: socket serving + `main()` apply wiring + unit files

**Files:**
- Modify: `cmd/piperd/enroll.go` (add `enrollSocketPath`, `listenEnrollSocket`)
- Modify: `cmd/piperd/enroll_test.go`
- Modify: `cmd/piperd/main.go`
- Modify: `cmd/piperd/main_test.go` (contract test)
- Modify: `packaging/deb/piperd.service`, `packaging/systemd/piperd.service`
- Modify: `packaging/systemd/piperd_test.go`
- Modify: `packaging/systemd/piperd.env.example`

**Interfaces:**
- Consumes: everything from Tasks 1–6; `main()`'s existing tail (`<-ctx.Done()` → join tunnel → `shutdownWithContext(overallCtx, apiServers{srv, authSrv}, ...)` → `os.Exit(0)`); `api.New` and `startAuthAPI` as-is.
- Produces: piperd serves the enroll mux on a unix socket at `enrollSocketPath(cfg.DataDir)`; an accepted enrollment sends on `applyExec`, which drains exactly like a signal shutdown and then calls `execSelf()`. **Plan 3 relies on:** the socket answering `GET /v1/version` and the socket path being one of `config.EnrollSocketCandidates(dataDir)`.

- [ ] **Step 1: Write the failing tests**

Add to `cmd/piperd/enroll_test.go`:

```go
func TestEnrollSocketPathPrecedence(t *testing.T) {
	t.Setenv("RUNTIME_DIRECTORY", "/run/piper")
	if got := enrollSocketPath("/dd"); got != "/run/piper/piperd.sock" {
		t.Fatalf("systemd path = %q", got)
	}
	t.Setenv("RUNTIME_DIRECTORY", "")
	if got := enrollSocketPath("/dd"); got != filepath.Join("/dd", "piperd.sock") {
		// On a non-root test run the darwin-root branch cannot fire, so the
		// data-dir fallback is the expected answer on both platforms.
		t.Fatalf("fallback path = %q", got)
	}
}

func TestListenEnrollSocketReplacesStaleSocket(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "piperd.sock")
	ln1, err := listenEnrollSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	ln1.Close() // leaves the socket file behind, as a crash would
	ln2, err := listenEnrollSocket(path)
	if err != nil {
		t.Fatalf("rebind over stale socket: %v", err)
	}
	defer ln2.Close()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o666 {
		t.Fatalf("socket mode = %v, want 0666 (dir perms are the auth)", fi.Mode().Perm())
	}
}
```

Add to `cmd/piperd/main_test.go`:

```go
// The enrollment routes must not exist on the control-API handler: that mux is
// also served, bearer-wrapped, to the relay tunnel (startAuthAPI), and a route
// there would let whoever holds the relay-side control bearer re-point this
// box at another relay. Serving them only from enrollServer.mux() on the unix
// socket is the isolation; this test pins it.
func TestEnrollRoutesNotOnControlAPI(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "piper.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := api.New(st, nil, "piper.localhost", "", func() {}, nil, nil,
		func() string { return "" }, nil)
	for _, probe := range []struct{ method, path string }{
		{http.MethodPost, enrollapi.PathEnroll},
		{http.MethodGet, enrollapi.PathStatus},
	} {
		req := httptest.NewRequest(probe.method, probe.path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s on the control API: status %d, want 404", probe.method, probe.path, rec.Code)
		}
	}
}
```

(If `api.New`'s nil arguments trip a typed-nil guard, mirror how existing `main_test.go` tests construct the handler — the probe paths never dispatch into any dependency.)

In `packaging/systemd/piperd_test.go`, add to the `required` slice in `TestPiperdServiceContract`:

```go
		"RuntimeDirectory=piper",
		"RuntimeDirectoryMode=0755",
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./cmd/piperd/ -run 'TestEnrollSocketPath|TestListenEnrollSocket|TestEnrollRoutesNotOnControlAPI' -v && go test ./packaging/systemd/ -run TestPiperdServiceContract -v`
Expected: compile FAIL in cmd/piperd (functions undefined); FAIL in packaging (missing directives).

- [ ] **Step 3: Implement the socket helpers**

Add to `cmd/piperd/enroll.go`:

```go
// enrollSocketPath decides where this piperd serves its enrollment socket:
// the systemd RuntimeDirectory when the unit provides one (systemd exports
// RUNTIME_DIRECTORY for RuntimeDirectory=piper), the fixed /var/run path for a
// root macOS (`sudo brew services`) install, else the per-user data dir. The
// CLI probes the same set via config.EnrollSocketCandidates.
func enrollSocketPath(dataDir string) string {
	if rd := os.Getenv("RUNTIME_DIRECTORY"); rd != "" {
		return filepath.Join(rd, "piperd.sock")
	}
	if runtime.GOOS == "darwin" && os.Geteuid() == 0 {
		return config.DarwinRootSocket
	}
	return filepath.Join(dataDir, "piperd.sock")
}

// listenEnrollSocket binds the unix socket, replacing a stale file left by a
// crash. The socket itself is 0666: the DIRECTORY is what authenticates the
// server — only piperd's user (or root) can create a socket inside the
// RuntimeDirectory / the 0700 data dir, while any local user may talk to it,
// the same trust stance as the tokenless loopback API. Browsers cannot speak
// unix sockets and a port squatter cannot bind here, which is why this is a
// socket and not a TCP port (one-command login design).
func listenEnrollSocket(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o666); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}
```

(Add `runtime` to `enroll.go`'s imports.)

- [ ] **Step 4: Wire `main()`**

Four edits in `cmd/piperd/main.go`:

**(a)** First line of the non-token path in `main()` (right after `cfg := config.Load()`): add

```go
	captureBootExe()
```

**(b)** After the `startAuthAPI` block and before the relay wiring block, construct the enroll surface (it needs `st`, `cfg`, and the `tc` variable which is already declared above it):

```go
	// The enrollment socket: the daemon-owned path for `piper login` to claim
	// this box (one-command login design). Deliberately not part of apiHandler:
	// these routes must be unreachable through the relay tunnel and the TCP
	// listeners (pinned by TestEnrollRoutesNotOnControlAPI).
	applyExec := make(chan struct{}, 1)
	es := &enrollServer{
		dataDir:    cfg.DataDir,
		version:    version.String(),
		envManaged: func() bool { return os.Getenv("PIPER_RELAY_ADDR") != "" },
		relayStatus: func() (string, string) { return cfg.RelayAddr, cfg.BaseDomain },
		tunnelStatus: func() (string, string) {
			if tc == nil {
				return "off", ""
			}
			return tc.Status()
		},
		relayEnroll: func(ctx context.Context, api, cred, boxID, org string) (relayclient.Enrollment, error) {
			return relayclient.New(api).Enroll(ctx, cred, boxID, org)
		},
		validate:      validateEnrollment,
		countBuilding: st.CountBuildingDeployments,
		apply: func() {
			select {
			case applyExec <- struct{}{}:
			default: // an apply is already queued
			}
		},
	}
	sockPath := enrollSocketPath(cfg.DataDir)
	sockLn, err := listenEnrollSocket(sockPath)
	if err != nil {
		log.Fatalf("enroll socket: %v", err)
	}
	enrollSrv := &http.Server{Handler: es.mux()}
	go func() {
		if err := enrollSrv.Serve(sockLn); err != nil && err != http.ErrServerClosed {
			log.Printf("enroll socket serve: %v", err)
		}
	}()
	log.Printf("enrollment socket at %s", sockPath)
```

(Add `github.com/piperbox/piper/internal/relayclient` to `main.go`'s imports.)

**(c)** Replace the tail's `<-ctx.Done()` line with the apply-aware select:

```go
	reexec := false
	select {
	case <-ctx.Done():
	case <-applyExec:
		log.Println("enrollment accepted; restarting to apply")
		reexec = true
		stop() // wind the tunnel and stream handlers down exactly like a signal
	}
```

**(d)** Fold `enrollSrv` into the drain and branch the exit: change the `shutdownWithContext` call to

```go
	shutdownWithContext(overallCtx, apiServers{srv, authSrv, enrollSrv}, whLifecycle, mgrStop, st, drainTimeout)
	if reexec {
		execSelf() // never returns on success; exits 1 on refusal/failure
	}
	os.Exit(0)
```

The full tested drain (`FailBuildingDeployments`, tunnel join before the ALPN close, webhook stop, Caddy stop, store close) now runs before every re-exec — a raw exec skipping it would resurrect the permanent-`building`-row bug (#158).

- [ ] **Step 5: Update the unit files**

In **both** `packaging/deb/piperd.service` and `packaging/systemd/piperd.service`, insert directly under `StateDirectoryMode=0700`:

```
RuntimeDirectory=piper
RuntimeDirectoryMode=0755
```

(The two units must stay identical except `ExecStart` — `TestDebUnitOnlyDiffersInExecStart` enforces it.)

In `packaging/systemd/piperd.env.example`, replace the two relay-key comment lines:

```
# --- Relay (operator pin / BYO only — `piper login` manages enrollment itself) ---
# Setting PIPER_RELAY_ADDR here overrides the daemon-managed relay.json AND
# locks piperd's enrollment endpoint (piper login will refuse; edit here
# instead). Leave unset for the normal `piper login` flow.
#PIPER_RELAY_ADDR=
# Enrollment token presented to the relay (operator/BYO installs only).
#PIPER_RELAY_TOKEN=
```

- [ ] **Step 6: Run the full gate**

Run: `make verify`
Expected: exit 0 — the new cmd/piperd tests, the packaging contract tests, and everything pre-existing.

- [ ] **Step 7: Commit**

```bash
git add cmd/piperd/ packaging/deb/piperd.service packaging/systemd/piperd.service packaging/systemd/piperd_test.go packaging/systemd/piperd.env.example
git commit -m "feat(agent): serve the enrollment socket; apply via drain-then-exec"
```

---

### Task 8: PR

- [ ] **Step 1:** `make verify` (exit 0), then `git push -u origin HEAD`.

- [ ] **Step 2: Open the PR**

```bash
gh pr create --base main --title "feat(agent): daemon-owned enrollment over a unix socket, applied by drain-then-exec" --body "Agent half of the one-command login design (docs/superpowers/specs/2026-07-31-one-command-login-design.md): piperd serves /v1/relay/{status,enroll} on a piperd-owned unix socket, mints a durable box-id, calls the relay's idempotent enroll itself, validates the token with a real tunnel handshake BEFORE persisting relay.json, then drains through the existing shutdown path and re-execs in place (boot-identity gated). Env PIPER_RELAY_* becomes an operator pin that locks the endpoint. Units gain RuntimeDirectory=piper.

Closes #<the issue filed for this plan>

🤖 Generated with [Claude Code](https://claude.com/claude-code)"
```

Squash-merge after review.
