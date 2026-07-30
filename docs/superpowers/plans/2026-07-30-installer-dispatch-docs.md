# Installer Platform Dispatch + Docs Overhaul Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `curl -fsSL …/install.sh | sh` becomes the universal front door — configuring the apt repo on Debian-family, handing off to Homebrew on macOS, and falling back to today's diet binary placement (with printed service commands) everywhere else — with the docs restructured around the channels and the release skill gaining the brew smoke.

**Architecture:** install.sh gains a dispatch block ahead of the existing diet flow: Debian-family detection (`/etc/os-release` + apt-get) triggers keyring + deb822 sources + `apt-get update` (with rollback on failure) + `apt-get install piperd piper`, all through a `$sudo` prefix so pipe-mode works; macOS-with-brew triggers `brew install piperbox/tap/piper` + `brew services start piper`. `--cli-only`/`--rc`/`--version` always force diet (apt/tap carry stable only). Test seams: `PIPER_APT_URL` and `PIPER_OS_RELEASE` env overrides plus PATH-faked `uname`/`sudo`/`apt-get`/`brew` binaries in the existing Go harness. The old "dumb binary placer" guardrail is deliberately replaced (spec supersedes #345): the installer may configure apt and may *print* systemctl commands, but must never *run* a service manager.

**Tech Stack:** POSIX sh, the existing `packaging/install` Go test harness (httptest fake GitHub + env overlay), Markdown docs.

This is **plan 4 of 4** — the final deliverable (issue #447) of [the onboarding & packaging spec](../specs/2026-07-30-onboarding-packaging-design.md). Plans 1–3 are live (apt since v0.13.0, brew since v0.14.0, one-tier CLI merged).

## Global Constraints

- POSIX sh, `#!/bin/sh`, `set -eu`, tabs — match the existing script style.
- Dispatch order (exact): diet-forcing flags first (`--cli-only`, `--rc`, `--version`/`PIPER_VERSION` set); then darwin+brew → brew path; then Debian-family+apt-get → apt path; else diet. The apt/brew paths never hit the GitHub API (no version resolution needed).
- apt repo config: keyring at `/etc/apt/keyrings/piperbox.gpg` (0644), sources at `/etc/apt/sources.list.d/piperbox.sources`, fetched from `PIPER_APT_URL` (default `https://apt.piperbox.dev`). A failed `apt-get update` removes both files and dies — never half-configured (spec's error-handling requirement).
- Debian-family detection: `PIPER_OS_RELEASE` (default `/etc/os-release`) has an `ID=`/`ID_LIKE=` line containing `debian`, `ubuntu`, or `raspbian`, AND `apt-get` is on PATH, AND the detected OS is linux.
- Privilege: `$sudo` prefix per privileged command (`""` when root, `sudo` otherwise; die with a clear message if neither) — pipe-safe, Tailscale-style. Never re-exec the script.
- Guardrails (new): the script never *executes* `systemctl` or `launchctl` (they may appear only in `echo` lines); non-echo lines may reference only `/etc/apt/` and `/etc/os-release` under `/etc/`.
- Post-install shadow warnings: after apt, warn if `command -v piper|piperd` resolves outside `/usr/bin`; after brew, outside `$(brew --prefix)/bin`. The diet path keeps the existing `warn_shadow`.
- Diet path behavior stays byte-compatible for downloads/checksums/prefix/PATH-note; only the next-steps text changes.
- Run `make verify` before every commit. Conventional commits, `Part of #447`, trailer `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.

---

### Task 1: install.sh dispatch + rewritten guardrails + harness tests

**Files:**
- Modify: `install.sh`
- Test: `packaging/install/install_test.go`

**Interfaces:**
- Consumes: the live apt repo contract (`piperbox.gpg`, `piperbox.sources` at the repo root — plan 1) and tap formula name `piperbox/tap/piper` (plan 2).
- Produces: env seams `PIPER_APT_URL`, `PIPER_OS_RELEASE`; shell functions `deb_family`, `install_apt`, `install_brew`, `warn_stale_copies EXPECTED_DIR`. Task 2's docs and Task 3's release-skill smoke reference the printed strings verbatim.

- [ ] **Step 1: Write the failing tests.** In `packaging/install/install_test.go`:

**(a) Delete** `TestInstallerIsDumbBinaryPlacer` (:422-434) — its contract (`no systemctl string, no /etc/`) is deliberately superseded by the spec. **Replace** with:

```go
// TestInstallerNeverRunsServiceManagers is the successor to the old
// dumb-binary-placer guard, updated for the dispatch era (the spec supersedes
// #345): the installer may configure the apt repo and may PRINT service
// commands, but must never RUN a service manager — deb postinst and brew
// services own that. /etc/ references outside echo'd hints are limited to the
// apt repo config and the os-release probe.
func TestInstallerNeverRunsServiceManagers(t *testing.T) {
	b, err := os.ReadFile(scriptPath(t))
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimSpace(line)
		isEcho := strings.HasPrefix(trimmed, "echo")
		for _, mgr := range []string{"systemctl", "launchctl"} {
			if strings.Contains(line, mgr) && !isEcho {
				t.Errorf("line %d runs %s (only echo'd hints may mention it): %s", i+1, mgr, trimmed)
			}
		}
		if isEcho {
			continue
		}
		rest := line
		for {
			idx := strings.Index(rest, "/etc/")
			if idx < 0 {
				break
			}
			tail := rest[idx:]
			if !strings.HasPrefix(tail, "/etc/apt/") && !strings.HasPrefix(tail, "/etc/os-release") {
				t.Errorf("line %d references %q — only /etc/apt/ and /etc/os-release are allowed outside echo'd hints", i+1, tail[:min(20, len(tail))])
			}
			rest = tail[len("/etc/"):]
		}
	}
}
```

(Go 1.21+ has builtin `min`; the repo is on 1.26.)

**(b) Add the fake-binary helper + dispatch tests:**

```go
// fakeBin installs an executable stub named name in dir; each invocation
// appends "name <argv>" to logFile. body, when non-empty, replaces the default
// log-and-exit-0 behavior (it still receives the log path as $PIPER_FAKE_LOG).
func fakeBin(t *testing.T, dir, name, logFile, body string) {
	t.Helper()
	if body == "" {
		body = fmt.Sprintf("echo %q \"$@\" >> %q\nexit 0\n", name, logFile)
	}
	script := "#!/bin/sh\n" + body
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// fakeUname reports the given kernel/machine so dispatch tests run identically
// on any host OS.
func fakeUname(t *testing.T, dir, kernel, machine string) {
	t.Helper()
	body := fmt.Sprintf("case \"$1\" in -m) echo %q ;; *) echo %q ;; esac\n", machine, kernel)
	fakeBin(t, dir, "uname", "/dev/null", body)
}

// debianFixture writes an os-release identifying a Debian-family system.
func debianFixture(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "os-release")
	if err := os.WriteFile(p, []byte("ID=raspbian\nID_LIKE=debian\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// newAptServer serves the two repo-config files the apt path fetches.
func newAptServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case "piperbox.gpg":
			w.Write([]byte("fake-keyring"))
		case "piperbox.sources":
			w.Write([]byte("Types: deb\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAptPathConfiguresRepoAndInstalls(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("apt-path assertions expect the sudo-prefixed call log; as root the prefix is empty")
	}
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "calls.log")
	fakeUname(t, bin, "Linux", "x86_64")
	fakeBin(t, bin, "apt-get", log, "") // presence satisfies deb_family; sudo-prefixed calls go to the sudo fake
	fakeBin(t, bin, "sudo", log, "")    // logs, never executes — the test box's /etc stays untouched
	apt := newAptServer(t)

	out, err := run(t, nil, map[string]string{
		"PATH":             bin + ":" + os.Getenv("PATH"),
		"PIPER_OS_RELEASE": debianFixture(t),
		"PIPER_APT_URL":    apt.URL,
		"PIPER_VERSION":    "", // no pin: dispatch may choose apt
	})
	if err != nil {
		t.Fatalf("apt path failed: %v\n%s", err, out)
	}
	got, readErr := os.ReadFile(log)
	if readErr != nil {
		t.Fatalf("no calls logged: %v\n%s", readErr, out)
	}
	calls := string(got)
	for _, want := range []string{
		"sudo install -d -m 0755 /etc/apt/keyrings",
		"/etc/apt/keyrings/piperbox.gpg",
		"/etc/apt/sources.list.d/piperbox.sources",
		"sudo apt-get update",
		"sudo apt-get install -y piperd piper",
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("call log missing %q:\n%s", want, calls)
		}
	}
	if strings.Index(calls, "apt-get update") > strings.Index(calls, "apt-get install") {
		t.Error("apt-get install ran before apt-get update")
	}
	if !strings.Contains(out, "installed piper + piperd via apt") {
		t.Errorf("missing success line:\n%s", out)
	}
}

func TestAptPathRollsBackOnUpdateFailure(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("apt-path assertions expect the sudo-prefixed call log; as root the prefix is empty")
	}
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "calls.log")
	fakeUname(t, bin, "Linux", "x86_64")
	fakeBin(t, bin, "apt-get", log, "")
	// sudo fails exactly on `apt-get update`, logging everything.
	fakeBin(t, bin, "sudo", log, fmt.Sprintf(
		"echo sudo \"$@\" >> %q\ncase \"$*\" in *\"apt-get update\"*) exit 1 ;; esac\nexit 0\n", log))
	apt := newAptServer(t)

	out, err := run(t, nil, map[string]string{
		"PATH":             bin + ":" + os.Getenv("PATH"),
		"PIPER_OS_RELEASE": debianFixture(t),
		"PIPER_APT_URL":    apt.URL,
		"PIPER_VERSION":    "",
	})
	if err == nil {
		t.Fatalf("expected failure when apt-get update fails:\n%s", out)
	}
	calls, _ := os.ReadFile(log)
	if !strings.Contains(string(calls), "rm -f /etc/apt/keyrings/piperbox.gpg /etc/apt/sources.list.d/piperbox.sources") {
		t.Errorf("rollback rm not invoked:\n%s", calls)
	}
	if !strings.Contains(out, "rolled back") {
		t.Errorf("expected rollback message:\n%s", out)
	}
}

func TestBrewPathInstallsTapAndStartsService(t *testing.T) {
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "calls.log")
	fakeUname(t, bin, "Darwin", "arm64")
	fakeBin(t, bin, "brew", log, fmt.Sprintf(
		"echo brew \"$@\" >> %q\ncase \"$1\" in --prefix) echo /opt/fakebrew ;; esac\nexit 0\n", log))

	out, err := run(t, nil, map[string]string{
		"PATH":          bin + ":" + os.Getenv("PATH"),
		"PIPER_VERSION": "",
	})
	if err != nil {
		t.Fatalf("brew path failed: %v\n%s", err, out)
	}
	calls, _ := os.ReadFile(log)
	for _, want := range []string{"brew install piperbox/tap/piper", "brew services start piper"} {
		if !strings.Contains(string(calls), want) {
			t.Errorf("call log missing %q:\n%s", want, calls)
		}
	}
	if !strings.Contains(out, "installed piper + piperd via Homebrew") {
		t.Errorf("missing success line:\n%s", out)
	}
}

