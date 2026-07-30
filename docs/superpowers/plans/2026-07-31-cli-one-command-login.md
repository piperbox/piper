# One-command login — Plan 3: the merged `piper login` pipeline

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `piper login` becomes the whole onboarding: identity → local-box probe → claim through piperd's enrollment socket → wait for the tunnel → advisory GitHub-App poll. `piper connect` is deleted. No printed sudo, no restart hint, idempotent on re-run.

**Architecture:** A unix-socket flavor of `internal/client` talks to the surface Plan 2 built (`internal/enrollapi` types). A new `cmd/piper/enrollflow.go` holds the claim stage: the probe matrix (live / stopped / absent / stale), the staleness cross-check against the account's relay box list, the enroll POST, and the post-apply poll with the staged exit-code discipline (#297 extended). `relayLogin`/`relayLoginWeb` call it between "logged in" and the advisory install poll. Everything is seam-driven (`piperdPath`, `config.SystemEnvDir`, socket-path vars, `pollSleep`) so the existing test style carries over.

**Tech Stack:** Go 1.26, stdlib `net`/`net/http`/`net/http/httptest`, `internal/enrollapi`, `internal/relayclient`.

This is **Plan 3 of 3** for [`docs/superpowers/specs/2026-07-31-one-command-login-design.md`](../specs/2026-07-31-one-command-login-design.md). It **requires Plans 1 and 2** merged.

## Global Constraints

