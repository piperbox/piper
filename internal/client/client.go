// Package client is the piper CLI's HTTP client to piperd.
package client

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"
	"github.com/piperbox/piper/internal/api"
	"github.com/piperbox/piper/internal/domain"
	"github.com/piperbox/piper/internal/store"
)

type Client struct {
	base         string
	token        string
	http         *http.Client
	pollInterval time.Duration
}

func New(base, token string) *Client {
	if base == "" {
		base = "http://127.0.0.1:8088"
	}
	return &Client{base: base, token: token, http: &http.Client{}, pollInterval: time.Second}
}

// WithTimeout sets an overall per-request timeout on the client's HTTP
// transport and returns the client for chaining. The interactive TUI uses
// it so a blackholed box surfaces as unreachable instead of hanging the
// poll. Not for streaming calls (it would abort a long response); the
// deploy upload is exempt (see Deploy).
func (c *Client) WithTimeout(d time.Duration) *Client {
	c.http.Timeout = d
	return c
}

// do builds a request to c.base+path, attaches the auth header (when set) and
// the content type (when non-empty), and sends it.
func (c *Client) do(method, path, contentType string, body io.Reader) (*http.Response, error) {
	return c.doWith(c.http, method, path, contentType, body)
}

func (c *Client) doWith(h *http.Client, method, path, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return h.Do(req)
}

// ErrVersionUnsupported means the agent answered 404: it predates the version
// endpoint. Distinguishing that from an unreachable box matters, because "too
// old to tell you" is the single most useful thing to print when someone is
// wondering whether their upgrade actually took (#375).
var ErrVersionUnsupported = errors.New("agent does not report its version")

// AgentInfo is the running daemon's self-report: its build, and the listen
// config it actually loaded. The addr/dir fields are empty when the daemon
// predates them (#476); callers treat that as "did not report".
type AgentInfo struct {
	Version   string `json:"version"`
	HTTPAddr  string `json:"http_addr"`
	HTTPSAddr string `json:"https_addr"`
	DataDir   string `json:"data_dir"`
}

// AgentInfo returns the self-report of the piperd that is actually running —
// which is not necessarily the piperd binary on disk, nor this CLI's own
// version.
func (c *Client) AgentInfo() (AgentInfo, error) {
	resp, err := c.do(http.MethodGet, "/v1/version", "", nil)
	if err != nil {
		return AgentInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return AgentInfo{}, ErrVersionUnsupported
	}
	if resp.StatusCode >= http.StatusMultipleChoices {
		return AgentInfo{}, responseError("version", resp)
	}
	var out AgentInfo
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return AgentInfo{}, err
	}
	return out, nil
}

// AgentVersion is AgentInfo reduced to the build string, for callers that
// only care whether the daemon answers and with which version.
func (c *Client) AgentVersion() (string, error) {
	info, err := c.AgentInfo()
	return info.Version, err
}

func (c *Client) CreateApp(name string, port int) error {
	body, err := json.Marshal(map[string]any{"name": name, "port": port})
	if err != nil {
		return err
	}
	resp, err := c.do(http.MethodPost, "/v1/apps", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return responseError("create app", resp)
	}
	return nil
}

func (c *Client) ListApps() ([]api.App, error) {
	resp, err := c.do(http.MethodGet, "/v1/apps", "", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return nil, responseError("list apps", resp)
	}
	var apps []api.App
	if err := json.NewDecoder(resp.Body).Decode(&apps); err != nil {
		return nil, err
	}
	return apps, nil
}

// Liveness reports the relay's view of the box: whether its tunnel session is
// currently connected. It GETs the client's base path itself, which on a
// remote client is the relay's /agents/<base-domain> resource — it has no
// meaning against a local piperd address.
type Liveness struct {
	Agent     string `json:"agent"`
	Connected bool   `json:"connected"`
}

