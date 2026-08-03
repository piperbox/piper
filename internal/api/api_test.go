package api

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
	"testing"
	"time"

	"github.com/piperbox/piper/internal/domain"
	"github.com/piperbox/piper/internal/store"
	"github.com/piperbox/piper/internal/version"
)

type fakeDeployer struct {
	store         *store.Store
	gotApp        string
	gotFile       string
	stopped       []string
	started       []string
	deleted       []string
	stopErr       error
	startErr      error
	deleteErr     error
	panicOnFinish bool
}

func (f *fakeDeployer) Begin(app string) (store.Deployment, error) {
	f.gotApp = app
	return f.store.CreateDeployment(app, "", "", 0, "building", "")
}

func (f *fakeDeployer) Finish(_ context.Context, dep store.Deployment, srcDir string) error {
	if f.panicOnFinish {
		panic("boom: simulated Finish panic")
	}
	contents, err := os.ReadFile(filepath.Join(srcDir, "Dockerfile"))
	if err != nil {
		_ = f.store.FinalizeDeployment(dep.ID, "", "", 0, "failed", err.Error())
		return err
	}
	f.gotFile = string(contents)
	return f.store.FinalizeDeployment(dep.ID, "img1", "cid1", 40001, "running", "built ok")
}

func (f *fakeDeployer) Stop(_ context.Context, app string) error {
	if f.stopErr != nil {
		return f.stopErr
	}
	f.stopped = append(f.stopped, app)
	return nil
}

func (f *fakeDeployer) Start(_ context.Context, app string) error {
	if f.startErr != nil {
		return f.startErr
	}
	f.started = append(f.started, app)
	return nil
}

func (f *fakeDeployer) Delete(_ context.Context, app string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, app)
	return nil
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	s := newTestStore(t)
	return New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil, nil, nil, nil, AgentInfo{})
}

// The running daemon is the only thing that knows its own version — the
// binary on disk may already have been replaced, and the CLI asking is a
// separate build entirely (#375). Reachable over the tunnel like any other
// /v1 route, which is what lets `piper status --remote` report a box's agent.
func TestVersionReportsTheRunningDaemon(t *testing.T) {
	h := newTestHandler(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/version", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Version != version.String() {
		t.Errorf("version = %q, want this build's %q", got.Version, version.String())
	}
}

