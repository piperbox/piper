package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/piperbox/piper/internal/relay"
	"github.com/piperbox/piper/internal/relay/relaytest"
)

// The relay binaries these tests spawn need a Postgres; relaytest provides
// one per process (RUN_E2E already implies Docker is present).
func TestMain(m *testing.M) {
	code := relaytest.Main(m)
	removeBins()
	os.Exit(code)
}

var (
	binOnce sync.Once
	binDir  string
	binErr  error
)

// bins builds the four binaries once per test process and returns the
// directory holding them. Every relay test spawns the same set, and a build
// per test bought nothing but wall clock.
func bins(t *testing.T) string {
	t.Helper()
	binOnce.Do(func() {
		binDir, binErr = os.MkdirTemp("", "piper-e2e-bin")
		if binErr != nil {
			return
		}
		repoRoot, _ := filepath.Abs("../..")
		for _, c := range []string{"piperd", "piper", "piper-relay", "piper-edge"} {
			b := exec.Command("go", "build", "-o", filepath.Join(binDir, c), "./cmd/"+c)
			b.Dir = repoRoot
			if out, err := b.CombinedOutput(); err != nil {
				binErr = fmt.Errorf("build %s: %v\n%s", c, err, out)
				return
			}
		}
	})
	if binErr != nil {
		t.Fatal(binErr)
	}
	return binDir
}

func removeBins() {
	if binDir != "" {
		os.RemoveAll(binDir)
	}
}

// The public addresses: the edge owns them, exactly as the compose
// deployment hands it the host's :443/:80/:7000. A box points
// PIPER_RELAY_ADDR at edgeTunnelAddr and never learns a relay's address.
const (
	edgeTLSAddr    = "127.0.0.1:8443"
	edgeHTTPAddr   = "127.0.0.1:8880"
	edgeTunnelAddr = "127.0.0.1:7000"
)

// relayAddrs is one relay process's four listeners plus its failure zone.
// Both relays sit on private ports: nothing but the edge dials the first
// three, and the control hop between relays uses api from the pool row.
type relayAddrs struct{ tls, http, tunnel, api, zone string }

var relayLayout = [2]relayAddrs{
	{tls: "127.0.0.1:18443", http: "127.0.0.1:18880", tunnel: "127.0.0.1:17000", api: "127.0.0.1:18080", zone: "zone-a"},
	{tls: "127.0.0.1:28443", http: "127.0.0.1:28880", tunnel: "127.0.0.1:27000", api: "127.0.0.1:28080", zone: "zone-b"},
}

// clusterOpts is what differs between the tests: the apex the relays serve
// and, for the relay-terminated tests, the wildcard pair they terminate
// with. Leaving the pair empty leaves the relays in SNI-passthrough mode.
type clusterOpts struct {
	apex              string
	certFile, keyFile string
}

// cluster is the relay side of an e2e run: one piper-edge on the public
// ports, two piper-relay processes on private ones, one relaytest database
// shared by all three. This is the smallest supported topology since #530 —
// a box holds two tunnel sessions and they must land on two relays, so a
// single directly-dialled relay leaves the second session permanently in
// backoff.
type cluster struct {
	t    *testing.T
	ctx  context.Context
	opts clusterOpts
	dsn  string
	bin  string
	dirs [2]string
	proc [2]*exec.Cmd
	st   *relay.Store
}

// startCluster brings the topology up and returns once every listener
// accepts. Processes die with the test: ctx cancellation, plus a SIGKILL on
// cleanup so the next test's binaries find the ports free.
func startCluster(t *testing.T, ctx context.Context, o clusterOpts) *cluster {
	t.Helper()
	c := &cluster{t: t, ctx: ctx, opts: o, dsn: relaytest.DSN(t), bin: bins(t)}
	for i := range c.dirs {
		c.dirs[i] = t.TempDir()
	}
	for i := range relayLayout {
		c.startRelay(i)
	}
	c.startEdge()
	return c
}

// startRelay starts (or restarts) relay i. A restart keeps the ports and the
// data dir but mints a fresh instance id, which is what a rolling replace
// does.
func (c *cluster) startRelay(i int) {
	c.t.Helper()
	a := relayLayout[i]
	cmd := exec.CommandContext(c.ctx, filepath.Join(c.bin, "piper-relay"))
	cmd.Env = append(os.Environ(),
		"PIPER_RELAY_DATA_DIR="+c.dirs[i],
		"PIPER_RELAY_DB_URL="+c.dsn,
		"PIPER_RELAY_TLS_ADDR="+a.tls,
		"PIPER_RELAY_HTTP_ADDR="+a.http,
		"PIPER_RELAY_TUNNEL_ADDR="+a.tunnel,
		"PIPER_RELAY_API_ADDR="+a.api,
		// The edge is the only way to these three ports and it always adds a
		// PROXY v2 header, so the relay must expect one.
		"PIPER_RELAY_PROXY_PROTOCOL=1",
		// What the edge dials to reach this relay. The default (first
		// non-loopback IPv4) advertises an address these loopback-bound
		// listeners never answer on.
		"PIPER_RELAY_ADVERTISE_HOST=127.0.0.1",
		"PIPER_RELAY_ZONE="+a.zone,
		// What the API hands a box to dial: the edge, never a relay (#531).
		"PIPER_RELAY_TUNNEL_PUBLIC="+edgeTunnelAddr,
		"PIPER_RELAY_APEX="+c.opts.apex,
		"PIPER_RELAY_TLS_CERT="+c.opts.certFile,
		"PIPER_RELAY_TLS_KEY="+c.opts.keyFile,
		// The login tests drive the device flow; nothing else reaches it.
		"PIPER_RELAY_FAKE_APPROVE=1",
	)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		c.t.Fatalf("start relay %d: %v", i, err)
	}
	c.proc[i] = cmd
	killOnCleanup(c.t, cmd)
	waitPort(c.t, a.tunnel, 10*time.Second)
	waitPort(c.t, a.api, 10*time.Second)
}

