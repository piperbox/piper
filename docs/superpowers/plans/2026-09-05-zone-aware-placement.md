# Zone-aware Second-Session Placement Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When an agent already holds a tunnel session on a relay in zone X, the edge prefers a relay outside X for the next session, so one zone outage cannot take both sessions (#531).

**Architecture:** One nullable `zone` column on `relay_instances`, heartbeated by each relay from `PIPER_RELAY_ZONE`, flows through `InstanceRow` into the edge's in-memory pool. `pickTunnel` on the edge gains a leading comparator clause: a candidate whose zone is not among the agent's current owners' zones sorts first, then the existing fewest-sessions / earliest order. Zone is a preference, never a filter.

**Tech Stack:** Go, Postgres via `database/sql` (`lib/pq` already in use), Go's `testing` package. Store tests need a Postgres from `relaytest.DSN(t)` (they skip without one).

Spec: `docs/superpowers/specs/2026-09-05-zone-aware-placement-design.md`.

## Global Constraints

- `CGO_ENABLED=0` must build (`make cross`).
- Pre-1.x: `internal/relay/schema.sql` is edited in place; no migration.
- Env name is exactly `PIPER_RELAY_ZONE`; unset ⇒ empty ⇒ NULL ⇒ zone plays no part.
- Empty zone never clashes with anything. A zoneless candidate sorts with the preferred group.
- `ownersOf` and `pickAPI` are not changed.
- Commits: conventional-commit style, body `Part of #531`, trailer `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`.
- `make verify` must pass before the PR.

---

### Task 1: Store carries the zone

**Files:**
- Modify: `internal/relay/schema.sql` (the `relay_instances` table, ~line 150)
- Modify: `internal/relay/ownership.go` (`InstanceRow`, `UpsertInstance`, `instanceCols`, `scanInstance`, `ownerSelect`, `OwnerOf`, `Owners`)
- Test: `internal/relay/ownership_test.go`

**Interfaces:**
- Produces: `InstanceRow.Zone string` (empty = unset). Every store read of an instance row (`LiveInstances`, `OwnerOf`) fills it; `UpsertInstance` writes it (NULL when empty).

- [ ] **Step 1: Write the failing test**

Append to `internal/relay/ownership_test.go`:

```go
// The zone a relay heartbeats (#531) must come back through both instance
// projections the edge reads, and an unset zone must read back empty, not
// as a scan error on NULL.
func TestInstanceZoneRoundTripsThroughLiveInstancesAndOwnerOf(t *testing.T) {
	st := openTestStore(t)
	// Zone rides the first insert: the heartbeat's conflict branch leaves it
	// alone, since a zone is fixed for the life of a process id.
	zoned := &Instance{ID: "zoned", StartedAt: time.Now().Add(-time.Minute).UTC(), Zone: "eu-central-1a",
		TLSAddr: "127.0.0.1:1", HTTPAddr: "127.0.0.1:1", TunnelAddr: "127.0.0.1:1", APIAddr: "127.0.0.1:1"}
	if err := st.UpsertInstance(zoned.row(0)); err != nil {
		t.Fatal(err)
	}
	stampInstance(t, st, "zoneless", "127.0.0.1:1", time.Now())

	rows, err := st.LiveInstances()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Zone != "eu-central-1a" || rows[1].Zone != "" {
		t.Fatalf("LiveInstances zones = %+v, want [eu-central-1a \"\"]", rows)
	}

	en := enrollTestAgent(t, st)
	for _, id := range []string{"zoned", "zoneless"} {
		if err := st.SetOwner(en.BaseDomain, id); err != nil {
			t.Fatal(err)
		}
	}
	owners, err := st.OwnerOf(en.BaseDomain)
	if err != nil {
		t.Fatal(err)
	}
	if len(owners) != 2 || owners[0].Zone != "eu-central-1a" || owners[1].Zone != "" {
		t.Fatalf("OwnerOf zones = %+v, want [eu-central-1a \"\"]", owners)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/relay -run TestInstanceZoneRoundTrips -v`
Expected: compile error, `zoned.Zone undefined` / `rows[0].Zone undefined`.

- [ ] **Step 3: Add the column to the schema**

In `internal/relay/schema.sql`, inside `CREATE TABLE IF NOT EXISTS relay_instances (...)`, after the `draining` column (add a comma to the `draining` line):

