# Remove a box from a relay account — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an account reclaim an agent slot by removing a box it no longer runs, so hitting the agent cap stops being a dead end.

**Architecture:** A `DELETE /agents/<base-domain>` on the relay's existing control proxy, reusing the authorization already written for the sibling GET, backed by a `Store.DeleteAgent` that clears only rows keyed on `agents(name)`. The CLI gains a `box` noun (`ls`, `rm`) over `internal/relayclient`. Removal is refused while the box holds a live tunnel session.

**Tech Stack:** Go, `modernc.org/sqlite` (pure Go, no cgo), `net/http`, `httptest`.

**Spec:** [`docs/superpowers/specs/2026-07-26-remove-a-box-design.md`](../specs/2026-07-26-remove-a-box-design.md) · **Issue:** [#401](https://github.com/piperbox/piper/issues/401)

## Global Constraints

- **No cgo.** Every build must pass `CGO_ENABLED=0`. Never add a cgo SQLite driver.
- **Module path** is `github.com/piperbox/piper`.
- **Layering — nothing imports "up".** `store` knows persistence only; it must never learn about the router or HTTP. Liveness checks belong in the handler.
- **Pre-1.x: break freely, no migrations.** This change adds no schema change at all — only `DELETE`s. Do not add columns.
- **Run `make verify` before claiming a task done.** It runs gofmt → `go vet` → `go test ./...` → `make cross`, which is the same gate CI applies.
- **Test-first.** Every step below writes the failing test, runs it to watch it fail, then implements.
- **Commit style:** conventional (`feat:`, `fix:`, `test:`), body references `Part of #401`, and ends with the trailer `Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>`.
- **Branch:** `ozykhan/remove-a-box` (already created; the spec commit is on it).

## File Structure

| File | Responsibility |
| --- | --- |
| `internal/relay/agents.go` (create) | Agent lifecycle beyond enrollment: `ErrUnknownAgent`, `DeleteAgent` |
| `internal/relay/agents_test.go` (create) | Cascade, unknown-agent, isolation, and hostnames-survive tests |
| `internal/relay/proxy.go` (modify) | `DELETE` branch in the `tail == ""` section |
| `internal/relay/proxy_test.go` (modify) | 409 / 404 / 204 / 405 handler tests |
| `internal/relayclient/relayclient.go` (modify) | `Agent` type, `Agents`, `RemoveAgent` |
| `internal/relayclient/relayclient_test.go` (modify) | Client decode + status mapping |
| `cmd/piper/box.go` (create) | `cmdBox`, `boxList`, `boxRemove` |
| `cmd/piper/box_test.go` (create) | CLI dispatch, listing output, `--yes` gate |
| `cmd/piper/main.go` (modify) | `case "box"` dispatch + usage line |
| `cmd/piper/relayonboard.go` (modify) | The quota-exceeded sentence |

Why a new `internal/relay/agents.go` rather than appending to `accounts.go`: enrollment lives in `accounts.go` and agent lookups are scattered through `hostnames.go`. Deletion is agent-lifecycle, not account or hostname logic, and a new focused file keeps it findable.

---

### Task 1: `Store.DeleteAgent`

**Files:**
- Create: `internal/relay/agents.go`
- Test: `internal/relay/agents_test.go`

**Interfaces:**
- Consumes: `openTestStore(t)` from `internal/relay/accounts_test.go`; `Store.UpsertAccount`, `Store.EnrollForAccount`, `Store.Configure` (all existing).
- Produces: `var ErrUnknownAgent = errors.New("unknown agent")` and `func (s *Store) DeleteAgent(baseDomain string) error`.

The two child tables reference `agents(name)`, not `base_domain`, so the name must be resolved first. `hostnames` keys on `account_id` and is deliberately left alone — see the spec.

- [ ] **Step 1: Write the failing tests**

Create `internal/relay/agents_test.go`:

```go
package relay

import (
	"errors"
	"testing"
	"time"
)

// bindAndPark gives an agent one repo_bindings row and one pending_events row,
// so a deletion has child rows to cascade through.
func bindAndPark(t *testing.T, st *Store, agentName string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := st.db.Exec(
		`INSERT INTO repo_bindings(agent_name, app, repo, branch, created_at) VALUES(?,?,?,?,?)`,
		agentName, "blog", "alice/blog", "main", now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(
		`INSERT INTO pending_events(agent_name, app, ref, event, payload, created_at, attempts, next_try_at)
		 VALUES(?,?,?,?,?,?,?,?)`,
		agentName, "blog", "main", "push", []byte("{}"), now, 1, now); err != nil {
		t.Fatal(err)
	}
}

func countRows(t *testing.T, st *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := st.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDeleteAgentClearsAgentAndItsChildRows(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	acc, err := st.UpsertAccount("sub-1", "alice")
	if err != nil {
		t.Fatal(err)
	}
	en, err := st.EnrollForAccount(acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	var name string
	if err := st.db.QueryRow(`SELECT name FROM agents WHERE base_domain=?`, en.BaseDomain).Scan(&name); err != nil {
		t.Fatal(err)
	}
	bindAndPark(t, st, name)

	if err := st.DeleteAgent(en.BaseDomain); err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}

	if n := countRows(t, st, `SELECT COUNT(*) FROM agents WHERE base_domain=?`, en.BaseDomain); n != 0 {
		t.Errorf("agents rows = %d, want 0", n)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM repo_bindings WHERE agent_name=?`, name); n != 0 {
		t.Errorf("repo_bindings rows = %d, want 0", n)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM pending_events WHERE agent_name=?`, name); n != 0 {
		t.Errorf("pending_events rows = %d, want 0", n)
	}
}

func TestDeleteAgentUnknownBaseDomain(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	if err := st.DeleteAgent("nope.public.getpiper.co"); !errors.Is(err, ErrUnknownAgent) {
		t.Fatalf("DeleteAgent(unknown) = %v, want ErrUnknownAgent", err)
	}
}

func TestDeleteAgentLeavesTheAccountsOtherAgentAlone(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	acc, err := st.UpsertAccount("sub-1", "alice")
	if err != nil {
		t.Fatal(err)
	}
	doomed, err := st.EnrollForAccount(acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	keeper, err := st.EnrollForAccount(acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	var keeperName string
	if err := st.db.QueryRow(`SELECT name FROM agents WHERE base_domain=?`, keeper.BaseDomain).Scan(&keeperName); err != nil {
		t.Fatal(err)
	}
	bindAndPark(t, st, keeperName)

	if err := st.DeleteAgent(doomed.BaseDomain); err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}

	if n := countRows(t, st, `SELECT COUNT(*) FROM agents WHERE base_domain=?`, keeper.BaseDomain); n != 1 {
		t.Errorf("keeper agent rows = %d, want 1", n)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM repo_bindings WHERE agent_name=?`, keeperName); n != 1 {
		t.Errorf("keeper repo_bindings = %d, want 1", n)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM pending_events WHERE agent_name=?`, keeperName); n != 1 {
		t.Errorf("keeper pending_events = %d, want 1", n)
	}
}

// hostnames key on account_id with no agent column, so a removal cannot know
// which rows were this box's. Deleting the account's hostnames would destroy
// the URLs of its *other* boxes, so they are deliberately left behind: removal
// frees an agent slot, never an app slot. Pinned here so a future change cannot
// quietly turn this into cross-box data loss.
func TestDeleteAgentLeavesHostnamesIntact(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	acc, err := st.UpsertAccount("sub-1", "alice")
	if err != nil {
		t.Fatal(err)
	}
	en, err := st.EnrollForAccount(acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterHostname(en.BaseDomain, "blog", 0); err != nil {
		t.Fatal(err)
	}

	if err := st.DeleteAgent(en.BaseDomain); err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}

	if n := countRows(t, st, `SELECT COUNT(*) FROM hostnames WHERE account_id=?`, acc.ID); n != 1 {
		t.Errorf("hostnames rows = %d, want 1 (removal must not reclaim app slots)", n)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/relay/ -run TestDeleteAgent`
Expected: build failure — `undefined: ErrUnknownAgent`, `st.DeleteAgent undefined`.

- [ ] **Step 3: Write the implementation**

Create `internal/relay/agents.go`:

```go
package relay

import (
	"database/sql"
	"errors"
)

// ErrUnknownAgent is returned when no agent holds the given base domain. The
// existing sentinels do not fit: ErrUnknownAccount names a missing account, and
// AgentAccount answers ErrBadToken for an unknown base domain — right on an
// authentication path, misleading on an explicit delete.
var ErrUnknownAgent = errors.New("unknown agent")

// DeleteAgent retires one box: its agents row plus the rows keyed on its name.
//
// hostnames is deliberately NOT touched. That table keys on account_id with no
// agent column (see schema.sql), so there is no way to tell which rows were
// this box's — the relay already depends on this being unknowable, which is why
// repushRelayApps re-pushes hostnames from the box instead. Deleting the
// account's hostnames would destroy its *other* boxes' URLs. The consequence is
// that removal frees an agent slot but not an app slot; that gap is tracked
// separately rather than guessed at here.
//
// The name is resolved first because repo_bindings and pending_events both
// reference agents(name), not base_domain.
func (s *Store) DeleteAgent(baseDomain string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var name string
	err = tx.QueryRow(`SELECT name FROM agents WHERE base_domain = ?`, baseDomain).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUnknownAgent
	}
	if err != nil {
		return err
	}
	for _, stmt := range []string{
		`DELETE FROM pending_events WHERE agent_name = ?`,
		`DELETE FROM repo_bindings WHERE agent_name = ?`,
		`DELETE FROM agents WHERE name = ?`,
	} {
		if _, err := tx.Exec(stmt, name); err != nil {
			return err
		}
	}
	return tx.Commit()
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/relay/ -run TestDeleteAgent -v`
Expected: all four PASS.

- [ ] **Step 5: Run the full gate**

Run: `make verify`
Expected: exit 0.

- [ ] **Step 6: Commit**

```bash
git add internal/relay/agents.go internal/relay/agents_test.go
git commit -m "feat: add Store.DeleteAgent to retire one box

Deletes the agents row plus the rows keyed on agents(name) —
repo_bindings and pending_events — in one transaction.

hostnames is left alone on purpose: it keys on account_id with no agent
column, so a removal cannot tell which rows were this box's, and deleting
the account's would destroy its other boxes' URLs. A test pins that.

Part of #401

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 2: `DELETE /agents/<base-domain>`

**Files:**
- Modify: `internal/relay/proxy.go` — the `if tail == ""` branch (currently GET-only, returns 405 otherwise)
- Test: `internal/relay/proxy_test.go`

**Interfaces:**
- Consumes: `Store.DeleteAgent` and `ErrUnknownAgent` from Task 1; existing `proxyFixture(t)`, `pipeSession(t, base)`, `router.Register`, `router.Lookup`.
- Produces: `DELETE /agents/<base-domain>` → 204 / 409 / 404 / 405.

The authorization (`AgentAccount` + `CanControl`, 404 on unknown *or* foreign) already runs above this branch and is reused untouched.

- [ ] **Step 1: Write the failing tests**

Append to `internal/relay/proxy_test.go`:

```go
// proxyDelete issues a DELETE with the given credential, mirroring proxyGet.
func proxyDelete(t *testing.T, api http.Handler, path, cred string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	if cred != "" {
		req.Header.Set("Authorization", "Bearer "+cred)
	}
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	return rr
}

func TestControlProxyRemoveAgent(t *testing.T) {
	api, st, _, aliceCred, _, base := proxyFixture(t)

	if rr := proxyDelete(t, api, "/agents/"+base, aliceCred); rr.Code != http.StatusNoContent {
		t.Fatalf("remove: %d, want 204 (body %q)", rr.Code, rr.Body.String())
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM agents WHERE base_domain=?`, base).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("agent row still present after 204")
	}
	// Now genuinely unknown: a second removal is a 404, like any other
	// unknown agent, with no hint that it ever existed.
	if rr := proxyDelete(t, api, "/agents/"+base, aliceCred); rr.Code != http.StatusNotFound {
		t.Fatalf("second remove: %d, want 404", rr.Code)
	}
}

// A box holding a live tunnel session is refused rather than evicted: removal
// is irreversible, so mistyping a base domain must not take down a running box.
func TestControlProxyRemoveConnectedAgentIsRefused(t *testing.T) {
	api, st, router, aliceCred, _, base := proxyFixture(t)
	relaySess, agentSess := pipeSession(t, base)
	router.Register(relaySess)
	go fakeBox(agentSess)

	rr := proxyDelete(t, api, "/agents/"+base, aliceCred)
	if rr.Code != http.StatusConflict {
		t.Fatalf("remove connected: %d, want 409", rr.Code)
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM agents WHERE base_domain=?`, base).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("connected agent was removed despite the 409")
	}
}