func TestVersionPinForcesDietOnDebian(t *testing.T) {
	tag := "v9.9.9"
	// Diet downloads use the FAKED os/arch (linux/amd64), not the host's.
	ver := strings.TrimPrefix(tag, "v")
	assets := map[string][]byte{
		"piper_" + ver + "_linux_amd64.tar.gz":  tarGz(t, "piper", "fake-piper"),
		"piperd_" + ver + "_linux_amd64.tar.gz": tarGz(t, "piperd", "fake-piperd"),
	}
	srv := newReleaseServer(t, assets, nil)
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "calls.log")
	fakeUname(t, bin, "Linux", "x86_64")
	fakeBin(t, bin, "apt-get", log, "")
	fakeBin(t, bin, "sudo", log, "")

	prefix := t.TempDir()
	out, err := run(t, nil, map[string]string{
		"PATH":             bin + ":" + os.Getenv("PATH"),
		"PIPER_OS_RELEASE": debianFixture(t),
		"PIPER_BASE_URL":   srv.URL,
		"PIPER_VERSION":    tag,
		"PIPER_PREFIX":     prefix,
	})
	if err != nil {
		t.Fatalf("diet install failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(prefix, "piperd")); err != nil {
		t.Fatalf("version pin must take the diet path: %v\n%s", err, out)
	}
	if calls, _ := os.ReadFile(log); strings.Contains(string(calls), "apt-get") {
		t.Errorf("version pin must never touch apt:\n%s", calls)
	}
}

