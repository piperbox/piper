package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/piperbox/piper/internal/config"
	"github.com/piperbox/piper/internal/relayclient"
)

// fakeRelay is a scriptable RelayAPI for wizard and picker tests.
type fakeRelay struct {
	handle, code string
	startErr     error
	acc          relayclient.Account
	pollErr      error
	st           relayclient.Status
	stErr        error
	repos        []relayclient.Repo
	reposErr     error
	agents       []relayclient.Agent
	agentsErr    error
}

func (f fakeRelay) CLILoginStart(context.Context) (string, string, error) {
	return f.handle, f.code, f.startErr
}
func (f fakeRelay) CLILoginPoll(context.Context, string) (relayclient.Account, error) {
	return f.acc, f.pollErr
}
func (f fakeRelay) GitHubStatus(context.Context, string) (relayclient.Status, error) {
	return f.st, f.stErr
}
func (f fakeRelay) GitHubRepos(context.Context, string, string) ([]relayclient.Repo, error) {
	return f.repos, f.reposErr
}
func (f fakeRelay) Agents(context.Context, string) ([]relayclient.Agent, error) {
	return f.agents, f.agentsErr
}

func relayFor(f fakeRelay) RelayDialer { return func(string) RelayAPI { return f } }

func TestWizardNoCredShowsLogin(t *testing.T) {
	v := newGithubWizard(relayFor(fakeRelay{}))
	next, _ := v.Update(wizStatusMsg{noCred: true, base: "https://r.example"})
	out := next.(githubWizardView).View()
	if !strings.Contains(out, "sign in with GitHub") || !strings.Contains(out, "https://r.example") {
		t.Fatalf("want the armed login step, got:\n%s", out)
	}
}

func TestWizardStaleCredentialFallsBackToLogin(t *testing.T) {
	v := newGithubWizard(relayFor(fakeRelay{}))
	next, _ := v.Update(wizStatusMsg{base: "https://r.example", cred: "stale",
		err: fmt.Errorf("relay github status: %w", relayclient.ErrBadCredential)})
	nv := next.(githubWizardView)
	if nv.state != wizLogin {
		t.Fatalf("a rejected credential must drop to the login step, got state %d", nv.state)
	}
	out := nv.View()
	if !strings.Contains(out, "sign in with GitHub") || !strings.Contains(out, "https://r.example") {
		t.Fatalf("want the armed login step, got:\n%s", out)
	}
	if cmd := nv.refresh(nil); cmd != nil {
		t.Fatal("the login step must not keep polling the rejected credential")
	}
}

func TestWizardEnterStartsLoginAndShowsCode(t *testing.T) {
	orig := openBrowser
	opened := ""
	openBrowser = func(u string) error { opened = u; return nil }
	defer func() { openBrowser = orig }()

	v := newGithubWizard(relayFor(fakeRelay{handle: "h1", code: "ABCD-1234"}))
	next, _ := v.Update(wizStatusMsg{noCred: true, base: "https://r.example"})
	next, cmd := next.(githubWizardView).Update(keyEnter())
	if cmd == nil {
		t.Fatal("enter on the login step should start the flow")
	}
	started, ok := cmd().(wizLoginStartedMsg)
	if !ok || started.handle != "h1" || started.code != "ABCD-1234" {
		t.Fatalf("want wizLoginStartedMsg{h1, ABCD-1234}, got %#v", cmd())
	}
	if opened != "https://r.example/v1/login/cli" {
		t.Fatalf("browser should open the verify URL, got %q", opened)
	}
	next, _ = next.(githubWizardView).Update(started)
	out := next.(githubWizardView).View()
	if !strings.Contains(out, "ABCD-1234") || !strings.Contains(out, "https://r.example/v1/login/cli") {
		t.Fatalf("polling view must show code + URL, got:\n%s", out)
	}
	if c := next.(githubWizardView).refresh(nil); c == nil {
		t.Fatal("the polling state must poll on the tick")
	}
}

func TestWizardLoginPendingKeepsPolling(t *testing.T) {
	v := newGithubWizard(relayFor(fakeRelay{}))
	v.state, v.base, v.handle = wizLoginPolling, "https://r.example", "h1"
	next, _ := v.Update(wizLoginDoneMsg{pending: true})
	if next.(githubWizardView).state != wizLoginPolling {
		t.Fatal("pending must stay in the polling state")
	}
}

