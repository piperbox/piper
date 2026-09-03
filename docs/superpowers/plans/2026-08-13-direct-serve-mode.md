# Direct Serve Mode Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A relay-enrolled box can serve its BYO custom domain's traffic directly (DNS A record → box, box-terminated TLS) while the relay keeps login, control streams, webhooks, and free-tier URLs.

**Architecture:** A `serve: "relay" | "direct"` field on the box-wide `domain_config` changes *only* DNS guidance and the `dns_ok` check — cert issuance, Caddy config, deploy routing, and the relay claim are untouched. The box learns its public IP from a new `observed_addr` field in the tunnel handshake ack (override: `PIPER_PUBLIC_IP`).

**Tech Stack:** Go, `modernc.org/sqlite` (pure Go — no cgo anywhere), embedded Caddy, yamux tunnel. Spec: [`docs/superpowers/specs/2026-08-13-direct-serve-mode-design.md`](../specs/2026-08-13-direct-serve-mode-design.md).

## Global Constraints

- `CGO_ENABLED=0` everywhere; never add a cgo dependency.
- Module path `github.com/piperbox/piper`.
- Pre-1.0: **no migrations** — edit `schema.sql`'s `CREATE TABLE` directly; old DBs are unsupported.
- Layering: `store` → (`domain`, `deploy`) → `api` → `client`; `tunnel` → `agent`; nothing imports "up".
- Every task ends with the tree compiling and `go test ./internal/... ./cmd/...` green (Docker-dependent tests skip cleanly).
- Commits: conventional-commit style, one per task step-group, ending with `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.
- Run `make verify` (gofmt → vet → test → cross) before the final claim of done.
- Match the style of the surrounding package; comments state constraints, not narration.

---

### Task 1: store — `serve` column and accessors

**Files:**
- Modify: `internal/store/schema.sql:38-47` (the `domain_config` table)
- Modify: `internal/store/store.go:551-624` (`DomainConfig`, `SetDomainConfig`, `GetDomainConfig`; new `UpdateDomainServe`)
- Modify: `internal/domain/domain.go:399` (one mechanical call-site fix to keep the tree green)
- Test: `internal/store/store_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: `DomainConfig.Serve string`; `SetDomainConfig(domain, provider, token, serve string) error`; `UpdateDomainServe(domain, serve string) error` (returns `ErrNotFound` when no row matches `domain`). Tasks 5–6 rely on these exact names.

- [ ] **Step 1: Write the failing tests**

Append to `internal/store/store_test.go`, following the file's existing open pattern (open a store the same way the neighboring `TestDomainConfig*` tests do):

```go
func TestDomainConfigServeRoundTrip(t *testing.T) {
	s := openTestStore(t) // use the same helper/open pattern as the existing domain_config tests

	if err := s.SetDomainConfig("shop.example.com", "cloudflare", "tok", "direct"); err != nil {
		t.Fatal(err)
	}
	dc, err := s.GetDomainConfig()
	if err != nil {
		t.Fatal(err)
	}
	if dc.Serve != "direct" {
		t.Fatalf("Serve = %q, want direct", dc.Serve)
	}

	// Upsert replaces serve along with the rest.
	if err := s.SetDomainConfig("shop.example.com", "cloudflare", "tok", "relay"); err != nil {
		t.Fatal(err)
	}
	dc, _ = s.GetDomainConfig()
	if dc.Serve != "relay" {
		t.Fatalf("Serve after upsert = %q, want relay", dc.Serve)
	}
}

func TestUpdateDomainServeFlipsWithoutTouchingStatus(t *testing.T) {
	s := openTestStore(t)
	if err := s.SetDomainConfig("shop.example.com", "cloudflare", "tok", "relay"); err != nil {
		t.Fatal(err)
	}
	na := time.Now().Add(60 * 24 * time.Hour)
	if err := s.UpdateDomainStatus("shop.example.com", "active", "", na); err != nil {
		t.Fatal(err)
	}

	if err := s.UpdateDomainServe("shop.example.com", "direct"); err != nil {
		t.Fatal(err)
	}
	dc, _ := s.GetDomainConfig()
	if dc.Serve != "direct" || dc.Status != "active" || dc.CertNotAfter.IsZero() {
		t.Fatalf("after serve flip: serve=%q status=%q notAfter=%v", dc.Serve, dc.Status, dc.CertNotAfter)
	}

	// Stale-domain guard, same contract as UpdateDomainStatus.
	if err := s.UpdateDomainServe("other.example.com", "relay"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("mismatched domain = %v, want ErrNotFound", err)
	}
}
```

If the existing tests open stores inline rather than via a helper, do the same inline and drop `openTestStore`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/store/ -run 'DomainConfigServe|UpdateDomainServe' -v`
Expected: FAIL — compile error (`SetDomainConfig` takes 3 args; `Serve`/`UpdateDomainServe` undefined).

- [ ] **Step 3: Implement**

`internal/store/schema.sql` — add the column (straight edit, no migration):

```sql
CREATE TABLE IF NOT EXISTS domain_config (
    id             INTEGER PRIMARY KEY CHECK (id = 1),
    domain         TEXT NOT NULL,
    dns_provider   TEXT NOT NULL,
    dns_token      TEXT NOT NULL,
    serve          TEXT NOT NULL DEFAULT 'relay',
    status         TEXT NOT NULL,
    error          TEXT NOT NULL DEFAULT '',
    cert_not_after TEXT NOT NULL DEFAULT '',
    updated_at     TEXT NOT NULL
);
```

`internal/store/store.go`:

```go
// In DomainConfig, after DNSToken:
	Serve string // "relay" | "direct": which path the public reaches this domain by
