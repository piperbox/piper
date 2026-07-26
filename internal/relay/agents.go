package relay

import (
	"database/sql"
	"errors"
)

// ErrUnknownAgent is returned when no agent holds the given base domain. The
// existing sentinels do not fit: ErrUnknownAccount names a missing account, and
// AgentAccount answers ErrBadToken for an unknown base domain — right on an
// authentication path, misleading on an explicit delete.
var ErrUnknownAgent = errors.New("unknown agent")

// DeleteAgent retires one box: its agents row plus the rows keyed on its name.
//
// hostnames is deliberately NOT touched. That table keys on account_id with no
// agent column (see schema.sql), so there is no way to tell which rows were
// this box's — the relay already depends on this being unknowable, which is why
// repushRelayApps re-pushes hostnames from the box instead. Deleting the
// account's hostnames would destroy its *other* boxes' URLs. The consequence is
// that removal frees an agent slot but not an app slot; that gap is tracked
// separately rather than guessed at here.
//
// The name is resolved first because repo_bindings and pending_events both
// reference agents(name), not base_domain.
func (s *Store) DeleteAgent(baseDomain string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var name string
	err = tx.QueryRow(`SELECT name FROM agents WHERE base_domain = ?`, baseDomain).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUnknownAgent
	}
	if err != nil {
		return err
	}
	for _, stmt := range []string{
		`DELETE FROM pending_events WHERE agent_name = ?`,
		`DELETE FROM repo_bindings WHERE agent_name = ?`,
		`DELETE FROM agents WHERE name = ?`,
	} {
		if _, err := tx.Exec(stmt, name); err != nil {
			return err
		}
	}
	return tx.Commit()
}
