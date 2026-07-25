// Package deploy orchestrates building, running, health-checking, and routing an app.
package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/piperbox/piper/internal/runtime"
	"github.com/piperbox/piper/internal/store"
)

const deploymentCleanupTimeout = 5 * time.Second

// imageRetentionPerApp bounds how many of an app's most-recent built images the
// deployer keeps after a successful deploy; older piper/<app>:<ts> tags are
// GC'd so a Pi-class box's disk doesn't grow unbounded (#125). Generous enough
// to cover the live production image plus a couple of active PR previews (which
// share the app's image repo) with headroom.
const imageRetentionPerApp = 5

// logFlushInterval bounds how often a running build's growing log is persisted
// to its deployment row, so a slow build's output reaches pollers (the CLI and
// dashboard) without a store write per line.
const logFlushInterval = time.Second

// logSink is the progress io.Writer handed to the build: it accumulates output
// in a tail-capped buffer and flushes the whole buffer to the deployment's log
// column at most once per logFlushInterval. Written from a single goroutine
// (the deploy), so it needs no locking. The authoritative final log is written
// separately by FinalizeDeployment.
type logSink struct {
	buf       runtime.TailBuffer
	store     *store.Store
	id        string
	lastFlush time.Time
}

func (ls *logSink) Write(p []byte) (int, error) {
	n, err := ls.buf.Write(p)
	if time.Since(ls.lastFlush) >= logFlushInterval {
		_ = ls.store.UpdateDeploymentLogs(ls.id, ls.buf.String())
		ls.lastFlush = time.Now()
	}
	return n, err
}

type RouteSetter interface {
	UpsertRoute(host string, upstreamHostPort int) error
	UpsertRouteTLS(host string, upstreamHostPort int) error
	RemoveRoute(host string) error
}

// HostnameRegistrar assigns a relay-terminated public hostname for an app over
// the tunnel. In terminated (free-tier) mode the Deployer routes that hostname
// instead of "<app>.<baseDom>". Implemented by *agent.TunnelClient; injected by
// piperd. Nil in LAN / BYO-domain mode.
type HostnameRegistrar interface {
	Register(app string, pr int) (string, error)
	Deregister(hostname string) error
}

type Deployer struct {
	store     *store.Store
	runtime   runtime.Runtime
	routes    RouteSetter
	baseDom   string
	registrar HostnameRegistrar

	mu    sync.Mutex             // guards locks
	locks map[string]*sync.Mutex // per-app serialization of mutating ops
}

func New(s *store.Store, rt runtime.Runtime, routes RouteSetter, baseDomain string) *Deployer {
	return &Deployer{store: s, runtime: rt, routes: routes, baseDom: baseDomain, locks: map[string]*sync.Mutex{}}
}

