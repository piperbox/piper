package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func newCLILoginAPI(t *testing.T, slug string) (http.Handler, *FakeVerifier, *Store) {
	t.Helper()
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	fv := NewFakeVerifier()
	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s", Slug: slug,
	})
	if err != nil {
		t.Fatal(err)
	}
	api := NewAPIWithTunnel(st, fv, "", nil, nil, app, nil)
	return api, fv, st
}

// confirmCode drives the browser code-entry step: POST the code, expect a
// redirect to the GitHub authorize URL, and return the state + binding cookie.
func confirmCode(t *testing.T, api http.Handler, code string) (state string, cookie *http.Cookie) {
	t.Helper()
	form := url.Values{"code": {code}}
	req := httptest.NewRequest(http.MethodPost, "/v1/login/cli", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	if rr.Code != http.StatusFound {
		t.Fatalf("confirm status = %d, body = %s", rr.Code, rr.Body.String())
	}
	loc, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatalf("bad Location: %v", err)
	}
	state = loc.Query().Get("state")
	if state == "" {
		t.Fatalf("no state in authorize redirect %q", rr.Header().Get("Location"))
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == stateCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("confirm set no binding cookie")
	}
	return state, cookie
}

// TestCLILoginBrokeredFlowBouncesToInstall walks the whole one-trip CLI login:
// start → poll(pending) → confirm the user code → GitHub callback → the browser
// is bounced to the install page (first-timer), and the CLI's poll returns the
// credential plus the install URL.
func TestCLILoginBrokeredFlowBouncesToInstall(t *testing.T) {
	api, fv, _ := newCLILoginAPI(t, "piper-app")

	// 1. CLI starts the flow.
	rr := apiReq(t, api, "POST", "/v1/login/cli/start", "", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("start = %d, body %s", rr.Code, rr.Body.String())
	}
	var start struct {
		Handle   string `json:"handle"`
		UserCode string `json:"user_code"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &start); err != nil {
		t.Fatal(err)
	}
	if start.Handle == "" || start.UserCode == "" {
		t.Fatalf("start body = %s", rr.Body.String())
	}

	// 2. Nothing to collect yet.
	if rr := apiReq(t, api, "POST", "/v1/login/cli/poll", "", `{"handle":"`+start.Handle+`"}`); rr.Code != http.StatusAccepted {
		t.Fatalf("early poll = %d, want 202", rr.Code)
	}

	// 3. The human enters the code shown in their terminal.
	state, cookie := confirmCode(t, api, start.UserCode)
	if state != start.Handle {
		t.Fatalf("authorize state = %q, want handle %q", state, start.Handle)
	}

	// 4. GitHub redirects back after the user authorizes. The account has no
	// installation yet, so the browser is bounced to the install page.
	fv.GrantCode("code-1", Identity{Subject: "42", Login: "alice"})
	req := httptest.NewRequest(http.MethodGet, "/v1/login/callback?code=code-1&state="+url.QueryEscape(state), nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("callback = %d, body %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "/apps/piper-app/installations/new") {
		t.Fatalf("callback did not bounce to install: %q", loc)
	}

	// 5. The CLI collects the credential and the install URL.
	rr2 := apiReq(t, api, "POST", "/v1/login/cli/poll", "", `{"handle":"`+start.Handle+`"}`)
	if rr2.Code != http.StatusOK {
		t.Fatalf("final poll = %d, body %s", rr2.Code, rr2.Body.String())
	}
	var out struct {
		AccountCredential string `json:"account_credential"`
		Username          string `json:"username"`
		InstallURL        string `json:"install_url"`
	}
	if err := json.Unmarshal(rr2.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.AccountCredential == "" || out.Username != "alice" {
		t.Fatalf("poll body = %s", rr2.Body.String())
	}
	if !strings.Contains(out.InstallURL, "/apps/piper-app/installations/new") {
		t.Fatalf("install_url = %q", out.InstallURL)
	}
}

// The brokered browser login is the same door as the device flow, so the
// kill-switch must answer the same way: a disabled account's callback is
// refused instead of minting a credential for the CLI to collect (#81).
func TestCLILoginDisabledAccountIsForbidden(t *testing.T) {
	api, fv, st := newCLILoginAPI(t, "piper-app")
	if _, err := st.UpsertAccount("42", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := st.DisableAccount("alice", "user"); err != nil {
		t.Fatal(err)
	}

	handle, cookie := startCLILogin(t, api)
	fv.GrantCode("code-1", Identity{Subject: "42", Login: "alice"})
	req := httptest.NewRequest(http.MethodGet, "/v1/login/callback?code=code-1&state="+url.QueryEscape(handle), nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("disabled callback = %d, want 403; body %s", rec.Code, rec.Body.String())
	}

	// The CLI's poll must stay pending, not collect a credential.
	if rr := apiReq(t, api, "POST", "/v1/login/cli/poll", "", `{"handle":"`+handle+`"}`); rr.Code == http.StatusOK {
		t.Fatalf("poll handed back a credential after a disabled callback: %s", rr.Body.String())
	}
}

// startCLILogin runs start + confirm and returns the handle and binding cookie.
func startCLILogin(t *testing.T, api http.Handler) (handle string, cookie *http.Cookie) {
	t.Helper()
	rr := apiReq(t, api, "POST", "/v1/login/cli/start", "", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("start = %d", rr.Code)
	}
	var start struct {
		Handle   string `json:"handle"`
		UserCode string `json:"user_code"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &start); err != nil {
		t.Fatal(err)
	}
	state, cookie := confirmCode(t, api, start.UserCode)
	if state != start.Handle {
		t.Fatalf("state %q != handle %q", state, start.Handle)
	}
	return start.Handle, cookie
}

