package install

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// scriptPath returns the repo-root install.sh, found by walking up to go.mod.
func scriptPath(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, "install.sh")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above test dir")
		}
		dir = parent
	}
}

// hostOSArch maps the running machine to goreleaser's os/arch tokens.
func hostOSArch() (string, string) {
	arch := runtime.GOARCH
	if arch == "arm" {
		arch = "armv7"
	}
	return runtime.GOOS, arch
}

// tarGz wraps a single named file (mode 0755) in a gzipped tar.
func tarGz(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// newReleaseServer serves assets under /{repo}/releases/download/{tag}/{file}
// and synthesises a checksums.txt from them. checksumsOverride, when non-nil,
// replaces the computed checksums.txt body (to simulate corruption).
func newReleaseServer(t *testing.T, assets map[string][]byte, checksumsOverride []byte) *httptest.Server {
	t.Helper()
	var sums strings.Builder
	for name, body := range assets {
		sum := sha256.Sum256(body)
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(sum[:]), name)
	}
	body := []byte(sums.String())
	if checksumsOverride != nil {
		body = checksumsOverride
	}
	assets["checksums.txt"] = body
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := filepath.Base(r.URL.Path)
		if b, ok := assets[base]; ok {
			w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// bothArchives returns fake piper + piperd archives for the host os/arch.
func bothArchives(t *testing.T, tag string) map[string][]byte {
	t.Helper()
	osTok, archTok := hostOSArch()
	ver := strings.TrimPrefix(tag, "v")
	return map[string][]byte{
		fmt.Sprintf("piper_%s_%s_%s.tar.gz", ver, osTok, archTok):  tarGz(t, "piper", "fake-piper"),
		fmt.Sprintf("piperd_%s_%s_%s.tar.gz", ver, osTok, archTok): tarGz(t, "piperd", "fake-piperd"),
	}
}

// run executes install.sh with the given args and env overlay.
func run(t *testing.T, args []string, env map[string]string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar not available")
	}
	cmd := exec.Command("sh", append([]string{scriptPath(t)}, args...)...)
	merged := map[string]string{}
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			merged[kv[:i]] = kv[i+1:]
		}
	}
	for k, v := range env {
		merged[k] = v
	}
	cmd.Env = nil
	for k, v := range merged {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestCLIOnlyInstallsOnlyPiper(t *testing.T) {
	tag := "v9.9.9"
	srv := newReleaseServer(t, bothArchives(t, tag), nil)

	prefix := t.TempDir()
	out, err := run(t, []string{"--cli-only", "--version", tag}, map[string]string{
		"PIPER_REPO":     "piperbox/piper",
		"PIPER_BASE_URL": srv.URL,
		"PIPER_PREFIX":   prefix,
	})
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	info, err := os.Stat(filepath.Join(prefix, "piper"))
	if err != nil {
		t.Fatalf("piper not installed: %v\n%s", err, out)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("piper not executable: %v", info.Mode())
	}
	if _, err := os.Stat(filepath.Join(prefix, "piperd")); err == nil {
		t.Errorf("--cli-only installed piperd:\n%s", out)
	}
	// A fresh temp prefix is never on PATH; the script must say so.
	if !strings.Contains(out, "not on your PATH") {
		t.Errorf("expected PATH note, got:\n%s", out)
	}
}

func TestDefaultInstallsBothBinaries(t *testing.T) {
	tag := "v9.9.9"
	srv := newReleaseServer(t, bothArchives(t, tag), nil)

	prefix := t.TempDir()
	out, err := run(t, nil, map[string]string{
		"PIPER_BASE_URL": srv.URL,
		"PIPER_VERSION":  tag,
		"PIPER_PREFIX":   prefix,
	})
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	for _, name := range []string{"piper", "piperd"} {
		if _, err := os.Stat(filepath.Join(prefix, name)); err != nil {
			t.Errorf("%s not installed: %v\n%s", name, err, out)
		}
	}
	// Next-step hint: lifecycle belongs to the CLI, not the installer. This
	// pins a version, so dispatch always takes the diet path.
	switch runtime.GOOS {
	case "linux":
		if !strings.Contains(out, "systemctl enable --now piperd") {
			t.Errorf("expected linux next-step hint, got:\n%s", out)
		}
	case "darwin":
		if !strings.Contains(out, "brew install piperbox/tap/piper") {
			t.Errorf("expected darwin next-step hint, got:\n%s", out)
		}
	}
}

// A copy earlier on PATH silently wins over the one just installed. On a real
// Pi this made three consecutive upgrades look like they had not taken: the
// installer reported success writing 0.8.7 to /usr/local/bin while the shell
// kept running an 0.8.6 from ~/.local/bin. Reporting "installed" without
// mentioning the shadow is the installer telling a half-truth.
func TestWarnsWhenAnotherCopyShadowsThePrefix(t *testing.T) {
	tag := "v9.9.9"
	srv := newReleaseServer(t, bothArchives(t, tag), nil)

	// A decoy piper earlier on PATH than the install prefix.
	shadowDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(shadowDir, "piper"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	prefix := t.TempDir()
	out, err := run(t, nil, map[string]string{
		"PIPER_BASE_URL": srv.URL,
		"PIPER_VERSION":  tag,
		"PIPER_PREFIX":   prefix,
		"PATH":           shadowDir + ":" + prefix + ":" + os.Getenv("PATH"),
	})
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, filepath.Join(shadowDir, "piper")) {
		t.Errorf("installer must name the shadowing copy, got:\n%s", out)
	}
	if !strings.Contains(out, "shadow") {
		t.Errorf("installer must call it a shadow, got:\n%s", out)
	}
}

// The warning must not fire when the installed copy is the one that resolves —
// a caveat printed on every clean install is one people stop reading.
func TestNoShadowWarningWhenPrefixWins(t *testing.T) {
	tag := "v9.9.9"
	srv := newReleaseServer(t, bothArchives(t, tag), nil)

	prefix := t.TempDir()
	out, err := run(t, nil, map[string]string{
		"PIPER_BASE_URL": srv.URL,
		"PIPER_VERSION":  tag,
		"PIPER_PREFIX":   prefix,
		"PATH":           prefix + ":" + os.Getenv("PATH"),
	})
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	if strings.Contains(out, "shadow") {
		t.Errorf("no shadow exists; warning must stay quiet:\n%s", out)
	}
}

func TestDefaultPrefixIsHomeLocalBin(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("non-root default prefix requires a non-root user")
	}
	tag := "v9.9.9"
	srv := newReleaseServer(t, bothArchives(t, tag), nil)

	home := t.TempDir()
	out, err := run(t, []string{"--cli-only"}, map[string]string{
		"PIPER_BASE_URL": srv.URL,
		"PIPER_VERSION":  tag,
		"PIPER_PREFIX":   "", // unset: fall back to $HOME/.local/bin
		"HOME":           home,
	})
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "bin", "piper")); err != nil {
		t.Fatalf("piper not installed under $HOME/.local/bin: %v\n%s", err, out)
	}
}