- **No cgo**; `make verify` before every push, judged by exit status. **Module path** `github.com/piperbox/piper`.
- **TDD**, failing-test-first, one commit per task step group.
- **Layering:** `client` is the CLI's view of the daemon's HTTP surfaces; `cmd/piper` orchestrates `client` + `relayclient` + `config`. Nothing imports "up".
- **Staged exit codes (spec §4):** credential persisted ⇒ identity durable; hard claim failures after that exit 1 with "logged in, but this box is not connected"; once piperd's 200 persisted the enrollment, a pending tunnel is **advisory** (exit 0 + note) unless a recorded handshake rejection makes it definitive; the GitHub-App poll stays advisory (#297 — its four pinned tests must keep passing).
- **Deliberate test rewrites only:** `TestLANLoginPreservesRelayCreds` (#84), `TestLoginUsageMentionsSudo`, `TestDefaultRelayAPIIsLiveHostedRelay`, and `TestAgentInstalledProbesBinaryAndState` stay untouched. The `connect` suite is deleted/replaced as listed in Task 4.
- **Quota message pins carry over:** mention `piper box ls` / `piper box rm`, never "upgrade".
- **`--remote` stays rejected** for `login` and `agent` (and no longer needs a `connect` entry).
- **Before Task 1:** file the tracking issue with the `file-issue` skill — title `[cli] merge piper connect into piper login`, labels `cli`, `enhancement`, `P1`, `size/M` — and reference it as `Part of #<that issue>` in every commit.
- Commits end with:
  `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`

## File Structure

- Create `internal/client/local.go` (+ `local_test.go`) — `NewUnix`, `RelayStatus`, `EnrollRelay`, sentinel errors.
- Create `cmd/piper/enrollflow.go` (+ `enrollflow_test.go`) — socket probe, claim pipeline, apply-wait.
- Modify `cmd/piper/relayonboard.go` — `relayLogin`/`relayLoginWeb` gain the claim stage; delete `connect`, `connectOpts`, `restartHint`; keep `agentInstalled`.
- Modify `cmd/piper/main.go` — login flags `--org/--no-enroll/--re-enroll/--data-dir`; delete the `connect` case; shrink the `--remote` rejection list; update `usage`.
- Modify `cmd/piper/relayonboard_test.go`, `cmd/piper/login_test.go` — rewrites listed in Task 4.
- Modify `cmd/piper/box.go` — recovery strings point at `piper login --re-enroll`.
- Create `test/e2e/login_test.go` — merged-login e2e.
- Modify docs: `README.md`, `docs/getting-started.md`, `docs/runbooks/git-deploy-e2e.md`, `docs/superpowers/specs/2026-07-30-onboarding-packaging-design.md` (§6 golden-path line), `docs/superpowers/specs/2026-07-13-tui-phase6-wizards-design.md` (verb note), `PROGRESS.md`.

---

### Task 1: unix-socket client — `NewUnix`, `RelayStatus`, `EnrollRelay`

**Files:**
- Create: `internal/client/local.go`
- Create: `internal/client/local_test.go`

**Interfaces:**
- Consumes: `internal/client.Client{base, token, http, pollInterval}` and `do`; `internal/enrollapi` types + paths (Plan 2).
- Produces:
  - `func NewUnix(socketPath string) *Client` — same `Client`, transport pinned to the socket; `AgentVersion()` works unchanged over it (the probe's positive ID).
  - `func (c *Client) RelayStatus() (enrollapi.Status, error)` — 404 ⇒ `ErrRelayStatusUnsupported` (pre-merge piperd).
  - `func (c *Client) EnrollRelay(req enrollapi.EnrollRequest) (enrollapi.EnrollResponse, error)` mapping error bodies: `env-managed` ⇒ `ErrEnvManaged`; `already-enrolled` ⇒ `*AlreadyEnrolledError{BaseDomain}`; `busy` ⇒ `ErrEnrollBusy`; `bad-credential` ⇒ `ErrEnrollCredential`; `quota` ⇒ `ErrEnrollQuota`; anything else ⇒ error carrying the `Detail`.

- [ ] **Step 1: Write the failing test**

Create `internal/client/local_test.go`:

```go
package client

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/piperbox/piper/internal/enrollapi"
)

// serveUnix runs handler on a unix socket in a temp dir and returns its path.
func serveUnix(t *testing.T, handler http.Handler) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "piperd.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return path
}

func TestUnixClientStatusAndEnroll(t *testing.T) {
	var gotEnroll enrollapi.EnrollRequest
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/version", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"version": "test"})
	})
	mux.HandleFunc("GET "+enrollapi.PathStatus, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(enrollapi.Status{Enrolled: false, Tunnel: "off"})
	})
	mux.HandleFunc("POST "+enrollapi.PathEnroll, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotEnroll)
		_ = json.NewEncoder(w).Encode(enrollapi.EnrollResponse{
			BaseDomain: "ab12-erin.public.getpiper.co", RelayAddr: "relay:7000"})
	})
	c := NewUnix(serveUnix(t, mux))

	if v, err := c.AgentVersion(); err != nil || v != "test" {
		t.Fatalf("AgentVersion over unix = %q, %v", v, err)
	}
	st, err := c.RelayStatus()
	if err != nil || st.Tunnel != "off" {
		t.Fatalf("RelayStatus = %+v, %v", st, err)
	}
	resp, err := c.EnrollRelay(enrollapi.EnrollRequest{RelayAPI: "https://api.relay", AccountCredential: "cred-xyz"})
	if err != nil || resp.BaseDomain != "ab12-erin.public.getpiper.co" {
		t.Fatalf("EnrollRelay = %+v, %v", resp, err)
	}
	if gotEnroll.RelayAPI != "https://api.relay" || gotEnroll.AccountCredential != "cred-xyz" {
		t.Fatalf("wire body = %+v", gotEnroll)
	}
}

func TestEnrollRelayMapsErrorCodes(t *testing.T) {
	cases := []struct {
		status int
		body   enrollapi.ErrorResponse
		check  func(error) bool
	}{
		{409, enrollapi.ErrorResponse{Error: "env-managed"}, func(e error) bool { return errors.Is(e, ErrEnvManaged) }},
		{409, enrollapi.ErrorResponse{Error: "busy"}, func(e error) bool { return errors.Is(e, ErrEnrollBusy) }},
		{401, enrollapi.ErrorResponse{Error: "bad-credential"}, func(e error) bool { return errors.Is(e, ErrEnrollCredential) }},
		{429, enrollapi.ErrorResponse{Error: "quota"}, func(e error) bool { return errors.Is(e, ErrEnrollQuota) }},
		{409, enrollapi.ErrorResponse{Error: "already-enrolled", BaseDomain: "b.example"}, func(e error) bool {
			var ae *AlreadyEnrolledError
			return errors.As(e, &ae) && ae.BaseDomain == "b.example"
		}},
	}
	for _, tc := range cases {
		mux := http.NewServeMux()
		mux.HandleFunc("POST "+enrollapi.PathEnroll, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_ = json.NewEncoder(w).Encode(tc.body)
		})
		c := NewUnix(serveUnix(t, mux))
		_, err := c.EnrollRelay(enrollapi.EnrollRequest{RelayAPI: "a", AccountCredential: "c"})
		if err == nil || !tc.check(err) {
			t.Errorf("code %s: err = %v", tc.body.Error, err)
		}
	}
}

func TestRelayStatusUnsupportedOn404(t *testing.T) {
	c := NewUnix(serveUnix(t, http.NewServeMux())) // no routes at all
	if _, err := c.RelayStatus(); !errors.Is(err, ErrRelayStatusUnsupported) {
		t.Fatalf("err = %v, want ErrRelayStatusUnsupported", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/client/ -run 'TestUnixClient|TestEnrollRelay|TestRelayStatusUnsupported' -v`
Expected: compile FAIL — `NewUnix` undefined.

- [ ] **Step 3: Implement**

Create `internal/client/local.go`:

```go
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/piperbox/piper/internal/enrollapi"
)

// NewUnix returns a Client whose transport dials piperd's enrollment socket.
// The base host is a placeholder — unix sockets have no authority — and no
// bearer is attached: the socket's directory permissions are the trust
// boundary (one-command login design).
func NewUnix(socketPath string) *Client {
	t := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socketPath)
	}}
	return &Client{base: "http://piperd", http: &http.Client{Transport: t, Timeout: 10 * time.Second}, pollInterval: time.Second}
}

// ErrRelayStatusUnsupported means the daemon answered 404 for the relay-status
// route: a piperd from before the one-command login — stale process or old
// install (#375-style trap).
var ErrRelayStatusUnsupported = errors.New("piperd does not support relay status (too old?)")

// Sentinels for the enroll endpoint's machine error codes.
var (
	ErrEnvManaged       = errors.New("enrollment is env-managed on this box")
	ErrEnrollBusy       = errors.New("piperd is busy (deployment building or another enrollment in flight)")
	ErrEnrollCredential = errors.New("relay rejected the account credential")
	ErrEnrollQuota      = errors.New("account agent quota exceeded")
)

// AlreadyEnrolledError reports the box already holds an enrollment; BaseDomain
// is its current identity.
type AlreadyEnrolledError struct{ BaseDomain string }

func (e *AlreadyEnrolledError) Error() string {
	return "box already enrolled as " + e.BaseDomain
}

// RelayStatus reads the fixed, secrets-free relay state from the enrollment
// socket.
func (c *Client) RelayStatus() (enrollapi.Status, error) {
	resp, err := c.do(http.MethodGet, enrollapi.PathStatus, "", nil)
	if err != nil {
		return enrollapi.Status{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return enrollapi.Status{}, ErrRelayStatusUnsupported
	}
	if resp.StatusCode != http.StatusOK {
		return enrollapi.Status{}, responseError("relay status", resp)
	}
	var st enrollapi.Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return enrollapi.Status{}, err
	}
	return st, nil
}

// EnrollRelay asks the local piperd to claim this box on the relay and apply
// the enrollment. piperd does the relay round-trip itself; the credential is
// used once and never persisted on the box.
func (c *Client) EnrollRelay(req enrollapi.EnrollRequest) (enrollapi.EnrollResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return enrollapi.EnrollResponse{}, err
	}
	resp, err := c.do(http.MethodPost, enrollapi.PathEnroll, "application/json", bytes.NewReader(body))
	if err != nil {
		return enrollapi.EnrollResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		var out enrollapi.EnrollResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return enrollapi.EnrollResponse{}, err
		}
		return out, nil
	}
	var e enrollapi.ErrorResponse
	_ = json.NewDecoder(resp.Body).Decode(&e)
	switch e.Error {
	case "env-managed":
		return enrollapi.EnrollResponse{}, ErrEnvManaged
	case "already-enrolled":
		return enrollapi.EnrollResponse{}, &AlreadyEnrolledError{BaseDomain: e.BaseDomain}
	case "busy":
		return enrollapi.EnrollResponse{}, ErrEnrollBusy
	case "bad-credential":
		return enrollapi.EnrollResponse{}, ErrEnrollCredential
	case "quota":
		return enrollapi.EnrollResponse{}, ErrEnrollQuota
	default:
		if e.Detail != "" {
			return enrollapi.EnrollResponse{}, fmt.Errorf("enroll: %s: %s", resp.Status, e.Detail)
		}
		return enrollapi.EnrollResponse{}, fmt.Errorf("enroll: %s", resp.Status)
	}
}
```

- [ ] **Step 4: Run the tests** — `go test ./internal/client/ -v` — expected PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/client/local.go internal/client/local_test.go
git commit -m "feat(cli): unix-socket client for piperd's enrollment surface"
```

---

### Task 2: the claim pipeline — probe matrix + enroll + apply-wait

**Files:**
- Create: `cmd/piper/enrollflow.go`
- Create: `cmd/piper/enrollflow_test.go`

**Interfaces:**
- Consumes: Task 1's client; `config.EnrollSocketCandidates` + the `config.SystemRuntimeSocket`/`DarwinRootSocket` vars (Plan 2); existing seams `piperdPath`, `agentInstalled`, `agentGOOS`, `systemctlRun`, `pollSleep`; `relayclient.New(api).Agents(ctx, cred)`.
- Produces:
  - `type enrollFlowOpts struct{ relayAPI, dataDir, org string; noEnroll, reEnroll bool }`
  - `func enrollAfterLogin(ctx context.Context, o enrollFlowOpts, cred string, stdout, stderr io.Writer) int` — the whole claim stage; returns the process exit code per the staged discipline. Task 3 calls it from `relayLogin`/`relayLoginWeb`.
  - Test seams: `var enrollApplyTimeout = 60 * time.Second`, `var enrollPollInterval = time.Second`, `var enrollSocketDial` (probe one candidate).

- [ ] **Step 1: Write the failing tests**

Create `cmd/piper/enrollflow_test.go`:

```go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/piperbox/piper/internal/config"
	"github.com/piperbox/piper/internal/enrollapi"
)

// stubNoLocalPiperd makes every install-detection signal negative and returns
// a data dir that does not exist. Without this, a dev machine's real piperd on
// PATH (or a real /run/piper socket) would leak into the tests.
func stubNoLocalPiperd(t *testing.T) string {
	t.Helper()
	oldPath := piperdPath
	piperdPath = func() (string, error) { return "", errors.New("not installed") }
	t.Cleanup(func() { piperdPath = oldPath })
	oldEnv := config.SystemEnvDir
	config.SystemEnvDir = filepath.Join(t.TempDir(), "absent")
	t.Cleanup(func() { config.SystemEnvDir = oldEnv })
	oldRun, oldDarwin := config.SystemRuntimeSocket, config.DarwinRootSocket
	scratch := t.TempDir()
	config.SystemRuntimeSocket = filepath.Join(scratch, "absent-run.sock")
	config.DarwinRootSocket = filepath.Join(scratch, "absent-root.sock")
	t.Cleanup(func() { config.SystemRuntimeSocket, config.DarwinRootSocket = oldRun, oldDarwin })
	return filepath.Join(t.TempDir(), "absent-datadir")
}

// startFakeEnrollSocket serves handler at <dir>/piperd.sock and returns the
// data dir whose candidate list finds it.
func startFakeEnrollSocket(t *testing.T, mux http.Handler) string {
	t.Helper()
	dir := t.TempDir()
	ln, err := net.Listen("unix", filepath.Join(dir, "piperd.sock"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return dir
}

// fakePiperd is a scriptable enrollment-socket daemon.
type fakePiperd struct {
	status atomic.Value // enrollapi.Status
	enroll func(enrollapi.EnrollRequest) (int, any)
	got    atomic.Value // last enrollapi.EnrollRequest
}

func (f *fakePiperd) mux() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /v1/version", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"version": "test"})
	})
	m.HandleFunc("GET "+enrollapi.PathStatus, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(f.status.Load())
	})
	m.HandleFunc("POST "+enrollapi.PathEnroll, func(w http.ResponseWriter, r *http.Request) {
		var req enrollapi.EnrollRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.got.Store(req)
		code, body := f.enroll(req)
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	})
	return m
}