// An account that already has an installation gets no install bounce: the
// callback lands on the done page and the poll carries an empty install URL.
func TestCLILoginAlreadyInstalledShowsDonePage(t *testing.T) {
	api, fv, st := newCLILoginAPI(t, "piper-app")
	acc, err := st.UpsertAccount("42", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.LinkInstallation("999", "42", "user", "alice"); err != nil {
		t.Fatal(err)
	}
	_ = acc

	handle, cookie := startCLILogin(t, api)
	fv.GrantCode("code-1", Identity{Subject: "42", Login: "alice"})
	req := httptest.NewRequest(http.MethodGet, "/v1/login/callback?code=code-1&state="+handle, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("callback = %d (want 200 done page), body %s", rec.Code, rec.Body.String())
	}

	rr := apiReq(t, api, "POST", "/v1/login/cli/poll", "", `{"handle":"`+handle+`"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("poll = %d", rr.Code)
	}
	var out struct {
		InstallURL string `json:"install_url"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out.InstallURL != "" {
		t.Fatalf("install_url = %q, want empty (already installed)", out.InstallURL)
	}
}

// The cookie binds the authorizing browser to the one that entered the code: a
// callback whose cookie doesn't match state is rejected, and the login stays
// pending — the login-CSRF guard.
func TestCLILoginCallbackRejectsCookieMismatch(t *testing.T) {
	api, fv, _ := newCLILoginAPI(t, "piper-app")
	handle, _ := startCLILogin(t, api)
	fv.GrantCode("code-1", Identity{Subject: "42", Login: "alice"})

	req := httptest.NewRequest(http.MethodGet, "/v1/login/callback?code=code-1&state="+handle, nil)
	req.AddCookie(&http.Cookie{Name: stateCookie, Value: "someone-elses-handle"})
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("callback = %d, want 400 on cookie mismatch", rec.Code)
	}
	if rr := apiReq(t, api, "POST", "/v1/login/cli/poll", "", `{"handle":"`+handle+`"}`); rr.Code != http.StatusAccepted {
		t.Fatalf("poll after rejected callback = %d, want 202 pending", rr.Code)
	}
}

// A confirmed handle is required: a callback for a handle whose code was never
// entered is rejected even with a matching cookie.
func TestCLILoginCallbackRequiresConfirmation(t *testing.T) {
	api, fv, _ := newCLILoginAPI(t, "piper-app")
	rr := apiReq(t, api, "POST", "/v1/login/cli/start", "", "")
	var start struct {
		Handle string `json:"handle"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &start)
	fv.GrantCode("code-1", Identity{Subject: "42", Login: "alice"})

	req := httptest.NewRequest(http.MethodGet, "/v1/login/callback?code=code-1&state="+start.Handle, nil)
	req.AddCookie(&http.Cookie{Name: stateCookie, Value: start.Handle})
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unconfirmed callback = %d, want 400", rec.Code)
	}
}

// A wrong code re-renders the entry page (HTML, 200) rather than redirecting to
// GitHub, and leaves the handle unconfirmed.
func TestCLILoginConfirmRejectsWrongCode(t *testing.T) {
	api, _, _ := newCLILoginAPI(t, "piper-app")
	if rr := apiReq(t, api, "POST", "/v1/login/cli/start", "", ""); rr.Code != http.StatusOK {
		t.Fatalf("start = %d", rr.Code)
	}
	form := url.Values{"code": {"0000-0000"}}
	req := httptest.NewRequest(http.MethodPost, "/v1/login/cli", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("wrong code = %d, want 200 re-render", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Fatalf("wrong code redirected to %q, want no redirect", loc)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
}

// Brokered CLI login needs an App configured; without one, start is 503.
func TestCLILoginStartRequiresApp(t *testing.T) {
	st := openTestStore(t)
	api := NewAPIWithTunnel(st, NewFakeVerifier(), "", nil, nil, nil, nil)
	if rr := apiReq(t, api, "POST", "/v1/login/cli/start", "", ""); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("start without app = %d, want 503", rr.Code)
	}
}
