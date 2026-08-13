// Package api exposes piperd's HTTP control plane.
package api

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/piperbox/piper/internal/domain"
	"github.com/piperbox/piper/internal/source"
	"github.com/piperbox/piper/internal/source/github"
	"github.com/piperbox/piper/internal/store"
	"github.com/piperbox/piper/internal/version"
)

// envKeyRE is the accepted env var name shape; PORT is additionally reserved.
var envKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type Deployerer interface {
	Begin(app string) (store.Deployment, error)
	Finish(ctx context.Context, dep store.Deployment, srcDir string) error
	Stop(ctx context.Context, app string) error
	Start(ctx context.Context, app string) error
	Delete(ctx context.Context, app string) error
}

// App is the wire shape of an app in API responses: the stored app plus the
// status of its latest non-preview deployment — exactly one of "building",
// "running", "failed", "stopped" — or "" when never deployed; except a
// "failed" latest deployment still reports "running" when an older
// deployment is still up and serving.
type App struct {
	store.App
	Status string
	// Scheme is the URL scheme this box serves the app on, "http" or "https".
	// The daemon is the only party that can answer it: a client reaches a
	// relay-backed box over the relay and a directly-served box over the LAN
	// (#507), so how the client dialled stopped being a usable proxy for
	// whether the app itself is on TLS.
	Scheme string
}

// appScheme names the URL scheme this box's apps are served on.
func appScheme(self AgentInfo) string {
	if self.AppsOverHTTPS {
		return "https"
	}
	return "http"
}

// latestStatus resolves the App.Status for one app; never-deployed is "".
func latestStatus(s *store.Store, app string) (string, error) {
	d, err := s.LatestDeployment(app)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if d.Status == "failed" {
		if _, err := s.LatestRunning(app); err == nil {
			return "running", nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return "", err
		}
	}
	return d.Status, nil
}

// DomainManager is the domain-config surface: the box-wide custom domain
// (#102) and the per-app domains collection (#231). Nil when the box has no
// relay configured: the endpoints then answer 409.
type DomainManager interface {
	Set(domain, provider, token, serve string) (domain.Status, error)
	Status() (domain.Status, error)
	Remove() error
	AddAppDomain(app, domain string) (store.AppDomain, error)
	RemoveAppDomain(domain string) error
	AppDomainStatus(domain string) (domain.AppDomainStatus, error)
	AppDomainStatuses(app string) ([]domain.AppDomainStatus, error)
}

// RepoBinder tells the relay which repository an app deploys from, so brokered
// webhooks can be routed to this box. Nil on LAN-only boxes.
type RepoBinder interface {
	BindRepo(app, repo, branch string) error
}

// FetchRepoFunc downloads the tree of repo at ref into destDir, so a linked
// app can be deployed on demand rather than only on a webhook push. Nil (or
// returning ErrNoGitHubApp) when the box has no GitHub credential source; the
// deploy-from-repo endpoint then answers 409.
type FetchRepoFunc func(ctx context.Context, repo, ref, destDir string) error

// ErrNoGitHubApp is returned by a FetchRepoFunc when no GitHub credential
// source is configured at call time.
var ErrNoGitHubApp = errors.New("no GitHub App configured — run `piper github setup` first")

// AgentInfo is the daemon's self-report beyond its version: the listen config
// it actually loaded. `agent status` can't always read the process env (the
// system piperd's /proc environ is root-only), so the daemon states it (#476).
type AgentInfo struct {
	HTTPAddr  string
	HTTPSAddr string
	DataDir   string
	// AppsOverHTTPS records that the public reaches this box's apps on TLS —
	// terminated by the relay on a free-tier box, by the box itself on a BYO
	// relay box or a never-enrolled direct one (#507). It sets App.Scheme.
	AppsOverHTTPS bool
}

