package webhook_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/piperbox/piper/internal/source"
	"github.com/piperbox/piper/internal/store"
	"github.com/piperbox/piper/internal/webhook"
)

type fakeProvider struct {
	mu       sync.Mutex
	parseErr error
	ev       source.Event
	reports  []source.Status
	urls     []string
	fetchErr error
}

func (f *fakeProvider) Parse(http.Header, []byte) (source.Event, error) {
	return f.ev, f.parseErr
}
func (f *fakeProvider) Fetch(context.Context, source.Event, string) error { return f.fetchErr }
func (f *fakeProvider) Report(_ context.Context, _ source.Event, s source.Status, url string) error {
	f.mu.Lock()
	f.reports = append(f.reports, s)
	f.urls = append(f.urls, url)
	f.mu.Unlock()
	return nil
}

func (f *fakeProvider) successURL() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, st := range f.reports {
		if st == source.StatusSuccess {
			return f.urls[i]
		}
	}
	return ""
}
func (f *fakeProvider) statuses() []source.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]source.Status(nil), f.reports...)
}

type fakeDeployer struct {
	mu            sync.Mutex
	calls         int
	previewCalls  int
	teardownCalls int
	retired       bool
	err           error
	previewHost   string // relay-assigned preview host; empty means "no registrar"
}

type blockingDeployer struct {
	started  chan struct{}
	finished chan struct{}
}

func (d *blockingDeployer) Deploy(ctx context.Context, _, _ string) (store.Deployment, error) {
	close(d.started)
	<-ctx.Done()
	close(d.finished)
	return store.Deployment{}, ctx.Err()
}

func (d *blockingDeployer) DeployPreview(context.Context, string, int, string) (store.Deployment, error) {
	return store.Deployment{}, nil
}

func (d *blockingDeployer) TeardownPreview(context.Context, string, int) (bool, error) {
	return false, nil
}

func (d *blockingDeployer) PreviewHost(string, int) (string, bool) { return "", false }

func (d *fakeDeployer) Deploy(context.Context, string, string) (store.Deployment, error) {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()
	return store.Deployment{}, d.err
}
func (d *fakeDeployer) DeployPreview(context.Context, string, int, string) (store.Deployment, error) {
	d.mu.Lock()
	d.previewCalls++
	d.mu.Unlock()
	return store.Deployment{}, d.err
}
func (d *fakeDeployer) TeardownPreview(context.Context, string, int) (bool, error) {
	d.mu.Lock()
	d.teardownCalls++
	retired := d.retired
	d.mu.Unlock()
	return retired, d.err
}
func (d *fakeDeployer) PreviewHost(app string, pr int) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.previewHost == "" {
		return "", false
	}
	return d.previewHost, true
}
func (d *fakeDeployer) count() int     { d.mu.Lock(); defer d.mu.Unlock(); return d.calls }
func (d *fakeDeployer) previews() int  { d.mu.Lock(); defer d.mu.Unlock(); return d.previewCalls }
func (d *fakeDeployer) teardowns() int { d.mu.Lock(); defer d.mu.Unlock(); return d.teardownCalls }

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func post(h http.Handler) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
	h.ServeHTTP(rec, req)
	return rec
}

func TestBadSignatureReturns401(t *testing.T) {
	p := &fakeProvider{parseErr: source.ErrBadSignature}
	d := &fakeDeployer{}
	h := webhook.New(p, newStore(t), d, "piper.localhost")
	if rec := post(h); rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d", rec.Code)
	}
	h.Wait()
	if d.count() != 0 {
		t.Fatal("deploy must not run on bad signature")
	}
}

func TestUnknownRepoNoOp(t *testing.T) {
	p := &fakeProvider{ev: source.Event{Kind: source.KindPush, Repo: "nobody/x", Ref: "refs/heads/main"}}
	d := &fakeDeployer{}
	h := webhook.New(p, newStore(t), d, "piper.localhost")
	rec := post(h)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d", rec.Code)
	}
	h.Wait()
	if d.count() != 0 {
		t.Fatal("deploy must not run for unknown repo")
	}
}

func TestPushDeploysAndReports(t *testing.T) {
	s := newStore(t)
	s.CreateApp("blog", 8080)
	s.UpdateAppRepo("blog", "alice/blog", "main", "")
	p := &fakeProvider{ev: source.Event{
		Kind: source.KindPush, Repo: "alice/blog", Ref: "refs/heads/main", SHA: "s1",
	}}
	d := &fakeDeployer{}
	h := webhook.New(p, s, d, "piper.localhost")

	if rec := post(h); rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d", rec.Code)
	}
	h.Wait()
	if d.count() != 1 {
		t.Fatalf("deploy calls = %d", d.count())
	}
	got := p.statuses()
	if len(got) != 2 || got[0] != source.StatusPending || got[1] != source.StatusSuccess {
		t.Fatalf("statuses = %v", got)
	}
}

