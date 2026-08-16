package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/piperbox/piper/internal/api"
	"github.com/piperbox/piper/internal/config"
	"github.com/piperbox/piper/internal/store"
)

func TestRunCreateSupportsNameFirstFlags(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/apps" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			Name string `json:"name"`
			Port int    `json:"port"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if body.Name != "blog" || body.Port != 3000 {
			t.Errorf("body = %+v", body)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"create", "blog", "--port", "3000"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, `created app "blog" (port 3000)`) {
		t.Errorf("stdout = %q", got)
	}
}

func TestRunDeploySupportsNameFirstFlags(t *testing.T) {
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "Dockerfile"), []byte("FROM alpine\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/apps/blog/deploy":
			tr := tar.NewReader(r.Body)
			hdr, err := tr.Next()
			if err != nil && err != io.EOF {
				t.Errorf("Next: %v", err)
			} else if err == nil && hdr.Name != "Dockerfile" {
				t.Errorf("tar entry = %q", hdr.Name)
			}
			_ = json.NewEncoder(w).Encode(store.Deployment{ID: "dep1", App: "blog", Status: "building"})
		case r.URL.Path == "/v1/apps/blog/deployments/dep1/logs":
			io.WriteString(w, "")
		case r.URL.Path == "/v1/apps/blog/deployments":
			_ = json.NewEncoder(w).Encode([]store.Deployment{{ID: "dep1", App: "blog", Status: "running"}})
		case r.URL.Path == "/v1/apps/blog":
			_ = json.NewEncoder(w).Encode(api.App{App: store.App{Name: "blog", Hostname: "blog.piper.localhost"}, Status: "running", Scheme: "http"})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"deploy", "blog", "--path", srcDir}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	want := "deployed blog: http://blog.piper.localhost (running)\n"
	if got := stdout.String(); got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

// Deploying an app that doesn't exist yet must point the user at the exact
// `piper create` command rather than a bare "404 unknown app". #139.
func TestDeployMissingAppSuggestsCreate(t *testing.T) {
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "Dockerfile"), []byte("FROM alpine\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/apps/ghost/deploy" {
			http.Error(w, "unknown app", http.StatusNotFound)
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"deploy", "ghost", "--path", srcDir}, &stdout, &stderr); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if got := stderr.String(); !strings.Contains(got, `piper create ghost`) {
		t.Errorf("stderr = %q, want a 'piper create ghost' hint", got)
	}
}

// A github-linked app deploys from its repo when --path isn't given: the CLI
// asks the agent to fetch and build repo@branch instead of tarring a local
// directory that may not even hold the source. #331.
func TestDeployLinkedAppWithoutPathDeploysFromRepo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/apps/web/deploy-from-repo":
			_ = json.NewEncoder(w).Encode(store.Deployment{ID: "dep1", App: "web", Status: "building"})
		case r.URL.Path == "/v1/apps/web/deployments/dep1/logs":
			io.WriteString(w, "")
		case r.URL.Path == "/v1/apps/web/deployments":
			_ = json.NewEncoder(w).Encode([]store.Deployment{{ID: "dep1", App: "web", Status: "running"}})
		case r.URL.Path == "/v1/apps/web":
			_ = json.NewEncoder(w).Encode(api.App{App: store.App{
				Name: "web", Hostname: "web.piper.localhost", Repo: "me/web", Branch: "main",
			}, Status: "running", Scheme: "http"})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"deploy", "web"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if got := stderr.String(); !strings.Contains(got, "me/web@main") {
		t.Errorf("stderr = %q, want the repo@branch being deployed", got)
	}
	if got := stdout.String(); !strings.Contains(got, "deployed web: http://web.piper.localhost (running)") {
		t.Errorf("stdout = %q", got)
	}
}

func TestDeployStreamsProgressAndReportsURL(t *testing.T) {
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "Dockerfile"), []byte("FROM alpine\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/apps/web/deploy":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(store.Deployment{ID: "dep1", App: "web", Status: "building"})
		case r.URL.Path == "/v1/apps/web/deployments/dep1/logs":
			io.WriteString(w, "pulling base image...\nbuilt ok\n")
		case r.URL.Path == "/v1/apps/web/deployments":
			json.NewEncoder(w).Encode([]store.Deployment{{ID: "dep1", App: "web", Status: "running"}})
		case r.URL.Path == "/v1/apps/web":
			json.NewEncoder(w).Encode(api.App{App: store.App{Name: "web", Hostname: "web.piper.localhost"}, Status: "running", Scheme: "http"})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	var stdout, stderr bytes.Buffer
	code := run([]string{"deploy", "web", "--path", srcDir}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "pulling base image") {
		t.Fatalf("progress not streamed to stderr: %q", stderr.String())
	}
	if !strings.Contains(stdout.String(), "deployed web: http://web.piper.localhost (running)") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestManifestFormHandlerServesHTML(t *testing.T) {
	// The manifest page starts with <form>; without an explicit Content-Type Go
	// sniffs it as text/plain and the browser shows source instead of submitting
	// the form to GitHub. Guard against that regression.
	rec := httptest.NewRecorder()
	manifestFormHandler(`<form id="f"></form>`).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
}

func TestRunList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/apps" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]store.App{{Name: "api", Port: 3000}, {Name: "blog", Port: 8080}})
	}))
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"list"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if got := stdout.String(); got != "api\tport=3000\nblog\tport=8080\n" {
		t.Errorf("stdout = %q", got)
	}
}

func TestRunAppLinkSendsRootDir(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/apps/blog/link" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	var stdout, stderr bytes.Buffer
	code := run([]string{"app", "link", "blog", "--repo", "alice/blog", "--root-dir", "apps/web"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(gotBody, `"root_dir":"apps/web"`) {
		t.Fatalf("body = %s, want root_dir", gotBody)
	}
}

func TestRunGithubSetupWithOrg(t *testing.T) {
	// Mock backend endpoints
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/github/manifest" {
			var body struct {
				RedirectURL string `json:"redirect_url"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			// Return a fake manifest and store redirect_url for callback trigger
			_ = json.NewEncoder(w).Encode(map[string]string{
				"manifest": `{"name":"testapp","url":"` + body.RedirectURL + `"}`,
			})
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/v1/github/exchange" {
			var body struct {
				Code string `json:"code"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Code != "testcode" {
				t.Errorf("exchange code = %q, want testcode", body.Code)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"slug": "piper-abc"})
			return
		}
		t.Errorf("unexpected backend request: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	// Save and restore openBrowserFn
	oldOpenBrowser := openBrowserFn
	defer func() { openBrowserFn = oldOpenBrowser }()

	// Mock openBrowserFn to assert the form action URL and simulate redirection
	openBrowserFn = func(url string) error {
		resp, err := http.Get(url)
		if err != nil {
			t.Errorf("Get form: %v", err)
			return err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Errorf("Read form body: %v", err)
			return err
		}

		// Ensure the form action targets the org settings apps endpoint
		expectedAction := `action="https://github.com/organizations/myorg/settings/apps/new"`
		if !strings.Contains(string(body), expectedAction) {
			t.Errorf("form HTML does not contain action targeting org: %s", string(body))
		}

		// Parse the redirect URL from the manifest body
		bodyStr := string(body)
		startIdx := strings.Index(bodyStr, `"url":"`)
		if startIdx == -1 {
			t.Errorf("form HTML missing redirect url: %s", bodyStr)
			return nil
		}
		endIdx := strings.Index(bodyStr[startIdx+7:], `"`)
		if endIdx == -1 {
			t.Errorf("form HTML malformed redirect url: %s", bodyStr)
			return nil
		}
		redirectURL := bodyStr[startIdx+7 : startIdx+7+endIdx]

		// Call the callback endpoint on the server to simulate GitHub redirecting back with ?code=testcode
		cbResp, err := http.Get(redirectURL + "?code=testcode")
		if err != nil {
			t.Errorf("callback GET failed: %v", err)
			return err
		}
		cbResp.Body.Close()
		return nil
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"github", "setup", "--org", "myorg"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "https://github.com/apps/piper-abc/installations/new") {
		t.Errorf("stdout missing install deep-link: %q", got)
	}
}

