package tui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/piperbox/piper/internal/api"
	"github.com/piperbox/piper/internal/domain"
	"github.com/piperbox/piper/internal/store"
)

// apiCalls records the mutating calls a test drives, so assertions can check
// the TUI passed the right arguments through to the client.
type apiCalls struct {
	createName     string
	createPort     int
	deployName     string
	deployDir      string
	deployRepoName string
	stopped        string
	started        string
	deleted        string
	linkName       string
	linkRepo       string
	linkBranch     string
	linkRootDir    string

	addedApp      string
	addedDomain   string
	removedApp    string
	removedDomain string
}

type fakeAPI struct {
	apps []api.App
	err  error
	app  api.App
	deps []store.Deployment
	logs string

	rec       *apiCalls
	deployDep store.Deployment
	createErr error
	deployErr error
	stopErr   error
	startErr  error
	deleteErr error
	linkErr   error

	manifest    string
	manifestErr error
	exchangeErr error

	domains   []domain.AppDomainStatus
	addSt     domain.AppDomainStatus
	addErr    error
	removeErr error
}

func (f fakeAPI) ListApps() ([]api.App, error)                   { return f.apps, f.err }
func (f fakeAPI) App(string) (api.App, error)                    { return f.app, f.err }
func (f fakeAPI) Deployments(string) ([]store.Deployment, error) { return f.deps, f.err }
func (f fakeAPI) DeploymentLogs(string, string) (string, error)  { return f.logs, f.err }

func (f fakeAPI) CreateApp(name string, port int) error {
	if f.rec != nil {
		f.rec.createName, f.rec.createPort = name, port
	}
	return f.createErr
}

func (f fakeAPI) Deploy(name, srcDir string) (store.Deployment, error) {
	if f.rec != nil {
		f.rec.deployName, f.rec.deployDir = name, srcDir
	}
	return f.deployDep, f.deployErr
}

func (f fakeAPI) DeployFromRepo(name string) (store.Deployment, error) {
	if f.rec != nil {
		f.rec.deployRepoName = name
	}
	return f.deployDep, f.deployErr
}

func (f fakeAPI) StopApp(name string) error {
	if f.rec != nil {
		f.rec.stopped = name
	}
	return f.stopErr
}

func (f fakeAPI) StartApp(name string) error {
	if f.rec != nil {
		f.rec.started = name
	}
	return f.startErr
}

func (f fakeAPI) DeleteApp(name string) error {
	if f.rec != nil {
		f.rec.deleted = name
	}
	return f.deleteErr
}

func (f fakeAPI) LinkApp(name, repo, branch, rootDir string) error {
	if f.rec != nil {
		f.rec.linkName, f.rec.linkRepo, f.rec.linkBranch, f.rec.linkRootDir = name, repo, branch, rootDir
	}
	return f.linkErr
}

func (f fakeAPI) Manifest(string) (string, error)       { return f.manifest, f.manifestErr }
func (f fakeAPI) ExchangeGitHub(string) (string, error) { return "", f.exchangeErr }

func (f fakeAPI) AppDomains(string) ([]domain.AppDomainStatus, error) { return f.domains, f.err }

func (f fakeAPI) AddAppDomain(app, dom string) (domain.AppDomainStatus, error) {
	if f.rec != nil {
		f.rec.addedApp, f.rec.addedDomain = app, dom
	}
	return f.addSt, f.addErr
}

func (f fakeAPI) RemoveAppDomain(app, dom string) error {
	if f.rec != nil {
		f.rec.removedApp, f.rec.removedDomain = app, dom
	}
	return f.removeErr
}

func keyRunes(r rune) tea.KeyMsg { return tea.KeyMsg(tea.Key{Type: tea.KeyRunes, Runes: []rune{r}}) }

func keyEsc() tea.KeyMsg       { return tea.KeyMsg(tea.Key{Type: tea.KeyEsc}) }
func keyEnter() tea.KeyMsg     { return tea.KeyMsg(tea.Key{Type: tea.KeyEnter}) }
func keyBackspace() tea.KeyMsg { return tea.KeyMsg(tea.Key{Type: tea.KeyBackspace}) }
func keyTab() tea.KeyMsg       { return tea.KeyMsg(tea.Key{Type: tea.KeyTab}) }

// pump runs the poll cmd and feeds its message back, like the tea runtime.
func pump(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a command")
	}
	next, _ := m.Update(cmd())
	return next.(Model)
}