func TestDietLinuxPrintsServiceCommands(t *testing.T) {
	tag := "v9.9.9"
	ver := strings.TrimPrefix(tag, "v")
	assets := map[string][]byte{
		"piper_" + ver + "_linux_amd64.tar.gz":  tarGz(t, "piper", "fake-piper"),
		"piperd_" + ver + "_linux_amd64.tar.gz": tarGz(t, "piperd", "fake-piperd"),
	}
	srv := newReleaseServer(t, assets, nil)
	bin := t.TempDir()
	fakeUname(t, bin, "Linux", "x86_64")

	out, err := run(t, nil, map[string]string{
		"PATH":             bin + ":" + os.Getenv("PATH"),
		"PIPER_OS_RELEASE": "/nonexistent", // not deb-family -> diet
		"PIPER_BASE_URL":   srv.URL,
		"PIPER_VERSION":    tag,
		"PIPER_PREFIX":     t.TempDir(),
	})
	if err != nil {
		t.Fatalf("diet install failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"piperd.service",
		"systemctl enable --now piperd",
		"docs/manual-setup.md",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("diet next-steps missing %q:\n%s", want, out)
		}
	}
}
```

**(c) Update** `TestDefaultInstallsBothBinaries` (:163-192): it pins a version (diet path); its next-step assertions become — linux: `systemctl enable --now piperd` present in output; darwin: `brew install piperbox/tap/piper` present (the brew-less suggestion). Keep the rest.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./packaging/install/ -count=1`
Expected: FAIL — the dispatch tests can't find the new behavior (apt/brew fakes never invoked, success lines absent); `TestInstallerNeverRunsServiceManagers` passes vacuously for now (current script has no systemctl).

