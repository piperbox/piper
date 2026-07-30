package main

import (
	_ "embed"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/piperbox/piper/internal/client"
	"github.com/piperbox/piper/internal/config"
)

//go:embed piperd.env.macos.example
var embeddedMacEnv string

const launchdLabel = "com.piperbox.piperd"

// agentGOOS is runtime.GOOS; a var so tests can exercise the non-darwin gate.
var agentGOOS = runtime.GOOS

// launchdPlistPath returns where `piper agent up` materializes the LaunchAgent.
// It is deliberately NOT ~/Library/LaunchAgents: launchd scans that directory at
// every login and would auto-start piperd, but macOS is a dev target with no
// `daemonize` tier — `up` runs it, a reboot ends it. A var so tests can point it
// at a temp file.
var launchdPlistPath = func() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".piper", launchdLabel+".plist"), nil
}

// legacyLaunchAgentPath is the login-scanned plist that piper shipped before the
// agent generated its own. `up` evicts it (see agentUp); a var so tests can
// point it at a temp file.
var legacyLaunchAgentPath = func() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"), nil
}

// piperdPath resolves the piperd the LaunchAgent should exec: the one sitting
// next to this piper binary (the installer places both in one prefix), else
// whatever is on PATH. Resolving it here is what keeps the plist correct for any
// prefix, instead of pinning one install location. A var so tests can stub it.
var piperdPath = func() (string, error) {
	if exe, err := os.Executable(); err == nil {
		if cand := filepath.Join(filepath.Dir(exe), "piperd"); fileExists(cand) {
			return cand, nil
		}
	}
	return exec.LookPath("piperd")
}

// runningAgentVersion asks the control API which build is actually serving.
// Short timeout: status is a glance, and a wedged daemon must not hang it.
//
// The bearer is not optional in practice. piperd serves its local listener
// tokenless only on a loopback bind; the documented LAN setup
// (PIPER_API_ADDR=0.0.0.0:8088) wraps it in RequireToken, so a bare dial 401s —
// which is exactly how v0.8.6 shipped, reporting "unreachable" about a daemon
// that had answered. The token comes from this CLI's own config, and only when
// it was issued for the box being asked: `piper agent status` on a laptop whose
// CLI points at a Pi must not hand the Pi's credential to the laptop's daemon.
//
// A var so tests can stub it.
var runningAgentVersion = func(apiAddr string) (string, error) {
	target := dialableAddr(apiAddr)
	token := ""
	if cc, err := config.LoadClient(); err == nil && sameBox(cc.Addr, target) {
		token = cc.Token
	}
	return client.New("http://"+target, token).WithTimeout(2 * time.Second).AgentVersion()
}

// sameBox reports whether a configured client address (a URL) points at the
// same host:port as target (a bare host:port), after both are normalized for
// wildcard binds.
func sameBox(configured, target string) bool {
	u, err := url.Parse(configured)
	if err != nil || u.Host == "" {
		return false
	}
	return dialableAddr(u.Host) == target
}

