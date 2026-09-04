package relay

import (
	"database/sql"
	"errors"
	"time"
)

// Per-app BYO custom domains (#227). Each row is one domain claimed by one
// agent, pending until the box confirms cert issuance. A pending claim is
// routable immediately — that is what lets the TLS-ALPN-01 challenge reach
// the box before any cert exists — but expires after pendingTTL if never
// confirmed, so an unproven claim can squat a name for at most an hour.
// Expiry is lazy: rival claims evict, and CustomDomains filters at reconnect
// re-derivation; there is no background sweeper.

// pendingTTL is how long an unconfirmed pending claim holds a domain.
const pendingTTL = time.Hour

// ErrDomainNotFound is returned when an agent confirms a domain it does not hold.
var ErrDomainNotFound = errors.New("domain not registered to this agent")

func (s *Store) maxDomainsOrDefault() int {
	if s.maxDomains <= 0 {
		return 5
	}
	return s.maxDomains
}

// liveAt reports whether a custom_domains row still counts: active rows
// always, pending rows only within pendingTTL of their claim. Timestamps are
// parsed here rather than compared in SQL — RFC3339Nano trims trailing
// zeros, so lexical order is unreliable. Unparsable rows count as expired.
func liveAt(status, createdAt string, now time.Time) bool {
	if status == "active" {
		return true
	}
	t, err := time.Parse(time.RFC3339Nano, createdAt)
	return err == nil && now.Sub(t) < pendingTTL
}

// AddCustomDomain claims domain for the agent enrolled at baseDomain as a
// pending custom domain. First-come-first-served: a domain live under
// another agent is ErrDomainTaken, but an expired pending claim is evicted.
// Re-adding your own pending domain refreshes its TTL window (an operator
// retrying resets their clock); re-adding your own active domain is a no-op.
// New claims count toward the per-agent cap, pending included — pending rows
// are the squattable kind.
func (s *Store) AddCustomDomain(baseDomain, domain string) error {
	if !customDomainRE.MatchString(domain) {
		return ErrInvalidDomain
	}
	if err := s.domainClaimable(domain); err != nil {
		return err
	}
	if off, err := s.AgentDisabled(baseDomain); err != nil {
		if !errors.Is(err, ErrUnknownAccount) {
			return err
		}
	} else if off {
		return ErrBadCredential
	}
	now := s.nowFunc().UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The per-agent domain cap is enforced under the agent row's lock so two
	// claims for one agent, on any relay, count in sequence.
	var one int
	if err := tx.QueryRow(`SELECT 1 FROM agents WHERE base_domain=$1 FOR UPDATE`, baseDomain).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrBadToken
		}
		return err
	}
	var owner, status, created string
	err = tx.QueryRow(
		`SELECT agent_base, status, created_at FROM custom_domains WHERE domain=$1`, domain).
		Scan(&owner, &status, &created)
	switch {
	case err == nil && owner == baseDomain:
		if status == "pending" {
			if _, err := tx.Exec(`UPDATE custom_domains SET created_at=$1 WHERE domain=$2`,
				now.Format(time.RFC3339Nano), domain); err != nil {
				return err
			}
		}
		return tx.Commit() // own active row: no-op re-add
	case err == nil:
		if liveAt(status, created, now) {
			return ErrDomainTaken
		}
		// Expired pending claim by another agent: evict and claim below.
		if _, err := tx.Exec(`DELETE FROM custom_domains WHERE domain=$1`, domain); err != nil {
			return err
		}
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	live, err := countLive(tx, baseDomain, now)
	if err != nil {
		return err
	}
	if live >= s.maxDomainsOrDefault() {
		return ErrQuotaExceeded
	}
	if _, err := tx.Exec(
		`INSERT INTO custom_domains(domain, agent_base, status, created_at) VALUES($1, $2, 'pending', $3)`,
		domain, baseDomain, now.Format(time.RFC3339Nano)); err != nil {
		if isUniqueViolation(err) {
			return ErrDomainTaken // PK backstop: lost the FCFS race
		}
		return err
	}
	if err := notify(tx, chanHostnames, domain); err != nil {
		return err
	}
	return tx.Commit()
}

// countLive counts the agent's live rows inside tx (cap enforcement).
func countLive(tx *sql.Tx, baseDomain string, now time.Time) (int, error) {
	rows, err := tx.Query(
		`SELECT status, created_at FROM custom_domains WHERE agent_base=$1`, baseDomain)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var st, ca string
		if err := rows.Scan(&st, &ca); err != nil {
			return 0, err
		}
		if liveAt(st, ca, now) {
			n++
		}
	}
	return n, rows.Err()
}

// CustomDomains returns the agent's live custom domains — active plus
// unexpired pending, sorted — for reconnect re-derivation. Expired pending
// rows are filtered, so a squat mapping dies at the next reconnect even if
// never contested.
func (s *Store) CustomDomains(baseDomain string) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT domain, status, created_at FROM custom_domains WHERE agent_base=$1 ORDER BY domain`,
		baseDomain)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := s.nowFunc().UTC()
	var out []string
	for rows.Next() {
		var d, st, ca string
		if err := rows.Scan(&d, &st, &ca); err != nil {
			return nil, err
		}
		if liveAt(st, ca, now) {
			out = append(out, d)
		}
	}
	return out, rows.Err()
}

// ConfirmCustomDomain flips the agent's own claim to active: the box reports
// it holds the issued cert (#229 sends this after TLS-ALPN-01 completes).
// Pending age is deliberately not checked — eviction by a rival claim is the
// only thing that kills a claim, so a slow issuance still confirms if nobody
// contested the name. Idempotent on active rows.
func (s *Store) ConfirmCustomDomain(baseDomain, domain string) error {
	res, err := s.db.Exec(
		`UPDATE custom_domains SET status='active' WHERE domain=$1 AND agent_base=$2`,
		domain, baseDomain)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrDomainNotFound
	}
	return notify(s.db, chanHostnames, domain)
}

// RemoveCustomDomain drops the agent's own claim on domain. Idempotent —
// removing a domain the agent does not hold is a no-op, so teardown retries
// are safe.
func (s *Store) RemoveCustomDomain(baseDomain, domain string) error {
	_, err := s.removeCustomDomainOwned(baseDomain, domain)
	return err
}

// removeCustomDomainOwned does the same delete as RemoveCustomDomain but also
// reports whether a row was actually deleted. Callers need this to know
// whether the requester ever held the domain: a rival's remove-domain for a
// name they don't own must stay a no-op at the store layer (idempotency, kept
// above) but must NOT cascade into unrouting another agent's live domain —
// that would let any authenticated agent DoS a tenant it doesn't own.
func (s *Store) removeCustomDomainOwned(baseDomain, domain string) (bool, error) {
	res, err := s.db.Exec(
		`DELETE FROM custom_domains WHERE domain=$1 AND agent_base=$2`, domain, baseDomain)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}
	return true, notify(s.db, chanHostnames, domain)
}
