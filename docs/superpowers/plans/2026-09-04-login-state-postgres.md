# Login-Flow State in Postgres Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move the relay's four pieces of per-process login state (web-login states, CLI browser-login handles, GitHub device flows, login rate limiter) into the shared Postgres so any relay serves any login request, then make `piper-edge` round-robin `api.<apex>`.

**Architecture:** Four additive tables in `internal/relay/schema.sql`, each with a server-side `expires_at` predicate and an opportunistic sweep on insert. Store methods live in a new `internal/relay/login_state.go`. API handlers and `GitHubVerifier` change body only; every `/v1/login/*` wire shape is unchanged. The CLI callback records the account and the poll mints the credential, so no secret sits at rest. Device flows are stateless: the poll that arrives after `next_poll_at` makes one upstream call, and the poll that gets a terminal answer returns it and deletes the row.

**Tech Stack:** Go, Postgres via `pgx/v5/stdlib` (`database/sql`), `relaytest` for throwaway test databases (Docker or `PIPER_TEST_POSTGRES_URL`).

**Spec:** [`docs/superpowers/specs/2026-09-04-login-state-postgres-design.md`](../specs/2026-09-04-login-state-postgres-design.md). One deviation, applied in Task 9: `login_device_flows` has no `github_id`/`github_login`/`error` columns and no `ResolveDeviceFlow`/`FailDeviceFlow`, because the poll that learns the outcome returns it directly.

## Global Constraints

- `CGO_ENABLED=0` must build; no cgo dependencies.
- Layering: `store` knows only persistence; nothing imports "up". All of this work is inside `internal/relay` and `cmd/piper-relay`.
- Pre-1.x: no migrations, no compat shims. `schema.sql` is edited in place; it is `CREATE TABLE IF NOT EXISTS`, so adding tables is additive on a live database.
- Test-first: every task writes the failing test, runs it, then implements.
- Run tests with `go test ./internal/relay -run '<Name>' -count=1`. Postgres tests skip when neither Docker nor `PIPER_TEST_POSTGRES_URL` is available; a skip is not a pass — make sure they actually run on your machine.
- Commits: conventional-commit style, one per task, ending with `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`, referencing `#522`.
- `make verify` (gofmt → vet → test → cross) must exit 0 before the PR. Judge it by exit status, not by grepping output.
- Branch: `ozykhan/login-state-postgres` (already exists, spec committed on it). PR into `main`, body carries `Closes #522` and `Part of #524`.

---

## File map

| File | Role |
| --- | --- |
| `internal/relay/schema.sql` | add `login_web_states`, `login_cli_handles`, `login_device_flows`, `login_rate` |
| `internal/relay/login_state.go` (new) | `Store` methods for the four tables |
| `internal/relay/login_state_test.go` (new) | store-level tests |
| `internal/relay/api.go` | `loginWeb`/`loginCallback` use the store; drop `webStates`/`cliStates` |
| `internal/relay/weblogin_cli.go` | CLI handlers use the store; poll mints |
| `internal/relay/login_rate.go` | `loginLimiter` becomes a fixed window over `Store.LoginHit` |
| `internal/relay/verifier_github.go` | stateless device flow over the store |
| `internal/relay/verifier.go` / `verifier_test.go` | constructor signature change |
| `internal/relay/edge_state.go`, `edge.go` | `pickAPI` round-robin, comment removal |
| `cmd/piper-relay/main.go` | pass the store to `NewGitHubVerifier` |
| `internal/relay/{api,weblogin_cli,login_rate,verifier_github,proxyproto,edge}_test.go` | updated/added tests |
| `PROGRESS.md`, spec | tracking |

---

### Task 1: Schema + web-state store methods

**Files:**
- Modify: `internal/relay/schema.sql` (append after `agent_owners`)
- Create: `internal/relay/login_state.go`
- Create: `internal/relay/login_state_test.go`

**Interfaces:**
- Produces: `func (s *Store) PutWebState(state, redirectURI string, ttl time.Duration) error`, `func (s *Store) TakeWebState(state string) (redirectURI string, ok bool, err error)`.

- [ ] **Step 1: Write the failing tests**

Create `internal/relay/login_state_test.go`:

```go
package relay

import (
	"testing"
	"time"
)

func countRows(t *testing.T, st *Store, table string) int {
	t.Helper()
	var n int
	if err := st.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestWebStateTakeIsSingleUse(t *testing.T) {
	st := openTestStore(t)
	if err := st.PutWebState("s1", "https://dash.getpiper.co/auth", 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	ru, ok, err := st.TakeWebState("s1")
	if err != nil || !ok || ru != "https://dash.getpiper.co/auth" {
		t.Fatalf("first take = (%q, %v, %v)", ru, ok, err)
	}
	if _, ok, err := st.TakeWebState("s1"); err != nil || ok {
		t.Fatalf("second take ok=%v err=%v, want not found", ok, err)
	}
}

func TestWebStateExpiredIsInvisibleAndSwept(t *testing.T) {
	st := openTestStore(t)
	// A negative TTL is already expired the moment it lands.
	if err := st.PutWebState("stale", "https://dash.getpiper.co/x", -time.Second); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.TakeWebState("stale"); ok {
		t.Fatal("expired state was redeemable")
	}
	if err := st.PutWebState("fresh", "https://dash.getpiper.co/y", time.Minute); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, st, "login_web_states"); n != 1 {
		t.Fatalf("rows after sweep = %d, want 1 (only the fresh state)", n)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/relay -run 'TestWebState' -count=1`
Expected: build failure, `st.PutWebState undefined`.

- [ ] **Step 3: Add the four tables to schema.sql**

Append to `internal/relay/schema.sql`:

```sql

-- Login-flow state (#522). Each row is one in-flight login step that has to
-- survive the next request landing on a different relay. Expiry is a
-- server-side predicate (expires_at > now()) so relay clocks never disagree;
-- every insert first sweeps its table's expired rows, so there is no janitor.

-- login_web_states: a pending dashboard browser login. Redeemed once by the
-- callback; the browser's cookie must also carry the same state.
CREATE TABLE IF NOT EXISTS login_web_states (
    state        TEXT PRIMARY KEY,
    redirect_uri TEXT NOT NULL,
    expires_at   TIMESTAMPTZ NOT NULL
);

-- login_cli_handles: a brokered CLI browser login (#291). confirmed flips
-- when the user types the code; account_id is set by the callback and read
-- by the poll, which mints the credential and deletes the row. No credential
-- is ever stored here.
CREATE TABLE IF NOT EXISTS login_cli_handles (
    handle     TEXT PRIMARY KEY,
    user_code  TEXT NOT NULL,          -- normalized: upper-case, no dash
    confirmed  BOOLEAN NOT NULL DEFAULT FALSE,
    account_id TEXT REFERENCES accounts(id) ON DELETE CASCADE, -- NULL until the callback
    expires_at TIMESTAMPTZ NOT NULL
);

-- login_device_flows: a GitHub device-code login. The relay serving a poll
-- asks GitHub only once next_poll_at has passed; the poll that gets a
-- terminal answer returns it and deletes the row.
CREATE TABLE IF NOT EXISTS login_device_flows (
    handle        TEXT PRIMARY KEY,
    device_code   TEXT NOT NULL,
    interval_secs INTEGER NOT NULL,
    next_poll_at  TIMESTAMPTZ NOT NULL,
    expires_at    TIMESTAMPTZ NOT NULL
);

-- login_rate: fixed-window login rate limit per client key (#106).
CREATE TABLE IF NOT EXISTS login_rate (
    key          TEXT PRIMARY KEY,
    window_start TIMESTAMPTZ NOT NULL,
    hits         INTEGER NOT NULL
);
```

- [ ] **Step 4: Create login_state.go with the web-state methods**

```go
package relay

import (
	"database/sql"
	"errors"
	"time"
)

// Login-flow state (#522). These methods back the steps of a login that span
// two HTTP requests, so the second request may land on any relay. Every put
// sweeps its table's expired rows first — the same opportunistic pattern the
// in-memory maps used — and every read or take is guarded by expires_at >
// now(), evaluated on the server so relay clocks never disagree.

// PutWebState records a pending dashboard browser login.
func (s *Store) PutWebState(state, redirectURI string, ttl time.Duration) error {
	if _, err := s.db.Exec(`DELETE FROM login_web_states WHERE expires_at <= now()`); err != nil {
		return err
	}
	_, err := s.db.Exec(
		`INSERT INTO login_web_states(state, redirect_uri, expires_at)
		 VALUES($1, $2, now() + make_interval(secs => $3))`,
		state, redirectURI, ttl.Seconds())
	return err
}

// TakeWebState redeems a web state exactly once: the row is deleted as it is
// read, so two relays racing the same callback cannot both succeed.
func (s *Store) TakeWebState(state string) (string, bool, error) {
	var ru string
	err := s.db.QueryRow(
		`DELETE FROM login_web_states WHERE state = $1 AND expires_at > now() RETURNING redirect_uri`,
		state).Scan(&ru)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return ru, true, nil
}
```

- [ ] **Step 5: Run to verify they pass**

Run: `go test ./internal/relay -run 'TestWebState' -count=1 -v`
Expected: both PASS (not SKIP).

