package domain

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/piperbox/piper/internal/certs"
	"github.com/piperbox/piper/internal/store"
)

const (
	renewInterval = 12 * time.Hour
	renewWindow   = 30 * 24 * time.Hour
)

// Resume restores state after a restart: an active config re-arms from the
// disk cert (no re-issuance); issuing/failed configs — or an active one whose
// disk cert is damaged — re-enter the issue loop. The relay is not re-notified:
// it re-derives the mapping from its own store at session registration.
func (m *Manager) Resume() {
	if m.envDomain != "" {
		return
	}
	// issueMu spans the config read and the generation bump so a concurrent
	// Set can't land its replace between them: a bump from a stale read (old
	// domain) arriving after the replace's own bump would supersede the new
	// domain's loop while this loop exits on the domain mismatch, stranding
	// the replacement in "issuing" from boot (#275). The lock is released
	// before the goroutine starts — issueOnce re-acquires issueMu for the
	// duration of the ACME run, so holding it here would only serialize the
	// kick against in-flight issuance, never cover the run.
	m.issueMu.Lock()
	dc, err := m.st.GetDomainConfig()
	if err != nil {
		m.issueMu.Unlock()
		return
	}
	if dc.Status == StatusActive {
		certPEM, keyPEM, err := m.readCert()
		if err == nil && certCovers(certPEM, dc.Domain, time.Now()) {
			if err := m.arm(dc, certPEM, keyPEM); err == nil {
				m.issueMu.Unlock()
				return
			}
		}
		// Damaged or missing disk cert: degrade to re-issuance, not a crash.
		if err := m.st.UpdateDomainStatus(dc.Domain, StatusIssuing, "", time.Time{}); err != nil {
			log.Printf("domain: status write for %s: %v", dc.Domain, err)
		}
	}
	gen := m.nextGen()
	m.issueMu.Unlock()
	m.spawn(func() { m.issueLoop(dc.Domain, gen) })
}

// OnRelayConnect re-kicks issuance for a non-active box-wide config when the
// tunnel (re)connects, so a restart's not-connected wait retries on the
// connect event instead of waiting out its backoff (#166); wired from the
// tunnel client's OnConnect in cmd/piperd.
func (m *Manager) OnRelayConnect() {
	if m.envDomain != "" {
		return
	}
	// issueMu spans the config read and the generation bump so a concurrent
	// Set can't land its replace between them: a bump from a stale read (old
	// domain) arriving after the replace's own bump would supersede the new
	// domain's loop while this loop exits on the domain mismatch, stranding
	// the replacement in "issuing" until the next reconnect. The lock is
	// released before the goroutine starts — issueOnce re-acquires issueMu
	// for the duration of the ACME run, so holding it here would only
	// serialize the kick against in-flight issuance, never cover the run.
	m.issueMu.Lock()
	dc, err := m.st.GetDomainConfig()
	if err != nil {
		m.issueMu.Unlock()
		return
	}
	if dc.Status == StatusActive {
		m.issueMu.Unlock()
		return // arm-only: the relay re-derives the mapping at session registration
	}
	gen := m.nextGen()
	m.issueMu.Unlock()
	m.spawn(func() { m.issueLoop(dc.Domain, gen) })
}

// setEnvStatus records the env-managed (PIPER_BASE_DOMAIN) path's real state so
// GET /v1/domain reflects issued/failed + expiry instead of a constant "active"
// (#116). No-op fields are zeroed by the caller (e.g. notAfter on failure).
func (m *Manager) setEnvStatus(status, errMsg string, notAfter time.Time) {
	m.envMu.Lock()
	m.envStatus = status
	m.envError = errMsg
	m.envNotAfter = notAfter
	m.envMu.Unlock()
}

// StartRenewals renews the API-managed box-wide cert and every active per-app
// domain cert: every renewInterval, when a disk cert is within renewWindow of
// expiry, re-obtain and hot-swap it. Blocks until ctx ends; run it in a
// goroutine.
func (m *Manager) StartRenewals(ctx context.Context) {
	t := time.NewTicker(renewInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.renewCheck(time.Now())
			m.renewCheckApps(time.Now())
		}
	}
}

