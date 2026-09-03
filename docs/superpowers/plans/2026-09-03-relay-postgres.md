# Relay on Postgres Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move `piper-relay`'s store from a per-process SQLite file to a shared Postgres database so several relay processes can share one source of truth.

**Architecture:** In-place rewrite of the existing `internal/relay` `Store` on `database/sql`, swapping the `modernc.org/sqlite` driver for `github.com/jackc/pgx/v5/stdlib`. Method signatures and callers stay put; `schema.sql` becomes Postgres DDL; every query gets `$N` placeholders; the concurrency guarantees SQLite's `_txlock=immediate` gave for free are re-established with `SELECT … FOR UPDATE` on the three cap checks and `FOR UPDATE SKIP LOCKED` on the parked-event drain. A small `internal/relay/relaytest` package provisions a throwaway Postgres (Docker-spawned, or `PIPER_TEST_POSTGRES_URL`) for the three test packages that need one, skipping cleanly when neither is available.

**Tech Stack:** Go 1.26, `database/sql`, `github.com/jackc/pgx/v5` (stdlib driver + `pgconn` for error codes — already in `go.mod` as an indirect dependency, promoted to direct), Postgres 17, Docker CLI for the test harness.

**Spec:** [`docs/superpowers/specs/2026-08-04-relay-postgres-design.md`](../specs/2026-08-04-relay-postgres-design.md). Read it first.

## Global Constraints

