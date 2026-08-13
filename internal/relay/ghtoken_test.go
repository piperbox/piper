package relay

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGitHubTokenForRejectsUnboundRepoWithNonNilApp proves the binding check
// — not the nil-App guard — is what rejects an unbound repo: the App here is
// non-nil, so ErrRepoNotBound can only come from AgentBoundToRepo.
func TestGitHubTokenForRejectsUnboundRepoWithNonNilApp(t *testing.T) {
	st := openTestStore(t)
	_, agent := enrolledAgent(t, st, "1001", "alice")

	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = st.GitHubTokenFor(context.Background(), app, agent, "alice/blog")
	if !errors.Is(err, ErrRepoNotBound) {
		t.Fatalf("err = %v, want ErrRepoNotBound", err)
	}
}

// TestGitHubTokenForReturnsTokenWhenBoundAndLinked is the happy path: a
// bound repo whose account holds a linked installation gets a real token
// through to the App.
func TestGitHubTokenForReturnsTokenWhenBoundAndLinked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations/55/access_tokens" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"ghs_ok","expires_at":"2026-07-20T12:00:00Z"}`))
	}))
	defer srv.Close()

	st := openTestStore(t)
	_, agent := enrolledAgent(t, st, "1001", "alice")
	if err := st.BindRepo(agent, "blog", "alice/blog", "main"); err != nil {
		t.Fatal(err)
	}
	if err := st.LinkInstallation("55", "1001", "User", "alice"); err != nil {
		t.Fatal(err)
	}

	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s", APIBase: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	tok, _, err := st.GitHubTokenFor(context.Background(), app, agent, "alice/blog")
	if err != nil {
		t.Fatalf("GitHubTokenFor: %v", err)
	}
	if tok != "ghs_ok" {
		t.Fatalf("token = %q, want ghs_ok", tok)
	}
}

// TestGitHubTokenForPicksInstallationByRepoOwner: an account holding a personal
// install (id 55) and a getpiper-org install (id 66) mints a token for
// getpiper/app from the org installation, not the most-recent one.
func TestGitHubTokenForPicksInstallationByRepoOwner(t *testing.T) {
	var hit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"ghs_ok","expires_at":"2026-07-20T12:00:00Z"}`))
	}))
	defer srv.Close()

	st := openTestStore(t)
	_, agent := enrolledAgent(t, st, "1001", "alice")
	// The owner-matching org install (66, "GetPiper") is linked FIRST, so the
	// personal install (55) is the most-recent — a most-recent-wins bug would
	// mint from 55, not 66. Mixed-case target_login also forces EqualFold.
	if err := st.LinkInstallation("66", "1001", "org", "GetPiper"); err != nil {
		t.Fatal(err)
	}
	if err := st.LinkInstallation("55", "1001", "user", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := st.BindRepo(agent, "app", "getpiper/app", "main"); err != nil {
		t.Fatal(err)
	}

	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s", APIBase: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := st.GitHubTokenFor(context.Background(), app, agent, "getpiper/app"); err != nil {
		t.Fatalf("GitHubTokenFor: %v", err)
	}
	if hit != "/app/installations/66/access_tokens" {
		t.Fatalf("minted from %q, want installation 66 (getpiper org)", hit)
	}
}

// TestGitHubTokenForDuplicateTargetLoginUsesNewestInstallation proves the
// precedence GitHubTokenFor inherits from InstallationsForAccount: duplicate
// target_login rows are allowed, and the newest installation must win.
func TestGitHubTokenForDuplicateTargetLoginUsesNewestInstallation(t *testing.T) {
	var hit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"ghs_ok","expires_at":"2026-07-20T12:00:00Z"}`))
	}))
	defer srv.Close()

	st := openTestStore(t)
	_, agent := enrolledAgent(t, st, "1001", "alice")
	if err := st.LinkInstallation("66", "1001", "user", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := st.LinkInstallation("55", "1001", "user", "alice"); err != nil {
		t.Fatal(err)
	}
	// Make the text timestamps sort opposite to insertion order. The rowid
	// order must still select the later-linked installation, 55.
	stampInstallation(t, st, "66", "2026-01-01T00:00:00.1Z")
	stampInstallation(t, st, "55", "2026-01-01T00:00:00.15Z")
	if err := st.BindRepo(agent, "blog", "alice/blog", "main"); err != nil {
		t.Fatal(err)
	}

	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s", APIBase: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := st.GitHubTokenFor(context.Background(), app, agent, "alice/blog"); err != nil {
		t.Fatalf("GitHubTokenFor: %v", err)
	}
	if hit != "/app/installations/55/access_tokens" {
		t.Fatalf("minted from %q, want newest installation 55", hit)
	}
}