// stopRelay SIGTERMs relay i and waits for it to go: the rolling-restart
// gesture (#523). It drains, deletes its pool row, and the sessions it held
// are the ones their boxes must replace.
func (c *cluster) stopRelay(i int) {
	c.t.Helper()
	if err := c.proc[i].Process.Signal(syscall.SIGTERM); err != nil {
		c.t.Fatalf("SIGTERM relay %d: %v", i, err)
	}
	_ = c.proc[i].Wait() // a drained relay exits 0; either way it is gone
}

func (c *cluster) startEdge() {
	c.t.Helper()
	cmd := exec.CommandContext(c.ctx, filepath.Join(c.bin, "piper-edge"))
	cmd.Env = append(os.Environ(),
		"PIPER_EDGE_DB_URL="+c.dsn,
		"PIPER_EDGE_APEX="+c.opts.apex,
		"PIPER_EDGE_TLS_ADDR="+edgeTLSAddr,
		"PIPER_EDGE_HTTP_ADDR="+edgeHTTPAddr,
		"PIPER_EDGE_TUNNEL_ADDR="+edgeTunnelAddr,
	)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		c.t.Fatalf("start edge: %v", err)
	}
	killOnCleanup(c.t, cmd)
	waitPort(c.t, edgeTunnelAddr, 10*time.Second)
	waitPort(c.t, edgeTLSAddr, 10*time.Second)
}

// relayAPI is the control API a `piper login` dials. The edge routes the API
// by TLS SNI on api.<apex> only, which a CLI cannot reach over plain HTTP
// against a self-signed cert, so the tests dial one relay's API port. Relay
// state lives in the shared Postgres, so either relay answers the same.
func (c *cluster) relayAPI() string { return "http://" + relayLayout[0].api }

// enroll mints an enrollment token the way an operator does, against the
// relays' database rather than through a running process.
func (c *cluster) enroll(name, baseDomain string) string {
	c.t.Helper()
	cmd := exec.Command(filepath.Join(c.bin, "piper-relay"), "enroll", name, "--domain", baseDomain)
	cmd.Env = append(os.Environ(), "PIPER_RELAY_DATA_DIR="+c.dirs[0], "PIPER_RELAY_DB_URL="+c.dsn)
	out, err := cmd.CombinedOutput()
	if err != nil {
		c.t.Fatalf("enroll: %v\n%s", err, out)
	}
	return parseToken(c.t, string(out))
}

func (c *cluster) store() *relay.Store {
	c.t.Helper()
	if c.st == nil {
		st, err := relay.Open(c.dsn)
		if err != nil {
			c.t.Fatalf("open relay store: %v", err)
		}
		c.st = st
		c.t.Cleanup(func() { st.Close() })
	}
	return c.st
}

// waitOwners blocks until exactly n live relays hold a session for base,
// and returns them. These are the rows the edge routes on, so they are also
// the answer to "did the box's two sessions land on two relays".
func (c *cluster) waitOwners(base string, n int, d time.Duration) []relay.InstanceRow {
	c.t.Helper()
	deadline := time.Now().Add(d)
	var last []relay.InstanceRow
	for {
		rows, err := c.store().OwnerOf(base)
		if err != nil {
			c.t.Fatalf("owners of %s: %v", base, err)
		}
		last = rows
		if len(rows) == n {
			return rows
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("owners of %s = %v after %s, want %d relays", base, relayIndexes(last), d, n)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// relayIndexOf names a pool row by its slot in relayLayout — the tunnel port
// is unique per relay, and an index is what a failure message can be read
// against. -1 for a row from no known relay, which cannot happen.
func relayIndexOf(row relay.InstanceRow) int {
	for i, a := range relayLayout {
		if a.tunnel == row.TunnelAddr {
			return i
		}
	}
	return -1
}

func relayIndexes(rows []relay.InstanceRow) []int {
	out := make([]int, 0, len(rows))
	for _, r := range rows {
		out = append(out, relayIndexOf(r))
	}
	return out
}