// binaryVersion runs a piper binary's own --version. A var so tests can stub
// it: the fake binaries they write are not executable programs.
var binaryVersion = func(path string) (string, error) {
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// installedPiperdVersion runs the on-disk piperd's own --version. This is the
// half that catches the daemonize trap: `piper agent daemonize` installs a new
// binary but leaves the old process running, so disk and running disagree and
// the upgrade silently has not taken (#375). A var so tests can stub it.
var installedPiperdVersion = func() (string, error) {
	p, err := piperdPath()
	if err != nil {
		return "", err
	}
	return binaryVersion(p)
}

// versionNewer reports whether a is a strictly newer X.Y.Z than b. Unparseable
// input is never "newer", so an unknown build can't win a comparison it should
// not be in.
func versionNewer(a, b string) bool {
	av, aok := parseVersion(a)
	bv, bok := parseVersion(b)
	if !aok || !bok {
		return false
	}
	for i := range av {
		if av[i] != bv[i] {
			return av[i] > bv[i]
		}
	}
	return false
}

// parseVersion reads the leading X.Y.Z of a version string, ignoring any
// pre-release suffix.
func parseVersion(s string) ([3]int, bool) {
	var out [3]int
	parts := strings.SplitN(strings.TrimPrefix(strings.TrimSpace(s), "v"), ".", 3)
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		if i == 2 {
			p, _, _ = strings.Cut(p, "-")
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// dialableAddr turns a listen address into one a client can connect to: a
// wildcard bind (the documented PIPER_API_ADDR=0.0.0.0:8088 LAN flow) is where
// the daemon listens, not an address to dial.
func dialableAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		return net.JoinHostPort("127.0.0.1", port)
	}
	return addr
}

// printAgentVersions reports the running build, and flags a piperd on disk that
// differs — the state a restart-less upgrade leaves behind, which otherwise
// reads as "the fix didn't work".
func printAgentVersions(stdout io.Writer, apiAddr string) {
	running, rerr := runningAgentVersion(apiAddr)
	disk, derr := installedPiperdVersion()

	var se *client.StatusError
	switch {
	case rerr == nil:
	case errors.Is(rerr, client.ErrVersionUnsupported):
		running = "unknown (agent too old to report it)"
	case errors.As(rerr, &se) && se.Code == http.StatusUnauthorized:
		// A daemon that answered, not one that could not be reached. Conflating
		// the two sent a live investigation looking at systemd and sockets while
		// the control API was fine.
		running = "unknown (control API needs a token — run `piper login`)"
	case errors.As(rerr, &se):
		running = fmt.Sprintf("unknown (control API error: %v)", rerr)
	default:
		fmt.Fprintf(stdout, "  version      unknown (control API unreachable at %s)\n", apiAddr)
		return
	}
	if derr == nil && disk != running {
		fmt.Fprintf(stdout, "  version      %s  ⚠ %s is installed on disk — restart piperd to apply\n", running, disk)
		return
	}
	fmt.Fprintf(stdout, "  version      %s\n", running)
}

// launchctlRun runs `launchctl <args...>` and returns combined output; a var so
// tests can substitute it without shelling out to a real launchd.
var launchctlRun = func(args ...string) (string, error) {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	return string(out), err
}

func guiTarget() string { return "gui/" + strconv.Itoa(os.Getuid()) }

const userUnitName = "piperd"

// systemctlRun runs `systemctl <args...>` and returns combined output; a var so
// tests can substitute it without a real systemd.
var systemctlRun = func(args ...string) (string, error) {
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	return string(out), err
}

// activePollAttempts/activePollDelay bound how long waitActive watches a
// just-started unit to prove it holds `active`; vars so tests run instantly.
var (
	activePollAttempts = 12
	activePollDelay    = 150 * time.Millisecond
	statusPollAttempts = 3
)

// waitActive reports whether the unit reaches and holds `active`, returning the
// last state seen. A Type=simple unit reports `active` the instant ExecStart
// forks, so one that immediately exits and enters Restart= backoff only shows
// as `activating`/`failed`/`inactive` a moment later — so we poll and fail on
// the first non-active sample rather than trusting the initial `active`.
func waitActive() (string, bool) {
	return pollActive(activePollAttempts)
}

// pollActive samples `is-active` up to attempts times, returning on the first
// non-active sample. Callers differ in how long they can afford to watch:
// `up` has just started the unit and takes the full activePollAttempts, while
// `status` must stay snappy and takes statusPollAttempts — enough to catch a
// unit cycling through Restart= backoff, since a flapping unit shows a
// non-active sample almost at once (#392).
func pollActive(attempts int) (string, bool) {
	args := []string{"is-active", userUnitName}
	var state string
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(activePollDelay)
		}
		out, _ := systemctlRun(args...)
		state = strings.TrimSpace(out)
		if state != "active" {
			return state, false
		}
	}
	return state, true
}

// printRestartHint explains a crash loop, which is the one state a user cannot
// act on from the word alone: "restarting" without context reads like a
// transient blip rather than a unit that will never come up. journalCmd is the
// scope-correct journal invocation, since the rootless unit needs --user.
func printRestartHint(stdout io.Writer, state, journalCmd string) {
	if state != "activating" {
		return
	}
	fmt.Fprintf(stdout, "  the unit is cycling through Restart= backoff — see `%s`\n", journalCmd)
}

// unitStateWord names what systemd settled on for a unit that is not reliably
// active. "activating" is the one that matters: reached after an "active"
// sample it means the unit is cycling through Restart= backoff, which is
// neither running nor stopped, and collapsing it into "stopped" hides an
// actively failing box almost as badly as calling it "running".
func unitStateWord(state string) string {
	switch state {
	case "activating":
		return "restarting"
	case "failed":
		return "failed"
	default:
		return "stopped"
	}
}