- [ ] **Step 3: Implement the dispatch in install.sh.** After the existing `os="$(detect_os)"` line (move it up if needed so dispatch precedes version resolution), insert:

```sh
# --- platform dispatch -------------------------------------------------------
# Hand off to the native package channel when possible, so every install lands
# on a real upgrade channel: the apt repo on Debian-family, Homebrew on macOS.
# --cli-only/--rc/--version always take the diet path below (apt + the tap
# carry stable releases only). Spec: docs/superpowers/specs/2026-07-30-*.md

apt_repo_url="${PIPER_APT_URL:-https://apt.piperbox.dev}"
os_release="${PIPER_OS_RELEASE:-/etc/os-release}"

deb_family() {
	[ "$os" = linux ] || return 1
	[ -r "$os_release" ] || return 1
	grep -E '^(ID|ID_LIKE)=' "$os_release" | grep -Eq 'debian|ubuntu|raspbian' || return 1
	have apt-get
}

# warn_stale_copies EXPECTED_DIR — after a package install, an older diet copy
# earlier on PATH silently keeps running (the #375 shadow trap at install time).
warn_stale_copies() {
	expected="$1"
	for name in piper piperd; do
		found="$(command -v "$name" 2>/dev/null || true)"
		if [ -n "$found" ] && [ "$found" != "$expected/$name" ]; then
			echo "warning: $found shadows $expected/$name on your PATH — remove that older copy"
		fi
	done
}

install_apt() {
	sudo=""
	if [ "$(id -u)" -ne 0 ]; then
		have sudo || die "installing via apt needs root — re-run as root, or install sudo"
		sudo="sudo"
	fi
	echo "setting up the Piper apt repository ($apt_repo_url)…" >&2
	tmp="$(mktemp -d)"
	trap 'rm -rf "$tmp"' EXIT
	fetch "$apt_repo_url/piperbox.gpg" "$tmp/piperbox.gpg" || die "download failed: piperbox.gpg"
	fetch "$apt_repo_url/piperbox.sources" "$tmp/piperbox.sources" || die "download failed: piperbox.sources"
	$sudo install -d -m 0755 /etc/apt/keyrings || die "cannot create /etc/apt/keyrings"
	$sudo install -m 0644 "$tmp/piperbox.gpg" /etc/apt/keyrings/piperbox.gpg || die "cannot install the keyring"
	$sudo install -m 0644 "$tmp/piperbox.sources" /etc/apt/sources.list.d/piperbox.sources || die "cannot install the sources file"
	if ! $sudo apt-get update; then
		# Never leave a half-configured source breaking every later apt run.
		$sudo rm -f /etc/apt/keyrings/piperbox.gpg /etc/apt/sources.list.d/piperbox.sources
		die "apt-get update failed — repository configuration rolled back"
	fi
	$sudo apt-get install -y piperd piper || die "apt-get install failed"
	echo "installed piper + piperd via apt — the piperd service is enabled and running"
	warn_stale_copies /usr/bin
}

install_brew() {
	echo "installing via Homebrew…" >&2
	brew install piperbox/tap/piper || die "brew install failed"
	brew services start piper || die "brew services start failed"
	echo "installed piper + piperd via Homebrew — piperd is running (and starts at every login)"
	warn_stale_copies "$(brew --prefix)/bin"
}

if [ -z "$cli_only" ] && [ -z "$use_rc" ] && [ -z "$PIPER_VERSION" ]; then
	if [ "$os" = darwin ] && have brew; then
		install_brew
		exit 0
	fi
	if deb_family; then
		install_apt
		exit 0
	fi
fi
# --- diet path: verified binaries into a prefix ------------------------------
```

