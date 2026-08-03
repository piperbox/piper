package client

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/piperbox/piper/internal/api"
	"github.com/piperbox/piper/internal/domain"
	"github.com/piperbox/piper/internal/store"
)

func writeJSONTest(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func TestTarDirWritesRelativeFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "cmd"), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM alpine\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmd", "run.sh"), []byte("echo ready\n"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var buf bytes.Buffer
	if err := TarDir(dir, &buf); err != nil {
		t.Fatalf("TarDir: %v", err)
	}

	got := make(map[string]string)
	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		contents, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("ReadAll %s: %v", hdr.Name, err)
		}
		got[hdr.Name] = string(contents)
	}
	if got["Dockerfile"] != "FROM alpine\n" || got["cmd/run.sh"] != "echo ready\n" {
		t.Errorf("tar contents = %#v", got)
	}
}

// tarEntries tars dir and returns the entry names it produced.
func tarEntries(t *testing.T, dir string) map[string]bool {
	t.Helper()
	var buf bytes.Buffer
	if err := TarDir(dir, &buf); err != nil {
		t.Fatalf("TarDir: %v", err)
	}
	got := make(map[string]bool)
	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		got[hdr.Name] = true
	}
	return got
}

func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, contents := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
}

func TestTarDirHonorsDockerignore(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"Dockerfile":                "FROM alpine\n",
		".dockerignore":             "node_modules\n.git\n",
		"app.js":                    "app\n",
		"node_modules/pkg/index.js": "dep\n",
		".git/HEAD":                 "ref\n",
		"public/logo.svg":           "svg\n",
	})

	got := tarEntries(t, dir)
	for _, want := range []string{"Dockerfile", ".dockerignore", "app.js", "public/logo.svg"} {
		if !got[want] {
			t.Errorf("missing %q; entries = %v", want, got)
		}
	}
	for _, banned := range []string{"node_modules/pkg/index.js", ".git/HEAD"} {
		if got[banned] {
			t.Errorf("shipped ignored file %q", banned)
		}
	}
}

func TestTarDirDockerignoreNegation(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"Dockerfile":    "FROM alpine\n",
		".dockerignore": "dist\n!dist/keep.txt\n",
		"dist/keep.txt": "keep\n",
		"dist/drop.txt": "drop\n",
	})

	got := tarEntries(t, dir)
	if !got["dist/keep.txt"] {
		t.Errorf("negated file dropped; entries = %v", got)
	}
	if got["dist/drop.txt"] {
		t.Errorf("shipped ignored file dist/drop.txt")
	}
}

func TestTarDirAlwaysShipsDockerfileAndIgnorefile(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"Dockerfile":    "FROM alpine\n",
		".dockerignore": "*\n",
		"app.js":        "app\n",
	})

	got := tarEntries(t, dir)
	if !got["Dockerfile"] || !got[".dockerignore"] {
		t.Errorf("Dockerfile/.dockerignore must always ship; entries = %v", got)
	}
	if got["app.js"] {
		t.Errorf("shipped ignored file app.js")
	}
}

func TestListApps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/apps" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]api.App{
			{App: store.App{Name: "blog", Port: 8080}, Status: "running"},
		})
	}))
	defer srv.Close()

	apps, err := New(srv.URL, "").ListApps()
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	if len(apps) != 1 || apps[0].Name != "blog" || apps[0].Port != 8080 || apps[0].Status != "running" {
		t.Errorf("apps = %+v", apps)
	}
}

func TestLiveness(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The liveness resource is the client's base path itself.
		if r.Method != http.MethodGet || r.URL.Path != "/agents/box.public.getpiper.co" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer cred" {
			t.Errorf("Authorization = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"agent": "box.public.getpiper.co", "connected": true,
		})
	}))
	defer srv.Close()

	live, err := New(srv.URL+"/agents/box.public.getpiper.co", "cred").Liveness()
	if err != nil {
		t.Fatalf("Liveness: %v", err)
	}
	if live.Agent != "box.public.getpiper.co" || !live.Connected {
		t.Errorf("live = %+v", live)
	}
}

