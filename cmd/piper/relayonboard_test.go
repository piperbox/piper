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

	// No real sleeps or browser during the poll loop.
	pollSleep = func(time.Duration) {}
	defer func() { pollSleep = time.Sleep }()
	openBrowserFn = func(string) error { return nil }
	defer func() { openBrowserFn = openBrowser }()

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
	if code := run([]string{"login", "--relay", srv.URL}, &out, &errb); code != 0 {
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

	pollSleep = func(time.Duration) {}
	defer func() { pollSleep = time.Sleep }()
	openBrowserFn = func(string) error { return nil }
	defer func() { openBrowserFn = openBrowser }()

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
	if code := run([]string{"login", "--relay", srv.URL}, &out, &errb); code != 0 {
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

	pollSleep = func(time.Duration) {}
	defer func() { pollSleep = time.Sleep }()
	openBrowserFn = func(string) error { return nil }
	defer func() { openBrowserFn = openBrowser }()

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
	if code := run([]string{"login", "--relay", srv.URL}, &out, &errb); code != 0 {
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
	openBrowserFn = func(string) error { return nil }
	defer func() { openBrowserFn = openBrowser }()

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

	pollSleep = func(time.Duration) {}
	defer func() { pollSleep = time.Sleep }()
	var opened string
	openBrowserFn = func(u string) error { opened = u; return nil }
	defer func() { openBrowserFn = openBrowser }()

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
	if code := run([]string{"login", "--web", "--relay", srv.URL}, &out, &errb); code != 0 {
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

func TestConnectRequiresLogin(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	var out, errb bytes.Buffer
	if code := run([]string{"connect", "--data-dir", t.TempDir()}, &out, &errb); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !bytes.Contains(errb.Bytes(), []byte("piper login")) {
		t.Fatalf("stderr = %q, want a `piper login` hint", errb.String())
	}
}

func TestConnectEnrollsAndWritesRelayFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/enroll" || r.Header.Get("Authorization") != "Bearer cred-xyz" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"enrollment_token": "enr-1", "base_domain": "ab12-alice.public.getpiper.co",
			"tunnel_endpoint": "relay.getpiper.co:7000",
			"webhook_secret":  "whsec-1", "github_app": true,
		})
	}))
	defer srv.Close()

	// Prior `piper login` state.
	if err := config.SaveClient(config.ClientConfig{
		Addr: "http://127.0.0.1:8088", RelayAPI: srv.URL, AccountCredential: "cred-xyz",
	}); err != nil {
		t.Fatal(err)
	}

	dataDir := t.TempDir()
	old := config.SystemEnvDir
	config.SystemEnvDir = filepath.Join(t.TempDir(), "absent") // force the non-systemd path
	defer func() { config.SystemEnvDir = old }()
	var out, errb bytes.Buffer
	if code := run([]string{"connect", "--data-dir", dataDir}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	rf, found, err := config.LoadRelayFile(dataDir)
	if err != nil || !found {
		t.Fatalf("relay file: found=%v err=%v", found, err)
	}
	want := config.RelayFile{RelayAddr: "relay.getpiper.co:7000", RelayToken: "enr-1", BaseDomain: "ab12-alice.public.getpiper.co", Terminated: true, WebhookSecret: "whsec-1", GitHubBrokered: true}
	if rf != want {
		t.Fatalf("relay file = %+v, want %+v", rf, want)
	}
	if !strings.Contains(out.String(), "restart piperd to connect: ") {
		t.Fatalf("stdout = %q, want a framed restart hint", out.String())
	}
	if !strings.HasSuffix(out.String(), "\n") {
		t.Fatalf("stdout = %q, want a trailing newline", out.String())
	}
}

func TestConnectWritesTerminated(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/enroll" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"enrollment_token": "enr-1", "base_domain": "aaaa-alice.public.getpiper.co",
			"tunnel_endpoint": "relay.getpiper.co:7000",
			"webhook_secret":  "whsec-1", "github_app": true,
		})
	}))
	defer srv.Close()
	if err := config.SaveClient(config.ClientConfig{
		Addr: "http://127.0.0.1:8088", RelayAPI: srv.URL, AccountCredential: "cred-xyz",
	}); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	old := config.SystemEnvDir
	config.SystemEnvDir = filepath.Join(t.TempDir(), "absent") // force the non-systemd path
	defer func() { config.SystemEnvDir = old }()
	var out, errb bytes.Buffer
	if code := run([]string{"connect", "--data-dir", dataDir}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	rf, _, err := config.LoadRelayFile(dataDir)
	if err != nil || !rf.Terminated {
		t.Fatalf("relay file terminated = %v (err %v)", rf.Terminated, err)
	}
}

