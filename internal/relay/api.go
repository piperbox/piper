package relay

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// NewAPI returns the account API without a tunnel endpoint, control proxy, or
// web login (tests / LAN). Use NewAPIWithTunnel in production.
func NewAPI(st *Store, v Verifier) http.Handler { return NewAPIWithTunnel(st, v, "", nil, nil, nil) }

// NewAPIWithTunnel is the full account-facing API: device login, browser
// (authorization-code) login, enroll, and — when router is non-nil — the
// /agents/ control proxy (#73). webRedirects is the allowlist of redirect_uri
// prefixes for the browser flow; empty disables web login (503). ghApp is nil
// when the relay holds no GitHub App, in which case enroll advertises
// "github_app": false and boxes stay on the BYO path.
func NewAPIWithTunnel(st *Store, v Verifier, tunnelEndpoint string, router *Router, webRedirects []string, ghApp *GitHubApp) http.Handler {
	a := &api{st: st, v: v, tunnelEndpoint: tunnelEndpoint,
		webRedirects: webRedirects, webStates: map[string]webState{},
		cliStates: map[string]*cliLogin{}, ghApp: ghApp}
	if wv, ok := v.(WebVerifier); ok {
		a.webv = wv
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/login/device", a.loginDevice)
	mux.HandleFunc("POST /v1/login/poll", a.loginPoll)
	mux.HandleFunc("GET /v1/login/web", a.loginWeb)
	mux.HandleFunc("POST /v1/login/cli/start", a.cliLoginStart)
	mux.HandleFunc("GET /v1/login/cli", a.cliLoginPage)
	mux.HandleFunc("POST /v1/login/cli", a.cliLoginConfirm)
	mux.HandleFunc("POST /v1/login/cli/poll", a.cliLoginPoll)
	mux.HandleFunc("GET /v1/login/callback", a.loginCallback)
	mux.HandleFunc("POST /v1/enroll", a.enroll)
	mux.HandleFunc("GET /v1/github/repos", a.githubRepos)
	mux.HandleFunc("GET /v1/github/status", a.githubStatus)
	a.registerOrgRoutes(mux)
	if router != nil {
		proxy := NewControlProxy(st, router)
		// Bare /agents (account's agent list, #98) plus the per-agent subtree;
		// registering the exact path avoids ServeMux's implicit /agents → /agents/
		// redirect.
		mux.Handle("/agents", proxy)
		mux.Handle("/agents/", proxy)
	}
	return mux
}

type api struct {
	st             *Store
	v              Verifier
	webv           WebVerifier // nil ⇒ web login disabled
	tunnelEndpoint string
	webRedirects   []string   // allowed redirect_uri prefixes; empty ⇒ web login disabled
	ghApp          *GitHubApp // nil ⇒ relay serves BYO users only

	mu        sync.Mutex
	webStates map[string]webState  // state → pending dashboard browser flow
	cliStates map[string]*cliLogin // handle → pending CLI browser login (#291)

	// Shared per-IP bucket for the two unauthenticated login endpoints (#106):
	// one budget per IP, so hammering one endpoint can't dodge the limit by
	// switching to the other.
	loginLimit loginLimiter
}

// webState is a pending browser login: where to send the credential, and how
// long the state stays redeemable.
type webState struct {
	redirectURI string
	expires     time.Time
}

const stateCookie = "piper_login_state"

// webLoginEnabled gates both browser endpoints: a WebVerifier must be wired
// and at least one redirect prefix allowed.
func (a *api) webLoginEnabled() bool { return a.webv != nil && len(a.webRedirects) > 0 }

func (a *api) redirectAllowed(uri string) bool {
	if uri == "" || strings.Contains(uri, "#") {
		return false
	}
	for _, p := range a.webRedirects {
		if strings.HasPrefix(uri, p) {
			return true
		}
	}
	return false
}

// loginWeb starts the browser flow: bind a fresh state to the validated
// redirect_uri (server-side map + browser cookie), then hand the browser to
// the IdP.
func (a *api) loginWeb(w http.ResponseWriter, r *http.Request) {
	if !a.loginLimit.allow(clientIP(r)) {
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	if !a.webLoginEnabled() {
		http.Error(w, "web login not configured", http.StatusServiceUnavailable)
		return
	}
	ru := r.URL.Query().Get("redirect_uri")
	if !a.redirectAllowed(ru) {
		http.Error(w, "redirect_uri not allowed", http.StatusBadRequest)
		return
	}
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	state := hex.EncodeToString(raw)
	now := time.Now()
	a.mu.Lock()
	for s, ws := range a.webStates {
		if now.After(ws.expires) {
			delete(a.webStates, s)
		}
	}
	a.webStates[state] = webState{redirectURI: ru, expires: now.Add(10 * time.Minute)}
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: stateCookie, Value: state, MaxAge: 600, Path: "/v1/login",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, a.webv.AuthCodeURL(state), http.StatusFound)
}

// loginCallback finishes the browser flow: state must match both the
// server-side map (single-use, unexpired) and the browser's cookie
// (login-CSRF guard); then code → identity → account → credential, delivered
// in the URL fragment so it never reaches server logs.
func (a *api) loginCallback(w http.ResponseWriter, r *http.Request) {
	// The App's single callback URL serves both browser flows; a CLI handle
	// (#291) is completed here and collected by the CLI's poll.
	if a.cliCallback(w, r) {
		return
	}
	if !a.webLoginEnabled() {
		http.Error(w, "web login not configured", http.StatusServiceUnavailable)
		return
	}
	state, code := r.URL.Query().Get("state"), r.URL.Query().Get("code")
	c, err := r.Cookie(stateCookie)
	if state == "" || code == "" || err != nil || c.Value != state {
		http.Error(w, "bad state", http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	ws, ok := a.webStates[state]
	delete(a.webStates, state) // single use
	a.mu.Unlock()
	if !ok || time.Now().After(ws.expires) {
		http.Error(w, "bad state", http.StatusBadRequest)
		return
	}
	id, err := a.webv.Exchange(r.Context(), code)
	if err != nil {
		log.Printf("relay: web login code exchange failed: %v", err)
		http.Error(w, "code exchange failed", http.StatusBadGateway)
		return
	}
	acc, err := a.st.UpsertAccount(id.Subject, id.Login)
	if err != nil {
		http.Error(w, "account error", http.StatusInternalServerError)
		return
	}
	if denyDisabled(w, acc) {
		return
	}
	// Login carries no installation linking: the authorize redirect never
	// includes an installation_id, and installations link through the
	// HMAC-signed "installation" webhook instead — an unsigned query
	// parameter here could otherwise rebind someone else's installation.
	cred, err := a.st.MintAccountCredential(acc.ID)
	if err != nil {
		http.Error(w, "credential error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: stateCookie, Value: "", MaxAge: -1, Path: "/v1/login",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r,
		ws.redirectURI+"#credential="+url.QueryEscape(cred)+"&username="+url.QueryEscape(acc.Username),
		http.StatusFound)
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func (a *api) loginDevice(w http.ResponseWriter, r *http.Request) {
	if !a.loginLimit.allow(clientIP(r)) {
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	handle, d, err := a.v.Start(r.Context())
	if err != nil {
		http.Error(w, "device flow start failed", http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user_code":        d.UserCode,
		"verification_uri": d.VerificationURI,
		"device_code":      handle,
		"interval":         d.Interval,
		"expires_in":       d.ExpiresIn,
	})
}

func (a *api) loginPoll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceCode string `json:"device_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.DeviceCode == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	id, err := a.v.Poll(r.Context(), req.DeviceCode)
	if errors.Is(err, ErrAuthPending) {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "authorization_pending"})
		return
	}
	if err != nil {
		http.Error(w, "unknown or failed device code", http.StatusBadRequest)
		return
	}
	acc, err := a.st.UpsertAccount(id.Subject, id.Login)
	if err != nil {
		http.Error(w, "account error", http.StatusInternalServerError)
		return
	}
	if denyDisabled(w, acc) {
		return
	}
	cred, err := a.st.MintAccountCredential(acc.ID)
	if err != nil {
		http.Error(w, "credential error", http.StatusInternalServerError)
		return
	}
	resp := map[string]string{
		"account_credential": cred,
		"username":           acc.Username,
	}
	if a.ghApp != nil {
		resp["install_url"] = a.ghApp.InstallURL()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *api) enroll(w http.ResponseWriter, r *http.Request) {
	acc, ok := a.authAccount(w, r)
	if !ok {
		return
	}
	// Optional body: {"org":"<slug>"} enrolls the box into an org the caller
	// owns (no/empty body is personal enrollment); {"box_id":"..."} is the
	// durable identity piperd minted on the box, making the enroll an upsert —
	// see EnrollForAccount.
	var req struct {
		Org   string `json:"org"`
		BoxID string `json:"box_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	targetID := acc.ID
	if req.Org != "" {
		orgID, role, err := a.st.OrgRole(req.Org, acc.ID)
		if errors.Is(err, ErrNoOrg) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "org error", http.StatusInternalServerError)
			return
		}
		if role != "owner" {
			http.Error(w, "owner role required", http.StatusForbidden)
			return
		}
		targetID = orgID
	}
	en, err := a.st.EnrollForAccount(targetID, req.BoxID)
	if errors.Is(err, ErrQuotaExceeded) {
		http.Error(w, "agent quota exceeded", http.StatusTooManyRequests)
		return
	}
	if err != nil {
		http.Error(w, "enroll error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enrollment_token": en.Token,
		"base_domain":      en.BaseDomain,
		"tunnel_endpoint":  a.tunnelEndpoint,
		"webhook_secret":   en.WebhookSecret,
		"github_app":       a.ghApp != nil,
	})
}

// githubRepos lists the repositories reachable through the installation named by
// ?installation_id=, which must belong to the caller (#321). No list is cached:
// it is read live through a fresh installation token, so a repository revoked in
// GitHub disappears here immediately. An unknown or foreign installation id is
// reported as 404 identically — the picker never learns another account's
// installation exists.
func (a *api) githubRepos(w http.ResponseWriter, r *http.Request) {
	acc, ok := a.authAccount(w, r)
	if !ok {
		return
	}
	if a.ghApp == nil {
		http.Error(w, "relay has no github app configured", http.StatusServiceUnavailable)
		return
	}
	instID := r.URL.Query().Get("installation_id")
	if instID == "" {
		http.Error(w, "installation_id required", http.StatusBadRequest)
		return
	}
	owner, err := a.st.AccountForInstallation(instID)
	if errors.Is(err, ErrNoInstallation) || (err == nil && owner != acc.ID) {
		http.Error(w, "github app not installed for this account", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "lookup error", http.StatusInternalServerError)
		return
	}
	repos, err := a.ghApp.Repos(r.Context(), instID)
	if err != nil {
		log.Printf("relay: list repos for %s: %v", acc.Username, err)
		http.Error(w, "github unavailable", http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repos": repos})
}

// githubStatus tells the web dashboard whether the relay holds a GitHub App and,
// if so, every installation linked to the caller's account — each with its own
// target identity (target_type / target_login), so the wizard's label always
// matches the repos an installation serves (#321). Plus the install-and-authorize
// URL to add an installation (#315). It never 404s on a missing installation:
// an empty list is the answer the Connect step needs, not an error — and it
// answers 200 even with no App configured so the dashboard learns
// github_app:false rather than reading a 503 as an outage.
func (a *api) githubStatus(w http.ResponseWriter, r *http.Request) {
	acc, ok := a.authAccount(w, r)
	if !ok {
		return
	}
	resp := map[string]any{
		"github_app":    a.ghApp != nil,
		"installations": []Installation{},
		"install_url":   "",
	}
	if a.ghApp != nil {
		resp["install_url"] = a.ghApp.InstallURL()
		insts, err := a.st.InstallationsForAccount(acc.ID)
		if err != nil {
			http.Error(w, "lookup error", http.StatusInternalServerError)
			return
		}
		if insts != nil {
			resp["installations"] = insts
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// denyDisabled refuses a login for an account the operator kill-switch has cut
// off, writing the 403 itself and reporting whether it did. Every door that
// mints an account credential goes through it. The kill-switch holds regardless
// — a credential minted for a disabled account is inert, since
// AuthenticateAccount rejects it — but handing a cut-off user a success and a
// live-looking secret is a confusing way to say no, and mints a credential row
// per attempt (#81).
func denyDisabled(w http.ResponseWriter, acc Account) bool {
	if !acc.Disabled {
		return false
	}
	http.Error(w, "account disabled", http.StatusForbidden)
	return true
}

// authAccount authenticates the request's bearer account credential, writing
// the error response itself when it fails.
func (a *api) authAccount(w http.ResponseWriter, r *http.Request) (Account, bool) {
	cred, ok := bearerToken(r)
	if !ok {
		http.Error(w, "missing bearer credential", http.StatusUnauthorized)
		return Account{}, false
	}
	acc, err := a.st.AuthenticateAccount(cred)
	if errors.Is(err, ErrBadCredential) || errors.Is(err, ErrUnknownAccount) {
		http.Error(w, "bad credential", http.StatusUnauthorized)
		return Account{}, false
	}
	if err != nil {
		http.Error(w, "auth error", http.StatusInternalServerError)
		return Account{}, false
	}
	return acc, true
}

// bearerToken extracts a "Bearer <tok>" Authorization header. The scheme is
// matched case-insensitively (RFC 7235 auth-scheme tokens are case-insensitive);
// the rest of the parse stays strict — exactly one space, and a non-empty token.
func bearerToken(r *http.Request) (string, bool) {
	scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || tok == "" || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	return tok, true
}