// The daemon is likewise the only thing that knows the listen config it
// actually loaded: `agent status` reads /proc/<pid>/environ for it, which is
// root-only for a system piperd, so a non-root status used to state the
// built-in defaults (:80/:443) as fact on boxes whose piperd.env says
// otherwise (#476).
func TestVersionReportsEffectiveConfig(t *testing.T) {
	s := newTestStore(t)
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil, nil, nil, nil,
		AgentInfo{HTTPAddr: "127.0.0.1:8081", HTTPSAddr: "127.0.0.1:8444", DataDir: "/var/lib/piper"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/version", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got struct {
		HTTPAddr  string `json:"http_addr"`
		HTTPSAddr string `json:"https_addr"`
		DataDir   string `json:"data_dir"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.HTTPAddr != "127.0.0.1:8081" || got.HTTPSAddr != "127.0.0.1:8444" || got.DataDir != "/var/lib/piper" {
		t.Errorf("self-report = %+v, want the config the daemon loaded", got)
	}
}

func TestCreateAndListApp(t *testing.T) {
	h := newTestHandler(t)

	create := httptest.NewRequest(http.MethodPost, "/v1/apps", strings.NewReader(`{"name":"blog","port":8080}`))
	created := httptest.NewRecorder()
	h.ServeHTTP(created, create)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", created.Code, created.Body.String())
	}

	listed := httptest.NewRecorder()
	h.ServeHTTP(listed, httptest.NewRequest(http.MethodGet, "/v1/apps", nil))
	if listed.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", listed.Code, listed.Body.String())
	}
	var apps []store.App
	if err := json.NewDecoder(listed.Body).Decode(&apps); err != nil {
		t.Fatalf("decode apps: %v", err)
	}
	if len(apps) != 1 || apps[0].Name != "blog" || apps[0].Port != 8080 {
		t.Errorf("apps = %+v, want blog on port 8080", apps)
	}
}

func TestCreateDuplicateReturnsConflict(t *testing.T) {
	h := newTestHandler(t)
	body := `{"name":"blog","port":8080}`
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/apps", strings.NewReader(body)))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps", strings.NewReader(body)))
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusConflict)
	}
}

func TestCreateAppDefaultsPort(t *testing.T) {
	h := newTestHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps", strings.NewReader(`{"name":"blog"}`)))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var app store.App
	if err := json.NewDecoder(rec.Body).Decode(&app); err != nil {
		t.Fatalf("decode app: %v", err)
	}
	if app.Port != 8080 {
		t.Errorf("port = %d, want 8080", app.Port)
	}
}

func TestGetApp(t *testing.T) {
	h := newTestHandler(t)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/apps", strings.NewReader(`{"name":"blog"}`)))

	found := httptest.NewRecorder()
	h.ServeHTTP(found, httptest.NewRequest(http.MethodGet, "/v1/apps/blog", nil))
	if found.Code != http.StatusOK {
		t.Fatalf("found status = %d, body = %s", found.Code, found.Body.String())
	}
	var app store.App
	if err := json.NewDecoder(found.Body).Decode(&app); err != nil {
		t.Fatalf("decode app: %v", err)
	}
	if app.Name != "blog" {
		t.Errorf("name = %q, want blog", app.Name)
	}

	missing := httptest.NewRecorder()
	h.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/v1/apps/missing", nil))
	if missing.Code != http.StatusNotFound {
		t.Errorf("missing status = %d, want %d", missing.Code, http.StatusNotFound)
	}
}

func TestListAppsEmptyReturnsArray(t *testing.T) {
	h := newTestHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/apps", nil))
	if body := strings.TrimSpace(rec.Body.String()); body != "[]" {
		t.Errorf("body = %s, want []", body)
	}
}

func TestDeployIsAsyncAndDrivesRowToRunning(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateApp("web", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	deployer := &fakeDeployer{store: s}
	h := New(s, deployer, "piper.localhost", "", nil, nil, nil, nil, nil, AgentInfo{})

	var tarball bytes.Buffer
	tw := tar.NewWriter(&tarball)
	body := []byte("FROM scratch\n")
	_ = tw.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0o644, Size: int64(len(body))})
	_, _ = tw.Write(body)
	_ = tw.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/apps/web/deploy", &tarball)
	req.Header.Set("Content-Type", "application/x-tar")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	var dep store.Deployment
	if err := json.Unmarshal(rr.Body.Bytes(), &dep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dep.ID == "" || dep.Status != "building" {
		t.Fatalf("202 body = %+v, want building row with id", dep)
	}

	// The goroutine finalizes the row; poll the store until it does.
	waitForStatus(t, s, "web", "running")
	if deployer.gotFile != "FROM scratch\n" {
		t.Fatalf("Finish saw Dockerfile %q", deployer.gotFile)
	}
}

func TestDeployPanicInFinishIsRecoveredAndFailsRow(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateApp("web", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	deployer := &fakeDeployer{store: s, panicOnFinish: true}
	h := New(s, deployer, "piper.localhost", "", nil, nil, nil, nil, nil, AgentInfo{})

	var tarball bytes.Buffer
	tw := tar.NewWriter(&tarball)
	body := []byte("FROM scratch\n")
	_ = tw.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0o644, Size: int64(len(body))})
	_, _ = tw.Write(body)
	_ = tw.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/apps/web/deploy", &tarball)
	req.Header.Set("Content-Type", "application/x-tar")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}

	// If the panic isn't recovered, it propagates and crashes this test
	// process's goroutine (test binary aborts) instead of reaching here.
	waitForStatus(t, s, "web", "failed")
}

func TestDeployUnknownAppIs404(t *testing.T) {
	s := newTestStore(t)
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil, nil, nil, nil, AgentInfo{})
	req := httptest.NewRequest(http.MethodPost, "/v1/apps/ghost/deploy", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-tar")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestDeployFromRepoFetchesLinkedRepoAndDeploys(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateApp("web", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if err := s.UpdateAppRepo("web", "alice/blog", "main", ""); err != nil {
		t.Fatalf("UpdateAppRepo: %v", err)
	}
	deployer := &fakeDeployer{store: s}
	var gotRepo, gotRef string
	fetch := func(_ context.Context, repo, ref, destDir string) error {
		gotRepo, gotRef = repo, ref
		return os.WriteFile(filepath.Join(destDir, "Dockerfile"), []byte("FROM repo\n"), 0o644)
	}
	h := New(s, deployer, "piper.localhost", "", nil, nil, nil, nil, fetch, AgentInfo{})

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/apps/web/deploy-from-repo", nil))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	var dep store.Deployment
	if err := json.Unmarshal(rr.Body.Bytes(), &dep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dep.ID == "" || dep.Status != "building" {
		t.Fatalf("202 body = %+v, want building row with id", dep)
	}

	waitForStatus(t, s, "web", "running")
	if gotRepo != "alice/blog" || gotRef != "main" {
		t.Fatalf("fetched %s@%s, want alice/blog@main", gotRepo, gotRef)
	}
	if deployer.gotFile != "FROM repo\n" {
		t.Fatalf("Finish saw Dockerfile %q", deployer.gotFile)
	}
}

func TestDeployFromRepoUnlinkedAppIs409(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateApp("web", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	fetch := func(_ context.Context, _, _, _ string) error { return nil }
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil, nil, nil, fetch, AgentInfo{})

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/apps/web/deploy-from-repo", nil))
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
}

func TestDeployFromRepoUnknownAppIs404(t *testing.T) {
	s := newTestStore(t)
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil, nil, nil, nil, AgentInfo{})

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/apps/ghost/deploy-from-repo", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestDeployFromRepoWithoutGitHubIs409(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateApp("web", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if err := s.UpdateAppRepo("web", "alice/blog", "main", ""); err != nil {
		t.Fatalf("UpdateAppRepo: %v", err)
	}
	for name, fetch := range map[string]FetchRepoFunc{
		"nil fetcher":    nil,
		"fetcher errors": func(_ context.Context, _, _, _ string) error { return ErrNoGitHubApp },
	} {
		h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil, nil, nil, fetch, AgentInfo{})
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/apps/web/deploy-from-repo", nil))
		if rr.Code != http.StatusConflict {
			t.Fatalf("%s: status = %d, want 409; body=%s", name, rr.Code, rr.Body.String())
		}
	}
}

func TestDeployFromRepoFetchErrorIs502AndCreatesNoDeployment(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateApp("web", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if err := s.UpdateAppRepo("web", "alice/blog", "main", ""); err != nil {
		t.Fatalf("UpdateAppRepo: %v", err)
	}
	fetch := func(_ context.Context, _, _, _ string) error { return errors.New("tarball: 500") }
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil, nil, nil, fetch, AgentInfo{})

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/apps/web/deploy-from-repo", nil))
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rr.Code, rr.Body.String())
	}
	if _, err := s.LatestDeployment("web"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("LatestDeployment after failed fetch: %v, want ErrNotFound", err)
	}
}

func waitForStatus(t *testing.T, s *store.Store, app, want string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		d, err := s.LatestDeployment(app)
		if err == nil && d.Status == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("deployment for %s never reached %q", app, want)
}

func TestAppsAPIIncludesHostname(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if err := s.SetAppHostname("blog", "hash-blog-alice.public.getpiper.co"); err != nil {
		t.Fatalf("SetAppHostname: %v", err)
	}
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil, nil, nil, nil, AgentInfo{})

	get := httptest.NewRecorder()
	h.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/v1/apps/blog", nil))
	var one App
	if err := json.NewDecoder(get.Body).Decode(&one); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if one.Hostname != "hash-blog-alice.public.getpiper.co" {
		t.Errorf("GET app hostname = %q", one.Hostname)
	}

	list := httptest.NewRecorder()
	h.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/v1/apps", nil))
	var many []App
	if err := json.NewDecoder(list.Body).Decode(&many); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(many) != 1 || many[0].Hostname != "hash-blog-alice.public.getpiper.co" {
		t.Errorf("list hostname = %+v", many)
	}
}

func TestReservedNameRejected(t *testing.T) {
	s := newTestStore(t)
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil, nil, nil, nil, AgentInfo{})
	body := strings.NewReader(`{"name":"hooks","port":8080}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps", body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d", rec.Code)
	}
}

