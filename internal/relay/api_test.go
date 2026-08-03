package relay

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func newTestAPI(t *testing.T) (http.Handler, *Store, *FakeVerifier) {
	t.Helper()
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	fv := NewFakeVerifier()
	return NewAPI(st, fv), st, fv
}

func TestLoginDeviceThenPoll(t *testing.T) {
	api, _, fv := newTestAPI(t)

	// Start device flow.
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/login/device", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("device status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var dev struct {
		UserCode   string `json:"user_code"`
		DeviceCode string `json:"device_code"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &dev); err != nil {
		t.Fatal(err)
	}
	if dev.UserCode == "" || dev.DeviceCode == "" {
		t.Fatalf("empty device response: %+v", dev)
	}

	// Poll before approval → 202 pending.
	rr = httptest.NewRecorder()
	api.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/login/poll",
		strings.NewReader(`{"device_code":"`+dev.DeviceCode+`"}`)))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("pending poll status = %d, want 202", rr.Code)
	}

	// Approve, then poll → 200 with a credential.
	fv.Approve(dev.DeviceCode, Identity{Subject: "sub-1", Login: "ivan"})
	rr = httptest.NewRecorder()
	api.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/login/poll",
		strings.NewReader(`{"device_code":"`+dev.DeviceCode+`"}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("success poll status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var ok struct {
		AccountCredential string `json:"account_credential"`
		Username          string `json:"username"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &ok); err != nil {
		t.Fatal(err)
	}
	if ok.AccountCredential == "" || ok.Username != "ivan" {
		t.Fatalf("poll success body = %+v", ok)
	}
}

// A disabled account re-running the device flow must be told no, not handed a
// fresh credential with a 200. The credential would be inert (AuthenticateAccount
// rejects disabled accounts) so the kill-switch holds either way, but a 200 plus
// a live-looking secret is a confusing, mildly leaky way to say "you are cut off"
// — and it mints a row per attempt (#81).
func TestLoginPollDisabledAccountIsForbidden(t *testing.T) {
	api, st, fv := newTestAPI(t)
	id := Identity{Subject: "sub-disabled", Login: "ivan"}

	first := loginOnce(t, api, fv, id)
	if first.Code != http.StatusOK {
		t.Fatalf("first login status = %d, body = %s", first.Code, first.Body.String())
	}

	if err := st.DisableAccount("ivan", "user"); err != nil {
		t.Fatal(err)
	}

	rr := loginOnce(t, api, fv, id)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("disabled re-login status = %d, want 403; body = %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "account_credential") {
		t.Errorf("disabled re-login handed back a credential: %s", rr.Body.String())
	}
}

// loginOnce drives one full device flow (start → approve → poll) and returns the
// poll's response recorder.
func loginOnce(t *testing.T, api http.Handler, fv *FakeVerifier, id Identity) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/login/device", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("device status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var dev struct {
		DeviceCode string `json:"device_code"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &dev); err != nil {
		t.Fatal(err)
	}
	fv.Approve(dev.DeviceCode, id)
	rr = httptest.NewRecorder()
	api.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/login/poll",
		strings.NewReader(`{"device_code":"`+dev.DeviceCode+`"}`)))
	return rr
}

func TestLoginPollUnknownHandle(t *testing.T) {
	api, _, _ := newTestAPI(t)
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/login/poll",
		strings.NewReader(`{"device_code":"nope"}`)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown-handle poll status = %d, want 400", rr.Code)
	}
}

