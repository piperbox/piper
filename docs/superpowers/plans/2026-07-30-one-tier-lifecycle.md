# One-Tier Lifecycle Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `piper agent up|down|status` become thin wrappers — systemctl over the deb-installed system unit on Linux, `brew services` on macOS — and `daemonize`, `--undo`, the rootless tier, and the self-generated LaunchAgent machinery are deleted.

**Architecture:** Pure deletion + thin rewrite of `cmd/piper/agent.go` (965 → ~450 lines) and its tests (1515 lines, 51 tests → far fewer), plus the collateral: tier-aware restart hints in `relayonboard.go` collapse to two constants, five embedded asset files and two `packaging/systemd` user-tier files become consumer-less and die, and docs/installer text stop naming deleted verbs. The CLI never materializes units or plists again — packages own installation, the CLI only drives lifecycles.

**Tech Stack:** Go (stdlib only), systemctl, `brew services` (JSON info output), existing var-stubbing test pattern.

This is **plan 3 of 4** of [the onboarding & packaging spec](../specs/2026-07-30-onboarding-packaging-design.md) (deliverable 3, issue #446). Plans 1 (apt, live since v0.13.0) and 2 (brew tap, live since v0.14.0) are prerequisites — both channels exist, so the deletions are safe. A precise blast-radius map (file:line for every reference) was produced on 2026-07-30 and is baked into the tasks below.

## Global Constraints

- **Pre-1.x policy: no migration code, no legacy readers.** Existing rootless/LaunchAgent installs get a manual-cleanup docs note, nothing else.
- Deployment status strings, module path, no-cgo, default addresses: unchanged (this plan touches no runtime agent code).
- `piper agent` verbs after this plan: exactly `up`, `down`, `status` on both platforms. Usage line: `usage: piper agent <up|down|status>`.
- Linux: the CLI drives ONLY the system unit `piperd` (deb-installed at `/usr/lib/systemd/system/`, or manually at `/etc/systemd/system/`); presence is detected via `systemctl show piperd --property=LoadState --value` == `loaded` (works unprivileged, either location).
- Not-installed messages (exact): Linux → `piperd is not installed — sudo apt install piperd (see the README for other channels)`; macOS → `piperd on macOS is managed by Homebrew — brew install piperbox/tap/piper`.
- macOS: delegate to `brew services start|stop|info piper`; `info --json` parsed for status. Never invoke launchctl.
- `install.sh` must not mention `systemctl` (guardrail test `packaging/install/install_test.go:428` stays).
- Test style: reassign the package-level `var name = func(...)` stubs and `defer`-restore, matching the existing `agent_test.go` pattern.
- `make verify` before every commit. Conventional commits, `Part of #446`, trailer `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.

---

### Task 1: Linux one-tier — delete daemonize + rootless, rewrite up/down/status

**Files:**
- Modify: `cmd/piper/agent.go` (deletions + rewritten Linux dispatch)
- Delete: `cmd/piper/piperd.service`, `cmd/piper/piperd.env.example`, `cmd/piper/piperd.user.service`, `cmd/piper/piperd.env.user.example` (embeds whose only consumers die; the canonical `packaging/systemd/piperd.service` + `piperd.env.example` stay — the deb and manual installs use them)
- Delete: `packaging/systemd/piperd.user.service`, `packaging/systemd/piperd.env.user.example`
- Modify: `packaging/systemd/piperd_test.go` (delete `TestPiperdUserServiceContract` at :92-122; leave `TestPiperdDocumentation` alone — Task 5 rewrites it with the docs)
- Test: `cmd/piper/agent_test.go`

**Interfaces:**
- Consumes: existing vars `systemctlRun`, `selfExecSudo`, `agentEUID`, `waitActive`/`pollActive`, `agentEnviron`, `envOr`, `printAgentVersions`, `unitStateWord`, `printRestartHint` — all kept as-is.
- Produces: `unitLoaded() bool`, `notInstalledLinux` const, rewritten `agentLinux/agentUpLinux/agentDownLinux/agentStatusLinux`. Task 2 relies on `agent()` still dispatching `darwin → agentDarwin`, `linux → agentLinux`. Task 3 relies on `piperdPath` surviving in this package.

**Deletions in `agent.go`, by symbol** (from the blast-radius map; everything listed has no other consumer):
`embeddedSystemUnit`, `embeddedSystemEnv`, `embeddedUserUnit`, `embeddedUserEnv` (+ their four `//go:embed` directives), `userUnitPath`, `userEnvPath`, `materializeRootless`, `systemTier`, `systemBinDir`, `systemUnitDir`, `systemEnvDir`, `systemUnitFile`, `userHomeDir`, `copyFile`, `agentDaemonize`, `agentDaemonizeUndo`. In `agentLinux`: the `daemonize` case and the two-tier branches of up/down/status. NOTE: `seedUserEnv` keeps one caller (`materializeLaunchd`, macOS) until Task 2 — in this task delete only its rootless call site; Task 2 deletes the function itself.

- [ ] **Step 1: Write the failing tests** — replace the Linux-tier tests in `cmd/piper/agent_test.go` (delete every `Test*` exercising daemonize/undo/rootless/user-unit paths and `TestEmbeddedSystemFilesMatchCanonical` at :889-903; keep crash-loop/`pollActive`/`unitStateWord`/version-print tests that are tier-independent) and add:

```go
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
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./cmd/piper/ -run 'TestAgent(Up|Down|Status|Rejects)' -v`
Expected: FAIL — compile errors are acceptable at this point only if you wrote tests referencing `unitLoaded` before it exists; otherwise behavioral failures (daemonize still accepted, up materializes units).

- [ ] **Step 3: Rewrite the Linux side**

In `agent.go`: perform the deletion list above, then replace `agentLinux` and the three verbs with:

```go
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
```

Note `pollActive`/`waitActive` call `systemctlRun` with a scope prefix; after deleting the user tier, simplify their signatures to drop the `scope ...string` variadic (grep confirms no remaining `--user` caller). `userUnitName` const can shrink to the literal `"piperd"` inline or stay — keep the const, rename references untouched. Delete the four embed FILES in the same commit as the directives (a dangling `//go:embed` breaks the build).

Also delete `packaging/systemd/piperd.user.service`, `packaging/systemd/piperd.env.user.example`, and `TestPiperdUserServiceContract` from `packaging/systemd/piperd_test.go`.

- [ ] **Step 4: Run to verify pass**

Run: `go test ./cmd/piper/ ./packaging/...`
Expected: PASS. Then `go vet ./...` — catches any orphaned symbol the deletion missed.

- [ ] **Step 5: `make verify`, commit**

```bash
git add -A cmd/piper packaging/systemd
git commit -m "feat!: one-tier Linux lifecycle — delete daemonize and the rootless tier

piper agent up/down/status now wrap the deb-installed system unit only;
the CLI never materializes units. Pre-1.x: no migration, docs get a
manual-cleanup note in a later task.

Part of #446

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 2: macOS — delete LaunchAgent machinery, delegate to brew services

**Files:**
- Modify: `cmd/piper/agent.go` (darwin side)
- Delete: `cmd/piper/piperd.env.macos.example`
- Test: `cmd/piper/agent_test.go`

**Interfaces:**
- Consumes: `agent()` dispatch and `printAgentVersions` from Task 1's surviving code.
- Produces: `brewServicesRun` var (stub point: `func(args ...string) (string, error)`), `brewFound` var (`func() bool`), `notInstalledDarwin` const, rewritten `agentDarwin`. Task 3's restart hint references `brew services restart piper` textually only.

**Deletions in `agent.go`, by symbol:** `embeddedMacEnv` (+ directive + file), `launchdLabel`, `launchdPlistPath`, `legacyLaunchAgentPath`, `launchdPlistTemplate`, `xmlText`, `renderLaunchdPlist`, `materializeLaunchd`, `evictLoginScannedPlist`, `launchctlRun`, `guiTarget`, `seedUserEnv` (last caller gone), old `agentUp`/`agentDown`/`agentStatus` (darwin trio). In `agent_test.go`: every test exercising launchd/plist/mac-env paths, plus `TestMacosDocsMatchGeneratedAgent` (:908-937 — its premise, CLI-generated launchd, no longer exists; docs assertions return in Task 5's rewritten `TestPiperdDocumentation`).

- [ ] **Step 1: Write the failing tests**

```go
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
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./cmd/piper/ -run TestAgentDarwin -v`
Expected: FAIL (compile: `brewServicesRun`/`brewFound` undefined).

- [ ] **Step 3: Rewrite the darwin side**

Perform the deletion list, then:

```go
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
		// default.
		printAgentVersions(stdout, "127.0.0.1:8088")
		return 0
	}
	fmt.Fprintln(stdout, "piperd: stopped")
	return 0
}
```

Add `encoding/json` and drop now-unused imports (`os/user` etc. — let goimports/vet guide).

- [ ] **Step 4: Run to verify pass**

Run: `go test ./cmd/piper/`
Expected: PASS, and `go vet ./cmd/piper/` clean (catches unused imports/symbols).

- [ ] **Step 5: `make verify`, commit**

```bash
git add -A cmd/piper
git commit -m "feat!: macOS agent lifecycle delegates to brew services