func TestWizardPollLoginSavesConfigAndReportsAccount(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	relay := relayFor(fakeRelay{acc: relayclient.Account{
		AccountCredential: "cred-1", Username: "alice", InstallURL: "https://gh/install",
	}})
	msg := pollLogin(relay, "https://r.example", "h1")
	done, ok := msg.(wizLoginDoneMsg)
	if !ok || done.err != nil || done.pending {
		t.Fatalf("want a success wizLoginDoneMsg, got %#v", msg)
	}
	cc, err := config.LoadClient()
	if err != nil || cc.AccountCredential != "cred-1" || cc.RelayAPI != "https://r.example" {
		t.Fatalf("credential not saved: %+v err=%v", cc, err)
	}
	// the view advances to the install step because login carried install_url
	v := newGithubWizard(relay)
	v.state = wizLoginPolling
	next, _ := v.Update(done)
	nv := next.(githubWizardView)
	if nv.state != wizInstall || !strings.Contains(nv.View(), "https://gh/install") {
		t.Fatalf("want the install step showing the URL, got state %d:\n%s", nv.state, nv.View())
	}
}

func TestWizardPollLoginPendingMapsErrAuthPending(t *testing.T) {
	msg := pollLogin(relayFor(fakeRelay{pollErr: relayclient.ErrAuthPending}), "b", "h")
	if done := msg.(wizLoginDoneMsg); !done.pending || done.err != nil {
		t.Fatalf("ErrAuthPending must map to pending, got %#v", msg)
	}
}

func TestWizardByoOffersManifest(t *testing.T) {
	v := newGithubWizard(relayFor(fakeRelay{}))
	next, _ := v.Update(wizStatusMsg{base: "b", cred: "c", st: relayclient.Status{GitHubApp: false}})
	out := next.(githubWizardView).View()
	if !strings.Contains(out, "doesn't broker") {
		t.Fatalf("want the BYO explanation, got:\n%s", out)
	}
	_, cmd := next.(githubWizardView).Update(keyRunes('m'))
	if cmd == nil {
		t.Fatal("m should push the manifest view")
	}
	push, ok := cmd().(pushMsg)
	if !ok {
		t.Fatalf("want pushMsg, got %T", cmd())
	}
	if _, ok := push.view.(manifestView); !ok {
		t.Fatalf("want manifestView pushed, got %T", push.view)
	}
}

func TestWizardNoInstallPollsUntilInstalled(t *testing.T) {
	v := newGithubWizard(relayFor(fakeRelay{st: relayclient.Status{
		GitHubApp:     true,
		InstallURL:    "https://gh/install",
		Installations: []relayclient.Installation{{ID: "66", TargetType: "org", TargetLogin: "getpiper"}},
	}}))
	next, _ := v.Update(wizStatusMsg{base: "b", cred: "c",
		st: relayclient.Status{GitHubApp: true, InstallURL: "https://gh/install"}})
	nv := next.(githubWizardView)
	if nv.state != wizInstall || !strings.Contains(nv.View(), "https://gh/install") {
		t.Fatalf("want the install step, got state %d", nv.state)
	}
	cmd := nv.refresh(nil)
	if cmd == nil {
		t.Fatal("the install state must poll status on the tick")
	}
	next, _ = nv.Update(cmd()) // fakeRelay now reports one installation
	nv = next.(githubWizardView)
	if nv.state != wizInstalled || !strings.Contains(nv.View(), "getpiper") {
		t.Fatalf("an appearing install must flip to installed, got state %d:\n%s", nv.state, nv.View())
	}
}

func TestWizardInstalledBrowsesRepos(t *testing.T) {
	insts := []relayclient.Installation{
		{ID: "55", TargetType: "user", TargetLogin: "alice"},
		{ID: "66", TargetType: "org", TargetLogin: "getpiper"},
	}
	v := newGithubWizard(relayFor(fakeRelay{}))
	next, _ := v.Update(wizStatusMsg{base: "b", cred: "c",
		st: relayclient.Status{GitHubApp: true, Installations: insts}})
	nv := next.(githubWizardView)
	out := nv.View()
	if !strings.Contains(out, "alice (user)") || !strings.Contains(out, "getpiper (org)") {
		t.Fatalf("installations must list with their target, got:\n%s", out)
	}
	next, _ = nv.Update(keyRunes('j'))
	next, cmd := next.(githubWizardView).Update(keyEnter())
	if cmd == nil {
		t.Fatal("enter should push the repos view")
	}
	push := cmd().(pushMsg)
	sub, ok := push.view.(wizardReposView)
	if !ok || sub.inst.ID != "66" {
		t.Fatalf("want the second installation's repos view, got %#v", push.view)
	}
}

