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
			switch args[2] {
			case "--property=LoadState":
				return "loaded\n", nil
			case "--property=ExecStart":
				return execStartValue, nil
			}
			return "0\n", nil // MainPID probe from agentEnviron
		case "is-active":
			return "active\n", nil
		}
		return "", nil
	})
	stubRunningVersion(t, "9.9.9")
	stubDiskBinaries(t, servicePiperd, map[string]string{servicePiperd: "9.9.9"})
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

// The binary a Linux system unit runs, and the stale user-level copy that
// shadows it on PATH — the pair from #472.
const (
	servicePiperd  = "/usr/local/bin/piperd"
	stalePiperd    = "/home/u/.local/bin/piperd"
	execStartValue = "{ path=" + servicePiperd + " ; argv[]=" + servicePiperd + " ; ignore_errors=no }\n"
)

// stubVersions makes a running/on-disk pair for the Linux system service: the
// daemon reports running, and the unit's ExecStart binary reports disk.
func stubVersions(t *testing.T, running string, rerr error, disk string, derr error) {
	t.Helper()
	oldRunning, oldVer := runningAgentInfo, binaryVersion
	runningAgentInfo = func(string) (client.AgentInfo, error) { return client.AgentInfo{Version: running}, rerr }
	binaryVersion = func(string) (string, error) { return disk, derr }
	t.Cleanup(func() { runningAgentInfo, binaryVersion = oldRunning, oldVer })
}

// statusOutput runs `piper agent status` against a loaded, active unit whose
// ExecStart names the service binary, with PATH resolving to that same binary
// — these tests are about the version lines, and the developer's real PATH
// must not leak a shadow warning into them.
func statusOutput(t *testing.T) string {
	t.Helper()
	old := piperdOnPATH
	piperdOnPATH = func() (string, error) { return servicePiperd, nil }
	t.Cleanup(func() { piperdOnPATH = old })
	return statusOutputUnit(t, execStartValue, nil)
}