```sql
    draining    BOOLEAN NOT NULL DEFAULT false,
    -- Failure zone from PIPER_RELAY_ZONE (#531): :7000 placement prefers a
    -- relay outside the zones already holding one of the agent's sessions.
    -- NULL means unknown and never counts as a clash.
    zone        TEXT
);
```

- [ ] **Step 4: Carry the zone through the store**

In `internal/relay/ownership.go`:

Add the field to `InstanceRow` after `Draining`:

```go
	Draining   bool
	// Zone is the relay's failure zone; "" when it did not set one.
	Zone string
```

Replace `UpsertInstance` with:

```go
// UpsertInstance inserts or refreshes an instance row — the heartbeat — and
// announces it on piper_instances. zone is fixed for a process id, so the
// conflict branch leaves it alone.
func (s *Store) UpsertInstance(r InstanceRow) error {
	if _, err := s.db.Exec(
		`INSERT INTO relay_instances(id, started_at, last_seen, sessions, tls_addr, http_addr, tunnel_addr, api_addr, draining, zone)
		 VALUES($1, $2, now(), $3, $4, $5, $6, $7, $8, $9)
		 ON CONFLICT(id) DO UPDATE SET last_seen = now(), sessions = excluded.sessions, draining = excluded.draining`,
		r.ID, r.StartedAt, r.Sessions, r.TLSAddr, r.HTTPAddr, r.TunnelAddr, r.APIAddr, r.Draining, nullIfEmpty(r.Zone)); err != nil {
		return err
	}
	return notify(s.db, chanInstances, r.ID)
}

// nullIfEmpty maps "" to SQL NULL for the nullable text columns.
func nullIfEmpty(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
```

Replace `instanceCols` and `scanInstance` with:

```go
const instanceCols = `id, started_at, sessions, tls_addr, http_addr, tunnel_addr, api_addr, draining, zone`

func scanInstance(sc interface{ Scan(...any) error }) (InstanceRow, error) {
	var r InstanceRow
	var zone sql.NullString
	err := sc.Scan(&r.ID, &r.StartedAt, &r.Sessions, &r.TLSAddr, &r.HTTPAddr, &r.TunnelAddr, &r.APIAddr, &r.Draining, &zone)
	r.Zone = zone.String
	return r, err
}
```

Replace `ownerSelect` with:

```go
var ownerSelect = `SELECT a.base_domain, i.id, i.started_at, i.sessions, i.tls_addr, i.http_addr, i.tunnel_addr, i.api_addr, i.draining, i.zone
	   FROM agent_owners o
	   JOIN agents a ON a.name = o.agent_name
	   JOIN relay_instances i ON i.id = o.instance_id
	  WHERE i.` + liveWhere
```

In `OwnerOf`, replace the scan block with:

```go
		var base string
		var r InstanceRow
		var zone sql.NullString
		if err := rows.Scan(&base, &r.ID, &r.StartedAt, &r.Sessions, &r.TLSAddr, &r.HTTPAddr, &r.TunnelAddr, &r.APIAddr, &r.Draining, &zone); err != nil {
			return nil, err
		}
		r.Zone = zone.String
		out = append(out, r)
```

In `Owners`, replace the scan block with:

```go
		var base string
		var r InstanceRow
		var zone sql.NullString
		if err := rows.Scan(&base, &r.ID, &r.StartedAt, &r.Sessions, &r.TLSAddr, &r.HTTPAddr, &r.TunnelAddr, &r.APIAddr, &r.Draining, &zone); err != nil {
			return nil, err
		}
		out[base] = append(out[base], r.ID)
```

- [ ] **Step 5: Add the field to `Instance` so the test's `zoned.Zone` compiles**

In `internal/relay/instance.go`, add to the `Instance` struct after `APIAddr`:

```go
	APIAddr    string
	// Zone is the failure zone from PIPER_RELAY_ZONE; "" when unset (#531).
	Zone string
```

and in `row()` add `Zone: i.Zone` to the literal:

```go
func (i *Instance) row(sessions int) InstanceRow {
	return InstanceRow{ID: i.ID, StartedAt: i.StartedAt, Sessions: sessions,
		TLSAddr: i.TLSAddr, HTTPAddr: i.HTTPAddr, TunnelAddr: i.TunnelAddr, APIAddr: i.APIAddr,
		Draining: i.draining.Load(), Zone: i.Zone}
}
```

- [ ] **Step 6: Run the store tests**