func (m *Manager) renewCheck(now time.Time) {
	dc, err := m.st.GetDomainConfig()
	if err != nil || dc.Status != StatusActive {
		return
	}
	certPEM, _, err := m.readCert()
	if err != nil {
		return
	}
	due, err := certs.NeedsRenewal(certPEM, renewWindow, now)
	if err != nil || !due {
		return
	}
	if err := m.reissue(dc); err != nil && !errors.Is(err, errStaleConfig) {
		// Old cert keeps serving until expiry; surface the error, stay active.
		if err := m.st.UpdateDomainStatus(dc.Domain, StatusActive, "renew: "+err.Error(), dc.CertNotAfter); err != nil {
			log.Printf("domain: status write for %s: %v", dc.Domain, err)
		}
	}
}

// reissue obtains a fresh cert for an already-active config and swaps it in.
// Like issueOnce, it re-validates the snapshot under issueMu and aborts with
// errStaleConfig when the config was replaced or removed meanwhile.
func (m *Manager) reissue(snap store.DomainConfig) error {
	m.issueMu.Lock()
	defer m.issueMu.Unlock()
	dc, err := m.st.GetDomainConfig()
	if err != nil || dc.Domain != snap.Domain {
		return errStaleConfig
	}
	iss, err := m.newIssuer(dc.DNSProvider, dc.DNSToken)
	if err != nil {
		return err
	}
	certPEM, keyPEM, err := iss.Obtain([]string{"*." + dc.Domain, dc.Domain})
	if err != nil {
		return err
	}
	// Defense in depth (see issueOnce): writing the cert for a torn-down
	// config would resurrect its deleted cert dir.
	if cur, err := m.st.GetDomainConfig(); err != nil || cur.Domain != dc.Domain {
		return errStaleConfig
	}
	if err := m.writeCert(certPEM, keyPEM); err != nil {
		return err
	}
	if err := m.setCert(boxWideKey, certPEM, keyPEM); err != nil {
		return err
	}
	notAfter, err := certs.NotAfter(certPEM)
	if err != nil {
		return err
	}
	return m.st.UpdateDomainStatus(dc.Domain, StatusActive, "", notAfter)
}

// RunEnv drives the env-managed (PIPER_BASE_DOMAIN) BYO path: issue the
// wildcard cert for the env domain now, then renew in the background — the
// fold-in of piperd's former setupRelayTLS/renewLoop pair. The Caddy manager
// was started WithHTTPS in this mode, so no EnsureHTTPS is needed.
func (m *Manager) RunEnv(ctx context.Context, iss Issuer) error {
	certPEM, keyPEM, err := iss.Obtain([]string{"*." + m.envDomain, m.envDomain})
	if err != nil {
		m.setEnvStatus(StatusFailed, err.Error(), time.Time{})
		return err
	}
	if err := m.setCert(boxWideKey, certPEM, keyPEM); err != nil {
		m.setEnvStatus(StatusFailed, err.Error(), time.Time{})
		return err
	}
	notAfter, _ := certs.NotAfter(certPEM)
	m.setEnvStatus(StatusActive, "", notAfter)
	go func() {
		t := time.NewTicker(renewInterval)
		defer t.Stop()
		m.runEnvRenew(ctx, iss, certPEM, t.C, time.Now)
	}()
	return nil
}

func (m *Manager) runEnvRenew(ctx context.Context, iss Issuer, certPEM []byte, ticks <-chan time.Time, now func() time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			due, err := certs.NeedsRenewal(certPEM, renewWindow, now())
			if err != nil || !due {
				continue
			}
			newCert, newKey, err := iss.Obtain([]string{"*." + m.envDomain, m.envDomain})
			if err != nil {
				log.Printf("domain: env renew: %v", err)
				continue
			}
			if err := m.setCert(boxWideKey, newCert, newKey); err != nil {
				log.Printf("domain: env renew load: %v", err)
				continue
			}
			certPEM = newCert
			notAfter, _ := certs.NotAfter(certPEM)
			m.setEnvStatus(StatusActive, "", notAfter)
		}
	}
}
