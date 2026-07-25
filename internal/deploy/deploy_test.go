package deploy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/piperbox/piper/internal/runtime"
	"github.com/piperbox/piper/internal/store"
)

type fakeCaddy struct {
	upserts   map[string]int
	removes   []string
	tlsRoutes []string
	removeErr error
	upsertErr error
	tlsErr    error
}

func newFakeCaddy() *fakeCaddy {
	return &fakeCaddy{upserts: make(map[string]int)}
}

func (f *fakeCaddy) UpsertRoute(host string, port int) error {
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.upserts[host] = port
	return nil
}

func (f *fakeCaddy) UpsertRouteTLS(host string, port int) error {
	if f.tlsErr != nil {
		return f.tlsErr
	}
	f.tlsRoutes = append(f.tlsRoutes, fmt.Sprintf("%s->%d", host, port))
	return nil
}

func (f *fakeCaddy) RemoveRoute(host string) error {
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removes = append(f.removes, host)
	return nil
}

func (f *fakeCaddy) removed() []string { return f.removes }

func newStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "deploy.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	return s, path
}

func deploymentCountWithStatus(t *testing.T, path, status string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("Open database: %v", err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM deployments WHERE status=?`, status).Scan(&count); err != nil {
		t.Fatalf("Count deployments: %v", err)
	}
	return count
}

func TestDeploySuccessRoutesAndRecords(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")

	dep, err := d.Deploy(context.Background(), "blog", t.TempDir())
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if dep.Status != "running" {
		t.Errorf("status = %q, want running", dep.Status)
	}
	if routes.upserts["blog.piper.localhost"] != 40001 {
		t.Errorf("routes = %+v, want blog.piper.localhost -> 40001", routes.upserts)
	}
	got, err := s.LatestRunning("blog")
	if err != nil {
		t.Fatalf("LatestRunning: %v", err)
	}
	if got.ContainerID != "c1" {
		t.Errorf("container ID = %q, want c1", got.ContainerID)
	}
}

func TestDeployBuildsFromRootDir(t *testing.T) {
	s, _ := newStore(t)
	if err := s.UpdateAppRepo("blog", "alice/blog", "main", "apps/web"); err != nil {
		t.Fatalf("UpdateAppRepo: %v", err)
	}
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")

	checkout := t.TempDir()
	if _, err := d.Deploy(context.Background(), "blog", checkout); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	want := filepath.Join(checkout, "apps/web")
	if rt.BuildSrcDir != want {
		t.Fatalf("build context = %q, want %q", rt.BuildSrcDir, want)
	}
}

func TestDeployEmptyRootDirBuildsCheckout(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")

	checkout := t.TempDir()
	if _, err := d.Deploy(context.Background(), "blog", checkout); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if rt.BuildSrcDir != checkout {
		t.Fatalf("build context = %q, want %q (checkout root)", rt.BuildSrcDir, checkout)
	}
}

func TestDeploySecondStopsPrevious(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("first Deploy: %v", err)
	}

	rt.BuildResultVal = runtime.BuildResult{ImageID: "img2"}
	rt.RunResultVal = runtime.RunResult{ContainerID: "c2", HostPort: 40002}
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("second Deploy: %v", err)
	}

	if len(rt.Stopped) != 1 || rt.Stopped[0] != "c1" {
		t.Errorf("stopped = %v, want [c1]", rt.Stopped)
	}
	got, err := s.LatestRunning("blog")
	if err != nil {
		t.Fatalf("LatestRunning: %v", err)
	}
	if got.ContainerID != "c2" {
		t.Errorf("container ID = %q, want c2", got.ContainerID)
	}
}

func TestDeployHealthFailureStopsContainerAndRecordsFailed(t *testing.T) {
	s, path := newStore(t)
	healthErr := errors.New("unhealthy")
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
		HealthErr:      healthErr,
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")

	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); !errors.Is(err, healthErr) {
		t.Fatalf("Deploy error = %v, want health error", err)
	}
	if len(rt.Stopped) != 1 || rt.Stopped[0] != "c1" {
		t.Errorf("stopped = %v, want [c1]", rt.Stopped)
	}
	if got := deploymentCountWithStatus(t, path, "failed"); got != 1 {
		t.Errorf("failed deployment count = %d, want 1", got)
	}
	if _, err := s.LatestRunning("blog"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("LatestRunning error = %v, want ErrNotFound", err)
	}
}

func TestDeployBuildFailureRecordsFailed(t *testing.T) {
	s, path := newStore(t)
	buildErr := errors.New("build failed")
	rt := &runtime.FakeRuntime{BuildErr: buildErr}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")

	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); !errors.Is(err, buildErr) {
		t.Fatalf("Deploy error = %v, want build error", err)
	}
	if got := deploymentCountWithStatus(t, path, "failed"); got != 1 {
		t.Errorf("failed deployment count = %d, want 1", got)
	}
}

func TestDeployRunFailureRecordsFailed(t *testing.T) {
	s, path := newStore(t)
	runErr := errors.New("run failed")
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunErr:         runErr,
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")

	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); !errors.Is(err, runErr) {
		t.Fatalf("Deploy error = %v, want run error", err)
	}
	if got := deploymentCountWithStatus(t, path, "failed"); got != 1 {
		t.Errorf("failed deployment count = %d, want 1", got)
	}
}

func TestDeployCancelledRunCleansPartialContainerWithLiveContext(t *testing.T) {
	s, path := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "partial-c"},
		RunErr:         context.Canceled,
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")

	if _, err := d.Deploy(ctx, "blog", t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Deploy error = %v, want context.Canceled", err)
	}
	if len(rt.Stopped) != 1 || rt.Stopped[0] != "partial-c" {
		t.Fatalf("stopped = %v, want [partial-c]", rt.Stopped)
	}
	if len(rt.StopContextErrs) != 1 || rt.StopContextErrs[0] != nil {
		t.Fatalf("stop context errors = %v, want [nil]", rt.StopContextErrs)
	}
	if got := deploymentCountWithStatus(t, path, "failed"); got != 1 {
		t.Fatalf("failed deployment count = %d, want 1", got)
	}
}

func TestDeployPreviewRoutesFlattenedHostAndKeepsMain(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "main-c", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")

	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	rt.RunResultVal = runtime.RunResult{ContainerID: "preview-c", HostPort: 40002}
	if _, err := d.DeployPreview(context.Background(), "blog", 5, t.TempDir()); err != nil {
		t.Fatalf("DeployPreview: %v", err)
	}

	if routes.upserts["pr-5-blog.piper.localhost"] != 40002 {
		t.Errorf("routes = %+v, want pr-5-blog.piper.localhost -> 40002", routes.upserts)
	}
	if len(rt.Stopped) != 0 {
		t.Errorf("stopped = %v, want none (main must survive)", rt.Stopped)
	}
	main, err := s.LatestRunning("blog")
	if err != nil || main.ContainerID != "main-c" {
		t.Errorf("main running = %+v (err %v), want main-c", main, err)
	}
}

func TestDeployPreviewSecondStopsPreviousPreview(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "p1", HostPort: 40001},
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")
	if _, err := d.DeployPreview(context.Background(), "blog", 5, t.TempDir()); err != nil {
		t.Fatalf("first DeployPreview: %v", err)
	}
	rt.RunResultVal = runtime.RunResult{ContainerID: "p2", HostPort: 40002}
	if _, err := d.DeployPreview(context.Background(), "blog", 5, t.TempDir()); err != nil {
		t.Fatalf("second DeployPreview: %v", err)
	}
	if len(rt.Stopped) != 1 || rt.Stopped[0] != "p1" {
		t.Errorf("stopped = %v, want [p1]", rt.Stopped)
	}
}

func TestTeardownPreviewStopsAndUnroutes(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "p1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")
	if _, err := d.DeployPreview(context.Background(), "blog", 5, t.TempDir()); err != nil {
		t.Fatalf("DeployPreview: %v", err)
	}

	retired, err := d.TeardownPreview(context.Background(), "blog", 5)
	if err != nil {
		t.Fatalf("TeardownPreview: %v", err)
	}
	if !retired {
		t.Errorf("retired = false, want true (a running preview was torn down)")
	}
	if len(rt.Stopped) != 1 || rt.Stopped[0] != "p1" {
		t.Errorf("stopped = %v, want [p1]", rt.Stopped)
	}
	if len(routes.removed()) != 1 || routes.removed()[0] != "pr-5-blog.piper.localhost" {
		t.Errorf("removed = %v, want [pr-5-blog.piper.localhost]", routes.removed())
	}
	if _, err := s.PreviewRunning("blog", 5); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("PreviewRunning after teardown err = %v, want ErrNotFound", err)
	}
}

func TestTeardownPreviewNoRunningIsNoOp(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")
	retired, err := d.TeardownPreview(context.Background(), "blog", 99)
	if err != nil {
		t.Fatalf("TeardownPreview no-op err = %v, want nil", err)
	}
	if retired {
		t.Errorf("retired = true, want false (nothing was running)")
	}
}

// A route can outlive its store row (e.g. a crash between UpsertRoute and the
// row write). TeardownPreview must still attempt RemoveRoute when PreviewRunning
// reports ErrNotFound, so the orphaned route can't leak.
func TestTeardownPreviewRemovesOrphanRouteWhenRowGone(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")

	// No preview deployment exists for PR 42 (store row absent).
	retired, err := d.TeardownPreview(context.Background(), "blog", 42)
	if err != nil {
		t.Fatalf("TeardownPreview: %v", err)
	}
	if retired {
		t.Errorf("retired = true, want false (no row existed)")
	}
	if len(routes.removed()) != 1 || routes.removed()[0] != "pr-42-blog.piper.localhost" {
		t.Errorf("removed = %v, want [pr-42-blog.piper.localhost]", routes.removed())
	}
}

// When UpsertRoute fails after the container is running, DeployPreview must
// unwind rather than leave a running container and a "running" row with no
// route: stop the container and mark the row "failed".
func TestDeployPreviewUpsertRouteFailureUnwinds(t *testing.T) {
	s, path := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "p1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	routes.upsertErr = errors.New("caddy down")
	d := New(s, rt, routes, "piper.localhost")

	if _, err := d.DeployPreview(context.Background(), "blog", 5, t.TempDir()); err == nil {
		t.Fatal("DeployPreview err = nil, want route error")
	}
	if len(rt.Stopped) != 1 || rt.Stopped[0] != "p1" {
		t.Errorf("stopped = %v, want [p1] (container unwound)", rt.Stopped)
	}
	if n := deploymentCountWithStatus(t, path, "running"); n != 0 {
		t.Errorf("running rows = %d, want 0 (row must not stay running)", n)
	}
	if _, err := s.PreviewRunning("blog", 5); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("PreviewRunning err = %v, want ErrNotFound (no running preview left)", err)
	}
}

type fakeRegistrar struct {
	host    string
	deregs  []string
	failing bool
}

func (f *fakeRegistrar) Register(app string, pr int) (string, error) {
	if f.failing {
		return "", errors.New("quota")
	}
	f.host = "hash-" + app + "-alice.public.getpiper.co"
	if pr > 0 {
		f.host = fmt.Sprintf("pr%d-%s", pr, f.host)
	}
	return f.host, nil
}
func (f *fakeRegistrar) Deregister(hostname string) error {
	f.deregs = append(f.deregs, hostname)
	return nil
}

func TestDeployTerminatedRoutesAssignedHostname(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "public.getpiper.co")
	reg := &fakeRegistrar{}
	d.SetHostnameRegistrar(reg)

	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	// Route must be the relay-assigned single-label host, NOT blog.public.getpiper.co.
	if _, ok := routes.upserts["hash-blog-alice.public.getpiper.co"]; !ok {
		t.Fatalf("routes = %v, want the assigned hostname", routes.upserts)
	}
	if _, ok := routes.upserts["blog.public.getpiper.co"]; ok {
		t.Fatal("terminated deploy must not route <app>.<baseDom>")
	}
}

func TestDeployPersistsRoutedHostname(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}

	// LAN/BYO: the app row records "<app>.<baseDom>".
	d := New(s, rt, newFakeCaddy(), "piper.localhost")
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	app, err := s.GetApp("blog")
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if app.Hostname != "blog.piper.localhost" {
		t.Fatalf("LAN hostname = %q, want blog.piper.localhost", app.Hostname)
	}

	// Relay-terminated: the app row records the relay-assigned hostname.
	dt := New(s, rt, newFakeCaddy(), "public.getpiper.co")
	dt.SetHostnameRegistrar(&fakeRegistrar{})
	if _, err := dt.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("terminated Deploy: %v", err)
	}
	app, err = s.GetApp("blog")
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if app.Hostname != "hash-blog-alice.public.getpiper.co" {
		t.Fatalf("terminated hostname = %q", app.Hostname)
	}
}

func TestDeployTerminatedRegistrarFails(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	d := New(s, rt, newFakeCaddy(), "public.getpiper.co")
	d.SetHostnameRegistrar(&fakeRegistrar{failing: true})
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err == nil {
		t.Fatal("expected deploy to fail when registration fails")
	}
}

func deploymentLog(t *testing.T, s *store.Store, app string) string {
	t.Helper()
	deps, err := s.ListDeployments(app)
	if err != nil || len(deps) == 0 {
		t.Fatalf("ListDeployments: %v (%d rows)", err, len(deps))
	}
	logs, err := s.DeploymentLogs(app, deps[0].ID)
	if err != nil {
		t.Fatalf("DeploymentLogs: %v", err)
	}
	return logs
}

func TestDeployBuildFailurePersistsBuildLog(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{Log: "Step 1/2 : FROM busybox\nboom\n"},
		BuildErr:       errors.New("build failed"),
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")

	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err == nil {
		t.Fatal("expected build error")
	}
	logs := deploymentLog(t, s, "blog")
	if !strings.Contains(logs, "boom") {
		t.Errorf("failed deployment logs = %q, want build output", logs)
	}
}

func TestDeployHealthFailureAppendsContainerOutput(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1", Log: "build ok\n"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
		HealthErr:      errors.New("unhealthy"),
		LogsVal:        "panic: kaboom\n",
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")

	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err == nil {
		t.Fatal("expected health error")
	}
	logs := deploymentLog(t, s, "blog")
	for _, want := range []string{"build ok", "--- container output ---", "panic: kaboom", "unhealthy"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs missing %q:\n%s", want, logs)
		}
	}
	if strings.Index(logs, "build ok") > strings.Index(logs, "container output") {
		t.Error("build log must precede container output")
	}
	if strings.Index(logs, "panic: kaboom") > strings.Index(logs, "unhealthy") {
		t.Error("error text must follow container output")
	}
}

func TestDeployHealthFailureLogsErrDoesNotMaskHealthError(t *testing.T) {
	s, _ := newStore(t)
	healthErr := errors.New("unhealthy")
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1", Log: "build ok\n"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
		HealthErr:      healthErr,
		LogsErr:        errors.New("container logs unavailable"),
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")

	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); !errors.Is(err, healthErr) {
		t.Fatalf("Deploy error = %v, want health error", err)
	}
	logs := deploymentLog(t, s, "blog")
	if !strings.Contains(logs, "build ok") {
		t.Errorf("logs missing build output: %q", logs)
	}
	if strings.Contains(logs, "--- container output ---") {
		t.Errorf("logs must not contain container output separator when Logs fails: %q", logs)
	}
	if !strings.Contains(logs, "unhealthy") {
		t.Errorf("logs missing health error: %q", logs)
	}
}

func TestDeployBuildFailureFromStageRecordsErrorInLog(t *testing.T) {
	s, _ := newStore(t)
	buildErr := errors.New(`The command '/bin/sh -c false' returned a non-zero code: 7`)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{Log: ""},
		BuildErr:       buildErr,
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")

	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err == nil {
		t.Fatal("expected build error")
	}
	logs := deploymentLog(t, s, "blog")
	if !strings.Contains(logs, buildErr.Error()) {
		t.Errorf("failed deployment logs = %q, want build error text", logs)
	}
}

func TestDeploySuccessPersistsBuildLog(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1", Log: "build ok\n"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")

	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	logs := deploymentLog(t, s, "blog")
	if !strings.Contains(logs, "build ok") || strings.Contains(logs, "container output") {
		t.Errorf("logs = %q, want build log present, no container output on success", logs)
	}
}

func TestDeployRoutesCustomDomainWhenActive(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")

	if err := s.SetDomainConfig("shop.dev", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateDomainStatus("shop.dev", "active", "", time.Now().Add(60*24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	want := "blog.shop.dev->40001"
	found := false
	for _, r := range routes.tlsRoutes {
		found = found || r == want
	}
	if !found {
		t.Fatalf("tlsRoutes = %v, want %s", routes.tlsRoutes, want)
	}
}

func TestDeploySkipsCustomDomainWhenNotActive(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")

	// Fresh config is "issuing": no cert is armed yet, so no TLS route.
	if err := s.SetDomainConfig("shop.dev", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(routes.tlsRoutes) != 0 {
		t.Fatalf("tlsRoutes = %v, want none while domain is issuing", routes.tlsRoutes)
	}

	// A failed config must not get one either.
	if err := s.UpdateDomainStatus("shop.dev", "failed", "acme: boom", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(routes.tlsRoutes) != 0 {
		t.Fatalf("tlsRoutes = %v, want none while domain is failed", routes.tlsRoutes)
	}
}

func TestDeploySkipsCustomDomainWhenAbsent(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")

	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(routes.tlsRoutes) != 0 {
		t.Fatalf("tlsRoutes = %v, want none without an active custom domain", routes.tlsRoutes)
	}
}

func TestDeployCombinedLogIsTailCapped(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1", Log: strings.Repeat("b", runtime.LogCap)},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
		HealthErr:      errors.New("unhealthy"),
		LogsVal:        "THE END",
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")

	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err == nil {
		t.Fatal("expected health error")
	}
	logs := deploymentLog(t, s, "blog")
	if !strings.HasPrefix(logs, "[log truncated]\n") {
		t.Error("combined log over cap must carry the truncation marker")
	}
	if !strings.Contains(logs, "THE END") {
		t.Error("tail (container output) must be kept")
	}
	if !strings.HasSuffix(logs, "unhealthy\n") {
		t.Error("error text must be the tail (recorded after container output)")
	}
}

func TestBeginCreatesBuildingRow(t *testing.T) {
	s, _ := newStore(t)
	d := New(s, &runtime.FakeRuntime{}, newFakeCaddy(), "piper.localhost")
	dep, err := d.Begin("blog")
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if dep.Status != "building" {
		t.Fatalf("status = %q, want building", dep.Status)
	}
	// LatestRunning must ignore a building row.
	if _, err := s.LatestRunning("blog"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("LatestRunning on building row = %v, want ErrNotFound", err)
	}
}

func TestFinishSucceedsAndPersistsLog(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img-1", Log: "built ok\n"},
		BuildOutput:    "pulling...\nbuilt ok\n",
		RunResultVal:   runtime.RunResult{ContainerID: "cid-1", HostPort: 40001},
	}
	caddy := newFakeCaddy()
	d := New(s, rt, caddy, "piper.localhost")

	dep, err := d.Begin("blog")
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := d.Finish(context.Background(), dep, t.TempDir()); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	got, err := s.LatestDeployment("blog")
	if err != nil {
		t.Fatalf("LatestDeployment: %v", err)
	}
	if got.Status != "running" || got.ContainerID != "cid-1" || got.HostPort != 40001 {
		t.Fatalf("finalized = %+v", got)
	}
	if caddy.upserts["blog.piper.localhost"] != 40001 {
		t.Fatalf("route not set: %+v", caddy.upserts)
	}
	logs, _ := s.DeploymentLogs("blog", dep.ID)
	if !strings.Contains(logs, "built ok") {
		t.Fatalf("logs missing build output: %q", logs)
	}
}

func TestFinishRouteFailureFinalizesFailed(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img-1", Log: "built ok\n"},
		RunResultVal:   runtime.RunResult{ContainerID: "cid-1", HostPort: 40001},
	}
	caddy := newFakeCaddy()
	caddy.upsertErr = errors.New("caddy admin API unreachable")
	d := New(s, rt, caddy, "piper.localhost")

	dep, err := d.Begin("blog")
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := d.Finish(context.Background(), dep, t.TempDir()); err == nil {
		t.Fatal("Finish: expected route error")
	}
	// Same row, finalized failed — not left "running" despite the build/run/health
	// succeeding, since the app is unreachable without a route.
	got, err := s.LatestDeployment("blog")
	if err != nil {
		t.Fatalf("LatestDeployment: %v", err)
	}
	if got.ID != dep.ID || got.Status != "failed" {
		t.Fatalf("row = %+v, want %s failed", got, dep.ID)
	}
	logs, _ := s.DeploymentLogs("blog", dep.ID)
	if !strings.Contains(logs, "caddy admin API unreachable") {
		t.Fatalf("failed log missing route error: %q", logs)
	}
}

func TestFinishBuildFailureFinalizesSameRowFailed(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{Log: "boom\n"},
		BuildErr:       errors.New("build blew up"),
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")

	dep, err := d.Begin("blog")
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := d.Finish(context.Background(), dep, t.TempDir()); err == nil {
		t.Fatal("Finish: expected error")
	}
	// Same row, now failed — no second row created.
	all, _ := s.ListDeployments("blog")
	if len(all) != 1 {
		t.Fatalf("want 1 deployment row, got %d", len(all))
	}
	if all[0].ID != dep.ID || all[0].Status != "failed" {
		t.Fatalf("row = %+v", all[0])
	}
	logs, _ := s.DeploymentLogs("blog", dep.ID)
	if !strings.Contains(logs, "boom") {
		t.Fatalf("failed log missing build output: %q", logs)
	}
}

func TestDeployWrapperEqualsBeginFinish(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img-1"},
		RunResultVal:   runtime.RunResult{ContainerID: "cid-1", HostPort: 40002},
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")
	dep, err := d.Deploy(context.Background(), "blog", t.TempDir())
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if dep.Status != "running" || dep.HostPort != 40002 {
		t.Fatalf("deploy result = %+v", dep)
	}
}

func TestLogSinkFlushesOnInterval(t *testing.T) {
	s, _ := newStore(t)
	dep, err := s.CreateDeployment("blog", "", "", 0, "building", "")
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	sink := &logSink{store: s, id: dep.ID}
	// First write: lastFlush is zero → flushes immediately.
	_, _ = sink.Write([]byte("chunk-a\n"))
	if logs, _ := s.DeploymentLogs("blog", dep.ID); logs != "chunk-a\n" {
		t.Fatalf("after first write logs = %q", logs)
	}
	// Second write within the interval: buffered, not yet flushed.
	sink.lastFlush = time.Now()
	_, _ = sink.Write([]byte("chunk-b\n"))
	if logs, _ := s.DeploymentLogs("blog", dep.ID); logs != "chunk-a\n" {
		t.Fatalf("debounced logs = %q, want unchanged", logs)
	}
	// Interval elapsed: next write flushes the whole buffer.
	sink.lastFlush = time.Now().Add(-2 * logFlushInterval)
	_, _ = sink.Write([]byte("chunk-c\n"))
	if logs, _ := s.DeploymentLogs("blog", dep.ID); logs != "chunk-a\nchunk-b\nchunk-c\n" {
		t.Fatalf("after interval logs = %q", logs)
	}
}

func TestStopRetiresRunningAndRemovesRoute(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	if err := d.Stop(context.Background(), "blog"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(rt.Stopped) != 1 || rt.Stopped[0] != "c1" {
		t.Errorf("stopped = %v, want [c1]", rt.Stopped)
	}
	if len(routes.removed()) != 1 || routes.removed()[0] != "blog.piper.localhost" {
		t.Errorf("removed = %v, want [blog.piper.localhost]", routes.removed())
	}
	if _, err := s.LatestRunning("blog"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("LatestRunning after stop err = %v, want ErrNotFound", err)
	}
	// App and history remain: latest deployment is the same row, now stopped.
	dep, err := s.LatestDeployment("blog")
	if err != nil || dep.Status != "stopped" || dep.ContainerID != "c1" {
		t.Errorf("latest = %+v (err %v), want c1 stopped", dep, err)
	}
}

func TestStopNothingRunningIsNoOp(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")
	if err := d.Stop(context.Background(), "blog"); err != nil {
		t.Fatalf("Stop no-op err = %v, want nil", err)
	}
	if len(rt.Stopped) != 0 || len(routes.removed()) != 0 {
		t.Errorf("no-op stop touched runtime/routes: %v %v", rt.Stopped, routes.removed())
	}
}

func TestStopUnknownAppIsNotFound(t *testing.T) {
	s, _ := newStore(t)
	d := New(s, &runtime.FakeRuntime{}, newFakeCaddy(), "piper.localhost")
	if err := d.Stop(context.Background(), "ghost"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Stop(ghost) err = %v, want ErrNotFound", err)
	}
}

func TestStopRemovesCustomDomainRoute(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")
	if err := s.SetDomainConfig("shop.dev", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateDomainStatus("shop.dev", "active", "", time.Now().Add(60*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	if err := d.Stop(context.Background(), "blog"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	got := routes.removed()
	want := map[string]bool{"blog.piper.localhost": true, "blog.shop.dev": true}
	if len(got) != 2 || !want[got[0]] || !want[got[1]] {
		t.Errorf("removed = %v, want both primary and custom-domain hosts", got)
	}
}

func TestStopTerminatedRemovesAssignedHostname(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "public.getpiper.co")
	reg := &fakeRegistrar{}
	d.SetHostnameRegistrar(reg)
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	if err := d.Stop(context.Background(), "blog"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(routes.removed()) != 1 || routes.removed()[0] != "hash-blog-alice.public.getpiper.co" {
		t.Errorf("removed = %v, want the assigned hostname", routes.removed())
	}
	if len(reg.deregs) != 0 {
		t.Errorf("deregs = %v, stop must not deregister (delete-only)", reg.deregs)
	}
}

func TestStopTerminatedRelayDownSkipsRouteBestEffort(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "public.getpiper.co")
	reg := &fakeRegistrar{}
	d.SetHostnameRegistrar(reg)
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	reg.failing = true // relay unreachable at stop time
	if err := d.Stop(context.Background(), "blog"); err != nil {
		t.Fatalf("Stop with relay down err = %v, want nil (best-effort)", err)
	}
	if len(rt.Stopped) != 1 || rt.Stopped[0] != "c1" {
		t.Errorf("stopped = %v, want [c1] despite relay outage", rt.Stopped)
	}
	if len(routes.removed()) != 0 {
		t.Errorf("removed = %v, want none (hostname unknown)", routes.removed())
	}
	dep, err := s.LatestDeployment("blog")
	if err != nil || dep.Status != "stopped" {
		t.Errorf("latest = %+v (err %v), want stopped", dep, err)
	}
}

func TestStartReRunsStoppedAndAddsRoute(t *testing.T) {
	s, path := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if err := d.Stop(context.Background(), "blog"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// The restart re-runs the built image as a fresh container on a new port.
	rt.RunResultVal = runtime.RunResult{ContainerID: "c2", HostPort: 40002}
	if err := d.Start(context.Background(), "blog"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if routes.upserts["blog.piper.localhost"] != 40002 {
		t.Errorf("route = %v, want blog.piper.localhost->40002", routes.upserts)
	}
	// The same row flips back to running with the new container and port.
	dep, err := s.LatestRunning("blog")
	if err != nil {
		t.Fatalf("LatestRunning after start: %v", err)
	}
	if dep.ContainerID != "c2" || dep.HostPort != 40002 || dep.Status != "running" {
		t.Errorf("latest = %+v, want c2/40002/running", dep)
	}
	// No extra deployment row was created — the stopped row was reused.
	if n := deploymentCountWithStatus(t, path, "running"); n != 1 {
		t.Errorf("running rows = %d, want 1", n)
	}
	if n := deploymentCountWithStatus(t, path, "stopped"); n != 0 {
		t.Errorf("stopped rows = %d, want 0", n)
	}
}

func TestStartNothingStoppedIsNoOp(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	// The app is already running: Start must not re-run or re-route it.
	rt.RunResultVal = runtime.RunResult{ContainerID: "c2", HostPort: 40002}
	if err := d.Start(context.Background(), "blog"); err != nil {
		t.Fatalf("Start no-op err = %v, want nil", err)
	}
	if routes.upserts["blog.piper.localhost"] != 40001 {
		t.Errorf("route = %v, want unchanged 40001", routes.upserts)
	}
	dep, _ := s.LatestRunning("blog")
	if dep.ContainerID != "c1" {
		t.Errorf("container = %q, want unchanged c1", dep.ContainerID)
	}
}

func TestStartNeverDeployedIsNoOp(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")
	if err := d.Start(context.Background(), "blog"); err != nil {
		t.Fatalf("Start never-deployed err = %v, want nil", err)
	}
	if len(routes.upserts) != 0 {
		t.Errorf("no-op start touched routes: %v", routes.upserts)
	}
}

func TestStartUnknownAppIsNotFound(t *testing.T) {
	s, _ := newStore(t)
	d := New(s, &runtime.FakeRuntime{}, newFakeCaddy(), "piper.localhost")
	if err := d.Start(context.Background(), "ghost"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Start(ghost) err = %v, want ErrNotFound", err)
	}
}

func TestStartRestoresCustomDomainRoute(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")
	if err := s.SetDomainConfig("shop.dev", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateDomainStatus("shop.dev", "active", "", time.Now().Add(60*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if err := d.Stop(context.Background(), "blog"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	rt.RunResultVal = runtime.RunResult{ContainerID: "c2", HostPort: 40002}
	if err := d.Start(context.Background(), "blog"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	want := "blog.shop.dev->40002"
	var found bool
	for _, r := range routes.tlsRoutes {
		if r == want {
			found = true
		}
	}
	if !found {
		t.Errorf("tlsRoutes = %v, want to include %q", routes.tlsRoutes, want)
	}
}

func TestStartTerminatedRestoresAssignedHostname(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "public.getpiper.co")
	d.SetHostnameRegistrar(&fakeRegistrar{})
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if err := d.Stop(context.Background(), "blog"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	rt.RunResultVal = runtime.RunResult{ContainerID: "c2", HostPort: 40002}
	if err := d.Start(context.Background(), "blog"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if routes.upserts["hash-blog-alice.public.getpiper.co"] != 40002 {
		t.Errorf("route = %v, want the assigned hostname -> 40002", routes.upserts)
	}
}

func TestStartRunFailureLeavesStopped(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if err := d.Stop(context.Background(), "blog"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	rt.RunErr = errors.New("no such image")
	if err := d.Start(context.Background(), "blog"); err == nil {
		t.Fatal("Start with run failure err = nil, want error")
	}
	// The row stays stopped so the dashboard can retry; nothing is routed.
	dep, err := s.LatestDeployment("blog")
	if err != nil || dep.Status != "stopped" {
		t.Errorf("latest = %+v (err %v), want stopped", dep, err)
	}
}

func TestDeleteTearsDownProductionAndPreviews(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "main-c", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	rt.RunResultVal = runtime.RunResult{ContainerID: "preview-c", HostPort: 40002}
	if _, err := d.DeployPreview(context.Background(), "blog", 5, t.TempDir()); err != nil {
		t.Fatalf("DeployPreview: %v", err)
	}

	if err := d.Delete(context.Background(), "blog"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	stopped := map[string]bool{}
	for _, id := range rt.Stopped {
		stopped[id] = true
	}
	if !stopped["main-c"] || !stopped["preview-c"] {
		t.Errorf("stopped = %v, want main-c and preview-c", rt.Stopped)
	}
	removed := map[string]bool{}
	for _, h := range routes.removed() {
		removed[h] = true
	}
	if !removed["blog.piper.localhost"] || !removed["pr-5-blog.piper.localhost"] {
		t.Errorf("removed = %v, want production and preview hosts", routes.removed())
	}
	if _, err := s.GetApp("blog"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetApp after delete err = %v, want ErrNotFound", err)
	}
	if deps, _ := s.ListDeployments("blog"); len(deps) != 0 {
		t.Errorf("deployments after delete = %v, want none", deps)
	}
}

func TestDeleteUnknownAppIsNotFound(t *testing.T) {
	s, _ := newStore(t)
	d := New(s, &runtime.FakeRuntime{}, newFakeCaddy(), "piper.localhost")
	if err := d.Delete(context.Background(), "ghost"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Delete(ghost) err = %v, want ErrNotFound", err)
	}
}

func TestDeleteNothingRunningStillDeletesState(t *testing.T) {
	s, _ := newStore(t) // "blog" exists, never deployed
	rt := &runtime.FakeRuntime{}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")
	if err := d.Delete(context.Background(), "blog"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(rt.Stopped) != 0 {
		t.Errorf("stopped = %v, want none", rt.Stopped)
	}
	if _, err := s.GetApp("blog"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetApp after delete err = %v, want ErrNotFound", err)
	}
}

func TestDeleteTerminatedDeregistersHostname(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "public.getpiper.co")
	reg := &fakeRegistrar{}
	d.SetHostnameRegistrar(reg)
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	if err := d.Delete(context.Background(), "blog"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	want := "hash-blog-alice.public.getpiper.co"
	if len(reg.deregs) != 1 || reg.deregs[0] != want {
		t.Errorf("deregs = %v, want [%s]", reg.deregs, want)
	}
	if len(routes.removed()) != 1 || routes.removed()[0] != want {
		t.Errorf("removed = %v, want [%s]", routes.removed(), want)
	}
}

func TestDeleteTerminatedRelayDownStillDeletesState(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "public.getpiper.co")
	reg := &fakeRegistrar{}
	d.SetHostnameRegistrar(reg)
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	reg.failing = true // relay unreachable at delete time
	if err := d.Delete(context.Background(), "blog"); err != nil {
		t.Fatalf("Delete with relay down err = %v, want nil (best-effort)", err)
	}
	if len(reg.deregs) != 0 {
		t.Errorf("deregs = %v, want none (hostname unknown)", reg.deregs)
	}
	if _, err := s.GetApp("blog"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetApp after delete err = %v, want ErrNotFound", err)
	}
}

func TestDeleteRemovesCustomDomainRoute(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")
	if err := s.SetDomainConfig("shop.dev", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateDomainStatus("shop.dev", "active", "", time.Now().Add(60*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	if err := d.Delete(context.Background(), "blog"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	found := false
	for _, h := range routes.removed() {
		found = found || h == "blog.shop.dev"
	}
	if !found {
		t.Errorf("removed = %v, want blog.shop.dev among them", routes.removed())
	}
}

func TestDeleteRouteRemovalFailureLeavesStateIntact(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	routes.removeErr = errors.New("caddy down")
	if err := d.Delete(context.Background(), "blog"); err == nil {
		t.Fatal("Delete with failing unroute must error")
	}
	if _, err := s.GetApp("blog"); err != nil {
		t.Errorf("GetApp = %v, want app still present (delete stays retryable)", err)
	}
}

// gateRuntime blocks in Build until released, so a test can hold the per-app
// deploy lock open and observe whether a second operation proceeds.
type gateRuntime struct {
	started chan struct{}
	release chan struct{}
	run     runtime.RunResult
}

func (g *gateRuntime) Build(context.Context, string, string, io.Writer) (runtime.BuildResult, error) {
	g.started <- struct{}{}
	<-g.release
	return runtime.BuildResult{ImageID: "img"}, nil
}
func (g *gateRuntime) Run(context.Context, string, int, map[string]string) (runtime.RunResult, error) {
	return g.run, nil
}
func (g *gateRuntime) WaitHealthy(context.Context, int) error            { return nil }
func (g *gateRuntime) Stop(context.Context, string) error                { return nil }
func (g *gateRuntime) PruneAppImages(context.Context, string, int) error { return nil }
func (g *gateRuntime) Logs(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func newGate() *gateRuntime {
	return &gateRuntime{
		started: make(chan struct{}, 2),
		release: make(chan struct{}),
		run:     runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
}

// Two concurrent deploys of one app must not build at the same time: the second
// waits on the per-app lock until the first finishes. #159.
func TestDeploySerializesPerApp(t *testing.T) {
	s, _ := newStore(t)
	g := newGate()
	d := New(s, g, newFakeCaddy(), "piper.localhost")

	done := make(chan error, 2)
	go func() { _, err := d.Deploy(context.Background(), "blog", t.TempDir()); done <- err }()
	go func() { _, err := d.Deploy(context.Background(), "blog", t.TempDir()); done <- err }()

	<-g.started // first deploy is inside Build, holding the lock
	select {
	case <-g.started:
		t.Fatal("second deploy built concurrently — per-app deploy lock missing")
	case <-time.After(200 * time.Millisecond):
	}
	g.release <- struct{}{} // first finishes, releasing the lock
	<-g.started             // second now proceeds
	g.release <- struct{}{}
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatalf("deploy: %v", err)
		}
	}
}

// A delete racing an in-flight deploy must wait for it, not tear down state
// mid-deploy (orphan container + resurrected route). #121.
func TestDeleteSerializesAgainstDeploy(t *testing.T) {
	s, _ := newStore(t)
	g := newGate()
	d := New(s, g, newFakeCaddy(), "piper.localhost")

	deployErr := make(chan error, 1)
	go func() { _, err := d.Deploy(context.Background(), "blog", t.TempDir()); deployErr <- err }()
	<-g.started // deploy holds the lock inside Build

	deleteDone := make(chan error, 1)
	go func() { deleteDone <- d.Delete(context.Background(), "blog") }()
	select {
	case <-deleteDone:
		t.Fatal("delete proceeded while a deploy held the per-app lock")
	case <-time.After(200 * time.Millisecond):
	}
	g.release <- struct{}{} // deploy finishes, releasing the lock
	if err := <-deployErr; err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if err := <-deleteDone; err != nil {
		t.Fatalf("delete: %v", err)
	}
}

// A flaky Caddy admin call on the secondary custom-domain hostname must not
// fail a deploy that already succeeded on its primary URL. #115.
func TestDeployCustomDomainRouteFailureDoesNotAbort(t *testing.T) {
	s, _ := newStore(t)
	if err := s.SetDomainConfig("example.com", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateDomainStatus("example.com", "active", "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	routes.tlsErr = errors.New("caddy admin flaky")
	d := New(s, rt, routes, "piper.localhost")

	dep, err := d.Deploy(context.Background(), "blog", t.TempDir())
	if err != nil {
		t.Fatalf("deploy should succeed despite a secondary TLS-route failure: %v", err)
	}
	if dep.Status != "running" {
		t.Errorf("status = %q, want running", dep.Status)
	}
	if routes.upserts["blog.piper.localhost"] != 40001 {
		t.Errorf("primary route = %+v, want blog.piper.localhost -> 40001", routes.upserts)
	}
}

// A routing failure must stop the just-started container, not orphan it while
// finalizing the row "failed". #162.
func TestDeployRouteFailureStopsContainerAndRecordsFailed(t *testing.T) {
	s, path := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	routes.upsertErr = errors.New("caddy admin down")
	d := New(s, rt, routes, "piper.localhost")

	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err == nil {
		t.Fatal("Deploy should fail on a routing error")
	}
	if len(rt.Stopped) != 1 || rt.Stopped[0] != "c1" {
		t.Errorf("stopped = %v, want [c1] (route failure must not orphan the container)", rt.Stopped)
	}
	if got := deploymentCountWithStatus(t, path, "failed"); got != 1 {
		t.Errorf("failed deployment count = %d, want 1", got)
	}
	if _, err := s.LatestRunning("blog"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("LatestRunning err = %v, want ErrNotFound", err)
	}
}

// panickyCaddy panics on UpsertRoute, standing in for a bug in the routing path
// after the container is up.
type panickyCaddy struct{ *fakeCaddy }

func (panickyCaddy) UpsertRoute(string, int) error { panic("caddy client boom") }

// A panic in the routing/finalize section must be recovered, the container
// stopped, and the row finalized "failed" — not left running with a leaked
// container. #162.
func TestDeployPanicStopsContainerAndRecordsFailed(t *testing.T) {
	s, path := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	d := New(s, rt, panickyCaddy{newFakeCaddy()}, "piper.localhost")

	_, err := d.Deploy(context.Background(), "blog", t.TempDir())
	if err == nil {
		t.Fatal("Deploy should return an error after recovering the panic")
	}
	if len(rt.Stopped) != 1 || rt.Stopped[0] != "c1" {
		t.Errorf("stopped = %v, want [c1] (panic must not orphan the container)", rt.Stopped)
	}
	if got := deploymentCountWithStatus(t, path, "failed"); got != 1 {
		t.Errorf("failed deployment count = %d, want 1", got)
	}
}

// addAppDomain registers a per-app custom domain in the given status.
func addAppDomain(t *testing.T, s *store.Store, domain, app, status string) {
	t.Helper()
	if err := s.AddAppDomain(domain, app); err != nil {
		t.Fatalf("AddAppDomain(%s): %v", domain, err)
	}
	if status != "" {
		if err := s.UpdateAppDomainStatus(domain, status, "", time.Now().Add(60*24*time.Hour)); err != nil {
			t.Fatalf("UpdateAppDomainStatus(%s): %v", domain, err)
		}
	}
}

// Deploy arms an exact-host TLS route for each of the app's active per-app
// domains — and only those: not pending/failed ones, not other apps'. #230.
func TestDeployRoutesActiveAppDomainsOnly(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.CreateApp("shop", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	addAppDomain(t, s, "blog.example.com", "blog", "active")
	addAppDomain(t, s, "www.blog.example.com", "blog", "active")
	addAppDomain(t, s, "pending.example.com", "blog", "pending")
	addAppDomain(t, s, "failed.example.com", "blog", "failed")
	addAppDomain(t, s, "shop.example.com", "shop", "active")

	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")

	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	want := []string{"blog.example.com->40001", "www.blog.example.com->40001"}
	if len(routes.tlsRoutes) != len(want) {
		t.Fatalf("tlsRoutes = %v, want %v", routes.tlsRoutes, want)
	}
	for i, w := range want {
		if routes.tlsRoutes[i] != w {
			t.Errorf("tlsRoutes[%d] = %q, want %q", i, routes.tlsRoutes[i], w)
		}
	}
}

// A flaky Caddy admin call on a per-app domain route must not fail a deploy
// that already succeeded on its primary URL (same stance as #115).
func TestDeployAppDomainRouteFailureDoesNotAbort(t *testing.T) {
	s, _ := newStore(t)
	addAppDomain(t, s, "blog.example.com", "blog", "active")
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	routes.tlsErr = errors.New("caddy admin flaky")
	d := New(s, rt, routes, "piper.localhost")

	dep, err := d.Deploy(context.Background(), "blog", t.TempDir())
	if err != nil {
		t.Fatalf("deploy should succeed despite an app-domain route failure: %v", err)
	}
	if dep.Status != "running" {
		t.Errorf("status = %q, want running", dep.Status)
	}
	if routes.upserts["blog.piper.localhost"] != 40001 {
		t.Errorf("primary route = %+v, want blog.piper.localhost -> 40001", routes.upserts)
	}
}

// Stop drops exactly the stopped app's per-app domain routes; another app's
// domains are untouched. #230.
func TestStopRemovesAppDomainRoutes(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.CreateApp("shop", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	addAppDomain(t, s, "blog.example.com", "blog", "active")
	addAppDomain(t, s, "pending.example.com", "blog", "pending")
	addAppDomain(t, s, "shop.example.com", "shop", "active")
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	if err := d.Stop(context.Background(), "blog"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	removed := map[string]bool{}
	for _, h := range routes.removed() {
		removed[h] = true
	}
	if !removed["blog.piper.localhost"] || !removed["blog.example.com"] {
		t.Errorf("removed = %v, want primary and blog.example.com", routes.removed())
	}
	if removed["shop.example.com"] || removed["pending.example.com"] {
		t.Errorf("removed = %v, must not touch other apps' or inactive domains", routes.removed())
	}
}

// Delete drops the app's per-app domain routes along with everything else. #230.
func TestDeleteRemovesAppDomainRoutes(t *testing.T) {
	s, _ := newStore(t)
	addAppDomain(t, s, "blog.example.com", "blog", "active")
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	if err := d.Delete(context.Background(), "blog"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	found := false
	for _, h := range routes.removed() {
		found = found || h == "blog.example.com"
	}
	if !found {
		t.Errorf("removed = %v, want blog.example.com among them", routes.removed())
	}
}

// RouteAppDomain backfills the exact-host route when a domain goes active
// while its app is already running. #230.
func TestRouteAppDomainBackfillsRunningApp(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	if err := d.RouteAppDomain("blog", "blog.example.com"); err != nil {
		t.Fatalf("RouteAppDomain: %v", err)
	}
	want := "blog.example.com->40001"
	found := false
	for _, r := range routes.tlsRoutes {
		found = found || r == want
	}
	if !found {
		t.Errorf("tlsRoutes = %v, want %s", routes.tlsRoutes, want)
	}
}

// With nothing running there is no upstream to route to: RouteAppDomain is a
// no-op — the next deploy arms the route.
func TestRouteAppDomainNothingRunningIsNoOp(t *testing.T) {
	s, _ := newStore(t)
	routes := newFakeCaddy()
	d := New(s, &runtime.FakeRuntime{}, routes, "piper.localhost")
	if err := d.RouteAppDomain("blog", "blog.example.com"); err != nil {
		t.Fatalf("RouteAppDomain no-op err = %v, want nil", err)
	}
	if len(routes.tlsRoutes) != 0 {
		t.Errorf("tlsRoutes = %v, want none", routes.tlsRoutes)
	}
}

// A successful deploy GCs superseded images, keeping the newest N per app. #125.
func TestDeployPrunesOldImages(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")

	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(rt.Pruned) != 1 || rt.Pruned[0] != (runtime.PruneCall{App: "blog", Keep: imageRetentionPerApp}) {
		t.Fatalf("pruned = %+v, want one keep=%d prune of blog", rt.Pruned, imageRetentionPerApp)
	}
}

// A failed deploy must NOT prune (its image is the newest and may still be
// wanted for debugging; nothing was superseded).
func TestFailedDeployDoesNotPrune(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
		HealthErr:      errors.New("unhealthy"),
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")

	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err == nil {
		t.Fatal("Deploy should fail")
	}
	if len(rt.Pruned) != 0 {
		t.Fatalf("pruned = %+v, want none on a failed deploy", rt.Pruned)
	}
}

// Delete removes the app's every image (keep=0). #125.
func TestDeletePrunesAllImages(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")

	if err := d.Delete(context.Background(), "blog"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(rt.Pruned) != 1 || rt.Pruned[0] != (runtime.PruneCall{App: "blog", Keep: 0}) {
		t.Fatalf("pruned = %+v, want one keep=0 prune of blog", rt.Pruned)
	}
}

// TestDeployPreviewTerminatedRoutesAssignedHostname covers previews on a
// relay-terminated box (#302). There baseDom is itself a label under the apex,
// so the constructed "pr-<N>-<app>.<baseDom>" sits two labels deep — outside
// the relay's single-label wildcard cert and unknown to its router — and the
// preview is unreachable however healthy the container is. Like production, the
// host must come from the registrar.
func TestDeployPreviewTerminatedRoutesAssignedHostname(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "preview-c", HostPort: 40002},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "85b90055-alice.public.getpiper.co")
	reg := &fakeRegistrar{}
	d.SetHostnameRegistrar(reg)

	if _, err := d.DeployPreview(context.Background(), "blog", 7, t.TempDir()); err != nil {
		t.Fatalf("DeployPreview: %v", err)
	}
	want := "pr7-hash-blog-alice.public.getpiper.co"
	if routes.upserts[want] != 40002 {
		t.Fatalf("routes = %+v, want %s -> 40002", routes.upserts, want)
	}
	if _, ok := routes.upserts["pr-7-blog.85b90055-alice.public.getpiper.co"]; ok {
		t.Error("routed the constructed two-label host; the relay cannot serve it")
	}
	if host, ok := d.PreviewHost("blog", 7); !ok || host != want {
		t.Fatalf("PreviewHost = %q,%v want %q (what gets reported to GitHub)", host, ok, want)
	}

	// Teardown releases the relay-side hostname; otherwise every closed PR
	// leaks a hostnames row.
	if _, err := d.TeardownPreview(context.Background(), "blog", 7); err != nil {
		t.Fatalf("TeardownPreview: %v", err)
	}
	if len(routes.removed()) != 1 || routes.removed()[0] != want {
		t.Errorf("removed routes = %v, want [%s]", routes.removed(), want)
	}
	if len(reg.deregs) != 1 || reg.deregs[0] != want {
		t.Errorf("deregistered = %v, want [%s]", reg.deregs, want)
	}
}

// TestPreviewHostWithoutRegistrar keeps LAN and BYO boxes on today's
// construction: there baseDom is the apex, so "pr-<N>-<app>.<apex>" is a single
// label and is exactly right.
func TestPreviewHostWithoutRegistrar(t *testing.T) {
	s, _ := newStore(t)
	d := New(s, &runtime.FakeRuntime{}, newFakeCaddy(), "alice.dev")
	if host, ok := d.PreviewHost("blog", 5); !ok || host != "pr-5-blog.alice.dev" {
		t.Fatalf("PreviewHost = %q,%v want pr-5-blog.alice.dev", host, ok)
	}
}

// Deleting an app must release its previews' relay hostnames too, or every
// deleted app leaks one hostnames row (and one router entry) per open PR.
func TestDeleteTerminatedReleasesPreviewHostnames(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "main-c", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "85b90055-alice.public.getpiper.co")
	reg := &fakeRegistrar{}
	d.SetHostnameRegistrar(reg)

	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	rt.RunResultVal = runtime.RunResult{ContainerID: "preview-c", HostPort: 40002}
	if _, err := d.DeployPreview(context.Background(), "blog", 7, t.TempDir()); err != nil {
		t.Fatalf("DeployPreview: %v", err)
	}
	if err := d.Delete(context.Background(), "blog"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	const previewHost = "pr7-hash-blog-alice.public.getpiper.co"
	removed := map[string]bool{}
	for _, h := range routes.removed() {
		removed[h] = true
	}
	if !removed[previewHost] || !removed["hash-blog-alice.public.getpiper.co"] {
		t.Errorf("removed routes = %v, want both the preview and production hosts", routes.removed())
	}
	dereg := map[string]bool{}
	for _, h := range reg.deregs {
		dereg[h] = true
	}
	if !dereg[previewHost] || !dereg["hash-blog-alice.public.getpiper.co"] {
		t.Errorf("deregistered = %v, want both the preview and production hosts", reg.deregs)
	}
}

// deployedThenRestarted deploys app and returns a fresh Deployer over the same
// store with an empty route table — the state piperd is in after a restart,
// since it embeds Caddy and reloads a bare base config on every start.
func deployedThenRestarted(t *testing.T, reg HostnameRegistrar) (*store.Store, *fakeCaddy, *Deployer) {
	t.Helper()
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	d := New(s, rt, newFakeCaddy(), "public.getpiper.co")
	if reg != nil {
		d.SetHostnameRegistrar(reg)
	}
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	fresh := newFakeCaddy() // Caddy came back with no routes
	d2 := New(s, rt, fresh, "public.getpiper.co")
	if reg != nil {
		d2.SetHostnameRegistrar(reg)
	}
	return s, fresh, d2
}

// TestResumeRoutesReArmsRunningApp pins #371: piperd embeds Caddy and reloads a
// bare base config on every start, dropping every route. Custom domains are
// re-armed by the domain manager's resume; the app's own host route had nothing
// restoring it, so a running app answered Caddy's empty-200 no-route fallback
// until it was redeployed.
func TestResumeRoutesReArmsRunningApp(t *testing.T) {
	_, routes, d := deployedThenRestarted(t, nil)

	d.ResumeRoutes()

	if routes.upserts["blog.public.getpiper.co"] != 40001 {
		t.Fatalf("routes = %+v, want blog.public.getpiper.co -> 40001", routes.upserts)
	}
}

// TestResumeRoutesTerminatedReArmsAssignedHostname is the relay-terminated case
// — the one that broke live: the route must be the relay-assigned hostname, not
// <app>.<base>, or the relay routes traffic to a host Caddy does not know.
func TestResumeRoutesTerminatedReArmsAssignedHostname(t *testing.T) {
	_, routes, d := deployedThenRestarted(t, &fakeRegistrar{})

	d.ResumeRoutes()

	if routes.upserts["hash-blog-alice.public.getpiper.co"] != 40001 {
		t.Fatalf("routes = %+v, want hash-blog-alice.public.getpiper.co -> 40001", routes.upserts)
	}
	if _, ok := routes.upserts["blog.public.getpiper.co"]; ok {
		t.Errorf("routed the base-domain host too: %+v", routes.upserts)
	}
}

// TestResumeRoutesSkipsStoppedApp keeps a deliberately stopped app down: routing
// it again would resurrect a URL the owner took offline.
func TestResumeRoutesSkipsStoppedApp(t *testing.T) {
	s, routes, d := deployedThenRestarted(t, nil)
	if err := d.Stop(context.Background(), "blog"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := s.LatestRunning("blog"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("LatestRunning after Stop: %v, want ErrNotFound", err)
	}

	d.ResumeRoutes()

	if len(routes.upserts) != 0 {
		t.Fatalf("routed a stopped app: %+v", routes.upserts)
	}
}

// TestResumeRoutesSkipsNeverDeployedApp covers an app row with no deployment at
// all — there is no port to route to, and it must not abort the whole sweep.
func TestResumeRoutesSkipsNeverDeployedApp(t *testing.T) {
	s, routes, d := deployedThenRestarted(t, nil)
	if _, err := s.CreateApp("fresh", 3000); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}

	d.ResumeRoutes()

	if _, ok := routes.upserts["fresh.public.getpiper.co"]; ok {
		t.Errorf("routed a never-deployed app: %+v", routes.upserts)
	}
	if routes.upserts["blog.public.getpiper.co"] != 40001 {
		t.Errorf("the deployed app was not re-armed: %+v", routes.upserts)
	}
}

// TestResumeRoutesUnresolvableHostnameDoesNotStopSweep: in terminated mode
// primaryHost needs the relay, and a failing Register must not strand the rest.
func TestResumeRoutesUnresolvableHostnameDoesNotStopSweep(t *testing.T) {
	reg := &fakeRegistrar{}
	_, routes, d := deployedThenRestarted(t, reg)
	reg.failing = true

	d.ResumeRoutes() // must not panic or abort

	if len(routes.upserts) != 0 {
		t.Fatalf("routed despite an unresolvable hostname: %+v", routes.upserts)
	}
}

// TestStopMarksStoppedEvenWhenUnrouteFails pins #377: Stop halts the container
// first and writes the row last, so a route-teardown failure in between used to
// return early and leave the deployment marked "running" while its container was
// already gone. Start only acts on a "stopped" row, so that state was
// unrecoverable through the CLI/TUI/dashboard — stop reported an error but
// really stopped the app, and start then silently did nothing forever. Only a
// redeploy rewrote the row.
func TestStopMarksStoppedEvenWhenUnrouteFails(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	routes.removeErr = errors.New("caddy remove route: status 500")
	err := d.Stop(context.Background(), "blog")

	// The failure must still be reported — routing really did not get cleaned up.
	if err == nil {
		t.Fatal("Stop returned nil, want the unroute failure surfaced")
	}
	// ...but the row must match reality: the container is down.
	if _, err := s.LatestRunning("blog"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deployment still marked running after Stop: %v", err)
	}
}

// TestStartRecoversAfterFailedUnroute is the half that matters to the user: once
// the row is honest, the app can be started again from the UI instead of needing
// a redeploy.
func TestStartRecoversAfterFailedUnroute(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	routes.removeErr = errors.New("caddy remove route: status 500")
	_ = d.Stop(context.Background(), "blog")
	routes.removeErr = nil // the transient Caddy problem clears

	// A genuinely fresh container, so "Start did something" can't be confused
	// with "the row was never updated and still describes the old one".
	rt.RunResultVal = runtime.RunResult{ContainerID: "c2", HostPort: 40002}

	if err := d.Start(context.Background(), "blog"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	got, err := s.LatestRunning("blog")
	if err != nil {
		t.Fatalf("app did not come back up: %v", err)
	}
	if got.ContainerID != "c2" {
		t.Errorf("container = %q, want c2 — Start did not re-run it", got.ContainerID)
	}
	if routes.upserts["blog.piper.localhost"] != 40002 {
		t.Errorf("route not re-armed to the new port: %+v", routes.upserts)
	}
}
