package relay

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"html"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// cliLogin is one in-flight brokered CLI browser login (#291).
//
// Two bindings keep it safe on a public relay. The user code, shown only in the
// caller's terminal, must be entered in the browser before the flow proceeds —
// so an attacker who phishes a victim into authorizing cannot also supply the
// code, exactly the device-flow trust model. The cookie set at that moment then
// binds the same browser through to the callback. Nothing here links an
// installation — that stays on the HMAC-signed webhook; the callback only reads
// whether one exists yet, to decide whether to bounce the browser to install.
type cliLogin struct {
	userCode  string
	expires   time.Time
	confirmed bool // the user entered the code in the browser
	// Set once the callback completes; collected by the CLI's poll.
	done       bool
	credential string
	username   string
	installURL string // "" once the account already has an installation
}

const cliLoginTTL = 10 * time.Minute

// cliLoginEnabled gates the brokered CLI flow: it needs a web
// (authorization-code) verifier and a configured App (for the install bounce).
func (a *api) cliLoginEnabled() bool { return a.webv != nil && a.ghApp != nil }

func randToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// userCode is a short, human-typeable confirmation code (8 hex chars, dashed).
func userCode() string {
	s := strings.ToUpper(randToken(4))
	return s[:4] + "-" + s[4:]
}

// normalizeCode makes code entry forgiving: case- and dash-insensitive.
func normalizeCode(s string) string {
	return strings.ReplaceAll(strings.ToUpper(strings.TrimSpace(s)), "-", "")
}

// cliLoginStart mints a handle and user code for a brokered browser login and
// returns them to the CLI. The CLI opens <relay>/v1/login/cli, where the user
// enters the code.
func (a *api) cliLoginStart(w http.ResponseWriter, r *http.Request) {
	if !a.loginLimit.allow(clientIP(r)) {
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	if !a.cliLoginEnabled() {
		http.Error(w, "brokered login not configured", http.StatusServiceUnavailable)
		return
	}
	now := time.Now()
	a.mu.Lock()
	for h, s := range a.cliStates {
		if now.After(s.expires) {
			delete(a.cliStates, h)
		}
	}
	handle, code := randToken(16), userCode()
	a.cliStates[handle] = &cliLogin{userCode: code, expires: now.Add(cliLoginTTL)}
	a.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"handle": handle, "user_code": code})
}

func (a *api) cliLoginPage(w http.ResponseWriter, r *http.Request) {
	a.renderCLILoginPage(w, "")
}

func (a *api) renderCLILoginPage(w http.ResponseWriter, errMsg string) {
	// text/html is explicit: the body starts with <!doctype, but relying on
	// content sniffing risks a text/plain guess that shows source, not a form.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	banner := ""
	if errMsg != "" {
		banner = `<p style="color:#b00">` + html.EscapeString(errMsg) + `</p>`
	}
	_, _ = io.WriteString(w, `<!doctype html><html lang="en"><head><meta charset="utf-8">`+
		`<meta name="viewport" content="width=device-width,initial-scale=1">`+
		`<title>Sign in to Piper</title>`+
		`<style>body{font:16px system-ui,sans-serif;max-width:24rem;margin:4rem auto;padding:0 1rem}`+
		`input{font:inherit;padding:.5rem;width:100%;box-sizing:border-box;letter-spacing:.1em;text-align:center}`+
		`button{font:inherit;padding:.5rem 1rem;margin-top:1rem}</style></head><body>`+
		`<h1>Sign in to Piper</h1>`+
		`<p>Enter the code shown in your terminal:</p>`+banner+
		`<form method="post" action="/v1/login/cli">`+
		`<input name="code" autofocus autocomplete="off" spellcheck="false" placeholder="XXXX-XXXX">`+
		`<button type="submit">Continue</button></form></body></html>`)
}

