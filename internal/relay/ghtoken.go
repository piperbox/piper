package relay

import (
	"context"
	"errors"
	"strings"
	"time"
)

// ErrNoGitHubApp is returned when a relay without App credentials is asked to
// broker a token.
var ErrNoGitHubApp = errors.New("relay has no github app configured")

// ErrRepoNotBound is returned when an agent asks for a token for a repository
// it does not deploy.
var ErrRepoNotBound = errors.New("repo not bound to this agent")

// GitHubTokenFor mints a repo-scoped installation token for agentName, after
// checking the full chain: the agent must have a binding for repo, and the
// account owning that agent must hold the installation the token comes from.
// Without the binding check a single compromised box could read every
// repository its account granted the App.
func (s *Store) GitHubTokenFor(ctx context.Context, app *GitHubApp, agentName, repo string) (string, time.Time, error) {
	// Binding check first — it is the authz boundary, and keeping it ahead of
	// the nil-App guard means a relay without App credentials still exercises
	// it (the control-op test would otherwise pass with the check deleted).
	bound, err := s.AgentBoundToRepo(agentName, repo)
	if err != nil {
		return "", time.Time{}, err
	}
	if !bound {
		return "", time.Time{}, ErrRepoNotBound
	}
	if app == nil {
		return "", time.Time{}, ErrNoGitHubApp
	}
	accountID, _, _, err := s.AgentAccount(agentName)
	if err != nil {
		return "", time.Time{}, err
	}
	// The installation that can mint a token for owner/name is the one whose
	// target_login is that owner — an installation only reaches its own
	// account's repos. This replaces most-recent-wins, which broke deploys
	// from any non-newest installation.
	insts, err := s.InstallationsForAccount(accountID)
	if err != nil {
		return "", time.Time{}, err
	}
	owner, _, _ := strings.Cut(normalizeRepo(repo), "/")
	for _, in := range insts {
		if strings.EqualFold(in.TargetLogin, owner) {
			return app.RepoToken(ctx, in.ID, repo)
		}
	}
	// Miss: a GitHub org/user rename strands the stored target_login, which is
	// written once at link time and never refreshed (#433). Re-fetch each
	// candidate installation's live target, and on a match persist the new
	// login through the existing upsert (self-healing) and mint. This runs only
	// on the miss path — one extra API round-trip, never on the happy path.
	for _, in := range insts {
		target, err := app.Installation(ctx, in.ID)
		if err != nil {
			continue // unreachable/deleted installation: try the next candidate
		}
		if !strings.EqualFold(target.Login, owner) {
			continue
		}
		// Heal the login only: in.TargetType is the normalized "org"/"user" the
		// webhook path writes, while GitHub reports a raw account.type
		// ("Organization"), and persisting that would leak a value the storage
		// and /v1/github/status contract doesn't use. A rename doesn't change
		// the target's kind, so this path has no business writing that field.
		//
		// Best-effort heal: the mint must not fail over a local write hiccup;
		// the next miss retries the write.
		_ = s.LinkInstallationForAccount(in.ID, accountID, in.TargetType, target.Login)
		return app.RepoToken(ctx, in.ID, repo)
	}
	return "", time.Time{}, ErrNoInstallation
}