func TestRunGithubSetupDefault(t *testing.T) {
	// Mock backend endpoints
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/github/manifest" {
			var body struct {
				RedirectURL string `json:"redirect_url"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"manifest": `{"name":"testapp","url":"` + body.RedirectURL + `"}`,
			})
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/v1/github/exchange" {
			_ = json.NewEncoder(w).Encode(map[string]string{"slug": "piper-abc"})
			return
		}
	}))
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	oldOpenBrowser := openBrowserFn
	defer func() { openBrowserFn = oldOpenBrowser }()

	openBrowserFn = func(url string) error {
		resp, err := http.Get(url)
		if err != nil {
			t.Errorf("Get form: %v", err)
			return err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Errorf("Read form body: %v", err)
			return err
		}

		// Ensure the form action targets the personal settings apps endpoint by default
		expectedAction := `action="https://github.com/settings/apps/new"`
		if !strings.Contains(string(body), expectedAction) {
			t.Errorf("form HTML does not contain action targeting personal: %s", string(body))
		}

		bodyStr := string(body)
		startIdx := strings.Index(bodyStr, `"url":"`)
		if startIdx == -1 {
			t.Errorf("form HTML missing redirect url: %s", bodyStr)
			return nil
		}
		endIdx := strings.Index(bodyStr[startIdx+7:], `"`)
		if endIdx == -1 {
			t.Errorf("form HTML malformed redirect url: %s", bodyStr)
			return nil
		}
		redirectURL := bodyStr[startIdx+7 : startIdx+7+endIdx]

		cbResp, err := http.Get(redirectURL + "?code=testcode")
		if err != nil {
			t.Errorf("callback GET failed: %v", err)
			return err
		}
		cbResp.Body.Close()
		return nil
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"github", "setup"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "https://github.com/apps/piper-abc/installations/new") {
		t.Errorf("stdout missing install deep-link: %q", got)
	}
}