- [ ] **Step 6: Commit**

```bash
git add internal/relay/schema.sql internal/relay/login_state.go internal/relay/login_state_test.go
git commit -m "feat(relay): login-flow tables + web-state store methods

Part of #522.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 2: Web login handlers read the store

**Files:**
- Modify: `internal/relay/api.go` (struct at ~lines 76-90, `newAPI` at ~40-43, `loginWeb` ~120-150, `loginCallback` ~156-177)
- Modify: `internal/relay/api_test.go` (`TestWebLoginSweepsExpiredStates` ~449-470; add a cross-relay test)
- Modify: `internal/relay/login_rate_test.go` (three `&api{... webStates: ...}` literals at ~92, ~145, ~167, ~187 — drop the `webStates:` field so the package compiles; those tests are rewritten in Task 5)

**Interfaces:**
- Consumes: `PutWebState`, `TakeWebState` from Task 1.

- [ ] **Step 1: Write the failing cross-relay test**

Append to `internal/relay/api_test.go`:

```go
// Two relays over one store: the browser starts on one process and GitHub's
// callback lands on the other. The state must be found there (#522).
func TestWebLoginCompletesOnAnotherRelay(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	fv := NewFakeVerifier()
	redirects := []string{"https://dash.getpiper.co/"}
	relayA := NewAPIWithTunnel(st, fv, "", nil, redirects, nil, nil)
	relayB := NewAPIWithTunnel(st, fv, "", nil, redirects, nil, nil)

	state, cookie := startWebLogin(t, relayA, "https://dash.getpiper.co/auth")
	fv.GrantCode("code-1", Identity{Subject: "583231", Login: "ivan"})

	req := httptest.NewRequest(http.MethodGet,
		"/v1/login/callback?code=code-1&state="+url.QueryEscape(state), nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	relayB.ServeHTTP(rr, req)
	if rr.Code != http.StatusFound {
		t.Fatalf("callback on relay B = %d, body %s", rr.Code, rr.Body.String())
	}
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "#credential=") {
		t.Fatalf("callback Location = %q, want credential fragment", loc)
	}
}
```

Also rewrite `TestWebLoginSweepsExpiredStates` (it reaches into the map today):

```go
func TestWebLoginSweepsExpiredStates(t *testing.T) {
	api, _, st := newWebTestAPI(t)
	if err := st.PutWebState("stale", "https://dash.getpiper.co/x", -time.Minute); err != nil {
		t.Fatal(err)
	}
	startWebLogin(t, api, "https://dash.getpiper.co/auth")
	if n := countRows(t, st, "login_web_states"); n != 1 {
		t.Fatalf("login_web_states rows = %d, want 1 (only the fresh state)", n)
	}
}
```

`newWebTestAPI` returns only the handler today; change it to return the store as a third value:

```go
func newWebTestAPI(t *testing.T) (http.Handler, *FakeVerifier, *Store) {
	t.Helper()
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	fv := NewFakeVerifier()
	api := NewAPIWithTunnel(st, fv, "", nil, []string{"https://dash.getpiper.co/"}, nil, nil)
	return api, fv, st
}
```

and update every `api, fv := newWebTestAPI(t)` / `api, _ := newWebTestAPI(t)` call in `api_test.go` and `login_rate_test.go` to take three values (`api, fv, _ :=` / `api, _, _ :=`). Then `TestWebLoginSweepsExpiredStates` reads `api, _, st := newWebTestAPI(t)`.

In `login_rate_test.go`, delete `webStates: map[string]webState{}` from the four `&api{...}` literals so the package compiles once the field is gone.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/relay -run 'TestWebLogin' -count=1`
Expected: `TestWebLoginCompletesOnAnotherRelay` FAILS with `callback on relay B = 400` (relay B has no memory of A's state). The sweep test fails on `countRows` (2 rows: the map, not the table, was swept) or compile error until step 3.

- [ ] **Step 3: Implement**

In `internal/relay/api.go`:

1. Remove `webStates` from the struct and from `newAPI`'s literal. Remove the `webState` type. Keep `mu` and `lastReconcile`.
2. `loginWeb`: replace the sweep + map write with

```go
	state := hex.EncodeToString(raw)
	if err := a.st.PutWebState(state, ru, 10*time.Minute); err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
```

3. `loginCallback`: replace the map take with

```go
	ru, ok, err := a.st.TakeWebState(state)
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "bad state", http.StatusBadRequest)
		return
	}
```

and use `ru` where `ws.redirectURI` was used in the final redirect. Remove the `now := time.Now()` in `loginWeb` if nothing else uses it.

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/relay -run 'TestWebLogin|TestLoginCallback' -count=1`
Expected: all PASS, including the pre-existing single-use, cookie-mismatch, exchange-failure, and disallowed-redirect tests.

- [ ] **Step 5: Commit**

```bash
git add internal/relay/api.go internal/relay/api_test.go internal/relay/login_rate_test.go
git commit -m "feat(relay): web-login state lives in Postgres

Part of #522.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 3: CLI handle store methods

**Files:**
- Modify: `internal/relay/login_state.go`
- Modify: `internal/relay/login_state_test.go`

**Interfaces:**
- Produces:
  - `type CLIHandle struct { Handle string; Confirmed bool; AccountID string }`
  - `type cliHandleState int` with `cliHandleUnknown`, `cliHandlePending`, `cliHandleDone`
  - `func (s *Store) PutCLIHandle(handle, userCode string, ttl time.Duration) error`
  - `func (s *Store) ConfirmCLIHandle(enteredCode string) (handle string, ok bool, err error)`
  - `func (s *Store) CLIHandle(handle string) (CLIHandle, bool, error)`
  - `func (s *Store) FinishCLIHandle(handle, accountID string) error` (returns `errCLIHandleGone` when nothing was updated)
  - `func (s *Store) TakeFinishedCLIHandle(handle string) (accountID, username string, state cliHandleState, err error)`

- [ ] **Step 1: Write the failing tests**

Append to `internal/relay/login_state_test.go`:

```go
func TestCLIHandleStateMachine(t *testing.T) {
	st := openTestStore(t)
	acc, err := st.UpsertAccount("42", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutCLIHandle("h1", "ABCD-1234", 10*time.Minute); err != nil {
		t.Fatal(err)
	}

	// Pending until confirmed and finished.
	if _, _, state, err := st.TakeFinishedCLIHandle("h1"); err != nil || state != cliHandlePending {
		t.Fatalf("take before confirm = state %v err %v, want pending", state, err)
	}
	// Finishing an unconfirmed handle is refused.
	if err := st.FinishCLIHandle("h1", acc.ID); err == nil {
		t.Fatal("FinishCLIHandle succeeded on an unconfirmed handle")
	}

	// Code entry is forgiving (case, dashes) and confirms exactly once.
	h, ok, err := st.ConfirmCLIHandle("abcd1234")
	if err != nil || !ok || h != "h1" {
		t.Fatalf("confirm = (%q, %v, %v)", h, ok, err)
	}
	if _, ok, _ := st.ConfirmCLIHandle("ABCD-1234"); ok {
		t.Fatal("second confirm of the same code succeeded")
	}
	row, ok, err := st.CLIHandle("h1")
	if err != nil || !ok || !row.Confirmed || row.AccountID != "" {
		t.Fatalf("CLIHandle after confirm = %+v ok=%v err=%v", row, ok, err)
	}

	if err := st.FinishCLIHandle("h1", acc.ID); err != nil {
		t.Fatalf("FinishCLIHandle: %v", err)
	}
	id, username, state, err := st.TakeFinishedCLIHandle("h1")
	if err != nil || state != cliHandleDone || id != acc.ID || username != "alice" {
		t.Fatalf("take after finish = (%q, %q, %v, %v)", id, username, state, err)
	}
	// Single use.
	if _, _, state, _ := st.TakeFinishedCLIHandle("h1"); state != cliHandleUnknown {
		t.Fatalf("second take state = %v, want unknown", state)
	}
}

func TestCLIHandleWrongOrEmptyCodeDoesNotConfirm(t *testing.T) {
	st := openTestStore(t)
	if err := st.PutCLIHandle("h1", "ABCD-1234", time.Minute); err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"", "WXYZ-9999", "ABCD-123"} {
		if _, ok, err := st.ConfirmCLIHandle(code); err != nil || ok {
			t.Fatalf("ConfirmCLIHandle(%q) = ok %v err %v, want no match", code, ok, err)
		}
	}
}

func TestCLIHandleExpiredIsInvisibleAndSwept(t *testing.T) {
	st := openTestStore(t)
	if err := st.PutCLIHandle("stale", "ABCD-1234", -time.Second); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.ConfirmCLIHandle("ABCD-1234"); ok {
		t.Fatal("expired handle confirmed")
	}
	if _, ok, _ := st.CLIHandle("stale"); ok {
		t.Fatal("expired handle readable")
	}
	if _, _, state, _ := st.TakeFinishedCLIHandle("stale"); state != cliHandleUnknown {
		t.Fatalf("expired take state = %v, want unknown", state)
	}
	if err := st.PutCLIHandle("fresh", "EFGH-5678", time.Minute); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, st, "login_cli_handles"); n != 1 {
		t.Fatalf("rows after sweep = %d, want 1", n)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/relay -run 'TestCLIHandle' -count=1`
Expected: build failure, `st.PutCLIHandle undefined`.

- [ ] **Step 3: Implement**

Append to `internal/relay/login_state.go` (add `"crypto/subtle"` to the imports):

```go
// CLIHandle is one brokered CLI browser login as the callback sees it.
type CLIHandle struct {
	Handle    string
	Confirmed bool
	AccountID string // "" until the callback finished it
}

// cliHandleState is what the CLI's poll learns about a handle.
type cliHandleState int

const (
	cliHandleUnknown cliHandleState = iota // never existed, expired, or already taken
	cliHandlePending                       // waiting for the browser
	cliHandleDone                          // finished; the caller now holds the account
)

var errCLIHandleGone = errors.New("cli login handle gone or unconfirmed")

// PutCLIHandle records a new brokered CLI login. The user code is stored
// normalized so ConfirmCLIHandle compares like with like.
func (s *Store) PutCLIHandle(handle, userCode string, ttl time.Duration) error {
	if _, err := s.db.Exec(`DELETE FROM login_cli_handles WHERE expires_at <= now()`); err != nil {
		return err
	}
	_, err := s.db.Exec(
		`INSERT INTO login_cli_handles(handle, user_code, expires_at)
		 VALUES($1, $2, now() + make_interval(secs => $3))`,
		handle, normalizeCode(userCode), ttl.Seconds())
	return err
}

// ConfirmCLIHandle matches an entered code against the unconfirmed, unexpired
// handles and claims the match. The comparison is constant-time in Go, as
// before; the claim is a guarded UPDATE so two relays cannot both win.
func (s *Store) ConfirmCLIHandle(enteredCode string) (string, bool, error) {
	entered := normalizeCode(enteredCode)
	if entered == "" {
		return "", false, nil
	}
	rows, err := s.db.Query(`SELECT handle, user_code FROM login_cli_handles WHERE NOT confirmed AND expires_at > now()`)
	if err != nil {
		return "", false, err
	}
	defer rows.Close()
	match := ""
	for rows.Next() {
		var h, code string
		if err := rows.Scan(&h, &code); err != nil {
			return "", false, err
		}
		if match == "" && subtle.ConstantTimeCompare([]byte(code), []byte(entered)) == 1 {
			match = h
		}
	}
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	if match == "" {
		return "", false, nil
	}
	res, err := s.db.Exec(
		`UPDATE login_cli_handles SET confirmed = TRUE WHERE handle = $1 AND NOT confirmed AND expires_at > now()`, match)
	if err != nil {
		return "", false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", false, nil
	}
	return match, true, nil
}

// CLIHandle reads one unexpired handle.
func (s *Store) CLIHandle(handle string) (CLIHandle, bool, error) {
	var h CLIHandle
	var acc sql.NullString
	err := s.db.QueryRow(
		`SELECT handle, confirmed, account_id FROM login_cli_handles WHERE handle = $1 AND expires_at > now()`,
		handle).Scan(&h.Handle, &h.Confirmed, &acc)
	if errors.Is(err, sql.ErrNoRows) {
		return CLIHandle{}, false, nil
	}
	if err != nil {
		return CLIHandle{}, false, err
	}
	h.AccountID = acc.String
	return h, true, nil
}

// FinishCLIHandle records the account a confirmed handle logged in as. The
// credential is minted later, by whichever relay serves the poll.
func (s *Store) FinishCLIHandle(handle, accountID string) error {
	res, err := s.db.Exec(
		`UPDATE login_cli_handles SET account_id = $2 WHERE handle = $1 AND confirmed AND expires_at > now()`,
		handle, accountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errCLIHandleGone
	}
	return nil
}

// TakeFinishedCLIHandle is the poll's read. A finished handle is deleted as
// it is read (single use) and its account returned; otherwise it reports
// pending or unknown.
func (s *Store) TakeFinishedCLIHandle(handle string) (string, string, cliHandleState, error) {
	var id, username string
	err := s.db.QueryRow(
		`DELETE FROM login_cli_handles h USING accounts a
		  WHERE h.handle = $1 AND h.account_id = a.id AND h.expires_at > now()
		  RETURNING a.id, a.username`, handle).Scan(&id, &username)
	if err == nil {
		return id, username, cliHandleDone, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", "", cliHandleUnknown, err
	}
	var pending bool
	if err := s.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM login_cli_handles WHERE handle = $1 AND expires_at > now())`,
		handle).Scan(&pending); err != nil {
		return "", "", cliHandleUnknown, err
	}
	if pending {
		return "", "", cliHandlePending, nil
	}
	return "", "", cliHandleUnknown, nil
}
```

`normalizeCode` already exists in `weblogin_cli.go` (same package).

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/relay -run 'TestCLIHandle' -count=1 -v`
Expected: three PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/relay/login_state.go internal/relay/login_state_test.go
git commit -m "feat(relay): CLI login handle store methods

