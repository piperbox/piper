package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/piperbox/piper/internal/client"
	"github.com/piperbox/piper/internal/config"
)

func TestAgentUnsupportedGOOS(t *testing.T) {
	agentGOOS = "windows"
	defer func() { agentGOOS = runtime.GOOS }()
	var out, errb bytes.Buffer
	if code := agent([]string{"up"}, &out, &errb); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "macOS and Linux only") {
		t.Errorf("stderr = %q", errb.String())
	}
}

// onLinux points agentGOOS at linux; restoration is registered via t.Cleanup.
func onLinux(t *testing.T) {
	t.Helper()
	agentGOOS = "linux"
	t.Cleanup(func() { agentGOOS = runtime.GOOS })
}

// fastPoll zeroes waitActive's inter-poll delay so readiness-check tests don't
// sleep; it returns a restore func for defer.
func fastPoll(t *testing.T) func() {
	t.Helper()
	old := activePollDelay
	activePollDelay = 0
	return func() { activePollDelay = old }
}

// stubSystemctl replaces systemctlRun with a scripted fake; returns the call log.
func stubSystemctl(t *testing.T, script func(args []string) (string, error)) *[][]string {
	t.Helper()
	var calls [][]string
	orig := systemctlRun
	systemctlRun = func(args ...string) (string, error) {
		calls = append(calls, args)
		return script(args)
	}
	t.Cleanup(func() { systemctlRun = orig })
	return &calls
}

func TestAgentUpLinuxNotInstalled(t *testing.T) {
	agentGOOS = "linux"
	t.Cleanup(func() { agentGOOS = runtime.GOOS })
	stubSystemctl(t, func(args []string) (string, error) {
		if args[0] == "show" {
			return "not-found\n", nil
		}
		t.Fatalf("unexpected systemctl %v", args)
		return "", nil
	})
	var out, errb bytes.Buffer
	if code := agent([]string{"up"}, &out, &errb); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "sudo apt install piperd") {
		t.Errorf("stderr %q lacks the install hint", errb.String())
	}
}