Run: `go test ./internal/relay -run 'TestInstanceZoneRoundTrips|TestLiveInstances|TestUpsertInstance|TestOwner|TestHeartbeat' -v`
Expected: all PASS. If the Postgres test DSN is unavailable they SKIP; in that case run `go vet ./internal/relay` and `go build ./...` instead and note the skip in the commit body.

- [ ] **Step 7: Commit**

```bash
git add internal/relay/schema.sql internal/relay/ownership.go internal/relay/instance.go internal/relay/ownership_test.go
git commit -m "feat(relay): relay_instances carries an optional zone

Part of #531

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 2: Relay reads PIPER_RELAY_ZONE

**Files:**
- Modify: `cmd/piper-relay/main.go:207-212`
- Test: `cmd/piper-relay/main_test.go`
- Test: `internal/relay/instance_test.go` (`TestHeartbeatPublishesSessionsAndLeavesOnStop`)

**Interfaces:**
- Consumes: `Instance.Zone string`, `InstanceRow.Zone string` from Task 1.
- Produces: nothing new; the heartbeat row now carries the relay's zone.

- [ ] **Step 1: Write the failing heartbeat test**

In `internal/relay/instance_test.go`, in `TestHeartbeatPublishesSessionsAndLeavesOnStop`, after the `NewInstance` error check add `inst.Zone = "zone-a"`, and extend the `waitCond` predicate to check the zone:

```go
	inst, err := NewInstance("127.0.0.1", ":443", ":80", ":7000", ":8080")
	if err != nil {
		t.Fatal(err)
	}
	inst.Zone = "zone-a"

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { inst.heartbeat(ctx, st, router); close(done) }()

	waitCond(t, 3*time.Second, "heartbeat row with one session and its zone", func() bool {
		rows, _ := st.LiveInstances()
		return len(rows) == 1 && rows[0].ID == inst.ID && rows[0].Sessions == 1 && rows[0].TLSAddr == "127.0.0.1:443" && rows[0].Zone == "zone-a"
	})
```

- [ ] **Step 2: Write the failing main test**

Append to `cmd/piper-relay/main_test.go`:

```go
func TestZoneEnvIsOptional(t *testing.T) {
	t.Setenv("PIPER_RELAY_ZONE", "")
	inst, err := newInstanceFromEnv(":443", ":80", ":7000", ":8080")
	if err != nil {
		t.Fatal(err)
	}
	if inst.Zone != "" {
		t.Fatalf("zone = %q, want empty when PIPER_RELAY_ZONE is unset", inst.Zone)
	}
	t.Setenv("PIPER_RELAY_ZONE", "eu-central-1a")
	inst, err = newInstanceFromEnv(":443", ":80", ":7000", ":8080")
	if err != nil {
		t.Fatal(err)
	}
	if inst.Zone != "eu-central-1a" {
		t.Fatalf("zone = %q, want eu-central-1a", inst.Zone)
	}
}
```

- [ ] **Step 3: Run both tests to verify they fail**

Run: `go test ./cmd/piper-relay -run TestZoneEnvIsOptional -v`
Expected: compile error, `undefined: newInstanceFromEnv`.

Run: `go test ./internal/relay -run TestHeartbeatPublishesSessionsAndLeavesOnStop -v`
Expected: PASS already (Task 1 wired `row()`); that is fine, it now pins the behaviour. If it SKIPs for lack of Postgres, note it.

- [ ] **Step 4: Factor the instance construction in main.go**

In `cmd/piper-relay/main.go`, replace the block

```go
	inst, err := relay.NewInstance(env("PIPER_RELAY_ADVERTISE_HOST", ""), tlsAddr, httpAddr, tunnelAddr, apiAddr)
	if err != nil {
		log.Fatalf("instance: %v", err)
	}
	log.Printf("piper-relay: instance %s advertising tls=%s http=%s tunnel=%s api=%s (PIPER_RELAY_ADVERTISE_HOST to override the host)",
		inst.ID, inst.TLSAddr, inst.HTTPAddr, inst.TunnelAddr, inst.APIAddr)
```

with

```go
	inst, err := newInstanceFromEnv(tlsAddr, httpAddr, tunnelAddr, apiAddr)
	if err != nil {
		log.Fatalf("instance: %v", err)
	}
	log.Printf("piper-relay: instance %s advertising tls=%s http=%s tunnel=%s api=%s zone=%q (PIPER_RELAY_ADVERTISE_HOST to override the host, PIPER_RELAY_ZONE to set the zone)",
		inst.ID, inst.TLSAddr, inst.HTTPAddr, inst.TunnelAddr, inst.APIAddr, inst.Zone)
