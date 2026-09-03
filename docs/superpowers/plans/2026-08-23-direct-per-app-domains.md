# Direct-Served Per-App Custom Domains Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** On a box whose serve mode is `direct`, per-app custom domains issue via DNS-01 reusing the box-wide token source and serve straight from the box's Caddy (#506).

**Architecture:** The `domain.Manager` learns one new fact — `boxServe()`, the box-wide serve mode — and branches per-app issuance on it: relay mode keeps today's TLS-ALPN-01 path untouched; direct mode obtains via a DNS-01 issuer built from the box-wide token source, makes the relay claim only when a relay is connected, and skips the DNS gate. The API's synchronous `AppDomainsActivatable` refusal (a #509 stopgap that existed only because #506 wasn't built) is deleted in favor of the manager's own add-time check.

**Tech Stack:** Go, `modernc.org/sqlite` store, lego DNS-01 (`internal/certs`), Caddy admin API. Spec: `docs/superpowers/specs/2026-08-23-direct-per-app-domains-design.md`.

## Global Constraints

- **No cgo** — all code must build with `CGO_ENABLED=0` (`make cross` proves it).
- **Layering** — `internal/domain` must not import `api`, `deploy`, or `cmd`; `api` is transport over the manager interface.
- **Pre-1.0 break-freely** — no compat shims; the `AppDomainsActivatable` interface method is deleted outright, not deprecated.
- Deployment status strings unchanged; per-app domain statuses stay exactly `"pending" | "issuing" | "active" | "failed"`.
- Serve mode strings are the existing consts `domain.ServeRelay` = `"relay"`, `domain.ServeDirect` = `"direct"`.
- One commit per task, conventional-commit style, trailer: `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.
- Run tests as `go test ./internal/... ./cmd/...` per task; full `make verify` in the final task.
- Work on branch `claude/issue-506-f7254d`; reference `Part of #506` in commit bodies.

---

### Task 1: Box serve mode + add-time guard (`ErrNoDNSIssuer`)

The manager learns `boxServe()` (which path the public reaches this box by) and `AddAppDomain` refuses synchronously on a direct box with no DNS-01 source.

**Files:**
- Modify: `internal/domain/domain.go` (errors block ~line 60; `Options` ~line 108; `Manager` struct ~line 145; `New` ~line 235)
- Modify: `internal/domain/appdomain.go` (`AddAppDomain`, ~line 42)
- Test: `internal/domain/appdomain_test.go`

**Interfaces:**
- Consumes: existing `Options`, `store.SetDomainConfig(domain, provider, token, serve)`, consts `ServeRelay`/`ServeDirect`.
- Produces (later tasks rely on these exact names):
  - `var ErrNoDNSIssuer error` (exported; Task 4 maps it to 409)
  - `Options.EnvIssuer func() (Issuer, error)` (Task 4 wires it from `cmd/piperd`)
  - `func (m *Manager) boxServe() string` (Tasks 2 and 3 branch on it)
  - `func (m *Manager) hasAppDNSSource() bool`

- [ ] **Step 1: Write the failing tests**

Append to `internal/domain/appdomain_test.go`:

```go
// newDirectAppManager builds a Manager on a box whose box-wide domain config
// is serve=direct ("box.example.com", cloudflare token): dnsIss backs the
// box-wide Issuer factory (the DNS-01 token source), alpnIss backs the ALPN
// AppIssuer factory. No relay is set — direct boxes may have none; tests
// that want one call m.SetRelay.
func newDirectAppManager(t *testing.T, dnsIss, alpnIss Issuer) (*Manager, *store.Store, *fakeProxy, *fakeRouter) {
	t.Helper()
	dataDir := t.TempDir()
	st, err := store.Open(filepath.Join(dataDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.SetDomainConfig("box.example.com", "cloudflare", "tok", ServeDirect); err != nil {
		t.Fatal(err)
	}
	proxy := &fakeProxy{}
	router := &fakeRouter{}
	m := New(Options{
		Store:     st,
		Issuer:    func(provider, token string) (Issuer, error) { return dnsIss, nil },
		AppIssuer: func() (Issuer, error) { return alpnIss, nil },
		Proxy:     proxy, Router: router, DataDir: dataDir,
		RelayHost: "relay.example.net", HTTPSListen: ":8443",
	})
	t.Cleanup(m.Close)
	m.retryDelay = func(int) time.Duration { return time.Millisecond }
	m.dnsWait = time.Millisecond
	m.resolve = func(_ context.Context, _ string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("203.0.113.7")}, nil
	}
	return m, st, proxy, router
}

// A direct box with no DNS-01 source can never issue a per-app cert:
// TLS-ALPN-01 has no relay splice to arrive by and DNS-01 has no token.
// AddAppDomain must refuse synchronously — nothing stored, no lifecycle
// spinning on an unrecoverable failure.
func TestAddAppDomainDirectWithoutDNSSourceRefuses(t *testing.T) {
	// API-managed shape: direct config whose row has no token.
	m, st, _, _ := newDirectAppManager(t, &fakeIssuer{}, &fakeIssuer{})
	if err := st.SetDomainConfig("box.example.com", "cloudflare", "", ServeDirect); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddAppDomain("blog", "shop.example.com"); !errors.Is(err, ErrNoDNSIssuer) {
		t.Fatalf("tokenless direct box: err = %v, want ErrNoDNSIssuer", err)
	}
	if _, err := st.GetAppDomain("shop.example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("refused add must store nothing, got err %v", err)
	}
}

// Env-managed direct box on static cert files (no EnvIssuer): same refusal.
func TestAddAppDomainDirectEnvStaticCertRefuses(t *testing.T) {
	dataDir := t.TempDir()
	st, err := store.Open(filepath.Join(dataDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := New(Options{
		Store:  st,
		Issuer: func(provider, token string) (Issuer, error) { return &fakeIssuer{}, nil },
		Proxy:  &fakeProxy{}, DataDir: dataDir,
		RelayHost: "relay.example.net", HTTPSListen: ":8443",
		EnvDomain: "box.example.com", EnvServe: ServeDirect, // EnvIssuer nil: static certs
	})
	t.Cleanup(m.Close)
	if _, err := st.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddAppDomain("blog", "shop.example.com"); !errors.Is(err, ErrNoDNSIssuer) {
		t.Fatalf("static-cert env direct box: err = %v, want ErrNoDNSIssuer", err)
	}
}

// A direct box WITH a token source accepts the add (the row lands pending);
// a relay box never hits the guard regardless of token presence — that path
// is pinned by TestAddAppDomainValidation running against a tokenless
// relay-mode manager.
func TestAddAppDomainDirectWithTokenAccepts(t *testing.T) {
	m, st, _, _ := newDirectAppManager(t, &fakeIssuer{}, &fakeIssuer{})
	if _, err := st.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	row, err := m.AddAppDomain("blog", "shop.example.com")
	if err != nil {
		t.Fatalf("AddAppDomain on a tokened direct box: %v", err)
	}
	if row.Domain != "shop.example.com" {
		t.Fatalf("row = %+v", row)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/domain/ -run 'TestAddAppDomainDirect' -v`
Expected: compile FAIL — `undefined: ErrNoDNSIssuer` (and `EnvServe`-adjacent field `EnvIssuer` unused yet is fine; the compile error is the failure signal).

- [ ] **Step 3: Implement**

In `internal/domain/domain.go`:

(a) In the `var (...)` errors block (the one holding `ErrInvalidServe`, ~line 60), add:

```go
	// ErrNoDNSIssuer rejects adding a per-app domain on a direct-served box
	// with no DNS-01 source: TLS-ALPN-01 has no relay splice to arrive by,
	// and DNS-01 has no token to write records with (#506).
	ErrNoDNSIssuer = errors.New("direct-served per-app domains need a box-wide domain with a DNS token: this box serves direct, so per-app certificates are issued via DNS-01 with the box-wide token")
```

(b) In `Options` (after the `PublicIP` field):

```go
	// EnvIssuer builds the env-managed box's DNS-01 issuer (provider creds
	// from the provider's own env vars). Nil means the env box has no ACME
	// path (static cert files) — or the box is API-managed. Direct-mode
	// per-app issuance on an env box uses it as the box-wide token source.
	EnvIssuer func() (Issuer, error)
```

(c) In the `Manager` struct, after `envServe string`:

```go
	envIssuer   func() (Issuer, error) // see Options.EnvIssuer
```

(d) In `New`, add to the composite literal (next to `envServe: envServe`):

```go
		envIssuer: o.EnvIssuer,
```

(e) Below `nextGenFor` (or near `notifier()`), add:

```go
// boxServe answers which path the public reaches this box by: the env-pinned
// mode on env-managed boxes, the domain config row's serve mode when one
// exists, relay otherwise. Per-app domains follow this box-wide fact (#506)
// — there is no per-domain serve choice.
func (m *Manager) boxServe() string {
	if m.envDomain != "" {
		return m.envServe
	}
	if dc, err := m.st.GetDomainConfig(); err == nil && dc.Serve == ServeDirect {
		return ServeDirect
	}
	return ServeRelay
}

// hasAppDNSSource reports whether a DNS-01 issuer for per-app domains could
// be built, without building one — AddAppDomain's synchronous guard.
func (m *Manager) hasAppDNSSource() bool {
	if m.envDomain != "" {
		return m.envIssuer != nil
	}
	dc, err := m.st.GetDomainConfig()
	return err == nil && dc.DNSToken != ""
}
```

In `internal/domain/appdomain.go`, in `AddAppDomain`, after the `ErrBoxWideDomain` check and before `m.st.AddAppDomain`:

```go
	if m.boxServe() == ServeDirect && !m.hasAppDNSSource() {
		return store.AppDomain{}, ErrNoDNSIssuer
	}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/domain/ -run 'TestAddAppDomain' -v`
Expected: PASS (the three new tests plus the existing `TestAddAppDomainIssuesAndActivates` / `TestAddAppDomainValidation`, which pin that relay-mode adds are untouched).

- [ ] **Step 5: Run the whole package**

Run: `go test ./internal/domain/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/domain/domain.go internal/domain/appdomain.go internal/domain/appdomain_test.go
git commit -m "feat(domain): boxServe mode + add-time guard for direct per-app domains

Part of #506

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 2: Direct-mode issuance and renewal

`appIssueOnce` branches on `boxServe()`: direct obtains via the box-wide DNS-01 token source, claims the relay only when connected, skips the DNS gate. `reissueApp` picks its issuer the same way, so renewal follows a flipped mode.

**Files:**
- Modify: `internal/domain/appdomain.go` (`appIssueOnce` ~line 109, `reissueApp` ~line 327)
- Modify: `internal/domain/domain.go` (add `appDNSIssuer` near `boxServe`)
- Test: `internal/domain/appdomain_test.go`

**Interfaces:**
- Consumes: Task 1's `boxServe()`, `Manager.envIssuer`, `ErrNoDNSIssuer`; existing `m.newIssuer IssuerFactory`, `m.appIssuer`, `fakeNotifier`, `newDirectAppManager` (Task 1 test helper).
- Produces:
  - `func (m *Manager) appDNSIssuer() (Issuer, error)`
  - `func (m *Manager) appIssuerFor(serve string) (Issuer, error)`

- [ ] **Step 1: Write the failing tests**

Append to `internal/domain/appdomain_test.go`:

```go
// Direct-mode activation: the DNS-01 issuer (box-wide token source) obtains
// the exact-host cert; the ALPN issuer is never consulted; with no relay
// connected there is no claim and no DNS gate — even unresolvable DNS
// doesn't park the domain (the user flips DNS at their own pace, like
// box-wide direct).
func TestAppDomainDirectIssuesViaDNS01(t *testing.T) {
	dnsIss := &fakeIssuer{}
	alpnIss := &fakeIssuer{}
	m, st, proxy, router := newDirectAppManager(t, dnsIss, alpnIss)
	// No DNS gate: resolution failing must not matter in direct mode.
	m.resolve = func(_ context.Context, _ string) ([]net.IP, error) {
		return nil, errors.New("no such host")
	}
	if _, err := st.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddAppDomain("blog", "shop.example.com"); err != nil {
		t.Fatalf("AddAppDomain: %v", err)
	}
	got := waitAppStatus(t, st, "shop.example.com", StatusActive)
	if got.Error != "" || got.CertNotAfter.IsZero() {
		t.Fatalf("active row = %+v", got)
	}
	dnsIss.mu.Lock()
	dnsCalls := dnsIss.calls
	dnsIss.mu.Unlock()
	alpnIss.mu.Lock()
	alpnCalls := alpnIss.calls
	alpnIss.mu.Unlock()
	if dnsCalls != 1 || alpnCalls != 0 {
		t.Fatalf("obtain calls: dns=%d alpn=%d, want 1/0", dnsCalls, alpnCalls)
	}
	// Activation is fully local: HTTPS armed, cert loaded, route backfilled.
	proxy.mu.Lock()
	certs := len(proxy.certs)
	proxy.mu.Unlock()
	if certs != 1 {
		t.Fatalf("proxy certs = %d, want 1", certs)
	}
	if r := router.routes(); len(r) != 1 || r[0] != "blog:shop.example.com" {
		t.Fatalf("router backfill = %v", r)
	}
}

// Direct-mode with a relay connected keeps the claim (both paths serve
// during the user's DNS flip): add before, confirm after activation.
func TestAppDomainDirectWithRelayStillClaims(t *testing.T) {
	m, st, _, _ := newDirectAppManager(t, &fakeIssuer{}, &fakeIssuer{})
	relay := &fakeNotifier{}
	m.SetRelay(relay)
	if _, err := st.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddAppDomain("blog", "shop.example.com"); err != nil {
		t.Fatal(err)
	}
	waitAppStatus(t, st, "shop.example.com", StatusActive)
	pushes := relay.pushes()
	if len(pushes) < 2 || pushes[0] != "add:shop.example.com" ||
		pushes[len(pushes)-1] != "confirm:shop.example.com" {
		t.Fatalf("relay ops = %v, want add first and trailing confirm", pushes)
	}
}

// A connected relay whose claim fails must fail the attempt — the claim is
// load-bearing for the migration window, exactly the box-wide posture.
func TestAppDomainDirectClaimFailureFailsTheAttempt(t *testing.T) {
	m, st, _, _ := newDirectAppManager(t, &fakeIssuer{}, &fakeIssuer{})
	m.SetRelay(&fakeNotifier{failAdd: errors.New("relay says no")})
	if _, err := st.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddAppDomain("blog", "shop.example.com"); err != nil {
		t.Fatal(err)
	}
	row := waitAppStatus(t, st, "shop.example.com", StatusFailed)
	if !strings.Contains(row.Error, "relay says no") {
		t.Fatalf("failed row error = %q", row.Error)
	}
}

// A direct-mode Obtain failure must name the token source in the failed
// row: the likeliest cause is the box-wide token not covering this domain's
// zone (a different zone than the box-wide domain), which the raw provider
// error does not say in Piper's terms.
func TestAppDomainDirectObtainFailureNamesTheTokenSource(t *testing.T) {
	dnsIss := &fakeIssuer{failures: 1 << 30} // every Obtain fails
	m, st, _, _ := newDirectAppManager(t, dnsIss, &fakeIssuer{})
	if _, err := st.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddAppDomain("blog", "shop.example.com"); err != nil {
		t.Fatal(err)
	}
	row := waitAppStatus(t, st, "shop.example.com", StatusFailed)
	if !strings.Contains(row.Error, "box-wide DNS token") || !strings.Contains(row.Error, "acme: boom") {
		t.Fatalf("failed row error = %q, want the token-source guidance wrapping the provider error", row.Error)
	}
}

// Renewal follows the mode current at renew time, both directions: a domain
// activated under direct renews via ALPN after a flip to relay, and vice
// versa. reissueApp is the single reissue path renewApp drives.
func TestAppDomainRenewalFollowsFlippedMode(t *testing.T) {
	dnsIss := &fakeIssuer{}
	alpnIss := &fakeIssuer{}
	m, st, _, _ := newDirectAppManager(t, dnsIss, alpnIss)
	if _, err := st.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddAppDomain("blog", "shop.example.com"); err != nil {
		t.Fatal(err)
	}
	waitAppStatus(t, st, "shop.example.com", StatusActive) // via dnsIss (call 1)

	// Flip the box to relay: the next reissue must use the ALPN issuer.
	if err := st.SetDomainConfig("box.example.com", "cloudflare", "tok", ServeRelay); err != nil {
		t.Fatal(err)
	}
	row, err := st.GetAppDomain("shop.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.reissueApp(row); err != nil {
		t.Fatalf("reissueApp after flip to relay: %v", err)
	}
	alpnIss.mu.Lock()
	alpnCalls := alpnIss.calls
	alpnIss.mu.Unlock()
	if alpnCalls != 1 {
		t.Fatalf("alpn obtain calls after relay flip = %d, want 1", alpnCalls)
	}

	// And back: direct again renews via DNS-01.
	if err := st.SetDomainConfig("box.example.com", "cloudflare", "tok", ServeDirect); err != nil {
		t.Fatal(err)
	}
	if err := m.reissueApp(row); err != nil {
		t.Fatalf("reissueApp after flip to direct: %v", err)
	}
	dnsIss.mu.Lock()
	dnsCalls := dnsIss.calls
	dnsIss.mu.Unlock()
	if dnsCalls != 2 {
		t.Fatalf("dns obtain calls after direct flip = %d, want 2 (activation + renewal)", dnsCalls)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/domain/ -run 'TestAppDomainDirect|TestAppDomainRenewalFollows' -v`
Expected: FAIL — `TestAppDomainDirectIssuesViaDNS01` parks (direct add reaches the relay-mode path and fails "relay not connected"; `waitAppStatus` times out on `active`), before that likely compile is fine since no new symbols are referenced by the tests themselves. If the run takes the full `waitCeiling`, that is the expected failure signature.

- [ ] **Step 3: Implement**

In `internal/domain/domain.go`, next to `boxServe`:

```go
// appDNSIssuer builds the DNS-01 issuer a direct box issues per-app domains
// with — the box-wide token source: the env box's issuer, or the domain
// config row's provider+token through the box-wide factory. ErrNoDNSIssuer
// when the box has neither (AddAppDomain guards this, but issuance re-checks:
// the config can be removed after the row lands).
func (m *Manager) appDNSIssuer() (Issuer, error) {
	if m.envDomain != "" {
		if m.envIssuer == nil {
			return nil, ErrNoDNSIssuer
		}
		return m.envIssuer()
	}
	dc, err := m.st.GetDomainConfig()
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNoDNSIssuer
	}
	if err != nil {
		return nil, err
	}
	if dc.DNSToken == "" {
		return nil, ErrNoDNSIssuer
	}
	return m.newIssuer(dc.DNSProvider, dc.DNSToken)
}

// appIssuerFor picks the per-app issuer for the serve mode: DNS-01 with the
// box-wide token on a direct box, TLS-ALPN-01 through the relay splice
// otherwise (#506).
func (m *Manager) appIssuerFor(serve string) (Issuer, error) {
	if serve == ServeDirect {
		return m.appDNSIssuer()
	}
	return m.appIssuer()
}
```

In `internal/domain/appdomain.go`, `appIssueOnce`: replace the block from `r := m.notifier()` through the `errWaitDNS` return (currently):

```go
	r := m.notifier()
	if r == nil {
		return errors.New("relay not connected")
	}
	if err := r.AddCustomDomain(row.Domain); err != nil {
		return err
	}
	if !m.dnsPointsAt(m.dnsTarget, row.Domain) {
		return fmt.Errorf("%w: point a CNAME or A record for %s at %s", errWaitDNS, row.Domain, m.dnsTarget)
	}
```

with:

```go
	serve := m.boxServe()
	r := m.notifier()
	if serve == ServeDirect {
		// Direct: the relay claim is optional — kept when connected so both
		// paths serve identically during the user's DNS flip (the TTL-window
		// migration property), skipped on a never-enrolled box. No DNS gate:
		// DNS-01 does not depend on where visitor DNS points, so issue
		// immediately, like the box-wide instance.
		if r != nil {
			if err := r.AddCustomDomain(row.Domain); err != nil {
				return err
			}
		}
	} else {
		if r == nil {
			return errors.New("relay not connected")
		}
		if err := r.AddCustomDomain(row.Domain); err != nil {
			return err
		}
		if !m.dnsPointsAt(m.dnsTarget, row.Domain) {
			return fmt.Errorf("%w: point a CNAME or A record for %s at %s", errWaitDNS, row.Domain, m.dnsTarget)
		}
	}
```

Further down in `appIssueOnce`, replace `iss, err := m.appIssuer()` with:

```go
		iss, err := m.appIssuerFor(serve)
```

In the same disk-cert-reuse block, wrap the direct-mode Obtain error so the failed row names the token source (the likeliest failure is the box-wide token not covering this domain's zone). Replace:

```go
		certPEM, keyPEM, err = iss.Obtain([]string{row.Domain})
		if err != nil {
			return err
		}
```

with:

```go
		certPEM, keyPEM, err = iss.Obtain([]string{row.Domain})
		if err != nil {
			if serve == ServeDirect {
				return fmt.Errorf("dns-01 obtain (the box-wide DNS token must cover %s's zone): %w", row.Domain, err)
			}
			return err
		}
```

and replace the unconditional confirm:

```go
	if err := r.ConfirmCustomDomain(row.Domain); err != nil {
		return err
	}
```

with (relay mode always has `r != nil` by the guard above, so this only relaxes direct):

```go
	if r != nil {
		if err := r.ConfirmCustomDomain(row.Domain); err != nil {
			return err
		}
	}
```

In `reissueApp`, replace `iss, err := m.appIssuer()` with:

```go
	iss, err := m.appIssuerFor(m.boxServe())
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/domain/ -run 'TestAppDomainDirect|TestAppDomainRenewalFollows' -v`
Expected: PASS.

- [ ] **Step 5: Run the whole package — relay-mode behavior must be pinned unchanged**

Run: `go test ./internal/domain/`
Expected: PASS — in particular `TestAddAppDomainIssuesAndActivates` (claim-before-obtain ordering), `TestAppDomainWaitsForDNS` (relay DNS gate), and `TestAppIssuanceWithoutARelayStopsBeforeTheIssuer` (relay-mode "relay not connected") all still green.

- [ ] **Step 6: Commit**

```bash
git add internal/domain/domain.go internal/domain/appdomain.go internal/domain/appdomain_test.go
git commit -m "feat(domain): direct-mode per-app issuance via DNS-01 with the box-wide token

Part of #506

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 3: Direct-mode status reporting

`appDomainStatus` reports `A <domain> → box public IP` and an IP-based `dns_ok` on a direct box; unknown IP gets the box-wide "no public IP" note.

**Files:**
- Modify: `internal/domain/appdomain.go` (`AppDomainStatus` struct ~line 354, `appDomainStatus` ~line 392)
- Test: `internal/domain/appdomain_test.go`

**Interfaces:**
- Consumes: Task 1's `boxServe()`; existing `m.publicIP`, `m.cachedDNSResolvesTo(host, ip)`, `noteNoPublicIP`, `DNSRecord`.
- Produces: `AppDomainStatus.Note string` (JSON `note,omitempty`) — the API/TUI/CLI already render statuses generically; no consumer change needed.

- [ ] **Step 1: Write the failing tests**

Append to `internal/domain/appdomain_test.go`:

```go
// Direct-mode status guides an A record at the box's public IP and answers
// dns_ok against that IP; an unknown IP degrades to an empty-value record
// plus the box-wide explanatory note. Relay-mode status (CNAME at the relay)
// is pinned by TestAppDomainStatuses.
func TestAppDomainStatusDirect(t *testing.T) {
	m, st, _, _ := newDirectAppManager(t, &fakeIssuer{}, &fakeIssuer{})
	if _, err := st.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddAppDomain("blog", "shop.example.com"); err != nil {
		t.Fatal(err)
	}
	waitAppStatus(t, st, "shop.example.com", StatusActive)

	// Public IP unknown: empty-value record, note, dns_ok false.
	stt, err := m.AppDomainStatus("shop.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(stt.DNSRecords) != 1 || stt.DNSRecords[0].Type != "A" ||
		stt.DNSRecords[0].Name != "shop.example.com" || stt.DNSRecords[0].Value != "" {
		t.Fatalf("records (no IP) = %+v", stt.DNSRecords)
	}
	if stt.Note == "" || stt.DNSOK {
		t.Fatalf("no-IP status: note=%q dns_ok=%v, want note and false", stt.Note, stt.DNSOK)
	}

	// Known public IP that the domain resolves to: full record, dns_ok true.
	m.publicIP = func() string { return "203.0.113.7" } // resolve fake returns this IP
	stt, err = m.AppDomainStatus("shop.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(stt.DNSRecords) != 1 || stt.DNSRecords[0].Type != "A" ||
		stt.DNSRecords[0].Value != "203.0.113.7" {
		t.Fatalf("records (with IP) = %+v", stt.DNSRecords)
	}
	if stt.Note != "" || !stt.DNSOK {
		t.Fatalf("with-IP status: note=%q dns_ok=%v, want no note and true", stt.Note, stt.DNSOK)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/domain/ -run TestAppDomainStatusDirect -v`
Expected: compile FAIL — `stt.Note undefined` (or, once compiled, CNAME records instead of A).

- [ ] **Step 3: Implement**

In `internal/domain/appdomain.go`:

(a) Add to `AppDomainStatus` (after `Error`):

```go
	// Note is a human-readable hint when guidance is incomplete (direct mode
	// before the box knows its public IP). Not an error.
	Note string `json:"note,omitempty"`
```

(b) Replace `appDomainStatus` with:

```go
// appDomainStatus converts one row: the guided-setup record (CNAME at the
// relay, or in direct mode an A/AAAA at the box's public IP) and a
// best-effort, cached dns_ok — a field, never an error.
func (m *Manager) appDomainStatus(row store.AppDomain) AppDomainStatus {
	st := AppDomainStatus{
		Domain: row.Domain, App: row.App,
		Status: row.Status, Error: row.Error,
	}
	if m.boxServe() == ServeDirect {
		ip := m.publicIP()
		typ := "A"
		if p := net.ParseIP(ip); p != nil && p.To4() == nil {
			typ = "AAAA"
		}
		st.DNSRecords = []DNSRecord{{Type: typ, Name: row.Domain, Value: ip}}
		if ip == "" {
			st.Note = noteNoPublicIP
		} else {
			st.DNSOK = m.cachedDNSResolvesTo(row.Domain, ip)
		}
	} else {
		st.DNSRecords = []DNSRecord{{Type: "CNAME", Name: row.Domain, Value: m.dnsTarget}}
		st.DNSOK = m.cachedDNSPointsAt(row.Domain)
	}
	if !row.CertNotAfter.IsZero() {
		t := row.CertNotAfter
		st.CertNotAfter = &t
	}
	return st
}
```

Add `"net"` to the file's imports if not already present.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/domain/`
Expected: PASS (including the relay-mode status pins `TestAppDomainStatuses` / `TestAppDomainStatusSingle`).

- [ ] **Step 5: Commit**

```bash
git add internal/domain/appdomain.go internal/domain/appdomain_test.go
git commit -m "feat(domain): direct-mode DNS guidance and dns_ok for per-app domains

Part of #506

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 4: API + piperd wiring — drop the #509 stopgap, wire the env issuer

The synchronous `AppDomainsActivatable` refusal existed only because #506 wasn't built; the manager now answers at add time with `ErrNoDNSIssuer`. Delete the interface method, the POST gate, and the `appDomainCapability` wrapper; map `ErrNoDNSIssuer` → 409; wire `Options.EnvIssuer` from `newEnvIssuer`.

**Files:**
- Modify: `internal/api/api.go` (interface ~line 95, POST handler ~line 625)
- Modify: `cmd/piperd/main.go` (`newDomainOptions` ~line 356, `appDomainCapability` ~line 416, wiring ~line 707)
- Test: `internal/api/api_test.go`, `cmd/piperd/main_test.go`

**Interfaces:**
- Consumes: Task 1's `domain.ErrNoDNSIssuer`, `domain.Options.EnvIssuer`; existing `newEnvIssuer(cfg config.Config) (domain.Issuer, error)`.
- Produces: `api.DomainManager` no longer has `AppDomainsActivatable()`; `*domain.Manager` satisfies it directly.

- [ ] **Step 1: Write the failing API test**

In `internal/api/api_test.go`, replace `TestAppDomainsPostConflictWhenTheBoxCanNeverActivateIt` (~line 1361) with:

```go
// A per-app domain POST on a direct box with no DNS-01 source is refused by
// the manager at add time (#506); the API maps ErrNoDNSIssuer to 409 naming
// the missing token, the way ErrBoxWideDomain conflicts do. A capable box
// keeps its 201 (pinned by TestAppDomainsEndpoints).
func TestAppDomainsPostConflictWithoutDNSIssuer(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	fdm := &fakeDomainManager{addErr: domain.ErrNoDNSIssuer}
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, fdm, nil, nil, nil, AgentInfo{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps/blog/domains",
		strings.NewReader(`{"domain":"myshop.com"}`)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("POST = %d, want 409 when the box has no DNS-01 source", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "DNS token") {
		t.Errorf("409 body = %q, want it to name the missing DNS token", strings.TrimSpace(body))
	}
}
```

Also delete from `fakeDomainManager`: the `appDomainsBlocked` field (~line 1031) and the `AppDomainsActivatable` method (~line 1077).

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/api/ -run TestAppDomainsPostConflictWithoutDNSIssuer -v`
Expected: FAIL — 500 instead of 409 (`ErrNoDNSIssuer` falls into the `err != nil` server-error arm; the compile still passes because the interface method is only removed in Step 3).

- [ ] **Step 3: Implement**

In `internal/api/api.go`:

(a) Remove the `AppDomainsActivatable() bool` method and its comment from the `DomainManager` interface.

(b) In the POST handler, delete the gate:

```go
		if !dom.AppDomainsActivatable() {
			http.Error(w, "per-app custom domains cannot activate on this box: their TLS-ALPN-01 challenge only arrives spliced down a relay tunnel this box does not have (#506 lifts this)", http.StatusConflict)
			return
		}
```

(c) In the same handler's error switch, extend the conflict arm:

```go
		case errors.Is(err, domain.ErrBoxWideDomain), errors.Is(err, store.ErrDomainExists),
			errors.Is(err, domain.ErrNoDNSIssuer):
			http.Error(w, err.Error(), http.StatusConflict)
			return
```

In `cmd/piperd/main.go`:

(d) Delete the `appDomainCapability` type and its method (~lines 416–425), and replace the wiring (~line 707):

```go
	if domMgr != nil {
		dm = appDomainCapability{Manager: domMgr, activatable: cfg.RelayAddr != ""}
	}
```

with:

```go
	if domMgr != nil {
		dm = domMgr
	}
```

(e) In `newDomainOptions`, extend the non-terminated env block (~line 401):

```go
	if !cfg.Terminated {
		opts.EnvDomain = cfg.BaseDomain // env-managed BYO: API writes are 409
		opts.EnvServe = serveMode(cfg.Serve)
		// The env box's DNS-01 source, doubling as the per-app token source
		// on a direct box (#506). Static-cert boxes (PIPER_TLS_CERT_FILE)
		// have no ACME path and stay nil — per-app adds are refused there.
		if cfg.TLSCertFile == "" {
			opts.EnvIssuer = func() (domain.Issuer, error) { return newEnvIssuer(cfg) }
		}
	}
```

(f) Update the stale comment on the `AppIssuer` nil-solver guard (~line 379): it currently says "#506 is what lifts this" / "The add path never gets here — appIssueOnce refuses at the relay notifier first". Replace that comment paragraph with:

```go
			// No solver means no relay to splice acme-tls/1 down. Since #506
			// a direct box never asks for this issuer (appIssuerFor picks
			// DNS-01), so this guard is a relay-mode box whose solver went
			// missing — kept because certs.New would otherwise reject the
			// typed-nil solver as "exactly one of DNSProvider or ALPNSolver
			// must be set" (#242), naming neither relay nor domain.
```

(g) In `cmd/piperd/main_test.go`, update `TestAppIssuerRefusesWithoutAnALPNSolver`'s comment (~line 1230) — the behavior it pins is unchanged (the ALPN factory still refuses without a solver); reword the first sentence to say the factory is now only reachable in relay mode:

```go
// The ALPN issuer factory refuses without a solver. Since #506 a direct box
// never asks for it (appIssuerFor picks DNS-01), so this is a relay-mode box
// whose solver went missing — the guard must still name the relay splice
// rather than certs.New's "exactly one of DNSProvider or ALPNSolver must be
// set" (#242), which names neither the relay nor the domain.
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/api/ ./cmd/piperd/`
Expected: PASS — including `TestAppDomainsEndpoints` (201 path) and `TestAppDomainsWithoutRelay` (nil-manager 409, untouched).

- [ ] **Step 5: Build everything**

Run: `go build ./... && go vet ./...`
Expected: clean — proves no other package referenced `AppDomainsActivatable`.

- [ ] **Step 6: Commit**

```bash
git add internal/api/api.go internal/api/api_test.go cmd/piperd/main.go cmd/piperd/main_test.go
git commit -m "feat(api,piperd): direct boxes accept per-app domains; drop the #509 activatable stopgap

Part of #506

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 5: E2e — per-app domain on a never-enrolled direct box, PROGRESS, verify

Extend the never-enrolled direct e2e: add a per-app domain over the API, watch it activate with no relay anywhere, and assert the box's own `:443` presents the exact-host cert. Then the repo bookkeeping and the full gate.

**Files:**
- Modify: `test/e2e/direct_test.go` (`TestNeverEnrolledDirectServesHTTPS`, end of function ~line 354)
- Modify: `PROGRESS.md` (the per-app custom domains line)

**Interfaces:**
- Consumes: the running `piperd` from the test (env box: `PIPER_BASE_DOMAIN=direct.localhost`, `PIPER_SERVE=direct`, `PIPER_TEST_ISSUER=selfsigned`, no relay), `apiToken` already created in the test; Task 4's env-issuer wiring (the selfsigned test issuer arrives via `newEnvIssuer`'s `PIPER_TEST_ISSUER` short-circuit).

- [ ] **Step 1: Extend the e2e (failing without the feature)**

Append at the end of `TestNeverEnrolledDirectServesHTTPS` (after the `"Scheme":"https"` assertion):

```go
	// Per-app custom domain on a never-enrolled box (#506): DNS-01 via the
	// box-wide token source (the selfsigned test issuer here), no relay
	// claim anywhere, exact-host cert on the box's own listener. Before
	// #506 this POST was refused with 409 "cannot activate on this box".
	addDom, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:8188/v1/apps/blog/domains",
		strings.NewReader(`{"domain":"shop.localhost"}`))
	addDom.Header.Set("Authorization", "Bearer "+apiToken)
	resp, err = http.DefaultClient.Do(addDom)
	if err != nil {
		t.Fatalf("POST /v1/apps/blog/domains: %v", err)
	}
	db, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/apps/blog/domains = %d: %s", resp.StatusCode, db)
	}

	statusReq, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:8188/v1/apps/blog/domains", nil)
	statusReq.Header.Set("Authorization", "Bearer "+apiToken)
	deadline = time.Now().Add(30 * time.Second)
	for {
		r, err := http.DefaultClient.Do(statusReq)
		if err == nil {
			sb, _ := io.ReadAll(r.Body)
			r.Body.Close()
			if strings.Contains(string(sb), `"status":"active"`) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("shop.localhost never activated: %s", sb)
			}
		} else if time.Now().After(deadline) {
			t.Fatalf("GET /v1/apps/blog/domains: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}

	appDialer := &tls.Dialer{Config: &tls.Config{
		ServerName: "shop.localhost", InsecureSkipVerify: true,
	}}
	appConn, err := appDialer.DialContext(ctx, "tcp", "127.0.0.1:8543")
	if err != nil {
		t.Fatalf("TLS dial for the per-app domain: %v", err)
	}
	defer appConn.Close()
	appState := appConn.(*tls.Conn).ConnectionState()
	if len(appState.PeerCertificates) == 0 {
		t.Fatal("no certificate presented for the per-app domain")
	}
	if err := appState.PeerCertificates[0].VerifyHostname("shop.localhost"); err != nil {
		t.Fatalf("per-app cert does not cover shop.localhost: %v (SANs %v)",
			err, appState.PeerCertificates[0].DNSNames)
	}
```

- [ ] **Step 2: Compile-check the e2e (it skips without RUN_E2E)**

Run: `go vet ./test/e2e/ && go test ./test/e2e/ -run TestNeverEnrolledDirectServesHTTPS`
Expected: vet clean; test SKIP (needs `RUN_E2E=1`).

- [ ] **Step 3: Run the e2e for real if the box allows**

Run: `RUN_E2E=1 go test ./test/e2e/ -run TestNeverEnrolledDirectServesHTTPS -v -timeout 300s`
Expected: PASS. **Caution (machine-local):** e2e clashes with a brew-service `piperd` holding `:8088`/`:2019` and with stray Caddy processes on `:80`/`:2019` — if the run fails on ports, report it and ask before stopping any service; do not `brew services stop` on your own. If Docker or the environment blocks the run entirely, say so explicitly in the task report rather than claiming it passed.

- [ ] **Step 4: Update PROGRESS.md**

In `PROGRESS.md`, directly below the direct-serve line (line ~70: `- ✅ Direct serve mode: box-wide custom domain … [#505]`), add one line in the same style:

```markdown
  - ✅ Direct-served per-app custom domains — DNS-01 via the box-wide token source, no relay splice needed — [#506](https://github.com/piperbox/piper/issues/506)
```

- [ ] **Step 5: Full gate**

Run: `make verify`
Expected: exit 0 (judge by exit status, not by grepping output — it halts at the first failing gate).

- [ ] **Step 6: Commit**

```bash
git add test/e2e/direct_test.go PROGRESS.md
git commit -m "test(e2e): per-app custom domain activates and serves on a never-enrolled direct box

Part of #506

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

## Completion

All five tasks done ⇒ the branch implements the spec. Open the PR into `main` with `gh pr create --base main`, body carrying `Closes #506`, and squash-merge per the repo workflow (the executing skill's finishing flow handles this).
