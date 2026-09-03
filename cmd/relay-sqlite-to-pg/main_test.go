package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/piperbox/piper/internal/relay"
	"github.com/piperbox/piper/internal/relay/relaytest"
	_ "modernc.org/sqlite"
)

func TestMain(m *testing.M) { os.Exit(relaytest.Main(m)) }

// The production relay.db as of v0.18.0 — the shape this copier reads.
const oldSchema = `
CREATE TABLE agents (name TEXT PRIMARY KEY, token_hash TEXT NOT NULL UNIQUE, base_domain TEXT NOT NULL,
  account_id TEXT, box_id TEXT, control_token TEXT, webhook_secret TEXT, created_at TEXT NOT NULL);
CREATE TABLE accounts (id TEXT PRIMARY KEY, github_id TEXT UNIQUE, github_login TEXT, username TEXT NOT NULL,
  type TEXT NOT NULL DEFAULT 'user', disabled INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, UNIQUE(username, type));
CREATE TABLE account_creds (token_hash TEXT PRIMARY KEY, account_id TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE hostnames (hostname TEXT PRIMARY KEY, agent_name TEXT NOT NULL, account_id TEXT NOT NULL, app TEXT NOT NULL,
  pr INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, UNIQUE(agent_name, app, pr));
CREATE TABLE org_members (org_id TEXT NOT NULL, account_id TEXT NOT NULL, role TEXT NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY (org_id, account_id));
CREATE TABLE org_invites (org_id TEXT NOT NULL, github_login TEXT NOT NULL, invited_by TEXT NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY (org_id, github_login));
CREATE TABLE custom_domains (domain TEXT PRIMARY KEY, agent_base TEXT NOT NULL, status TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE github_installations (installation_id TEXT PRIMARY KEY, account_id TEXT NOT NULL, target_type TEXT NOT NULL, target_login TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE repo_bindings (agent_name TEXT NOT NULL, app TEXT NOT NULL, repo TEXT NOT NULL, branch TEXT NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY (agent_name, app));
CREATE TABLE pending_events (agent_name TEXT NOT NULL, app TEXT NOT NULL, ref TEXT NOT NULL, event TEXT NOT NULL, payload BLOB NOT NULL,
  created_at TEXT NOT NULL, attempts INTEGER NOT NULL, next_try_at TEXT NOT NULL, PRIMARY KEY (agent_name, app, ref));
`

func TestCopyAllRoundTrips(t *testing.T) {
	src, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if _, err := src.Exec(oldSchema); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`INSERT INTO accounts VALUES('acc-1','gh-1','alice','alice','user',1,'2026-01-01T00:00:00Z')`,
		`INSERT INTO accounts VALUES('org-1',NULL,NULL,'acme','org',0,'2026-01-02T00:00:00Z')`,
		`INSERT INTO account_creds VALUES('h1','acc-1','2026-01-01T00:00:00Z')`,
		`INSERT INTO agents VALUES('a.example','th','a.example','acc-1','box-1','ctl','whs','2026-01-03T00:00:00Z')`,
		`INSERT INTO hostnames VALUES('x-alice.example','a.example','acc-1','blog',0,'2026-01-04T00:00:00Z')`,
		`INSERT INTO org_members VALUES('org-1','acc-1','owner','2026-01-02T00:00:00Z')`,
		`INSERT INTO org_invites VALUES('org-1','bob','acc-1','2026-01-05T00:00:00Z')`,
		`INSERT INTO custom_domains VALUES('shop.dev','a.example','active','2026-01-06T00:00:00Z')`,
		`INSERT INTO github_installations VALUES('inst-1','acc-1','User','alice','2026-01-07T00:00:00Z')`,
		`INSERT INTO repo_bindings VALUES('a.example','blog','alice/blog','main','2026-01-08T00:00:00Z')`,
		`INSERT INTO pending_events VALUES('a.example','blog','main','push',X'7B7D','2026-01-09T00:00:00.000000000Z',1,'2026-01-09T00:01:00.000000000Z')`,
	} {
		if _, err := src.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	dsn := relaytest.DSN(t)
	st, err := relay.Open(dsn) // creates the Postgres schema
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	dst, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	counts, err := copyAll(src, dst)
	if err != nil {
		t.Fatalf("copyAll: %v", err)
	}
	for table, want := range map[string]int{
		"accounts": 2, "account_creds": 1, "agents": 1, "hostnames": 1, "org_members": 1,
		"org_invites": 1, "custom_domains": 1, "github_installations": 1, "repo_bindings": 1, "pending_events": 1,
	} {
		if counts[table] != want {
			t.Errorf("%s: copied %d rows, want %d", table, counts[table], want)
		}
	}
	var disabled bool
	if err := dst.QueryRow(`SELECT disabled FROM accounts WHERE id='acc-1'`).Scan(&disabled); err != nil || !disabled {
		t.Errorf("disabled flag not converted to boolean: %v %v", disabled, err)
	}
	var payload []byte
	if err := dst.QueryRow(`SELECT payload FROM pending_events`).Scan(&payload); err != nil || string(payload) != "{}" {
		t.Errorf("payload = %q, %v; want {}", payload, err)
	}
	// The relay must be able to use the copied rows through its own API.
	st, err = relay.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if secret, err := st.AgentWebhookSecret("a.example"); err != nil || secret != "whs" {
		t.Errorf("AgentWebhookSecret = %q, %v", secret, err)
	}
}
