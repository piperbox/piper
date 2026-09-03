package relay

import (
	"database/sql"
	"errors"
	"strings"
)

// SetOrgGitHub records the GitHub organization login a Piper org account
// corresponds to, so an org-target App installation can be routed to that org's
// boxes. Stored lowercased; the stable numeric id is pinned later, from the
// first installation webhook (see OrgForGitHubInstall). ErrNoOrg if orgID is not
// an org account.
func (s *Store) SetOrgGitHub(orgID, githubLogin string) error {
	res, err := s.db.Exec(
		`UPDATE accounts SET github_login=$1 WHERE id=$2 AND type='org'`,
		strings.ToLower(strings.TrimSpace(githubLogin)), orgID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoOrg
	}
	return nil
}

// OrgForGitHubInstall resolves an org-target GitHub App installation to the
// Piper org account it belongs to: by the pinned GitHub org id when present,
// else by the declared login (SetOrgGitHub). The installing sender must be a
// member of that org — the tenancy guard that stops a non-member's install from
// binding to an org whose login another account merely declared. On a match it
// pins the stable org id for id-based resolution thereafter. ErrNoOrg when no
// org matches or the sender is not a member (indistinguishable, so neither leaks
// org existence).
func (s *Store) OrgForGitHubInstall(orgGitHubID, orgGitHubLogin, senderGitHubID string) (string, error) {
	login := strings.ToLower(strings.TrimSpace(orgGitHubLogin))
	var orgID string
	err := s.db.QueryRow(
		`SELECT id FROM accounts
		  WHERE type='org' AND (github_id=$1 OR lower(github_login)=$2)
		  ORDER BY (github_id=$3) DESC LIMIT 1`,
		orgGitHubID, login, orgGitHubID).Scan(&orgID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoOrg
	}
	if err != nil {
		return "", err
	}
	var member int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM org_members m JOIN accounts a ON a.id=m.account_id
		  WHERE m.org_id=$1 AND a.github_id=$2`, orgID, senderGitHubID).Scan(&member); err != nil {
		return "", err
	}
	if member == 0 {
		return "", ErrNoOrg
	}
	// Pin the stable id for future events (best-effort; a unique-violation means
	// another org already claimed it, which the login match above did not hit).
	_, _ = s.db.Exec(
		`UPDATE accounts SET github_id=$1 WHERE id=$2 AND (github_id IS NULL OR github_id='')`,
		orgGitHubID, orgID)
	return orgID, nil
}
