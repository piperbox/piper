package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// fastAgentPoll zeroes waitActive's inter-poll delay so readiness-check tests don't
// sleep; it returns a restore func for defer.
func fastAgentPoll(t *testing.T) func() {
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

// The whole point of #375: a binary replaced on disk without a service
// restart leaves the old process running, so an upgrade silently does not
// take and looks exactly like a fix that did not work. Status must say so.
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

// stubBrew replaces brewServicesRun and brewFound with scripted fakes; returns
// the call log.
func stubBrew(t *testing.T, found bool, script func(args []string) (string, error)) *[][]string {
	t.Helper()
	var calls [][]string
	origRun, origFound := brewServicesRun, brewFound
	brewServicesRun = func(args ...string) (string, error) {
		calls = append(calls, args)
		return script(args)
	}
	brewFound = func() bool { return found }
	t.Cleanup(func() { brewServicesRun, brewFound = origRun, origFound })
	return &calls
}

func TestAgentDarwinNoBrew(t *testing.T) {
	agentGOOS = "darwin"
	t.Cleanup(func() { agentGOOS = runtime.GOOS })
	stubBrew(t, false, func([]string) (string, error) { return "", nil })
	var out, errb bytes.Buffer
	if code := agent([]string{"up"}, &out, &errb); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "brew install piperbox/tap/piper") {
		t.Errorf("stderr %q lacks the install hint", errb.String())
	}
}

func TestAgentDarwinUpStartsBrewService(t *testing.T) {
	agentGOOS = "darwin"
	t.Cleanup(func() { agentGOOS = runtime.GOOS })
	calls := stubBrew(t, true, func(args []string) (string, error) { return "", nil })
	var out, errb bytes.Buffer
	if code := agent([]string{"up"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d (stderr %s)", code, errb.String())
	}
	if len(*calls) != 1 || (*calls)[0][0] != "start" || (*calls)[0][1] != "piper" {
		t.Errorf("brew services calls = %v, want [[start piper]]", *calls)
	}
	if !strings.Contains(out.String(), "piperd started") {
		t.Errorf("stdout %q", out.String())
	}
}

func TestAgentDarwinUpFailurePassesThroughHint(t *testing.T) {
	agentGOOS = "darwin"
	t.Cleanup(func() { agentGOOS = runtime.GOOS })
	stubBrew(t, true, func([]string) (string, error) {
		return "Error: No available formula with the name \"piper\"", errors.New("exit 1")
	})
	var out, errb bytes.Buffer
	if code := agent([]string{"up"}, &out, &errb); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "brew install piperbox/tap/piper") {
		t.Errorf("stderr %q lacks the install hint", errb.String())
	}
}

func TestAgentDarwinDownStopsBrewService(t *testing.T) {
	agentGOOS = "darwin"
	t.Cleanup(func() { agentGOOS = runtime.GOOS })
	calls := stubBrew(t, true, func(args []string) (string, error) { return "", nil })
	var out, errb bytes.Buffer
	if code := agent([]string{"down"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d", code)
	}
	if len(*calls) != 1 || (*calls)[0][0] != "stop" {
		t.Errorf("calls = %v, want [[stop piper]]", *calls)
	}
}

func TestAgentDarwinStatusRunning(t *testing.T) {
	agentGOOS = "darwin"
	t.Cleanup(func() { agentGOOS = runtime.GOOS })
	stubBrew(t, true, func(args []string) (string, error) {
		return `[{"name":"piper","running":true,"status":"started"}]`, nil
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
	if !strings.Contains(out.String(), "piperd: running (brew services)") {
		t.Errorf("stdout %q", out.String())
	}
	if !strings.Contains(out.String(), "9.9.9") {
		t.Errorf("stdout %q lacks version", out.String())
	}
}

func TestAgentDarwinStatusStopped(t *testing.T) {
	agentGOOS = "darwin"
	t.Cleanup(func() { agentGOOS = runtime.GOOS })
	stubBrew(t, true, func(args []string) (string, error) {
		return `[{"name":"piper","running":false,"status":"none"}]`, nil
	})
	var out, errb bytes.Buffer
	if code := agent([]string{"status"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(out.String(), "piperd: stopped") {
		t.Errorf("stdout %q", out.String())
	}
}

func TestAgentDarwinStatusNotInstalled(t *testing.T) {
	agentGOOS = "darwin"
	t.Cleanup(func() { agentGOOS = runtime.GOOS })
	stubBrew(t, true, func(args []string) (string, error) {
		return "Error: No available formula", errors.New("exit 1")
	})
	var out, errb bytes.Buffer
	if code := agent([]string{"status"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(out.String(), "not installed") {
		t.Errorf("stdout %q", out.String())
	}
}

func TestAgentUsage(t *testing.T) {
	agentGOOS = "darwin"
	defer func() { agentGOOS = runtime.GOOS }()
	// brewFound stubbed false: usage must win regardless of Homebrew's
	// presence on the machine running the test (e.g. CI runners lack it).
	stubBrew(t, false, func([]string) (string, error) { return "", nil })
	var out, errb bytes.Buffer
	if code := agent([]string{"bogus"}, &out, &errb); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "usage: piper agent") {
		t.Errorf("stderr = %q", errb.String())
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
	defer fastAgentPoll(t)()

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