Part of #522.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 4: CLI login handlers read the store; poll mints

**Files:**
- Modify: `internal/relay/weblogin_cli.go` (`cliLoginStart` ~58-82, `cliLoginConfirm` ~110-152, `cliCallback` ~158-218, `cliLoginPoll` ~223-259; delete `cliLogin` type ~25-34)
- Modify: `internal/relay/api.go` (drop `cliStates` from struct and `newAPI`)
- Modify: `internal/relay/weblogin_cli_test.go` (add cross-relay test)

**Interfaces:**
- Consumes: Task 3's methods.

- [ ] **Step 1: Write the failing cross-relay test**

Append to `internal/relay/weblogin_cli_test.go`:

```go
// The brokered CLI login spans four requests from two clients (CLI and
// browser); behind an edge they land on arbitrary relays. Start on A, poll on
// B, confirm on B, callback on A, collect on B — and the credential is minted
// exactly once, by the poll, so nothing secret ever sat in the handle (#522).
func TestCLILoginSpansTwoRelays(t *testing.T) {
	relayA, fv, st := newCLILoginAPI(t, "piper-app")
	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s", Slug: "piper-app",
	})
	if err != nil {
		t.Fatal(err)
	}
	relayB := NewAPIWithTunnel(st, fv, "", nil, nil, app, nil)

	rr := apiReq(t, relayA, "POST", "/v1/login/cli/start", "", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("start = %d", rr.Code)
	}
	var start struct {
		Handle   string `json:"handle"`
		UserCode string `json:"user_code"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &start); err != nil {
		t.Fatal(err)
	}
	if rr := apiReq(t, relayB, "POST", "/v1/login/cli/poll", "", `{"handle":"`+start.Handle+`"}`); rr.Code != http.StatusAccepted {
		t.Fatalf("early poll on B = %d, want 202", rr.Code)
	}
	state, cookie := confirmCode(t, relayB, start.UserCode)

	fv.GrantCode("code-1", Identity{Subject: "42", Login: "alice"})
	req := httptest.NewRequest(http.MethodGet, "/v1/login/callback?code=code-1&state="+url.QueryEscape(state), nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	relayA.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("callback on A = %d, body %s", rec.Code, rec.Body.String())
	}
	if n := countRows(t, st, "account_creds"); n != 0 {
		t.Fatalf("account_creds after callback = %d, want 0 (poll mints, not callback)", n)
	}

	rr2 := apiReq(t, relayB, "POST", "/v1/login/cli/poll", "", `{"handle":"`+start.Handle+`"}`)
	if rr2.Code != http.StatusOK {
		t.Fatalf("final poll on B = %d, body %s", rr2.Code, rr2.Body.String())
	}
	var out struct {
		AccountCredential string `json:"account_credential"`
		Username          string `json:"username"`
		InstallURL        string `json:"install_url"`
	}
	if err := json.Unmarshal(rr2.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Username != "alice" || !strings.Contains(out.InstallURL, "/apps/piper-app/installations/new") {
		t.Fatalf("poll body = %s", rr2.Body.String())
	}
	if acc, err := st.AuthenticateAccount(out.AccountCredential); err != nil || acc.Username != "alice" {
		t.Fatalf("minted credential does not authenticate: %v", err)
	}
	if n := countRows(t, st, "login_cli_handles"); n != 0 {
		t.Fatalf("handle rows after collect = %d, want 0", n)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/relay -run 'TestCLILoginSpansTwoRelays' -count=1`
Expected: FAIL at `early poll on B = 400` (B has no memory of A's handle).

- [ ] **Step 3: Implement**

In `internal/relay/api.go` remove `cliStates` from the struct and `newAPI`. In `internal/relay/weblogin_cli.go`:

Delete the `cliLogin` type. Keep `cliLoginTTL`.

`cliLoginStart` body after the enabled check:

```go
	handle, code := randToken(16), userCode()
	if err := a.st.PutCLIHandle(handle, code, cliLoginTTL); err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"handle": handle, "user_code": code})
```

`cliLoginConfirm` after `ParseForm`:

```go
	handle, ok, err := a.st.ConfirmCLIHandle(r.PostFormValue("code"))
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	if !ok {
		a.renderCLILoginPage(w, "That code didn't match. Check your terminal and try again.")
		return
	}
	// cookie + redirect unchanged