Then replace the current next-steps block (the `if [ -n "$cli_only" ]…else…` around the old line 140) so the full diet install prints, per platform:

```sh
	if [ "$os" = linux ]; then
		echo "next — run piperd as a durable service (details: docs/manual-setup.md):"
		[ "$prefix" = /usr/local/bin ] || echo "  sudo install -m 0755 \"$prefix/piperd\" /usr/local/bin/piperd"
		echo "  sudo curl -fsSL $PIPER_BASE_URL/$PIPER_REPO/releases/download/$tag/piperd.service -o /etc/systemd/system/piperd.service"
		echo "  sudo systemctl daemon-reload && sudo systemctl enable --now piperd"
	else
		echo "next: install Homebrew, then \`brew install piperbox/tap/piper && brew services start piper\` — or run \"$prefix/piperd\" directly (foreground)"
	fi
```

(Note every `/etc/` + `systemctl` mention above is inside an `echo` — the new guardrail enforces exactly this.)

- [ ] **Step 4: Run to verify pass**

Run: `go test ./packaging/install/ -count=1 -v`
Expected: PASS — all new dispatch tests, the rewritten guardrail, and every pre-existing diet test (they all pin versions or pass `--cli-only`/`--rc`, so they take the diet path untouched).

- [ ] **Step 5: `make verify`, commit**

