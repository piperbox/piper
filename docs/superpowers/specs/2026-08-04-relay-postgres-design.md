# Design: relay persistence on Postgres

Moves `piper-relay`'s store from a per-process SQLite file to a shared Postgres
database, so multiple relay instances behind a reverse proxy read and write one
source of truth. Postgres-only: SQLite support in the relay is deleted, not kept
as a second backend. The agent (`piperd`) is untouched — it stays on
`modernc.org/sqlite`, which is the right shape for a single box.

## Scope boundary — DB only

The relay holds three kinds of state. This design moves exactly one of them.

1. **Persistent state** (the store): agents, accounts, credentials, hostnames,
   orgs, invites, custom domains, GitHub installations, repo bindings, pending
   webhook events. **In scope** — all of it moves to Postgres.
2. **Tunnel sessions** (the in-memory `Router`): each agent's tunnel is a
   long-lived TCP session terminated by exactly one relay process. Sharing the
   DB does **not** make public traffic reach the right relay — if agent X's
   tunnel is on relay A and the proxy sends `x.getpiper.dev` traffic to relay
   B, relay B has the row but not the tunnel. **Out of scope.**
3. **In-flight login flows** (per-process maps in `api.go`,
   `verifier_github.go`, `weblogin_cli.go`, `login_rate.go`): device-flow
   polls, web-login `state` cookies, CLI browser-login handles, the login
   rate limiter, and the per-account installation-reconcile throttle. A
   device flow started on relay A and polled on relay B fails today. These
   are minutes-lived and small, so moving them into Postgres with a TTL is
   straightforward — but it is its own change. **Out of scope.**

Consequence, stated plainly: **after this change alone the relay still runs as
exactly one replica.** The DB stops being a reason; tunnel affinity (2) and
login-flow state (3) remain reasons, each a follow-up design. The runbook's
"scale up, never out" note stays, reworded to name the remaining blockers.

