package relay

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
)

// fakeGitHub fakes github.com (device code + token) and api.github.com (/user)
// on one httptest server. Poll responses are scripted via tokenResponses.
type fakeGitHub struct {
	t *testing.T

	mu             sync.Mutex
	tokenResponses []map[string]any // popped one per access_token poll
	tokenForms     []map[string]string
	userCalls      int
}

func (f *fakeGitHub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/device/code", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.FormValue("client_id") != "test-client" {
			f.t.Errorf("device/code client_id = %q", r.FormValue("client_id"))
		}
		if r.FormValue("scope") != "" {
			f.t.Errorf("device/code sent scope %q, want none", r.FormValue("scope"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code": "dc-1", "user_code": "ABCD-1234",
			"verification_uri": "https://github.test/login/device",
			"expires_in":       900, "interval": 5,
		})
	})
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form := map[string]string{}
		for k := range r.Form {
			form[k] = r.FormValue(k)
		}
		f.mu.Lock()
		f.tokenForms = append(f.tokenForms, form)
		var resp map[string]any
		if len(f.tokenResponses) > 0 {
			resp = f.tokenResponses[0]
			f.tokenResponses = f.tokenResponses[1:]
		} else {
			resp = map[string]any{"error": "authorization_pending"}
		}
		f.mu.Unlock()
		// GitHub returns poll errors in 200-OK bodies.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer gho_tok" {
			f.t.Errorf("/user Authorization = %q", got)
		}
		f.mu.Lock()
		f.userCalls++
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 583231, "login": "Octo-Cat"})
	})
	return mux
}

// newTestGitHubVerifier points a GitHubVerifier at the fake, over st.
func newTestGitHubVerifier(t *testing.T, fake *fakeGitHub, st *Store) *GitHubVerifier {
	t.Helper()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	v := NewGitHubVerifier("test-client", "test-secret", st)
	v.oauthBase = srv.URL
	v.apiBase = srv.URL
	return v
}

func (f *fakeGitHub) tokenPolls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tokenForms)
}

// A device flow resolves through the polls the CLI already makes: nothing
// runs in the background, GitHub is asked only once the interval has passed,
// and the handle redeems exactly once (#522).
func TestGitHubDeviceFlowApproved(t *testing.T) {
	st := openTestStore(t)
	fake := &fakeGitHub{t: t, tokenResponses: []map[string]any{
		{"error": "authorization_pending"},
		{"access_token": "gho_tok", "token_type": "bearer"},
	}}
	v := newTestGitHubVerifier(t, fake, st)

	handle, da, err := v.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if da.UserCode != "ABCD-1234" || da.VerificationURI != "https://github.test/login/device" ||
		da.Interval != 5 || da.ExpiresIn != 900 {
		t.Fatalf("DeviceAuth = %+v", da)
	}

	// Before the interval has passed a poll is pending and costs no upstream call.
	if _, err := v.Poll(context.Background(), handle); !errors.Is(err, ErrAuthPending) {
		t.Fatalf("early Poll err = %v, want pending", err)
	}
	if n := fake.tokenPolls(); n != 0 {
		t.Fatalf("early poll made %d upstream calls, want 0", n)
	}

	makeDue(t, st, handle)
	if _, err := v.Poll(context.Background(), handle); !errors.Is(err, ErrAuthPending) {
		t.Fatalf("Poll (github pending) err = %v, want pending", err)
	}
	// That poll spent the slot: the next one waits again.
	if _, err := v.Poll(context.Background(), handle); !errors.Is(err, ErrAuthPending) || fake.tokenPolls() != 1 {
		t.Fatalf("Poll right after upstream = %v with %d calls, want pending and 1", err, fake.tokenPolls())
	}

	makeDue(t, st, handle)
	id, err := v.Poll(context.Background(), handle)
	if err != nil {
		t.Fatalf("Poll (approved): %v", err)
	}
	if id.Subject != "583231" || id.Login != "Octo-Cat" {
		t.Fatalf("identity = %+v", id)
	}
	fake.mu.Lock()
	form := fake.tokenForms[0]
	fake.mu.Unlock()
	if form["grant_type"] != "urn:ietf:params:oauth:grant-type:device_code" || form["device_code"] != "dc-1" {
		t.Fatalf("token form = %+v", form)
	}
	// Redeemed once: the row is gone.
	if _, err := v.Poll(context.Background(), handle); err == nil || errors.Is(err, ErrAuthPending) {
		t.Fatalf("Poll(redeemed handle) = %v, want unknown-handle error", err)
	}
}

func TestGitHubDeviceFlowSlowDownDefersByInterval(t *testing.T) {
	st := openTestStore(t)
	fake := &fakeGitHub{t: t, tokenResponses: []map[string]any{
		{"error": "slow_down"},
		{"access_token": "gho_tok", "token_type": "bearer"},
	}}
	v := newTestGitHubVerifier(t, fake, st)
	handle, _, err := v.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	makeDue(t, st, handle)
	if _, err := v.Poll(context.Background(), handle); !errors.Is(err, ErrAuthPending) {
		t.Fatalf("Poll (slow_down) err = %v, want pending", err)
	}
	// GitHub semantics: wait interval + 5s. The fake's interval is 5.
	var secs float64
	if err := st.db.QueryRow(`SELECT EXTRACT(EPOCH FROM next_poll_at - now()) FROM login_device_flows WHERE handle = $1`, handle).Scan(&secs); err != nil {
		t.Fatal(err)
	}
	if secs < 9 || secs > 10 {
		t.Fatalf("next poll in %.1fs, want ~10s after slow_down", secs)
	}
	makeDue(t, st, handle)
	if _, err := v.Poll(context.Background(), handle); err != nil {
		t.Fatalf("Poll (approved): %v", err)
	}
}