func TestConnectQuotaExceeded(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	if err := config.SaveClient(config.ClientConfig{
		Addr: "http://127.0.0.1:8088", RelayAPI: srv.URL, AccountCredential: "cred-xyz",
	}); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := run([]string{"connect", "--data-dir", t.TempDir()}, &out, &errb); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	// The message must name only remedies that exist. It used to say "remove an
	// existing box or upgrade" when neither was possible (#401).
	got := errb.String()
	for _, want := range []string{"quota", "piper box ls", "piper box rm"} {
		if !strings.Contains(got, want) {
			t.Errorf("stderr = %q, want it to mention %q", got, want)
		}
	}
	if strings.Contains(got, "upgrade") {
		t.Errorf("stderr = %q, must not offer an upgrade path that does not exist", got)
	}
}

func TestConnectOffBoxFailsLoudly(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")

	// The enroll endpoint must NOT be hit: an off-box run fails before burning
	// an account quota slot on an enrollment nothing would read (#173).
	var enrolls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		enrolls++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"enrollment_token": "enr-1", "base_domain": "ab12-alice.public.getpiper.co",
			"tunnel_endpoint": "relay.getpiper.co:7000",
			"webhook_secret":  "whsec-1", "github_app": true,
		})
	}))
	defer srv.Close()
	if err := config.SaveClient(config.ClientConfig{
		Addr: "http://127.0.0.1:8088", RelayAPI: srv.URL, AccountCredential: "cred-xyz",
	}); err != nil {
		t.Fatal(err)
	}

	// No piperd install of any flavor: no /etc/piper, no resolvable piperd
	// binary, and no existing data dir.
	old := config.SystemEnvDir
	config.SystemEnvDir = filepath.Join(t.TempDir(), "absent")
	defer func() { config.SystemEnvDir = old }()
	origPath := piperdPath
	piperdPath = func() (string, error) { return "", errors.New("not found") }
	defer func() { piperdPath = origPath }()
	dataDir := filepath.Join(t.TempDir(), "absent")

	var out, errb bytes.Buffer
	if code := run([]string{"connect", "--data-dir", dataDir}, &out, &errb); code != 1 {
		t.Fatalf("code = %d, want 1 (stdout = %s)", code, out.String())
	}
	if !bytes.Contains(errb.Bytes(), []byte("on the box")) {
		t.Fatalf("stderr = %q, want a must-run-on-the-box message", errb.String())
	}
	if enrolls != 0 {
		t.Fatalf("enroll endpoint hit %d times off-box; want 0 (fail before burning quota)", enrolls)
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatalf("data dir created off-box: stat err = %v", err)
	}
}

// restartHint names the one restart command per platform: brew's service on
// macOS, the system unit elsewhere.
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

// agentInstalled must recognize every surviving signal of an install: an
// existing data dir (an enrolled box) or a resolvable piperd binary
// (deb/brew/manual). The all-absent case is also pinned end-to-end by
// TestConnectOffBoxFailsLoudly.
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

func TestConnectSystemManagedGuidesEnvInstall(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/enroll" || r.Header.Get("Authorization") != "Bearer cred-xyz" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"enrollment_token": "enr-1", "base_domain": "ab12-alice.public.getpiper.co",
			"tunnel_endpoint": "relay.getpiper.co:7000",
			"webhook_secret":  "whsec-1", "github_app": true,
		})
	}))
	defer srv.Close()

	if err := config.SaveClient(config.ClientConfig{
		Addr: "http://127.0.0.1:8088", RelayAPI: srv.URL, AccountCredential: "cred-xyz",
	}); err != nil {
		t.Fatal(err)
	}

	// A present /etc/piper marks a systemd-managed box.
	dataDir := t.TempDir()
	old := config.SystemEnvDir
	config.SystemEnvDir = t.TempDir()
	defer func() { config.SystemEnvDir = old }()

	var out, errb bytes.Buffer
	if code := run([]string{"connect", "--data-dir", dataDir}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	// It must guide the env-file install, not write relay.json.
	if _, found, _ := config.LoadRelayFile(dataDir); found {
		t.Fatalf("relay.json written on a systemd-managed box; expected a guided env-file install")
	}
	for _, want := range []string{
		// The sudo upsert must be framed unmistakably as the action to take (#173).
		"Next step:",
		"sudo sh -c",
		"piperd.env",
		"PIPER_RELAY_ADDR=relay.getpiper.co:7000",
		"PIPER_RELAY_TOKEN=enr-1",
		"PIPER_BASE_DOMAIN=ab12-alice.public.getpiper.co",
		"PIPER_RELAY_TERMINATED=1",
		"PIPER_WEBHOOK_SECRET=whsec-1",
		"PIPER_GITHUB_BROKERED=1",
	} {
		if !bytes.Contains(out.Bytes(), []byte(want)) {
			t.Fatalf("stdout missing %q; got:\n%s", want, out.String())
		}
	}
}