func TestPushScopedToRootDir(t *testing.T) {
	cases := []struct {
		name       string
		rootDir    string
		paths      []string
		wantDeploy int
	}{
		{"changes under the root dir rebuild", "apps/web", []string{"apps/api/main.go", "apps/web/index.html"}, 1},
		{"changes outside the root dir do not", "apps/web", []string{"apps/api/main.go", "README.md"}, 0},
		{"a sibling sharing the prefix does not", "apps/web", []string{"apps/website/index.html"}, 0},
		{"an app without a root dir rebuilds", "", []string{"docs/README.md"}, 1},
		{"an unreported file list rebuilds", "apps/web", nil, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newStore(t)
			s.CreateApp("blog", 8080)
			s.UpdateAppRepo("blog", "alice/blog", "main", c.rootDir)
			p := &fakeProvider{ev: source.Event{
				Kind: source.KindPush, Repo: "alice/blog", Ref: "refs/heads/main",
				SHA: "s1", Paths: c.paths,
			}}
			d := &fakeDeployer{}
			h := webhook.New(p, s, d, "piper.localhost")
			post(h)
			h.Wait()
			if d.count() != c.wantDeploy {
				t.Fatalf("deploy calls = %d, want %d", d.count(), c.wantDeploy)
			}
			if c.wantDeploy == 0 && len(p.statuses()) != 0 {
				t.Fatalf("statuses = %v, want none for a skipped push", p.statuses())
			}
		})
	}
}

func TestWrongBranchNoOp(t *testing.T) {
	s := newStore(t)
	s.CreateApp("blog", 8080)
	s.UpdateAppRepo("blog", "alice/blog", "main", "")
	p := &fakeProvider{ev: source.Event{
		Kind: source.KindPush, Repo: "alice/blog", Ref: "refs/heads/dev", SHA: "s1",
	}}
	d := &fakeDeployer{}
	h := webhook.New(p, s, d, "piper.localhost")
	post(h)
	h.Wait()
	if d.count() != 0 {
		t.Fatal("deploy must not run for non-tracked branch")
	}
}

func TestDeployFailureReportsFailure(t *testing.T) {
	s := newStore(t)
	s.CreateApp("blog", 8080)
	s.UpdateAppRepo("blog", "alice/blog", "main", "")
	p := &fakeProvider{ev: source.Event{
		Kind: source.KindPush, Repo: "alice/blog", Ref: "refs/heads/main", SHA: "s1",
	}}
	d := &fakeDeployer{err: context.DeadlineExceeded}
	h := webhook.New(p, s, d, "piper.localhost")
	post(h)
	h.Wait()
	got := p.statuses()
	if len(got) != 2 || got[1] != source.StatusFailure {
		t.Fatalf("statuses = %v", got)
	}
}

func TestWaitContextTimesOutAndCancelStopsInFlightDeploy(t *testing.T) {
	s := newStore(t)
	s.CreateApp("blog", 8080)
	s.UpdateAppRepo("blog", "alice/blog", "main", "")
	p := &fakeProvider{ev: source.Event{
		Kind: source.KindPush, Repo: "alice/blog", Ref: "refs/heads/main", SHA: "s1",
	}}
	d := &blockingDeployer{started: make(chan struct{}), finished: make(chan struct{})}
	h := webhook.New(p, s, d, "piper.localhost")

	if rec := post(h); rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d", rec.Code)
	}
	<-d.started

	waitCtx, cancelWait := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelWait()
	if h.WaitContext(waitCtx) {
		t.Fatal("WaitContext reported a drain while deploy was blocked")
	}

	h.Cancel()
	if !h.WaitContext(context.Background()) {
		t.Fatal("WaitContext did not report drain after cancellation")
	}
	select {
	case <-d.finished:
	default:
		t.Fatal("deployment context was not cancelled")
	}
}

func TestWaitContextReportsCompletedWork(t *testing.T) {
	h := webhook.New(&fakeProvider{}, newStore(t), &fakeDeployer{}, "piper.localhost")
	if !h.WaitContext(context.Background()) {
		t.Fatal("WaitContext reported timeout with no in-flight work")
	}
}

func TestStopAcceptingRejectsNewWork(t *testing.T) {
	h := webhook.New(&fakeProvider{}, newStore(t), &fakeDeployer{}, "piper.localhost")
	h.StopAccepting()
	if rec := post(h); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestPROpenedDeploysPreviewAndReports(t *testing.T) {
	s := newStore(t)
	s.CreateApp("blog", 8080)
	s.UpdateAppRepo("blog", "alice/blog", "main", "")
	p := &fakeProvider{ev: source.Event{
		Kind: source.KindPROpened, Repo: "alice/blog", PR: 7, SHA: "s1", Ref: "feature",
	}}
	d := &fakeDeployer{}
	h := webhook.New(p, s, d, "piper.localhost")

	if rec := post(h); rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d", rec.Code)
	}
	h.Wait()
	if d.previews() != 1 || d.count() != 0 {
		t.Fatalf("previews=%d deploys=%d", d.previews(), d.count())
	}
	got := p.statuses()
	if len(got) != 2 || got[0] != source.StatusPending || got[1] != source.StatusSuccess {
		t.Fatalf("statuses = %v", got)
	}
}