```

```go
// SetDomainConfig upserts the custom-domain config, resetting it to a fresh
// "issuing" state. serve is "relay" or "direct" (validated by the domain layer).
func (s *Store) SetDomainConfig(domain, provider, token, serve string) error {
	_, err := s.db.Exec(
		`INSERT INTO domain_config(id, domain, dns_provider, dns_token, serve, status, error, cert_not_after, updated_at)
		 VALUES(1,?,?,?,?,'issuing','','',?)
		 ON CONFLICT(id) DO UPDATE SET domain=excluded.domain,
		   dns_provider=excluded.dns_provider, dns_token=excluded.dns_token,
		   serve=excluded.serve,
		   status='issuing', error='', cert_not_after='', updated_at=excluded.updated_at`,
		domain, provider, token, serve, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}
```

In `GetDomainConfig`, extend the SELECT and Scan:

```go
	err := s.db.QueryRow(
		`SELECT domain, dns_provider, dns_token, serve, status, error, cert_not_after, updated_at
		 FROM domain_config WHERE id=1`).
		Scan(&dc.Domain, &dc.DNSProvider, &dc.DNSToken, &dc.Serve, &dc.Status, &dc.Error, &notAfter, &updated)
```

New accessor, after `UpdateDomainStatus`:

```go
// UpdateDomainServe flips the serve mode without touching issuance state —
// the cert is identical either way, so a serve-only change must not re-enter
// the issue loop. Conditional on the stored domain like UpdateDomainStatus:
// ErrNotFound when no row matches.
func (s *Store) UpdateDomainServe(domain, serve string) error {
	res, err := s.db.Exec(
		`UPDATE domain_config SET serve=?, updated_at=? WHERE id=1 AND domain=?`,
		serve, time.Now().UTC().Format(time.RFC3339Nano), domain)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
```

Mechanical green-keeper — `internal/domain/domain.go:399` (inside `Set`), pass the literal for now (Task 5 replaces it):

```go
	if err := s.st.SetDomainConfig(d, provider, token, "relay"); err != nil {
```

(The variable is `m.st` in context — keep the receiver as-is.) Also update any `SetDomainConfig(` calls in `internal/domain/domain_test.go` / `internal/store/store_test.go` to pass `"relay"`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/store/ ./internal/domain/ -v -run 'Domain'`
Expected: PASS (all pre-existing domain tests still green).

- [ ] **Step 5: Commit**

```bash
git add internal/store/ internal/domain/
git commit -m "feat(store): serve column on domain_config with serve-only update

Part of the direct serve mode design (docs/superpowers/specs/2026-08-13).

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 2: tunnel — `observed_addr` in the handshake ack

**Files:**
- Modify: `internal/tunnel/tunnel.go:104-152` (`handshakeAck`, `Dial`) and `:250-291` (`Serve`)
- Test: `internal/tunnel/tunnel_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: `tunnel.Session.ObservedAddr string` — the client-side host (no port) the relay saw the tunnel connection come from; `""` when the relay's view isn't host:port-shaped (e.g. `net.Pipe` in tests). Task 3 reads it.

- [ ] **Step 1: Write the failing test**

Append to `internal/tunnel/tunnel_test.go`. `net.Pipe`'s `RemoteAddr` is not host:port-shaped, so this test needs a real TCP pair:

```go
// The relay tells the agent the source address it accepted the tunnel from
// (host only): the box's best guess at its own public IP for direct serve
// mode's DNS guidance. Advisory — dns_ok stays the truth signal.
func TestDialCarriesObservedAddr(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	srvCh := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			srvCh <- err
			return
		}
		_, err = Serve(conn, func(string, string) error { return nil })
		srvCh <- err
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	sess, err := Dial(conn, "tok", "alice.example.com")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if err := <-srvCh; err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if sess.ObservedAddr != "127.0.0.1" {
		t.Fatalf("ObservedAddr = %q, want 127.0.0.1", sess.ObservedAddr)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tunnel/ -run TestDialCarriesObservedAddr -v`
Expected: FAIL — `sess.ObservedAddr undefined`.

- [ ] **Step 3: Implement**

`internal/tunnel/tunnel.go`:

```go
// handshakeAck is the relay's verdict on a handshake, sent before yamux starts.
// An empty Error means accepted. ObservedAddr is the source host (no port) the
// relay accepted the connection from — the agent's best guess at its own
// public IP for direct serve mode; advisory, since it can be a NAT egress.
type handshakeAck struct {
	Error        string `json:"error,omitempty"`
	ObservedAddr string `json:"observed_addr,omitempty"`
}
```

`Session` gains the field (after `BaseDomain`):

```go
	ObservedAddr string // host the relay saw this connection from; "" if unknown
```

In `Dial`, after the `ack.Error != ""` check, thread it into the return:

```go
	return &Session{BaseDomain: baseDomain, ObservedAddr: ack.ObservedAddr, mux: mux}, nil
```

In `Serve`, replace the accept-path ack construction (`handshakeAck{}`) with:

```go
	observed := ""
	if host, _, err := net.SplitHostPort(conn.RemoteAddr().String()); err == nil {
		observed = host
	}
	ackPayload, _ := json.Marshal(handshakeAck{ObservedAddr: observed})
```

(The rejection-path ack keeps only `Error` — an unauthenticated peer learns nothing extra.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/tunnel/ -v`
Expected: PASS, including `TestDialServeRoundTrip` (net.Pipe → `ObservedAddr` stays `""`, harmless).

- [ ] **Step 5: Commit**

```bash
git add internal/tunnel/
git commit -m "feat(relay): report the observed source host in the tunnel handshake ack

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 3: agent — `TunnelClient.ObservedIP`

**Files:**
- Modify: `internal/agent/tunnelclient.go:24-34` (struct), `:88-145` (`Run`)
- Test: `internal/agent/tunnelclient_test.go`

**Interfaces:**
- Consumes: `tunnel.Session.ObservedAddr` (Task 2).
- Produces: `(*agent.TunnelClient).ObservedIP() string` — last non-empty observed host, sticky across disconnects (`""` before first connect). Task 7 wires it into the domain manager.

- [ ] **Step 1: Write the failing test**

Append to `internal/agent/tunnelclient_test.go`, reusing the file's existing fake-relay harness if one exists (a TCP listener whose accept loop calls `tunnel.Serve` with an always-OK auth). If none fits, add:

```go
// ObservedIP surfaces the relay-reported source host and survives disconnects:
// direct mode's DNS guidance must not blank out whenever the tunnel drops.
func TestObservedIPStickyAcrossDisconnect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			sess, err := tunnel.Serve(conn, func(string, string) error { return nil })
			if err != nil {
				conn.Close()
				continue
			}
			sess.Close() // immediate close: the client sees a disconnect
		}
	}()

	c := &TunnelClient{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go c.Run(ctx, ln.Addr().String(), "tok", "alice.example.com",
		func(kind byte, stream net.Conn) (net.Conn, error) { return nil, errors.New("no backend") })

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c.ObservedIP() == "127.0.0.1" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := c.ObservedIP(); got != "127.0.0.1" {
		t.Fatalf("ObservedIP = %q, want 127.0.0.1", got)
	}

	cancel() // Run exits; the value must survive the session teardown
	time.Sleep(50 * time.Millisecond)
	if got := c.ObservedIP(); got != "127.0.0.1" {
		t.Fatalf("ObservedIP after disconnect = %q, want 127.0.0.1", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agent/ -run TestObservedIPSticky -v`
Expected: FAIL — `c.ObservedIP undefined`.

- [ ] **Step 3: Implement**

`internal/agent/tunnelclient.go` — struct field (after `lastErr`):

```go
	observedIP string // last relay-reported source host; sticky across reconnects
```

Accessors (next to `setErr`/`setSession`):

```go
// ObservedIP is the source host the relay last saw this box connect from —
// the box's best guess at its own public IP (advisory: it can be a NAT
// egress). "" until the first successful handshake; deliberately never
// cleared on disconnect, so direct mode's DNS guidance survives tunnel blips.
func (c *TunnelClient) ObservedIP() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.observedIP
}

func (c *TunnelClient) setObservedIP(ip string) {
	if ip == "" {
		return
	}
	c.mu.Lock()
	c.observedIP = ip
	c.mu.Unlock()
}
```

In `Run`, right after `c.setSession(sess)` (line ~132):

```go
		c.setObservedIP(sess.ObservedAddr)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/agent/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agent/
git commit -m "feat(agent): surface the relay-observed public IP on the tunnel client

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 4: config — `PIPER_PUBLIC_IP` and `PIPER_SERVE`

**Files:**
- Modify: `internal/config/config.go:12-83` (`Config`, `Load`)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: `Config.PublicIP string` and `Config.Serve string` (raw env values; validation happens in `cmd/piperd`, Task 7).

- [ ] **Step 1: Write the failing test**

Append to `internal/config/config_test.go`:

```go
func TestLoadPublicIPAndServe(t *testing.T) {
	t.Setenv("PIPER_PUBLIC_IP", "203.0.113.7")
	t.Setenv("PIPER_SERVE", "direct")
	c := Load()
	if c.PublicIP != "203.0.113.7" {
		t.Errorf("PublicIP = %q", c.PublicIP)
	}
	if c.Serve != "direct" {
		t.Errorf("Serve = %q", c.Serve)
	}
}
```

(If the file's default-value test asserts the full zero Config, extend it: both default to `""`.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestLoadPublicIPAndServe -v`
Expected: FAIL — fields undefined.

- [ ] **Step 3: Implement**

`internal/config/config.go` — in `Config`, after `Terminated`:

```go
	// PublicIP overrides the relay-observed public IP used by direct serve
	// mode's DNS guidance (split-horizon setups, never-enrolled boxes).
	PublicIP string
	// Serve pins the env-managed BYO domain's serve mode: "" (relay) | "direct".
	// API-managed domains carry serve in the store instead.
	Serve string
```

In `Load`'s returned literal, after the `Terminated:` line:

```go
		PublicIP: env("PIPER_PUBLIC_IP", ""),
		Serve:    env("PIPER_SERVE", ""),
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/config/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/
git commit -m "feat(agent): PIPER_PUBLIC_IP and PIPER_SERVE config knobs

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 5: domain — serve mode through Set/Status/DNS guidance

The heart of the change. `domain.Manager` learns the serve mode; `Set` grows a `serve` parameter (with a no-reissue fast path for serve-only flips); `Status` reports `serve`, mode-appropriate `dns_records`, a `note` when the public IP is unknown, and a direct-mode `dns_ok`. Includes the mechanical ripple through `api.DomainManager` so the tree stays green (behavioral API work is Task 6).

**Files:**
- Modify: `internal/domain/domain.go` (constants, `Options`, `Manager`, `Set`, `Status`, `dnsRecords`, new `cachedDNSResolvesTo`)
- Modify: `internal/api/api.go:70-78` (`DomainManager` interface), `:523` (pass `""`)
- Modify: `internal/api/api_test.go:1016` (`fakeDomainManager.Set` signature)
- Test: `internal/domain/domain_test.go`

**Interfaces:**
- Consumes: `store.SetDomainConfig(domain, provider, token, serve)`, `store.UpdateDomainServe(domain, serve)`, `store.DomainConfig.Serve` (Task 1).
- Produces (Tasks 6–7 rely on these exact names):
  - `domain.ServeRelay = "relay"`, `domain.ServeDirect = "direct"`, `domain.ErrInvalidServe`
  - `(*Manager).Set(domainName, provider, token, serve string) (Status, error)` — `serve == ""` means `ServeRelay`
  - `Options.PublicIP func() string` (nil ⇒ always unknown), `Options.EnvServe string`
  - `Status.Serve string` (`json:"serve"`), `Status.Note string` (`json:"note,omitempty"`)

- [ ] **Step 1: Write the failing tests**

Append to `internal/domain/domain_test.go`. Reuse `newTestManager(t, &fakeIssuer{})` (returns `m, st, proxy, relay, dataDir`) and the file's existing wait-for-active pattern; for tests needing custom `Options`, construct `New(Options{...})` directly the way the env-mode test at line ~511 does.

```go
func TestSetInvalidServeRejected(t *testing.T) {
	m, _, _, _, _ := newTestManager(t, &fakeIssuer{})
	if _, err := m.Set("shop.example.com", "cloudflare", "tok", "bogus"); !errors.Is(err, ErrInvalidServe) {
		t.Fatalf("err = %v, want ErrInvalidServe", err)
	}
}

func TestSetServeDirectPersistsAndReports(t *testing.T) {
	m, _, _, _, _ := newTestManager(t, &fakeIssuer{})
	if _, err := m.Set("shop.example.com", "cloudflare", "tok", "direct"); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, m, StatusActive) // use the file's existing polling helper/pattern
	st, err := m.Status()
	if err != nil {
		t.Fatal(err)
	}
	if st.Serve != ServeDirect {
		t.Fatalf("Serve = %q, want direct", st.Serve)
	}
}

// A serve-only flip on an active config must not burn an ACME order or leave
// "active": the cert is identical either way (spec: "must not re-issue").
func TestServeOnlyFlipDoesNotReissue(t *testing.T) {
	iss := &fakeIssuer{}
	m, _, _, _, _ := newTestManager(t, iss)
	if _, err := m.Set("shop.example.com", "cloudflare", "tok", "relay"); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, m, StatusActive)
	calls := iss.obtainCalls()

	st, err := m.Set("shop.example.com", "cloudflare", "tok", "direct")
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != StatusActive || st.Serve != ServeDirect {
		t.Fatalf("after flip: status=%q serve=%q, want active/direct", st.Status, st.Serve)
	}
	if got := iss.obtainCalls(); got != calls {
		t.Fatalf("obtain calls %d → %d: serve flip re-issued", calls, got)
	}
}

func TestDirectDNSRecordsAndDNSOK(t *testing.T) {
	// Manager with a known public IP and a resolver that agrees with it.
	st := openDomainTestStore(t) // same store setup newTestManagerWith uses
	m := New(Options{
		Store: st, Proxy: &fakeProxy{}, DataDir: t.TempDir(),
		Issuer:      func(string, string) (Issuer, error) { return &fakeIssuer{}, nil },
		HTTPSListen: ":443",
		PublicIP:    func() string { return "203.0.113.7" },
		Resolve: func(ctx context.Context, host string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("203.0.113.7")}, nil
		},
	})
	t.Cleanup(m.Close)
	if _, err := m.Set("shop.example.com", "cloudflare", "tok", "direct"); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, m, StatusActive)

	got, err := m.Status()
	if err != nil {
		t.Fatal(err)
	}
	want := []DNSRecord{
		{Type: "A", Name: "*.shop.example.com", Value: "203.0.113.7"},
		{Type: "A", Name: "shop.example.com", Value: "203.0.113.7"},
	}
	if !reflect.DeepEqual(got.DNSRecords, want) {
		t.Fatalf("DNSRecords = %+v, want %+v", got.DNSRecords, want)
	}
	if !got.DNSOK {
		t.Fatal("dns_ok = false with matching resolver")
	}
	if got.Note != "" {
		t.Fatalf("Note = %q, want empty with known IP", got.Note)
	}
}

func TestDirectUnknownIPCarriesNote(t *testing.T) {
	st := openDomainTestStore(t)
	m := New(Options{
		Store: st, Proxy: &fakeProxy{}, DataDir: t.TempDir(),
		Issuer:      func(string, string) (Issuer, error) { return &fakeIssuer{}, nil },
		HTTPSListen: ":443",
		// PublicIP nil: never enrolled, no override — IP unknown.
	})
	t.Cleanup(m.Close)
	if _, err := m.Set("shop.example.com", "cloudflare", "tok", "direct"); err != nil {
		t.Fatal(err)
	}
	got, err := m.Status()
	if err != nil {
		t.Fatal(err)
	}
	if got.Note == "" {
		t.Fatal("want a human-readable note when the public IP is unknown")
	}
	if got.DNSOK {
		t.Fatal("dns_ok must stay false with no IP to check against")
	}
	for _, r := range got.DNSRecords {
		if r.Type != "A" || r.Value != "" {
			t.Fatalf("record = %+v, want empty-value A record", r)
		}
	}
}

func TestEnvServeDirect(t *testing.T) {
	st := openDomainTestStore(t)
	m := New(Options{
		Store: st, Proxy: &fakeProxy{}, EnvDomain: "env.example.com",
		EnvServe: "direct",
		PublicIP: func() string { return "203.0.113.7" },
	})
	t.Cleanup(m.Close)
	got, err := m.Status()
	if err != nil {
		t.Fatal(err)
	}
	if got.Serve != ServeDirect {
		t.Fatalf("env Serve = %q, want direct", got.Serve)
	}
	if got.DNSRecords[0].Type != "A" || got.DNSRecords[0].Value != "203.0.113.7" {
		t.Fatalf("env direct records = %+v", got.DNSRecords)
	}
}
```

Adapt helper names (`waitStatus`, `openDomainTestStore`) to whatever the file actually uses — the assertions are the contract, the scaffolding follows the neighbors.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/domain/ -run 'Serve|DirectDNS|EnvServe' -v`
Expected: FAIL — compile errors (`Set` arity, `ServeDirect`, `Options.PublicIP` undefined).

- [ ] **Step 3: Implement `internal/domain/domain.go`**

Constants and error (next to the Status* constants and the Err* block):

```go
// Serve modes: which path the public reaches the domain by. Relay is the
// default (CNAME at the relay, SNI-spliced down the tunnel); direct means the
// user's DNS points straight at this box and its Caddy answers :443 itself.
const (
	ServeRelay  = "relay"
	ServeDirect = "direct"
)

var ErrInvalidServe = errors.New(`invalid serve mode (want "relay" or "direct")`)
```

`Options` additions (after `EnvDomain`):

```go
	// EnvServe pins the env-managed domain's serve mode ("" ⇒ relay).
	EnvServe string
	// PublicIP reports the box's best-known public IP for direct mode's DNS
	// guidance ("" ⇒ unknown). Nil tolerated: always unknown.
	PublicIP func() string
```

`Manager` fields (after `envDomain`):

```go
	envServe string
	publicIP func() string
```

In `New`, normalize and default:

```go
	envServe := o.EnvServe
	if envServe == "" {
		envServe = ServeRelay
	}
	publicIP := o.PublicIP
	if publicIP == nil {
		publicIP = func() string { return "" }
	}
```

…and add `envServe: envServe, publicIP: publicIP,` to the returned literal.

`Set` — new signature and the serve-only fast path (replace the current signature and the body between the env-managed check and the domainRE check accordingly):

```go
func (m *Manager) Set(domainName, provider, token, serve string) (Status, error) {
	if m.envDomain != "" {
		return Status{}, ErrEnvManaged
	}
	d := strings.ToLower(strings.TrimSpace(domainName))
	if !domainRE.MatchString(d) {
		return Status{}, ErrInvalidDomain
	}
	if serve == "" {
		serve = ServeRelay
	}
	if serve != ServeRelay && serve != ServeDirect {
		return Status{}, ErrInvalidServe
	}
	if provider != "cloudflare" {
		return Status{}, ErrUnsupportedProvider
	}
	if token == "" {
		return Status{}, ErrTokenRequired
	}
	m.issueMu.Lock()
	prev, prevErr := m.st.GetDomainConfig()
	// Serve-only flip on an active, otherwise-identical config: the cert is
	// the same either way, so update the row in place — no status reset, no
	// generation bump, no ACME order.
	if prevErr == nil && prev.Domain == d && prev.DNSProvider == provider &&
		prev.DNSToken == token && prev.Status == StatusActive && prev.Serve != serve {
		if err := m.st.UpdateDomainServe(d, serve); err != nil {
			m.issueMu.Unlock()
			return Status{}, err
		}
		m.issueMu.Unlock()
		return m.Status()
	}
	// Replacing a different domain tears the old one down first.
	if prevErr == nil && prev.Domain != d {
		m.teardown(prev)
	}
	if err := m.st.SetDomainConfig(d, provider, token, serve); err != nil {
		m.issueMu.Unlock()
		return Status{}, err
	}
	m.issueMu.Unlock()
	// (rest of Set unchanged: snapshot Status, bump gen, spawn issueLoop)
```

`Status` struct additions (after `Source`):

```go
	Serve string `json:"serve"` // "relay" | "direct"
	// Note is a human-readable hint when guidance is incomplete (e.g. direct
	// mode before the box knows its public IP). Not an error.
	Note string `json:"note,omitempty"`
```

`dnsRecords` becomes mode-aware:

```go
// dnsRecords renders the records the user must create: CNAMEs at the relay in
// relay mode, A/AAAA at the box's public IP in direct mode. An unknown IP
// yields empty-value records — Status attaches the explanatory note.
func (m *Manager) dnsRecords(domain, serve string) []DNSRecord {
	if serve != ServeDirect {
		return []DNSRecord{
			{Type: "CNAME", Name: "*." + domain, Value: m.dnsTarget},
			{Type: "CNAME", Name: domain, Value: m.dnsTarget},
		}
	}
	ip := m.publicIP()
	typ := "A"
	if p := net.ParseIP(ip); p != nil && p.To4() == nil {
		typ = "AAAA"
	}
	return []DNSRecord{
		{Type: typ, Name: "*." + domain, Value: ip},
		{Type: typ, Name: domain, Value: ip},
	}
}
```

The unknown-IP note, as a package constant near the serve constants:

```go
// noteNoPublicIP explains empty-value direct-mode records.
const noteNoPublicIP = "box public IP not yet known — connect to the relay once or set PIPER_PUBLIC_IP"
```

`Status()` — both branches gain serve awareness. Env branch:

```go
		st := Status{
			Domain: m.envDomain, Source: "env", Serve: m.envServe,
			Status: m.envStatus, Error: m.envError,
			DNSRecords: m.dnsRecords(m.envDomain, m.envServe),
		}
		if m.envServe == ServeDirect {
			if ip := m.publicIP(); ip == "" {
				st.Note = noteNoPublicIP
			} else {
				st.DNSOK = m.cachedDNSResolvesTo("piper-probe."+m.envDomain, ip)
			}
		}
```

API branch (replace the existing `st.DNSOK = ...` line and extend the literal):

```go
	st := Status{
		Domain: dc.Domain, DNSProvider: dc.DNSProvider,
		DNSTokenSet: dc.DNSToken != "", Source: "api", Serve: dc.Serve,
		Status: dc.Status, Error: dc.Error,
		DNSRecords: m.dnsRecords(dc.Domain, dc.Serve),
	}
	// piper-probe.<domain>: any label matches the user's wildcard record. Relay
	// mode checks it lands on the relay; direct mode checks it lands on the box.
	if dc.Serve == ServeDirect {
		if ip := m.publicIP(); ip == "" {
			st.Note = noteNoPublicIP
		} else {
			st.DNSOK = m.cachedDNSResolvesTo("piper-probe."+dc.Domain, ip)
		}
	} else {
		st.DNSOK = m.cachedDNSPointsAt("piper-probe." + dc.Domain)
	}
```

New cached check (next to `cachedDNSPointsAt`, sharing `dnsCache` with a composite key so relay- and direct-mode probes never collide):

```go
// cachedDNSResolvesTo serves "does host resolve to ip" from the same short
// TTL cache as cachedDNSPointsAt (keyed with the target ip so a serve-mode
// flip never reads the other mode's cached verdict).
func (m *Manager) cachedDNSResolvesTo(host, ip string) bool {
	key := host + "→" + ip
	m.dnsMu.Lock()
	if e, ok := m.dnsCache[key]; ok && m.now().Sub(e.at) < dnsOKTTL {
		m.dnsMu.Unlock()
		return e.ok
	}
	m.dnsMu.Unlock()

	want := net.ParseIP(ip)
	ok := false
	if want != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if ips, err := m.resolve(ctx, host); err == nil {
			for _, p := range ips {
				if p.Equal(want) {
					ok = true
					break
				}
			}
		}
		cancel()
	}

	m.dnsMu.Lock()
	m.dnsCache[key] = dnsCacheEntry{ok: ok, at: m.now()}
	m.dnsMu.Unlock()
	return ok
}
```

Remove the Task-1 `"relay"` literal: the `SetDomainConfig` call now passes `serve`.

**Mechanical ripple (same commit, keeps the tree green):**

- `internal/api/api.go:71`: `Set(domain, provider, token, serve string) (domain.Status, error)`
- `internal/api/api.go:523`: `st, err := dom.Set(in.Domain, in.DNSProvider, in.DNSToken, "")` (Task 6 threads the real value)
- `internal/api/api_test.go:1016`: `func (f *fakeDomainManager) Set(d, p, tok, serve string) (domain.Status, error) {` (body unchanged)
- Any `m.Set(` calls in `internal/domain/domain_test.go` and `lifecycle` tests gain a fourth arg (`""` or `"relay"`).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/domain/ ./internal/api/ -v`
Expected: PASS — new tests green, every pre-existing domain/api test untouched behaviorally.

- [ ] **Step 5: Commit**

```bash
git add internal/domain/ internal/api/
git commit -m "feat(agent): serve mode on the box-wide domain — direct DNS guidance and dns_ok

Serve-only flips on an active config update the row in place: no status
reset, no generation bump, no ACME order.

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 6: api — accept and report `serve`

**Files:**
- Modify: `internal/api/api.go:510-538` (PUT handler)
- Test: `internal/api/api_test.go`

**Interfaces:**
- Consumes: `domain.ErrInvalidServe`, `DomainManager.Set(domain, provider, token, serve)` (Task 5).
- Produces: `PUT /v1/domain` accepts `{"serve": "direct"}` (absent ⇒ relay); invalid serve → 400. `GET /v1/domain` responses carry `"serve"`/`"note"` via `domain.Status`'s JSON tags — no handler change needed for GET.

- [ ] **Step 1: Write the failing tests**

Append to `internal/api/api_test.go`, following the file's existing domain-endpoint test pattern (handler built with a `fakeDomainManager`, `httptest` requests). Extend `fakeDomainManager` to record the serve argument:

```go
// In fakeDomainManager: add field `gotServe string`; in Set: `f.gotServe = serve`.

func TestPutDomainThreadsServe(t *testing.T) {
	f := &fakeDomainManager{status: domain.Status{Domain: "shop.example.com", Serve: "direct"}}
	h := newTestHandler(t, f) // whatever helper the neighboring domain tests use
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/domain",
		strings.NewReader(`{"domain":"shop.example.com","dns_provider":"cloudflare","dns_token":"tok","serve":"direct"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body)
	}
	if f.gotServe != "direct" {
		t.Fatalf("serve passed to manager = %q, want direct", f.gotServe)
	}
	if !strings.Contains(rec.Body.String(), `"serve":"direct"`) {
		t.Fatalf("response missing serve: %s", rec.Body)
	}
}

