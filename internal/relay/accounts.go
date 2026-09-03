package relay

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Account is a relay tenant. One account owns many agents.
type Account struct {
	ID          string
	Username    string
	GithubID    string // stable GitHub user id; "" for org accounts
	GithubLogin string // raw GitHub login, refreshed at every login; "" for org accounts
	Disabled    bool
}

// deriveUsername turns a GitHub login into a DNS-safe label component:
// lowercased, every rune outside [a-z0-9-] replaced by '-', trimmed of
// leading/trailing '-', and capped at 30 chars so the eventual
// "<hash>-<username>.<apex>" label stays under DNS's 63-char limit.
// (GitHub logins are already <= 39 chars of [A-Za-z0-9-], so this is
// nearly a lowercase passthrough.)
func deriveUsername(login string) string {
	login = strings.ToLower(login)
	var b strings.Builder
	for _, r := range login {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	u := strings.Trim(b.String(), "-")
	if u == "" {
		u = "user"
	}
	if len(u) > 30 {
		u = strings.Trim(u[:30], "-")
	}
	return u
}

// UpsertAccount returns the account for githubID, creating it (with a unique
// username derived from the GitHub login) on first sight. Idempotent by
// githubID.
func (s *Store) UpsertAccount(githubID, login string) (Account, error) {
	var acc Account
	var disabled bool
	var storedLogin sql.NullString
	err := s.db.QueryRow(
		`SELECT id, username, disabled, github_login FROM accounts WHERE github_id=$1`, githubID).
		Scan(&acc.ID, &acc.Username, &disabled, &storedLogin)
	if err == nil {
		acc.Disabled = disabled
		acc.GithubLogin = login
		if storedLogin.String != login {
			// GitHub logins can be renamed; keep the invite-matching login fresh.
			if _, err := s.db.Exec(`UPDATE accounts SET github_login=$1 WHERE id=$2`, login, acc.ID); err != nil {
				return Account{}, err
			}
		}
		return acc, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Account{}, err
	}

	base := deriveUsername(login)
	id := uuid.NewString()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for i := 1; ; i++ {
		username := base
		if i > 1 {
			username = base + "-" + strconv.Itoa(i)
		}
		_, err := s.db.Exec(
			`INSERT INTO accounts(id, github_id, github_login, username, type, disabled, created_at)
			 VALUES($1,$2,$3,$4,'user',false,$5)`,
			id, githubID, login, username, now)
		if err == nil {
			return Account{ID: id, Username: username, GithubLogin: login}, nil
		}
		if isUniqueViolation(err) {
			// Another account already holds this username; try the next suffix.
			// (A racing insert of the same github_id is vanishingly unlikely on a
			// single relay; the SELECT above handles the common re-login path.)
			continue
		}
		return Account{}, err
	}
}

// isUniqueViolation reports whether err is a Postgres unique-constraint
// failure (SQLSTATE 23505) — a primary key, UNIQUE column, or unique index.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// ErrBadCredential is returned for an unknown account credential or one whose
// account has been disabled by the operator kill-switch.
var ErrBadCredential = errors.New("bad credential")

// ErrUnknownAccount is returned when an operation names an account that does
// not exist in the store.
var ErrUnknownAccount = errors.New("unknown account")

// MintAccountCredential issues a fresh random credential for accountID and stores
// only its hash. The plaintext is returned once, to the caller.
func (s *Store) MintAccountCredential(accountID string) (string, error) {
	// Orgs are inert principals: they never hold credentials, so they can
	// never authenticate (belt-and-braces on top of the NULL github_id).
	var typ string
	if err := s.db.QueryRow(`SELECT type FROM accounts WHERE id=$1`, accountID).Scan(&typ); err != nil {
		return "", err
	}
	if typ != "user" {
		return "", errors.New("only user accounts hold credentials")
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	cred := hex.EncodeToString(raw)
	_, err := s.db.Exec(
		`INSERT INTO account_creds(token_hash, account_id, created_at) VALUES($1,$2,$3)`,
		hashToken(cred), accountID, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return "", err
	}
	return cred, nil
}

// AuthenticateAccount resolves a plaintext credential to its Account. A disabled
// account is treated as unauthenticated (ErrBadCredential).
func (s *Store) AuthenticateAccount(cred string) (Account, error) {
	var acc Account
	var disabled bool
	var gl, gid sql.NullString
	err := s.db.QueryRow(
		`SELECT a.id, a.username, a.github_id, a.github_login, a.disabled
		   FROM account_creds c JOIN accounts a ON a.id = c.account_id
		  WHERE c.token_hash = $1`, hashToken(cred)).
		Scan(&acc.ID, &acc.Username, &gid, &gl, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrBadCredential
	}
	if err != nil {
		return Account{}, err
	}
	if disabled {
		return Account{}, ErrBadCredential
	}
	acc.GithubID, acc.GithubLogin = gid.String, gl.String
	return acc, nil
}

// DisableAccount flips the kill-switch flag in the database for the account of
// type accountType ("user" or "org") holding username. New connects are then
// rejected at auth (Authenticate and AuthenticateAccount return a
// bad-credential error for a disabled account), and live tunnel sessions are
// evicted by the relay's per-session watchdog within one poll interval (see
// acceptTunnels). The operator trigger is the admin CLI:
// `piper-relay admin disable [--org] <name>`.
//
// The type is required because usernames are only unique per type (#411): a
// user and an org may both be called "acme", and severing one must not sever
// the other.
func (s *Store) DisableAccount(username, accountType string) error {
	res, err := s.db.Exec(
		`UPDATE accounts SET disabled=true WHERE username=$1 AND type=$2`, username, accountType)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrUnknownAccount
	}
	return nil
}

// Enrollment is the result of a self-service claim: an enrollment token, the
// single-label base domain the relay assigned the agent under the apex, and the
// secret the relay signs brokered webhook deliveries to this box with.
type Enrollment struct {
	Token         string
	BaseDomain    string
	WebhookSecret string
}

// ErrQuotaExceeded is returned when an account is already at its agent cap.
var ErrQuotaExceeded = errors.New("account agent quota exceeded")

// EnrollForAccount mints an enrollment token for an agent bound to accountID,
// assigning it "<hash>-<username>.<apex>". Enforces the per-account agent cap.
//
// A non-empty boxID makes the call idempotent per box: if (accountID, boxID)
// already has an agent row, the row is kept — same name, base domain, webhook
// secret, quota slot — and only the enrollment token rotates (the old token
// stops authenticating). An empty boxID keeps insert-per-call semantics for
// operator/legacy enrolls.
func (s *Store) EnrollForAccount(accountID, boxID string) (Enrollment, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Enrollment{}, err
	}
	defer tx.Rollback()

	// Locking the account row serializes the cap check and the insert across
	// every relay process sharing this database: a second enroll for the same
	// account waits here until this one commits, then counts the new row.
	var username string
	if err := tx.QueryRow(`SELECT username FROM accounts WHERE id=$1 FOR UPDATE`, accountID).Scan(&username); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Enrollment{}, ErrUnknownAccount
		}
		return Enrollment{}, err
	}

	if boxID != "" {
		var base, secret string
		err := tx.QueryRow(
			`SELECT base_domain, webhook_secret FROM agents WHERE account_id=$1 AND box_id=$2`,
			accountID, boxID).Scan(&base, &secret)
		if err == nil {
			raw := make([]byte, 32)
			if _, err := rand.Read(raw); err != nil {
				return Enrollment{}, err
			}
			tok := hex.EncodeToString(raw)
			if _, err := tx.Exec(
				`UPDATE agents SET token_hash=$1 WHERE account_id=$2 AND box_id=$3`,
				hashToken(tok), accountID, boxID); err != nil {
				return Enrollment{}, err
			}
			if err := tx.Commit(); err != nil {
				return Enrollment{}, err
			}
			return Enrollment{Token: tok, BaseDomain: base, WebhookSecret: secret}, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return Enrollment{}, err
		}
	}

	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM agents WHERE account_id=$1`, accountID).Scan(&count); err != nil {
		return Enrollment{}, err
	}
	if count >= s.maxAgentsOrDefault() {
		return Enrollment{}, ErrQuotaExceeded
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	for attempt := 0; attempt < 5; attempt++ {
		hash := make([]byte, 4)
		if _, err := rand.Read(hash); err != nil {
			return Enrollment{}, err
		}
		base := hex.EncodeToString(hash) + "-" + username + "." + s.apexOrDefault()

		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return Enrollment{}, err
		}
		tok := hex.EncodeToString(raw)

		rawSecret := make([]byte, 32)
		if _, err := rand.Read(rawSecret); err != nil {
			return Enrollment{}, err
		}
		secret := hex.EncodeToString(rawSecret)

		if _, err := tx.Exec(`SAVEPOINT try`); err != nil {
			return Enrollment{}, err
		}
		_, err := tx.Exec(
			`INSERT INTO agents(name, token_hash, base_domain, account_id, box_id, webhook_secret, created_at)
			 VALUES($1,$2,$3,$4,$5,$6,$7)`,
			base, hashToken(tok), base, accountID, nullIfEmpty(boxID), secret, now)
		if err == nil {
			if err := tx.Commit(); err != nil {
				return Enrollment{}, err
			}
			return Enrollment{Token: tok, BaseDomain: base, WebhookSecret: secret}, nil
		}
		if isUniqueViolation(err) {
			// A failed statement aborts a Postgres transaction; roll back to
			// the savepoint so the next attempt's INSERT can run.
			if _, err := tx.Exec(`ROLLBACK TO SAVEPOINT try`); err != nil {
				return Enrollment{}, err
			}
			continue // hash collided with an existing base_domain; retry
		}
		return Enrollment{}, err
	}
	return Enrollment{}, errors.New("could not assign a unique base domain")
}

// nullIfEmpty stores an absent box_id as NULL, keeping the partial unique
// index's WHERE clause and this column's "no box identity" reading aligned.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
