package relay

import (
	"database/sql"
	"errors"
	"time"
)

// ErrNoInstallation is returned when no GitHub App installation is on record
// for the requested installation id or account.
var ErrNoInstallation = errors.New("no github installation")

// LinkInstallation records a GitHub App installation against the account of the
// user who installed it (the webhook's sender). An org-target install still
// links to the installing user; target_login is not display metadata but the
// routing key GitHubTokenFor matches the repo owner against.
//
// Idempotent by installation_id, because the OAuth redirect and the
// installation webhook race and either may land first.
func (s *Store) LinkInstallation(installationID, senderGithubID, targetType, targetLogin string) error {
	var accountID string
	err := s.db.QueryRow(`SELECT id FROM accounts WHERE github_id=?`, senderGithubID).Scan(&accountID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUnknownAccount
	}
	if err != nil {
		return err
	}
	return s.LinkInstallationForAccount(installationID, accountID, targetType, targetLogin)
}

// LinkInstallationForAccount records an installation against an already-resolved
// account id. The org-routing path resolves the org account itself
// (OrgForGitHubInstall) and links through here; LinkInstallation is the
// sender-resolving convenience over it.
func (s *Store) LinkInstallationForAccount(installationID, accountID, targetType, targetLogin string) error {
	_, err := s.db.Exec(
		`INSERT INTO github_installations(installation_id, account_id, target_type, target_login, created_at)
		 VALUES(?,?,?,?,?)
		 ON CONFLICT(installation_id) DO UPDATE SET
		     account_id   = excluded.account_id,
		     target_type  = excluded.target_type,
		     target_login = excluded.target_login`,
		installationID, accountID, targetType, targetLogin,
		time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// LinkInstallationIfAbsent is LinkInstallation with insert-if-absent
// semantics: it reports whether the link was actually inserted. An existing
// row — whatever account it names — is left untouched. This is the recovery
// path for installation_repositories events, whose sender is not necessarily
// the installation's owner: a read-then-upsert can miss a legitimate link
// committing concurrently and would then replace its owner.
//
// The sender's account is resolved inside the INSERT ... SELECT itself, so
// the statement stays a single atomic operation and never consults the
// sender before honouring an existing row: an already-linked installation
// reports not-inserted (owner preserved) even when the sender has no piper
// account. Only when zero rows were written AND no installation row exists
// is the zero-row result an unknown sender, reported as ErrUnknownAccount.
func (s *Store) LinkInstallationIfAbsent(installationID, senderGithubID, targetType, targetLogin string) (bool, error) {
	res, err := s.db.Exec(
		`INSERT INTO github_installations(installation_id, account_id, target_type, target_login, created_at)
		 SELECT ?, id, ?, ?, ? FROM accounts WHERE github_id=?
		 ON CONFLICT(installation_id) DO NOTHING`,
		installationID, targetType, targetLogin,
		time.Now().UTC().Format(time.RFC3339Nano),
		senderGithubID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n > 0 {
		return true, nil
	}
	// Zero rows is ambiguous: the conflict fired (installation already
	// linked — owner preserved, no error) or the SELECT matched no account
	// (unknown sender). Distinguish by the row that decides recovery: an
	// existing installation needs no link, whoever the sender is.
	if _, err := s.AccountForInstallation(installationID); err == nil {
		return false, nil
	} else if !errors.Is(err, ErrNoInstallation) {
		return false, err
	}
	return false, ErrUnknownAccount
}

// LinkInstallationForAccountIfAbsent is LinkInstallationForAccount with
// insert-if-absent semantics, for callers that resolved the account
// themselves (the org-routing path through OrgForGitHubInstall). The bool
// reports whether the row was inserted; ON CONFLICT DO NOTHING makes the
// check-and-insert a single statement, so a concurrent legitimate link
// always wins over a recovery attempt.
func (s *Store) LinkInstallationForAccountIfAbsent(installationID, accountID, targetType, targetLogin string) (bool, error) {
	res, err := s.db.Exec(
		`INSERT INTO github_installations(installation_id, account_id, target_type, target_login, created_at)
		 VALUES(?,?,?,?,?)
		 ON CONFLICT(installation_id) DO NOTHING`,
		installationID, accountID, targetType, targetLogin,
		time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// UnlinkInstallation drops an installation, e.g. on installation.deleted.
func (s *Store) UnlinkInstallation(installationID string) error {
	_, err := s.db.Exec(`DELETE FROM github_installations WHERE installation_id=?`, installationID)
	return err
}

// AccountForInstallation resolves an installation to its owning account id.
func (s *Store) AccountForInstallation(installationID string) (string, error) {
	var id string
	err := s.db.QueryRow(
		`SELECT account_id FROM github_installations WHERE installation_id=?`,
		installationID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoInstallation
	}
	return id, err
}

// Installation is one GitHub App installation linked to an account, carrying
// the identity of its target — the user or org the App is installed on
// (github_installations.target_type / target_login). target_login is the
// routing key for token minting, not mere display metadata.
type Installation struct {
	ID          string `json:"installation_id"`
	TargetType  string `json:"target_type"`
	TargetLogin string `json:"target_login"`
}

// InstallationsForAccount lists every installation linked to the account,
// newest first. Empty (not an error) when the account has none.
func (s *Store) InstallationsForAccount(accountID string) ([]Installation, error) {
	rows, err := s.db.Query(
		`SELECT installation_id, target_type, target_login FROM github_installations
		  WHERE account_id=? ORDER BY created_at DESC, rowid DESC`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Installation
	for rows.Next() {
		var in Installation
		if err := rows.Scan(&in.ID, &in.TargetType, &in.TargetLogin); err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}