// App names flow unescaped into URL paths and hostnames (<app>.<baseDom>,
// pr-N-<app>.…), so create rejects anything that isn't a DNS label. #120.
func TestInvalidAppNameRejected(t *testing.T) {
	s := newTestStore(t)
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil, nil, nil, nil, AgentInfo{})
	for _, name := range []string{"Blog", "my_app", "a/b", "-lead", "trail-", "app.dot", "app name", strings.Repeat("x", 64)} {
		rec := httptest.NewRecorder()
		body := strings.NewReader(`{"name":"` + name + `","port":8080}`)
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps", body))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("name %q: code = %d, want 400", name, rec.Code)
		}
	}
	// A valid DNS-label name still succeeds.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps",
		strings.NewReader(`{"name":"my-app-1","port":8080}`)))
	if rec.Code != http.StatusCreated {
		t.Errorf("valid name: code = %d, want 201", rec.Code)
	}
}

func TestLinkApp(t *testing.T) {
	s := newTestStore(t)
	s.CreateApp("blog", 8080)
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil, nil, nil, nil, AgentInfo{})
	body := strings.NewReader(`{"repo":"alice/blog","branch":"main"}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps/blog/link", body))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("code = %d", rec.Code)
	}
	got, _ := s.AppByRepo("alice/blog")
	if got.Name != "blog" || got.Branch != "main" {
		t.Fatalf("link not persisted: %+v", got)
	}
}

func TestLinkAppPersistsRootDir(t *testing.T) {
	s := newTestStore(t)
	s.CreateApp("blog", 8080)
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil, nil, nil, nil, AgentInfo{})
	body := strings.NewReader(`{"repo":"alice/blog","branch":"main","root_dir":"apps/web"}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps/blog/link", body))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("code = %d", rec.Code)
	}
	got, _ := s.AppByRepo("alice/blog")
	if got.RootDir != "apps/web" {
		t.Fatalf("root_dir not persisted: %+v", got)
	}
}

func TestLinkAppRejectsEscapingRootDir(t *testing.T) {
	s := newTestStore(t)
	s.CreateApp("blog", 8080)
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil, nil, nil, nil, AgentInfo{})
	for _, bad := range []string{`"../etc"`, `"apps/../../etc"`, `"/abs/path"`} {
		body := strings.NewReader(`{"repo":"alice/blog","branch":"main","root_dir":` + bad + `}`)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps/blog/link", body))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("root_dir %s: code = %d, want 400", bad, rec.Code)
		}
	}
}

// A repo without an owner/ prefix is accepted by nothing downstream — the
// relay's installation match, webhook full_name routing and tarball fetch all
// assume owner/name — so the link boundary must reject it rather than store a
// binding that silently deploys nowhere (#333).
func TestLinkAppRejectsRepoWithoutOwner(t *testing.T) {
	s := newTestStore(t)
	s.CreateApp("blog", 8080)
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil, nil, nil, nil, AgentInfo{})
	for _, bad := range []string{"next", "next/", "/next", "octo/blog/extra"} {
		body := strings.NewReader(`{"repo":"` + bad + `","branch":"main"}`)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps/blog/link", body))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("repo %q: code = %d, want 400", bad, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "owner/name") {
			t.Errorf("repo %q: error %q does not name the expected shape", bad, rec.Body.String())
		}
		if _, err := s.GetApp("blog"); err == nil {
			if got, _ := s.GetApp("blog"); got.Repo != "" {
				t.Errorf("repo %q: link was persisted as %q", bad, got.Repo)
			}
		}
	}
}

// A rejected link must not have reached the relay either: a binding for a repo
// nothing can match is worse than no binding at all.
func TestLinkAppRejectsBadRepoBeforeBinding(t *testing.T) {
	s := newTestStore(t)
	s.CreateApp("blog", 8080)
	fb := &fakeBinder{}
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil, fb, nil, nil, AgentInfo{})
	body := strings.NewReader(`{"repo":"next","branch":"main"}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps/blog/link", body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rec.Code)
	}
	if fb.calls != 0 {
		t.Fatalf("binder called %d times for a rejected link", fb.calls)
	}
}

type fakeBinder struct {
	app, repo, branch string
	calls             int
}

func (f *fakeBinder) BindRepo(app, repo, branch string) error {
	f.app, f.repo, f.branch = app, repo, branch
	f.calls++
	return nil
}

func TestLinkRegistersBindingWithRelay(t *testing.T) {
	s := newTestStore(t)
	s.CreateApp("blog", 8080)
	fb := &fakeBinder{}
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil, fb, nil, nil, AgentInfo{})
	body := strings.NewReader(`{"repo":"alice/blog","branch":"main"}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps/blog/link", body))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("code = %d", rec.Code)
	}
	if fb.calls != 1 || fb.app != "blog" || fb.repo != "alice/blog" || fb.branch != "main" {
		t.Fatalf("binder got %+v", fb)
	}
}

