# Relay scale-out: `piper-edge` + tunnel ownership — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let N `piper-relay` processes share one public address by adding a stateless L4 entrypoint, `piper-edge`, that routes every connection to the relay that owns the agent's tunnel.

**Architecture:** Two new Postgres tables (`relay_instances`, `agent_owners`) plus four NOTIFY channels tell every process which relay terminates which agent. `piper-relay` gains a heartbeat, an ownership write on tunnel register/unregister, a control-API hop to the owner, and a NOTIFY-woken webhook drain. `piper-edge` (new binary, code in `internal/relay/edge*.go`) peeks SNI/Host, resolves owner from an in-memory copy of those tables, and splices bytes to the owner with a PROXY v2 header. Nothing else in the relay changes shape.

**Tech Stack:** Go 1.26, `database/sql` over `jackc/pgx/v5/stdlib` (store), `jackc/pgx/v5` directly (LISTEN), `pires/go-proxyproto` (PROXY v2 read and write), `hashicorp/yamux` via `internal/tunnel`, Prometheus client. Spec: [`docs/superpowers/specs/2026-09-04-relay-edge-ownership-design.md`](../specs/2026-09-04-relay-edge-ownership-design.md).

## Global Constraints

- **No cgo.** Every build and test runs with `CGO_ENABLED=0`; `make cross` must stay green.
- **No new Go module.** `go.mod` `require` block is unchanged by this plan; only already-direct dependencies are imported.
- **Pre-1.x schema policy.** `internal/relay/schema.sql` is edited in place to its complete current shape. No migration, no compat reader.
- **Timings (verbatim from the spec):** heartbeat every `5s`; instance live while `last_seen` within `15s`; backend dial timeout `2s`; edge names cache `30s` (positive and negative); edge instance poll `15s`; `:7000` retries the next candidate exactly once, `:443`/`:80` never retry.
- **NOTIFY channels (exact names):** `piper_instances`, `piper_owners`, `piper_hostnames`, `piper_events`. Payload is the key that changed.
- **Env names (exact):** relay gains `PIPER_RELAY_ADVERTISE_HOST`; edge reads `PIPER_EDGE_DB_URL`, `PIPER_EDGE_APEX`, `PIPER_EDGE_TLS_ADDR`, `PIPER_EDGE_HTTP_ADDR`, `PIPER_EDGE_TUNNEL_ADDR`, `PIPER_EDGE_PROXY_PROTOCOL`, `PIPER_EDGE_OPS_ADDR`, `PIPER_EDGE_METRICS`, `PIPER_EDGE_LOGS`. Nothing else.
- **Image:** `ghcr.io/piperbox/piper-edge`, container-only, same distroless base as the relay.
- **Ownership semantics:** register upserts; unregister deletes only `WHERE instance_id = <me>`.
- **Layering:** `store` methods know only persistence. `edge.go` reads the store and deletes dead instance rows; it never calls a store write path other than `DeleteInstance`/`PurgeDeadInstances`.
- **Commits:** one per task, conventional-commit style, reference the child issue (`Part of #CHILD`), and end with `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`.
- **Verification:** `make verify` before every commit; judge it by exit status, not by grepping output. Postgres-backed tests need Docker (they skip cleanly otherwise; a skip is not a pass — run them with Docker available before claiming a task done).
- **Issue numbers:** Task 1 creates the epic and children. Everywhere below, `#EPIC` and `#CHILD` mean the numbers Task 1 printed; substitute them literally.

---

## File Structure

| File | Responsibility |
| --- | --- |
| `internal/relay/schema.sql` | + `relay_instances`, `agent_owners` (Task 2) |
| `internal/relay/ownership.go` (new) | Store methods for instances, owners, hostname→agent lookups, the `notify` helper, channel constants (Tasks 2–4) |
| `internal/relay/notify.go` (new) | `listen`: one dedicated pgx LISTEN connection with reconnect + resync (Task 4) |
| `internal/relay/instance.go` (new) | `Instance` identity, advertise-host default, heartbeat, `RunInstance` (relay-side watcher) (Tasks 5, 7) |
| `internal/relay/router.go` | + `Holds`, `Bases` (Task 7) |
| `internal/relay/server.go` | `Serve`/`acceptTunnels`/`serveTunnel` carry `*Instance`; `SetOwner`/`ClearOwner` beside `Register`/`Unregister` (Task 6) |
| `internal/relay/proxy.go` | Control hop + cluster-wide liveness (Task 8) |
| `internal/relay/api.go` | `NewAPIWithTunnel` threads `*Instance` to the proxy (Task 8) |
| `internal/relay/hostnames.go`, `domains.go`, `delivery.go` | Fire `piper_hostnames` / `piper_events` (Task 4) |
| `internal/relay/ops.go` | `newMetrics(prefix, router)`, `NewEdgeMetrics`, `DialFailed` (Task 10) |
| `internal/relay/edge_state.go` (new) | In-memory tables + pure resolver (Task 10) |
| `internal/relay/edge.go` (new) | `EdgeConfig`, `ServeEdge`, listeners, forward with PROXY v2, failure handling (Task 11) |
| `cmd/piper-relay/main.go` | Advertise host, instance, `RunInstance`, signal handling (Task 9) |
| `cmd/piper-edge/main.go` (new) | Thin main (Task 13) |
| `Makefile`, `.goreleaser.yaml`, `Dockerfile.edge` (new), `CLAUDE.md` | Packaging (Task 13) |
| `docs/runbooks/relay-deploy.md`, `PROGRESS.md` | Docs (Task 14) |
| Tests | `ownership_test.go`, `notify_test.go`, `instance_test.go`, `edge_state_test.go`, `edge_test.go` (new); additions to `server_test.go`, `proxy_test.go`, `router_test.go`; existing callers updated for the new parameters |

---

### Task 1: GitHub tracking — epic and three children

**Files:** none in the repo. Output: issue numbers used by every later commit.

- [ ] **Step 1: Create the three children first, then the epic that links them**

```bash
gh issue create --title "[relay] piper-edge + tunnel ownership" \
  --label enhancement,relay,P2,size/L \
  --body "$(cat <<'BODY'
First child of the scale-out epic. Spec: docs/superpowers/specs/2026-09-04-relay-edge-ownership-design.md. Plan: docs/superpowers/plans/2026-09-04-relay-edge-ownership.md.

Ships: `relay_instances` + `agent_owners` tables and four NOTIFY channels; relay heartbeat and ownership writes; control-API hop to the owner over :8080; NOTIFY-woken webhook drain; the `piper-edge` binary (SNI/Host/least-sessions routing with PROXY v2 to the owner); packaging (`ghcr.io/piperbox/piper-edge`); runbook "Scale out" section.

Acceptance: two in-process relays behind one edge pass the integration test in `internal/relay/edge_test.go` (placement, passthrough to the right relay with the client address intact, control hop, webhook drain after NOTIFY, ownership move on reconnect); `make verify` green.
BODY
)"
gh issue create --title "[relay] login-flow state in Postgres" \
  --label enhancement,relay,P3,size/M \
  --body "Second child of the scale-out epic. Device-flow polls, web-login state, CLI browser-login handles, and the login rate limiter are per-process today, so piper-edge pins api.<apex> to the earliest-started relay. Move them to Postgres with TTLs, then make the edge's api.<apex> rule round-robin. Spec context: docs/superpowers/specs/2026-09-04-relay-edge-ownership-design.md (Follow-ups, 1)."
gh issue create --title "[relay] graceful drain on relay restart" \
  --label enhancement,relay,agent,P3,size/M \
  --body "Third child of the scale-out epic. On SIGTERM a relay should stop accepting :7000, tell each agent to reconnect now, and wait for sessions to leave; an edge should flip readiness off and keep splices alive until they end. Needs a small agent-side change. Turns N replicas into zero-drop deploys. Spec context: docs/superpowers/specs/2026-09-04-relay-edge-ownership-design.md (Follow-ups, 2)."
```

Record the three numbers the commands print as `CHILD`, `LOGIN`, `DRAIN`.

- [ ] **Step 2: Create the epic with the checklist**

```bash
gh issue create --title "[relay] scale out behind a reverse proxy" \
  --label epic,enhancement,relay,P2,size/XL \
  --body "$(cat <<'BODY'
Run `piper-relay` as N processes behind one public address. Today the relay is exactly one replica: each agent's yamux session lives in one process, and every public connection, control call, and webhook for that agent must reach that process.

Design: docs/superpowers/specs/2026-09-04-relay-edge-ownership-design.md — route to the owner, never forward between relays; a stateless `piper-edge` binary is the only public entrypoint and learns ownership from Postgres (LISTEN/NOTIFY).

- [ ] #CHILD piper-edge + tunnel ownership (this ships a working tiered deployment)
- [ ] #LOGIN login-flow state in Postgres (until then the edge pins api.<apex> to one relay)
- [ ] #DRAIN graceful drain (until then a relay restart drops its tunnels; agents redial from 1s backoff)

The "per-process background work" concern from the Postgres design (2026-08-04) is closed here without code: the parked-webhook sweeper already drains only agents with a local session, so N replicas never race on one agent.
BODY
)"
```

Substitute `#CHILD`, `#LOGIN`, `#DRAIN` with the real numbers before running. Record the epic number as `EPIC`.

- [ ] **Step 3: Verify labels landed**

Run: `gh issue view EPIC --json labels,title` and `gh issue view CHILD --json labels,title`
Expected: titles as above; labels include `relay` and a `P*` and `size/*`.

No commit for this task.

---

### Task 2: Schema + instance/owner store methods

**Files:**
- Modify: `internal/relay/schema.sql` (append after `pending_events`)
- Create: `internal/relay/ownership.go`
- Test: `internal/relay/ownership_test.go`

**Interfaces:**
- Produces: `type InstanceRow struct{ID string; StartedAt time.Time; Sessions int; TLSAddr, HTTPAddr, TunnelAddr, APIAddr string}`; `(*Store).UpsertInstance(InstanceRow) error`; `(*Store).DeleteInstance(id string) error`; `(*Store).PurgeDeadInstances() error`; `(*Store).LiveInstances() ([]InstanceRow, error)`; `(*Store).SetOwner(baseDomain, instanceID string) error`; `(*Store).ClearOwner(baseDomain, instanceID string) error`; `(*Store).OwnerOf(baseDomain string) (InstanceRow, bool, error)`; `(*Store).Owners() (map[string]string, error)`; constants `instanceTTL`, `chanInstances`, `chanOwners`, `chanHostnames`, `chanEvents`; test helper `stampInstance`.
- Note: `notify` is called from these methods but written in Task 4. For this task, write `notify` as the final version shown in Task 4 Step 3 (it is five lines) — the NOTIFY *test* comes in Task 4.

- [ ] **Step 1: Write the failing tests**

`internal/relay/ownership_test.go`:

```go
package relay

import (
	"crypto/rand"
	"encoding/hex"
	"testing"
	"time"
)

// stampInstance inserts a live relay_instances row for a fake relay whose
// four listener addrs are all addr, and returns the matching Instance. Tests
// that never dial pass "127.0.0.1:1".
func stampInstance(t *testing.T, st *Store, id, addr string, started time.Time) *Instance {
	t.Helper()
	inst := &Instance{ID: id, StartedAt: started.UTC(), TLSAddr: addr, HTTPAddr: addr, TunnelAddr: addr, APIAddr: addr}
	if err := st.UpsertInstance(inst.row(0)); err != nil {
		t.Fatal(err)
	}
	return inst
}

// testInstance stamps a uniquely named placeholder instance for tests that
// only need serveTunnel's ownership writes to have a parent row.
func testInstance(t *testing.T, st *Store) *Instance {
	t.Helper()
	var raw [4]byte
	_, _ = rand.Read(raw[:])
	return stampInstance(t, st, "test-"+hex.EncodeToString(raw[:]), "127.0.0.1:1", time.Now())
}

// ageInstance pushes an instance's last_seen back so it reads as dead.
func ageInstance(t *testing.T, st *Store, id string) {
	t.Helper()
	if _, err := st.db.Exec(`UPDATE relay_instances SET last_seen = now() - interval '20 seconds' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
}

func enrollTestAgent(t *testing.T, st *Store) Enrollment {
	t.Helper()
	st.Configure("public.getpiper.co", 3, 10, 5)
	acc, err := st.UpsertAccount("sub-1", "alice")
	if err != nil {
		t.Fatal(err)
	}
	en, err := st.EnrollForAccount(acc.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	return en
}

func TestLiveInstancesOrdersByStartAndSkipsStale(t *testing.T) {
	st := openTestStore(t)
	stampInstance(t, st, "b", "127.0.0.1:1", time.Now())
	stampInstance(t, st, "a", "127.0.0.1:1", time.Now().Add(-time.Minute))
	stampInstance(t, st, "stale", "127.0.0.1:1", time.Now().Add(-time.Hour))
	ageInstance(t, st, "stale")

	rows, err := st.LiveInstances()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].ID != "a" || rows[1].ID != "b" {
		t.Fatalf("live = %+v, want [a b]", rows)
	}
}

func TestUpsertInstanceRefreshesSessionsAndLastSeen(t *testing.T) {
	st := openTestStore(t)
	inst := stampInstance(t, st, "a", "127.0.0.1:1", time.Now())
	ageInstance(t, st, "a")
	if rows, _ := st.LiveInstances(); len(rows) != 0 {
		t.Fatalf("aged row still live: %+v", rows)
	}
	if err := st.UpsertInstance(inst.row(3)); err != nil {
		t.Fatal(err)
	}
	rows, _ := st.LiveInstances()
	if len(rows) != 1 || rows[0].Sessions != 3 {
		t.Fatalf("after heartbeat: %+v, want one live row with 3 sessions", rows)
	}
}

func TestPurgeDeadInstancesDeletesOnlyStaleRows(t *testing.T) {
	st := openTestStore(t)
	stampInstance(t, st, "a", "127.0.0.1:1", time.Now())
	stampInstance(t, st, "stale", "127.0.0.1:1", time.Now())
	ageInstance(t, st, "stale")
	if err := st.PurgeDeadInstances(); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM relay_instances`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows after purge = %d (%v), want 1", n, err)
	}
}

func TestSetOwnerOverwritesAndClearOwnerIsConditional(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	stampInstance(t, st, "a", "127.0.0.1:1", time.Now())
	stampInstance(t, st, "b", "127.0.0.1:1", time.Now())

	if err := st.SetOwner(en.BaseDomain, "a"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetOwner(en.BaseDomain, "b"); err != nil {
		t.Fatal(err)
	}
	// a's late unregister must not remove b's row.
	if err := st.ClearOwner(en.BaseDomain, "a"); err != nil {
		t.Fatal(err)
	}
	if r, ok, err := st.OwnerOf(en.BaseDomain); err != nil || !ok || r.ID != "b" {
		t.Fatalf("owner after conditional clear = %+v ok=%v err=%v, want b", r, ok, err)
	}
	if err := st.ClearOwner(en.BaseDomain, "b"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.OwnerOf(en.BaseDomain); ok {
		t.Fatal("owner still set after the holder cleared it")
	}
}

func TestSetOwnerUnknownAgentIsBadToken(t *testing.T) {
	st := openTestStore(t)
	stampInstance(t, st, "a", "127.0.0.1:1", time.Now())
	if err := st.SetOwner("nobody.public.getpiper.co", "a"); err != ErrBadToken {
		t.Fatalf("SetOwner unknown agent = %v, want ErrBadToken", err)
	}
}

func TestDeleteInstanceCascadesOwnership(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	stampInstance(t, st, "a", "127.0.0.1:1", time.Now())
	if err := st.SetOwner(en.BaseDomain, "a"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteInstance("a"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM agent_owners`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("agent_owners after cascade = %d (%v), want 0", n, err)
	}
}

func TestOwnerOfIgnoresDeadOwner(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	stampInstance(t, st, "a", "127.0.0.1:1", time.Now())
	if err := st.SetOwner(en.BaseDomain, "a"); err != nil {
		t.Fatal(err)
	}
	ageInstance(t, st, "a")
	if _, ok, err := st.OwnerOf(en.BaseDomain); err != nil || ok {
		t.Fatalf("dead owner reported live (ok=%v err=%v)", ok, err)
	}
	owners, err := st.Owners()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := owners[en.BaseDomain]; ok {
		t.Fatalf("Owners() lists a dead owner: %v", owners)
	}
}

func TestOwnersMapsBaseDomainToLiveInstance(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	stampInstance(t, st, "a", "127.0.0.1:1", time.Now())
	if err := st.SetOwner(en.BaseDomain, "a"); err != nil {
		t.Fatal(err)
	}
	owners, err := st.Owners()
	if err != nil {
		t.Fatal(err)
	}
	if owners[en.BaseDomain] != "a" {
		t.Fatalf("Owners() = %v, want %s→a", owners, en.BaseDomain)
	}
}

