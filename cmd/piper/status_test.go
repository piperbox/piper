package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/piperbox/piper/internal/api"
	"github.com/piperbox/piper/internal/config"
	"github.com/piperbox/piper/internal/store"
)

func TestRunStatusLocal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/version":
			_ = json.NewEncoder(w).Encode(map[string]string{"version": "0.8.5"})
		case "/v1/apps":
			_ = json.NewEncoder(w).Encode([]api.App{
				{App: store.App{Name: "api", Port: 3000}},
				{App: store.App{Name: "blog", Port: 8080, Hostname: "blog.piper.localhost"}, Status: "running"},
			})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	if err := config.SaveClient(config.ClientConfig{Addr: srv.URL, Token: "tok"}); err != nil {
		t.Fatalf("SaveClient: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"status"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	want := "agent: 0.8.5\napi\tstatus=-\tport=3000\nblog\tstatus=running\tport=8080\thttp://blog.piper.localhost\n"
	if got := stdout.String(); got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

// An agent from before the endpoint 404s. Saying so beats saying nothing:
// "too old to tell you" is itself the answer when someone is checking whether
// an upgrade took (#375). The apps still list — status must not fail over it.
func TestRunStatusOldAgentSaysUnknown(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/apps" {
			_ = json.NewEncoder(w).Encode([]api.App{{App: store.App{Name: "api", Port: 3000}}})
			return
		}
		http.Error(w, "404 page not found", http.StatusNotFound)
	}))
	defer srv.Close()
	if err := config.SaveClient(config.ClientConfig{Addr: srv.URL, Token: "tok"}); err != nil {
		t.Fatalf("SaveClient: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"status"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	got := stdout.String()
	if !strings.HasPrefix(got, "agent: unknown") {
		t.Errorf("stdout = %q, want it to lead with an unknown agent version", got)
	}
	if !strings.Contains(got, "api\tstatus=-\tport=3000") {
		t.Errorf("stdout = %q, want the apps listed anyway", got)
	}
}

func TestRunStatusRemoteConnected(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/agents/box.public.getpiper.co":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"agent": "box.public.getpiper.co", "connected": true,
			})
		case "/agents/box.public.getpiper.co/v1/version":
			_ = json.NewEncoder(w).Encode(map[string]string{"version": "0.8.5"})
		case "/agents/box.public.getpiper.co/v1/apps":
			_ = json.NewEncoder(w).Encode([]api.App{
				{App: store.App{Name: "blog", Port: 8080, Hostname: "ab12-alice.public.getpiper.co"}, Status: "running"},
			})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	if err := config.SaveClient(config.ClientConfig{RelayAPI: srv.URL, AccountCredential: "cred-xyz"}); err != nil {
		t.Fatalf("SaveClient: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--remote", "box.public.getpiper.co", "status"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	want := "box box.public.getpiper.co: connected\nagent: 0.8.5\nblog\tstatus=running\tport=8080\thttps://ab12-alice.public.getpiper.co\n"
	if got := stdout.String(); got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

func TestRunStatusRemoteOffline(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agents/box.public.getpiper.co" {
			// Offline must short-circuit: no app listing request.
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"agent": "box.public.getpiper.co", "connected": false,
		})
	}))
	defer srv.Close()
	if err := config.SaveClient(config.ClientConfig{RelayAPI: srv.URL, AccountCredential: "cred-xyz"}); err != nil {
		t.Fatalf("SaveClient: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--remote", "box.public.getpiper.co", "status"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if got := stdout.String(); got != "box box.public.getpiper.co: offline\n" {
		t.Errorf("stdout = %q", got)
	}
}

func TestRunStatusRejectsArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"status", "extra"}, &stdout, &stderr); code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
}
