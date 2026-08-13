package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/piperbox/piper/internal/config"
	"github.com/piperbox/piper/internal/enrollapi"
	"github.com/piperbox/piper/internal/relayclient"
)

// The shipped default must point at the live hosted relay: a stale default
// (e.g. a domain that no longer resolves) silently breaks first-run onboarding
// for anyone who doesn't pass --relay.
func TestDefaultRelayAPIIsLiveHostedRelay(t *testing.T) {
	const want = "https://api.public.getpiper.dev"
	if relayclient.DefaultAPI != want {
		t.Fatalf("relayclient.DefaultAPI = %q, want %q", relayclient.DefaultAPI, want)
	}
}

func TestRelayLoginStoresCredential(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	dataDir := stubNoLocalPiperd(t)

	// No real sleeps or browser during the poll loop.
	pollSleep = func(time.Duration) {}
	defer func() { pollSleep = time.Sleep }()
	oldOpenBrowser := openBrowserFn
	openBrowserFn = func(string) error { return nil }
	t.Cleanup(func() { openBrowserFn = oldOpenBrowser })

	var polls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/login/device":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"user_code": "ABCD-EFGH", "verification_uri": "https://relay.test/device",
				"device_code": "dev-1", "interval": 1, "expires_in": 300,
			})
		case "/v1/login/poll":
			polls++
			if polls == 1 {
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "authorization_pending"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{
				"account_credential": "cred-xyz", "username": "alice",
			})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	if code := run([]string{"login", "--relay", srv.URL, "--data-dir", dataDir}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	cc, err := config.LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if cc.RelayAPI != srv.URL || cc.AccountCredential != "cred-xyz" {
		t.Fatalf("cc = %+v", cc)
	}
}