```bash
git add install.sh packaging/install/install_test.go
git commit -m "feat: installer platform dispatch — apt bootstrap, brew handoff, diet fallback

curl|sh now lands every install on a real upgrade channel where one
exists; supersedes the #345 dumb-placer contract per the spec.

Part of #447

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 2: Docs overhaul — curl as the front door, channel-oriented install docs

**Files:**
- Modify: `README.md` (quick start), `docs/getting-started.md` (Install section), `docs/manual-setup.md` (non-deb service install), `packaging/install/install_test.go` (`TestInstallDocumentation`), `packaging/systemd/piperd_test.go` (`TestPiperdDocumentation` — only if assertions need widening)

**Interfaces:**
- Consumes: Task 1's printed strings (the diet next-steps commands must match manual-setup.md's commands verbatim).
- Produces: the final channel-oriented docs the spec's §6 describes.

- [ ] **Step 1: Update the doc-contract tests first.** `TestInstallDocumentation` becomes:

```go
func TestInstallDocumentation(t *testing.T) {
	// The README leads with the universal curl front door; the per-channel
	// detail (apt lines, brew, diet flags, source builds) lives in
	// docs/getting-started.md (#181, #447).
	docs := map[string][]string{
		"README.md": {
			"install.sh | sh",
			"sudo apt install piperd piper",
			"brew install piperbox/tap/piper",
		},
		filepath.Join("docs", "getting-started.md"): {
			"apt.piperbox.dev",
			"brew services start piper",
			"--cli-only",
			"--rc",
			"PIPER_ADDR",
		},
		filepath.Join("docs", "manual-setup.md"): {
			"systemctl enable --now piperd",
			"piperd.service",
		},
	}
	for name, wants := range docs {
		b, err := os.ReadFile(filepath.Join(repoRoot(t), name))
		if err != nil {
			t.Fatal(err)
		}
		content := string(b)
		for _, want := range wants {
			if !strings.Contains(content, want) {
				t.Errorf("%s missing %q", name, want)
			}
		}
	}
}
```

Run `go test ./packaging/install/ -run TestInstallDocumentation` → FAIL (README lacks the curl line).

- [ ] **Step 2: Rewrite the README quick start** to lead with the one-liner (this is the payoff of the whole epic — one command, any platform):

```markdown
## 60-second quick start

​```bash
curl -fsSL https://raw.githubusercontent.com/piperbox/piper/main/install.sh | sh
piper login                  # GitHub device-flow; stores your account credential
piper connect                # enrolls this box on the public relay
                             # (run the sudo command it prints)
sudo systemctl restart piperd
piper deploy blog --path .   # → https://<hash>-<you>.public.getpiper.dev
​```