```

and add, at file scope near the other helpers (after `env`):

```go
// newInstanceFromEnv mints this relay's pool identity: the advertise host
// from PIPER_RELAY_ADVERTISE_HOST (default: first non-loopback IPv4) and the
// optional failure zone from PIPER_RELAY_ZONE (#531).
func newInstanceFromEnv(tlsAddr, httpAddr, tunnelAddr, apiAddr string) (*relay.Instance, error) {
	inst, err := relay.NewInstance(env("PIPER_RELAY_ADVERTISE_HOST", ""), tlsAddr, httpAddr, tunnelAddr, apiAddr)
	if err != nil {
		return nil, err
	}
	inst.Zone = env("PIPER_RELAY_ZONE", "")
	return inst, nil
}
```

Update `TestAdvertiseHostEnvIsHonoured` in `main_test.go` to go through the helper so both env reads are covered by one path:

```go
func TestAdvertiseHostEnvIsHonoured(t *testing.T) {
	t.Setenv("PIPER_RELAY_ADVERTISE_HOST", "10.9.8.7")
	inst, err := newInstanceFromEnv(":443", ":80", ":7000", ":8080")
	if err != nil {
		t.Fatal(err)
	}
	if inst.TunnelAddr != "10.9.8.7:7000" {
		t.Fatalf("tunnel addr = %q", inst.TunnelAddr)
	}
}
```

- [ ] **Step 5: Run the tests**

Run: `go test ./cmd/piper-relay -run 'TestZoneEnvIsOptional|TestAdvertiseHostEnvIsHonoured' -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add cmd/piper-relay/main.go cmd/piper-relay/main_test.go internal/relay/instance_test.go
git commit -m "feat(relay): PIPER_RELAY_ZONE labels the relay's failure zone

Part of #531

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 3: Edge prefers a different zone

**Files:**
- Modify: `internal/relay/edge_state.go:152-172` (`pickTunnel`)
- Test: `internal/relay/edge_state_test.go`

**Interfaces:**
- Consumes: `InstanceRow.Zone string` from Task 1; the edge's `instances` map already holds full rows from `LiveInstances`.
- Produces: no signature change; `pickTunnel(base string, exclude map[string]bool) (InstanceRow, bool)` gains the zone preference.

- [ ] **Step 1: Write the failing test**

Append to `internal/relay/edge_state_test.go`:

```go
// zoned is instRow plus a zone label.
func zoned(id string, started time.Time, sessions int, zone string) InstanceRow {
	r := instRow(id, started, sessions)
	r.Zone = zone
	return r
}

// The second session prefers a relay outside the zone already holding the
// first (#531), ahead of load: a zone outage must not take both. Zone is a
// preference, not a filter — when every candidate shares the owner's zone,
// or nobody set one, placement is the old fewest-sessions rule.
func TestPickTunnelPrefersAnotherZoneThenFewestSessions(t *testing.T) {
	t0 := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	s := newEdgeState()
	s.setInstances([]InstanceRow{
		zoned("a1", t0, 1, "a"),
		zoned("a2", t0.Add(time.Second), 0, "a"),
		zoned("b1", t0.Add(time.Minute), 4, "b"),
		zoned("b2", t0.Add(2*time.Minute), 2, "b"),
	})
	s.setOwners(map[string][]string{"x.example": {"a1"}})

	if got, ok := s.pickTunnel("x.example", nil); !ok || got.ID != "b2" {
		t.Fatalf("owner in zone a: pickTunnel = %+v ok=%v, want b2 (other zone, fewest there)", got, ok)
	}
	if got, ok := s.pickTunnel("x.example", map[string]bool{"b2": true}); !ok || got.ID != "b1" {
		t.Fatalf("b2 undialable: pickTunnel = %+v ok=%v, want b1 (still zone b, however busy)", got, ok)
	}
	if got, ok := s.pickTunnel("x.example", map[string]bool{"b1": true, "b2": true}); !ok || got.ID != "a2" {
		t.Fatalf("zone b gone: pickTunnel = %+v ok=%v, want a2 (fall back to fewest sessions)", got, ok)
	}
	if got, ok := s.pickTunnel("y.example", nil); !ok || got.ID != "a2" {
		t.Fatalf("no owners yet: pickTunnel = %+v ok=%v, want a2 (zone plays no part)", got, ok)
	}

	// A zoneless candidate never clashes, so it sorts with the other-zone group.
	s.setInstances([]InstanceRow{
		zoned("a1", t0, 1, "a"),
		zoned("a2", t0.Add(time.Second), 0, "a"),
		zoned("none", t0.Add(time.Minute), 3, ""),
	})
	if got, ok := s.pickTunnel("x.example", nil); !ok || got.ID != "none" {
		t.Fatalf("zoneless candidate: pickTunnel = %+v ok=%v, want none (unknown zone is not a clash)", got, ok)
	}

	// A zoneless owner constrains nothing.
	s.setOwners(map[string][]string{"x.example": {"none"}})
	if got, ok := s.pickTunnel("x.example", nil); !ok || got.ID != "a2" {
		t.Fatalf("zoneless owner: pickTunnel = %+v ok=%v, want a2 (fewest sessions)", got, ok)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/relay -run TestPickTunnelPrefersAnotherZone -v`
