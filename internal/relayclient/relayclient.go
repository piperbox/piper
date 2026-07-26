// Package relayclient is the piper CLI's HTTP client for the relay control API:
// the GitHub device-flow login and account-bound box enrollment. It is the
// CLI-side counterpart of the relay's internal/relay control handlers.
package relayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DeviceAuth is the relay's response to starting a device-flow login.
type DeviceAuth struct {
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	DeviceCode      string `json:"device_code"`
	Interval        int    `json:"interval"`
	ExpiresIn       int    `json:"expires_in"`
}

// Account is the completed-login result: a relay account credential + username.
// InstallURL is the relay's GitHub App install-and-authorize page; empty when
// the relay has no App configured (or the App has no slug), in which case
// login ends without an install step.
type Account struct {
	AccountCredential string `json:"account_credential"`
	Username          string `json:"username"`
	InstallURL        string `json:"install_url"`
}

// Enrollment is the result of claiming a box: an enrollment token, the assigned
// base domain, and the relay tunnel endpoint the box should dial.
type Enrollment struct {
	EnrollmentToken string `json:"enrollment_token"`
	BaseDomain      string `json:"base_domain"`
	TunnelEndpoint  string `json:"tunnel_endpoint"`
	WebhookSecret   string `json:"webhook_secret"`
	GitHubApp       bool   `json:"github_app"`
}

// ErrAuthPending means the user has not yet completed the device flow.
var ErrAuthPending = errors.New("authorization_pending")

// ErrBadCredential means the relay rejected the account credential (unknown or
// a disabled account).
var ErrBadCredential = errors.New("relay rejected account credential")

// ErrQuotaExceeded means the account is already at its agent cap.
var ErrQuotaExceeded = errors.New("account agent quota exceeded")

// ErrNoInstallation means the relay's GitHub App is not installed for this
// account yet — the caller should keep polling, not fail.
var ErrNoInstallation = errors.New("github app not installed for this account")

// Client talks to a relay's control API rooted at base.
type Client struct {
	base string
	http *http.Client
}

// DefaultAPI is the hosted public relay's control API base URL. Override with
// `piper login --relay <url>` for a self-hosted relay.
const DefaultAPI = "https://api.public.getpiper.dev"

// New returns a Client for the relay control API at base (e.g.
// https://api.public.getpiper.dev).
func New(base string) *Client {
	return &Client{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: 30 * time.Second}}
}

func (c *Client) post(ctx context.Context, path string, body any, auth string) (*http.Response, error) {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	return c.http.Do(req)
}

