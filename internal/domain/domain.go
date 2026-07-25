// Package domain manages the box's BYO custom domains — one lifecycle per
// domain: relay claim, cert issuance via ACME (DNS-01 wildcard for the
// box-wide domain, TLS-ALPN-01 exact-host for per-app domains), live
// activation in Caddy, renewal, and teardown. It orchestrates
// certs/caddy/deploy/tunnel through interfaces (the deploy pattern) so it
// unit-tests with fakes. See
// docs/superpowers/specs/2026-07-10-domain-config-api-design.md and the
// per-app epic (#224).
package domain

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/piperbox/piper/internal/agent"
	"github.com/piperbox/piper/internal/caddy"
	"github.com/piperbox/piper/internal/certs"
	"github.com/piperbox/piper/internal/store"
)

const (
	StatusPending = "pending"
	StatusIssuing = "issuing"
	StatusActive  = "active"
	StatusFailed  = "failed"
)

// boxWideKey is the box-wide domain's slot in the keyed maps (loop generations,
// loaded certs). "*" cannot collide with a stored domain (domainRE rejects it)
// and reads as what the instance is: the wildcard-shaped one.
const boxWideKey = "*"

var (
	ErrEnvManaged          = errors.New("domain config is env-managed (PIPER_BASE_DOMAIN); unset it to manage via the API")
	ErrInvalidDomain       = errors.New("invalid domain")
	ErrUnsupportedProvider = errors.New("unsupported dns provider")
	ErrTokenRequired       = errors.New("dns_token required")
)

// errStaleConfig aborts an issuance/renewal run whose config snapshot no
// longer matches the store (replaced or removed while the run was pending).
// The run must exit without side effects; the teardown/replacement wins.
var errStaleConfig = errors.New("domain config changed; aborting stale run")

// Issuer obtains one PEM cert/key covering domains. *certs.Manager satisfies it.
type Issuer interface {
	Obtain(domains []string) (certPEM, keyPEM []byte, err error)
}

// IssuerFactory builds an Issuer for one issuance run from the stored DNS
// creds. Construction registers the ACME account, so it is deferred to
// issuance time rather than Manager construction.
type IssuerFactory func(provider, token string) (Issuer, error)

// Proxy is the caddy slice the Manager drives. *caddy.Client satisfies it.
type Proxy interface {
	EnsureHTTPS(listen string) error
	ReplaceCerts(pairs []caddy.CertPair) error
	UpsertRouteTLS(host string, upstreamHostPort int) error
	RemoveRoute(host string) error
}

// RelayNotifier drives the relay's per-domain claim lifecycle over the tunnel
// (#227): add-domain claims a routable pending mapping, domain-active confirms
// it durable, remove-domain drops it. *agent.TunnelClient satisfies it.
type RelayNotifier interface {
	AddCustomDomain(domain string) error
	ConfirmCustomDomain(domain string) error
	RemoveCustomDomain(domain string) error
}

// AppRouter is the deploy slice the Manager calls at per-app domain activation
// to backfill the exact-host route for an already-running app (#230); deploys
// otherwise own routes. *deploy.Deployer satisfies it.
type AppRouter interface {
	RouteAppDomain(appName, domain string) error
}

// Options wires a Manager. EnvDomain non-empty means domain config is
// env-managed (the pre-#102 PIPER_BASE_DOMAIN BYO path): API writes are
// rejected and Status reports source "env".
type Options struct {
	Store       *store.Store
	Issuer      IssuerFactory
	AppIssuer   func() (Issuer, error) // TLS-ALPN-01 issuer for per-app domains
	Proxy       Proxy
	Router      AppRouter // per-app route backfill; nil tolerated (tests)
	DataDir     string
	RelayHost   string // host part of the relay address; the DNS-record target
	HTTPSListen string // e.g. ":443"
	EnvDomain   string
	// Resolve overrides the DNS lookup behind dns_ok and the per-app DNS
	// gate; nil uses net.DefaultResolver. E2E seam: the test domains have no
	// real DNS (see PIPER_TEST_ISSUER in cmd/piperd).
	Resolve func(ctx context.Context, host string) ([]net.IP, error)
}

