package domain

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/piperbox/piper/internal/certs"
	"github.com/piperbox/piper/internal/store"
)

// Per-app BYO domains (#224): each domain runs its own lifecycle instance —
// pending (relay claim + wait for the user's DNS) → issuing (TLS-ALPN-01
// obtain through the relay splice) → active (cert in Caddy, backfill route,
// confirm the claim durable) → renewal; failed retries with capped backoff.
// The relay claim precedes issuance because the ALPN challenge can only reach
// this box through the relay's routing — the inversion of the box-wide DNS-01
// ordering. One goroutine and one run lock per domain: a failing or slow
// domain never blocks another's lifecycle.

// ErrBoxWideDomain rejects a per-app domain equal to the box-wide custom
// domain. The relay would not catch this: add-domain against the agent's own
// active claim is a no-op success, so without the local check two lifecycles
// (the box-wide "*" instance and a per-app one) would manage the same name —
// two cert-set entries and clashing routes.
var ErrBoxWideDomain = errors.New("domain is already the box-wide custom domain")

// errWaitDNS marks a run that stopped because the user's DNS record does not
// point at the relay yet: the domain stays pending and the loop polls at
// dnsWait instead of backing off — the wait is on the user, and issuing before
// DNS points would only burn failed ACME validations.
var errWaitDNS = errors.New("waiting for dns")

// AddAppDomain registers domain for app and starts its lifecycle. The row is
// the state (#231 reads status from the store); the returned row is the
// freshly-persisted pending state.
func (m *Manager) AddAppDomain(app, domainName string) (store.AppDomain, error) {
	d := strings.ToLower(strings.TrimSpace(domainName))
	if !domainRE.MatchString(d) {
		return store.AppDomain{}, ErrInvalidDomain
	}
	if _, err := m.st.GetApp(app); err != nil {
		return store.AppDomain{}, err
	}
	if dc, err := m.st.GetDomainConfig(); err == nil && dc.Domain == d {
		return store.AppDomain{}, ErrBoxWideDomain
	}
	if m.boxServe() == ServeDirect && !m.hasAppDNSSource() {
		return store.AppDomain{}, ErrNoDNSIssuer
	}
	if err := m.st.AddAppDomain(d, app); err != nil {
		return store.AppDomain{}, err
	}
	if err := m.st.UpdateAppDomainStatus(d, StatusPending, "", time.Time{}); err != nil {
		return store.AppDomain{}, err
	}
	gen := m.nextGenFor(d)
	m.spawn(func() { m.appLoop(d, gen) })
	return m.st.GetAppDomain(d)
}

// appLoop drives one per-app domain to activation. It exits when the row is
// gone (removed), activation succeeds, Close stops the manager, or a newer
// loop for the same domain supersedes this generation. A DNS wait polls flat
// at dnsWait and keeps the row pending; real failures mark it failed and back
// off.
func (m *Manager) appLoop(domain string, gen int) {
	for attempt := 0; ; {
		if m.stopped() || m.currentGenFor(domain) != gen {
			return
		}
		row, err := m.st.GetAppDomain(domain)
		if err != nil || row.Status == StatusActive {
			return
		}
		err = m.appIssueOnce(row)
		switch {
		case err == nil:
			return
		case errors.Is(err, errStaleConfig):
			return // removed or re-owned; the successor owns the state now
		case errors.Is(err, errWaitDNS):
			if err := m.st.UpdateAppDomainStatus(domain, StatusPending, err.Error(), time.Time{}); err != nil {
				log.Printf("domain: status write for %s: %v", domain, err)
			}
			if !m.sleepOrStopped(m.dnsWait) {
				return
			}
		default:
			if err := m.st.UpdateAppDomainStatus(domain, StatusFailed, err.Error(), time.Time{}); err != nil {
				log.Printf("domain: status write for %s: %v", domain, err)
			}
			if !m.sleepOrStopped(m.retryDelay(attempt)) {
				return
			}
			attempt++
		}
	}
}

