package relay

import (
	"crypto/subtle"
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

// CLIHandle is one brokered CLI browser login as the callback sees it.
type CLIHandle struct {
	Handle    string
	Confirmed bool
	AccountID string // "" until the callback finished it
}

// cliHandleState is what the CLI's poll learns about a handle.
type cliHandleState int

const (
	cliHandleUnknown cliHandleState = iota // never existed, expired, or already taken
	cliHandlePending                       // waiting for the browser
	cliHandleDone                          // finished; the caller now holds the account
)

var errCLIHandleGone = errors.New("cli login handle gone or unconfirmed")

// PutCLIHandle records a new brokered CLI login. The user code is stored
// normalized so ConfirmCLIHandle compares like with like.
func (s *Store) PutCLIHandle(handle, userCode string, ttl time.Duration) error {
	if _, err := s.db.Exec(`DELETE FROM login_cli_handles WHERE expires_at <= now()`); err != nil {
		return err
	}
	_, err := s.db.Exec(
		`INSERT INTO login_cli_handles(handle, user_code, expires_at)
		 VALUES($1, $2, now() + make_interval(secs => $3))`,
		handle, normalizeCode(userCode), ttl.Seconds())
	return err
}

// ConfirmCLIHandle matches an entered code against the unconfirmed, unexpired
// handles and claims the match. The comparison is constant-time in Go, as
// before; the claim is a guarded UPDATE so two relays cannot both win.
func (s *Store) ConfirmCLIHandle(enteredCode string) (string, bool, error) {
	entered := normalizeCode(enteredCode)
	if entered == "" {
		return "", false, nil
	}
	rows, err := s.db.Query(`SELECT handle, user_code FROM login_cli_handles WHERE NOT confirmed AND expires_at > now()`)
	if err != nil {
		return "", false, err
	}
	defer rows.Close()
	match := ""
	for rows.Next() {
		var h, code string
		if err := rows.Scan(&h, &code); err != nil {
			return "", false, err
		}
		if match == "" && subtle.ConstantTimeCompare([]byte(code), []byte(entered)) == 1 {
			match = h
		}
	}
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	if match == "" {
		return "", false, nil
	}
	res, err := s.db.Exec(
		`UPDATE login_cli_handles SET confirmed = TRUE WHERE handle = $1 AND NOT confirmed AND expires_at > now()`, match)
	if err != nil {
		return "", false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", false, nil
	}
	return match, true, nil
}

// CLIHandle reads one unexpired handle.
func (s *Store) CLIHandle(handle string) (CLIHandle, bool, error) {
	var h CLIHandle
	var acc sql.NullString
	err := s.db.QueryRow(
		`SELECT handle, confirmed, account_id FROM login_cli_handles WHERE handle = $1 AND expires_at > now()`,
		handle).Scan(&h.Handle, &h.Confirmed, &acc)
	if errors.Is(err, sql.ErrNoRows) {
		return CLIHandle{}, false, nil
	}
	if err != nil {
		return CLIHandle{}, false, err
	}
	h.AccountID = acc.String
	return h, true, nil
}

// FinishCLIHandle records the account a confirmed handle logged in as. The
// credential is minted later, by whichever relay serves the poll.
func (s *Store) FinishCLIHandle(handle, accountID string) error {
	res, err := s.db.Exec(
		`UPDATE login_cli_handles SET account_id = $2 WHERE handle = $1 AND confirmed AND expires_at > now()`,
		handle, accountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errCLIHandleGone
	}
	return nil
}

// TakeFinishedCLIHandle is the poll's read. A finished handle is deleted as
// it is read (single use) and its account returned; otherwise it reports
// pending or unknown.
func (s *Store) TakeFinishedCLIHandle(handle string) (string, string, cliHandleState, error) {
	var id, username string
	err := s.db.QueryRow(
		`DELETE FROM login_cli_handles h USING accounts a
		  WHERE h.handle = $1 AND h.account_id = a.id AND h.expires_at > now()
		  RETURNING a.id, a.username`, handle).Scan(&id, &username)
	if err == nil {
		return id, username, cliHandleDone, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", "", cliHandleUnknown, err
	}
	var pending bool
	if err := s.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM login_cli_handles WHERE handle = $1 AND expires_at > now())`,
		handle).Scan(&pending); err != nil {
		return "", "", cliHandleUnknown, err
	}
	if pending {
		return "", "", cliHandlePending, nil
	}
	return "", "", cliHandleUnknown, nil
}

// LoginHit records one login request from key and returns how many the
// current fixed window has seen, including this one. now is the caller's
// clock (a test seam); window is the fixed window length. Windows an hour
// stale are swept on the way in so the table stays bounded by recent clients.
func (s *Store) LoginHit(key string, now time.Time, window time.Duration) (int, error) {
	if _, err := s.db.Exec(`DELETE FROM login_rate WHERE window_start < $1::timestamptz - interval '1 hour'`, now); err != nil {
		return 0, err
	}
	var hits int
	err := s.db.QueryRow(
		`INSERT INTO login_rate(key, window_start, hits) VALUES($1, $2, 1)
		 ON CONFLICT(key) DO UPDATE SET
		     hits = CASE WHEN login_rate.window_start <= $2::timestamptz - make_interval(secs => $3)
		                 THEN 1 ELSE login_rate.hits + 1 END,
		     window_start = CASE WHEN login_rate.window_start <= $2::timestamptz - make_interval(secs => $3)
		                         THEN $2::timestamptz ELSE login_rate.window_start END
		 RETURNING hits`,
		key, now, window.Seconds()).Scan(&hits)
	return hits, err
}
