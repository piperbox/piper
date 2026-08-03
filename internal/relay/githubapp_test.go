package relay

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func relayTestKeyPEM(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(k),
	}))
}

func TestVerifySignature(t *testing.T) {
	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s3cret",
	})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"hello":"world"}`)
	m := hmac.New(sha256.New, []byte("s3cret"))
	m.Write(body)
	good := "sha256=" + hex.EncodeToString(m.Sum(nil))

	if !app.VerifySignature(good, body) {
		t.Fatal("valid signature rejected")
	}
	if app.VerifySignature("sha256=deadbeef", body) {
		t.Fatal("bad signature accepted")
	}
	if app.VerifySignature("", body) {
		t.Fatal("empty signature accepted")
	}
}

func TestNewGitHubAppRequiresWebhookSecret(t *testing.T) {
	if _, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t),
	}); err == nil {
		t.Fatal("expected error when WebhookSecret is empty")
	}

	if _, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s3cret",
	}); err != nil {
		t.Fatalf("unexpected error with a webhook secret set: %v", err)
	}
}

func TestRepoTokenIsScopedToOneRepo(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"ghs_scoped","expires_at":"2026-07-20T12:00:00Z"}`))
	}))
	defer srv.Close()

	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s", APIBase: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	tok, exp, err := app.RepoToken(context.Background(), "55", "Alice/Blog")
	if err != nil {
		t.Fatalf("RepoToken: %v", err)
	}
	if tok != "ghs_scoped" {
		t.Fatalf("token = %q", tok)
	}
	if exp.IsZero() {
		t.Fatal("expiry not parsed")
	}
	if gotPath != "/app/installations/55/access_tokens" {
		t.Fatalf("path = %q", gotPath)
	}
	repos, _ := gotBody["repositories"].([]any)
	if len(repos) != 1 || repos[0] != "Blog" {
		t.Fatalf("repositories = %v, want [Blog]", gotBody["repositories"])
	}
	perms, _ := gotBody["permissions"].(map[string]any)
	if perms["contents"] != "read" || perms["deployments"] != "write" {
		t.Fatalf("permissions = %v", perms)
	}
}

// The App's own installation listing is what reconciliation reads when an
// account has nothing on record and no webhook is ever going to arrive (#470).
func TestInstallationsListsEveryAppInstallation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("per_page"); got != "100" {
			t.Errorf("per_page = %q, want 100", got)
		}
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Errorf("Authorization = %q, want an app JWT", got)
		}
		_, _ = w.Write([]byte(`[` +
			`{"id":55,"account":{"id":4242,"login":"alice","type":"User"}},` +
			`{"id":56,"account":{"id":9001,"login":"acme","type":"Organization"}}]`))
	}))
	defer srv.Close()

	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s", APIBase: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := app.Installations(context.Background())
	if err != nil {
		t.Fatalf("Installations: %v", err)
	}
	want := []AppInstallation{
		{ID: "55", AccountGithubID: "4242", AccountLogin: "alice", AccountType: "User"},
		{ID: "56", AccountGithubID: "9001", AccountLogin: "acme", AccountType: "Organization"},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("installations = %+v, want %+v", got, want)
	}
}

