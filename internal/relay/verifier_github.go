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
	"sync"
	"time"
)

// GitHubVerifier brokers GitHub's OAuth device authorization grant and, for the
// browser, the authorization-code exchange. It holds the relay's GitHub client
// secret so callers never see it. GitHub's device flow returns no ID token —
// identity comes from GET /user with the granted access token, which is used
// once and discarded. Each Start spawns a background goroutine that polls
// GitHub until the user approves, the code expires, or the process exits; Poll
// reports progress without blocking.
type GitHubVerifier struct {
	clientID, clientSecret string
	oauthBase              string // https://github.com; tests override
	apiBase                string // https://api.github.com; tests override
	httpc                  *http.Client
	sleep                  func(time.Duration) // poll delay seam; tests override
	now                    func() time.Time    // clock seam; tests override

	mu    sync.Mutex
	flows map[string]*githubFlow
}

type githubFlow struct {
	done bool
	id   Identity
	err  error
	// expires bounds how long this entry may occupy the map: the device code's
	// own lifetime plus a grace for the poll that redeems it. A flow polled to
	// completion is deleted by Poll; this is what retires the ones nobody ever
	// comes back for (#81).
	expires time.Time
}

// flowGrace is how long past a device code's expiry a flow stays resolvable, so
// a poll racing the deadline still gets the real "device code expired" error
// rather than a bare unknown-handle.
const flowGrace = time.Minute

func NewGitHubVerifier(clientID, clientSecret string) *GitHubVerifier {
	return &GitHubVerifier{
		clientID:     clientID,
		clientSecret: clientSecret,
		oauthBase:    "https://github.com",
		apiBase:      "https://api.github.com",
		httpc:        &http.Client{Timeout: 15 * time.Second},
		sleep:        time.Sleep,
		now:          time.Now,
		flows:        map[string]*githubFlow{},
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
	fl := &githubFlow{expires: g.now().Add(time.Duration(expiresIn)*time.Second + flowGrace)}
	g.mu.Lock()
	g.sweepLocked()
	g.flows[handle] = fl
	g.mu.Unlock()

	go g.pollUntilDone(res.DeviceCode, res.Interval, res.ExpiresIn, fl)

	return handle, DeviceAuth{
		UserCode:        res.UserCode,
		VerificationURI: res.VerificationURI,
		Interval:        res.Interval,
		ExpiresIn:       res.ExpiresIn,
	}, nil
}

// pollUntilDone polls GitHub's token endpoint at the server-given interval,
// stretching by 5s on slow_down (GitHub's documented semantics), until the
// grant resolves or the device code's lifetime elapses.
func (g *GitHubVerifier) pollUntilDone(deviceCode string, interval, expiresIn int, fl *githubFlow) {
	finish := func(id Identity, err error) {
		g.mu.Lock()
		fl.done, fl.id, fl.err = true, id, err
		g.mu.Unlock()
	}
	if interval <= 0 {
		interval = 5
	}
	deadline := g.now().Add(time.Duration(expiresIn) * time.Second)
	ctx := context.Background()
	for {
		if g.now().After(deadline) {
			finish(Identity{}, errors.New("device code expired"))
			return
		}
		g.sleep(time.Duration(interval) * time.Second)

		var tr githubTokenResponse
		err := g.postForm(ctx, g.oauthBase+"/login/oauth/access_token", url.Values{
			"client_id":   {g.clientID},
			"device_code": {deviceCode},
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		}, &tr)
		if err != nil {
			finish(Identity{}, err)
			return
		}
		switch tr.Error {
		case "":
			if tr.AccessToken == "" {
				finish(Identity{}, errors.New("github token response missing access_token"))
				return
			}
			finish(g.fetchUser(ctx, tr.AccessToken))
			return
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5
			continue
		default: // expired_token, access_denied, incorrect_device_code, ...
			finish(Identity{}, fmt.Errorf("github device flow: %s", tr.Error))
			return
		}
	}
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

func (g *GitHubVerifier) Poll(_ context.Context, handle string) (Identity, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	fl, ok := g.flows[handle]
	if !ok || g.now().After(fl.expires) {
		return Identity{}, errors.New("unknown handle")
	}
	if !fl.done {
		return Identity{}, ErrAuthPending
	}
	delete(g.flows, handle)
	return fl.id, fl.err
}

// sweepLocked drops flows past their expiry. Called from Start, so the map is
// bounded by the number of logins actually in flight: entries can only be added
// there, and every add pays one pass over a map that size. No background
// goroutine, nothing to shut down.
func (g *GitHubVerifier) sweepLocked() {
	now := g.now()
	for h, fl := range g.flows {
		if now.After(fl.expires) {
			delete(g.flows, h)
		}
	}
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