// statusOutputUnit is statusOutput for a unit whose `systemctl show
// --property=ExecStart --value` answers with execStart, or fails with execErr.
func statusOutputUnit(t *testing.T, execStart string, execErr error) string {
	t.Helper()
	oldRun := systemctlRun
	systemctlRun = func(args ...string) (string, error) {
		if len(args) > 0 && args[0] == "show" {
			for _, a := range args {
				if a == "--property=ExecStart" {
					return execStart, execErr
				}
			}
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

// The system piperd's /proc environ is root-only, so a non-root status can't
// see env-file overrides and used to print the built-in defaults (:80/:443)
// as fact (#476). The daemon's self-report is the truth about the running
// process and must win.
func TestAgentStatusPrintsDaemonReportedAddrs(t *testing.T) {
	onLinux(t)
	oldInfo, oldDisk := runningAgentInfo, installedPiperdVersion
	runningAgentInfo = func(string) (client.AgentInfo, error) {
		return client.AgentInfo{Version: "0.16.1", HTTPAddr: "127.0.0.1:8081", HTTPSAddr: "127.0.0.1:8444", DataDir: "/data/piper"}, nil
	}
	installedPiperdVersion = func() (string, error) { return "0.16.1", nil }
	t.Cleanup(func() { runningAgentInfo, installedPiperdVersion = oldInfo, oldDisk })

	got := statusOutput(t)
	for _, want := range []string{"127.0.0.1:8081 / 127.0.0.1:8444", "/data/piper"} {
		if !strings.Contains(got, want) {
			t.Errorf("status missing daemon-reported %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "assumed") {
		t.Errorf("daemon-reported values must not be marked assumed:\n%s", got)
	}
}

// When nothing can answer — daemon unreachable and environ unreadable — the
// defaults may be printed, but as a guess, not as fact (#476).
func TestAgentStatusMarksUnverifiedDefaults(t *testing.T) {
	onLinux(t)
	stubVersions(t, "", errors.New("dial tcp 127.0.0.1:8088: connection refused"), "", errors.New("no piperd on disk"))

	got := statusOutput(t)
	for _, want := range []string{":80 / :443 (assumed", "/var/lib/piper (assumed"} {
		if !strings.Contains(got, want) {
			t.Errorf("status missing %q:\n%s", want, got)
		}
	}
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

// The hint assumed disk ≥ running. When the running daemon is the *newer*
// build — a stale copy left behind on disk — "restart piperd to apply" is
// advice that accomplishes nothing, and would be a downgrade dressed up as an
// upgrade if the unit did point at the stale binary (#472).
func TestAgentStatusWordsVersionSkewByDirection(t *testing.T) {
	onLinux(t)
	stubVersions(t, "0.16.0", nil, "0.12.0", nil)

	got := statusOutput(t)
	if strings.Contains(got, "restart piperd to apply") {
		t.Errorf("told the user to restart into an older build:\n%s", got)
	}
	for _, want := range []string{"0.12.0", "running build is newer"} {
		if !strings.Contains(got, want) {
			t.Errorf("status missing %q:\n%s", want, got)
		}
	}
}

// stubDiskBinaries makes each path report its own version and puts onPATH at
// the front of the caller's PATH.
func stubDiskBinaries(t *testing.T, onPATH string, versions map[string]string) {
	t.Helper()
	oldLook, oldVer := piperdOnPATH, binaryVersion
	piperdOnPATH = func() (string, error) {
		if onPATH == "" {
			return "", errors.New("executable file not found in $PATH")
		}
		return onPATH, nil
	}
	binaryVersion = func(p string) (string, error) {
		v, ok := versions[p]
		if !ok {
			return "", fmt.Errorf("no such binary %s", p)
		}
		return v, nil
	}
	t.Cleanup(func() { piperdOnPATH, binaryVersion = oldLook, oldVer })
}

// stubRunningVersion makes the control API report v.
func stubRunningVersion(t *testing.T, v string) {
	t.Helper()
	old := runningAgentInfo
	runningAgentInfo = func(string) (client.AgentInfo, error) { return client.AgentInfo{Version: v}, nil }
	t.Cleanup(func() { runningAgentInfo = old })
}

// #472: the on-disk probe exec'd whatever `piperd` resolved to in the caller's
// PATH. On a box whose unit runs a current /usr/local/bin/piperd while a stale
// 0.12.0 copy sits in ~/.local/bin, that invented a pending upgrade for a
// system install that was already current. The unit's ExecStart is the only
// honest comparison target.
func TestAgentStatusComparesAgainstUnitExecStart(t *testing.T) {
	onLinux(t)
	stubRunningVersion(t, "0.16.0")
	stubDiskBinaries(t, stalePiperd, map[string]string{
		servicePiperd: "0.16.0",
		stalePiperd:   "0.12.0",
	})

	got := statusOutputUnit(t, execStartValue, nil)
	if strings.Contains(got, "0.12.0") {
		t.Errorf("compared against the PATH copy, not the unit's ExecStart:\n%s", got)
	}
	if !strings.Contains(got, "version      0.16.0\n") {
		t.Errorf("status should report the version line clean:\n%s", got)
	}
}

// systemd will not always say what a unit runs. Falling back to the caller's
// PATH there re-creates #472 exactly: a stale ~/.local/bin copy reported as if
// it were the service's install. With no honest target, status says nothing
// about disk at all — and still reports the running build.
func TestAgentStatusSkipsDiskCompareWithoutExecStart(t *testing.T) {
	for name, unit := range map[string]struct {
		value string
		err   error
	}{
		"empty":       {"", nil},
		"malformed":   {"{ argv[]=/usr/local/bin/piperd ; ignore_errors=no }\n", nil},
		"query error": {"", errors.New("Failed to get properties: Unit piperd.service not loaded")},
	} {
		t.Run(name, func(t *testing.T) {
			onLinux(t)
			stubRunningVersion(t, "0.16.0")
			stubDiskBinaries(t, stalePiperd, map[string]string{
				servicePiperd: "0.16.0",
				stalePiperd:   "0.12.0",
			})

			got := statusOutputUnit(t, unit.value, unit.err)
			if strings.Contains(got, "0.12.0") {
				t.Errorf("reported the PATH copy's version with no ExecStart to compare:\n%s", got)
			}
			if strings.Contains(got, "restart piperd to apply") {
				t.Errorf("restart advice derived from a PATH guess:\n%s", got)
			}
			if !strings.Contains(got, "version      0.16.0\n") {
				t.Errorf("the running build must still be reported:\n%s", got)
			}
		})
	}
}

// The stale PATH copy is the actual problem, and nothing detected it after
// install time. Name both paths so the fix is obvious (#472).
func TestAgentStatusWarnsWhenPATHShadowsTheService(t *testing.T) {
	onLinux(t)
	stubRunningVersion(t, "0.16.0")
	stubDiskBinaries(t, stalePiperd, map[string]string{servicePiperd: "0.16.0", stalePiperd: "0.12.0"})

	got := statusOutputUnit(t, execStartValue, nil)
	for _, want := range []string{stalePiperd, servicePiperd, "PATH"} {
		if !strings.Contains(got, want) {
			t.Errorf("status missing %q:\n%s", want, got)
		}
	}
}

// No shadow, no warning — the common case must stay quiet.
func TestAgentStatusQuietWhenPATHMatchesTheService(t *testing.T) {
	onLinux(t)
	stubRunningVersion(t, "0.16.0")
	stubDiskBinaries(t, servicePiperd, map[string]string{servicePiperd: "0.16.0"})

	if got := statusOutputUnit(t, execStartValue, nil); strings.Contains(got, "PATH") {
		t.Errorf("warned about a PATH shadow that does not exist:\n%s", got)
	}
}

// A packaged symlink into the real binary is the same install, not a shadow.
func TestAgentStatusQuietWhenPATHSymlinksToTheService(t *testing.T) {
	onLinux(t)
	dir := t.TempDir()
	real := filepath.Join(dir, "piperd")
	if err := os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "piperd-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	stubRunningVersion(t, "0.16.0")
	stubDiskBinaries(t, link, map[string]string{real: "0.16.0"})

	got := statusOutputUnit(t, "{ path="+real+" ; argv[]="+real+" ; ignore_errors=no }\n", nil)
	if strings.Contains(got, "PATH") {
		t.Errorf("reported a symlink to the service binary as a shadow:\n%s", got)
	}
}

func TestExecStartPath(t *testing.T) {
	for in, want := range map[string]string{
		execStartValue: "/usr/local/bin/piperd",
		"{ path=/usr/bin/piperd ; argv[]=/usr/bin/piperd --foo ; ignore_errors=no ; start_time=[n/a] }\n": "/usr/bin/piperd",
		"loaded\n": "",
		"":         "",
	} {
		if got := execStartPath(in); got != want {
			t.Errorf("execStartPath(%q) = %q, want %q", in, got, want)
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
// when the on-disk hint matters most. GET /v1/version landed in #388 and first
// shipped in v0.8.6 (absent at v0.8.5), so a 404 from the daemon proves it is
// older than 0.8.6 — which makes a 0.8.6 on disk definitely the newer build.
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

// A 404 from the version endpoint proves the daemon is older than 0.8.6. It
// proves nothing about the file on disk. A 0.8.4 binary there is *older* than
// the running process, so restarting into it is a downgrade — and the endpoint
// being absent is no evidence at all when the disk build cannot be placed
// relative to 0.8.6.
func TestAgentStatusOldAgentWithUnprovableDiskStaysNeutral(t *testing.T) {
	for name, disk := range map[string]string{
		"disk predates the version endpoint": "0.8.4",
		"disk is a git describe build":       "v0.17.0-3-gabc123",
		"disk version is unparseable":        "devel",
		// A lenient parser coerces this to 20221209.0.0-… and ranks it above
		// 0.8.6 — an arbitrary build label manufacturing its own evidence.
		"disk reports an arbitrary label": "20221209-update-renovatejson-v4",
	} {
		t.Run(name, func(t *testing.T) {
			onLinux(t)
			stubVersions(t, "", client.ErrVersionUnsupported, disk, nil)

			got := statusOutput(t)
			if strings.Contains(got, "restart piperd to apply") {
				t.Errorf("advised restarting into %s without evidence it is newer:\n%s", disk, got)
			}
			if strings.Contains(got, "the running build is newer") {
				t.Errorf("claimed a direction for a daemon that cannot report its version:\n%s", got)
			}
			for _, want := range []string{"too old to report", disk} {
				if !strings.Contains(got, want) {
					t.Errorf("status missing %q:\n%s", want, got)
				}
			}
		})
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

	got, err := runningAgentInfo(hostPort(t, srv.URL))
	if err != nil {
		t.Fatalf("runningAgentInfo: %v (auth sent: %q)", err, gotAuth)
	}
	if got.Version != "0.8.7" {
		t.Errorf("version = %q, want 0.8.7", got.Version)
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

	if _, err := runningAgentInfo(hostPort(t, srv.URL)); err != nil {
		t.Fatalf("runningAgentInfo: %v", err)
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
	origVer := runningAgentInfo
	runningAgentInfo = func(string) (client.AgentInfo, error) { return client.AgentInfo{Version: "9.9.9"}, nil }
	t.Cleanup(func() { runningAgentInfo = origVer })
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

// parseVersion threw away the prerelease suffix, so a final release running
// against an RC on disk compared as equal and fell through to "restart piperd
// to apply" — recommending a downgrade, the exact failure #472 is about.
func TestSkewNoteOrdersPrereleases(t *testing.T) {
	const restart, newer = "restart piperd to apply", "the running build is newer"
	for _, c := range []struct{ running, disk, want string }{
		{"0.16.0", "0.16.0-rc.1", newer},
		{"0.16.0-rc.2", "0.16.0-rc.1", newer},
		{"0.16.0-rc.1", "0.16.0", restart},
		{"0.8.4", "0.8.5", restart},
		{"0.8.5", "0.8.4", newer},
	} {
		if got := skewNote(c.running, c.disk, false); !strings.Contains(got, c.want) {
			t.Errorf("running %s vs disk %s: note = %q, want it to contain %q", c.running, c.disk, got, c.want)
		}
	}
}

// "restart piperd to apply" is a claim that the disk build is the newer one.
// When nothing can be ordered — a locally built binary reporting something
// semver cannot read — status must report the mismatch without picking a
// direction, rather than talk the user into an unknown build.
func TestSkewNoteWithoutOrderingGivesNoRestartAdvice(t *testing.T) {
	got := skewNote("0.16.0", "devel", false)
	if strings.Contains(got, "restart piperd to apply") {
		t.Errorf("advised a restart into an unorderable build: %q", got)
	}
	if strings.Contains(got, "the running build is newer") {
		t.Errorf("claimed a direction it cannot know: %q", got)
	}
	if !strings.Contains(got, "devel") {
		t.Errorf("note should still name the binary on disk: %q", got)
	}
}

// The Makefile stamps builds with `git describe --tags --always --dirty`, and
// semver happily parses what that produces as a prerelease — then orders it by
// rules that have nothing to do with git history: `-10-g…` sorts *below*
// `-9-g…` lexically, a post-tag build sorts below the tag it is ahead of, and
// two branches at equal distance cannot be ordered at all. Commit count and
// hash are not a total order, so refuse rather than invent one.
func TestSkewNoteRefusesGitDescribeBuilds(t *testing.T) {
	for _, c := range []struct{ name, running, disk string }{
		{"more commits sorts lexically lower", "v0.17.0-10-gbbbb", "v0.17.0-9-gaaaa"},
		{"fewer commits, reversed", "v0.17.0-9-gaaaa", "v0.17.0-10-gbbbb"},
		{"post-tag build against its own tag", "v0.17.0-1-gbbbb", "0.17.0"},
		{"dirty tree against the clean tag", "v0.17.0-dirty", "0.17.0"},
		{"dirty post-tag build", "v0.17.0-1-gabcdef-dirty", "0.17.0"},
		{"describe from an RC tag", "v0.17.0-rc.1-2-gabcdef", "0.17.0-rc.1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := skewNote(c.running, c.disk, false)
			if strings.Contains(got, "restart piperd to apply") {
				t.Errorf("advised a restart it cannot justify: %q", got)
			}
			if strings.Contains(got, "the running build is newer") {
				t.Errorf("claimed a direction git describe cannot establish: %q", got)
			}
			if !strings.Contains(got, c.disk) {
				t.Errorf("note should still name the binary on disk: %q", got)
			}
		})
	}
}

func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b   string
		want   int
		wantOK bool
	}{
		{"0.8.7", "0.8.6", 1, true},
		{"0.8.6", "0.8.7", -1, true},
		{"0.9.0", "0.8.99", 1, true},
		{"1.0.0", "0.99.99", 1, true},
		{"0.8.7", "0.8.7", 0, true},
		{"0.8.10", "0.8.9", 1, true},       // numeric, not lexical
		{"v0.8.7", "0.8.7", 0, true},       // the leading v is not a difference
		{"0.16.0", "0.16.0-rc.1", 1, true}, // a final release beats its RC
		{"0.16.0-rc.2", "0.16.0-rc.1", 1, true},
		{"0.16.0-rc.1", "0.16.0", -1, true},
		{"garbage", "0.8.7", 0, false},
		{"0.8.7", "garbage", 0, false},
		{"devel", "devel", 0, false},
		// `git describe` output: parseable by semver, not orderable by it.
		{"v0.17.0-9-gaaaa", "v0.17.0-10-gbbbb", 0, false},
		{"v0.17.0-1-gabcdef", "0.17.0", 0, false},
		{"0.17.0", "v0.17.0-1-gabcdef", 0, false},
		{"v0.17.0-dirty", "0.17.0", 0, false},
		{"v0.17.0-1-gabcdef-dirty", "0.17.0", 0, false},
		{"v0.17.0-rc.1-2-gabcdef", "0.17.0-rc.1", 0, false},
		// Labels a lenient parser silently coerces into a version. Every one of
		// these compares *above* 0.8.6 once coerced, which is exactly how a
		// meaningless build label turns into evidence for "restart to apply".
		{"1", "0.8.6", 0, false},
		{"1.2", "0.8.6", 0, false},
		{"v1.2", "0.8.6", 0, false},
		{"01.2.3", "0.8.6", 0, false},
		{"20221209-update-renovatejson-v4", "0.8.6", 0, false},
		{"abc1234", "0.8.6", 0, false}, // a bare git hash
		// Piper tags releases with a leading v, so that must stay comparable.
		{"v0.17.0", "0.17.0", 0, true},
		{"v0.17.0-rc.2", "v0.17.0-rc.1", 1, true},
		{"v0.8.6", "0.8.6", 0, true},
		{"0.17.0+build.1", "0.17.0", 0, true}, // build metadata is not precedence
	} {
		got, ok := compareVersions(c.a, c.b)
		if ok != c.wantOK || got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, %v; want %d, %v", c.a, c.b, got, ok, c.want, c.wantOK)
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