// lockApp serializes every mutating operation for one app (deploy, preview,
// stop, delete) so they can't interleave: two concurrent deploys racing on
// routing/finalize, a deploy racing a delete into an orphan container + a
// resurrected route, or two same-second builds colliding on the image tag.
// The webhook path has its own appLock; this closes the API + delete paths.
// Returns the unlock func so callers can `defer d.lockApp(name)()`. #159, #121.
func (d *Deployer) lockApp(name string) func() {
	d.mu.Lock()
	m := d.locks[name]
	if m == nil {
		m = &sync.Mutex{}
		d.locks[name] = m
	}
	d.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// SetHostnameRegistrar puts the Deployer into relay-terminated mode: Deploy asks
// the registrar for each app's public hostname and routes that. Nil restores
// LAN/BYO behavior.
func (d *Deployer) SetHostnameRegistrar(r HostnameRegistrar) { d.registrar = r }

func (d *Deployer) hostFor(app string) string {
	return app + "." + d.baseDom
}

func (d *Deployer) hostForPreview(app string, pr int) string {
	return fmt.Sprintf("pr-%d-%s.%s", pr, app, d.baseDom)
}

// PreviewHost resolves the routed host for app's PR-pr preview, the mirror of
// primaryHost: the relay-assigned name in registrar mode (recovered via the
// idempotent Register), else "pr-<N>-<app>.<baseDom>". ok is false when the
// relay is unreachable and the hostname can't be recovered.
//
// A relay-terminated box must go through the registrar here. Its baseDom is
// already a label under the apex, so the constructed host sits two labels deep:
// outside the relay's single-label wildcard cert and absent from its router, so
// the preview is unreachable however healthy the container is (#302). LAN and
// BYO boxes have no registrar and keep the construction, which is correct there
// — their base domain is the apex.
func (d *Deployer) PreviewHost(appName string, pr int) (string, bool) {
	if d.registrar == nil {
		return d.hostForPreview(appName, pr), true
	}
	host, err := d.registrar.Register(appName, pr)
	return host, err == nil
}

func (d *Deployer) stopPartial(ctx context.Context, containerID string) {
	if containerID == "" {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deploymentCleanupTimeout)
	defer cancel()
	_ = d.runtime.Stop(cleanupCtx, containerID)
}

// buildRunHealthy builds, runs, and health-checks app, capturing one
// tail-capped log blob (build output, plus container output when the run or
// health check fails). When progress is non-nil (the production Finish path),
// stage-transition banners ("→ building image", etc.) and the build's live
// output are written to it as they happen, in addition to the returned log;
// previews pass nil and see no live output. On failure it invokes recordFailed
// with whatever ids and log are known so the caller persists a "failed" record
// for the right (app, pr) row, then returns a wrapped error.
func (d *Deployer) buildRunHealthy(ctx context.Context, app store.App, srcDir string, progress io.Writer, recordFailed func(imageID, containerID string, hostPort int, logs string)) (runtime.BuildResult, runtime.RunResult, string, error) {
	tag := fmt.Sprintf("piper/%s:%d", app.Name, time.Now().Unix())
	var log runtime.TailBuffer
	out := io.Writer(&log)
	if progress != nil {
		out = io.MultiWriter(&log, progress)
	}
	// For a monorepo-linked app, build from the configured subpath of the
	// checkout rather than its root (#316); an empty RootDir builds the root.
	buildDir := srcDir
	if app.RootDir != "" {
		buildDir = filepath.Join(srcDir, app.RootDir)
	}
	_, _ = io.WriteString(out, "→ building image\n")
	build, err := d.runtime.Build(ctx, buildDir, tag, progress)
	_, _ = io.WriteString(&log, build.Log)
	if err != nil {
		_, _ = io.WriteString(&log, "\nerror: "+err.Error()+"\n")
		recordFailed(build.ImageID, "", 0, log.String())
		return build, runtime.RunResult{}, log.String(), fmt.Errorf("build: %w", err)
	}
	_, _ = io.WriteString(out, "→ starting container\n")
	run, err := d.runtime.Run(ctx, tag, app.Port, map[string]string{"PORT": fmt.Sprint(app.Port)})
	if err != nil {
		d.appendContainerOutput(ctx, &log, run.ContainerID)
		_, _ = io.WriteString(&log, "\nerror: "+err.Error()+"\n")
		d.stopPartial(ctx, run.ContainerID)
		recordFailed(build.ImageID, run.ContainerID, run.HostPort, log.String())
		return build, run, log.String(), fmt.Errorf("run: %w", err)
	}
	_, _ = io.WriteString(out, "→ health-checking\n")
	if err := d.runtime.WaitHealthy(ctx, run.HostPort); err != nil {
		d.appendContainerOutput(ctx, &log, run.ContainerID)
		_, _ = io.WriteString(&log, "\nerror: "+err.Error()+"\n")
		d.stopPartial(ctx, run.ContainerID)
		recordFailed(build.ImageID, run.ContainerID, run.HostPort, log.String())
		return build, run, log.String(), fmt.Errorf("health: %w", err)
	}
	return build, run, log.String(), nil
}

// appendContainerOutput best-effort appends the container's stdout/stderr to
// log; it must run before stopPartial removes the container. A fetch failure
// never masks the deploy error. Detached context so a cancelled deploy can
// still capture (same rationale as stopPartial).
func (d *Deployer) appendContainerOutput(ctx context.Context, log io.Writer, containerID string) {
	if containerID == "" {
		return
	}
	logCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deploymentCleanupTimeout)
	defer cancel()
	rc, err := d.runtime.Logs(logCtx, containerID)
	if err != nil {
		return
	}
	defer rc.Close()
	_, _ = io.WriteString(log, "\n--- container output ---\n")
	_, _ = io.Copy(log, rc)
}