// This is the App-global list — every tenant's installation, not one account's
// — so it outgrows a single page long before any individual does. Stopping at
// page one would mean accounts past the first hundred could never reconcile,
// with no symptom except the original bug quietly persisting for them (#470).
func TestInstallationsFollowsPagination(t *testing.T) {
	var srv *httptest.Server
	pages := 0
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Errorf("page %s dropped the app JWT: Authorization = %q", r.URL.Query().Get("page"), got)
		}
		pages++
		switch r.URL.Query().Get("page") {
		case "", "1":
			// rel="next" alongside rel="last", in the order GitHub sends them.
			w.Header().Set("Link", `<`+srv.URL+`/app/installations?per_page=100&page=2>; rel="next", `+
				`<`+srv.URL+`/app/installations?per_page=100&page=2>; rel="last"`)
			_, _ = w.Write([]byte(`[{"id":55,"account":{"id":4242,"login":"alice","type":"User"}}]`))
		case "2":
			// Last page: rel="prev"/"first" only, no next — the stop condition.
			w.Header().Set("Link", `<`+srv.URL+`/app/installations?per_page=100&page=1>; rel="prev", `+
				`<`+srv.URL+`/app/installations?per_page=100&page=1>; rel="first"`)
			_, _ = w.Write([]byte(`[{"id":56,"account":{"id":9001,"login":"acme","type":"Organization"}}]`))
		default:
			t.Errorf("walked past the last page: page=%q", r.URL.Query().Get("page"))
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()

	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s", APIBase: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := app.Installations(context.Background())
	if err != nil {
		t.Fatalf("Installations: %v", err)
	}
	want := []AppInstallation{
		{ID: "55", AccountGithubID: "4242", AccountLogin: "alice", AccountType: "User"},
		{ID: "56", AccountGithubID: "9001", AccountLogin: "acme", AccountType: "Organization"},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("installations = %+v, want both pages %+v", got, want)
	}
	if pages != 2 {
		t.Errorf("fetched %d page(s), want exactly 2", pages)
	}
}

// Every page request carries the App JWT and the next-page URL comes from a
// response header, so a Link pointing off-host must end the walk rather than
// hand that credential to whoever set the header.
func TestInstallationsIgnoresAnOffHostNextLink(t *testing.T) {
	leaked := make(chan string, 1)
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case leaked <- r.Header.Get("Authorization"):
		default:
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer attacker.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `<`+attacker.URL+`/app/installations?page=2>; rel="next"`)
		_, _ = w.Write([]byte(`[{"id":55,"account":{"id":4242,"login":"alice","type":"User"}}]`))
	}))
	defer srv.Close()

	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s", APIBase: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := app.Installations(context.Background())
	if err != nil {
		t.Fatalf("Installations: %v", err)
	}
	if len(got) != 1 || got[0].ID != "55" {
		t.Fatalf("installations = %+v, want just the on-host page", got)
	}
	select {
	case auth := <-leaked:
		t.Fatalf("followed an off-host Link and sent it %q", auth)
	default:
	}
}

// A cancelled context must stop the walk rather than paging on.
func TestInstallationsStopsOnCancelledContext(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Always another page: only cancellation ends this.
		w.Header().Set("Link", `<`+srv.URL+`/app/installations?page=99>; rel="next"`)
		_, _ = w.Write([]byte(`[{"id":1,"account":{"id":1,"login":"a","type":"User"}}]`))
	}))
	defer srv.Close()

	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s", APIBase: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := app.Installations(ctx); err == nil {
		t.Fatal("Installations with a cancelled context returned nil error")
	}
}

func TestReposListsInstallationRepositories(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations/55/access_tokens" {
			_, _ = w.Write([]byte(`{"token":"t","expires_at":"2026-07-20T12:00:00Z"}`))
			return
		}
		if r.URL.Path != "/installation/repositories" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("per_page"); got != "100" {
			t.Errorf("per_page = %q, want 100", got)
		}
		_, _ = w.Write([]byte(`{"repositories":[` +
			`{"full_name":"alice/blog","visibility":"public","pushed_at":"2026-07-20T12:34:56Z"},` +
			`{"full_name":"alice/api","visibility":"private","pushed_at":""}]}`))
	}))
	defer srv.Close()

	app, err := NewGitHubApp(GitHubAppConfig{
		AppID: "1", PrivateKeyPEM: relayTestKeyPEM(t), WebhookSecret: "s", APIBase: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	repos, err := app.Repos(context.Background(), "55")
	if err != nil {
		t.Fatalf("Repos: %v", err)
	}
	want := []Repo{
		{FullName: "alice/blog", Visibility: "public", PushedAt: "2026-07-20T12:34:56Z"},
		{FullName: "alice/api", Visibility: "private", PushedAt: ""},
	}
	if len(repos) != len(want) || repos[0] != want[0] || repos[1] != want[1] {
		t.Fatalf("repos = %+v, want %+v", repos, want)
	}
}