func fastPoll(t *testing.T) {
	t.Helper()
	oldT, oldI, oldS := enrollApplyTimeout, enrollPollInterval, pollSleep
	enrollApplyTimeout = 300 * time.Millisecond
	enrollPollInterval = 10 * time.Millisecond
	pollSleep = func(d time.Duration) { time.Sleep(d) }
	t.Cleanup(func() { enrollApplyTimeout, enrollPollInterval, pollSleep = oldT, oldI, oldS })
}

func TestEnrollFlowIdentityOnlyOffBox(t *testing.T) {
	dataDir := stubNoLocalPiperd(t)
	var out, errb bytes.Buffer
	code := enrollAfterLogin(context.Background(), enrollFlowOpts{relayAPI: "https://api.relay", dataDir: dataDir}, "cred", &out, &errb)
	if code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "identity only") {
		t.Fatalf("stdout = %q, want the explicit identity-only note", out.String())
	}
}

func TestEnrollFlowInstalledButStoppedExitsOne(t *testing.T) {
	dataDir := stubNoLocalPiperd(t)
	// Installed signal: the data dir exists (an enrolled-before box).
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := enrollAfterLogin(context.Background(), enrollFlowOpts{relayAPI: "https://api.relay", dataDir: dataDir}, "cred", &out, &errb)
	if code != 1 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(errb.String(), "piper agent up") || !strings.Contains(errb.String(), "NOT connected") {
		t.Fatalf("stderr = %q", errb.String())
	}
}

