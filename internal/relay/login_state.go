package relay

import (
	"database/sql"
	"errors"
	"time"
)

// Login-flow state (#522). These methods back the steps of a login that span
// two HTTP requests, so the second request may land on any relay. Every put
// sweeps its table's expired rows first — the same opportunistic pattern the
// in-memory maps used — and every read or take is guarded by expires_at >
// now(), evaluated on the server so relay clocks never disagree.

// PutWebState records a pending dashboard browser login.
func (s *Store) PutWebState(state, redirectURI string, ttl time.Duration) error {
	if _, err := s.db.Exec(`DELETE FROM login_web_states WHERE expires_at <= now()`); err != nil {
		return err
	}
	_, err := s.db.Exec(
		`INSERT INTO login_web_states(state, redirect_uri, expires_at)
		 VALUES($1, $2, now() + make_interval(secs => $3))`,
		state, redirectURI, ttl.Seconds())
	return err
}

// TakeWebState redeems a web state exactly once: the row is deleted as it is
// read, so two relays racing the same callback cannot both succeed.
func (s *Store) TakeWebState(state string) (string, bool, error) {
	var ru string
	err := s.db.QueryRow(
		`DELETE FROM login_web_states WHERE state = $1 AND expires_at > now() RETURNING redirect_uri`,
		state).Scan(&ru)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return ru, true, nil
}