```

`cliCallback`:

```go
func (a *api) cliCallback(w http.ResponseWriter, r *http.Request) bool {
	state := r.URL.Query().Get("state")
	if state == "" {
		return false
	}
	h, ok, err := a.st.CLIHandle(state)
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return true
	}
	if !ok {
		return false // not a CLI handle — let the dashboard flow try
	}

	code := r.URL.Query().Get("code")
	c, err := r.Cookie(stateCookie)
	if !h.Confirmed || code == "" || err != nil || c.Value != state {
		http.Error(w, "bad state", http.StatusBadRequest)
		return true
	}
	id, err := a.webv.Exchange(r.Context(), code)
	if err != nil {
		log.Printf("relay: cli login code exchange failed: %v", err)
		http.Error(w, "code exchange failed", http.StatusBadGateway)
		return true
	}
	acc, err := a.st.UpsertAccount(id.Subject, id.Login)
	if err != nil {
		http.Error(w, "account error", http.StatusInternalServerError)
		return true
	}
	if denyDisabled(w, acc) {
		return true
	}
	// Record who logged in; the credential is minted by the poll that
	// collects it, so no secret sits in the handle row (#522).
	if err := a.st.FinishCLIHandle(state, acc.ID); err != nil {
		http.Error(w, "bad state", http.StatusBadRequest)
		return true
	}
	installURL := ""
	if insts, _ := a.st.InstallationsForAccount(acc.ID); len(insts) == 0 {
		installURL = a.ghApp.InstallURL()
	}

	http.SetCookie(w, &http.Cookie{
		Name: stateCookie, Value: "", MaxAge: -1, Path: "/v1/login",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	if installURL != "" {
		http.Redirect(w, r, installURL, http.StatusFound)
		return true
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, `<!doctype html><meta charset="utf-8"><title>Signed in</title>`+
		`<p style="font:16px system-ui,sans-serif;max-width:24rem;margin:4rem auto">`+
		`You're signed in to Piper. Return to your terminal.</p>`)
	return true
}
```

`cliLoginPoll`:

```go
func (a *api) cliLoginPoll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Handle string `json:"handle"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Handle == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	accountID, username, state, err := a.st.TakeFinishedCLIHandle(req.Handle)
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	switch state {
	case cliHandleUnknown:
		http.Error(w, "unknown or expired handle", http.StatusBadRequest)
		return
	case cliHandlePending:
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "authorization_pending"})
		return
	}
	// Done: this relay mints. The row is already gone, so a retry after a
	// mint failure restarts the flow — the same thing a 500 here always meant.
	cred, err := a.st.MintAccountCredential(accountID)
	if err != nil {
		http.Error(w, "credential error", http.StatusInternalServerError)
		return
	}
	installURL := ""
	if insts, _ := a.st.InstallationsForAccount(accountID); len(insts) == 0 && a.ghApp != nil {
		installURL = a.ghApp.InstallURL()
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"account_credential": cred,
		"username":           username,
		"install_url":        installURL,
	})
}
```

Remove now-unused imports (`crypto/subtle`, `time` if only `cliLoginTTL` remains it is still needed — check with `go vet`).

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/relay -run 'TestCLILogin' -count=1`
Expected: all PASS, including `TestCLILoginDisabledAccountIsForbidden` (callback 403s before `FinishCLIHandle`, so the poll stays 202) and `TestCLILoginAlreadyInstalledShowsDonePage` (poll's `install_url` is empty).

- [ ] **Step 5: Commit**

```bash
git add internal/relay/api.go internal/relay/weblogin_cli.go internal/relay/weblogin_cli_test.go
git commit -m "feat(relay): CLI login handles live in Postgres; the poll mints

Part of #522.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 5: Rate limiter as a fixed window in Postgres

**Files:**
- Modify: `internal/relay/login_state.go` (add `LoginHit`)
- Modify: `internal/relay/login_state_test.go`
- Rewrite: `internal/relay/login_rate.go`
- Rewrite: `internal/relay/login_rate_test.go`
- Modify: `internal/relay/api.go` (`newAPI` sets `loginLimit: loginLimiter{st: st}`)
- Modify: `internal/relay/proxyproto_test.go:583` (`loginLimitBurst` → `loginLimitPerMin`)

**Interfaces:**
- Produces: `func (s *Store) LoginHit(key string, now time.Time, window time.Duration) (hits int, err error)`; `type loginLimiter struct { st *Store; now func() time.Time }` with `allow(ip string) bool`; constants `loginLimitPerMin = 30`, `loginWindow = time.Minute`.
- Deleted: `loginLimitBurst`, `loginLimitMaxIdle`, `loginBucket`, the `golang.org/x/time/rate` import (run `go mod tidy` if nothing else imports it — check with `grep -rn '"golang.org/x/time/rate"' --include='*.go' .`).

- [ ] **Step 1: Write the failing store test**

Append to `internal/relay/login_state_test.go`:

```go
func TestLoginHitCountsWithinWindowAndResets(t *testing.T) {
	st := openTestStore(t)
	now := time.Now()
	for want := 1; want <= 3; want++ {
		hits, err := st.LoginHit("203.0.113.1", now, time.Minute)
		if err != nil || hits != want {
			t.Fatalf("hit %d = (%d, %v)", want, hits, err)
		}
	}
	// Another key is independent.
	if hits, _ := st.LoginHit("203.0.113.2", now, time.Minute); hits != 1 {
		t.Fatalf("other key hits = %d, want 1", hits)
	}
	// Past the window the count restarts.
	if hits, _ := st.LoginHit("203.0.113.1", now.Add(time.Minute), time.Minute); hits != 1 {
		t.Fatalf("hits after window = %d, want 1", hits)
	}
	// Windows an hour stale are swept.
	if _, err := st.LoginHit("203.0.113.3", now.Add(2*time.Hour), time.Minute); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, st, "login_rate"); n != 1 {
		t.Fatalf("login_rate rows = %d, want 1 (stale windows swept)", n)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/relay -run 'TestLoginHit' -count=1`
Expected: build failure, `st.LoginHit undefined`.

- [ ] **Step 3: Implement LoginHit**

Append to `internal/relay/login_state.go`:

```go
// LoginHit records one login request from key and returns how many the
// current fixed window has seen, including this one. now is the caller's
// clock (a test seam); window is the fixed window length. Windows an hour
// stale are swept on the way in so the table stays bounded by recent clients.
func (s *Store) LoginHit(key string, now time.Time, window time.Duration) (int, error) {
	if _, err := s.db.Exec(`DELETE FROM login_rate WHERE window_start < $1::timestamptz - interval '1 hour'`, now); err != nil {
		return 0, err
	}
	var hits int
	err := s.db.QueryRow(
		`INSERT INTO login_rate(key, window_start, hits) VALUES($1, $2, 1)
		 ON CONFLICT(key) DO UPDATE SET
		     hits = CASE WHEN login_rate.window_start <= $2::timestamptz - make_interval(secs => $3)
		                 THEN 1 ELSE login_rate.hits + 1 END,
		     window_start = CASE WHEN login_rate.window_start <= $2::timestamptz - make_interval(secs => $3)
		                         THEN $2::timestamptz ELSE login_rate.window_start END
		 RETURNING hits`,
		key, now, window.Seconds()).Scan(&hits)
	return hits, err
}
```

- [ ] **Step 4: Run the store test**

Run: `go test ./internal/relay -run 'TestLoginHit' -count=1 -v`
Expected: PASS.

- [ ] **Step 5: Rewrite login_rate_test.go for the window semantics**

Replace the file's tests (keep `hitLogin` and `TestRateLimitKey` verbatim):

```go
func TestLoginDeviceRateLimited(t *testing.T) {
	api, _, _ := newTestAPI(t)
	for i := 0; i < loginLimitPerMin; i++ {
		if c := hitLogin(t, api, http.MethodPost, "/v1/login/device", "203.0.113.1"); c != http.StatusOK {
			t.Fatalf("device login #%d = %d, want 200", i+1, c)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/login/device", nil)
	req.RemoteAddr = "203.0.113.1:12345"
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("limit+1 device login = %d, want 429", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "rate limit") {
		t.Fatalf("429 body = %q, want a short plain explanation", rr.Body.String())
	}
}

func TestLoginWebRateLimited(t *testing.T) {
	api, _, _ := newWebTestAPI(t)
	target := "/v1/login/web?redirect_uri=" + url.QueryEscape("https://dash.getpiper.co/auth")
	for i := 0; i < loginLimitPerMin; i++ {
		if c := hitLogin(t, api, http.MethodGet, target, "203.0.113.2"); c != http.StatusFound {
			t.Fatalf("web login #%d = %d, want 302", i+1, c)
		}
	}
	if c := hitLogin(t, api, http.MethodGet, target, "203.0.113.2"); c != http.StatusTooManyRequests {
		t.Fatalf("limit+1 web login = %d, want 429", c)
	}
}

// Both unauthenticated login endpoints draw from the same per-IP window.
func TestLoginRateLimitSharedAcrossEndpoints(t *testing.T) {
	api, _, _ := newWebTestAPI(t)
	target := "/v1/login/web?redirect_uri=" + url.QueryEscape("https://dash.getpiper.co/auth")
	for i := 0; i < loginLimitPerMin/2; i++ {
		hitLogin(t, api, http.MethodPost, "/v1/login/device", "203.0.113.3")
		hitLogin(t, api, http.MethodGet, target, "203.0.113.3")
	}
	if c := hitLogin(t, api, http.MethodPost, "/v1/login/device", "203.0.113.3"); c != http.StatusTooManyRequests {
		t.Fatalf("device login after mixed window = %d, want 429", c)
	}
	if c := hitLogin(t, api, http.MethodGet, target, "203.0.113.3"); c != http.StatusTooManyRequests {
		t.Fatalf("web login after mixed window = %d, want 429", c)
	}
}

func TestLoginRateLimitPerIPIndependent(t *testing.T) {
	api, _, _ := newTestAPI(t)
	for i := 0; i < loginLimitPerMin; i++ {
		hitLogin(t, api, http.MethodPost, "/v1/login/device", "203.0.113.4")
	}
	if c := hitLogin(t, api, http.MethodPost, "/v1/login/device", "203.0.113.4"); c != http.StatusTooManyRequests {
		t.Fatalf("exhausted IP = %d, want 429", c)
	}
	if c := hitLogin(t, api, http.MethodPost, "/v1/login/device", "203.0.113.5"); c != http.StatusOK {
		t.Fatalf("fresh IP = %d, want 200", c)
	}
}

// A new window admits again. No sleeps — the limiter's now func is the seam.
func TestLoginRateLimitNewWindowAdmits(t *testing.T) {
	st := openTestStore(t)
	fakeNow := time.Now()
	l := loginLimiter{st: st, now: func() time.Time { return fakeNow }}
	for i := 0; i < loginLimitPerMin; i++ {
		if !l.allow("203.0.113.6") {
			t.Fatalf("request #%d rejected before the window filled", i+1)
		}
	}
	if l.allow("203.0.113.6") {
		t.Fatal("request past the window's limit allowed, want limited")
	}
	fakeNow = fakeNow.Add(loginWindow)
	if !l.allow("203.0.113.6") {
		t.Fatal("request in the next window rejected, want allowed")
	}
}

// Two IPv6 addresses in the same /64 share a window.
func TestLoginRateLimitIPv6SamePrefixSharesBucket(t *testing.T) {
	st := openTestStore(t)
	l := loginLimiter{st: st}
	for i := 0; i < loginLimitPerMin; i++ {
		if !l.allow("2001:db8:1234:5678::1") {
			t.Fatalf("request #%d from first address rejected before the window filled", i+1)
		}
	}
	if l.allow("2001:db8:1234:5678:ffff:ffff:ffff:ffff") {
		t.Fatal("second address in same /64 allowed, want limited (shared window)")
	}
}

// Two IPv6 addresses in different /64 prefixes get independent windows.
func TestLoginRateLimitIPv6DifferentPrefixIndependent(t *testing.T) {
	st := openTestStore(t)
	l := loginLimiter{st: st}
	for i := 0; i < loginLimitPerMin; i++ {
		l.allow("2001:db8:1111:1111::1")
	}
	if l.allow("2001:db8:1111:1111::1") {
		t.Fatal("request past the limit allowed for first prefix, want limited")
	}
	if !l.allow("2001:db8:2222:2222::1") {
		t.Fatal("request from a different /64 prefix rejected, want allowed")
	}
}

// Behind an edge the same client's requests are spread over every relay; the
// budget is one per client, not one per relay (#522).
func TestLoginRateLimitSharedAcrossRelays(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	fv := NewFakeVerifier()
	relayA, relayB := NewAPI(st, fv), NewAPI(st, fv)
	for i := 0; i < loginLimitPerMin/2; i++ {
		hitLogin(t, relayA, http.MethodPost, "/v1/login/device", "203.0.113.9")
		hitLogin(t, relayB, http.MethodPost, "/v1/login/device", "203.0.113.9")
	}
	if c := hitLogin(t, relayA, http.MethodPost, "/v1/login/device", "203.0.113.9"); c != http.StatusTooManyRequests {
		t.Fatalf("relay A after a full shared window = %d, want 429", c)
	}
	if c := hitLogin(t, relayB, http.MethodPost, "/v1/login/device", "203.0.113.9"); c != http.StatusTooManyRequests {
		t.Fatalf("relay B after a full shared window = %d, want 429", c)
	}
}

// A limiter that cannot reach its store fails closed.
func TestLoginRateLimitFailsClosedWithoutStore(t *testing.T) {
	st := openTestStore(t)
	st.Close()
	l := loginLimiter{st: st}
	if l.allow("203.0.113.10") {
		t.Fatal("limiter allowed a request it could not count")
	}
}
```

Delete `TestLoginRateLimitRefills` and `TestLoginRateLimitSweepsIdleBuckets` (both replaced above; the sweep is covered by `TestLoginHitCountsWithinWindowAndResets`). In `proxyproto_test.go`, change `loginLimitBurst` to `loginLimitPerMin` in the loop at ~line 583 and adjust the message's word "burst" to "limit".

- [ ] **Step 6: Run to verify failure**

Run: `go test ./internal/relay -run 'TestLoginRateLimit|TestLogin.*RateLimited' -count=1`
Expected: build failure on `loginLimitPerMin`/`loginWindow`/`loginLimiter{st:` — those names do not exist yet.

- [ ] **Step 7: Rewrite login_rate.go**

Replace the constants, `loginLimiter`, `loginBucket`, and `allow` (keep `rateLimitKey` and `clientIP` verbatim; drop the `sync` and `golang.org/x/time/rate` imports, add `log`):

```go
// Login rate limits (#106, #522): the unauthenticated login endpoints
// (POST /v1/login/device, GET /v1/login/web, the CLI start and confirm) share
// one per-client fixed window, counted in Postgres so N relays behind an edge
// enforce one budget, not N. Real logins are human-paced — single-digit
// attempts per minute even with retries — and every device-flow start also
// costs an upstream IdP call, so this cap gives honest users ample headroom
// while capping scripted abuse.
const (
	loginLimitPerMin = 30          // requests one client key may make per window
	loginWindow      = time.Minute // fixed window length
)

// loginLimiter is the per-client login rate limit over Store.LoginHit.
type loginLimiter struct {
	st  *Store
	now func() time.Time // test seam; nil ⇒ time.Now
}

// allow reports whether ip may make another login request now. A store error
// fails closed: these are the unauthenticated endpoints, and a relay that
// cannot reach Postgres cannot finish a login anyway.
func (l *loginLimiter) allow(ip string) bool {
	now := time.Now()
	if l.now != nil {
		now = l.now()
	}
	hits, err := l.st.LoginHit(rateLimitKey(ip), now, loginWindow)
	if err != nil {
		log.Printf("relay: login rate limit: %v; refusing", err)
		return false
	}
	return hits <= loginLimitPerMin
}
```

In `api.go`'s `newAPI`, add `loginLimit: loginLimiter{st: st}` to the literal. Then:

```bash
grep -rn '"golang.org/x/time/rate"' --include='*.go' . || go mod tidy
```

- [ ] **Step 8: Run to verify pass**

Run: `go test ./internal/relay -run 'TestLoginRateLimit|TestLogin.*RateLimited|TestRateLimitKey|TestProxyProto' -count=1`
Expected: all PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/relay/login_state.go internal/relay/login_state_test.go internal/relay/login_rate.go internal/relay/login_rate_test.go internal/relay/api.go internal/relay/proxyproto_test.go go.mod go.sum
git commit -m "feat(relay): login rate limit is a fixed window in Postgres

Part of #522.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 6: Device-flow store methods

**Files:**
- Modify: `internal/relay/login_state.go`
- Modify: `internal/relay/login_state_test.go`

**Interfaces:**
- Produces:
  - `type DeviceFlow struct { DeviceCode string; Interval int; Due bool }`
  - `func (s *Store) PutDeviceFlow(handle, deviceCode string, interval int, ttl time.Duration) error`
  - `func (s *Store) DeviceFlow(handle string) (DeviceFlow, bool, error)`
  - `func (s *Store) DeferDeviceFlow(handle string, by time.Duration) error`
  - `func (s *Store) DeleteDeviceFlow(handle string) error`

- [ ] **Step 1: Write the failing tests**

Append to `internal/relay/login_state_test.go`:

```go
// makeDue moves a device flow's next poll into the past.
func makeDue(t *testing.T, st *Store, handle string) {
	t.Helper()
	if _, err := st.db.Exec(`UPDATE login_device_flows SET next_poll_at = now() WHERE handle = $1`, handle); err != nil {
		t.Fatal(err)
	}
}

func TestDeviceFlowDueAndDefer(t *testing.T) {
	st := openTestStore(t)
	if err := st.PutDeviceFlow("h1", "dc-1", 5, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	fl, ok, err := st.DeviceFlow("h1")
	if err != nil || !ok || fl.DeviceCode != "dc-1" || fl.Interval != 5 || fl.Due {
		t.Fatalf("fresh flow = %+v ok=%v err=%v, want not yet due", fl, ok, err)
	}
	makeDue(t, st, "h1")
	if fl, _, _ := st.DeviceFlow("h1"); !fl.Due {
		t.Fatal("flow not due after next_poll_at passed")
	}
	if err := st.DeferDeviceFlow("h1", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if fl, _, _ := st.DeviceFlow("h1"); fl.Due {
		t.Fatal("flow still due after defer")
	}
	if err := st.DeleteDeviceFlow("h1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.DeviceFlow("h1"); ok {
		t.Fatal("flow readable after delete")
	}
}

func TestDeviceFlowExpiredIsInvisibleAndSwept(t *testing.T) {
	st := openTestStore(t)
	if err := st.PutDeviceFlow("stale", "dc-0", 5, -time.Second); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.DeviceFlow("stale"); ok {
		t.Fatal("expired flow readable")
	}
	if err := st.PutDeviceFlow("fresh", "dc-1", 5, time.Minute); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, st, "login_device_flows"); n != 1 {
		t.Fatalf("rows after sweep = %d, want 1", n)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/relay -run 'TestDeviceFlow' -count=1`
Expected: build failure, `st.PutDeviceFlow undefined`.

- [ ] **Step 3: Implement**

Append to `internal/relay/login_state.go`:

```go
// DeviceFlow is one GitHub device-code login as the poll sees it.
type DeviceFlow struct {
	DeviceCode string
	Interval   int  // seconds between upstream polls, as GitHub asked
	Due        bool // next_poll_at has passed: the caller may ask GitHub now
}

// PutDeviceFlow records a started device flow. The first upstream poll is
// due one interval from now, as GitHub's protocol requires.
func (s *Store) PutDeviceFlow(handle, deviceCode string, interval int, ttl time.Duration) error {
	if _, err := s.db.Exec(`DELETE FROM login_device_flows WHERE expires_at <= now()`); err != nil {
		return err
	}
	_, err := s.db.Exec(
		`INSERT INTO login_device_flows(handle, device_code, interval_secs, next_poll_at, expires_at)
		 VALUES($1, $2, $3, now() + make_interval(secs => $3), now() + make_interval(secs => $4))`,
		handle, deviceCode, interval, ttl.Seconds())
	return err
}

// DeviceFlow reads one unexpired flow; Due is evaluated on the server clock.
func (s *Store) DeviceFlow(handle string) (DeviceFlow, bool, error) {
	var fl DeviceFlow
	err := s.db.QueryRow(
		`SELECT device_code, interval_secs, next_poll_at <= now()
		   FROM login_device_flows WHERE handle = $1 AND expires_at > now()`,
		handle).Scan(&fl.DeviceCode, &fl.Interval, &fl.Due)
	if errors.Is(err, sql.ErrNoRows) {
		return DeviceFlow{}, false, nil
	}
	if err != nil {
		return DeviceFlow{}, false, err
	}
	return fl, true, nil
}

// DeferDeviceFlow pushes the next upstream poll `by` into the future.
func (s *Store) DeferDeviceFlow(handle string, by time.Duration) error {
	_, err := s.db.Exec(
		`UPDATE login_device_flows SET next_poll_at = now() + make_interval(secs => $2) WHERE handle = $1`,
		handle, by.Seconds())
	return err
}

// DeleteDeviceFlow retires a flow once its outcome has been handed out.
func (s *Store) DeleteDeviceFlow(handle string) error {
	_, err := s.db.Exec(`DELETE FROM login_device_flows WHERE handle = $1`, handle)
	return err
}
```

Note `$3` in `PutDeviceFlow` is used both as the `INTEGER` column value and inside `make_interval(secs => $3)`, which takes `double precision`. If Postgres rejects the mixed inference ("inconsistent types deduced for parameter $3"), pass the interval twice: `interval, float64(interval)` and use `$3` for the column and `$4` for `make_interval`, shifting the TTL to `$5`.

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/relay -run 'TestDeviceFlow' -count=1 -v`
Expected: both PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/relay/login_state.go internal/relay/login_state_test.go
git commit -m "feat(relay): device-flow store methods

Part of #522.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 7: Stateless GitHub device flow

**Files:**
- Modify: `internal/relay/verifier_github.go` (struct ~26-47, `NewGitHubVerifier` ~52-63, `Start` ~73-118, delete `pollUntilDone` ~120-165, rewrite `Poll` ~209-220, delete `sweepLocked` ~226-234, delete `githubFlow` and `flowGrace`)
- Rewrite device-flow tests in: `internal/relay/verifier_github_test.go` (keep `fakeGitHub`, `TestGitHubAuthCodeURL`, `TestGitHubExchange*`)
- Modify: `internal/relay/verifier_test.go:61`
- Modify: `cmd/piper-relay/main.go:223`

**Interfaces:**
- Consumes: Task 6's methods.
- Produces: `func NewGitHubVerifier(clientID, clientSecret string, st *Store) *GitHubVerifier`. `Verifier`/`WebVerifier` interfaces unchanged.

- [ ] **Step 1: Rewrite the device-flow tests (failing)**

In `internal/relay/verifier_github_test.go`, replace `newTestGitHubVerifier`, `waitDone`, and the five `TestGitHubDeviceFlow*` tests plus `TestGitHubVerifierPollUnknownHandle` with:

```go
// newTestGitHubVerifier points a GitHubVerifier at the fake, over st.
func newTestGitHubVerifier(t *testing.T, fake *fakeGitHub, st *Store) *GitHubVerifier {
	t.Helper()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	v := NewGitHubVerifier("test-client", "test-secret", st)
	v.oauthBase = srv.URL
	v.apiBase = srv.URL
	return v
}

func (f *fakeGitHub) tokenPolls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tokenForms)
}