func (c *Client) Liveness() (Liveness, error) {
	resp, err := c.do(http.MethodGet, "", "", nil)
	if err != nil {
		return Liveness{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return Liveness{}, responseError("liveness", resp)
	}
	var l Liveness
	if err := json.NewDecoder(resp.Body).Decode(&l); err != nil {
		return Liveness{}, err
	}
	return l, nil
}

func (c *Client) Deploy(name, srcDir string) (store.Deployment, error) {
	var body bytes.Buffer
	if err := TarDir(srcDir, &body); err != nil {
		return store.Deployment{}, err
	}
	// The upload ships the whole source tar in one POST; the overall request
	// timeout (the TUI's poll guard) must not cut it short on a slow link.
	noTimeout := *c.http
	noTimeout.Timeout = 0
	resp, err := c.doWith(&noTimeout, http.MethodPost, "/v1/apps/"+name+"/deploy", "application/x-tar", &body)
	if err != nil {
		return store.Deployment{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return store.Deployment{}, responseError("deploy", resp)
	}
	var dep store.Deployment
	if err := json.NewDecoder(resp.Body).Decode(&dep); err != nil {
		return store.Deployment{}, err
	}
	return dep, nil
}

// DeployFromRepo asks the agent to fetch the app's linked repository at its
// tracked branch and deploy it — no local source involved.
func (c *Client) DeployFromRepo(name string) (store.Deployment, error) {
	// The agent downloads the repo before answering; like Deploy's upload, that
	// must not be cut short by the overall request timeout (the TUI's poll guard).
	noTimeout := *c.http
	noTimeout.Timeout = 0
	resp, err := c.doWith(&noTimeout, http.MethodPost, "/v1/apps/"+name+"/deploy-from-repo", "", nil)
	if err != nil {
		return store.Deployment{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return store.Deployment{}, responseError("deploy", resp)
	}
	var dep store.Deployment
	if err := json.NewDecoder(resp.Body).Decode(&dep); err != nil {
		return store.Deployment{}, err
	}
	return dep, nil
}

func (c *Client) Deployments(name string) ([]store.Deployment, error) {
	resp, err := c.do(http.MethodGet, "/v1/apps/"+name+"/deployments", "", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return nil, responseError("deployments", resp)
	}
	var deps []store.Deployment
	if err := json.NewDecoder(resp.Body).Decode(&deps); err != nil {
		return nil, err
	}
	return deps, nil
}

func (c *Client) DeploymentLogs(name, id string) (string, error) {
	resp, err := c.do(http.MethodGet, "/v1/apps/"+name+"/deployments/"+id+"/logs", "", nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return "", responseError("deployment logs", resp)
	}
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

func (c *Client) App(name string) (api.App, error) {
	resp, err := c.do(http.MethodGet, "/v1/apps/"+name, "", nil)
	if err != nil {
		return api.App{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return api.App{}, responseError("app", resp)
	}
	var a api.App
	if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
		return api.App{}, err
	}
	return a, nil
}

// followMaxConsecutiveErrors is how many poll cycles in a row may fail before
// FollowDeploy gives up: a follow runs for minutes against a busy box, and one
// transient failure must not end the watch while the build runs on (#282).
const followMaxConsecutiveErrors = 5

// FollowDeploy polls until the deployment finishes, writing new log output as
// it appears. If the stored tail rotates after reaching its cap, it prints the
// current snapshot again. It stops when ctx is cancelled or times out, returning
// the last-seen deployment and ctx.Err() instead of polling a stranded
// "building" row forever (#161).
func (c *Client) FollowDeploy(ctx context.Context, name, id string, progress io.Writer) (store.Deployment, error) {
	lastLogs := ""
	var last store.Deployment
	failures := 0
	for {
		logs, err := c.DeploymentLogs(name, id)
		if err == nil {
			if strings.HasPrefix(logs, lastLogs) {
				_, _ = io.WriteString(progress, logs[len(lastLogs):])
			} else {
				// Tail-cap dropped the front (log exceeded the cap): reprint whole.
				_, _ = io.WriteString(progress, logs)
			}
			lastLogs = logs

			var deps []store.Deployment
			deps, err = c.Deployments(name)
			if err == nil {
				for _, d := range deps {
					if d.ID != id {
						continue
					}
					last = d
					switch d.Status {
					case "running", "failed", "stopped":
						return d, nil
					}
				}
			}
		}
		if err != nil {
			failures++
			if failures >= followMaxConsecutiveErrors {
				return store.Deployment{}, err
			}
		} else {
			failures = 0
		}

		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(c.pollInterval):
		}
	}
}

func (c *Client) LinkApp(name, repo, branch, rootDir string) error {
	body, _ := json.Marshal(map[string]string{"repo": repo, "branch": branch, "root_dir": rootDir})
	resp, err := c.do(http.MethodPost, "/v1/apps/"+name+"/link", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return responseError("link", resp)
	}
	return nil
}

// actionTimeout bounds the mutating calls that do real work on the box. Start
// re-runs a container and waits up to 30s for its health check, and Stop waits
// out Docker's 10s stop grace (both in internal/runtime) — far outside the 5s
// the TUI applies to its poll loop. That short timeout aborted the client while
// the box went on to finish, so the TUI reported failure for an action that had
// actually succeeded (#379).
//
// Delete and RemoveAppDomain are the same shape: Delete pays that stop grace
// once per running deployment, tears every route and relay hostname down and
// then prunes the app's images, and RemoveAppDomain makes a relay round-trip
// before removing the route, cert and directory. AddAppDomain deliberately is
// not here — it writes a pending row, spawns the issuance loop and returns, so
// it is a fast call by construction.
//
// Bounded rather than unbounded (cf. Deploy's exemption) because the box bounds
// these itself: a blackholed box still surfaces instead of hanging the UI.
const actionTimeout = 60 * time.Second

// forAction returns an HTTP client generous enough for work the box measures in
// tens of seconds. It only ever raises the caller's timeout — a client with none
// (the plain CLI) keeps waiting indefinitely, and a longer one is left alone.
func (c *Client) forAction() *http.Client {
	h := *c.http
	if h.Timeout != 0 && h.Timeout < actionTimeout {
		h.Timeout = actionTimeout
	}
	return &h
}

func (c *Client) StopApp(name string) error {
	resp, err := c.doWith(c.forAction(), http.MethodPost, "/v1/apps/"+name+"/stop", "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return responseError("stop", resp)
	}
	return nil
}

func (c *Client) StartApp(name string) error {
	resp, err := c.doWith(c.forAction(), http.MethodPost, "/v1/apps/"+name+"/start", "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return responseError("start", resp)
	}
	return nil
}

func (c *Client) DeleteApp(name string) error {
	resp, err := c.doWith(c.forAction(), http.MethodDelete, "/v1/apps/"+name, "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return responseError("delete", resp)
	}
	return nil
}

// AppEnvWithTimestamps returns app's saved environment variables and the time
// each var was last updated.
func (c *Client) AppEnvWithTimestamps(app string) (map[string]string, map[string]time.Time, error) {
	resp, err := c.do(http.MethodGet, "/v1/apps/"+app+"/env", "", nil)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, responseError("env", resp)
	}
	var out struct {
		Env       map[string]string `json:"env"`
		UpdatedAt map[string]string `json:"updated_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, nil, err
	}
	updated := make(map[string]time.Time, len(out.UpdatedAt))
	for k, v := range out.UpdatedAt {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return nil, nil, fmt.Errorf("parse updated_at for %s: %w", k, err)
		}
		updated[k] = t
	}
	return out.Env, updated, nil
}

// SetAppEnv saves one env var; it applies on the app's next deploy or restart.
func (c *Client) SetAppEnv(app, key, value string) error {
	body, err := json.Marshal(map[string]string{"key": key, "value": value})
	if err != nil {
		return err
	}
	resp, err := c.do(http.MethodPost, "/v1/apps/"+app+"/env", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return responseError("env set", resp)
	}
	return nil
}

// DeleteAppEnv removes one env var from app.
func (c *Client) DeleteAppEnv(app, key string) error {
	resp, err := c.do(http.MethodDelete, "/v1/apps/"+app+"/env/"+key, "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return responseError("env rm", resp)
	}
	return nil
}

// AppDomains lists the per-app custom domains attached to app (#232).
func (c *Client) AppDomains(app string) ([]domain.AppDomainStatus, error) {
	resp, err := c.do(http.MethodGet, "/v1/apps/"+app+"/domains", "", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return nil, responseError("list domains", resp)
	}
	var ds []domain.AppDomainStatus
	if err := json.NewDecoder(resp.Body).Decode(&ds); err != nil {
		return nil, err
	}
	return ds, nil
}

// AddAppDomain attaches dom to app and returns its initial status, including
// the CNAME record to create.
func (c *Client) AddAppDomain(app, dom string) (domain.AppDomainStatus, error) {
	body, err := json.Marshal(map[string]string{"domain": dom})
	if err != nil {
		return domain.AppDomainStatus{}, err
	}
	resp, err := c.do(http.MethodPost, "/v1/apps/"+app+"/domains", "application/json", bytes.NewReader(body))
	if err != nil {
		return domain.AppDomainStatus{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return domain.AppDomainStatus{}, responseError("add domain", resp)
	}
	var st domain.AppDomainStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return domain.AppDomainStatus{}, err
	}
	return st, nil
}

// RemoveAppDomain detaches dom from app.
func (c *Client) RemoveAppDomain(app, dom string) error {
	resp, err := c.doWith(c.forAction(), http.MethodDelete, "/v1/apps/"+app+"/domains/"+dom, "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return responseError("remove domain", resp)
	}
	return nil
}

func (c *Client) Manifest(redirectURL string) (string, error) {
	body, _ := json.Marshal(map[string]string{"redirect_url": redirectURL})
	resp, err := c.do(http.MethodPost, "/v1/github/manifest", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", responseError("manifest", resp)
	}
	var out struct {
		Manifest string `json:"manifest"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Manifest, nil
}

// ExchangeGitHub exchanges a manifest code for App credentials stored on the
// box and returns the created App's slug, so the caller can deep-link its
// install page.
func (c *Client) ExchangeGitHub(code string) (string, error) {
	body, _ := json.Marshal(map[string]string{"code": code})
	resp, err := c.do(http.MethodPost, "/v1/github/exchange", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", responseError("exchange", resp)
	}
	var out struct {
		Slug string `json:"slug"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Slug, nil
}

// ResetGitHub drops the box's own GitHub App and returns the webhook credential
// source it will use once piperd restarts ("brokered", "none", or "unknown").
func (c *Client) ResetGitHub() (string, error) {
	resp, err := c.do(http.MethodDelete, "/v1/github/app", "", nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", responseError("reset github", resp)
	}
	var out struct {
		Provider string `json:"provider"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Provider, nil
}

// StatusError is the error for a request that reached the server but got a
// non-2xx response; Code lets callers tell auth failures (401) from other
// HTTP errors and from transport errors (which are never a StatusError).
type StatusError struct {
	Action string
	Code   int
	Status string
	Body   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s: %s: %s", e.Action, e.Status, e.Body)
}

// Unauthorized reports whether the server rejected the request as
// unauthenticated (HTTP 401), letting callers tell a bad or absent token from
// other HTTP errors and from transport errors (which are never a StatusError).
func (e *StatusError) Unauthorized() bool { return e.Code == http.StatusUnauthorized }

func responseError(action string, resp *http.Response) error {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s: %s: read response: %w", action, resp.Status, err)
	}
	return &StatusError{Action: action, Code: resp.StatusCode, Status: resp.Status, Body: strings.TrimSpace(string(body))}
}

// TarDir writes the regular files under dir to w using relative, slash-separated
// names. It honors dir's .dockerignore with docker's semantics — docker build
// would drop the matched files from the context anyway; skipping them here keeps
// the bytes off the wire. Dockerfile and .dockerignore always ship, as docker's
// own CLI does.
func TarDir(dir string, w io.Writer) error {
	pm, err := loadDockerignore(dir)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(w)
	walkErr := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if info.IsDir() {
			// Skip a matched subtree wholesale — unless a ! pattern exists,
			// which could re-include something under it.
			if pm != nil && rel != "." && !pm.Exclusions() {
				if ignored, err := pm.MatchesOrParentMatches(rel); err != nil {
					return err
				} else if ignored {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if pm != nil && rel != "Dockerfile" && rel != ".dockerignore" {
			ignored, err := pm.MatchesOrParentMatches(rel)
			if err != nil {
				return err
			}
			if ignored {
				return nil
			}
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, f)
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	closeErr := tw.Close()
	if walkErr != nil {
		return walkErr
	}
	return closeErr
}

// loadDockerignore parses dir's .dockerignore into a matcher, or nil when the
// file is absent.
func loadDockerignore(dir string) (*patternmatcher.PatternMatcher, error) {
	f, err := os.Open(filepath.Join(dir, ".dockerignore"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	patterns, err := ignorefile.ReadAll(f)
	if err != nil {
		return nil, err
	}
	return patternmatcher.New(patterns)
}