func TestAgentUpLinuxStartsSystemUnit(t *testing.T) {
	agentGOOS = "linux"
	t.Cleanup(func() { agentGOOS = runtime.GOOS })
	origEUID := agentEUID
	agentEUID = func() int { return 0 }
	t.Cleanup(func() { agentEUID = origEUID })
	calls := stubSystemctl(t, func(args []string) (string, error) {
		switch args[0] {
		case "show":
			return "loaded\n", nil
		case "start":
			return "", nil
		case "is-active":
			return "active\n", nil
		}
		return "", nil
	})
	var out, errb bytes.Buffer
	if code := agent([]string{"up"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, want 0 (stderr: %s)", code, errb.String())
	}
	if !strings.Contains(out.String(), "piperd started") {
		t.Errorf("stdout %q", out.String())
	}
	var started bool
	for _, c := range *calls {
		if c[0] == "start" && c[1] == "piperd" {
			started = true
		}
	}
	if !started {
		t.Error("systemctl start piperd was never invoked")
	}
}

func TestAgentUpLinuxNonRootReexecsSudo(t *testing.T) {
	agentGOOS = "linux"
	t.Cleanup(func() { agentGOOS = runtime.GOOS })
	origEUID := agentEUID
	agentEUID = func() int { return 1000 }
	t.Cleanup(func() { agentEUID = origEUID })
	stubSystemctl(t, func(args []string) (string, error) { return "loaded\n", nil })
	origSudo := selfExecSudo
	var sudoArgs []string
	selfExecSudo = func(args []string, stdout, stderr io.Writer) int {
		sudoArgs = args
		return 0
	}
	t.Cleanup(func() { selfExecSudo = origSudo })
	var out, errb bytes.Buffer
	if code := agent([]string{"up"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if len(sudoArgs) != 2 || sudoArgs[0] != "agent" || sudoArgs[1] != "up" {
		t.Errorf("selfExecSudo args = %v, want [agent up]", sudoArgs)
	}
}

func TestAgentUpLinuxCrashLoopDetected(t *testing.T) {
	agentGOOS = "linux"
	t.Cleanup(func() { agentGOOS = runtime.GOOS })
	origEUID := agentEUID
	agentEUID = func() int { return 0 }
	t.Cleanup(func() { agentEUID = origEUID })
	n := 0
	stubSystemctl(t, func(args []string) (string, error) {
		switch args[0] {
		case "show":
			return "loaded\n", nil
		case "start":
			return "", nil
		case "is-active":
			n++
			if n > 1 {
				return "activating\n", nil
			}
			return "active\n", nil
		}
		return "", nil
	})
	var out, errb bytes.Buffer
	if code := agent([]string{"up"}, &out, &errb); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "crash-looping") {
		t.Errorf("stderr %q lacks crash-loop hint", errb.String())
	}
}

func TestAgentDownLinuxStopsSystemUnit(t *testing.T) {
	agentGOOS = "linux"
	t.Cleanup(func() { agentGOOS = runtime.GOOS })
	origEUID := agentEUID
	agentEUID = func() int { return 0 }
	t.Cleanup(func() { agentEUID = origEUID })
	calls := stubSystemctl(t, func(args []string) (string, error) {
		if args[0] == "show" {
			return "loaded\n", nil
		}
		return "", nil
	})
	var out, errb bytes.Buffer
	if code := agent([]string{"down"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d (stderr %s)", code, errb.String())
	}
	var stopped bool
	for _, c := range *calls {
		if c[0] == "stop" && c[1] == "piperd" {
			stopped = true
		}
	}
	if !stopped {
		t.Error("systemctl stop piperd was never invoked")
	}
}

func TestAgentStatusLinuxNotInstalled(t *testing.T) {
	agentGOOS = "linux"
	t.Cleanup(func() { agentGOOS = runtime.GOOS })
	stubSystemctl(t, func(args []string) (string, error) { return "not-found\n", nil })
	var out, errb bytes.Buffer
	if code := agent([]string{"status"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "not installed") {
		t.Errorf("stdout %q", out.String())
	}
}

func TestAgentStatusLinuxRunning(t *testing.T) {
	agentGOOS = "linux"
	t.Cleanup(func() { agentGOOS = runtime.GOOS })
	stubSystemctl(t, func(args []string) (string, error) {
		switch args[0] {
		case "show":
			if args[1] == "piperd" && strings.HasPrefix(args[2], "--property=LoadState") {
				return "loaded\n", nil
			}
			return "0\n", nil // MainPID probe from agentEnviron
		case "is-active":
			return "active\n", nil
		}
		return "", nil
	})
	origVer := runningAgentVersion
	runningAgentVersion = func(string) (string, error) { return "9.9.9", nil }
	t.Cleanup(func() { runningAgentVersion = origVer })
	origDisk := installedPiperdVersion
	installedPiperdVersion = func() (string, error) { return "9.9.9", nil }
	t.Cleanup(func() { installedPiperdVersion = origDisk })
	var out, errb bytes.Buffer
	if code := agent([]string{"status"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d", code)
	}
	got := out.String()
	for _, want := range []string{"piperd: running", "9.9.9", "control API", "data dir"} {
		if !strings.Contains(got, want) {
			t.Errorf("status output %q missing %q", got, want)
		}
	}
}

func TestAgentRejectsDaemonize(t *testing.T) {
	agentGOOS = "linux"
	t.Cleanup(func() { agentGOOS = runtime.GOOS })
	var out, errb bytes.Buffer
	if code := agent([]string{"daemonize"}, &out, &errb); code != 2 {
		t.Fatalf("code = %d, want 2 (usage)", code)
	}
	if !strings.Contains(errb.String(), "usage: piper agent <up|down|status>") {
		t.Errorf("stderr %q", errb.String())
	}
}

// stubVersions makes a running/on-disk pair, restoring both afterwards.
func stubVersions(t *testing.T, running string, rerr error, disk string, derr error) {
	t.Helper()
	oldRunning, oldDisk := runningAgentVersion, installedPiperdVersion
	runningAgentVersion = func(string) (string, error) { return running, rerr }
	installedPiperdVersion = func() (string, error) { return disk, derr }
	t.Cleanup(func() { runningAgentVersion, installedPiperdVersion = oldRunning, oldDisk })
}

// statusOutput runs `piper agent status` against a unit systemctl reports as
// loaded and active.
func statusOutput(t *testing.T) string {
	t.Helper()
	oldRun := systemctlRun
	systemctlRun = func(args ...string) (string, error) {
		if len(args) > 0 && args[0] == "show" {
			return "loaded\n", nil
		}
		return "active\n", nil
	}
	t.Cleanup(func() { systemctlRun = oldRun })

	var out, errb bytes.Buffer
	if code := agent([]string{"status"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errb.String())
	}
	return out.String()
}

// The whole point of #375: `piper agent daemonize` installs a new binary but
// leaves the old process running, so an upgrade silently does not take and
// looks exactly like a fix that did not work. Status must say so.
func TestAgentStatusFlagsUnrestartedUpgrade(t *testing.T) {
	onLinux(t)
	stubVersions(t, "0.8.4", nil, "0.8.5", nil)

	got := statusOutput(t)
	for _, want := range []string{"0.8.4", "0.8.5 is installed on disk", "restart piperd"} {
		if !strings.Contains(got, want) {
			t.Errorf("status missing %q:\n%s", want, got)
		}
	}
}

// Matching versions must not nag — a warning that fires when nothing is wrong
// is one people learn to ignore.
func TestAgentStatusQuietWhenVersionsMatch(t *testing.T) {
	onLinux(t)
	stubVersions(t, "0.8.5", nil, "0.8.5", nil)

	got := statusOutput(t)
	if !strings.Contains(got, "version      0.8.5") {
		t.Errorf("status missing the version line:\n%s", got)
	}
	if strings.Contains(got, "⚠") {
		t.Errorf("warned about a matching version:\n%s", got)
	}
}

// An agent predating the endpoint still reports usefully — and this is exactly
// when the on-disk hint matters most, since anything that old is behind.
func TestAgentStatusOldAgentStillFlagsDisk(t *testing.T) {
	onLinux(t)
	stubVersions(t, "", client.ErrVersionUnsupported, "0.8.6", nil)

	got := statusOutput(t)
	for _, want := range []string{"too old to report", "0.8.6 is installed on disk"} {
		if !strings.Contains(got, want) {
			t.Errorf("status missing %q:\n%s", want, got)
		}
	}
}

// systemd says active but the control API does not answer: report that plainly
// rather than inventing a version or hanging on a wedged daemon.
func TestAgentStatusUnreachableControlAPI(t *testing.T) {
	onLinux(t)
	stubVersions(t, "", errFake, "0.8.6", nil)

	got := statusOutput(t)
	if !strings.Contains(got, "control API unreachable") {
		t.Errorf("status missing the unreachable note:\n%s", got)
	}
}

// v0.8.6 shipped this broken on exactly the box it was built for. The
// documented LAN setup sets PIPER_API_ADDR=0.0.0.0:8088, and piperd's local
// listener requires a bearer on any non-loopback bind — so the bare dial 401'd
// and `piper agent status` reported "control API unreachable" about a daemon
// that had answered perfectly well.
func TestRunningAgentVersionUsesSavedTokenForThisBox(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if gotAuth != "Bearer lan-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"version": "0.8.7"})
	}))
	defer srv.Close()
	if err := config.SaveClient(config.ClientConfig{Addr: srv.URL, Token: "lan-token"}); err != nil {
		t.Fatal(err)
	}

	got, err := runningAgentVersion(hostPort(t, srv.URL))
	if err != nil {
		t.Fatalf("runningAgentVersion: %v (auth sent: %q)", err, gotAuth)
	}
	if got != "0.8.7" {
		t.Errorf("version = %q, want 0.8.7", got)
	}
}

// The token belongs to the box it was issued for. `piper agent status` on a
// laptop whose CLI points at a Pi must not hand the Pi's credential to the
// laptop's own daemon.
func TestRunningAgentVersionWithholdsAnotherBoxToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]string{"version": "0.8.7"})
	}))
	defer srv.Close()
	// Configured box is somewhere else entirely.
	if err := config.SaveClient(config.ClientConfig{Addr: "http://192.168.1.6:8088", Token: "other-box-token"}); err != nil {
		t.Fatal(err)
	}

	if _, err := runningAgentVersion(hostPort(t, srv.URL)); err != nil {
		t.Fatalf("runningAgentVersion: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("sent %q to a different box's daemon, want no Authorization header", gotAuth)
	}
}