func TestLinkSucceedsWithoutABinder(t *testing.T) {
	s := newTestStore(t)
	s.CreateApp("blog", 8080)
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil, nil, nil, nil, AgentInfo{})
	body := strings.NewReader(`{"repo":"alice/blog","branch":"main"}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps/blog/link", body))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("code = %d", rec.Code)
	}
	got, _ := s.AppByRepo("alice/blog")
	if got.Name != "blog" || got.Branch != "main" {
		t.Fatalf("link not persisted: %+v", got)
	}
}

func TestManifestEndpoint(t *testing.T) {
	s := newTestStore(t)
	h := New(s, &fakeDeployer{store: s}, "alice.dev", "", nil, nil, nil, nil, nil, AgentInfo{})
	body := strings.NewReader(`{"redirect_url":"http://localhost:5000/cb"}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/github/manifest", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "hooks.alice.dev") {
		t.Fatalf("manifest missing webhook host: %s", rec.Body.String())
	}
}

func TestExchangeSavesCredsAndInvokesCallback(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app-manifests/thecode/conversions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"id":42,"slug":"piper-abc","pem":"KEY","webhook_secret":"SEKRIT"}`)
	}))
	defer gh.Close()

	s := newTestStore(t)
	called := false
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", gh.URL, func() { called = true }, nil, nil, nil, nil, AgentInfo{})

	rec := httptest.NewRecorder()
	body := strings.NewReader(`{"code":"thecode"}`)
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/github/exchange", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode exchange response: %v", err)
	}
	if out.Slug != "piper-abc" {
		t.Fatalf("slug = %q, want piper-abc", out.Slug)
	}
	if !called {
		t.Fatal("onGitHubApp callback was not invoked after exchange")
	}
	saved, err := s.GetGitHubApp()
	if err != nil {
		t.Fatalf("GetGitHubApp: %v", err)
	}
	if saved.AppID != 42 || saved.Slug != "piper-abc" || saved.WebhookSecret != "SEKRIT" {
		t.Fatalf("creds not persisted: %+v", saved)
	}
}

// TestGitHubStatus covers the read-only status endpoint the dashboard gates its
// Connect step on: unconfigured reports configured:false, and a stored App
// reports its id and slug so the dashboard can deep-link the install page.
func TestGitHubStatus(t *testing.T) {
	s := newTestStore(t)
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil, nil, nil, nil, AgentInfo{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/github", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var before struct {
		Configured bool   `json:"configured"`
		Slug       string `json:"slug"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &before); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if before.Configured {
		t.Fatalf("configured = true with no App stored: %s", rec.Body.String())
	}

	if err := s.SaveGitHubApp(store.GitHubApp{AppID: 42, Slug: "piper-abc", PrivateKey: "KEY", WebhookSecret: "SEKRIT"}); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/github", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	var after struct {
		Configured bool   `json:"configured"`
		AppID      int64  `json:"app_id"`
		Slug       string `json:"slug"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &after); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !after.Configured || after.AppID != 42 || after.Slug != "piper-abc" {
		t.Fatalf("status = %+v", after)
	}
}

// TestResetClearsStoredApp covers the escape hatch from BYO (#299): the row a
// box has kept since `piper github setup` is what shadows a relay's brokered
// App, and reset is the only supported way to drop it. The response names the
// provider the box will use once restarted, so the operator does not have to
// read the log to find out whether anything replaced it.
func TestResetClearsStoredApp(t *testing.T) {
	s := newTestStore(t)
	if err := s.SaveGitHubApp(store.GitHubApp{AppID: 42, PrivateKey: "KEY", WebhookSecret: "SEKRIT"}); err != nil {
		t.Fatal(err)
	}
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil, nil,
		func() string { return "brokered" }, nil, AgentInfo{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/github/app", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Provider string `json:"provider"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Provider != "brokered" {
		t.Fatalf("provider = %q, want brokered", out.Provider)
	}
	if _, err := s.GetGitHubApp(); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("app still stored: %v", err)
	}
}

// TestResetWithNoStoredAppIsNotAnError keeps reset idempotent: an operator
// running it on a box that never went BYO gets the same answer as one that did.
func TestResetWithNoStoredAppIsNotAnError(t *testing.T) {
	s := newTestStore(t)
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil, nil, nil, nil, AgentInfo{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/github/app", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestUntarRejectsPathTraversal(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "destination")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	var body bytes.Buffer
	tw := tar.NewWriter(&body)
	contents := []byte("escaped")
	if err := tw.WriteHeader(&tar.Header{Name: "../escape", Mode: 0o644, Size: int64(len(contents))}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if _, err := tw.Write(contents); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("Close tar: %v", err)
	}

	if err := untar(&body, dir); err == nil {
		t.Fatal("untar returned nil, want path traversal error")
	}
	if _, err := os.Stat(filepath.Join(parent, "escape")); !os.IsNotExist(err) {
		t.Errorf("escape file exists or stat failed: %v", err)
	}
}

func TestListAppsIncludesDeployStatus(t *testing.T) {
	s := newTestStore(t)
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil, nil, nil, nil, AgentInfo{})
	if _, err := s.CreateApp("api", 3000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDeployment("blog", "img1", "c1", 40001, "running", ""); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/apps", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var apps []App
	if err := json.NewDecoder(rr.Body).Decode(&apps); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// ListApps orders by name: api, blog.
	if len(apps) != 2 || apps[0].Name != "api" || apps[0].Status != "" {
		t.Errorf("apps[0] = %+v, want api with empty status", apps)
	}
	if apps[1].Name != "blog" || apps[1].Status != "running" {
		t.Errorf("apps[1] = %+v, want blog running", apps)
	}
}

func TestGetAppIncludesDeployStatus(t *testing.T) {
	s := newTestStore(t)
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil, nil, nil, nil, AgentInfo{})
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDeployment("blog", "img1", "c1", 40001, "failed", ""); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/apps/blog", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var app App
	if err := json.NewDecoder(rr.Body).Decode(&app); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if app.Name != "blog" || app.Status != "failed" {
		t.Errorf("app = %+v, want blog failed", app)
	}
}

