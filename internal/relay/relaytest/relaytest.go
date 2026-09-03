// Package relaytest provisions a throwaway Postgres for tests of the relay
// store and of the binaries that embed it.
//
// One server is provisioned per test process, lazily, on the first DSN call:
// PIPER_TEST_POSTGRES_URL when set (any database on a server whose role may
// CREATE DATABASE), else a postgres:17 container started through the docker
// CLI, else every caller skips — the same Docker-skip convention the runtime
// package uses. Each DSN call creates its own database on that server, so
// tests never see each other's rows and may run in parallel.
package relaytest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const image = "postgres:17"

var (
	once      sync.Once
	adminDSN  string // server-level DSN; "" when nothing could be provisioned
	skipMsg   string // why adminDSN is empty
	container string // docker container id when this process started one
)

// DSN returns a connection string for a fresh, empty database, skipping t
// when no Postgres is reachable. The database is dropped when t finishes.
func DSN(t *testing.T) string {
	t.Helper()
	once.Do(provision)
	if adminDSN == "" {
		t.Skip(skipMsg)
	}
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatal(err)
	}
	name := "relaytest_" + hex.EncodeToString(raw[:])
	admin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err := admin.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Fatalf("relaytest: create database: %v", err)
	}
	t.Cleanup(func() {
		admin, err := sql.Open("pgx", adminDSN)
		if err != nil {
			return
		}
		defer admin.Close()
		// FORCE closes stragglers; Cleanup runs LIFO so a store opened after
		// this call is already closed by the time we get here anyway.
		_, _ = admin.Exec(`DROP DATABASE ` + name + ` WITH (FORCE)`)
	})
	return withDatabase(adminDSN, name)
}

// Main wraps m.Run so a Docker-started server is removed when the package's
// tests finish. Use from a package TestMain: os.Exit(relaytest.Main(m)).
func Main(m *testing.M) int {
	code := m.Run()
	teardown()
	return code
}

func provision() {
	if dsn := os.Getenv("PIPER_TEST_POSTGRES_URL"); dsn != "" {
		adminDSN = dsn
		return
	}
	if _, err := exec.LookPath("docker"); err != nil {
		skipMsg = "postgres unavailable: no docker on PATH and PIPER_TEST_POSTGRES_URL unset"
		return
	}
	out, err := exec.Command("docker", "run", "-d", "--rm",
		"-e", "POSTGRES_PASSWORD=piper",
		"-p", "127.0.0.1:0:5432", image).Output()
	if err != nil {
		skipMsg = fmt.Sprintf("postgres unavailable: docker run %s: %v", image, err)
		return
	}
	container = strings.TrimSpace(string(out))
	portOut, err := exec.Command("docker", "port", container, "5432/tcp").Output()
	if err != nil {
		teardown()
		skipMsg = fmt.Sprintf("postgres unavailable: docker port: %v", err)
		return
	}
	// First line is the IPv4 mapping, e.g. "127.0.0.1:55001".
	hostPort := strings.TrimSpace(strings.SplitN(string(portOut), "\n", 2)[0])
	dsn := "postgres://postgres:piper@" + hostPort + "/postgres?sslmode=disable"

	// The image's entrypoint runs initdb on a socket-only server before the
	// real one listens on TCP, so a successful TCP ping means it is ready.
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if ping(dsn) == nil {
			adminDSN = dsn
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	teardown()
	skipMsg = "postgres unavailable: container never became ready"
}

func ping(dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return db.PingContext(ctx)
}

func teardown() {
	if container != "" {
		_ = exec.Command("docker", "rm", "-f", container).Run()
		container = ""
	}
}

// withDatabase swaps the database name in a postgres:// URL.
func withDatabase(dsn, name string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	u.Path = "/" + name
	return u.String()
}
