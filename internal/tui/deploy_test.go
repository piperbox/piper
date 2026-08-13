package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/piperbox/piper/internal/store"
)

func TestDeployViewConfirmRender(t *testing.T) {
	out := newDeployView("blog", "/Users/me/code/blog", true).View()
	for _, want := range []string{"deploy blog", "/Users/me/code/blog", "found ✓", "ship it"} {
		if !strings.Contains(out, want) {
			t.Fatalf("deploy confirm missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(newDeployView("blog", "/x", false).View(), "not found ✗") {
		t.Fatal("missing-Dockerfile state not rendered")
	}
}

func TestDeployViewShipEmitsDeployIntent(t *testing.T) {
	v := newDeployView("blog", "/x/blog", true)
	m, cmd := v.Update(keyRunes('y'))
	if cmd == nil {
		t.Fatal("y should ship")
	}
	msg, ok := cmd().(deployMsg)
	if !ok || msg.name != "blog" || msg.cwd != "/x/blog" {
		t.Fatalf("want deployMsg{blog,/x/blog}, got %#v", cmd())
	}
	if !strings.Contains(m.(deployView).View(), "shipping") {
		t.Fatalf("should show shipping state:\n%s", m.(deployView).View())
	}
}

func TestRepoDeployViewConfirmRender(t *testing.T) {
	out := newRepoDeployView("blog", "me/blog", "main").View()
	for _, want := range []string{"deploy blog", "me/blog@main", "ship it"} {
		if !strings.Contains(out, want) {
			t.Fatalf("repo deploy confirm missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Dockerfile") {
		t.Fatalf("repo deploy confirm must not stat a local Dockerfile:\n%s", out)
	}
}

func TestRepoDeployViewShipEmitsRepoDeployIntent(t *testing.T) {
	v := newRepoDeployView("blog", "me/blog", "main")
	m, cmd := v.Update(keyRunes('y'))
	if cmd == nil {
		t.Fatal("y should ship")
	}
	msg, ok := cmd().(deployMsg)
	if !ok || msg.name != "blog" || !msg.fromRepo {
		t.Fatalf("want deployMsg{blog, fromRepo}, got %#v", cmd())
	}
	if !strings.Contains(m.(deployView).View(), "shipping") {
		t.Fatalf("should show shipping state:\n%s", m.(deployView).View())
	}
}

func TestRootRepoDeployMsgKicksOffFromRepo(t *testing.T) {
	rec := &apiCalls{}
	m := NewModel("b", "a", false, fakeAPI{rec: rec, deployDep: store.Deployment{ID: "dep-xyz"}})
	_, cmd := m.Update(deployMsg{name: "blog", fromRepo: true})
	msg, ok := cmd().(deployStartedMsg)
	if !ok || msg.app != "blog" || msg.id != "dep-xyz" || msg.err != nil {
		t.Fatalf("want deployStartedMsg{blog,dep-xyz,nil}, got %#v", cmd())
	}
	if rec.deployRepoName != "blog" {
		t.Fatalf("DeployFromRepo not called: %+v", rec)
	}
	if rec.deployName != "" {
		t.Fatalf("local Deploy must not be called for a repo deploy: %+v", rec)
	}
}

func TestRootDeployMsgKicksOffAndRecords(t *testing.T) {
	rec := &apiCalls{}
	m := NewModel("b", "a", false, fakeAPI{rec: rec, deployDep: store.Deployment{ID: "dep-xyz"}})
	_, cmd := m.Update(deployMsg{name: "blog", cwd: "/x/blog"})
	msg, ok := cmd().(deployStartedMsg)
	if !ok || msg.app != "blog" || msg.id != "dep-xyz" || msg.err != nil {
		t.Fatalf("want deployStartedMsg{blog,dep-xyz,nil}, got %#v", cmd())
	}
	if rec.deployName != "blog" || rec.deployDir != "/x/blog" {
		t.Fatalf("Deploy not passed through: %+v", rec)
	}
}

func TestRootDeployStartedReplacesConfirmWithLogs(t *testing.T) {
	m := NewModel("b", "a", false, fakeAPI{})
	// stack: apps -> detail -> deploy
	m2, _ := m.Update(pushMsg{newAppDetailView("blog")})
	m = m2.(Model)
	m2, _ = m.Update(pushMsg{newDeployView("blog", "/x", true)})
	m = m2.(Model)
	depth := len(m.stack)

	// success: the deploy confirm is replaced (not stacked) by a logs view
	m2, _ = m.Update(deployStartedMsg{app: "blog", id: "dep-xyz"})
	m = m2.(Model)
	if len(m.stack) != depth {
		t.Fatalf("replace should keep depth %d, got %d", depth, len(m.stack))
	}
	if m.top().title() != "logs" {
		t.Fatalf("top should be the logs view, got %q", m.top().title())
	}
}

func TestRootDeployStartedErrorBannersDeployView(t *testing.T) {
	m := NewModel("b", "a", false, fakeAPI{})
	m2, _ := m.Update(pushMsg{newDeployView("blog", "/x", true)})
	m = m2.(Model)
	m2, _ = m.Update(deployStartedMsg{app: "blog", err: errors.New("upload failed")})
	m = m2.(Model)
	if m.top().title() != "deploy" {
		t.Fatalf("error must not replace the deploy view, got %q", m.top().title())
	}
	if !strings.Contains(m.View(), "upload failed") {
		t.Fatalf("kickoff error banner missing:\n%s", m.View())
	}
}