func TestCreateApp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/apps" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		var body struct {
			Name string `json:"name"`
			Port int    `json:"port"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if body.Name != "blog" || body.Port != 3000 {
			t.Errorf("body = %+v", body)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	if err := New(srv.URL, "").CreateApp("blog", 3000); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
}

func TestDeploy(t *testing.T) {
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "Dockerfile"), []byte("FROM alpine\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/apps/blog/deploy" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/x-tar" {
			t.Errorf("Content-Type = %q", got)
		}
		tr := tar.NewReader(r.Body)
		hdr, err := tr.Next()
		if err != nil {
			t.Errorf("Next: %v", err)
		} else if hdr.Name != "Dockerfile" {
			t.Errorf("tar entry = %q", hdr.Name)
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(store.Deployment{ID: "dep1", App: "blog", Status: "building"})
	}))
	defer srv.Close()

	dep, err := New(srv.URL, "").Deploy("blog", srcDir)
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if dep.ID != "dep1" || dep.Status != "building" {
		t.Errorf("deployment = %+v", dep)
	}
}

func TestDeployNotBoundByClientTimeout(t *testing.T) {
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "Dockerfile"), []byte("FROM alpine\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Outlive the client's overall timeout, like a big upload to a slow box.
		time.Sleep(250 * time.Millisecond)
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(store.Deployment{ID: "dep1", App: "blog", Status: "building"})
	}))
	defer srv.Close()

	dep, err := New(srv.URL, "").WithTimeout(50*time.Millisecond).Deploy("blog", srcDir)
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if dep.ID != "dep1" {
		t.Errorf("deployment = %+v", dep)
	}
}

func TestDeployFromRepo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/apps/blog/deploy-from-repo" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(store.Deployment{ID: "dep1", App: "blog", Status: "building"})
	}))
	defer srv.Close()

	dep, err := New(srv.URL, "").DeployFromRepo("blog")
	if err != nil {
		t.Fatalf("DeployFromRepo: %v", err)
	}
	if dep.ID != "dep1" || dep.Status != "building" {
		t.Errorf("deployment = %+v", dep)
	}
}

func TestDeployFromRepoNotBoundByClientTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Outlive the client's overall timeout, like a slow repo fetch on the box.
		time.Sleep(250 * time.Millisecond)
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(store.Deployment{ID: "dep1", App: "blog", Status: "building"})
	}))
	defer srv.Close()

	dep, err := New(srv.URL, "").WithTimeout(50 * time.Millisecond).DeployFromRepo("blog")
	if err != nil {
		t.Fatalf("DeployFromRepo: %v", err)
	}
	if dep.ID != "dep1" {
		t.Errorf("deployment = %+v", dep)
	}
}

func TestFollowDeployStreamsThenReportsTerminal(t *testing.T) {
	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/apps/web/deployments/dep1/logs":
			n := atomic.AddInt32(&polls, 1)
			w.Header().Set("Content-Type", "text/plain")
			if n < 3 {
				io.WriteString(w, "line1\n")
			} else {
				io.WriteString(w, "line1\nline2\n")
			}
		case r.URL.Path == "/v1/apps/web/deployments":
			status := "building"
			if atomic.LoadInt32(&polls) >= 3 {
				status = "running"
			}
			writeJSONTest(w, []store.Deployment{{ID: "dep1", App: "web", Status: status}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	c.pollInterval = time.Millisecond
	var progress bytes.Buffer
	dep, err := c.FollowDeploy(context.Background(), "web", "dep1", &progress)
	if err != nil {
		t.Fatalf("FollowDeploy: %v", err)
	}
	if dep.Status != "running" {
		t.Fatalf("status = %q, want running", dep.Status)
	}
	if progress.String() != "line1\nline2\n" {
		t.Fatalf("progress = %q, want the full log printed once (no dupes)", progress.String())
	}
}

func TestFollowDeploySurvivesTransientPollErrors(t *testing.T) {
	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/apps/web/deployments/dep1/logs":
			n := atomic.AddInt32(&polls, 1)
			if n == 2 || n == 3 {
				// A momentarily unhappy box (e.g. store busy mid-build) must
				// not end a minutes-long follow.
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			io.WriteString(w, "line1\n")
		case "/v1/apps/web/deployments":
			status := "building"
			if atomic.LoadInt32(&polls) >= 4 {
				status = "running"
			}
			writeJSONTest(w, []store.Deployment{{ID: "dep1", App: "web", Status: status}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	c.pollInterval = time.Millisecond
	var progress bytes.Buffer
	dep, err := c.FollowDeploy(context.Background(), "web", "dep1", &progress)
	if err != nil {
		t.Fatalf("FollowDeploy: %v", err)
	}
	if dep.Status != "running" {
		t.Fatalf("status = %q, want running", dep.Status)
	}
}

func TestFollowDeployGivesUpAfterPersistentErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	c.pollInterval = time.Millisecond
	if _, err := c.FollowDeploy(context.Background(), "web", "dep1", io.Discard); err == nil {
		t.Fatal("FollowDeploy = nil error, want failure after persistent errors")
	}
}

func TestFollowDeployReprintsRotatedTail(t *testing.T) {
	const first = "[log truncated]\nold tail\n"
	const second = "[log truncated]\nnew tail\n"
	if len(first) != len(second) {
		t.Fatal("test snapshots must have equal length")
	}
	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/apps/web/deployments/dep1/logs":
			if atomic.AddInt32(&polls, 1) == 1 {
				io.WriteString(w, first)
			} else {
				io.WriteString(w, second)
			}
		case "/v1/apps/web/deployments":
			status := "building"
			if atomic.LoadInt32(&polls) >= 2 {
				status = "running"
			}
			writeJSONTest(w, []store.Deployment{{ID: "dep1", App: "web", Status: status}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	c.pollInterval = time.Millisecond
	var progress bytes.Buffer
	dep, err := c.FollowDeploy(context.Background(), "web", "dep1", &progress)
	if err != nil {
		t.Fatalf("FollowDeploy: %v", err)
	}
	if dep.Status != "running" {
		t.Fatalf("status = %q, want running", dep.Status)
	}
	if progress.String() != first+second {
		t.Fatalf("progress = %q, want both equal-length tail snapshots", progress.String())
	}
}

// A row stuck "building" (piperd stranded it) must not poll forever: FollowDeploy
// gives up when the context deadline passes, returning the last-seen row. #161.
func TestFollowDeployHonorsContextDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/apps/web/deployments/dep1/logs":
			w.Header().Set("Content-Type", "text/plain")
			io.WriteString(w, "building...\n")
		case "/v1/apps/web/deployments":
			writeJSONTest(w, []store.Deployment{{ID: "dep1", App: "web", Status: "building"}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	c.pollInterval = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	var progress bytes.Buffer
	dep, err := c.FollowDeploy(ctx, "web", "dep1", &progress)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if dep.Status != "building" {
		t.Fatalf("last status = %q, want building (the stranded row)", dep.Status)
	}
}

func TestLinkApp(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	if err := c.LinkApp("blog", "alice/blog", "main", "apps/web"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/apps/blog/link" {
		t.Fatalf("path = %s", gotPath)
	}
	if !strings.Contains(gotBody, `"alice/blog"`) || !strings.Contains(gotBody, `"main"`) {
		t.Fatalf("body = %s", gotBody)
	}
	if !strings.Contains(gotBody, `"root_dir":"apps/web"`) {
		t.Fatalf("body = %s, want root_dir", gotBody)
	}
}

func TestManifestAndExchange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/github/manifest":
			io.WriteString(w, `{"manifest":"{\"name\":\"x\"}"}`)
		case "/v1/github/exchange":
			io.WriteString(w, `{"slug":"piper-abc"}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	m, err := c.Manifest("http://localhost:5000/cb")
	if err != nil || !strings.Contains(m, `"name"`) {
		t.Fatalf("Manifest m=%q err=%v", m, err)
	}
	slug, err := c.ExchangeGitHub("thecode")
	if err != nil {
		t.Fatalf("ExchangeGitHub: %v", err)
	}
	if slug != "piper-abc" {
		t.Fatalf("slug = %q, want piper-abc", slug)
	}
}

func TestResetGitHubReportsNextProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/github/app" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		io.WriteString(w, `{"provider":"brokered"}`)
	}))
	defer srv.Close()

	got, err := New(srv.URL, "").ResetGitHub()
	if err != nil {
		t.Fatalf("ResetGitHub: %v", err)
	}
	if got != "brokered" {
		t.Fatalf("provider = %q, want brokered", got)
	}
}

func TestSetsAuthorizationHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode([]store.App{})
	}))
	defer srv.Close()

	if _, err := New(srv.URL, "s3cr3t").ListApps(); err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	if gotAuth != "Bearer s3cr3t" {
		t.Fatalf("Authorization = %q, want Bearer s3cr3t", gotAuth)
	}
}

func TestClientMethodsReportHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusTeapot)
	}))
	defer srv.Close()
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "Dockerfile"), []byte("FROM alpine\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	c := New(srv.URL, "")
	tests := map[string]func() error{
		"create": func() error { return c.CreateApp("blog", 8080) },
		"list": func() error {
			_, err := c.ListApps()
			return err
		},
		"deploy": func() error {
			_, err := c.Deploy("blog", srcDir)
			return err
		},
		"stop":   func() error { return c.StopApp("blog") },
		"start":  func() error { return c.StartApp("blog") },
		"delete": func() error { return c.DeleteApp("blog") },
	}
	for name, call := range tests {
		t.Run(name, func(t *testing.T) {
			err := call()
			if err == nil || !strings.Contains(err.Error(), "418") || !strings.Contains(err.Error(), "boom") {
				t.Errorf("error = %v, want status and response body", err)
			}
		})
	}
}

