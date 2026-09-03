package relay

import (
	"strings"
	"time"
)

// Binding is one repo→app deploy route on a specific agent.
type Binding struct {
	AgentName string
	App       string
	Repo      string // "owner/name", lowercased
	Branch    string
}

// normalizeRepo lowercases an "owner/name" so lookups match however GitHub
// spelled the repository in a given payload.
func normalizeRepo(repo string) string { return strings.ToLower(strings.TrimSpace(repo)) }

// BindRepo records that agentName's app deploys from repo@branch. One binding
// per (agent, app): re-linking an app to a different repo replaces the old row.
func (s *Store) BindRepo(agentName, app, repo, branch string) error {
	_, err := s.db.Exec(
		`INSERT INTO repo_bindings(agent_name, app, repo, branch, created_at)
		 VALUES($1,$2,$3,$4,$5)
		 ON CONFLICT(agent_name, app) DO UPDATE SET
		     repo = excluded.repo, branch = excluded.branch`,
		agentName, app, normalizeRepo(repo), branch,
		time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// UnbindRepo removes an app's binding. Removing an absent binding is not an error.
func (s *Store) UnbindRepo(agentName, app string) error {
	_, err := s.db.Exec(`DELETE FROM repo_bindings WHERE agent_name=$1 AND app=$2`, agentName, app)
	return err
}

// BindingsForRepo returns every binding for repo among accountID's own agents.
// Scoping by account is what keeps one tenant's push from ever reaching
// another tenant's box, even if both bound the same repository name.
func (s *Store) BindingsForRepo(accountID, repo string) ([]Binding, error) {
	rows, err := s.db.Query(
		`SELECT b.agent_name, b.app, b.repo, b.branch
		   FROM repo_bindings b JOIN agents a ON a.name = b.agent_name
		  WHERE b.repo = $1 AND a.account_id = $2`,
		normalizeRepo(repo), accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Binding
	for rows.Next() {
		var b Binding
		if err := rows.Scan(&b.AgentName, &b.App, &b.Repo, &b.Branch); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// AgentBoundToRepo reports whether agentName has any binding for repo. This is
// the token-brokering authz check: a box may mint a token only for a repository
// it actually deploys, so one compromised box cannot read every repository the
// account granted the App.
func (s *Store) AgentBoundToRepo(agentName, repo string) (bool, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM repo_bindings WHERE agent_name=$1 AND repo=$2`,
		agentName, normalizeRepo(repo)).Scan(&n)
	return n > 0, err
}