Expected: FAIL at the first assertion with `want b2`, got `a2` (today's rule picks the idle relay in zone a).

- [ ] **Step 3: Add the zone preference to pickTunnel**

In `internal/relay/edge_state.go`, replace `pickTunnel` with:

```go
// pickTunnel is :7000 placement for the agent named base: a relay outside
// the zones already holding one of base's sessions (#531), then fewest
// sessions, then earliest started, among relays that do not already own
// base (#530). exclude names instances a failed dial has just ruled out.
// Zone is a preference — a relay with no zone never clashes, and a pool
// entirely in the owner's zone just falls through to the load order. The
// owner exclusion is soft: if it empties the pool the pick runs again over
// every relay and the one dialled rejects the duplicate after auth, so a
// claimed base never changes what an unauthenticated peer can observe.
func (s *edgeState) pickTunnel(base string, exclude map[string]bool) (InstanceRow, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	owned := map[string]bool{}
	zones := map[string]bool{}
	for _, id := range s.owners[base] {
		owned[id] = true
		if r, ok := s.instances[id]; ok && r.Zone != "" {
			zones[r.Zone] = true
		}
	}
	clashes := func(r InstanceRow) bool { return r.Zone != "" && zones[r.Zone] }
	less := func(a, b InstanceRow) bool {
		if ca, cb := clashes(a), clashes(b); ca != cb {
			return !ca
		}
		if a.Sessions != b.Sessions {
			return a.Sessions < b.Sessions
		}
		return earlier(a, b)
	}
	if r, ok := s.pickLocked(less, func(r InstanceRow) bool { return exclude[r.ID] || owned[r.ID] }); ok {
		return r, true
	}
	return s.pickLocked(less, func(r InstanceRow) bool { return exclude[r.ID] })
}
```

- [ ] **Step 4: Run the edge state tests**

Run: `go test ./internal/relay -run 'TestPickTunnel|TestPlacement|TestOwnersOf|TestEdgeState' -v`
Expected: all PASS, including the pre-existing `TestPickTunnelFewestSessionsThenEarliest` and `TestPickTunnelExcludesOwnersThenFallsBack` (no zones set there, so behaviour is unchanged).

- [ ] **Step 5: Commit**

```bash
git add internal/relay/edge_state.go internal/relay/edge_state_test.go
git commit -m "feat(relay): edge places the second session in another zone

Part of #531

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 4: Docs, progress, and the verify gate

**Files:**
- Modify: `docs/runbooks/relay-deploy.md` (relay configuration paragraph ~line 407; Kubernetes paragraph ~line 467)
- Modify: `deploy/compose/relay/docker-compose.yml:69-70`
- Modify: `docs/superpowers/specs/2026-09-05-tunnel-placement-prior-art.md` ("Proposed follow-up 5" section)
- Modify: `PROGRESS.md` (header line 5 and the scale-out list after line 58)

**Interfaces:** none.

- [ ] **Step 1: Runbook — relay configuration**

In `docs/runbooks/relay-deploy.md`, after the paragraph beginning `**Relay configuration.** \`PIPER_RELAY_ADVERTISE_HOST\`` (ends `ports.`), add:

```markdown
`PIPER_RELAY_ZONE` is optional: a failure-zone label (an availability zone,
a rack, a host) the edge uses when placing an agent's second tunnel session
— it prefers a relay whose zone differs from the one already holding a
session, so a zone outage does not take both (#531). Unset means unknown
and never counts as a clash, so set it on every relay or on none: a
zoneless relay is always eligible as "a different zone", and a mixed pool
can put both sessions in one physical zone without the edge knowing. On a
single host leave it unset.
```

- [ ] **Step 2: Runbook — where the value comes from**

In the `**Kubernetes.**` paragraph, after the sentence ending `NetworkPolicy admitting only edge pods and other relays.`, insert:

```markdown
`PIPER_RELAY_ZONE` comes from the node's `topology.kubernetes.io/zone`
label: the downward API cannot read node labels, so either run one relay
Deployment per zone with the value hard-coded and a node selector, or have
an init step copy the label in. On ECS the task metadata endpoint
(`${ECS_CONTAINER_METADATA_URI_V4}/task`, field `AvailabilityZone`) reports
the zone; an entrypoint exports it before `exec`ing `piper-relay`.
```

- [ ] **Step 3: Compose comment**

In `deploy/compose/relay/docker-compose.yml`, after the two-line comment ending `is what the edge dials.`, add:

```yaml
      # PIPER_RELAY_ZONE is left unset too: one host is one zone (#531).
```

- [ ] **Step 4: Prior-art spec — mark follow-up 5 landed**

In `docs/superpowers/specs/2026-09-05-tunnel-placement-prior-art.md`, change the heading `### Proposed follow-up 5: failure domains in placement (#531)` to `### Follow-up 5: failure domains in placement (#531) — landed` and append to that section, before `## What not to copy`:

```markdown
Landed as designed: see
[the zone-aware placement design](2026-09-05-zone-aware-placement-design.md).
Zone ranks ahead of session count, not as a literal tie-break, because a
tie-break that only fires on equal counts would rarely change placement.
```

- [ ] **Step 5: PROGRESS.md**

In `PROGRESS.md`, line 5, replace the opening `_Last updated: 2026-09-05 — relay scale-out child 4 (epic [#524](https://github.com/piperbox/piper/issues/524)) landed: two tunnel sessions per agent on two relays. Earlier:` with:

```markdown
_Last updated: 2026-09-05 — relay scale-out child 5 (epic [#524](https://github.com/piperbox/piper/issues/524)) landed: zone-aware placement for the second tunnel session. Earlier: relay scale-out child 4 (epic [#524](https://github.com/piperbox/piper/issues/524)) landed: two tunnel sessions per agent on two relays. Earlier:
```

After the line 58 entry (`- ✅ two tunnel sessions per agent — ...`), add:

```markdown
- ✅ zone-aware second-session placement — optional `PIPER_RELAY_ZONE` on `relay_instances`; `:7000` prefers a relay outside the zones already holding one of the agent's sessions, ahead of load, falling back to fewest sessions — [#531](https://github.com/piperbox/piper/issues/531) (child of epic [#524](https://github.com/piperbox/piper/issues/524))
```

- [ ] **Step 6: Run the full verify gate**

Run: `make verify`
Expected: exit status 0. Judge by exit status, not by grepping output — it halts at the first failing gate (gofmt → vet → test → cross). If gofmt fails, run `make fmt` and re-run.

- [ ] **Step 7: Commit**

```bash
git add docs/runbooks/relay-deploy.md deploy/compose/relay/docker-compose.yml docs/superpowers/specs/2026-09-05-tunnel-placement-prior-art.md PROGRESS.md
git commit -m "docs(relay): PIPER_RELAY_ZONE and zone-aware placement

Part of #531

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 5: Pull request

- [ ] **Step 1: Push and open the PR**

```bash
git push -u origin claude/531-dc2aaa
gh pr create --base main --title "[relay] zone-aware placement for the second tunnel session" --body "$(cat <<'EOF'
Closes #531. Fifth child of the scale-out epic #524.

Design: `docs/superpowers/specs/2026-09-05-zone-aware-placement-design.md`.

- `relay_instances.zone` (nullable), heartbeated from the optional `PIPER_RELAY_ZONE`.
- `piper-edge` `:7000` placement prefers a relay outside the zones already holding one of the agent's sessions, ahead of load, falling back to fewest sessions. Empty zone never clashes, so pools that never set it are placed exactly as before.
- Runbook: what to set, where Kubernetes/ECS expose it, and the all-or-none caveat.

`make verify` green.

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```