func TestStopApp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/apps/blog/stop" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	if err := New(srv.URL, "").StopApp("blog"); err != nil {
		t.Fatalf("StopApp: %v", err)
	}
}

func TestStopAppErrorIncludesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unknown app", http.StatusNotFound)
	}))
	defer srv.Close()
	err := New(srv.URL, "").StopApp("ghost")
	if err == nil || !strings.Contains(err.Error(), "unknown app") {
		t.Fatalf("err = %v, want body in message", err)
	}
}

func TestStartApp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/apps/blog/start" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	if err := New(srv.URL, "").StartApp("blog"); err != nil {
		t.Fatalf("StartApp: %v", err)
	}
}

func TestStartAppErrorIncludesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unknown app", http.StatusNotFound)
	}))
	defer srv.Close()
	err := New(srv.URL, "").StartApp("ghost")
	if err == nil || !strings.Contains(err.Error(), "unknown app") {
		t.Fatalf("err = %v, want body in message", err)
	}
}

func TestDeleteApp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/apps/blog" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	if err := New(srv.URL, "").DeleteApp("blog"); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
}

func TestDeleteAppErrorIncludesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unknown app", http.StatusNotFound)
	}))
	defer srv.Close()
	err := New(srv.URL, "").DeleteApp("ghost")
	if err == nil || !strings.Contains(err.Error(), "unknown app") {
		t.Fatalf("err = %v, want body in message", err)
	}
}

func TestApp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/apps/web" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		writeJSONTest(w, api.App{
			App:    store.App{Name: "web", Port: 8080, Hostname: "web.piper.localhost"},
			Status: "running",
		})
	}))
	defer srv.Close()

	app, err := New(srv.URL, "").App("web")
	if err != nil {
		t.Fatalf("App: %v", err)
	}
	if app.Name != "web" || app.Hostname != "web.piper.localhost" || app.Status != "running" {
		t.Errorf("app = %+v", app)
	}
}