// onGitHubApp, if non-nil, is invoked after a GitHub App is configured via the
// exchange endpoint, so the daemon can start serving webhooks without a restart.
// nextGitHubProvider, if non-nil, names the webhook credential source the box
// would pick with no App stored locally; the reset endpoint reports it so the
// operator learns whether anything takes over. Nil answers "unknown".
func New(s *store.Store, d Deployerer, baseDomain, githubAPIBase string, onGitHubApp func(), dom DomainManager, binder RepoBinder, nextGitHubProvider func() string, fetchRepo FetchRepoFunc, self AgentInfo) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/apps", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name string `json:"name"`
			Port int    `json:"port"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Name == "" {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		if in.Name == "hooks" {
			http.Error(w, "name reserved", http.StatusBadRequest)
			return
		}
		if !validAppName(in.Name) {
			http.Error(w, "invalid app name: use a DNS label (lowercase letters, digits, and hyphens; 1-63 chars; no leading/trailing hyphen)", http.StatusBadRequest)
			return
		}
		if in.Port == 0 {
			in.Port = 8080
		}
		if _, err := s.GetApp(in.Name); err == nil {
			http.Error(w, "app exists", http.StatusConflict)
			return
		} else if !errors.Is(err, store.ErrNotFound) {
			serverError(w, r, err)
			return
		}
		app, err := s.CreateApp(in.Name, in.Port)
		if err != nil {
			serverError(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, App{App: app, Scheme: appScheme(self)})
	})
	// The running daemon's own build. Nothing else can answer this: the piperd
	// binary on disk may already have been replaced by an upgrade that has not
	// been restarted into, and the piper asking is a separate build on a
	// separate machine. Without it a half-applied upgrade is indistinguishable
	// from a fix that did not work (#375). The same holds for the listen
	// config it loaded — see AgentInfo (#476).
	mux.HandleFunc("GET /v1/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"version":    version.String(),
			"http_addr":  self.HTTPAddr,
			"https_addr": self.HTTPSAddr,
			"data_dir":   self.DataDir,
		})
	})
	mux.HandleFunc("GET /v1/apps", func(w http.ResponseWriter, r *http.Request) {
		apps, err := s.ListApps()
		if err != nil {
			serverError(w, r, err)
			return
		}
		out := make([]App, 0, len(apps))
		for _, a := range apps {
			status, err := latestStatus(s, a.Name)
			if err != nil {
				serverError(w, r, err)
				return
			}
			out = append(out, App{App: a, Status: status, Scheme: appScheme(self)})
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /v1/apps/{name}", func(w http.ResponseWriter, r *http.Request) {
		app, err := s.GetApp(r.PathValue("name"))
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			serverError(w, r, err)
			return
		}
		status, err := latestStatus(s, app.Name)
		if err != nil {
			serverError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, App{App: app, Status: status, Scheme: appScheme(self)})
	})
	mux.HandleFunc("GET /v1/apps/{name}/deployments", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if _, err := s.GetApp(name); errors.Is(err, store.ErrNotFound) {
			http.Error(w, "unknown app", http.StatusNotFound)
			return
		} else if err != nil {
			serverError(w, r, err)
			return
		}
		deps, err := s.ListDeployments(name)
		if err != nil {
			serverError(w, r, err)
			return
		}
		if deps == nil {
			deps = []store.Deployment{}
		}
		writeJSON(w, http.StatusOK, deps)
	})
	mux.HandleFunc("GET /v1/apps/{name}/deployments/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		logs, err := s.DeploymentLogs(r.PathValue("name"), r.PathValue("id"))
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "unknown deployment", http.StatusNotFound)
			return
		}
		if err != nil {
			serverError(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, logs)
	})
	mux.HandleFunc("POST /v1/apps/{name}/deploy", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if _, err := s.GetApp(name); errors.Is(err, store.ErrNotFound) {
			http.Error(w, "unknown app", http.StatusNotFound)
			return
		} else if err != nil {
			serverError(w, r, err)
			return
		}
		dir, err := os.MkdirTemp("", "piper-src-*")
		if err != nil {
			serverError(w, r, err)
			return
		}
		if err := untar(r.Body, dir); err != nil {
			os.RemoveAll(dir)
			http.Error(w, "bad tar: "+err.Error(), http.StatusBadRequest)
			return
		}
		dep, err := d.Begin(name)
		if err != nil {
			os.RemoveAll(dir)
			serverError(w, r, err)
			return
		}
		// Deploy runs past this request: own the temp dir and use a background
		// context, since r.Context() is cancelled once the 202 is written. The
		// build outcome is observed by polling the deployment's status + logs.
		go func() {
			defer os.RemoveAll(dir)
			defer func() {
				if r := recover(); r != nil {
					log.Printf("deploy %s: panic: %v", name, r)
					_ = s.FinalizeDeployment(dep.ID, "", "", 0, "failed", fmt.Sprintf("deploy panicked: %v", r))
				}
			}()
			if err := d.Finish(context.Background(), dep, dir); err != nil {
				log.Printf("deploy %s: %v", name, err)
			}
		}()
		writeJSON(w, http.StatusAccepted, dep)
	})
	mux.HandleFunc("POST /v1/apps/{name}/deploy-from-repo", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		app, err := s.GetApp(name)
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "unknown app", http.StatusNotFound)
			return
		} else if err != nil {
			serverError(w, r, err)
			return
		}
		if app.Repo == "" {
			http.Error(w, "app is not linked to a repository — run `piper app link` first", http.StatusConflict)
			return
		}
		if fetchRepo == nil {
			http.Error(w, ErrNoGitHubApp.Error(), http.StatusConflict)
			return
		}
		dir, err := os.MkdirTemp("", "piper-src-*")
		if err != nil {
			serverError(w, r, err)
			return
		}
		if err := fetchRepo(r.Context(), app.Repo, app.Branch, dir); err != nil {
			os.RemoveAll(dir)
			if errors.Is(err, ErrNoGitHubApp) {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
			http.Error(w, "fetch "+app.Repo+"@"+app.Branch+": "+err.Error(), http.StatusBadGateway)
			return
		}
		dep, err := d.Begin(name)
		if err != nil {
			os.RemoveAll(dir)
			serverError(w, r, err)
			return
		}
		// Same contract as the tarball deploy above: the build runs past this
		// request, so own the temp dir and use a background context.
		go func() {
			defer os.RemoveAll(dir)
			defer func() {
				if r := recover(); r != nil {
					log.Printf("deploy %s: panic: %v", name, r)
					_ = s.FinalizeDeployment(dep.ID, "", "", 0, "failed", fmt.Sprintf("deploy panicked: %v", r))
				}
			}()
			if err := d.Finish(context.Background(), dep, dir); err != nil {
				log.Printf("deploy %s: %v", name, err)
			}
		}()
		writeJSON(w, http.StatusAccepted, dep)
	})
	mux.HandleFunc("POST /v1/apps/{name}/link", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Repo    string `json:"repo"`
			Branch  string `json:"branch"`
			RootDir string `json:"root_dir"` // optional monorepo build subpath
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Repo == "" || in.Branch == "" {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		// Everything downstream of a link assumes owner/name — the relay cuts
		// the owner off to match installation targets, push routing compares
		// against the payload's full_name, and tarball fetch builds
		// /repos/<repo>/tarball/<ref>. A bare name breaks all three silently,
		// so it is rejected here rather than stored (#333).
		if !source.ValidRepo(in.Repo) {
			http.Error(w, "repo must be "+source.RepoShape+" (e.g. octocat/hello-world)", http.StatusBadRequest)
			return
		}
		rootDir, ok := cleanRootDir(in.RootDir)
		if !ok {
			http.Error(w, "root_dir must be a relative path within the repo", http.StatusBadRequest)
			return
		}
		name := r.PathValue("name")
		if err := s.UpdateAppRepo(name, in.Repo, in.Branch, rootDir); errors.Is(err, store.ErrNotFound) {
			http.Error(w, "unknown app", http.StatusNotFound)
			return
		} else if err != nil {
			serverError(w, r, err)
			return
		}
		if binder != nil {
			if err := binder.BindRepo(name, in.Repo, in.Branch); err != nil {
				// The local binding is authoritative and already stored; a relay
				// that is briefly unreachable must not fail the link. The binding
				// is re-pushed when the tunnel reconnects.
				log.Printf("api: register binding for %s with relay: %v", name, err)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /v1/apps/{name}/stop", func(w http.ResponseWriter, r *http.Request) {
		if err := d.Stop(r.Context(), r.PathValue("name")); errors.Is(err, store.ErrNotFound) {
			http.Error(w, "unknown app", http.StatusNotFound)
			return
		} else if err != nil {
			serverError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /v1/apps/{name}/start", func(w http.ResponseWriter, r *http.Request) {
		if err := d.Start(r.Context(), r.PathValue("name")); errors.Is(err, store.ErrNotFound) {
			http.Error(w, "unknown app", http.StatusNotFound)
			return
		} else if err != nil {
			serverError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /v1/apps/{name}", func(w http.ResponseWriter, r *http.Request) {
		// Tear down the app's per-app custom domains through the manager first
		// (#267): store.DeleteApp cascades the rows away, so this is the last
		// moment anything can still release the relay claims, loaded certs, and
		// cert dirs. A mandatory removal failure aborts the delete — the app
		// (and its rows) survive, so the delete stays retryable.
		if dom != nil {
			domains, err := s.ListAppDomains(r.PathValue("name"))
			if err != nil {
				serverError(w, r, err)
				return
			}
			for _, ad := range domains {
				if err := dom.RemoveAppDomain(ad.Domain); err != nil {
					serverError(w, r, err)
					return
				}
			}
		}
		if err := d.Delete(r.Context(), r.PathValue("name")); errors.Is(err, store.ErrNotFound) {
			http.Error(w, "unknown app", http.StatusNotFound)
			return
		} else if err != nil {
			serverError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /v1/github/manifest", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			RedirectURL string `json:"redirect_url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.RedirectURL == "" {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		manifest, err := github.BuildManifest("piper-"+baseDomain, "https://hooks."+baseDomain, in.RedirectURL)
		if err != nil {
			serverError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"manifest": string(manifest)})
	})
	mux.HandleFunc("POST /v1/github/exchange", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Code string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Code == "" {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		creds, err := github.ExchangeCode(r.Context(), githubAPIBase, in.Code)
		if err != nil {
			log.Printf("api: %s %s: github exchange: %v", r.Method, r.URL.Path, err)
			http.Error(w, "github code exchange failed", http.StatusBadGateway)
			return
		}
		if err := s.SaveGitHubApp(store.GitHubApp{
			AppID: creds.AppID, Slug: creds.Slug, PrivateKey: creds.PrivateKeyPEM, WebhookSecret: creds.WebhookSecret,
		}); err != nil {
			serverError(w, r, err)
			return
		}
		if onGitHubApp != nil {
			onGitHubApp()
		}
		// Return the created App's slug so the caller can deep-link its install
		// page (https://github.com/apps/<slug>/installations/new) instead of
		// hunting for it in GitHub's installed-apps list.
		writeJSON(w, http.StatusOK, map[string]string{"slug": creds.Slug})
	})
	// Read-only status: whether a GitHub App is configured on this box, and its
	// slug so a caller can deep-link the install page. A dashboard gates its
	// Connect step on this instead of always offering a skippable one.
	mux.HandleFunc("GET /v1/github", func(w http.ResponseWriter, r *http.Request) {
		app, err := s.GetGitHubApp()
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusOK, map[string]any{"configured": false})
			return
		}
		if err != nil {
			serverError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"configured": true,
			"app_id":     app.AppID,
			"slug":       app.Slug,
		})
	})
	// Dropping the stored App is the only way off BYO: while the row exists it
	// is treated as a deliberate operator override and shadows any App a relay
	// brokers, so brokered deliveries fail their signature check (#299). The
	// running listener keeps the old credentials until piperd restarts — the
	// provider is chosen once, at start — which is why the reply names the
	// provider that restart will pick rather than one now in effect.
	mux.HandleFunc("DELETE /v1/github/app", func(w http.ResponseWriter, r *http.Request) {
		if err := s.DeleteGitHubApp(); err != nil {
			serverError(w, r, err)
			return
		}
		provider := "unknown"
		if nextGitHubProvider != nil {
			provider = nextGitHubProvider()
		}
		writeJSON(w, http.StatusOK, map[string]string{"provider": provider})
	})

	noRelay := func(w http.ResponseWriter) bool {
		if dom == nil {
			http.Error(w, "domain config requires a relay connection, or direct serve (PIPER_BASE_DOMAIN with PIPER_SERVE=direct)", http.StatusConflict)
			return true
		}
		return false
	}
	mux.HandleFunc("GET /v1/domain", func(w http.ResponseWriter, r *http.Request) {
		if noRelay(w) {
			return
		}
		st, err := dom.Status()
		if err != nil {
			serverError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, st)
	})
	mux.HandleFunc("PUT /v1/domain", func(w http.ResponseWriter, r *http.Request) {
		if noRelay(w) {
			return
		}
		var in struct {
			Domain      string `json:"domain"`
			DNSProvider string `json:"dns_provider"`
			DNSToken    string `json:"dns_token"`
			Serve       string `json:"serve"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		st, err := dom.Set(in.Domain, in.DNSProvider, in.DNSToken, in.Serve)
		switch {
		case errors.Is(err, domain.ErrEnvManaged):
			http.Error(w, err.Error(), http.StatusConflict)
			return
		case errors.Is(err, domain.ErrInvalidDomain),
			errors.Is(err, domain.ErrUnsupportedProvider),
			errors.Is(err, domain.ErrTokenRequired),
			errors.Is(err, domain.ErrInvalidServe):
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		case err != nil:
			serverError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, st)
	})
	mux.HandleFunc("DELETE /v1/domain", func(w http.ResponseWriter, r *http.Request) {
		if noRelay(w) {
			return
		}
		if err := dom.Remove(); errors.Is(err, domain.ErrEnvManaged) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		} else if err != nil {
			serverError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// Per-app custom domains collection (#231). knownApp maps the app path
	// segment onto the 404, shared by GET and DELETE (POST gets it from
	// AddAppDomain's own app check).
	knownApp := func(w http.ResponseWriter, r *http.Request, name string) bool {
		_, err := s.GetApp(name)
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "unknown app", http.StatusNotFound)
			return false
		}
		if err != nil {
			serverError(w, r, err)
			return false
		}
		return true
	}
	mux.HandleFunc("GET /v1/apps/{name}/domains", func(w http.ResponseWriter, r *http.Request) {
		if noRelay(w) {
			return
		}
		name := r.PathValue("name")
		if !knownApp(w, r, name) {
			return
		}
		statuses, err := dom.AppDomainStatuses(name)
		if err != nil {
			serverError(w, r, err)
			return
		}
		if statuses == nil {
			statuses = []domain.AppDomainStatus{}
		}
		writeJSON(w, http.StatusOK, statuses)
	})
	mux.HandleFunc("POST /v1/apps/{name}/domains", func(w http.ResponseWriter, r *http.Request) {
		if noRelay(w) {
			return
		}
		var in struct {
			Domain string `json:"domain"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Domain == "" {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		row, err := dom.AddAppDomain(r.PathValue("name"), in.Domain)
		switch {
		case errors.Is(err, store.ErrNotFound):
			http.Error(w, "unknown app", http.StatusNotFound)
			return
		case errors.Is(err, domain.ErrInvalidDomain):
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		case errors.Is(err, domain.ErrBoxWideDomain), errors.Is(err, store.ErrDomainExists):
			http.Error(w, err.Error(), http.StatusConflict)
			return
		case err != nil:
			serverError(w, r, err)
			return
		}
		st, err := dom.AppDomainStatus(row.Domain)
		if err != nil {
			serverError(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, st)
	})
	mux.HandleFunc("DELETE /v1/apps/{name}/domains/{domain}", func(w http.ResponseWriter, r *http.Request) {
		if noRelay(w) {
			return
		}
		name := r.PathValue("name")
		if !knownApp(w, r, name) {
			return
		}
		// Rows are stored lowercase (AddAppDomain normalizes), so match the
		// path segment the same way.
		dn := strings.ToLower(r.PathValue("domain"))
		row, err := s.GetAppDomain(dn)
		if errors.Is(err, store.ErrNotFound) || (err == nil && row.App != name) {
			http.Error(w, "unknown domain", http.StatusNotFound)
			return
		}
		if err != nil {
			serverError(w, r, err)
			return
		}
		if err := dom.RemoveAppDomain(dn); err != nil {
			serverError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1/apps/{name}/env", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !knownApp(w, r, name) {
			return
		}
		env, updated, err := s.AppEnvWithTimestamps(name)
		if err != nil {
			serverError(w, r, err)
			return
		}
		updatedAt := make(map[string]string, len(updated))
		for k, t := range updated {
			updatedAt[k] = t.UTC().Format(time.RFC3339Nano)
		}
		writeJSON(w, http.StatusOK, map[string]any{"env": env, "updated_at": updatedAt})
	})
	mux.HandleFunc("POST /v1/apps/{name}/env", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !knownApp(w, r, name) {
			return
		}
		var in struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Key == "" {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		if !envKeyRE.MatchString(in.Key) {
			http.Error(w, "invalid env key", http.StatusBadRequest)
			return
		}
		// The deploy path owns PORT; a stored one would be silently overwritten,
		// so reject it here where the user can see why.
		if strings.EqualFold(in.Key, "PORT") {
			http.Error(w, "PORT is reserved", http.StatusBadRequest)
			return
		}
		if err := s.SetAppEnv(name, in.Key, in.Value); err != nil {
			serverError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /v1/apps/{name}/env/{key}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !knownApp(w, r, name) {
			return
		}
		if err := s.DeleteAppEnv(name, r.PathValue("key")); err != nil {
			serverError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

// validAppName reports whether name is a single DNS label: 1-63 chars of
// lowercase letters, digits, or hyphens, not starting or ending with a hyphen.
// App names are interpolated unescaped into URL paths and hostnames
// (<app>.<baseDom>, pr-N-<app>.…), so constraining them at create closes the
// interpolation/hostname-shape gap for every downstream endpoint at once (#120).
func validAppName(name string) bool {
	if len(name) == 0 || len(name) > 63 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-':
			if i == 0 || i == len(name)-1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// serverError logs the detailed error server-side and returns a generic 500 to
// the caller. Handler errors can carry container IDs, Caddy admin URLs, and file
// paths, and the control API is reachable remotely through the relay proxy, so
// the raw text must not reach the response body (#122).
func serverError(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("api: %s %s: %v", r.Method, r.URL.Path, err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// cleanRootDir validates and normalizes an app's monorepo build subpath. It
// must stay inside the checkout — deploy joins it onto the clone dir, so an
// absolute path or a ".." escape would build from outside the repo. Empty is
// valid (build the repo root) and normalizes to empty. Returns the cleaned,
// forward-slash relative path and whether it was acceptable.
func cleanRootDir(p string) (string, bool) {
	if p == "" {
		return "", true
	}
	if path.IsAbs(p) {
		return "", false
	}
	c := path.Clean(p)
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", false
	}
	if c == "." {
		return "", true
	}
	return c, true
}

func untar(r io.Reader, dir string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dir, filepath.Clean(hdr.Name))
		if !isWithin(dir, target) {
			return errors.New("tar entry escapes destination")
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		}
	}
}

func isWithin(base, target string) bool {
	rel, err := filepath.Rel(base, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// RequireToken wraps next so every request must carry a valid
// `Authorization: Bearer <token>`. Unknown, malformed, or revoked tokens get 401.
func RequireToken(s *store.Store, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := bearerToken(r)
		if !ok {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		if _, err := s.AuthenticateToken(tok); err != nil {
			if errors.Is(err, store.ErrBadToken) {
				http.Error(w, "invalid token", http.StatusUnauthorized)
				return
			}
			// A failing store is a box problem, not a credential problem —
			// reporting it as 401 sends the user chasing their token (#281).
			log.Printf("api: %s %s: authenticate token: %v", r.Method, r.URL.Path, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	tok := strings.TrimSpace(strings.TrimPrefix(h, prefix))
	return tok, tok != ""
}
