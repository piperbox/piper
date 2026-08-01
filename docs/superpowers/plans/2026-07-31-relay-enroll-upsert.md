# One-command login — Plan 1: Relay idempotent enroll upsert (`box_id`)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `POST /v1/enroll` idempotent per box: an enroll carrying a `box_id` upserts the agent row keyed on `(account_id, box_id)` — rotating the enrollment token but reusing the base domain, webhook secret, and quota slot — so a crashed or repeated claim can never strand a quota slot or mint a surprise second base domain.

**Architecture:** One schema edit (`agents.box_id` + a partial unique index), one store change (`EnrollForAccount` gains a `boxID` parameter and an upsert branch), one API change (the enroll handler parses `box_id` from the body it already reads for `org`), and one client change (`relayclient.Enroll` sends `{box_id, org}`). An **empty `box_id` keeps today's insert-every-time semantics** so the existing `piper connect` and e2e flows stay green until Plans 2–3 replace them.

**Tech Stack:** Go 1.26, `modernc.org/sqlite` (pure Go), stdlib `net/http` + `net/http/httptest`.

This is **Plan 1 of 3** for [`docs/superpowers/specs/2026-07-31-one-command-login-design.md`](../specs/2026-07-31-one-command-login-design.md). Plan 2 = piperd unix-socket enrollment + drain-then-exec apply; Plan 3 = the merged `piper login` CLI pipeline.

## Global Constraints

- **No cgo.** `CGO_ENABLED=0` everywhere; SQLite is `modernc.org/sqlite` only. `make cross` must stay green.
- **Module path** `github.com/piperbox/piper`.
- **TDD.** Every task is failing-test-first. `make verify` (gofmt → vet → test → cross) before every push; judge it by exit status, not output grep.
- **Pre-1.x schema policy:** `schema.sql` is always the complete current shape — edit `CREATE TABLE` directly, **no migrations, no legacy readers**. Existing relay DBs (ours only) are wiped/re-enrolled.
- **Layering:** `internal/relay` never imports `store`/`deploy`/`api`/`runtime`/`caddy`; `internal/relayclient` stays a thin HTTP view of the relay API.
- **Secrets hashed at rest:** enrollment tokens stored only as `sha256` hex via the existing `hashToken` in `internal/relay/store.go`.
- **Before Task 1:** file the tracking issue with the `file-issue` skill — title `[relay] idempotent enroll upsert keyed by box_id`, labels `relay`, `enhancement`, `P1`, `size/S` — and reference it as `Part of #<that issue>` in every commit in this plan.
- Commits are conventional-commit style ending with:
  `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`

## File Structure

- Modify `internal/relay/schema.sql` — `agents` gains `box_id`; partial unique index on `(account_id, box_id)`.
- Modify `internal/relay/accounts.go` — `EnrollForAccount(accountID, boxID string)` with the upsert branch.
- Modify `internal/relay/accounts_test.go` — upsert tests + signature fix-ups for existing calls.
- Modify `internal/relay/api.go` — enroll handler parses `box_id`.
- Create `internal/relay/enroll_api_test.go` — handler-level box_id tests (kept out of the crowded `orgs_api_test.go`).
- Modify `internal/relayclient/relayclient.go` — `Enroll` gains `boxID, org` and posts a JSON body.
- Create `internal/relayclient/enroll_test.go` — asserts the wire body.
- Modify `cmd/piper/relayonboard.go` — `connect` passes `"", ""` (behavior unchanged).
- Modify every other `.Enroll(` call site found by grep (e2e/tests) the same way.

---

### Task 1: Schema + `EnrollForAccount` upsert

**Files:**
- Modify: `internal/relay/schema.sql`
- Modify: `internal/relay/accounts.go:200` (the `EnrollForAccount` function)
- Modify: `internal/relay/accounts_test.go`

**Interfaces:**
- Consumes: existing `Store`, `hashToken`, `maxAgentsOrDefault`, `apexOrDefault`, `isUniqueViolation` from `internal/relay/store.go`; `openTestStore(t)` helper from `accounts_test.go`.
- Produces: `func (s *Store) EnrollForAccount(accountID, boxID string) (Enrollment, error)` — `boxID == ""` behaves exactly as today (fresh row per call); `boxID != ""` upserts on `(account_id, box_id)`: existing row keeps `name`/`base_domain`/`webhook_secret`, gets a fresh token (old token stops authenticating), and skips the quota check. The `Enrollment` result type is unchanged.

- [ ] **Step 1: Write the failing tests**

Add to `internal/relay/accounts_test.go`:

```go
func TestEnrollForAccountUpsertsByBoxID(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	acc, _ := st.UpsertAccount("sub-1", "erin")

	first, err := st.EnrollForAccount(acc.ID, "box-aaaa")
	if err != nil {
		t.Fatalf("first enroll: %v", err)
	}
	second, err := st.EnrollForAccount(acc.ID, "box-aaaa")
	if err != nil {
		t.Fatalf("re-enroll: %v", err)
	}
	if second.BaseDomain != first.BaseDomain {
		t.Fatalf("base domain changed on re-enroll: %q -> %q", first.BaseDomain, second.BaseDomain)
	}
	if second.WebhookSecret != first.WebhookSecret {
		t.Fatalf("webhook secret changed on re-enroll")
	}
	if second.Token == first.Token {
		t.Fatal("token did not rotate on re-enroll")
	}
	// The rotated token authenticates; the old one no longer does.
	if _, err := st.Authenticate(second.Token); err != nil {
		t.Fatalf("new token rejected: %v", err)
	}
	if _, err := st.Authenticate(first.Token); err == nil {
		t.Fatal("old token still authenticates after rotation")
	}
	// One row, one quota slot.
	var count int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM agents WHERE account_id=?`, acc.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("agents rows = %d, want 1", count)
	}
}

func TestEnrollForAccountUpsertSkipsQuotaAtCap(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 1, 10, 5) // cap of one
	acc, _ := st.UpsertAccount("sub-1", "erin")

	if _, err := st.EnrollForAccount(acc.ID, "box-aaaa"); err != nil {
		t.Fatalf("enroll at empty cap: %v", err)
	}
	// Re-enrolling the same box at cap reuses the slot.
	if _, err := st.EnrollForAccount(acc.ID, "box-aaaa"); err != nil {
		t.Fatalf("re-enroll at cap: %v", err)
	}
	// A different box is over quota.
	if _, err := st.EnrollForAccount(acc.ID, "box-bbbb"); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("new box at cap: err = %v, want ErrQuotaExceeded", err)
	}
}