// Begin opens a building deployment row for appName and returns it; its id is
// what an async caller (the deploy API) hands back before the build finishes.
func (d *Deployer) Begin(appName string) (store.Deployment, error) {
	if _, err := d.store.GetApp(appName); err != nil {
		return store.Deployment{}, err
	}
	return d.store.CreateDeployment(appName, "", "", 0, "building", "")
}

// Finish builds, runs, health-checks, and routes dep's app from srcDir,
// streaming the build's output into dep's log and finalizing the row
// running/failed. On build/run/health failure it finalizes the same row failed.
// It serializes per app against other deploys/deletes (see lockApp).
func (d *Deployer) Finish(ctx context.Context, dep store.Deployment, srcDir string) error {
	defer d.lockApp(dep.App)()
	return d.finish(ctx, dep, srcDir)
}

// finish is Finish without the per-app lock, for callers that already hold it
// (Deploy). Never call it directly from an unlocked path.
func (d *Deployer) finish(ctx context.Context, dep store.Deployment, srcDir string) (retErr error) {
	app, err := d.store.GetApp(dep.App)
	if err != nil {
		return err
	}
	previous, err := d.store.LatestRunning(dep.App)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}

	sink := &logSink{store: d.store, id: dep.ID}
	build, run, logs, err := d.buildRunHealthy(ctx, app, srcDir, sink, func(img, cid string, hp int, logs string) {
		_ = d.store.FinalizeDeployment(dep.ID, img, cid, hp, "failed", logs)
	})
	if err != nil {
		return err
	}

	// A container is now running. From here a panic in routing/finalize (a nil
	// registrar, a Caddy client bug) would orphan it, so recover, stop it, and
	// finalize the row "failed" rather than leaking it (#162). The deploy layer
	// owns this cleanup because only it knows the container id — the api
	// goroutine's recover can't.
	defer func() {
		if r := recover(); r != nil {
			d.stopPartial(ctx, run.ContainerID)
			logs += fmt.Sprintf("\ndeploy panicked: %v\n", r)
			_ = d.store.FinalizeDeployment(dep.ID, build.ImageID, run.ContainerID, run.HostPort, "failed", logs)
			retErr = fmt.Errorf("deploy panicked: %v", r)
		}
	}()

	// Route BEFORE marking the row "running": if routing fails, the app isn't
	// reachable yet, so the row must finalize "failed" rather than report a
	// success the CLI/dashboard would trust.
	host := d.hostFor(dep.App)
	if d.registrar != nil {
		host, err = d.registrar.Register(dep.App, 0)
		if err != nil {
			return d.failFinish(ctx, dep.ID, build.ImageID, run.ContainerID, run.HostPort, logs, fmt.Errorf("register hostname: %w", err))
		}
	}
	if err := d.routes.UpsertRoute(host, run.HostPort); err != nil {
		return d.failFinish(ctx, dep.ID, build.ImageID, run.ContainerID, run.HostPort, logs, fmt.Errorf("route: %w", err))
	}
	// Record the host the app is now served on so the apps API (#100) and deploy
	// response (#93) can report the real URL, not a guessed one.
	if err := d.store.SetAppHostname(dep.App, host); err != nil {
		return d.failFinish(ctx, dep.ID, build.ImageID, run.ContainerID, run.HostPort, logs, fmt.Errorf("record hostname: %w", err))
	}
	// An active BYO custom domain (#102) serves the app at <app>.<custom> over
	// the box-terminated :443 alongside the primary host. This is a secondary
	// hostname: the primary route is already live and the domain manager re-arms
	// custom-domain routes on renewal/resume, so a transient Caddy error here is
	// logged and skipped rather than failing an otherwise-successful deploy (#115).
	if dc, err := d.store.GetDomainConfig(); err == nil && dc.Status == "active" {
		host := dep.App + "." + dc.Domain
		if err := d.routes.UpsertRouteTLS(host, run.HostPort); err != nil {
			log.Printf("deploy %s: custom-domain route %s (deploy still succeeded on primary): %v", dep.App, host, err)
		}
	}
	// Per-app custom domains (#224) are exact hosts owned by this app; each
	// active one is served over the box-terminated :443 alongside the primary
	// host. Same secondary-hostname stance as above (#115).
	if doms, err := d.store.ListAppDomains(dep.App); err != nil {
		log.Printf("deploy %s: list app domains (deploy still succeeded on primary): %v", dep.App, err)
	} else {
		for _, ad := range doms {
			if ad.Status != "active" {
				continue
			}
			if err := d.routes.UpsertRouteTLS(ad.Domain, run.HostPort); err != nil {
				log.Printf("deploy %s: app-domain route %s (deploy still succeeded on primary): %v", dep.App, ad.Domain, err)
			}
		}
	}

	if err := d.store.FinalizeDeployment(dep.ID, build.ImageID, run.ContainerID, run.HostPort, "running", logs); err != nil {
		return err
	}
	if previous.ContainerID != "" && previous.ContainerID != run.ContainerID {
		_ = d.runtime.Stop(ctx, previous.ContainerID)
		_ = d.store.UpdateDeploymentStatus(previous.ID, "stopped")
	}
	// GC superseded images so per-deploy tags don't fill a Pi-class box's disk;
	// best-effort, and the newest (this deploy's, in use) is always kept (#125).
	if err := d.runtime.PruneAppImages(ctx, dep.App, imageRetentionPerApp); err != nil {
		log.Printf("deploy %s: prune images: %v", dep.App, err)
	}
	return nil
}