func TestEnrollFlowClaimsAndWaitsForConnect(t *testing.T) {
	stubNoLocalPiperd(t)
	fastPoll(t)
	f := &fakePiperd{}
	f.status.Store(enrollapi.Status{Enrolled: false, Tunnel: "off"})
	f.enroll = func(req enrollapi.EnrollRequest) (int, any) {
		f.status.Store(enrollapi.Status{Enrolled: true, BaseDomain: "ab12-erin.public.getpiper.co",
			RelayAddr: "relay:7000", Tunnel: "connected"})
		return http.StatusOK, enrollapi.EnrollResponse{BaseDomain: "ab12-erin.public.getpiper.co", RelayAddr: "relay:7000"}
	}
	dataDir := startFakeEnrollSocket(t, f.mux())
	var out, errb bytes.Buffer
	code := enrollAfterLogin(context.Background(),
		enrollFlowOpts{relayAPI: "https://api.relay", dataDir: dataDir, org: "acme"}, "cred-xyz", &out, &errb)
	if code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	got := f.got.Load().(enrollapi.EnrollRequest)
	if got.RelayAPI != "https://api.relay" || got.AccountCredential != "cred-xyz" || got.Org != "acme" {
		t.Fatalf("enroll body = %+v", got)
	}
	if !strings.Contains(out.String(), "ab12-erin.public.getpiper.co") || !strings.Contains(out.String(), "live") {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestEnrollFlowQuotaMessage(t *testing.T) {
	stubNoLocalPiperd(t)
	f := &fakePiperd{}
	f.status.Store(enrollapi.Status{Enrolled: false, Tunnel: "off"})
	f.enroll = func(enrollapi.EnrollRequest) (int, any) {
		return http.StatusTooManyRequests, enrollapi.ErrorResponse{Error: "quota"}
	}
	dataDir := startFakeEnrollSocket(t, f.mux())
	var out, errb bytes.Buffer
	code := enrollAfterLogin(context.Background(), enrollFlowOpts{relayAPI: "a", dataDir: dataDir}, "cred", &out, &errb)
	if code != 1 {
		t.Fatalf("code = %d", code)
	}
	msg := errb.String()
	for _, want := range []string{"quota", "piper box ls", "piper box rm"} {
		if !strings.Contains(msg, want) {
			t.Errorf("stderr missing %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "upgrade") {
		t.Errorf("quota message must not say upgrade: %s", msg)
	}
}

func TestEnrollFlowEnvManagedIsInformational(t *testing.T) {
	stubNoLocalPiperd(t)
	f := &fakePiperd{}
	f.status.Store(enrollapi.Status{Enrolled: true, EnvManaged: true,
		BaseDomain: "byo.example.com", Tunnel: "connected"})
	dataDir := startFakeEnrollSocket(t, f.mux())
	var out, errb bytes.Buffer
	code := enrollAfterLogin(context.Background(), enrollFlowOpts{relayAPI: "a", dataDir: dataDir}, "cred", &out, &errb)
	if code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "operator-managed") {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestEnrollFlowStaleEnrollmentSuggestsReEnroll(t *testing.T) {
	stubNoLocalPiperd(t)
	f := &fakePiperd{}
	f.status.Store(enrollapi.Status{Enrolled: true, BaseDomain: "gone.public.getpiper.co", Tunnel: "retrying"})
	dataDir := startFakeEnrollSocket(t, f.mux())
	// The account's relay box list does NOT contain this base domain.
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/agents" {
			_ = json.NewEncoder(w).Encode(map[string]any{"agents": []any{}})
			return
		}
		http.NotFound(w, r)
	}))
	defer relay.Close()
	var out, errb bytes.Buffer
	code := enrollAfterLogin(context.Background(), enrollFlowOpts{relayAPI: relay.URL, dataDir: dataDir}, "cred", &out, &errb)
	if code != 1 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(errb.String(), "--re-enroll") {
		t.Fatalf("stderr = %q, want a --re-enroll hint", errb.String())
	}
}

func TestEnrollFlowReEnrollSendsReplace(t *testing.T) {
	stubNoLocalPiperd(t)
	fastPoll(t)
	f := &fakePiperd{}
	f.status.Store(enrollapi.Status{Enrolled: true, BaseDomain: "old.public.getpiper.co", Tunnel: "connected"})
	f.enroll = func(req enrollapi.EnrollRequest) (int, any) {
		f.status.Store(enrollapi.Status{Enrolled: true, BaseDomain: "new.public.getpiper.co", Tunnel: "connected"})
		return http.StatusOK, enrollapi.EnrollResponse{BaseDomain: "new.public.getpiper.co", RelayAddr: "relay:7000"}
	}
	dataDir := startFakeEnrollSocket(t, f.mux())
	var out, errb bytes.Buffer
	code := enrollAfterLogin(context.Background(), enrollFlowOpts{relayAPI: "a", dataDir: dataDir, reEnroll: true}, "cred", &out, &errb)
	if code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	if got := f.got.Load().(enrollapi.EnrollRequest); !got.Replace {
		t.Fatalf("re-enroll must send replace=true, got %+v", got)
	}
}

func TestEnrollFlowAdvisoryWhenTunnelPending(t *testing.T) {
	stubNoLocalPiperd(t)
	fastPoll(t)
	f := &fakePiperd{}
	f.status.Store(enrollapi.Status{Enrolled: false, Tunnel: "off"})
	f.enroll = func(enrollapi.EnrollRequest) (int, any) {
		f.status.Store(enrollapi.Status{Enrolled: true, BaseDomain: "b.public.getpiper.co",
			Tunnel: "retrying", LastTunnelError: "dial tcp: connection refused"})
		return http.StatusOK, enrollapi.EnrollResponse{BaseDomain: "b.public.getpiper.co", RelayAddr: "relay:7000"}
	}
	dataDir := startFakeEnrollSocket(t, f.mux())
	var out, errb bytes.Buffer
	code := enrollAfterLogin(context.Background(), enrollFlowOpts{relayAPI: "a", dataDir: dataDir}, "cred", &out, &errb)
	if code != 0 {
		t.Fatalf("pending tunnel must stay advisory (enrollment is durable); code = %d, err = %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "retrying") && !strings.Contains(out.String(), "not connected yet") {
		t.Fatalf("stdout = %q, want the advisory note", out.String())
	}
}

func TestEnrollFlowRejectedTokenIsDefinitive(t *testing.T) {
	stubNoLocalPiperd(t)
	fastPoll(t)
	f := &fakePiperd{}
	f.status.Store(enrollapi.Status{Enrolled: false, Tunnel: "off"})
	f.enroll = func(enrollapi.EnrollRequest) (int, any) {
		f.status.Store(enrollapi.Status{Enrolled: true, BaseDomain: "b.public.getpiper.co",
			Tunnel: "retrying", LastTunnelError: "relay handshake: relay rejected b.public.getpiper.co: unknown or revoked enrollment"})
		return http.StatusOK, enrollapi.EnrollResponse{BaseDomain: "b.public.getpiper.co", RelayAddr: "relay:7000"}
	}
	dataDir := startFakeEnrollSocket(t, f.mux())
	var out, errb bytes.Buffer
	code := enrollAfterLogin(context.Background(), enrollFlowOpts{relayAPI: "a", dataDir: dataDir}, "cred", &out, &errb)
	if code != 1 {
		t.Fatalf("a recorded rejection is definitive; code = %d", code)
	}
	if !strings.Contains(errb.String(), "rejected") {
		t.Fatalf("stderr = %q", errb.String())
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./cmd/piper/ -run TestEnrollFlow -v`
Expected: compile FAIL — `enrollAfterLogin`, `enrollFlowOpts`, `enrollApplyTimeout` undefined.

- [ ] **Step 3: Implement**

Create `cmd/piper/enrollflow.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/piperbox/piper/internal/client"
	"github.com/piperbox/piper/internal/config"
	"github.com/piperbox/piper/internal/enrollapi"
	"github.com/piperbox/piper/internal/relayclient"
)

// enrollFlowOpts parameterizes the claim half of the merged `piper login`.
type enrollFlowOpts struct {
	relayAPI string
	dataDir  string
	org      string
	noEnroll bool
	reEnroll bool
}

// Seams so tests run instantly.
var (
	enrollApplyTimeout = 60 * time.Second
	enrollPollInterval = time.Second
)

// enrollSocketDial probes one candidate socket and positively identifies
// piperd behind it (GET /v1/version); a var so tests can stub the probe.
var enrollSocketDial = func(path string) (*client.Client, bool) {
	c := client.NewUnix(path)
	if _, err := c.AgentVersion(); err != nil {
		return nil, false
	}
	return c, true
}

// findEnrollSocket returns a client on the first live enrollment socket.
func findEnrollSocket(dataDir string) (*client.Client, bool) {
	for _, p := range config.EnrollSocketCandidates(dataDir) {
		if c, ok := enrollSocketDial(p); ok {
			return c, true
		}
	}
	return nil, false
}

// enrollAfterLogin is the claim stage of the merged `piper login` (one-command
// login design). The caller has already persisted the account credential, so
// identity is durable no matter what happens here; this stage's own discipline
// is staged too — a hard claim failure exits 1 saying so, and once piperd has
// persisted the enrollment a pending tunnel is advisory (#297 extended).
func enrollAfterLogin(ctx context.Context, o enrollFlowOpts, cred string, stdout, stderr io.Writer) int {
	if o.noEnroll {
		return 0
	}
	c, ok := findEnrollSocket(o.dataDir)
	if !ok {
		if agentInstalled(o.dataDir) {
			fmt.Fprintln(stderr, "logged in, but this box is NOT connected — piperd is installed but not running.")
			fmt.Fprintln(stderr, "start it with `piper agent up`, then run `piper login` again.")
			return 1
		}
		fmt.Fprintln(stdout, "identity only — no piperd on this machine; run `piper login` on a box to connect it.")
		return 0
	}
	st, err := c.RelayStatus()
	if errors.Is(err, client.ErrRelayStatusUnsupported) {
		fmt.Fprintln(stderr, "logged in, but this piperd predates one-command login (a stale process or old install).")
		fmt.Fprintln(stderr, "restart it (`piper agent down`, then `piper agent up`) and run `piper login` again.")
		return 1
	}
	if err != nil {
		fmt.Fprintln(stderr, "error: cannot read piperd's relay status:", err)
		return 1
	}
	if st.EnvManaged {
		fmt.Fprintln(stdout, "this box's enrollment is operator-managed via "+config.SystemEnvFile()+" — nothing to do.")
		return 0
	}
	if st.Enrolled && !o.reEnroll {
		// Staleness cross-check: enrolled locally but unknown to the account
		// (piper box rm, account/relay switch) would otherwise deadlock — the
		// claim would be skipped forever with no verb left to fix it.
		agents, err := relayclient.New(o.relayAPI).Agents(ctx, cred)
		if err == nil {
			known := false
			for _, a := range agents {
				if a.BaseDomain == st.BaseDomain {
					known = true
					break
				}
			}
			if !known {
				fmt.Fprintf(stderr, "this box is enrolled as %s, but that box is not on your account (removed with `piper box rm`, or a different account/relay).\n", st.BaseDomain)
				fmt.Fprintln(stderr, "run `piper login --re-enroll` to claim it fresh.")
				return 1
			}
		}
		fmt.Fprintf(stdout, "already enrolled as %s\n", st.BaseDomain)
		return waitConnected(o.dataDir, st.BaseDomain, stdout, stderr)
	}
	fmt.Fprintln(stdout, "claiming this box…")
	resp, err := c.EnrollRelay(enrollapi.EnrollRequest{
		RelayAPI: o.relayAPI, AccountCredential: cred, Org: o.org, Replace: o.reEnroll,
	})
	var already *client.AlreadyEnrolledError
	switch {
	case errors.Is(err, client.ErrEnvManaged):
		fmt.Fprintln(stdout, "this box's enrollment is operator-managed via "+config.SystemEnvFile()+" — nothing to do.")
		return 0
	case errors.As(err, &already):
		// Raced with another login; treat like the enrolled path.
		fmt.Fprintf(stdout, "already enrolled as %s\n", already.BaseDomain)
		return waitConnected(o.dataDir, already.BaseDomain, stdout, stderr)
	case errors.Is(err, client.ErrEnrollQuota):
		fmt.Fprintln(stderr, "error: account agent quota exceeded")
		fmt.Fprintln(stderr, "run `piper box ls` to see your boxes, then `piper box rm <base-domain>` to free a slot")
		return 1
	case errors.Is(err, client.ErrEnrollCredential):
		fmt.Fprintln(stderr, "error: relay rejected your account credential; run `piper login` again")
		return 1
	case errors.Is(err, client.ErrEnrollBusy):
		fmt.Fprintln(stderr, "error: piperd is busy (a deployment is building, or another enrollment is in flight); retry shortly")
		return 1
	case err != nil:
		// A transport drop here can mean the apply already tore the listener
		// down; the status poll below settles whether the claim persisted.
		fmt.Fprintf(stderr, "note: enrollment response lost (%v); checking whether it applied…\n", err)
		return waitConnected(o.dataDir, "", stdout, stderr)
	}
	fmt.Fprintf(stdout, "enrolled as %s\napplying…\n", resp.BaseDomain)
	return waitConnected(o.dataDir, resp.BaseDomain, stdout, stderr)
}

// waitConnected polls the enrollment socket (re-finding it: the re-exec
// replaces the listener) until the tunnel reports connected. Once piperd has
// persisted the enrollment a quiet deadline is ADVISORY — exit 0 with a note,
// the tunnel client retries in the background — while a recorded handshake
// rejection is definitive. A socket that never comes back is a hard failure
// with a per-platform diagnosis hint.
func waitConnected(dataDir, baseDomain string, stdout, stderr io.Writer) int {
	deadline := time.Now().Add(enrollApplyTimeout)
	sawStatus := false
	enrolled := false
	for time.Now().Before(deadline) {
		if c, ok := findEnrollSocket(dataDir); ok {
			if st, err := c.RelayStatus(); err == nil {
				sawStatus = true
				enrolled = enrolled || st.Enrolled
				if st.BaseDomain != "" {
					baseDomain = st.BaseDomain
				}
				if st.Tunnel == "connected" {
					fmt.Fprintf(stdout, "piperd connected — this box is live (apps at https://<app>.%s)\n", baseDomain)
					return 0
				}
				if strings.Contains(st.LastTunnelError, "rejected") {
					fmt.Fprintf(stderr, "error: the relay rejected this box's enrollment: %s\n", st.LastTunnelError)
					fmt.Fprintln(stderr, "run `piper login --re-enroll` to claim it fresh.")
					return 1
				}
			}
		}
		pollSleep(enrollPollInterval)
	}
	if !sawStatus || !enrolled {
		fmt.Fprintln(stderr, "error: piperd did not come back after applying the enrollment.")
		fmt.Fprintln(stderr, applyDiagnosisHint())
		return 1
	}
	fmt.Fprintf(stdout, "enrollment applied; the tunnel is still retrying in the background — check later with `piper box ls`.\n")
	return 0
}

// applyDiagnosisHint names where to look when the daemon vanished mid-apply.
func applyDiagnosisHint() string {
	if agentGOOS == "darwin" {
		return "check: brew services info piper — logs: $(brew --prefix)/var/log/piperd.err.log"
	}
	if out, err := systemctlRun("is-active", "piperd"); err == nil || strings.TrimSpace(out) != "" {
		return "piperd service state: " + strings.TrimSpace(out) + " — check: journalctl -u piperd -n 50"
	}
	return "check: systemctl status piperd; logs: journalctl -u piperd -n 50"
}
```

- [ ] **Step 4: Run the tests** — `go test ./cmd/piper/ -run TestEnrollFlow -v` — expected PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/piper/enrollflow.go cmd/piper/enrollflow_test.go
git commit -m "feat(cli): claim pipeline — probe matrix, socket enroll, staged apply-wait"
```

---

### Task 3: hook the pipeline into `relayLogin`/`relayLoginWeb`

**Files:**
- Modify: `cmd/piper/relayonboard.go`
- Modify: `cmd/piper/main.go` (login flag parsing only)
- Modify: `cmd/piper/relayonboard_test.go`, `cmd/piper/login_test.go` (updates only — deletions happen in Task 4)

**Interfaces:**
- Consumes: Task 2's `enrollAfterLogin`.
- Produces: `relayLogin(relayAPI string, o enrollFlowOpts, stdout, stderr io.Writer) int` and `relayLoginWeb(relayAPI string, o enrollFlowOpts, stdout, stderr io.Writer) int` — after the credential is saved and "logged in" printed, the claim stage runs; the advisory GitHub-App poll (`finishInstall`) runs **only when the claim stage returned 0**, keeping the spec's UX order (login → claim → apply → install prompt).

- [ ] **Step 1: Write the failing test**

Add to `cmd/piper/relayonboard_test.go` (reusing that file's existing relay-stub pattern for `/v1/login/device` + `/v1/login/poll`, and Task 2's helpers):

```go
func TestRelayLoginRunsClaimStage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	stubNoLocalPiperd(t)
	fastPoll(t)
	stubPollSleep(t) // this file's existing pollSleep stub helper, if present; otherwise pollSleep is already stubbed by fastPoll

	// Device-flow relay stub that immediately grants the login.
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/login/device":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"verification_uri": "https://github.com/login/device", "user_code": "ABCD-1234",
				"device_code": "dev-1", "interval": 0, "expires_in": 300})
		case "/v1/login/poll":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"account_credential": "cred-xyz", "username": "erin"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer relay.Close()

	f := &fakePiperd{}
	f.status.Store(enrollapi.Status{Enrolled: false, Tunnel: "off"})
	f.enroll = func(req enrollapi.EnrollRequest) (int, any) {
		f.status.Store(enrollapi.Status{Enrolled: true, BaseDomain: "ab12-erin.public.getpiper.co", Tunnel: "connected"})
		return http.StatusOK, enrollapi.EnrollResponse{BaseDomain: "ab12-erin.public.getpiper.co", RelayAddr: "relay:7000"}
	}
	dataDir := startFakeEnrollSocket(t, f.mux())

	var out, errb bytes.Buffer
	code := run([]string{"login", "--relay", relay.URL, "--data-dir", dataDir}, &out, &errb)
	if code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	got := f.got.Load().(enrollapi.EnrollRequest)
	if got.RelayAPI != relay.URL || got.AccountCredential != "cred-xyz" {
		t.Fatalf("claim did not carry the fresh credential: %+v", got)
	}
	for _, want := range []string{"logged in to relay as erin", "claiming this box", "ab12-erin.public.getpiper.co"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("stdout missing %q:\n%s", want, out.String())
		}
	}
}
```

(If `stubPollSleep` does not exist as a helper in that file, drop the line — `fastPoll` already stubs `pollSleep`.)

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cmd/piper/ -run TestRelayLoginRunsClaimStage -v`
Expected: FAIL — `run` doesn't accept `--data-dir` for login and no claim output appears.

- [ ] **Step 3: Implement**

**(a)** In `cmd/piper/main.go`, extend the login flag set (the `case "login":` block):

```go
		org := fs.String("org", "", "enroll this box for a GitHub org you own")
		noEnroll := fs.Bool("no-enroll", false, "stop after identity; do not claim this box")
		reEnroll := fs.Bool("re-enroll", false, "claim this box fresh even if already enrolled (after `piper box rm`, or switching accounts/relays)")
		dataDir := fs.String("data-dir", config.DefaultDataDir(), "piperd data directory (enrollment-socket probe)")
```

and pass them through:

```go
		o := enrollFlowOpts{relayAPI: *relay, dataDir: *dataDir, org: *org, noEnroll: *noEnroll, reEnroll: *reEnroll}
		if *token != "" {
			return login(*addr, *token, stdout, stderr) // LAN login: identity only, unchanged
		}
		if *web {
			return relayLoginWeb(*relay, o, stdout, stderr)
		}
		return relayLogin(*relay, o, stdout, stderr)
```

**(b)** In `cmd/piper/relayonboard.go`, change both login functions' signatures to take `o enrollFlowOpts`, and replace their success tails. In `relayLogin` the current tail

```go
		fmt.Fprintf(stdout, "logged in to relay as %s\n", acc.Username)
		finishInstall(ctx, rc, acc, stdout, stderr)
		return 0
```

becomes

```go
		fmt.Fprintf(stdout, "logged in to relay as %s\n", acc.Username)
		if code := enrollAfterLogin(ctx, o, acc.AccountCredential, stdout, stderr); code != 0 {
			return code // identity is durable; re-running login resumes at the claim
		}
		finishInstall(ctx, rc, acc, stdout, stderr)
		return 0
```

Apply the same change to `relayLoginWeb`'s success tail.

- [ ] **Step 4: Fix the compile fallout in existing tests**

The #297 suite and any other test calling `relayLogin(srv.URL, &out, &errb)` directly now needs `relayLogin(srv.URL, enrollFlowOpts{dataDir: stubNoLocalPiperd(t)}, &out, &errb)` — with `stubNoLocalPiperd` making the claim stage take the identity-only path, which is exactly what preserves their pinned semantics (login exit 0 despite install-poll timeout/interrupt). Update each call site mechanically; do not change any assertion.

- [ ] **Step 5: Run the package tests** — `go test ./cmd/piper/ -v` — expected PASS, including the four #297 tests and `TestLANLoginPreservesRelayCreds` unchanged.

- [ ] **Step 6: Commit**

```bash
git add cmd/piper/
git commit -m "feat(cli): piper login runs the claim stage after identity"
```

---

### Task 4: delete `piper connect`

**Files:**
- Modify: `cmd/piper/main.go` — remove the `case "connect":` block; shrink the `--remote` rejection list to `{"version", "login", "agent"}`; remove `connect` from `usage`'s command listing.
- Modify: `cmd/piper/relayonboard.go` — delete `connect`, `connectOpts`, `restartHint`; **keep** `agentInstalled` and `piperdPath` (the probe matrix uses them).
- Modify: `cmd/piper/relayonboard_test.go`.
- Modify: `cmd/piper/box.go` — recovery strings.

- [ ] **Step 1: Write the failing test**

Add to `cmd/piper/relayonboard_test.go`:

```go
func TestConnectVerbIsGone(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"connect"}, &out, &errb); code == 0 {
		t.Fatalf("piper connect must no longer exist; out=%s", out.String())
	}
	if !strings.Contains(errb.String(), "piper login") {
		t.Fatalf("unknown-command output should point at piper login: %q", errb.String())
	}
}
```

(Route the unknown-command path through `usage(stderr)` as it already does for other unknown verbs; add one line to `usage`'s output: `connect is gone — piper login claims the box too`.)

- [ ] **Step 2: Delete the verb and its dead code**

- `cmd/piper/main.go`: delete the whole `case "connect":` block; change the `--remote` rejection switch to `case "version", "login", "agent":`; update `usage`.
- `cmd/piper/relayonboard.go`: delete `connect`, `connectOpts`, `restartHint`. `agentInstalled` stays (with its doc comment updated to name the login probe as its caller).
- `cmd/piper/box.go`: in `boxRemove`, change the confirm prompt to
  `"remove "+baseDomain+"? it must run `piper login --re-enroll` again to come back"`
  and update the function's doc comment the same way; in `boxList`'s empty-state / `box.go`'s "claim one" hint string (`run \`piper connect\` on a box to claim one`), point at `piper login`.

