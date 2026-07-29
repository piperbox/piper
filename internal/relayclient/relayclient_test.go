package relayclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLoginDevice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/login/device" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"user_code": "ABCD-EFGH", "verification_uri": "https://relay.test/device",
			"device_code": "dev-1", "interval": 5, "expires_in": 300,
		})
	}))
	defer srv.Close()

	da, err := New(srv.URL).LoginDevice(context.Background())
	if err != nil {
		t.Fatalf("LoginDevice: %v", err)
	}
	if da.UserCode != "ABCD-EFGH" || da.VerificationURI != "https://relay.test/device" ||
		da.DeviceCode != "dev-1" || da.Interval != 5 || da.ExpiresIn != 300 {
		t.Fatalf("device auth = %+v", da)
	}
}

func TestLoginPollPendingThenSuccess(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			DeviceCode string `json:"device_code"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.DeviceCode != "dev-1" {
			t.Errorf("device_code = %q", body.DeviceCode)
		}
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "authorization_pending"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"account_credential": "cred-xyz", "username": "alice",
		})
	}))
	defer srv.Close()
	c := New(srv.URL)

	if _, err := c.LoginPoll(context.Background(), "dev-1"); err != ErrAuthPending {
		t.Fatalf("first poll err = %v, want ErrAuthPending", err)
	}
	acc, err := c.LoginPoll(context.Background(), "dev-1")
	if err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if acc.AccountCredential != "cred-xyz" || acc.Username != "alice" {
		t.Fatalf("account = %+v", acc)
	}
}

func TestCLILoginStartAndPoll(t *testing.T) {
	var polls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/login/cli/start":
			_ = json.NewEncoder(w).Encode(map[string]string{"handle": "h-1", "user_code": "ABCD-1234"})
		case "/v1/login/cli/poll":
			var body struct {
				Handle string `json:"handle"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Handle != "h-1" {
				t.Errorf("handle = %q", body.Handle)
			}
			polls++
			if polls == 1 {
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "authorization_pending"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{
				"account_credential": "cred-xyz", "username": "alice",
				"install_url": "https://github.com/apps/piper/installations/new",
			})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	c := New(srv.URL)

	handle, code, err := c.CLILoginStart(context.Background())
	if err != nil || handle != "h-1" || code != "ABCD-1234" {
		t.Fatalf("start = (%q,%q,%v)", handle, code, err)
	}
	if _, err := c.CLILoginPoll(context.Background(), "h-1"); err != ErrAuthPending {
		t.Fatalf("first poll err = %v, want ErrAuthPending", err)
	}
	acc, err := c.CLILoginPoll(context.Background(), "h-1")
	if err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if acc.AccountCredential != "cred-xyz" || acc.Username != "alice" ||
		acc.InstallURL != "https://github.com/apps/piper/installations/new" {
		t.Fatalf("account = %+v", acc)
	}
}

func TestEnroll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer cred-xyz" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"enrollment_token": "enr-1", "base_domain": "ab12-alice.public.getpiper.co",
			"tunnel_endpoint": "relay.getpiper.co:7000",
		})
	}))
	defer srv.Close()

	en, err := New(srv.URL).Enroll(context.Background(), "cred-xyz")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if en.EnrollmentToken != "enr-1" || en.BaseDomain != "ab12-alice.public.getpiper.co" ||
		en.TunnelEndpoint != "relay.getpiper.co:7000" {
		t.Fatalf("enrollment = %+v", en)
	}
}

func TestEnrollErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		code int
		want error
	}{{http.StatusUnauthorized, ErrBadCredential}, {http.StatusTooManyRequests, ErrQuotaExceeded}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.code)
		}))
		_, err := New(srv.URL).Enroll(context.Background(), "whatever")
		srv.Close()
		if err != tc.want {
			t.Fatalf("code %d err = %v, want %v", tc.code, err, tc.want)
		}
	}
}