func TestRunStop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/apps/blog/stop" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"stop", "blog"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "stopped blog") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestRunStart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/apps/blog/start" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"start", "blog"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "started blog") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestRunDeleteWithYesSkipsPrompt(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/apps/blog" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"delete", "blog", "--yes"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if !called {
		t.Fatal("DELETE was not sent")
	}
	if !strings.Contains(stdout.String(), "deleted blog") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestRunDeletePromptConfirms(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	old := stdinReader
	stdinReader = strings.NewReader("y\n")
	t.Cleanup(func() { stdinReader = old })

	var stdout, stderr bytes.Buffer
	if code := run([]string{"delete", "blog"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `delete app "blog"`) {
		t.Errorf("stdout = %q, want the confirmation prompt", stdout.String())
	}
	if !strings.Contains(stdout.String(), "deleted blog") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestRunDeletePromptAborts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("declined delete must not reach the API")
	}))
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	old := stdinReader
	stdinReader = strings.NewReader("n\n")
	t.Cleanup(func() { stdinReader = old })

	var stdout, stderr bytes.Buffer
	if code := run([]string{"delete", "blog"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "aborted") {
		t.Errorf("stdout = %q, want aborted", stdout.String())
	}
}

// A deploy whose row never leaves "building" gives up at --timeout with a hint,
// rather than following forever. #161.
func TestDeployTimesOutWithHint(t *testing.T) {
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "Dockerfile"), []byte("FROM alpine\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/apps/web/deploy":
			_ = json.NewEncoder(w).Encode(store.Deployment{ID: "dep1", App: "web", Status: "building"})
		case r.URL.Path == "/v1/apps/web/deployments/dep1/logs":
			io.WriteString(w, "building...\n")
		case r.URL.Path == "/v1/apps/web/deployments":
			_ = json.NewEncoder(w).Encode([]store.Deployment{{ID: "dep1", App: "web", Status: "building"}})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"deploy", "web", "--path", srcDir, "--timeout", "40ms"}, &stdout, &stderr); code != 1 {
		t.Fatalf("code = %d, want 1 (stderr: %s)", code, stderr.String())
	}
	if got := stderr.String(); !strings.Contains(got, "gave up waiting") || !strings.Contains(got, "piper app web") {
		t.Errorf("stderr = %q, want a give-up hint naming `piper app web`", got)
	}
}