func TestEnrollWithAccountCredential(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	api := NewAPIWithTunnel(st, NewFakeVerifier(), "relay.getpiper.co:7000", nil, nil, nil)

	acc, _ := st.UpsertAccount("sub-1", "judy")
	cred, _ := st.MintAccountCredential(acc.ID)

	req := httptest.NewRequest(http.MethodPost, "/v1/enroll", nil)
	req.Header.Set("Authorization", "Bearer "+cred)
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("enroll status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var out struct {
		EnrollmentToken string `json:"enrollment_token"`
		BaseDomain      string `json:"base_domain"`
		TunnelEndpoint  string `json:"tunnel_endpoint"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.EnrollmentToken == "" {
		t.Fatal("empty enrollment token")
	}
	if !strings.HasSuffix(out.BaseDomain, "-judy.public.getpiper.co") {
		t.Fatalf("base domain = %q", out.BaseDomain)
	}
	if out.TunnelEndpoint != "relay.getpiper.co:7000" {
		t.Fatalf("tunnel endpoint = %q", out.TunnelEndpoint)
	}
}

func TestEnrollReturnsWebhookSecretAndAppFlag(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s",
	})
	if err != nil {
		t.Fatal(err)
	}
	api := NewAPIWithTunnel(st, NewFakeVerifier(), "relay.getpiper.co:7000", nil, nil, app)

	acc, _ := st.UpsertAccount("sub-1", "judy")
	cred, _ := st.MintAccountCredential(acc.ID)

	req := httptest.NewRequest(http.MethodPost, "/v1/enroll", nil)
	req.Header.Set("Authorization", "Bearer "+cred)
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("enroll status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var out struct {
		WebhookSecret string `json:"webhook_secret"`
		GitHubApp     bool   `json:"github_app"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.WebhookSecret == "" {
		t.Fatal("enroll returned no webhook_secret")
	}
	if !out.GitHubApp {
		t.Fatal("github_app flag not advertised despite a configured App")
	}
}

func TestEnrollRejectsBadCredential(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	api := NewAPIWithTunnel(st, NewFakeVerifier(), "relay:7000", nil, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/enroll", nil)
	req.Header.Set("Authorization", "Bearer nope")
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad-cred enroll status = %d, want 401", rr.Code)
	}
}

func TestEnrollOverCapReturns429(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 1, 10, 5)
	api := NewAPIWithTunnel(st, NewFakeVerifier(), "relay:7000", nil, nil, nil)
	acc, _ := st.UpsertAccount("sub-1", "ken")
	cred, _ := st.MintAccountCredential(acc.ID)

	do := func() int {
		req := httptest.NewRequest(http.MethodPost, "/v1/enroll", nil)
		req.Header.Set("Authorization", "Bearer "+cred)
		rr := httptest.NewRecorder()
		api.ServeHTTP(rr, req)
		return rr.Code
	}
	if c := do(); c != http.StatusOK {
		t.Fatalf("first enroll = %d, want 200", c)
	}
	if c := do(); c != http.StatusTooManyRequests {
		t.Fatalf("over-cap enroll = %d, want 429", c)
	}
}

// startWebLogin drives GET /v1/login/web and returns the minted state and the
// state cookie. The FakeVerifier's AuthCodeURL embeds the state, so it's
// recoverable from the redirect Location.
func startWebLogin(t *testing.T, api http.Handler, redirectURI string) (state string, cookie *http.Cookie) {
	t.Helper()
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
		"/v1/login/web?redirect_uri="+url.QueryEscape(redirectURI), nil))
	if rr.Code != http.StatusFound {
		t.Fatalf("web login status = %d, body = %s", rr.Code, rr.Body.String())
	}
	loc, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatalf("bad Location: %v", err)
	}
	state = loc.Query().Get("state")
	if state == "" {
		t.Fatalf("no state in redirect %q", loc)
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == "piper_login_state" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no piper_login_state cookie set")
	}
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie flags = %+v, want HttpOnly Secure SameSite=Lax", cookie)
	}
	return state, cookie
}

func newWebTestAPI(t *testing.T) (http.Handler, *FakeVerifier) {
	t.Helper()
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	fv := NewFakeVerifier()
	api := NewAPIWithTunnel(st, fv, "", nil, []string{"https://dash.getpiper.co/"}, nil)
	return api, fv
}