// userUnitPath returns the installed systemd user-unit path; a var so tests can
// point it at a temp file.
var userUnitPath = func() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user", userUnitName+".service"), nil
}

// userEnvPath returns the rootless agent's env-file path; a var so tests can
// point it at a temp file.
var userEnvPath = func() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".piper", "piperd.env"), nil
}

// agentEUID is os.Geteuid; a var so tests can stub identity.
var agentEUID = os.Geteuid

// agent dispatches `piper agent ...` to the platform's rootless agent manager.
func agent(args []string, stdout, stderr io.Writer) int {
	switch agentGOOS {
	case "darwin":
		return agentDarwin(args, stdout, stderr)
	case "linux":
		return agentLinux(args, stdout, stderr)
	default:
		fmt.Fprintln(stderr, "error: `piper agent` supports macOS and Linux only")
		return 2
	}
}

func agentDarwin(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: piper agent <up|down|status>")
		return 2
	}
	switch args[0] {
	case "up":
		return agentUp(stdout, stderr)
	case "down":
		return agentDown(stdout, stderr)
	case "status":
		return agentStatus(stdout, stderr)
	case "daemonize":
		// macOS is a dev target: piperd runs rootless on high ports for as long
		// as you're logged in. Durability is the Linux system-service tier.
		fmt.Fprintln(stderr, "error: `piper agent daemonize` is Linux-only — on macOS piperd is a dev agent that `piper agent up` runs until you stop it or reboot")
		return 2
	default:
		fmt.Fprintln(stderr, "usage: piper agent <up|down|status>")
		return 2
	}
}

// agentLinux drives the one and only tier: the systemd system service,
// installed by the deb (or manually) — never materialized by the CLI.
func agentLinux(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: piper agent <up|down|status>")
		return 2
	}
	switch args[0] {
	case "up":
		return agentUpLinux(stdout, stderr)
	case "down":
		return agentDownLinux(stdout, stderr)
	case "status":
		return agentStatusLinux(stdout, stderr)
	default:
		fmt.Fprintln(stderr, "usage: piper agent <up|down|status>")
		return 2
	}
}

// unitLoaded reports whether the piperd system unit exists. `systemctl show`
// answers for any caller (no root), and LoadState is "not-found" when no unit
// file is installed — covering both the deb's /usr/lib and a manual /etc path.
func unitLoaded() bool {
	out, err := systemctlRun("show", "piperd", "--property=LoadState", "--value")
	return err == nil && strings.TrimSpace(out) == "loaded"
}

const notInstalledLinux = "piperd is not installed — `sudo apt install piperd` (see the README for other channels)"

func agentUpLinux(stdout, stderr io.Writer) int {
	if !unitLoaded() {
		fmt.Fprintln(stderr, "error: "+notInstalledLinux)
		return 1
	}
	if agentEUID() != 0 {
		fmt.Fprintln(stderr, "controlling the piperd service needs root — re-running under sudo…")
		return selfExecSudo([]string{"agent", "up"}, stdout, stderr)
	}
	if out, err := systemctlRun("start", "piperd"); err != nil {
		fmt.Fprintf(stderr, "error: systemctl start failed: %v\n%s", err, out)
		return 1
	}
	// `systemctl start` returns before a Type=simple unit can fail, so confirm
	// it actually stays up rather than crash-looping (#211).
	if state, ok := waitActive(); !ok {
		fmt.Fprintf(stderr, "error: piperd started but is not active (state: %s) — it may be crash-looping.\nCheck: systemctl status piperd\n", state)
		return 1
	}
	fmt.Fprintln(stdout, "piperd started")
	return 0
}

func agentDownLinux(stdout, stderr io.Writer) int {
	if !unitLoaded() {
		fmt.Fprintln(stderr, "error: "+notInstalledLinux)
		return 1
	}
	if agentEUID() != 0 {
		fmt.Fprintln(stderr, "controlling the piperd service needs root — re-running under sudo…")
		return selfExecSudo([]string{"agent", "down"}, stdout, stderr)
	}
	if out, err := systemctlRun("stop", "piperd"); err != nil {
		fmt.Fprintf(stderr, "error: systemctl stop failed: %v\n%s", err, out)
		return 1
	}
	fmt.Fprintln(stdout, "piperd stopped")
	return 0
}