func TestModelPollSuccessUpdatesStatusBar(t *testing.T) {
	f := fakeAPI{apps: []api.App{{App: store.App{Name: "blog", Hostname: "blog.piper.localhost"}, Status: "running"}}}
	m := NewModel("pi4", "http://192.168.1.6:8088", false, f)
	m = pump(t, m, m.refresh())
	out := m.View()
	for _, want := range []string{"● pi4", "http://192.168.1.6:8088", "1 app", "blog", "piper", "apps"} {
		if !strings.Contains(out, want) {
			t.Fatalf("view missing %q:\n%s", want, out)
		}
	}
}

func TestModelPollFailureShowsUnreachable(t *testing.T) {
	m := NewModel("pi4", "http://192.168.1.6:8088", false, fakeAPI{err: errors.New("dial tcp: refused")})
	m = pump(t, m, m.refresh())
	out := m.View()
	if !strings.Contains(out, "○ pi4") || !strings.Contains(out, "unreachable") {
		t.Fatalf("want unreachable bar, got:\n%s", out)
	}
}

func TestModelUnauthorizedShowsAuthStateNotUnreachable(t *testing.T) {
	m := NewModel("default", "http://127.0.0.1:8088", false, fakeAPI{err: a401{}})
	m = pump(t, m, m.refresh())
	out := m.View()
	if strings.Contains(out, "unreachable") {
		t.Fatalf("a 401 must not render as unreachable:\n%s", out)
	}
	if !strings.Contains(out, "unauthorized") || !strings.Contains(out, "log in") {
		t.Fatalf("want an auth state pointing at login, got:\n%s", out)
	}
	// authenticating clears it: a later successful poll returns to ●.
	m.client = fakeAPI{apps: nil}
	m = pump(t, m, m.refresh())
	if out := m.View(); !strings.Contains(out, "● default") || strings.Contains(out, "unauthorized") {
		t.Fatalf("auth state did not clear after a good poll:\n%s", out)
	}
}

func TestModelRecoversAfterFailure(t *testing.T) {
	m := NewModel("pi4", "addr", false, fakeAPI{err: errors.New("down")})
	m = pump(t, m, m.refresh())
	m.client = fakeAPI{apps: nil}
	m = pump(t, m, m.refresh())
	if out := m.View(); !strings.Contains(out, "● pi4") {
		t.Fatalf("bar did not recover:\n%s", out)
	}
}

func TestModelTickReschedulesAndRefreshes(t *testing.T) {
	m := NewModel("pi4", "addr", false, fakeAPI{})
	if _, cmd := m.Update(tickMsg{}); cmd == nil {
		t.Fatal("tick must return refresh+tick batch")
	}
}

func TestModelQuitKeys(t *testing.T) {
	m := NewModel("pi4", "addr", false, fakeAPI{})
	keys := []tea.KeyMsg{
		tea.KeyMsg(tea.Key{Type: tea.KeyRunes, Runes: []rune{'q'}}), // q at root quits
		tea.KeyMsg(tea.Key{Type: tea.KeyCtrlC}),                     // ctrl+c quits anywhere
	}
	for _, key := range keys {
		_, cmd := m.Update(key)
		if cmd == nil {
			t.Fatalf("%s: expected quit cmd", key)
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Fatalf("%s: expected tea.QuitMsg, got %T", key, cmd())
		}
	}
}

func TestModelBreadcrumbAndRelayBar(t *testing.T) {
	f := fakeAPI{apps: []api.App{{App: store.App{Name: "blog", Hostname: "blog.example.dev"}, Status: "running"}}}
	m := NewModel("pi4", "pi4.example.dev", true, f)
	m = pump(t, m, m.refresh())
	out := m.View()
	if !strings.Contains(out, "piper") || !strings.Contains(out, "apps") {
		t.Fatalf("breadcrumb missing:\n%s", out)
	}
	if !strings.Contains(out, "pi4.example.dev") {
		t.Fatalf("relay base domain missing from bar:\n%s", out)
	}
	if !strings.Contains(out, "https://blog.example.dev") {
		t.Fatalf("relay app should render https:\n%s", out)
	}
}

func TestRootRunsCreateStopDeleteIntents(t *testing.T) {
	rec := &apiCalls{}
	m := NewModel("b", "a", false, fakeAPI{rec: rec})

	_, cmd := m.Update(createAppMsg{name: "blog", port: 9000})
	res, ok := cmd().(actionResultMsg)
	if !ok || res.err != nil || res.popLevels != 1 {
		t.Fatalf("create: want actionResultMsg{nil,1}, got %#v (ok=%v)", cmd(), ok)
	}
	if rec.createName != "blog" || rec.createPort != 9000 {
		t.Fatalf("create not passed through: %+v", rec)
	}

	_, cmd = m.Update(stopAppMsg{name: "blog"})
	if res := cmd().(actionResultMsg); res.popLevels != 1 || rec.stopped != "blog" {
		t.Fatalf("stop: got %#v rec=%+v", res, rec)
	}

	_, cmd = m.Update(startAppMsg{name: "blog"})
	if res := cmd().(actionResultMsg); res.popLevels != 1 || rec.started != "blog" {
		t.Fatalf("start: got %#v rec=%+v", res, rec)
	}

	_, cmd = m.Update(deleteAppMsg{name: "blog"})
	if res := cmd().(actionResultMsg); res.popLevels != 2 || rec.deleted != "blog" {
		t.Fatalf("delete: want popLevels 2, got %#v rec=%+v", res, rec)
	}
}