func TestBareInvocationNonTTYPrintsUsage(t *testing.T) {
	old := isTerminal
	isTerminal = func() bool { return false }
	defer func() { isTerminal = old }()

	var out, errb bytes.Buffer
	if code := run(nil, &out, &errb); code != 2 {
		t.Fatalf("want exit 2, got %d", code)
	}
	if !strings.Contains(errb.String(), "usage:") {
		t.Fatalf("want usage, got: %s", errb.String())
	}
}

func TestBareInvocationTTYLaunchesTUI(t *testing.T) {
	oldT, oldL := isTerminal, launchTUI
	isTerminal = func() bool { return true }
	var gotRemote string
	called := false
	launchTUI = func(remote string, stderr io.Writer) int {
		called, gotRemote = true, remote
		return 0
	}
	defer func() { isTerminal, launchTUI = oldT, oldL }()

	var out, errb bytes.Buffer
	if code := run(nil, &out, &errb); code != 0 {
		t.Fatalf("want exit 0, got %d (stderr: %s)", code, errb.String())
	}
	if !called || gotRemote != "" {
		t.Fatalf("want TUI launch with empty remote, called=%v remote=%q", called, gotRemote)
	}

	if code := run([]string{"--remote", "pi4.example.dev"}, &out, &errb); code != 0 {
		t.Fatalf("want exit 0, got %d", code)
	}
	if gotRemote != "pi4.example.dev" {
		t.Fatalf("remote not forwarded: %q", gotRemote)
	}
}

// TestRunGithubReset covers the way off BYO (#299): the box drops its own App
// and says what will serve webhooks after a restart, so the operator does not
// have to guess whether the relay's brokered App takes over.
func TestRunGithubReset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/github/app" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		io.WriteString(w, `{"provider":"brokered"}`)
	}))
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	old := stdinReader
	stdinReader = strings.NewReader("y\n")
	t.Cleanup(func() { stdinReader = old })

	var stdout, stderr bytes.Buffer
	if code := run([]string{"github", "reset"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	got := stdout.String()
	if !strings.Contains(got, "removed") {
		t.Errorf("stdout = %q, want confirmation that the App was removed", got)
	}
	if !strings.Contains(got, "brokered") {
		t.Errorf("stdout = %q, want the provider the box will use next", got)
	}
	if !strings.Contains(got, "restart") {
		t.Errorf("stdout = %q, want the restart instruction", got)
	}
}

func TestRunGithubResetAborts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("declined reset must not reach the API")
	}))
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	old := stdinReader
	stdinReader = strings.NewReader("n\n")
	t.Cleanup(func() { stdinReader = old })

	var stdout, stderr bytes.Buffer
	if code := run([]string{"github", "reset"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "aborted") {
		t.Errorf("stdout = %q, want aborted", stdout.String())
	}
}

func boxWithPersistedAgentIdentity(t *testing.T, box config.Box, baseDomain string) config.Box {
	t.Helper()
	box.BaseDomain = baseDomain
	return box
}

func TestDialBoxRelayOnlyUsesPersistedAgentIdentity(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		if r.URL.Path != "/agents/cloud.example/v1/apps" {
			t.Errorf("request path = %q", r.URL.Path)
		}
		io.WriteString(w, "[]")
	}))
	defer srv.Close()

	c, addr, remote, err := dialBox(boxWithPersistedAgentIdentity(t, config.Box{
		Name:              "living-room",
		RelayAPI:          srv.URL,
		AccountCredential: "cred-xyz",
	}, "cloud.example"))
	if err != nil {
		t.Fatalf("dialBox: %v", err)
	}
	if addr != "cloud.example" || !remote {
		t.Fatalf("relay box identity = addr %q remote %v, want cloud.example/true", addr, remote)
	}
	if _, err := c.ListApps(); err != nil {
		t.Fatalf("relay client request: %v", err)
	}
	if gotPath != "/agents/cloud.example/v1/apps" || gotAuth != "Bearer cred-xyz" {
		t.Fatalf("relay request = path %q auth %q", gotPath, gotAuth)
	}
}