// failFinish finalizes dep's row "failed" after a routing error, appending the
// error to logs, and returns the wrapped error unchanged. It best-effort stops
// the just-started container: the row is unreachable, so leaving the container
// running would orphan it (#162).
func (d *Deployer) failFinish(ctx context.Context, id, imageID, containerID string, hostPort int, logs string, wrapped error) error {
	d.stopPartial(ctx, containerID)
	logs += "\nerror: " + wrapped.Error() + "\n"
	_ = d.store.FinalizeDeployment(id, imageID, containerID, hostPort, "failed", logs)
	return wrapped
}

// Deploy is the synchronous Begin+Finish used by the webhook path; it returns
// the finalized deployment.
func (d *Deployer) Deploy(ctx context.Context, appName, srcDir string) (store.Deployment, error) {
	defer d.lockApp(appName)()
	dep, err := d.Begin(appName)
	if err != nil {
		return store.Deployment{}, err
	}
	if err := d.finish(ctx, dep, srcDir); err != nil {
		return store.Deployment{}, err
	}
	return d.store.LatestDeployment(appName)
}

func (d *Deployer) DeployPreview(ctx context.Context, appName string, pr int, srcDir string) (store.Deployment, error) {
	defer d.lockApp(appName)()
	app, err := d.store.GetApp(appName)
	if err != nil {
		return store.Deployment{}, err
	}
	previous, err := d.store.PreviewRunning(appName, pr)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return store.Deployment{}, err
	}

	build, run, logs, err := d.buildRunHealthy(ctx, app, srcDir, nil, func(img, cid string, hp int, logs string) {
		_, _ = d.store.CreatePreviewDeployment(appName, pr, img, cid, hp, "failed", logs)
	})
	if err != nil {
		return store.Deployment{}, err
	}

	dep, err := d.store.CreatePreviewDeployment(appName, pr, build.ImageID, run.ContainerID, run.HostPort, "running", logs)
	if err != nil {
		return store.Deployment{}, err
	}
	host, ok := d.PreviewHost(appName, pr)
	if !ok {
		d.stopPartial(ctx, run.ContainerID)
		_ = d.store.UpdateDeploymentStatus(dep.ID, "failed")
		return store.Deployment{}, fmt.Errorf("register preview hostname for %s PR %d", appName, pr)
	}
	if err := d.routes.UpsertRoute(host, run.HostPort); err != nil {
		// The container is running and the row is "running", but with no route
		// it's unreachable. Unwind rather than leak both: best-effort stop the
		// container and mark the row "failed" (mirrors failFinish on the Deploy
		// path, #157).
		d.stopPartial(ctx, run.ContainerID)
		_ = d.store.UpdateDeploymentStatus(dep.ID, "failed")
		return store.Deployment{}, fmt.Errorf("route: %w", err)
	}
	if previous.ContainerID != "" && previous.ContainerID != run.ContainerID {
		_ = d.runtime.Stop(ctx, previous.ContainerID)
		_ = d.store.UpdateDeploymentStatus(previous.ID, "stopped")
	}
	return dep, nil
}