- [ ] **Step 3: Delete/replace the orphaned tests**

In `cmd/piper/relayonboard_test.go` delete: `TestConnectSystemManagedGuidesEnvInstall`, `TestConnectEnrollsAndWritesRelayFile`, `TestConnectWritesTerminated`, `TestConnectRequiresLogin`, `TestConnectQuotaExceeded`, `TestConnectOffBoxFailsLoudly`, `TestRestartHintPerPlatform`. Their pinned behaviors live on in Task 2's suite (`TestEnrollFlowQuotaMessage`, `TestEnrollFlowIdentityOnlyOffBox`, `TestEnrollFlowInstalledButStoppedExitsOne`, happy-path relay.json now covered by Plan 2's `TestEnrollHappyPathPersistsThenApplies`). **Keep** `TestAgentInstalledProbesBinaryAndState`, `TestLoginUsageMentionsSudo`, `TestDefaultRelayAPIIsLiveHostedRelay`, the #297 suite, `TestLANLoginPreservesRelayCreds`.

Also update `cmd/piper/box_test.go` if it pins the old `piper connect` prompt string.

- [ ] **Step 4: Run the gate** — `make verify` — expected exit 0.

- [ ] **Step 5: Commit**

```bash
git add cmd/piper/
git commit -m "feat(cli)!: delete piper connect — piper login claims the box"
```