The installer lands you on a real upgrade channel: on Debian/Ubuntu/Raspberry
Pi OS it configures [apt.piperbox.dev](https://apt.piperbox.dev) and runs
`sudo apt install piperd piper`; on macOS it hands off to Homebrew
(`brew install piperbox/tap/piper`). On macOS, `brew services restart piper`
replaces the `systemctl` line above. Everything else — and `--cli-only`
laptop installs — gets verified binaries plus printed next steps.
```

(Real fences in the actual file.) Keep the surrounding sections (TUI note, walkthrough pointer) intact.

- [ ] **Step 3: Restructure `docs/getting-started.md`'s Install section** into the channel-oriented final form (this is the plan-4 overhaul the interim plan-3 edit deferred): sub-sections **apt (Debian-family)** — the manual keyring/sources/install lines (same as the piperbox/apt README), noting the curl installer does exactly this; **Homebrew (macOS)** — install + `brew services start piper` + the sudo/boot-durable variant + `brew services restart piper` after upgrades; **Anywhere else (diet)** — the curl flags (`--cli-only`, `--rc`, `--version`), prefix/PATH behavior, pointer to manual-setup for the service; **From source** — pointer to manual-setup. Extend the "Upgrading from a pre-0.15 install" note with the system-tier cleanup the final plan-3 review flagged: old `/usr/local/bin/piper{,d}` copies and a hand-placed `/etc/systemd/system/piperd.service` shadow the deb's `/usr/bin` + `/usr/lib` versions after switching to apt — remove them (`sudo rm`) once on apt.

- [ ] **Step 4: Align `docs/manual-setup.md`** so its Linux service-install commands are verbatim the ones Task 1's diet path prints (fetch `piperd.service` from the release → `/etc/systemd/system/` → seed `/etc/piper/piperd.env` from `piperd.env.example` → `daemon-reload` → `enable --now piperd`), keeping its source-build and relay sections untouched.

- [ ] **Step 5: Run to verify pass** — `go test ./packaging/...` (both doc tests + full install harness) → PASS.

- [ ] **Step 6: `make verify`, commit**

```bash
git add README.md docs/getting-started.md docs/manual-setup.md packaging/install/install_test.go packaging/systemd/piperd_test.go
git commit -m "docs: channel-oriented install story — curl front door, per-channel depth

Part of #447

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 3: Release-skill smokes — brew + full-dispatch curl

**Files:**
- Modify: `.claude/skills/release/SKILL.md`

**Interfaces:**
- Consumes: the live channels; Task 1's dispatch behavior.
- Produces: the standing post-release checks the spec's Testing section assigns to plan 4.

- [ ] **Step 1: Extend the "Smoke-test the published installer" section.** After the existing CLI-only smoke, add a full-dispatch smoke (Debian container; exercises curl → apt-repo config → apt install end-to-end — the postinst service start no-ops in a container by design):

```sh
docker run --rm debian:stable sh -c 'apt-get update -qq && apt-get install -y -qq curl ca-certificates >/dev/null && curl -fsSL https://raw.githubusercontent.com/piperbox/piper/main/install.sh | sh && piper --version && piperd --version'
```

Expected: the installer configures apt.piperbox.dev, installs both debs, both versions print the latest stable. (Runs as root in the container, so no sudo is involved.)

- [ ] **Step 2: Extend the "Homebrew tap" section** with the local brew smoke (non-invasive — do not `brew services start` on a box already running piperd):

```sh
brew install piperbox/tap/piper && piper --version && piperd --version && brew uninstall piper
```

Expected: both versions print the tag just cut.

- [ ] **Step 3: Commit**

```bash
git add .claude/skills/release/SKILL.md
git commit -m "docs: release skill gains the brew + full-dispatch installer smokes

Part of #447

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 4: PROGRESS + PR, then live verification

**Files:**
- Modify: `PROGRESS.md`

- [ ] **Step 1:** Under the one-tier line add: `- ✅ installer dispatch: curl|sh configures apt (Debian-family) / hands off to brew (macOS) / diet fallback with printed service steps — [#447](https://github.com/piperbox/piper/issues/447)`

- [ ] **Step 2: `make verify`, commit, open the PR**

```bash
git add PROGRESS.md
git commit -m "docs: track installer dispatch in PROGRESS

Part of #447

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
git push -u origin HEAD
gh pr create --base main --title "installer platform dispatch + channel-oriented docs" \
  --body "Deliverable 4 of 4 of the onboarding & packaging spec (docs/superpowers/specs/2026-07-30-onboarding-packaging-design.md): curl|sh is the universal front door — apt bootstrap on Debian-family (with rollback on failed update), brew handoff on macOS, diet fallback with printed service commands elsewhere; docs restructured around the channels; release skill gains the brew and full-dispatch smokes. Closes #447 and completes the epic.

🤖 Generated with [Claude Code](https://claude.com/claude-code)"
```

- [ ] **Step 3 (post-merge, controller):** live smokes against the merged main — the Debian-container full-dispatch smoke from Task 3 Step 1 (installer + apt repo are live; no release needed since install.sh is fetched from main), and on this Mac a `--cli-only` curl smoke (the full brew path would `brew services start` against the already-running local piperd — skip by design). Then close out the epic in PROGRESS/memory.