// installationAccountStub fakes the GitHub "get an installation" endpoint,
// reporting the given account id for any installation id it's asked about.
func installationAccountStub(t *testing.T, accountID string) *GitHubApp {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"account":{"id":` + accountID + `,"login":"whoever","type":"User"}}`))
	}))
	t.Cleanup(srv.Close)
	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s", APIBase: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	return app
}

// TestLoginCallbackIgnoresInstallationID pins down that login and
// installation are separate flows: even when the callback query smuggles an
// installation_id the authenticated user really owns (per the stub), the
// callback must not link it — linking happens only via the HMAC-signed
// installation webhook. The login itself must still succeed.
func TestLoginCallbackIgnoresInstallationID(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	fv := NewFakeVerifier()
	app := installationAccountStub(t, "583231")
	api := NewAPIWithTunnel(st, fv, "", nil, []string{"https://dash.getpiper.co/"}, app)

	state, cookie := startWebLogin(t, api, "https://dash.getpiper.co/auth")
	fv.GrantCode("code-1", Identity{Subject: "583231", Login: "ivan"})
	req := httptest.NewRequest(http.MethodGet,
		"/v1/login/callback?code=code-1&state="+url.QueryEscape(state)+
			"&installation_id=55&setup_action=install", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	if rr.Code != http.StatusFound {
		t.Fatalf("callback status = %d, body = %s", rr.Code, rr.Body.String())
	}

	acc, err := st.UpsertAccount("583231", "ivan") // idempotent: fetches the row the callback created
	if err != nil {
		t.Fatal(err)
	}
	if insts, err := st.InstallationsForAccount(acc.ID); err != nil || len(insts) != 0 {
		t.Fatalf("InstallationsForAccount = %+v, err = %v, want none (callback must not link)", insts, err)
	}
}

func TestWebLoginCallbackHappyPath(t *testing.T) {
	api, fv := newWebTestAPI(t)
	state, cookie := startWebLogin(t, api, "https://dash.getpiper.co/auth")

	fv.GrantCode("code-1", Identity{Subject: "583231", Login: "ivan"})
	req := httptest.NewRequest(http.MethodGet,
		"/v1/login/callback?code=code-1&state="+url.QueryEscape(state), nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	if rr.Code != http.StatusFound {
		t.Fatalf("callback status = %d, body = %s", rr.Code, rr.Body.String())
	}
	loc, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatalf("bad Location: %v", err)
	}
	if got := loc.Scheme + "://" + loc.Host + loc.Path; got != "https://dash.getpiper.co/auth" {
		t.Fatalf("redirect target = %q", got)
	}
	frag, err := url.ParseQuery(loc.Fragment)
	if err != nil {
		t.Fatalf("bad fragment %q: %v", loc.Fragment, err)
	}
	if frag.Get("credential") == "" || frag.Get("username") != "ivan" {
		t.Fatalf("fragment = %q", loc.Fragment)
	}
	var stateCookieOut *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == stateCookie {
			stateCookieOut = c
		}
	}
	if stateCookieOut == nil {
		t.Fatal("callback response did not clear the state cookie")
	}
	if stateCookieOut.MaxAge >= 0 {
		t.Fatalf("state cookie MaxAge = %d, want < 0 (expired)", stateCookieOut.MaxAge)
	}
}

func TestWebLoginRejectsDisallowedRedirect(t *testing.T) {
	api, _ := newWebTestAPI(t)
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
		"/v1/login/web?redirect_uri="+url.QueryEscape("https://evil.example/auth"), nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("disallowed redirect status = %d, want 400", rr.Code)
	}
}

func TestWebLoginRejectsFragmentRedirect(t *testing.T) {
	api, _ := newWebTestAPI(t)
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
		"/v1/login/web?redirect_uri="+url.QueryEscape("https://dash.getpiper.co/#x"), nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("fragment redirect status = %d, want 400", rr.Code)
	}
}

func TestWebLoginCallbackStateSingleUse(t *testing.T) {
	api, fv := newWebTestAPI(t)
	state, cookie := startWebLogin(t, api, "https://dash.getpiper.co/auth")
	fv.GrantCode("code-1", Identity{Subject: "583231", Login: "ivan"})

	do := func() int {
		req := httptest.NewRequest(http.MethodGet,
			"/v1/login/callback?code=code-1&state="+url.QueryEscape(state), nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		api.ServeHTTP(rr, req)
		return rr.Code
	}
	if c := do(); c != http.StatusFound {
		t.Fatalf("first callback = %d, want 302", c)
	}
	if c := do(); c != http.StatusBadRequest {
		t.Fatalf("replayed callback = %d, want 400", c)
	}
}

func TestWebLoginCallbackRejectsCookieMismatch(t *testing.T) {
	api, fv := newWebTestAPI(t)
	state, _ := startWebLogin(t, api, "https://dash.getpiper.co/auth")
	fv.GrantCode("code-1", Identity{Subject: "583231", Login: "ivan"})

	// No cookie at all.
	req := httptest.NewRequest(http.MethodGet,
		"/v1/login/callback?code=code-1&state="+url.QueryEscape(state), nil)
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("cookieless callback = %d, want 400", rr.Code)
	}

	// Wrong cookie value.
	req = httptest.NewRequest(http.MethodGet,
		"/v1/login/callback?code=code-1&state="+url.QueryEscape(state), nil)
	req.AddCookie(&http.Cookie{Name: "piper_login_state", Value: "someone-else"})
	rr = httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("wrong-cookie callback = %d, want 400", rr.Code)
	}
}

func TestWebLoginCallbackExchangeFailure(t *testing.T) {
	api, _ := newWebTestAPI(t) // no GrantCode → Exchange fails
	state, cookie := startWebLogin(t, api, "https://dash.getpiper.co/auth")

	req := httptest.NewRequest(http.MethodGet,
		"/v1/login/callback?code=bad&state="+url.QueryEscape(state), nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("failed-exchange callback = %d, want 502", rr.Code)
	}
}

func TestWebLoginSweepsExpiredStates(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	a := &api{st: st, v: NewFakeVerifier(), webv: NewFakeVerifier(),
		webRedirects: []string{"https://dash.getpiper.co/"}, webStates: map[string]webState{}}
	a.webStates["stale"] = webState{redirectURI: "https://dash.getpiper.co/x", expires: time.Now().Add(-time.Minute)}

	rr := httptest.NewRecorder()
	a.loginWeb(rr, httptest.NewRequest(http.MethodGet,
		"/v1/login/web?redirect_uri="+url.QueryEscape("https://dash.getpiper.co/auth"), nil))
	if rr.Code != http.StatusFound {
		t.Fatalf("web login status = %d", rr.Code)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.webStates["stale"]; ok {
		t.Fatal("expired state not swept on new login")
	}
	if len(a.webStates) != 1 {
		t.Fatalf("webStates size = %d, want 1 (only the fresh state)", len(a.webStates))
	}
}

func TestWebLoginNotConfigured(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	api := NewAPI(st, NewFakeVerifier()) // no webRedirects

	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
		"/v1/login/web?redirect_uri="+url.QueryEscape("https://dash.getpiper.co/auth"), nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured web login = %d, want 503", rr.Code)
	}
	rr = httptest.NewRecorder()
	api.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/login/callback?code=x&state=y", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured callback = %d, want 503", rr.Code)
	}
}

// ghAPIStub serves the two GitHub endpoints repo listing touches.
func ghAPIStub(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/55/access_tokens", "/app/installations/66/access_tokens":
			_, _ = w.Write([]byte(`{"token":"t","expires_at":"2026-07-20T12:00:00Z"}`))
		case "/installation/repositories":
			_, _ = w.Write([]byte(`{"repositories":[` +
				`{"full_name":"alice/blog","visibility":"public","pushed_at":"2026-07-20T12:34:56Z"},` +
				`{"full_name":"alice/api","visibility":"private","pushed_at":""}]}`))
		default:
			t.Errorf("unexpected GitHub path %q", r.URL.Path)
		}
	}))
}

// reposAPI builds the account API with a GitHub App pointed at gh.
func reposAPI(t *testing.T, st *Store, gh *httptest.Server) http.Handler {
	t.Helper()
	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s", APIBase: gh.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	return NewAPIWithTunnel(st, NewFakeVerifier(), "", nil, nil, app)
}

func getRepos(t *testing.T, h http.Handler, cred, instID string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/v1/github/repos"
	if instID != "" {
		target += "?installation_id=" + url.QueryEscape(instID)
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if cred != "" {
		req.Header.Set("Authorization", "Bearer "+cred)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestGitHubReposListsInstallationRepos(t *testing.T) {
	gh := ghAPIStub(t)
	defer gh.Close()

	st := openTestStore(t)
	acc, err := st.UpsertAccount("1001", "alice")
	if err != nil {
		t.Fatal(err)
	}
	cred, err := st.MintAccountCredential(acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.LinkInstallation("55", "1001", "user", "alice"); err != nil {
		t.Fatal(err)
	}

	rec := getRepos(t, reposAPI(t, st, gh), cred, "55")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var body struct {
		Repos []Repo `json:"repos"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	want := []Repo{
		{FullName: "alice/blog", Visibility: "public", PushedAt: "2026-07-20T12:34:56Z"},
		{FullName: "alice/api", Visibility: "private", PushedAt: ""},
	}
	if len(body.Repos) != len(want) || body.Repos[0] != want[0] || body.Repos[1] != want[1] {
		t.Fatalf("repos = %+v, want %+v", body.Repos, want)
	}
}