// Cross-tenant removal is indistinguishable from an unknown agent, and must not
// delete anything.
func TestControlProxyRemoveForeignAgentIs404(t *testing.T) {
	api, st, _, _, malloryCred, base := proxyFixture(t)

	if rr := proxyDelete(t, api, "/agents/"+base, malloryCred); rr.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant remove: %d, want 404", rr.Code)
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM agents WHERE base_domain=?`, base).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("another tenant removed alice's agent")
	}
}

// Adding DELETE must not widen the path to other verbs.
func TestControlProxyAgentPathStillRejectsOtherMethods(t *testing.T) {
	api, _, _, aliceCred, _, base := proxyFixture(t)
	req := httptest.NewRequest(http.MethodPut, "/agents/"+base, nil)
	req.Header.Set("Authorization", "Bearer "+aliceCred)
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT: %d, want 405", rr.Code)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/relay/ -run TestControlProxyRemove`
Expected: FAIL — `remove: 405, want 204`, since the branch is GET-only.

- [ ] **Step 3: Write the implementation**

In `internal/relay/proxy.go`, replace the `if tail == ""` block:

```go
		if tail == "" {
			switch r.Method {
			case http.MethodGet:
				// Liveness: answered by the relay itself from its in-memory
				// session map — never opens a tunnel stream. Offline is an
				// answer, not an error: 200 with connected:false.
				_, connected := router.Lookup(base)
				writeJSON(w, http.StatusOK, map[string]any{"agent": base, "connected": connected})
			case http.MethodDelete:
				// Refuse while the box is live. Removal is irreversible — the
				// enrollment token is gone and the box must re-enroll — so a
				// mistyped base domain must not be able to retire a running
				// box. The caller stops piperd on it and retries.
				if _, connected := router.Lookup(base); connected {
					http.Error(w, "box is connected; stop piperd on it first", http.StatusConflict)
					return
				}
				if err := st.DeleteAgent(base); err != nil {
					if errors.Is(err, ErrUnknownAgent) {
						http.NotFound(w, r)
						return
					}
					log.Printf("relay: remove agent %s: %v", base, err)
					http.Error(w, "remove failed", http.StatusInternalServerError)
					return
				}
				log.Printf("agent removed: %s", base)
				w.WriteHeader(http.StatusNoContent)
			default:
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}
```

No import changes are needed — `errors` and `log` are both already imported in `proxy.go`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/relay/ -run TestControlProxy -v`
Expected: the four new tests PASS and every pre-existing `TestControlProxy*` still passes.

- [ ] **Step 5: Run the full gate**

Run: `make verify`
Expected: exit 0.

- [ ] **Step 6: Commit**

```bash
git add internal/relay/proxy.go internal/relay/proxy_test.go
git commit -m "feat: DELETE /agents/<base-domain> on the relay control proxy

Reuses the authorization already written for the sibling GET, so unknown
and foreign agents both 404 and existence never leaks across tenants.

A box holding a live tunnel session is refused with 409 rather than
evicted: removal is irreversible, so a mistyped base domain must not
retire a running box.

Part of #401

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 3: `relayclient.Agents` and `relayclient.RemoveAgent`

**Files:**
- Modify: `internal/relayclient/relayclient.go`
- Test: `internal/relayclient/relayclient_test.go`

**Interfaces:**
- Consumes: the relay endpoints from Task 2 and the existing bare `GET /agents` listing.
- Produces:
  ```go
  type Agent struct {
      BaseDomain string `json:"agent"`
      Name       string `json:"name"`
      Owner      string `json:"owner"`
      Connected  bool   `json:"connected"`
  }
  var ErrAgentConnected = errors.New("box is connected; stop piperd on it first")
  var ErrNoAgent = errors.New("no such box")
  func (c *Client) Agents(ctx context.Context, accountCredential string) ([]Agent, error)
  func (c *Client) RemoveAgent(ctx context.Context, accountCredential, baseDomain string) error
  ```

The JSON tags match what the relay already emits: `{"agent","name","owner","connected"}`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/relayclient/relayclient_test.go`:

```go
func TestAgentsDecodesTheListing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agents" || r.Method != http.MethodGet {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer cred-1" {
			t.Errorf("auth = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"agents":[
			{"agent":"a1.example","name":"a1.example","owner":"alice","connected":true},
			{"agent":"a2.example","name":"a2.example","owner":"alice","connected":false}]}`)
	}))
	defer srv.Close()

	agents, err := New(srv.URL).Agents(context.Background(), "cred-1")
	if err != nil {
		t.Fatalf("Agents: %v", err)
	}
	if len(agents) != 2 {
		t.Fatalf("got %d agents, want 2", len(agents))
	}
	if agents[0].BaseDomain != "a1.example" || !agents[0].Connected {
		t.Errorf("agents[0] = %+v", agents[0])
	}
	if agents[1].Owner != "alice" || agents[1].Connected {
		t.Errorf("agents[1] = %+v", agents[1])
	}
}

func TestRemoveAgentStatusMapping(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		want   error
	}{
		{"removed", http.StatusNoContent, nil},
		{"connected", http.StatusConflict, ErrAgentConnected},
		{"unknown or foreign", http.StatusNotFound, ErrNoAgent},
		{"bad credential", http.StatusUnauthorized, ErrBadCredential},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodDelete || r.URL.Path != "/agents/box.example" {
					t.Errorf("got %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(c.status)
			}))
			defer srv.Close()

			err := New(srv.URL).RemoveAgent(context.Background(), "cred-1", "box.example")
			if c.want == nil {
				if err != nil {
					t.Fatalf("RemoveAgent = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("RemoveAgent = %v, want %v", err, c.want)
			}
		})
	}
}
```

Add `"io"` to the test file's imports if it is not already there.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/relayclient/ -run 'TestAgents|TestRemoveAgent'`
Expected: build failure — `undefined: ErrAgentConnected`, `ErrNoAgent`, `Agents`, `RemoveAgent`.

- [ ] **Step 3: Write the implementation**

Append to `internal/relayclient/relayclient.go`:

```go
// Agent is one box on the account, as the relay's /agents listing reports it.
// Connected is the relay's in-memory view of the tunnel session, so it is a
// live answer rather than a stored flag.
type Agent struct {
	BaseDomain string `json:"agent"`
	Name       string `json:"name"`
	Owner      string `json:"owner"`
	Connected  bool   `json:"connected"`
}

// ErrAgentConnected means the box still holds a live tunnel session. Removal is
// irreversible, so the relay refuses rather than evicting it.
var ErrAgentConnected = errors.New("box is connected; stop piperd on it first")

// ErrNoAgent means the relay has no such box for this account. Unknown and
// another tenant's box are indistinguishable by design.
var ErrNoAgent = errors.New("no such box")

// Agents lists the boxes the account may drive — its own plus any org's it
// belongs to.
func (c *Client) Agents(ctx context.Context, accountCredential string) ([]Agent, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/agents", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accountCredential)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrBadCredential
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("relay agents: %s", resp.Status)
	}
	var body struct {
		Agents []Agent `json:"agents"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.Agents, nil
}