// appIssueOnce runs one activation attempt: claim the relay mapping, gate on
// the user's DNS, obtain (or reuse from disk) the exact-host cert, arm Caddy,
// confirm the claim, persist active. Re-adding an existing claim is a relay
// no-op for active rows and a TTL refresh for pending ones — exactly what a
// domain waiting on DNS needs. snap is the caller's row snapshot; the run
// re-reads under the domain's lock and aborts with errStaleConfig if the row
// was removed (or re-owned) meanwhile.
func (m *Manager) appIssueOnce(snap store.AppDomain) error {
	lock := m.appLock(snap.Domain)
	lock.Lock()
	defer lock.Unlock()
	row, err := m.st.GetAppDomain(snap.Domain)
	if err != nil || row.App != snap.App {
		return errStaleConfig
	}
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
	if err := m.st.UpdateAppDomainStatus(row.Domain, StatusIssuing, "", time.Time{}); err != nil {
		return err
	}
	// Disk-cert reuse keeps retries and restarts inside LE rate limits, same
	// as the box-wide instance: a relay or Caddy hiccup must not burn a fresh
	// certificate.
	certPEM, keyPEM, err := m.readAppCert(row.Domain)
	if err != nil || !certValidFor(certPEM, row.Domain, time.Now()) {
		iss, err := m.appIssuer()
		if err != nil {
			return err
		}
		certPEM, keyPEM, err = iss.Obtain([]string{row.Domain})
		if err != nil {
			return err
		}
		if cur, err := m.st.GetAppDomain(row.Domain); err != nil || cur.App != row.App {
			return errStaleConfig
		}
		if err := m.writeAppCert(row.Domain, certPEM, keyPEM); err != nil {
			return err
		}
	}
	if err := m.armApp(row, certPEM, keyPEM); err != nil {
		return err
	}
	if err := r.ConfirmCustomDomain(row.Domain); err != nil {
		return err
	}
	notAfter, err := certs.NotAfter(certPEM)
	if err != nil {
		return err
	}
	return m.st.UpdateAppDomainStatus(row.Domain, StatusActive, "", notAfter)
}

// armApp loads the exact-host cert and backfills the route for an already-
// running app (deploy/stop/delete otherwise own routes). Shared by first
// activation and restart resume.
func (m *Manager) armApp(row store.AppDomain, certPEM, keyPEM []byte) error {
	if err := m.proxy.EnsureHTTPS(m.httpsListen); err != nil {
		return err
	}
	if err := m.setCert(row.Domain, certPEM, keyPEM); err != nil {
		return err
	}
	if m.router != nil {
		if err := m.router.RouteAppDomain(row.App, row.Domain); err != nil {
			return err
		}
	}
	return nil
}

// RemoveAppDomain tears the domain down fully: relay claim, Caddy route and
// loaded cert, disk cert, row. For an active domain the relay removal must
// succeed before anything local is deleted — once the row is gone nothing
// would ever retry the removal and the relay would splice the domain forever.
// A never-confirmed claim expires on the relay by its pending TTL, so its
// removal is best-effort and teardown proceeds. Removing an absent domain is
// a no-op. Taking the domain's run lock can block for an in-flight ACME
// Obtain (minutes, bounded) — the box-wide Remove trade, per domain.
func (m *Manager) RemoveAppDomain(domain string) error {
	m.nextGenFor(domain) // supersede the running lifecycle loop
	lock := m.appLock(domain)
	lock.Lock()
	defer lock.Unlock()
	row, err := m.st.GetAppDomain(domain)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	r := m.notifier()
	if r == nil {
		if row.Status == StatusActive {
			return errors.New("relay not connected; cannot clear the domain mapping")
		}
	} else if err := r.RemoveCustomDomain(domain); err != nil && row.Status == StatusActive {
		return fmt.Errorf("clear relay domain mapping: %w", err)
	}
	_ = m.proxy.RemoveRoute(domain)
	_ = m.unloadCert(domain)
	_ = os.RemoveAll(m.appCertDir(domain))
	return m.st.DeleteAppDomain(domain)
}