func TestGitHubReposRequiresCredential(t *testing.T) {
	gh := ghAPIStub(t)
	defer gh.Close()
	rec := getRepos(t, reposAPI(t, openTestStore(t), gh), "", "55")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestGitHubReposRequiresInstallationID(t *testing.T) {
	gh := ghAPIStub(t)
	defer gh.Close()
	st := openTestStore(t)
	cred := accountWithCred(t, st)
	rec := getRepos(t, reposAPI(t, st, gh), cred, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestGitHubReposUnknownInstallation(t *testing.T) {
	gh := ghAPIStub(t)
	defer gh.Close()
	st := openTestStore(t)
	cred := accountWithCred(t, st)
	rec := getRepos(t, reposAPI(t, st, gh), cred, "999")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestGitHubReposForeignInstallation: an installation owned by a different
// account must not be readable, reported as 404 (no existence leak).
func TestGitHubReposForeignInstallation(t *testing.T) {
	gh := ghAPIStub(t)
	defer gh.Close()
	st := openTestStore(t)
	cred := accountWithCred(t, st) // account 1001 / alice
	if _, err := st.UpsertAccount("2002", "mallory"); err != nil {
		t.Fatal(err)
	}
	if err := st.LinkInstallation("77", "2002", "user", "mallory"); err != nil {
		t.Fatal(err)
	}
	rec := getRepos(t, reposAPI(t, st, gh), cred, "77")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for foreign installation", rec.Code)
	}
}

type ghStatus struct {
	GitHubApp     bool           `json:"github_app"`
	Installations []Installation `json:"installations"`
	InstallURL    string         `json:"install_url"`
}

// statusAPI builds the account API with a GitHub App that has a slug, so
// install_url is populated (reposAPI omits the slug).
func statusAPI(t *testing.T, st *Store) http.Handler {
	t.Helper()
	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s", Slug: "piper-relay",
	})
	if err != nil {
		t.Fatal(err)
	}
	return NewAPIWithTunnel(st, NewFakeVerifier(), "", nil, nil, app)
}

func getStatus(t *testing.T, h http.Handler, cred string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/github/status", nil)
	if cred != "" {
		req.Header.Set("Authorization", "Bearer "+cred)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func accountWithCred(t *testing.T, st *Store) string {
	t.Helper()
	acc, err := st.UpsertAccount("1001", "alice")
	if err != nil {
		t.Fatal(err)
	}
	cred, err := st.MintAccountCredential(acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	return cred
}

// reconcileAPI builds the account API against a stubbed GitHub whose
// /app/installations returns insts verbatim, and reports how many times that
// endpoint was called.
func reconcileAPI(t *testing.T, st *Store, insts string) (*api, http.Handler, *int) {
	t.Helper()
	calls := 0
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations" {
			t.Errorf("unexpected github path %q", r.URL.Path)
		}
		calls++
		_, _ = w.Write([]byte(insts))
	}))
	t.Cleanup(gh.Close)
	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s",
		Slug: "piper-relay", APIBase: gh.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	a, h := newAPI(st, NewFakeVerifier(), "", nil, nil, app)
	return a, h, &calls
}

