// Package store persists Piper apps and deployments in SQLite (pure-Go driver).
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

var ErrNotFound = errors.New("not found")

type App struct {
	Name      string
	Port      int
	Repo      string
	Branch    string
	RootDir   string // build subpath for monorepos; "" builds the repo root
	Hostname  string
	CreatedAt time.Time
}

type Deployment struct {
	ID          string
	App         string
	PR          int
	ImageID     string
	ContainerID string
	HostPort    int
	Status      string
	// Hostname is a preview's URL. Production's lives on the app row: hostnames
	// are keyed (agent, app, pr), so one apps column cannot hold both (#478).
	Hostname  string
	CreatedAt time.Time
}

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	// busy_timeout makes a second writer (e.g. `piperd token create` run
	// against a live daemon's piper.db) wait for the lock instead of
	// failing immediately with SQLITE_BUSY. journal_mode(WAL) lets readers
	// proceed while a writer holds the write lock (#422).
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	// One connection: a larger pool opens several connections to the same
	// file and they race each other to the busy timeout (#422).
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) CreateApp(name string, port int) (App, error) {
	now := time.Now().UTC()
	_, err := s.db.Exec(`INSERT INTO apps(name, port, repo, branch, created_at) VALUES(?,?,?,?,?)`,
		name, port, "", "", now.Format(time.RFC3339Nano))
	if err != nil {
		return App{}, err
	}
	return App{Name: name, Port: port, CreatedAt: now}, nil
}

func (s *Store) GetApp(name string) (App, error) {
	return s.scanApp(s.db.QueryRow(
		`SELECT name, port, repo, branch, root_dir, hostname, created_at FROM apps WHERE name=?`, name))
}

func (s *Store) AppByRepo(repo string) (App, error) {
	return s.scanApp(s.db.QueryRow(
		`SELECT name, port, repo, branch, root_dir, hostname, created_at FROM apps WHERE repo=?`, repo))
}