// A device flow resolves through the polls the CLI already makes: nothing
// runs in the background, GitHub is asked only once the interval has passed,
// and the handle redeems exactly once (#522).
func TestGitHubDeviceFlowApproved(t *testing.T) {
	st := openTestStore(t)
	fake := &fakeGitHub{t: t, tokenResponses: []map[string]any{
		{"error": "authorization_pending"},
		{"access_token": "gho_tok", "token_type": "bearer"},
	}}
	v := newTestGitHubVerifier(t, fake, st)

	handle, da, err := v.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if da.UserCode != "ABCD-1234" || da.VerificationURI != "https://github.test/login/device" ||
		da.Interval != 5 || da.ExpiresIn != 900 {
		t.Fatalf("DeviceAuth = %+v", da)
	}

	// Before the interval has passed a poll is pending and costs no upstream call.
	if _, err := v.Poll(context.Background(), handle); !errors.Is(err, ErrAuthPending) {
		t.Fatalf("early Poll err = %v, want pending", err)
	}
	if n := fake.tokenPolls(); n != 0 {
		t.Fatalf("early poll made %d upstream calls, want 0", n)
	}

	makeDue(t, st, handle)
	if _, err := v.Poll(context.Background(), handle); !errors.Is(err, ErrAuthPending) {
		t.Fatalf("Poll (github pending) err = %v, want pending", err)
	}
	// That poll spent the slot: the next one waits again.
	if _, err := v.Poll(context.Background(), handle); !errors.Is(err, ErrAuthPending) || fake.tokenPolls() != 1 {
		t.Fatalf("Poll right after upstream = %v with %d calls, want pending and 1", err, fake.tokenPolls())
	}

	makeDue(t, st, handle)
	id, err := v.Poll(context.Background(), handle)
	if err != nil {
		t.Fatalf("Poll (approved): %v", err)
	}
	if id.Subject != "583231" || id.Login != "Octo-Cat" {
		t.Fatalf("identity = %+v", id)
	}
	fake.mu.Lock()
	form := fake.tokenForms[0]
	fake.mu.Unlock()
	if form["grant_type"] != "urn:ietf:params:oauth:grant-type:device_code" || form["device_code"] != "dc-1" {
		t.Fatalf("token form = %+v", form)
	}
	// Redeemed once: the row is gone.
	if _, err := v.Poll(context.Background(), handle); err == nil || errors.Is(err, ErrAuthPending) {
		t.Fatalf("Poll(redeemed handle) = %v, want unknown-handle error", err)
	}
}