Also out of scope: connection pooling beyond `database/sql` defaults, and HA
for Postgres itself (the operator's problem, as for any stateful service).

## Decision summary

- **Approach:** in-place rewrite of the existing `Store` on `database/sql`,
  swapping `modernc.org/sqlite` for `github.com/jackc/pgx/v5/stdlib`. Pure Go,
  so `CGO_ENABLED=0` and `make cross` hold. pgx v5 is already in `go.mod` as
  an indirect dependency (via Caddy → smallstep); this promotes it to direct,
  adding no new module. Method signatures, file layout, and callers stay put;
  the diff is confined to the SQL layer.
- **Postgres-only.** One schema, one implementation, one test matrix. Aligned
  with the relay's container/K8s packaging direction. No dual backend.
- **`schema.sql` stays the complete current shape** (pre-1.x policy), now in
  Postgres DDL, applied on startup with `CREATE TABLE IF NOT EXISTS` as today.
- **Foreign keys become real.** SQLite never enforced them (`Open` sets no
  `foreign_keys` pragma); Postgres does. We keep the `REFERENCES` clauses and
  fix the two places that relied on non-enforcement (below) rather than
  dropping the constraints.
- **Cutover** for the production (Hetzner) relay is a one-off throwaway copier,
  deleted after use — not a compat shim, no legacy readers in the tree.

## Store layer

`Open(dsn string)` replaces `Open(path string)`; the DSN is a standard
`postgres://` URL. `Configure` and every store method keep their signatures.

Dialect changes across the queries:

- Placeholders `?` → `$1, $2, …`. `drainEvents` builds its `WHERE` clause
  dynamically, so its numbering is computed, not pasted.
- `BLOB` → `BYTEA` (`pending_events.payload`).
- `accounts.disabled INTEGER` → `BOOLEAN`. Its three scan sites
  (`accounts.go` ×2, `hostnames.go`) move from `int`/`sql.NullInt64` to
  `bool`/`sql.NullBool`; the `SET disabled=1` update becomes `true`.
  `hostnames.pr` is a **PR number**, not a flag — it stays `INTEGER`.
- `INSERT OR IGNORE` → `INSERT … ON CONFLICT DO NOTHING` (`orgs.go:353`).
- Existing upserts already use `ON CONFLICT … DO UPDATE SET x = excluded.x`
  (including the conditional `WHERE excluded.created_at > …` in
  `ReparkEvent`), valid Postgres as written.
- Postgres has no `rowid`. The five tables that order or dedupe by it —
  `org_members`, `org_invites`, `github_installations`, `agents`,
  `pending_events` — gain an explicit `id BIGSERIAL` column used the same way
  (insertion-order tiebreak in `orgs.go`/`installations.go`, the keep-newest
  window in `evictOldestPending`).
- The partial unique index `agents_account_box_unique`
  (`WHERE box_id IS NOT NULL AND box_id != ''`) is valid Postgres as written.
- No `LastInsertId` anywhere (pgx's stdlib driver doesn't support it), and
  every `RowsAffected` use is fine.

**Timestamps stay TEXT.** `created_at`/`next_try_at` remain RFC3339Nano /
fixed-width `pendingTimeLayout` strings compared lexicographically, exactly as
today. Switching to `timestamptz` would touch every query and every caller for
zero behavioral gain; not worth it.

## Foreign-key enforcement

Two things depended on SQLite silently ignoring `REFERENCES`:

- **`DeleteOrg`** deletes `org_invites`, `org_members`, `hostnames`, then the
  `accounts` row — but not `github_installations`, which org-target App
  installs link to the org account (`ingress.go:180`). On Postgres the account
  delete would fail. Fix: `DeleteOrg` also deletes the org's
  `github_installations` rows. (`account_creds` never reference orgs; only
  users log in. `DeleteAgent` already deletes its children in the right
  order.)
- **Test fixtures** that insert child rows for parents that don't exist, e.g.
  `orgs_test.go:454` inserts a hostname for an agent named `alice-box` that
  was never enrolled. Each such fixture creates its parent first. The suite
  finds them: they fail loudly on the first Postgres run.

## Multi-writer correctness

This is the substantive work. SQLite's `_txlock=immediate` DSN option gave every
explicit transaction a global write lock from `BEGIN`. With N relay processes
on one Postgres that guarantee is gone and must be replaced with row-level
locking. Four call sites:

1. **`EnrollForAccount`** (`accounts.go`, agents-per-account cap) — already a
   transaction; adds `SELECT … FROM accounts WHERE id=$1 FOR UPDATE` before
   the count. Competing enrolls on the *same* account serialize; unrelated
   accounts stay concurrent. `DeleteOrg`'s "refuse while the org owns agents"
   check locks the same row, closing that TOCTOU too.
2. **`AddCustomDomain`** (`domains.go`, domains-per-agent cap) — already a
   transaction; adds `FOR UPDATE` on the owning `agents` row.
3. **`RegisterHostname`** (`hostnames.go`, apps-per-account cap) — **not in a
   transaction today**, so this race is pre-existing even on SQLite. Becomes
   a transaction with `FOR UPDATE` on the `accounts` row.
4. **`drainEvents`** (`delivery.go`) — one transaction that `SELECT`s then
   `DELETE`s an agent's parked events, commits, and only *then* delivers,
   re-parking on failure. That shape is correct and stays. What breaks on
   Postgres is two drains for the same agent overlapping: under read
   committed both `SELECT`s can return the same rows before either `DELETE`
   commits, and the event is delivered twice. The `SELECT` gains
   `FOR UPDATE SKIP LOCKED`: rows another drain holds are skipped, and that
   drain delivers them. No transaction is held across a network call.

   In practice the retry sweep only drains agents whose tunnel is on *this*
   relay (`drain` bails on `router.Lookup` miss), so the overlap window is
   the reconnect race — an agent moving between relays while a drain is in
   flight — not steady state. The lock is cheap insurance against exactly
   that.

Single-statement reads and writes need no change — read committed covers
them. `ParkEvent`/`ReparkEvent` followed by `evictOldestPending` are two
autocommit statements, as today; the transient over-cap window that allows is
already accepted in the existing comments.

## Config

- New required env: **`PIPER_RELAY_DB_URL`** (`postgres://user:pass@host/db`).
  Fatal at startup when unset — the relay has no local-file fallback. The
  `admin` and `enroll` subcommands open the store before dispatching, so they
  need it too (they run via `docker exec` / the unit's environment, which
  already carries it).
- `PIPER_RELAY_DATA_DIR` survives for TLS certs and the GitHub App key;
  `relay.db` is simply no longer created there.

## Tests

- Store-backed tests live in two packages: `internal/relay` (14 direct `Open`
  calls across 7 files, plus the existing `openTestStore(t)` helper in
  `accounts_test.go` that ~all other tests already use) and
  `cmd/piper-relay` (3 calls in `main_test.go`). ~25 test sites also run raw
  SQL through `st.db` with `?` placeholders; those get `$N` too.
- A small **`internal/relay/relaytest`** package owns the harness so both test
  packages share it:
  - If `PIPER_TEST_POSTGRES_URL` is set, use that server.
  - Else, if the `docker` CLI works, start one disposable `postgres:17`
    container for the process, wait for readiness, remove it on exit.
  - Else, **skip cleanly** — the repo's `dockerAvailable(t)` convention from
    `internal/runtime`.
  - `relaytest.Open(t)` issues `CREATE DATABASE` with a unique name on that
    server and returns a `*relay.Store` for it, so tests keep today's
    isolation and parallelism.
- `openTestStore(t)` in `internal/relay` becomes a one-line wrapper over the
  harness; the 14 + 3 direct `Open` call sites become helper calls.
- CI needs no workflow change: GitHub's ubuntu runners have Docker, so the
  spawn path runs. Exporting `PIPER_TEST_POSTGRES_URL` against a service
  container is an optional speedup, not a requirement.

## Cutover (production relay on Hetzner)

Existing accounts, agents, hostnames, installations, and bindings survive; no
re-enroll, no GitHub App re-link, no repeat of the 2026-07-31 reset.

- **Postgres on the box:** a `postgresql` package install, localhost-only,
  one database and one password-authenticated role for the relay. (Docker is
  present on the box, but a systemd-managed Postgres next to a systemd-managed
  relay is the simpler pairing.)
- **The copier:** a small pure-Go program in the tree during the cutover PR
  only, opening `relay.db` read-only via `modernc.org/sqlite` and inserting
  into Postgres table-by-table, `id` columns left to `BIGSERIAL` in
  `rowid` order. Run from the Mac against a `scp`'d copy of the DB
  (the established way to work on this DB — the live file is root-only under
  `/var/lib/private/piper-relay`) through an `ssh -L` forward to the box's
  Postgres. **Deleted from the tree after cutover.** It never ships in a
  release binary, so the no-legacy-readers rule stays intact.
- **Ops order:** install Postgres → stop relay → copy `relay.db` down → run
  copier → set `PIPER_RELAY_DB_URL` in `/etc/piper-relay.env` → install the
  new binary → start → verify all three agents re-register with their
  existing tokens, `/v1/github/status` still reports the linked App, and the
  app URLs serve. Keep `relay.db` as the matched-pair backup with the old
  binary, as for every prior upgrade.
- Schema change ⇒ **minor version bump**; relay-before-agents release ordering
  as usual (agents are wire-compatible — nothing in the tunnel protocol
  changes).

## Packaging & docs

- `Dockerfile.relay` unchanged (same static binary).
- `docs/runbooks/relay-deploy.md` is the doc that actually changes:
  - "What's on disk": `relay.db` row becomes the Postgres DSN in
    `/etc/piper-relay.env`; state lives in Postgres.
  - "Upgrade across a schema change": the fresh-DB / `DROP TABLE` recipes are
    rewritten for Postgres (same policy, `psql` instead of a cross-compiled
    helper).
  - "Run as a container": the SQLite-on-a-volume guidance becomes a `docker
    compose` example (relay + `postgres:17`) as the standard self-host path;
    the "exactly one replica" bullet stays, now citing tunnel affinity and
    login-flow state as the reasons.
- `docs/manual-setup.md` and `docs/getting-started.md` mention the relay's
  data dir; both get the env var.

## Testing the multi-writer claims

Beyond porting the existing suite, three targeted tests earn their keep:

- Two concurrent `EnrollForAccount` calls against one account at the cap must
  yield exactly one success (exercises `FOR UPDATE` serialization).
- Two concurrent `RegisterHostname` calls against one account at the app cap,
  likewise — this one is a new guarantee, not a preserved one.
- Two concurrent `DrainEvents` calls for one agent with one parked event must
  return it exactly once between them (exercises `SKIP LOCKED`).

All run against real Postgres via the harness above — they are meaningless on
SQLite and impossible with fakes.
