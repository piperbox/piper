# Design: relay persistence on Postgres

Moves `piper-relay`'s store from a per-process SQLite file to a shared Postgres
database, so multiple relay instances behind a reverse proxy read and write one
source of truth. Postgres-only: SQLite support in the relay is deleted, not kept
as a second backend. The agent (`piperd`) is untouched — it stays on
`modernc.org/sqlite`, which is the right shape for a single box.

## Scope boundary — DB only

The relay holds two kinds of state:

1. **Persistent state** (the store): agents, accounts, credentials, hostnames,
   orgs, invites, custom domains, GitHub installations, repo bindings, pending
   webhook events. This design moves all of it to Postgres.
2. **Live state** (the in-memory `Router`): each agent's tunnel is a long-lived
   TCP session terminated by exactly one relay process. Sharing the DB does
   **not** make public traffic reach the right relay — if agent X's tunnel is
   on relay A and the proxy sends `x.getpiper.dev` traffic to relay B, relay B
   has the row but not the tunnel.

Cross-relay traffic routing (SNI affinity at the proxy, cross-relay session
lookup/forwarding) is explicitly **out of scope** — a follow-up design. Also out
of scope: connection pooling beyond `database/sql` defaults, and HA for
Postgres itself (that's the operator's problem, same as any stateful service).

## Decision summary

- **Approach:** in-place rewrite of the existing `Store` on `database/sql`,
  swapping `modernc.org/sqlite` for `github.com/jackc/pgx/v5/stdlib`. Pure Go,
  so `CGO_ENABLED=0` and `make cross` hold. Method signatures, file layout, and
  callers stay put; the diff is confined to the SQL layer.
- **Postgres-only.** One schema, one implementation, one test matrix. Aligned
  with the relay's container/K8s packaging direction. No dual backend.
- **`schema.sql` stays the complete current shape** (pre-1.x policy), now in
  Postgres DDL, applied on startup with `CREATE TABLE IF NOT EXISTS` as today.
- **Cutover** for the production (Hetzner) relay is a one-off throwaway copier,
  deleted after use — not a compat shim, no legacy readers in the tree.

## Store layer

`Open(dsn string)` replaces `Open(path string)`; the DSN is a standard
`postgres://` URL. `Configure` and every store method keep their signatures.

Dialect changes across the ~40 queries:

- Placeholders `?` → `$1, $2, …`.
- `BLOB` → `BYTEA` (`pending_events.payload`).
- Integer booleans (`accounts.disabled`, `hostnames.pr`) → `BOOLEAN`. Scans of
  `sql.NullInt64` for `disabled` become `sql.NullBool`.
- `INSERT OR IGNORE` → `INSERT … ON CONFLICT DO NOTHING` (`orgs.go`).
- Existing upserts already use `ON CONFLICT … DO UPDATE SET x = excluded.x`,
  valid Postgres as written.
- Postgres has no `rowid`. The five tables that order or dedupe by it —
  `org_members`, `org_invites`, `github_installations`, `agents`,
  `pending_events` — gain an explicit `id BIGSERIAL` column used the same way
  (insertion-order tiebreak in `orgs.go`/`installations.go`, the keep-newest
  window in `delivery.go`).
- The partial unique index `agents_account_box_unique`
  (`WHERE box_id IS NOT NULL AND box_id != ''`) is valid Postgres as written.

**Timestamps stay TEXT.** `created_at`/`next_try_at` remain RFC3339Nano /
fixed-width `pendingTimeLayout` strings compared lexicographically, exactly as
today. Switching to `timestamptz` would touch every query and every caller for
zero behavioral gain; not worth it.

## Multi-writer correctness

This is the substantive work. SQLite's `_txlock=immediate` DSN option gave every
write transaction a global lock from BEGIN, which the store leaned on in two
places. With N relay processes on one Postgres, that guarantee is gone and must
be replaced with row-level locking:

1. **Cap checks** (COUNT-then-INSERT races): agents-per-account
   (`EnrollForAccount`), apps-per-account (`RegisterHostname`),
   domains-per-agent (`AddCustomDomain`). Each becomes a transaction that first
   `SELECT … FOR UPDATE`s the owning `accounts` (or `agents`) row, then counts,
   then inserts. Competing enrolls on the *same* account serialize; unrelated
   accounts stay fully concurrent.
2. **Pending-events retry sweep** (`delivery.go`): two relays now run the sweep
   simultaneously. Row claims use `SELECT … FOR UPDATE SKIP LOCKED` inside a
   transaction held for the delivery attempt, so a parked webhook is delivered
   by exactly one relay; the other's sweep skips in-flight rows instead of
   blocking or double-delivering. The park/dedupe/keep-newest logic that today
   relies on the immediate-transaction comment moves into the same explicit
   transactions.

Single-statement reads and writes need no change — Postgres's default
read-committed semantics cover them.

## Config

- New required env: **`PIPER_RELAY_DB_URL`** (`postgres://user:pass@host/db`).
  Fatal at startup when unset — the relay has no local-file fallback.
- `PIPER_RELAY_DATA_DIR` survives for TLS certs and the GitHub App key;
  `relay.db` is simply no longer created there.

## Tests

- A package-level `TestMain` in `internal/relay`:
  - If `PIPER_TEST_POSTGRES_URL` is set, use it (CI service container, or a
    developer's long-running local Postgres).
  - Else, if the `docker` CLI works, start one disposable `postgres:17`
    container for the package run, wait for readiness, tear it down after.
  - Else, store-backed tests **skip cleanly** — the repo's existing
    Docker-skip convention.
- A helper `openTestStore(t)` issues `CREATE DATABASE` with a unique name on
  the shared server per test, so tests keep today's isolation and parallelism.
  The ~40 `Open(filepath.Join(t.TempDir(), "relay.db"))` call sites become
  `openTestStore(t)` mechanically.
- CI: the test job gains a `postgres` service and exports
  `PIPER_TEST_POSTGRES_URL`.

## Cutover (production relay on Hetzner)

Existing accounts, agents, hostnames, and bindings survive; no re-enroll, no
repeat of the 2026-07-31 reset pain.

- A throwaway copier — a small `go run`-able program that opens the old
  `relay.db` (read-only) and the new Postgres and copies rows table-by-table.
  Used once at cutover, then **deleted from the tree**. It never ships in a
  release binary, so the no-legacy-readers rule stays intact.
- Ops order: stand up Postgres on the box (container next to the relay) → stop
  relay → run copier → start new relay with `PIPER_RELAY_DB_URL` → verify the
  Mac agent reconnects with its existing token and hostnames resolve.
- Schema change ⇒ **minor version bump**; relay-before-agents release ordering
  as usual (agents are wire-compatible — nothing in the tunnel protocol
  changes).

## Packaging & docs

- `Dockerfile.relay` unchanged (same static binary).
- The relay deployment doc gains a `docker compose` example — relay + postgres —
  as the standard self-host path.
- The Hetzner runbook gets the Postgres container and the cutover steps.

## Testing the multi-writer claims

Beyond porting the existing suite, two targeted tests earn their keep:

- Two concurrent `EnrollForAccount` calls against one account at the cap must
  yield exactly one success (exercises `FOR UPDATE` serialization).
- Two concurrent retry sweeps over one parked event must deliver it exactly
  once (exercises `SKIP LOCKED`).

Both run against real Postgres via the harness above — they are meaningless on
SQLite and impossible with fakes.