// Manager owns the custom-domain lifecycles — one state machine per domain,
// keyed by domain, each persisted in the store so the dashboard can poll it and
// restarts resume it. The box-wide BYO domain (#102) is the one wildcard-shaped
// instance (DNS-01, token, routes for all apps, domain_config row); per-app BYO
// domains (#224) are exact-host instances (TLS-ALPN-01, tokenless, one route,
// app_domains rows).
type Manager struct {
	st          *store.Store
	newIssuer   IssuerFactory
	appIssuer   func() (Issuer, error)
	proxy       Proxy
	router      AppRouter
	dataDir     string
	relayHost   string
	httpsListen string
	envDomain   string

	relayMu sync.Mutex
	relay   RelayNotifier

	issueMu sync.Mutex // serializes the box-wide instance's issuance/renewal runs

	appMuMu sync.Mutex             // guards appMu
	appMu   map[string]*sync.Mutex // per-app-domain run locks: one domain's ACME never blocks another's

	genMu sync.Mutex     // guards gens
	gens  map[string]int // per-domain loop generations; a loop exits when superseded

	stopCh   chan struct{}  // closed by Close: loops exit at their next checkpoint
	stopOnce sync.Once      // makes Close idempotent
	wg       sync.WaitGroup // joins every spawned lifecycle goroutine on Close

	certMu sync.Mutex                // guards loaded and serializes cert syncs to Caddy
	loaded map[string]caddy.CertPair // certs currently loaded, keyed like gens

	dnsMu    sync.Mutex // guards dnsCache
	dnsCache map[string]dnsCacheEntry

	envMu       sync.Mutex // guards the env-managed status fields below
	envStatus   string     // "" | issuing | active | failed (env mode only)
	envError    string
	envNotAfter time.Time

	// test seams
	retryDelay func(attempt int) time.Duration
	dnsWait    time.Duration // pending poll while waiting for the user's DNS
	resolve    func(ctx context.Context, host string) ([]net.IP, error)
	now        func() time.Time
	// genBumpHook runs at the top of nextGenFor, before the bump: a test-only
	// seam for scripting interleavings that must pause a caller between its
	// config read and its generation bump.
	genBumpHook func()
}

// dnsCacheEntry memoizes one host's dns-points-at-relay result so a polling
// dashboard doesn't issue live DNS lookups on every status read (#114).
type dnsCacheEntry struct {
	ok bool
	at time.Time
}

// dnsOKTTL bounds how long a cached dnsOK result is served before a re-lookup.
const dnsOKTTL = 5 * time.Second

func New(o Options) *Manager {
	envStatus := ""
	if o.EnvDomain != "" {
		envStatus = StatusIssuing // pending until RunEnv reports its outcome
	}
	resolve := o.Resolve
	if resolve == nil {
		resolve = func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		}
	}
	return &Manager{
		st: o.Store, newIssuer: o.Issuer, appIssuer: o.AppIssuer,
		proxy: o.Proxy, router: o.Router,
		dataDir: o.DataDir, relayHost: o.RelayHost,
		httpsListen: o.HTTPSListen, envDomain: o.EnvDomain,
		appMu:      map[string]*sync.Mutex{},
		gens:       map[string]int{},
		loaded:     map[string]caddy.CertPair{},
		dnsCache:   map[string]dnsCacheEntry{},
		stopCh:     make(chan struct{}),
		envStatus:  envStatus,
		retryDelay: defaultRetryDelay,
		dnsWait:    defaultDNSWait,
		resolve:    resolve,
		now:        time.Now,
	}
}