func TestGitHubDeviceFlowSlowDownDefersByInterval(t *testing.T) {
	st := openTestStore(t)
	fake := &fakeGitHub{t: t, tokenResponses: []map[string]any{
		{"error": "slow_down"},
		{"access_token": "gho_tok", "token_type": "bearer"},
	}}
	v := newTestGitHubVerifier(t, fake, st)
	handle, _, err := v.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	makeDue(t, st, handle)
	if _, err := v.Poll(context.Background(), handle); !errors.Is(err, ErrAuthPending) {
		t.Fatalf("Poll (slow_down) err = %v, want pending", err)
	}
	// GitHub semantics: wait interval + 5s. The fake's interval is 5.
	var secs float64
	if err := st.db.QueryRow(`SELECT EXTRACT(EPOCH FROM next_poll_at - now()) FROM login_device_flows WHERE handle = $1`, handle).Scan(&secs); err != nil {
		t.Fatal(err)
	}
	if secs < 9 || secs > 10 {
		t.Fatalf("next poll in %.1fs, want ~10s after slow_down", secs)
	}
	makeDue(t, st, handle)
	if _, err := v.Poll(context.Background(), handle); err != nil {
		t.Fatalf("Poll (approved): %v", err)
	}
}

func TestGitHubDeviceFlowDenied(t *testing.T) {
	st := openTestStore(t)
	fake := &fakeGitHub{t: t, tokenResponses: []map[string]any{{"error": "access_denied"}}}
	v := newTestGitHubVerifier(t, fake, st)
	handle, _, err := v.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	makeDue(t, st, handle)
	if _, err := v.Poll(context.Background(), handle); err == nil || errors.Is(err, ErrAuthPending) {
		t.Fatalf("denied flow err = %v, want terminal error", err)
	}
	// Terminal: the row is gone, a retry is unknown.
	if _, err := v.Poll(context.Background(), handle); err == nil || errors.Is(err, ErrAuthPending) {
		t.Fatalf("Poll after denial = %v, want unknown-handle error", err)
	}
}