func TestChecksumMismatchAborts(t *testing.T) {
	osTok, archTok := hostOSArch()
	tag := "v9.9.9"
	archive := fmt.Sprintf("piper_%s_%s_%s.tar.gz", strings.TrimPrefix(tag, "v"), osTok, archTok)
	assets := map[string][]byte{archive: tarGz(t, "piper", "real-bytes")}
	// checksums.txt claims a bogus hash for the archive.
	bogus := []byte(fmt.Sprintf("%064d  %s\n", 0, archive))
	srv := newReleaseServer(t, assets, bogus)

	prefix := t.TempDir()
	out, err := run(t, []string{"--cli-only"}, map[string]string{
		"PIPER_BASE_URL": srv.URL,
		"PIPER_VERSION":  tag,
		"PIPER_PREFIX":   prefix,
	})
	if err == nil {
		t.Fatalf("expected non-zero exit on checksum mismatch, got success:\n%s", out)
	}
	if _, statErr := os.Stat(filepath.Join(prefix, "piper")); statErr == nil {
		t.Error("piper was installed despite checksum mismatch")
	}
	if !strings.Contains(out, "checksum mismatch") {
		t.Errorf("expected checksum error message, got:\n%s", out)
	}
}

// newAPIServer serves GitHub-shaped release JSON at /repos/{repo}/releases and
// /repos/{repo}/releases/latest. latestTag may be "" to simulate no stable
// release (404 on /latest). allTags lists newest-first for the /releases list.
func newAPIServer(t *testing.T, latestTag string, allTags []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			if latestTag == "" {
				http.NotFound(w, r)
				return
			}
			fmt.Fprintf(w, `{"tag_name": %q}`, latestTag)
		case strings.HasSuffix(r.URL.Path, "/releases"):
			parts := make([]string, len(allTags))
			for i, tg := range allTags {
				parts[i] = fmt.Sprintf(`{"tag_name": %q, "prerelease": true}`, tg)
			}
			fmt.Fprintf(w, "[%s]", strings.Join(parts, ","))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestResolveLatestStable(t *testing.T) {
	tag := "v1.2.3"
	dl := newReleaseServer(t, bothArchives(t, tag), nil)
	api := newAPIServer(t, tag, []string{tag})

	prefix := t.TempDir()
	out, err := run(t, []string{"--cli-only"}, map[string]string{
		"PIPER_BASE_URL": dl.URL,
		"PIPER_API_URL":  api.URL,
		"PIPER_VERSION":  "", // unset: resolve from the API
		"PIPER_PREFIX":   prefix,
	})
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(prefix, "piper")); err != nil {
		t.Fatalf("piper (from %s) not installed: %v\n%s", tag, err, out)
	}
}