---

### Task 5: docs sweep

**Files:**
- Modify: `README.md`, `docs/getting-started.md`, `docs/runbooks/git-deploy-e2e.md`, `docs/superpowers/specs/2026-07-30-onboarding-packaging-design.md`, `docs/superpowers/specs/2026-07-13-tui-phase6-wizards-design.md`, `PROGRESS.md`

- [ ] **Step 1: Update each mention**

Grep first: `grep -rn "piper connect" README.md docs/ PROGRESS.md` (historic plans/specs under `docs/superpowers/plans/` and dated design docs other than the two below are archival — leave them).

- `README.md:29-30` — collapse the two-step quick start into:
  ```
  piper login                  # GitHub sign-in + claims this box on the public relay
  ```
- `docs/getting-started.md` — rewrite the relay-onboarding section (lines ~180–224): one command, what it does (identity → claim → piperd applies and reconnects itself), the `--org`/`--no-enroll`/`--re-enroll` flags, the operator env pin (`/etc/piper/piperd.env` locks enrollment), and the laptop identity-only behavior. Update line ~66 (brew note: `brew services restart piper` is no longer part of onboarding), ~234 (`box rm` recovery → `piper login --re-enroll`), ~243. Keep the strings `apt install piperd` and `brew services start piper` present — `TestPiperdDocumentation` pins them.
- `docs/runbooks/git-deploy-e2e.md` (lines ~216, 336, 442, 454) — replace `piper connect` steps with `piper login`.
- `docs/superpowers/specs/2026-07-30-onboarding-packaging-design.md` §6 (line ~176) — change the golden-path line to `One curl command (or the brew line) → piper login → deploy.` with a pointer to the 2026-07-31 one-command-login spec.
- `docs/superpowers/specs/2026-07-13-tui-phase6-wizards-design.md` — add one note at the interim-scope section (~line 53): the relay wizard targets the merged `piper login` (connect no longer exists).
- `PROGRESS.md` — one line under the CLI/agent sections linking the three issues.

