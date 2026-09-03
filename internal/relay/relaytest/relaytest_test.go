package relaytest

import (
	"database/sql"
	"os"
	"testing"
)

func TestMain(m *testing.M) { os.Exit(Main(m)) }

// Two DSN calls must land in different databases: a table created through
// one is invisible through the other.
func TestDSNGivesIsolatedDatabases(t *testing.T) {
	a, b := DSN(t), DSN(t)
	if a == b {
		t.Fatalf("both DSN calls returned %q", a)
	}
	dbA, err := sql.Open("pgx", a)
	if err != nil {
		t.Fatal(err)
	}
	defer dbA.Close()
	if _, err := dbA.Exec(`CREATE TABLE probe(x INT)`); err != nil {
		t.Fatalf("create in a: %v", err)
	}
	dbB, err := sql.Open("pgx", b)
	if err != nil {
		t.Fatal(err)
	}
	defer dbB.Close()
	var n int
	if err := dbB.QueryRow(`SELECT COUNT(*) FROM probe`).Scan(&n); err == nil {
		t.Fatal("table created in database a is visible from database b")
	}
}