func TestWizardReposViewLoadsOnceAndRenders(t *testing.T) {
	sub := wizardReposView{
		relay: relayFor(fakeRelay{repos: []relayclient.Repo{
			{FullName: "getpiper/piper"}, {FullName: "getpiper/secrets", Visibility: "private"},
		}}),
		base: "b", cred: "c", inst: relayclient.Installation{ID: "66", TargetLogin: "getpiper", TargetType: "org"},
	}
	cmd := sub.refresh(nil)
	if cmd == nil {
		t.Fatal("first refresh must load repos")
	}
	next, _ := sub.Update(cmd())
	sub = next.(wizardReposView)
	out := sub.View()
	if !strings.Contains(out, "getpiper/piper") || !strings.Contains(out, "getpiper/secrets (private)") {
		t.Fatalf("repos must render with visibility badges, got:\n%s", out)
	}
	if sub.refresh(nil) != nil {
		t.Fatal("repos load once — GitHubRepos proxies to GitHub's API, no tick polling")
	}
}

func TestWizardRelayErrorBannersWithoutMarkingBoxDown(t *testing.T) {
	m := NewModel("pi4", "a", false, fakeAPI{}).WithRelay(relayFor(fakeRelay{}))
	next, _ := m.Update(pushMsg{view: newGithubWizard(m.relay)})
	m = next.(Model)
	next, _ = m.Update(wizStatusMsg{err: errors.New("relay 502")})
	m = next.(Model)
	if m.down {
		t.Fatal("a relay error must not render the box unreachable")
	}
	if !strings.Contains(m.top().View(), "relay 502") {
		t.Fatalf("relay error should banner in the wizard, got:\n%s", m.top().View())
	}
}

func TestGKeyOpensWizard(t *testing.T) {
	m := NewModel("pi4", "a", false, fakeAPI{}).WithRelay(relayFor(fakeRelay{}))
	next, cmd := m.Update(keyRunes('g'))
	m = pump(t, next.(Model), cmd)
	if _, ok := m.top().(githubWizardView); !ok {
		t.Fatalf("g should push the github wizard, got %T", m.top())
	}
}

func TestWizardOOpensInstallURLViaCmdNotUpdate(t *testing.T) {
	orig := openBrowser
	opened := ""
	openBrowser = func(u string) error { opened = u; return nil }
	defer func() { openBrowser = orig }()

	v := newGithubWizard(relayFor(fakeRelay{}))
	v.state, v.installURL = wizInstall, "https://gh/install"
	_, cmd := v.Update(keyRunes('o'))
	if cmd == nil {
		t.Fatal("o on the install step should return a cmd to open the browser")
	}
	if opened != "" {
		t.Fatal("o must not open the browser synchronously on the Update path")
	}
	cmd()
	if opened != "https://gh/install" {
		t.Fatalf("the cmd should open the install URL, got %q", opened)
	}
}

func TestWizardReposRetryReArmsAfterError(t *testing.T) {
	sub := wizardReposView{relay: relayFor(fakeRelay{}), loaded: true, err: errors.New("boom")}
	sub = sub.retry()
	if sub.loaded || sub.err != nil {
		t.Fatalf("retry after an error load must clear loaded/err, got loaded=%v err=%v", sub.loaded, sub.err)
	}
	if cmd := sub.refresh(nil); cmd == nil {
		t.Fatal("refresh must fire again once retry has cleared the error state")
	}
}

func TestWizardReposRetryNoopsAfterSuccess(t *testing.T) {
	sub := wizardReposView{
		relay: relayFor(fakeRelay{}), loaded: true,
		repos: []relayclient.Repo{{FullName: "getpiper/piper"}},
	}
	sub = sub.retry()
	if !sub.loaded {
		t.Fatal("retry must not touch a successful load")
	}
	if cmd := sub.refresh(nil); cmd != nil {
		t.Fatal("a successful load stays loaded-once even after retry")
	}
}