func TestResolveRCPicksNewestPrerelease(t *testing.T) {
	tag := "v0.2.0-rc.1"
	dl := newReleaseServer(t, bothArchives(t, tag), nil)
	api := newAPIServer(t, "", []string{tag, "v0.1.0-rc.1"})

	prefix := t.TempDir()
	out, err := run(t, []string{"--cli-only", "--rc"}, map[string]string{
		"PIPER_BASE_URL": dl.URL,
		"PIPER_API_URL":  api.URL,
		"PIPER_VERSION":  "",
		"PIPER_PREFIX":   prefix,
	})
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(prefix, "piper")); err != nil {
		t.Fatalf("piper (from %s) not installed: %v\n%s", tag, err, out)
	}
}

func TestDefaultNoStableReleaseErrors(t *testing.T) {
	api := newAPIServer(t, "", []string{"v0.1.0-rc.1"}) // no stable
	out, err := run(t, []string{"--cli-only"}, map[string]string{
		"PIPER_BASE_URL": "http://127.0.0.1:0",
		"PIPER_API_URL":  api.URL,
		"PIPER_VERSION":  "",
		"PIPER_PREFIX":   t.TempDir(),
	})
	if err == nil {
		t.Fatalf("expected error when no stable release exists:\n%s", out)
	}
	if !strings.Contains(out, "--rc") {
		t.Errorf("expected message pointing to --rc, got:\n%s", out)
	}
}

// TestDownloadAnnouncesEachBinary: a default install pulls ~36 MB of archives.
// The bar itself is TTY-gated (curl and wget render a meter only when stderr is
// a terminal), so the announcement is the part a piped or CI run still sees —
// and the part that says which binary the wait belongs to.
func TestDownloadAnnouncesEachBinary(t *testing.T) {
	tag := "v9.9.9"
	srv := newReleaseServer(t, bothArchives(t, tag), nil)

	out, err := run(t, nil, map[string]string{
		"PIPER_BASE_URL": srv.URL,
		"PIPER_VERSION":  tag,
		"PIPER_PREFIX":   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	for _, want := range []string{"downloading piperd " + tag, "downloading piper " + tag} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// TestArchiveDownloadShowsProgress pins the meter to the archive fetch only: the
// release-metadata fetch is captured in a command substitution and the checksum
// file is a few hundred bytes, so neither should render one.
func TestArchiveDownloadShowsProgress(t *testing.T) {
	b, err := os.ReadFile(scriptPath(t))
	if err != nil {
		t.Fatal(err)
	}
	script := string(b)
	for _, want := range []string{"--progress-bar", "--show-progress", "-t 2"} {
		if !strings.Contains(script, want) {
			t.Errorf("install.sh missing %q — archive downloads should show progress on a terminal", want)
		}
	}
	if !strings.Contains(script, "curl -fsSL") {
		t.Error("install.sh no longer has a silent fetch; metadata and checksums must stay quiet")
	}
}

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

func repoRoot(t *testing.T) string {
	t.Helper()
	return filepath.Dir(scriptPath(t))
}

func TestInstallDocumentation(t *testing.T) {
	// The README is the lean quick-start entry point (apt on Linux, brew on
	// macOS); the curl installer and its flags live in docs/getting-started.md
	// (see #181, #446).
	docs := map[string][]string{
		"README.md": {
			"sudo apt install piperd piper",
			"brew install piperbox/tap/piper",
		},
		filepath.Join("docs", "getting-started.md"): {
			"--cli-only",
			"--rc",
			"PIPER_ADDR",
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