- `CGO_ENABLED=0` must hold for every build; `make cross` (linux/arm64) must pass. pgx is pure Go.
- `schema.sql` is always the complete current shape, applied with `CREATE TABLE IF NOT EXISTS`. No migrations, no compat shims, no legacy readers (pre-1.x policy).
- Timestamps stay `TEXT` (RFC3339Nano / `pendingTimeLayout`), compared as strings, exactly as today.
- `hostnames.pr` stays `INTEGER` (it is a PR number). Only `accounts.disabled` becomes `BOOLEAN`.
- Foreign keys are enforced by Postgres and are kept. Fix code and fixtures, never drop a `REFERENCES`.
- The agent (`piperd`, `internal/store`) is untouched and stays on SQLite.
- Layering: `internal/relay` knows persistence; `cmd/piper-relay` is wiring; nothing imports "up". `relaytest` must not import `internal/relay` (the relay package's own tests use it — an import would be a cycle).
- `make verify` (gofmt → vet → test → cross) must be green before any commit is called done.
- Commits: conventional-commit style, one per task step group, trailer `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`. Branch is `claude/relay-postgres-migration-fdd140`; never commit to `main`.
- pgx gotchas that recur throughout: (1) `db.Exec(sqlText)` with **no** arguments uses the simple protocol and accepts multiple `;`-separated statements — that is how the schema is applied; (2) a parameter whose type Postgres cannot infer from context (e.g. `SELECT $1, …`) needs an explicit cast like `$1::text`; (3) a Go `[]int64` argument binds as `bigint[]`, so `WHERE id = ANY($1)` works; (4) `Result.LastInsertId()` is unsupported — nothing in the relay uses it.

---

## File structure

**Created**

| Path | Responsibility |
| --- | --- |
| `internal/relay/relaytest/relaytest.go` | Provision one throwaway Postgres server per test process (env DSN, else Docker, else skip); hand out one fresh database per `DSN(t)` call; tear the server down in `Main`. |
| `internal/relay/relaytest/relaytest_test.go` | Proves two `DSN` calls give isolated databases; proves the skip path. |
| `internal/relay/concurrency_test.go` | The three multi-writer tests (enroll cap, app cap, drain-once). |
| `cmd/relay-sqlite-to-pg/main.go` | One-off copier `relay.db` → Postgres for the Hetzner cutover. **Deleted after cutover** in a follow-up PR. |
| `cmd/relay-sqlite-to-pg/main_test.go` | Round-trips a small SQLite fixture into a test Postgres and checks counts. |

**Modified**

| Path | Change |
| --- | --- |
| `internal/relay/schema.sql` | Postgres DDL: `BIGSERIAL id` on five tables, `BYTEA`, `BOOLEAN`. |
| `internal/relay/store.go` | `Open(dsn)`, pgx driver import, `$N` placeholders, `NullBool` scan. |
| `internal/relay/accounts.go` | `$N`, `BOOLEAN` scans, `isUniqueViolation` via `pgconn`, `FOR UPDATE` in `EnrollForAccount`. |
| `internal/relay/hostnames.go` | `$N`, `NullBool` scans, `RegisterHostname` becomes a transaction with `FOR UPDATE`. |
| `internal/relay/domains.go` | `$N`, `FOR UPDATE` on the agent row, `isUniqueViolation`. |
| `internal/relay/orgs.go` | `$N`, `rowid`→`id`, `ON CONFLICT DO NOTHING`, `DeleteOrg` locks + deletes installations. |
| `internal/relay/orginstall.go`, `installations.go`, `bindings.go`, `agents.go` | `$N`, `rowid`→`id`, `::text` casts. |
| `internal/relay/delivery.go` | `$N`, `rowid`→`id`, `FOR UPDATE SKIP LOCKED`, delete-by-id. |
| `internal/relay/*_test.go` | `openTestStore` over the harness, `$N` in raw SQL, `rowid`→`id`, orphan fixtures fixed, one SQLite-only test replaced. |
| `internal/relay/watchdog_test.go` | `TestMain` also wraps `relaytest.Main`. |
| `cmd/piper-relay/main.go`, `main_test.go` | `PIPER_RELAY_DB_URL`; tests use the harness. |
| `test/e2e/relay_test.go`, `login_test.go`, `relay_terminated_test.go` (+ new `main_test.go`) | Spawned relays get `PIPER_RELAY_DB_URL`. |
| `go.mod`, `go.sum` | pgx promoted to direct. |
| `docs/runbooks/relay-deploy.md`, `docs/manual-setup.md`, `PROGRESS.md` | Postgres operations, compose example, cutover. |

---

## Task 1: `relaytest` harness

**Files:**
- Create: `internal/relay/relaytest/relaytest.go`
- Create: `internal/relay/relaytest/relaytest_test.go`

**Interfaces:**
- Produces: `func DSN(t *testing.T) string` — returns a `postgres://` DSN for a fresh, empty database; calls `t.Skip` when no Postgres can be provisioned. `func Main(m *testing.M) int` — wraps `m.Run()` and removes the Docker container afterwards; callers do `os.Exit(relaytest.Main(m))` from their `TestMain`.
- Consumes: nothing from the repo. Imports `github.com/jackc/pgx/v5/stdlib` for the driver.

- [ ] **Step 1: Write the failing test**

```go
// internal/relay/relaytest/relaytest_test.go
package relaytest

import (
	"database/sql"
	"os"
	"testing"
)

func TestMain(m *testing.M) { os.Exit(Main(m)) }

// Two DSN calls must land in different databases: a table created through
// one is invisible through the other.
func TestDSNGivesIsolatedDatabases(t *testing.T) {
	a, b := DSN(t), DSN(t)
	if a == b {
		t.Fatalf("both DSN calls returned %q", a)
	}
	dbA, err := sql.Open("pgx", a)
	if err != nil {
		t.Fatal(err)
	}
	defer dbA.Close()
	if _, err := dbA.Exec(`CREATE TABLE probe(x INT)`); err != nil {
		t.Fatalf("create in a: %v", err)
	}
	dbB, err := sql.Open("pgx", b)
	if err != nil {
		t.Fatal(err)
	}
	defer dbB.Close()
	var n int
	if err := dbB.QueryRow(`SELECT COUNT(*) FROM probe`).Scan(&n); err == nil {
		t.Fatal("table created in database a is visible from database b")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/relay/relaytest/ -run TestDSNGivesIsolatedDatabases -v`
Expected: FAIL to compile — `undefined: DSN`, `undefined: Main`.

- [ ] **Step 3: Write the harness**

```go
// internal/relay/relaytest/relaytest.go

// Package relaytest provisions a throwaway Postgres for tests of the relay
// store and of the binaries that embed it.
//
// One server is provisioned per test process, lazily, on the first DSN call:
// PIPER_TEST_POSTGRES_URL when set (any database on a server whose role may
// CREATE DATABASE), else a postgres:17 container started through the docker
// CLI, else every caller skips — the same Docker-skip convention the runtime
// package uses. Each DSN call creates its own database on that server, so
// tests never see each other's rows and may run in parallel.
package relaytest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const image = "postgres:17"

var (
	once      sync.Once
	adminDSN  string // server-level DSN; "" when nothing could be provisioned
	skipMsg   string // why adminDSN is empty
	container string // docker container id when this process started one
)

// DSN returns a connection string for a fresh, empty database, skipping t
// when no Postgres is reachable. The database is dropped when t finishes.
func DSN(t *testing.T) string {
	t.Helper()
	once.Do(provision)
	if adminDSN == "" {
		t.Skip(skipMsg)
	}
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatal(err)
	}
	name := "relaytest_" + hex.EncodeToString(raw[:])
	admin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err := admin.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Fatalf("relaytest: create database: %v", err)
	}
	t.Cleanup(func() {
		admin, err := sql.Open("pgx", adminDSN)
		if err != nil {
			return
		}
		defer admin.Close()
		// FORCE closes stragglers; Cleanup runs LIFO so a store opened after
		// this call is already closed by the time we get here anyway.
		_, _ = admin.Exec(`DROP DATABASE ` + name + ` WITH (FORCE)`)
	})
	return withDatabase(adminDSN, name)
}

// Main wraps m.Run so a Docker-started server is removed when the package's
// tests finish. Use from a package TestMain: os.Exit(relaytest.Main(m)).
func Main(m *testing.M) int {
	code := m.Run()
	teardown()
	return code
}

func provision() {
	if dsn := os.Getenv("PIPER_TEST_POSTGRES_URL"); dsn != "" {
		adminDSN = dsn
		return
	}
	if _, err := exec.LookPath("docker"); err != nil {
		skipMsg = "postgres unavailable: no docker on PATH and PIPER_TEST_POSTGRES_URL unset"
		return
	}
	out, err := exec.Command("docker", "run", "-d", "--rm",
		"-e", "POSTGRES_PASSWORD=piper",
		"-p", "127.0.0.1:0:5432", image).Output()
	if err != nil {
		skipMsg = fmt.Sprintf("postgres unavailable: docker run %s: %v", image, err)
		return
	}
	container = strings.TrimSpace(string(out))
	portOut, err := exec.Command("docker", "port", container, "5432/tcp").Output()
	if err != nil {
		teardown()
		skipMsg = fmt.Sprintf("postgres unavailable: docker port: %v", err)
		return
	}
	// First line is the IPv4 mapping, e.g. "127.0.0.1:55001".
	hostPort := strings.TrimSpace(strings.SplitN(string(portOut), "\n", 2)[0])
	dsn := "postgres://postgres:piper@" + hostPort + "/postgres?sslmode=disable"

	// The image's entrypoint runs initdb on a socket-only server before the
	// real one listens on TCP, so a successful TCP ping means it is ready.
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if ping(dsn) == nil {
			adminDSN = dsn
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	teardown()
	skipMsg = "postgres unavailable: container never became ready"
}

func ping(dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return db.PingContext(ctx)
}

func teardown() {
	if container != "" {
		_ = exec.Command("docker", "rm", "-f", container).Run()
		container = ""
	}
}

// withDatabase swaps the database name in a postgres:// URL.
func withDatabase(dsn, name string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	u.Path = "/" + name
	return u.String()
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/relay/relaytest/ -v`
Expected: PASS (first run pulls `postgres:17`, allow a minute). If Docker is not running on your machine, expected: `SKIP postgres unavailable: …` — start Docker and rerun; the PASS is required before moving on.

- [ ] **Step 5: Verify the skip path**

Run: `PATH=/usr/bin:/bin go test ./internal/relay/relaytest/ -v` (a PATH without `docker`)
Expected: `--- SKIP: TestDSNGivesIsolatedDatabases … postgres unavailable: no docker on PATH …`

- [ ] **Step 6: Tidy modules and commit**

```bash
go mod tidy
git add internal/relay/relaytest go.mod go.sum
git commit -m "test: relaytest provisions a throwaway Postgres per test process

Part of the relay-on-Postgres migration (spec 2026-08-04). Env DSN, else a
docker-run postgres:17, else skip; one CREATE DATABASE per DSN call.

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

`go mod tidy` moves `github.com/jackc/pgx/v5` from the indirect block to the direct `require` block. Check `git diff go.mod` shows exactly that and nothing else new.

---

## Task 2: Postgres schema, `Open(dsn)`, and test wiring

This task makes `store.go` and `store_test.go` green on Postgres and does every *mechanical* test-side edit in the package (helper calls, `$N` in raw test SQL, `rowid`→`id`). Tests that exercise the other store files stay red until Tasks 3–8 convert them; that is expected and is called out in the verification step.

**Files:**
- Modify: `internal/relay/schema.sql` (whole file)
- Modify: `internal/relay/store.go:6-19, 71-85, 96-109, 113-131, 136-173`
- Modify: `internal/relay/accounts_test.go:10-18` (`openTestStore`), `:237`, `:286`
- Modify: `internal/relay/watchdog_test.go:17-20` (`TestMain`), `:143`
- Modify: `internal/relay/store_test.go` (`Open` sites, delete `TestOpenSetsBusyTimeout`, add `TestOpenIsIdempotent`)
- Modify: `internal/relay/domains_test.go:12`, `hostnames_test.go:14`, `accepttunnels_test.go:17,147`, `server_test.go:23,407,387,615`, `proxy_test.go:449,475,492,510,527`, `orgs_test.go:431,468`, `orgs_api_test.go:398`, `agents_test.go:48,92`, `delivery_test.go:195,502,510`

**Interfaces:**
- Consumes: `relaytest.DSN(t)`, `relaytest.Main(m)` from Task 1.
- Produces: `func Open(dsn string) (*Store, error)`; `openTestStore(t *testing.T) *Store` (already exists — now Postgres-backed); `isUniqueViolation(err error) bool` keeps its name (rewritten in Task 3).

- [ ] **Step 1: Rewrite `openTestStore` and `TestMain`**

In `internal/relay/accounts_test.go` replace lines 10–18 with:

```go
func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(relaytest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}
```

and add `"github.com/piperbox/piper/internal/relay/relaytest"` to that file's imports (drop `"path/filepath"` if it becomes unused).

In `internal/relay/watchdog_test.go` replace the `TestMain` body:

```go
func TestMain(m *testing.M) {
	disabledPollInterval = 20 * time.Millisecond
	os.Exit(relaytest.Main(m))
}
```

and add the `relaytest` import.

- [ ] **Step 2: Replace every direct `Open(filepath.Join(t.TempDir(), "relay.db"))`**

Each of these sites becomes a call to the helper. For a site shaped like

```go
st, err := Open(filepath.Join(t.TempDir(), "relay.db"))
if err != nil {
	t.Fatal(err)
}
defer st.Close()
```

write `st := openTestStore(t)` and delete the `err` check and the `defer`/`t.Cleanup` close (the helper registers the close). Sites: `store_test.go:10, 52, 107`; `domains_test.go:12` (inside `openDomainsStore`); `hostnames_test.go:14` (inside `newAccountAgent`); `accepttunnels_test.go:17, 147`; `server_test.go:23` (inside `startTestRelay`), `:407`. Remove `"path/filepath"` from each file's imports once unused (`go vet` will tell you).

- [ ] **Step 3: Replace the SQLite-only test in `store_test.go`**

Delete `TestOpenSetsBusyTimeout` (lines 35–49) and add in its place:

```go
// Open applies schema.sql with CREATE … IF NOT EXISTS, so a second Open on
// the same database — every relay restart, every extra replica — is a no-op.
func TestOpenIsIdempotent(t *testing.T) {
	dsn := relaytest.DSN(t)
	first, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Enroll("alice", "alice.example.com"); err != nil {
		t.Fatal(err)
	}
	first.Close()
	second, err := Open(dsn)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer second.Close()
	if _, err := second.Authenticate("bogus"); !errors.Is(err, ErrBadToken) {
		t.Fatalf("store not usable after reopen: %v", err)
	}
	var n int
	if err := second.db.QueryRow(`SELECT COUNT(*) FROM agents`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("agents after reopen = %d, %v; want 1 (schema re-apply must not drop rows)", n, err)
	}
}
```

Add the `relaytest` import to `store_test.go`.

- [ ] **Step 4: Convert raw SQL in the test files**

Every `?` in a test's `st.db.Exec`/`QueryRow` becomes a numbered placeholder, and `rowid` becomes `id`. Exact replacements:

- `accounts_test.go:286`: `SELECT COUNT(*) FROM agents WHERE account_id=$1`
- `agents_test.go:15`: `INSERT INTO repo_bindings(agent_name, app, repo, branch, created_at) VALUES($1,$2,$3,$4,$5)`
- `agents_test.go:20-21`: `INSERT INTO pending_events(agent_name, app, ref, event, payload, created_at, attempts, next_try_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`
- `agents_test.go:48, 92`: `SELECT name FROM agents WHERE base_domain=$1`
- `delivery_test.go:195-196` (`pendingRowID`): `SELECT id FROM pending_events WHERE agent_name=$1 AND app=$2 AND ref=$3`
- `delivery_test.go:502`: `UPDATE pending_events SET next_try_at=$1 WHERE agent_name=$2`
- `delivery_test.go:510`: `UPDATE pending_events SET created_at=$1 WHERE agent_name=$2 AND ref=$3`
- `orgs_test.go:188-189` (`addMember`): `INSERT INTO org_members(org_id, account_id, role, created_at) VALUES($1,$2,$3,'2026-01-01T00:00:00Z')`
- `orgs_test.go:431`, `orgs_api_test.go:398`: `DELETE FROM agents WHERE account_id=$1`
- `orgs_test.go:454`: `INSERT INTO hostnames(hostname, agent_name, account_id, app, created_at) VALUES($1,$2,$3,$4,$5)`
- `orgs_test.go:468`: `SELECT COUNT(*) FROM hostnames WHERE account_id=$1`
- `proxy_test.go:449, 475, 492, 510, 527`: `SELECT COUNT(*) FROM agents WHERE base_domain=$1`
- `watchdog_test.go:143`: `DELETE FROM agents WHERE base_domain=$1`

`accounts_test.go:237`, `server_test.go:387`, `server_test.go:615`, `store_test.go:162` have no placeholders and stay as they are. `countRows` in `agents_test.go:26-34` passes its query through — check its callers (`agents_test.go:36-165`) and number any `?` they pass the same way.

- [ ] **Step 5: Write the Postgres schema**

Replace `internal/relay/schema.sql` in full:

```sql
-- Postgres DDL, applied on every start with IF NOT EXISTS. This file is
-- always the complete current shape: a schema change edits it directly
-- (pre-1.x policy, no migrations). Timestamps are TEXT on purpose — RFC3339Nano
-- (or the fixed-width pendingTimeLayout) compared as strings, as the Go code
-- has always done.
--
-- id BIGSERIAL columns stand in for SQLite's rowid where the code orders or
-- dedupes by insertion order; they are never exposed.

CREATE TABLE IF NOT EXISTS agents (
    id             BIGSERIAL,
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
    WHERE box_id IS NOT NULL AND box_id <> '';

-- username is unique per type, not globally: users and orgs hold separate
-- namespaces, so an org can never take a GitHub login out from under the user
-- who owns it (#411). Hostnames stay distinct regardless — appHostname keys its
-- hash on the account id, and the slug is only the human-readable half.
CREATE TABLE IF NOT EXISTS accounts (
    id           TEXT PRIMARY KEY,
    github_id    TEXT UNIQUE,
    github_login TEXT,
    username     TEXT NOT NULL,
    type         TEXT NOT NULL DEFAULT 'user',
    disabled     BOOLEAN NOT NULL DEFAULT false,
    created_at   TEXT NOT NULL,
    UNIQUE(username, type)
);

CREATE TABLE IF NOT EXISTS account_creds (
    token_hash  TEXT PRIMARY KEY,
    account_id  TEXT NOT NULL REFERENCES accounts(id),
    created_at  TEXT NOT NULL
);

-- agent_name attributes each hostname to the box that claimed it, so removing a
-- box reclaims its app slots and two boxes on one account can hold the same app
-- name without colliding (#405). account_id stays because the app cap is still
-- per account, not per box.
CREATE TABLE IF NOT EXISTS hostnames (
    hostname    TEXT PRIMARY KEY,
    agent_name  TEXT NOT NULL REFERENCES agents(name),
    account_id  TEXT NOT NULL REFERENCES accounts(id),
    app         TEXT NOT NULL,
    pr          INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL,
    UNIQUE(agent_name, app, pr)
);

CREATE TABLE IF NOT EXISTS org_members (
    id         BIGSERIAL,
    org_id     TEXT NOT NULL REFERENCES accounts(id),
    account_id TEXT NOT NULL REFERENCES accounts(id),
    role       TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (org_id, account_id)
);

CREATE TABLE IF NOT EXISTS org_invites (
    id           BIGSERIAL,
    org_id       TEXT NOT NULL REFERENCES accounts(id),
    github_login TEXT NOT NULL,
    invited_by   TEXT NOT NULL REFERENCES accounts(id),
    created_at   TEXT NOT NULL,
    PRIMARY KEY (org_id, github_login)
);

CREATE TABLE IF NOT EXISTS custom_domains (
    domain      TEXT PRIMARY KEY,
    agent_base  TEXT NOT NULL,
    status      TEXT NOT NULL,
    created_at  TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS custom_domains_agent_base ON custom_domains(agent_base);

CREATE TABLE IF NOT EXISTS github_installations (
    id              BIGSERIAL,
    installation_id TEXT PRIMARY KEY,
    account_id      TEXT NOT NULL REFERENCES accounts(id),
    target_type     TEXT NOT NULL,
    target_login    TEXT NOT NULL,
    created_at      TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS github_installations_account
    ON github_installations(account_id);

CREATE TABLE IF NOT EXISTS repo_bindings (
    agent_name TEXT NOT NULL REFERENCES agents(name),
    app        TEXT NOT NULL,
    repo       TEXT NOT NULL,
    branch     TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (agent_name, app)
);

CREATE INDEX IF NOT EXISTS repo_bindings_repo ON repo_bindings(repo);

-- attempts counts failed delivery attempts for this slot (a park only happens
-- after one has failed, so it starts at 1); next_try_at is the earliest time
-- the retry sweep may pick the row up, so a permanently-failing box backs off
-- instead of being retried every sweep forever. Both use the fixed-width
-- pendingTimeLayout, so string comparison is chronological.
CREATE TABLE IF NOT EXISTS pending_events (
    id          BIGSERIAL,
    agent_name  TEXT NOT NULL REFERENCES agents(name),
    app         TEXT NOT NULL,
    ref         TEXT NOT NULL,
    event       TEXT NOT NULL,
    payload     BYTEA NOT NULL,
    created_at  TEXT NOT NULL,
    attempts    INTEGER NOT NULL,
    next_try_at TEXT NOT NULL,
    PRIMARY KEY (agent_name, app, ref)
);
```

- [ ] **Step 6: Rewrite `store.go` for Postgres**

Imports: replace `_ "modernc.org/sqlite"` with `_ "github.com/jackc/pgx/v5/stdlib"`.

Replace `Open` (lines 71–85):

```go
// Open connects to the Postgres database at dsn (a postgres:// URL) and
// applies schema.sql. Several relay processes may share one database; the
// store relies on row locks, not on being the only writer.
func Open(dsn string) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect: %w", err)
	}
	// No arguments ⇒ pgx uses the simple protocol, which accepts the
	// multi-statement schema in one round trip.
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db, nowFunc: time.Now}, nil
}
```

Then the queries:

```go
// Enroll
`INSERT INTO agents(name, token_hash, base_domain, created_at) VALUES($1,$2,$3,$4)`

// Authenticate
var disabled sql.NullBool
err := s.db.QueryRow(
	`SELECT ag.name, ag.base_domain, acc.disabled
	   FROM agents ag LEFT JOIN accounts acc ON acc.id = ag.account_id
	  WHERE ag.token_hash = $1`, hashToken(token)).
	Scan(&ag.Name, &ag.BaseDomain, &disabled)
// …
if disabled.Valid && disabled.Bool {
	return Agent{}, ErrBadToken
}

// SetControlToken
`UPDATE agents SET control_token=$1 WHERE base_domain=$2`

// ControlToken
`SELECT control_token FROM agents WHERE base_domain=$1`

// AgentWebhookSecret
`SELECT webhook_secret FROM agents WHERE name=$1`
```

`domainClaimable`'s `SELECT base_domain FROM agents` has no parameters and stays.

- [ ] **Step 7: Verify the store.go tests pass and the rest fail for the expected reason**

Run: `go test ./internal/relay/ -run 'TestEnrollAndAuthenticate|TestOpenIsIdempotent|TestEnrollRejectsDuplicateBaseDomain|TestAddCustomDomainRejectsRelayNamespace|TestOpenCreatesOrgTables' -v`
Expected: PASS for all five.

Run: `go test ./internal/relay/ 2>&1 | grep -c 'syntax error at or near "?"'`
Expected: a non-zero count — every unconverted query in the other files fails with Postgres's placeholder syntax error. That is the work of Tasks 3–8.

Run: `go vet ./internal/relay/...`
Expected: clean (unused imports removed).

- [ ] **Step 8: Commit**

```bash
git add internal/relay/schema.sql internal/relay/store.go internal/relay/*_test.go
git commit -m "feat(relay): open the store on Postgres; test wiring over relaytest

schema.sql is Postgres DDL (BIGSERIAL ids for the rowid users, BYTEA,
BOOLEAN disabled). Open takes a DSN. Test helpers and raw test SQL are
converted; store files other than store.go still carry ? placeholders
and are converted file by file next.

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

## Task 3: `accounts.go` — placeholders, booleans, unique-violation detection, enroll-cap lock

**Files:**
- Modify: `internal/relay/accounts.go`
- Test: `internal/relay/accounts_test.go` (existing tests), `internal/relay/concurrency_test.go` (new)

**Interfaces:**
- Produces: `isUniqueViolation(err error) bool` — true for Postgres SQLSTATE `23505`. Used by `orgs.go` (Task 6) and `domains.go` (Task 5).

- [ ] **Step 1: Write the failing concurrency test**

```go
// internal/relay/concurrency_test.go
package relay

import (
	"errors"
	"sync"
	"testing"
)

// Under SQLite every write transaction took a global lock, so a cap check
// and its insert could never interleave. On Postgres that guarantee comes
// from SELECT … FOR UPDATE on the owning row; these tests exist to fail the
// moment it is removed.

func TestEnrollForAccountCapHoldsUnderConcurrency(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	acc, err := st.UpsertAccount("gh-race", "race")
	if err != nil {
		t.Fatal(err)
	}
	const callers = 8
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := st.EnrollForAccount(acc.ID, "")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	ok := 0
	for err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrQuotaExceeded):
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 3 {
		t.Fatalf("%d concurrent enrolls succeeded against a cap of 3", ok)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/relay/ -run TestEnrollForAccountCapHoldsUnderConcurrency -v`
Expected: FAIL with `syntax error at or near "?"` (the file is unconverted).

- [ ] **Step 3: Convert `accounts.go`**

Imports: add `"github.com/jackc/pgx/v5/pgconn"`; keep `strings` (still used by `deriveUsername`).

`UpsertAccount` (lines 53–98):

```go
	var acc Account
	var disabled bool
	var storedLogin sql.NullString
	err := s.db.QueryRow(
		`SELECT id, username, disabled, github_login FROM accounts WHERE github_id=$1`, githubID).
		Scan(&acc.ID, &acc.Username, &disabled, &storedLogin)
	if err == nil {
		acc.Disabled = disabled
		acc.GithubLogin = login
		if storedLogin.String != login {
			// GitHub logins can be renamed; keep the invite-matching login fresh.
			if _, err := s.db.Exec(`UPDATE accounts SET github_login=$1 WHERE id=$2`, login, acc.ID); err != nil {
				return Account{}, err
			}
		}
		return acc, nil
	}
	// … unchanged …
		_, err := s.db.Exec(
			`INSERT INTO accounts(id, github_id, github_login, username, type, disabled, created_at)
			 VALUES($1,$2,$3,$4,'user',false,$5)`,
			id, githubID, login, username, now)
```

`isUniqueViolation` (lines 100–103):

```go
// isUniqueViolation reports whether err is a Postgres unique-constraint
// failure (SQLSTATE 23505) — a primary key, UNIQUE column, or unique index.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
```

`MintAccountCredential`: `SELECT type FROM accounts WHERE id=$1` and `INSERT INTO account_creds(token_hash, account_id, created_at) VALUES($1,$2,$3)`.

`AuthenticateAccount` (lines 142–162):

```go
	var acc Account
	var disabled bool
	var gl, gid sql.NullString
	err := s.db.QueryRow(
		`SELECT a.id, a.username, a.github_id, a.github_login, a.disabled
		   FROM account_creds c JOIN accounts a ON a.id = c.account_id
		  WHERE c.token_hash = $1`, hashToken(cred)).
		Scan(&acc.ID, &acc.Username, &gid, &gl, &disabled)
	// …
	if disabled {
		return Account{}, ErrBadCredential
	}
```

`DisableAccount`: `UPDATE accounts SET disabled=true WHERE username=$1 AND type=$2`.

`EnrollForAccount` (lines 208–295) — replace the opening comment and the username lookup; the rest only changes placeholders:

```go
func (s *Store) EnrollForAccount(accountID, boxID string) (Enrollment, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Enrollment{}, err
	}
	defer tx.Rollback()

	// Locking the account row serializes the cap check and the insert across
	// every relay process sharing this database: a second enroll for the same
	// account waits here until this one commits, then counts the new row.
	var username string
	if err := tx.QueryRow(`SELECT username FROM accounts WHERE id=$1 FOR UPDATE`, accountID).Scan(&username); err != nil {
		// … unchanged …
	}

	if boxID != "" {
		var base, secret string
		err := tx.QueryRow(
			`SELECT base_domain, webhook_secret FROM agents WHERE account_id=$1 AND box_id=$2`,
			accountID, boxID).Scan(&base, &secret)
		if err == nil {
			// …
			if _, err := tx.Exec(
				`UPDATE agents SET token_hash=$1 WHERE account_id=$2 AND box_id=$3`,
				hashToken(tok), accountID, boxID); err != nil {
			// …
	}

	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM agents WHERE account_id=$1`, accountID).Scan(&count); err != nil {
	// …
		_, err := tx.Exec(
			`INSERT INTO agents(name, token_hash, base_domain, account_id, box_id, webhook_secret, created_at)
			 VALUES($1,$2,$3,$4,$5,$6,$7)`,
			base, hashToken(tok), base, accountID, nullIfEmpty(boxID), secret, now)
```

One subtlety on the retry loop at the end: on Postgres, a statement that fails inside a transaction (the unique-violation branch that `continue`s) **aborts the transaction** — every later statement fails with `current transaction is aborted`. The loop must use a savepoint per attempt:

```go
	for attempt := 0; attempt < 5; attempt++ {
		// … mint base/tok/secret as before …
		if _, err := tx.Exec(`SAVEPOINT try`); err != nil {
			return Enrollment{}, err
		}
		_, err := tx.Exec(
			`INSERT INTO agents(name, token_hash, base_domain, account_id, box_id, webhook_secret, created_at)
			 VALUES($1,$2,$3,$4,$5,$6,$7)`,
			base, hashToken(tok), base, accountID, nullIfEmpty(boxID), secret, now)
		if err == nil {
			if err := tx.Commit(); err != nil {
				return Enrollment{}, err
			}
			return Enrollment{Token: tok, BaseDomain: base, WebhookSecret: secret}, nil
		}
		if isUniqueViolation(err) {
			// A failed statement aborts a Postgres transaction; roll back to
			// the savepoint so the next attempt's INSERT can run.
			if _, err := tx.Exec(`ROLLBACK TO SAVEPOINT try`); err != nil {
				return Enrollment{}, err
			}
			continue // hash collided with an existing base_domain; retry
		}
		return Enrollment{}, err
	}
```

`UpsertAccount`'s retry loop runs in autocommit (no transaction), so it needs no savepoint.

- [ ] **Step 4: Run the accounts tests and the concurrency test**

Run: `go test ./internal/relay/ -run 'TestUpsertAccount|TestMintAndAuthenticate|TestDisable|TestEnrollForAccount|TestAuthenticateRejectsDisabled' -v`
Expected: PASS for every test in that set, including `TestEnrollForAccountCapHoldsUnderConcurrency`.

- [ ] **Step 5: Commit**

```bash
git add internal/relay/accounts.go internal/relay/concurrency_test.go
git commit -m "feat(relay): accounts on Postgres — FOR UPDATE serializes the agent cap

isUniqueViolation reads SQLSTATE 23505 through pgconn; the base-domain
retry loop uses a savepoint because a failed INSERT aborts a Postgres
transaction. Adds the concurrent-enroll cap test.

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

## Task 4: `hostnames.go` — `RegisterHostname` becomes a locked transaction

**Files:**
- Modify: `internal/relay/hostnames.go:48-94, 104-137, 169, 194-196, 227`
- Test: `internal/relay/hostnames_test.go` (existing), `internal/relay/concurrency_test.go`

- [ ] **Step 1: Write the failing concurrency test**

Append to `internal/relay/concurrency_test.go`:

```go
// RegisterHostname had no transaction at all on SQLite, so this guarantee is
// new, not preserved: two boxes on one account registering at once must not
// overshoot the app cap.
func TestRegisterHostnameCapHoldsUnderConcurrency(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 2, 5)
	acc, err := st.UpsertAccount("gh-apps", "apps")
	if err != nil {
		t.Fatal(err)
	}
	en, err := st.EnrollForAccount(acc.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	const callers = 8
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := st.RegisterHostname(en.BaseDomain, fmt.Sprintf("app%d", i), 0)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	ok := 0
	for err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrQuotaExceeded):
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 2 {
		t.Fatalf("%d concurrent registrations succeeded against a cap of 2", ok)
	}
}
```

Add `"fmt"` to that file's imports.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/relay/ -run TestRegisterHostnameCapHoldsUnderConcurrency -v`
Expected: FAIL with `syntax error at or near "?"`.

- [ ] **Step 3: Convert `hostnames.go`**

`AgentAccount` (lines 48–65) and `AgentDisabled` (81–94): `WHERE ag.base_domain = $1`, `var disabled sql.NullBool`, and the checks become `disabled.Valid && disabled.Bool`.

`RegisterHostname` (lines 104–137), whole body:

```go
func (s *Store) RegisterHostname(baseDomain, app string, pr int) (string, error) {
	accountID, username, agentName, err := s.AgentAccount(baseDomain)
	if err != nil {
		return "", err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	// The cap is per account, so the account row is the lock: two boxes on
	// one account registering at once count in sequence, on any relay.
	var one int
	if err := tx.QueryRow(`SELECT 1 FROM accounts WHERE id=$1 FOR UPDATE`, accountID).Scan(&one); err != nil {
		return "", err
	}

	var existing string
	err = tx.QueryRow(`SELECT hostname FROM hostnames WHERE agent_name=$1 AND app=$2 AND pr=$3`, agentName, app, pr).Scan(&existing)
	if err == nil {
		return existing, nil // idempotent; the deferred rollback releases the lock
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}

	if pr == 0 {
		var count int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM hostnames WHERE account_id=$1 AND pr=0`, accountID).Scan(&count); err != nil {
			return "", err
		}
		if count >= s.maxAppsOrDefault() {
			return "", ErrQuotaExceeded
		}
	}

	hostname := appHostname(agentName, app, username, s.apexOrDefault(), pr)
	if _, err := tx.Exec(
		`INSERT INTO hostnames(hostname, agent_name, account_id, app, pr, created_at) VALUES($1,$2,$3,$4,$5,$6)`,
		hostname, agentName, accountID, app, pr, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return hostname, nil
}
```

`ReconcileHostnames`: `SELECT hostname, app, pr FROM hostnames WHERE agent_name=$1` and `DELETE FROM hostnames WHERE agent_name=$1 AND app=$2 AND pr=$3`.
`DeregisterHostname`: `DELETE FROM hostnames WHERE agent_name=$1 AND hostname=$2`.

- [ ] **Step 4: Run the hostname tests**

Run: `go test ./internal/relay/ -run 'TestRegister|TestPreview|TestAgentDisabled|TestDeregister|TestReconcile|TestDeleteAgentReclaimsItsAppSlots' -v`
Expected: PASS for all, including `TestRegisterHostnameCapHoldsUnderConcurrency`. (`TestDeleteAgentReclaimsItsAppSlots` may still fail on `agents.go`'s placeholders — that is Task 7; everything else in the set must pass.)

- [ ] **Step 5: Commit**

```bash
git add internal/relay/hostnames.go internal/relay/concurrency_test.go
git commit -m "feat(relay): hostnames on Postgres — RegisterHostname locks the account row

The app-cap check ran outside any transaction before; it is now a
transaction with SELECT … FOR UPDATE on accounts, with a concurrency test.

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

## Task 5: `domains.go` — placeholders and the agent-row lock

**Files:**
- Modify: `internal/relay/domains.go:3-8, 65-116, 120-123, 145-147, 172-174, 203-204`
- Test: `internal/relay/domains_test.go`, `internal/relay/store_test.go` (`TestAddCustomDomainRejectsRelayNamespace`)

- [ ] **Step 1: Run the domain tests to see them fail**

Run: `go test ./internal/relay/ -run 'Domain' -v`
Expected: FAIL with `syntax error at or near "?"` on every `AddCustomDomain` call.

- [ ] **Step 2: Convert `domains.go`**

Remove `"strings"` from the imports (its only use was the error-string check).

`AddCustomDomain` (lines 65–116):

```go
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The per-agent domain cap is enforced under the agent row's lock so two
	// claims for one agent, on any relay, count in sequence.
	var one int
	if err := tx.QueryRow(`SELECT 1 FROM agents WHERE base_domain=$1 FOR UPDATE`, baseDomain).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrBadToken
		}
		return err
	}
	var owner, status, created string
	err = tx.QueryRow(
		`SELECT agent_base, status, created_at FROM custom_domains WHERE domain=$1`, domain).
		Scan(&owner, &status, &created)
	switch {
	case err == nil && owner == baseDomain:
		if status == "pending" {
			if _, err := tx.Exec(`UPDATE custom_domains SET created_at=$1 WHERE domain=$2`,
				now.Format(time.RFC3339Nano), domain); err != nil {
				return err
			}
		}
		return tx.Commit() // own active row: no-op re-add
	case err == nil:
		if liveAt(status, created, now) {
			return ErrDomainTaken
		}
		// Expired pending claim by another agent: evict and claim below.
		if _, err := tx.Exec(`DELETE FROM custom_domains WHERE domain=$1`, domain); err != nil {
			return err
		}
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	live, err := countLive(tx, baseDomain, now)
	if err != nil {
		return err
	}
	if live >= s.maxDomainsOrDefault() {
		return ErrQuotaExceeded
	}
	if _, err := tx.Exec(
		`INSERT INTO custom_domains(domain, agent_base, status, created_at) VALUES($1, $2, 'pending', $3)`,
		domain, baseDomain, now.Format(time.RFC3339Nano)); err != nil {
		if isUniqueViolation(err) {
			return ErrDomainTaken // PK backstop: lost the FCFS race
		}
		return err
	}
	return tx.Commit()
```

`countLive`: `SELECT status, created_at FROM custom_domains WHERE agent_base=$1`.
`CustomDomains`: `SELECT domain, status, created_at FROM custom_domains WHERE agent_base=$1 ORDER BY domain`.
`ConfirmCustomDomain`: `UPDATE custom_domains SET status='active' WHERE domain=$1 AND agent_base=$2`.
`removeCustomDomainOwned`: `DELETE FROM custom_domains WHERE domain=$1 AND agent_base=$2`.

- [ ] **Step 3: Run the domain tests**

Run: `go test ./internal/relay/ -run 'Domain' -v`
Expected: PASS for every test whose name contains `Domain` in `domains_test.go` and `store_test.go`. (`TestDeleteAgentClearsItsCustomDomains` in `agents_test.go` waits for Task 7.)

- [ ] **Step 4: Commit**

```bash
git add internal/relay/domains.go
git commit -m "feat(relay): custom domains on Postgres — cap check under the agent row lock

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

## Task 6: `orgs.go` + `orginstall.go` — ordering by `id`, `ON CONFLICT`, `DeleteOrg` under FK enforcement

**Files:**
- Modify: `internal/relay/orgs.go` (every query), `internal/relay/orginstall.go:15-17, 37-41, 49-52, 59-62`
- Modify: `internal/relay/orgs_test.go:450-475` (`TestDeleteOrgRefusesNonOrgAccounts` fixture)
- Test: `internal/relay/orgs_test.go`, `orgs_api_test.go`, `orginstall_test.go`

- [ ] **Step 1: Write the failing FK test for `DeleteOrg`**

Append to `internal/relay/orgs_test.go`:

```go
// An org-target App installation is linked to the org account
// (ingress routes it through OrgForGitHubInstall). Postgres enforces the
// github_installations.account_id foreign key, so DeleteOrg must remove
// those rows or the account delete fails.
func TestDeleteOrgRemovesItsInstallations(t *testing.T) {
	st := openTestStore(t)
	alice, _ := st.UpsertAccount("gh-alice", "alice")
	org, err := st.CreateOrg(alice.ID, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.LinkInstallationForAccount("inst-9", org.ID, "org", "acme"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteOrg(org.ID); err != nil {
		t.Fatalf("DeleteOrg with an installation: %v", err)
	}
	if _, err := st.AccountForInstallation("inst-9"); !errors.Is(err, ErrNoInstallation) {
		t.Fatalf("installation survived org delete: %v", err)
	}
}
```

- [ ] **Step 2: Fix the orphan fixture**

In `TestDeleteOrgRefusesNonOrgAccounts` (`orgs_test.go:450-475`) the hostname row names an agent `alice-box` that was never enrolled. Insert the parent first — add, immediately after `alice, _ := st.UpsertAccount(...)`:

```go
	if _, err := st.Enroll("alice-box", "alice-box.example.com"); err != nil {
		t.Fatal(err)
	}
```

- [ ] **Step 3: Run to verify failure**

Run: `go test ./internal/relay/ -run 'TestDeleteOrg' -v`
Expected: FAIL with `syntax error at or near "?"`.

- [ ] **Step 4: Convert `orgs.go`**

```go
// CreateOrg
`SELECT type FROM accounts WHERE id=$1`
`INSERT INTO accounts(id, github_id, github_login, username, type, disabled, created_at)
 VALUES($1,NULL,NULL,$2,'org',false,$3)`
`INSERT INTO org_members(org_id, account_id, role, created_at) VALUES($1,$2,'owner',$3)`

// OrgsForAccount
`SELECT o.id, o.username, m.role
   FROM org_members m JOIN accounts o ON o.id = m.org_id
  WHERE m.account_id = $1 ORDER BY m.id`

// OrgRole
`SELECT o.id, m.role
   FROM accounts o JOIN org_members m ON m.org_id = o.id AND m.account_id = $1
  WHERE o.username = $2 AND o.type = 'org'`

// Members
`SELECT a.username, m.role
   FROM org_members m JOIN accounts a ON a.id = m.account_id
  WHERE m.org_id = $1 ORDER BY m.id`

// memberForUpdate
`SELECT a.id, m.role
   FROM org_members m JOIN accounts a ON a.id = m.account_id
  WHERE m.org_id = $1 AND a.username = $2`
`SELECT COUNT(*) FROM org_members WHERE org_id = $1 AND role = 'owner'`

// SetMemberRole
`UPDATE org_members SET role=$1 WHERE org_id=$2 AND account_id=$3`

// RemoveMember
`DELETE FROM org_members WHERE org_id=$1 AND account_id=$2`

// CreateInvite
`SELECT COUNT(*) FROM org_members m JOIN accounts a ON a.id = m.account_id
  WHERE m.org_id = $1 AND lower(a.github_login) = $2`
`INSERT INTO org_invites(org_id, github_login, invited_by, created_at) VALUES($1,$2,$3,$4)`

// OrgInvites
`SELECT github_login FROM org_invites WHERE org_id = $1 ORDER BY id`

// RevokeInvite
`DELETE FROM org_invites WHERE org_id = $1 AND github_login = $2`

// InvitesForAccount
`SELECT github_login FROM accounts WHERE id = $1`
`SELECT o.username FROM org_invites i JOIN accounts o ON o.id = i.org_id
  WHERE i.github_login = $1 ORDER BY i.id`

// takeInvite
`SELECT id FROM accounts WHERE username = $1 AND type = 'org'`
`SELECT github_login FROM accounts WHERE id = $1`
`DELETE FROM org_invites WHERE org_id = $1 AND github_login = $2`

// AcceptInvite — INSERT OR IGNORE has no Postgres spelling
`INSERT INTO org_members(org_id, account_id, role, created_at) VALUES($1,$2,'member',$3)
 ON CONFLICT (org_id, account_id) DO NOTHING`

// CanControl
`SELECT COUNT(*) FROM org_members WHERE org_id = $1 AND account_id = $2`

// CanManage
`SELECT COUNT(*) FROM org_members WHERE org_id = $1 AND account_id = $2 AND role = 'owner'`

// AgentsVisibleTo
`SELECT ag.base_domain, ag.name, acc.username
   FROM agents ag JOIN accounts acc ON acc.id = ag.account_id
  WHERE ag.account_id = $1
     OR ag.account_id IN (SELECT org_id FROM org_members WHERE account_id = $2)
  ORDER BY ag.id`
```

`DeleteOrg` (lines 440–475): lock the account row so the "owns no agents" check cannot race an `EnrollForAccount` (which locks the same row), and delete the installations:

```go
	var otype string
	err = tx.QueryRow(`SELECT type FROM accounts WHERE id = $1 FOR UPDATE`, orgID).Scan(&otype)
	// … unchanged checks …
	var agents int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM agents WHERE account_id = $1`, orgID).Scan(&agents); err != nil {
		return err
	}
	if agents > 0 {
		return ErrOrgHasAgents
	}
	// Every table that REFERENCES accounts(id) for this org, then the row.
	for _, stmt := range []string{
		`DELETE FROM org_invites WHERE org_id = $1`,
		`DELETE FROM org_members WHERE org_id = $1`,
		`DELETE FROM hostnames WHERE account_id = $1`,
		`DELETE FROM github_installations WHERE account_id = $1`,
		`DELETE FROM accounts WHERE id = $1 AND type = 'org'`,
	} {
```

Update the `DeleteOrg` doc comment's list to include "GitHub App installations".

- [ ] **Step 5: Convert `orginstall.go`**

```go
// SetOrgGitHub
`UPDATE accounts SET github_login=$1 WHERE id=$2 AND type='org'`

// OrgForGitHubInstall
`SELECT id FROM accounts
  WHERE type='org' AND (github_id=$1 OR lower(github_login)=$2)
  ORDER BY (github_id=$3) DESC LIMIT 1`
`SELECT COUNT(*) FROM org_members m JOIN accounts a ON a.id=m.account_id
  WHERE m.org_id=$1 AND a.github_id=$2`
`UPDATE accounts SET github_id=$1 WHERE id=$2 AND (github_id IS NULL OR github_id='')`
```

The argument lists are unchanged (`orgGitHubID, login, orgGitHubID` for the first).

- [ ] **Step 6: Run the org tests**

Run: `go test ./internal/relay/ -run 'Org|Member|Invite|CanControl|CanManage|AgentsVisibleTo' -v`
Expected: PASS for all, including `TestDeleteOrgRemovesItsInstallations` and `TestDeleteOrgRefusesNonOrgAccounts`. If a test fails with `violates foreign key constraint`, its fixture inserts a child before its parent — fix the fixture the way Step 2 did (create the parent via the store API), never by removing the constraint.

- [ ] **Step 7: Commit**

```bash
git add internal/relay/orgs.go internal/relay/orginstall.go internal/relay/orgs_test.go
git commit -m "feat(relay): orgs on Postgres — DeleteOrg deletes installations under FK enforcement

SQLite never enforced REFERENCES; Postgres does. DeleteOrg also locks the
account row so its no-agents check cannot race an enroll.

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

## Task 7: `installations.go`, `bindings.go`, `agents.go`

**Files:**
- Modify: `internal/relay/installations.go:23, 37-46, 63-69, 100-104, 182, 190, 214-215, 226-229, 239-243`
- Modify: `internal/relay/bindings.go:23-27, 35, 43-46, 69-71`
- Modify: `internal/relay/agents.go:41, 48-56`
- Test: `installations_test.go`, `bindings_test.go`, `agents_test.go`, `ghtoken_test.go`

- [ ] **Step 1: Run their tests to see them fail**

Run: `go test ./internal/relay/ -run 'Installation|Bind|DeleteAgent|GitHubToken' -v`
Expected: FAIL with `syntax error at or near "?"`.

- [ ] **Step 2: Convert `installations.go`**

```go
// LinkInstallation
`SELECT id FROM accounts WHERE github_id=$1`

// LinkInstallationForAccount
`INSERT INTO github_installations(installation_id, account_id, target_type, target_login, created_at)
 VALUES($1,$2,$3,$4,$5)
 ON CONFLICT(installation_id) DO UPDATE SET
     account_id   = excluded.account_id,
     target_type  = excluded.target_type,
     target_login = excluded.target_login`

// LinkInstallationIfAbsent — parameters in a bare SELECT list have no
// column to infer a type from, so each carries an explicit cast.
`INSERT INTO github_installations(installation_id, account_id, target_type, target_login, created_at)
 SELECT $1::text, id, $2::text, $3::text, $4::text FROM accounts WHERE github_id=$5
 ON CONFLICT(installation_id) DO NOTHING`

// LinkInstallationForAccountIfAbsent
`INSERT INTO github_installations(installation_id, account_id, target_type, target_login, created_at)
 VALUES($1,$2,$3,$4,$5)
 ON CONFLICT(installation_id) DO NOTHING`

// UnlinkInstallation
`DELETE FROM github_installations WHERE installation_id=$1`

// AccountForInstallation
`SELECT account_id FROM github_installations WHERE installation_id=$1`

// InstallationsForAccount
`SELECT installation_id, target_type, target_login FROM github_installations
  WHERE account_id=$1 ORDER BY created_at DESC, id DESC`

// InstallationsVisibleTo
`SELECT installation_id, target_type, target_login FROM github_installations
  WHERE account_id = $1
     OR account_id IN (SELECT org_id FROM org_members WHERE account_id = $2)
  ORDER BY created_at DESC, id DESC`

// InstallationVisibleTo
`SELECT COUNT(*) FROM github_installations
  WHERE installation_id = $1
    AND (account_id = $2
         OR account_id IN (SELECT org_id FROM org_members WHERE account_id = $3))`
```

Any other `?` in `installations.go` (there are queries between lines 105 and 178 not listed here — `grep -n '?' internal/relay/installations.go` finds them) gets the same positional treatment.

- [ ] **Step 3: Convert `bindings.go`**

```go
// BindRepo
`INSERT INTO repo_bindings(agent_name, app, repo, branch, created_at)
 VALUES($1,$2,$3,$4,$5)
 ON CONFLICT(agent_name, app) DO UPDATE SET
     repo = excluded.repo, branch = excluded.branch`

// UnbindRepo
`DELETE FROM repo_bindings WHERE agent_name=$1 AND app=$2`

// BindingsForRepo
`SELECT b.agent_name, b.app, b.repo, b.branch
   FROM repo_bindings b JOIN agents a ON a.name = b.agent_name
  WHERE b.repo = $1 AND a.account_id = $2`

// AgentBoundToRepo
`SELECT COUNT(*) FROM repo_bindings WHERE agent_name=$1 AND repo=$2`
```

- [ ] **Step 4: Convert `agents.go`**

```go
	err = tx.QueryRow(`SELECT name FROM agents WHERE base_domain = $1`, baseDomain).Scan(&name)
	// …
	if _, err := tx.Exec(`DELETE FROM custom_domains WHERE agent_base = $1`, baseDomain); err != nil {
		return err
	}
	for _, stmt := range []string{
		`DELETE FROM hostnames WHERE agent_name = $1`,
		`DELETE FROM pending_events WHERE agent_name = $1`,
		`DELETE FROM repo_bindings WHERE agent_name = $1`,
		`DELETE FROM agents WHERE name = $1`,
	} {
```

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/relay/ -run 'Installation|Bind|DeleteAgent|GitHubToken|OrgInstall' -v`
Expected: PASS for all.

- [ ] **Step 6: Commit**

```bash
git add internal/relay/installations.go internal/relay/bindings.go internal/relay/agents.go
git commit -m "feat(relay): installations, bindings, agent deletion on Postgres

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

## Task 8: `delivery.go` — drain claims rows with `FOR UPDATE SKIP LOCKED`

**Files:**
- Modify: `internal/relay/delivery.go:296-452`
- Test: `internal/relay/delivery_test.go` (existing), `internal/relay/concurrency_test.go`

- [ ] **Step 1: Write the failing drain-once test**

Append to `internal/relay/concurrency_test.go`:

```go
// Two drains of the same agent — the reconnect race, where an agent moves
// between relays while a sweep is in flight — must hand each parked event to
// exactly one of them. Under read committed without a row lock both SELECTs
// see the row before either DELETE commits and the webhook goes out twice.
func TestDrainEventsDeliversEachEventOnce(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	acc, err := st.UpsertAccount("gh-drain", "drain")
	if err != nil {
		t.Fatal(err)
	}
	en, err := st.EnrollForAccount(acc.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	agent := en.BaseDomain // EnrollForAccount names the agent by its base domain
	for i := 0; i < 5; i++ {
		if err := st.ParkEvent(agent, fmt.Sprintf("app%d", i), "main", "push", []byte("{}")); err != nil {
			t.Fatal(err)
		}
	}
	const drainers = 6
	got := make(chan int, drainers)
	var wg sync.WaitGroup
	for i := 0; i < drainers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			evs, err := st.DrainEvents(agent)
			if err != nil {
				t.Error(err)
			}
			got <- len(evs)
		}()
	}
	wg.Wait()
	close(got)
	total := 0
	for n := range got {
		total += n
	}
	if total != 5 {
		t.Fatalf("%d events returned across concurrent drains, want exactly 5", total)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/relay/ -run TestDrainEventsDeliversEachEventOnce -v`
Expected: FAIL with `syntax error at or near "?"`.

- [ ] **Step 3: Convert `delivery.go`**

`ParkEvent` (lines 300–306):

```go
	if _, err := s.db.Exec(
		`INSERT INTO pending_events(agent_name, app, ref, event, payload, created_at, attempts, next_try_at)
		 VALUES($1,$2,$3,$4,$5,$6,1,$7)
		 ON CONFLICT(agent_name, app, ref) DO UPDATE SET
		     event = excluded.event, payload = excluded.payload, created_at = excluded.created_at,
		     attempts = 1, next_try_at = excluded.next_try_at`,
		agentName, app, ref, event, payload, stamp, next); err != nil {
```

`evictOldestPending` (lines 318–327):

```go
	_, err := s.db.Exec(
		`DELETE FROM pending_events
		  WHERE agent_name = $1
		    AND id NOT IN (
		        SELECT id FROM pending_events WHERE agent_name = $2
		         ORDER BY created_at DESC LIMIT $3)`,
		agentName, agentName, maxPendingPerAgent)
```

`ReparkEvent` (lines 342–349):

```go
	if _, err := s.db.Exec(
		`INSERT INTO pending_events(agent_name, app, ref, event, payload, created_at, attempts, next_try_at)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8)
		 ON CONFLICT(agent_name, app, ref) DO UPDATE SET
		     event = excluded.event, payload = excluded.payload, created_at = excluded.created_at,
		     attempts = excluded.attempts, next_try_at = excluded.next_try_at
		 WHERE excluded.created_at > pending_events.created_at`,
		agentName, app, ref, event, payload, createdAt, attempts, next); err != nil {
```

Replace the `DrainEvents` doc comment (lines 360–367) and `drainEvents` (379–420):

```go
// DrainEvents returns and removes every parked event for agentName. The read
// locks the rows it returns (FOR UPDATE SKIP LOCKED) and the delete names
// exactly those rows, so two drains of one agent — on one relay or two — split
// the events between them and never both deliver the same one: a row the
// other drain holds is skipped here and returned there. A concurrent
// ParkEvent either commits before the read and is returned, or after the
// delete and survives for the next drain.
// Events older than pendingEventTTL are deleted with the rest but never
// returned: a box gone for a week should not have a week-old push replayed at
// it, and the delete is what reclaims its capped slots.
func (s *Store) DrainEvents(agentName string) ([]PendingEvent, error) {
	return s.drainEvents(agentName, "")
}
```

```go
// drainEvents reads, locks, and deletes agentName's parked events in one
// transaction. dueBy empty means every event regardless of backoff.
func (s *Store) drainEvents(agentName, dueBy string) ([]PendingEvent, error) {
	where, args := `agent_name=$1`, []any{agentName}
	if dueBy != "" {
		where += ` AND next_try_at<=$2`
		args = append(args, dueBy)
	}
	expiry := time.Now().UTC().Add(-pendingEventTTL).Format(pendingTimeLayout)

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(
		`SELECT id, app, ref, event, payload, created_at, attempts FROM pending_events
		  WHERE `+where+` ORDER BY created_at FOR UPDATE SKIP LOCKED`, args...)
	if err != nil {
		return nil, err
	}
	var out []PendingEvent
	var ids []int64
	for rows.Next() {
		var id int64
		ev := PendingEvent{AgentName: agentName}
		if err := rows.Scan(&id, &ev.App, &ev.Ref, &ev.Event, &ev.Payload, &ev.CreatedAt, &ev.Attempts); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
		if ev.CreatedAt < expiry {
			continue // deleted below with the rest, but too stale to replay
		}
		out = append(out, ev)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, tx.Commit()
	}
	if _, err := tx.Exec(`DELETE FROM pending_events WHERE id = ANY($1)`, ids); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}
```

`PurgeExpiredPending`: `DELETE FROM pending_events WHERE created_at < $1`.
`AgentsWithDuePending`: `SELECT DISTINCT agent_name FROM pending_events WHERE next_try_at<=$1`.

- [ ] **Step 4: Run the delivery tests**

Run: `go test ./internal/relay/ -run 'Deliver|Drain|Sweep|Backoff|Pending|Dispatch|Shutdown' -v`
Expected: PASS for all, including `TestDrainEventsDeliversEachEventOnce` and the existing `TestDrainForReplaysOnlyTheNewestPerRef` (which relies on `evictOldestPending`'s `id` subquery).

- [ ] **Step 5: Run the whole package**

Run: `go test ./internal/relay/... -count=1`
Expected: `ok` for `internal/relay` and `internal/relay/relaytest`. Any remaining failure is either a `?` you missed (`grep -n "?" internal/relay/*.go | grep -v _test | grep -v '//'` should list only Go ternary-free code — i.e. nothing in a backtick string) or an FK-orphan fixture; fix as in Task 6 Step 6.

- [ ] **Step 6: Commit**

```bash
git add internal/relay/delivery.go internal/relay/concurrency_test.go
git commit -m "feat(relay): parked-event drain claims rows with FOR UPDATE SKIP LOCKED

Two drains of one agent split its events instead of both delivering
them; the delete names the claimed ids rather than the whole agent.

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

## Task 9: `piper-relay` reads `PIPER_RELAY_DB_URL`

**Files:**
- Modify: `cmd/piper-relay/main.go:145-153`
- Modify: `cmd/piper-relay/main_test.go:1-62`

- [ ] **Step 1: Convert `main_test.go`**

Replace the three `relay.Open(filepath.Join(t.TempDir(), "relay.db"))` calls with `relay.Open(relaytest.DSN(t))` and add a `TestMain`:

```go
import (
	"os"
	"path/filepath"
	"testing"

	"github.com/piperbox/piper/internal/relay"
	"github.com/piperbox/piper/internal/relay/relaytest"
	"github.com/piperbox/piper/internal/version"
)

func TestMain(m *testing.M) { os.Exit(relaytest.Main(m)) }
```

(`path/filepath` stays — `TestReadAppKeyMode` uses it.)

Run: `go test ./cmd/piper-relay/ -v`
Expected: the three `TestRunAdmin*` tests PASS against Postgres; the rest are unaffected.

- [ ] **Step 2: Wire the env var**

In `cmd/piper-relay/main.go` replace lines 149–153:

```go
	dsn := os.Getenv("PIPER_RELAY_DB_URL")
	if dsn == "" {
		log.Fatal("PIPER_RELAY_DB_URL is required (postgres://user:password@host/dbname)")
	}
	st, err := relay.Open(dsn)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()
```

`dataDir` and the `MkdirAll` above it stay — certs and the App key still live there. If `path/filepath` becomes unused in `main.go`, remove it (`go vet` reports it).

- [ ] **Step 3: Prove the fatal path by hand**

Run: `go run ./cmd/piper-relay 2>&1 | head -1`
Expected: `… PIPER_RELAY_DB_URL is required (postgres://user:password@host/dbname)` and exit status 1.

- [ ] **Step 4: Commit**

```bash
git add cmd/piper-relay/main.go cmd/piper-relay/main_test.go
git commit -m "feat(relay): piper-relay connects to PIPER_RELAY_DB_URL; relay.db is gone

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

## Task 10: e2e harness spawns relays against a test Postgres; full verify

**Files:**
- Create: `test/e2e/main_test.go`
- Modify: `test/e2e/relay_test.go:52-70`, `test/e2e/login_test.go:43-56`, `test/e2e/relay_terminated_test.go:50-62`

- [ ] **Step 1: Add the package `TestMain`**

```go
// test/e2e/main_test.go
package e2e

import (
	"os"
	"testing"

	"github.com/piperbox/piper/internal/relay/relaytest"
)

// The relay binaries these tests spawn need a Postgres; relaytest provides
// one per process (RUN_E2E already implies Docker is present).
func TestMain(m *testing.M) { os.Exit(relaytest.Main(m)) }
```

- [ ] **Step 2: Hand each spawned relay a DSN**

In `relay_test.go`, after `relayData := t.TempDir()` add `relayDB := relaytest.DSN(t)`, and add `"PIPER_RELAY_DB_URL="+relayDB,` to **both** env lists — the `enroll` command's (`enroll.Env = append(os.Environ(), "PIPER_RELAY_DATA_DIR="+relayData, "PIPER_RELAY_DB_URL="+relayDB)`) and the relay's. The enroll subcommand writes the agent row the relay then reads, so both must point at the same database.

In `login_test.go` and `relay_terminated_test.go`, after `relayData := t.TempDir()` add `relayDB := relaytest.DSN(t)` and `"PIPER_RELAY_DB_URL="+relayDB,` to the relay's env list.

Add the `relaytest` import to all three files.

- [ ] **Step 3: Run the e2e suite**

Run: `RUN_E2E=1 go test ./test/e2e/... -count=1 -run 'Relay|Login' -v`
Expected: PASS for `TestRelay…`, `TestRelayTerminated…`, `TestOneCommandLogin`. (If a stray Caddy or a running `piperd` holds `:80`/`:2019`/`:8088`, the deploy-path tests fail for unrelated reasons — see the repo's e2e notes; the relay-path tests above are the ones this task owns.)

- [ ] **Step 4: Full verify**

Run: `make verify`
Expected: exit status 0. Judge by the exit status, not by grepping the output — it halts at the first failing gate.

- [ ] **Step 5: Commit**

```bash
git add test/e2e/main_test.go test/e2e/relay_test.go test/e2e/login_test.go test/e2e/relay_terminated_test.go
git commit -m "test(e2e): spawned relays run against a relaytest Postgres

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

## Task 11: Docs — runbook, manual-setup, PROGRESS

**Files:**
- Modify: `docs/runbooks/relay-deploy.md` (§What's on disk, §2 Configure, §Upgrade across a schema change, §Ops surface, §Run as a container)
- Modify: `docs/manual-setup.md:118-124`
- Modify: `PROGRESS.md` (Plan 2 relay list)

- [ ] **Step 1: "What's on disk"** — replace the table and the paragraph under it:

```markdown
| Path | What |
| --- | --- |
| `/usr/local/bin/piper-relay` | the binary |
| `/etc/systemd/system/piper-relay.service` | the shipped unit (`DynamicUser`, `StateDirectory=piper-relay`) |
| `/var/lib/piper-relay/` | TLS wildcard pair and (via a credential drop-in) the GitHub App key — no database lives here |
| `/etc/piper-relay.env` | env overrides read by the unit (`EnvironmentFile=-`); **must** carry `PIPER_RELAY_DB_URL` |
| Postgres, at `PIPER_RELAY_DB_URL` | **all relay state** — agents, tokens, accounts, orgs, domains, installations, parked webhooks |

The store applies `schema.sql` with `CREATE TABLE IF NOT EXISTS` on every start
and there are **no migrations** (pre-1.x policy): a release that adds a *table*
upgrades in place; a release that changes an *existing table* needs that table
dropped (or the database recreated) before the new binary starts. Each
release's notes say which case it is — or check yourself:

​```bash
git diff <old-tag>..<new-tag> -- internal/relay/schema.sql
​```

New `CREATE TABLE` → in-place. Changed `CREATE TABLE` body → drop that table (see
[Upgrade across a schema change](#upgrade-across-a-schema-change)).
```

(Remove the zero-width characters before the fences when pasting — they are only there to keep this plan's own code block intact.)

- [ ] **Step 2: "2. Configure"** — add a Postgres block before the listeners block:

```markdown
The relay needs a Postgres database (13 or newer; 17 is what the tests run).
On a single host the distribution package is the simplest pairing with a
systemd-run relay:

​```bash
sudo apt install postgresql
sudo -u postgres psql -c "CREATE ROLE piper_relay LOGIN PASSWORD '<password>'"
sudo -u postgres psql -c "CREATE DATABASE piper_relay OWNER piper_relay"
​```

Then, **required**, in `/etc/piper-relay.env`:

​```bash
PIPER_RELAY_DB_URL=postgres://piper_relay:<password>@127.0.0.1:5432/piper_relay
​```

The relay creates its tables on first start. Any `admin` / `enroll` invocation
needs the same variable — the transient-unit commands below pass it with
`--setenv`.
```

Also change the `systemd-run … enroll` example later in the same section (if present) and the "Ops surface" bullet to say the admin CLI "runs against the same `PIPER_RELAY_DB_URL`".

- [ ] **Step 3: "Upgrade across a schema change"** — replace the `mv relay.db` recipe:

```markdown
​```bash
sudo systemctl stop piper-relay
sudo cp /usr/local/bin/piper-relay /usr/local/bin/piper-relay.prev
sudo -u postgres pg_dump piper_relay > "relay-$(date +%Y%m%d).sql"   # rollback point
# install the new binary as in the same-schema upgrade, then EITHER
# (a) drop only the changed table(s) — the release notes name them:
sudo -u postgres psql piper_relay -c "DROP TABLE <table>"
# OR (b) start from empty, which resets ALL relay state:
sudo -u postgres psql -c "DROP DATABASE piper_relay" -c "CREATE DATABASE piper_relay OWNER piper_relay"
sudo systemctl start piper-relay
​```

Prefer (a) whenever the changed table is one the boxes repopulate on
reconnect (`hostnames`, `repo_bindings`, `custom_domains`, `pending_events`)
or one you can rebuild by hand in `psql`; (b) is the documented full reset
and the rest of this section describes recovering from it.
```

and in the rollback paragraph replace "restore … the moved `relay.db.pre-*`" with "restore the `pg_dump` (`psql piper_relay < relay-<date>.sql` into a freshly created database)".

- [ ] **Step 4: "Run as a container"** — replace the `docker run` example and the State / Upgrade / Exactly-one-replica bullets:

```markdown
The standard self-host path is one compose file — relay plus Postgres:

​```yaml
services:
  postgres:
    image: postgres:17
    restart: unless-stopped
    environment:
      POSTGRES_USER: piper_relay
      POSTGRES_PASSWORD: change-me
      POSTGRES_DB: piper_relay
    volumes:
      - relay_pg:/var/lib/postgresql/data
  relay:
    image: ghcr.io/piperbox/piper-relay:<version>
    restart: unless-stopped
    depends_on: [postgres]
    env_file: piper-relay.env
    environment:
      PIPER_RELAY_DB_URL: postgres://piper_relay:change-me@postgres:5432/piper_relay
    ports: ["443:443", "80:80", "7000:7000", "8080:8080"]
    volumes:
      - ./certs:/var/lib/piper-relay:ro
volumes:
  relay_pg:
​```

- **State** lives in Postgres. On K8s/ECS point `PIPER_RELAY_DB_URL` at a
  managed instance and give the relay no volume at all beyond its certs.
- **Certs and the GitHub App key** are file mounts, as before; the key must
  not be world-readable (mount `0600`).
- **Admin/enroll** run in the relay container against the same database:
  `docker compose exec relay piper-relay admin …`.
- **Upgrade** = pull the new tag, recreate the relay container. The
  schema-change caveats above apply identically — drop the named table(s) in
  `psql` first for a schema-change release.
- **Still exactly one replica.** The database is no longer the reason, but two
  remain: each agent's tunnel lives in one relay process's memory (traffic for
  it must reach that process), and in-flight logins (device-flow polls,
  browser-login state) are per-process. Both are tracked follow-ups; until
  they land, scale up, never out.
```

- [ ] **Step 5: `docs/manual-setup.md`** — in the `systemd-run … enroll` block add a line `--setenv=PIPER_RELAY_DB_URL=postgres://… \` after the `PIPER_RELAY_DATA_DIR` line, and one sentence before the block: "The relay stores everything in Postgres; create a database and put its URL in `/etc/piper-relay.env` as `PIPER_RELAY_DB_URL` first (see the [relay runbook](runbooks/relay-deploy.md#2-configure))."

- [ ] **Step 6: `PROGRESS.md`** — add under the Plan 2 relay list (next to the `#484` cert-reload line):

```markdown
- ✅ `piper-relay` store on Postgres — one shared database for every relay process; SQLite removed from the relay; `PIPER_RELAY_DB_URL` required — [#N](https://github.com/piperbox/piper/issues/N)
```

Open the tracking issue first if none exists (`[relay] persistence on Postgres`, labels `enhancement`, `relay`, `P2`, `size/L`) and substitute its number.

- [ ] **Step 7: Commit**

```bash
git add docs/runbooks/relay-deploy.md docs/manual-setup.md PROGRESS.md
git commit -m "docs(relay): Postgres operations — configure, schema upgrades, compose, one-replica reasons

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

## Task 12: One-off copier for the Hetzner cutover

Ships in this PR so the cutover can be rehearsed against a copy of production; **a follow-up PR deletes `cmd/relay-sqlite-to-pg` once the cutover is done** — it must not survive into a release after that.

**Files:**
- Create: `cmd/relay-sqlite-to-pg/main.go`
- Create: `cmd/relay-sqlite-to-pg/main_test.go`

**Interfaces:**
- Consumes: `relay.Open(dsn)` (to create the target schema), `relaytest.DSN(t)` (test), `modernc.org/sqlite` (still a module dependency via the agent store).
- Produces: `func copyAll(src *sql.DB, dst *sql.DB) (map[string]int, error)` — rows copied per table.

- [ ] **Step 1: Write the failing test**

```go
// cmd/relay-sqlite-to-pg/main_test.go
package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/piperbox/piper/internal/relay"
	"github.com/piperbox/piper/internal/relay/relaytest"
	_ "modernc.org/sqlite"
)

func TestMain(m *testing.M) { os.Exit(relaytest.Main(m)) }

// The production relay.db as of v0.18.0 — the shape this copier reads.
const oldSchema = `
CREATE TABLE agents (name TEXT PRIMARY KEY, token_hash TEXT NOT NULL UNIQUE, base_domain TEXT NOT NULL,
  account_id TEXT, box_id TEXT, control_token TEXT, webhook_secret TEXT, created_at TEXT NOT NULL);
CREATE TABLE accounts (id TEXT PRIMARY KEY, github_id TEXT UNIQUE, github_login TEXT, username TEXT NOT NULL,
  type TEXT NOT NULL DEFAULT 'user', disabled INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, UNIQUE(username, type));
CREATE TABLE account_creds (token_hash TEXT PRIMARY KEY, account_id TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE hostnames (hostname TEXT PRIMARY KEY, agent_name TEXT NOT NULL, account_id TEXT NOT NULL, app TEXT NOT NULL,
  pr INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, UNIQUE(agent_name, app, pr));
CREATE TABLE org_members (org_id TEXT NOT NULL, account_id TEXT NOT NULL, role TEXT NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY (org_id, account_id));
CREATE TABLE org_invites (org_id TEXT NOT NULL, github_login TEXT NOT NULL, invited_by TEXT NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY (org_id, github_login));
CREATE TABLE custom_domains (domain TEXT PRIMARY KEY, agent_base TEXT NOT NULL, status TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE github_installations (installation_id TEXT PRIMARY KEY, account_id TEXT NOT NULL, target_type TEXT NOT NULL, target_login TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE repo_bindings (agent_name TEXT NOT NULL, app TEXT NOT NULL, repo TEXT NOT NULL, branch TEXT NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY (agent_name, app));
CREATE TABLE pending_events (agent_name TEXT NOT NULL, app TEXT NOT NULL, ref TEXT NOT NULL, event TEXT NOT NULL, payload BLOB NOT NULL,
  created_at TEXT NOT NULL, attempts INTEGER NOT NULL, next_try_at TEXT NOT NULL, PRIMARY KEY (agent_name, app, ref));
`

func TestCopyAllRoundTrips(t *testing.T) {
	src, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if _, err := src.Exec(oldSchema); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`INSERT INTO accounts VALUES('acc-1','gh-1','alice','alice','user',1,'2026-01-01T00:00:00Z')`,
		`INSERT INTO accounts VALUES('org-1',NULL,NULL,'acme','org',0,'2026-01-02T00:00:00Z')`,
		`INSERT INTO account_creds VALUES('h1','acc-1','2026-01-01T00:00:00Z')`,
		`INSERT INTO agents VALUES('a.example','th','a.example','acc-1','box-1','ctl','whs','2026-01-03T00:00:00Z')`,
		`INSERT INTO hostnames VALUES('x-alice.example','a.example','acc-1','blog',0,'2026-01-04T00:00:00Z')`,
		`INSERT INTO org_members VALUES('org-1','acc-1','owner','2026-01-02T00:00:00Z')`,
		`INSERT INTO org_invites VALUES('org-1','bob','acc-1','2026-01-05T00:00:00Z')`,
		`INSERT INTO custom_domains VALUES('shop.dev','a.example','active','2026-01-06T00:00:00Z')`,
		`INSERT INTO github_installations VALUES('inst-1','acc-1','User','alice','2026-01-07T00:00:00Z')`,
		`INSERT INTO repo_bindings VALUES('a.example','blog','alice/blog','main','2026-01-08T00:00:00Z')`,
		`INSERT INTO pending_events VALUES('a.example','blog','main','push',X'7B7D','2026-01-09T00:00:00.000000000Z',1,'2026-01-09T00:01:00.000000000Z')`,
	} {
		if _, err := src.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	dsn := relaytest.DSN(t)
	st, err := relay.Open(dsn) // creates the Postgres schema
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	dst, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	counts, err := copyAll(src, dst)
	if err != nil {
		t.Fatalf("copyAll: %v", err)
	}
	for table, want := range map[string]int{
		"accounts": 2, "account_creds": 1, "agents": 1, "hostnames": 1, "org_members": 1,
		"org_invites": 1, "custom_domains": 1, "github_installations": 1, "repo_bindings": 1, "pending_events": 1,
	} {
		if counts[table] != want {
			t.Errorf("%s: copied %d rows, want %d", table, counts[table], want)
		}
	}
	var disabled bool
	if err := dst.QueryRow(`SELECT disabled FROM accounts WHERE id='acc-1'`).Scan(&disabled); err != nil || !disabled {
		t.Errorf("disabled flag not converted to boolean: %v %v", disabled, err)
	}
	var payload []byte
	if err := dst.QueryRow(`SELECT payload FROM pending_events`).Scan(&payload); err != nil || string(payload) != "{}" {
		t.Errorf("payload = %q, %v; want {}", payload, err)
	}
	// The relay must be able to use the copied rows through its own API.
	st, err = relay.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if secret, err := st.AgentWebhookSecret("a.example"); err != nil || secret != "whs" {
		t.Errorf("AgentWebhookSecret = %q, %v", secret, err)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./cmd/relay-sqlite-to-pg/ -v`
Expected: FAIL to compile — `undefined: copyAll`.

- [ ] **Step 3: Write the copier**

```go
// cmd/relay-sqlite-to-pg/main.go

// Command relay-sqlite-to-pg copies a pre-Postgres piper-relay SQLite database
// into a Postgres database whose schema the new relay has already created
// (start the relay once, or call relay.Open). One-off cutover tool: it is
// deleted from the tree once the hosted relay has moved.
//
//	relay-sqlite-to-pg -sqlite ./relay.db -pg postgres://user:pass@host/db
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

// tables lists (table, columns) in foreign-key order: every parent before its
// children. Columns are the SQLite column names, which the Postgres schema
// keeps; the BIGSERIAL id columns are left to Postgres, in rowid order.
var tables = []struct {
	name string
	cols []string
}{
	{"accounts", []string{"id", "github_id", "github_login", "username", "type", "disabled", "created_at"}},
	{"account_creds", []string{"token_hash", "account_id", "created_at"}},
	{"agents", []string{"name", "token_hash", "base_domain", "account_id", "box_id", "control_token", "webhook_secret", "created_at"}},
	{"hostnames", []string{"hostname", "agent_name", "account_id", "app", "pr", "created_at"}},
	{"org_members", []string{"org_id", "account_id", "role", "created_at"}},
	{"org_invites", []string{"org_id", "github_login", "invited_by", "created_at"}},
	{"custom_domains", []string{"domain", "agent_base", "status", "created_at"}},
	{"github_installations", []string{"installation_id", "account_id", "target_type", "target_login", "created_at"}},
	{"repo_bindings", []string{"agent_name", "app", "repo", "branch", "created_at"}},
	{"pending_events", []string{"agent_name", "app", "ref", "event", "payload", "created_at", "attempts", "next_try_at"}},
}

func main() {
	sqlitePath := flag.String("sqlite", "", "path to the old relay.db (opened read-only)")
	pgDSN := flag.String("pg", "", "postgres:// DSN of the new, schema-initialised database")
	flag.Parse()
	if *sqlitePath == "" || *pgDSN == "" {
		log.Fatal("usage: relay-sqlite-to-pg -sqlite relay.db -pg postgres://…")
	}
	src, err := sql.Open("sqlite", "file:"+*sqlitePath+"?mode=ro")
	if err != nil {
		log.Fatal(err)
	}
	defer src.Close()
	dst, err := sql.Open("pgx", *pgDSN)
	if err != nil {
		log.Fatal(err)
	}
	defer dst.Close()
	counts, err := copyAll(src, dst)
	if err != nil {
		log.Fatal(err)
	}
	for _, t := range tables {
		fmt.Printf("%-22s %d\n", t.name, counts[t.name])
	}
}

// copyAll copies every table in one Postgres transaction and returns the row
// count per table. The target must be empty: a re-run against a populated
// database fails on the first primary-key collision and rolls back.
func copyAll(src, dst *sql.DB) (map[string]int, error) {
	tx, err := dst.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	counts := map[string]int{}
	for _, t := range tables {
		n, err := copyTable(src, tx, t.name, t.cols)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t.name, err)
		}
		counts[t.name] = n
	}
	return counts, tx.Commit()
}

func copyTable(src *sql.DB, tx *sql.Tx, table string, cols []string) (int, error) {
	rows, err := src.Query(`SELECT ` + strings.Join(cols, ", ") + ` FROM ` + table + ` ORDER BY rowid`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	placeholders := make([]string, len(cols))
	for i := range cols {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
	}
	insert := `INSERT INTO ` + table + `(` + strings.Join(cols, ", ") + `) VALUES(` + strings.Join(placeholders, ",") + `)`
	n := 0
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return n, err
		}
		for i, c := range cols {
			if table == "accounts" && c == "disabled" {
				// INTEGER 0/1 on SQLite, BOOLEAN on Postgres.
				vals[i] = vals[i] != nil && vals[i].(int64) != 0
			}
		}
		if _, err := tx.Exec(insert, vals...); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}
```

- [ ] **Step 4: Run the test**

Run: `go test ./cmd/relay-sqlite-to-pg/ -v`
Expected: PASS.

- [ ] **Step 5: Verify cross-compile and the whole gate**

Run: `make verify`
Expected: exit status 0 (`make cross` proves the copier also builds for the box's amd64/arm64 without cgo — `modernc.org/sqlite` and pgx are both pure Go).

- [ ] **Step 6: Commit**

```bash
git add cmd/relay-sqlite-to-pg
git commit -m "chore(relay): one-off relay.db → Postgres copier for the hosted-relay cutover

Temporary: delete after the Hetzner relay has moved (tracked in the
migration issue).

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

## Task 13: PR and cutover hand-off

**Files:** none (git / GitHub only).

- [ ] **Step 1: Push and open the PR**

```bash
git push -u origin claude/relay-postgres-migration-fdd140
gh pr create --base main --title "[relay] persistence on Postgres" --body "$(cat <<'EOF'
Moves the relay store from SQLite to Postgres so several relay processes can share one source of truth. Spec: docs/superpowers/specs/2026-08-04-relay-postgres-design.md.

- `internal/relay` on `database/sql` + pgx stdlib; `schema.sql` is Postgres DDL; `PIPER_RELAY_DB_URL` required.
- SQLite's global write lock is replaced by `SELECT … FOR UPDATE` on the three cap checks and `FOR UPDATE SKIP LOCKED` on the parked-event drain, each with a concurrency test.
- Foreign keys are now enforced: `DeleteOrg` deletes the org's installations; orphan test fixtures fixed.
- `internal/relay/relaytest` provisions a Docker Postgres for `internal/relay`, `cmd/piper-relay`, and `test/e2e`; skips cleanly without Docker.
- `cmd/relay-sqlite-to-pg` is a **temporary** copier for the hosted-relay cutover — delete in a follow-up once done.
- Still one replica: tunnel affinity and in-flight login state are per-process (documented in the runbook as follow-ups).

Schema change ⇒ minor version bump; relay before agents (wire-compatible).

Closes #N

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

- [ ] **Step 2: Cutover is an ops action for the user, not this plan**

The runbook written in Task 11 carries the procedure. The order, for the record — install Postgres on the box → stop relay → `scp` `relay.db` down → run `relay-sqlite-to-pg` from the Mac through `ssh -L 5432:127.0.0.1:5432 hetzner-box-root` → set `PIPER_RELAY_DB_URL` in `/etc/piper-relay.env` → install the released binary → start → verify all three agents re-register with their existing tokens, `GET /v1/github/status` still shows the linked App, and the app URLs serve. Keep `relay.db` beside the previous binary as the matched-pair rollback.

---

## Self-review

**Spec coverage.** Scope boundary → Task 11 (runbook wording), Task 13 PR body. Store layer → Tasks 2–8 (every dialect bullet has a task; `rowid` users: `org_members`/`org_invites`/`agents` in Task 6, `github_installations` in Task 7, `pending_events` in Task 8). Foreign-key enforcement → Task 6 (`DeleteOrg`, fixture). Multi-writer correctness → Task 3 (enroll), Task 4 (hostnames), Task 5 (domains), Task 8 (drain), each with its test. Config → Task 9. Tests/harness → Tasks 1–2, e2e in Task 10. Cutover → Task 12 (copier), Task 13 (procedure), Task 11 (runbook). Packaging & docs → Task 11. Concurrency tests → Tasks 3, 4, 8. No gaps.

**Placeholders.** Every code step shows the code; the only "grep for the rest" instruction (Task 7 Step 2) names the exact command and the mechanical rule to apply.

**Type consistency.** `relaytest.DSN(t) string` and `relaytest.Main(m) int` are used identically in Tasks 2, 9, 10, 12. `isUniqueViolation(err) bool` is defined in Task 3 and consumed in Tasks 5 and 6. `copyAll(src, dst *sql.DB) (map[string]int, error)` matches its test. `openTestStore(t) *Store` keeps its existing name.