func TestGitHubDeviceFlowDenied(t *testing.T) {
	st := openTestStore(t)
	fake := &fakeGitHub{t: t, tokenResponses: []map[string]any{{"error": "access_denied"}}}
	v := newTestGitHubVerifier(t, fake, st)
	handle, _, err := v.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	makeDue(t, st, handle)
	if _, err := v.Poll(context.Background(), handle); err == nil || errors.Is(err, ErrAuthPending) {
		t.Fatalf("denied flow err = %v, want terminal error", err)
	}
	// Terminal: the row is gone, a retry is unknown.
	if _, err := v.Poll(context.Background(), handle); err == nil || errors.Is(err, ErrAuthPending) {
		t.Fatalf("Poll after denial = %v, want unknown-handle error", err)
	}
}

// Most device flows are never polled to completion: the user closes the tab,
// the CLI is Ctrl-C'd, the code expires unapproved. Expired rows are
// invisible, and the next Start sweeps them (#81).
func TestGitHubDeviceFlowSweepsExpired(t *testing.T) {
	st := openTestStore(t)
	v := newTestGitHubVerifier(t, &fakeGitHub{t: t}, st)
	var abandoned []string
	for i := 0; i < 3; i++ {
		h, _, err := v.Start(context.Background())
		if err != nil {
			t.Fatalf("Start %d: %v", i, err)
		}
		abandoned = append(abandoned, h)
	}
	if _, err := st.db.Exec(`UPDATE login_device_flows SET expires_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	for i, h := range abandoned {
		if _, err := v.Poll(context.Background(), h); err == nil || errors.Is(err, ErrAuthPending) {
			t.Errorf("Poll(expired handle %d) = %v, want unknown-handle error", i, err)
		}
	}
	if _, _, err := v.Start(context.Background()); err != nil {
		t.Fatalf("Start (post-expiry): %v", err)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM login_device_flows`); n != 1 {
		t.Errorf("flows after sweep = %d, want 1 (only the live flow)", n)
	}
}

// Start on one relay, poll on another: the flow lives in the store, not the
// process that started it.
func TestGitHubDeviceFlowSpansTwoRelays(t *testing.T) {
	st := openTestStore(t)
	fake := &fakeGitHub{t: t, tokenResponses: []map[string]any{
		{"access_token": "gho_tok", "token_type": "bearer"},
	}}
	relayA := newTestGitHubVerifier(t, fake, st)
	relayB := newTestGitHubVerifier(t, fake, st)
	handle, _, err := relayA.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := relayB.Poll(context.Background(), handle); !errors.Is(err, ErrAuthPending) {
		t.Fatalf("early Poll on B = %v, want pending", err)
	}
	makeDue(t, st, handle)
	id, err := relayB.Poll(context.Background(), handle)
	if err != nil || id.Login != "Octo-Cat" {
		t.Fatalf("Poll on B = (%+v, %v)", id, err)
	}
}

func TestGitHubVerifierPollUnknownHandle(t *testing.T) {
	v := NewGitHubVerifier("test-client", "test-secret", openTestStore(t))
	if _, err := v.Poll(context.Background(), "never-started"); err == nil {
		t.Fatal("Poll(unknown) succeeded, want error")
	}
}

func TestGitHubAuthCodeURL(t *testing.T) {
	v := NewGitHubVerifier("test-client", "test-secret", nil)
	got := v.AuthCodeURL("state-123")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("AuthCodeURL not a URL: %v", err)
	}
	if u.Host != "github.com" || u.Path != "/login/oauth/authorize" {
		t.Fatalf("authorize URL = %q", got)
	}
	q := u.Query()
	if q.Get("client_id") != "test-client" || q.Get("state") != "state-123" {
		t.Fatalf("authorize query = %q", u.RawQuery)
	}
	if q.Get("scope") != "" {
		t.Fatalf("authorize URL carries scope %q, want none", q.Get("scope"))
	}
	if q.Get("redirect_uri") != "" {
		t.Fatalf("authorize URL carries redirect_uri %q, want none (single registered callback)", q.Get("redirect_uri"))
	}
	if q.Get("prompt") != "select_account" {
		t.Fatalf("authorize URL prompt = %q, want select_account (multi-account 404, #320)", q.Get("prompt"))
	}
}

func TestGitHubExchange(t *testing.T) {
	fake := &fakeGitHub{t: t, tokenResponses: []map[string]any{
		{"access_token": "gho_tok", "token_type": "bearer"},
	}}
	v := newTestGitHubVerifier(t, fake, nil)

	id, err := v.Exchange(context.Background(), "code-1")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if id.Subject != "583231" || id.Login != "Octo-Cat" {
		t.Fatalf("identity = %+v", id)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	f := fake.tokenForms[0]
	if f["client_id"] != "test-client" || f["client_secret"] != "test-secret" || f["code"] != "code-1" {
		t.Fatalf("exchange form = %+v", f)
	}
}

func TestGitHubExchangeBadCode(t *testing.T) {
	fake := &fakeGitHub{t: t, tokenResponses: []map[string]any{
		{"error": "bad_verification_code"},
	}}
	v := newTestGitHubVerifier(t, fake, nil)
	if _, err := v.Exchange(context.Background(), "nope"); err == nil {
		t.Fatal("Exchange(bad code) succeeded, want error")
	}
}
