package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/piperbox/piper/internal/config"
	"github.com/piperbox/piper/internal/relayclient"
)

// pushView emits a pushMsg for v and applies it, returning the model with v on
// top. It mirrors what the root does when a view requests navigation.
func pushView(t *testing.T, m Model, v view) (Model, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(pushMsg{view: v})
	return next.(Model), cmd
}

func typeLinkRepo(t *testing.T, v linkFormView, s string) linkFormView {
	t.Helper()
	for _, r := range s {
		next, _ := v.Update(keyRunes(r))
		v = next.(linkFormView)
	}
	return v
}

func TestLinkFormSubmitEmitsLinkAppMsg(t *testing.T) {
	v := typeLinkRepo(t, newLinkForm("blog"), "octo/blog")
	next, cmd := v.Update(keyEnter())
	_ = next
	if cmd == nil {
		t.Fatal("a filled repo should emit a link cmd")
	}
	msg, ok := cmd().(linkAppMsg)
	if !ok {
		t.Fatalf("want linkAppMsg, got %T", cmd())
	}
	if msg.name != "blog" || msg.repo != "octo/blog" || msg.branch != "main" {
		t.Fatalf("unexpected linkAppMsg: %+v", msg)
	}
}

func TestLinkFormEmptyRepoRejected(t *testing.T) {
	next, cmd := newLinkForm("blog").Update(keyEnter())
	if cmd != nil {
		t.Fatal("empty repo should not emit a cmd")
	}
	if !strings.Contains(next.(linkFormView).View(), "repo is required") {
		t.Fatalf("expected 'repo is required', got:\n%s", next.(linkFormView).View())
	}
}

// Free text always submits, so the form is where a bare repo name gets caught
// before it becomes a broken binding — the agent rejects it too, but a form
// error names the shape without a round trip (#333).
func TestLinkFormRepoWithoutOwnerRejected(t *testing.T) {
	v := typeLinkRepo(t, newLinkForm("blog"), "next")
	next, cmd := v.Update(keyEnter())
	if cmd != nil {
		t.Fatal("a repo without an owner/ prefix should not emit a cmd")
	}
	if !strings.Contains(next.(linkFormView).View(), "owner/name") {
		t.Fatalf("expected the error to name owner/name, got:\n%s", next.(linkFormView).View())
	}
}

func TestLinkAppRootRunsClientAndPops(t *testing.T) {
	rec := &apiCalls{}
	m := NewModel("pi4", "a", false, fakeAPI{rec: rec})
	// push app detail then the link form so a successful link pops back to detail
	m, _ = pushView(t, m, newAppDetailView("blog"))
	m, _ = pushView(t, m, newLinkForm("blog"))
	depth := len(m.stack)
	next, cmd := m.Update(linkAppMsg{name: "blog", repo: "octo/blog", branch: "main", rootDir: "apps/web"})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("expected LinkApp to run as a cmd")
	}
	m = pump(t, m, cmd) // actionResultMsg{nil,1}
	if rec.linkRepo != "octo/blog" || rec.linkName != "blog" || rec.linkRootDir != "apps/web" {
		t.Fatalf("LinkApp not called with the right args: %+v", rec)
	}
	if len(m.stack) != depth-1 {
		t.Fatalf("success should pop one level: was %d now %d", depth, len(m.stack))
	}
}

func TestLKeyLowercaseOpensLinkFromDetail(t *testing.T) {
	m := NewModel("pi4", "a", false, fakeAPI{})
	m, _ = pushView(t, m, newAppDetailView("blog"))
	next, cmd := m.top().Update(keyRunes('l'))
	m.stack[len(m.stack)-1] = next.(view)
	m = pump(t, m, cmd)
	if _, ok := m.top().(linkFormView); !ok {
		t.Fatalf("l from app detail should push the link form, got %T", m.top())
	}
}

func TestLinkFormRootDirSubmitted(t *testing.T) {
	v := typeLinkRepo(t, newLinkForm("blog"), "octo/blog")
	next, _ := v.Update(keyTab())                  // → branch
	next, _ = next.(linkFormView).Update(keyTab()) // → root dir
	lf := next.(linkFormView)
	for _, r := range "apps/web" {
		n, _ := lf.Update(keyRunes(r))
		lf = n.(linkFormView)
	}
	_, cmd := lf.Update(keyEnter())
	if cmd == nil {
		t.Fatal("expected a link cmd")
	}
	msg := cmd().(linkAppMsg)
	if msg.rootDir != "apps/web" || msg.repo != "octo/blog" || msg.branch != "main" {
		t.Fatalf("unexpected linkAppMsg: %+v", msg)
	}
}

func fixturePickRepos() linkReposMsg {
	return linkReposMsg{repos: []pickRepo{
		{fullName: "octo/blog", target: "octo"},
		{fullName: "octo/site", target: "octo"},
		{fullName: "acme/api", target: "acme-org"},
	}}
}

