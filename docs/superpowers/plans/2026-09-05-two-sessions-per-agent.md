# Two Tunnel Sessions Per Agent Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every agent holds two tunnel sessions on two different relays, so a relay going away is never an agent's only link, and the edge keeps routing through the survivor.

**Architecture:** The tunnel handshake splits into a routing preface (base domain) and a credential frame, so `piper-edge` can peek the base and place a second dial away from the relays that already own the agent. `agent_owners` is keyed on `(agent, instance)` and every read returns the set of live owners; the edge picks among them and retries once on dial failure. Relays reject a duplicate session after auth, derive their hostname and custom-domain routes from Postgres on register and on `piper_hostnames` NOTIFY, and the agent runs two dial loops.

**Tech Stack:** Go, `modernc.org/sqlite`-free relay on Postgres (`pgx`), `hashicorp/yamux`, `pires/go-proxyproto`. Tests in `internal/relay` need the `relaytest` Postgres (`make test` provisions it; see `internal/relay/relaytest`).

Spec: [`docs/superpowers/specs/2026-09-05-two-sessions-per-agent-design.md`](../specs/2026-09-05-two-sessions-per-agent-design.md). Issue: #530.

## Global Constraints

- `CGO_ENABLED=0` everywhere; `make cross` must pass.
- Pre-1.x: schema and wire formats change in place. No compat readers, no migrations. `schema.sql` is edited directly.
- Layering: `tunnel` knows nothing of `relay`; `agent` imports only `tunnel`; `relay` imports `tunnel`. Nothing imports up.
- Deployment status strings and defaults are untouched by this work.
- Every commit: conventional-commit style, `Part of #530` in the body, and the trailer `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`.
- Run `make verify` (gofmt → vet → test → cross) before claiming any task done. Judge it by exit status, not by grepping its output.
- Tests: package-level `go test ./internal/relay/ -run <Name>` runs one test; `go test ./internal/relay/` runs the package. The relay package needs Postgres; if `relaytest` reports it cannot provision one, stop and say so rather than skipping.

## File map

| File | Responsibility after this plan |
| --- | --- |
| `internal/tunnel/tunnel.go` | two-frame handshake (preface, credential); `ErrDuplicateSession`; `ReadPreface` for the edge |
| `internal/relay/schema.sql` | `agent_owners` keyed on `(agent_name, instance_id)` |
| `internal/relay/ownership.go` | `SetOwner` idempotent insert; `ClearOwner`; `OwnerOf`/`Owners` return sets |
| `internal/relay/hostnames.go` | `HostnamesFor(base)` |
| `internal/relay/edge_state.go` | owners as sets; `ownersOf` ordering rule; `pickTunnel(base, exclude)` with soft exclusion |
| `internal/relay/edge.go` | `readPreface`; :7000 placement by base; :443/:80 retry once across owners |
| `internal/relay/router.go` | `Register` returns `tunnel.ErrDuplicateSession` on a held base; `SetHosts`, `SetCustom` |
| `internal/relay/server.go` | duplicate check in the auth callback; `syncRoutes` |
| `internal/relay/instance.go` | `reassertOwnership` every beat; `RunInstance` listens on `piper_events` + `piper_hostnames`, no stale-close |
| `internal/relay/proxy.go` | control hop picks any live owner that is not itself |
| `internal/agent/tunnelclient.go` | two slots; duplicate backoff |
| `docs/runbooks/relay-deploy.md`, `PROGRESS.md` | docs |

---

### Task 1: Tunnel handshake — preface frame, credential frame, duplicate sentinel

**Files:**
- Modify: `internal/tunnel/tunnel.go`
- Test: `internal/tunnel/tunnel_test.go`

**Interfaces:**
- Produces: `var ErrDuplicateSession error`; `func ReadPreface(r io.Reader) (base string, raw []byte, err error)`; `Dial`/`Serve` signatures unchanged. `Serve` passes `(credential.Token, preface.BaseDomain)` to `auth`. An `auth` that returns an error wrapping `ErrDuplicateSession` makes the peer's `Dial` return an error wrapping `ErrDuplicateSession`; any other error yields the existing undifferentiated reason.

- [ ] **Step 1: Write the failing tests**

Append to `internal/tunnel/tunnel_test.go`:

```go
// The handshake is two frames: a routing preface carrying only the base
// domain, then the credential. ReadPreface is what the edge peeks: it must
// return the base and exactly the preface's bytes, leaving the credential
// frame unread on the stream.
func TestReadPrefaceReturnsBaseAndLeavesCredentialUnread(t *testing.T) {
	c, s := net.Pipe()
	t.Cleanup(func() { c.Close(); s.Close() })
	go Dial(c, "tok-123", "alice.example.com")

	base, raw, err := ReadPreface(s)
	if err != nil {
		t.Fatalf("ReadPreface: %v", err)
	}
	if base != "alice.example.com" {
		t.Fatalf("base = %q", base)
	}
	if strings.Contains(string(raw), "tok-123") {
		t.Fatalf("preface bytes carry the token: %q", raw)
	}
	// The raw bytes are a complete frame: header + payload, replayable as is.
	if got := binary.BigEndian.Uint16(raw[:2]); int(got) != len(raw)-2 {
		t.Fatalf("raw frame length header %d, want %d", got, len(raw)-2)
	}
	// The next frame on the stream is the credential, untouched.
	next, err := readFrame(s)
	if err != nil {
		t.Fatalf("credential frame: %v", err)
	}
	var cred credential
	if err := json.Unmarshal(next, &cred); err != nil || cred.Token != "tok-123" {
		t.Fatalf("credential frame = %q (%v), want the token", next, err)
	}
}

func TestReadPrefaceRejectsAnEmptyBase(t *testing.T) {
	c, s := net.Pipe()
	t.Cleanup(func() { c.Close(); s.Close() })
	go func() {
		payload, _ := json.Marshal(preface{})
		_ = writeFrame(c, payload)
	}()
	if _, _, err := ReadPreface(s); err == nil {
		t.Fatal("ReadPreface accepted a preface with no base domain")
	}
}

// A duplicate is the one rejection the relay may name: it is checked only
// after the token has been validated, so the peer has proven who it is.
func TestDuplicateRejectionSurvivesTheAck(t *testing.T) {
	c, s := net.Pipe()
	t.Cleanup(func() { c.Close(); s.Close() })
	go Serve(s, func(token, base string) error { return ErrDuplicateSession })

	_, err := Dial(c, "tok", "alice.example.com")
	if !errors.Is(err, ErrDuplicateSession) {
		t.Fatalf("Dial error = %v, want ErrDuplicateSession", err)
	}
}

func TestOtherRejectionsStayUndifferentiated(t *testing.T) {
	c, s := net.Pipe()
	t.Cleanup(func() { c.Close(); s.Close() })
	go Serve(s, func(token, base string) error { return errors.New("bad token") })

	_, err := Dial(c, "tok", "alice.example.com")
	if err == nil || errors.Is(err, ErrDuplicateSession) {
		t.Fatalf("Dial error = %v, want a generic rejection", err)
	}
	if !strings.Contains(err.Error(), rejectedReason) {
		t.Fatalf("Dial error = %q, want the undifferentiated reason %q", err, rejectedReason)
	}
}
```

Add `"encoding/binary"`, `"encoding/json"`, `"errors"` to the test file's imports if not already present (`errors` and `strings` are).

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/tunnel/ -run 'TestReadPreface|TestDuplicateRejection|TestOtherRejections' 2>&1 | head -20`
Expected: compile errors naming `ReadPreface`, `preface`, `credential`, `ErrDuplicateSession`.

- [ ] **Step 3: Implement the two-frame handshake**

In `internal/tunnel/tunnel.go`, add `"errors"` to the imports. Replace the `handshake` type with:

```go
// The handshake is two frames, written in one flush by Dial. The preface is
// the routing key: it carries only the base domain, so the edge can peek it
// (ReadPreface) and place the dial without ever reading a credential — the
// same shape as SNI on :443 and the Host line on :80. The credential frame
// carries the token and nothing else, so the two frames cannot disagree
// about the base; the relay's auth check that the token's enrolled base
// equals the claimed one is what makes the unauthenticated preface
// trustworthy once auth passes.
type preface struct {
	BaseDomain string `json:"base_domain"`
}

type credential struct {
	Token string `json:"token"`
}

// ErrDuplicateSession is what a relay's Auth returns when it already holds a
// session for the claimed base. It is the one rejection Serve names to the
// peer (duplicateReason): it can only be raised after the token was
// validated, so naming it confirms nothing to an unauthenticated peer. Dial
// maps the reason back to this sentinel so the agent can errors.Is it.
var ErrDuplicateSession = errors.New("relay already holds a session for this agent")

// duplicateReason is the ack text for ErrDuplicateSession.
const duplicateReason = "duplicate session"

// ReadPreface reads exactly the preface frame from r and returns the base
// domain it names plus the frame's raw bytes (length header included), so a
// peeking proxy can replay them verbatim. Nothing past the preface is
// consumed. An empty base is an error: the frame is the routing key and a
// blank one routes nowhere.
func ReadPreface(r io.Reader) (string, []byte, error) {
	payload, raw, err := readFrameRaw(r)
	if err != nil {
		return "", nil, err
	}
	var p preface
	if err := json.Unmarshal(payload, &p); err != nil {
		return "", nil, fmt.Errorf("malformed preface: %w", err)
	}
	if p.BaseDomain == "" {
		return "", nil, errors.New("preface names no base domain")
	}
	return p.BaseDomain, raw, nil
}
```

Change `readFrame` to be built on a raw-returning variant:

```go
// readFrameRaw reads one frame and returns both its payload and the raw
// bytes (header + payload) that carried it.
func readFrameRaw(r io.Reader) (payload, raw []byte, err error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, nil, err
	}
	buf := make([]byte, binary.BigEndian.Uint16(hdr[:]))
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, nil, err
	}
	return buf, append(hdr[:], buf...), nil
}

func readFrame(r io.Reader) ([]byte, error) {
	payload, _, err := readFrameRaw(r)
	return payload, err
}
```

In `Dial`, replace the marshal-and-write of the single frame with both frames under the one write deadline:

```go
	prefacePayload, _ := json.Marshal(preface{BaseDomain: baseDomain})
	credPayload, _ := json.Marshal(credential{Token: token})
	// Bound the write like the ack wait below: a relay that accepts the
	// connection and then stops reading lets the send buffer and receive
	// window fill, and an undeadlined write pins the reconnect loop the
	// same way an undeadlined read does. Both frames go under one deadline
	// and, on a TCP conn, out in one flush.
	_ = conn.SetWriteDeadline(time.Now().Add(handshakeWriteTimeout))
	writeErr := writeFrame(conn, prefacePayload)
	if writeErr == nil {
		writeErr = writeFrame(conn, credPayload)
	}
	_ = conn.SetWriteDeadline(time.Time{})
	if writeErr != nil {
		return nil, fmt.Errorf("writing handshake: %w", writeErr)
	}
```

and change the ack check so the duplicate reason maps to the sentinel:

```go
	if ack.Error == duplicateReason {
		return nil, fmt.Errorf("relay rejected %s: %w", baseDomain, ErrDuplicateSession)
	}
	if ack.Error != "" {
		return nil, fmt.Errorf("relay rejected %s: %s", baseDomain, ack.Error)
	}