// nextGenFor bumps key's loop generation and returns it; the goroutine started
// with this value drives issuance until a later start for the same key
// supersedes it.
func (m *Manager) nextGenFor(key string) int {
	if m.genBumpHook != nil {
		m.genBumpHook()
	}
	m.genMu.Lock()
	defer m.genMu.Unlock()
	m.gens[key]++
	return m.gens[key]
}

func (m *Manager) currentGenFor(key string) int {
	m.genMu.Lock()
	defer m.genMu.Unlock()
	return m.gens[key]
}

// nextGen / currentGen are the box-wide instance's generation slot.
func (m *Manager) nextGen() int    { return m.nextGenFor(boxWideKey) }
func (m *Manager) currentGen() int { return m.currentGenFor(boxWideKey) }

// appLock returns the named domain's run lock, creating it on first use.
func (m *Manager) appLock(domain string) *sync.Mutex {
	m.appMuMu.Lock()
	defer m.appMuMu.Unlock()
	if m.appMu[domain] == nil {
		m.appMu[domain] = &sync.Mutex{}
	}
	return m.appMu[domain]
}

// Close stops and joins the lifecycle goroutines the manager tracks in its
// WaitGroup — the box-wide issue loop, per-app domain loops, and relay
// re-asserts — and blocks until they return. It does not join runEnvRenew:
// that goroutine is context-owned (it stops via its own ctx) and writes
// nothing under the data dir, so it plays no part in the TempDir race. Loops
// exit at their next checkpoint (loop top or backoff sleep); an in-flight ACME
// run finishes first, so Close can block for an Obtain the same way
// RemoveAppDomain's run lock can.
//
// Close is a shutdown join, not safe to call concurrently with a live
// AddAppDomain/Set/Resume/OnRelayConnect spawn — that races a WaitGroup Add
// against Wait — so callers must ensure no lifecycle is being started
// concurrently. Tests Close before t.TempDir teardown so a loop still writing
// its cert dir can't race the RemoveAll (#279).
func (m *Manager) Close() {
	m.stopOnce.Do(func() { close(m.stopCh) })
	m.wg.Wait()
}

// spawn runs fn as a lifecycle goroutine joined by Close. The generation must
// be bumped by the caller before spawning — supersede semantics rely on the
// bump landing synchronously, ahead of the loop's first gen check.
func (m *Manager) spawn(fn func()) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		fn()
	}()
}

// stopped reports whether Close has been called.
func (m *Manager) stopped() bool {
	select {
	case <-m.stopCh:
		return true
	default:
		return false
	}
}

// sleepOrStopped waits out a loop backoff, reporting false if Close stopped
// the manager first — a stopped loop exits instead of sleeping through the
// join (production backoffs run to an hour).
func (m *Manager) sleepOrStopped(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-m.stopCh:
		return false
	case <-t.C:
		return true
	}
}

// setCert loads (or refreshes) key's cert in Caddy by re-syncing the complete
// load_pem set: with one wildcard cert and N exact-host certs live at once, a
// whole-set replace is the only shape that can never strand or duplicate an
// entry. certMu spans the sync so a concurrent change can't land a stale set.
func (m *Manager) setCert(key string, certPEM, keyPEM []byte) error {
	m.certMu.Lock()
	defer m.certMu.Unlock()
	m.loaded[key] = caddy.CertPair{CertPEM: string(certPEM), KeyPEM: string(keyPEM)}
	return m.proxy.ReplaceCerts(m.loadedPairsLocked())
}

// LoadStaticCert loads an operator-provided box-wide cert (piperd's
// TLSCertFile config) into the shared cert set. Routing it through the set —
// rather than appending straight to Caddy — keeps a later per-app domain sync
// from clobbering it.
func (m *Manager) LoadStaticCert(certPEM, keyPEM []byte) error {
	return m.setCert(boxWideKey, certPEM, keyPEM)
}

