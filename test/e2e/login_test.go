package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestOneCommandLogin proves the merged `piper login` end-to-end (#465): a
// GitHub device-flow identity against the relay (auto-approved here by the
// fake verifier), then piperd's unix enrollment socket claims the box —
// piperd does the relay round-trip itself, persists relay.json, re-execs
// itself (drain-then-exec), and reconnects the tunnel — all from one command.
// A second `piper login` is idempotent: the relay upserts by box_id, so the
// account still shows exactly one agent.
func TestOneCommandLogin(t *testing.T) {
	if os.Getenv("RUN_E2E") != "1" {
		t.Skip("set RUN_E2E=1 to run (needs Docker; Caddy is embedded)")
	}
	apex := "public.localhost"
	certFile, keyFile := writeSelfSigned(t, apex) // *.public.localhost

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// piper-edge on the public ports with two relays behind it: the
	// smallest topology both of a box's tunnel sessions can place in (#530).
	// The relays fake-approve the device flow, so the login needs no real
	// browser/GitHub round-trip (same setup as TestRelayTerminatedSelfService).
	cl := startCluster(t, ctx, clusterOpts{apex: apex, certFile: certFile, keyFile: keyFile})
	binDir := bins(t)
	relayAPI := cl.relayAPI()

	// A dev-tier LAN piperd boot: no PIPER_RELAY_* env, so its enrollment
	// socket lands at <piperdData>/piperd.sock for `piper login` to claim
	// (one-command login design).
	//
	// os.MkdirTemp, not t.TempDir(): t.TempDir() embeds the (long) test name in
	// the path, which blows darwin's ~104-byte sockaddr_un sun_path limit under
	// a normal $TMPDIR. MkdirTemp("", "p") drops that segment and stays well
	// under the limit on both darwin and linux.
	piperdData, err := os.MkdirTemp("", "p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(piperdData) })
	pd := exec.CommandContext(ctx, filepath.Join(binDir, "piperd"))
	pd.Env = append(os.Environ(),
		"PIPER_DATA_DIR="+piperdData,
		"PIPER_API_ADDR=127.0.0.1:8088",
	)
	pd.Stdout, pd.Stderr = os.Stdout, os.Stderr
	if err := pd.Start(); err != nil {
		t.Fatalf("start piperd: %v", err)
	}
	killOnCleanup(t, pd)
	waitPort(t, "127.0.0.1:8088", 15*time.Second)

	// `piper login`: device flow (auto-approved) then claims this box through
	// the enrollment socket. HOME is a scratch dir so the CLI config lands
	// there; PIPER_ADDR/PIPER_TOKEN are cleared so no ambient LAN target leaks
	// in from the outer environment; PIPER_NO_BROWSER stops the real binary
	// popping a tab at the fake verifier's URL on the developer's desktop
	// (nothing here asserts on the browser, so the tab tested nothing).
	home := t.TempDir()
	piperEnv := append(os.Environ(), "HOME="+home, "PIPER_ADDR=", "PIPER_TOKEN=", "PIPER_NO_BROWSER=1")
	login := exec.Command(filepath.Join(binDir, "piper"), "login", "--relay", relayAPI, "--data-dir", piperdData)
	login.Env = piperEnv
	out, err := login.CombinedOutput()
	if err != nil {
		t.Fatalf("piper login: %v\n%s", err, out)
	}
	loginOut := string(out)
	t.Logf("piper login output:\n%s", loginOut)

	for _, want := range []string{"logged in to relay as", "claiming this box", "." + apex} {
		if !strings.Contains(loginOut, want) {
			t.Fatalf("piper login output missing %q:\n%s", want, loginOut)
		}
	}

	// piperd validated the enrollment, persisted relay.json (terminated:
	// shared relay-assigned domain, no box cert), and minted its durable
	// box-id — both written before it re-execs to apply.
	relayFile := filepath.Join(piperdData, "relay.json")
	rfBytes, err := os.ReadFile(relayFile)
	if err != nil {
		t.Fatalf("read %s: %v", relayFile, err)
	}
	if !strings.Contains(string(rfBytes), `"terminated": true`) {
		t.Fatalf("%s missing terminated:true:\n%s", relayFile, rfBytes)
	}
	if _, err := os.Stat(filepath.Join(piperdData, "box-id")); err != nil {
		t.Fatalf("box-id missing: %v", err)
	}

	// The relay's own view must show the agent connected within a deadline —
	// proof the re-exec'd piperd came back up and reconnected its tunnel.
	boxLs := func() string {
		t.Helper()
		ls := exec.Command(filepath.Join(binDir, "piper"), "box", "ls")
		ls.Env = piperEnv
		out, err := ls.CombinedOutput()
		if err != nil {
			t.Fatalf("piper box ls: %v\n%s", err, out)
		}
		return string(out)
	}
	waitBoxConnected := func() string {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		var last string
		for time.Now().Before(deadline) {
			last = boxLs()
			if strings.Contains(last, "connected") {
				return last
			}
			time.Sleep(500 * time.Millisecond)
		}
		t.Fatalf("box never showed connected within deadline; last output:\n%s", last)
		return ""
	}
	rowCount := func(ls string) int {
		ls = strings.TrimSpace(ls)
		if ls == "" {
			return 0
		}
		return strings.Count(ls, "\n") + 1
	}

	ls := waitBoxConnected()
	if n := rowCount(ls); n != 1 {
		t.Fatalf("box ls after first login = %d row(s), want 1:\n%s", n, ls)
	}

	// Re-run `piper login`: idempotent — the relay upserts the agent row by
	// box_id, so the same box claims the same identity instead of a duplicate.
	relogin := exec.Command(filepath.Join(binDir, "piper"), "login", "--relay", relayAPI, "--data-dir", piperdData)
	relogin.Env = piperEnv
	reOut, err := relogin.CombinedOutput()
	if err != nil {
		t.Fatalf("second piper login: %v\n%s", err, reOut)
	}
	if !strings.Contains(string(reOut), "already enrolled as") {
		t.Fatalf("second piper login missing 'already enrolled as':\n%s", reOut)
	}

	ls2 := waitBoxConnected()
	if n := rowCount(ls2); n != 1 {
		t.Fatalf("box ls after re-login = %d row(s), want 1 (idempotent enroll):\n%s", n, ls2)
	}
}