// RemoveAgent retires baseDomain, freeing its agent slot. The relay refuses
// while the box is connected.
func (c *Client) RemoveAgent(ctx context.Context, accountCredential, baseDomain string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.base+"/agents/"+url.PathEscape(baseDomain), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accountCredential)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil
	case http.StatusConflict:
		return ErrAgentConnected
	case http.StatusNotFound:
		return ErrNoAgent
	case http.StatusUnauthorized:
		return ErrBadCredential
	default:
		return fmt.Errorf("relay remove box: %s", resp.Status)
	}
}
```

`url` is already imported (`GitHubRepos` uses `url.QueryEscape`).

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/relayclient/ -run 'TestAgents|TestRemoveAgent' -v`
Expected: all PASS, including the four `TestRemoveAgentStatusMapping` subtests.

- [ ] **Step 5: Run the full gate**

Run: `make verify`
Expected: exit 0.

- [ ] **Step 6: Commit**

```bash
git add internal/relayclient/relayclient.go internal/relayclient/relayclient_test.go
git commit -m "feat: relayclient Agents and RemoveAgent

Agents reads the bare /agents listing, which had no client method until
now. RemoveAgent maps 409 to ErrAgentConnected and 404 to ErrNoAgent so
the CLI can explain a refusal without parsing bodies.

Part of #401

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 4: `piper box ls`

**Files:**
- Create: `cmd/piper/box.go`
- Modify: `cmd/piper/main.go` — the subcommand switch (`case "github":` is at ~line 493) and the usage line (~line 728)
- Test: `cmd/piper/box_test.go`

**Interfaces:**
- Consumes: `relayclient.Agent`, `relayclient.Client.Agents` from Task 3; `config.LoadClient()` returning a struct with `RelayAPI` and `AccountCredential`.
- Produces: `func cmdBox(args []string, stdout, stderr io.Writer) int`, `const boxUsage`.

Note `cmdBox` takes no `remote` argument: unlike `app`/`domains`/`github` this talks to the relay with the account credential, not to a box. Follow `githubRepos` in `cmd/piper/relayonboard.go` for that shape.

- [ ] **Step 1: Write the failing test**

Create `cmd/piper/box_test.go`:

```go
package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestCmdBoxRejectsUnknownSubcommand(t *testing.T) {
	var out, errb bytes.Buffer
	if code := cmdBox([]string{"frobnicate"}, &out, &errb); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "usage: piper box") {
		t.Fatalf("stderr = %q, want the box usage line", errb.String())
	}
}

func TestCmdBoxNoArgsPrintsUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := cmdBox(nil, &out, &errb); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "usage: piper box") {
		t.Fatalf("stderr = %q, want the box usage line", errb.String())
	}
}

func TestBoxListRendersAgents(t *testing.T) {
	rendered := renderBoxes([]boxRow{
		{BaseDomain: "a1.example", Owner: "alice", Connected: true},
		{BaseDomain: "a2.example", Owner: "alice", Connected: false},
	})
	for _, want := range []string{"a1.example", "connected", "a2.example", "offline", "alice"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered listing missing %q:\n%s", want, rendered)
		}
	}
}

func TestBoxListSaysSoWhenEmpty(t *testing.T) {
	if got := renderBoxes(nil); !strings.Contains(got, "no boxes") {
		t.Fatalf("empty listing = %q, want it to say there are no boxes", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cmd/piper/ -run 'TestCmdBox|TestBoxList'`
Expected: build failure — `undefined: cmdBox`, `renderBoxes`, `boxRow`.

- [ ] **Step 3: Write the implementation**

Create `cmd/piper/box.go`:

```go
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/piperbox/piper/internal/config"
	"github.com/piperbox/piper/internal/relayclient"
)

const boxUsage = "usage: piper box ls | piper box rm <base-domain> [--yes]"

// boxRow is one line of `piper box ls`. It exists so rendering can be tested
// without a relay: renderBoxes is pure, and the command is the thin part.
type boxRow struct {
	BaseDomain string
	Owner      string
	Connected  bool
}

func cmdBox(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, boxUsage)
		return 2
	}
	switch args[0] {
	case "ls":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "usage: piper box ls")
			return 2
		}
		return boxList(stdout, stderr)
	default:
		fmt.Fprintln(stderr, boxUsage)
		return 2
	}
}