// A login succeeds the moment the credential is persisted; the install poll
// that follows is advisory. When that poll times out, `piper login` must still
// exit 0 — the credential is on disk and usable — and point the user at how to
// finish the install (#297). A non-zero exit here reported a failure that was
// not one and broke scripted use. The poll timeout is short but NON-zero, so
// the poll really runs: it queries the relay, enters the wait between polls,
// and the deadline cuts that wait short — nothing about the path is skipped.
func TestRelayLoginExitsZeroWhenInstallPollTimesOut(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	dataDir := stubNoLocalPiperd(t)

	pollSleep = func(time.Duration) {}
	defer func() { pollSleep = time.Sleep }()
	oldOpenBrowser := openBrowserFn
	openBrowserFn = func(string) error { return nil }
	t.Cleanup(func() { openBrowserFn = oldOpenBrowser })

	// The advisory install poll really runs and really waits; its deadline
	// (well under the 3s poll interval) expires mid-wait.
	oldTimeout := installPollTimeout
	installPollTimeout = 100 * time.Millisecond
	defer func() { installPollTimeout = oldTimeout }()

	var statusPolls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/login/device":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"user_code": "ABCD-EFGH", "verification_uri": "https://relay.test/device",
				"device_code": "dev-1", "interval": 1, "expires_in": 300,
			})
		case "/v1/login/poll":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"account_credential": "cred-xyz", "username": "alice",
				"install_url": "https://github.com/apps/piper/installations/new",
			})
		case "/v1/github/status":
			statusPolls++
			_ = json.NewEncoder(w).Encode(map[string]any{"installations": []map[string]any{}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	start := time.Now()
	var out, errb bytes.Buffer
	if code := run([]string{"login", "--relay", srv.URL, "--data-dir", dataDir}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, want 0 (a successful login must not fail on a timed-out install poll); stderr = %s", code, errb.String())
	}
	// The deadline cut the wait short: the poll did not sit out the full
	// installPollInterval, let alone the real 10-minute timeout.
	if elapsed := time.Since(start); elapsed >= installPollInterval {
		t.Fatalf("install poll took %v, want it cut short well under the %v poll interval", elapsed, installPollInterval)
	}
	if statusPolls == 0 {
		t.Fatal("the install poll never queried the relay; the test is not exercising the poll")
	}
	// The credential the login produced is persisted.
	cc, err := config.LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if cc.RelayAPI != srv.URL || cc.AccountCredential != "cred-xyz" {
		t.Fatalf("cc = %+v", cc)
	}
	// The user is told how to finish the install.
	if !strings.Contains(out.String(), "https://github.com/apps/piper/installations/new") ||
		!strings.Contains(out.String(), "piper github repos") {
		t.Fatalf("stdout did not point the user at the outstanding install: %q", out.String())
	}
}

// Ctrl-C during the advisory install poll must end the poll promptly — the
// interrupt-aware context the login flow created is threaded all the way into
// waitForInstall — and, the poll being advisory, the login must still exit 0
// with the credential persisted (#297, consequence 2).
func TestRelayLoginExitsZeroWhenInstallPollInterrupted(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	dataDir := stubNoLocalPiperd(t)

	pollSleep = func(time.Duration) {}
	defer func() { pollSleep = time.Sleep }()
	oldOpenBrowser := openBrowserFn
	openBrowserFn = func(string) error { return nil }
	t.Cleanup(func() { openBrowserFn = oldOpenBrowser })

	// The relay never records an installation; once the install poll starts,
	// deliver SIGINT to this process — the login flow's signal.NotifyContext
	// must turn it into a prompt context cancellation.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/login/device":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"user_code": "ABCD-EFGH", "verification_uri": "https://relay.test/device",
				"device_code": "dev-1", "interval": 1, "expires_in": 300,
			})
		case "/v1/login/poll":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"account_credential": "cred-xyz", "username": "alice",
				"install_url": "https://github.com/apps/piper/installations/new",
			})
		case "/v1/github/status":
			go func() {
				time.Sleep(20 * time.Millisecond)
				p, err := os.FindProcess(os.Getpid())
				if err == nil {
					_ = p.Signal(os.Interrupt)
				}
			}()
			_ = json.NewEncoder(w).Encode(map[string]any{"installations": []map[string]any{}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	start := time.Now()
	var out, errb bytes.Buffer
	if code := run([]string{"login", "--relay", srv.URL, "--data-dir", dataDir}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, want 0 (an interrupted advisory install poll must not fail the login); stderr = %s", code, errb.String())
	}
	// Without context propagation the interrupt would surface only after the
	// current 3s poll interval — or the 10-minute timeout — elapsed.
	if elapsed := time.Since(start); elapsed >= installPollInterval {
		t.Fatalf("interrupted install poll took %v, want a prompt return well under the %v poll interval", elapsed, installPollInterval)
	}
	cc, err := config.LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if cc.RelayAPI != srv.URL || cc.AccountCredential != "cred-xyz" {
		t.Fatalf("cc = %+v", cc)
	}
	if !strings.Contains(out.String(), "https://github.com/apps/piper/installations/new") ||
		!strings.Contains(out.String(), "piper github repos") {
		t.Fatalf("stdout did not point the user at the outstanding install: %q", out.String())
	}
}

// A genuine login failure — the relay never issues a credential — must keep its
// non-zero exit. The advisory-poll fix (#297) only softens the post-login poll;
// it must not swallow real failures.
func TestRelayLoginExitsNonZeroOnLoginFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")

	pollSleep = func(time.Duration) {}
	defer func() { pollSleep = time.Sleep }()
	oldOpenBrowser := openBrowserFn
	openBrowserFn = func(string) error { return nil }
	t.Cleanup(func() { openBrowserFn = oldOpenBrowser })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/login/device":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"user_code": "ABCD-EFGH", "verification_uri": "https://relay.test/device",
				"device_code": "dev-1", "interval": 1, "expires_in": 300,
			})
		case "/v1/login/poll":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	if code := run([]string{"login", "--relay", srv.URL}, &out, &errb); code != 1 {
		t.Fatalf("code = %d, want 1 (a real login failure must stay non-zero)", code)
	}
	// No credential should have been persisted.
	if cc, err := config.LoadClient(); err == nil && cc.AccountCredential != "" {
		t.Fatalf("a failed login persisted a credential: %+v", cc)
	}
}

