// Package webhook receives a git host's signed webhook, resolves the bound app,
// and drives the deployer. It is the only public surface exposed through the
// tunnel; everything else in piperd stays loopback-only.
package webhook

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/piperbox/piper/internal/source"
	"github.com/piperbox/piper/internal/store"
)

const maxBody = 5 << 20 // 5 MiB

type Deployer interface {
	Deploy(ctx context.Context, app, srcDir string) (store.Deployment, error)
	DeployPreview(ctx context.Context, app string, pr int, srcDir string) (store.Deployment, error)
	TeardownPreview(ctx context.Context, app string, pr int) (retired bool, err error)
	// PreviewHost is the host the preview is actually routed at — relay-assigned
	// where a registrar exists, constructed from the base domain otherwise.
	PreviewHost(app string, pr int) (string, bool)
}

type Handler struct {
	prov    source.Provider
	store   *store.Store
	deploy  Deployer
	baseDom string

	wg      sync.WaitGroup
	mu      sync.Mutex
	locks   map[string]*sync.Mutex
	lastSHA map[string]string

	ctx         context.Context
	cancel      context.CancelFunc
	lifecycleMu sync.Mutex
	accepting   bool
}

func New(p source.Provider, s *store.Store, d Deployer, baseDomain string) *Handler {
	ctx, cancel := context.WithCancel(context.Background())
	return &Handler{
		prov: p, store: s, deploy: d, baseDom: baseDomain,
		locks: map[string]*sync.Mutex{}, lastSHA: map[string]string{},
		ctx: ctx, cancel: cancel, accepting: true,
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.lifecycleMu.Lock()
	if !h.accepting {
		h.lifecycleMu.Unlock()
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	h.wg.Add(1)
	h.lifecycleMu.Unlock()
	defer h.wg.Done()

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	ev, err := h.prov.Parse(r.Header, body)
	if errors.Is(err, source.ErrBadSignature) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	if err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	if ev.Kind == source.KindPing {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "pong")
		return
	}
	w.WriteHeader(http.StatusAccepted)
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		h.process(h.ctx, ev)
	}()
}

// Wait blocks until all in-flight deploy goroutines finish.
func (h *Handler) Wait() { h.wg.Wait() }