func TestPRSyncedIsIdempotentOnSHA(t *testing.T) {
	s := newStore(t)
	s.CreateApp("blog", 8080)
	s.UpdateAppRepo("blog", "alice/blog", "main", "")
	ev := source.Event{Kind: source.KindPRSynced, Repo: "alice/blog", PR: 7, SHA: "s1"}
	p := &fakeProvider{ev: ev}
	d := &fakeDeployer{}
	h := webhook.New(p, s, d, "piper.localhost")
	post(h)
	h.Wait()
	post(h) // same SHA again
	h.Wait()
	if d.previews() != 1 {
		t.Fatalf("previews = %d, want 1 (dedupe)", d.previews())
	}
}

func TestPRClosedWithPreviewTearsDownAndReportsInactive(t *testing.T) {
	s := newStore(t)
	s.CreateApp("blog", 8080)
	s.UpdateAppRepo("blog", "alice/blog", "main", "")
	p := &fakeProvider{ev: source.Event{
		Kind: source.KindPRClosed, Repo: "alice/blog", PR: 7, SHA: "s1",
	}}
	d := &fakeDeployer{retired: true}
	h := webhook.New(p, s, d, "piper.localhost")
	post(h)
	h.Wait()
	if d.teardowns() != 1 {
		t.Fatalf("teardowns = %d, want 1", d.teardowns())
	}
	got := p.statuses()
	if len(got) != 1 || got[0] != source.StatusInactive {
		t.Fatalf("statuses = %v, want [inactive]", got)
	}
}

func TestPRClosedWithoutPreviewReportsNothing(t *testing.T) {
	s := newStore(t)
	s.CreateApp("blog", 8080)
	s.UpdateAppRepo("blog", "alice/blog", "main", "")
	p := &fakeProvider{ev: source.Event{
		Kind: source.KindPRClosed, Repo: "alice/blog", PR: 7, SHA: "s1",
	}}
	d := &fakeDeployer{retired: false}
	h := webhook.New(p, s, d, "piper.localhost")
	post(h)
	h.Wait()
	if d.teardowns() != 1 {
		t.Fatalf("teardowns = %d, want 1", d.teardowns())
	}
	// Nothing was retired, so no deployment status should be posted at all —
	// no wasted deployments lookup, no swallowed "no deployment" error.
	if got := p.statuses(); len(got) != 0 {
		t.Fatalf("statuses = %v, want none", got)
	}
}

// The URL reported to GitHub must be the host the deploy actually routed, not
// "<app>.<baseDom>". On a relay-terminated box the routed host is a flattened
// single-label name the relay assigned; the guessed one sits two labels under
// the apex, outside the relay's wildcard certificate, so GitHub's Deployments
// tab would link somewhere that cannot serve — while the CLI and TUI, which
// read the recorded hostname, showed the working URL.
func TestReportsTheRoutedHostname(t *testing.T) {
	s := newStore(t)
	s.CreateApp("blog", 8080)
	s.UpdateAppRepo("blog", "alice/blog", "main", "")
	if err := s.SetAppHostname("blog", "abc123-alice.public.getpiper.dev"); err != nil {
		t.Fatal(err)
	}
	p := &fakeProvider{ev: source.Event{
		Kind: source.KindPush, Repo: "alice/blog", Ref: "refs/heads/main", SHA: "s1",
	}}
	h := webhook.New(p, s, &fakeDeployer{}, "85b90055-ozykhan.public.getpiper.dev")

	if rec := post(h); rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d", rec.Code)
	}
	h.Wait()

	if got, want := p.successURL(), "https://abc123-alice.public.getpiper.dev"; got != want {
		t.Fatalf("reported %q, want %q", got, want)
	}
}

// A preview on a relay-terminated box is reachable only at the hostname the
// relay assigned; "pr-<N>-<app>.<baseDom>" is two labels under the apex there,
// so GitHub's Deployments tab linked at a host that fails TLS and has no route
// (#302).
func TestReportsTheRoutedPreviewHostname(t *testing.T) {
	s := newStore(t)
	s.CreateApp("blog", 8080)
	s.UpdateAppRepo("blog", "alice/blog", "main", "")
	p := &fakeProvider{ev: source.Event{
		Kind: source.KindPROpened, Repo: "alice/blog", PR: 7, SHA: "s1",
	}}
	d := &fakeDeployer{previewHost: "pr7-abc123-alice.public.getpiper.dev"}
	h := webhook.New(p, s, d, "85b90055-ozykhan.public.getpiper.dev")

	if rec := post(h); rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d", rec.Code)
	}
	h.Wait()

	if got, want := p.successURL(), "https://pr7-abc123-alice.public.getpiper.dev"; got != want {
		t.Fatalf("reported %q, want %q", got, want)
	}
}