func agentStatusLinux(stdout, stderr io.Writer) int {
	if !unitLoaded() {
		fmt.Fprintln(stdout, "piperd: not installed — `sudo apt install piperd` (see the README for other channels)")
		return 0
	}
	if state, ok := pollActive(statusPollAttempts); !ok {
		fmt.Fprintf(stdout, "piperd: %s\n", unitStateWord(state))
		printRestartHint(stdout, state, "journalctl -u piperd")
		return 0
	}
	fmt.Fprintln(stdout, "piperd: running")
	// The system piperd's /proc environ is root-only, so env is usually nil
	// here and the unit's known defaults apply.
	env := agentEnviron()
	apiAddr := envOr(env, "PIPER_API_ADDR", "127.0.0.1:8088")
	printAgentVersions(stdout, apiAddr)
	fmt.Fprintf(stdout, "  control API  http://%s\n", apiAddr)
	fmt.Fprintf(stdout, "  http/https   %s / %s\n", envOr(env, "PIPER_HTTP_ADDR", ":80"), envOr(env, "PIPER_HTTPS_ADDR", ":443"))
	fmt.Fprintf(stdout, "  data dir     %s\n", envOr(env, "PIPER_DATA_DIR", "/var/lib/piper"))
	return 0
}

// seedUserEnv writes content to the rootless agent's env file if it isn't there
// yet — skip-if-exists, so operator edits are never clobbered.
func seedUserEnv(content string, stderr io.Writer) int {
	envPath, err := userEnvPath()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if _, err := os.Stat(envPath); err != nil {
		if err := os.MkdirAll(filepath.Dir(envPath), 0o700); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		if err := os.WriteFile(envPath, []byte(content), 0o600); err != nil {
			fmt.Fprintf(stderr, "error: writing env: %v\n", err)
			return 1
		}
	}
	return 0
}

// agentEnviron reads the running agent's start-time environment from
// /proc/<MainPID>/environ (NUL-separated KEY=VALUE), so `status` can report the
// address the agent is actually bound to — honoring any env-file overrides
// (e.g. PIPER_API_ADDR=0.0.0.0:8088 for LAN access). Returns nil when the
// agent isn't running or /proc can't be read (e.g. the system piperd's
// environ as non-root, the common case). A var so tests stub it.
var agentEnviron = func(scope ...string) map[string]string {
	args := append(append([]string{}, scope...), "show", userUnitName, "--property=MainPID", "--value")
	out, err := systemctlRun(args...)
	if err != nil {
		return nil
	}
	pid, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil || pid <= 0 {
		return nil
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return nil
	}
	m := map[string]string{}
	for _, kv := range strings.Split(string(raw), "\x00") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
}

// envOr looks key up in the running agent's environment, falling back to def
// (the same default piperd's config.Load would apply) when unset or unread.
func envOr(env map[string]string, key, def string) string {
	if v := env[key]; v != "" {
		return v
	}
	return def
}

// launchdPlistTemplate is the LaunchAgent `up` materializes. The job is a
// /bin/sh wrapper so it can pin the rootless environment (high ports, and a
// Caddy admin off the default :2019, matching the Linux rootless unit), source
// the optional env file, and append to the agent's logs before exec'ing piperd.
// The verbs are %s label, %s resolved piperd path. RunAtLoad starts the job as
// soon as `up` bootstraps it and KeepAlive restarts it if it crashes — both
// scoped to this login session, since the plist is not login-scanned.
const launchdPlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>/bin/sh</string>
    <string>-c</string>
    <string>mkdir -p "$HOME/.piper"