// renderBoxes formats the listing. Connectedness is the relay's live view, so
// "offline" here is exactly what makes a box removable.
func renderBoxes(rows []boxRow) string {
	if len(rows) == 0 {
		return "no boxes on this account — run `piper connect` on a box to claim one\n"
	}
	var b strings.Builder
	for _, r := range rows {
		state := "offline"
		if r.Connected {
			state = "connected"
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\n", r.BaseDomain, r.Owner, state)
	}
	return b.String()
}

// relayAccount loads the relay API base and account credential the box
// commands need, or explains that a login is missing.
func relayAccount(stderr io.Writer) (api, cred string, ok bool) {
	cc, err := config.LoadClient()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return "", "", false
	}
	if cc.RelayAPI == "" || cc.AccountCredential == "" {
		fmt.Fprintln(stderr, "error: not logged in to a relay; run `piper login` first")
		return "", "", false
	}
	return cc.RelayAPI, cc.AccountCredential, true
}

func boxList(stdout, stderr io.Writer) int {
	api, cred, ok := relayAccount(stderr)
	if !ok {
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	agents, err := relayclient.New(api).Agents(ctx, cred)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	rows := make([]boxRow, 0, len(agents))
	for _, a := range agents {
		rows = append(rows, boxRow{BaseDomain: a.BaseDomain, Owner: a.Owner, Connected: a.Connected})
	}
	fmt.Fprint(stdout, renderBoxes(rows))
	return 0
}
```

In `cmd/piper/main.go`, add to the subcommand switch, next to `case "github":`:

```go
	case "box":
		return cmdBox(args[1:], stdout, stderr)
```

and add `box` to the usage line so it reads:

```go
	fmt.Fprintln(w, "usage: piper [--remote <base-domain>] [--version] <version|login|connect|create|deploy|list|status|stop|start|delete|app|domains|github|box|agent> [args]")
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./cmd/piper/ -run 'TestCmdBox|TestBoxList' -v`
Expected: all four PASS.

- [ ] **Step 5: Run the full gate**

Run: `make verify`
Expected: exit 0.

- [ ] **Step 6: Commit**

```bash
git add cmd/piper/box.go cmd/piper/box_test.go cmd/piper/main.go
git commit -m "feat: piper box ls

There was no CLI way to see an account's boxes; finding dead enrollment
slots on the hosted relay needed a hand-written script against /agents.
Rendering is a pure function so the listing is testable without a relay.

Part of #401

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 5: `piper box rm`

**Files:**
- Modify: `cmd/piper/box.go`
- Test: `cmd/piper/box_test.go`

**Interfaces:**
- Consumes: `relayclient.Client.RemoveAgent`, `ErrAgentConnected`, `ErrNoAgent` from Task 3; `relayAccount`, `boxUsage` from Task 4; existing `confirmPrompt(stdout, question)` and the `stdinReader` seam in `cmd/piper/main.go`.
- Produces: `boxRemove(baseDomain string, yes bool, stdout, stderr io.Writer) int`.

Removal is irreversible — the enrollment token is gone and the box must re-enroll — so it confirms unless `--yes`, matching `piper github reset`.

- [ ] **Step 1: Write the failing tests**

Append to `cmd/piper/box_test.go`:

```go
func TestCmdBoxRmNeedsABaseDomain(t *testing.T) {
	var out, errb bytes.Buffer
	if code := cmdBox([]string{"rm"}, &out, &errb); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "usage: piper box rm") {
		t.Fatalf("stderr = %q, want the rm usage line", errb.String())
	}
}

// Declining the prompt must make no request at all — the check has to happen
// before the relay is dialed, or a "no" would still have removed the box.
func TestBoxRemoveAbortsOnDeclinedPrompt(t *testing.T) {
	oldStdin := stdinReader
	stdinReader = strings.NewReader("n\n")
	defer func() { stdinReader = oldStdin }()

	var out, errb bytes.Buffer
	if code := boxRemove("a1.example", false, &out, &errb); code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "aborted") {
		t.Fatalf("stdout = %q, want an abort notice", out.String())
	}
	if errb.Len() != 0 {
		t.Fatalf("stderr = %q, want empty (no relay call attempted)", errb.String())
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./cmd/piper/ -run 'TestCmdBoxRm|TestBoxRemove'`
Expected: build failure — `undefined: boxRemove`; and the `rm` case is not in `cmdBox` yet.

- [ ] **Step 3: Write the implementation**

Add the `rm` case to `cmdBox` in `cmd/piper/box.go`, between `case "ls":` and `default:`:

```go
	case "rm":
		fs := flag.NewFlagSet("rm", flag.ContinueOnError)
		fs.SetOutput(stderr)
		yes := fs.Bool("yes", false, "skip the confirmation prompt")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if fs.NArg() != 1 {
			fmt.Fprintln(stderr, "usage: piper box rm <base-domain> [--yes]")
			return 2
		}
		return boxRemove(fs.Arg(0), *yes, stdout, stderr)
```

Add `"errors"` and `"flag"` to the imports, and append:

```go
// boxRemove retires a box, freeing its agent slot. The confirmation comes
// before the relay is dialed: removal cannot be undone — the enrollment token
// is gone and the box must run `piper connect` again — so a declined prompt
// must not have sent anything.
//
// Removing a box does not free its app-cap slots. The relay's hostnames table
// keys on the account, not the agent, so it cannot tell which URLs were this
// box's; saying so here is better than a user inferring it from a still-full
// app quota.
func boxRemove(baseDomain string, yes bool, stdout, stderr io.Writer) int {
	if !yes && !confirmPrompt(stdout, "remove "+baseDomain+"? it must run `piper connect` again to come back") {
		fmt.Fprintln(stdout, "aborted")
		return 0
	}
	api, cred, ok := relayAccount(stderr)
	if !ok {
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	switch err := relayclient.New(api).RemoveAgent(ctx, cred, baseDomain); {
	case err == nil:
		fmt.Fprintf(stdout, "removed %s\n", baseDomain)
		fmt.Fprintln(stdout, "its app URLs stay reserved on the account; only the box slot is freed.")
		return 0
	case errors.Is(err, relayclient.ErrAgentConnected):
		fmt.Fprintf(stderr, "error: %s is still connected — stop piperd on that box, then retry\n", baseDomain)
		return 1
	case errors.Is(err, relayclient.ErrNoAgent):
		fmt.Fprintf(stderr, "error: no box %s on this account — run `piper box ls` to see them\n", baseDomain)
		return 1
	default:
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./cmd/piper/ -run 'TestCmdBox|TestBoxList|TestBoxRemove' -v`
Expected: all PASS.

- [ ] **Step 5: Run the full gate**

Run: `make verify`
Expected: exit 0.

- [ ] **Step 6: Commit**

```bash
git add cmd/piper/box.go cmd/piper/box_test.go
git commit -m "feat: piper box rm

Confirms before dialing the relay, so a declined prompt sends nothing.
Maps the relay's 409 to a message naming the fix — stop piperd on that
box — and says plainly that removal frees the box slot but not the
account's app slots.

Part of #401

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 6: Make the quota error actionable

**Files:**
- Modify: `cmd/piper/relayonboard.go:289`
- Test: `cmd/piper/relayonboard_test.go`

**Interfaces:**
- Consumes: `piper box ls` / `piper box rm` from Tasks 4–5.
- Produces: no new symbols; a changed user-facing string.

This is the sentence that started #401. The store and client sentinels (`internal/relay/accounts.go`, `internal/relayclient/relayclient.go`) both read "account agent quota exceeded" and are **not** changed — only the CLI's own line. The "or upgrade" clause goes: there is still no upgrade path, and naming a second remedy that does not exist is half of what made the original misleading.

- [ ] **Step 1: Strengthen the existing test**

`TestConnectQuotaExceeded` already exists at `cmd/piper/relayonboard_test.go:569` and asserts only that stderr contains "quota" — which the misleading message also satisfies. Do **not** add a second test; tighten this one so it pins the remedies. Replace its final assertion block:

```go
	if !bytes.Contains(errb.Bytes(), []byte("quota")) {
		t.Fatalf("stderr = %q, want a quota message", errb.String())
	}
```

with:

```go
	// The message must name only remedies that exist. It used to say "remove an
	// existing box or upgrade" when neither was possible (#401).
	got := errb.String()
	for _, want := range []string{"quota", "piper box ls", "piper box rm"} {
		if !strings.Contains(got, want) {
			t.Errorf("stderr = %q, want it to mention %q", got, want)
		}
	}
	if strings.Contains(got, "upgrade") {
		t.Errorf("stderr = %q, must not offer an upgrade path that does not exist", got)
	}
```

Add `"strings"` to the file's imports if it is not already there.

- [ ] **Step 1b: Run the test to verify it fails**

Run: `go test ./cmd/piper/ -run TestConnectQuotaExceeded -v`
Expected: FAIL on the missing `piper box ls` / `piper box rm` mentions and on the present "upgrade".

- [ ] **Step 2: Write the implementation**

In `cmd/piper/relayonboard.go`, replace line 289:

```go
	case errors.Is(err, relayclient.ErrQuotaExceeded):
		fmt.Fprintln(stderr, "error: account agent quota exceeded")
		fmt.Fprintln(stderr, "run `piper box ls` to see your boxes, then `piper box rm <base-domain>` to free a slot")
		return 1
```

- [ ] **Step 3: Run the test to verify it passes**

Run: `go test ./cmd/piper/ -run TestConnectQuotaExceeded -v`
Expected: PASS.

- [ ] **Step 4: Run the full gate**

Run: `make verify`
Expected: exit 0.

- [ ] **Step 5: Commit**

```bash
git add cmd/piper/relayonboard.go cmd/piper/relayonboard_test.go
git commit -m "fix: point the quota error at the box commands

The message told users to remove a box or upgrade when neither was
possible. Removal exists now; an upgrade path still does not, so that
clause goes rather than naming a second remedy that is not there.

Part of #401

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 7: Follow-up issue and PR

**Files:** none — tracker and PR only.

- [ ] **Step 1: File the follow-up issue**

The spec's accepted gap needs its own tracking. Use the `file-issue` skill, or `gh issue create` with title `[relay] hostnames key on the account, so a removed box's app slots are never reclaimed` and labels `bug`, `P3`, `size/M`, `relay`. The body should cover both consequences of `hostnames` having no agent column:

1. Removing a box frees an agent slot but not its app-cap slots, because `RegisterHostname` counts `WHERE account_id=? AND pr=0`.
2. Two boxes on one account deploying the same app name collide on `UNIQUE(account_id, app, pr)` and share a hostname row — a latent routing conflict independent of removal.

Note that the fix — an `agent_name` column on `hostnames` — is a new column on an existing table, so under the no-migrations rule it needs a fresh relay DB and a full fleet re-enrollment, and should be batched with any other schema change rather than taken alone.

- [ ] **Step 2: Push and open the PR**

```bash
git push -u origin ozykhan/remove-a-box
```

PR body should state: what the endpoint and commands do; that connected boxes are refused rather than evicted, and why; that removal frees an agent slot but **not** app slots, with a link to the follow-up issue; and that there is **no schema or wire change**, so the relay can be upgraded on its own — unlike v0.10.0, boxes need not move in lockstep. Close with `Closes #401`.

- [ ] **Step 3: Verify CI is green on the PR**

```bash
gh pr checks --watch
```

Expected: CI, e2e, and Code Quality all pass.

---

## Self-Review

**Spec coverage.** Store method → Task 1. `DELETE` endpoint with 409/404/204/405 → Task 2. `relayclient` methods → Task 3. `piper box ls` → Task 4. `piper box rm --yes` → Task 5. Quota error → Task 6. Follow-up issue for the hostname gap → Task 7. The spec's "hostnames survive" assertion is `TestDeleteAgentLeavesHostnamesIntact` in Task 1. Deployment notes carry into the Task 7 PR body. No spec section is unimplemented.

**Placeholder scan.** None. Every code step is literal. Two facts were checked against the tree rather than assumed: `proxy.go` already imports `errors` and `log`, and `TestConnectQuotaExceeded` already exists at `relayonboard_test.go:569` — so Task 6 tightens that test instead of adding a near-duplicate, which is what a first draft of this plan got wrong. The helpers Tasks 1–2 lean on (`openTestStore`, `proxyFixture`, `pipeSession`, `fakeBox`) and the `stdinReader` seam Task 5 uses were all confirmed present.

**Type consistency.** `boxRow{BaseDomain, Owner, Connected}` is used identically in Tasks 4 and 5. `relayclient.Agent` field names (`BaseDomain`, `Name`, `Owner`, `Connected`) match between Task 3's definition and Task 4's consumption. `ErrAgentConnected` / `ErrNoAgent` are defined in Task 3 and consumed in Task 5 under the same names. `relayAccount` and `boxUsage` are defined in Task 4 and reused in Task 5. `DeleteAgent` / `ErrUnknownAgent` are defined in Task 1 and consumed in Task 2.