// ResumeAppDomains restores per-app domains after a restart, rows driving it:
// an active row with a valid disk cert re-arms locally without re-issuing and
// re-asserts its relay claim in the background (the tunnel is typically not
// connected yet); any other row — or an active one whose disk cert is damaged
// — re-enters the lifecycle loop from the top.
func (m *Manager) ResumeAppDomains() {
	rows, err := m.st.AllAppDomains()
	if err != nil {
		return
	}
	for _, row := range rows {
		if row.Status == StatusActive {
			certPEM, keyPEM, err := m.readAppCert(row.Domain)
			if err == nil && certValidFor(certPEM, row.Domain, time.Now()) {
				if err := m.armApp(row, certPEM, keyPEM); err == nil {
					gen := m.nextGenFor(row.Domain)
					m.spawn(func() { m.reassertLoop(row.Domain, gen) })
					continue
				}
			}
			// Damaged or missing disk cert: degrade to re-issuance.
			if err := m.st.UpdateAppDomainStatus(row.Domain, StatusIssuing, "", time.Time{}); err != nil {
				log.Printf("domain: status write for %s: %v", row.Domain, err)
			}
		}
		gen := m.nextGenFor(row.Domain)
		m.spawn(func() { m.appLoop(row.Domain, gen) })
	}
}

// reassertLoop re-pushes a resumed active domain's claim + confirmation until
// the relay accepts. Active mappings are durable on the relay (#227), so this
// is belt-and-braces for a relay that lost state — and the retry absorbs the
// tunnel not being connected yet at resume. Failures never touch the row.
func (m *Manager) reassertLoop(domain string, gen int) {
	for attempt := 0; ; attempt++ {
		if m.stopped() || m.reassertOnce(domain, gen) {
			return
		}
		if !m.sleepOrStopped(m.retryDelay(attempt)) {
			return
		}
	}
}

// reassertOnce reports whether the loop is finished: superseded, row gone, or
// claim + confirmation pushed. The domain's run lock spans the checks AND the
// push — like every other relay-pushing path — so a concurrent
// RemoveAppDomain cannot interleave between them and let this push mint a
// fresh durable claim for a domain the removal just cleared (the relay would
// hold it forever: the row is gone, so nothing on the box would ever remove
// it again).
func (m *Manager) reassertOnce(domain string, gen int) bool {
	lock := m.appLock(domain)
	lock.Lock()
	defer lock.Unlock()
	if m.currentGenFor(domain) != gen {
		return true
	}
	if _, err := m.st.GetAppDomain(domain); err != nil {
		return true
	}
	r := m.notifier()
	if r == nil {
		return false
	}
	if err := r.AddCustomDomain(domain); err != nil {
		return false
	}
	_ = r.ConfirmCustomDomain(domain)
	return true
}

// renewCheckApps renews every active per-app domain whose disk cert is inside
// the renewal window. Each domain renews under its own lock and records its
// own outcome — one failing renewal never stops the sweep.
func (m *Manager) renewCheckApps(now time.Time) {
	rows, err := m.st.ListActiveAppDomains()
	if err != nil {
		return
	}
	for _, row := range rows {
		m.renewApp(row, now)
	}
}

func (m *Manager) renewApp(snap store.AppDomain, now time.Time) {
	lock := m.appLock(snap.Domain)
	lock.Lock()
	defer lock.Unlock()
	row, err := m.st.GetAppDomain(snap.Domain)
	if err != nil || row.App != snap.App || row.Status != StatusActive {
		return
	}
	certPEM, _, err := m.readAppCert(row.Domain)
	if err != nil {
		return
	}
	due, err := certs.NeedsRenewal(certPEM, renewWindow, now)
	if err != nil || !due {
		return
	}
	if err := m.reissueApp(row); err != nil && !errors.Is(err, errStaleConfig) {
		// Old cert keeps serving until expiry; surface the error, stay active.
		if err := m.st.UpdateAppDomainStatus(row.Domain, StatusActive, "renew: "+err.Error(), row.CertNotAfter); err != nil {
			log.Printf("domain: status write for %s: %v", row.Domain, err)
		}
	}
}

