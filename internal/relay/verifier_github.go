package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// GitHubVerifier brokers GitHub's OAuth device authorization grant and, for the
// browser, the authorization-code exchange. It holds the relay's GitHub client
// secret so callers never see it. GitHub's device flow returns no ID token —
// identity comes from GET /user with the granted access token, which is used
// once and discarded. Flows live in the store (#522): Start records the device
// code, and the poll that arrives once GitHub's interval has passed makes one
// upstream request, so any relay serves any poll and nothing runs in the
// background.
type GitHubVerifier struct {
	clientID, clientSecret string
	oauthBase              string // https://github.com; tests override
	apiBase                string // https://api.github.com; tests override
	httpc                  *http.Client
	st                     *Store
}

// flowGrace is how long past a device code's expiry a flow stays resolvable, so
// a poll racing the deadline still gets GitHub's own "expired" answer rather
// than a bare unknown-handle.
const flowGrace = time.Minute

func NewGitHubVerifier(clientID, clientSecret string, st *Store) *GitHubVerifier {
	return &GitHubVerifier{
		clientID:     clientID,
		clientSecret: clientSecret,
		oauthBase:    "https://github.com",
		apiBase:      "https://api.github.com",
		httpc:        &http.Client{Timeout: 15 * time.Second},
		st:           st,
	}
}

// githubTokenResponse mirrors GitHub's token-endpoint JSON (device poll and
// authorization-code exchange share this shape). GitHub reports poll errors
// ("authorization_pending", "slow_down", ...) as fields in 200-OK bodies, not
// RFC-style 4xx responses.
type githubTokenResponse struct {
	AccessToken string `json:"access_token"`
	Error       string `json:"error"`
}

func (g *GitHubVerifier) Start(ctx context.Context) (string, DeviceAuth, error) {
	var res struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
		Error           string `json:"error"`
	}
	err := g.postForm(ctx, g.oauthBase+"/login/device/code",
		url.Values{"client_id": {g.clientID}}, &res)
	if err != nil {
		return "", DeviceAuth{}, err
	}
	if res.Error != "" || res.DeviceCode == "" {
		return "", DeviceAuth{}, fmt.Errorf("github device code: %q", res.Error)
	}

	raw := make([]byte, 8)
	_, _ = rand.Read(raw)
	handle := hex.EncodeToString(raw)

	expiresIn := res.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 900 // GitHub's documented device-code lifetime
	}
	interval := res.Interval
	if interval <= 0 {
		interval = 5
	}
	if err := g.st.PutDeviceFlow(handle, res.DeviceCode, interval,
		time.Duration(expiresIn)*time.Second+flowGrace); err != nil {
		return "", DeviceAuth{}, err
	}

	return handle, DeviceAuth{
		UserCode:        res.UserCode,
		VerificationURI: res.VerificationURI,
		Interval:        res.Interval,
		ExpiresIn:       res.ExpiresIn,
	}, nil
}

// fetchUser resolves an access token to the GitHub identity behind it.
func (g *GitHubVerifier) fetchUser(ctx context.Context, token string) (Identity, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.apiBase+"/user", nil)
	if err != nil {
		return Identity{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := g.httpc.Do(req)
	if err != nil {
		return Identity{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Identity{}, fmt.Errorf("github /user: status %d", resp.StatusCode)
	}
	var u struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return Identity{}, err
	}
	if u.ID == 0 || u.Login == "" {
		return Identity{}, errors.New("github /user: missing id or login")
	}
	return Identity{Subject: strconv.FormatInt(u.ID, 10), Login: u.Login}, nil
}

// postForm POSTs a form and decodes the JSON response into out. GitHub encodes
// protocol errors inside 200-OK bodies, so only transport/HTTP-level failures
// are errors here; callers inspect the decoded error field.
func (g *GitHubVerifier) postForm(ctx context.Context, u string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := g.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("github: POST %s: status %d", u, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Poll advances a flow by at most one upstream request. Before next_poll_at it
// is pending for free; after it, GitHub is asked once and the row is deferred
// (authorization_pending, slow_down) or retired (any terminal answer, which is
// returned to this caller and to nobody else).
func (g *GitHubVerifier) Poll(ctx context.Context, handle string) (Identity, error) {
	fl, ok, err := g.st.DeviceFlow(handle)
	if err != nil {
		return Identity{}, err
	}
	if !ok {
		return Identity{}, errors.New("unknown handle")
	}
	if !fl.Due {
		return Identity{}, ErrAuthPending
	}
	var tr githubTokenResponse
	err = g.postForm(ctx, g.oauthBase+"/login/oauth/access_token", url.Values{
		"client_id":   {g.clientID},
		"device_code": {fl.DeviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}, &tr)
	if err != nil {
		return g.finish(handle, Identity{}, err)
	}
	switch tr.Error {
	case "":
		if tr.AccessToken == "" {
			return g.finish(handle, Identity{}, errors.New("github token response missing access_token"))
		}
		id, err := g.fetchUser(ctx, tr.AccessToken)
		return g.finish(handle, id, err)
	case "authorization_pending":
		return g.deferFlow(handle, fl.Interval)
	case "slow_down":
		// GitHub's documented semantics: wait interval + 5s.
		return g.deferFlow(handle, fl.Interval+5)
	default: // expired_token, access_denied, incorrect_device_code, ...
		return g.finish(handle, Identity{}, fmt.Errorf("github device flow: %s", tr.Error))
	}
}

// deferFlow pushes the flow's next upstream request secs into the future.
func (g *GitHubVerifier) deferFlow(handle string, secs int) (Identity, error) {
	if err := g.st.DeferDeviceFlow(handle, time.Duration(secs)*time.Second); err != nil {
		return Identity{}, err
	}
	return Identity{}, ErrAuthPending
}

// finish retires the flow and hands its outcome to this one caller.
func (g *GitHubVerifier) finish(handle string, id Identity, outcome error) (Identity, error) {
	if err := g.st.DeleteDeviceFlow(handle); err != nil {
		return Identity{}, err
	}
	return id, outcome
}

// AuthCodeURL is the GitHub authorize URL for the browser flow. No
// redirect_uri parameter: the OAuth app's single registered callback URL
// (the relay's /v1/login/callback) is used. Always the authorize endpoint,
// never the App install page: installations/new only yields a code as a side
// effect of an actual install, so it dead-ends on the installation's settings
// page once the App is already installed (#305). Installing the App is a
// separate step after login, and linking rides the installation webhook.
// prompt=select_account forces GitHub's account picker: without it, a browser
// signed into multiple GitHub accounts 404s on /login/oauth/authorize (#320).
func (g *GitHubVerifier) AuthCodeURL(state string) string {
	return g.oauthBase + "/login/oauth/authorize?client_id=" +
		url.QueryEscape(g.clientID) + "&state=" + url.QueryEscape(state) +
		"&prompt=select_account"
}

// Exchange resolves an authorization code to the GitHub identity behind it.
func (g *GitHubVerifier) Exchange(ctx context.Context, code string) (Identity, error) {
	var tr githubTokenResponse
	err := g.postForm(ctx, g.oauthBase+"/login/oauth/access_token", url.Values{
		"client_id":     {g.clientID},
		"client_secret": {g.clientSecret},
		"code":          {code},
	}, &tr)
	if err != nil {
		return Identity{}, err
	}
	if tr.Error != "" || tr.AccessToken == "" {
		return Identity{}, fmt.Errorf("github code exchange: %q", tr.Error)
	}
	return g.fetchUser(ctx, tr.AccessToken)
}