- [ ] **Step 2: Run the doc-pinning tests** — `go test ./packaging/systemd/ -run TestPiperdDocumentation -v` — expected PASS.

- [ ] **Step 3: Commit**

```bash
git add README.md docs/ PROGRESS.md
git commit -m "docs: one-command login — piper connect removed from every flow"
```

---

### Task 6: merged-login e2e

**Files:**
- Create: `test/e2e/login_test.go`

**Interfaces:**
- Consumes: the e2e harness conventions in `test/e2e/relay_test.go` (RUN_E2E gating, binary build helpers, in-process relay with the fake verifier, piperd process management). **Read that file first and reuse its helpers by name** — this task extends the harness, it does not invent a new one.

- [ ] **Step 1: Write the e2e test**

`TestOneCommandLogin` (gated exactly like the other relay e2e tests — skipped unless `RUN_E2E=1` and Docker present):

1. Start the in-process relay the way `relay_test.go` does (API with fake verifier that grants a device-flow login immediately, tunnel listener, apex configured).
2. Start a real `piperd` (built binary) with `PIPER_DATA_DIR=<tmp>`, no `PIPER_RELAY_*` env — a dev-tier LAN boot whose enrollment socket lands at `<tmp>/piperd.sock`.
3. Run the real `piper` binary: `piper login --relay <relay-api-url> --data-dir <tmp>` with a scripted stdin/browser (the device flow needs no real browser against the fake verifier).
4. Assert: exit 0; stdout contains `logged in to relay as`, `claiming this box`, and a `*.{apex}` base domain; `<tmp>/relay.json` exists with `terminated: true`; `<tmp>/box-id` exists; the relay's agent shows connected (poll the relay store or `piper box ls` output within a deadline — the piperd process re-exec'd itself and reconnected).
5. Re-run the same `piper login`; assert exit 0, `already enrolled as` in stdout, and the relay still shows exactly **one** agent for the account (idempotency end-to-end).