// A 401 is a daemon that answered, not one that could not be reached. Saying
// "unreachable" sent this investigation looking at systemd and sockets when the
// control API was fine — the message has to name the actual problem.
func TestAgentStatusUnauthorizedIsNotReportedAsUnreachable(t *testing.T) {
	onLinux(t)
	stubVersions(t, "", &client.StatusError{Code: http.StatusUnauthorized}, "0.8.6", nil)

	got := statusOutput(t)
	if strings.Contains(got, "unreachable") {
		t.Errorf("a 401 must not be reported as unreachable:\n%s", got)
	}
	if !strings.Contains(got, "piper login") {
		t.Errorf("status should say how to fix a 401:\n%s", got)
	}
}

// hostPort strips the scheme from a test server URL, matching what
// PIPER_API_ADDR carries.
func hostPort(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

func TestDialableAddrRewritesWildcardBinds(t *testing.T) {
	for addr, want := range map[string]string{
		"0.0.0.0:8088":   "127.0.0.1:8088",
		":8088":          "127.0.0.1:8088",
		"[::]:8088":      "127.0.0.1:8088",
		"127.0.0.1:8088": "127.0.0.1:8088",
		"192.168.1.6:80": "192.168.1.6:80",
		"garbage":        "garbage",
	} {
		if got := dialableAddr(addr); got != want {
			t.Errorf("dialableAddr(%q) = %q, want %q", addr, got, want)
		}
	}
}

var errFake = fmt.Errorf("exit status 3")

// onDarwin points agentGOOS at darwin for the launchd tests; restores via
// t.Cleanup.
func onDarwin(t *testing.T) {
	t.Helper()
	agentGOOS = "darwin"
	t.Cleanup(func() { agentGOOS = runtime.GOOS })
}

// darwinPaths points the generated plist, the login-scanned legacy plist, the
// env file, and the resolved piperd binary into a temp dir, so `up`
// materializes into a sandbox. Restore via the returned func.
func darwinPaths(t *testing.T) (plist, legacy, env, piperd string, restore func()) {
	t.Helper()
	dir := t.TempDir()
	plist = filepath.Join(dir, ".piper", "com.piperbox.piperd.plist")
	legacy = filepath.Join(dir, "Library", "LaunchAgents", "com.piperbox.piperd.plist")
	env = filepath.Join(dir, ".piper", "piperd.env")
	piperd = filepath.Join(dir, "bin", "piperd")
	oldPlist, oldLegacy, oldEnv, oldBin := launchdPlistPath, legacyLaunchAgentPath, userEnvPath, piperdPath
	launchdPlistPath = func() (string, error) { return plist, nil }
	legacyLaunchAgentPath = func() (string, error) { return legacy, nil }
	userEnvPath = func() (string, error) { return env, nil }
	piperdPath = func() (string, error) { return piperd, nil }
	return plist, legacy, env, piperd, func() {
		launchdPlistPath, legacyLaunchAgentPath, userEnvPath, piperdPath = oldPlist, oldLegacy, oldEnv, oldBin
	}
}

// TestLaunchdPlistIsNotLoginScanned pins the core of macOS's ephemeral
// contract: the plist must live outside ~/Library/LaunchAgents, which launchd
// scans and auto-loads at every login.
func TestLaunchdPlistIsNotLoginScanned(t *testing.T) {
	got, err := launchdPlistPath()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, filepath.Join("Library", "LaunchAgents")) {
		t.Errorf("plist path %q is login-scanned; piperd would survive a reboot", got)
	}
	if !strings.Contains(got, filepath.Join(".piper", "com.piperbox.piperd.plist")) {
		t.Errorf("plist path = %q, want it under ~/.piper", got)
	}
}

