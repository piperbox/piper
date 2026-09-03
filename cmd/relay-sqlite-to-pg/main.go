// Command relay-sqlite-to-pg copies a pre-Postgres piper-relay SQLite database
// into a Postgres database whose schema the new relay has already created
// (start the relay once, or call relay.Open). One-off cutover tool: it is
// deleted from the tree once the hosted relay has moved.
//
//	relay-sqlite-to-pg -sqlite ./relay.db -pg postgres://user:pass@host/db
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

// tables lists (table, columns) in foreign-key order: every parent before its
// children. Columns are the SQLite column names, which the Postgres schema
// keeps; the BIGSERIAL id columns are left to Postgres, in rowid order.
var tables = []struct {
	name string
	cols []string
}{
	{"accounts", []string{"id", "github_id", "github_login", "username", "type", "disabled", "created_at"}},
	{"account_creds", []string{"token_hash", "account_id", "created_at"}},
	{"agents", []string{"name", "token_hash", "base_domain", "account_id", "box_id", "control_token", "webhook_secret", "created_at"}},
	{"hostnames", []string{"hostname", "agent_name", "account_id", "app", "pr", "created_at"}},
	{"org_members", []string{"org_id", "account_id", "role", "created_at"}},
	{"org_invites", []string{"org_id", "github_login", "invited_by", "created_at"}},
	{"custom_domains", []string{"domain", "agent_base", "status", "created_at"}},
	{"github_installations", []string{"installation_id", "account_id", "target_type", "target_login", "created_at"}},
	{"repo_bindings", []string{"agent_name", "app", "repo", "branch", "created_at"}},
	{"pending_events", []string{"agent_name", "app", "ref", "event", "payload", "created_at", "attempts", "next_try_at"}},
}

func main() {
	sqlitePath := flag.String("sqlite", "", "path to the old relay.db (opened read-only)")
	pgDSN := flag.String("pg", "", "postgres:// DSN of the new, schema-initialised database")
	flag.Parse()
	if *sqlitePath == "" || *pgDSN == "" {
		log.Fatal("usage: relay-sqlite-to-pg -sqlite relay.db -pg postgres://…")
	}
	src, err := sql.Open("sqlite", "file:"+*sqlitePath+"?mode=ro")
	if err != nil {
		log.Fatal(err)
	}
	defer src.Close()
	dst, err := sql.Open("pgx", *pgDSN)
	if err != nil {
		log.Fatal(err)
	}
	defer dst.Close()
	counts, err := copyAll(src, dst)
	if err != nil {
		log.Fatal(err)
	}
	for _, t := range tables {
		fmt.Printf("%-22s %d\n", t.name, counts[t.name])
	}
}

// copyAll copies every table in one Postgres transaction and returns the row
// count per table. The target must be empty: a re-run against a populated
// database fails on the first primary-key collision and rolls back.
func copyAll(src, dst *sql.DB) (map[string]int, error) {
	tx, err := dst.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	counts := map[string]int{}
	for _, t := range tables {
		n, err := copyTable(src, tx, t.name, t.cols)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t.name, err)
		}
		counts[t.name] = n
	}
	return counts, tx.Commit()
}

func copyTable(src *sql.DB, tx *sql.Tx, table string, cols []string) (int, error) {
	rows, err := src.Query(`SELECT ` + strings.Join(cols, ", ") + ` FROM ` + table + ` ORDER BY rowid`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	placeholders := make([]string, len(cols))
	for i := range cols {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
	}
	insert := `INSERT INTO ` + table + `(` + strings.Join(cols, ", ") + `) VALUES(` + strings.Join(placeholders, ",") + `)`
	n := 0
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return n, err
		}
		for i, c := range cols {
			if table == "accounts" && c == "disabled" {
				// INTEGER 0/1 on SQLite, BOOLEAN on Postgres.
				vals[i] = vals[i] != nil && vals[i].(int64) != 0
			}
		}
		if _, err := tx.Exec(insert, vals...); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}