- [ ] **Step 2: Run it**

Run: `RUN_E2E=1 go test ./test/e2e/ -run TestOneCommandLogin -v`
Expected: PASS on a machine with Docker; skips cleanly otherwise. (Memory note: stop any brew-service piperd first — it holds :8088/:2019 — ask before `brew services stop piper`.)

- [ ] **Step 3: Commit**

```bash
git add test/e2e/login_test.go
git commit -m "test(e2e): one-command login — enroll, re-exec, reconnect, idempotent re-run"
```

---

### Task 7: PR

- [ ] **Step 1:** `make verify` (exit 0), then `git push -u origin HEAD`.

- [ ] **Step 2: Open the PR**

```bash
gh pr create --base main --title "feat(cli)!: one-command login — piper login claims and connects the box" --body "CLI half of the one-command login design (docs/superpowers/specs/2026-07-31-one-command-login-design.md): piper login now runs identity → probe → claim (through piperd's enrollment socket) → apply-wait → advisory GitHub-App poll, with staged exit codes extending #297. piper connect is deleted; --org/--no-enroll/--re-enroll join login; box rm recovery points at piper login --re-enroll; docs updated across README/getting-started/runbooks and the two affected specs.

Closes #<the issue filed for this plan>

🤖 Generated with [Claude Code](https://claude.com/claude-code)"
```

Squash-merge after review.
