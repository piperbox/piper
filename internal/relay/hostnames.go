package relay

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/piperbox/piper/internal/tunnel"
)

// appHostname derives the single-label public hostname for (agent, app, pr):
// "<hex>-<username>.<apex>" for production (pr 0), "pr<N>-<hex>-<username>.<apex>"
// for a PR preview, where <hex> is the first 8 hex chars of sha256 over the
// same triple. It truncates <username> so the whole first label stays within
// DNS's 63-char limit (the 8-char hash preserves uniqueness). Deterministic —
// the same (agent, app, pr) always maps to the same hostname.
//
// The hash keys on the agent, not the account: two boxes on one account may
// each run an app called "blog", and hashing the account would derive one
// hostname for both, silently handing the second box the first's URL (#405).
//
// Everything stays in ONE label: the relay serves a "*.<apex>" wildcard, which
// matches exactly one label, so a preview at "pr-<N>-<app>.<agent>.<apex>"
// would fail TLS outright (#302).
func appHostname(agentName, app, username, apex string, pr int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%s/%d", agentName, app, pr)))
	h := hex.EncodeToString(sum[:])[:8]
	prefix := ""
	if pr > 0 {
		prefix = fmt.Sprintf("pr%d-", pr)
	}
	// first label is "<prefix><h>-<username>": the rest is the username budget.
	if max := 63 - len(prefix) - len(h) - 1; len(username) > max {
		username = username[:max]
	}
	return prefix + h + "-" + username + "." + apex
}

// AgentAccount resolves the account owning the agent whose base_domain is
// baseDomain, along with the agent's own name — which hostname rows are keyed
// by, and which is not the base domain for an operator-enrolled agent
// (`piper-relay enroll <name> --domain <base>`). ErrBadToken if there is no such
// agent; ErrBadCredential if the owning account is disabled.
func (s *Store) AgentAccount(baseDomain string) (accountID, username, agentName string, err error) {
	var disabled sql.NullInt64
	err = s.db.QueryRow(
		`SELECT acc.id, acc.username, ag.name, acc.disabled
		   FROM agents ag JOIN accounts acc ON acc.id = ag.account_id
		  WHERE ag.base_domain = ?`, baseDomain).
		Scan(&accountID, &username, &agentName, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", "", ErrBadToken
	}
	if err != nil {
		return "", "", "", err
	}
	if disabled.Valid && disabled.Int64 != 0 {
		return "", "", "", ErrBadCredential
	}
	return accountID, username, agentName, nil
}

// AgentDisabled reports whether the account owning the agent whose base_domain
// is baseDomain has been disabled by the operator kill-switch. It is the
// narrowest read the relay's per-session watchdog needs, and it distinguishes
// three outcomes so the watchdog can tell transient from permanent:
//   - (false, nil)              the agent is live and enabled;
//   - (true, nil)               the owning account is disabled;
//   - (false, ErrUnknownAccount) there is no such agent row — the base is gone.
//
// A store failure comes back as its raw (transient) error, so the watchdog
// leaves healthy sessions running on a blip and evicts only on the two
// affirmative reads (disabled=true or ErrUnknownAccount). The LEFT JOIN mirrors
// Authenticate: an account-less agent (operator-enrolled via `piper-relay
// enroll`) still has an agents row, so its NULL acc.disabled reads as
// not-disabled — only a *missing agent row* is unknown.
func (s *Store) AgentDisabled(baseDomain string) (bool, error) {
	var disabled sql.NullInt64
	err := s.db.QueryRow(
		`SELECT acc.disabled
		   FROM agents ag LEFT JOIN accounts acc ON acc.id = ag.account_id
		  WHERE ag.base_domain = ?`, baseDomain).Scan(&disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrUnknownAccount
	}
	if err != nil {
		return false, err
	}
	return disabled.Valid && disabled.Int64 != 0, nil
}