// An App that is already installed fires no webhook, so an account with nothing
// on record would wait out login's 10-minute install poll and see an empty
// `piper github repos` forever. Status asks GitHub directly instead (#470).
func TestGitHubStatusReconcilesWhenNothingIsOnRecord(t *testing.T) {
	st := openTestStore(t)
	cred := accountWithCred(t, st) // github id 1001, login alice
	_, api, calls := reconcileAPI(t, st, `[{"id":55,"account":{"id":1001,"login":"alice","type":"User"}}]`)

	rec := getStatus(t, api, cred)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var out ghStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Installations) != 1 || out.Installations[0].ID != "55" {
		t.Fatalf("installations = %+v, want the reconciled one", out.Installations)
	}
	if out.Installations[0].TargetLogin != "alice" || out.Installations[0].TargetType != "user" {
		t.Errorf("target = %+v, want user/alice", out.Installations[0])
	}
	if *calls != 1 {
		t.Errorf("github calls = %d, want 1", *calls)
	}

	// The link is durable: the next status is served from the store.
	if rec := getStatus(t, api, cred); rec.Code != http.StatusOK {
		t.Fatalf("second status = %d", rec.Code)
	}
	if *calls != 1 {
		t.Errorf("github calls after a satisfied status = %d, want still 1", *calls)
	}
}