func TestDeleteAgentDropsItsOwnerRow(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	stampInstance(t, st, "a", "127.0.0.1:1", time.Now())
	if err := st.SetOwner(en.BaseDomain, "a"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteAgent(en.BaseDomain); err != nil {
		t.Fatalf("DeleteAgent with an owner row: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/relay/ -run 'Instance|Owner|PurgeDead' -count=1`
Expected: compile error — `undefined: Instance`, `st.UpsertInstance`, etc.

- [ ] **Step 3: Append the tables to `schema.sql`**

Append to the end of `internal/relay/schema.sql`:

```sql
-- relay_instances is the pool of live relay processes an edge can dial. A
-- row is live while last_seen is within instanceTTL (heartbeat every 5 s);
-- liveness is a read-side predicate, so a crashed relay drops out of routing
-- without anyone deleting anything. Whoever reads a dead row deletes it. The
-- four addrs are what an edge dials for each of the relay's listeners.
-- TIMESTAMPTZ here (unlike the TEXT stamps above) because the liveness
-- predicate compares against the server's now(), never a relay's clock.
CREATE TABLE IF NOT EXISTS relay_instances (
    id          TEXT PRIMARY KEY,
    started_at  TIMESTAMPTZ NOT NULL,
    last_seen   TIMESTAMPTZ NOT NULL,
    sessions    INTEGER NOT NULL DEFAULT 0,
    tls_addr    TEXT NOT NULL,
    http_addr   TEXT NOT NULL,
    tunnel_addr TEXT NOT NULL,
    api_addr    TEXT NOT NULL
);

-- agent_owners says which instance terminates an agent's tunnel. The
-- instance cascade takes ownership down with a deleted instance row; the
-- agents cascade lets DeleteAgent stay unchanged.
CREATE TABLE IF NOT EXISTS agent_owners (
    agent_name  TEXT PRIMARY KEY REFERENCES agents(name) ON DELETE CASCADE,
    instance_id TEXT NOT NULL REFERENCES relay_instances(id) ON DELETE CASCADE,
    since       TIMESTAMPTZ NOT NULL
);
```

- [ ] **Step 4: Write `ownership.go` (store side) and the `Instance` type stub**

`internal/relay/ownership.go`:

```go
package relay

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// instanceTTL is how long a relay_instances row stays live after its last
// heartbeat. Liveness is a read-side predicate (last_seen within the TTL), so
// a crashed relay disappears from routing without anyone deleting anything.
const instanceTTL = 15 * time.Second

// NOTIFY channels. Fired inside the store method that changes the rows;
// payload is the key that changed.
const (
	chanInstances = "piper_instances" // instance id
	chanOwners    = "piper_owners"    // agent base domain
	chanHostnames = "piper_hostnames" // hostname or custom domain
	chanEvents    = "piper_events"    // agent name (= base domain for account enrollments)
)

// liveWhere is the liveness predicate, evaluated against the server clock.
var liveWhere = fmt.Sprintf(`last_seen > now() - interval '%d seconds'`, int(instanceTTL/time.Second))

// InstanceRow is one relay process as the pool sees it.
type InstanceRow struct {
	ID         string
	StartedAt  time.Time
	Sessions   int
	TLSAddr    string
	HTTPAddr   string
	TunnelAddr string
	APIAddr    string
}

// execer is the slice of *sql.DB and *sql.Tx that notify needs.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// notify fires a Postgres NOTIFY. Inside a transaction it is delivered on
// commit, which is exactly when the row it announces becomes visible.
func notify(ex execer, channel, payload string) error {
	_, err := ex.Exec(`SELECT pg_notify($1, $2)`, channel, payload)
	return err
}

// UpsertInstance inserts or refreshes an instance row — the heartbeat — and
// announces it on piper_instances.
func (s *Store) UpsertInstance(r InstanceRow) error {
	if _, err := s.db.Exec(
		`INSERT INTO relay_instances(id, started_at, last_seen, sessions, tls_addr, http_addr, tunnel_addr, api_addr)
		 VALUES($1, $2, now(), $3, $4, $5, $6, $7)
		 ON CONFLICT(id) DO UPDATE SET last_seen = now(), sessions = excluded.sessions`,
		r.ID, r.StartedAt, r.Sessions, r.TLSAddr, r.HTTPAddr, r.TunnelAddr, r.APIAddr); err != nil {
		return err
	}
	return notify(s.db, chanInstances, r.ID)
}

// DeleteInstance removes an instance row; the cascade takes its agent_owners
// rows. Clean shutdown calls it, and so does whoever finds the instance dead.
func (s *Store) DeleteInstance(id string) error {
	if _, err := s.db.Exec(`DELETE FROM relay_instances WHERE id=$1`, id); err != nil {
		return err
	}
	return notify(s.db, chanInstances, id)
}

// PurgeDeadInstances deletes every row past instanceTTL. Rows it removes were
// already invisible to LiveInstances/OwnerOf, so it does not notify.
func (s *Store) PurgeDeadInstances() error {
	_, err := s.db.Exec(`DELETE FROM relay_instances WHERE NOT (` + liveWhere + `)`)
	return err
}

const instanceCols = `id, started_at, sessions, tls_addr, http_addr, tunnel_addr, api_addr`

func scanInstance(sc interface{ Scan(...any) error }) (InstanceRow, error) {
	var r InstanceRow
	err := sc.Scan(&r.ID, &r.StartedAt, &r.Sessions, &r.TLSAddr, &r.HTTPAddr, &r.TunnelAddr, &r.APIAddr)
	return r, err
}

// LiveInstances lists the instances heard from within instanceTTL, earliest
// started first (ties by id, so the order is total).
func (s *Store) LiveInstances() ([]InstanceRow, error) {
	rows, err := s.db.Query(`SELECT ` + instanceCols + ` FROM relay_instances WHERE ` + liveWhere + ` ORDER BY started_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InstanceRow
	for rows.Next() {
		r, err := scanInstance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetOwner records that instanceID now terminates baseDomain's tunnel. An
// upsert: an agent that reconnected elsewhere is the truth, so the new owner
// overwrites. Unknown agents are ErrBadToken.
func (s *Store) SetOwner(baseDomain, instanceID string) error {
	res, err := s.db.Exec(
		`INSERT INTO agent_owners(agent_name, instance_id, since)
		 SELECT name, $2, now() FROM agents WHERE base_domain=$1
		 ON CONFLICT(agent_name) DO UPDATE SET instance_id = excluded.instance_id, since = excluded.since`,
		baseDomain, instanceID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrBadToken
	}
	return notify(s.db, chanOwners, baseDomain)
}

// ClearOwner drops baseDomain's owner row only while instanceID still holds
// it, so a relay whose half-open session dies late never removes the new
// owner's row. Clearing a row someone else holds is a silent no-op.
func (s *Store) ClearOwner(baseDomain, instanceID string) error {
	res, err := s.db.Exec(
		`DELETE FROM agent_owners WHERE instance_id=$2
		    AND agent_name = (SELECT name FROM agents WHERE base_domain=$1)`,
		baseDomain, instanceID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	return notify(s.db, chanOwners, baseDomain)
}

// OwnerOf returns the live instance holding baseDomain's tunnel. ok is false
// when nobody does, or when the recorded owner has stopped heartbeating.
func (s *Store) OwnerOf(baseDomain string) (InstanceRow, bool, error) {
	r, err := scanInstance(s.db.QueryRow(
		`SELECT i.id, i.started_at, i.sessions, i.tls_addr, i.http_addr, i.tunnel_addr, i.api_addr
		   FROM agent_owners o
		   JOIN agents a ON a.name = o.agent_name
		   JOIN relay_instances i ON i.id = o.instance_id
		  WHERE a.base_domain=$1 AND i.`+liveWhere, baseDomain))
	if errors.Is(err, sql.ErrNoRows) {
		return InstanceRow{}, false, nil
	}
	if err != nil {
		return InstanceRow{}, false, err
	}
	return r, true, nil
}

// Owners maps every agent base domain to the id of its live owner.
func (s *Store) Owners() (map[string]string, error) {
	rows, err := s.db.Query(
		`SELECT a.base_domain, o.instance_id
		   FROM agent_owners o
		   JOIN agents a ON a.name = o.agent_name
		   JOIN relay_instances i ON i.id = o.instance_id
		  WHERE i.` + liveWhere)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var base, id string
		if err := rows.Scan(&base, &id); err != nil {
			return nil, err
		}
		out[base] = id
	}
	return out, rows.Err()
}
```

(`i.` + liveWhere` works because `liveWhere` begins with the bare column name `last_seen`.)

Create `internal/relay/instance.go` with just the type this task needs (the rest arrives in Task 5):

```go
package relay

import "time"

// Instance is this relay process's identity in the pool: a random id per
// start plus the addresses an edge dials to reach each of its listeners.
type Instance struct {
	ID         string
	StartedAt  time.Time
	TLSAddr    string
	HTTPAddr   string
	TunnelAddr string
	APIAddr    string
}

// row is the heartbeat payload: the identity plus the current session count.
func (i *Instance) row(sessions int) InstanceRow {
	return InstanceRow{ID: i.ID, StartedAt: i.StartedAt, Sessions: sessions,
		TLSAddr: i.TLSAddr, HTTPAddr: i.HTTPAddr, TunnelAddr: i.TunnelAddr, APIAddr: i.APIAddr}
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/relay/ -run 'Instance|Owner|PurgeDead' -count=1 -v`
Expected: all PASS (not SKIP — confirm Docker/Postgres was available).

- [ ] **Step 6: Verify and commit**

Run: `make verify` (exit 0).

```bash
git add internal/relay/schema.sql internal/relay/ownership.go internal/relay/instance.go internal/relay/ownership_test.go
git commit -m "feat(relay): relay_instances and agent_owners tables with liveness predicate

Part of #CHILD

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 3: Hostname → agent lookups for the edge

**Files:**
- Modify: `internal/relay/ownership.go`
- Test: `internal/relay/ownership_test.go`

**Interfaces:**
- Produces: `(*Store).AgentForHost(host string) (base string, ok bool, err error)` — exact terminated hostname → custom domain (exact/subdomain) → agent base domain (exact/subdomain), the `handlePublic` precedence; `(*Store).AgentForCustomHost(host string) (string, bool, error)` — custom domains only, the `:80` rule.

- [ ] **Step 1: Write the failing tests** (append to `ownership_test.go`)

```go
func TestAgentForHostPrecedence(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	base := en.BaseDomain
	host, err := st.RegisterHostname(base, "blog", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddCustomDomain(base, "shop.example.com"); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		host string
		want string
		ok   bool
	}{
		{"terminated hostname", host, base, true},
		{"custom exact", "shop.example.com", base, true},
		{"custom subdomain", "www.shop.example.com", base, true},
		{"base exact", base, base, true},
		{"base subdomain", "app." + base, base, true},
		{"unknown", "nobody.example.net", "", false},
		{"suffix without dot", "x" + base, "", false},
	}
	for _, c := range cases {
		got, ok, err := st.AgentForHost(c.host)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want || ok != c.ok {
			t.Errorf("%s: AgentForHost(%q) = %q,%v want %q,%v", c.name, c.host, got, ok, c.want, c.ok)
		}
	}
}

func TestAgentForCustomHostMatchesCustomDomainsOnly(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	base := en.BaseDomain
	if err := st.AddCustomDomain(base, "shop.example.com"); err != nil {
		t.Fatal(err)
	}
	if got, ok, _ := st.AgentForCustomHost("www.shop.example.com"); !ok || got != base {
		t.Fatalf("custom subdomain = %q,%v want %q,true", got, ok, base)
	}
	if _, ok, _ := st.AgentForCustomHost("app." + base); ok {
		t.Fatal("shared-domain host matched the :80 rule")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/relay/ -run 'AgentFor' -count=1`
Expected: compile error `st.AgentForHost undefined`.

- [ ] **Step 3: Implement** (append to `ownership.go`)

```go
// AgentForHost resolves a public hostname to the base domain of the agent
// that serves it, in the order handlePublic routes: an exact relay-terminated
// hostname, then a BYO custom domain (exact or subdomain), then an agent base
// domain (exact or subdomain). Longest match wins within a class.
func (s *Store) AgentForHost(host string) (string, bool, error) {
	if base, ok, err := s.scanOne(
		`SELECT a.base_domain FROM hostnames h JOIN agents a ON a.name = h.agent_name WHERE h.hostname=$1`, host); ok || err != nil {
		return base, ok, err
	}
	if base, ok, err := s.AgentForCustomHost(host); ok || err != nil {
		return base, ok, err
	}
	return s.scanOne(
		`SELECT base_domain FROM agents
		  WHERE base_domain=$1 OR right($1, length(base_domain)+1) = '.' || base_domain
		  ORDER BY length(base_domain) DESC LIMIT 1`, host)
}

// AgentForCustomHost is AgentForHost restricted to custom domains — the :80
// rule (Router.LookupCustom), so shared-domain hosts never route over plain
// HTTP.
func (s *Store) AgentForCustomHost(host string) (string, bool, error) {
	return s.scanOne(
		`SELECT agent_base FROM custom_domains
		  WHERE domain=$1 OR right($1, length(domain)+1) = '.' || domain
		  ORDER BY length(domain) DESC LIMIT 1`, host)
}

func (s *Store) scanOne(query, arg string) (string, bool, error) {
	var v string
	err := s.db.QueryRow(query, arg).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/relay/ -run 'AgentFor' -count=1 -v`
Expected: PASS.

- [ ] **Step 5: Verify and commit**

Run: `make verify` (exit 0).

```bash
git add internal/relay/ownership.go internal/relay/ownership_test.go
git commit -m "feat(relay): hostname-to-agent lookups with the :443/:80 precedence

Part of #CHILD

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 4: NOTIFY from every mutator + the LISTEN connection

**Files:**
- Create: `internal/relay/notify.go`
- Modify: `internal/relay/store.go` (keep the DSN), `internal/relay/hostnames.go`, `internal/relay/domains.go`, `internal/relay/delivery.go`
- Test: `internal/relay/notify_test.go`

**Interfaces:**
- Consumes: `notify`, channel constants (Task 2).
- Produces: `listen(ctx context.Context, dsn string, channels []string, resync func(), handle func(channel, payload string))` — blocks until ctx is done; `Store.dsn` field.

- [ ] **Step 1: Write the failing tests**

`internal/relay/notify_test.go`:

```go
package relay

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

type notification struct{ channel, payload string }

// startListener runs listen on st's DSN for channels and returns the
// notification stream plus a counter of resyncs (one per connect).
func startListener(t *testing.T, st *Store, channels ...string) (<-chan notification, *atomic.Int32) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	got := make(chan notification, 64)
	var resyncs atomic.Int32
	ready := make(chan struct{}, 8)
	go listen(ctx, st.dsn, channels,
		func() { resyncs.Add(1); ready <- struct{}{} },
		func(channel, payload string) { got <- notification{channel, payload} })
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("listener never connected")
	}
	return got, &resyncs
}

func expectNotification(t *testing.T, got <-chan notification, channel, payload string) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case n := <-got:
			if n.channel == channel && n.payload == payload {
				return
			}
			// Other channels' traffic (e.g. a heartbeat) is fine to skip past.
		case <-deadline:
			t.Fatalf("no NOTIFY on %s with payload %q", channel, payload)
		}
	}
}

func TestEveryMutatorNotifiesItsChannel(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	base := en.BaseDomain
	got, _ := startListener(t, st, chanInstances, chanOwners, chanHostnames, chanEvents)

	stampInstance(t, st, "a", "127.0.0.1:1", time.Now())
	expectNotification(t, got, chanInstances, "a")

	if err := st.SetOwner(base, "a"); err != nil {
		t.Fatal(err)
	}
	expectNotification(t, got, chanOwners, base)
	if err := st.ClearOwner(base, "a"); err != nil {
		t.Fatal(err)
	}
	expectNotification(t, got, chanOwners, base)

	host, err := st.RegisterHostname(base, "blog", 0)
	if err != nil {
		t.Fatal(err)
	}
	expectNotification(t, got, chanHostnames, host)
	if err := st.DeregisterHostname(base, host); err != nil {
		t.Fatal(err)
	}
	expectNotification(t, got, chanHostnames, host)

	if err := st.AddCustomDomain(base, "shop.example.com"); err != nil {
		t.Fatal(err)
	}
	expectNotification(t, got, chanHostnames, "shop.example.com")
	if err := st.ConfirmCustomDomain(base, "shop.example.com"); err != nil {
		t.Fatal(err)
	}
	expectNotification(t, got, chanHostnames, "shop.example.com")
	if _, err := st.removeCustomDomainOwned(base, "shop.example.com"); err != nil {
		t.Fatal(err)
	}
	expectNotification(t, got, chanHostnames, "shop.example.com")

	if err := st.ParkEvent(base, "blog", "main", "push", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	expectNotification(t, got, chanEvents, base)

	if err := st.DeleteInstance("a"); err != nil {
		t.Fatal(err)
	}
	expectNotification(t, got, chanInstances, "a")
}

func TestReconcileNotifiesPrunedHostnames(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	host, err := st.RegisterHostname(en.BaseDomain, "old", 0)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := startListener(t, st, chanHostnames)
	if _, pruned, err := st.ReconcileHostnames(en.BaseDomain, nil); err != nil || len(pruned) != 1 {
		t.Fatalf("reconcile: pruned=%v err=%v", pruned, err)
	}
	expectNotification(t, got, chanHostnames, host)
}

func TestListenReconnectsAndResyncs(t *testing.T) {
	st := openTestStore(t)
	got, resyncs := startListener(t, st, "piper_test_reconnect")

	// Kill the listener's backend from another connection; the last query
	// it ran is its final LISTEN, which is how pg_stat_activity finds it.
	if _, err := st.db.Exec(
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE query = 'LISTEN piper_test_reconnect'`); err != nil {
		t.Fatal(err)
	}
	waitCond(t, 10*time.Second, "listener resynced after reconnect", func() bool { return resyncs.Load() >= 2 })

	if err := notify(st.db, "piper_test_reconnect", "after"); err != nil {
		t.Fatal(err)
	}
	expectNotification(t, got, "piper_test_reconnect", "after")
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/relay/ -run 'Notif|Listen|Reconcile' -count=1`
Expected: compile error `undefined: listen` / `st.dsn undefined`.

- [ ] **Step 3: Keep the DSN on the store**

In `internal/relay/store.go`, add `dsn string` to the `Store` struct (first field, with the comment `// dsn is what listen dials for LISTEN; the pool cannot.`) and change the last line of `Open` to `return &Store{db: db, dsn: dsn, nowFunc: time.Now}, nil`.

- [ ] **Step 4: Write `notify.go`**

```go
package relay

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
)

// listen holds one dedicated connection LISTENing on channels for the life
// of ctx and calls handle for every notification. The store's database/sql
// pool cannot LISTEN (a notification arrives on one specific connection), so
// this dials pgx directly on the same DSN. On every connect — first and
// after each drop — it calls resync before handling anything, so a NOTIFY
// missed while disconnected costs one full reload, never correctness.
// Reconnects back off from 1 s to 15 s. Channel names are package constants,
// never user input.
func listen(ctx context.Context, dsn string, channels []string, resync func(), handle func(channel, payload string)) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := listenOnce(ctx, dsn, channels, resync, handle)
		if ctx.Err() != nil {
			return
		}
		log.Printf("relay: notify listener lost (%v); reconnecting in %s", err, backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		if backoff *= 2; backoff > 15*time.Second {
			backoff = 15 * time.Second
		}
	}
}

func listenOnce(ctx context.Context, dsn string, channels []string, resync func(), handle func(channel, payload string)) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	for _, ch := range channels {
		if _, err := conn.Exec(ctx, "LISTEN "+ch); err != nil {
			return err
		}
	}
	resync()
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		handle(n.Channel, n.Payload)
	}
}
```

- [ ] **Step 5: Fire the channels from the mutators**

`internal/relay/hostnames.go`:
- `RegisterHostname`: after the `INSERT INTO hostnames(...)` succeeds and before `tx.Commit()`, add:
  ```go
  	if err := notify(tx, chanHostnames, hostname); err != nil {
  		return "", err
  	}
  ```
- `ReconcileHostnames`: inside the `for _, sl := range stale` loop, after the DELETE succeeds and before `pruned = append(...)`, add:
  ```go
  		if err := notify(s.db, chanHostnames, sl.hostname); err != nil {
  			return nil, nil, err
  		}
  ```
- `DeregisterHostname`: replace the final two lines with:
  ```go
  	if _, err := s.db.Exec(`DELETE FROM hostnames WHERE agent_name=$1 AND hostname=$2`, agentName, hostname); err != nil {
  		return err
  	}
  	return notify(s.db, chanHostnames, hostname)
  ```

`internal/relay/domains.go`:
- `AddCustomDomain`: replace the final `return tx.Commit()` (after the INSERT) with:
  ```go
  	if err := notify(tx, chanHostnames, domain); err != nil {
  		return err
  	}
  	return tx.Commit()
  ```
  The earlier `return tx.Commit() // own active row: no-op re-add` stays as is — nothing changed.
- `ConfirmCustomDomain`: replace the trailing `return nil` with `return notify(s.db, chanHostnames, domain)`.
- `removeCustomDomainOwned`: replace `return n > 0, nil` with:
  ```go
  	if n == 0 {
  		return false, nil
  	}
  	return true, notify(s.db, chanHostnames, domain)
  ```

`internal/relay/delivery.go`, `ParkEvent`: replace the final `return s.evictOldestPending(agentName)` with:
```go
	if err := s.evictOldestPending(agentName); err != nil {
		return err
	}
	return notify(s.db, chanEvents, agentName)
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/relay/ -run 'Notif|Listen|Reconcile' -count=1 -v`
Expected: PASS for all three.

- [ ] **Step 7: Verify and commit**

Run: `make verify` (exit 0).

```bash
git add internal/relay/notify.go internal/relay/notify_test.go internal/relay/store.go internal/relay/hostnames.go internal/relay/domains.go internal/relay/delivery.go
git commit -m "feat(relay): NOTIFY on instance, owner, hostname and event changes; LISTEN helper

Part of #CHILD

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 5: Instance identity and heartbeat

**Files:**
- Modify: `internal/relay/instance.go`
- Modify: `internal/relay/watchdog_test.go` (TestMain: fast heartbeat)
- Test: `internal/relay/instance_test.go`

**Interfaces:**
- Consumes: `InstanceRow`, `UpsertInstance`, `DeleteInstance`, `Router.Counts()`.
- Produces: `NewInstance(advertiseHost, tlsAddr, httpAddr, tunnelAddr, apiAddr string) (*Instance, error)`; `defaultAdvertiseHost() (string, error)`; `(*Instance).heartbeat(ctx, st, router)`; package var `heartbeatInterval`.

- [ ] **Step 1: Write the failing tests**

`internal/relay/instance_test.go`:

```go
package relay

import (
	"context"
	"testing"
	"time"
)

func TestNewInstanceAdvertisesHostWithListenerPorts(t *testing.T) {
	inst, err := NewInstance("10.0.0.7", ":443", "0.0.0.0:80", "127.0.0.1:7000", ":8080")
	if err != nil {
		t.Fatal(err)
	}
	if inst.ID == "" || inst.StartedAt.IsZero() {
		t.Fatalf("identity not minted: %+v", inst)
	}
	want := map[string]string{"tls": "10.0.0.7:443", "http": "10.0.0.7:80", "tunnel": "10.0.0.7:7000", "api": "10.0.0.7:8080"}
	got := map[string]string{"tls": inst.TLSAddr, "http": inst.HTTPAddr, "tunnel": inst.TunnelAddr, "api": inst.APIAddr}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s addr = %q, want %q", k, got[k], w)
		}
	}
	other, _ := NewInstance("10.0.0.7", ":443", ":80", ":7000", ":8080")
	if other.ID == inst.ID {
		t.Fatal("two instances share an id")
	}
}

func TestNewInstanceRejectsAddrWithoutPort(t *testing.T) {
	if _, err := NewInstance("10.0.0.7", "443", ":80", ":7000", ":8080"); err == nil {
		t.Fatal("bare port accepted")
	}
}

func TestDefaultAdvertiseHostIsNonLoopbackIPv4(t *testing.T) {
	host, err := defaultAdvertiseHost()
	if err != nil {
		t.Skip("no non-loopback IPv4 on this machine:", err)
	}
	if host == "" || host == "127.0.0.1" {
		t.Fatalf("advertise host = %q", host)
	}
}

func TestHeartbeatPublishesSessionsAndLeavesOnStop(t *testing.T) {
	st := openTestStore(t)
	router := NewRouter()
	relaySess, _ := pipeSession(t, "box.public.getpiper.co")
	router.Register(relaySess)
	inst, err := NewInstance("127.0.0.1", ":443", ":80", ":7000", ":8080")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { inst.heartbeat(ctx, st, router); close(done) }()

	waitCond(t, 3*time.Second, "heartbeat row with one session", func() bool {
		rows, _ := st.LiveInstances()
		return len(rows) == 1 && rows[0].ID == inst.ID && rows[0].Sessions == 1 && rows[0].TLSAddr == "127.0.0.1:443"
	})
	cancel()
	<-done
	if rows, _ := st.LiveInstances(); len(rows) != 0 {
		t.Fatalf("row survived a clean stop: %+v", rows)
	}
}
```

Add to `TestMain` in `internal/relay/watchdog_test.go`, right after `disabledPollInterval = 20 * time.Millisecond`:

```go
	heartbeatInterval = 20 * time.Millisecond
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/relay/ -run 'Instance|Heartbeat|AdvertiseHost' -count=1`
Expected: compile error `undefined: NewInstance` / `heartbeatInterval`.

- [ ] **Step 3: Implement** (replace `internal/relay/instance.go` with)

```go
package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"net"
	"time"
)

// heartbeatInterval is how often a relay refreshes its relay_instances row —
// a third of instanceTTL, so two missed beats still read as live. A package
// var (cf. disabledPollInterval) so tests tick fast; production leaves it at 5s.
var heartbeatInterval = 5 * time.Second

// Instance is this relay process's identity in the pool: a random id per
// start plus the addresses an edge dials to reach each of its listeners.
type Instance struct {
	ID         string
	StartedAt  time.Time
	TLSAddr    string
	HTTPAddr   string
	TunnelAddr string
	APIAddr    string
}

// NewInstance mints an instance identity. advertiseHost is the host an edge
// dials ("" ⇒ the first non-loopback IPv4, which in a container or pod is
// the address an edge can actually reach); the four addrs are the relay's
// own listener addresses, of which only the port is kept.
func NewInstance(advertiseHost, tlsAddr, httpAddr, tunnelAddr, apiAddr string) (*Instance, error) {
	if advertiseHost == "" {
		h, err := defaultAdvertiseHost()
		if err != nil {
			return nil, err
		}
		advertiseHost = h
	}
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, err
	}
	inst := &Instance{ID: hex.EncodeToString(raw[:]), StartedAt: time.Now().UTC()}
	for _, a := range []struct {
		dst  *string
		addr string
	}{{&inst.TLSAddr, tlsAddr}, {&inst.HTTPAddr, httpAddr}, {&inst.TunnelAddr, tunnelAddr}, {&inst.APIAddr, apiAddr}} {
		_, port, err := net.SplitHostPort(a.addr)
		if err != nil {
			return nil, err
		}
		*a.dst = net.JoinHostPort(advertiseHost, port)
	}
	return inst, nil
}

// defaultAdvertiseHost picks the first non-loopback IPv4 on any interface.
func defaultAdvertiseHost() (string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() {
			continue
		}
		if ip4 := ipn.IP.To4(); ip4 != nil {
			return ip4.String(), nil
		}
	}
	return "", errors.New("no non-loopback IPv4 address; set PIPER_RELAY_ADVERTISE_HOST")
}

// row is the heartbeat payload: the identity plus the current session count.
func (i *Instance) row(sessions int) InstanceRow {
	return InstanceRow{ID: i.ID, StartedAt: i.StartedAt, Sessions: sessions,
		TLSAddr: i.TLSAddr, HTTPAddr: i.HTTPAddr, TunnelAddr: i.TunnelAddr, APIAddr: i.APIAddr}
}

// heartbeat upserts the instance row now and every heartbeatInterval until
// ctx is done, then deletes it: a clean stop leaves routing at once instead
// of after instanceTTL. The session count is the router's live agent total.
func (i *Instance) heartbeat(ctx context.Context, st *Store, router *Router) {
	beat := func() {
		agents, _, _ := router.Counts()
		if err := st.UpsertInstance(i.row(agents)); err != nil {
			log.Printf("relay: heartbeat: %v", err)
		}
	}
	beat()
	tick := time.NewTicker(heartbeatInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := st.DeleteInstance(i.ID); err != nil {
				log.Printf("relay: leave pool: %v", err)
			}
			return
		case <-tick.C:
			beat()
		}
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/relay/ -run 'Instance|Heartbeat|AdvertiseHost' -count=1 -v`
Expected: PASS.

- [ ] **Step 5: Verify and commit**

Run: `make verify` (exit 0).

```bash
git add internal/relay/instance.go internal/relay/instance_test.go internal/relay/watchdog_test.go
git commit -m "feat(relay): instance identity with advertised addrs and a 5s heartbeat

Part of #CHILD

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 6: Ownership writes on tunnel register/unregister

**Files:**
- Modify: `internal/relay/server.go` (`Serve`, `acceptTunnels`, `serveTunnel`)
- Modify: existing callers — `cmd/piper-relay/main.go:334`, `internal/relay/proxyproto_test.go:279` (`Serve`), and the nine `acceptTunnels`/`serveTunnel` test call sites listed by `grep -n "serveTunnel(\|acceptTunnels(" internal/relay/*_test.go`
- Test: `internal/relay/server_test.go`

**Interfaces:**
- Consumes: `*Instance`, `SetOwner`, `ClearOwner`, `testInstance` (Task 2).
- Produces: `Serve(tlsAddr, httpAddr, tunnelAddr string, st *Store, tlsCfg *tls.Config, router *Router, ctrl http.Handler, ghApp *GitHubApp, delivery *TunnelDelivery, m *Metrics, inst *Instance, proxyProto bool) error`; `acceptTunnels(ln, st, router, ghApp, delivery, m, inst)`; `serveTunnel(conn, st, router, disabled, ghApp, delivery, inst)`.

- [ ] **Step 1: Write the failing tests** (append to `server_test.go`)

```go
// dialTestTunnel runs serveTunnel for inst on one end of a pipe and dials
// the agent side, returning the agent's session once the relay holds it.
func dialTestTunnel(t *testing.T, st *Store, router *Router, inst *Instance, en Enrollment) *tunnel.Session {
	t.Helper()
	cc, sc := net.Pipe()
	t.Cleanup(func() { cc.Close(); sc.Close() })
	go serveTunnel(sc, st, router, st.AgentDisabled, nil, nil, inst)
	sess, err := tunnel.Dial(cc, en.Token, en.BaseDomain)
	if err != nil {
		t.Fatal(err)
	}
	waitCond(t, 3*time.Second, "session registered", func() bool {
		_, ok := router.Lookup(en.BaseDomain)
		return ok
	})
	return sess
}

func TestServeTunnelRecordsAndClearsOwner(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	inst := testInstance(t, st)
	router := NewRouter()

	sess := dialTestTunnel(t, st, router, inst, en)
	waitCond(t, 3*time.Second, "owner row written", func() bool {
		r, ok, _ := st.OwnerOf(en.BaseDomain)
		return ok && r.ID == inst.ID
	})
	sess.Close()
	waitCond(t, 3*time.Second, "owner row cleared", func() bool {
		_, ok, _ := st.OwnerOf(en.BaseDomain)
		return !ok
	})
}

func TestServeTunnelNeverClearsAnotherRelaysOwnership(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	mine := testInstance(t, st)
	other := testInstance(t, st)
	router := NewRouter()

	sess := dialTestTunnel(t, st, router, mine, en)
	waitCond(t, 3*time.Second, "owner row written", func() bool {
		r, ok, _ := st.OwnerOf(en.BaseDomain)
		return ok && r.ID == mine.ID
	})
	// The agent reconnected elsewhere while our half-open session lingers.
	if err := st.SetOwner(en.BaseDomain, other.ID); err != nil {
		t.Fatal(err)
	}
	sess.Close()
	waitCond(t, 3*time.Second, "stale session unregistered", func() bool {
		_, ok := router.Lookup(en.BaseDomain)
		return !ok
	})
	if r, ok, _ := st.OwnerOf(en.BaseDomain); !ok || r.ID != other.ID {
		t.Fatalf("owner after stale unregister = %+v ok=%v, want %s", r, ok, other.ID)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/relay/ -run 'ServeTunnel' -count=1`
Expected: compile error — `too many arguments in call to serveTunnel`.

- [ ] **Step 3: Thread the instance through `Serve` and write ownership**

In `internal/relay/server.go`:

1. `Serve` signature: insert `inst *Instance` before `proxyProto bool`. Add to its doc comment: `inst is this process's pool identity; every tunnel it registers is recorded as owned by inst.ID so an edge can route to it.`
2. `go acceptTunnels(tunLn, st, router, ghApp, delivery, m)` → `go acceptTunnels(tunLn, st, router, ghApp, delivery, m, inst)`.
3. `acceptTunnels` signature gains `inst *Instance` last; its `go serveTunnel(conn, st, router, st.AgentDisabled, ghApp, delivery)` gains `, inst`.
4. `serveTunnel` signature gains `inst *Instance` last. Body changes:

   After `router.Register(sess)`:
   ```go
   	if err := st.SetOwner(sess.BaseDomain, inst.ID); err != nil {
   		log.Printf("agent %s: record owner: %v", sess.BaseDomain, err)
   	}
   ```
   In the post-register kill path, replace `router.Unregister(sess)` with:
   ```go
   		router.Unregister(sess)
   		clearOwner(st, sess.BaseDomain, inst)
   ```
   In the `case <-sess.CloseChan():` branch, after `router.Unregister(sess)` add `clearOwner(st, sess.BaseDomain, inst)`.

   Add below `serveTunnel`:
   ```go
   // clearOwner releases baseDomain's owner row if inst still holds it. The
   // store's WHERE instance_id = inst.ID is what makes a late unregister from a
   // half-open session harmless after the agent reconnected elsewhere.
   func clearOwner(st *Store, baseDomain string, inst *Instance) {
   	if err := st.ClearOwner(baseDomain, inst.ID); err != nil {
   		log.Printf("agent %s: release owner: %v", baseDomain, err)
   	}
   }
   ```

- [ ] **Step 4: Update every caller**

- `cmd/piper-relay/main.go:334`: temporarily pass `nil` is **not** acceptable (`inst.ID` would panic). Task 9 wires the real instance; for this task make `main` compile by building the instance now with the minimal form:
  ```go
  	inst, err := relay.NewInstance(env("PIPER_RELAY_ADVERTISE_HOST", ""), tlsAddr, httpAddr, tunnelAddr, apiAddr)
  	if err != nil {
  		log.Fatalf("instance: %v", err)
  	}
  ```
  placed right after the four `xxxAddr := env(...)` lines, and `relay.Serve(..., metrics, inst, proxyProto)`.
- `internal/relay/proxyproto_test.go:279`: `Serve(tlsAddr, httpAddr, tunnelAddr, st, tlsCfg, router, ctrl, nil, nil, nil, testInstance(t, st), true)`.
- Each of the nine `acceptTunnels(...)`/`serveTunnel(...)` test call sites: append `, testInstance(t, st)` as the last argument (each of those tests already has `st` in scope).

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/relay/ -count=1` and `go build ./...`
Expected: PASS across the package (the existing accept/watchdog/proxyproto tests still pass with the extra argument).

- [ ] **Step 6: Verify and commit**

Run: `make verify` (exit 0).

```bash
git add internal/relay/server.go internal/relay/server_test.go internal/relay/proxyproto_test.go internal/relay/accepttunnels_test.go internal/relay/watchdog_test.go cmd/piper-relay/main.go
git commit -m "feat(relay): record tunnel ownership on register, release it conditionally on unregister

Part of #CHILD

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 7: Relay-side watcher — stale-session close and NOTIFY-woken drain

**Files:**
- Modify: `internal/relay/router.go` (+ `Holds`, `Bases`)
- Modify: `internal/relay/instance.go` (+ `RunInstance`)
- Test: `internal/relay/router_test.go`, `internal/relay/instance_test.go`

**Interfaces:**
- Consumes: `listen` (Task 4), `heartbeat` (Task 5), `TunnelDelivery.Dispatch`/`DrainFor`.
- Produces: `(*Router).Holds(base string) (*tunnel.Session, bool)` — exact base-domain match, no suffix walk; `(*Router).Bases() []string` — agent base domains held (no custom domains); `RunInstance(ctx context.Context, st *Store, inst *Instance, router *Router, delivery *TunnelDelivery)` — blocks until ctx is done and the row is deleted; `delivery` may be nil.

- [ ] **Step 1: Write the failing tests**

Append to `internal/relay/router_test.go`:

```go
func TestHoldsIsExactAndBasesListsAgentsOnly(t *testing.T) {
	r := NewRouter()
	relaySess, _ := pipeSession(t, "box.public.getpiper.co")
	r.Register(relaySess)
	r.RegisterCustom("shop.example.com", relaySess)

	if _, ok := r.Holds("box.public.getpiper.co"); !ok {
		t.Fatal("Holds missed the registered base")
	}
	if _, ok := r.Holds("app.box.public.getpiper.co"); ok {
		t.Fatal("Holds walked a suffix; it must be exact")
	}
	if got := r.Bases(); len(got) != 1 || got[0] != "box.public.getpiper.co" {
		t.Fatalf("Bases() = %v, want the one agent base", got)
	}
}
```

Append to `internal/relay/instance_test.go`:

```go
func TestRunInstanceClosesSessionOwnedElsewhere(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	mine, err := NewInstance("127.0.0.1", ":443", ":80", ":7000", ":8080")
	if err != nil {
		t.Fatal(err)
	}
	other := testInstance(t, st)
	router := NewRouter()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go RunInstance(ctx, st, mine, router, nil)
	waitCond(t, 3*time.Second, "instance live", func() bool {
		rows, _ := st.LiveInstances()
		for _, r := range rows {
			if r.ID == mine.ID {
				return true
			}
		}
		return false
	})

	dialTestTunnel(t, st, router, mine, en)
	waitCond(t, 3*time.Second, "owned by mine", func() bool {
		r, ok, _ := st.OwnerOf(en.BaseDomain)
		return ok && r.ID == mine.ID
	})
	// The agent reconnected on another live relay: our copy must go.
	if err := st.SetOwner(en.BaseDomain, other.ID); err != nil {
		t.Fatal(err)
	}
	waitCond(t, 3*time.Second, "stale session closed", func() bool {
		_, ok := router.Holds(en.BaseDomain)
		return !ok
	})
	if r, ok, _ := st.OwnerOf(en.BaseDomain); !ok || r.ID != other.ID {
		t.Fatalf("owner after stale close = %+v ok=%v, want %s", r, ok, other.ID)
	}
}

func TestRunInstanceDrainsParkedEventsOnNotify(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	inst, err := NewInstance("127.0.0.1", ":443", ":80", ":7000", ":8080")
	if err != nil {
		t.Fatal(err)
	}
	router := NewRouter()
	delivery := NewTunnelDelivery(st, router)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go RunInstance(ctx, st, inst, router, delivery)

	sess := dialTestTunnel(t, st, router, inst, en)
	bodies := make(chan string, 4)
	go func() {
		for {
			kind, stream, err := sess.AcceptKind()
			if err != nil {
				return
			}
			if kind != tunnel.KindHTTP {
				stream.Close()
				continue
			}
			req, err := http.ReadRequest(bufio.NewReader(stream))
			if err != nil {
				stream.Close()
				return
			}
			body, _ := io.ReadAll(req.Body)
			bodies <- string(body)
			_, _ = io.WriteString(stream, "HTTP/1.1 202 Accepted\r\nContent-Length: 0\r\n\r\n")
			stream.Close()
		}
	}()

	// Parked by "another relay" (any store on the same database).
	if err := st.ParkEvent(en.BaseDomain, "blog", "main", "push", []byte(`{"after":"x"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-bodies:
		if got != `{"after":"x"}` {
			t.Fatalf("drained %s", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("owner never drained the parked event after NOTIFY")
	}
}
```

Add `"bufio"`, `"io"`, `"net/http"` and `"github.com/piperbox/piper/internal/tunnel"` to `instance_test.go`'s imports.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/relay/ -run 'Holds|RunInstance' -count=1`
Expected: compile error `r.Holds undefined` / `undefined: RunInstance`.

- [ ] **Step 3: Add `Holds` and `Bases` to `router.go`** (after `Lookup`)

```go
// Holds reports the session registered for exactly base — no suffix walk.
// It is what cluster-wide bookkeeping keys on: a NOTIFY payload is an exact
// base domain, and a suffix match could name a different agent.
func (r *Router) Holds(base string) (*tunnel.Session, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.byBase[base]
	return s, ok
}

// Bases lists the agent base domains this router holds (custom domains,
// which share byBase, are excluded).
func (r *Router) Bases() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	for base := range r.byBase {
		if _, custom := r.custom[base]; !custom {
			out = append(out, base)
		}
	}
	return out
}
```

- [ ] **Step 4: Add `RunInstance` to `instance.go`**

```go
// RunInstance keeps this relay in the pool and reacts to the cluster. It
// heartbeats until ctx is done, then leaves. Meanwhile it LISTENs on two
// channels: a piper_owners payload naming an agent this process holds that
// is now owned by another live instance closes the stale session (the agent
// has reconnected elsewhere; a late webhook drain or control answer from
// here would be wrong), and a piper_events payload naming an agent this
// process holds drains its parked webhooks. On every listener (re)connect the
// same checks run over every held agent, so a NOTIFY missed while
// disconnected is caught up. delivery may be nil (no GitHub App).
func RunInstance(ctx context.Context, st *Store, inst *Instance, router *Router, delivery *TunnelDelivery) {
	handle := func(channel, base string) {
		sess, ok := router.Holds(base)
		if !ok {
			return
		}
		switch channel {
		case chanOwners:
			owner, live, err := st.OwnerOf(base)
			if err != nil || !live || owner.ID == inst.ID {
				return
			}
			log.Printf("agent %s reconnected on %s; closing stale session", base, owner.ID)
			sess.Close()
		case chanEvents:
			if delivery != nil {
				delivery.Dispatch(func(ctx context.Context) { delivery.DrainFor(ctx, base) })
			}
		}
	}
	resync := func() {
		for _, base := range router.Bases() {
			handle(chanOwners, base)
			handle(chanEvents, base)
		}
	}
	go listen(ctx, st.dsn, []string{chanOwners, chanEvents}, resync, handle)
	inst.heartbeat(ctx, st, router)
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/relay/ -run 'Holds|RunInstance' -count=1 -v`
Expected: PASS.

- [ ] **Step 6: Verify and commit**

Run: `make verify` (exit 0).

```bash
git add internal/relay/router.go internal/relay/router_test.go internal/relay/instance.go internal/relay/instance_test.go
git commit -m "feat(relay): RunInstance closes sessions owned elsewhere and drains on piper_events

Part of #CHILD

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 8: Control hop to the owner and cluster-wide liveness

**Files:**
- Modify: `internal/relay/proxy.go` (`NewControlProxy`), `internal/relay/api.go` (`NewAPIWithTunnel`, `NewAPI`)
- Modify: the 13 test call sites listed by `grep -n "NewAPIWithTunnel(" internal/relay/*_test.go` and `cmd/piper-relay/main.go:288`
- Test: `internal/relay/proxy_test.go`

**Interfaces:**
- Consumes: `OwnerOf`, `*Instance`, `stampInstance`.
- Produces: `NewControlProxy(st *Store, router *Router, self *Instance) http.Handler`; `NewAPIWithTunnel(st, v, tunnelEndpoint, router, webRedirects, ghApp, self *Instance) http.Handler`. `self == nil` means single-process mode: no hop, liveness from the router only (what every existing test exercises).

- [ ] **Step 1: Write the failing tests** (append to `proxy_test.go`)

```go
// ownerEcho stands in for the owner relay's :8080: it echoes what arrived so
// a test can prove the hop forwarded the request unchanged.
func ownerEcho(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s %s auth=%s", r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestControlProxyHopsToLiveOwnerUnchanged(t *testing.T) {
	_, st, router, aliceCred, _, base := proxyFixture(t)
	self := stampInstance(t, st, "self", "127.0.0.1:1", time.Now())
	api := NewAPIWithTunnel(st, NewFakeVerifier(), "", router, nil, nil, self)
	owner := ownerEcho(t)
	stampInstance(t, st, "owner", owner.Listener.Addr().String(), time.Now())
	if err := st.SetOwner(base, "owner"); err != nil {
		t.Fatal(err)
	}

	rr := proxyGet(t, api, "/agents/"+base+"/v1/apps?x=1", aliceCred)
	if rr.Code != http.StatusOK {
		t.Fatalf("hop: %d %s", rr.Code, rr.Body.String())
	}
	want := "GET /agents/" + base + "/v1/apps?x=1 auth=Bearer " + aliceCred
	if rr.Body.String() != want {
		t.Fatalf("owner saw %q, want %q", rr.Body.String(), want)
	}
}

func TestControlProxyDoesNotHopToItselfOrADeadOwner(t *testing.T) {
	_, st, router, aliceCred, _, base := proxyFixture(t)
	self := stampInstance(t, st, "self", "127.0.0.1:1", time.Now())
	api := NewAPIWithTunnel(st, NewFakeVerifier(), "", router, nil, nil, self)

	// Owner row names us but the router misses (unregister race): 503, no loop.
	if err := st.SetOwner(base, "self"); err != nil {
		t.Fatal(err)
	}
	if rr := proxyGet(t, api, "/agents/"+base+"/v1/apps", aliceCred); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("self-owned miss: %d, want 503", rr.Code)
	}
	// Owner row names a relay that stopped heartbeating: 503, not a hop.
	owner := ownerEcho(t)
	stampInstance(t, st, "dead", owner.Listener.Addr().String(), time.Now())
	if err := st.SetOwner(base, "dead"); err != nil {
		t.Fatal(err)
	}
	ageInstance(t, st, "dead")
	if rr := proxyGet(t, api, "/agents/"+base+"/v1/apps", aliceCred); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("dead owner: %d, want 503", rr.Code)
	}
}

func TestControlProxyHopFailureIs502(t *testing.T) {
	_, st, router, aliceCred, _, base := proxyFixture(t)
	self := stampInstance(t, st, "self", "127.0.0.1:1", time.Now())
	api := NewAPIWithTunnel(st, NewFakeVerifier(), "", router, nil, nil, self)
	stampInstance(t, st, "owner", freeTCPAddr(t), time.Now()) // nothing listens there
	if err := st.SetOwner(base, "owner"); err != nil {
		t.Fatal(err)
	}
	if rr := proxyGet(t, api, "/agents/"+base+"/v1/apps", aliceCred); rr.Code != http.StatusBadGateway {
		t.Fatalf("unreachable owner: %d, want 502", rr.Code)
	}
}

func TestLivenessCountsARemoteOwner(t *testing.T) {
	_, st, router, aliceCred, _, base := proxyFixture(t)
	self := stampInstance(t, st, "self", "127.0.0.1:1", time.Now())
	api := NewAPIWithTunnel(st, NewFakeVerifier(), "", router, nil, nil, self)
	stampInstance(t, st, "owner", "127.0.0.1:1", time.Now())
	if err := st.SetOwner(base, "owner"); err != nil {
		t.Fatal(err)
	}

	rr := proxyGet(t, api, "/agents/"+base, aliceCred)
	var one struct {
		Connected bool `json:"connected"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &one); err != nil || !one.Connected {
		t.Fatalf("GET /agents/<base> = %d %s, want connected:true", rr.Code, rr.Body.String())
	}

	rr = proxyGet(t, api, "/agents", aliceCred)
	var list struct {
		Agents []struct {
			Connected bool `json:"connected"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil || len(list.Agents) != 1 || !list.Agents[0].Connected {
		t.Fatalf("GET /agents = %d %s, want one connected agent", rr.Code, rr.Body.String())
	}

	req := httptest.NewRequest(http.MethodDelete, "/agents/"+base, nil)
	req.Header.Set("Authorization", "Bearer "+aliceCred)
	del := httptest.NewRecorder()
	api.ServeHTTP(del, req)
	if del.Code != http.StatusConflict {
		t.Fatalf("DELETE while owned elsewhere = %d, want 409", del.Code)
	}
}
```

Add `"time"` to `proxy_test.go`'s imports.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/relay/ -run 'ControlProxyHop|ControlProxyDoesNot|LivenessCounts' -count=1`
Expected: compile error — `too many arguments in call to NewAPIWithTunnel`.

- [ ] **Step 3: Implement the hop in `proxy.go`**

Add after `routeFromContext`:

```go
// hopCtxKey carries the owner relay's api_addr for a hopped request.
type hopCtxKey struct{}

func withHop(ctx context.Context, apiAddr string) context.Context {
	return context.WithValue(ctx, hopCtxKey{}, apiAddr)
}

func hopFromContext(ctx context.Context) string {
	addr, _ := ctx.Value(hopCtxKey{}).(string)
	return addr
}
```

Change the signature to `func NewControlProxy(st *Store, router *Router, self *Instance) http.Handler` and extend its doc comment with:

```go
// With N relays an agent's tunnel may live in another process. self is this
// process's pool identity; when the router misses and Postgres names a
// different live owner, the request is forwarded unchanged — original path,
// original Authorization — to that relay's control API over plain HTTP on
// the internal network (:8080 is documented as "front with TLS"), where it
// is authenticated and rewritten exactly as here. The hop is taken only on a
// router miss with a live owner that is not self, so it cannot loop: the
// owner's own miss answers 503. Liveness answers (GET /agents, GET
// /agents/<base>, the DELETE refusal) count a live owner row as connected.
// self == nil is single-process mode: no hop, router-only liveness.
```

Inside `NewControlProxy`, after `rp := &httputil.ReverseProxy{...}`, add:

```go
	hop := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = hopFromContext(pr.Out.Context())
			pr.Out.Host = pr.In.Host
			// Everything else — path, query, Authorization — travels as is.
		},
		Transport:     &http.Transport{ResponseHeaderTimeout: responseHeaderTimeout},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("relay: control hop to %s failed: %v", hopFromContext(r.Context()), err)
			http.Error(w, "owner relay unreachable", http.StatusBadGateway)
		},
	}

	// liveOwner is the cluster half of liveness: the live instance holding
	// base, if it is not this process.
	liveOwner := func(base string) (InstanceRow, bool) {
		if self == nil {
			return InstanceRow{}, false
		}
		owner, ok, err := st.OwnerOf(base)
		if err != nil {
			log.Printf("relay: owner lookup for %s: %v", base, err)
			return InstanceRow{}, false
		}
		return owner, ok && owner.ID != self.ID
	}
	connected := func(base string) bool {
		if _, ok := router.Lookup(base); ok {
			return true
		}
		_, ok := liveOwner(base)
		return ok
	}
```

Then replace the three router-only liveness reads:
- in the `/agents` list loop: `_, connected := router.Lookup(a.BaseDomain)` → `connected := connected(a.BaseDomain)` (rename the local to `up` to avoid shadowing: `up := connected(a.BaseDomain)` and use `"connected": up`);
- `GET /agents/<base>`: `_, connected := router.Lookup(base)` → `up := connected(base)` and `"connected": up`;
- the DELETE guard: `if _, connected := router.Lookup(base); connected {` → `if connected(base) {`.

And the v1 forwarding block becomes:

```go
		sess, ok := router.Lookup(base)
		if !ok {
			if owner, ok := liveOwner(base); ok {
				hop.ServeHTTP(w, r.WithContext(withHop(r.Context(), owner.APIAddr)))
				return
			}
			http.Error(w, "agent not connected", http.StatusServiceUnavailable)
			return
		}
```

In `internal/relay/api.go`: `NewAPIWithTunnel` gains a final parameter `self *Instance`, passes it as `NewControlProxy(st, router, self)`, and `NewAPI` becomes `return NewAPIWithTunnel(st, v, "", nil, nil, nil, nil)`.

- [ ] **Step 4: Update callers**

- `cmd/piper-relay/main.go:288`: `relay.NewAPIWithTunnel(st, v, tunnelPublic, router, webRedirects, ghApp, inst)` (`inst` exists since Task 6; move the `NewInstance` block above this line if it is not already).
- The 13 test call sites: append `, nil` as the last argument to each (`api_test.go` ×9, `proxy_test.go` ×2 — the two `proxyFixture`-style ones, not the new tests — and `weblogin_cli_test.go` ×2).

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/relay/ -count=1`
Expected: PASS, including every pre-existing proxy/api test.

- [ ] **Step 6: Verify and commit**

Run: `make verify` (exit 0).

```bash
git add internal/relay/proxy.go internal/relay/api.go internal/relay/proxy_test.go internal/relay/api_test.go internal/relay/weblogin_cli_test.go cmd/piper-relay/main.go
git commit -m "feat(relay): control proxy hops to the owning relay; liveness counts live owner rows

Part of #CHILD

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 9: Wire `piper-relay` main — advertise host, pool membership, signal handling

**Files:**
- Modify: `cmd/piper-relay/main.go`

**Interfaces:**
- Consumes: `NewInstance`, `RunInstance`, `Serve(..., inst, proxyProto)`.

- [ ] **Step 1: Write the failing test** (`cmd/piper-relay/main_test.go`, append)

```go
func TestAdvertiseHostEnvIsHonoured(t *testing.T) {
	t.Setenv("PIPER_RELAY_ADVERTISE_HOST", "10.9.8.7")
	inst, err := relay.NewInstance(env("PIPER_RELAY_ADVERTISE_HOST", ""), ":443", ":80", ":7000", ":8080")
	if err != nil {
		t.Fatal(err)
	}
	if inst.TunnelAddr != "10.9.8.7:7000" {
		t.Fatalf("tunnel addr = %q", inst.TunnelAddr)
	}
}
```

Add `"github.com/piperbox/piper/internal/relay"` to the test file's imports if it is not there.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cmd/piper-relay/ -run AdvertiseHost -count=1`
Expected: it passes already if Task 6 placed the `NewInstance` block as described (the env read exists). If it passes, that is the confirmation the env is read; continue — the remaining wiring below has no unit seam and is verified by `go build` plus the integration test in Task 11.

- [ ] **Step 3: Restructure the lifecycle in `main`**

Replace the block that begins `ctrl := apiHandler` and ends with the closing `}` of `if ghApp != nil { ... }` (the one containing the `signal.Notify` goroutine) with:

```go
	ctrl := apiHandler
	var delivery *relay.TunnelDelivery
	if ghApp != nil {
		delivery = relay.NewTunnelDelivery(st, router)
		// Retries parked events for boxes that are connected but were not
		// accepting deliveries; without it such an event waits for a reconnect
		// or another webhook that may never come (#294). Safe on N replicas:
		// the sweep only drains agents whose session is in this process.
		delivery.StartSweeper(0)
		outer := http.NewServeMux()
		outer.Handle("POST /gh", relay.NewGitHubIngress(st, ghApp, delivery))
		outer.Handle("/", apiHandler)
		ctrl = outer
	}

	// Pool membership: heartbeat, ownership watch, NOTIFY-woken drains. On
	// SIGTERM/SIGINT leave the pool first (the row delete is what tells edges
	// to stop routing here), then let in-flight webhook deliveries park (#295).
	ctx, cancel := context.WithCancel(context.Background())
	poolDone := make(chan struct{})
	go func() {
		relay.RunInstance(ctx, st, inst, router, delivery)
		close(poolDone)
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-sig
		log.Printf("piper-relay: %s — leaving the pool and draining", s)
		cancel()
		select {
		case <-poolDone:
		case <-time.After(5 * time.Second):
			log.Print("piper-relay: pool row not released in time; edges will expire it")
		}
		if delivery != nil {
			ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
			defer cancel()
			delivery.Shutdown(ctx)
		}
		os.Exit(0)
	}()
```

Then, where the instance is created (Task 6 placed it after the `xxxAddr` reads), add a log line right after the `log.Fatalf("instance: %v", err)` check:

```go
	log.Printf("piper-relay: instance %s advertising tls=%s http=%s tunnel=%s api=%s (PIPER_RELAY_ADVERTISE_HOST to override the host)",
		inst.ID, inst.TLSAddr, inst.HTTPAddr, inst.TunnelAddr, inst.APIAddr)
```

Extend the existing `proxyProto` log line so operators see the pairing: append ` — required when a piper-edge fronts this relay` to its message.

- [ ] **Step 4: Build and smoke-run**

Run: `go build ./cmd/piper-relay && go vet ./cmd/piper-relay`
Expected: exit 0.

Run (with a throwaway Postgres, e.g. `docker run -d --rm -e POSTGRES_PASSWORD=piper -p 127.0.0.1:5499:5432 postgres:17` and a 5 s wait):

```bash
PIPER_RELAY_DB_URL='postgres://postgres:piper@127.0.0.1:5499/postgres?sslmode=disable' \
PIPER_RELAY_TLS_ADDR=127.0.0.1:0 PIPER_RELAY_HTTP_ADDR=127.0.0.1:0 PIPER_RELAY_TUNNEL_ADDR=127.0.0.1:0 \
PIPER_RELAY_API_ADDR=127.0.0.1:0 PIPER_RELAY_ADVERTISE_HOST=127.0.0.1 \
timeout 3 go run ./cmd/piper-relay; echo "exit=$?"
```

Expected: log shows `instance <id> advertising tls=127.0.0.1:0 …`; after `timeout` sends SIGTERM the log shows `leaving the pool and draining` and the process exits 0. Then `docker exec <container> psql -U postgres -c 'SELECT count(*) FROM relay_instances'` prints `0`. Stop the container.

- [ ] **Step 5: Verify and commit**

Run: `make verify` (exit 0).

```bash
git add cmd/piper-relay/main.go cmd/piper-relay/main_test.go
git commit -m "feat(relay): join the instance pool at start and leave it on SIGTERM

Part of #CHILD

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 10: Edge metrics and the in-memory routing state (no sockets)

**Files:**
- Modify: `internal/relay/ops.go`
- Create: `internal/relay/edge_state.go`
- Test: `internal/relay/edge_state_test.go`, `internal/relay/ops_test.go`

**Interfaces:**
- Produces: `NewEdgeMetrics() *Metrics` (counters named `piper_edge_*`), `(*Metrics).DialFailed(listener string)`; `newEdgeState() *edgeState` with `setInstances([]InstanceRow)`, `setOwners(map[string]string)`, `setOwner(agent, id string)`, `evict(id string)`, `ownerOf(agent string) (InstanceRow, bool)`, `pickAPI() (InstanceRow, bool)`, `pickTunnel(exclude map[string]bool) (InstanceRow, bool)`, `cachedName(key string) (agent string, cached, fresh bool)`, `cacheName(key, agent string)`, `clearNames()`; const `nameCacheTTL = 30 * time.Second`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/relay/ops_test.go`:

```go
func TestEdgeMetricsUseTheEdgePrefixAndCountDialFailures(t *testing.T) {
	m := NewEdgeMetrics()
	m.ConnAccepted("tls")
	m.DialFailed("tunnel")
	rr := httptest.NewRecorder()
	NewOpsHandler(m, nil).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rr.Body.String()
	for _, want := range []string{
		`piper_edge_conns_accepted_total{listener="tls"} 1`,
		`piper_edge_backend_dial_failures_total{listener="tunnel"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
	if strings.Contains(body, "piper_edge_agents_connected") {
		t.Error("edge exposes a router gauge it has no router for")
	}
}
```

(Add `"strings"`, `"net/http"`, `"net/http/httptest"` imports if `ops_test.go` lacks them.)

`internal/relay/edge_state_test.go`:

```go
package relay

import (
	"testing"
	"time"
)

func instRow(id string, started time.Time, sessions int) InstanceRow {
	return InstanceRow{ID: id, StartedAt: started, Sessions: sessions,
		TLSAddr: id + ":443", HTTPAddr: id + ":80", TunnelAddr: id + ":7000", APIAddr: id + ":8080"}
}

func TestPickAPIPinsEarliestStarted(t *testing.T) {
	t0 := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	s := newEdgeState()
	if _, ok := s.pickAPI(); ok {
		t.Fatal("empty pool picked something")
	}
	s.setInstances([]InstanceRow{instRow("b", t0.Add(time.Minute), 0), instRow("a", t0, 9), instRow("c", t0.Add(time.Hour), 0)})
	if got, ok := s.pickAPI(); !ok || got.ID != "a" {
		t.Fatalf("pickAPI = %+v ok=%v, want a (earliest, load ignored)", got, ok)
	}
}

func TestPickTunnelFewestSessionsThenEarliest(t *testing.T) {
	t0 := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	s := newEdgeState()
	s.setInstances([]InstanceRow{
		instRow("busy", t0, 5),
		instRow("late-idle", t0.Add(time.Minute), 1),
		instRow("early-idle", t0.Add(time.Second), 1),
	})
	cases := []struct {
		name    string
		exclude map[string]bool
		want    string
	}{
		{"fewest then earliest", nil, "early-idle"},
		{"excluded first choice", map[string]bool{"early-idle": true}, "late-idle"},
		{"only the busy one left", map[string]bool{"early-idle": true, "late-idle": true}, "busy"},
	}
	for _, c := range cases {
		got, ok := s.pickTunnel(c.exclude)
		if !ok || got.ID != c.want {
			t.Errorf("%s: pickTunnel = %+v ok=%v, want %s", c.name, got, ok, c.want)
		}
	}
	if _, ok := s.pickTunnel(map[string]bool{"early-idle": true, "late-idle": true, "busy": true}); ok {
		t.Fatal("every candidate excluded still picked one")
	}
}

func TestOwnerOfRequiresALiveInstanceAndEvictCascades(t *testing.T) {
	t0 := time.Now()
	s := newEdgeState()
	s.setInstances([]InstanceRow{instRow("a", t0, 0), instRow("b", t0, 0)})
	s.setOwners(map[string]string{"x.example": "a", "y.example": "b"})
	if got, ok := s.ownerOf("x.example"); !ok || got.TLSAddr != "a:443" {
		t.Fatalf("ownerOf x = %+v ok=%v", got, ok)
	}
	if _, ok := s.ownerOf("nobody.example"); ok {
		t.Fatal("unowned agent resolved")
	}
	s.setOwner("x.example", "")
	if _, ok := s.ownerOf("x.example"); ok {
		t.Fatal("cleared owner still resolved")
	}
	s.setOwner("x.example", "a")
	s.evict("a")
	if _, ok := s.ownerOf("x.example"); ok {
		t.Fatal("owner survived its instance's eviction")
	}
	if got, ok := s.pickTunnel(nil); !ok || got.ID != "b" {
		t.Fatalf("after evict pickTunnel = %+v ok=%v, want b", got, ok)
	}
	// An owner row that points at an unknown (dead) instance is unroutable.
	s.setOwner("y.example", "ghost")
	if _, ok := s.ownerOf("y.example"); ok {
		t.Fatal("owner naming an unknown instance resolved")
	}
}

func TestNameCacheExpiresAndClears(t *testing.T) {
	now := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	s := newEdgeState()
	s.now = func() time.Time { return now }

	if _, cached, _ := s.cachedName("app.example"); cached {
		t.Fatal("empty cache reported an entry")
	}
	s.cacheName("app.example", "box.example")
	s.cacheName("nothing.example", "")
	if agent, cached, fresh := s.cachedName("app.example"); !cached || !fresh || agent != "box.example" {
		t.Fatalf("fresh positive = %q %v %v", agent, cached, fresh)
	}
	if agent, cached, fresh := s.cachedName("nothing.example"); !cached || !fresh || agent != "" {
		t.Fatalf("fresh negative = %q %v %v", agent, cached, fresh)
	}
	now = now.Add(nameCacheTTL + time.Second)
	if agent, cached, fresh := s.cachedName("app.example"); !cached || fresh || agent != "box.example" {
		t.Fatalf("expired entry = %q cached=%v fresh=%v, want stale-but-present", agent, cached, fresh)
	}
	s.clearNames()
	if _, cached, _ := s.cachedName("app.example"); cached {
		t.Fatal("clearNames left an entry")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/relay/ -run 'EdgeMetrics|PickAPI|PickTunnel|OwnerOfRequires|NameCache' -count=1`
Expected: compile error `undefined: NewEdgeMetrics` / `newEdgeState`.

- [ ] **Step 3: Refactor `ops.go` for a prefix and add the dial-failure counter**

Add `dialFailures *prometheus.CounterVec` to the `Metrics` struct. Replace `func NewMetrics(router *Router) *Metrics {` and its body with:

```go
// NewMetrics builds the relay's instruments: traffic counters plus topology
// gauges read from router at scrape time.
func NewMetrics(router *Router) *Metrics { return newMetrics("piper_relay", router) }

// NewEdgeMetrics builds piper-edge's instruments: the same traffic counters
// under the piper_edge prefix plus backend dial failures. The edge holds no
// sessions, so there are no topology gauges.
func NewEdgeMetrics() *Metrics { return newMetrics("piper_edge", nil) }

func newMetrics(prefix string, router *Router) *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	if router != nil {
		reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: prefix + "_agents_connected",
			Help: "Agent tunnel sessions currently registered.",
		}, func() float64 { a, _, _ := router.Counts(); return float64(a) }))
		reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: prefix + "_hostnames_routed",
			Help: "Relay-terminated app hostnames currently routed.",
		}, func() float64 { _, h, _ := router.Counts(); return float64(h) }))
		reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: prefix + "_custom_domains_routed",
			Help: "BYO custom domains currently routed.",
		}, func() float64 { _, _, c := router.Counts(); return float64(c) }))
	}

	m := &Metrics{
		reg: reg,
		connsAccepted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "_conns_accepted_total",
			Help: "Connections accepted, by public listener.",
		}, []string{"listener"}),
		connsRouted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "_conns_routed_total",
			Help: "Connections whose SNI/Host matched a registered session.",
		}, []string{"listener"}),
		connsUnrouted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "_conns_unrouted_total",
			Help: "Connections that completed a head read but matched no session.",
		}, []string{"listener"}),
		activeStreams: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: prefix + "_active_streams",
			Help: "Connections currently being spliced.",
		}),
		dialFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "_backend_dial_failures_total",
			Help: "Backend relay dials that failed, by public listener (edge only).",
		}, []string{"listener"}),
	}
	reg.MustRegister(m.connsAccepted, m.connsRouted, m.connsUnrouted, m.activeStreams, m.dialFailures)
	return m
}
```

Add after `ConnUnrouted`:

```go
func (m *Metrics) DialFailed(listener string) {
	if m == nil {
		return
	}
	m.dialFailures.WithLabelValues(listener).Inc()
}
```

(The relay's existing metric names are unchanged: `piper_relay_` + the same suffixes. The `activeStreams` help text drops "down an agent tunnel" because the edge shares it.)

- [ ] **Step 4: Write `edge_state.go`**

```go
package relay

import (
	"sync"
	"time"
)

// nameCacheTTL bounds how long the edge trusts a hostname → agent answer,
// positive or negative, before asking Postgres again. piper_hostnames clears
// the cache the moment a name changes, so this is only the backstop.
const nameCacheTTL = 30 * time.Second

// edgeState is the edge's in-memory picture of the cluster: the live
// instance pool, who owns which agent, and a hostname → agent cache. Every
// routing decision reads it; the listener and poll goroutines write it.
type edgeState struct {
	mu        sync.RWMutex
	instances map[string]InstanceRow
	owners    map[string]string // agent base domain → instance id
	names     map[string]nameEntry
	now       func() time.Time
}

// nameEntry caches one lookup. agent == "" is a negative entry: nothing
// serves this name (yet).
type nameEntry struct {
	agent   string
	expires time.Time
}

func newEdgeState() *edgeState {
	return &edgeState{
		instances: map[string]InstanceRow{},
		owners:    map[string]string{},
		names:     map[string]nameEntry{},
		now:       time.Now,
	}
}

func (s *edgeState) setInstances(rows []InstanceRow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.instances = make(map[string]InstanceRow, len(rows))
	for _, r := range rows {
		s.instances[r.ID] = r
	}
}

func (s *edgeState) setOwners(m map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.owners = make(map[string]string, len(m))
	for agent, id := range m {
		s.owners[agent] = id
	}
}

// setOwner records one ownership change; id == "" clears it.
func (s *edgeState) setOwner(agent, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" {
		delete(s.owners, agent)
		return
	}
	s.owners[agent] = id
}

// evict drops an instance the edge found dead on dial, with every agent it
// owned — the in-memory twin of the agent_owners cascade.
func (s *edgeState) evict(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.instances, id)
	for agent, owner := range s.owners {
		if owner == id {
			delete(s.owners, agent)
		}
	}
}

// ownerOf returns the live instance owning agent.
func (s *edgeState) ownerOf(agent string) (InstanceRow, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.owners[agent]
	if !ok {
		return InstanceRow{}, false
	}
	r, ok := s.instances[id]
	return r, ok
}

// pickAPI is the api.<apex> pin: the live instance that started first, so
// every login-flow poll lands on one process until that state moves to
// Postgres.
func (s *edgeState) pickAPI() (InstanceRow, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pickLocked(earlier, nil)
}

// pickTunnel is :7000 placement: fewest sessions, ties to the earliest
// started. exclude names instances a failed dial has just ruled out.
func (s *edgeState) pickTunnel(exclude map[string]bool) (InstanceRow, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pickLocked(func(a, b InstanceRow) bool {
		if a.Sessions != b.Sessions {
			return a.Sessions < b.Sessions
		}
		return earlier(a, b)
	}, exclude)
}

// earlier is a total order on instances: start time, then id.
func earlier(a, b InstanceRow) bool {
	if !a.StartedAt.Equal(b.StartedAt) {
		return a.StartedAt.Before(b.StartedAt)
	}
	return a.ID < b.ID
}

func (s *edgeState) pickLocked(less func(a, b InstanceRow) bool, exclude map[string]bool) (InstanceRow, bool) {
	var best InstanceRow
	found := false
	for _, r := range s.instances {
		if exclude[r.ID] {
			continue
		}
		if !found || less(r, best) {
			best, found = r, true
		}
	}
	return best, found
}

// cachedName reports the cached agent for key: cached says an entry exists
// at all (stale entries are kept so an unreachable Postgres serves old
// answers rather than none), fresh says it is within nameCacheTTL.
func (s *edgeState) cachedName(key string) (agent string, cached, fresh bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.names[key]
	if !ok {
		return "", false, false
	}
	return e.agent, true, s.now().Before(e.expires)
}

func (s *edgeState) cacheName(key, agent string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.names[key] = nameEntry{agent: agent, expires: s.now().Add(nameCacheTTL)}
}

func (s *edgeState) clearNames() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.names = map[string]nameEntry{}
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/relay/ -run 'EdgeMetrics|PickAPI|PickTunnel|OwnerOfRequires|NameCache|Ops' -count=1 -v`
Expected: PASS, and the pre-existing ops tests still pass.

- [ ] **Step 6: Verify and commit**

Run: `make verify` (exit 0).

```bash
git add internal/relay/ops.go internal/relay/ops_test.go internal/relay/edge_state.go internal/relay/edge_state_test.go
git commit -m "feat(relay): edge routing state — instance pool, owner map, name cache, placement rules

Part of #CHILD

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 11: `ServeEdge` — listeners, owner routing, PROXY v2 forwarding

**Files:**
- Create: `internal/relay/edge.go`
- Test: `internal/relay/edge_test.go` (harness + first integration test)

**Interfaces:**
- Consumes: `edgeState` (Task 10), `readSNI`, `readHost`, `proxyV2Listener`, `listen`, `LiveInstances`, `Owners`, `OwnerOf`, `AgentForHost`, `AgentForCustomHost`, `DeleteInstance`, `PurgeDeadInstances`, `Metrics`.
- Produces: `type EdgeConfig struct{Apex, TLSAddr, HTTPAddr, TunnelAddr string; ProxyProto bool}`; `ServeEdge(ctx context.Context, cfg EdgeConfig, st *Store, m *Metrics) error` (blocks until a listener fails or ctx is done); package vars `edgeDialTimeout = 2 * time.Second`, `edgePollInterval = 15 * time.Second`; `errBackendDial`. Test harness: `startRelayBehindEdge`, `startEdge`, `dialAgentThroughEdge`, `fakeAgent`, `edgeRelay`.

- [ ] **Step 1: Write the harness and the first failing integration test**

`internal/relay/edge_test.go`:

```go
package relay

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/piperbox/piper/internal/tunnel"
)

// edgeRelay is one in-process relay standing behind the edge under test.
type edgeRelay struct {
	inst   *Instance
	router *Router
	apiURL string
	stop   func() // leaves the pool and closes every listener; idempotent
}

// startRelayBehindEdge runs a relay the way Serve does — tunnel, TLS and
// HTTP listeners wrapped for PROXY v2, control API on an httptest server —
// tagged with X-Relay so a response names the process that produced it, plus
// RunInstance so it heartbeats and owns. Returns once the pool lists it.
func startRelayBehindEdge(t *testing.T, st *Store, tlsCfg *tls.Config) *edgeRelay {
	t.Helper()
	router := NewRouter()
	delivery := NewTunnelDelivery(st, router)

	var api http.Handler
	var inst *Instance
	tagged := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("X-Relay", inst.ID)
		api.ServeHTTP(w, r)
	})
	apiSrv := httptest.NewServer(tagged)
	t.Cleanup(apiSrv.Close)

	tunLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	httpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tlsLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	inst, err = NewInstance("127.0.0.1", tlsLn.Addr().String(), httpLn.Addr().String(), tunLn.Addr().String(), apiSrv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	api = NewAPIWithTunnel(st, NewFakeVerifier(), "", router, nil, nil, inst)

	ctrlQ := newConnQueue()
	ctrlSrv := &http.Server{Handler: tagged, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = ctrlSrv.Serve(ctrlQ) }()
	ctrlHost := "api." + st.apexOrDefault()

	go acceptTunnels(proxyV2Listener(tunLn), st, router, nil, delivery, nil, inst)
	go acceptHTTP(proxyV2Listener(httpLn), router, nil)
	wrappedTLS := proxyV2Listener(tlsLn)
	go func() {
		for {
			c, err := wrappedTLS.Accept()
			if err != nil {
				return
			}
			go handlePublic(c, router, tlsCfg, ctrlHost, ctrlQ, nil)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	poolDone := make(chan struct{})
	go func() { RunInstance(ctx, st, inst, router, delivery); close(poolDone) }()
	waitCond(t, 5*time.Second, "relay "+inst.ID+" in the pool", func() bool {
		rows, _ := st.LiveInstances()
		for _, r := range rows {
			if r.ID == inst.ID {
				return true
			}
		}
		return false
	})

	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			<-poolDone
			tunLn.Close()
			httpLn.Close()
			tlsLn.Close()
			ctrlQ.Close()
		})
	}
	t.Cleanup(stop)
	return &edgeRelay{inst: inst, router: router, apiURL: apiSrv.URL, stop: stop}
}

// startEdge runs ServeEdge on ephemeral ports against st and returns its
// config once the TLS listener accepts.
func startEdge(t *testing.T, st *Store) EdgeConfig {
	t.Helper()
	cfg := EdgeConfig{Apex: "public.getpiper.co", TLSAddr: freeTCPAddr(t), HTTPAddr: freeTCPAddr(t), TunnelAddr: freeTCPAddr(t)}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errc := make(chan error, 1)
	go func() { errc <- ServeEdge(ctx, cfg, st, nil) }()
	waitCond(t, 5*time.Second, "edge listening", func() bool {
		select {
		case err := <-errc:
			t.Fatalf("ServeEdge returned: %v", err)
		default:
		}
		// The TLS listener is opened last, so it accepting means all three are up.
		c, err := net.DialTimeout("tcp", cfg.TLSAddr, 200*time.Millisecond)
		if err != nil {
			return false
		}
		c.Close()
		return true
	})
	return cfg
}

// dialAgentThroughEdge dials the edge's :7000 as a box would and returns the
// agent-side session plus the dialer's local address (what PROXY v2 must
// carry to the relay).
func dialAgentThroughEdge(t *testing.T, cfg EdgeConfig, en Enrollment) (*tunnel.Session, string) {
	t.Helper()
	conn, err := net.Dial("tcp", cfg.TunnelAddr)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := tunnel.Dial(conn, en.Token, en.BaseDomain)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess, conn.LocalAddr().String()
}

// fakeAgent answers the streams a box would: passthrough (report the SNI it
// peeked), KindHTTP (record the body, answer 202), control API (echo method
// and path, like fakeBox).
func fakeAgent(sess *tunnel.Session, snis, bodies chan string) {
	for {
		kind, stream, err := sess.AcceptKind()
		if err != nil {
			return
		}
		go func() {
			defer stream.Close()
			switch kind {
			case tunnel.KindPassthrough:
				sni, _, _ := readSNI(stream)
				snis <- sni
			case tunnel.KindHTTP:
				req, err := http.ReadRequest(bufio.NewReader(stream))
				if err != nil {
					return
				}
				body, _ := io.ReadAll(req.Body)
				bodies <- string(body)
				_, _ = io.WriteString(stream, "HTTP/1.1 202 Accepted\r\nContent-Length: 0\r\n\r\n")
			case tunnel.KindControlAPI:
				req, err := http.ReadRequest(bufio.NewReader(stream))
				if err != nil {
					return
				}
				body := req.Method + " " + req.URL.RequestURI()
				fmt.Fprintf(stream, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
			}
		}()
	}
}

// waitOwner blocks until base has a live owner and returns it.
func waitOwner(t *testing.T, st *Store, base string) InstanceRow {
	t.Helper()
	var owner InstanceRow
	waitCond(t, 5*time.Second, "owner of "+base, func() bool {
		r, ok, _ := st.OwnerOf(base)
		owner = r
		return ok
	})
	return owner
}

// waitSessions blocks until the pool reports id with n sessions — the
// heartbeat has to publish a placement before the next one can balance
// against it.
func waitSessions(t *testing.T, st *Store, id string, n int) {
	t.Helper()
	waitCond(t, 5*time.Second, fmt.Sprintf("%s reports %d sessions", id, n), func() bool {
		rows, _ := st.LiveInstances()
		for _, r := range rows {
			if r.ID == id {
				return r.Sessions == n
			}
		}
		return false
	})
}

// sendClientHello starts a TLS handshake to addr with the given SNI and
// returns the conn; the handshake never completes (only the ClientHello has
// to travel), so the caller reads the far side's reaction elsewhere.
func sendClientHello(t *testing.T, addr, sni string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	go tls.Client(conn, &tls.Config{ServerName: sni, InsecureSkipVerify: true}).Handshake()
	return conn
}

func expectString(t *testing.T, ch <-chan string, want, what string) {
	t.Helper()
	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("%s: got %q, want %q", what, got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: nothing arrived", what)
	}
}

// TestEdgePlacesAndRoutesAcrossTwoRelays is the test that earns the design
// its keep: two relays in one pool, one edge in front, two boxes dialled
// through it. Placement spreads them; passthrough follows ownership; the
// relay sees the box's own address through PROXY v2; api.<apex> is pinned.
func TestEdgePlacesAndRoutesAcrossTwoRelays(t *testing.T) {
	logged := captureRelayLog(t)
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	acc, err := st.UpsertAccount("sub-1", "alice")
	if err != nil {
		t.Fatal(err)
	}
	en1, err := st.EnrollForAccount(acc.ID, "box-1")
	if err != nil {
		t.Fatal(err)
	}
	en2, err := st.EnrollForAccount(acc.ID, "box-2")
	if err != nil {
		t.Fatal(err)
	}
	cert, key := writeWildcard(t, "public.getpiper.co")
	tlsCfg, err := LoadWildcardConfig(cert, key)
	if err != nil {
		t.Fatal(err)
	}

	rA := startRelayBehindEdge(t, st, tlsCfg)
	rB := startRelayBehindEdge(t, st, tlsCfg)
	cfg := startEdge(t, st)

	// Placement: the first box lands somewhere; once that relay reports the
	// session, the second box must land on the other relay.
	s1, _ := dialAgentThroughEdge(t, cfg, en1)
	owner1 := waitOwner(t, st, en1.BaseDomain)
	waitSessions(t, st, owner1.ID, 1)
	s2, _ := dialAgentThroughEdge(t, cfg, en2)
	owner2 := waitOwner(t, st, en2.BaseDomain)
	if owner1.ID == owner2.ID {
		t.Fatalf("both boxes placed on %s; least-sessions placement failed", owner1.ID)
	}

	// Passthrough follows ownership: each SNI reaches its own box, through
	// whichever relay holds it.
	snis1, snis2 := make(chan string, 4), make(chan string, 4)
	go fakeAgent(s1, snis1, make(chan string, 4))
	go fakeAgent(s2, snis2, make(chan string, 4))
	sendClientHello(t, cfg.TLSAddr, "app."+en1.BaseDomain)
	expectString(t, snis1, "app."+en1.BaseDomain, "box 1 passthrough")
	sendClientHello(t, cfg.TLSAddr, "app."+en2.BaseDomain)
	expectString(t, snis2, "app."+en2.BaseDomain, "box 2 passthrough")

	// PROXY v2: a rejected handshake is logged with the dialer's address,
	// not the edge's.
	bogus, err := net.Dial("tcp", cfg.TunnelAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer bogus.Close()
	_, _ = tunnel.Dial(bogus, "not-a-token", "nobody.public.getpiper.co")
	waitForLog(t, logged, "from "+bogus.LocalAddr().String())

	// api.<apex> is pinned to the earliest-started relay (rA) and reaches
	// its control plane: no bearer → 401 from that process.
	tc, err := tls.Dial("tcp", cfg.TLSAddr, &tls.Config{ServerName: "api.public.getpiper.co", InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	if _, err := io.WriteString(tc, "GET /agents HTTP/1.1\r\nHost: api.public.getpiper.co\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tc), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("X-Relay") != rA.inst.ID {
		t.Fatalf("api.<apex> via edge: %d from %q, want 401 from %s (rB is %s)", resp.StatusCode, resp.Header.Get("X-Relay"), rA.inst.ID, rB.inst.ID)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/relay/ -run TestEdgePlacesAndRoutes -count=1`
Expected: compile error `undefined: EdgeConfig` / `ServeEdge`.

- [ ] **Step 3: Write `edge.go`**

```go
package relay

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"sync/atomic"
	"time"

	proxyproto "github.com/pires/go-proxyproto"
)

// EdgeConfig is what ServeEdge needs; cmd/piper-edge fills it from the
// PIPER_EDGE_* environment.
type EdgeConfig struct {
	Apex       string // recognises api.<apex>
	TLSAddr    string
	HTTPAddr   string
	TunnelAddr string
	ProxyProto bool // our own listeners expect PROXY v2 from a balancer in front
}

// edgeDialTimeout bounds a backend dial; a relay that does not answer in
// this long is treated as dead.
var edgeDialTimeout = 2 * time.Second

// edgePollInterval is the belt-and-braces refresh of the instance pool: it
// also evicts rows that expired silently, which no NOTIFY announces.
var edgePollInterval = 15 * time.Second

// errBackendDial marks a backend that could not be reached. forward has
// already evicted it when this is returned.
var errBackendDial = errors.New("edge: backend dial failed")

// edge is one running piper-edge: stateless apart from its copy of the
// cluster tables. It holds no cert, speaks no HTTP beyond peeking a Host
// line, and its only store writes delete dead instance rows.
type edge struct {
	cfg     EdgeConfig
	st      *Store
	state   *edgeState
	m       *Metrics
	apiHost string
	dbDown  atomic.Bool
}

// ServeEdge runs the public entrypoint: :443 by SNI, :80 by Host, :7000 by
// load, each spliced to the owning relay behind a PROXY v2 header that
// carries the client's address. Blocks until a listener fails or ctx is done.
func ServeEdge(ctx context.Context, cfg EdgeConfig, st *Store, m *Metrics) error {
	e := &edge{cfg: cfg, st: st, state: newEdgeState(), m: m, apiHost: "api." + cfg.Apex}
	e.resync()
	go listen(ctx, st.dsn, []string{chanInstances, chanOwners, chanHostnames}, e.resync, e.onNotify)
	go e.poll(ctx)

	errc := make(chan error, 3)
	var lns []net.Listener
	for _, l := range []struct {
		name   string
		addr   string
		handle func(net.Conn)
	}{
		{"tunnel", cfg.TunnelAddr, e.handleTunnel},
		{"http", cfg.HTTPAddr, e.handleHTTP},
		{"tls", cfg.TLSAddr, e.handleTLS},
	} {
		ln, err := net.Listen("tcp", l.addr)
		if err != nil {
			for _, o := range lns {
				o.Close()
			}
			return err
		}
		if cfg.ProxyProto {
			ln = proxyV2Listener(ln)
		}
		lns = append(lns, ln)
		go func(ln net.Listener, name string, handle func(net.Conn)) {
			for {
				conn, err := ln.Accept()
				if err != nil {
					errc <- err
					return
				}
				m.ConnAccepted(name)
				go handle(conn)
			}
		}(ln, l.name, l.handle)
	}
	defer func() {
		for _, ln := range lns {
			ln.Close()
		}
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// resync reloads the pool and the owner map and drops the name cache. It
// runs at start, on every listener (re)connect, and from poll.
func (e *edge) resync() {
	rows, err := e.st.LiveInstances()
	if err != nil {
		e.dbLost(err)
		return
	}
	owners, err := e.st.Owners()
	if err != nil {
		e.dbLost(err)
		return
	}
	e.dbBack()
	e.state.setInstances(rows)
	e.state.setOwners(owners)
	e.state.clearNames()
}

func (e *edge) onNotify(channel, payload string) {
	switch channel {
	case chanInstances:
		if rows, err := e.st.LiveInstances(); err != nil {
			e.dbLost(err)
		} else {
			e.dbBack()
			e.state.setInstances(rows)
		}
	case chanOwners:
		owner, ok, err := e.st.OwnerOf(payload)
		if err != nil {
			e.dbLost(err)
			return
		}
		e.dbBack()
		if ok {
			e.state.setOwner(payload, owner.ID)
		} else {
			e.state.setOwner(payload, "")
		}
	case chanHostnames:
		e.state.clearNames()
	}
}

// poll is the fallback for a NOTIFY the listener missed and the only thing
// that removes rows which expired without anyone deleting them.
func (e *edge) poll(ctx context.Context) {
	tick := time.NewTicker(edgePollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := e.st.PurgeDeadInstances(); err != nil {
				e.dbLost(err)
				continue
			}
			e.resync()
		}
	}
}

// dbLost/dbBack log an outage once each way. During one the edge keeps
// serving from memory and lets the name cache go stale rather than empty.
func (e *edge) dbLost(err error) {
	if e.dbDown.CompareAndSwap(false, true) {
		log.Printf("edge: postgres unreachable (%v); serving from memory until it returns", err)
	}
}

func (e *edge) dbBack() {
	if e.dbDown.CompareAndSwap(true, false) {
		log.Print("edge: postgres reachable again")
	}
}

// resolveAgent maps a public name to the base domain of the agent serving
// it, through the cache. customOnly is the :80 rule (custom domains only).
// When Postgres is unreachable a stale entry is served rather than nothing.
func (e *edge) resolveAgent(host string, customOnly bool) (string, bool) {
	key := host
	lookup := e.st.AgentForHost
	if customOnly {
		key = "custom:" + host
		lookup = e.st.AgentForCustomHost
	}
	agent, cached, fresh := e.state.cachedName(key)
	if cached && fresh {
		return agent, agent != ""
	}
	got, ok, err := lookup(host)
	if err != nil {
		e.dbLost(err)
		return agent, cached && agent != ""
	}
	e.dbBack()
	if !ok {
		got = ""
	}
	e.state.cacheName(key, got)
	return got, ok
}

func (e *edge) handleTLS(conn net.Conn) {
	defer conn.Close()
	sni, buffered, err := readSNI(conn)
	if err != nil {
		return
	}
	var target InstanceRow
	var ok bool
	if sni == e.apiHost {
		// Login-flow state is per-process until its follow-up lands: pin the
		// control plane to one relay so every poll sees the same memory.
		target, ok = e.state.pickAPI()
	} else if agent, found := e.resolveAgent(sni, false); found {
		target, ok = e.state.ownerOf(agent)
	}
	if !ok {
		e.m.ConnUnrouted("tls")
		return
	}
	// No retry here: the owner is unique. If it is dead the agent will
	// reconnect and a new owner row will arrive.
	_ = e.forward("tls", conn, buffered, target, target.TLSAddr)
}

func (e *edge) handleHTTP(conn net.Conn) {
	defer conn.Close()
	host, buffered, err := readHost(conn)
	if err != nil {
		return
	}
	var target InstanceRow
	var ok bool
	if agent, found := e.resolveAgent(host, true); found {
		target, ok = e.state.ownerOf(agent)
	}
	if !ok {
		e.m.ConnUnrouted("http")
		return
	}
	_ = e.forward("http", conn, buffered, target, target.HTTPAddr)
}

// handleTunnel places a new agent on the least-loaded relay. A dial failure
// evicts that relay and retries the next candidate exactly once; the pool is
// small and a second failure means something bigger is wrong.
func (e *edge) handleTunnel(conn net.Conn) {
	defer conn.Close()
	exclude := map[string]bool{}
	for attempt := 0; attempt < 2; attempt++ {
		target, ok := e.state.pickTunnel(exclude)
		if !ok {
			break
		}
		if err := e.forward("tunnel", conn, nil, target, target.TunnelAddr); !errors.Is(err, errBackendDial) {
			return
		}
		exclude[target.ID] = true
	}
	e.m.ConnUnrouted("tunnel")
}

// forward dials addr on target, writes a PROXY v2 header naming the client,
// replays the bytes consumed while peeking, then splices both ways until one
// side ends. A refused or timed-out dial marks target dead: dropped from
// memory and its row deleted, which cascades its ownership rows, so nothing
// routes there again until it heartbeats afresh.
func (e *edge) forward(listener string, conn net.Conn, buffered []byte, target InstanceRow, addr string) error {
	backend, err := net.DialTimeout("tcp", addr, edgeDialTimeout)
	if err != nil {
		e.m.DialFailed(listener)
		log.Printf("edge: %s: dial relay %s at %s: %v; evicting it", listener, target.ID, addr, err)
		e.evict(target.ID)
		return errBackendDial
	}
	defer backend.Close()
	e.m.ConnRouted(listener)
	if _, err := proxyproto.HeaderProxyFromAddrs(2, conn.RemoteAddr(), conn.LocalAddr()).WriteTo(backend); err != nil {
		return nil
	}
	if len(buffered) > 0 {
		if _, err := backend.Write(buffered); err != nil {
			return nil
		}
	}
	e.m.StreamStart()
	defer e.m.StreamEnd()
	done := make(chan struct{}, 2)
	go func() { io.Copy(backend, conn); done <- struct{}{} }()
	go func() { io.Copy(conn, backend); done <- struct{}{} }()
	<-done
	return nil
}

func (e *edge) evict(id string) {
	e.state.evict(id)
	if err := e.st.DeleteInstance(id); err != nil {
		log.Printf("edge: delete dead instance %s: %v", id, err)
	}
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/relay/ -run TestEdgePlacesAndRoutes -count=1 -v`
Expected: PASS. If placement flakes because both boxes land on the same relay, `waitSessions` is not being honoured — check that `Router.Counts()` reports 1 after the first tunnel registers and that `heartbeatInterval` is 20 ms in `TestMain`.

- [ ] **Step 5: Verify and commit**

Run: `make verify` (exit 0).

```bash
git add internal/relay/edge.go internal/relay/edge_test.go
git commit -m "feat(relay): piper-edge core — SNI/Host/least-sessions routing spliced to the owner with PROXY v2

Part of #CHILD

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 12: Edge integration, part 2 — control hop, webhook drain, ownership move, dead-relay eviction

**Files:**
- Test: `internal/relay/edge_test.go`

**Interfaces:**
- Consumes: everything from Task 11's harness. No production code should change in this task; if a test exposes a bug, fix it in the file that owns the behaviour and say so in the commit message.

- [ ] **Step 1: Write the failing tests** (append to `edge_test.go`)

```go
func TestEdgeClusterControlHopWebhookDrainAndOwnershipMove(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	acc, err := st.UpsertAccount("sub-1", "alice")
	if err != nil {
		t.Fatal(err)
	}
	aliceCred, err := st.MintAccountCredential(acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	en1, err := st.EnrollForAccount(acc.ID, "box-1")
	if err != nil {
		t.Fatal(err)
	}
	en2, err := st.EnrollForAccount(acc.ID, "box-2")
	if err != nil {
		t.Fatal(err)
	}

	rA := startRelayBehindEdge(t, st, nil)
	rB := startRelayBehindEdge(t, st, nil)
	cfg := startEdge(t, st)

	s1, _ := dialAgentThroughEdge(t, cfg, en1)
	owner1 := waitOwner(t, st, en1.BaseDomain)
	waitSessions(t, st, owner1.ID, 1)
	dialAgentThroughEdge(t, cfg, en2)
	waitOwner(t, st, en2.BaseDomain)

	bodies1 := make(chan string, 4)
	go fakeAgent(s1, make(chan string, 4), bodies1)

	ownerRelay, otherRelay := rA, rB
	if owner1.ID == rB.inst.ID {
		ownerRelay, otherRelay = rB, rA
	}

	// Control hop: a call that lands on the non-owner is answered by the
	// box through the owner. The response carries both relays' tags.
	req, _ := http.NewRequest(http.MethodGet, otherRelay.apiURL+"/agents/"+en1.BaseDomain+"/v1/apps", nil)
	req.Header.Set("Authorization", "Bearer "+aliceCred)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "GET /v1/apps" {
		t.Fatalf("hop via non-owner: %d %q", resp.StatusCode, body)
	}
	if tags := strings.Join(resp.Header.Values("X-Relay"), ","); !strings.Contains(tags, ownerRelay.inst.ID) {
		t.Fatalf("response tags %q do not name the owner %s", tags, ownerRelay.inst.ID)
	}

	// Webhook drain: an event parked by any process wakes the owner.
	if err := st.ParkEvent(en1.BaseDomain, "blog", "main", "push", []byte(`{"after":"x"}`)); err != nil {
		t.Fatal(err)
	}
	expectString(t, bodies1, `{"after":"x"}`, "parked webhook drained by the owner")

	// Ownership move: the owner leaves the pool and its box reconnects;
	// the edge must place it on the survivor and the row must follow.
	ownerRelay.stop()
	waitCond(t, 5*time.Second, "stopped relay out of the pool", func() bool {
		rows, _ := st.LiveInstances()
		for _, r := range rows {
			if r.ID == ownerRelay.inst.ID {
				return false
			}
		}
		return true
	})
	s1.Close()
	dialAgentThroughEdge(t, cfg, en1)
	waitCond(t, 5*time.Second, "box 1 owned by the survivor", func() bool {
		r, ok, _ := st.OwnerOf(en1.BaseDomain)
		return ok && r.ID == otherRelay.inst.ID
	})
}

func TestEdgeEvictsDeadRelayOnDialFailureAndRetriesOnceOnTunnel(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	deadAddr := freeTCPAddr(t) // nothing listens: dial is refused at once
	stampInstance(t, st, "dead", deadAddr, time.Now().Add(-time.Minute))
	liveLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { liveLn.Close() })
	stampInstance(t, st, "live", liveLn.Addr().String(), time.Now())
	if err := st.SetOwner(en.BaseDomain, "dead"); err != nil {
		t.Fatal(err)
	}
	cfg := startEdge(t, st)

	// :7000 — equal load, so the earliest (dead) is tried first; the refused
	// dial evicts it and the single retry lands on live.
	accepted := acceptOne(liveLn)
	conn, err := net.Dial("tcp", cfg.TunnelAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	awaitAccept(t, accepted).Close()
	waitCond(t, 5*time.Second, "dead row deleted", func() bool {
		rows, _ := st.LiveInstances()
		return len(rows) == 1 && rows[0].ID == "live"
	})
	if _, ok, _ := st.OwnerOf(en.BaseDomain); ok {
		t.Fatal("ownership survived its instance's eviction")
	}
}

func TestEdgeNeverRetriesOnTLS(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	deadAddr := freeTCPAddr(t)
	stampInstance(t, st, "dead", deadAddr, time.Now())
	liveLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { liveLn.Close() })
	stampInstance(t, st, "live", liveLn.Addr().String(), time.Now())
	if err := st.SetOwner(en.BaseDomain, "dead"); err != nil {
		t.Fatal(err)
	}
	cfg := startEdge(t, st)

	accepted := acceptOne(liveLn)
	conn := sendClientHello(t, cfg.TLSAddr, "app."+en.BaseDomain)
	// The edge closes the client: the owner is dead and :443 has no second candidate.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("edge kept the connection open after the owner dial failed")
	}
	select {
	case r := <-accepted:
		if r.conn != nil {
			r.conn.Close()
		}
		t.Fatal(":443 retried onto a relay that does not own the agent")
	case <-time.After(300 * time.Millisecond):
	}
	waitCond(t, 5*time.Second, "dead row deleted", func() bool {
		rows, _ := st.LiveInstances()
		return len(rows) == 1 && rows[0].ID == "live"
	})
}
```

Add `"strings"` to `edge_test.go`'s imports. `acceptOne` and `awaitAccept` already exist in `proxyproto_test.go` (same package).

- [ ] **Step 2: Run the tests to verify they fail or pass for the right reasons**

Run: `go test ./internal/relay/ -run 'TestEdgeCluster|TestEdgeEvicts|TestEdgeNeverRetries' -count=1 -v`
Expected: these exercise code written in Tasks 7, 8 and 11, so they may pass immediately. If one fails, the failure names the behaviour to fix — the likely culprits, in order: the owner NOTIFY not reaching `RunInstance` before the stale-session check, the edge still holding the stopped relay because `DeleteInstance` fired before the listener connected (the `poll` fallback is 15 s in production; the test relies on NOTIFY), or `HeaderProxyFromAddrs` emitting `UNSPEC` because the edge's listener conn is not a `*net.TCPAddr` (it always is for a plain `net.Listen`).

- [ ] **Step 3: Run the whole package with the race detector once**

Run: `go test ./internal/relay/ -race -count=1`
Expected: PASS with no `DATA RACE` report. `edgeState` is fully mutex-guarded and `Instance` is read-only after construction; a race here means a test shares a `chan` or map it should not.

- [ ] **Step 4: Verify and commit**

Run: `make verify` (exit 0).

```bash
git add internal/relay/edge_test.go
git commit -m "test(relay): two relays behind one edge — control hop, NOTIFY drain, ownership move, dead-relay eviction

Part of #CHILD

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 13: `cmd/piper-edge` binary and packaging

**Files:**
- Create: `cmd/piper-edge/main.go`, `cmd/piper-edge/main_test.go`, `Dockerfile.edge`
- Modify: `Makefile`, `.goreleaser.yaml`, `CLAUDE.md`

**Interfaces:**
- Consumes: `relay.Open`, `relay.EdgeConfig`, `relay.ServeEdge`, `relay.NewEdgeMetrics`, `relay.NewLogRing`, `relay.NewOpsHandler`, `version.String`.

- [ ] **Step 1: Write the failing test**

`cmd/piper-edge/main_test.go`:

```go
package main

import "testing"

func TestVersionRequested(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}} {
		if !versionRequested(args) {
			t.Errorf("versionRequested(%v) = false", args)
		}
	}
	if versionRequested(nil) || versionRequested([]string{"serve"}) {
		t.Error("versionRequested true for a non-version invocation")
	}
}

func TestEdgeConfigFromEnv(t *testing.T) {
	t.Setenv("PIPER_EDGE_APEX", "public.getpiper.dev")
	t.Setenv("PIPER_EDGE_TLS_ADDR", "")
	t.Setenv("PIPER_EDGE_HTTP_ADDR", "0.0.0.0:8080")
	t.Setenv("PIPER_EDGE_TUNNEL_ADDR", "")
	t.Setenv("PIPER_EDGE_PROXY_PROTOCOL", "1")
	cfg, err := configFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Apex != "public.getpiper.dev" || cfg.TLSAddr != ":443" || cfg.HTTPAddr != "0.0.0.0:8080" || cfg.TunnelAddr != ":7000" || !cfg.ProxyProto {
		t.Fatalf("cfg = %+v", cfg)
	}
	t.Setenv("PIPER_EDGE_APEX", "")
	if _, err := configFromEnv(); err == nil {
		t.Fatal("missing apex accepted")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cmd/piper-edge/ -count=1`
Expected: `no Go files` / compile error.

- [ ] **Step 3: Write `main.go`**

```go
// Command piper-edge is the public entrypoint in front of N piper-relay
// processes: it routes :443 by SNI and :80 by Host to the relay that owns the
// agent's tunnel, and :7000 to the least-loaded relay, learning ownership
// from the relays' Postgres. It holds no certificate and terminates nothing.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/piperbox/piper/internal/relay"
	"github.com/piperbox/piper/internal/version"
)

// versionRequested reports whether args ask for the build version (cf.
// piper-relay); it also imports internal/version so the release ldflags
// stamp lands in this binary too.
func versionRequested(args []string) bool {
	return len(args) > 0 && (args[0] == "version" || args[0] == "--version")
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// configFromEnv reads the PIPER_EDGE_* listener settings. The prefix is
// deliberately not PIPER_RELAY_: a relay env file handed to an edge must
// fail, not half-work.
func configFromEnv() (relay.EdgeConfig, error) {
	apex := os.Getenv("PIPER_EDGE_APEX")
	if apex == "" {
		return relay.EdgeConfig{}, errors.New("PIPER_EDGE_APEX is required (the relays' apex, e.g. public.getpiper.dev)")
	}
	return relay.EdgeConfig{
		Apex:       apex,
		TLSAddr:    env("PIPER_EDGE_TLS_ADDR", ":443"),
		HTTPAddr:   env("PIPER_EDGE_HTTP_ADDR", ":80"),
		TunnelAddr: env("PIPER_EDGE_TUNNEL_ADDR", ":7000"),
		ProxyProto: os.Getenv("PIPER_EDGE_PROXY_PROTOCOL") == "1",
	}, nil
}

func main() {
	if versionRequested(os.Args[1:]) {
		fmt.Println(version.String())
		return
	}
	dsn := os.Getenv("PIPER_EDGE_DB_URL")
	if dsn == "" {
		log.Fatal("PIPER_EDGE_DB_URL is required (postgres://user:password@host/dbname — the relays' database)")
	}
	cfg, err := configFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	if cfg.ProxyProto {
		log.Print("piper-edge: PIPER_EDGE_PROXY_PROTOCOL=1 — :443/:80/:7000 require a PROXY v2 header (trusted balancer in front)")
	}
	st, err := relay.Open(dsn)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	// Infra-only ops surface, same shape and defaults as the relay's.
	opsAddr := env("PIPER_EDGE_OPS_ADDR", "127.0.0.1:9090")
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
	if metricsOn || logsOn {
		opsHandler := relay.NewOpsHandler(metrics, ring)
		go func() {
			log.Printf("piper-edge: ops endpoint %s (metrics=%v logs=%v)", opsAddr, metricsOn, logsOn)
			srv := &http.Server{Addr: opsAddr, Handler: opsHandler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
			if err := srv.ListenAndServe(); err != nil {
				log.Fatalf("ops endpoint: %v", err)
			}
		}()
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	log.Printf("piper-edge: TLS %s, HTTP %s, tunnel %s, apex %s", cfg.TLSAddr, cfg.HTTPAddr, cfg.TunnelAddr, cfg.Apex)
	if err := relay.ServeEdge(ctx, cfg, st, metrics); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
	log.Print("piper-edge: stopped")
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./cmd/piper-edge/ -count=1 -v && go build ./cmd/piper-edge`
Expected: PASS, binary builds.

- [ ] **Step 5: Packaging**

`Makefile` — add after the `piper-relay` build line:

```make
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o bin/piper-edge ./cmd/piper-edge
```

`Dockerfile.edge`:

```dockerfile
# syntax=docker/dockerfile:1

# Built by goreleaser (dockers_v2: piper-edge) from the prebuilt, version-
# stamped binary at <os>/<arch>/piper-edge — no compile stage, so a bare
# `docker build -f Dockerfile.edge .` will not work; use
# `goreleaser release --snapshot`. Same distroless base as the relay.

FROM gcr.io/distroless/static-debian12
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/piper-edge /usr/local/bin/piper-edge
EXPOSE 443 80 7000
ENTRYPOINT ["/usr/local/bin/piper-edge"]
```

`.goreleaser.yaml` — add a build entry after the `piper-relay` one:

```yaml
  - id: piper-edge
    main: ./cmd/piper-edge
    binary: piper-edge
    env:
      - CGO_ENABLED=0
    goos: [linux, darwin]
    goarch: [amd64, arm64, arm]
    goarm: ["7"]
    ignore:
      - goos: darwin
        goarch: arm
    ldflags:
      - -s -w -X github.com/piperbox/piper/internal/version.value={{ .Version }}
    mod_timestamp: "{{ .CommitTimestamp }}"
```

an archive entry after the `piper-relay` archive:

```yaml
  - id: piper-edge
    ids: [piper-edge]
    name_template: "piper-edge_{{ .Version }}_{{ .Os }}_{{ .Arch }}{{ if .Arm }}v{{ .Arm }}{{ end }}"
```

and a second `dockers_v2` entry after the relay's (same comment block applies; add one line to it: `piper-edge ships the same way: the scale-out entrypoint is container-only too.`):

```yaml
  - id: piper-edge
    ids: [piper-edge]
    dockerfile: Dockerfile.edge
    images:
      - ghcr.io/piperbox/piper-edge
    tags:
      - "{{ .Version }}"
      - "{{ if not .Prerelease }}latest{{ end }}"
    platforms:
      - linux/amd64
      - linux/arm64
    labels:
      org.opencontainers.image.title: piper-edge
      org.opencontainers.image.description: Piper edge — owner-routed L4 entrypoint in front of piper-relay
      org.opencontainers.image.source: https://github.com/piperbox/piper
      org.opencontainers.image.version: "{{ .Version }}"
      org.opencontainers.image.revision: "{{ .FullCommit }}"
      org.opencontainers.image.licenses: Apache-2.0
```

`CLAUDE.md` — replace the binary list line

```
- `piper-relay` — the optional self-hostable cloud relay (SNI passthrough + tunnel server). *Not built yet.*
```

with

```
- `piper-relay` — the optional self-hostable cloud relay (SNI passthrough + tunnel server).
- `piper-edge` — the L4 entrypoint in front of N relays: routes each connection to the relay that owns the agent's tunnel (container-only, like the relay).
```

- [ ] **Step 6: Check the release config**

Run: `goreleaser check; echo "rc=$?"` (install with `brew install goreleaser` if absent)
Expected: `rc=0` or `rc=2` (2 = the pre-existing deprecated `brews` warning); never 1.

Run: `make build && ls -la bin/piper-edge && ./bin/piper-edge --version`
Expected: the binary exists and prints the version.

- [ ] **Step 7: Verify and commit**

Run: `make verify` (exit 0).

```bash
git add cmd/piper-edge Dockerfile.edge Makefile .goreleaser.yaml CLAUDE.md
git commit -m "feat(edge): piper-edge binary, image and release entries

Part of #CHILD

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 14: Docs — runbook "Scale out" section and PROGRESS.md

**Files:**
- Modify: `docs/runbooks/relay-deploy.md`, `PROGRESS.md`

- [ ] **Step 1: Replace the single-replica bullets**

In `docs/runbooks/relay-deploy.md`, under "Run as a container", replace the bullet that starts `- **Still exactly one replica.**` (five lines) with:

```markdown
- **More than one replica:** put `piper-edge` in front — see [Scale out](#scale-out).
  Without it, scale up, never out: each agent's tunnel lives in one relay
  process's memory and traffic for it must reach that process.
```

Under "Single host with compose" → "Day-to-day afterwards", replace the bullet `- Still exactly one replica, for the reasons above; this is a packaging change, not a scaling one.` with:

```markdown
- Scaling out is a separate step ([Scale out](#scale-out)); the file as
  shipped is still one relay.
```

- [ ] **Step 2: Add the Scale out section**

Insert the following immediately before the `## Single host with compose` heading:

````markdown
## Scale out

`piper-edge` is the only public entrypoint; `piper-relay` runs as N processes
behind it and nothing else changes. Design:
[`docs/superpowers/specs/2026-09-04-relay-edge-ownership-design.md`](../superpowers/specs/2026-09-04-relay-edge-ownership-design.md).

**How it routes.** Each agent's tunnel is terminated by exactly one relay,
which records itself as the owner in Postgres (`relay_instances` with a 5 s
heartbeat, `agent_owners`). The edge keeps an in-memory copy, refreshed by
LISTEN/NOTIFY and a 15 s poll, and forwards raw bytes:

| Listener | Decision |
| --- | --- |
| `:443` | SNI → agent → owner relay. `api.<apex>` is pinned to the earliest-started relay until login-flow state moves to Postgres. |
| `:80` | Host → custom domain → owner relay (custom domains only, as today). |
| `:7000` | the live relay with the fewest sessions. |

Control-API calls that land on a non-owner relay hop to the owner over the
internal `:8080`; webhooks parked by any relay wake the owner. Relays never
forward data traffic to each other.

**Client IP chain.** The edge writes a PROXY v2 header on every backend
connection, so every relay behind an edge **must** run with
`PIPER_RELAY_PROXY_PROTOCOL=1` (and must not be reachable except through the
edge). If a cloud balancer that speaks PROXY protocol sits in front of the
edge, set `PIPER_EDGE_PROXY_PROTOCOL=1` on the edge too.

**Edge configuration.** `PIPER_EDGE_DB_URL` (the relays' database) and
`PIPER_EDGE_APEX` are required; `PIPER_EDGE_TLS_ADDR` / `HTTP_ADDR` /
`TUNNEL_ADDR` default to `:443` / `:80` / `:7000`; `PIPER_EDGE_OPS_ADDR`,
`PIPER_EDGE_METRICS`, `PIPER_EDGE_LOGS` mirror the relay's ops surface. The
edge holds no certificate, no GitHub App and no relay env: a relay env file
handed to it fails on the missing `PIPER_EDGE_*` names.

**Relay configuration.** `PIPER_RELAY_ADVERTISE_HOST` is the host edges dial;
it defaults to the first non-loopback IPv4 (the container or pod IP), which
is right on a bridge network and in a pod. Ports are the relay's own listener
ports.

**Single host (compose).** The edge owns the public ports on the host
network; relays scale on the bridge network with no published ports:

```yaml
services:
  postgres:
    # as in "Run as a container"
  edge:
    image: ghcr.io/piperbox/piper-edge:<version>
    restart: unless-stopped
    network_mode: host
    depends_on:
      postgres:
        condition: service_healthy
    environment:
      PIPER_EDGE_DB_URL: postgres://piper_relay:change-me@127.0.0.1:5432/piper_relay
      PIPER_EDGE_APEX: public.getpiper.dev
  relay:
    image: ghcr.io/piperbox/piper-relay:<version>
    restart: unless-stopped
    depends_on:
      postgres:
        condition: service_healthy
    env_file: piper-relay.env
    environment:
      PIPER_RELAY_DB_URL: postgres://piper_relay:change-me@postgres:5432/piper_relay
      PIPER_RELAY_PROXY_PROTOCOL: "1"
      PIPER_RELAY_API_ADDR: ":8080"
    volumes:
      - ./certs:/var/lib/piper-relay:ro
volumes:
  relay_pg:
```

Postgres must publish `127.0.0.1:5432` for the host-networked edge (the
Hetzner file already does). Add capacity with
`docker compose up -d --scale relay=3`; nothing is configured. The relay's
`:8080` is reachable only on the bridge network, which is what the control hop
relies on. Each relay's ops endpoint is on its own container IP; scrape it
there or publish it per replica.

**Kubernetes.** `piper-edge` as a Deployment behind a TCP Service that holds
the public IP (or a cloud NLB with PROXY protocol and
`PIPER_EDGE_PROXY_PROTOCOL=1`; otherwise `externalTrafficPolicy: Local`).
`piper-relay` as a Deployment with `PIPER_RELAY_ADVERTISE_HOST` from the
downward API (`status.podIP`), `PIPER_RELAY_PROXY_PROTOCOL=1`, and a
NetworkPolicy admitting only edge pods and other relays. An L7 ingress may
terminate `api.<apex>` with a cert-manager certificate and route it to the
relays' `:8080` Service — that port is plain HTTP written to be fronted with
TLS — with DNS pointing `api.<apex>` at the ingress and the wildcard at the
edge. The ingress must never take the wildcard: per-hostname routing to the
owning pod is the dynamic map the edge exists to hold, and box-held
certificates need L4 passthrough anyway.

**What still drops.** Restarting a relay drops its tunnels; agents redial
from a 1 s backoff and the edge places them on a survivor. Restarting the
edge on a single host drops every tunnel through it (on Kubernetes the
Service holds the port across a rolling restart). Both are closed by the
graceful-drain follow-up in the scale-out epic.
````

- [ ] **Step 3: PROGRESS.md**

After the line `- ✅ \`piper-relay\` store on Postgres — … [#515]` under Plan 2, add:

```markdown
- ✅ `piper-edge` + tunnel ownership — owner-routed L4 entrypoint in front of N relays (`relay_instances`/`agent_owners`, LISTEN/NOTIFY, control hop, NOTIFY-woken webhook drain); login-flow state and graceful drain tracked in the epic — [#CHILD](https://github.com/piperbox/piper/issues/CHILD) (child of epic [#EPIC](https://github.com/piperbox/piper/issues/EPIC))
```

Update the `_Last updated:` line's date to `2026-09-04` and prepend a clause: `relay scale-out child 1 (epic [#EPIC](https://github.com/piperbox/piper/issues/EPIC)) landed: `piper-edge` in front of N relays. Earlier:` — keep the rest of the sentence.

- [ ] **Step 4: Check links and formatting**

Run: `grep -n "Scale out\|scale-out" docs/runbooks/relay-deploy.md PROGRESS.md`
Expected: the anchor `#scale-out` is referenced twice and the heading exists once.

- [ ] **Step 5: Commit**

```bash
git add docs/runbooks/relay-deploy.md PROGRESS.md
git commit -m "docs(relay): scale-out runbook section and progress entry for piper-edge

Part of #CHILD

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 15: Final verification and pull request

- [ ] **Step 1: Full gate with Postgres available**

Run: `docker info >/dev/null && make verify; echo "rc=$?"`
Expected: `rc=0`. Then confirm nothing skipped silently:

Run: `go test ./internal/relay/ -count=1 -v 2>&1 | grep -c -- '--- SKIP'`
Expected: `0`.

- [ ] **Step 2: Race pass on the relay package**

Run: `go test ./internal/relay/ -race -count=1`
Expected: PASS.

- [ ] **Step 3: Push and open the PR**

```bash
git push -u origin claude/relay-scalable-reverse-proxy-510343
gh pr create --base main --title "[relay] piper-edge + tunnel ownership: scale out behind an owner-routed edge" --body "$(cat <<'BODY'
## Summary

- New tables `relay_instances` (5s heartbeat, advertised addrs) and `agent_owners` (upsert on register, conditional clear on unregister), plus NOTIFY on `piper_instances`, `piper_owners`, `piper_hostnames`, `piper_events`.
- `piper-relay`: joins the pool at start and leaves on SIGTERM; `PIPER_RELAY_ADVERTISE_HOST`; closes a session the agent has moved elsewhere; control proxy hops to the owner over `:8080` when the router misses; parked webhooks wake the owner through NOTIFY.
- `piper-edge` (new binary, `ghcr.io/piperbox/piper-edge`): routes `:443` by SNI and `:80` by Host to the owner, `:7000` to the least-loaded relay, pins `api.<apex>` to the earliest relay, splices with a PROXY v2 header, evicts a relay whose dial fails (one retry on `:7000` only), serves from memory when Postgres is away.
- Runbook "Scale out" section; PROGRESS entry; CLAUDE.md binary list.

Design: `docs/superpowers/specs/2026-09-04-relay-edge-ownership-design.md`. Plan: `docs/superpowers/plans/2026-09-04-relay-edge-ownership.md`.

Closes #CHILD
Part of #EPIC

## Test plan

- [ ] `make verify` green with Docker present (Postgres-backed tests ran, none skipped)
- [ ] `go test ./internal/relay/ -race` green
- [ ] `internal/relay/edge_test.go`: two relays behind one edge — placement, passthrough to the right relay with the client address intact, api pin, control hop, NOTIFY drain, ownership move, dead-relay eviction
- [ ] `goreleaser check` exit 0/2
- [ ] Existing loopback relay e2e unchanged

🤖 Generated with [Claude Code](https://claude.com/claude-code)
BODY
)"
```

- [ ] **Step 4: Watch CI**

Run: `gh pr checks --watch`
Expected: `verify` green. If the packaging job runs (it will — `.goreleaser.yaml` changed), the snapshot build must produce `piper-edge` archives and an image build for `Dockerfile.edge`.

After merge: the Hetzner cutover to edge + scaled relays is an ops step against the released image (compose file under `/opt/piper-relay`), not part of this PR.
