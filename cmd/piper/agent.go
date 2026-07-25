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
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/piperbox/piper/internal/client"
	"github.com/piperbox/piper/internal/config"
)

//go:embed piperd.service
var embeddedSystemUnit string

//go:embed piperd.env.example
var embeddedSystemEnv string

//go:embed piperd.user.service
var embeddedUserUnit string

//go:embed piperd.env.user.example
var embeddedUserEnv string

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

// installedPiperdVersion runs the on-disk piperd's own --version. This is the
// half that catches the daemonize trap: `piper agent daemonize` installs a new
// binary but leaves the old process running, so disk and running disagree and
// the upgrade silently has not taken (#375). A var so tests can stub it.
var installedPiperdVersion = func() (string, error) {
	p, err := piperdPath()
	if err != nil {
		return "", err
	}
	out, err := exec.Command(p, "--version").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
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
)

// waitActive reports whether the unit reaches and holds `active`, returning the
// last state seen. A Type=simple unit reports `active` the instant ExecStart
// forks, so one that immediately exits and enters Restart= backoff only shows
// as `activating`/`failed`/`inactive` a moment later — so we poll and fail on
// the first non-active sample rather than trusting the initial `active`. scope
// is the systemctl scope prefix (nil for the system manager, {"--user"} for the
// per-user one).
func waitActive(scope ...string) (string, bool) {
	args := append(append([]string{}, scope...), "is-active", userUnitName)
	var state string
	for i := 0; i < activePollAttempts; i++ {
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

// Overridable system install targets + identity, so daemonize unit-tests
// against temp dirs and stubbed identity.
var (
	agentEUID     = os.Geteuid
	systemBinDir  = "/usr/local/bin"
	systemUnitDir = "/etc/systemd/system"
	systemEnvDir  = "/etc/piper"
)

func systemUnitFile() string { return filepath.Join(systemUnitDir, userUnitName+".service") }

// systemTier reports whether this box has been daemonized: the system unit's
// presence decides which service up/down/status control.
func systemTier() bool {
	_, err := os.Stat(systemUnitFile())
	return err == nil
}

// userHomeDir resolves a username to its home directory; a var so tests can stub it.
var userHomeDir = func(username string) (string, error) {
	u, err := user.Lookup(username)
	if err != nil {
		return "", err
	}
	return u.HomeDir, nil
}

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

func agentLinux(args []string, stdout, stderr io.Writer) int {
	usage := func() int {
		fmt.Fprintln(stderr, "usage: piper agent <up|down|status|daemonize [--undo]>")
		return 2
	}
	if len(args) == 0 {
		return usage()
	}
	switch args[0] {
	case "up":
		if len(args) != 1 {
			return usage()
		}
		return agentUpLinux(stdout, stderr)
	case "down":
		if len(args) != 1 {
			return usage()
		}
		return agentDownLinux(stdout, stderr)
	case "status":
		if len(args) != 1 {
			return usage()
		}
		return agentStatusLinux(stdout, stderr)
	case "daemonize":
		switch {
		case len(args) == 1:
			return agentDaemonize(false, stdout, stderr)
		case len(args) == 2 && args[1] == "--undo":
			return agentDaemonize(true, stdout, stderr)
		default:
			return usage()
		}
	default:
		return usage()
	}
}

// materializeRootless writes the embedded rootless user unit (refreshing a
// stale copy left by an older piper) and seeds ~/.piper/piperd.env
// skip-if-exists, so `up` works on a box that has only the binaries.
func materializeRootless(stderr io.Writer) int {
	unit, err := userUnitPath()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if cur, err := os.ReadFile(unit); err != nil || string(cur) != embeddedUserUnit {
		if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		if err := os.WriteFile(unit, []byte(embeddedUserUnit), 0o644); err != nil {
			fmt.Fprintf(stderr, "error: writing user unit: %v\n", err)
			return 1
		}
		if out, err := systemctlRun("--user", "daemon-reload"); err != nil {
			fmt.Fprintf(stderr, "error: systemctl --user daemon-reload: %v\n%s", err, out)
			return 1
		}
	}
	return seedUserEnv(embeddedUserEnv, stderr)
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

func agentUpLinux(stdout, stderr io.Writer) int {
	if systemTier() {
		if agentEUID() != 0 {
			fmt.Fprintln(stderr, "piperd is daemonized — controlling the system service needs root, re-running under sudo…")
			return selfExecSudo([]string{"agent", "up"}, stdout, stderr)
		}
		if out, err := systemctlRun("start", userUnitName); err != nil {
			fmt.Fprintf(stderr, "error: systemctl start failed: %v\n%s", err, out)
			return 1
		}
		if state, ok := waitActive(); !ok {
			fmt.Fprintf(stderr, "error: piperd started but is not active (state: %s) — it may be crash-looping.\nCheck: systemctl status piperd\n", state)
			return 1
		}
		fmt.Fprintln(stdout, "piperd started (system service)")
		return 0
	}
	if code := materializeRootless(stderr); code != 0 {
		return code
	}
	if out, err := systemctlRun("--user", "start", userUnitName); err != nil {
		fmt.Fprintf(stderr, "error: systemctl --user start failed: %v\n%s", err, out)
		return 1
	}
	// `systemctl start` returns before a Type=simple unit can fail, so confirm
	// it actually stays up rather than crash-looping (e.g. a port already held
	// by a leftover system piperd) (#211).
	if state, ok := waitActive("--user"); !ok {
		fmt.Fprintf(stderr, "error: piperd started but is not active (state: %s) — it may be crash-looping.\nCheck: systemctl --user status piperd (a leftover system piperd holding :8088 is a common cause — stop it with `sudo systemctl stop piperd`).\nSee startup logs: docs/getting-started.md (Rootless on Linux).\n", state)
		return 1
	}
	fmt.Fprintln(stdout, "piperd started")
	fmt.Fprintln(stdout, "note: won't survive a reboot — run `piper agent daemonize` to make it permanent")
	return 0
}

func agentDownLinux(stdout, stderr io.Writer) int {
	if systemTier() {
		if agentEUID() != 0 {
			fmt.Fprintln(stderr, "piperd is daemonized — controlling the system service needs root, re-running under sudo…")
			return selfExecSudo([]string{"agent", "down"}, stdout, stderr)
		}
		if out, err := systemctlRun("stop", userUnitName); err != nil {
			fmt.Fprintf(stderr, "error: systemctl stop failed: %v\n%s", err, out)
			return 1
		}
		fmt.Fprintln(stdout, "piperd stopped (system service)")
		return 0
	}
	if out, err := systemctlRun("--user", "stop", userUnitName); err != nil {
		fmt.Fprintf(stderr, "error: systemctl --user stop failed: %v\n%s", err, out)
		return 1
	}
	fmt.Fprintln(stdout, "piperd stopped")
	return 0
}

// agentEnviron reads the running agent's start-time environment from
// /proc/<MainPID>/environ (NUL-separated KEY=VALUE), so `status` can report the
// address the agent is actually bound to — honoring any env-file overrides
// (e.g. PIPER_API_ADDR=0.0.0.0:8088 for LAN access). scope is the systemctl
// scope prefix ({"--user"} for the rootless agent, nil for the system one).
// Returns nil when the agent isn't running or /proc can't be read (e.g. the
// system piperd's environ as non-root). A var so tests stub it.
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

func agentStatusLinux(stdout, stderr io.Writer) int {
	if systemTier() {
		out, _ := systemctlRun("is-active", userUnitName)
		if strings.TrimSpace(out) != "active" {
			fmt.Fprintln(stdout, "piperd: stopped (system service)")
			return 0
		}
		fmt.Fprintln(stdout, "piperd: running (system service)")
		// The system piperd's /proc environ is root-only, so env is usually nil
		// here and the system unit's known defaults apply.
		env := agentEnviron()
		apiAddr := envOr(env, "PIPER_API_ADDR", "127.0.0.1:8088")
		printAgentVersions(stdout, apiAddr)
		fmt.Fprintf(stdout, "  control API  http://%s\n", apiAddr)
		fmt.Fprintf(stdout, "  http/https   %s / %s\n", envOr(env, "PIPER_HTTP_ADDR", ":80"), envOr(env, "PIPER_HTTPS_ADDR", ":443"))
		fmt.Fprintf(stdout, "  data dir     %s\n", envOr(env, "PIPER_DATA_DIR", "/var/lib/piper"))
		return 0
	}
	unit, err := userUnitPath()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if _, err := os.Stat(unit); err != nil {
		fmt.Fprintln(stdout, "piperd: not set up (run `piper agent up`)")
		return 0
	}
	out, _ := systemctlRun("--user", "is-active", userUnitName)
	if strings.TrimSpace(out) != "active" {
		fmt.Fprintln(stdout, "piperd: stopped")
		return 0
	}
	fmt.Fprintln(stdout, "piperd: running")
	env := agentEnviron("--user")
	apiAddr := envOr(env, "PIPER_API_ADDR", "127.0.0.1:8088")
	printAgentVersions(stdout, apiAddr)
	fmt.Fprintf(stdout, "  control API  http://%s\n", apiAddr)
	// http/https are set by the user unit (:8080/:8443); only shown when we
	// could read them, since piperd's built-in :80/:443 defaults would misreport
	// a rootless instance.
	if h := env["PIPER_HTTP_ADDR"]; h != "" {
		fmt.Fprintf(stdout, "  http/https   %s / %s\n", h, envOr(env, "PIPER_HTTPS_ADDR", "?"))
	}
	fmt.Fprintf(stdout, "  data dir     %s\n", envOr(env, "PIPER_DATA_DIR", config.DefaultDataDir()))
	return 0
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

// agentDaemonize promotes the rootless per-user agent into the systemd system
// daemon (durable, :80/:443, boot-surviving); with undo it demotes back. Linux
// only. Promotion does NOT migrate ~/.piper state to /var/lib/piper — a fresh
// durable install.
func agentDaemonize(undo bool, stdout, stderr io.Writer) int {
	if agentEUID() != 0 {
		// Needs root; re-run ourselves under sudo by absolute path so the user
		// runs a bare `piper agent daemonize` — no sudo, no path (#211).
		verb := "promotion"
		if undo {
			verb = "demotion"
		}
		fmt.Fprintf(stderr, "%s needs root — re-running under sudo…\n", verb)
		args := []string{"agent", "daemonize"}
		if undo {
			args = append(args, "--undo")
		}
		return selfExecSudo(args, stdout, stderr)
	}
	if undo {
		return agentDaemonizeUndo(stdout, stderr)
	}
	sudoUser := os.Getenv("SUDO_USER")

	// 1. Tear down the invoking user's rootless service (best-effort; a real
	// root login has no rootless service to tear down).
	if sudoUser != "" {
		if out, err := systemctlRun("--user", "--machine="+sudoUser+"@.host", "disable", "--now", userUnitName); err != nil {
			fmt.Fprintf(stderr, "warning: could not stop the rootless service for %s (run `piper agent down` as %s if it lingers): %v\n%s", sudoUser, sudoUser, err, out)
		}
	}

	// 2. Ensure piperd + the CLI live in the system bindir: copied from the
	// invoking user's ~/.local/bin when running via sudo (so the box afterward
	// matches a root install — piper on sudo's secure_path resolves by name,
	// #211); a real-root box already has them there from the installer.
	var home string
	if sudoUser != "" {
		var err error
		home, err = userHomeDir(sudoUser)
		if err != nil {
			fmt.Fprintf(stderr, "error: cannot resolve home for %s: %v\n", sudoUser, err)
			return 1
		}
	}
	if err := os.MkdirAll(systemBinDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	for _, name := range []string{"piperd", "piper"} {
		dst := filepath.Join(systemBinDir, name)
		if home != "" {
			if src := filepath.Join(home, ".local", "bin", name); fileExists(src) {
				if err := copyFile(src, dst, 0o755); err != nil {
					fmt.Fprintf(stderr, "error: installing %s to %s: %v\n", name, systemBinDir, err)
					return 1
				}
				continue
			}
		}
		if !fileExists(dst) {
			fmt.Fprintf(stderr, "error: %s not found in %s or ~/.local/bin — run the installer first (see README)\n", name, systemBinDir)
			return 1
		}
	}

	// 3. Write the system unit.
	if err := os.MkdirAll(systemUnitDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if err := os.WriteFile(filepath.Join(systemUnitDir, "piperd.service"), []byte(embeddedSystemUnit), 0o644); err != nil {
		fmt.Fprintf(stderr, "error: writing unit: %v\n", err)
		return 1
	}

	// 4. Seed the env file (skip-if-exists — never clobber operator edits).
	if err := os.MkdirAll(systemEnvDir, 0o700); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	envPath := filepath.Join(systemEnvDir, "piperd.env")
	if _, err := os.Stat(envPath); err != nil {
		if err := os.WriteFile(envPath, []byte(embeddedSystemEnv), 0o600); err != nil {
			fmt.Fprintf(stderr, "error: writing env: %v\n", err)
			return 1
		}
	}

	// 5. Enable + start the system service.
	if out, err := systemctlRun("daemon-reload"); err != nil {
		fmt.Fprintf(stderr, "error: systemctl daemon-reload: %v\n%s", err, out)
		return 1
	}
	if out, err := systemctlRun("enable", "--now", "piperd"); err != nil {
		fmt.Fprintf(stderr, "error: systemctl enable --now piperd: %v\n%s", err, out)
		return 1
	}
	// `enable --now` returns before a Type=simple unit can fail, so confirm the
	// system service actually stays up. A rootless piperd still holding
	// :2019/:8088 (when the best-effort teardown above could not reach the user
	// manager) is the common cause of a crash-loop here (#211).
	if state, ok := waitActive(); !ok {
		fmt.Fprintf(stderr, "error: system piperd enabled but is not active (state: %s) — it may be crash-looping.\nCheck: systemctl status piperd. If the rootless service is still holding :2019/:8088, stop it: run `piper agent down` as %s.\n", state, sudoUser)
		return 1
	}
	fmt.Fprintln(stdout, "piperd daemonized — system service on :80/:443, boot-surviving")
	return 0
}

// agentDaemonizeUndo demotes the system daemon back to rootless-capable: stop +
// disable, remove the system unit. /etc/piper/piperd.env and the binaries stay,
// so re-daemonizing later picks the config back up. State in /var/lib/piper is
// not migrated to ~/.piper — the same fresh-state stance as promotion. Runs as
// root (agentDaemonize escalates before dispatching here).
func agentDaemonizeUndo(stdout, stderr io.Writer) int {
	if !systemTier() {
		fmt.Fprintln(stdout, "piperd is not daemonized — nothing to undo")
		return 0
	}
	if out, err := systemctlRun("disable", "--now", userUnitName); err != nil {
		fmt.Fprintf(stderr, "error: systemctl disable --now piperd: %v\n%s", err, out)
		return 1
	}
	if err := os.Remove(systemUnitFile()); err != nil {
		fmt.Fprintf(stderr, "error: removing %s: %v\n", systemUnitFile(), err)
		return 1
	}
	if out, err := systemctlRun("daemon-reload"); err != nil {
		fmt.Fprintf(stderr, "error: systemctl daemon-reload: %v\n%s", err, out)
		return 1
	}
	fmt.Fprintln(stdout, "piperd un-daemonized — system service removed (kept /etc/piper/piperd.env and the binaries)")
	fmt.Fprintln(stdout, "note: state in /var/lib/piper is not migrated — apps deployed by the system agent won't appear rootless. `piper agent up` starts a fresh rootless agent.")
	return 0
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	// Remove any existing file first: WriteFile applies mode only on create, and
	// writing in place to a running binary fails with ETXTBSY. Removing then
	// recreating dodges both (re-running daemonize over a live system piperd).
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.WriteFile(dst, b, mode)
}