func TestGitHubRepos(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/github/repos" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("installation_id"); got != "66" {
			t.Errorf("installation_id = %q, want 66", got)
		}
		if r.Header.Get("Authorization") != "Bearer cred-xyz" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"repos":[{"full_name":"alice/blog","visibility":"public","pushed_at":"2026-07-20T12:34:56Z"}]}`))
	}))
	defer srv.Close()

	repos, err := New(srv.URL).GitHubRepos(context.Background(), "cred-xyz", "66")
	if err != nil {
		t.Fatalf("GitHubRepos: %v", err)
	}
	if len(repos) != 1 || repos[0].FullName != "alice/blog" {
		t.Fatalf("repos = %+v", repos)
	}
}

func TestGitHubStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/github/status" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer cred-xyz" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"github_app":true,"installations":[` +
			`{"installation_id":"66","target_type":"org","target_login":"getpiper"},` +
			`{"installation_id":"55","target_type":"user","target_login":"alice"}],"install_url":"x"}`))
	}))
	defer srv.Close()

	st, err := New(srv.URL).GitHubStatus(context.Background(), "cred-xyz")
	if err != nil {
		t.Fatalf("GitHubStatus: %v", err)
	}
	if !st.GitHubApp || st.InstallURL != "x" {
		t.Fatalf("status flags = %+v", st)
	}
	insts := st.Installations
	if len(insts) != 2 || insts[0].ID != "66" || insts[0].TargetLogin != "getpiper" || insts[1].ID != "55" {
		t.Fatalf("installations = %+v", insts)
	}
}

func TestGitHubStatusMapsUnauthorizedToBadCredential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	_, err := New(srv.URL).GitHubStatus(context.Background(), "stale-cred")
	if !errors.Is(err, ErrBadCredential) {
		t.Fatalf("want ErrBadCredential for a 401, got %v", err)
	}
}

func TestGitHubStatusNoApp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"github_app":false,"installations":[],"install_url":""}`))
	}))
	defer srv.Close()
	st, err := New(srv.URL).GitHubStatus(context.Background(), "cred-xyz")
	if err != nil {
		t.Fatalf("GitHubStatus: %v", err)
	}
	if st.GitHubApp || st.InstallURL != "" || len(st.Installations) != 0 {
		t.Fatalf("want empty no-app status, got %+v", st)
	}
}

func TestGitHubReposErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		code int
		want error
	}{{http.StatusNotFound, ErrNoInstallation}, {http.StatusInternalServerError, nil}, {http.StatusBadGateway, nil}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.code)
		}))
		_, err := New(srv.URL).GitHubRepos(context.Background(), "whatever", "66")
		srv.Close()
		if tc.want != nil {
			if !errors.Is(err, tc.want) {
				t.Fatalf("code %d err = %v, want %v", tc.code, err, tc.want)
			}
			continue
		}
		if err == nil || errors.Is(err, ErrNoInstallation) {
			t.Fatalf("code %d err = %v, want a non-nil, non-ErrNoInstallation error", tc.code, err)
		}
	}
}

// A cancelled context aborts an in-flight request promptly instead of waiting
// out the 30s client timeout — the cancellation seam #85/#95 asked for.
func TestRequestRespectsContextCancellation(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-block // never respond until the test unblocks teardown
	}))
	defer srv.Close()
	defer close(block)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New(srv.URL).LoginDevice(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("LoginDevice err = %v, want context.Canceled", err)
	}
}

func TestAgentsDecodesTheListing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agents" || r.Method != http.MethodGet {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer cred-1" {
			t.Errorf("auth = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"agents":[
			{"agent":"a1.example","name":"a1.example","owner":"alice","connected":true},
			{"agent":"a2.example","name":"a2.example","owner":"alice","connected":false}]}`)
	}))
	defer srv.Close()

	agents, err := New(srv.URL).Agents(context.Background(), "cred-1")
	if err != nil {
		t.Fatalf("Agents: %v", err)
	}
	if len(agents) != 2 {
		t.Fatalf("got %d agents, want 2", len(agents))
	}
	if agents[0].BaseDomain != "a1.example" || !agents[0].Connected {
		t.Errorf("agents[0] = %+v", agents[0])
	}
	if agents[1].Owner != "alice" || agents[1].Connected {
		t.Errorf("agents[1] = %+v", agents[1])
	}
}

func TestRemoveAgentStatusMapping(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		want   error
	}{
		{"removed", http.StatusNoContent, nil},
		{"connected", http.StatusConflict, ErrAgentConnected},
		{"unknown or foreign", http.StatusNotFound, ErrNoAgent},
		{"org member, not owner", http.StatusForbidden, ErrNotOwner},
		{"bad credential", http.StatusUnauthorized, ErrBadCredential},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodDelete || r.URL.Path != "/agents/box.example" {
					t.Errorf("got %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(c.status)
			}))
			defer srv.Close()

			err := New(srv.URL).RemoveAgent(context.Background(), "cred-1", "box.example")
			if c.want == nil {
				if err != nil {
					t.Fatalf("RemoveAgent = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("RemoveAgent = %v, want %v", err, c.want)
			}
		})
	}
}