func TestLinkFormPickerFiltersAndFills(t *testing.T) {
	next, _ := newLinkForm("blog").Update(fixturePickRepos())
	v := typeLinkRepo(t, next.(linkFormView), "octo")
	out := v.View()
	if !strings.Contains(out, "octo/blog") || !strings.Contains(out, "octo/site") || strings.Contains(out, "acme/api") {
		t.Fatalf("filter should keep octo repos only, got:\n%s", out)
	}
	n, _ := v.Update(tea.KeyMsg(tea.Key{Type: tea.KeyDown})) // select first match
	n, _ = n.(linkFormView).Update(keyEnter())               // accept → fills repo, focus branch
	v = n.(linkFormView)
	if got := strings.TrimSpace(v.repo.Value()); got != "octo/blog" {
		t.Fatalf("accept should fill the repo field, got %q", got)
	}
	_, cmd := v.Update(keyEnter()) // enter from branch submits
	msg := cmd().(linkAppMsg)
	if msg.repo != "octo/blog" {
		t.Fatalf("submit after pick: %+v", msg)
	}
}

// The root's ~2s poll delivers non-key messages to the top view; they must not
// wipe the picker selection out from under the user (#334) — enter after a
// tick must still accept the highlighted repo, not submit the typed filter as
// free text.
func TestLinkFormPickerSelectionSurvivesPollTick(t *testing.T) {
	next, _ := newLinkForm("blog").Update(fixturePickRepos())
	v := typeLinkRepo(t, next.(linkFormView), "octo")
	n, _ := v.Update(tea.KeyMsg(tea.Key{Type: tea.KeyDown})) // highlight octo/blog
	n, _ = n.(linkFormView).Update(tickMsg{})                // a poll tick passes through
	n, _ = n.(linkFormView).Update(keyEnter())               // must accept the pick
	v = n.(linkFormView)
	if got := strings.TrimSpace(v.repo.Value()); got != "octo/blog" {
		t.Fatalf("selection lost to the tick; repo = %q, want octo/blog", got)
	}
}

func TestLinkFormEnterWithoutSelectionSubmitsFreeText(t *testing.T) {
	next, _ := newLinkForm("blog").Update(fixturePickRepos())
	v := typeLinkRepo(t, next.(linkFormView), "someone/else")
	_, cmd := v.Update(keyEnter())
	if cmd == nil {
		t.Fatal("free text must submit without a selection")
	}
	if msg := cmd().(linkAppMsg); msg.repo != "someone/else" {
		t.Fatalf("want the typed repo, got %+v", msg)
	}
}

func TestLinkFormTargetLabelOnlyWhenMultipleInstallations(t *testing.T) {
	multi := fixturePickRepos()
	multi.multi = true
	next, _ := newLinkForm("blog").Update(multi)
	if out := next.(linkFormView).View(); !strings.Contains(out, "acme-org") {
		t.Fatalf("multi-installation matches must carry their target, got:\n%s", out)
	}
	next, _ = newLinkForm("blog").Update(fixturePickRepos())
	if out := next.(linkFormView).View(); strings.Contains(out, "acme-org") {
		t.Fatalf("single-installation matches must not repeat the target, got:\n%s", out)
	}
}

func TestLinkFormNoCredShowsHint(t *testing.T) {
	next, _ := newLinkForm("blog").Update(linkReposMsg{noCred: true})
	if out := next.(linkFormView).View(); !strings.Contains(out, "press g to connect GitHub") {
		t.Fatalf("want the login hint, got:\n%s", out)
	}
}

func TestLinkFormLoadsRelayReposOnceAndFlattens(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := config.SaveClient(config.ClientConfig{RelayAPI: "https://r.example", AccountCredential: "cred-1"}); err != nil {
		t.Fatal(err)
	}
	relay := relayFor(fakeRelay{
		st: relayclient.Status{GitHubApp: true, Installations: []relayclient.Installation{
			{ID: "55", TargetLogin: "octo"}, {ID: "66", TargetLogin: "acme-org"},
		}},
		repos: []relayclient.Repo{{FullName: "octo/blog"}},
	})
	msg := loadLinkRepos(relay).(linkReposMsg)
	// the fake returns the same repos for both installations: 2 flattened entries
	if len(msg.repos) != 2 || !msg.multi || msg.repos[1].target != "acme-org" {
		t.Fatalf("want flattened multi-install repos, got %+v", msg)
	}
	v := newLinkForm("blog")
	v.relay = relay
	if v.refresh(nil) == nil {
		t.Fatal("with a relay and no repos yet, refresh must load")
	}
	next, _ := v.Update(msg)
	if next.(linkFormView).refresh(nil) != nil {
		t.Fatal("repos load once; refresh must go quiet after the load")
	}
}

func TestRootInjectsRelayIntoLinkForm(t *testing.T) {
	m := NewModel("pi4", "a", false, fakeAPI{}).WithRelay(relayFor(fakeRelay{}))
	next, _ := m.Update(pushMsg{view: newLinkForm("blog")})
	lf, ok := next.(Model).top().(linkFormView)
	if !ok || lf.relay == nil {
		t.Fatal("pushing a link form must inject the root's relay dialer")
	}
}