func TestAgentUpBootstraps(t *testing.T) {
	onDarwin(t)
	plist, _, _, _, restore := darwinPaths(t)
	defer restore()

	var gotArgs []string
	oldRun := launchctlRun
	launchctlRun = func(args ...string) (string, error) { gotArgs = args; return "", nil }
	defer func() { launchctlRun = oldRun }()

	var out, errb bytes.Buffer
	if code := agent([]string{"up"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errb.String())
	}
	if len(gotArgs) < 3 || gotArgs[0] != "bootstrap" || gotArgs[2] != plist {
		t.Errorf("launchctl args = %v, want bootstrap <gui> %s", gotArgs, plist)
	}
	if !strings.Contains(out.String(), "started") {
		t.Errorf("stdout = %q", out.String())
	}
}

// TestAgentUpDarwinMaterializesPlist is the fix for the shipped plist's
// hard-coded /usr/local/bin/piperd: the generated one execs whichever piperd
// the CLI actually resolved.
func TestAgentUpDarwinMaterializesPlist(t *testing.T) {
	onDarwin(t)
	plist, _, _, piperd, restore := darwinPaths(t)
	defer restore()
	oldRun := launchctlRun
	launchctlRun = func(args ...string) (string, error) { return "", nil }
	defer func() { launchctlRun = oldRun }()

	var out, errb bytes.Buffer
	if code := agent([]string{"up"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errb.String())
	}
	b, err := os.ReadFile(plist)
	if err != nil {
		t.Fatalf("plist not materialized: %v", err)
	}
	got := string(b)
	for _, want := range []string{
		"<string>com.piperbox.piperd</string>",
		"<key>KeepAlive</key>",
		`PIPER_HTTP_ADDR=":8080"`,
		`PIPER_HTTPS_ADDR=":8443"`,
		`PIPER_CADDY_ADMIN="http://127.0.0.1:2020"`,
		`XDG_DATA_HOME="$HOME/.piper/piperd"`,
		`$HOME/.piper/piper.log`,
		`exec "` + piperd + `"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("generated plist missing %q\n%s", want, got)
		}
	}
	if strings.Contains(got, "/usr/local/bin/piperd") {
		t.Errorf("generated plist still hard-codes the system-tier path:\n%s", got)
	}
}

// TestAgentUpDarwinRefreshesStalePlist covers the self-heal: a plist written by
// an older piper (pointing at a binary that has since moved) is rewritten.
func TestAgentUpDarwinRefreshesStalePlist(t *testing.T) {
	onDarwin(t)
	plist, _, _, piperd, restore := darwinPaths(t)
	defer restore()
	os.MkdirAll(filepath.Dir(plist), 0o755)
	os.WriteFile(plist, []byte("stale plist execing /usr/local/bin/piperd"), 0o644)
	oldRun := launchctlRun
	launchctlRun = func(args ...string) (string, error) { return "", nil }
	defer func() { launchctlRun = oldRun }()

	var out, errb bytes.Buffer
	if code := agent([]string{"up"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errb.String())
	}
	if b, _ := os.ReadFile(plist); !strings.Contains(string(b), `exec "`+piperd+`"`) {
		t.Errorf("stale plist not refreshed; got %q", string(b))
	}
}

// TestAgentUpDarwinEvictsLoginScannedPlist covers the migration off the shipped
// LaunchAgent: it is booted out (it holds the same label, so bootstrap would
// fail) and deleted, so login stops starting a stale piperd behind our back.
func TestAgentUpDarwinEvictsLoginScannedPlist(t *testing.T) {
	onDarwin(t)
	_, legacy, _, _, restore := darwinPaths(t)
	defer restore()
	os.MkdirAll(filepath.Dir(legacy), 0o755)
	os.WriteFile(legacy, []byte("shipped plist execing /usr/local/bin/piperd"), 0o644)

	var calls [][]string
	oldRun := launchctlRun
	launchctlRun = func(args ...string) (string, error) { calls = append(calls, args); return "", nil }
	defer func() { launchctlRun = oldRun }()

	var out, errb bytes.Buffer
	if code := agent([]string{"up"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errb.String())
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("login-scanned plist still present at %s", legacy)
	}
	if len(calls) < 2 || calls[0][0] != "bootout" || calls[1][0] != "bootstrap" {
		t.Errorf("calls = %v, want bootout before bootstrap", calls)
	}
}

func TestAgentUpDarwinSeedsEnvWithoutClobbering(t *testing.T) {
	onDarwin(t)
	_, _, env, _, restore := darwinPaths(t)
	defer restore()
	oldRun := launchctlRun
	launchctlRun = func(args ...string) (string, error) { return "", nil }
	defer func() { launchctlRun = oldRun }()

	var out, errb bytes.Buffer
	if code := agent([]string{"up"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errb.String())
	}
	if b, _ := os.ReadFile(env); string(b) != embeddedMacEnv {
		t.Fatalf("env not seeded; got %q", string(b))
	}

	edited := "PIPER_BASE_DOMAIN=dev.local\n"
	os.WriteFile(env, []byte(edited), 0o600)
	if code := agent([]string{"up"}, &out, &errb); code != 0 {
		t.Fatalf("second up: code = %d, stderr = %s", code, errb.String())
	}
	if b, _ := os.ReadFile(env); string(b) != edited {
		t.Errorf("env clobbered: got %q", string(b))
	}
}

// TestAgentUpDarwinSaysItIsEphemeral holds macOS to the same contract as Linux
// rootless: `up` runs it until reboot, and nothing promotes it.
func TestAgentUpDarwinSaysItIsEphemeral(t *testing.T) {
	onDarwin(t)
	_, _, _, _, restore := darwinPaths(t)
	defer restore()
	oldRun := launchctlRun
	launchctlRun = func(args ...string) (string, error) { return "", nil }
	defer func() { launchctlRun = oldRun }()

	var out, errb bytes.Buffer
	if code := agent([]string{"up"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "won't survive a reboot") {
		t.Errorf("stdout = %q, want the ephemeral note", out.String())
	}
}

func TestAgentDaemonizeUnsupportedOnDarwin(t *testing.T) {
	onDarwin(t)
	var out, errb bytes.Buffer
	if code := agent([]string{"daemonize"}, &out, &errb); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "macOS") {
		t.Errorf("stderr = %q, want it to name macOS", errb.String())
	}
}

func TestAgentStatusDarwinStoppedBeforeFirstUp(t *testing.T) {
	onDarwin(t)
	_, _, _, _, restore := darwinPaths(t)
	defer restore()
	oldRun := launchctlRun
	launchctlRun = func(args ...string) (string, error) { return "", errFake }
	defer func() { launchctlRun = oldRun }()

	var out, errb bytes.Buffer
	if code := agent([]string{"status"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "stopped") {
		t.Errorf("stdout = %q, want stopped", out.String())
	}
}

func TestAgentDownBootsOut(t *testing.T) {
	agentGOOS = "darwin"
	defer func() { agentGOOS = runtime.GOOS }()
	var gotArgs []string
	oldRun := launchctlRun
	launchctlRun = func(args ...string) (string, error) { gotArgs = args; return "", nil }
	defer func() { launchctlRun = oldRun }()

	var out, errb bytes.Buffer
	if code := agent([]string{"down"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errb.String())
	}
	if len(gotArgs) < 1 || gotArgs[0] != "bootout" {
		t.Errorf("launchctl args = %v, want bootout ...", gotArgs)
	}
}

func TestAgentUsage(t *testing.T) {
	agentGOOS = "darwin"
	defer func() { agentGOOS = runtime.GOOS }()
	var out, errb bytes.Buffer
	if code := agent([]string{"bogus"}, &out, &errb); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "usage: piper agent") {
		t.Errorf("stderr = %q", errb.String())
	}
}

// TestMacosDocsMatchGeneratedAgent inherits the doc contract the shipped-plist
// package used to own: nothing may point users at a plist to install by hand,
// and the macOS flow is the same `piper agent` verbs as everywhere else.
func TestMacosDocsMatchGeneratedAgent(t *testing.T) {
	repoFile := func(parts ...string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(append([]string{"..", ".."}, parts...)...))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for _, doc := range [][]string{
		{"docs", "manual-setup.md"},
		{"docs", "getting-started.md"},
	} {
		if body := repoFile(doc...); strings.Contains(body, "packaging/launchd") {
			t.Errorf("%s still points at the deleted shipped plist", filepath.Join(doc...))
		}
	}
	manual := repoFile("docs", "manual-setup.md")
	for _, s := range []string{"piper agent up", "piper agent down"} {
		if !strings.Contains(manual, s) {
			t.Errorf("docs/manual-setup.md missing %q", s)
		}
	}
	runbook := repoFile("docs", "runbooks", "git-deploy-e2e.md")
	for _, s := range []string{"piper agent status", "~/.piper/piper.log", "piper agent down"} {
		if !strings.Contains(runbook, s) {
			t.Errorf("runbook missing %q", s)
		}
	}
}

func TestVersionNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"0.8.7", "0.8.6", true},
		{"0.8.6", "0.8.7", false},
		{"0.9.0", "0.8.99", true},
		{"1.0.0", "0.99.99", true},
		{"0.8.7", "0.8.7", false},
		{"0.8.10", "0.8.9", true}, // numeric, not lexical
		{"garbage", "0.8.7", false},
		{"0.8.7", "garbage", false},
	} {
		if got := versionNewer(c.a, c.b); got != c.want {
			t.Errorf("versionNewer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// stubUnitLoadedAndIsActive scripts systemctlRun: `show` calls report the unit
// loaded (so unitLoaded() is satisfied), and `is-active` calls step through
// states in turn, repeating the last one once exhausted. Restores via
// t.Cleanup.
func stubUnitLoadedAndIsActive(t *testing.T, states ...string) {
	t.Helper()
	old := systemctlRun
	var n int
	systemctlRun = func(args ...string) (string, error) {
		if len(args) > 0 && args[0] == "show" {
			return "loaded\n", nil
		}
		s := states[min(n, len(states)-1)]
		n++
		return s, nil
	}
	t.Cleanup(func() { systemctlRun = old })
}

// One `active` sample proves nothing: a Type=simple unit reports active the
// instant ExecStart forks, so a piperd that dies immediately reads as active
// and only drops to activating/failed once Restart= backoff kicks in. status
// sampled once and called that "running" — while `piper agent up`, a few
// functions away, already polled for exactly this reason (#392).
func TestAgentStatusLinuxDoesNotTrustOneActiveSample(t *testing.T) {
	onLinux(t)
	stubVersions(t, "0.8.5", nil, "0.8.5", nil)
	defer fastPoll(t)()

	for _, c := range []struct {
		name    string
		states  []string
		want    string
		notWant string
	}{
		{"crash loop", []string{"active\n", "activating\n"}, "piperd: restarting", "piperd: running"},
		{"dies into failed", []string{"active\n", "failed\n"}, "piperd: failed", "piperd: running"},
		{"steady", []string{"active\n"}, "piperd: running", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			stubUnitLoadedAndIsActive(t, c.states...)
			var out, errb bytes.Buffer
			if code := agent([]string{"status"}, &out, &errb); code != 0 {
				t.Fatalf("code = %d", code)
			}
			if !strings.Contains(out.String(), c.want) {
				t.Errorf("stdout = %q, want %q", out.String(), c.want)
			}
			if c.notWant != "" && strings.Contains(out.String(), c.notWant) {
				t.Errorf("stdout = %q, must not contain %q", out.String(), c.notWant)
			}
		})
	}
}