func TestEnrollForAccountEmptyBoxIDInsertsFreshRows(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	acc, _ := st.UpsertAccount("sub-1", "erin")

	a, err := st.EnrollForAccount(acc.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.EnrollForAccount(acc.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if a.BaseDomain == b.BaseDomain {
		t.Fatal("empty box_id must keep insert-per-call semantics")
	}
}

func TestEnrollForAccountBoxIDScopedPerAccount(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	a1, _ := st.UpsertAccount("sub-1", "erin")
	a2, _ := st.UpsertAccount("sub-2", "frank")

	e1, err := st.EnrollForAccount(a1.ID, "box-aaaa")
	if err != nil {
		t.Fatal(err)
	}
	// The same box_id under another account is a distinct agent, not an upsert
	// into someone else's row.
	e2, err := st.EnrollForAccount(a2.ID, "box-aaaa")
	if err != nil {
		t.Fatal(err)
	}
	if e1.BaseDomain == e2.BaseDomain {
		t.Fatal("box_id collided across accounts")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/relay/ -run 'TestEnrollForAccount' -v`
Expected: compile FAIL — `too many arguments in call to st.EnrollForAccount` (the new tests pass two args; the existing function takes one). This is the signature-change signal; the existing single-arg tests fail the same way once the signature changes, which Step 3 fixes together.

- [ ] **Step 3: Edit the schema**

In `internal/relay/schema.sql`, replace the `agents` table and its index block (the file's first two statements) with:

```sql
CREATE TABLE IF NOT EXISTS agents (
    name           TEXT PRIMARY KEY,
    token_hash     TEXT NOT NULL UNIQUE,
    base_domain    TEXT NOT NULL,
    account_id     TEXT,
    -- box_id is the durable identity piperd mints on first enroll
    -- (<dataDir>/box-id). Non-empty box_id makes enroll an upsert keyed on
    -- (account_id, box_id): token rotates, base domain and quota slot are
    -- reused. Empty box_id is an operator/legacy enroll: fresh row per call.
    box_id         TEXT,
    control_token  TEXT,
    webhook_secret TEXT,
    created_at     TEXT NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS agents_base_domain_unique
    ON agents(base_domain);

CREATE UNIQUE INDEX IF NOT EXISTS agents_account_box_unique
    ON agents(account_id, box_id)
    WHERE box_id IS NOT NULL AND box_id != '';
```

Per the pre-1.x policy this edits the `CREATE TABLE` in place — no `ALTER`, no migration. An existing relay DB predating the column is unsupported; the hosted relay gets a wipe/redeploy and our boxes re-enroll (already flagged in the spec).

- [ ] **Step 4: Implement the upsert**

In `internal/relay/accounts.go`, change `EnrollForAccount`. The function keeps its shape (immediate transaction, cap check, retry loop); the upsert branch goes after the username lookup and before the cap check:

```go
// EnrollForAccount mints an enrollment token for an agent bound to accountID,
// assigning it "<hash>-<username>.<apex>". Enforces the per-account agent cap.
//
// A non-empty boxID makes the call idempotent per box: if (accountID, boxID)
// already has an agent row, the row is kept — same name, base domain, webhook
// secret, quota slot — and only the enrollment token rotates (the old token
// stops authenticating). An empty boxID keeps insert-per-call semantics for
// operator/legacy enrolls.
func (s *Store) EnrollForAccount(accountID, boxID string) (Enrollment, error) {
	// The immediate transaction (see Open) serializes the cap check and insert
	// so concurrent enrollments cannot overshoot the cap.
	tx, err := s.db.Begin()
	if err != nil {
		return Enrollment{}, err
	}
	defer tx.Rollback()

	var username string
	if err := tx.QueryRow(`SELECT username FROM accounts WHERE id=?`, accountID).Scan(&username); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Enrollment{}, ErrUnknownAccount
		}
		return Enrollment{}, err
	}

	if boxID != "" {
		var base, secret string
		err := tx.QueryRow(
			`SELECT base_domain, webhook_secret FROM agents WHERE account_id=? AND box_id=?`,
			accountID, boxID).Scan(&base, &secret)
		if err == nil {
			raw := make([]byte, 32)
			if _, err := rand.Read(raw); err != nil {
				return Enrollment{}, err
			}
			tok := hex.EncodeToString(raw)
			if _, err := tx.Exec(
				`UPDATE agents SET token_hash=? WHERE account_id=? AND box_id=?`,
				hashToken(tok), accountID, boxID); err != nil {
				return Enrollment{}, err
			}
			if err := tx.Commit(); err != nil {
				return Enrollment{}, err
			}
			return Enrollment{Token: tok, BaseDomain: base, WebhookSecret: secret}, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return Enrollment{}, err
		}
	}

	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM agents WHERE account_id=?`, accountID).Scan(&count); err != nil {
		return Enrollment{}, err
	}
	if count >= s.maxAgentsOrDefault() {
		return Enrollment{}, ErrQuotaExceeded
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	for attempt := 0; attempt < 5; attempt++ {
		hash := make([]byte, 4)
		if _, err := rand.Read(hash); err != nil {
			return Enrollment{}, err
		}
		base := hex.EncodeToString(hash) + "-" + username + "." + s.apexOrDefault()

		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return Enrollment{}, err
		}
		tok := hex.EncodeToString(raw)

		rawSecret := make([]byte, 32)
		if _, err := rand.Read(rawSecret); err != nil {
			return Enrollment{}, err
		}
		secret := hex.EncodeToString(rawSecret)

		_, err := tx.Exec(
			`INSERT INTO agents(name, token_hash, base_domain, account_id, box_id, webhook_secret, created_at)
			 VALUES(?,?,?,?,?,?,?)`,
			base, hashToken(tok), base, accountID, nullIfEmpty(boxID), secret, now)
		if err == nil {
			if err := tx.Commit(); err != nil {
				return Enrollment{}, err
			}
			return Enrollment{Token: tok, BaseDomain: base, WebhookSecret: secret}, nil
		}
		if isUniqueViolation(err) {
			continue // hash collided with an existing base_domain; retry
		}
		return Enrollment{}, err
	}
	return Enrollment{}, errors.New("could not assign a unique base domain")
}

// nullIfEmpty stores an absent box_id as NULL, keeping the partial unique
// index's WHERE clause and this column's "no box identity" reading aligned.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
```

- [ ] **Step 5: Fix the existing call sites in the relay package's tests**

Run: `grep -rn "EnrollForAccount(" internal/relay/ | grep -v accounts.go`
Update every existing call from `st.EnrollForAccount(x)` to `st.EnrollForAccount(x, "")` (the api.go handler call site is Task 2's job — for now change it mechanically the same way so the package compiles: `a.st.EnrollForAccount(targetID, "")`).

- [ ] **Step 6: Run the relay tests**

Run: `go test ./internal/relay/ -v`
Expected: PASS, including the four new tests and all pre-existing enroll/org/quota tests.

- [ ] **Step 7: Commit**

```bash
git add internal/relay/schema.sql internal/relay/accounts.go internal/relay/accounts_test.go internal/relay/api.go
git commit -m "feat(relay): EnrollForAccount upserts by (account, box_id), rotating the token"
```

---

### Task 2: Enroll handler parses `box_id`

**Files:**
- Modify: `internal/relay/api.go` (the `enroll` handler, currently lines 366–410 in the extracted numbering)
- Create: `internal/relay/enroll_api_test.go`

**Interfaces:**
- Consumes: `NewAPI(st, v)` test constructor, `MintAccountCredential`, the fake `Verifier` already used by the package's api tests, Task 1's `EnrollForAccount(accountID, boxID)`.
- Produces: `POST /v1/enroll` accepts an optional JSON body `{"box_id": "...", "org": "..."}` (both optional, independently). The response shape is unchanged: `{enrollment_token, base_domain, tunnel_endpoint, webhook_secret, github_app}`.

- [ ] **Step 1: Write the failing test**

Create `internal/relay/enroll_api_test.go`:

```go
package relay

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// enrollOnce POSTs /v1/enroll with the given JSON body and returns the decoded
// 200 response.
func enrollOnce(t *testing.T, h http.Handler, cred string, body map[string]any) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/enroll", &buf)
	req.Header.Set("Authorization", "Bearer "+cred)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("enroll: status %d, body %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestEnrollAPIUpsertsByBoxID(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	acc, _ := st.UpsertAccount("sub-1", "erin")
	cred, err := st.MintAccountCredential(acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	h := NewAPI(st, nil)

	first := enrollOnce(t, h, cred, map[string]any{"box_id": "box-aaaa"})
	second := enrollOnce(t, h, cred, map[string]any{"box_id": "box-aaaa"})

	if first["base_domain"] != second["base_domain"] {
		t.Fatalf("base domain changed: %v -> %v", first["base_domain"], second["base_domain"])
	}
	if first["enrollment_token"] == second["enrollment_token"] {
		t.Fatal("token did not rotate")
	}
}

func TestEnrollAPIWithoutBoxIDMintsFreshDomains(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	acc, _ := st.UpsertAccount("sub-1", "erin")
	cred, err := st.MintAccountCredential(acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	h := NewAPI(st, nil)

	first := enrollOnce(t, h, cred, nil)
	second := enrollOnce(t, h, cred, nil)
	if first["base_domain"] == second["base_domain"] {
		t.Fatal("bodyless enroll must keep insert-per-call semantics")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/relay/ -run 'TestEnrollAPI' -v`
Expected: FAIL — `TestEnrollAPIUpsertsByBoxID` gets two different base domains, because the handler still passes `""` (Task 1 Step 5's mechanical fix) and ignores the body's `box_id`.

- [ ] **Step 3: Parse `box_id` in the handler**

In `internal/relay/api.go`, in the `enroll` handler: extend the body struct and pass the value through.

```go
	// Optional body: {"org":"<slug>"} enrolls the box into an org the caller
	// owns (no/empty body is personal enrollment); {"box_id":"..."} is the
	// durable identity piperd minted on the box, making the enroll an upsert —
	// see EnrollForAccount.
	var req struct {
		Org   string `json:"org"`
		BoxID string `json:"box_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
```

and at the call site:

```go
	en, err := a.st.EnrollForAccount(targetID, req.BoxID)
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/relay/ -v`
Expected: PASS (new file and all pre-existing api/org tests).

- [ ] **Step 5: Commit**

```bash
git add internal/relay/api.go internal/relay/enroll_api_test.go
git commit -m "feat(relay): enroll handler accepts box_id for idempotent claims"
```

---

### Task 3: `relayclient.Enroll` sends `{box_id, org}`

**Files:**
- Modify: `internal/relayclient/relayclient.go` (the `Enroll` method)
- Create: `internal/relayclient/enroll_test.go`
- Modify: `cmd/piper/relayonboard.go` (the `connect` call site, currently `relayclient.New(cc.RelayAPI).Enroll(ctx, cc.AccountCredential)`)
- Modify: every other `.Enroll(` call site grep finds (Step 5)

**Interfaces:**
- Consumes: the existing `Client`, `post`, `Enrollment`, `ErrBadCredential`, `ErrQuotaExceeded` in `internal/relayclient/relayclient.go`.
- Produces: `func (c *Client) Enroll(ctx context.Context, accountCredential, boxID, org string) (Enrollment, error)` — always POSTs a JSON body `{"box_id": boxID, "org": org}` (empty strings included; the handler treats empty as absent). **Plan 2 calls this from piperd with a real boxID; Plan 3 passes `--org` through.** Until then every existing caller passes `"", ""`.

- [ ] **Step 1: Write the failing test**

Create `internal/relayclient/enroll_test.go`:

```go
package relayclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEnrollPostsBoxIDAndOrg(t *testing.T) {
	var got struct {
		BoxID string `json:"box_id"`
		Org   string `json:"org"`
	}
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/enroll" {
			http.NotFound(w, r)
			return
		}
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"enrollment_token": "enr-1", "base_domain": "ab12-erin.public.getpiper.co",
			"tunnel_endpoint": "relay.getpiper.co:7000",
			"webhook_secret":  "whsec-1", "github_app": true,
		})
	}))
	defer srv.Close()

	en, err := New(srv.URL).Enroll(context.Background(), "cred-xyz", "box-aaaa", "acme")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if auth != "Bearer cred-xyz" {
		t.Fatalf("auth = %q", auth)
	}
	if got.BoxID != "box-aaaa" || got.Org != "acme" {
		t.Fatalf("body = %+v, want box-aaaa/acme", got)
	}
	if en.BaseDomain != "ab12-erin.public.getpiper.co" || en.EnrollmentToken != "enr-1" {
		t.Fatalf("enrollment = %+v", en)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/relayclient/ -run TestEnrollPostsBoxIDAndOrg -v`
Expected: compile FAIL — `too many arguments in call to New(srv.URL).Enroll`.

- [ ] **Step 3: Change `Enroll` to send the body**

In `internal/relayclient/relayclient.go`:

```go
// Enroll claims a box for the account behind accountCredential, returning the
// enrollment token, assigned base domain, and tunnel endpoint. A non-empty
// boxID makes the claim idempotent per box (the relay upserts on it); a
// non-empty org claims the box for that org (caller must hold the owner role).
func (c *Client) Enroll(ctx context.Context, accountCredential, boxID, org string) (Enrollment, error) {
	body := map[string]string{"box_id": boxID, "org": org}
	resp, err := c.post(ctx, "/v1/enroll", body, accountCredential)
	if err != nil {
		return Enrollment{}, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var en Enrollment
		if err := json.NewDecoder(resp.Body).Decode(&en); err != nil {
			return Enrollment{}, err
		}
		return en, nil
	case http.StatusUnauthorized:
		return Enrollment{}, ErrBadCredential
	case http.StatusTooManyRequests:
		return Enrollment{}, ErrQuotaExceeded
	default:
		return Enrollment{}, fmt.Errorf("relay enroll: %s", resp.Status)
	}
}
```

- [ ] **Step 4: Run the relayclient tests**

Run: `go test ./internal/relayclient/ -v`
Expected: PASS.

- [ ] **Step 5: Update every call site**

Run: `grep -rn "\.Enroll(" --include="*.go" .` (from the repo root).
For each call site outside `internal/relayclient` (expected: `cmd/piper/relayonboard.go`'s `connect`, plus any e2e/test helpers the grep surfaces), append the two new arguments with empty values, e.g. in `cmd/piper/relayonboard.go`:

```go
	en, err := relayclient.New(cc.RelayAPI).Enroll(ctx, cc.AccountCredential, "", "")
```

No behavior changes: empty `box_id` keeps today's insert semantics, so every pinned `connect` test (`TestConnectEnrollsAndWritesRelayFile`, `TestConnectSystemManagedGuidesEnvInstall`, `TestConnectQuotaExceeded`, …) must still pass untouched.

- [ ] **Step 6: Run the full gate**

Run: `make verify`
Expected: exit 0. If gofmt rewrites anything, `make fmt` and re-run.

- [ ] **Step 7: Commit**

```bash
git add internal/relayclient/relayclient.go internal/relayclient/enroll_test.go cmd/piper/relayonboard.go
git commit -m "feat(relayclient): Enroll carries box_id and org in the request body"
```

(Include any other grep-found call-site files in the `git add`.)

---

### Task 4: PR

- [ ] **Step 1: Re-run the gate and push**

Run: `make verify` — exit 0 required.

```bash
git push -u origin HEAD
```

- [ ] **Step 2: Open the PR**

```bash
gh pr create --base main --title "feat(relay): idempotent enroll upsert keyed by box_id" --body "Relay half of the one-command login design (docs/superpowers/specs/2026-07-31-one-command-login-design.md): POST /v1/enroll with a box_id upserts the agent row per (account, box_id) — token rotates, base domain / webhook secret / quota slot are reused — so repeated or crashed claims cannot strand quota slots. Empty box_id keeps legacy insert semantics for the interim piper connect path.

Closes #<the issue filed for this plan>

🤖 Generated with [Claude Code](https://claude.com/claude-code)"
```

Squash-merge after review per the repo's trunk workflow.