// The App's listing spans every tenant, so reconciliation must link only what
// the asking account can prove it owns. Another user's installation is not it.
func TestGitHubStatusReconcileIgnoresAnotherTenantsInstallation(t *testing.T) {
	st := openTestStore(t)
	cred := accountWithCred(t, st) // github id 1001
	_, api, _ := reconcileAPI(t, st, `[{"id":99,"account":{"id":2002,"login":"mallory","type":"User"}}]`)

	rec := getStatus(t, api, cred)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var out ghStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Installations) != 0 {
		t.Fatalf("claimed another account's installation: %+v", out.Installations)
	}
	if _, err := st.AccountForInstallation("99"); !errors.Is(err, ErrNoInstallation) {
		t.Errorf("installation 99 was linked; err = %v", err)
	}
}

// An org-target installation with no linked Piper org has no provable owner
// from a reconcile's point of view — the webhook's installer identity is what
// resolves it. Claiming it for whoever asks first would be a tenancy hole.
func TestGitHubStatusReconcileSkipsUnlinkedOrgInstallation(t *testing.T) {
	st := openTestStore(t)
	cred := accountWithCred(t, st)
	_, api, _ := reconcileAPI(t, st, `[{"id":77,"account":{"id":3003,"login":"acme","type":"Organization"}}]`)

	if rec := getStatus(t, api, cred); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if _, err := st.AccountForInstallation("77"); !errors.Is(err, ErrNoInstallation) {
		t.Errorf("unlinked org installation was claimed; err = %v", err)
	}
}

// ghOrgWithMember builds a Piper org that has declared a GitHub login, owned by
// a fresh account, and returns the member's credential plus the org id.
func ghOrgWithMember(t *testing.T, st *Store, githubID, login, orgName, orgGitHub string) (cred, orgID string) {
	t.Helper()
	acc, err := st.UpsertAccount(githubID, login)
	if err != nil {
		t.Fatal(err)
	}
	cred, err = st.MintAccountCredential(acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	org, err := st.CreateOrg(acc.ID, orgName) // creator ⇒ owner ⇒ member
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetOrgGitHub(org.ID, orgGitHub); err != nil {
		t.Fatal(err)
	}
	return cred, org.ID
}

// An org-target installation reconciles to the Piper org the asking account
// belongs to — never to the asking user, which would hand one member's
// identity an org-wide installation. It must nonetheless be visible in the
// very response that reconciled it: linking it somewhere the caller cannot see
// leaves #470's symptom exactly as it was, with login waiting out its ten
// minutes and `piper github repos` empty.
func TestGitHubStatusReconcilesOrgInstallationForAMember(t *testing.T) {
	st := openTestStore(t)
	cred, orgID := ghOrgWithMember(t, st, "1001", "alice", "acme", "Acme-Inc")
	_, api, _ := reconcileAPI(t, st, `[{"id":77,"account":{"id":3003,"login":"acme-inc","type":"Organization"}}]`)

	rec := getStatus(t, api, cred)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var out ghStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Installations) != 1 || out.Installations[0].ID != "77" {
		t.Fatalf("installations = %+v, want the reconciled org installation", out.Installations)
	}
	if out.Installations[0].TargetType != "org" || out.Installations[0].TargetLogin != "acme-inc" {
		t.Errorf("target = %+v, want org/acme-inc", out.Installations[0])
	}

	// Ownership stays with the org, not the member who triggered the reconcile.
	owner, err := st.AccountForInstallation("77")
	if err != nil {
		t.Fatalf("org installation was not reconciled: %v", err)
	}
	if owner != orgID {
		t.Errorf("installation linked to %q, want the org account %q", owner, orgID)
	}
}