func TestAppStatusStaysRunningWhenFailedRedeployLeavesOldVersionServing(t *testing.T) {
	s := newTestStore(t)
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil, nil, nil, nil, AgentInfo{})
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDeployment("blog", "img1", "c1", 40001, "running", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDeployment("blog", "img2", "c2", 40002, "failed", ""); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/apps/blog", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var app App
	if err := json.NewDecoder(rr.Body).Decode(&app); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if app.Name != "blog" || app.Status != "running" {
		t.Errorf("app = %+v, want blog running (old version still serving)", app)
	}
}

func TestListDeploymentsEndpoint(t *testing.T) {
	s := newTestStore(t)
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil, nil, nil, nil, AgentInfo{})
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDeployment("blog", "img1", "c1", 40001, "failed", "boom"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDeployment("blog", "img2", "c2", 40002, "running", "ok"); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/apps/blog/deployments", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var deps []store.Deployment
	if err := json.NewDecoder(rr.Body).Decode(&deps); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(deps) != 2 || deps[0].ImageID != "img2" || deps[1].Status != "failed" {
		t.Errorf("deps = %+v, want [img2 running, img1 failed]", deps)
	}

	missing := httptest.NewRecorder()
	h.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/v1/apps/nope/deployments", nil))
	if missing.Code != http.StatusNotFound {
		t.Errorf("unknown app status = %d, want 404", missing.Code)
	}
}

// The whole point of #478: a preview's hostname has to cross the HTTP boundary
// under the name clients read it by, or the dashboard knows a deployment is a
// preview but has nothing to link to. Asserted on the raw body, since the
// serialised key is the contract — decoding into store.Deployment would pass
// even if the field were renamed on the wire.
func TestListDeploymentsEndpointCarriesPreviewHostname(t *testing.T) {
	s := newTestStore(t)
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil, nil, nil, nil, AgentInfo{})
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePreviewDeployment("blog", 42, "img", "pr42-c", 41000, "running", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPreviewHostname("blog", 42, "pr42-855d1432-ozykhan.relay.example"); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/apps/blog/deployments", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var raw []map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(raw) != 1 {
		t.Fatalf("deps = %v, want 1", raw)
	}
	if got := raw[0]["Hostname"]; got != "pr42-855d1432-ozykhan.relay.example" {
		t.Errorf("Hostname = %v, want the preview URL; body = %s", got, rr.Body.String())
	}
}

func TestDeploymentLogsEndpoint(t *testing.T) {
	s := newTestStore(t)
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil, nil, nil, nil, AgentInfo{})
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateApp("api", 3000); err != nil {
		t.Fatal(err)
	}
	dep, err := s.CreateDeployment("blog", "img1", "c1", 40001, "failed", "Step 1/2\nboom\n")
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/apps/blog/deployments/"+dep.ID+"/logs", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	if !strings.Contains(rr.Body.String(), "boom") {
		t.Errorf("body = %q, want build output", rr.Body.String())
	}

	// The same deployment id under a different app must 404.
	crossApp := httptest.NewRecorder()
	h.ServeHTTP(crossApp, httptest.NewRequest(http.MethodGet, "/v1/apps/api/deployments/"+dep.ID+"/logs", nil))
	if crossApp.Code != http.StatusNotFound {
		t.Errorf("cross-app status = %d, want 404", crossApp.Code)
	}
	unknown := httptest.NewRecorder()
	h.ServeHTTP(unknown, httptest.NewRequest(http.MethodGet, "/v1/apps/blog/deployments/no-such-id/logs", nil))
	if unknown.Code != http.StatusNotFound {
		t.Errorf("unknown id status = %d, want 404", unknown.Code)
	}

	blank, err := s.CreateDeployment("blog", "img0", "c0", 40000, "failed", "")
	if err != nil {
		t.Fatalf("CreateDeployment blank logs: %v", err)
	}
	blankRec := httptest.NewRecorder()
	h.ServeHTTP(blankRec, httptest.NewRequest(http.MethodGet, "/v1/apps/blog/deployments/"+blank.ID+"/logs", nil))
	if blankRec.Code != http.StatusOK {
		t.Fatalf("blank log status = %d, want 200", blankRec.Code)
	}
	if ct := blankRec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("blank log Content-Type = %q, want text/plain", ct)
	}
	if blankRec.Body.String() != "" {
		t.Errorf("blank log body = %q, want empty", blankRec.Body.String())
	}
}

type fakeDomainManager struct {
	status  domain.Status
	setErr  error
	gotSet  []string
	removed bool

	appStatuses    []domain.AppDomainStatus
	addErr         error
	added          []string // "app:domain"
	removeAppErr   error
	removedDomains []string
}

func (f *fakeDomainManager) Set(d, p, tok string) (domain.Status, error) {
	f.gotSet = []string{d, p, tok}
	if f.setErr != nil {
		return domain.Status{}, f.setErr
	}
	return f.status, nil
}
func (f *fakeDomainManager) Status() (domain.Status, error) { return f.status, nil }
func (f *fakeDomainManager) Remove() error                  { f.removed = true; return nil }

func (f *fakeDomainManager) AddAppDomain(app, d string) (store.AppDomain, error) {
	if f.addErr != nil {
		return store.AppDomain{}, f.addErr
	}
	f.added = append(f.added, app+":"+d)
	return store.AppDomain{Domain: d, App: app, Status: "pending"}, nil
}

func (f *fakeDomainManager) RemoveAppDomain(d string) error {
	if f.removeAppErr != nil {
		return f.removeAppErr
	}
	f.removedDomains = append(f.removedDomains, d)
	return nil
}