func TestRootActionResultSuccessPopsAndErrorBanners(t *testing.T) {
	m := NewModel("b", "a", false, fakeAPI{})
	// stack: apps -> detail -> detail (simulate depth 3)
	m2, _ := m.Update(pushMsg{newAppDetailView("blog", false)})
	m = m2.(Model)
	m2, _ = m.Update(pushMsg{newAppDetailView("blog", false)})
	m = m2.(Model)
	if len(m.stack) != 3 {
		t.Fatalf("setup: want depth 3, got %d", len(m.stack))
	}

	// success pops popLevels and does not exceed the root
	m2, _ = m.Update(actionResultMsg{err: nil, popLevels: 2})
	m = m2.(Model)
	if len(m.stack) != 1 {
		t.Fatalf("success: want popped to root (1), got %d", len(m.stack))
	}

	// error keeps the stack and banners the top view
	m2, _ = m.Update(pushMsg{newAppDetailView("blog", false)})
	m = m2.(Model)
	m2, _ = m.Update(actionResultMsg{err: errors.New("name taken")})
	m = m2.(Model)
	if len(m.stack) != 2 {
		t.Fatalf("error must not pop: got depth %d", len(m.stack))
	}
	if !strings.Contains(m.View(), "name taken") {
		t.Fatalf("error banner missing:\n%s", m.View())
	}
}

func TestRootPopMsg(t *testing.T) {
	m := NewModel("b", "a", false, fakeAPI{})
	m2, _ := m.Update(pushMsg{newAppDetailView("blog", false)})
	m = m2.(Model)
	m2, _ = m.Update(popMsg{1})
	m = m2.(Model)
	if len(m.stack) != 1 {
		t.Fatalf("popMsg{1} should return to root, got depth %d", len(m.stack))
	}
	// popN never removes the root
	m2, _ = m.Update(popMsg{5})
	m = m2.(Model)
	if len(m.stack) != 1 {
		t.Fatalf("popMsg over-pop must keep root, got depth %d", len(m.stack))
	}
}

func TestNavViewsRenderFooterLegend(t *testing.T) {
	// apps list (root) footer
	f := fakeAPI{apps: []api.App{{App: store.App{Name: "blog"}, Status: "running"}}}
	m := NewModel("pi4", "addr", false, f)
	m = pump(t, m, m.refresh())
	if out := m.View(); !strings.Contains(out, "n new") || !strings.Contains(out, "q quit") {
		t.Fatalf("apps-list footer missing keys:\n%s", out)
	}

	// app detail footer
	m2, _ := m.Update(pushMsg{newAppDetailView("blog", false)})
	m = m2.(Model)
	m = pump(t, m, m.refresh())
	out := m.View()
	for _, want := range []string{"d deploy", "s stop", "x delete", "esc back"} {
		if !strings.Contains(out, want) {
			t.Fatalf("app-detail footer missing %q:\n%s", want, out)
		}
	}
}

// Every view carries its own key legend, so there is no help overlay to point
// at: "?" must stay off the global keymap and out of the footers.
func TestNoHelpOverlay(t *testing.T) {
	f := fakeAPI{apps: []api.App{{App: store.App{Name: "blog"}, Status: "running"}}}
	m := NewModel("pi4", "addr", false, f)
	m = pump(t, m, m.refresh())

	_, cmd := m.Update(keyRunes('?'))
	if cmd != nil {
		if _, ok := cmd().(pushMsg); ok {
			t.Fatal("? must not push a help overlay")
		}
	}
	if len(m.stack) != 1 {
		t.Fatalf("? must not change the stack, got depth %d", len(m.stack))
	}

	for _, v := range []view{
		m.top(),
		newAppDetailView("blog", false),
		newBoxesView(nil),
		newLogsView("blog", "dep-1", "building"),
		newDomainForm("blog"),
		newDomainDetailView("blog", fixtureDomains()[0]),
		newLoginView(nil, "pi4"),
		newGithubWizard(nil),
	} {
		fv, ok := v.(footered)
		if !ok {
			t.Fatalf("%T should offer a key legend", v)
		}
		if strings.Contains(fv.footer(), "? help") {
			t.Fatalf("%T footer still advertises the help overlay: %q", v, fv.footer())
		}
	}
}