// reissueApp obtains a fresh exact-host cert for an already-active domain and
// swaps it in. Caller holds the domain's run lock.
func (m *Manager) reissueApp(row store.AppDomain) error {
	iss, err := m.appIssuer()
	if err != nil {
		return err
	}
	certPEM, keyPEM, err := iss.Obtain([]string{row.Domain})
	if err != nil {
		return err
	}
	// Writing the cert for a removed domain would resurrect its deleted dir.
	if cur, err := m.st.GetAppDomain(row.Domain); err != nil || cur.App != row.App {
		return errStaleConfig
	}
	if err := m.writeAppCert(row.Domain, certPEM, keyPEM); err != nil {
		return err
	}
	if err := m.setCert(row.Domain, certPEM, keyPEM); err != nil {
		return err
	}
	notAfter, err := certs.NotAfter(certPEM)
	if err != nil {
		return err
	}
	return m.st.UpdateAppDomainStatus(row.Domain, StatusActive, "", notAfter)
}

// AppDomainStatus is the wire state of one per-app custom domain (#231).
type AppDomainStatus struct {
	Domain       string      `json:"domain"`
	App          string      `json:"app"`
	Status       string      `json:"status"` // "pending" | "issuing" | "active" | "failed"
	Error        string      `json:"error"`
	CertNotAfter *time.Time  `json:"cert_not_after,omitempty"`
	DNSRecords   []DNSRecord `json:"dns_records"`
	DNSOK        bool        `json:"dns_ok"`
}

// AppDomainStatuses assembles the wire state of every domain attached to app
// (GET /v1/apps/<app>/domains). Read-only over the store rows the lifecycle
// loops maintain.
func (m *Manager) AppDomainStatuses(app string) ([]AppDomainStatus, error) {
	rows, err := m.st.ListAppDomains(app)
	if err != nil {
		return nil, err
	}
	out := make([]AppDomainStatus, 0, len(rows))
	for _, row := range rows {
		out = append(out, m.appDomainStatus(row))
	}
	return out, nil
}

// AppDomainStatus assembles one domain's wire state (the POST response), or
// store.ErrNotFound.
func (m *Manager) AppDomainStatus(domain string) (AppDomainStatus, error) {
	row, err := m.st.GetAppDomain(domain)
	if err != nil {
		return AppDomainStatus{}, err
	}
	return m.appDomainStatus(row), nil
}

// appDomainStatus converts one row: the guided-setup CNAME (exact host → the
// relay) and a best-effort, cached dns_ok — a field, never an error.
func (m *Manager) appDomainStatus(row store.AppDomain) AppDomainStatus {
	st := AppDomainStatus{
		Domain: row.Domain, App: row.App,
		Status: row.Status, Error: row.Error,
		DNSRecords: []DNSRecord{{Type: "CNAME", Name: row.Domain, Value: m.dnsTarget}},
		DNSOK:      m.cachedDNSPointsAt(row.Domain),
	}
	if !row.CertNotAfter.IsZero() {
		t := row.CertNotAfter
		st.CertNotAfter = &t
	}
	return st
}

func (m *Manager) appCertDir(domain string) string {
	return filepath.Join(m.dataDir, "appdomains", domain)
}

func (m *Manager) writeAppCert(domain string, certPEM, keyPEM []byte) error {
	dir := m.appCertDir(domain)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "cert.pem"), certPEM, 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "key.pem"), keyPEM, 0o600)
}

func (m *Manager) readAppCert(domain string) (certPEM, keyPEM []byte, err error) {
	dir := m.appCertDir(domain)
	certPEM, err = os.ReadFile(filepath.Join(dir, "cert.pem"))
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err = os.ReadFile(filepath.Join(dir, "key.pem"))
	if err != nil {
		return nil, nil, err
	}
	return certPEM, keyPEM, nil
}
