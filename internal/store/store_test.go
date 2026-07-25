package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCreateAndGetApp(t *testing.T) {
	s := openTemp(t)
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	got, err := s.GetApp("blog")
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if got.Name != "blog" || got.Port != 8080 {
		t.Errorf("got %+v", got)
	}
}

func TestUpdateAppRepoAndAppByRepo(t *testing.T) {
	s := openTemp(t)
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateAppRepo("blog", "alice/blog", "main", "apps/web"); err != nil {
		t.Fatalf("UpdateAppRepo: %v", err)
	}

	got, err := s.AppByRepo("alice/blog")
	if err != nil {
		t.Fatalf("AppByRepo: %v", err)
	}
	if got.Name != "blog" || got.Repo != "alice/blog" || got.Branch != "main" || got.RootDir != "apps/web" {
		t.Fatalf("got %+v", got)
	}

	if _, err := s.AppByRepo("nobody/none"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestGitHubAppRoundTrip(t *testing.T) {
	s := openTemp(t)

	if _, err := s.GetGitHubApp(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	want := GitHubApp{AppID: 42, Slug: "piper-abc", PrivateKey: "-----KEY-----", WebhookSecret: "shh"}
	if err := s.SaveGitHubApp(want); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetGitHubApp()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
	// Upsert replaces, not duplicates.
	want.WebhookSecret = "newsecret"
	if err := s.SaveGitHubApp(want); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetGitHubApp()
	if got.WebhookSecret != "newsecret" {
		t.Fatalf("upsert failed: %+v", got)
	}
}

// TestDeleteGitHubApp covers giving up BYO: the row must go so the box stops
// claiming an explicit override and can fall back to a relay's brokered App
// (#299). Deleting when there is nothing stored is not an error — the caller
// wants "no BYO App", which is already true.
func TestDeleteGitHubApp(t *testing.T) {
	s := openTemp(t)

	if err := s.DeleteGitHubApp(); err != nil {
		t.Fatalf("delete with no row: %v", err)
	}
	if err := s.SaveGitHubApp(GitHubApp{AppID: 42, PrivateKey: "-----KEY-----", WebhookSecret: "shh"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteGitHubApp(); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetGitHubApp(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: want ErrNotFound, got %v", err)
	}
}

func TestSetAppHostname(t *testing.T) {
	s := openTemp(t)
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	// Fresh apps carry no hostname until first deploy.
	got, err := s.GetApp("blog")
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if got.Hostname != "" {
		t.Fatalf("new app hostname = %q, want empty", got.Hostname)
	}

	if err := s.SetAppHostname("blog", "hash-blog-alice.public.getpiper.co"); err != nil {
		t.Fatalf("SetAppHostname: %v", err)
	}
	got, err = s.GetApp("blog")
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if got.Hostname != "hash-blog-alice.public.getpiper.co" {
		t.Fatalf("hostname = %q", got.Hostname)
	}
	// ListApps surfaces it too.
	apps, err := s.ListApps()
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	if len(apps) != 1 || apps[0].Hostname != "hash-blog-alice.public.getpiper.co" {
		t.Fatalf("ListApps hostname = %+v", apps)
	}

	if err := s.SetAppHostname("nope", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetAppHostname unknown app err = %v, want ErrNotFound", err)
	}
}

func TestGetAppNotFound(t *testing.T) {
	s := openTemp(t)
	if _, err := s.GetApp("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestCreateAppDuplicate(t *testing.T) {
	s := openTemp(t)
	s.CreateApp("blog", 8080)
	if _, err := s.CreateApp("blog", 8080); err == nil {
		t.Error("expected error on duplicate app")
	}
}

func TestListApps(t *testing.T) {
	s := openTemp(t)
	s.CreateApp("blog", 8080)
	s.CreateApp("api", 3000)
	apps, err := s.ListApps()
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	if len(apps) != 2 || apps[0].Name != "api" || apps[1].Name != "blog" {
		t.Errorf("apps = %+v, want [api blog] ordered", apps)
	}
}

func TestLatestRunning(t *testing.T) {
	s := openTemp(t)
	s.CreateApp("blog", 8080)
	if _, err := s.LatestRunning("blog"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty LatestRunning err = %v, want ErrNotFound", err)
	}
	d1, _ := s.CreateDeployment("blog", "img1", "c1", 40001, "running", "")
	s.CreateDeployment("blog", "img2", "c2", 40002, "failed", "")
	got, err := s.LatestRunning("blog")
	if err != nil {
		t.Fatalf("LatestRunning: %v", err)
	}
	if got.ID != d1.ID {
		t.Errorf("LatestRunning ID = %s, want %s", got.ID, d1.ID)
	}
}

func TestUpdateDeploymentStatus(t *testing.T) {
	s := openTemp(t)
	s.CreateApp("blog", 8080)
	d, _ := s.CreateDeployment("blog", "img1", "c1", 40001, "running", "")
	if err := s.UpdateDeploymentStatus(d.ID, "stopped"); err != nil {
		t.Fatalf("UpdateDeploymentStatus: %v", err)
	}
	if _, err := s.LatestRunning("blog"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after status change to stopped, LatestRunning err = %v, want ErrNotFound", err)
	}
}

func TestPreviewDeploymentRoundTrip(t *testing.T) {
	s := openTemp(t)
	if _, err := s.CreatePreviewDeployment("blog", 7, "img", "cid", 41000, "running", ""); err != nil {
		t.Fatalf("CreatePreviewDeployment: %v", err)
	}
	got, err := s.PreviewRunning("blog", 7)
	if err != nil {
		t.Fatalf("PreviewRunning: %v", err)
	}
	if got.PR != 7 || got.ContainerID != "cid" || got.HostPort != 41000 {
		t.Errorf("got %+v", got)
	}
	if _, err := s.PreviewRunning("blog", 8); !errors.Is(err, ErrNotFound) {
		t.Errorf("PreviewRunning(missing) err = %v, want ErrNotFound", err)
	}
}

func TestLatestRunningIgnoresPreviews(t *testing.T) {
	s := openTemp(t)
	if _, err := s.CreateDeployment("blog", "img", "main-c", 40000, "running", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePreviewDeployment("blog", 3, "img", "preview-c", 41000, "running", ""); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestRunning("blog")
	if err != nil {
		t.Fatalf("LatestRunning: %v", err)
	}
	if got.ContainerID != "main-c" {
		t.Errorf("LatestRunning returned %q, want main-c", got.ContainerID)
	}
}

// RunningPreviews is what lets a restart re-arm preview routes and a reconnect
// re-announce preview hostnames: LatestRunning is pr=0 only, so previews were
// invisible to both (#376). It must return every live preview across all apps
// and nothing else — no production rows, no superseded previews.
func TestRunningPreviews(t *testing.T) {
	s := openTemp(t)
	if _, err := s.CreateDeployment("blog", "img", "main-c", 40000, "running", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePreviewDeployment("blog", 3, "img", "pr3-c", 41000, "running", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePreviewDeployment("blog", 4, "img", "pr4-old", 41001, "stopped", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePreviewDeployment("shop", 9, "img", "pr9-c", 42000, "running", ""); err != nil {
		t.Fatal(err)
	}

	got, err := s.RunningPreviews()
	if err != nil {
		t.Fatalf("RunningPreviews: %v", err)
	}
	byContainer := map[string]Deployment{}
	for _, d := range got {
		byContainer[d.ContainerID] = d
	}
	if len(got) != 2 || byContainer["pr3-c"].PR != 3 || byContainer["pr9-c"].PR != 9 {
		t.Fatalf("RunningPreviews = %+v, want the two running previews", got)
	}
	if byContainer["pr3-c"].App != "blog" || byContainer["pr3-c"].HostPort != 41000 {
		t.Errorf("preview row = %+v", byContainer["pr3-c"])
	}
}

// A second deploy of the same PR supersedes the first: only the newest running
// row may come back, or a restart re-arms the route to a dead container.
func TestRunningPreviewsReturnsOnlyTheNewestPerPR(t *testing.T) {
	s := openTemp(t)
	if _, err := s.CreatePreviewDeployment("blog", 3, "img", "pr3-old", 41000, "running", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePreviewDeployment("blog", 3, "img", "pr3-new", 41002, "running", ""); err != nil {
		t.Fatal(err)
	}

	got, err := s.RunningPreviews()
	if err != nil {
		t.Fatalf("RunningPreviews: %v", err)
	}
	if len(got) != 1 || got[0].ContainerID != "pr3-new" {
		t.Fatalf("RunningPreviews = %+v, want only pr3-new", got)
	}
}

func TestTokenCreateAuthenticateRevoke(t *testing.T) {
	s := openTemp(t)
	tok, err := s.CreateToken("laptop", "admin")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if tok == "" {
		t.Fatal("empty token")
	}
	got, err := s.AuthenticateToken(tok)
	if err != nil {
		t.Fatalf("AuthenticateToken: %v", err)
	}
	if got.Label != "laptop" || got.Scope != "admin" {
		t.Errorf("got %+v", got)
	}
	if _, err := s.AuthenticateToken("nope"); !errors.Is(err, ErrBadToken) {
		t.Fatalf("unknown token: want ErrBadToken, got %v", err)
	}
	if err := s.RevokeToken("laptop"); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if _, err := s.AuthenticateToken(tok); !errors.Is(err, ErrBadToken) {
		t.Fatalf("after revoke: want ErrBadToken, got %v", err)
	}
	if err := s.RevokeToken("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke unknown: want ErrNotFound, got %v", err)
	}
}

func TestTokenDuplicateLabelRejected(t *testing.T) {
	s := openTemp(t)
	if _, err := s.CreateToken("laptop", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateToken("laptop", "admin"); err == nil {
		t.Fatal("want error on duplicate label")
	}
}

func TestOpenSetsBusyTimeout(t *testing.T) {
	s := openTemp(t)
	var timeout int
	if err := s.db.QueryRow(`PRAGMA busy_timeout`).Scan(&timeout); err != nil {
		t.Fatalf("PRAGMA busy_timeout: %v", err)
	}
	if timeout != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", timeout)
	}
}

func TestListTokens(t *testing.T) {
	s := openTemp(t)
	if _, err := s.CreateToken("a", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateToken("b", "readonly"); err != nil {
		t.Fatal(err)
	}
	toks, err := s.ListTokens()
	if err != nil {
		t.Fatal(err)
	}
	if len(toks) != 2 {
		t.Fatalf("len = %d, want 2", len(toks))
	}
}

func TestDeleteTokenHardDeletes(t *testing.T) {
	st := openTemp(t)

	if _, err := st.CreateToken("relay:base.example.com", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteToken("relay:base.example.com"); err != nil {
		t.Fatal(err)
	}
	toks, err := st.ListTokens()
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range toks {
		if tk.Label == "relay:base.example.com" {
			t.Fatal("token row still present after DeleteToken")
		}
	}
	// Deleting a non-existent label is not an error (idempotent unwind).
	if err := st.DeleteToken("relay:base.example.com"); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

func TestLatestDeployment(t *testing.T) {
	s := openTemp(t)
	s.CreateApp("blog", 8080)
	if _, err := s.LatestDeployment("blog"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty LatestDeployment err = %v, want ErrNotFound", err)
	}
	s.CreateDeployment("blog", "img1", "c1", 40001, "running", "")
	d2, _ := s.CreateDeployment("blog", "img2", "c2", 40002, "failed", "")
	got, err := s.LatestDeployment("blog")
	if err != nil {
		t.Fatalf("LatestDeployment: %v", err)
	}
	if got.ID != d2.ID || got.Status != "failed" {
		t.Errorf("LatestDeployment = %+v, want id %s status failed", got, d2.ID)
	}
}

func TestLatestDeploymentIgnoresPreviews(t *testing.T) {
	s := openTemp(t)
	s.CreateApp("blog", 8080)
	s.CreateDeployment("blog", "img", "main-c", 40000, "running", "")
	// Created later, so it would win on created_at if pr>0 rows weren't excluded.
	s.CreatePreviewDeployment("blog", 3, "img", "preview-c", 41000, "failed", "")
	got, err := s.LatestDeployment("blog")
	if err != nil {
		t.Fatalf("LatestDeployment: %v", err)
	}
	if got.ContainerID != "main-c" || got.Status != "running" {
		t.Errorf("LatestDeployment = %+v, want main-c/running", got)
	}
}

func TestDeploymentLogsRoundTripAndScoping(t *testing.T) {
	s := openTemp(t)
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateApp("api", 3000); err != nil {
		t.Fatal(err)
	}
	d, err := s.CreateDeployment("blog", "img1", "c1", 40001, "failed", "step 1/2\nboom\n")
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}

	logs, err := s.DeploymentLogs("blog", d.ID)
	if err != nil {
		t.Fatalf("DeploymentLogs: %v", err)
	}
	if !strings.Contains(logs, "boom") {
		t.Errorf("logs = %q, want build output", logs)
	}
	// Same id under another app must not resolve.
	if _, err := s.DeploymentLogs("api", d.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-app lookup err = %v, want ErrNotFound", err)
	}
	if _, err := s.DeploymentLogs("blog", "no-such-id"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id err = %v, want ErrNotFound", err)
	}
}

func TestListDeploymentsNewestFirst(t *testing.T) {
	s := openTemp(t)
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDeployment("blog", "img1", "c1", 40001, "failed", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDeployment("blog", "img2", "c2", 40002, "running", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePreviewDeployment("blog", 5, "img3", "c3", 40003, "running", ""); err != nil {
		t.Fatal(err)
	}

	deps, err := s.ListDeployments("blog")
	if err != nil {
		t.Fatalf("ListDeployments: %v", err)
	}
	if len(deps) != 3 {
		t.Fatalf("len = %d, want 3 (previews included)", len(deps))
	}
	if deps[0].ImageID != "img3" || deps[2].ImageID != "img1" {
		t.Errorf("order = [%s %s %s], want newest first", deps[0].ImageID, deps[1].ImageID, deps[2].ImageID)
	}
	if deps[0].PR != 5 {
		t.Errorf("deps[0].PR = %d, want 5", deps[0].PR)
	}
	if empty, err := s.ListDeployments("never-deployed"); err != nil || len(empty) != 0 {
		t.Errorf("unknown app = %v (err %v), want empty, nil", empty, err)
	}
}

func TestDeploymentLogRetentionPrunesTo20PerApp(t *testing.T) {
	s := openTemp(t)
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateApp("api", 3000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDeployment("api", "img", "c", 40000, "running", "other-app log"); err != nil {
		t.Fatal(err)
	}
	var last Deployment
	for i := 0; i < 22; i++ {
		var err error
		last, err = s.CreateDeployment("blog", "img", "c", 40001, "running", "log body")
		if err != nil {
			t.Fatal(err)
		}
	}

	var withLogs int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM deployments WHERE app='blog' AND logs != ''`).Scan(&withLogs); err != nil {
		t.Fatal(err)
	}
	if withLogs != 20 {
		t.Errorf("blog rows with logs = %d, want 20", withLogs)
	}
	var rows int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM deployments WHERE app='blog'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 22 {
		t.Errorf("blog rows = %d, want 22 (rows are history; only logs are pruned)", rows)
	}
	// The newest deployment's log survives; the other app is untouched.
	if logs, err := s.DeploymentLogs("blog", last.ID); err != nil || logs != "log body" {
		t.Errorf("newest logs = %q (err %v), want kept", logs, err)
	}
	var otherWithLogs int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM deployments WHERE app='api' AND logs != ''`).Scan(&otherWithLogs); err != nil {
		t.Fatal(err)
	}
	if otherWithLogs != 1 {
		t.Errorf("api rows with logs = %d, want 1", otherWithLogs)
	}
}

func TestDomainConfigRoundTrip(t *testing.T) {
	s := openTemp(t)

	if _, err := s.GetDomainConfig(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetDomainConfig on empty store: err = %v, want ErrNotFound", err)
	}

	if err := s.SetDomainConfig("example.com", "cloudflare", "cf-token"); err != nil {
		t.Fatalf("SetDomainConfig: %v", err)
	}
	dc, err := s.GetDomainConfig()
	if err != nil {
		t.Fatalf("GetDomainConfig: %v", err)
	}
	if dc.Domain != "example.com" || dc.DNSProvider != "cloudflare" || dc.DNSToken != "cf-token" {
		t.Fatalf("round-trip = %+v", dc)
	}
	if dc.Status != "issuing" || dc.Error != "" || !dc.CertNotAfter.IsZero() {
		t.Fatalf("fresh config = %+v, want status=issuing, no error, zero not-after", dc)
	}

	notAfter := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	if err := s.UpdateDomainStatus("example.com", "active", "", notAfter); err != nil {
		t.Fatalf("UpdateDomainStatus: %v", err)
	}
	dc, _ = s.GetDomainConfig()
	if dc.Status != "active" || !dc.CertNotAfter.Equal(notAfter) {
		t.Fatalf("after update = %+v", dc)
	}

	if err := s.UpdateDomainStatus("example.com", "failed", "acme: boom", time.Time{}); err != nil {
		t.Fatalf("UpdateDomainStatus failed: %v", err)
	}
	dc, _ = s.GetDomainConfig()
	if dc.Status != "failed" || dc.Error != "acme: boom" {
		t.Fatalf("failed update = %+v", dc)
	}

	// Re-Set replaces the row and resets status/error.
	if err := s.SetDomainConfig("other.dev", "cloudflare", "tok2"); err != nil {
		t.Fatalf("re-SetDomainConfig: %v", err)
	}
	dc, _ = s.GetDomainConfig()
	if dc.Domain != "other.dev" || dc.Status != "issuing" || dc.Error != "" {
		t.Fatalf("after re-set = %+v", dc)
	}

	if err := s.DeleteDomainConfig(); err != nil {
		t.Fatalf("DeleteDomainConfig: %v", err)
	}
	if _, err := s.GetDomainConfig(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: err = %v, want ErrNotFound", err)
	}
}

func TestUpdateDomainStatusWithoutRow(t *testing.T) {
	s := openTemp(t)
	if err := s.UpdateDomainStatus("example.com", "active", "", time.Time{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestUpdateDomainStatusWrongDomain(t *testing.T) {
	s := openTemp(t)
	if err := s.SetDomainConfig("new.dev", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	// A run holding a snapshot of a replaced config must not stamp the new row.
	if err := s.UpdateDomainStatus("old.dev", "active", "", time.Time{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale-domain update: err = %v, want ErrNotFound", err)
	}
	dc, err := s.GetDomainConfig()
	if err != nil {
		t.Fatal(err)
	}
	if dc.Status != "issuing" || dc.Domain != "new.dev" {
		t.Fatalf("stale update mutated row: %+v", dc)
	}
}

func TestDeleteAppRemovesAppAndHistory(t *testing.T) {
	s := openTemp(t)
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateApp("api", 3000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDeployment("blog", "img1", "c1", 40001, "running", "log"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDeployment("api", "img2", "c2", 40002, "running", ""); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteApp("blog"); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
	if _, err := s.GetApp("blog"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetApp after delete err = %v, want ErrNotFound", err)
	}
	deps, err := s.ListDeployments("blog")
	if err != nil || len(deps) != 0 {
		t.Errorf("deployments after delete = %v (err %v), want none", deps, err)
	}
	// Other apps and their history are untouched.
	if _, err := s.GetApp("api"); err != nil {
		t.Errorf("GetApp(api) after delete: %v", err)
	}
	if deps, _ := s.ListDeployments("api"); len(deps) != 1 {
		t.Errorf("api deployments = %d, want 1", len(deps))
	}
}

func TestBuildingRowLifecycle(t *testing.T) {
	s := openTemp(t)
	if _, err := s.CreateApp("web", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	dep, err := s.CreateDeployment("web", "", "", 0, "building", "")
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if dep.Status != "building" {
		t.Fatalf("status = %q, want building", dep.Status)
	}

	if err := s.UpdateDeploymentLogs(dep.ID, "pulling base image...\n"); err != nil {
		t.Fatalf("UpdateDeploymentLogs: %v", err)
	}
	logs, err := s.DeploymentLogs("web", dep.ID)
	if err != nil {
		t.Fatalf("DeploymentLogs: %v", err)
	}
	if logs != "pulling base image...\n" {
		t.Fatalf("logs = %q", logs)
	}

	if err := s.FinalizeDeployment(dep.ID, "img-1", "cid-1", 40001, "running", "done\n"); err != nil {
		t.Fatalf("FinalizeDeployment: %v", err)
	}
	got, err := s.LatestDeployment("web")
	if err != nil {
		t.Fatalf("LatestDeployment: %v", err)
	}
	if got.Status != "running" || got.ImageID != "img-1" || got.ContainerID != "cid-1" || got.HostPort != 40001 {
		t.Fatalf("finalized row = %+v", got)
	}
	logs, _ = s.DeploymentLogs("web", dep.ID)
	if logs != "done\n" {
		t.Fatalf("finalized logs = %q", logs)
	}
}

func TestFailBuildingDeployments(t *testing.T) {
	s := openTemp(t)
	if _, err := s.CreateApp("web", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	building, err := s.CreateDeployment("web", "", "", 0, "building", "pulling base image...\n")
	if err != nil {
		t.Fatalf("CreateDeployment building: %v", err)
	}
	running, err := s.CreateDeployment("web", "img", "cid", 40001, "running", "done\n")
	if err != nil {
		t.Fatalf("CreateDeployment running: %v", err)
	}

	n, err := s.FailBuildingDeployments()
	if err != nil {
		t.Fatalf("FailBuildingDeployments: %v", err)
	}
	if n != 1 {
		t.Fatalf("rows changed = %d, want 1", n)
	}

	deps, err := s.ListDeployments("web")
	if err != nil {
		t.Fatalf("ListDeployments: %v", err)
	}
	byID := map[string]Deployment{}
	for _, d := range deps {
		byID[d.ID] = d
	}
	if got := byID[building.ID].Status; got != "failed" {
		t.Fatalf("building row status = %q, want failed", got)
	}
	if got := byID[running.ID].Status; got != "running" {
		t.Fatalf("running row status = %q, want untouched running", got)
	}
	logs, _ := s.DeploymentLogs("web", building.ID)
	if !strings.Contains(logs, "pulling base image...") || !strings.Contains(logs, "aborted") {
		t.Fatalf("failed building logs = %q, want streamed log kept + abort note", logs)
	}
}

func TestDeleteAppUnknownIsNotFound(t *testing.T) {
	s := openTemp(t)
	if err := s.DeleteApp("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteApp(ghost) err = %v, want ErrNotFound", err)
	}
}

func TestDeleteAppRemovesDomains(t *testing.T) {
	s := openTemp(t)
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateApp("api", 3000); err != nil {
		t.Fatal(err)
	}
	if err := s.AddAppDomain("example.com", "blog"); err != nil {
		t.Fatalf("AddAppDomain: %v", err)
	}

	if err := s.DeleteApp("blog"); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
	if _, err := s.GetAppDomain("example.com"); !errors.Is(err, ErrNotFound) {
		t.Errorf("domain row still present after DeleteApp: err = %v, want ErrNotFound", err)
	}
	// The domain must be reclaimable by another app.
	if err := s.AddAppDomain("example.com", "api"); err != nil {
		t.Fatalf("re-add domain after delete: %v", err)
	}
}

// insertDeploymentAt writes a deployment row with an explicit created_at so
// tie/ordering cases (identical or lexically-misordered timestamps) can be
// forced deterministically.
func insertDeploymentAt(t *testing.T, s *Store, app, id, containerID, status, createdAt string) {
	t.Helper()
	if _, err := s.db.Exec(
		`INSERT INTO deployments(id, app, image_id, container_id, host_port, status, logs, created_at)
		 VALUES(?,?,?,?,?,?,?,?)`,
		id, app, "", containerID, 0, status, "", createdAt); err != nil {
		t.Fatalf("insert deployment: %v", err)
	}
}

// created_at is RFC3339Nano text, whose lexical order is not chronological:
// trailing zeros are dropped, so "12:00:00.1Z" (100ms) sorts lexically AFTER
// "12:00:00.15Z" (150ms) even though it is earlier. Ordered queries must key on
// rowid (insertion order) so the truly-newest row wins. See #109.
func TestAppDomainRoundTrip(t *testing.T) {
	s := openTemp(t)

	if _, err := s.GetAppDomain("example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetAppDomain on empty store: err = %v, want ErrNotFound", err)
	}

	if err := s.AddAppDomain("example.com", "blog"); err != nil {
		t.Fatalf("AddAppDomain: %v", err)
	}
	d, err := s.GetAppDomain("example.com")
	if err != nil {
		t.Fatalf("GetAppDomain: %v", err)
	}
	if d.Domain != "example.com" || d.App != "blog" || d.Status != "" || d.Error != "" || !d.CertNotAfter.IsZero() {
		t.Fatalf("fresh domain = %+v, want empty status/error/zero not-after", d)
	}
	if d.UpdatedAt.IsZero() {
		t.Fatalf("UpdatedAt not set")
	}

	notAfter := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	if err := s.UpdateAppDomainStatus("example.com", "active", "", notAfter); err != nil {
		t.Fatalf("UpdateAppDomainStatus: %v", err)
	}
	d, _ = s.GetAppDomain("example.com")
	if d.Status != "active" || !d.CertNotAfter.Equal(notAfter) {
		t.Fatalf("after update = %+v", d)
	}

	if err := s.UpdateAppDomainStatus("example.com", "failed", "acme: boom", time.Time{}); err != nil {
		t.Fatalf("UpdateAppDomainStatus failed: %v", err)
	}
	d, _ = s.GetAppDomain("example.com")
	if d.Status != "failed" || d.Error != "acme: boom" || !d.CertNotAfter.IsZero() {
		t.Fatalf("failed update = %+v", d)
	}

	if err := s.DeleteAppDomain("example.com"); err != nil {
		t.Fatalf("DeleteAppDomain: %v", err)
	}
	if _, err := s.GetAppDomain("example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: err = %v, want ErrNotFound", err)
	}
	// Deleting an absent domain is not an error.
	if err := s.DeleteAppDomain("example.com"); err != nil {
		t.Fatalf("DeleteAppDomain missing: %v", err)
	}
}

func TestAddAppDomainDuplicateErrorsCleanly(t *testing.T) {
	s := openTemp(t)
	if err := s.AddAppDomain("example.com", "blog"); err != nil {
		t.Fatalf("AddAppDomain: %v", err)
	}
	// Same domain under a different app must error cleanly, not panic.
	if err := s.AddAppDomain("example.com", "api"); !errors.Is(err, ErrDomainExists) {
		t.Fatalf("duplicate domain err = %v, want ErrDomainExists", err)
	}
}

func TestUpdateAppDomainStatusMissingDomain(t *testing.T) {
	s := openTemp(t)
	if err := s.UpdateAppDomainStatus("example.com", "active", "", time.Time{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestListAppDomainsFiltersByApp(t *testing.T) {
	s := openTemp(t)
	for _, d := range []struct{ domain, app string }{
		{"blog1.dev", "blog"},
		{"blog2.dev", "blog"},
		{"api.dev", "api"},
	} {
		if err := s.AddAppDomain(d.domain, d.app); err != nil {
			t.Fatalf("AddAppDomain(%q,%q): %v", d.domain, d.app, err)
		}
	}

	blog, err := s.ListAppDomains("blog")
	if err != nil {
		t.Fatalf("ListAppDomains(blog): %v", err)
	}
	if len(blog) != 2 || blog[0].Domain != "blog1.dev" || blog[1].Domain != "blog2.dev" {
		t.Fatalf("blog domains = %+v", blog)
	}

	api, err := s.ListAppDomains("api")
	if err != nil {
		t.Fatalf("ListAppDomains(api): %v", err)
	}
	if len(api) != 1 || api[0].Domain != "api.dev" {
		t.Fatalf("api domains = %+v", api)
	}

	empty, err := s.ListAppDomains("none")
	if err != nil || len(empty) != 0 {
		t.Fatalf("unknown app = %v (err %v), want empty, nil", empty, err)
	}
}

func TestListActiveAppDomainsFiltersByStatus(t *testing.T) {
	s := openTemp(t)
	for _, d := range []struct{ domain, app, status string }{
		{"active1.dev", "blog", "active"},
		{"active2.dev", "api", "active"},
		{"pending.dev", "blog", "pending"},
		{"failed.dev", "api", "failed"},
	} {
		if err := s.AddAppDomain(d.domain, d.app); err != nil {
			t.Fatalf("AddAppDomain(%q,%q): %v", d.domain, d.app, err)
		}
		if err := s.UpdateAppDomainStatus(d.domain, d.status, "", time.Time{}); err != nil {
			t.Fatalf("UpdateAppDomainStatus: %v", err)
		}
	}

	active, err := s.ListActiveAppDomains()
	if err != nil {
		t.Fatalf("ListActiveAppDomains: %v", err)
	}
	if len(active) != 2 {
		t.Fatalf("len = %d, want 2", len(active))
	}
	if active[0].Domain != "active1.dev" || active[1].Domain != "active2.dev" {
		t.Fatalf("active domains = %+v", active)
	}
}

func TestAllAppDomainsListsEveryStatus(t *testing.T) {
	s := openTemp(t)

	all, err := s.AllAppDomains()
	if err != nil || len(all) != 0 {
		t.Fatalf("empty store = %v (err %v), want empty, nil", all, err)
	}

	for _, d := range []struct{ domain, app, status string }{
		{"b.dev", "blog", "active"},
		{"a.dev", "api", "pending"},
		{"c.dev", "blog", "failed"},
	} {
		if err := s.AddAppDomain(d.domain, d.app); err != nil {
			t.Fatalf("AddAppDomain(%q,%q): %v", d.domain, d.app, err)
		}
		if err := s.UpdateAppDomainStatus(d.domain, d.status, "", time.Time{}); err != nil {
			t.Fatalf("UpdateAppDomainStatus: %v", err)
		}
	}

	all, err = s.AllAppDomains()
	if err != nil {
		t.Fatalf("AllAppDomains: %v", err)
	}
	if len(all) != 3 || all[0].Domain != "a.dev" || all[1].Domain != "b.dev" || all[2].Domain != "c.dev" {
		t.Fatalf("all domains = %+v, want a.dev/b.dev/c.dev in order", all)
	}
	if all[0].Status != "pending" || all[1].App != "blog" {
		t.Fatalf("row contents = %+v", all)
	}
}

func TestDeploymentOrderingTiebreaksOnRowid(t *testing.T) {
	s := openTemp(t)
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	// d1 earlier (100ms) but lexically greater; d2 later (150ms) but lexically
	// smaller. created_at DESC would wrongly pick d1; rowid DESC picks d2.
	insertDeploymentAt(t, s, "blog", "d1", "c1", "running", "2026-07-12T12:00:00.1Z")
	insertDeploymentAt(t, s, "blog", "d2", "c2", "running", "2026-07-12T12:00:00.15Z")

	if got, err := s.LatestDeployment("blog"); err != nil || got.ID != "d2" {
		t.Fatalf("LatestDeployment = %q (err %v), want d2", got.ID, err)
	}
	if got, err := s.LatestRunning("blog"); err != nil || got.ID != "d2" {
		t.Fatalf("LatestRunning = %q (err %v), want d2", got.ID, err)
	}
	deps, err := s.ListDeployments("blog")
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 2 || deps[0].ID != "d2" || deps[1].ID != "d1" {
		t.Fatalf("ListDeployments order = %+v, want [d2 d1]", deps)
	}
}
