package main

import (
	"encoding/json"
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

// agentGOOS is runtime.GOOS; a var so tests can exercise the non-darwin gate.
var agentGOOS = runtime.GOOS

// piperdOnPATH is what the user's shell runs when they type `piperd`. A var so
// tests can stub it.
var piperdOnPATH = func() (string, error) { return exec.LookPath("piperd") }

// piperdPath resolves the piperd to probe when no system unit names one: the
// one sitting next to this piper binary (the installer places both in one
// prefix), else whatever is on PATH. A var so tests can stub it.
var piperdPath = func() (string, error) {
	if exe, err := os.Executable(); err == nil {
		if cand := filepath.Join(filepath.Dir(exe), "piperd"); fileExists(cand) {
			return cand, nil
		}
	}
	return piperdOnPATH()
}

// runningAgentInfo asks the control API which build is actually serving, and
// for the listen config it loaded (#476).
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
var runningAgentInfo = func(apiAddr string) (client.AgentInfo, error) {
	target := dialableAddr(apiAddr)
	token := ""
	if cc, err := config.LoadClient(); err == nil && sameBox(cc.Addr, target) {
		token = cc.Token
	}
	return client.New("http://"+target, token).WithTimeout(2 * time.Second).AgentInfo()
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
// half that catches a disk-vs-running mismatch: a binary replaced on disk
// without a service restart leaves the old process running, so disk and
// running disagree and the upgrade silently has not taken (#375).
//
// path is the binary to probe — the unit's ExecStart when there is a system
// unit, which is the only binary the running daemon can be compared against.
// Empty means "resolve one yourself", the user-mode/macOS case. A var so tests
// can stub it.
var installedPiperdVersion = func(path string) (string, error) {
	if path == "" {
		p, err := piperdPath()
		if err != nil {
			return "", err
		}
		path = p
	}
	return binaryVersion(path)
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
// reads as "the fix didn't work". diskPath is the binary to compare against
// (the unit's ExecStart; empty to resolve one). It returns the daemon's full
// self-report so status can print the listen config the daemon actually
// loaded; zero when the daemon could not answer (#476).
func printAgentVersions(stdout io.Writer, apiAddr, diskPath string) client.AgentInfo {
	info, rerr := runningAgentInfo(apiAddr)
	running := info.Version
	disk, derr := installedPiperdVersion(diskPath)

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
		return client.AgentInfo{}
	}
	if derr == nil && disk != running {
		fmt.Fprintf(stdout, "  version      %s  ⚠ %s\n", running, skewNote(running, disk))
		return info
	}
	fmt.Fprintf(stdout, "  version      %s\n", running)
	return info
}

// skewNote words a running-vs-disk mismatch by direction. The old wording
// assumed disk ≥ running and always said "restart to apply"; when the running
// daemon is the newer build that advice accomplishes nothing, and would be a
// downgrade presented as an upgrade if it did take (#472).
//
// A running build we cannot parse (an agent too old to report one) falls to the
// restart wording, which is the advice that helps when disk is ahead or unknown.
func skewNote(running, disk string) string {
	if versionNewer(running, disk) {
		return fmt.Sprintf("the binary on disk is %s — the running build is newer", disk)
	}
	return fmt.Sprintf("%s is installed on disk — restart piperd to apply", disk)
}

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
// journal invocation to point at.
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

// agentEUID is os.Geteuid; a var so tests can stub identity.
var agentEUID = os.Geteuid

// agent dispatches `piper agent ...` to the platform's agent manager.
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

// brewServicesRun shells out to `brew services <args...>`; a var so tests can
// substitute it without a real Homebrew.
var brewServicesRun = func(args ...string) (string, error) {
	out, err := exec.Command("brew", append([]string{"services"}, args...)...).CombinedOutput()
	return string(out), err
}

// brewFound reports whether Homebrew is on PATH; a var so tests can stub it.
var brewFound = func() bool {
	_, err := exec.LookPath("brew")
	return err == nil
}

const notInstalledDarwin = "piperd on macOS is managed by Homebrew — `brew install piperbox/tap/piper`"

// agentDarwin delegates lifecycle to brew services: the formula's service
// block owns the launchd plumbing, so the CLI never touches plists.
func agentDarwin(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: piper agent <up|down|status>")
		return 2
	}
	switch args[0] {
	case "up", "down", "status":
	default:
		fmt.Fprintln(stderr, "usage: piper agent <up|down|status>")
		return 2
	}
	if !brewFound() {
		fmt.Fprintln(stderr, "error: Homebrew not found — "+notInstalledDarwin)
		return 1
	}
	switch args[0] {
	case "up":
		out, err := brewServicesRun("start", "piper")
		if err != nil {
			fmt.Fprintf(stderr, "error: brew services start failed: %v\n%s\nhint: %s\n", err, out, notInstalledDarwin)
			return 1
		}
		fmt.Fprintln(stdout, "piperd started (brew services — runs now and at every login)")
		return 0
	case "down":
		out, err := brewServicesRun("stop", "piper")
		if err != nil {
			fmt.Fprintf(stderr, "error: brew services stop failed: %v\n%s", err, out)
			return 1
		}
		fmt.Fprintln(stdout, "piperd stopped")
		return 0
	case "status":
		return agentStatusDarwin(stdout)
	default:
		fmt.Fprintln(stderr, "usage: piper agent <up|down|status>")
		return 2
	}
}

func agentStatusDarwin(stdout io.Writer) int {
	out, err := brewServicesRun("info", "piper", "--json")
	if err != nil {
		fmt.Fprintln(stdout, "piperd: not installed — "+notInstalledDarwin)
		return 0
	}
	var infos []struct {
		Running bool   `json:"running"`
		Status  string `json:"status"`
	}
	if json.Unmarshal([]byte(out), &infos) != nil || len(infos) == 0 {
		fmt.Fprintln(stdout, "piperd: unknown (unexpected brew services output)")
		return 0
	}
	if infos[0].Running {
		fmt.Fprintln(stdout, "piperd: running (brew services)")
		// The formula's service block leaves the control API on piperd's
		// default, and launchd has no ExecStart to read.
		printAgentVersions(stdout, "127.0.0.1:8088", "")
		return 0
	}
	fmt.Fprintln(stdout, "piperd: stopped")
	return 0
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

// unitExecStart returns the binary the piperd unit actually runs, or "" when
// systemd won't say. Everything about a system service's version — is it
// current, would a restart help — is a claim about *this* path, not about
// whatever the caller's PATH resolves to (#472).
func unitExecStart() string {
	out, err := systemctlRun("show", userUnitName, "--property=ExecStart", "--value")
	if err != nil {
		return ""
	}
	return execStartPath(out)
}

// execStartPath pulls the binary out of systemd's ExecStart value, which reads
// `{ path=/usr/local/bin/piperd ; argv[]=… ; ignore_errors=no }`.
func execStartPath(value string) string {
	_, rest, ok := strings.Cut(value, "path=")
	if !ok {
		return ""
	}
	path, _, _ := strings.Cut(rest, ";")
	return strings.TrimSpace(path)
}

// warnPATHShadow flags a `piperd` earlier on the caller's PATH than the one the
// service runs. That stale user-level copy is what made the version report lie
// in #472, and until now nothing detected it after install time.
func warnPATHShadow(stdout io.Writer, execStart string) {
	onPath, err := piperdOnPATH()
	if execStart == "" || err != nil || samePiperd(onPath, execStart) {
		return
	}
	fmt.Fprintf(stdout, "  ⚠ `piperd` on your PATH is %s, but the service runs %s\n", onPath, execStart)
}

// samePiperd compares two piperd paths through any symlinks, so a packaged
// symlink into the real binary is not reported as a shadow.
func samePiperd(a, b string) bool {
	if r, err := filepath.EvalSymlinks(a); err == nil {
		a = r
	}
	if r, err := filepath.EvalSymlinks(b); err == nil {
		b = r
	}
	return a == b
}

const notInstalledLinuxHint = "`sudo apt install piperd` (see the README for other channels)"
const notInstalledLinux = "piperd is not installed — " + notInstalledLinuxHint

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
		fmt.Fprintln(stdout, "piperd: not installed — "+notInstalledLinuxHint)
		return 0
	}
	if state, ok := pollActive(statusPollAttempts); !ok {
		fmt.Fprintf(stdout, "piperd: %s\n", unitStateWord(state))
		printRestartHint(stdout, state, "journalctl -u piperd")
		return 0
	}
	fmt.Fprintln(stdout, "piperd: running")
	env := agentEnviron()
	apiAddr := envOr(env, "PIPER_API_ADDR", "127.0.0.1:8088")
	execStart := unitExecStart()
	info := printAgentVersions(stdout, apiAddr, execStart)
	warnPATHShadow(stdout, execStart)
	fmt.Fprintf(stdout, "  control API  http://%s\n", apiAddr)
	if info.HTTPAddr != "" {
		// The daemon's self-report: the config it actually loaded.
		fmt.Fprintf(stdout, "  http/https   %s / %s\n", info.HTTPAddr, info.HTTPSAddr)
		fmt.Fprintf(stdout, "  data dir     %s\n", info.DataDir)
		return 0
	}
	// No self-report (daemon unreachable, token-locked, or too old). The
	// process env is still truth when readable, but the system piperd's
	// /proc environ is root-only, so for a non-root caller env is nil and
	// the built-in defaults below are a guess — say so rather than state
	// them as fact (#476).
	assumed := ""
	if env == nil {
		assumed = " (assumed — the agent did not report)"
	}
	fmt.Fprintf(stdout, "  http/https   %s / %s%s\n", envOr(env, "PIPER_HTTP_ADDR", ":80"), envOr(env, "PIPER_HTTPS_ADDR", ":443"), assumed)
	fmt.Fprintf(stdout, "  data dir     %s%s\n", envOr(env, "PIPER_DATA_DIR", "/var/lib/piper"), assumed)
	return 0
}

// agentEnviron reads the running agent's start-time environment from
// /proc/<MainPID>/environ (NUL-separated KEY=VALUE), so `status` can report the
// address the agent is actually bound to — honoring any env-file overrides
// (e.g. PIPER_API_ADDR=0.0.0.0:8088 for LAN access). Returns nil when the
// agent isn't running or /proc can't be read (e.g. the system piperd's
// environ as non-root, the common case). A var so tests stub it.
var agentEnviron = func() map[string]string {
	out, err := systemctlRun("show", userUnitName, "--property=MainPID", "--value")
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