// RegisterHostname assigns (idempotently) the public hostname for app on the
// account owning baseDomain, enforcing the per-account app cap. pr is 0 for the
// production host and the PR number for a preview, which gets a hostname of its
// own so it can never overwrite production's. Returns the assigned
// "<app-hash>-<username>.<apex>".
//
// Only production hosts count against the app cap: the cap bounds apps, and an
// open PR must not lock an account out of creating one.
func (s *Store) RegisterHostname(baseDomain, app string, pr int) (string, error) {
	accountID, username, agentName, err := s.AgentAccount(baseDomain)
	if err != nil {
		return "", err
	}

	var existing string
	err = s.db.QueryRow(`SELECT hostname FROM hostnames WHERE agent_name=? AND app=? AND pr=?`, agentName, app, pr).Scan(&existing)
	if err == nil {
		return existing, nil // idempotent
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}

	if pr == 0 {
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM hostnames WHERE account_id=? AND pr=0`, accountID).Scan(&count); err != nil {
			return "", err
		}
		if count >= s.maxAppsOrDefault() {
			return "", ErrQuotaExceeded
		}
	}

	hostname := appHostname(agentName, app, username, s.apexOrDefault(), pr)
	_, err = s.db.Exec(
		`INSERT INTO hostnames(hostname, agent_name, account_id, app, pr, created_at) VALUES(?,?,?,?,?,?)`,
		hostname, agentName, accountID, app, pr, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return "", err
	}
	return hostname, nil
}

// ReconcileHostnames settles the relay's hostname rows for one agent against
// the set of slots the box says it holds, and returns each survivor's assigned
// hostname — for the router to map, and for the box to persist, since the relay
// names the slots and that name can change under a box. It is what a session
// sends on connect (#418).
//
// The box is the authority on which apps exist; the relay is the authority on
// what each one is called. So this prunes rows for slots the box no longer has
// — otherwise orphaned forever, since deleting an app while the relay is
// unreachable skips deregistration entirely — and then ensures a row for every
// slot it does, which is how a box recovers after a relay DB wipe.
//
// Pruning before ensuring matters: a box at its app cap that dropped an app
// needs the freed slot to re-register the ones it kept.
//
// Nothing here is atomic across slots, and it does not need to be. Hostnames
// are deterministic in (agent, app, pr), so a row pruned in error returns
// identical, and the whole exchange repeats on the next connect. One slot's
// failure — a cap it cannot fit under, say — is skipped rather than failing the
// rest, matching the per-app re-push this replaces.
func (s *Store) ReconcileHostnames(baseDomain string, apps []tunnel.AppRef) (live []tunnel.AppHost, pruned []string, err error) {
	_, _, agentName, err := s.AgentAccount(baseDomain)
	if err != nil {
		return nil, nil, err
	}

	keep := make(map[string]bool, len(apps))
	for _, a := range apps {
		keep[hostnameSlot(a.App, a.PR)] = true
	}
	rows, err := s.db.Query(`SELECT hostname, app, pr FROM hostnames WHERE agent_name=?`, agentName)
	if err != nil {
		return nil, nil, err
	}
	type slot struct {
		hostname string
		app      string
		pr       int
	}
	var stale []slot
	for rows.Next() {
		var sl slot
		if err := rows.Scan(&sl.hostname, &sl.app, &sl.pr); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if !keep[hostnameSlot(sl.app, sl.pr)] {
			stale = append(stale, sl)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	for _, sl := range stale {
		if _, err := s.db.Exec(
			`DELETE FROM hostnames WHERE agent_name=? AND app=? AND pr=?`,
			agentName, sl.app, sl.pr); err != nil {
			return nil, nil, err
		}
		pruned = append(pruned, sl.hostname)
	}

	live = make([]tunnel.AppHost, 0, len(apps))
	for _, a := range apps {
		host, err := s.RegisterHostname(baseDomain, a.App, a.PR)
		if err != nil {
			log.Printf("relay: sync %s %s pr %d: %v", agentName, a.App, a.PR, err)
			continue
		}
		live = append(live, tunnel.AppHost{App: a.App, PR: a.PR, Hostname: host})
	}
	return live, pruned, nil
}

// hostnameSlot keys one (app, pr) pair for set membership.
func hostnameSlot(app string, pr int) string {
	return fmt.Sprintf("%s/%d", app, pr)
}

// DeregisterHostname removes the hostname row for the agent whose base domain is
// baseDomain. Scoped to that agent, not its account, so one box cannot drop
// another's hostname. A missing row is not an error.
func (s *Store) DeregisterHostname(baseDomain, hostname string) error {
	_, _, agentName, err := s.AgentAccount(baseDomain)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM hostnames WHERE agent_name=? AND hostname=?`, agentName, hostname)
	return err
}