// A member may list the org installation's repositories: same trust boundary
// as AgentsVisibleTo, which already lets a member drive the org's boxes.
func TestGitHubReposAllowsAnOrgMember(t *testing.T) {
	gh := ghAPIStub(t)
	defer gh.Close()
	st := openTestStore(t)
	cred, orgID := ghOrgWithMember(t, st, "1001", "alice", "acme", "Acme-Inc")
	if err := st.LinkInstallationForAccount("55", orgID, "org", "acme-inc"); err != nil {
		t.Fatal(err)
	}

	rec := getRepos(t, reposAPI(t, st, gh), cred, "55")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a member; body = %s", rec.Code, rec.Body)
	}
}

// Membership is the whole authorization: a user who is not in the Piper org
// gets the same 404 as any other foreign installation — no existence leak.
func TestGitHubReposRejectsANonMemberOfTheOrg(t *testing.T) {
	gh := ghAPIStub(t)
	defer gh.Close()
	st := openTestStore(t)
	_, orgID := ghOrgWithMember(t, st, "1001", "alice", "acme", "Acme-Inc")
	if err := st.LinkInstallationForAccount("77", orgID, "org", "acme-inc"); err != nil {
		t.Fatal(err)
	}
	// mallory has an account, but no membership in acme.
	mallory, err := st.UpsertAccount("2002", "mallory")
	if err != nil {
		t.Fatal(err)
	}
	outsider, err := st.MintAccountCredential(mallory.ID)
	if err != nil {
		t.Fatal(err)
	}

	if rec := getRepos(t, reposAPI(t, st, gh), outsider, "77"); rec.Code != http.StatusNotFound {
		t.Fatalf("repos status = %d, want 404 for a non-member", rec.Code)
	}
	// ...and it must not surface in their status listing either.
	_, sapi, _ := reconcileAPI(t, st, `[]`)
	rec := getStatus(t, sapi, outsider)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out ghStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Installations) != 0 {
		t.Fatalf("non-member sees the org installation: %+v", out.Installations)
	}
}

// GitHub being unreachable must not fail a status call — login's install poll
// depends on it answering, and an empty answer is the honest one.
func TestGitHubStatusSurvivesAFailedReconcile(t *testing.T) {
	st := openTestStore(t)
	cred := accountWithCred(t, st)
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer gh.Close()
	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s",
		Slug: "piper-relay", APIBase: gh.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	api := NewAPIWithTunnel(st, NewFakeVerifier(), "", nil, nil, app)

	rec := getStatus(t, api, cred)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var out ghStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Installations) != 0 {
		t.Fatalf("installations = %+v, want empty", out.Installations)
	}
}

// login's install poll hits status every few seconds for up to ten minutes. A
// new install fires a webhook, so re-asking GitHub on every poll would spend
// hundreds of app-global listings per waiting user against a shared hourly
// budget. Simulated over the whole install-wait window on a fake clock: the
// throttle has to outlast the window, not merely damp it.
func TestGitHubStatusReconcilesOnceAcrossAWholeInstallWait(t *testing.T) {
	st := openTestStore(t)
	cred := accountWithCred(t, st)
	// Nothing this account owns, so every poll stays on the reconcile path.
	a, api, calls := reconcileAPI(t, st, `[{"id":99,"account":{"id":2002,"login":"mallory","type":"User"}}]`)

	// installPollTimeout is ten minutes and the CLI polls every three seconds.
	const installWait = 10 * time.Minute
	const pollInterval = 3 * time.Second
	clock := time.Now()
	a.now = func() time.Time { return clock }

	for elapsed := time.Duration(0); elapsed <= installWait; elapsed += pollInterval {
		if rec := getStatus(t, api, cred); rec.Code != http.StatusOK {
			t.Fatalf("status at %v = %d", elapsed, rec.Code)
		}
		clock = clock.Add(pollInterval)
	}
	if *calls != 1 {
		t.Fatalf("app-global listings across a %v install wait = %d, want 1", installWait, *calls)
	}

	// Well past the window, a fresh login may ask again.
	clock = clock.Add(reconcileEvery)
	if rec := getStatus(t, api, cred); rec.Code != http.StatusOK {
		t.Fatalf("status after the window = %d", rec.Code)
	}
	if *calls != 2 {
		t.Errorf("listings after the interval lapsed = %d, want 2", *calls)
	}
}