Deletes the self-generated LaunchAgent machinery; the formula's service
block owns launchd now. macOS graduates from dev-only to a real tier.

Part of #446

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 3: Collapse tier-aware hints and stale comments

**Files:**
- Modify: `cmd/piper/relayonboard.go:341-375` (`restartHint`, `agentInstalled`)
- Modify: `internal/caddy/manager.go:116` (two-tier hint in error text)
- Modify: `cmd/piperd/main.go:304-308`, `cmd/piperd/main_test.go:794,827,889` (stale "LaunchAgent" comments → "brew service")
- Test: `cmd/piper/relayonboard_test.go`

**Interfaces:**
- Consumes: `agentGOOS`, `piperdPath` (both in package main, from agent.go); `config.SystemManaged()`.
- Produces: simplified `restartHint() string` and `agentInstalled(dataDir string) bool` with unchanged signatures — `connect` call sites at relayonboard.go:276/328 stay as-is.

- [ ] **Step 1: Rewrite the tests** — in `relayonboard_test.go`, delete `TestAgentInstalledDetectsEachFlavor` (:648), `TestRestartHintMatchesInstallFlavor` (:756), `TestConnectPrintsUserUnitRestartHint` (:836) (keep `TestConnectSystemManagedGuidesEnvInstall` :692 — system path unchanged) and add:

```go
func TestRestartHintPerPlatform(t *testing.T) {
	orig := agentGOOS
	t.Cleanup(func() { agentGOOS = orig })
	agentGOOS = "darwin"
	if got := restartHint(); got != "brew services restart piper" {
		t.Errorf("darwin hint = %q", got)
	}
	agentGOOS = "linux"
	if got := restartHint(); got != "sudo systemctl restart piperd" {
		t.Errorf("linux hint = %q", got)
	}
}

func TestAgentInstalledProbesBinaryAndState(t *testing.T) {
	dir := t.TempDir()
	origPath := piperdPath
	piperdPath = func() (string, error) { return "", errors.New("not found") }
	t.Cleanup(func() { piperdPath = origPath })
	// no data dir, no binary -> not installed
	if agentInstalled(filepath.Join(dir, "absent")) {
		t.Error("reported installed with no evidence")
	}
	// existing data dir counts (an enrolled box)
	if !agentInstalled(dir) {
		t.Error("existing data dir not treated as installed")
	}
	// a resolvable piperd binary counts (fresh deb/brew/manual install)
	piperdPath = func() (string, error) { return "/usr/bin/piperd", nil }
	if !agentInstalled(filepath.Join(dir, "absent")) {
		t.Error("resolvable piperd not treated as installed")
	}
}
```