func TestRelayLoginWebStoresCredentialAndWaitsForInstall(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	dataDir := stubNoLocalPiperd(t)

	pollSleep = func(time.Duration) {}
	defer func() { pollSleep = time.Sleep }()
	var opened string
	oldOpenBrowser := openBrowserFn
	openBrowserFn = func(u string) error { opened = u; return nil }
	t.Cleanup(func() { openBrowserFn = oldOpenBrowser })

	var polls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/login/cli/start":
			_ = json.NewEncoder(w).Encode(map[string]string{"handle": "h-1", "user_code": "ABCD-1234"})
		case "/v1/login/cli/poll":
			polls++
			if polls == 1 {
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "authorization_pending"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{
				"account_credential": "cred-web", "username": "alice",
				"install_url": "https://github.com/apps/piper/installations/new",
			})
		case "/v1/github/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"installations": []map[string]any{
				{"installation_id": "66", "target_type": "org", "target_login": "getpiper"},
			}})
		case "/v1/github/repos":
			_ = json.NewEncoder(w).Encode(map[string]any{"repos": []map[string]any{
				{"full_name": "alice/blog", "visibility": "public", "pushed_at": ""},
			}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	if code := run([]string{"login", "--web", "--relay", srv.URL, "--data-dir", dataDir}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	// The browser is pointed at the relay's code-entry page, and the user code
	// is shown in the terminal.
	if opened != srv.URL+"/v1/login/cli" {
		t.Fatalf("opened browser at %q, want the code-entry page", opened)
	}
	if !strings.Contains(out.String(), "ABCD-1234") {
		t.Fatalf("stdout did not show the user code: %q", out.String())
	}
	cc, err := config.LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if cc.RelayAPI != srv.URL || cc.AccountCredential != "cred-web" {
		t.Fatalf("cc = %+v", cc)
	}
}

// TestWaitForInstallPollsUntilInstalled cribs TestRelayLoginStoresCredential's
// httptest-stub-relay shape. The stub's /v1/github/status answers with an empty
// installations list twice, then one installation, pinning that waitForInstall
// keeps retrying while there is no installation and returns nil once one lands.
func TestWaitForInstallPollsUntilInstalled(t *testing.T) {
	// Shrink the wait between polls; the poll itself really runs.
	oldInterval := installPollInterval
	installPollInterval = time.Millisecond
	defer func() { installPollInterval = oldInterval }()

	var polls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/github/status":
			polls++
			if polls < 3 {
				_ = json.NewEncoder(w).Encode(map[string]any{"installations": []map[string]any{}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"installations": []map[string]any{
				{"installation_id": "66", "target_type": "org", "target_login": "getpiper"},
			}})
		case "/v1/github/repos":
			if got := r.URL.Query().Get("installation_id"); got != "66" {
				t.Errorf("installation_id = %q, want 66", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"repos": []map[string]any{
				{"full_name": "alice/blog", "visibility": "public", "pushed_at": ""},
				{"full_name": "alice/api", "visibility": "private", "pushed_at": ""},
			}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	rc := relayclient.New(srv.URL)
	if err := waitForInstall(context.Background(), rc, "cred-xyz", "https://github.com/apps/piper/installations/new"); err != nil {
		t.Fatalf("waitForInstall: %v", err)
	}
	if polls != 3 {
		t.Fatalf("polls = %d, want 3", polls)
	}
}

// A cancelled context must end waitForInstall promptly — mid-wait, not after
// the current poll interval or the 10-minute timeout — so Ctrl-C during the
// advisory install poll returns at once (#297, consequence 2). The real 3s
// installPollInterval stays in force: cancellation, not a shrunken wait, is
// what makes the return prompt.
func TestWaitForInstallRespectsCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/github/status" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"installations": []map[string]any{}})
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := waitForInstall(ctx, relayclient.New(srv.URL), "cred-xyz", "https://github.com/apps/piper/installations/new")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForInstall err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed >= installPollInterval {
		t.Fatalf("waitForInstall took %v after cancellation, want a prompt return well under the %v poll interval", elapsed, installPollInterval)
	}
}

func TestGitHubReposCommandListsRepos(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer cred-xyz" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/github/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"installations": []map[string]any{
				{"installation_id": "66", "target_type": "org", "target_login": "getpiper"},
			}})
		case "/v1/github/repos":
			if got := r.URL.Query().Get("installation_id"); got != "66" {
				t.Errorf("installation_id = %q, want 66", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"repos": []map[string]any{
				{"full_name": "alice/blog", "visibility": "public", "pushed_at": "2026-07-20T12:34:56Z"},
				{"full_name": "alice/api", "visibility": "private", "pushed_at": ""},
			}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	if err := config.SaveClient(config.ClientConfig{
		Addr: "http://127.0.0.1:8088", RelayAPI: srv.URL, AccountCredential: "cred-xyz",
	}); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := run([]string{"github", "repos"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	if got := out.String(); got != "alice/blog\nalice/api (private)\n" {
		t.Fatalf("stdout = %q", got)
	}
}

func TestGitHubReposCommandNotInstalledYet(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/github/status" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"installations": []map[string]any{}})
	}))
	defer srv.Close()
	if err := config.SaveClient(config.ClientConfig{
		Addr: "http://127.0.0.1:8088", RelayAPI: srv.URL, AccountCredential: "cred-xyz",
	}); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := run([]string{"github", "repos"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "run `piper login`") {
		t.Fatalf("stdout = %q, want the install hint", out.String())
	}
}

func TestGitHubReposCommandRequiresLogin(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")

	var out, errb bytes.Buffer
	if code := run([]string{"github", "repos"}, &out, &errb); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "piper login") {
		t.Fatalf("stderr = %q, want a `piper login` hint", errb.String())
	}
}

func TestConnectVerbIsGone(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"connect"}, &out, &errb); code == 0 {
		t.Fatalf("piper connect must no longer exist; out=%s", out.String())
	}
	if !strings.Contains(errb.String(), "piper login") {
		t.Fatalf("unknown-command output should point at piper login: %q", errb.String())
	}
}