// A GitHub outage must leave status answering from the store rather than
// failing, and must not turn every polling client into a retry loop against
// the shared budget.
func TestGitHubStatusFailedReconcileKeepsStoredAnswerAndStaysThrottled(t *testing.T) {
	st := openTestStore(t)
	cred := accountWithCred(t, st) // github id 1001 / alice
	// A linked installation the store already knows about, so there is an
	// honest stored answer to preserve...
	if err := st.LinkInstallation("55", "1001", "user", "alice"); err != nil {
		t.Fatal(err)
	}
	calls := 0
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer gh.Close()
	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s",
		Slug: "piper-relay", APIBase: gh.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	a, api := newAPI(st, NewFakeVerifier(), "", nil, nil, app)
	clock := time.Now()
	a.now = func() time.Time { return clock }

	rec := getStatus(t, api, cred)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var out ghStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Installations) != 1 || out.Installations[0].ID != "55" {
		t.Fatalf("installations = %+v, want the stored answer", out.Installations)
	}
	// A stored answer means the reconcile path is never entered at all.
	if calls != 0 {
		t.Errorf("asked github despite a stored installation: %d call(s)", calls)
	}

	// Now with nothing stored: the attempt fails, status still answers, and the
	// failure is throttled like any other attempt.
	if err := st.UnlinkInstallation("55"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if rec := getStatus(t, api, cred); rec.Code != http.StatusOK {
			t.Fatalf("status after github failure = %d, body %s", rec.Code, rec.Body.String())
		}
		clock = clock.Add(time.Minute)
	}
	if calls != 1 {
		t.Errorf("failed reconciles retried %d times, want 1 within the interval", calls)
	}
}

func TestGitHubStatusInstalled(t *testing.T) {
	st := openTestStore(t)
	cred := accountWithCred(t, st)
	if err := st.LinkInstallation("55", "1001", "user", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := st.LinkInstallation("66", "1001", "org", "getpiper"); err != nil {
		t.Fatal(err)
	}

	rec := getStatus(t, statusAPI(t, st), cred)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var got ghStatus
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	wantInst := []Installation{
		{ID: "66", TargetType: "org", TargetLogin: "getpiper"},
		{ID: "55", TargetType: "user", TargetLogin: "alice"},
	}
	if !got.GitHubApp || got.InstallURL != "https://github.com/apps/piper-relay/installations/new" ||
		len(got.Installations) != len(wantInst) ||
		got.Installations[0] != wantInst[0] || got.Installations[1] != wantInst[1] {
		t.Fatalf("status = %+v, want github_app + %+v", got, wantInst)
	}
}

// TestGitHubStatusLabelsOrgInstallByOrgLogin is the #321 Gap-2 regression: a
// personal login whose only installation targets an org must report the org as
// the installation's target_login, not the logged-in username.
func TestGitHubStatusLabelsOrgInstallByOrgLogin(t *testing.T) {
	st := openTestStore(t)
	cred := accountWithCred(t, st) // account "1001", login "alice"
	if err := st.LinkInstallation("66", "1001", "org", "getpiper"); err != nil {
		t.Fatal(err)
	}
	rec := getStatus(t, statusAPI(t, st), cred)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var got ghStatus
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Installations) != 1 ||
		got.Installations[0].TargetLogin != "getpiper" || got.Installations[0].TargetType != "org" {
		t.Fatalf("installations = %+v, want single org getpiper (not login alice)", got.Installations)
	}
}

func TestGitHubStatusNotInstalled(t *testing.T) {
	st := openTestStore(t)
	cred := accountWithCred(t, st)

	rec := getStatus(t, statusAPI(t, st), cred)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var got ghStatus
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !got.GitHubApp || got.InstallURL != "https://github.com/apps/piper-relay/installations/new" ||
		len(got.Installations) != 0 {
		t.Fatalf("status = %+v, want github_app + no installations", got)
	}
}

func TestGitHubStatusRequiresCredential(t *testing.T) {
	rec := getStatus(t, statusAPI(t, openTestStore(t)), "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestGitHubStatusNoAppConfigured(t *testing.T) {
	st := openTestStore(t)
	cred := accountWithCred(t, st)

	// No GitHub App wired: status still answers (200) so the dashboard learns
	// the App isn't available, rather than a 503 it would treat as an outage.
	rec := getStatus(t, NewAPI(st, NewFakeVerifier()), cred)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var got ghStatus
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.GitHubApp || got.InstallURL != "" || len(got.Installations) != 0 {
		t.Fatalf("status = %+v, want github_app:false + no installations", got)
	}
}