(If `TestConnect*` tests currently rely on `userUnitPath` stubs for setup, update them to the new probes; the blast-radius map lists :680-683 and :814-817 as the stub sites.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./cmd/piper/ -run 'TestRestartHint|TestAgentInstalled' -v`
Expected: FAIL (old flavor-probing implementations).

- [ ] **Step 3: Implement**

```go
// restartHint names the restart command for a non-system-managed install.
// One tier per platform: brew's service on macOS, the system unit elsewhere.
func restartHint() string {
	if agentGOOS == "darwin" {
		return "brew services restart piper"
	}
	return "sudo systemctl restart piperd"
}

// agentInstalled reports whether this box has piperd at all — connect must
// fail loudly when pointed at a box with no agent (#173). One tier: an
// existing data dir (an enrolled box), a system-managed install, or a
// resolvable piperd binary (deb/brew/manual) all count.
func agentInstalled(dataDir string) bool {
	if _, err := os.Stat(dataDir); err == nil {
		return true
	}
	if config.SystemManaged() {
		return true
	}
	_, err := piperdPath()
	return err == nil
}
```

In `internal/caddy/manager.go:116`, replace the rootless/system two-tier stop hint with: `` stop it first (`sudo systemctl stop piperd`, or `brew services stop piper` on macOS) `` — keep the surrounding error text intact. Update the three `cmd/piperd` comments to say "brew service" instead of "macOS LaunchAgent" where they describe the rootless mac setup (wording only; no behavior). Check `cmd/piperd/token.go:69`'s hint still reads correctly (it does — system path survives; leave it).

- [ ] **Step 4: Run to verify pass**

Run: `go test ./cmd/piper/ ./cmd/piperd/ ./internal/caddy/`
Expected: PASS.

- [ ] **Step 5: `make verify`, commit**

```bash
git add cmd/piper/relayonboard.go cmd/piper/relayonboard_test.go internal/caddy/manager.go cmd/piperd/main.go cmd/piperd/main_test.go
git commit -m "refactor: collapse tier-aware restart hints to one per platform

Part of #446

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 4: Pin the brew service's data dir (settles the parked plan-2 finding)

**Files:**
- Modify: `.goreleaser.yaml` (brews `service:` block)

**Interfaces:**
- Consumes: the `brews:` entry from plan 2.
- Produces: a formula whose service env matches `config.DefaultDataDir()`; takes effect at the next stable release.

- [ ] **Step 1: Amend the service block** — replace its `environment_variables` line with:

```yaml
      environment_variables PIPER_DATA_DIR: "#{ENV["HOME"]}/.piper/piperd", XDG_DATA_HOME: "#{ENV["HOME"]}/.piper/piperd", XDG_CONFIG_HOME: "#{ENV["HOME"]}/.piper/piperd"
```

Rationale (record in the yaml as a comment above the line): `ENV["HOME"]` is evaluated when `brew services` generates the plist, as the invoking user — so the user-level service gets `~/.piper/piperd` (exactly `config.DefaultDataDir()`, keeping `piper connect`'s relay.json where piperd reads it, and preserving data from pre-brew macOS installs), and `sudo brew services` gets `/var/root/.piper/piperd` (deterministic, self-consistent). The XDG pair relocates embedded-Caddy/certmagic state to the same place, matching the old LaunchAgent's behavior.

- [ ] **Step 2: Validate** —

```bash
goreleaser check; rc=$?; [ "$rc" = 0 ] || [ "$rc" = 2 ] || exit "$rc"
goreleaser release --snapshot --clean
ruby -c dist/homebrew/Formula/piper.rb
./test/packaging/verify_deb.sh && ./test/packaging/verify_brew.sh
```

Expected: check exits 2 (deprecated-only, accepted), snapshot renders the formula, `ruby -c` says Syntax OK, both verify scripts ok.

- [ ] **Step 3: `make verify`, commit**

```bash
git add .goreleaser.yaml
git commit -m "fix: pin the brew service's PIPER_DATA_DIR to ~/.piper/piperd

Settles the plan-2 parked finding: user-level service matches
config.DefaultDataDir() (connect coherence + data continuity from the
old LaunchAgent); sudo variant lands deterministically in /var/root.

Part of #446

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 5: Interim docs truth + installer next-steps line

Scope guard: this makes the docs stop naming deleted verbs and state the one-tier reality tersely. The full channel-oriented docs overhaul is plan 4 — do not restructure documents here.

**Files:**
- Modify: `README.md:29-40` (quick start), `docs/getting-started.md` (rootless/daemonize sections), `docs/manual-setup.md` (rootless + macOS LaunchAgent sections), `docs/runbooks/git-deploy-e2e.md` (only the agent-lifecycle lines: 194, 203, 205, 211, 489-492, 525-548, 554-562 per the blast map — its piper-relay sections are out of scope)
- Modify: `install.sh:140` + `packaging/install/install_test.go:184`
- Modify: `packaging/systemd/piperd_test.go` (`TestPiperdDocumentation` :59-90)

**Interfaces:**
- Consumes: nothing from other tasks (text only).
- Produces: docs consistent with the new CLI; the rewritten doc-contract test.

- [ ] **Step 1: Rewrite `TestPiperdDocumentation` first** (it pins the docs, TDD on text):

```go
// TestPiperdDocumentation pins the one-tier install story in the docs: the
// deleted daemonize/rootless verbs must not resurface, and each platform's
// managed path is named.
func TestPiperdDocumentation(t *testing.T) {
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	gettingStarted := read("../../docs/getting-started.md")
	manualSetup := read("../../docs/manual-setup.md")
	readme := read("../../README.md")

	for name, doc := range map[string]string{"getting-started": gettingStarted, "manual-setup": manualSetup, "README": readme} {
		if strings.Contains(doc, "daemonize") {
			t.Errorf("%s still mentions daemonize", name)
		}
		if strings.Contains(doc, "systemctl --user") {
			t.Errorf("%s still documents the rootless user unit", name)
		}
	}
	for _, want := range []string{"apt install piperd", "brew services start piper"} {
		if !strings.Contains(gettingStarted, want) {
			t.Errorf("getting-started.md missing %q", want)
		}
	}
	if !strings.Contains(manualSetup, "systemctl enable --now piperd") {
		t.Errorf("manual-setup.md missing the manual unit-install command")
	}
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./packaging/systemd/ -run TestPiperdDocumentation` → FAIL (docs still say daemonize).

- [ ] **Step 3: Edit the docs.** README quick start becomes (Linux primary + macOS line):

```markdown
## 60-second quick start

On a Debian-family box (a Raspberry Pi counts):

​```bash
sudo install -d -m 0755 /etc/apt/keyrings
sudo curl -fsSL https://apt.piperbox.dev/piperbox.gpg -o /etc/apt/keyrings/piperbox.gpg
sudo curl -fsSL https://apt.piperbox.dev/piperbox.sources -o /etc/apt/sources.list.d/piperbox.sources
sudo apt update && sudo apt install piperd piper   # installs, enables, and starts the agent
piper login                  # GitHub device-flow; stores your account credential
piper connect                # enrolls this box on the public relay
sudo systemctl restart piperd
piper deploy blog --path .   # → https://<hash>-<you>.public.getpiper.dev
​```

On macOS: `brew install piperbox/tap/piper && brew services start piper`, then the same `piper login` flow.
```

(Write real fences, not the escaped ones shown here.) In `getting-started.md`: replace the "up runs until reboot; daemonize makes it permanent" narrative and both tier walkthroughs with: install via apt (Linux) or brew (macOS) → the service is durable from install; `piper agent up|down|status` drive it; add a short **"Upgrading from an older install"** subsection with the manual cleanup:

```markdown
### Upgrading from a pre-0.15 install

The rootless tier and CLI-generated LaunchAgent are gone. One-time cleanup:

- Linux rootless: `systemctl --user disable --now piperd` and delete
  `~/.config/systemd/user/piperd.service`, then install via apt.
- macOS: `launchctl bootout gui/$(id -u)/com.piperbox.piperd` and delete
  `~/.piper/com.piperbox.piperd.plist`, then `brew install piperbox/tap/piper
  && brew services start piper`. Data in `~/.piper/piperd` is picked up as-is.
```

(The cleanup snippet is the one allowed mention-shaped exception: it appears inside fenced commands, and the doc test greps prose via `systemctl --user` — put the cleanup commands in a fenced block and adjust the test's rootless assertion to tolerate it: change that check to `strings.Count(doc, "systemctl --user") > 1` failing, i.e. allow at most one occurrence. Encode exactly that in the test above — replace the simple Contains check with:

```go
		if strings.Count(doc, "systemctl --user") > 1 {
			t.Errorf("%s documents the rootless user unit beyond the one-time cleanup note", name)
		}
```

) In `manual-setup.md`: drop the rootless and LaunchAgent sections; the Linux manual path becomes: download `piperd.service` from the release assets, `install` binaries to `/usr/local/bin`, unit to `/etc/systemd/system/`, seed `/etc/piper/piperd.env` from `piperd.env.example`, `sudo systemctl daemon-reload && sudo systemctl enable --now piperd`; the macOS section becomes one line pointing at brew. In the runbook, update only the listed agent-lifecycle lines to the new verbs/paths.

Update `install.sh:140` to:

```sh
		echo "next: sudo apt install piperd piper (managed service — see README) or docs/manual-setup.md"
```

and `packaging/install/install_test.go:184`'s assertion to match the new text (keep the :428 no-systemctl guardrail untouched and passing — the word `systemctl` must not appear in install.sh).

- [ ] **Step 4: Run to verify pass** — `go test ./packaging/...` (doc test + install harness) → PASS.

- [ ] **Step 5: `make verify`, commit**

```bash
git add README.md docs/getting-started.md docs/manual-setup.md docs/runbooks/git-deploy-e2e.md install.sh packaging/install/install_test.go packaging/systemd/piperd_test.go
git commit -m "docs: one-tier install story — drop daemonize/rootless from docs and installer hints

Part of #446

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 6: PROGRESS + PR

**Files:**
- Modify: `PROGRESS.md`

- [ ] **Step 1:** Add under the Homebrew line: `- ✅ one-tier lifecycle: agent up/down/status wrap systemctl (Linux) / brew services (macOS); daemonize, rootless tier, and the CLI LaunchAgent deleted — [#446](https://github.com/piperbox/piper/issues/446)`

- [ ] **Step 2: `make verify`, commit, open the PR**

```bash
git add PROGRESS.md
git commit -m "docs: track one-tier lifecycle in PROGRESS

Part of #446

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
git push -u origin HEAD
gh pr create --base main --title "one-tier lifecycle: delete daemonize/rootless/LaunchAgent; agent wraps systemctl + brew services" \
  --body "Deliverable 3 of the onboarding & packaging spec (docs/superpowers/specs/2026-07-30-onboarding-packaging-design.md): the CLI never materializes units or plists; up/down/status wrap the deb system unit (Linux) and brew services (macOS). Also pins the brew service's PIPER_DATA_DIR (plan-2 parked finding) and updates docs/installer text to the one-tier story. Breaking pre-1.x change, no migration by policy — docs carry a one-time cleanup note. Closes #446.

🤖 Generated with [Claude Code](https://claude.com/claude-code)"
```

- [ ] **Step 3 (post-merge, controller):** this deliverable is complete at merge (no release-riding); the formula data-dir pin simply lands with the next release. The user's own Mac runs the old LaunchAgent — offer to run its cleanup + brew migration after merge.