// agentInstalled must recognize every surviving signal of an install: an
// existing data dir (an enrolled box) or a resolvable piperd binary
// (deb/brew/manual).
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

// After identity is saved, `piper login` runs the claim stage against the
// enrollment socket — logging in and claiming this box happen in one command
// (the merged-login design). The relay stub grants the login immediately; the
// fake piperd behind the enrollment socket then sees the claim, and the CLI's
// stdout narrates login → claim in order.
// saveCredential seeds the client config as a previous login would have.
func saveCredential(t *testing.T, relayAPI, cred string) {
	t.Helper()
	cc, err := config.LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	cc.RelayAPI, cc.AccountCredential = relayAPI, cred
	if err := config.SaveClient(cc); err != nil {
		t.Fatal(err)
	}
}

// `piper login` is the universal "make everything right" verb, so re-running it
// on a healthy box must be cheap. Identity durability skipped re-enrollment but
// not re-authentication: the second login, seconds after the first, still made
// the user fetch a code and go back to the browser before printing "already
// enrolled" (#473).
func TestRelayLoginReusesAValidSavedCredential(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	stubNoLocalPiperd(t)
	fastPoll(t)

	var deviceCalls int
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/github/status":
			if got := r.Header.Get("Authorization"); got != "Bearer cred-xyz" {
				t.Errorf("status Authorization = %q", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"github_app": true, "username": "erin",
				"install_url":   "https://github.com/apps/piper/installations/new",
				"installations": []map[string]string{{"installation_id": "1"}},
			})
		case "/v1/login/device":
			deviceCalls++
			http.Error(w, "should not start a device flow", http.StatusTeapot)
		case "/agents":
			_ = json.NewEncoder(w).Encode(map[string]any{"agents": []map[string]any{
				{"agent": "ab12-erin.public.getpiper.co", "connected": true}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer relay.Close()
	saveCredential(t, relay.URL, "cred-xyz")

	openedBrowser := false
	oldOpenBrowser := openBrowserFn
	openBrowserFn = func(string) error { openedBrowser = true; return nil }
	t.Cleanup(func() { openBrowserFn = oldOpenBrowser })

	f := &fakePiperd{}
	f.status.Store(enrollapi.Status{Enrolled: true, BaseDomain: "ab12-erin.public.getpiper.co", Tunnel: "connected"})
	f.enroll = func(enrollapi.EnrollRequest) (int, any) {
		t.Error("re-login must not re-claim an already-enrolled box")
		return http.StatusOK, enrollapi.EnrollResponse{}
	}
	dataDir := startFakeEnrollSocket(t, f.mux())

	var out, errb bytes.Buffer
	code := run([]string{"login", "--relay", relay.URL, "--data-dir", dataDir}, &out, &errb)
	if code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	if deviceCalls != 0 {
		t.Errorf("started %d device flow(s) with a valid credential in hand", deviceCalls)
	}
	if openedBrowser {
		t.Error("re-login opened a browser with a valid credential in hand")
	}
	for _, want := range []string{"logged in to relay as erin", "already enrolled as ab12-erin.public.getpiper.co"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout missing %q:\n%s", want, out.String())
		}
	}
	cf, err := config.LoadClientFile()
	if err != nil {
		t.Fatal(err)
	}
	if len(cf.Boxes) != 1 || cf.Boxes[0].BaseDomain != "ab12-erin.public.getpiper.co" {
		t.Fatalf("reusable login did not persist enrolled identity: %+v", cf)
	}
}

// A credential the relay no longer accepts must not strand the user: login
// falls through to the device flow exactly as it did before.
func TestRelayLoginFallsBackWhenSavedCredentialIsRejected(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	dataDir := stubNoLocalPiperd(t)
	fastPoll(t)
	pollSleep = func(time.Duration) {}
	oldOpenBrowser := openBrowserFn
	openBrowserFn = func(string) error { return nil }
	t.Cleanup(func() { openBrowserFn = oldOpenBrowser })

	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/github/status":
			http.Error(w, "bad credential", http.StatusUnauthorized)
		case "/v1/login/device":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"verification_uri": "https://github.com/login/device", "user_code": "ABCD-1234",
				"device_code": "dev-1", "interval": 0, "expires_in": 300})
		case "/v1/login/poll":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"account_credential": "cred-fresh", "username": "erin"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer relay.Close()
	saveCredential(t, relay.URL, "cred-stale")

	var out, errb bytes.Buffer
	if code := run([]string{"login", "--relay", relay.URL, "--data-dir", dataDir}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	cc, err := config.LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if cc.AccountCredential != "cred-fresh" {
		t.Fatalf("stale credential survived: %q", cc.AccountCredential)
	}
}

// --relogin is the escape hatch for switching GitHub accounts: it must reach the
// device flow even though the saved credential still works.
func TestRelayLoginReloginForcesTheDeviceFlow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	dataDir := stubNoLocalPiperd(t)
	fastPoll(t)
	pollSleep = func(time.Duration) {}
	oldOpenBrowser := openBrowserFn
	openBrowserFn = func(string) error { return nil }
	t.Cleanup(func() { openBrowserFn = oldOpenBrowser })

	var deviceCalls int
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/github/status":
			t.Error("--relogin must not consult the saved credential")
			http.Error(w, "unexpected", http.StatusTeapot)
		case "/v1/login/device":
			deviceCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"verification_uri": "https://github.com/login/device", "user_code": "ABCD-1234",
				"device_code": "dev-1", "interval": 0, "expires_in": 300})
		case "/v1/login/poll":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"account_credential": "cred-bob", "username": "bob"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer relay.Close()
	saveCredential(t, relay.URL, "cred-alice")

	var out, errb bytes.Buffer
	if code := run([]string{"login", "--relay", relay.URL, "--data-dir", dataDir, "--relogin"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	if deviceCalls != 1 {
		t.Errorf("device flows started = %d, want 1", deviceCalls)
	}
	if !strings.Contains(out.String(), "logged in to relay as bob") {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestRelayLoginRunsClaimStage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	stubNoLocalPiperd(t)
	fastPoll(t)
	// The device-flow stub answers "interval": 0, which relayLogin defaults to
	// 5s; cap the poll sleep so that real default doesn't turn into a real 5s
	// sleep in this test.
	pollSleep = func(d time.Duration) {
		if d > 20*time.Millisecond {
			d = 20 * time.Millisecond
		}
		time.Sleep(d)
	}

	// Device-flow relay stub that immediately grants the login.
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/login/device":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"verification_uri": "https://github.com/login/device", "user_code": "ABCD-1234",
				"device_code": "dev-1", "interval": 0, "expires_in": 300})
		case "/v1/login/poll":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"account_credential": "cred-xyz", "username": "erin"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer relay.Close()

	f := &fakePiperd{}
	f.status.Store(enrollapi.Status{Enrolled: false, Tunnel: "off"})
	f.enroll = func(req enrollapi.EnrollRequest) (int, any) {
		f.status.Store(enrollapi.Status{Enrolled: true, BaseDomain: "ab12-erin.public.getpiper.co", Tunnel: "connected"})
		return http.StatusOK, enrollapi.EnrollResponse{BaseDomain: "ab12-erin.public.getpiper.co", RelayAddr: "relay:7000"}
	}
	dataDir := startFakeEnrollSocket(t, f.mux())

	var out, errb bytes.Buffer
	code := run([]string{"login", "--relay", relay.URL, "--data-dir", dataDir}, &out, &errb)
	if code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	got := f.got.Load().(enrollapi.EnrollRequest)
	if got.RelayAPI != relay.URL || got.AccountCredential != "cred-xyz" {
		t.Fatalf("claim did not carry the fresh credential: %+v", got)
	}
	cf, err := config.LoadClientFile()
	if err != nil || len(cf.Boxes) != 1 {
		t.Fatalf("saved client config = %+v (%v)", cf, err)
	}
	identity, err := json.Marshal(cf.Boxes[0])
	if err != nil {
		t.Fatalf("marshal saved box: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(identity, &raw); err != nil {
		t.Fatalf("decode saved box: %v", err)
	}
	if got, _ := raw["base_domain"].(string); got != "ab12-erin.public.getpiper.co" {
		t.Fatalf("saved agent identity = %q, want ab12-erin.public.getpiper.co", got)
	}
	for _, want := range []string{"logged in to relay as erin", "claiming this box", "ab12-erin.public.getpiper.co"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("stdout missing %q:\n%s", want, out.String())
		}
	}
}