// LoginDevice starts a device-flow login and returns the user code + URL to show.
func (c *Client) LoginDevice(ctx context.Context) (DeviceAuth, error) {
	resp, err := c.post(ctx, "/v1/login/device", nil, "")
	if err != nil {
		return DeviceAuth{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return DeviceAuth{}, fmt.Errorf("relay device login: %s", resp.Status)
	}
	var da DeviceAuth
	if err := json.NewDecoder(resp.Body).Decode(&da); err != nil {
		return DeviceAuth{}, err
	}
	return da, nil
}

// LoginPoll polls once for completion of the device flow. It returns
// ErrAuthPending while the user has not finished, or the Account on success.
func (c *Client) LoginPoll(ctx context.Context, deviceCode string) (Account, error) {
	resp, err := c.post(ctx, "/v1/login/poll", map[string]string{"device_code": deviceCode}, "")
	if err != nil {
		return Account{}, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var acc Account
		if err := json.NewDecoder(resp.Body).Decode(&acc); err != nil {
			return Account{}, err
		}
		return acc, nil
	case http.StatusAccepted:
		return Account{}, ErrAuthPending
	default:
		return Account{}, fmt.Errorf("relay login poll: %s", resp.Status)
	}
}

// CLILoginStart begins a brokered browser login (#291), returning the handle to
// poll and the user code the human enters in the browser to bind the session.
func (c *Client) CLILoginStart(ctx context.Context) (handle, userCode string, err error) {
	resp, err := c.post(ctx, "/v1/login/cli/start", nil, "")
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("relay cli login start: %s", resp.Status)
	}
	var out struct {
		Handle   string `json:"handle"`
		UserCode string `json:"user_code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", err
	}
	return out.Handle, out.UserCode, nil
}

// CLILoginPoll polls once for completion of a brokered browser login. It returns
// ErrAuthPending until the user finishes in the browser, then the Account.
func (c *Client) CLILoginPoll(ctx context.Context, handle string) (Account, error) {
	resp, err := c.post(ctx, "/v1/login/cli/poll", map[string]string{"handle": handle}, "")
	if err != nil {
		return Account{}, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var acc Account
		if err := json.NewDecoder(resp.Body).Decode(&acc); err != nil {
			return Account{}, err
		}
		return acc, nil
	case http.StatusAccepted:
		return Account{}, ErrAuthPending
	default:
		return Account{}, fmt.Errorf("relay cli login poll: %s", resp.Status)
	}
}

// Enroll claims a box for the account behind accountCredential, returning the
// enrollment token, assigned base domain, and tunnel endpoint.
func (c *Client) Enroll(ctx context.Context, accountCredential string) (Enrollment, error) {
	resp, err := c.post(ctx, "/v1/enroll", nil, accountCredential)
	if err != nil {
		return Enrollment{}, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var en Enrollment
		if err := json.NewDecoder(resp.Body).Decode(&en); err != nil {
			return Enrollment{}, err
		}
		return en, nil
	case http.StatusUnauthorized:
		return Enrollment{}, ErrBadCredential
	case http.StatusTooManyRequests:
		return Enrollment{}, ErrQuotaExceeded
	default:
		return Enrollment{}, fmt.Errorf("relay enroll: %s", resp.Status)
	}
}

// Repo is one installation-accessible repository as the relay reports it: full
// name plus the visibility badge and last-pushed timestamp the picker renders.
type Repo struct {
	FullName   string `json:"full_name"`
	Visibility string `json:"visibility"`
	PushedAt   string `json:"pushed_at"`
}

// Installation is one GitHub App installation the account holds, as the relay
// reports it: the opaque id plus the target it is installed on.
type Installation struct {
	ID          string `json:"installation_id"`
	TargetType  string `json:"target_type"`
	TargetLogin string `json:"target_login"`
}

// Status is the relay's GitHub App report for an account: whether the relay
// brokers an App at all, where to install it, and the account's installations.
type Status struct {
	GitHubApp     bool           `json:"github_app"`
	InstallURL    string         `json:"install_url"`
	Installations []Installation `json:"installations"`
}

// GitHubStatus reports the account's GitHub App state: whether the relay
// brokers an App, its install page, and every installation linked to the
// account. It never 404s on a missing installation — an empty Installations
// is the answer — so a poll loop can wait for the first install to appear.
func (c *Client) GitHubStatus(ctx context.Context, accountCredential string) (Status, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/v1/github/status", nil)
	if err != nil {
		return Status{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accountCredential)
	resp, err := c.http.Do(req)
	if err != nil {
		return Status{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return Status{}, ErrBadCredential
	}
	if resp.StatusCode != http.StatusOK {
		return Status{}, fmt.Errorf("relay github status: %s", resp.Status)
	}
	var st Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return Status{}, err
	}
	return st, nil
}

// GitHubRepos lists the repositories the given installation can reach. The
// installation id comes from GitHubStatus; the relay authorizes it against the
// account and maps an unknown/foreign id to a 404 → ErrNoInstallation.
func (c *Client) GitHubRepos(ctx context.Context, accountCredential, installationID string) ([]Repo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.base+"/v1/github/repos?installation_id="+url.QueryEscape(installationID), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accountCredential)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var body struct {
			Repos []Repo `json:"repos"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return nil, err
		}
		return body.Repos, nil
	case http.StatusNotFound:
		return nil, ErrNoInstallation
	default:
		return nil, fmt.Errorf("relay github repos: %s", resp.Status)
	}
}

// Agent is one box on the account, as the relay's /agents listing reports it.
// Connected is the relay's in-memory view of the tunnel session, so it is a
// live answer rather than a stored flag.
type Agent struct {
	BaseDomain string `json:"agent"`
	Name       string `json:"name"`
	Owner      string `json:"owner"`
	Connected  bool   `json:"connected"`
}

// ErrAgentConnected means the box still holds a live tunnel session. Removal is
// irreversible, so the relay refuses rather than evicting it.
var ErrAgentConnected = errors.New("box is connected; stop piperd on it first")

// ErrNoAgent means the relay has no such box for this account. Unknown and
// another tenant's box are indistinguishable by design.
var ErrNoAgent = errors.New("no such box")

// Agents lists the boxes the account may drive — its own plus any org's it
// belongs to.
func (c *Client) Agents(ctx context.Context, accountCredential string) ([]Agent, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/agents", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accountCredential)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrBadCredential
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("relay agents: %s", resp.Status)
	}
	var body struct {
		Agents []Agent `json:"agents"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.Agents, nil
}

// RemoveAgent retires baseDomain, freeing its agent slot. The relay refuses
// while the box is connected.
func (c *Client) RemoveAgent(ctx context.Context, accountCredential, baseDomain string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.base+"/agents/"+url.PathEscape(baseDomain), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accountCredential)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil
	case http.StatusConflict:
		return ErrAgentConnected
	case http.StatusNotFound:
		return ErrNoAgent
	case http.StatusUnauthorized:
		return ErrBadCredential
	default:
		return fmt.Errorf("relay remove box: %s", resp.Status)
	}
}
