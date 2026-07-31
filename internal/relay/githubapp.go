package relay

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/piperbox/piper/internal/ghjwt"
)

const defaultGitHubAPIBase = "https://api.github.com"

// GitHubAppConfig is the relay's App credentials. An empty AppID means the
// relay runs BYO-only: no ingress endpoint, no token brokering.
type GitHubAppConfig struct {
	AppID         string
	PrivateKeyPEM string
	WebhookSecret string
	APIBase       string // defaults to https://api.github.com
	Slug          string // the App's URL slug; empty disables InstallURL
}

// GitHubApp is the relay's view of one GitHub App: webhook signature
// verification, repo-scoped installation tokens, and repository listing. The
// private key never leaves this type.
type GitHubApp struct {
	appID   string
	key     *rsa.PrivateKey
	secret  string
	apiBase string
	slug    string
	http    *http.Client
}

func NewGitHubApp(cfg GitHubAppConfig) (*GitHubApp, error) {
	// VerifySignature HMACs with this secret; an empty one is a key anyone can
	// compute, letting an attacker forge webhook deliveries (e.g. rebind an
	// installation to their own account). An App is unsafe to serve without it.
	if cfg.WebhookSecret == "" {
		return nil, fmt.Errorf("github app: PIPER_RELAY_GITHUB_WEBHOOK_SECRET is required (empty webhook secret makes /gh forgeable)")
	}
	key, err := ghjwt.ParseKey(cfg.PrivateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse app private key: %w", err)
	}
	base := cfg.APIBase
	if base == "" {
		base = defaultGitHubAPIBase
	}
	return &GitHubApp{
		appID:   cfg.AppID,
		key:     key,
		secret:  cfg.WebhookSecret,
		apiBase: strings.TrimRight(base, "/"),
		slug:    cfg.Slug,
		http:    &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// InstallURL is the App's install-and-authorize page. Empty when the operator
// configured no slug; the CLI then prints no install link.
func (g *GitHubApp) InstallURL() string {
	if g.slug == "" {
		return ""
	}
	return "https://github.com/apps/" + url.PathEscape(g.slug) + "/installations/new"
}

// VerifySignature checks GitHub's X-Hub-Signature-256 header against the App
// webhook secret in constant time.
func (g *GitHubApp) VerifySignature(header string, body []byte) bool {
	m := hmac.New(sha256.New, []byte(g.secret))
	m.Write(body)
	want := "sha256=" + hex.EncodeToString(m.Sum(nil))
	return hmac.Equal([]byte(header), []byte(want))
}

// installationToken mints an unscoped installation token. Only Repos uses it;
// everything on the deploy path goes through RepoToken.
func (g *GitHubApp) installationToken(ctx context.Context, installationID string, body any) (string, time.Time, error) {
	jwt, err := ghjwt.Sign(g.appID, g.key, time.Now())
	if err != nil {
		return "", time.Time{}, err
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return "", time.Time{}, err
		}
		rdr = bytes.NewReader(b)
	}
	url := g.apiBase + "/app/installations/" + installationID + "/access_tokens"
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, rdr)
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", time.Time{}, fmt.Errorf("installation token: %s: %s", resp.Status, b)
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", time.Time{}, err
	}
	return out.Token, out.ExpiresAt, nil
}

// RepoToken mints an installation token scoped to a single repository with the
// minimum permissions a deploy needs. Scoping is what bounds the blast radius
// of a compromised box to the one repo it already deploys.
//
// Only the name half of repo constrains anything here: GitHub's "repositories"
// field takes bare names, resolved within the installation. The owner half is
// spent before this call — GitHubTokenFor picks the installation whose
// target_login is that owner, and answers ErrNoInstallation when the account
// holds none — so passing "evil/blog" cannot reach a different owner's blog. An
// installation only ever covers one account's repos, and names are unique within
// an owner, so the bare name resolves exactly (#296).
func (g *GitHubApp) RepoToken(ctx context.Context, installationID, repo string) (string, time.Time, error) {
	name := repo
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		name = repo[i+1:]
	}
	return g.installationToken(ctx, installationID, map[string]any{
		"repositories": []string{name},
		"permissions": map[string]string{
			"contents":    "read",
			"deployments": "write",
		},
	})
}

// InstallationTarget is the live target of an installation as GitHub reports
// it: the user or org the App is currently installed on.
type InstallationTarget struct {
	Type  string
	Login string
}

// Installation fetches an installation's live target from GitHub
// (GET /app/installations/{id}). GitHubTokenFor calls this only when no stored
// target_login matches the repo owner — an org rename strands the stored value
// (#433) — so the cost is one API round-trip per miss, never on the happy path.
func (g *GitHubApp) Installation(ctx context.Context, installationID string) (InstallationTarget, error) {
	jwt, err := ghjwt.Sign(g.appID, g.key, time.Now())
	if err != nil {
		return InstallationTarget{}, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, g.apiBase+"/app/installations/"+installationID, nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := g.http.Do(req)
	if err != nil {
		return InstallationTarget{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return InstallationTarget{}, fmt.Errorf("get installation: %s: %s", resp.Status, b)
	}
	var out struct {
		Account struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"account"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return InstallationTarget{}, err
	}
	return InstallationTarget{Type: out.Account.Type, Login: out.Account.Login}, nil
}

// Repo is one installation-accessible repository, as the picker renders it:
// full name plus the visibility badge and last-pushed timestamp. Fields are
// passed straight through from GitHub (pushed_at is "" for a never-pushed repo).
type Repo struct {
	FullName   string `json:"full_name"`
	Visibility string `json:"visibility"`
	PushedAt   string `json:"pushed_at"`
}

// Repos lists the repositories an installation can reach. This is what a
// dashboard's repo picker renders; no list is ever cached. per_page=100 caps a
// single page; full Link-header pagination is a follow-up (#308).
func (g *GitHubApp) Repos(ctx context.Context, installationID string) ([]Repo, error) {
	tok, _, err := g.installationToken(ctx, installationID, nil)
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, g.apiBase+"/installation/repositories?per_page=100", nil)
	req.Header.Set("Authorization", "token "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("list repositories: %s: %s", resp.Status, b)
	}
	var out struct {
		Repositories []Repo `json:"repositories"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Repositories == nil {
		out.Repositories = []Repo{}
	}
	return out.Repositories, nil
}