```

In `Serve`, replace the single-frame read and unmarshal with two reads under the pre-auth deadline, and pick the reason by error:

```go
	_ = conn.SetReadDeadline(time.Now().Add(preAuthReadTimeout))
	base, _, err := ReadPreface(conn)
	if err != nil {
		_ = conn.SetReadDeadline(time.Time{})
		return nil, err
	}
	credPayload, err := readFrame(conn)
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		return nil, err
	}
	var cred credential
	if err := json.Unmarshal(credPayload, &cred); err != nil {
		return nil, err
	}
	if err := auth(cred.Token, base); err != nil {
		// Best-effort: tell the agent *why* before dropping it, so a stranded
		// enrollment is self-diagnosing instead of an invisible reconnect loop
		// (#400). A failed write changes nothing — the connection is going away.
		// Only a duplicate is named: it is raised after the token was
		// validated, so it confirms nothing to an unauthenticated peer.
		reason := rejectedReason
		if errors.Is(err, ErrDuplicateSession) {
			reason = duplicateReason
		}
		_ = conn.SetWriteDeadline(time.Now().Add(ackReadTimeout))
		ackPayload, _ := json.Marshal(handshakeAck{Error: reason})
		_ = writeFrame(conn, ackPayload)
		_ = conn.SetWriteDeadline(time.Time{})
		return nil, err
	}
```

and use `base` where `hs.BaseDomain` was used when building the returned `Session`.

- [ ] **Step 4: Run the tunnel package tests**

Run: `go test ./internal/tunnel/`
Expected: PASS. `TestServeClearsPreAuthDeadlineBeforeAuth` and `TestServe_PreAuthDeadline` must still pass: the deadline is set before the first read and cleared after the second.

- [ ] **Step 5: Build everything and run the packages that dial tunnels**

Run: `go build ./... && go test ./internal/agent/ ./internal/relay/ 2>&1 | tail -5`
Expected: PASS. Every caller uses `Dial`/`Serve` unchanged; only the bytes on the wire changed.

- [ ] **Step 6: Commit**

```bash
git add internal/tunnel/tunnel.go internal/tunnel/tunnel_test.go
git commit -m "feat(tunnel): split the handshake into a routing preface and a credential frame

The preface carries only the base domain so an edge can peek it without
reading a credential; ErrDuplicateSession is the one rejection the relay
may name, since it is raised only after auth.

Part of #530.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 2: Ownership is every live row

**Files:**
- Modify: `internal/relay/schema.sql:163-170`
- Modify: `internal/relay/ownership.go` (`SetOwner`, `OwnerOf`, `Owners`)
- Modify: `internal/relay/edge_state.go` (owners map, `setOwners`, `setOwner`, `evict`, `ownersOf`, `ownerOf`)
- Modify: `internal/relay/edge.go:159-170` (`onNotify`)
- Modify: `internal/relay/instance.go` (`reassertOwnership`, `RunInstance`)
- Modify: `internal/relay/proxy.go:208-216` (`liveOwner`)
- Test: `internal/relay/ownership_test.go`, `internal/relay/edge_state_test.go`, `internal/relay/instance_test.go`, `internal/relay/server_test.go`, `internal/relay/edge_test.go`, `internal/relay/notify_test.go`

**Interfaces:**
- Produces: `func (s *Store) SetOwner(baseDomain, instanceID string) error` (idempotent; notifies only on insert; `ErrBadToken` for an unknown agent); `func (s *Store) OwnerOf(baseDomain string) ([]InstanceRow, error)` (live owners ordered by `started_at, id`); `func (s *Store) Owners() (map[string][]string, error)`; `func (s *edgeState) setOwners(map[string][]string)`; `func (s *edgeState) setOwner(agent string, ids []string)`; `func (s *edgeState) ownersOf(agent string) []InstanceRow` (non-draining first, then fewest sessions, then earliest); `func (s *edgeState) ownerOf(agent string) (InstanceRow, bool)` (first of `ownersOf`).
- Test helper produced: `ownerIDs(t, st, base) []string` in `ownership_test.go`.

- [ ] **Step 1: Write the failing store tests**

In `internal/relay/ownership_test.go`, add the helper and replace `TestSetOwnerOverwritesAndClearOwnerIsConditional`, `TestOwnerOfIgnoresDeadOwner`, `TestOwnersMapsBaseDomainToLiveInstance`, and the tail of `TestUpsertInstanceRoundTripsDraining`:

```go
// ownerIDs is the live owner set for base, in OwnerOf's order.
func ownerIDs(t *testing.T, st *Store, base string) []string {
	t.Helper()
	rows, err := st.OwnerOf(base)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids
}

func TestSetOwnerAddsRowsAndClearOwnerRemovesOnlyItsOwn(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	t0 := time.Now()
	stampInstance(t, st, "a", "127.0.0.1:1", t0)
	stampInstance(t, st, "b", "127.0.0.1:1", t0.Add(time.Second))

	if err := st.SetOwner(en.BaseDomain, "b"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetOwner(en.BaseDomain, "a"); err != nil {
		t.Fatal(err)
	}
	if got := ownerIDs(t, st, en.BaseDomain); strings.Join(got, ",") != "a,b" {
		t.Fatalf("owners = %v, want [a b] (both rows, earliest started first)", got)
	}
	// a's late unregister must not remove b's row.
	if err := st.ClearOwner(en.BaseDomain, "a"); err != nil {
		t.Fatal(err)
	}
	if got := ownerIDs(t, st, en.BaseDomain); strings.Join(got, ",") != "b" {
		t.Fatalf("owners after a cleared = %v, want [b]", got)
	}
	if err := st.ClearOwner(en.BaseDomain, "b"); err != nil {
		t.Fatal(err)
	}
	if got := ownerIDs(t, st, en.BaseDomain); len(got) != 0 {
		t.Fatalf("owners after both cleared = %v, want none", got)
	}
}

// SetOwner is what the heartbeat calls for every held base on every beat,
// so a repeat must be a silent no-op: one row, one NOTIFY.
func TestSetOwnerIsIdempotentAndNotifiesOnce(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	stampInstance(t, st, "a", "127.0.0.1:1", time.Now())
	got, _ := startListener(t, st, chanOwners)

	for i := 0; i < 3; i++ {
		if err := st.SetOwner(en.BaseDomain, "a"); err != nil {
			t.Fatalf("SetOwner #%d: %v", i+1, err)
		}
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM agent_owners`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("agent_owners rows = %d (%v), want 1", n, err)
	}
	// A probe after the three inserts bounds the wait: everything the
	// inserts fired has landed once the probe has.
	if err := notify(st.db, chanOwners, "probe"); err != nil {
		t.Fatal(err)
	}
	seen := 0
	for {
		select {
		case nt := <-got:
			if nt.payload == "probe" {
				if seen != 1 {
					t.Fatalf("piper_owners fired %d times for one new row, want 1", seen)
				}
				return
			}
			if nt.payload == en.BaseDomain {
				seen++
			}
		case <-time.After(3 * time.Second):
			t.Fatal("probe NOTIFY never arrived")
		}
	}
}

func TestOwnerOfIgnoresDeadOwner(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	stampInstance(t, st, "a", "127.0.0.1:1", time.Now())
	stampInstance(t, st, "b", "127.0.0.1:1", time.Now())
	for _, id := range []string{"a", "b"} {
		if err := st.SetOwner(en.BaseDomain, id); err != nil {
			t.Fatal(err)
		}
	}
	ageInstance(t, st, "a")
	if got := ownerIDs(t, st, en.BaseDomain); strings.Join(got, ",") != "b" {
		t.Fatalf("owners with a dead = %v, want [b]", got)
	}
	owners, err := st.Owners()
	if err != nil {
		t.Fatal(err)
	}
	if got := owners[en.BaseDomain]; strings.Join(got, ",") != "b" {
		t.Fatalf("Owners() = %v, want %s→[b]", owners, en.BaseDomain)
	}
}

func TestOwnersMapsBaseDomainToItsLiveInstances(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	t0 := time.Now()
	stampInstance(t, st, "a", "127.0.0.1:1", t0)
	stampInstance(t, st, "b", "127.0.0.1:1", t0.Add(time.Second))
	for _, id := range []string{"b", "a"} {
		if err := st.SetOwner(en.BaseDomain, id); err != nil {
			t.Fatal(err)
		}
	}
	owners, err := st.Owners()
	if err != nil {
		t.Fatal(err)
	}
	if got := owners[en.BaseDomain]; strings.Join(got, ",") != "a,b" {
		t.Fatalf("Owners() = %v, want %s→[a b]", owners, en.BaseDomain)
	}
}
```

Replace the last five lines of `TestUpsertInstanceRoundTripsDraining` with:

```go
	owners, err := st.OwnerOf(en.BaseDomain)
	if err != nil || len(owners) != 1 || !owners[0].Draining {
		t.Fatalf("OwnerOf = %+v err=%v, want the one draining owner", owners, err)
	}
```

Add `"strings"` to the imports of `ownership_test.go`.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/relay/ -run 'TestSetOwner|TestOwnerOf|TestOwners|TestUpsertInstanceRoundTripsDraining' 2>&1 | head`
Expected: compile errors (`OwnerOf` returns three values; `owners[...]` is a string).

- [ ] **Step 3: Change the schema and the store**

In `internal/relay/schema.sql`, replace the `agent_owners` block:

```sql
-- agent_owners says which instances terminate an agent's tunnels: one row
-- per live session, two in steady state (#530). Ownership is every live row,
-- not whoever registered last. The instance cascade takes ownership down
-- with a deleted instance row; the agents cascade lets DeleteAgent stay
-- unchanged.
CREATE TABLE IF NOT EXISTS agent_owners (
    agent_name  TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    instance_id TEXT NOT NULL REFERENCES relay_instances(id) ON DELETE CASCADE,
    since       TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (agent_name, instance_id)
);
```

In `internal/relay/ownership.go`, replace `SetOwner`, `OwnerOf`, and `Owners`:

```go
// SetOwner records that instanceID terminates one of baseDomain's tunnels.
// Idempotent: a row that already exists is left alone and nothing is
// announced, which is what lets the heartbeat call it for every held base on
// every beat. A new row is announced on piper_owners. Unknown agents are
// ErrBadToken, told apart from "already there" by a separate existence check
// in the same statement.
func (s *Store) SetOwner(baseDomain, instanceID string) error {
	var inserted int
	var known bool
	err := s.db.QueryRow(
		`WITH ins AS (
		    INSERT INTO agent_owners(agent_name, instance_id, since)
		    SELECT name, $2, now() FROM agents WHERE base_domain=$1
		    ON CONFLICT DO NOTHING
		    RETURNING 1)
		 SELECT (SELECT count(*) FROM ins), EXISTS(SELECT 1 FROM agents WHERE base_domain=$1)`,
		baseDomain, instanceID).Scan(&inserted, &known)
	if err != nil {
		return err
	}
	if !known {
		return ErrBadToken
	}
	if inserted == 0 {
		return nil
	}
	return notify(s.db, chanOwners, baseDomain)
}
```

Keep `ClearOwner` as it is. Then:

```go
// ownerCols is the instance projection OwnerOf and Owners share, with the
// same total order LiveInstances uses so callers can tie-break the same way.
const ownerSelect = `SELECT a.base_domain, i.id, i.started_at, i.sessions, i.tls_addr, i.http_addr, i.tunnel_addr, i.api_addr, i.draining
	   FROM agent_owners o
	   JOIN agents a ON a.name = o.agent_name
	   JOIN relay_instances i ON i.id = o.instance_id
	  WHERE i.` + liveWhere

// OwnerOf returns the live instances holding baseDomain's tunnels, earliest
// started first. Empty when nobody does, or when every recorded owner has
// stopped heartbeating.
func (s *Store) OwnerOf(baseDomain string) ([]InstanceRow, error) {
	rows, err := s.db.Query(ownerSelect+` AND a.base_domain=$1 ORDER BY i.started_at, i.id`, baseDomain)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InstanceRow
	for rows.Next() {
		var base string
		var r InstanceRow
		if err := rows.Scan(&base, &r.ID, &r.StartedAt, &r.Sessions, &r.TLSAddr, &r.HTTPAddr, &r.TunnelAddr, &r.APIAddr, &r.Draining); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Owners maps every agent base domain to the ids of its live owners,
// earliest started first.
func (s *Store) Owners() (map[string][]string, error) {
	rows, err := s.db.Query(ownerSelect + ` ORDER BY i.started_at, i.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var base string
		var r InstanceRow
		if err := rows.Scan(&base, &r.ID, &r.StartedAt, &r.Sessions, &r.TLSAddr, &r.HTTPAddr, &r.TunnelAddr, &r.APIAddr, &r.Draining); err != nil {
			return nil, err
		}
		out[base] = append(out[base], r.ID)
	}
	return out, rows.Err()
}
```

Remove the now-unused `"database/sql"` and `"errors"` imports from `ownership.go` only if nothing else in the file uses them (`scanOne` still uses both; keep them).

- [ ] **Step 4: Update the edge state to sets, with the owner ordering rule**

In `internal/relay/edge_state.go`, change the field to `owners map[string][]string // agent base domain → live owner ids`, initialise it as `map[string][]string{}` in `newEdgeState`, and replace `setOwners`, `setOwner`, `evict`, and `ownerOf`:

```go
func (s *edgeState) setOwners(m map[string][]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.owners = make(map[string][]string, len(m))
	for agent, ids := range m {
		s.owners[agent] = append([]string(nil), ids...)
	}
}

// setOwner replaces one agent's owner set; an empty set clears it.
func (s *edgeState) setOwner(agent string, ids []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(ids) == 0 {
		delete(s.owners, agent)
		return
	}
	s.owners[agent] = append([]string(nil), ids...)
}

// evict drops an instance the edge found dead on dial, out of every owner
// set it was in — the in-memory twin of the agent_owners cascade.
func (s *edgeState) evict(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.instances, id)
	for agent, ids := range s.owners {
		kept := make([]string, 0, len(ids))
		for _, o := range ids {
			if o != id {
				kept = append(kept, o)
			}
		}
		if len(kept) == 0 {
			delete(s.owners, agent)
		} else {
			s.owners[agent] = kept
		}
	}
}

// ownersOf lists the live instances owning agent in routing preference:
// non-draining first, then fewest sessions, then earliest started. A
// draining relay still holds its session and keeps serving, so it stays a
// candidate; it only loses ties, which is what shifts new connections to
// the survivor the moment a rolling restart begins (#530).
func (s *edgeState) ownersOf(agent string) []InstanceRow {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []InstanceRow
	for _, id := range s.owners[agent] {
		if r, ok := s.instances[id]; ok {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Draining != b.Draining {
			return !a.Draining
		}
		if a.Sessions != b.Sessions {
			return a.Sessions < b.Sessions
		}
		return earlier(a, b)
	})
	return out
}

// ownerOf is the first choice among ownersOf.
func (s *edgeState) ownerOf(agent string) (InstanceRow, bool) {
	owners := s.ownersOf(agent)
	if len(owners) == 0 {
		return InstanceRow{}, false
	}
	return owners[0], true
}
```

- [ ] **Step 5: Update the callers**

`internal/relay/edge.go`, the `chanOwners` case of `onNotify`:

```go
	case chanOwners:
		owners, err := e.st.OwnerOf(payload)
		if err != nil {
			e.dbLost(err)
			return
		}
		e.dbBack()
		ids := make([]string, 0, len(owners))
		for _, o := range owners {
			ids = append(ids, o.ID)
		}
		e.state.setOwner(payload, ids)
```

`internal/relay/proxy.go`, `liveOwner`:

```go
	// liveOwner is the cluster half of liveness: a live instance holding
	// base that is not this process. With two sessions per agent (#530) any
	// other owner will do. The error surfaces a broken lookup to the caller
	// instead of swallowing it — a query-level failure here must not be
	// silently read as "nobody owns it".
	liveOwner := func(base string) (InstanceRow, bool, error) {
		if self == nil {
			return InstanceRow{}, false, nil
		}
		owners, err := st.OwnerOf(base)
		if err != nil {
			return InstanceRow{}, false, err
		}
		for _, o := range owners {
			if o.ID != self.ID {
				return o, true, nil
			}
		}
		return InstanceRow{}, false, nil
	}
```

`internal/relay/instance.go`: replace `reassertOwnership` and `RunInstance`:

```go
// reassertOwnership records this instance as an owner of every base its
// router holds. SetOwner is idempotent and silent when the row exists, so
// this is one cheap insert per base per beat, and a relay whose row was
// cascaded away — by an edge that found it undialable for one dial, say —
// gets every owner row back on the next beat instead of staying dark until
// each agent reconnects. It runs inside the beat, after the upsert, so our
// own row is live before the owner rows point at it.
func (i *Instance) reassertOwnership(st *Store, router *Router) {
	for _, base := range router.Bases() {
		if err := st.SetOwner(base, i.ID); err != nil {
			log.Printf("agent %s: re-record owner: %v", base, err)
		}
	}
}

// RunInstance keeps this relay in the pool and reacts to the cluster. It
// heartbeats until ctx is done, then leaves. Meanwhile it LISTENs for a
// piper_events payload naming an agent this process holds and drains its
// parked webhooks. On every listener (re)connect the same check runs over
// every held agent, so a NOTIFY missed while disconnected is caught up.
// Ownership needs no listener: two owners is the normal state (#530), so
// nothing here reacts to another instance taking a row. delivery may be nil
// (no GitHub App).
func RunInstance(ctx context.Context, st *Store, inst *Instance, router *Router, delivery *TunnelDelivery) {
	handle := func(channel, base string) {
		if _, ok := router.Holds(base); !ok || delivery == nil {
			return
		}
		delivery.Dispatch(func(ctx context.Context) { delivery.DrainFor(ctx, base) })
	}
	resync := func() {
		for _, base := range router.Bases() {
			handle(chanEvents, base)
		}
	}
	go listen(ctx, st.dsn, []string{chanEvents}, resync, handle)
	inst.heartbeat(ctx, st, router)
}
```

- [ ] **Step 6: Update the remaining tests that read a single owner**

`internal/relay/edge_state_test.go`: in `TestOwnerOfRequiresALiveInstanceAndEvictCascades` change the calls to `s.setOwners(map[string][]string{"x.example": {"a"}, "y.example": {"b"}})`, `s.setOwner("x.example", nil)`, `s.setOwner("x.example", []string{"a"})`, `s.setOwner("y.example", []string{"ghost"})`. In `TestPlacementSkipsDrainingInstances` change to `s.setOwners(map[string][]string{"x.example": {"a"}})`. Then add:

```go
// With two owners per agent (#530) the edge chooses: a non-draining owner
// over a draining one, then the one with fewer sessions, then the earliest.
func TestOwnersOfPrefersNonDrainingThenFewestThenEarliest(t *testing.T) {
	t0 := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	s := newEdgeState()
	draining := instRow("draining-idle", t0, 0)
	draining.Draining = true
	s.setInstances([]InstanceRow{
		draining,
		instRow("busy-early", t0.Add(time.Second), 9),
		instRow("quiet-late", t0.Add(time.Minute), 2),
		instRow("quiet-early", t0.Add(2*time.Second), 2),
	})
	s.setOwners(map[string][]string{"x.example": {"draining-idle", "busy-early", "quiet-late", "quiet-early", "ghost"}})
	got := s.ownersOf("x.example")
	var ids []string
	for _, r := range got {
		ids = append(ids, r.ID)
	}
	if want := "quiet-early,quiet-late,busy-early,draining-idle"; strings.Join(ids, ",") != want {
		t.Fatalf("ownersOf = %v, want %s (ghost is not a live instance)", ids, want)
	}
	if first, ok := s.ownerOf("x.example"); !ok || first.ID != "quiet-early" {
		t.Fatalf("ownerOf = %+v ok=%v, want quiet-early", first, ok)
	}
	s.evict("quiet-early")
	if first, ok := s.ownerOf("x.example"); !ok || first.ID != "quiet-late" {
		t.Fatalf("ownerOf after evict = %+v ok=%v, want quiet-late", first, ok)
	}
}
```

`internal/relay/instance_test.go`: in `TestHeartbeatReassertsOwnershipAfterTheInstanceRowIsDeleted` replace each `r, ok, _ := st.OwnerOf(en.BaseDomain); return ok && r.ID == inst.ID` predicate with `return strings.Join(ownerIDs(t, st, en.BaseDomain), ",") == inst.ID`, and the `if _, ok, _ := st.OwnerOf(...); ok` check with `if len(ownerIDs(t, st, en.BaseDomain)) != 0`. Replace `TestHeartbeatDoesNotStealOwnershipFromAnotherLiveInstance` with:

```go
// Two owners is the normal state (#530): another live relay's row for a base
// we hold is left alone, and ours is kept alongside it on every beat.
func TestHeartbeatKeepsBothOwnerRows(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	inst, err := NewInstance("127.0.0.1", ":443", ":80", ":7000", ":8080")
	if err != nil {
		t.Fatal(err)
	}
	router := NewRouter()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go inst.heartbeat(ctx, st, router)

	dialTestTunnel(t, st, router, inst, en)
	waitCond(t, 3*time.Second, "owner row written", func() bool {
		return strings.Join(ownerIDs(t, st, en.BaseDomain), ",") == inst.ID
	})
	other := testInstance(t, st)
	if err := st.SetOwner(en.BaseDomain, other.ID); err != nil {
		t.Fatal(err)
	}

	lastSeen := func() time.Time {
		var ts time.Time
		if err := st.db.QueryRow(`SELECT last_seen FROM relay_instances WHERE id=$1`, inst.ID).Scan(&ts); err != nil {
			t.Fatal(err)
		}
		return ts
	}
	// Three beats is proof the loop ran with the second owner in place.
	for i := 0; i < 3; i++ {
		was := lastSeen()
		waitCond(t, 3*time.Second, "heartbeat tick", func() bool { return lastSeen().After(was) })
	}
	got := ownerIDs(t, st, en.BaseDomain)
	if len(got) != 2 || !slices.Contains(got, inst.ID) || !slices.Contains(got, other.ID) {
		t.Fatalf("owners after three beats = %v, want both %s and %s", got, inst.ID, other.ID)
	}
}
```

Delete `TestRunInstanceClosesSessionOwnedElsewhere` entirely. Add `"slices"` and `"strings"` to the file's imports.

`internal/relay/server_test.go`: in `TestServeTunnelRecordsAndClearsOwner` and `TestServeTunnelNeverClearsAnotherRelaysOwnership`, replace the `OwnerOf` predicates the same way: `strings.Join(ownerIDs(t, st, en.BaseDomain), ",") == inst.ID` for "written", `len(ownerIDs(...)) == 0` for "cleared", and for the never-clears test's final check:

```go
	if got := ownerIDs(t, st, en.BaseDomain); strings.Join(got, ",") != other.ID {
		t.Fatalf("owners after stale unregister = %v, want [%s]", got, other.ID)
	}
```

Leave `TestServeTunnelKeepsOwnerWhenItStillHoldsANewerSession` compiling by changing its two `OwnerOf` checks to `strings.Join(ownerIDs(t, st, en.BaseDomain), ",") == inst.ID`; Task 3 rewrites it.

`internal/relay/edge_test.go`: at the end of `TestEdgeClusterControlHopWebhookDrainAndOwnershipMove` replace the store assertion with:

```go
	if got := ownerIDs(t, st, en1.BaseDomain); strings.Join(got, ",") != otherRelay.inst.ID {
		t.Fatalf("agent_owners after the move: %v, want [%s]", got, otherRelay.inst.ID)
	}
```

and in `TestEdgeEvictsDeadRelayOnDialFailureAndRetriesOnceOnTunnel` replace the final check with `if len(ownerIDs(t, st, en.BaseDomain)) != 0 { t.Fatal("ownership survived its instance's eviction") }`. Same in `TestEdgeNeverRetriesOnTLS` if it reads `OwnerOf` (it does not; leave it).

- [ ] **Step 7: Run the relay package**

Run: `go test ./internal/relay/ 2>&1 | tail -20`
Expected: PASS. If `TestEveryMutatorNotifiesItsChannel` fails on the second `SetOwner` expectation, it is because that test now needs a fresh row: it sets then clears, so both notify; it should pass unchanged.

- [ ] **Step 8: Verify and commit**

Run: `make verify`
Expected: exit 0.

```bash
git add internal/relay/schema.sql internal/relay/ownership.go internal/relay/edge_state.go internal/relay/edge.go internal/relay/instance.go internal/relay/proxy.go internal/relay/ownership_test.go internal/relay/edge_state_test.go internal/relay/instance_test.go internal/relay/server_test.go internal/relay/edge_test.go
git commit -m "feat(relay): ownership is every live row

agent_owners keyed on (agent, instance); SetOwner is an idempotent insert
that notifies once; OwnerOf and Owners return sets; the edge picks among
owners (non-draining, fewest sessions, earliest) and the heartbeat
re-records every held base each beat. The stale-session close on
piper_owners is gone: two owners is the normal state.

Part of #530.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 3: Relay rejects a duplicate session after auth

**Files:**
- Modify: `internal/relay/router.go` (`Register`)
- Modify: `internal/relay/server.go:170-210` (`serveTunnel` auth wrapper and `Register` call)
- Test: `internal/relay/router_test.go`, `internal/relay/server_test.go`

**Interfaces:**
- Produces: `func (r *Router) Register(sess *tunnel.Session) error` returning `tunnel.ErrDuplicateSession` when `sess.BaseDomain` is already held by a different session.
- Consumes: `tunnel.ErrDuplicateSession` (Task 1).

- [ ] **Step 1: Write the failing tests**

In `internal/relay/router_test.go`, replace `TestUnregisterKeepsSuccessorEntries` with:

```go
// A relay holds at most one session per agent (#530): a second Register for
// a held base is refused and the first session keeps every entry. Once the
// first is unregistered the base is free again.
func TestRegisterRefusesAHeldBase(t *testing.T) {
	r := NewRouter()
	base := "alice.example.com"
	first := &tunnel.Session{BaseDomain: base}
	second := &tunnel.Session{BaseDomain: base}

	if err := r.Register(first); err != nil {
		t.Fatal(err)
	}
	r.RegisterHost("blog-alice.public.getpiper.co", first)
	if err := r.Register(second); !errors.Is(err, tunnel.ErrDuplicateSession) {
		t.Fatalf("second Register = %v, want ErrDuplicateSession", err)
	}
	if s, ok := r.Lookup(base); !ok || s != first {
		t.Fatalf("Lookup after refused duplicate = %v (%p), want the first session %p", ok, s, first)
	}
	if err := r.Register(first); err != nil {
		t.Fatalf("re-registering the same session = %v, want nil (idempotent)", err)
	}
	r.Unregister(first)
	if err := r.Register(second); err != nil {
		t.Fatalf("Register after the holder left = %v, want nil", err)
	}
	if s, ok := r.Lookup(base); !ok || s != second {
		t.Fatal("freed base did not take the new session")
	}
}
```

Add `"errors"` to the file's imports. In `internal/relay/server_test.go`, replace `TestServeTunnelKeepsOwnerWhenItStillHoldsANewerSession` (and drop the `tunnelLogSpy` type if nothing else uses it) with:

```go
// The duplicate check runs after auth: a second dial for a base this relay
// holds is rejected with the named reason, and the first session keeps its
// registration and its owner row.
func TestServeTunnelRejectsADuplicateAfterAuth(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	inst := testInstance(t, st)
	router := NewRouter()

	first := dialTestTunnel(t, st, router, inst, en)
	held, _ := router.Holds(en.BaseDomain)
	waitCond(t, 3*time.Second, "owner row written", func() bool {
		return strings.Join(ownerIDs(t, st, en.BaseDomain), ",") == inst.ID
	})

	cc, sc := net.Pipe()
	t.Cleanup(func() { cc.Close(); sc.Close() })
	go serveTunnel(sc, st, router, st.AgentDisabled, nil, nil, inst)
	if _, err := tunnel.Dial(cc, en.Token, en.BaseDomain); !errors.Is(err, tunnel.ErrDuplicateSession) {
		t.Fatalf("second dial = %v, want ErrDuplicateSession", err)
	}
	if s, ok := router.Holds(en.BaseDomain); !ok || s != held {
		t.Fatal("first session lost its registration to the refused duplicate")
	}
	if got := ownerIDs(t, st, en.BaseDomain); strings.Join(got, ",") != inst.ID {
		t.Fatalf("owners after refused duplicate = %v, want [%s]", got, inst.ID)
	}
	first.Close()
}

// The check must not run before auth, or an unauthenticated peer could learn
// which agents a relay holds: a bad token for a held base gets the generic
// reason, not the duplicate one.
func TestServeTunnelDoesNotNameADuplicateToABadToken(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	inst := testInstance(t, st)
	router := NewRouter()
	dialTestTunnel(t, st, router, inst, en)

	cc, sc := net.Pipe()
	t.Cleanup(func() { cc.Close(); sc.Close() })
	go serveTunnel(sc, st, router, st.AgentDisabled, nil, nil, inst)
	_, err := tunnel.Dial(cc, "not-the-token", en.BaseDomain)
	if err == nil || errors.Is(err, tunnel.ErrDuplicateSession) {
		t.Fatalf("bad token on a held base = %v, want a generic rejection", err)
	}
}

// clearOwner's router guard: on one instance an unregister, a fresh register
// of the same agent, and the late clear can interleave, and both sessions
// map to the same (agent, instance) row. While the router still holds the
// base, the row is the newer session's and must stay.
func TestClearOwnerKeepsTheRowWhileTheRouterHoldsTheBase(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	inst := testInstance(t, st)
	router := NewRouter()
	if err := st.SetOwner(en.BaseDomain, inst.ID); err != nil {
		t.Fatal(err)
	}
	sess := &tunnel.Session{BaseDomain: en.BaseDomain}
	if err := router.Register(sess); err != nil {
		t.Fatal(err)
	}
	clearOwner(st, router, en.BaseDomain, inst)
	if got := ownerIDs(t, st, en.BaseDomain); strings.Join(got, ",") != inst.ID {
		t.Fatalf("owner row cleared while the base is still held: %v", got)
	}
	router.Unregister(sess)
	clearOwner(st, router, en.BaseDomain, inst)
	if got := ownerIDs(t, st, en.BaseDomain); len(got) != 0 {
		t.Fatalf("owner row survived a clear after the base was released: %v", got)
	}
}
```

Make sure `"errors"` is imported in `server_test.go`.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/relay/ -run 'TestRegisterRefusesAHeldBase|TestServeTunnelRejectsADuplicate|TestServeTunnelDoesNotName|TestClearOwnerKeeps' 2>&1 | head`
Expected: compile error (`Register` returns nothing), then failures.

- [ ] **Step 3: Implement**

`internal/relay/router.go`, replace `Register`:

```go
// Register maps sess's base domain to it. A base already held by a
// different session is refused with tunnel.ErrDuplicateSession: a relay
// holds one session per agent (#530), and the edge's placement is what
// spreads an agent's two sessions across relays. Re-registering the same
// session is a no-op. A closed session is refused silently (see below).
func (r *Router) Register(sess *tunnel.Session) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if sess.Closed() {
		return nil
	}
	if held, ok := r.byBase[sess.BaseDomain]; ok && held != sess {
		return tunnel.ErrDuplicateSession
	}
	r.byBase[sess.BaseDomain] = sess
	return nil
}
```

`internal/relay/server.go`, in `serveTunnel`: extend the auth wrapper so the duplicate check runs after the token check, and handle `Register`'s error:

```go
	var claimedBase string
	auth := func(token, base string) error {
		claimedBase = base
		if err := tunnelAuth(st)(token, base); err != nil {
			return err
		}
		// Post-auth only: the peer has proven which agent it is, so naming
		// the duplicate confirms nothing to a stranger. A duplicate landing
		// here at all means the edge had no relay left that does not hold
		// this agent, or two dials raced (#530).
		if _, held := router.Holds(base); held {
			return tunnel.ErrDuplicateSession
		}
		return nil
	}
	sess, err := tunnel.Serve(conn, auth)
	if err != nil {
		log.Printf("tunnel handshake rejected for %q from %s: %v", claimedBase, conn.RemoteAddr(), err)
		conn.Close()
		return
	}
	// Two handshakes for one base can both pass the check above; the router
	// is the tie-break.
	if err := router.Register(sess); err != nil {
		log.Printf("agent %s from %s: %v; closing", sess.BaseDomain, conn.RemoteAddr(), err)
		sess.Close()
		return
	}
```

- [ ] **Step 4: Run the relay package**

Run: `go test ./internal/relay/ 2>&1 | tail -20`
Expected: PASS. `TestAcceptTunnelsRebindsCustomDomainOnReconnect` reconnects after closing the first session and waits for the router to drop it, so it must still pass; if it dials the second session before the first is unregistered, add a `waitCond` on `router.Holds` returning false before the reconnect.

- [ ] **Step 5: Verify and commit**

Run: `make verify`
Expected: exit 0.

```bash
git add internal/relay/router.go internal/relay/server.go internal/relay/router_test.go internal/relay/server_test.go
git commit -m "feat(relay): reject a duplicate session for a held base after auth

Part of #530.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 4: Relays derive routes from Postgres and re-derive on NOTIFY

**Files:**
- Modify: `internal/relay/hostnames.go` (add `HostnamesFor`)
- Modify: `internal/relay/router.go` (add `SetHosts`, `SetCustom`)
- Modify: `internal/relay/server.go` (add `syncRoutes`; use it in `serveTunnel`)
- Modify: `internal/relay/instance.go` (`RunInstance` listens on `piper_hostnames`)
- Test: `internal/relay/hostnames_test.go`, `internal/relay/router_test.go`, `internal/relay/instance_test.go`, `internal/relay/server_test.go`

**Interfaces:**
- Produces: `func (s *Store) HostnamesFor(baseDomain string) ([]string, error)`; `func (r *Router) SetHosts(sess *tunnel.Session, hosts []string)`; `func (r *Router) SetCustom(sess *tunnel.Session, domains []string)`; `func syncRoutes(st *Store, router *Router, base string, sess *tunnel.Session)`.
- Consumes: `st.CustomDomains(base)` (existing), `router.Bases()`, `router.Holds(base)`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/relay/hostnames_test.go`:

```go
func TestHostnamesForListsTheAgentsTerminatedHostnames(t *testing.T) {
	st, base := newAccountAgent(t)
	if got, err := st.HostnamesFor(base); err != nil || len(got) != 0 {
		t.Fatalf("HostnamesFor before any register = %v (%v), want none", got, err)
	}
	h1, err := st.RegisterHostname(base, "blog", 0)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := st.RegisterHostname(base, "blog", 7)
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.HostnamesFor(base)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{h1, h2}
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("HostnamesFor = %v, want %v (sorted)", got, want)
	}
}
```