func TestWizardReposFooterAdvertisesRetryOnlyOnError(t *testing.T) {
	errored := wizardReposView{loaded: true, err: errors.New("boom")}
	if !strings.Contains(errored.footer(), "r retry") {
		t.Fatalf("footer should advertise retry after an error, got %q", errored.footer())
	}
	ok := wizardReposView{loaded: true, repos: []relayclient.Repo{{FullName: "getpiper/piper"}}}
	if strings.Contains(ok.footer(), "r retry") {
		t.Fatalf("footer should not advertise retry after a successful load, got %q", ok.footer())
	}
}

func TestRKeyRetriesFailedRepoLoadViaRoot(t *testing.T) {
	relay := relayFor(fakeRelay{reposErr: errors.New("boom")})
	sub := wizardReposView{relay: relay, inst: relayclient.Installation{ID: "1"}}
	m := NewModel("pi4", "a", false, fakeAPI{}).WithRelay(relay)
	next, _ := m.Update(pushMsg{view: sub})
	m = next.(Model)
	m = pump(t, m, m.refresh()) // initial load fails
	if !m.top().(wizardReposView).loaded || m.top().(wizardReposView).err == nil {
		t.Fatalf("want a failed load before retrying, got %#v", m.top())
	}
	next, cmd := m.Update(keyRunes('r'))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("r after a failed load should return a refresh cmd")
	}
	if m.top().(wizardReposView).loaded {
		t.Fatal("r should have cleared loaded so the refresh cmd fires again")
	}
	m = pump(t, m, cmd) // re-fires the (still-failing) load — confirms it re-armed
	if m.top().(wizardReposView).err == nil {
		t.Fatal("want the retried load's outcome to land back on the view")
	}
}

func TestRKeyDoesNotDisturbSuccessfulRepoLoad(t *testing.T) {
	relay := relayFor(fakeRelay{repos: []relayclient.Repo{{FullName: "getpiper/piper"}}})
	sub := wizardReposView{relay: relay, inst: relayclient.Installation{ID: "1"}}
	m := NewModel("pi4", "a", false, fakeAPI{}).WithRelay(relay)
	next, _ := m.Update(pushMsg{view: sub})
	m = next.(Model)
	m = pump(t, m, m.refresh()) // initial load succeeds
	if !m.top().(wizardReposView).loaded || m.top().(wizardReposView).err != nil {
		t.Fatalf("want a successful load before pressing r, got %#v", m.top())
	}
	next, _ = m.Update(keyRunes('r'))
	m = next.(Model)
	if !m.top().(wizardReposView).loaded {
		t.Fatal("r must not disturb a successful load — no manual retry needed")
	}
	if cmd := m.top().(wizardReposView).refresh(nil); cmd != nil {
		t.Fatal("a successful load must stay loaded-once even after r")
	}
}

func TestWizardReposRKeyRetriesAfterError(t *testing.T) {
	sub := wizardReposView{relay: relayFor(fakeRelay{}), loaded: true, err: errors.New("boom")}
	next, cmd := sub.Update(keyRunes('r'))
	if cmd == nil {
		t.Fatal("r on a failed repo load must re-fire the fetch")
	}
	if v := next.(wizardReposView); v.loaded || v.err != nil {
		t.Fatalf("r must clear the errored load, got loaded=%v err=%v", v.loaded, v.err)
	}
}

func TestWizardReposRKeyNoopsAfterSuccess(t *testing.T) {
	sub := wizardReposView{
		relay: relayFor(fakeRelay{}), loaded: true,
		repos: []relayclient.Repo{{FullName: "getpiper/piper"}},
	}
	next, cmd := sub.Update(keyRunes('r'))
	if cmd != nil {
		t.Fatal("r must not re-fetch a repo list that loaded fine")
	}
	if !next.(wizardReposView).loaded {
		t.Fatal("r must leave a successful load alone")
	}
}

func TestRKeyReachesRepoPickerFromRoot(t *testing.T) {
	m := NewModel("pi4", "a", false, fakeAPI{}).WithRelay(relayFor(fakeRelay{}))
	next, _ := m.Update(pushMsg{view: wizardReposView{
		relay: relayFor(fakeRelay{}), loaded: true, err: errors.New("boom"),
	}})
	m = next.(Model)
	next, cmd := m.Update(keyRunes('r'))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("r at the root must reach the repo picker's retry")
	}
	if v, ok := m.top().(wizardReposView); !ok || v.loaded {
		t.Fatalf("r should have re-armed the picker, got %#v", m.top())
	}
}