func (f *fakeDomainManager) AppDomainStatus(d string) (domain.AppDomainStatus, error) {
	for _, st := range f.appStatuses {
		if st.Domain == d {
			return st, nil
		}
	}
	return domain.AppDomainStatus{Domain: d, Status: "pending",
		DNSRecords: []domain.DNSRecord{{Type: "CNAME", Name: d, Value: "relay.example.net"}}}, nil
}

func (f *fakeDomainManager) AppDomainStatuses(app string) ([]domain.AppDomainStatus, error) {
	return f.appStatuses, nil
}

func TestDomainEndpoints(t *testing.T) {
	fdm := &fakeDomainManager{status: domain.Status{
		Domain: "shop.dev", DNSProvider: "cloudflare", DNSTokenSet: true,
		Source: "api", Status: "issuing",
		DNSRecords: []domain.DNSRecord{{Type: "CNAME", Name: "*.shop.dev", Value: "relay.example.net"}},
	}}
	s := newTestStore(t)
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, fdm, nil, nil, nil, AgentInfo{})

	// PUT kicks Set with the body fields.
	put := httptest.NewRequest(http.MethodPut, "/v1/domain",
		strings.NewReader(`{"domain":"shop.dev","dns_provider":"cloudflare","dns_token":"cf-tok"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, put)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body %s", rec.Code, rec.Body.String())
	}
	if len(fdm.gotSet) != 3 || fdm.gotSet[0] != "shop.dev" || fdm.gotSet[2] != "cf-tok" {
		t.Fatalf("Set called with %v", fdm.gotSet)
	}

	// GET returns the status; the token value must never appear.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/domain", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"dns_token_set":true`) {
		t.Fatalf("GET body missing dns_token_set: %s", body)
	}
	if strings.Contains(body, `"dns_token":`) || strings.Contains(body, "cf-tok") {
		t.Fatalf("GET leaks the dns token: %s", body)
	}

	// DELETE removes.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/domain", nil))
	if rec.Code != http.StatusNoContent || !fdm.removed {
		t.Fatalf("DELETE = %d, removed = %v", rec.Code, fdm.removed)
	}
}

func TestDomainEndpointErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"env-managed", domain.ErrEnvManaged, http.StatusConflict},
		{"invalid domain", domain.ErrInvalidDomain, http.StatusBadRequest},
		{"bad provider", domain.ErrUnsupportedProvider, http.StatusBadRequest},
		{"empty token", domain.ErrTokenRequired, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil,
				&fakeDomainManager{setErr: tc.err}, nil, nil, nil, AgentInfo{})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/domain",
				strings.NewReader(`{"domain":"x.dev","dns_provider":"cloudflare","dns_token":"t"}`)))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestDomainEndpointsWithoutRelay(t *testing.T) {
	s := newTestStore(t)
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil, nil, nil, nil, AgentInfo{})
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(m, "/v1/domain", strings.NewReader(`{}`)))
		if rec.Code != http.StatusConflict {
			t.Fatalf("%s without relay = %d, want 409", m, rec.Code)
		}
	}
}