// WaitContext waits for in-flight deploy goroutines until ctx expires. It
// returns true when every goroutine finished and false when ctx expired first.
func (h *Handler) WaitContext(ctx context.Context) bool {
	done := make(chan struct{})
	go func() {
		h.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// Cancel asks all in-flight webhook work to stop. It is idempotent.
func (h *Handler) Cancel() { h.cancel() }

// StopAccepting rejects new webhook work and closes the WaitGroup admission
// gate before shutdown starts waiting.
func (h *Handler) StopAccepting() {
	h.lifecycleMu.Lock()
	h.accepting = false
	h.lifecycleMu.Unlock()
}

func (h *Handler) appLock(name string) *sync.Mutex {
	h.mu.Lock()
	defer h.mu.Unlock()
	m, ok := h.locks[name]
	if !ok {
		m = &sync.Mutex{}
		h.locks[name] = m
	}
	return m
}

func (h *Handler) process(ctx context.Context, ev source.Event) {
	switch ev.Kind {
	case source.KindPush:
		h.processPush(ctx, ev)
	case source.KindPROpened, source.KindPRSynced:
		h.processPreview(ctx, ev)
	case source.KindPRClosed:
		h.processPRClosed(ctx, ev)
	}
}

// touchesRootDir reports whether a push carrying paths should rebuild an app
// that builds from rootDir. An empty rootDir builds the whole checkout, so
// every push touches it. Nil paths mean the host reported no complete file
// list, and a rebuild we didn't need beats silently swallowing a real push, so
// they rebuild too (#319). rootDir is stored path.Clean'd, so the subpath test
// is a plain "<rootDir>/" prefix — which also keeps a sibling like
// "apps/website" from matching "apps/web".
func touchesRootDir(rootDir string, paths []string) bool {
	if rootDir == "" || len(paths) == 0 {
		return true
	}
	for _, p := range paths {
		if strings.HasPrefix(p, rootDir+"/") {
			return true
		}
	}
	return false
}

func (h *Handler) processPush(ctx context.Context, ev source.Event) {
	app, err := h.store.AppByRepo(ev.Repo)
	if errors.Is(err, store.ErrNotFound) {
		log.Printf("webhook: no app bound to %s", ev.Repo)
		return
	}
	if err != nil {
		log.Printf("webhook: lookup %s: %v", ev.Repo, err)
		return
	}
	if ev.Ref != "refs/heads/"+app.Branch {
		log.Printf("webhook: %s ref %s != tracked %s", ev.Repo, ev.Ref, app.Branch)
		return
	}
	if !touchesRootDir(app.RootDir, ev.Paths) {
		log.Printf("webhook: %s push touched nothing under %s, skipping", app.Name, app.RootDir)
		return
	}

	lock := h.appLock(app.Name)
	lock.Lock()
	defer lock.Unlock()

	h.mu.Lock()
	dup := h.lastSHA[app.Name] == ev.SHA
	h.mu.Unlock()
	if dup {
		log.Printf("webhook: %s already at %s, skipping", app.Name, ev.SHA)
		return
	}

	_ = h.prov.Report(ctx, ev, source.StatusPending, "")

	dir, err := os.MkdirTemp("", "piper-src-*")
	if err != nil {
		log.Printf("webhook: tmpdir: %v", err)
		_ = h.prov.Report(ctx, ev, source.StatusFailure, "")
		return
	}
	defer os.RemoveAll(dir)

	if err := h.prov.Fetch(ctx, ev, dir); err != nil {
		log.Printf("webhook: fetch %s@%s: %v", ev.Repo, ev.SHA, err)
		_ = h.prov.Report(ctx, ev, source.StatusFailure, "")
		return
	}
	if _, err := h.deploy.Deploy(ctx, app.Name, dir); err != nil {
		log.Printf("webhook: deploy %s: %v", app.Name, err)
		_ = h.prov.Report(ctx, ev, source.StatusFailure, "")
		return
	}

	// Report the host the deploy actually routed, which the deployer recorded on
	// the app row, rather than guessing "<app>.<baseDom>". On a relay-terminated
	// box the routed host is a flattened single-label name the relay assigned;
	// the guess sits two labels under the apex, outside the relay's wildcard
	// certificate, so GitHub's Deployments tab would link somewhere unreachable.
	// Re-read after the deploy: the row read above predates it.
	url := "https://" + app.Name + "." + h.baseDom
	if routed, err := h.store.GetApp(app.Name); err == nil && routed.Hostname != "" {
		url = "https://" + routed.Hostname
	}
	_ = h.prov.Report(ctx, ev, source.StatusSuccess, url)

	h.mu.Lock()
	h.lastSHA[app.Name] = ev.SHA
	h.mu.Unlock()
}

func (h *Handler) processPreview(ctx context.Context, ev source.Event) {
	app, err := h.store.AppByRepo(ev.Repo)
	if errors.Is(err, store.ErrNotFound) {
		log.Printf("webhook: no app bound to %s", ev.Repo)
		return
	}
	if err != nil {
		log.Printf("webhook: lookup %s: %v", ev.Repo, err)
		return
	}

	lock := h.appLock(app.Name)
	lock.Lock()
	defer lock.Unlock()

	key := fmt.Sprintf("%s#%d", app.Name, ev.PR)
	h.mu.Lock()
	dup := h.lastSHA[key] == ev.SHA
	h.mu.Unlock()
	if dup {
		log.Printf("webhook: %s PR %d already at %s, skipping", app.Name, ev.PR, ev.SHA)
		return
	}

	_ = h.prov.Report(ctx, ev, source.StatusPending, "")

	dir, err := os.MkdirTemp("", "piper-src-*")
	if err != nil {
		log.Printf("webhook: tmpdir: %v", err)
		_ = h.prov.Report(ctx, ev, source.StatusFailure, "")
		return
	}
	defer os.RemoveAll(dir)

	if err := h.prov.Fetch(ctx, ev, dir); err != nil {
		log.Printf("webhook: fetch %s@%s: %v", ev.Repo, ev.SHA, err)
		_ = h.prov.Report(ctx, ev, source.StatusFailure, "")
		return
	}
	if _, err := h.deploy.DeployPreview(ctx, app.Name, ev.PR, dir); err != nil {
		log.Printf("webhook: preview deploy %s PR %d: %v", app.Name, ev.PR, err)
		_ = h.prov.Report(ctx, ev, source.StatusFailure, "")
		return
	}

	// Report the host the preview actually routed, for the same reason the push
	// path re-reads the app row: on a relay-terminated box the constructed
	// "pr-<N>-<app>.<baseDom>" sits two labels under the apex, outside the
	// relay's wildcard certificate and unknown to its router, so the Deployments
	// tab would link somewhere unreachable (#302).
	url := fmt.Sprintf("https://pr-%d-%s.%s", ev.PR, app.Name, h.baseDom)
	if routed, ok := h.deploy.PreviewHost(app.Name, ev.PR); ok {
		url = "https://" + routed
	}
	_ = h.prov.Report(ctx, ev, source.StatusSuccess, url)

	h.mu.Lock()
	h.lastSHA[key] = ev.SHA
	h.mu.Unlock()
}

func (h *Handler) processPRClosed(ctx context.Context, ev source.Event) {
	app, err := h.store.AppByRepo(ev.Repo)
	if errors.Is(err, store.ErrNotFound) {
		log.Printf("webhook: no app bound to %s", ev.Repo)
		return
	}
	if err != nil {
		log.Printf("webhook: lookup %s: %v", ev.Repo, err)
		return
	}

	lock := h.appLock(app.Name)
	lock.Lock()
	defer lock.Unlock()

	retired, err := h.deploy.TeardownPreview(ctx, app.Name, ev.PR)
	if err != nil {
		log.Printf("webhook: teardown %s PR %d: %v", app.Name, ev.PR, err)
		return
	}

	h.mu.Lock()
	delete(h.lastSHA, fmt.Sprintf("%s#%d", app.Name, ev.PR))
	h.mu.Unlock()

	// Only report the preview deployment inactive when a preview was actually
	// retired. With no running preview there's no pr-<N> deployment to mark, so
	// reporting would trigger a wasted deployments lookup whose "no deployment"
	// error is then swallowed.
	if retired {
		_ = h.prov.Report(ctx, ev, source.StatusInactive, "")
	}
}