// TestGitHubTokenForNoInstallationForRepoOwner: bound repo whose owner has no
// linked installation → ErrNoInstallation. The stub answers 404 to the
// miss-path installation re-fetch, so the test stays hermetic.
func TestGitHubTokenForNoInstallationForRepoOwner(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	st := openTestStore(t)
	_, agent := enrolledAgent(t, st, "1001", "alice")
	if err := st.LinkInstallation("55", "1001", "user", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := st.BindRepo(agent, "app", "getpiper/app", "main"); err != nil {
		t.Fatal(err)
	}
	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s", APIBase: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = st.GitHubTokenFor(context.Background(), app, agent, "getpiper/app")
	if !errors.Is(err, ErrNoInstallation) {
		t.Fatalf("err = %v, want ErrNoInstallation", err)
	}
}

// TestGitHubTokenForSelfHealsRenamedOrgTarget is issue #433: the stored
// target_login is the OLD org name (getpiper, written at link time), GitHub's
// live account.login is the NEW name (piperbox), and a mint for piperbox/app
// must succeed via the miss-path re-fetch AND persist the new login — a
// re-fetch-without-write version would pass the mint half but leave the row
// stranded.
func TestGitHubTokenForSelfHealsRenamedOrgTarget(t *testing.T) {
	var getHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/app/installations/66":
			getHits++
			_, _ = w.Write([]byte(`{"account":{"login":"piperbox","type":"Organization"}}`))
		case "/app/installations/66/access_tokens":
			_, _ = w.Write([]byte(`{"token":"ghs_healed","expires_at":"2026-07-30T12:00:00Z"}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	st := openTestStore(t)
	accID, agent := enrolledAgent(t, st, "1001", "alice")
	if err := st.LinkInstallation("66", "1001", "org", "getpiper"); err != nil {
		t.Fatal(err)
	}
	if err := st.BindRepo(agent, "app", "piperbox/app", "main"); err != nil {
		t.Fatal(err)
	}

	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s", APIBase: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	tok, _, err := st.GitHubTokenFor(context.Background(), app, agent, "piperbox/app")
	if err != nil {
		t.Fatalf("GitHubTokenFor: %v", err)
	}
	if tok != "ghs_healed" {
		t.Fatalf("token = %q, want ghs_healed", tok)
	}
	if getHits != 1 {
		t.Fatalf("installation re-fetches = %d, want exactly 1 (miss path only)", getHits)
	}
	insts, err := st.InstallationsForAccount(accID)
	if err != nil {
		t.Fatal(err)
	}
	if len(insts) != 1 || insts[0].TargetLogin != "piperbox" {
		t.Fatalf("stored installations = %+v, want target_login healed to piperbox", insts)
	}
	// Healing the login must not rewrite target_type. Storage and
	// /v1/github/status carry the normalized "org"/"user" that the webhook
	// path writes, not GitHub's raw account.type ("Organization") — which the
	// installation picker would render verbatim.
	if insts[0].TargetType != "org" {
		t.Fatalf("target_type = %q, want it left at %q by a login-only heal", insts[0].TargetType, "org")
	}
}

// TestGitHubTokenForHappyPathDoesNotRefetchInstallation pins the "one extra
// round-trip, only on misses" cost: when the stored target_login already
// matches the repo owner, the GitHub re-fetch (GET /app/installations/{id})
// must NOT happen — only the token mint may hit the API.
func TestGitHubTokenForHappyPathDoesNotRefetchInstallation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations/55/access_tokens" {
			t.Errorf("unexpected path %q (happy path must mint directly, no re-fetch)", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"ghs_ok","expires_at":"2026-07-20T12:00:00Z"}`))
	}))
	defer srv.Close()

	st := openTestStore(t)
	_, agent := enrolledAgent(t, st, "1001", "alice")
	if err := st.BindRepo(agent, "blog", "alice/blog", "main"); err != nil {
		t.Fatal(err)
	}
	if err := st.LinkInstallation("55", "1001", "user", "alice"); err != nil {
		t.Fatal(err)
	}

	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s", APIBase: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	tok, _, err := st.GitHubTokenFor(context.Background(), app, agent, "alice/blog")
	if err != nil {
		t.Fatalf("GitHubTokenFor: %v", err)
	}
	if tok != "ghs_ok" {
		t.Fatalf("token = %q, want ghs_ok", tok)
	}
}