// unloadCert drops key's cert from Caddy. A key that was never loaded is a
// no-op — teardown paths run before activation too, when the tls app may not
// even exist yet.
func (m *Manager) unloadCert(key string) error {
	m.certMu.Lock()
	defer m.certMu.Unlock()
	if _, ok := m.loaded[key]; !ok {
		return nil
	}
	delete(m.loaded, key)
	return m.proxy.ReplaceCerts(m.loadedPairsLocked())
}

// loadedPairsLocked snapshots the cert set in stable (sorted-key) order.
// Caller holds certMu.
func (m *Manager) loadedPairsLocked() []caddy.CertPair {
	keys := make([]string, 0, len(m.loaded))
	for k := range m.loaded {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]caddy.CertPair, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, m.loaded[k])
	}
	return pairs
}

// SetRelay injects the tunnel client once relay mode is up (piperd creates the
// tunnel client after the Manager). Nil is tolerated: activation then skips
// the relay push (LAN/tests).
func (m *Manager) SetRelay(r RelayNotifier) {
	m.relayMu.Lock()
	m.relay = r
	m.relayMu.Unlock()
}

func (m *Manager) notifier() RelayNotifier {
	m.relayMu.Lock()
	defer m.relayMu.Unlock()
	return m.relay
}

// domainRE accepts lowercase dotted DNS names ("shop.example.com").
var domainRE = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+[a-z][a-z0-9-]*[a-z0-9]$`)

// Set validates and persists a new custom-domain config, then starts issuance
// asynchronously. The returned Status is the freshly-kicked "issuing" state.
func (m *Manager) Set(domainName, provider, token string) (Status, error) {
	if m.envDomain != "" {
		return Status{}, ErrEnvManaged
	}
	d := strings.ToLower(strings.TrimSpace(domainName))
	if !domainRE.MatchString(d) {
		return Status{}, ErrInvalidDomain
	}
	if provider != "cloudflare" {
		return Status{}, ErrUnsupportedProvider
	}
	if token == "" {
		return Status{}, ErrTokenRequired
	}
	// issueMu makes the replace atomic w.r.t. issuance: an in-flight run
	// either completes for the still-current old config (and is then torn
	// down here) or sees the new config and aborts. Acquiring it can block
	// for the duration of an in-flight ACME Obtain (minutes, bounded) — a
	// deliberate trade: correctness of the state machine over PUT latency.
	m.issueMu.Lock()
	// Replacing a different domain tears the old one down first.
	if prev, err := m.st.GetDomainConfig(); err == nil && prev.Domain != d {
		m.teardown(prev)
	}
	if err := m.st.SetDomainConfig(d, provider, token); err != nil {
		m.issueMu.Unlock()
		return Status{}, err
	}
	m.issueMu.Unlock()
	// Snapshot the just-written "issuing" state BEFORE spawning: Status()
	// re-reads the store, so reading it after the spawn races the loop to
	// activation and can hand the caller an "active" it never asked about
	// (#354). The config is already persisted, so this is the same read —
	// only ordered so no loop can have moved it on yet.
	st, err := m.Status()
	if err != nil {
		return Status{}, err
	}
	gen := m.nextGen()
	m.spawn(func() { m.issueLoop(d, gen) })
	return st, nil
}

// issueLoop drives one config to activation with capped-backoff retries. It
// exits when the stored config no longer matches domain (replaced or deleted),
// activation succeeds, Close stops the manager, or a newer Set/Resume
// supersedes this generation — the last guard stops a same-domain re-PUT from
// leaving a second loop retrying alongside this one (#112).
func (m *Manager) issueLoop(domain string, gen int) {
	for attempt := 0; ; attempt++ {
		if m.stopped() || m.currentGen() != gen {
			return
		}
		dc, err := m.st.GetDomainConfig()
		if err != nil || dc.Domain != domain || dc.Status == StatusActive {
			return
		}
		if err := m.issueOnce(dc); err == nil {
			return
		} else if errors.Is(err, errStaleConfig) {
			return // replaced or removed; the successor owns the state now
		} else if errors.Is(err, agent.ErrNotConnected) {
			// The tunnel is down — a wait, not an issuance failure: stay
			// "issuing" so a restart never reports "failed" just because the
			// relay wasn't connected yet (#166). The OnConnect kick drives the
			// immediate retry; the backoff below is only the fallback.
			_ = m.st.UpdateDomainStatus(dc.Domain, StatusIssuing, "waiting for relay connection", time.Time{})
		} else {
			_ = m.st.UpdateDomainStatus(dc.Domain, StatusFailed, err.Error(), time.Time{})
		}
		if !m.sleepOrStopped(m.retryDelay(attempt)) {
			return
		}
	}
}

// defaultDNSWait is how often a pending per-app domain re-checks that the
// user's DNS record points at the relay before attempting issuance. A flat
// poll, not a backoff: the wait is on the user, not on a failing dependency.
const defaultDNSWait = 15 * time.Second

// defaultRetryDelay backs off 5s, then 1m, 2m, 4m, … capped at ~1h. The short
// first retry keeps a restart-mid-issuance from flapping to "failed" for a full
// minute: the common cause is the tunnel not being connected yet at Resume, and
// the immediate re-attempt (cert already on disk) succeeds once it is (#113).
func defaultRetryDelay(attempt int) time.Duration {
	if attempt <= 0 {
		return 5 * time.Second
	}
	shift := attempt - 1
	if shift > 6 {
		shift = 6
	}
	return time.Minute << uint(shift)
}

// issueOnce claims the relay mapping, obtains (or reuses) the cert, arms
// Caddy, then confirms the claim — the epic's uniform per-domain ordering
// (#224). For this DNS-01 wildcard shape the early claim is not load-bearing
// the way it is for ALPN, but it lets the relay's FCFS reject a contested name
// before an ACME order is burned. The disk-cert reuse keeps retries and
// restarts inside LE rate limits: a relay hiccup must not burn a fresh
// certificate. snap is the caller's config snapshot; the run re-reads under
// issueMu and aborts with errStaleConfig if the stored domain has moved on
// (Set-replace/Remove won the race).
func (m *Manager) issueOnce(snap store.DomainConfig) error {
	m.issueMu.Lock()
	defer m.issueMu.Unlock()
	dc, err := m.st.GetDomainConfig()
	if err != nil || dc.Domain != snap.Domain {
		return errStaleConfig
	}
	// Nil notifier tolerated (LAN/tests): activation then skips the relay.
	r := m.notifier()
	if r != nil {
		if err := r.AddCustomDomain(dc.Domain); err != nil {
			return err
		}
	}
	certPEM, keyPEM, err := m.readCert()
	if err != nil || !certCovers(certPEM, dc.Domain, time.Now()) {
		iss, err := m.newIssuer(dc.DNSProvider, dc.DNSToken)
		if err != nil {
			return err
		}
		certPEM, keyPEM, err = iss.Obtain([]string{"*." + dc.Domain, dc.Domain})
		if err != nil {
			return err
		}
		// Defense in depth: teardown paths all hold issueMu today, so the
		// config cannot have changed during Obtain — but re-check before any
		// side effect in case a future path mutates it without the lock.
		if cur, err := m.st.GetDomainConfig(); err != nil || cur.Domain != dc.Domain {
			return errStaleConfig
		}
		if err := m.writeCert(certPEM, keyPEM); err != nil {
			return err
		}
	}
	if err := m.arm(dc, certPEM, keyPEM); err != nil {
		return err
	}
	if r != nil {
		if err := r.ConfirmCustomDomain(dc.Domain); err != nil {
			return err
		}
	}
	notAfter, err := certs.NotAfter(certPEM)
	if err != nil {
		return err
	}
	return m.st.UpdateDomainStatus(dc.Domain, StatusActive, "", notAfter)
}

// arm loads the cert and app routes into Caddy — the box must answer before
// the relay routes to it. Shared by first activation and restart resume.
func (m *Manager) arm(dc store.DomainConfig, certPEM, keyPEM []byte) error {
	if err := m.proxy.EnsureHTTPS(m.httpsListen); err != nil {
		return err
	}
	if err := m.setCert(boxWideKey, certPEM, keyPEM); err != nil {
		return err
	}
	apps, err := m.st.ListApps()
	if err != nil {
		return err
	}
	for _, a := range apps {
		dep, err := m.st.LatestRunning(a.Name)
		if err != nil {
			continue // never deployed or not running: nothing to route
		}
		if err := m.proxy.UpsertRouteTLS(a.Name+"."+dc.Domain, dep.HostPort); err != nil {
			return err
		}
	}
	return nil
}

// teardown reverses activation: relay claim first, then routes, then certs.
// The relay removal is best-effort here (Set-replace only): a missed removal
// leaves a stale claim on the relay that still points at this same agent —
// harmless routing-wise, bounded by the relay's per-agent domain cap. Remove,
// the user-facing teardown, requires the relay removal to succeed. Caller must
// hold issueMu.
func (m *Manager) teardown(dc store.DomainConfig) {
	if r := m.notifier(); r != nil {
		_ = r.RemoveCustomDomain(dc.Domain)
	}
	m.removeRoutesAndCert(dc)
}

// removeRoutesAndCert drops the domain's Caddy routes, its loaded cert, and
// the on-disk cert.
func (m *Manager) removeRoutesAndCert(dc store.DomainConfig) {
	if apps, err := m.st.ListApps(); err == nil {
		for _, a := range apps {
			_ = m.proxy.RemoveRoute(a.Name + "." + dc.Domain)
		}
	}
	_ = m.unloadCert(boxWideKey)
	_ = os.RemoveAll(filepath.Join(m.dataDir, "domain"))
}

func (m *Manager) certDir() string  { return filepath.Join(m.dataDir, "domain") }
func (m *Manager) certPath() string { return filepath.Join(m.certDir(), "cert.pem") }
func (m *Manager) keyPath() string  { return filepath.Join(m.certDir(), "key.pem") }

func (m *Manager) writeCert(certPEM, keyPEM []byte) error {
	if err := os.MkdirAll(m.certDir(), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(m.certPath(), certPEM, 0o600); err != nil {
		return err
	}
	return os.WriteFile(m.keyPath(), keyPEM, 0o600)
}

func (m *Manager) readCert() (certPEM, keyPEM []byte, err error) {
	certPEM, err = os.ReadFile(m.certPath())
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err = os.ReadFile(m.keyPath())
	if err != nil {
		return nil, nil, err
	}
	return certPEM, keyPEM, nil
}

// certCovers reports whether the leaf in certPEM is valid for hosts under
// domain and not expiring within 24h — the wildcard disk-cert reuse test.
func certCovers(certPEM []byte, domain string, now time.Time) bool {
	return certValidFor(certPEM, "piper-probe."+domain, now)
}

// certValidFor reports whether the leaf in certPEM is valid for host and not
// expiring within 24h.
func certValidFor(certPEM []byte, host string, now time.Time) bool {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return false
	}
	crt, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	if crt.VerifyHostname(host) != nil {
		return false
	}
	return now.Add(24 * time.Hour).Before(crt.NotAfter)
}

// Status assembles the wire state (GET /v1/domain).
func (m *Manager) Status() (Status, error) {
	if m.envDomain != "" {
		m.envMu.Lock()
		st := Status{
			Domain: m.envDomain, Source: "env",
			Status: m.envStatus, Error: m.envError,
			DNSRecords: m.dnsRecords(m.envDomain),
		}
		if !m.envNotAfter.IsZero() {
			t := m.envNotAfter
			st.CertNotAfter = &t
		}
		m.envMu.Unlock()
		return st, nil
	}
	dc, err := m.st.GetDomainConfig()
	if errors.Is(err, store.ErrNotFound) {
		return Status{Source: "api", DNSRecords: []DNSRecord{}}, nil
	}
	if err != nil {
		return Status{}, err
	}
	st := Status{
		Domain: dc.Domain, DNSProvider: dc.DNSProvider,
		DNSTokenSet: dc.DNSToken != "", Source: "api",
		Status: dc.Status, Error: dc.Error,
		DNSRecords: m.dnsRecords(dc.Domain),
	}
	// piper-probe.<domain>: any label matches the user's wildcard record, so a
	// hit means wildcard traffic reaches the relay — readiness independent of
	// issuance, which needs only the DNS API token.
	st.DNSOK = m.cachedDNSPointsAt("piper-probe." + dc.Domain)
	if !dc.CertNotAfter.IsZero() {
		t := dc.CertNotAfter
		st.CertNotAfter = &t
	}
	return st, nil
}

// DNSRecord is one record the user must create at their DNS host.
type DNSRecord struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Status is the wire state of the box's custom-domain config. The DNS token
// never appears; DNSTokenSet signals presence.
type Status struct {
	Domain       string      `json:"domain"`
	DNSProvider  string      `json:"dns_provider"`
	DNSTokenSet  bool        `json:"dns_token_set"`
	Source       string      `json:"source"` // "api" | "env"
	Status       string      `json:"status"` // "" | "issuing" | "active" | "failed"
	Error        string      `json:"error"`
	CertNotAfter *time.Time  `json:"cert_not_after,omitempty"`
	DNSRecords   []DNSRecord `json:"dns_records"`
	DNSOK        bool        `json:"dns_ok"`
}

func (m *Manager) dnsRecords(domain string) []DNSRecord {
	return []DNSRecord{
		{Type: "CNAME", Name: "*." + domain, Value: m.relayHost},
		{Type: "CNAME", Name: domain, Value: m.relayHost},
	}
}

// cachedDNSPointsAt serves dnsPointsAt(relayHost, host) from a short TTL
// cache so a dashboard polling the status endpoints doesn't trigger a pair of
// blocking DNS lookups every call. The lookup runs outside the lock (#114).
func (m *Manager) cachedDNSPointsAt(host string) bool {
	m.dnsMu.Lock()
	if e, ok := m.dnsCache[host]; ok && m.now().Sub(e.at) < dnsOKTTL {
		m.dnsMu.Unlock()
		return e.ok
	}
	m.dnsMu.Unlock()

	ok := m.dnsPointsAt(m.relayHost, host)

	m.dnsMu.Lock()
	m.dnsCache[host] = dnsCacheEntry{ok: ok, at: m.now()}
	m.dnsMu.Unlock()
	return ok
}

// dnsPointsAt reports whether host resolves to an address target also
// resolves to.
func (m *Manager) dnsPointsAt(target, host string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	probe, err := m.resolve(ctx, host)
	if err != nil {
		return false
	}
	relay, err := m.resolve(ctx, target)
	if err != nil {
		return false
	}
	for _, p := range probe {
		for _, r := range relay {
			if p.Equal(r) {
				return true
			}
		}
	}
	return false
}

// Remove tears down the custom domain. Shared-domain URLs are untouched.
// The relay clear must succeed before the row is deleted: once the config is
// gone nothing on the box would ever retry the clear, and the relay would
// splice the domain forever. A failed clear fails Remove with the config
// intact so the user can retry DELETE. Holding issueMu can block for an
// in-flight ACME Obtain (minutes, bounded) — see Set for the rationale.
func (m *Manager) Remove() error {
	if m.envDomain != "" {
		return ErrEnvManaged
	}
	m.issueMu.Lock()
	defer m.issueMu.Unlock()
	dc, err := m.st.GetDomainConfig()
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if r := m.notifier(); r != nil {
		if err := r.RemoveCustomDomain(dc.Domain); err != nil {
			return fmt.Errorf("clear relay domain mapping: %w", err)
		}
	}
	m.removeRoutesAndCert(dc)
	return m.st.DeleteDomainConfig()
}