// SetAppHostname records the public hostname the app is currently routed on —
// relay-assigned when relay-terminated, "<app>.<baseDom>" for LAN/BYO. The
// Deployer writes it on each successful deploy. ErrNotFound if the app is gone.
func (s *Store) SetAppHostname(name, hostname string) error {
	res, err := s.db.Exec(`UPDATE apps SET hostname=? WHERE name=?`, hostname, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) UpdateAppRepo(name, repo, branch, rootDir string) error {
	res, err := s.db.Exec(`UPDATE apps SET repo=?, branch=?, root_dir=? WHERE name=?`, repo, branch, rootDir, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteApp removes the app and its entire deployment history in one
// transaction — the single exception to deployment rows never being deleted
// (they exist as history only while their app does). ErrNotFound when the
// app doesn't exist.
func (s *Store) DeleteApp(name string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM deployments WHERE app=?`, name); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM app_domains WHERE app=?`, name); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM app_env WHERE app=?`, name); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM apps WHERE name=?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

type rowScanner interface{ Scan(dest ...any) error }

func (s *Store) scanApp(row rowScanner) (App, error) {
	var a App
	var ts string
	err := row.Scan(&a.Name, &a.Port, &a.Repo, &a.Branch, &a.RootDir, &a.Hostname, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return App{}, ErrNotFound
	}
	if err != nil {
		return App{}, err
	}
	a.CreatedAt, _ = time.Parse(time.RFC3339Nano, ts)
	return a, nil
}

func (s *Store) ListApps() ([]App, error) {
	rows, err := s.db.Query(`SELECT name, port, repo, branch, root_dir, hostname, created_at FROM apps ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []App
	for rows.Next() {
		a, err := s.scanApp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// logRetentionPerApp bounds stored log blobs: only the newest N deployments
// per app keep their logs. Rows themselves are never deleted — they are the
// deployment history — except by DeleteApp, which removes the app wholesale.
const logRetentionPerApp = 20

func (s *Store) CreateDeployment(app, imageID, containerID string, hostPort int, status, logs string) (Deployment, error) {
	d := Deployment{
		ID: uuid.NewString(), App: app, ImageID: imageID,
		ContainerID: containerID, HostPort: hostPort, Status: status,
		CreatedAt: time.Now().UTC(),
	}
	_, err := s.db.Exec(
		`INSERT INTO deployments(id, app, image_id, container_id, host_port, status, logs, created_at)
		 VALUES(?,?,?,?,?,?,?,?)`,
		d.ID, d.App, d.ImageID, d.ContainerID, d.HostPort, d.Status, logs,
		d.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return Deployment{}, err
	}
	return d, s.pruneDeploymentLogs(app)
}

func (s *Store) UpdateDeploymentStatus(id, status string) error {
	_, err := s.db.Exec(`UPDATE deployments SET status=? WHERE id=?`, status, id)
	return err
}

// UpdateDeploymentLogs overwrites one deployment's captured log. Used to grow
// a building row's log as the build streams.
func (s *Store) UpdateDeploymentLogs(id, logs string) error {
	_, err := s.db.Exec(`UPDATE deployments SET logs=? WHERE id=?`, logs, id)
	return err
}

// FinalizeDeployment fills in a building row's real image/container/port and
// flips its status to running/failed, writing the complete log in one update.
func (s *Store) FinalizeDeployment(id, imageID, containerID string, hostPort int, status, logs string) error {
	_, err := s.db.Exec(
		`UPDATE deployments SET image_id=?, container_id=?, host_port=?, status=?, logs=? WHERE id=?`,
		imageID, containerID, hostPort, status, logs, id)
	return err
}

// CountBuildingDeployments reports how many deployments are mid-build. The
// enrollment apply refuses to restart piperd while one is in flight (409
// busy), so a Docker build is never killed halfway (see #158 for why a
// surviving "building" row is poison).
func (s *Store) CountBuildingDeployments() (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM deployments WHERE status = 'building'`).Scan(&n)
	return n, err
}

// FailBuildingDeployments flips every deployment still in "building" to
// "failed", appending an abort note to each row's captured log. Called on
// graceful shutdown: an in-flight build's goroutine cannot outlive the process,
// so its row would otherwise survive as a permanent "building" (#158). Returns
// the number of rows changed.
func (s *Store) FailBuildingDeployments() (int64, error) {
	res, err := s.db.Exec(
		`UPDATE deployments SET status='failed', logs = logs || ? WHERE status='building'`,
		"\n[deploy aborted: piperd shut down]\n")
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

type GitHubApp struct {
	AppID         int64
	Slug          string
	PrivateKey    string
	WebhookSecret string
}

func (s *Store) SaveGitHubApp(a GitHubApp) error {
	_, err := s.db.Exec(
		`INSERT INTO github_app(id, app_id, slug, private_key, webhook_secret) VALUES(1,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET app_id=excluded.app_id, slug=excluded.slug,
		   private_key=excluded.private_key, webhook_secret=excluded.webhook_secret`,
		a.AppID, a.Slug, a.PrivateKey, a.WebhookSecret)
	return err
}

func (s *Store) GetGitHubApp() (GitHubApp, error) {
	var a GitHubApp
	err := s.db.QueryRow(`SELECT app_id, slug, private_key, webhook_secret FROM github_app WHERE id=1`).
		Scan(&a.AppID, &a.Slug, &a.PrivateKey, &a.WebhookSecret)
	if errors.Is(err, sql.ErrNoRows) {
		return GitHubApp{}, ErrNotFound
	}
	return a, err
}

// DeleteGitHubApp drops this box's own GitHub App, so a stored row stops
// shadowing a relay's brokered App (#299). Deleting nothing is not an error.
func (s *Store) DeleteGitHubApp() error {
	_, err := s.db.Exec(`DELETE FROM github_app WHERE id=1`)
	return err
}

// Ordered deployment queries key on rowid (monotonic insertion order), not the
// created_at text: RFC3339Nano drops trailing zeros, so its lexical order is
// not chronological and equal timestamps have no tiebreaker (#109).
func (s *Store) LatestRunning(app string) (Deployment, error) {
	var d Deployment
	var ts string
	err := s.db.QueryRow(
		`SELECT id, app, image_id, container_id, host_port, status, created_at
		 FROM deployments WHERE app=? AND status='running' AND pr=0
		 ORDER BY rowid DESC LIMIT 1`, app).
		Scan(&d.ID, &d.App, &d.ImageID, &d.ContainerID, &d.HostPort, &d.Status, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if err != nil {
		return Deployment{}, err
	}
	d.CreatedAt, _ = time.Parse(time.RFC3339Nano, ts)
	return d, nil
}

// LatestDeployment returns the newest non-preview deployment for app,
// whatever its status — the app's production deploy state. PR previews
// (pr > 0) never color it. ErrNotFound when the app was never deployed.
func (s *Store) LatestDeployment(app string) (Deployment, error) {
	var d Deployment
	var ts string
	err := s.db.QueryRow(
		`SELECT id, app, image_id, container_id, host_port, status, created_at
		 FROM deployments WHERE app=? AND pr=0
		 ORDER BY rowid DESC LIMIT 1`, app).
		Scan(&d.ID, &d.App, &d.ImageID, &d.ContainerID, &d.HostPort, &d.Status, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if err != nil {
		return Deployment{}, err
	}
	d.CreatedAt, _ = time.Parse(time.RFC3339Nano, ts)
	return d, nil
}

func (s *Store) CreatePreviewDeployment(app string, pr int, imageID, containerID string, hostPort int, status, logs string) (Deployment, error) {
	d := Deployment{
		ID: uuid.NewString(), App: app, PR: pr, ImageID: imageID,
		ContainerID: containerID, HostPort: hostPort, Status: status,
		CreatedAt: time.Now().UTC(),
	}
	_, err := s.db.Exec(
		`INSERT INTO deployments(id, app, image_id, container_id, host_port, status, logs, created_at, pr)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		d.ID, d.App, d.ImageID, d.ContainerID, d.HostPort, d.Status, logs,
		d.CreatedAt.Format(time.RFC3339Nano), d.PR)
	if err != nil {
		return Deployment{}, err
	}
	return d, s.pruneDeploymentLogs(app)
}

func (s *Store) PreviewRunning(app string, pr int) (Deployment, error) {
	var d Deployment
	var ts string
	err := s.db.QueryRow(
		`SELECT id, app, image_id, container_id, host_port, status, created_at, pr
		 FROM deployments WHERE app=? AND pr=? AND status='running'
		 ORDER BY rowid DESC LIMIT 1`, app, pr).
		Scan(&d.ID, &d.App, &d.ImageID, &d.ContainerID, &d.HostPort, &d.Status, &ts, &d.PR)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if err != nil {
		return Deployment{}, err
	}
	d.CreatedAt, _ = time.Parse(time.RFC3339Nano, ts)
	return d, nil
}

// RunningPreviews returns the newest running preview deployment for every
// (app, pr) pair on the box. LatestRunning is deliberately pr=0 only, which
// left previews invisible to the restart and reconnect paths that rebuild
// routing (#376): a live preview URL went blank on a piperd restart and stayed
// blank until the PR pushed again. Superseded rows are filtered by taking the
// highest rowid per pair, so a redeployed preview never resurfaces its dead
// predecessor's port.
func (s *Store) RunningPreviews() ([]Deployment, error) {
	rows, err := s.db.Query(
		`SELECT id, app, image_id, container_id, host_port, status, created_at, pr
		 FROM deployments WHERE pr > 0 AND status='running'
		   AND rowid IN (SELECT MAX(rowid) FROM deployments
		                 WHERE pr > 0 AND status='running' GROUP BY app, pr)
		 ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var deps []Deployment
	for rows.Next() {
		var d Deployment
		var ts string
		if err := rows.Scan(&d.ID, &d.App, &d.ImageID, &d.ContainerID, &d.HostPort, &d.Status, &ts, &d.PR); err != nil {
			return nil, err
		}
		d.CreatedAt, _ = time.Parse(time.RFC3339Nano, ts)
		deps = append(deps, d)
	}
	return deps, rows.Err()
}

// SetPreviewHostname records the URL app's PR pr is served at, on the live
// preview row — the same row RunningPreviews announces. Older rows for that PR
// keep the hostname they served, so a torn-down preview's history survives a
// relay rename.
func (s *Store) SetPreviewHostname(app string, pr int, hostname string) error {
	_, err := s.db.Exec(
		`UPDATE deployments SET hostname=? WHERE rowid=(
		   SELECT MAX(rowid) FROM deployments
		   WHERE app=? AND pr=? AND status='running')`, hostname, app, pr)
	return err
}

func (s *Store) pruneDeploymentLogs(app string) error {
	_, err := s.db.Exec(
		`UPDATE deployments SET logs='' WHERE app=? AND logs != '' AND id NOT IN (
		   SELECT id FROM deployments WHERE app=? ORDER BY rowid DESC LIMIT ?)`,
		app, app, logRetentionPerApp)
	return err
}

// ListDeployments returns every deployment for app (previews included),
// newest first.
func (s *Store) ListDeployments(app string) ([]Deployment, error) {
	rows, err := s.db.Query(
		`SELECT id, app, pr, image_id, container_id, host_port, status, hostname, created_at
		 FROM deployments WHERE app=? ORDER BY rowid DESC`, app)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Deployment
	for rows.Next() {
		var d Deployment
		var ts string
		if err := rows.Scan(&d.ID, &d.App, &d.PR, &d.ImageID, &d.ContainerID, &d.HostPort, &d.Status, &d.Hostname, &ts); err != nil {
			return nil, err
		}
		d.CreatedAt, _ = time.Parse(time.RFC3339Nano, ts)
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeploymentLogs returns the captured log for one deployment, scoped by app
// so an id from another app is ErrNotFound. Empty string when the log was
// pruned by retention.
func (s *Store) DeploymentLogs(app, id string) (string, error) {
	var logs string
	err := s.db.QueryRow(
		`SELECT logs FROM deployments WHERE app=? AND id=?`, app, id).Scan(&logs)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return logs, err
}

// ErrBadToken is returned when a token is unknown or has been revoked.
var ErrBadToken = errors.New("bad token")

type Token struct {
	ID        string
	Label     string
	Scope     string
	CreatedAt time.Time
	RevokedAt *time.Time
}

// hashToken is the at-rest representation of an API token. Duplicated from
// internal/relay (store must not import relay); it is a three-line helper.
func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// CreateToken mints a random token with the given label and scope, stores only
// its hash, and returns the plaintext once.
func (s *Store) CreateToken(label, scope string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(raw)
	_, err := s.db.Exec(
		`INSERT INTO tokens(id, label, token_hash, scope, created_at) VALUES(?,?,?,?,?)`,
		uuid.NewString(), label, hashToken(tok), scope,
		time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return "", err
	}
	return tok, nil
}

// AuthenticateToken resolves a plaintext token to its Token, or ErrBadToken if
// the token is unknown or revoked.
func (s *Store) AuthenticateToken(tok string) (Token, error) {
	var t Token
	var created string
	var revoked sql.NullString
	err := s.db.QueryRow(
		`SELECT id, label, scope, created_at, revoked_at FROM tokens WHERE token_hash=?`,
		hashToken(tok)).Scan(&t.ID, &t.Label, &t.Scope, &created, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return Token{}, ErrBadToken
	}
	if err != nil {
		return Token{}, err
	}
	if revoked.Valid {
		return Token{}, ErrBadToken
	}
	t.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	return t, nil
}

// ListTokens returns all tokens (metadata only; never the plaintext or hash).
// rowid preserves creation order because created_at is RFC3339Nano text whose
// trimmed fractional seconds do not sort chronologically as text.
func (s *Store) ListTokens() ([]Token, error) {
	rows, err := s.db.Query(
		`SELECT id, label, scope, created_at, revoked_at FROM tokens ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		var t Token
		var created string
		var revoked sql.NullString
		if err := rows.Scan(&t.ID, &t.Label, &t.Scope, &created, &revoked); err != nil {
			return nil, err
		}
		t.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		if revoked.Valid {
			rt, _ := time.Parse(time.RFC3339Nano, revoked.String)
			t.RevokedAt = &rt
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeToken marks the token with the given label revoked. ErrNotFound if no
// active token with that label exists.
func (s *Store) RevokeToken(label string) error {
	res, err := s.db.Exec(
		`UPDATE tokens SET revoked_at=? WHERE label=? AND revoked_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339Nano), label)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteToken hard-deletes the token with the given label. It exists only to
// unwind a relay-provisioning push that failed after mint (cmd/piperd), so the
// next connect can retry. Owner-facing revocation is RevokeToken — soft, so the
// revoked row remains as the "never re-provision" marker.
func (s *Store) DeleteToken(label string) error {
	_, err := s.db.Exec(`DELETE FROM tokens WHERE label=?`, label)
	return err
}

// DomainConfig is the box's BYO custom-domain config (single row). DNSToken is
// a secret: it is stored for issuance and must never leave the box via the API.
type DomainConfig struct {
	Domain       string
	DNSProvider  string
	DNSToken     string
	Status       string // "issuing" | "active" | "failed"
	Error        string
	CertNotAfter time.Time
	UpdatedAt    time.Time
}

// SetDomainConfig upserts the custom-domain config, resetting it to a fresh
// "issuing" state.
func (s *Store) SetDomainConfig(domain, provider, token string) error {
	_, err := s.db.Exec(
		`INSERT INTO domain_config(id, domain, dns_provider, dns_token, status, error, cert_not_after, updated_at)
		 VALUES(1,?,?,?,'issuing','','',?)
		 ON CONFLICT(id) DO UPDATE SET domain=excluded.domain,
		   dns_provider=excluded.dns_provider, dns_token=excluded.dns_token,
		   status='issuing', error='', cert_not_after='', updated_at=excluded.updated_at`,
		domain, provider, token, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// GetDomainConfig returns the config, or ErrNotFound when no domain is set.
func (s *Store) GetDomainConfig() (DomainConfig, error) {
	var dc DomainConfig
	var notAfter, updated string
	err := s.db.QueryRow(
		`SELECT domain, dns_provider, dns_token, status, error, cert_not_after, updated_at
		 FROM domain_config WHERE id=1`).
		Scan(&dc.Domain, &dc.DNSProvider, &dc.DNSToken, &dc.Status, &dc.Error, &notAfter, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return DomainConfig{}, ErrNotFound
	}
	if err != nil {
		return DomainConfig{}, err
	}
	if notAfter != "" {
		dc.CertNotAfter, _ = time.Parse(time.RFC3339Nano, notAfter)
	}
	dc.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return dc, nil
}

// UpdateDomainStatus records the outcome of an issuance/renewal step for
// domain. The update is conditional on the stored domain so a run acting on a
// stale snapshot (the config was replaced or removed while ACME was in flight)
// can never stamp another domain's row: ErrNotFound when no row matches. A
// zero notAfter stores the empty string.
func (s *Store) UpdateDomainStatus(domain, status, errMsg string, notAfter time.Time) error {
	na := ""
	if !notAfter.IsZero() {
		na = notAfter.UTC().Format(time.RFC3339Nano)
	}
	res, err := s.db.Exec(
		`UPDATE domain_config SET status=?, error=?, cert_not_after=?, updated_at=? WHERE id=1 AND domain=?`,
		status, errMsg, na, time.Now().UTC().Format(time.RFC3339Nano), domain)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteDomainConfig removes the custom-domain config. Deleting an absent
// config is not an error.
func (s *Store) DeleteDomainConfig() error {
	_, err := s.db.Exec(`DELETE FROM domain_config WHERE id=1`)
	return err
}

// ErrDomainExists is returned when a domain is already registered to an app.
var ErrDomainExists = errors.New("domain already exists")

// AppDomain is a per-app custom domain. Domains are globally unique on the box
// (one domain maps to exactly one app).
type AppDomain struct {
	Domain       string
	App          string
	Status       string // "" | "pending" | "issuing" | "active" | "failed"
	Error        string
	CertNotAfter time.Time
	UpdatedAt    time.Time
}

// AddAppDomain registers domain for app. ErrDomainExists if the domain is
// already held by any app (including the same one).
func (s *Store) AddAppDomain(domain, app string) error {
	_, err := s.db.Exec(
		`INSERT INTO app_domains(domain, app, status, error, cert_not_after, updated_at)
		 VALUES(?,?,?,?,?,?)`,
		domain, app, "", "", "", time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return ErrDomainExists
	}
	return err
}

// GetAppDomain returns the app-domain row, or ErrNotFound.
func (s *Store) GetAppDomain(domain string) (AppDomain, error) {
	var d AppDomain
	var notAfter, updated string
	err := s.db.QueryRow(
		`SELECT domain, app, status, error, cert_not_after, updated_at FROM app_domains WHERE domain=?`, domain).
		Scan(&d.Domain, &d.App, &d.Status, &d.Error, &notAfter, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return AppDomain{}, ErrNotFound
	}
	if err != nil {
		return AppDomain{}, err
	}
	if notAfter != "" {
		d.CertNotAfter, _ = time.Parse(time.RFC3339Nano, notAfter)
	}
	d.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return d, nil
}

// ListAppDomains returns every domain registered for app, ordered by domain.
func (s *Store) ListAppDomains(app string) ([]AppDomain, error) {
	rows, err := s.db.Query(
		`SELECT domain, app, status, error, cert_not_after, updated_at
		 FROM app_domains WHERE app=? ORDER BY domain`, app)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanAppDomains(rows)
}

// AllAppDomains returns every per-app custom domain regardless of status,
// ordered by domain — the domain manager's restart-resume sweep.
func (s *Store) AllAppDomains() ([]AppDomain, error) {
	rows, err := s.db.Query(
		`SELECT domain, app, status, error, cert_not_after, updated_at
		 FROM app_domains ORDER BY domain`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanAppDomains(rows)
}

// ListActiveAppDomains returns every domain with status='active', ordered by domain.
func (s *Store) ListActiveAppDomains() ([]AppDomain, error) {
	rows, err := s.db.Query(
		`SELECT domain, app, status, error, cert_not_after, updated_at
		 FROM app_domains WHERE status='active' ORDER BY domain`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanAppDomains(rows)
}

func (s *Store) scanAppDomains(rows *sql.Rows) ([]AppDomain, error) {
	var out []AppDomain
	for rows.Next() {
		var d AppDomain
		var notAfter, updated string
		if err := rows.Scan(&d.Domain, &d.App, &d.Status, &d.Error, &notAfter, &updated); err != nil {
			return nil, err
		}
		if notAfter != "" {
			d.CertNotAfter, _ = time.Parse(time.RFC3339Nano, notAfter)
		}
		d.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		out = append(out, d)
	}
	return out, rows.Err()
}

// UpdateAppDomainStatus records the outcome of an issuance/renewal step for
// domain. ErrNotFound when no row matches. A zero notAfter stores the empty string.
func (s *Store) UpdateAppDomainStatus(domain, status, errMsg string, notAfter time.Time) error {
	na := ""
	if !notAfter.IsZero() {
		na = notAfter.UTC().Format(time.RFC3339Nano)
	}
	res, err := s.db.Exec(
		`UPDATE app_domains SET status=?, error=?, cert_not_after=?, updated_at=? WHERE domain=?`,
		status, errMsg, na, time.Now().UTC().Format(time.RFC3339Nano), domain)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteAppDomain removes a per-app custom domain. Deleting an absent domain
// is not an error.
func (s *Store) DeleteAppDomain(domain string) error {
	_, err := s.db.Exec(`DELETE FROM app_domains WHERE domain=?`, domain)
	return err
}

// SetAppEnv saves one env var for app, overwriting any existing value. The
// caller (api) validates the key and gates on app existence; the table's FK is
// documentation, not enforcement.
func (s *Store) SetAppEnv(app, key, value string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.Exec(`INSERT INTO app_env(app, key, value, updated_at) VALUES(?,?,?,?)
		ON CONFLICT(app, key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, app, key, value, now)
	return err
}

// DeleteAppEnv removes one env var; deleting an absent key is a no-op.
func (s *Store) DeleteAppEnv(app, key string) error {
	_, err := s.db.Exec(`DELETE FROM app_env WHERE app=? AND key=?`, app, key)
	return err
}

// AppEnv returns app's env vars; an empty (non-nil) map when it has none.
func (s *Store) AppEnv(app string) (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM app_env WHERE app=?`, app)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	env := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		env[k] = v
	}
	return env, rows.Err()
}

// AppEnvWithTimestamps returns app's env vars and the time each var was last
// updated; an empty (non-nil) map when it has none.
func (s *Store) AppEnvWithTimestamps(app string) (map[string]string, map[string]time.Time, error) {
	rows, err := s.db.Query(`SELECT key, value, updated_at FROM app_env WHERE app=?`, app)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	env := map[string]string{}
	updated := map[string]time.Time{}
	for rows.Next() {
		var k, v, ts string
		if err := rows.Scan(&k, &v, &ts); err != nil {
			return nil, nil, err
		}
		env[k] = v
		updated[k], _ = time.Parse(time.RFC3339Nano, ts)
	}
	return env, updated, rows.Err()
}