// cliLoginConfirm matches the entered code to a pending handle, binds the
// browser with a cookie, and redirects to the GitHub authorize URL. The code
// entry is what proves this browser belongs to the caller who started the flow.
func (a *api) cliLoginConfirm(w http.ResponseWriter, r *http.Request) {
	// Rate-limited like the other login endpoints: the code is short, and this
	// is the only place it can be guessed against pending handles.
	if !a.loginLimit.allow(clientIP(r)) {
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	if !a.cliLoginEnabled() {
		http.Error(w, "brokered login not configured", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	entered := normalizeCode(r.PostFormValue("code"))
	now := time.Now()
	var handle string
	a.mu.Lock()
	for h, s := range a.cliStates {
		if now.After(s.expires) {
			delete(a.cliStates, h)
			continue
		}
		if !s.confirmed && entered != "" &&
			subtle.ConstantTimeCompare([]byte(normalizeCode(s.userCode)), []byte(entered)) == 1 {
			s.confirmed = true
			handle = h
			break
		}
	}
	a.mu.Unlock()
	if handle == "" {
		a.renderCLILoginPage(w, "That code didn't match. Check your terminal and try again.")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: stateCookie, Value: handle, MaxAge: int(cliLoginTTL / time.Second), Path: "/v1/login",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, a.webv.AuthCodeURL(handle), http.StatusFound)
}

// cliCallback completes a CLI browser login if state names one, returning true
// when it owns the response. It mints the credential and, for a first-timer,
// bounces the same browser to the install page — installation linking itself
// stays on the webhook, so no unsigned installation_id is trusted here.
func (a *api) cliCallback(w http.ResponseWriter, r *http.Request) bool {
	state := r.URL.Query().Get("state")
	if state == "" {
		return false
	}
	a.mu.Lock()
	s, ok := a.cliStates[state]
	a.mu.Unlock()
	if !ok {
		return false // not a CLI handle — let the dashboard flow try
	}

	code := r.URL.Query().Get("code")
	c, err := r.Cookie(stateCookie)
	if !s.confirmed || code == "" || err != nil || c.Value != state || time.Now().After(s.expires) {
		http.Error(w, "bad state", http.StatusBadRequest)
		return true
	}
	id, err := a.webv.Exchange(r.Context(), code)
	if err != nil {
		log.Printf("relay: cli login code exchange failed: %v", err)
		http.Error(w, "code exchange failed", http.StatusBadGateway)
		return true
	}
	acc, err := a.st.UpsertAccount(id.Subject, id.Login)
	if err != nil {
		http.Error(w, "account error", http.StatusInternalServerError)
		return true
	}
	if denyDisabled(w, acc) {
		return true
	}
	cred, err := a.st.MintAccountCredential(acc.ID)
	if err != nil {
		http.Error(w, "credential error", http.StatusInternalServerError)
		return true
	}
	installURL := ""
	if insts, _ := a.st.InstallationsForAccount(acc.ID); len(insts) == 0 {
		installURL = a.ghApp.InstallURL()
	}

	a.mu.Lock()
	s.done, s.credential, s.username, s.installURL = true, cred, acc.Username, installURL
	a.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name: stateCookie, Value: "", MaxAge: -1, Path: "/v1/login",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	if installURL != "" {
		http.Redirect(w, r, installURL, http.StatusFound)
		return true
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, `<!doctype html><meta charset="utf-8"><title>Signed in</title>`+
		`<p style="font:16px system-ui,sans-serif;max-width:24rem;margin:4rem auto">`+
		`You're signed in to Piper. Return to your terminal.</p>`)
	return true
}

// cliLoginPoll is the CLI's collection endpoint: pending until the browser
// finishes, then the credential (and install URL, if the box still needs one)
// exactly once.
func (a *api) cliLoginPoll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Handle string `json:"handle"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Handle == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	s, ok := a.cliStates[req.Handle]
	if !ok {
		a.mu.Unlock()
		http.Error(w, "unknown or expired handle", http.StatusBadRequest)
		return
	}
	if s.done {
		delete(a.cliStates, req.Handle) // single use
		out := map[string]string{
			"account_credential": s.credential,
			"username":           s.username,
			"install_url":        s.installURL,
		}
		a.mu.Unlock()
		writeJSON(w, http.StatusOK, out)
		return
	}
	expired := time.Now().After(s.expires)
	if expired {
		delete(a.cliStates, req.Handle)
	}
	a.mu.Unlock()
	if expired {
		http.Error(w, "login expired", http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "authorization_pending"})
}
