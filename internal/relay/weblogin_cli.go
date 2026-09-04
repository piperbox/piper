package relay

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

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
	handle, code := randToken(16), userCode()
	if err := a.st.PutCLIHandle(handle, code, cliLoginTTL); err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
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
	handle, ok, err := a.st.ConfirmCLIHandle(r.PostFormValue("code"))
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	if !ok {
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
	h, ok, err := a.st.CLIHandle(state)
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return true
	}
	if !ok {
		return false // not a CLI handle — let the dashboard flow try
	}

	code := r.URL.Query().Get("code")
	c, err := r.Cookie(stateCookie)
	if !h.Confirmed || code == "" || err != nil || c.Value != state {
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
	// Record who logged in; the credential is minted by the poll that
	// collects it, so no secret sits in the handle row (#522).
	if err := a.st.FinishCLIHandle(state, acc.ID); err != nil {
		if errors.Is(err, errCLIHandleGone) {
			http.Error(w, "bad state", http.StatusBadRequest)
		} else {
			http.Error(w, "store error", http.StatusInternalServerError)
		}
		return true
	}
	installURL := ""
	if insts, _ := a.st.InstallationsForAccount(acc.ID); len(insts) == 0 {
		installURL = a.ghApp.InstallURL()
	}

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
	accountID, username, state, err := a.st.TakeFinishedCLIHandle(req.Handle)
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	switch state {
	case cliHandleUnknown:
		http.Error(w, "unknown or expired handle", http.StatusBadRequest)
		return
	case cliHandlePending:
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "authorization_pending"})
		return
	}
	// Done: this relay mints. The row is already gone, so a retry after a
	// mint failure restarts the flow — the same thing a 500 here always meant.
	cred, err := a.st.MintAccountCredential(accountID)
	if err != nil {
		http.Error(w, "credential error", http.StatusInternalServerError)
		return
	}
	installURL := ""
	if insts, _ := a.st.InstallationsForAccount(accountID); len(insts) == 0 && a.ghApp != nil {
		installURL = a.ghApp.InstallURL()
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"account_credential": cred,
		"username":           username,
		"install_url":        installURL,
	})
}