func TestPutDomainInvalidServeIs400(t *testing.T) {
	f := &fakeDomainManager{setErr: domain.ErrInvalidServe} // reuse/add the fake's error knob
	h := newTestHandler(t, f)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/domain",
		strings.NewReader(`{"domain":"shop.example.com","dns_provider":"cloudflare","dns_token":"tok","serve":"bogus"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT invalid serve = %d, want 400", rec.Code)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/api/ -run 'PutDomain' -v`
Expected: FAIL — `gotServe` stays `""` (handler passes the hardcoded `""`), and/or 400 mapping missing.

- [ ] **Step 3: Implement**

`internal/api/api.go` PUT handler — add the field and thread it:

```go
		var in struct {
			Domain      string `json:"domain"`
			DNSProvider string `json:"dns_provider"`
			DNSToken    string `json:"dns_token"`
			Serve       string `json:"serve"`
		}
		...
		st, err := dom.Set(in.Domain, in.DNSProvider, in.DNSToken, in.Serve)
```

Add `domain.ErrInvalidServe` to the 400 case list:

```go
		case errors.Is(err, domain.ErrInvalidDomain),
			errors.Is(err, domain.ErrUnsupportedProvider),
			errors.Is(err, domain.ErrTokenRequired),
			errors.Is(err, domain.ErrInvalidServe):
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/api/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/
git commit -m "feat(agent): PUT /v1/domain accepts serve mode

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 7: cmd/piperd — wire public IP and env serve mode

**Files:**
- Modify: `cmd/piperd/main.go:356-401` (`newDomainOptions`), `:521-559` (create `tc` before the domain manager; build the publicIP func)
- Test: `cmd/piperd/main_test.go`

**Interfaces:**
- Consumes: `config.Config.PublicIP/.Serve` (Task 4), `tc.ObservedIP()` (Task 3), `domain.Options.PublicIP/.EnvServe` (Task 5).
- Produces: `publicIPFunc(cfg config.Config, tc *agent.TunnelClient) func() string` and `envServe(cfg config.Config) string` helpers in `main.go` (exported to tests by being package-level).

- [ ] **Step 1: Write the failing tests**

Append to `cmd/piperd/main_test.go` (same style as the existing `newDomainOptions` tests around line 938):

```go
// PIPER_PUBLIC_IP beats the relay-observed address; with neither, unknown.
func TestPublicIPFuncPrecedence(t *testing.T) {
	if got := publicIPFunc(config.Config{PublicIP: "203.0.113.7"}, nil)(); got != "203.0.113.7" {
		t.Fatalf("override = %q", got)
	}
	if got := publicIPFunc(config.Config{}, nil)(); got != "" {
		t.Fatalf("no sources = %q, want empty", got)
	}
	// With a tunnel client the func must consult it lazily (the observed IP
	// arrives after the domain manager is constructed).
	tc := &agent.TunnelClient{}
	if got := publicIPFunc(config.Config{}, tc)(); got != "" {
		t.Fatalf("unconnected tunnel = %q, want empty", got)
	}
}

// PIPER_SERVE reaches the env-managed manager only when valid; junk degrades
// to relay (empty) rather than wedging boot.
func TestEnvServeValidation(t *testing.T) {
	if got := envServe(config.Config{Serve: "direct"}); got != "direct" {
		t.Fatalf("direct = %q", got)
	}
	if got := envServe(config.Config{Serve: "bogus"}); got != "" {
		t.Fatalf("bogus = %q, want empty (relay)", got)
	}
	if got := envServe(config.Config{}); got != "" {
		t.Fatalf("unset = %q, want empty", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cmd/piperd/ -run 'PublicIPFunc|EnvServe' -v`
Expected: FAIL — helpers undefined.

- [ ] **Step 3: Implement**

`cmd/piperd/main.go` — two helpers near `newDomainOptions`:

```go
// publicIPFunc resolves the box's public IP for direct serve mode:
// PIPER_PUBLIC_IP pins it; otherwise the relay-observed address, read lazily
// because it only exists after the first tunnel handshake — well after the
// domain manager is constructed.
func publicIPFunc(cfg config.Config, tc *agent.TunnelClient) func() string {
	return func() string {
		if cfg.PublicIP != "" {
			return cfg.PublicIP
		}
		if tc != nil {
			return tc.ObservedIP()
		}
		return ""
	}
}

// envServe validates PIPER_SERVE for the env-managed domain path. Junk logs
// and degrades to relay: a typo must not change where user traffic flows.
func envServe(cfg config.Config) string {
	switch cfg.Serve {
	case "", domain.ServeRelay:
		return ""
	case domain.ServeDirect:
		return domain.ServeDirect
	default:
		log.Printf("piper: ignoring invalid PIPER_SERVE=%q (want relay|direct)", cfg.Serve)
		return ""
	}
}
```

`newDomainOptions` gains a parameter and passes both through — signature:

```go
func newDomainOptions(cfg config.Config, st *store.Store, dep *deploy.Deployer, alpnSolver *certs.ALPNSolver, relayHost string, publicIP func() string) domain.Options {
```

…add to the `opts` literal: `PublicIP: publicIP,` and, inside the existing `if !cfg.Terminated` block alongside `EnvDomain`:

```go
	if !cfg.Terminated {
		opts.EnvDomain = cfg.BaseDomain // env-managed BYO: API writes are 409
		opts.EnvServe = envServe(cfg)
	}
```

In `main()`, hoist the tunnel client above the domain-manager block so the closure can capture it (currently `tc` is declared at line ~555, *after* `domMgr` is built at ~536). Move the declaration up, keeping the binder comment intact:

```go
	// Created before the domain manager so its DNS guidance can consult the
	// relay-observed public IP; Run and the rest of its wiring still start in
	// the relay block below.
	var tc *agent.TunnelClient
	if cfg.RelayAddr != "" {
		tc = &agent.TunnelClient{}
	}

	var domMgr *domain.Manager
	var alpnSolver *certs.ALPNSolver
	if cfg.RelayAddr != "" {
		...
		domMgr = domain.New(newDomainOptions(cfg, st, dep, alpnSolver, relayHost, publicIPFunc(cfg, tc)))
	}
```

…and delete the later `tc = &agent.TunnelClient{}` creation, leaving `binder = tc` where it is (the `var binder api.RepoBinder` block keeps its conditional assignment so a LAN box's binder stays a genuinely nil interface — see the comment at `main.go:549-554`).

Update the existing `newDomainOptions` call sites/tests to pass a stub: `func() string { return "" }`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/piperd/ -v`
Expected: PASS, including the pre-existing `newDomainOptions` tests (#434/#435).

- [ ] **Step 5: Commit**

```bash
git add cmd/piperd/
git commit -m "feat(agent): wire relay-observed public IP and PIPER_SERVE into the domain manager

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 8: e2e — direct serve on the relay-terminated loop

**Files:**
- Create: `test/e2e/direct_test.go`

**Interfaces:**
- Consumes: the full stack from Tasks 1–7; the existing e2e helpers `writeSelfSigned`, `killOnCleanup`, `waitPort`, `terminatedHostname` (see `test/e2e/domain_test.go` / `relay_terminated_test.go`).
- Produces: `TestRelayCustomDomainDirectServe`, gated on `RUN_E2E=1` like every e2e.

- [ ] **Step 1: Write the test**

Copy the scaffold of `TestRelayCustomDomainSelfService` (`test/e2e/domain_test.go:26-222`) verbatim — relay + piperd + `piper login` + create/deploy `blog` — into `test/e2e/direct_test.go` as `TestRelayCustomDomainDirectServe`, then replace everything from the `---- Custom domain ----` marker down with:

```go
	// ---- Direct-served custom domain ----
	custom := "shop.localhost"

	put := func() (*http.Response, error) {
		req, _ := http.NewRequest(http.MethodPut, "http://127.0.0.1:8088/v1/domain",
			strings.NewReader(`{"domain":"`+custom+`","dns_provider":"cloudflare","dns_token":"fake-for-selfsigned","serve":"direct"}`))
		req.Header.Set("Authorization", "Bearer "+apiToken)
		return http.DefaultClient.Do(req)
	}
	resp, err := put()
	if err != nil {
		t.Fatalf("PUT /v1/domain: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /v1/domain = %d: %s", resp.StatusCode, b)
	}

	// Poll until active; then the guidance must be direct-shaped: A records at
	// the box's relay-observed IP (loopback here) and a green dns_ok (the
	// selfsigned seam stubs the resolver to loopback too).
	deadline = time.Now().Add(30 * time.Second)
	var domBody string
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:8088/v1/domain", nil)
		req.Header.Set("Authorization", "Bearer "+apiToken)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			gb, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			domBody = string(gb)
			if strings.Contains(domBody, `"status":"active"`) {
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !strings.Contains(domBody, `"status":"active"`) {
		t.Fatalf("domain never became active: %s", domBody)
	}
	if !strings.Contains(domBody, `"serve":"direct"`) {
		t.Fatalf("GET /v1/domain missing serve mode: %s", domBody)
	}
	if !strings.Contains(domBody, `"type":"A"`) || !strings.Contains(domBody, `"value":"127.0.0.1"`) {
		t.Fatalf("direct mode must guide A records at the observed IP: %s", domBody)
	}
	if !strings.Contains(domBody, `"dns_ok":true`) {
		t.Fatalf("dns_ok not green in direct mode: %s", domBody)
	}

	// The point of direct mode: a visitor reaches the app at the BOX's :443 —
	// no relay in the path by construction.
	curlDirect := func() string {
		d := &tls.Dialer{Config: &tls.Config{ServerName: "blog." + custom, InsecureSkipVerify: true}}
		conn, err := d.DialContext(ctx, "tcp", "127.0.0.1:443")
		if err != nil {
			return ""
		}
		fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: blog.%s\r\nConnection: close\r\n\r\n", custom)
		cb, _ := io.ReadAll(conn)
		conn.Close()
		return string(cb)
	}
	var directResp string
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if directResp = curlDirect(); directResp != "" {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if directResp == "" {
		t.Fatal("no response dialing the box's :443 directly")
	}

	// Migration property: the relay claim is kept, so the same domain still
	// serves through the relay's public port while DNS is being flipped.
	d := &tls.Dialer{Config: &tls.Config{ServerName: "blog." + custom, InsecureSkipVerify: true}}
	conn, err := d.DialContext(ctx, "tcp", "127.0.0.1:8443")
	if err != nil {
		t.Fatalf("relay-path dial: %v", err)
	}
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: blog.%s\r\nConnection: close\r\n\r\n", custom)
	rb, _ := io.ReadAll(conn)
	conn.Close()
	if len(rb) == 0 {
		t.Fatal("relay path stopped serving the direct domain (migration property broken)")
	}
```

- [ ] **Step 2: Compile-check without Docker**

Run: `go vet ./test/e2e/ && go test ./test/e2e/ -run TestRelayCustomDomainDirectServe -v`
Expected: SKIP ("set RUN_E2E=1 to run").

- [ ] **Step 3: Run the e2e for real** (needs Docker; stop any brew-service piperd holding :8088/:2019/:443 first — ask the user before `brew services stop piper`)

Run: `RUN_E2E=1 go test ./test/e2e/ -run TestRelayCustomDomainDirectServe -v -timeout 10m`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add test/e2e/direct_test.go
git commit -m "test(e2e): direct-served custom domain — box :443 answers, relay path still serves

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 9: verify, progress, follow-ups

**Files:**
- Modify: `PROGRESS.md` (one line, linking the feature issue)

- [ ] **Step 1: Full gate**

Run: `make verify`
Expected: gofmt clean, vet clean, tests green, arm64 cross-build OK. Judge by exit status, not output grep.

- [ ] **Step 2: File the tracking + follow-up issues** (use the repo's `file-issue` skill if running interactively; otherwise:)

```bash
gh issue create --title "[proxy] direct serve mode for the box-wide custom domain" \
  --label enhancement,agent,relay,P2,size/M \
  --body "Implements docs/superpowers/specs/2026-08-13-direct-serve-mode-design.md: serve=relay|direct on domain_config, direct DNS guidance + dns_ok, observed_addr in the tunnel handshake ack, PIPER_PUBLIC_IP/PIPER_SERVE."
gh issue create --title "[proxy] direct-served per-app custom domains (DNS-01 per host)" \
  --label enhancement,agent,P3,size/M \
  --body "Follow-up to the direct serve mode spec: per-app domains stay relay-served because TLS-ALPN-01 needs the relay splice; direct would need per-host DNS-01 reusing the box-wide token."
gh issue create --title "[agent] never-enrolled direct HTTPS (hoist domain manager out of the relay gates)" \
  --label enhancement,agent,P3,size/M \
  --body "Follow-up to the direct serve mode spec: serve HTTPS with no relay config by hoisting domMgr/WithHTTPS/cert bootstrap out of the cfg.RelayAddr gates in cmd/piperd/main.go, plus a URL-scheme signal for the TUI."
```

Reference the first issue from the PR body (`Closes #N`); the follow-ups stay open.

- [ ] **Step 3: PROGRESS.md line**

Add under the relay/domain section, matching the file's one-line style:

```markdown
- Direct serve mode: box-wide custom domain served by the box's own :443 (DNS → box), relay kept for login/webhooks/control [#N]
```

- [ ] **Step 4: Commit and open the PR**

```bash
git add PROGRESS.md
git commit -m "docs: PROGRESS line for direct serve mode

Part of #N

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
git push -u origin HEAD
gh pr create --base main --title "feat: direct serve mode for the box-wide custom domain" \
  --body "Implements docs/superpowers/specs/2026-08-13-direct-serve-mode-design.md.

serve=relay|direct on the box-wide domain: direct changes only DNS guidance and dns_ok; certs, Caddy, deploy routing, and the relay claim are untouched, so the DNS flip is the whole (reversible) cutover. Public IP via observed_addr in the tunnel handshake ack, PIPER_PUBLIC_IP override.

Closes #N

🤖 Generated with [Claude Code](https://claude.com/claude-code)"
```
