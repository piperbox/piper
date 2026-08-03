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
	"time"
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

// newTestGitHubVerifier points a GitHubVerifier at the fake and makes sleeps
// instant, recording requested durations.
func newTestGitHubVerifier(t *testing.T, fake *fakeGitHub) (*GitHubVerifier, *[]time.Duration) {
	t.Helper()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	v := NewGitHubVerifier("test-client", "test-secret")
	v.oauthBase = srv.URL
	v.apiBase = srv.URL
	var slept []time.Duration
	var mu sync.Mutex
	v.sleep = func(d time.Duration) { mu.Lock(); slept = append(slept, d); mu.Unlock() }
	return v, &slept
}

// waitDone polls the verifier until the flow completes or times out.
func waitDone(t *testing.T, v *GitHubVerifier, handle string) (Identity, error) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		id, err := v.Poll(context.Background(), handle)
		if err != ErrAuthPending {
			return id, err
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("flow never completed")
	return Identity{}, nil
}

func TestGitHubDeviceFlowApproved(t *testing.T) {
	fake := &fakeGitHub{t: t, tokenResponses: []map[string]any{
		{"error": "authorization_pending"},
		{"access_token": "gho_tok", "token_type": "bearer"},
	}}
	v, _ := newTestGitHubVerifier(t, fake)

	handle, da, err := v.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if da.UserCode != "ABCD-1234" || da.VerificationURI != "https://github.test/login/device" ||
		da.Interval != 5 || da.ExpiresIn != 900 {
		t.Fatalf("DeviceAuth = %+v", da)
	}

	id, err := waitDone(t, v, handle)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if id.Subject != "583231" || id.Login != "Octo-Cat" {
		t.Fatalf("identity = %+v", id)
	}
	// The poll used the device grant.
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.tokenForms) == 0 ||
		fake.tokenForms[0]["grant_type"] != "urn:ietf:params:oauth:grant-type:device_code" ||
		fake.tokenForms[0]["device_code"] != "dc-1" {
		t.Fatalf("token forms = %+v", fake.tokenForms)
	}
}

func TestGitHubDeviceFlowSlowDown(t *testing.T) {
	fake := &fakeGitHub{t: t, tokenResponses: []map[string]any{
		{"error": "slow_down"},
		{"access_token": "gho_tok", "token_type": "bearer"},
	}}
	v, slept := newTestGitHubVerifier(t, fake)

	handle, _, err := v.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := waitDone(t, v, handle); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	// First sleep at the server interval (5s), then slow_down adds 5s (GitHub semantics).
	if len(*slept) < 2 || (*slept)[0] != 5*time.Second || (*slept)[1] != 10*time.Second {
		t.Fatalf("sleeps = %v, want [5s 10s ...]", *slept)
	}
}

func TestGitHubDeviceFlowDenied(t *testing.T) {
	fake := &fakeGitHub{t: t, tokenResponses: []map[string]any{
		{"error": "access_denied"},
	}}
	v, _ := newTestGitHubVerifier(t, fake)

	handle, _, err := v.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := waitDone(t, v, handle); err == nil || err == ErrAuthPending {
		t.Fatalf("denied flow err = %v, want terminal error", err)
	}
}

func TestGitHubDeviceFlowPollPrunesCompletedFlow(t *testing.T) {
	fake := &fakeGitHub{t: t, tokenResponses: []map[string]any{
		{"access_token": "gho_tok", "token_type": "bearer"},
	}}
	v, _ := newTestGitHubVerifier(t, fake)

	handle, _, err := v.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := waitDone(t, v, handle); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	// The flow completed and was already returned once; a second poll of the
	// same handle must not still redeem the cached identity.
	if _, err := v.Poll(context.Background(), handle); err == nil {
		t.Fatal("Poll(completed handle) succeeded a second time, want unknown-handle error")
	}
}

// Most device flows are never polled to completion: the user closes the tab,
// the CLI is Ctrl-C'd, the code expires unapproved. Those entries used to sit in
// the flows map for the life of the process, so a long-running relay's memory
// grew with every login ever started. Start sweeps what has expired (#81).
func TestGitHubDeviceFlowPrunesAbandonedFlows(t *testing.T) {
	// Never approved: every poll answers authorization_pending until the device
	// code's own lifetime runs out.
	v, _ := newTestGitHubVerifier(t, &fakeGitHub{t: t})

	// A fake clock the poll loop drives itself: each slept interval advances it,
	// so the fake's 900s expires_in is reached in a few hundred instant polls
	// rather than fifteen real minutes.
	var clockMu sync.Mutex
	now := time.Now()
	v.now = func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}
	v.sleep = func(d time.Duration) {
		clockMu.Lock()
		now = now.Add(d)
		clockMu.Unlock()
	}
	advance := func(d time.Duration) {
		clockMu.Lock()
		now = now.Add(d)
		clockMu.Unlock()
	}

	var abandoned []string
	for i := 0; i < 3; i++ {
		h, _, err := v.Start(context.Background())
		if err != nil {
			t.Fatalf("Start %d: %v", i, err)
		}
		abandoned = append(abandoned, h)
	}

	// Let the three background pollers run their codes out.
	deadline := time.Now().Add(5 * time.Second)
	for {
		v.mu.Lock()
		allDone := true
		for _, fl := range v.flows {
			if !fl.done {
				allDone = false
			}
		}
		v.mu.Unlock()
		if allDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("device codes never expired")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The codes have run out, but each entry is still resolvable through its
	// grace window — a poll landing right on the deadline deserves the real
	// "device code expired" error, not a bare unknown-handle.
	if _, err := v.Poll(context.Background(), abandoned[0]); err == nil || errors.Is(err, ErrAuthPending) {
		t.Errorf("Poll within grace = %v, want the flow's terminal error", err)
	}
	advance(flowGrace + time.Second)

	// A fresh login is the sweep trigger: the dead flows must not outlive it.
	if _, _, err := v.Start(context.Background()); err != nil {
		t.Fatalf("Start (post-expiry): %v", err)
	}
	v.mu.Lock()
	n := len(v.flows)
	v.mu.Unlock()
	if n != 1 {
		t.Errorf("flows after sweep = %d, want 1 (only the live flow)", n)
	}
	for i, h := range abandoned {
		if _, err := v.Poll(context.Background(), h); err == nil || errors.Is(err, ErrAuthPending) {
			t.Errorf("Poll(abandoned handle %d) = %v, want unknown-handle error", i, err)
		}
	}
}

func TestGitHubVerifierPollUnknownHandle(t *testing.T) {
	v := NewGitHubVerifier("test-client", "test-secret")
	if _, err := v.Poll(context.Background(), "never-started"); err == nil {
		t.Fatal("Poll(unknown) succeeded, want error")
	}
}

func TestGitHubAuthCodeURL(t *testing.T) {
	v := NewGitHubVerifier("test-client", "test-secret")
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
	v, _ := newTestGitHubVerifier(t, fake)

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
	v, _ := newTestGitHubVerifier(t, fake)
	if _, err := v.Exchange(context.Background(), "nope"); err == nil {
		t.Fatal("Exchange(bad code) succeeded, want error")
	}
}