func TestDeployments(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/apps/web/deployments" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		writeJSONTest(w, []store.Deployment{
			{ID: "dep1", App: "web", Status: "running"},
			{ID: "dep2", App: "web", Status: "failed"},
		})
	}))
	defer srv.Close()

	deps, err := New(srv.URL, "").Deployments("web")
	if err != nil {
		t.Fatalf("Deployments: %v", err)
	}
	if len(deps) != 2 || deps[0].ID != "dep1" || deps[0].Status != "running" || deps[1].ID != "dep2" || deps[1].Status != "failed" {
		t.Errorf("deployments = %+v", deps)
	}
}

func TestDeploymentLogs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/apps/web/deployments/dep1/logs" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		io.WriteString(w, "line1\nline2\n")
	}))
	defer srv.Close()

	logs, err := New(srv.URL, "").DeploymentLogs("web", "dep1")
	if err != nil {
		t.Fatalf("DeploymentLogs: %v", err)
	}
	if logs != "line1\nline2\n" {
		t.Errorf("logs = %q", logs)
	}
}

// TestWithTimeoutAbortsHungRequest proves WithTimeout cancels a request that
// outlives it, rather than waiting for the server to respond — a blackholed
// box must surface as unreachable instead of hanging the TUI's poll forever.
func TestWithTimeoutAbortsHungRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_ = json.NewEncoder(w).Encode([]api.App{})
	}))
	defer srv.Close()

	start := time.Now()
	_, err := New(srv.URL, "").WithTimeout(20 * time.Millisecond).ListApps()
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("ListApps: want error from timeout, got nil (elapsed %s)", elapsed)
	}
	if elapsed >= 150*time.Millisecond {
		t.Fatalf("ListApps took %s, want well under the handler's 200ms sleep (timeout should have cancelled it)", elapsed)
	}
}

// TestWithTimeoutAllowsFastResponse proves the failure above is caused by the
// timeout, not the endpoint: a generous timeout against the same kind of
// fast-responding server still succeeds.
func TestWithTimeoutAllowsFastResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]api.App{
			{App: store.App{Name: "blog", Port: 8080}, Status: "running"},
		})
	}))
	defer srv.Close()

	apps, err := New(srv.URL, "").WithTimeout(time.Second).ListApps()
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	if len(apps) != 1 || apps[0].Name != "blog" {
		t.Errorf("apps = %+v", apps)
	}
}

func TestResponseErrorsCarryHTTPStatusCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	defer srv.Close()
	_, err := New(srv.URL, "bad").ListApps()
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("error = %v (%T), want *StatusError", err, err)
	}
	if se.Code != http.StatusUnauthorized {
		t.Fatalf("Code = %d, want %d", se.Code, http.StatusUnauthorized)
	}
}

func TestAppDomains(t *testing.T) {
	notAfter := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/apps/blog/domains" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		writeJSONTest(w, []domain.AppDomainStatus{{
			Domain: "myshop.com", App: "blog", Status: "active",
			CertNotAfter: &notAfter,
			DNSRecords:   []domain.DNSRecord{{Type: "CNAME", Name: "myshop.com", Value: "relay.example.net"}},
			DNSOK:        true,
		}})
	}))
	defer srv.Close()

	ds, err := New(srv.URL, "").AppDomains("blog")
	if err != nil {
		t.Fatalf("AppDomains: %v", err)
	}
	if len(ds) != 1 {
		t.Fatalf("domains = %+v, want 1", ds)
	}
	d := ds[0]
	if d.Domain != "myshop.com" || d.App != "blog" || d.Status != "active" || !d.DNSOK {
		t.Errorf("domain = %+v", d)
	}
	if d.CertNotAfter == nil || !d.CertNotAfter.Equal(notAfter) {
		t.Errorf("CertNotAfter = %v, want %v", d.CertNotAfter, notAfter)
	}
	wantRec := domain.DNSRecord{Type: "CNAME", Name: "myshop.com", Value: "relay.example.net"}
	if len(d.DNSRecords) != 1 || d.DNSRecords[0] != wantRec {
		t.Errorf("DNSRecords = %+v, want [%+v]", d.DNSRecords, wantRec)
	}
}