Add `"sort"` to that file's imports. Append to `internal/relay/router_test.go`:

```go
// SetHosts and SetCustom make one session's entries exactly the given set,
// so a relay can re-derive an agent's routes from Postgres without touching
// what other sessions hold.
func TestSetHostsAndSetCustomReconcileOneSession(t *testing.T) {
	r := NewRouter()
	alice := &tunnel.Session{BaseDomain: "alice.example.com"}
	bob := &tunnel.Session{BaseDomain: "bob.example.com"}
	if err := r.Register(alice); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(bob); err != nil {
		t.Fatal(err)
	}
	r.RegisterHost("old-alice.public.getpiper.co", alice)
	r.RegisterHost("blog-bob.public.getpiper.co", bob)
	r.RegisterCustom("old.alice.dev", alice)
	r.RegisterCustom("shop.bob.dev", bob)

	r.SetHosts(alice, []string{"blog-alice.public.getpiper.co", "pr-3-blog-alice.public.getpiper.co"})
	r.SetCustom(alice, []string{"shop.alice.dev"})

	for _, host := range []string{"blog-alice.public.getpiper.co", "pr-3-blog-alice.public.getpiper.co"} {
		if s, ok := r.LookupHost(host); !ok || s != alice {
			t.Errorf("%s not routed to alice after SetHosts", host)
		}
	}
	if _, ok := r.LookupHost("old-alice.public.getpiper.co"); ok {
		t.Error("SetHosts kept an entry that is no longer in the set")
	}
	if s, ok := r.LookupHost("blog-bob.public.getpiper.co"); !ok || s != bob {
		t.Error("SetHosts on alice touched bob's hostname")
	}
	if s, ok := r.LookupCustom("shop.alice.dev"); !ok || s != alice {
		t.Error("shop.alice.dev not routed after SetCustom")
	}
	if _, ok := r.Lookup("old.alice.dev"); ok {
		t.Error("SetCustom kept a custom domain that is no longer in the set")
	}
	if s, ok := r.LookupCustom("shop.bob.dev"); !ok || s != bob {
		t.Error("SetCustom on alice touched bob's custom domain")
	}
	if s, ok := r.Lookup("alice.example.com"); !ok || s != alice {
		t.Error("SetCustom removed the agent's own base entry")
	}
	if a, h, c := r.Counts(); a != 2 || h != 3 || c != 2 {
		t.Fatalf("Counts() = %d,%d,%d; want 2,3,2", a, h, c)
	}
}
```

Append to `internal/relay/server_test.go`:

```go
// A session that registers on a relay gets every hostname the agent already
// holds in Postgres, whichever relay's session created them (#530).
func TestServeTunnelDerivesHostnamesAtRegister(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	inst := testInstance(t, st)
	router := NewRouter()
	host, err := st.RegisterHostname(en.BaseDomain, "blog", 0)
	if err != nil {
		t.Fatal(err)
	}
	sess := dialTestTunnel(t, st, router, inst, en)
	defer sess.Close()
	// dialTestTunnel returns once the base is registered; the route derive
	// runs right after, so wait for it rather than racing it.
	waitCond(t, 3*time.Second, host+" derived at register", func() bool {
		_, ok := router.LookupHost(host)
		return ok
	})
}
```

Append to `internal/relay/instance_test.go`:

```go
// A hostname or custom domain written through any relay reaches every relay
// that holds the agent: piper_hostnames wakes a re-derive from Postgres.
func TestRunInstanceRederivesRoutesOnHostnameNotify(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	inst, err := NewInstance("127.0.0.1", ":443", ":80", ":7000", ":8080")
	if err != nil {
		t.Fatal(err)
	}
	router := NewRouter()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go RunInstance(ctx, st, inst, router, nil)
	sess := dialTestTunnel(t, st, router, inst, en)
	defer sess.Close()

	// Written "by another relay": straight into the store, no control op here.
	host, err := st.RegisterHostname(en.BaseDomain, "blog", 0)
	if err != nil {
		t.Fatal(err)
	}
	waitCond(t, 5*time.Second, "hostname derived after NOTIFY", func() bool {
		_, ok := router.LookupHost(host)
		return ok
	})
	if err := st.AddCustomDomain(en.BaseDomain, "shop.example.com"); err != nil {
		t.Fatal(err)
	}
	waitCond(t, 5*time.Second, "custom domain derived after NOTIFY", func() bool {
		_, ok := router.LookupCustom("shop.example.com")
		return ok
	})
	if err := st.DeregisterHostname(en.BaseDomain, host); err != nil {
		t.Fatal(err)
	}
	waitCond(t, 5*time.Second, "hostname dropped after NOTIFY", func() bool {
		_, ok := router.LookupHost(host)
		return !ok
	})
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/relay/ -run 'TestHostnamesFor|TestSetHostsAndSetCustom|TestServeTunnelDerivesHostnames|TestRunInstanceRederivesRoutes' 2>&1 | head`
Expected: compile errors for `HostnamesFor`, `SetHosts`, `SetCustom`.

- [ ] **Step 3: Implement**

`internal/relay/hostnames.go`, after `DeregisterHostname`:

```go
// HostnamesFor lists the relay-terminated hostnames the agent holds, sorted.
// A relay derives its byHost routes from this at register and on every
// piper_hostnames NOTIFY (#530), so a hostname registered over the agent's
// other session routes here too.
func (s *Store) HostnamesFor(baseDomain string) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT h.hostname FROM hostnames h JOIN agents a ON a.name = h.agent_name
		  WHERE a.base_domain=$1 ORDER BY h.hostname`, baseDomain)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
```

`internal/relay/router.go`, after `UnregisterCustom`:

```go
// SetHosts makes sess's terminated-hostname entries exactly hosts: entries
// it holds outside the set are dropped, missing ones are added, and other
// sessions' entries are left alone. A closed session only loses entries.
func (r *Router) SetHosts(sess *tunnel.Session, hosts []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	want := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		want[h] = true
	}
	for host, s := range r.byHost {
		if s == sess && !want[host] {
			delete(r.byHost, host)
		}
	}
	if sess.Closed() {
		return
	}
	for _, h := range hosts {
		r.byHost[h] = sess
	}
}

// SetCustom is SetHosts for BYO custom domains, keeping byBase and custom in
// step the way RegisterCustom/UnregisterCustom do.
func (r *Router) SetCustom(sess *tunnel.Session, domains []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	want := make(map[string]bool, len(domains))
	for _, d := range domains {
		want[d] = true
	}
	for domain, s := range r.custom {
		if s == sess && !want[domain] {
			delete(r.custom, domain)
			delete(r.byBase, domain)
		}
	}
	if sess.Closed() {
		return
	}
	for _, d := range domains {
		r.byBase[d] = sess
		r.custom[d] = sess
	}
}
```

`internal/relay/server.go`: in `serveTunnel`, replace the custom-domain re-derive block (the comment beginning "Re-derive every live custom domain" through the closing brace of its `for`) with `syncRoutes(st, router, sess.BaseDomain, sess)`, and add after `clearOwner`:

```go
// syncRoutes makes sess's hostname and custom-domain entries exactly what
// Postgres says the agent holds. Called at register and on every
// piper_hostnames NOTIFY (#530): with two sessions on two relays, a control
// op over one session must route on the other relay too, and the store is
// where both already write. Expired pending custom domains are filtered by
// the store, so a squat dies here even if never contested (#227). A read
// failure leaves the current entries in place; the next NOTIFY or beat
// retries.
func syncRoutes(st *Store, router *Router, base string, sess *tunnel.Session) {
	hosts, err := st.HostnamesFor(base)
	if err != nil {
		log.Printf("agent %s: derive hostnames: %v", base, err)
	} else {
		router.SetHosts(sess, hosts)
	}
	domains, err := st.CustomDomains(base)
	if err != nil {
		log.Printf("agent %s: derive custom domains: %v", base, err)
	} else {
		router.SetCustom(sess, domains)
	}
}
```

`internal/relay/instance.go`, replace `RunInstance`:

```go
// RunInstance keeps this relay in the pool and reacts to the cluster. It
// heartbeats until ctx is done, then leaves. Meanwhile it LISTENs on two
// channels: a piper_events payload naming an agent this process holds
// drains its parked webhooks, and any piper_hostnames payload re-derives
// the routes of every held agent from Postgres — the payload is a hostname,
// not a base, and the held set is small (#530). On every listener
// (re)connect both run over every held agent, so a NOTIFY missed while
// disconnected is caught up. Ownership needs no listener: two owners is the
// normal state, so nothing here reacts to another instance taking a row.
// delivery may be nil (no GitHub App).
func RunInstance(ctx context.Context, st *Store, inst *Instance, router *Router, delivery *TunnelDelivery) {
	handle := func(channel, payload string) {
		switch channel {
		case chanEvents:
			if _, ok := router.Holds(payload); !ok || delivery == nil {
				return
			}
			delivery.Dispatch(func(ctx context.Context) { delivery.DrainFor(ctx, payload) })
		case chanHostnames:
			for _, base := range router.Bases() {
				if sess, ok := router.Holds(base); ok {
					syncRoutes(st, router, base, sess)
				}
			}
		}
	}
	resync := func() {
		for _, base := range router.Bases() {
			handle(chanEvents, base)
		}
		handle(chanHostnames, "")
	}
	go listen(ctx, st.dsn, []string{chanEvents, chanHostnames}, resync, handle)
	inst.heartbeat(ctx, st, router)
}
```

- [ ] **Step 4: Run the relay package**

Run: `go test ./internal/relay/ 2>&1 | tail -20`
Expected: PASS, including `TestAcceptTunnelsRebindsCustomDomainOnReconnect` (custom domains are now derived by `syncRoutes`).

- [ ] **Step 5: Verify and commit**

Run: `make verify`
Expected: exit 0.

```bash
git add internal/relay/hostnames.go internal/relay/router.go internal/relay/server.go internal/relay/instance.go internal/relay/hostnames_test.go internal/relay/router_test.go internal/relay/server_test.go internal/relay/instance_test.go
git commit -m "feat(relay): derive hostname and custom-domain routes from Postgres

At register and on every piper_hostnames NOTIFY, so a control op over one
of an agent's sessions routes on the relay holding the other.

Part of #530.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 5: Edge peeks the preface, excludes owners, retries across owners

**Files:**
- Modify: `internal/relay/edge_state.go` (`pickTunnel`, `pickLocked`)
- Modify: `internal/relay/edge.go` (`readPreface`, `handleTunnel`, `handleTLS`, `handleHTTP`, new `forwardToOwners`)
- Test: `internal/relay/edge_state_test.go`, `internal/relay/edge_test.go`