// The per-app domains collection (#231): POST attaches and returns the fresh
// wire status, GET lists the per-domain status shape, DELETE tears down.
func TestAppDomainsEndpoints(t *testing.T) {
	notAfter := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	fdm := &fakeDomainManager{appStatuses: []domain.AppDomainStatus{{
		Domain: "myshop.com", App: "blog", Status: "active", Error: "",
		CertNotAfter: &notAfter,
		DNSRecords:   []domain.DNSRecord{{Type: "CNAME", Name: "myshop.com", Value: "relay.example.net"}},
		DNSOK:        true,
	}}}
	s := newTestStore(t)
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, fdm, nil, nil, nil, AgentInfo{})
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}

	// POST kicks AddAppDomain and answers 201 with the domain's wire status.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps/blog/domains",
		strings.NewReader(`{"domain":"myshop.com"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST = %d, body %s", rec.Code, rec.Body.String())
	}
	if len(fdm.added) != 1 || fdm.added[0] != "blog:myshop.com" {
		t.Fatalf("AddAppDomain called with %v", fdm.added)
	}
	var created domain.AppDomainStatus
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode POST body: %v", err)
	}
	if created.Domain != "myshop.com" {
		t.Fatalf("POST body = %+v", created)
	}

	// GET lists the per-domain status shape.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/apps/blog/domains", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`"status":"active"`, `"cert_not_after"`, `"dns_ok":true`,
		`"dns_records":[{"type":"CNAME","name":"myshop.com","value":"relay.example.net"}]`} {
		if !strings.Contains(body, want) {
			t.Errorf("GET body missing %s: %s", want, body)
		}
	}

	// DELETE tears down through the manager. The row must exist in the store
	// and belong to this app for the handler to route the removal.
	if err := s.AddAppDomain("myshop.com", "blog"); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/apps/blog/domains/myshop.com", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d, body %s", rec.Code, rec.Body.String())
	}
	if len(fdm.removedDomains) != 1 || fdm.removedDomains[0] != "myshop.com" {
		t.Fatalf("RemoveAppDomain called with %v", fdm.removedDomains)
	}
}

// GET on an app with no domains serves JSON [] — never null.
func TestAppDomainsListEmptyReturnsArray(t *testing.T) {
	s := newTestStore(t)
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, &fakeDomainManager{}, nil, nil, nil, AgentInfo{})
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/apps/blog/domains", nil))
	if body := strings.TrimSpace(rec.Body.String()); body != "[]" {
		t.Errorf("body = %s, want []", body)
	}
}

func TestAppDomainsPostErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"invalid domain", domain.ErrInvalidDomain, http.StatusBadRequest},
		{"box-wide collision", domain.ErrBoxWideDomain, http.StatusConflict},
		{"already attached", store.ErrDomainExists, http.StatusConflict},
		{"unknown app", store.ErrNotFound, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil,
				&fakeDomainManager{addErr: tc.err}, nil, nil, nil, AgentInfo{})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps/blog/domains",
				strings.NewReader(`{"domain":"myshop.com"}`)))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}

	// Malformed / empty body never reaches the manager.
	s := newTestStore(t)
	fdm := &fakeDomainManager{}
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, fdm, nil, nil, nil, AgentInfo{})
	for _, body := range []string{`{`, `{}`, `{"domain":""}`} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps/blog/domains",
			strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, rec.Code)
		}
	}
	if len(fdm.added) != 0 {
		t.Errorf("manager reached on bad body: %v", fdm.added)
	}
}

func TestAppDomainsUnknownAppAndDomain(t *testing.T) {
	s := newTestStore(t)
	fdm := &fakeDomainManager{}
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, fdm, nil, nil, nil, AgentInfo{})
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateApp("api", 3000); err != nil {
		t.Fatal(err)
	}
	if err := s.AddAppDomain("owned.example.com", "api"); err != nil {
		t.Fatal(err)
	}

	// GET unknown app.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/apps/ghost/domains", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET ghost = %d, want 404", rec.Code)
	}
	// DELETE unknown app.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/apps/ghost/domains/x.com", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("DELETE ghost app = %d, want 404", rec.Code)
	}
	// DELETE unknown domain.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/apps/blog/domains/x.com", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("DELETE unknown domain = %d, want 404", rec.Code)
	}
	// DELETE a domain owned by a different app.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/apps/blog/domains/owned.example.com", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("DELETE cross-app domain = %d, want 404", rec.Code)
	}
	if len(fdm.removedDomains) != 0 {
		t.Errorf("RemoveAppDomain reached: %v", fdm.removedDomains)
	}
}

// No relay configured (nil manager): the collection answers 409, like /v1/domain.
func TestAppDomainsWithoutRelay(t *testing.T) {
	s := newTestStore(t)
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil, nil, nil, nil, AgentInfo{})
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/v1/apps/blog/domains", nil),
		httptest.NewRequest(http.MethodPost, "/v1/apps/blog/domains", strings.NewReader(`{"domain":"a.com"}`)),
		httptest.NewRequest(http.MethodDelete, "/v1/apps/blog/domains/a.com", nil),
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusConflict {
			t.Errorf("%s without relay = %d, want 409", req.Method, rec.Code)
		}
	}
}

// #267: deleting an app tears down its per-app custom domains through the
// manager before the deploy delete — otherwise the relay claim, loaded cert,
// and cert dir leak (the store cascade only removes the rows).
func TestDeleteAppTearsDownAppDomains(t *testing.T) {
	s := newTestStore(t)
	deployer := &fakeDeployer{store: s}
	fdm := &fakeDomainManager{}
	h := New(s, deployer, "piper.localhost", "", nil, fdm, nil, nil, nil, AgentInfo{})
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateApp("api", 3000); err != nil {
		t.Fatal(err)
	}
	for _, d := range []struct{ dom, app string }{
		{"a.example.com", "blog"}, {"b.example.com", "blog"}, {"other.example.com", "api"},
	} {
		if err := s.AddAppDomain(d.dom, d.app); err != nil {
			t.Fatal(err)
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/apps/blog", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE app = %d, body %s", rec.Code, rec.Body.String())
	}
	if len(fdm.removedDomains) != 2 || fdm.removedDomains[0] != "a.example.com" || fdm.removedDomains[1] != "b.example.com" {
		t.Fatalf("removed domains = %v, want blog's two", fdm.removedDomains)
	}
	if len(deployer.deleted) != 1 || deployer.deleted[0] != "blog" {
		t.Fatalf("deploy delete = %v, want [blog]", deployer.deleted)
	}
}

// #267: a mandatory relay-removal failure aborts the app delete (500), leaving
// it retryable — the deploy delete must not run and cascade the rows away.
func TestDeleteAppAbortsOnDomainTeardownFailure(t *testing.T) {
	s := newTestStore(t)
	deployer := &fakeDeployer{store: s}
	fdm := &fakeDomainManager{removeAppErr: errors.New("clear relay domain mapping: tunnel down")}
	h := New(s, deployer, "piper.localhost", "", nil, fdm, nil, nil, nil, AgentInfo{})
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if err := s.AddAppDomain("a.example.com", "blog"); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/apps/blog", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("DELETE app = %d, want 500", rec.Code)
	}
	if len(deployer.deleted) != 0 {
		t.Fatalf("deploy delete ran despite teardown failure: %v", deployer.deleted)
	}
	if _, err := s.GetAppDomain("a.example.com"); err != nil {
		t.Fatalf("domain row gone after aborted delete: %v", err)
	}
}

func TestStopEndpoint(t *testing.T) {
	s := newTestStore(t)
	deployer := &fakeDeployer{store: s}
	h := New(s, deployer, "piper.localhost", "", nil, nil, nil, nil, nil, AgentInfo{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps/blog/stop", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(deployer.stopped) != 1 || deployer.stopped[0] != "blog" {
		t.Fatalf("stopped = %v, want [blog]", deployer.stopped)
	}
}

func TestStopEndpointUnknownApp(t *testing.T) {
	s := newTestStore(t)
	h := New(s, &fakeDeployer{store: s, stopErr: store.ErrNotFound}, "piper.localhost", "", nil, nil, nil, nil, nil, AgentInfo{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps/ghost/stop", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestStartEndpoint(t *testing.T) {
	s := newTestStore(t)
	deployer := &fakeDeployer{store: s}
	h := New(s, deployer, "piper.localhost", "", nil, nil, nil, nil, nil, AgentInfo{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps/blog/start", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(deployer.started) != 1 || deployer.started[0] != "blog" {
		t.Fatalf("started = %v, want [blog]", deployer.started)
	}
}

func TestStartEndpointUnknownApp(t *testing.T) {
	s := newTestStore(t)
	h := New(s, &fakeDeployer{store: s, startErr: store.ErrNotFound}, "piper.localhost", "", nil, nil, nil, nil, nil, AgentInfo{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps/ghost/start", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// A non-ErrNotFound deployer error maps to 500 (not 404): the start handler must
// distinguish "unknown app" from a real backend failure.
func TestStartEndpointServerError(t *testing.T) {
	s := newTestStore(t)
	h := New(s, &fakeDeployer{store: s, startErr: errors.New("start failed")}, "piper.localhost", "", nil, nil, nil, nil, nil, AgentInfo{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps/blog/start", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestDeleteAppEndpoint(t *testing.T) {
	s := newTestStore(t)
	deployer := &fakeDeployer{store: s}
	h := New(s, deployer, "piper.localhost", "", nil, nil, nil, nil, nil, AgentInfo{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/apps/blog", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(deployer.deleted) != 1 || deployer.deleted[0] != "blog" {
		t.Fatalf("deleted = %v, want [blog]", deployer.deleted)
	}
}

func TestDeleteAppEndpointUnknownApp(t *testing.T) {
	s := newTestStore(t)
	h := New(s, &fakeDeployer{store: s, deleteErr: store.ErrNotFound}, "piper.localhost", "", nil, nil, nil, nil, nil, AgentInfo{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/apps/ghost", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// A non-ErrNotFound deployer error maps to 500 (not 404): the stop handler must
// distinguish "unknown app" from a real backend failure.
func TestStopEndpointServerError(t *testing.T) {
	s := newTestStore(t)
	h := New(s, &fakeDeployer{store: s, stopErr: errors.New("stop failed")}, "piper.localhost", "", nil, nil, nil, nil, nil, AgentInfo{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps/blog/stop", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// The delete handler's 500 path: a non-ErrNotFound deployer error is not a 404.
func TestDeleteAppEndpointServerError(t *testing.T) {
	s := newTestStore(t)
	h := New(s, &fakeDeployer{store: s, deleteErr: errors.New("delete failed")}, "piper.localhost", "", nil, nil, nil, nil, nil, AgentInfo{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/apps/blog", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// A 500 must not echo the raw internal error (container IDs, Caddy admin URLs,
// file paths) to the caller — the control API is reachable remotely through the
// relay proxy. #122.
func TestServerErrorDoesNotLeakInternalDetail(t *testing.T) {
	s := newTestStore(t)
	leak := errors.New("unroute: caddy admin http://127.0.0.1:2019 failed stopping container abc123def /var/lib/piper/state")
	h := New(s, &fakeDeployer{store: s, stopErr: leak}, "piper.localhost", "", nil, nil, nil, nil, nil, AgentInfo{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps/blog/stop", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	for _, secret := range []string{"caddy", "2019", "abc123def", "/var/lib/piper", "unroute"} {
		if strings.Contains(body, secret) {
			t.Errorf("500 body leaked %q: %s", secret, body)
		}
	}
	if !strings.Contains(body, "internal server error") {
		t.Errorf("body = %q, want a generic message", strings.TrimSpace(body))
	}
}

func TestAppEnvCRUD(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil, nil, nil, nil, AgentInfo{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps/blog/env",
		strings.NewReader(`{"key":"SESSION_SECRET","value":"s3cret"}`)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("post code = %d, body %s", rec.Code, rec.Body)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/apps/blog/env", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("get code = %d", rec.Code)
	}
	var out struct {
		Env map[string]string `json:"env"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Env["SESSION_SECRET"] != "s3cret" {
		t.Errorf("env = %v", out.Env)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/apps/blog/env/SESSION_SECRET", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete code = %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/apps/blog/env", nil))
	// Fresh struct: decoding into a populated map merges instead of clearing.
	var after struct {
		Env map[string]string `json:"env"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&after); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if after.Env == nil || len(after.Env) != 0 {
		t.Errorf("env after delete = %v, want empty non-null object", after.Env)
	}
}

// TestAppEnvEndpointCarriesUpdatedAtMap proves the wire shape is purely
// additive: the existing "env" map is unchanged and a parallel "updated_at"
// map appears beside it.
func TestAppEnvEndpointCarriesUpdatedAtMap(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil, nil, nil, nil, AgentInfo{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apps/blog/env",
		strings.NewReader(`{"key":"A","value":"one"}`)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("post code = %d, body %s", rec.Code, rec.Body)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/apps/blog/env", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("get code = %d, body %s", rec.Code, rec.Body)
	}
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	envMap, ok := raw["env"].(map[string]any)
	if !ok || len(envMap) != 1 || envMap["A"] != "one" {
		t.Errorf("env map = %v, want map[A:one]", raw["env"])
	}
	updated, ok := raw["updated_at"].(map[string]any)
	if !ok {
		t.Fatalf("updated_at missing or not an object: %v", raw["updated_at"])
	}
	if ts, ok := updated["A"].(string); !ok || ts == "" {
		t.Errorf("updated_at[A] = %v, want non-empty timestamp string", updated["A"])
	}
}

func TestAppEnvRejects(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	h := New(s, &fakeDeployer{store: s}, "piper.localhost", "", nil, nil, nil, nil, nil, AgentInfo{})

	for _, tc := range []struct {
		name, path, body string
		want             int
	}{
		{"unknown app", "/v1/apps/ghost/env", `{"key":"A","value":"b"}`, http.StatusNotFound},
		{"bad key", "/v1/apps/blog/env", `{"key":"9BAD-KEY","value":"b"}`, http.StatusBadRequest},
		{"reserved PORT", "/v1/apps/blog/env", `{"key":"port","value":"1"}`, http.StatusBadRequest},
		{"empty key", "/v1/apps/blog/env", `{"value":"b"}`, http.StatusBadRequest},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)))
		if rec.Code != tc.want {
			t.Errorf("%s: code = %d, want %d", tc.name, rec.Code, tc.want)
		}
	}
}
