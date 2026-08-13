package main

import (
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

// The daemon names the scheme its apps are served on and every CLI surface
// must print that name. How this client dialled stopped being a usable proxy
// for it: a box that terminates TLS for its own domain with no relay at all
// serves HTTPS while the CLI reaches it over the LAN (#507). Inferring the
// scheme from the dial made `piper list` print http:// for the very box whose
// TUI printed https:// — one binary, two answers for one box.
//
// One test per call site, because the inference lived at the call sites: the
// scheme has to be threaded through each of them, and a fix that reaches only
// `list` leaves `status` and `deploy` still guessing.

// listAppsServer answers GET /v1/apps with one app carrying scheme.
func listAppsServer(t *testing.T, scheme string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/version":
			_ = json.NewEncoder(w).Encode(map[string]string{"version": "0.8.5"})
		case "/v1/apps":
			_ = json.NewEncoder(w).Encode([]api.App{{
				App:    store.App{Name: "blog", Port: 8080, Hostname: "blog.example.dev"},
				Status: "running", Scheme: scheme,
			}})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
}

func TestListPrintsTheSchemeTheDaemonReports(t *testing.T) {
	srv := listAppsServer(t, "https")
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"list"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	want := "blog\tport=8080\thttps://blog.example.dev\n"
	if got := stdout.String(); got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

func TestStatusPrintsTheSchemeTheDaemonReports(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	srv := listAppsServer(t, "https")
	defer srv.Close()
	if err := config.SaveClient(config.ClientConfig{Addr: srv.URL, Token: "tok"}); err != nil {
		t.Fatalf("SaveClient: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"status"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	want := "agent: 0.8.5\nblog\tstatus=running\tport=8080\thttps://blog.example.dev\n"
	if got := stdout.String(); got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

func TestDeployPrintsTheSchemeTheDaemonReports(t *testing.T) {
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "Dockerfile"), []byte("FROM alpine\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/apps/blog/deploy":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(store.Deployment{ID: "dep1", App: "blog", Status: "building"})
		case r.URL.Path == "/v1/apps/blog/deployments/dep1/logs":
			_, _ = io.WriteString(w, "built ok\n")
		case r.URL.Path == "/v1/apps/blog/deployments":
			_ = json.NewEncoder(w).Encode([]store.Deployment{{ID: "dep1", App: "blog", Status: "running"}})
		case r.URL.Path == "/v1/apps/blog":
			_ = json.NewEncoder(w).Encode(api.App{
				App:    store.App{Name: "blog", Hostname: "blog.example.dev"},
				Status: "running", Scheme: "https",
			})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"deploy", "blog", "--path", srcDir}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	want := "deployed blog: https://blog.example.dev (running)\n"
	if got := stdout.String(); got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

// A relay-backed box reports "https" the same way, so a `--remote` dial is not
// what makes the URL secure either — dropping the dial-based inference must
// not quietly change what a relay box prints.
func TestListOverARelayStillFollowsTheReportedScheme(t *testing.T) {
	srv := listAppsServer(t, "https")
	defer srv.Close()
	t.Setenv("PIPER_ADDR", srv.URL)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"list"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "https://blog.example.dev") {
		t.Errorf("stdout = %q, want the https URL", stdout.String())
	}
}