**Interfaces:**
- Produces: `func (s *edgeState) pickTunnel(base string, exclude map[string]bool) (InstanceRow, bool)`; `func readPreface(conn net.Conn) (base string, raw []byte, err error)`.
- Consumes: `tunnel.ReadPreface` (Task 1); `ownersOf` (Task 2); test helpers `startRelayBehindEdge`, `startEdge`, `dialAgentThroughEdge`, `fakeAgent`, `waitEdgeOwner`, `waitEdgeSessions`, `sendClientHello`, `expectString`, `controlOp`, `acceptOne`, `awaitAccept`, `freeTCPAddr`, `stampInstance`, `ownerIDs` (all existing in the package's tests).

- [ ] **Step 1: Write the failing edge-state tests**

In `internal/relay/edge_state_test.go`, change every `s.pickTunnel(x)` call to `s.pickTunnel("", x)` (there are calls in `TestPickTunnelFewestSessionsThenEarliest`, `TestOwnerOfRequiresALiveInstanceAndEvictCascades`, and `TestPlacementSkipsDrainingInstances`). Then add:

```go
// :7000 placement for an agent leaves out the relays that already hold it,
// so its second session lands elsewhere. The exclusion is soft: when every
// candidate already holds the agent, the pick falls back to the full pool
// and the relay rejects the duplicate after auth — the same observable
// outcome for every claimed base, so nothing is learnable from a refusal.
func TestPickTunnelExcludesOwnersThenFallsBack(t *testing.T) {
	t0 := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	s := newEdgeState()
	s.setInstances([]InstanceRow{instRow("a", t0, 1), instRow("b", t0.Add(time.Minute), 3)})
	s.setOwners(map[string][]string{"x.example": {"a"}})

	if got, ok := s.pickTunnel("y.example", nil); !ok || got.ID != "a" {
		t.Fatalf("unowned agent: pickTunnel = %+v ok=%v, want a (fewest sessions)", got, ok)
	}
	if got, ok := s.pickTunnel("x.example", nil); !ok || got.ID != "b" {
		t.Fatalf("agent owned by a: pickTunnel = %+v ok=%v, want b", got, ok)
	}
	s.setOwner("x.example", []string{"a", "b"})
	if got, ok := s.pickTunnel("x.example", nil); !ok || got.ID != "a" {
		t.Fatalf("agent owned by all: pickTunnel = %+v ok=%v, want the fallback pick a", got, ok)
	}
	if got, ok := s.pickTunnel("x.example", map[string]bool{"a": true}); !ok || got.ID != "b" {
		t.Fatalf("fallback still honours a failed dial: pickTunnel = %+v ok=%v, want b", got, ok)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/relay/ -run 'TestPickTunnel' 2>&1 | head`
Expected: compile error (`pickTunnel` takes one argument).

- [ ] **Step 3: Implement placement by base**

In `internal/relay/edge_state.go`, replace `pickTunnel` and `pickLocked`:

```go
// pickTunnel is :7000 placement for the agent named base: fewest sessions,
// ties to the earliest started, among relays that do not already own base
// (#530). exclude names instances a failed dial has just ruled out. The
// owner exclusion is soft: if it empties the pool the pick runs again over
// every relay and the one dialled rejects the duplicate after auth, so a
// claimed base never changes what an unauthenticated peer can observe.
func (s *edgeState) pickTunnel(base string, exclude map[string]bool) (InstanceRow, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	owned := map[string]bool{}
	for _, id := range s.owners[base] {
		owned[id] = true
	}
	less := func(a, b InstanceRow) bool {
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

// pickLocked is the shared placement scan. A draining instance is never a
// candidate (#523): it is on its way out and refuses new tunnels anyway.
// ownersOf does not go through here on purpose — a draining relay still
// holds its agents' sessions and must keep receiving their traffic.
func (s *edgeState) pickLocked(less func(a, b InstanceRow) bool, skip func(InstanceRow) bool) (InstanceRow, bool) {
	var best InstanceRow
	found := false
	for _, r := range s.instances {
		if skip(r) || r.Draining {
			continue
		}
		if !found || less(r, best) {
			best, found = r, true
		}
	}
	return best, found
}
```

Run: `go test ./internal/relay/ -run 'TestPickTunnel|TestPlacement|TestOwnerOf' `
Expected: PASS.

- [ ] **Step 4: Write the failing edge tests**

In `internal/relay/edge_test.go`:

Replace `TestEdgeNeverRetriesOnTLS` with:

```go
// TestEdgeRetriesOnceOnTLSAcrossOwners: with two owners per agent (#530)
// :443 has a second candidate. A dead owner costs one retry, and the eviction
// that pays for it takes the dead relay's rows with it; the connection that
// discovers the dead relay is still served.
func TestEdgeRetriesOnceOnTLSAcrossOwners(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	deadAddr := freeTCPAddr(t)
	stampInstance(t, st, "dead", deadAddr, time.Now().Add(-time.Minute))
	liveLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { liveLn.Close() })
	stampInstance(t, st, "live", liveLn.Addr().String(), time.Now())
	for _, id := range []string{"dead", "live"} {
		if err := st.SetOwner(en.BaseDomain, id); err != nil {
			t.Fatal(err)
		}
	}
	cfg, e := startEdge(t, st)

	// Equal load, so the earliest (dead) owner is tried first; the refused
	// dial evicts it and the single retry lands on live.
	accepted := acceptOne(liveLn)
	sendClientHello(t, cfg.TLSAddr, "app."+en.BaseDomain)
	awaitAccept(t, accepted).Close()
	waitCond(t, 5*time.Second, "dead row deleted", func() bool {
		rows, _ := st.LiveInstances()
		return len(rows) == 1 && rows[0].ID == "live"
	})
	if got := ownerIDs(t, st, en.BaseDomain); strings.Join(got, ",") != "live" {
		t.Fatalf("owners after eviction = %v, want [live]", got)
	}
	// The retry succeeded, so the connection counts as routed, not unrouted.
	waitForScrape(t, e.m, `piper_edge_conns_routed_total{listener="tls"} 1`)
}
```

In `TestEdgeEvictsDeadRelayOnDialFailureAndRetriesOnceOnTunnel`, the edge now waits for a preface before placing, so replace the bare `net.Dial` block with a real handshake attempt:

```go
	accepted := acceptOne(liveLn)
	conn, err := net.Dial("tcp", cfg.TunnelAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	go tunnel.Dial(conn, "tok", en.BaseDomain) // writes the preface; the ack never comes
	awaitAccept(t, accepted).Close()
```

In `TestEdgeCountsDroppedHeads`, add `cfg.TunnelAddr` to the dialled addresses and `"tunnel"` to the listeners checked (a 2-byte header of `\x00\x01` followed by one byte is a frame that is not JSON, so the preface read fails and the connection counts as dropped).

Then add the cluster test:

```go
// TestEdgeGivesAnAgentTwoRelays is #530's contract end to end: an agent's
// two dials land on two different relays, both route its traffic, a
// hostname registered over one session routes through both, a relay dying
// leaves the agent served through the other — including the connection
// that discovers the death — and the second session re-places once a relay
// is back.
func TestEdgeGivesAnAgentTwoRelays(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	acc, err := st.UpsertAccount("sub-1", "alice")
	if err != nil {
		t.Fatal(err)
	}
	en, err := st.EnrollForAccount(acc.ID, "box-1")
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
	cfg, e := startEdge(t, st)

	// Slot 1, then slot 2 once the edge has seen slot 1's owner row.
	s1, _ := dialAgentThroughEdge(t, cfg, en)
	first := waitEdgeOwner(t, e, en.BaseDomain)
	waitEdgeSessions(t, e, first.ID, 1)
	s2, _ := dialAgentThroughEdge(t, cfg, en)
	waitCond(t, 5*time.Second, "two owners", func() bool { return len(e.state.ownersOf(en.BaseDomain)) == 2 })
	if got := ownerIDs(t, st, en.BaseDomain); len(got) != 2 || got[0] == got[1] {
		t.Fatalf("owners = %v, want two distinct relays", got)
	}
	relayOf := func(id string) *edgeRelay {
		if id == rA.inst.ID {
			return rA
		}
		return rB
	}
	firstRelay := relayOf(first.ID)
	var secondRelay *edgeRelay
	for _, id := range ownerIDs(t, st, en.BaseDomain) {
		if id != first.ID {
			secondRelay = relayOf(id)
		}
	}

	// Both sessions answer passthrough; the edge may use either owner.
	snis := make(chan string, 8)
	go fakeAgent(s1, snis, make(chan string, 4))
	go fakeAgent(s2, snis, make(chan string, 4))
	sendClientHello(t, cfg.TLSAddr, "app."+en.BaseDomain)
	expectString(t, snis, "app."+en.BaseDomain, "passthrough with two owners")

	// A hostname registered over slot 1's session routes on both relays.
	host := controlOp(t, s1, tunnel.ControlRequest{Op: "register", App: "blog"}, false).Hostname
	for _, r := range []*edgeRelay{rA, rB} {
		waitCond(t, 5*time.Second, "relay "+r.inst.ID+" routes "+host, func() bool {
			_, ok := r.router.LookupHost(host)
			return ok
		})
	}

	// The relay holding slot 1 leaves the pool. Whether the edge has already
	// seen its row go or discovers the dead dial itself (the retry path
	// TestEdgeRetriesOnceOnTLSAcrossOwners pins), the next connection is
	// served through the survivor.
	firstRelay.stop()
	sendClientHello(t, cfg.TLSAddr, "app."+en.BaseDomain)
	expectString(t, snis, "app."+en.BaseDomain, "passthrough after one owner died")
	waitCond(t, 5*time.Second, "edge drops the dead relay", func() bool {
		owners := e.state.ownersOf(en.BaseDomain)
		return len(owners) == 1 && owners[0].ID == secondRelay.inst.ID
	})

	// A replacement joins; the lost slot redials and lands on it, not on the
	// survivor that already holds the agent.
	rC := startRelayBehindEdge(t, st, tlsCfg)
	s1.Close()
	waitCond(t, 5*time.Second, "edge sees the replacement", func() bool {
		e.state.mu.RLock()
		defer e.state.mu.RUnlock()
		_, ok := e.state.instances[rC.inst.ID]
		return ok
	})
	dialAgentThroughEdge(t, cfg, en)
	waitCond(t, 5*time.Second, "second session re-placed on the replacement", func() bool {
		got := ownerIDs(t, st, en.BaseDomain)
		return len(got) == 2 && slices.Contains(got, secondRelay.inst.ID) && slices.Contains(got, rC.inst.ID)
	})
}
```

Add `"slices"` to the file's imports.

- [ ] **Step 5: Run to verify they fail**

Run: `go test ./internal/relay/ -run 'TestEdgeRetriesOnceOnTLS|TestEdgeGivesAnAgentTwoRelays|TestEdgeCountsDroppedHeads|TestEdgeEvictsDeadRelay' 2>&1 | tail -20`
Expected: `TestEdgeRetriesOnceOnTLSAcrossOwners` fails (no retry on :443); `TestEdgeGivesAnAgentTwoRelays` fails at "two owners" (the second dial lands on the same relay and is rejected); `TestEdgeCountsDroppedHeads` fails on the tunnel listener.

- [ ] **Step 6: Implement the edge side**

In `internal/relay/edge.go`, add `"github.com/piperbox/piper/internal/tunnel"` to the imports, and:

```go
// readPreface peeks the tunnel handshake's routing preface — the sibling of
// readSNI and readHost — under the same unauthenticated-read deadline. The
// bytes returned are exactly the preface frame, replayed to the relay
// verbatim; the credential frame behind it is never read here (#530).
func readPreface(conn net.Conn) (string, []byte, error) {
	_ = conn.SetReadDeadline(time.Now().Add(preAuthReadTimeout))
	defer conn.SetReadDeadline(time.Time{})
	return tunnel.ReadPreface(conn)
}
```

Replace `handleTLS`, `handleHTTP`, and `handleTunnel`:

```go
func (e *edge) handleTLS(conn net.Conn) {
	defer conn.Close()
	sni, buffered, err := readSNI(conn)
	if err != nil {
		e.m.ConnDropped("tls")
		return
	}
	if sni == e.apiHost {
		target, ok := e.state.pickAPI()
		if !ok || e.forward("tls", conn, buffered, target, target.TLSAddr) != nil {
			e.m.ConnUnrouted("tls")
		}
		return
	}
	agent, found := e.resolveAgent(sni, false)
	if !found {
		e.m.ConnUnrouted("tls")
		return
	}
	e.forwardToOwners("tls", conn, buffered, agent, func(r InstanceRow) string { return r.TLSAddr })
}

func (e *edge) handleHTTP(conn net.Conn) {
	defer conn.Close()
	host, buffered, err := readHost(conn)
	if err != nil {
		e.m.ConnDropped("http")
		return
	}
	agent, found := e.resolveAgent(host, true)
	if !found {
		e.m.ConnUnrouted("http")
		return
	}
	e.forwardToOwners("http", conn, buffered, agent, func(r InstanceRow) string { return r.HTTPAddr })
}

// forwardToOwners splices conn to agent's preferred owner, and on a failed
// dial evicts that relay and retries exactly once on the next owner: with
// two sessions per agent (#530) a dead owner is discovered by the first
// connection that tries it, and that connection is still served. If the
// agent has no live owner, or both dials fail, the connection went nowhere
// and counts as unrouted.
func (e *edge) forwardToOwners(listener string, conn net.Conn, buffered []byte, agent string, addrOf func(InstanceRow) string) {
	owners := e.state.ownersOf(agent)
	if len(owners) > 2 {
		owners = owners[:2]
	}
	for _, target := range owners {
		if err := e.forward(listener, conn, buffered, target, addrOf(target)); !errors.Is(err, errBackendDial) {
			return
		}
	}
	e.m.ConnUnrouted(listener)
}

// handleTunnel places an agent's dial on the least-loaded relay that does
// not already hold that agent, which is how its two sessions end up on two
// relays (#530). A dial failure evicts that relay and retries the next
// candidate exactly once; the pool is small and a second failure means
// something bigger is wrong. A connection that sends no usable preface is
// dropped, like a bad ClientHello on :443.
func (e *edge) handleTunnel(conn net.Conn) {
	defer conn.Close()
	base, preface, err := readPreface(conn)
	if err != nil {
		e.m.ConnDropped("tunnel")
		return
	}
	exclude := map[string]bool{}
	for attempt := 0; attempt < 2; attempt++ {
		target, ok := e.state.pickTunnel(base, exclude)
		if !ok {
			break
		}
		if err := e.forward("tunnel", conn, preface, target, target.TunnelAddr); !errors.Is(err, errBackendDial) {
			return
		}
		exclude[target.ID] = true
	}
	e.m.ConnUnrouted("tunnel")
}
```

- [ ] **Step 7: Run the relay package**

Run: `go test ./internal/relay/ 2>&1 | tail -20`
Expected: PASS. `TestEdgePlacesAndRoutesAcrossTwoRelays` still passes: its bogus dial writes a preface for an unowned base and is placed, then rejected by the relay with the PROXY-carried address in the log.

- [ ] **Step 8: Verify and commit**

Run: `make verify`
Expected: exit 0.

```bash
git add internal/relay/edge.go internal/relay/edge_state.go internal/relay/edge_state_test.go internal/relay/edge_test.go
git commit -m "feat(edge): place by preface, exclude current owners, retry once across owners

:7000 peeks the routing preface and places away from relays that already
own the agent (soft: falls back to the full pool). :443/:80 pick among an
agent's live owners and retry once on a dead one.

Part of #530.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 6: Agent holds two sessions

**Files:**
- Modify: `internal/agent/tunnelclient.go`
- Test: `internal/agent/tunnelclient_test.go`

**Interfaces:**
- Produces: `TunnelClient` with `Run`, `Status`, `Register`, and every control op unchanged in signature. Package constant `tunnelSessions = 2`; package var `duplicateBackoff = time.Minute` (tests shrink it).
- Consumes: `tunnel.ErrDuplicateSession` (Task 1).

- [ ] **Step 1: Make the test fake relay accept every connection**

In `internal/agent/tunnelclient_test.go`, replace `fakeRelay` so a second slot has somewhere to land:

```go
// fakeRelay accepts every agent tunnel and hands each session to the test
// (buffered: the client opens two slots, tests usually drive the first).
func fakeRelay(t *testing.T) (addr string, sessCh chan *tunnel.Session) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	sessCh = make(chan *tunnel.Session, 8)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				sess, err := tunnel.Serve(c, func(_, _ string) error { return nil })
				if err != nil {
					c.Close()
					return
				}
				sessCh <- sess
			}()
		}
	}()
	return ln.Addr().String(), sessCh
}
```

Run: `go test ./internal/agent/`
Expected: PASS (the client still opens one session; the loop accept changes nothing yet).

- [ ] **Step 2: Write the failing tests**

Append to `internal/agent/tunnelclient_test.go`:

```go
// syncBuf is a race-safe log sink for counting log lines.
type syncBuf struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureLog(t *testing.T) *syncBuf {
	t.Helper()
	var b syncBuf
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&b)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })
	return &b
}