func TestAddAppDomain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/apps/blog/domains" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		var body struct {
			Domain string `json:"domain"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Domain != "myshop.com" {
			t.Errorf("body = %+v (err %v)", body, err)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(domain.AppDomainStatus{
			Domain: "myshop.com", App: "blog", Status: "pending",
			DNSRecords: []domain.DNSRecord{{Type: "CNAME", Name: "myshop.com", Value: "relay.example.net"}},
		})
	}))
	defer srv.Close()

	st, err := New(srv.URL, "").AddAppDomain("blog", "myshop.com")
	if err != nil {
		t.Fatalf("AddAppDomain: %v", err)
	}
	if st.Domain != "myshop.com" || st.App != "blog" || st.Status != "pending" {
		t.Errorf("status = %+v", st)
	}
	if len(st.DNSRecords) != 1 || st.DNSRecords[0].Value != "relay.example.net" {
		t.Errorf("DNSRecords = %+v", st.DNSRecords)
	}
}

func TestAddAppDomainErrorIncludesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "domain config requires a relay: connect this box to a relay first", http.StatusConflict)
	}))
	defer srv.Close()
	_, err := New(srv.URL, "").AddAppDomain("blog", "myshop.com")
	if err == nil || !strings.Contains(err.Error(), "requires a relay") {
		t.Fatalf("err = %v, want body in message", err)
	}
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusConflict {
		t.Fatalf("err = %v, want *StatusError with 409", err)
	}
}

func TestRemoveAppDomain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/apps/blog/domains/myshop.com" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	if err := New(srv.URL, "").RemoveAppDomain("blog", "myshop.com"); err != nil {
		t.Fatalf("RemoveAppDomain: %v", err)
	}
}

func TestRemoveAppDomainErrorIncludesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unknown domain", http.StatusNotFound)
	}))
	defer srv.Close()
	err := New(srv.URL, "").RemoveAppDomain("blog", "ghost.com")
	if err == nil || !strings.Contains(err.Error(), "unknown domain") {
		t.Fatalf("err = %v, want body in message", err)
	}
}

func TestStatusErrorUnauthorized(t *testing.T) {
	if !(&StatusError{Code: http.StatusUnauthorized}).Unauthorized() {
		t.Fatal("401 StatusError should report Unauthorized")
	}
	if (&StatusError{Code: http.StatusInternalServerError}).Unauthorized() {
		t.Fatal("500 StatusError should not report Unauthorized")
	}
}

// TestMutatingCallsOutliveTheShortPollTimeout pins #379: the TUI applies a 5s
// timeout to the whole client so a blackholed box reads as unreachable, but
// these calls do real work on the box — Start re-runs a container and waits up
// to 30s for its health check, Stop waits out Docker's 10s stop grace, Delete
// pays that grace once per running deployment and then prunes images, and
// RemoveAppDomain makes a relay round-trip before tearing the cert and route
// down. The short poll timeout aborted the client while the box completed the
// work, so the TUI reported failure for an action that had actually succeeded.
//
// The handler here sleeps past the client's configured timeout; the call must
// still succeed.
func TestMutatingCallsOutliveTheShortPollTimeout(t *testing.T) {
	const pollTimeout = 50 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(4 * pollTimeout) // the box is busy doing the work
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	for _, tc := range []struct {
		name string
		call func(*Client) error
	}{
		{"stop", func(c *Client) error { return c.StopApp("blog") }},
		{"start", func(c *Client) error { return c.StartApp("blog") }},
		{"delete", func(c *Client) error { return c.DeleteApp("blog") }},
		{"remove domain", func(c *Client) error { return c.RemoveAppDomain("blog", "www.example.com") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New(srv.URL, "").WithTimeout(pollTimeout)
			if err := tc.call(c); err != nil {
				t.Fatalf("%s aborted by the poll timeout: %v", tc.name, err)
			}
		})
	}
}

// TestAppEnvReturnsUpdatedAt pins the new client method that exposes when
// each env var was last changed. Against older code this test fails to compile
// because AppEnvWithTimestamps does not exist.
func TestAppEnvReturnsUpdatedAt(t *testing.T) {
	updatedAt := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/apps/blog/env" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		writeJSONTest(w, map[string]any{
			"env":        map[string]string{"A": "one"},
			"updated_at": map[string]string{"A": updatedAt.Format(time.RFC3339Nano)},
		})
	}))
	defer srv.Close()

	env, updated, err := New(srv.URL, "").AppEnvWithTimestamps("blog")
	if err != nil {
		t.Fatalf("AppEnvWithTimestamps: %v", err)
	}
	if len(env) != 1 || env["A"] != "one" {
		t.Errorf("env = %v", env)
	}
	if len(updated) != 1 || !updated["A"].Equal(updatedAt) {
		t.Errorf("updated = %v, want %v", updated, updatedAt)
	}
}

func TestAgentVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/version" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		writeJSONTest(w, map[string]string{"version": "0.8.5"})
	}))
	defer srv.Close()

	got, err := New(srv.URL, "").AgentVersion()
	if err != nil {
		t.Fatalf("AgentVersion: %v", err)
	}
	if got != "0.8.5" {
		t.Errorf("version = %q, want 0.8.5", got)
	}
}

// An agent from before the endpoint existed answers 404. That is not an error
// to shout about — it is itself the answer ("too old to say"), and the caller
// needs to tell it apart from a real failure, so it comes back as a sentinel.
func TestAgentVersionOldAgentReportsUnsupported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "404 page not found", http.StatusNotFound)
	}))
	defer srv.Close()

	got, err := New(srv.URL, "").AgentVersion()
	if !errors.Is(err, ErrVersionUnsupported) {
		t.Fatalf("err = %v, want ErrVersionUnsupported", err)
	}
	if got != "" {
		t.Errorf("version = %q, want empty", got)
	}
}

// The daemon self-reports the listen config it loaded alongside its version,
// so `agent status` can print the truth without reading the process env
// (root-only for a system piperd, #476). Absent fields — an older daemon —
// decode as empty strings, which the caller treats as "did not report".
func TestAgentInfoReportsEffectiveConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/version" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		writeJSONTest(w, map[string]string{
			"version":    "0.16.1",
			"http_addr":  "127.0.0.1:8081",
			"https_addr": "127.0.0.1:8444",
			"data_dir":   "/var/lib/piper",
		})
	}))
	defer srv.Close()

	got, err := New(srv.URL, "").AgentInfo()
	if err != nil {
		t.Fatalf("AgentInfo: %v", err)
	}
	want := AgentInfo{Version: "0.16.1", HTTPAddr: "127.0.0.1:8081", HTTPSAddr: "127.0.0.1:8444", DataDir: "/var/lib/piper"}
	if got != want {
		t.Errorf("info = %+v, want %+v", got, want)
	}
}

func TestAgentInfoOldAgentReportsUnsupported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "404 page not found", http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := New(srv.URL, "").AgentInfo(); !errors.Is(err, ErrVersionUnsupported) {
		t.Fatalf("err = %v, want ErrVersionUnsupported", err)
	}
}

// TestReadsKeepTheShortPollTimeout guards the reason the timeout exists: a
// blackholed box must still surface as unreachable rather than hanging the
// TUI's poll loop.
func TestReadsKeepTheShortPollTimeout(t *testing.T) {
	const pollTimeout = 50 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(4 * pollTimeout)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL, "").WithTimeout(pollTimeout)
	if _, err := c.ListApps(); err == nil {
		t.Fatal("ListApps returned nil, want the poll timeout to still bound reads")
	}
}
