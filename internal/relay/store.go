// Package relay is the cloud-side SNI-passthrough tunnel server. It authenticates
// agents by per-agent token and routes public :443 traffic down the matching
// tunnel by SNI. It never decrypts traffic.
package relay

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

//go:embed schema.sql
var schema string

var ErrBadToken = errors.New("bad token")

type Agent struct {
	Name       string
	BaseDomain string
}

type Store struct {
	// dsn is what listen dials for LISTEN; the pool cannot.
	dsn        string
	db         *sql.DB
	apex       string
	maxAgents  int
	maxApps    int
	maxDomains int
	nowFunc    func() time.Time
}

// Configure sets the free-tier apex, the per-account agent cap (EnrollForAccount),
// the per-account app cap (RegisterHostname), and the per-agent custom-domain
// cap (AddCustomDomain). Safe to call once after Open.
func (s *Store) Configure(apex string, maxAgents, maxApps, maxDomains int) {
	s.apex = apex
	s.maxAgents = maxAgents
	s.maxApps = maxApps
	s.maxDomains = maxDomains
}

func (s *Store) maxAppsOrDefault() int {
	if s.maxApps <= 0 {
		return 10
	}
	return s.maxApps
}

func (s *Store) apexOrDefault() string {
	if s.apex == "" {
		return "public.getpiper.dev"
	}
	return s.apex
}

func (s *Store) maxAgentsOrDefault() int {
	if s.maxAgents <= 0 {
		return 3
	}
	return s.maxAgents
}

// Open connects to the Postgres database at dsn (a postgres:// URL) and
// applies schema.sql. Several relay processes may share one database; the
// store relies on row locks, not on being the only writer.
func Open(dsn string) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect: %w", err)
	}
	// No arguments ⇒ pgx uses the simple protocol, which accepts the
	// multi-statement schema in one round trip.
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db, dsn: dsn, nowFunc: time.Now}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// Enroll mints a random token for a new agent bound to baseDomain and stores
// only its hash. The plaintext token is returned once, to the operator.
func (s *Store) Enroll(name, baseDomain string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(raw)
	_, err := s.db.Exec(
		`INSERT INTO agents(name, token_hash, base_domain, created_at) VALUES($1,$2,$3,$4)`,
		name, hashToken(tok), baseDomain, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return "", err
	}
	return tok, nil
}

// Authenticate resolves a plaintext token to its Agent, or ErrBadToken. An agent
// whose owning account has been disabled is rejected as ErrBadToken.
func (s *Store) Authenticate(token string) (Agent, error) {
	var ag Agent
	var disabled sql.NullBool
	err := s.db.QueryRow(
		`SELECT ag.name, ag.base_domain, acc.disabled
		   FROM agents ag LEFT JOIN accounts acc ON acc.id = ag.account_id
		  WHERE ag.token_hash = $1`, hashToken(token)).
		Scan(&ag.Name, &ag.BaseDomain, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return Agent{}, ErrBadToken
	}
	if err != nil {
		return Agent{}, err
	}
	if disabled.Valid && disabled.Bool {
		return Agent{}, ErrBadToken
	}
	return ag, nil
}

// SetControlToken stores the plaintext control-API bearer the box pushed for
// this enrollment. Plaintext by necessity: the relay must present it verbatim
// on forwarded control requests (see the control-stream routing design).
func (s *Store) SetControlToken(baseDomain, token string) error {
	res, err := s.db.Exec(`UPDATE agents SET control_token=$1 WHERE base_domain=$2`, token, baseDomain)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrBadToken
	}
	return nil
}

// ControlToken returns the stored control bearer for baseDomain, "" if the box
// never provisioned one. Unknown agents are ErrBadToken.
func (s *Store) ControlToken(baseDomain string) (string, error) {
	var tok sql.NullString
	err := s.db.QueryRow(`SELECT control_token FROM agents WHERE base_domain=$1`, baseDomain).Scan(&tok)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrBadToken
	}
	if err != nil {
		return "", err
	}
	return tok.String, nil
}

// AgentWebhookSecret returns the secret the relay signs brokered webhook
// deliveries to agentName with. Unknown agents are ErrBadToken.
func (s *Store) AgentWebhookSecret(agentName string) (string, error) {
	var sec sql.NullString
	err := s.db.QueryRow(`SELECT webhook_secret FROM agents WHERE name=$1`, agentName).Scan(&sec)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrBadToken
	}
	if err != nil {
		return "", err
	}
	return sec.String, nil
}

// ErrDomainTaken is returned when another agent already holds a custom domain.
var ErrDomainTaken = errors.New("domain already in use")

// ErrInvalidDomain is returned for a custom domain that is not a lowercase
// dotted DNS name.
var ErrInvalidDomain = errors.New("invalid domain")

// ErrDomainReserved is returned when a custom domain would collide with the
// relay apex or an enrolled agent's base domain.
var ErrDomainReserved = errors.New("domain conflicts with a relay-managed domain")

// customDomainRE accepts lowercase dotted DNS names, mirroring the agent-side
// check in internal/domain. Anything else — including uppercase, so
// case-games cannot dodge the overlap checks below — is rejected.
var customDomainRE = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+[a-z][a-z0-9-]*[a-z0-9]$`)

// dnsOverlap reports whether two DNS names are equal or one is a label-suffix
// of the other: "x.example.com" overlaps "example.com", "xexample.com" does not.
func dnsOverlap(a, b string) bool {
	return a == b || strings.HasSuffix(a, "."+b) || strings.HasSuffix(b, "."+a)
}

// domainClaimable rejects a custom domain that would collide with the relay's
// own namespace: the apex (which also covers api.<apex> and every assigned
// hostname under it) or ANY enrolled agent's base domain, in either suffix
// direction. Domain control ops are authenticated but their Domain value is
// attacker-controlled on a compromised box; without this check an agent could
// splice another agent's SNI — or the relay control plane — to itself.
func (s *Store) domainClaimable(domain string) error {
	if dnsOverlap(domain, strings.ToLower(s.apexOrDefault())) {
		return ErrDomainReserved
	}
	rows, err := s.db.Query(`SELECT base_domain FROM agents`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var base string
		if err := rows.Scan(&base); err != nil {
			return err
		}
		if dnsOverlap(domain, strings.ToLower(base)) {
			return ErrDomainReserved
		}
	}
	return rows.Err()
}