// The client holds two sessions (#530), both live at once.
func TestTunnelClientHoldsTwoSessions(t *testing.T) {
	addr, sessCh := fakeRelay(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var c TunnelClient
	go c.Run(ctx, addr, "tok", "alice.example.com", func(byte, net.Conn) (net.Conn, error) {
		return nil, errors.New("no local dials expected")
	})
	var got []*tunnel.Session
	for len(got) < 2 {
		select {
		case s := <-sessCh:
			got = append(got, s)
		case <-time.After(5 * time.Second):
			t.Fatalf("relay saw %d sessions, want 2", len(got))
		}
	}
	if got[0] == got[1] {
		t.Fatal("the two sessions are the same object")
	}
	c.mu.Lock()
	live := 0
	for _, s := range c.sess {
		if s != nil {
			live++
		}
	}
	c.mu.Unlock()
	if live != 2 {
		t.Fatalf("client reports %d live slots, want 2", live)
	}
	// One session dying leaves the other as the session control ops use.
	got[0].Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		live = 0
		for _, s := range c.sess {
			if s != nil {
				live++
			}
		}
		c.mu.Unlock()
		if live == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("client reports %d live slots after one session closed, want 1", live)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if c.current() == nil {
		t.Fatal("current() is nil with one slot still live")
	}
	if state, _ := c.Status(); state != "connected" {
		t.Fatalf("Status with one live slot = %q, want connected", state)
	}
}

// Slot 2 dials only after slot 1 has connected once: both boot dials
// reaching the edge before the first owner row exists would land on the
// same relay and the second would be rejected on every start.
func TestSecondSlotWaitsForTheFirstConnect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var accepted atomic.Int32
	release := make(chan struct{})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			go func() {
				<-release // hold every handshake until the test releases
				tunnel.Serve(c, func(_, _ string) error { return nil })
			}()
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var c TunnelClient
	go c.Run(ctx, ln.Addr().String(), "tok", "alice.example.com", nil)

	time.Sleep(300 * time.Millisecond)
	if n := accepted.Load(); n != 1 {
		t.Fatalf("%d connections before the first handshake completed, want 1", n)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for accepted.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("second slot never dialled after the first connected")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// dupRelay serves the first session and rejects every later handshake as a
// duplicate: the shape of a pool with one relay.
func dupRelay(t *testing.T) (addr string, rejected *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	rejected = new(atomic.Int32)
	var served atomic.Int32
	var keep []*tunnel.Session
	var mu sync.Mutex
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				if served.Add(1) == 1 {
					sess, err := tunnel.Serve(c, func(_, _ string) error { return nil })
					if err == nil {
						mu.Lock()
						keep = append(keep, sess)
						mu.Unlock()
					}
					return
				}
				_, _ = tunnel.Serve(c, func(_, _ string) error {
					rejected.Add(1)
					return tunnel.ErrDuplicateSession
				})
				c.Close()
			}()
		}
	}()
	return ln.Addr().String(), rejected
}

// A duplicate rejection is not a fault to back off from on the 1s→30s
// ladder: the slot waits duplicateBackoff and says so once, not per attempt.
func TestDuplicateRejectionBacksOffToTheCapAndLogsOnce(t *testing.T) {
	prev := duplicateBackoff
	duplicateBackoff = 30 * time.Millisecond
	t.Cleanup(func() { duplicateBackoff = prev })
	logged := captureLog(t)
	addr, rejected := dupRelay(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var c TunnelClient
	go c.Run(ctx, addr, "tok", "alice.example.com", nil)

	deadline := time.Now().Add(5 * time.Second)
	for rejected.Load() < 4 {
		if time.Now().After(deadline) {
			t.Fatalf("only %d duplicate rejections in 5s; the slot is on the slow ladder, not duplicateBackoff", rejected.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := strings.Count(logged.String(), "already holds"); n != 1 {
		t.Fatalf("duplicate logged %d times across %d rejections, want once:\n%s", n, rejected.Load(), logged.String())
	}
	if state, _ := c.Status(); state != "connected" {
		t.Fatalf("Status with one live slot = %q, want connected", state)
	}
}
```

Add `"log"` to the test file's imports (`errors`, `strings`, `sync`, `sync/atomic` are present).

- [ ] **Step 3: Run to verify they fail**

Run: `go test ./internal/agent/ -run 'TestTunnelClientHoldsTwoSessions|TestSecondSlotWaits|TestDuplicateRejection' 2>&1 | head`
Expected: compile errors (`c.sess` is not indexable, `duplicateBackoff` undefined).

- [ ] **Step 4: Implement the two slots**

In `internal/agent/tunnelclient.go`, change the struct and its helpers:

```go
// tunnelSessions is how many relay sessions a box holds, on distinct relays
// when the pool allows (#530). Two is what makes a relay's rolling restart
// zero-drop: the edge routes to the surviving owner while the lost slot
// redials. Not a knob: a supported deployment has at least two relays.
const tunnelSessions = 2

// duplicateBackoff is a slot's retry interval after the relay reported that
// every relay the edge could offer already holds this agent — a one-relay
// pool, or a half-open session the relay has not yet noticed. It is the time
// to regain redundancy once a relay comes back, so it is short; the dial is
// cheap and the log line is once per state, so there is nothing to be quiet
// about. A var so tests can shrink it.
var duplicateBackoff = time.Minute

// TunnelClient maintains outbound tunnels to the relay and exposes hostname
// registration over them. The live sessions are published under a mutex, one
// per slot, so the deploy path can open control streams on whichever is up.
type TunnelClient struct {
	mu         sync.Mutex
	sess       [tunnelSessions]*tunnel.Session
	running    bool
	lastErr    string
	observedIP string // last relay-reported source host; sticky across reconnects

	// OnConnect, if set before Run, is invoked in its own goroutine each time a
	// relay session is established — piperd uses it to provision the relay's
	// control bearer (see the control-stream routing design). With two slots
	// it fires per slot; everything it does is idempotent.
	OnConnect func()
}

// Status reports the tunnel's state for the enrollment socket's status
// surface: "connected" with at least one live session, "retrying" while Run
// is looping without one (lastErr is the most recent dial/handshake failure
// from any slot — the only place a rejected enrollment token becomes visible
// outside the log), "off" when Run is not running.
func (c *TunnelClient) Status() (state, lastErr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.sess {
		if s != nil {
			return "connected", ""
		}
	}
	if c.running {
		return "retrying", c.lastErr
	}
	return "off", c.lastErr
}

func (c *TunnelClient) setSession(slot int, s *tunnel.Session) {
	c.mu.Lock()
	c.sess[slot] = s
	if s != nil {
		c.lastErr = ""
	}
	c.mu.Unlock()
}

// current returns any live session, lowest slot first.
func (c *TunnelClient) current() *tunnel.Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.sess {
		if s != nil {
			return s
		}
	}
	return nil
}
```

Replace `Run` with a per-slot loop:

```go
// Run maintains tunnelSessions tunnels to relayAddr, registering baseDomain
// on each, and forwards each relay-opened stream to dialLocal(kind, stream).
// dialLocal may peek (read) bytes from stream before choosing a backend; it
// must replay whatever it consumed into the returned conn. Each slot
// reconnects with backoff until ctx is cancelled. Slot 2 sends its first
// dial only after slot 1 has connected once: both boot dials reaching the
// edge before the first owner row exists would land on the same relay and
// the second would be rejected on every start. Blocks until every slot has
// returned.
func (c *TunnelClient) Run(ctx context.Context, relayAddr, token, baseDomain string, dialLocal func(kind byte, stream net.Conn) (net.Conn, error)) {
	c.mu.Lock()
	c.running = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.running = false
		c.mu.Unlock()
	}()
	first := make(chan struct{})
	var firstOnce sync.Once
	connected := func(slot int) {
		if slot == 0 {
			firstOnce.Do(func() { close(first) })
		}
	}
	var wg sync.WaitGroup
	for slot := 0; slot < tunnelSessions; slot++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			if slot > 0 {
				select {
				case <-first:
				case <-ctx.Done():
					return
				}
			}
			c.runSlot(ctx, slot, relayAddr, token, baseDomain, dialLocal, connected)
		}(slot)
	}
	wg.Wait()
}