func TestModalViewsRenderNoFooterLegend(t *testing.T) {
	m := NewModel("pi4", "addr", false, fakeAPI{})
	// push the new-app form (a text-capturing modal, not footered)
	m2, _ := m.Update(pushMsg{newFormView()})
	m = m2.(Model)
	if got := m.topFooter(); got != "" {
		t.Fatalf("modal must have no footer, got %q", got)
	}
}

func TestRootRemoveDomainCallsClientAndPops(t *testing.T) {
	rec := &apiCalls{}
	m := NewModel("b", "a", false, fakeAPI{rec: rec})
	m.stack = append(m.stack, newAppDetailView("blog", false), newRemoveDomainConfirm("blog", "blog.example.com"))
	_, cmd := m.Update(removeDomainMsg{app: "blog", domain: "blog.example.com"})
	res, ok := cmd().(actionResultMsg)
	if !ok || res.err != nil || res.popLevels != 1 {
		t.Fatalf("want actionResultMsg{nil, 1}, got %#v", cmd())
	}
	if rec.removedApp != "blog" || rec.removedDomain != "blog.example.com" {
		t.Fatalf("client not called with app+domain: %#v", rec)
	}
}

func TestRootAddDomainCallsClient(t *testing.T) {
	rec := &apiCalls{}
	m := NewModel("b", "a", false, fakeAPI{rec: rec, addSt: fixtureDomains()[0]})
	_, cmd := m.Update(addDomainMsg{app: "blog", domain: "blog.example.com"})
	added, ok := cmd().(domainAddedMsg)
	if !ok || added.err != nil || added.st.Domain != "blog.example.com" {
		t.Fatalf("want domainAddedMsg with status, got %#v", cmd())
	}
	if rec.addedApp != "blog" || rec.addedDomain != "blog.example.com" {
		t.Fatalf("client not called with app+domain: %#v", rec)
	}
}

func TestRootDomainAddedReplacesFormWithDetail(t *testing.T) {
	m := NewModel("b", "a", false, fakeAPI{})
	m.stack = append(m.stack, newAppDetailView("blog", false), newDomainForm("blog"))
	next, _ := m.Update(domainAddedMsg{app: "blog", st: fixtureDomains()[0]})
	nm := next.(Model)
	if nm.top().title() != "domain" {
		t.Fatalf("want the domain detail view on top, got %q", nm.top().title())
	}
	if !strings.Contains(nm.top().View(), "CNAME") {
		t.Fatalf("detail should show the CNAME:\n%s", nm.top().View())
	}
}

func TestRootDomainAddedErrorBannersForm(t *testing.T) {
	m := NewModel("b", "a", false, fakeAPI{})
	m.stack = append(m.stack, newAppDetailView("blog", false), newDomainForm("blog"))
	next, _ := m.Update(domainAddedMsg{app: "blog", err: errors.New("invalid domain")})
	nm := next.(Model)
	if nm.top().title() != "add domain" || !strings.Contains(nm.top().View(), "invalid domain") {
		t.Fatalf("want bannered form, got %q:\n%s", nm.top().title(), nm.top().View())
	}
}

func TestWithRelayAttachesFactory(t *testing.T) {
	m := NewModel("pi4", "a", false, fakeAPI{}).WithRelay(func(string) RelayAPI { return nil })
	if m.relay == nil {
		t.Fatal("WithRelay should attach the relay dialer")
	}
}

func TestRKeyIsNotAGlobalRefresh(t *testing.T) {
	f := fakeAPI{apps: []api.App{{App: store.App{Name: "blog"}, Status: "running"}}}
	m := NewModel("pi4", "addr", false, f)
	m = pump(t, m, m.refresh())
	_, cmd := m.Update(keyRunes('r'))
	if cmd != nil {
		t.Fatal("r must not force a poll — the tick already refreshes every 2s")
	}
}

func TestNoLegendAdvertisesRefreshKey(t *testing.T) {
	withDomain := appDetailView{name: "blog", domains: []domain.AppDomainStatus{{Domain: "blog.example.com"}}}
	legends := map[string]string{
		"apps":          appsView{}.footer(),
		"app detail":    newAppDetailView("blog", false).footer(),
		"domain row":    withDomain.footer(),
		"domain detail": domainDetailView{}.footer(),
		"logs":          newLogsView("blog", "dep-1", "building").footer(),
	}
	for where, legend := range legends {
		if strings.Contains(legend, "refresh") {
			t.Fatalf("%s legend still advertises refresh: %q", where, legend)
		}
	}
}