export XDG_DATA_HOME="$HOME/.piper/piperd" XDG_CONFIG_HOME="$HOME/.piper/piperd"
export PIPER_HTTP_ADDR=":8080" PIPER_HTTPS_ADDR=":8443" PIPER_CADDY_ADMIN="http://127.0.0.1:2020"
set -a
[ -f "$HOME/.piper/piperd.env" ] &amp;&amp; . "$HOME/.piper/piperd.env"
set +a
exec &gt;&gt; "$HOME/.piper/piper.log" 2&gt;&gt; "$HOME/.piper/piper.err.log"
exec "%s"</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
</dict>
</plist>
`

// xmlText escapes s for an XML text node.
var xmlText = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace

func renderLaunchdPlist(piperd string) string {
	return fmt.Sprintf(launchdPlistTemplate, launchdLabel, xmlText(piperd))
}

// materializeLaunchd writes the LaunchAgent for the piperd this CLI resolved,
// rewriting a stale one (an older piper's, or one naming a binary that has since
// moved), and seeds the env file without clobbering edits.
func materializeLaunchd(stderr io.Writer) int {
	piperd, err := piperdPath()
	if err != nil {
		fmt.Fprintln(stderr, "error: piperd not found next to the piper binary or on PATH — run the installer first (see README)")
		return 1
	}
	plist, err := launchdPlistPath()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	want := renderLaunchdPlist(piperd)
	if cur, err := os.ReadFile(plist); err != nil || string(cur) != want {
		if err := os.MkdirAll(filepath.Dir(plist), 0o700); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		if err := os.WriteFile(plist, []byte(want), 0o644); err != nil {
			fmt.Fprintf(stderr, "error: writing launchd agent: %v\n", err)
			return 1
		}
	}
	return seedUserEnv(embeddedMacEnv, stderr)
}

// evictLoginScannedPlist removes the LaunchAgent piper used to ship into
// ~/Library/LaunchAgents. launchd loads that directory at every login, so left
// in place it keeps starting a stale piperd behind this one's back — and it
// carries the same label, so bootstrap would fail while it is loaded.
func evictLoginScannedPlist(stderr io.Writer) int {
	legacy, err := legacyLaunchAgentPath()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if !fileExists(legacy) {
		return 0
	}
	launchctlRun("bootout", guiTarget()+"/"+launchdLabel) // best-effort: it may not be loaded
	if err := os.Remove(legacy); err != nil {
		fmt.Fprintf(stderr, "warning: could not remove the old login-scanned agent at %s (it will keep starting piperd at login): %v\n", legacy, err)
	}
	return 0
}

func agentUp(stdout, stderr io.Writer) int {
	if code := evictLoginScannedPlist(stderr); code != 0 {
		return code
	}
	if code := materializeLaunchd(stderr); code != 0 {
		return code
	}
	plist, err := launchdPlistPath()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	out, err := launchctlRun("bootstrap", guiTarget(), plist)
	if err != nil {
		if strings.Contains(out, "already") || strings.Contains(out, "5: Input/output error") {
			fmt.Fprintln(stdout, "piperd already running")
			return 0
		}
		fmt.Fprintf(stderr, "error: launchctl bootstrap failed: %v\n%s", err, out)
		return 1
	}
	fmt.Fprintln(stdout, "piperd started")
	fmt.Fprintln(stdout, "note: won't survive a reboot — run `piper agent up` again after one")
	return 0
}

func agentDown(stdout, stderr io.Writer) int {
	out, err := launchctlRun("bootout", guiTarget()+"/"+launchdLabel)
	if err != nil {
		if strings.Contains(out, "No such process") || strings.Contains(out, "not find") {
			fmt.Fprintln(stdout, "piperd already stopped")
			return 0
		}
		fmt.Fprintf(stderr, "error: launchctl bootout failed: %v\n%s", err, out)
		return 1
	}
	fmt.Fprintln(stdout, "piperd stopped")
	return 0
}

func agentStatus(stdout, stderr io.Writer) int {
	out, err := launchctlRun("print", guiTarget()+"/"+launchdLabel)
	if err != nil {
		fmt.Fprintln(stdout, "piperd: stopped")
		return 0
	}
	if strings.Contains(out, "state = running") {
		fmt.Fprintln(stdout, "piperd: running")
		// The LaunchAgent pins high ports but leaves the control API on
		// piperd's default, so there is no plist env to consult.
		printAgentVersions(stdout, "127.0.0.1:8088")
	} else {
		fmt.Fprintln(stdout, "piperd: loaded (not running)")
	}
	return 0
}

// selfExecSudo re-runs this binary under sudo with its own absolute path,
// passing args through and wiring the real stdio so sudo can prompt for a
// password. A rootless piper lives in ~/.local/bin, which sudo's secure_path
// skips — but an absolute path bypasses the PATH lookup entirely, so the user
// never needs to type the path or a symlink. A var so tests stub it. Returns
// the child's exit code.
var selfExecSudo = func(args []string, stdout, stderr io.Writer) int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "error: cannot locate the piper binary to re-run under sudo: %v\n", err)
		return 1
	}
	cmd := exec.Command("sudo", append([]string{exe}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		fmt.Fprintf(stderr, "error: could not re-run under sudo: %v\n", err)
		return 1
	}
	return 0
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
