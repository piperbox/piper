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
	// Next-step hint: lifecycle belongs to the CLI, not the installer.
	switch runtime.GOOS {
	case "linux":
		if !strings.Contains(out, "next: piper agent up") {
			t.Errorf("expected linux next-step hint, got:\n%s", out)
		}
	case "darwin":
		if !strings.Contains(out, "docs/manual-setup.md") {
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

// TestInstallerIsDumbBinaryPlacer guards the new contract: the installer never
// manages services or touches system config — that is `piper agent`'s job.
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

func TestInstallerIsDumbBinaryPlacer(t *testing.T) {
	b, err := os.ReadFile(scriptPath(t))
	if err != nil {
		t.Fatal(err)
	}
	script := string(b)
	if strings.Contains(script, "systemctl") {
		t.Error("install.sh mentions systemctl; the installer must not manage services")
	}
	if strings.Contains(script, "/etc/") {
		t.Error("install.sh references /etc/; the installer must not touch system config")
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	return filepath.Dir(scriptPath(t))
}

func TestInstallDocumentation(t *testing.T) {
	// The README is the lean quick-start entry point; install flags and env
	// overrides live in docs/getting-started.md (see #181).
	docs := map[string][]string{
		"README.md": {
			"raw.githubusercontent.com/piperbox/piper/main/install.sh",
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