// TeardownPreview retires PR pr's running preview: stop its container, drop its
// route, mark the row "stopped". retired reports whether a running preview was
// actually retired — false when nothing was running — so the caller only posts
// an "inactive" deployment status for a preview that existed. When the store row
// is already gone the route may have outlived it (e.g. a crash between routing
// and the row write), so RemoveRoute is still attempted best-effort; Caddy's
// RemoveRoute tolerates a missing route, so a genuinely absent one is a no-op.
func (d *Deployer) TeardownPreview(ctx context.Context, appName string, pr int) (retired bool, err error) {
	defer d.lockApp(appName)()
	dep, err := d.store.PreviewRunning(appName, pr)
	host, hostOK := d.PreviewHost(appName, pr)
	if errors.Is(err, store.ErrNotFound) {
		if hostOK {
			if rerr := d.routes.RemoveRoute(host); rerr != nil {
				log.Printf("teardown %s PR %d: remove orphan route: %v", appName, pr, rerr)
			}
			d.deregisterPreview(host)
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !hostOK {
		return false, fmt.Errorf("resolve preview hostname for %s PR %d", appName, pr)
	}
	_ = d.runtime.Stop(ctx, dep.ContainerID)
	if err := d.routes.RemoveRoute(host); err != nil {
		return false, fmt.Errorf("unroute: %w", err)
	}
	if err := d.store.UpdateDeploymentStatus(dep.ID, "stopped"); err != nil {
		return false, err
	}
	d.deregisterPreview(host)
	return true, nil
}

// deregisterPreview releases a closed PR's relay-side hostname. Best-effort:
// the preview is already stopped and unrouted, so a relay blip must not fail
// the teardown — the row is re-derived deterministically if the PR reopens.
func (d *Deployer) deregisterPreview(host string) {
	if d.registrar == nil {
		return
	}
	if err := d.registrar.Deregister(host); err != nil {
		log.Printf("teardown: deregister %s: %v", host, err)
	}
}

// primaryHost resolves the app's routed production host: the relay-assigned
// name in registrar mode (recovered via the idempotent Register), else
// <app>.<baseDom>. ok is false when the relay is unreachable and the
// hostname can't be recovered — callers skip route work best-effort.
func (d *Deployer) primaryHost(appName string) (string, bool) {
	if d.registrar == nil {
		return d.hostFor(appName), true
	}
	host, err := d.registrar.Register(appName, 0)
	return host, err == nil
}

// ResumeRoutes re-arms the primary host route for every app whose latest
// production deployment is running. piperd embeds Caddy and reloads a bare base
// config on every start, so a restart drops every route added since the last
// one. The domain manager's resume restores custom domains; nothing restored
// the app's own host, so a still-running app answered Caddy's empty-200
// no-route fallback until it was redeployed (#371) — the mirror, one layer
// down, of the relay forgetting the same hostnames on reconnect (#369).
//
// Best-effort per app: one app's failure must not strand the rest. In
// relay-terminated mode primaryHost resolves through the registrar, so callers
// run this after the tunnel connects; an unreachable relay leaves the route
// unarmed for the next attempt rather than routing a wrong host.
func (d *Deployer) ResumeRoutes() {
	apps, err := d.store.ListApps()
	if err != nil {
		log.Printf("resume routes: list apps: %v", err)
		return
	}
	for _, a := range apps {
		dep, err := d.store.LatestRunning(a.Name)
		if errors.Is(err, store.ErrNotFound) {
			continue // stopped, failed, or never deployed: nothing to route
		}
		if err != nil {
			log.Printf("resume routes: %s: %v", a.Name, err)
			continue
		}
		host, ok := d.primaryHost(a.Name)
		if !ok {
			log.Printf("resume routes: %s: cannot resolve hostname (relay unreachable?)", a.Name)
			continue
		}
		if err := d.routes.UpsertRoute(host, dep.HostPort); err != nil {
			log.Printf("resume routes: %s -> %s: %v", a.Name, host, err)
		}
	}
	d.resumePreviewRoutes()
}

// resumePreviewRoutes is the preview half of the sweep. It is a separate query
// rather than part of the loop above because LatestRunning is pr=0 by design;
// previews live on their own (app, pr) rows and an app may have several at
// once. Same best-effort contract as ResumeRoutes (#376).
func (d *Deployer) resumePreviewRoutes() {
	previews, err := d.store.RunningPreviews()
	if err != nil {
		log.Printf("resume routes: list previews: %v", err)
		return
	}
	for _, dep := range previews {
		host, ok := d.PreviewHost(dep.App, dep.PR)
		if !ok {
			log.Printf("resume routes: %s PR %d: cannot resolve hostname (relay unreachable?)", dep.App, dep.PR)
			continue
		}
		if err := d.routes.UpsertRoute(host, dep.HostPort); err != nil {
			log.Printf("resume routes: %s PR %d -> %s: %v", dep.App, dep.PR, host, err)
		}
	}
}

// RouteAppDomain arms the exact-host TLS route for a per-app custom domain
// that goes active while its app is already running (the domain manager's
// backfill; a deploy arms routes itself). Nothing running is a no-op — the
// next deploy adds the route.
func (d *Deployer) RouteAppDomain(appName, domain string) error {
	defer d.lockApp(appName)()
	dep, err := d.store.LatestRunning(appName)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return d.routes.UpsertRouteTLS(domain, dep.HostPort)
}

// removeCustomDomainRoute drops the app's custom-domain routes: <app>.<custom>
// when the box-wide BYO domain is active, plus each active per-app domain
// (mirrors the upserts in finish). Without any it's a no-op.
func (d *Deployer) removeCustomDomainRoute(appName string) error {
	if dc, err := d.store.GetDomainConfig(); err == nil && dc.Status == "active" {
		if err := d.routes.RemoveRoute(appName + "." + dc.Domain); err != nil {
			return fmt.Errorf("unroute custom domain: %w", err)
		}
	}
	doms, err := d.store.ListAppDomains(appName)
	if err != nil {
		return err
	}
	for _, ad := range doms {
		if ad.Status != "active" {
			continue
		}
		if err := d.routes.RemoveRoute(ad.Domain); err != nil {
			return fmt.Errorf("unroute app domain: %w", err)
		}
	}
	return nil
}

// Stop retires the app's running production container: stop it, drop its
// routes, mark the deployment "stopped". The app and its history remain.
// Nothing running is a no-op; previews are untouched. The relay keeps the
// app's hostname registration (Deregister is delete-only).
func (d *Deployer) Stop(ctx context.Context, appName string) error {
	defer d.lockApp(appName)()
	if _, err := d.store.GetApp(appName); err != nil {
		return err
	}
	dep, err := d.store.LatestRunning(appName)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	_ = d.runtime.Stop(ctx, dep.ContainerID)
	// The container is down, so the row must say so even if the route teardown
	// below fails: Start no-ops on anything but a "stopped" row, so returning
	// early here left the app stopped-but-marked-running and unstartable from
	// every UI — only a redeploy rewrote the row (#377). A stale route pointing
	// at a dead upstream is the lesser evil and the next deploy or stop clears
	// it. The failure is still reported, just after the row is honest.
	var routeErr error
	if host, ok := d.primaryHost(appName); ok {
		if err := d.routes.RemoveRoute(host); err != nil {
			routeErr = fmt.Errorf("unroute: %w", err)
		}
	}
	if err := d.removeCustomDomainRoute(appName); err != nil && routeErr == nil {
		routeErr = err
	}
	if err := d.store.UpdateDeploymentStatus(dep.ID, "stopped"); err != nil {
		return err
	}
	return routeErr
}

// addCustomDomainRoute re-arms the app's custom-domain routes over the
// box-terminated :443 — <app>.<custom> when the box-wide BYO domain is active,
// plus each active per-app domain. The inverse of removeCustomDomainRoute and
// mirror of finish's post-deploy arming; these are secondary hostnames, so a
// Caddy error is logged and skipped rather than failing the start (#115).
func (d *Deployer) addCustomDomainRoute(appName string, hostPort int) {
	if dc, err := d.store.GetDomainConfig(); err == nil && dc.Status == "active" {
		host := appName + "." + dc.Domain
		if err := d.routes.UpsertRouteTLS(host, hostPort); err != nil {
			log.Printf("start %s: custom-domain route %s: %v", appName, host, err)
		}
	}
	doms, err := d.store.ListAppDomains(appName)
	if err != nil {
		log.Printf("start %s: list app domains: %v", appName, err)
		return
	}
	for _, ad := range doms {
		if ad.Status != "active" {
			continue
		}
		if err := d.routes.UpsertRouteTLS(ad.Domain, hostPort); err != nil {
			log.Printf("start %s: app-domain route %s: %v", appName, ad.Domain, err)
		}
	}
}

// Start brings a stopped app back up: re-run its latest deployment's built image
// as a fresh container, health-check it, re-add the primary host route and
// re-attach any active custom-domain routes, and flip that same deployment row
// back to "running". The inverse of Stop — but a re-run, not a docker restart,
// because Stop removes the container; the image survives because Stop doesn't
// prune it. A no-op when the latest deployment isn't "stopped" (already running,
// failed, or never deployed): there is nothing to start. On a run/route failure
// the container is cleaned up and the row stays "stopped", so a retry is safe.
func (d *Deployer) Start(ctx context.Context, appName string) error {
	defer d.lockApp(appName)()
	app, err := d.store.GetApp(appName)
	if err != nil {
		return err
	}
	dep, err := d.store.LatestDeployment(appName)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if dep.Status != "stopped" {
		return nil
	}
	run, err := d.runtime.Run(ctx, dep.ImageID, app.Port, map[string]string{"PORT": fmt.Sprint(app.Port)})
	if err != nil {
		d.stopPartial(ctx, run.ContainerID)
		return fmt.Errorf("run: %w", err)
	}
	if err := d.runtime.WaitHealthy(ctx, run.HostPort); err != nil {
		d.stopPartial(ctx, run.ContainerID)
		return fmt.Errorf("health: %w", err)
	}
	host, ok := d.primaryHost(appName)
	if !ok {
		d.stopPartial(ctx, run.ContainerID)
		return fmt.Errorf("resolve hostname for %s", appName)
	}
	if err := d.routes.UpsertRoute(host, run.HostPort); err != nil {
		d.stopPartial(ctx, run.ContainerID)
		return fmt.Errorf("route: %w", err)
	}
	if err := d.store.SetAppHostname(appName, host); err != nil {
		d.stopPartial(ctx, run.ContainerID)
		return fmt.Errorf("record hostname: %w", err)
	}
	d.addCustomDomainRoute(appName, run.HostPort)
	logs, _ := d.store.DeploymentLogs(appName, dep.ID)
	return d.store.FinalizeDeployment(dep.ID, dep.ImageID, run.ContainerID, run.HostPort, "running", logs)
}

// Delete tears the app down completely: stops every running deployment
// (production and previews), drops all its routes, releases the relay
// hostname, and deletes the app plus its whole deployment history. Relay
// steps are best-effort; state is deleted last so a failed teardown leaves
// delete retryable.
func (d *Deployer) Delete(ctx context.Context, appName string) error {
	defer d.lockApp(appName)()
	if _, err := d.store.GetApp(appName); err != nil {
		return err
	}
	deps, err := d.store.ListDeployments(appName)
	if err != nil {
		return err
	}
	for _, dep := range deps {
		if dep.Status != "running" {
			continue
		}
		_ = d.runtime.Stop(ctx, dep.ContainerID)
		if dep.PR > 0 {
			host, ok := d.PreviewHost(appName, dep.PR)
			if !ok {
				return fmt.Errorf("resolve preview hostname for %s PR %d", appName, dep.PR)
			}
			if err := d.routes.RemoveRoute(host); err != nil {
				return fmt.Errorf("unroute: %w", err)
			}
			d.deregisterPreview(host)
		}
	}
	if host, ok := d.primaryHost(appName); ok {
		if err := d.routes.RemoveRoute(host); err != nil {
			return fmt.Errorf("unroute: %w", err)
		}
		if d.registrar != nil {
			_ = d.registrar.Deregister(host)
		}
	}
	if err := d.removeCustomDomainRoute(appName); err != nil {
		return err
	}
	if err := d.store.DeleteApp(appName); err != nil {
		return err
	}
	// The app is gone: drop all its built images (keep none).
	if err := d.runtime.PruneAppImages(ctx, appName, 0); err != nil {
		log.Printf("delete %s: prune images: %v", appName, err)
	}
	return nil
}