// runSlot is one slot's dial loop: today's single loop, with the duplicate
// case on top. On tunnel.ErrDuplicateSession the slot logs once — on the
// transition into that state — and waits duplicateBackoff instead of
// climbing the ladder; a successful connect resets the gate.
func (c *TunnelClient) runSlot(ctx context.Context, slot int, relayAddr, token, baseDomain string, dialLocal func(kind byte, stream net.Conn) (net.Conn, error), connected func(int)) {
	backoff := time.Second
	dupLogged := false
	for ctx.Err() == nil {
		conn, err := (&net.Dialer{Timeout: relayDialTimeout}).DialContext(ctx, "tcp", relayAddr)
		if err != nil {
			if ctx.Err() != nil {
				return // shutdown interrupted the dial; not a relay problem
			}
			c.setErr(err)
			log.Printf("tunnel: slot %d: dial relay: %v (retry in %s)", slot, err, backoff)
			sleep(ctx, backoff)
			backoff = nextBackoff(backoff)
			continue
		}
		// tunnel.Dial takes a bare net.Conn and answers only to deadlines, so a
		// relay that is slow but not dead can hold it for the write bound plus
		// a fresh ack bound — piperd's whole 20s shutdown budget. Closing conn
		// on cancel unblocks whichever half is in flight (#481). stop() runs
		// the instant Dial returns: an armed AfterFunc would later close conn
		// out from under yamux through a side door instead of the normal
		// serveStreams teardown.
		stop := context.AfterFunc(ctx, func() { conn.Close() })
		sess, err := tunnel.Dial(conn, token, baseDomain)
		stop()
		if err != nil {
			conn.Close()
			if ctx.Err() != nil {
				return // shutdown interrupted the handshake; not a relay problem
			}
			if errors.Is(err, tunnel.ErrDuplicateSession) {
				if !dupLogged {
					log.Printf("tunnel: slot %d: every relay the edge can offer already holds %s (one-relay pool, or a session it has not yet noticed is gone); retrying every %s", slot, baseDomain, duplicateBackoff)
					dupLogged = true
				}
				sleep(ctx, duplicateBackoff)
				continue
			}
			c.setErr(err)
			log.Printf("tunnel: slot %d: handshake: %v (retry in %s)", slot, err, backoff)
			sleep(ctx, backoff)
			backoff = nextBackoff(backoff)
			continue
		}
		log.Printf("tunnel: slot %d: connected to relay %s as %s", slot, relayAddr, baseDomain)
		dupLogged = false
		c.setSession(slot, sess)
		c.setObservedIP(sess.ObservedAddr)
		connected(slot)
		if c.OnConnect != nil {
			go c.OnConnect()
		}
		start := time.Now()
		serveStreams(ctx, sess, dialLocal)
		c.setSession(slot, nil)
		if time.Since(start) > healthyThreshold {
			backoff = time.Second
		}
		sleep(ctx, backoff)
		backoff = nextBackoff(backoff)
	}
}
```

Delete the old single-slot `Run` body entirely (the new `Run` replaces it; `setSession` now takes a slot).

- [ ] **Step 5: Run the agent package**

Run: `go test ./internal/agent/ 2>&1 | tail -20`
Expected: PASS. If an older test's hand-rolled listener accepts exactly one connection and then a test hangs or leaks, make that listener accept in a loop the way `fakeRelay` now does; the second slot's dial must have somewhere to go. `TestTunnelClientBacksOffOnImmediateSessionDeath` is unaffected: slot 1 never connects, so slot 2 never dials.

- [ ] **Step 6: Build piperd and run the whole tree**

Run: `go build ./... && go test ./... 2>&1 | tail -20`
Expected: PASS. `cmd/piperd` needs no change: `Run`'s signature and the control ops are unchanged.

- [ ] **Step 7: Verify and commit**

Run: `make verify`
Expected: exit 0.

```bash
git add internal/agent/tunnelclient.go internal/agent/tunnelclient_test.go
git commit -m "feat(agent): hold two tunnel sessions

Two dial loops, the second gated on the first connect; a duplicate
rejection backs off to a one-minute cap and is logged once per state.

Part of #530.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 7: Docs, progress, and the PR

**Files:**
- Modify: `docs/runbooks/relay-deploy.md` (scale-out section, "What still drops", upgrade notes)
- Modify: `PROGRESS.md:5,55-57`

- [ ] **Step 1: Update the runbook's scale-out section**

In `docs/runbooks/relay-deploy.md`, under `## Scale out`, replace the paragraph beginning "**How it routes.** Each agent's tunnel is terminated by exactly one relay" and the table's `:443`, `:80`, `:7000` rows with:

```markdown
**How it routes.** Each agent holds two tunnel sessions, on two different
relays whenever the pool has two live ones (#530). Each relay records itself
as an owner in Postgres (`relay_instances` with a 5 s heartbeat,
`agent_owners`, one row per session). The edge keeps an in-memory copy,
refreshed by LISTEN/NOTIFY and a 15 s poll, and forwards raw bytes:

| Listener | Decision |
| --- | --- |
| `:443` | SNI → agent → an owner relay: non-draining first, then fewest sessions, then earliest started; a dead owner costs one retry on the other. `api.<apex>` round-robins across the pool. |
| `:80` | Host → custom domain → an owner relay, same rule (custom domains only, as today). |
| `:7000` | the routing preface names the agent; the live relay with the fewest sessions that does not already hold it. |

**At least two relays.** A supported deployment is `piper-edge` in front of
two or more `piper-relay` processes. With one relay an agent's second session
can never place: the relay rejects it as a duplicate, the agent retries once
a minute and logs the reason once, and the relay logs each refusal. Nothing
breaks, but nothing is redundant either. A rolling restart with one relay
unavailable at a time (`maxUnavailable: 1`, or ECS `minimumHealthyPercent`)
is zero-drop: no agent loses both sessions while the replaced relay is out.
```

Replace the paragraph beginning "**What still drops.**" with:

```markdown
**What still drops.** A relay restart closes one of each agent's two
sessions, and the edge routes through the other while the lost slot redials
onto the replacement (#530); nothing is unrouted. Restarting the edge on a
single host drops every tunnel through it (on Kubernetes the Service holds
the port across a rolling restart). Both relays drain at once if they are
recreated together, and on compose they are: `docker compose up -d`
recreates every replica of a scaled service within the same second, and
`COMPOSE_PARALLEL_LIMIT=1` does not change that (confirmed on the Hetzner
host 2026-09-05; it bounds concurrent engine calls, not replica order). On
compose that means every agent loses both sessions for the seconds both
relays are down; a true one-at-a-time relay roll on compose is #535. On
Kubernetes or ECS the orchestrator's rolling update already provides it.
```

In the "Day-to-day afterwards" upgrade bullet, replace "so every agent redials once; each relay drains (#523) so requests in flight finish first." with "so every agent loses both sessions until they are back (#535); each relay drains (#523) so requests in flight finish first."

In the schema-change paragraph that names `DROP TABLE agent_owners, relay_instances;`, append one sentence: "The #530 release changes `agent_owners`' primary key and needs this drop; it also changes the tunnel handshake, so agents on the previous release cannot connect until they are upgraded — roll relays and edge first, then every agent the same day."

- [ ] **Step 2: Update PROGRESS.md**

In `PROGRESS.md`, change the `_Last updated:` line's opening to `_Last updated: 2026-09-05 — relay scale-out child 4 (epic [#524](https://github.com/piperbox/piper/issues/524)) landed: two tunnel sessions per agent on two relays. Earlier: relay scale-out child 3 ... landed: graceful drain on SIGTERM.` (keep the rest of the line as it is). After the graceful-drain bullet add:

```markdown
- ✅ two tunnel sessions per agent — the handshake carries a routing preface the edge peeks; `:7000` places away from the agent's current owners; `agent_owners` is one row per session and `:443`/`:80` pick among owners and retry once; relays refuse duplicates after auth and derive routes from Postgres on `piper_hostnames`; a relay going away is never an agent's only link — [#530](https://github.com/piperbox/piper/issues/530) (child of epic [#524](https://github.com/piperbox/piper/issues/524)); e2e on the supported topology tracked in [#537](https://github.com/piperbox/piper/issues/537)
```

In the graceful-drain bullet, change "zero-drop arrives with [#530]" to "zero-drop landed with [#530]".

- [ ] **Step 3: Commit the docs**

```bash
git add docs/runbooks/relay-deploy.md PROGRESS.md
git commit -m "docs: two sessions per agent in the runbook and progress map (#530)

Part of #530.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

- [ ] **Step 4: Final verification and PR**

Run: `make verify`
Expected: exit 0.

Push the branch and open the PR into `main`:

```bash
git push -u origin claude/issue-530-1f0c0c
gh pr create --base main --title "[relay] two tunnel sessions per agent on two relays" --body-file - <<'EOF'
Fourth child of the scale-out epic #524. Design: `docs/superpowers/specs/2026-09-05-two-sessions-per-agent-design.md`; plan: `docs/superpowers/plans/2026-09-05-two-sessions-per-agent.md`.

Each agent now holds two tunnel sessions on two different relays, so a relay going away is never an agent's only link.

- **tunnel**: the handshake is a routing preface (base domain) then a credential frame; `ErrDuplicateSession` is the one rejection the relay names, post-auth.
- **edge**: `:7000` peeks the preface and places away from the agent's current owners (soft exclusion, falls back to the full pool); `:443`/`:80` pick among live owners (non-draining, fewest sessions, earliest) and retry once on a dead one.
- **store**: `agent_owners` keyed on `(agent_name, instance_id)`; `SetOwner` is an idempotent insert that notifies once; `OwnerOf`/`Owners` return sets.
- **relay**: duplicate sessions refused after auth; routes derived from Postgres at register and on `piper_hostnames`; the stale-session close on `piper_owners` is gone; the heartbeat re-records every held base each beat.
- **agent**: two dial loops, the second gated on the first connect; duplicate rejection backs off to one minute and logs once.

**Release notes:** schema change — `DROP TABLE agent_owners, relay_instances;` before the relay `up`. Wire change — agents on the previous release cannot connect until upgraded; roll relays and edge first, then agents. Minor bump.

Follow-ups: #531 (zone-aware placement), #537 (e2e on edge + two relays).

Closes #530

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
```