// Most device flows are never polled to completion: the user closes the tab,
// the CLI is Ctrl-C'd, the code expires unapproved. Expired rows are
// invisible, and the next Start sweeps them (#81).
func TestGitHubDeviceFlowSweepsExpired(t *testing.T) {
	st := openTestStore(t)
	v := newTestGitHubVerifier(t, &fakeGitHub{t: t}, st)
	var abandoned []string
	for i := 0; i < 3; i++ {
		h, _, err := v.Start(context.Background())
		if err != nil {
			t.Fatalf("Start %d: %v", i, err)
		}
		abandoned = append(abandoned, h)
	}
	if _, err := st.db.Exec(`UPDATE login_device_flows SET expires_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	for i, h := range abandoned {
		if _, err := v.Poll(context.Background(), h); err == nil || errors.Is(err, ErrAuthPending) {
			t.Errorf("Poll(expired handle %d) = %v, want unknown-handle error", i, err)
		}
	}
	if _, _, err := v.Start(context.Background()); err != nil {
		t.Fatalf("Start (post-expiry): %v", err)
	}
	if n := countRows(t, st, "login_device_flows"); n != 1 {
		t.Errorf("flows after sweep = %d, want 1 (only the live flow)", n)
	}
}

// Start on one relay, poll on another: the flow lives in the store, not the
// process that started it.
func TestGitHubDeviceFlowSpansTwoRelays(t *testing.T) {
	st := openTestStore(t)
	fake := &fakeGitHub{t: t, tokenResponses: []map[string]any{
		{"access_token": "gho_tok", "token_type": "bearer"},
	}}
	relayA := newTestGitHubVerifier(t, fake, st)
	relayB := newTestGitHubVerifier(t, fake, st)
	handle, _, err := relayA.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := relayB.Poll(context.Background(), handle); !errors.Is(err, ErrAuthPending) {
		t.Fatalf("early Poll on B = %v, want pending", err)
	}
	makeDue(t, st, handle)
	id, err := relayB.Poll(context.Background(), handle)
	if err != nil || id.Login != "Octo-Cat" {
		t.Fatalf("Poll on B = (%+v, %v)", id, err)
	}
}

func TestGitHubVerifierPollUnknownHandle(t *testing.T) {
	v := NewGitHubVerifier("test-client", "test-secret", openTestStore(t))
	if _, err := v.Poll(context.Background(), "never-started"); err == nil {
		t.Fatal("Poll(unknown) succeeded, want error")
	}
}
```

Remove the now-unused `sync` and `time` imports from the test file if nothing else uses them (`fakeGitHub` still uses `sync`). In `verifier_test.go:61` change to `NewGitHubVerifier("id", "secret", nil)`.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/relay -run 'TestGitHub' -count=1`
Expected: build failure, `too many arguments in call to NewGitHubVerifier`.

- [ ] **Step 3: Implement**

In `internal/relay/verifier_github.go`:

Struct and constructor:

```go
// GitHubVerifier brokers GitHub's OAuth device flow. It holds the client
// secret so callers never see it. GitHub's device flow returns no ID token —
// identity comes from GET /user with the granted access token, which is used
// once and discarded. Flows live in the store (#522): Start records the
// device code, and the poll that arrives once GitHub's interval has passed
// makes one upstream request, so any relay serves any poll and nothing runs
// in the background.
type GitHubVerifier struct {
	clientID, clientSecret string
	oauthBase              string // https://github.com; tests override
	apiBase                string // https://api.github.com; tests override
	httpc                  *http.Client
	st                     *Store
}

// flowGrace is how long past a device code's expiry a flow stays resolvable,
// so a poll racing the deadline still gets GitHub's own "expired" answer
// rather than a bare unknown-handle.
const flowGrace = time.Minute

func NewGitHubVerifier(clientID, clientSecret string, st *Store) *GitHubVerifier {
	return &GitHubVerifier{
		clientID:     clientID,
		clientSecret: clientSecret,
		oauthBase:    "https://github.com",
		apiBase:      "https://api.github.com",
		httpc:        &http.Client{Timeout: 15 * time.Second},
		st:           st,
	}
}
```

`Start`, after the device-code request succeeds:

```go
	raw := make([]byte, 8)
	_, _ = rand.Read(raw)
	handle := hex.EncodeToString(raw)

	expiresIn := res.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 900 // GitHub's documented device-code lifetime
	}
	interval := res.Interval
	if interval <= 0 {
		interval = 5
	}
	if err := g.st.PutDeviceFlow(handle, res.DeviceCode, interval,
		time.Duration(expiresIn)*time.Second+flowGrace); err != nil {
		return "", DeviceAuth{}, err
	}
	return handle, DeviceAuth{
		UserCode:        res.UserCode,
		VerificationURI: res.VerificationURI,
		Interval:        res.Interval,
		ExpiresIn:       res.ExpiresIn,
	}, nil
```

Delete `pollUntilDone`, `sweepLocked`, `githubFlow`. Replace `Poll`:

```go
// Poll advances a flow by at most one upstream request. Before next_poll_at
// it is pending for free; after it, GitHub is asked once and the row is
// deferred (pending, slow_down) or retired (any terminal answer, which is
// returned to this caller and to nobody else).
func (g *GitHubVerifier) Poll(ctx context.Context, handle string) (Identity, error) {
	fl, ok, err := g.st.DeviceFlow(handle)
	if err != nil {
		return Identity{}, err
	}
	if !ok {
		return Identity{}, errors.New("unknown handle")
	}
	if !fl.Due {
		return Identity{}, ErrAuthPending
	}
	var tr githubTokenResponse
	err = g.postForm(ctx, g.oauthBase+"/login/oauth/access_token", url.Values{
		"client_id":   {g.clientID},
		"device_code": {fl.DeviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}, &tr)
	if err != nil {
		return g.finish(handle, Identity{}, err)
	}
	switch tr.Error {
	case "":
		if tr.AccessToken == "" {
			return g.finish(handle, Identity{}, errors.New("github token response missing access_token"))
		}
		id, err := g.fetchUser(ctx, tr.AccessToken)
		return g.finish(handle, id, err)
	case "authorization_pending":
		return g.defer_(handle, fl.Interval)
	case "slow_down":
		// GitHub's documented semantics: wait interval + 5s.
		return g.defer_(handle, fl.Interval+5)
	default: // expired_token, access_denied, incorrect_device_code, ...
		return g.finish(handle, Identity{}, fmt.Errorf("github device flow: %s", tr.Error))
	}
}

func (g *GitHubVerifier) defer_(handle string, secs int) (Identity, error) {
	if err := g.st.DeferDeviceFlow(handle, time.Duration(secs)*time.Second); err != nil {
		return Identity{}, err
	}
	return Identity{}, ErrAuthPending
}

// finish retires the flow and hands its outcome to this one caller.
func (g *GitHubVerifier) finish(handle string, id Identity, outcome error) (Identity, error) {
	if err := g.st.DeleteDeviceFlow(handle); err != nil {
		return Identity{}, err
	}
	return id, outcome
}
```

Name the deferral helper `deferFlow` rather than `defer_` if you prefer; either compiles. Remove the `sync` import. `fetchUser`, `postForm`, `AuthCodeURL`, `Exchange` are unchanged.

In `cmd/piper-relay/main.go:223`:

```go
		v = relay.NewGitHubVerifier(id, env("PIPER_RELAY_GITHUB_CLIENT_SECRET", ""), st)
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/relay ./cmd/piper-relay -run 'TestGitHub|TestVerifier|TestLoginDevice' -count=1` and `go vet ./...`
Expected: all PASS, vet clean.

- [ ] **Step 5: Commit**

```bash
git add internal/relay/verifier_github.go internal/relay/verifier_github_test.go internal/relay/verifier_test.go cmd/piper-relay/main.go
git commit -m "feat(relay): GitHub device flow is stateless over Postgres

Part of #522.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 8: Edge round-robins api.<apex>

**Files:**
- Modify: `internal/relay/edge_state.go` (struct ~17-22, `pickAPI` ~93-101)
- Modify: `internal/relay/edge.go` (`handleTLS` comment ~244-246)
- Modify: `internal/relay/edge_state_test.go` (add a unit test)
- Modify: `internal/relay/edge_test.go` (~310-327 in `TestEdgePlacesAndRoutesAcrossTwoRelays`)

- [ ] **Step 1: Write the failing tests**

Append to `internal/relay/edge_state_test.go`:

```go
// api.<apex> is round-robined: with login state in Postgres (#522) any relay
// serves any request, so the edge spreads them instead of pinning one.
func TestEdgeStatePickAPIRoundRobins(t *testing.T) {
	s := newEdgeState()
	t0 := time.Now()
	s.setInstances([]InstanceRow{
		{ID: "b", StartedAt: t0.Add(time.Second)},
		{ID: "a", StartedAt: t0},
	})
	var got []string
	for i := 0; i < 4; i++ {
		r, ok := s.pickAPI()
		if !ok {
			t.Fatal("pickAPI found nothing")
		}
		got = append(got, r.ID)
	}
	if want := []string{"a", "b", "a", "b"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("pickAPI order = %v, want %v", got, want)
	}
	s.evict("a")
	if r, ok := s.pickAPI(); !ok || r.ID != "b" {
		t.Fatalf("pickAPI after evict = (%v, %v), want b", r.ID, ok)
	}
}
```

Add `"strings"` and `"time"` to that file's imports if missing.

In `edge_test.go`, replace the pinned assertion block (from the `// api.<apex> is pinned…` comment through the closing `}` of the `if resp.StatusCode != …` check) with:

```go
	// api.<apex> round-robins across the pool (#522): two requests reach two
	// different relays' control planes (no bearer → 401 from each).
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		tc, err := tls.Dial("tcp", cfg.TLSAddr, &tls.Config{ServerName: "api.public.getpiper.co", InsecureSkipVerify: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tc, "GET /agents HTTP/1.1\r\nHost: api.public.getpiper.co\r\nConnection: close\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(tc), nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		tc.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("api.<apex> via edge: %d, want 401", resp.StatusCode)
		}
		seen[resp.Header.Get("X-Relay")] = true
	}
	if !seen[rA.inst.ID] || !seen[rB.inst.ID] {
		t.Fatalf("api.<apex> reached %v, want both %s and %s", seen, rA.inst.ID, rB.inst.ID)
	}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/relay -run 'TestEdgeStatePickAPIRoundRobins|TestEdgePlacesAndRoutesAcrossTwoRelays' -count=1`
Expected: both FAIL — the unit test gets `a,a,a,a`, the cluster test sees only `rA`.

- [ ] **Step 3: Implement**

In `edge_state.go`, add `"sort"` and `"sync/atomic"` to the imports, add a field to `edgeState`:

```go
	apiNext atomic.Uint64 // round-robin cursor for api.<apex>
```

and replace `pickAPI`:

```go
// pickAPI spreads api.<apex> across the live pool. Login-flow state lives in
// Postgres (#522), so any relay answers any control-plane request; a stable
// order plus a cursor gives each relay its turn. Eviction or a resync just
// shifts the cursor's target by one, which is harmless.
func (s *edgeState) pickAPI() (InstanceRow, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.instances) == 0 {
		return InstanceRow{}, false
	}
	live := make([]InstanceRow, 0, len(s.instances))
	for _, r := range s.instances {
		live = append(live, r)
	}
	sort.Slice(live, func(i, j int) bool { return earlier(live[i], live[j]) })
	n := s.apiNext.Add(1) - 1
	return live[int(n%uint64(len(live)))], true
}
```

In `edge.go`'s `handleTLS`, delete the two comment lines about pinning so the branch reads:

```go
	if sni == e.apiHost {
		target, ok = e.state.pickAPI()
	} else if agent, found := e.resolveAgent(sni, false); found {
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/relay -run 'TestEdge' -count=1`
Expected: all edge tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/relay/edge_state.go internal/relay/edge_state_test.go internal/relay/edge.go internal/relay/edge_test.go
git commit -m "feat(relay): piper-edge round-robins api.<apex>

Login-flow state is in Postgres, so the pin to one relay is no longer
needed.

Part of #522.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 9: Docs, verify, PR

**Files:**
- Modify: `PROGRESS.md` (after the `piper-edge + tunnel ownership` line, ~55)
- Modify: `docs/superpowers/specs/2026-09-04-login-state-postgres-design.md` (device-flow table and store method list)

- [ ] **Step 1: PROGRESS.md**

Insert after the `#521` line:

```markdown
- ✅ `piper-relay` login-flow state in Postgres — web-login states, CLI browser-login handles, GitHub device flows, and the login rate limit are shared tables with TTLs; `piper-edge` round-robins `api.<apex>` — [#522](https://github.com/piperbox/piper/issues/522) (child of epic [#524](https://github.com/piperbox/piper/issues/524))
```

Update the `_Last updated:` line at the top to mention child 2 of epic #524 landed.

- [ ] **Step 2: Spec touch-up**

In the spec's Data model, replace the `login_device_flows` block with the five-column version from Task 1 and change its comment to say the poll that gets a terminal answer returns it and deletes the row. In the Store section, replace the device-flow bullet list with `PutDeviceFlow`, `DeviceFlow` (returns `Due`), `DeferDeviceFlow(handle, by time.Duration)`, `DeleteDeviceFlow`. In the Verifier section, drop the mentions of `ResolveDeviceFlow`/`FailDeviceFlow` and "the outcome lands in the row": the resolving poll returns the outcome and retires the row. Under Failure handling, the "two relays poll the same device flow" paragraph stays true (the loser's upstream call gets `slow_down` or, if the winner already deleted the row, its Poll returns unknown handle — mention that second case in one sentence).

- [ ] **Step 3: Verify**

```bash
make verify; echo "exit=$?"
```

Expected: `exit=0`. If gofmt lists files, run `make fmt` and re-run. Confirm the Postgres-backed tests ran (not skipped):

```bash
go test ./internal/relay -run 'TestWebState|TestCLIHandle|TestDeviceFlow|TestLoginHit|TestGitHubDeviceFlow|SpansTwoRelays|AnotherRelay|AcrossRelays' -count=1 -v 2>&1 | grep -E '^(--- |ok|FAIL)' | sort | uniq -c
```

Every listed test shows `--- PASS`, none `--- SKIP`.

- [ ] **Step 4: Commit and push**

```bash
git add PROGRESS.md docs/superpowers/specs/2026-09-04-login-state-postgres-design.md
git commit -m "docs: login-flow state in Postgres landed (#522)

Part of #522.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
git push -u origin ozykhan/login-state-postgres
```

- [ ] **Step 5: Open the PR**

```bash
gh pr create --base main --title "[relay] login-flow state in Postgres; edge round-robins api.<apex>" --body-file - <<'EOF'
Closes #522. Part of #524.

## What

Every piece of login state that had to survive a second request landing on a different relay now lives in the shared Postgres, so `piper-edge` no longer pins `api.<apex>` to one relay.

- **`login_web_states`** — dashboard browser login; the callback redeems with one guarded `DELETE … RETURNING`.
- **`login_cli_handles`** — brokered CLI login. The callback records the account; **the poll mints the credential**, so no secret is ever at rest in a login table.
- **`login_device_flows`** — GitHub device flow is now stateless: no background poller. The CLI poll that arrives after `next_poll_at` makes one upstream call; `slow_down` defers by interval+5s; a terminal answer is returned to that caller and the row is deleted. A relay dying mid-flow strands nothing.
- **`login_rate`** — fixed one-minute window per client key, 30/min, one upsert. Replaces the per-process token bucket (burst 10 / 30 per min) so N relays enforce one budget, not N. Fails closed on a store error.
- **Edge** — `pickAPI` round-robins live relays in start order.

Wire shapes of `/v1/login/*` are unchanged. Spec: `docs/superpowers/specs/2026-09-04-login-state-postgres-design.md`.

## Deploy

Schema is additive (`CREATE TABLE IF NOT EXISTS`); no database reset. Relays and edges can roll in any order. Not in scope: graceful drain (#523).

## Tests

Store tests per table (single use, expiry, sweep, state machine, window reset); cross-relay tests for web login, CLI login, device flow and the rate limit (two `api`/verifier values over one store); GitHub device-flow tests rewritten for the stateless shape against the existing fake; edge round-robin unit + cluster test. `make verify` exits 0.

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
```

---

## Self-review

**Spec coverage.** Tables → Task 1. Web states → Tasks 1–2. CLI handles + poll mints → Tasks 3–4. Rate limiter fixed window, fail-closed, key masking kept → Task 5. Device flows stateless, grace kept, slow_down semantics → Tasks 6–7. Edge round-robin + comment removal → Task 8. `lastReconcile` untouched (non-goal). Docs/PROGRESS/deploy note → Task 9. The spec's outcome columns are dropped deliberately (header note + Task 9 step 2).

**Type consistency.** `TakeFinishedCLIHandle` returns `(accountID, username string, state cliHandleState, err error)` in Task 3 and is consumed with that shape in Task 4. `DeviceFlow.Due`/`Interval`/`DeviceCode` are used identically in Tasks 6–7. `newWebTestAPI` changes to three return values in Task 2 and Task 5's tests use `api, _, _ :=`. `makeDue` and `countRows` are defined in `login_state_test.go` (Tasks 1 and 6) and reused by Tasks 4, 5, 7 within the same package.

**Placeholders.** None: every code step carries the code.
