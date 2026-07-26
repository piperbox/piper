package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/piperbox/piper/internal/api"
	"github.com/piperbox/piper/internal/store"
)

func fixtureApps() []api.App {
	return []api.App{
		{App: store.App{Name: "blog", Hostname: "blog.piper.localhost"}, Status: "running"},
		{App: store.App{Name: "shop", Hostname: "shop.piper.localhost"}, Status: "building"},
		{App: store.App{Name: "new"}, Status: ""},
	}
}

func TestAppsViewRendersRows(t *testing.T) {
	m, _ := newAppsView(false).Update(appsLoadedMsg{apps: fixtureApps()})
	out := m.View()
	for _, want := range []string{
		"NAME", "STATUS", "URL",
		"blog", "● running", "http://blog.piper.localhost",
		"shop", "◐ building",
		"new", "—",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("view missing %q:\n%s", want, out)
		}
	}
}

func TestAppsViewLoadingAndEmptyStates(t *testing.T) {
	if out := newAppsView(false).View(); !strings.Contains(out, "loading") {
		t.Fatalf("want loading state, got:\n%s", out)
	}
	m, _ := newAppsView(false).Update(appsLoadedMsg{apps: nil})
	if out := m.View(); !strings.Contains(out, "no apps yet") {
		t.Fatalf("want empty state, got:\n%s", out)
	}
}

func TestAppsViewErrorBannerKeepsLastRows(t *testing.T) {
	m, _ := newAppsView(false).Update(appsLoadedMsg{apps: fixtureApps()})
	m, _ = m.Update(errMsg{err: errors.New("connection refused")})
	out := m.View()
	if !strings.Contains(out, "⚠") || !strings.Contains(out, "connection refused") {
		t.Fatalf("want error banner, got:\n%s", out)
	}
	if !strings.Contains(out, "blog") {
		t.Fatalf("stale rows dropped on error:\n%s", out)
	}
	m, _ = m.Update(appsLoadedMsg{apps: fixtureApps()})
	if out := m.View(); strings.Contains(out, "⚠") {
		t.Fatalf("banner must clear on next successful poll:\n%s", out)
	}
}

func TestAppsViewCursorAndEnterPushesDetail(t *testing.T) {
	m, _ := newAppsView(false).Update(appsLoadedMsg{apps: fixtureApps()})
	m, _ = m.Update(keyRunes('j')) // cursor to "shop"
	_, cmd := m.Update(keyEnter())
	if cmd == nil {
		t.Fatal("enter should emit a push command")
	}
	pm, ok := cmd().(pushMsg)
	if !ok {
		t.Fatalf("want pushMsg, got %T", cmd())
	}
	if pm.view.title() != "shop" {
		t.Fatalf("want detail for shop, got title %q", pm.view.title())
	}
}

func TestAppsViewNKeyPushesForm(t *testing.T) {
	m, _ := newAppsView(false).Update(appsLoadedMsg{apps: fixtureApps()})
	_, cmd := m.Update(keyRunes('n'))
	if cmd == nil {
		t.Fatal("n should emit a push command")
	}
	pm, ok := cmd().(pushMsg)
	if !ok {
		t.Fatalf("want pushMsg, got %T", cmd())
	}
	if pm.view.title() != "new app" {
		t.Fatalf("want the new-app form, got title %q", pm.view.title())
	}
}

// a401 is a stand-in for *client.StatusError{Code:401} — the tui must classify
// it without importing internal/client, so a local type satisfying the same
// interface exercises isUnauthorized and the hint bar.
type a401 struct{}

func (a401) Error() string      { return "unauthorized" }
func (a401) Unauthorized() bool { return true }

func TestAppsUnauthorizedLANPointsAtBoxEdit(t *testing.T) {
	m := NewModel("pi4", "addr", false, fakeAPI{err: a401{}})
	m = pump(t, m, m.refresh())
	out := m.View()
	if !strings.Contains(out, "press t") {
		t.Fatalf("expected a hint pointing at the boxes menu, got:\n%s", out)
	}
	if strings.Contains(out, "⚠") {
		t.Fatalf("401 should show the hint, not a raw error banner:\n%s", out)
	}
}

func TestAppsUnauthorizedRelayPointsAtGithubWizard(t *testing.T) {
	m := NewModel("pi4", "addr", true, fakeAPI{err: a401{}})
	m = pump(t, m, m.refresh())
	out := m.View()
	if !strings.Contains(out, "press g") {
		t.Fatalf("expected a hint pointing at the github wizard, got:\n%s", out)
	}
	if strings.Contains(out, "⚠") {
		t.Fatalf("401 should show the hint, not a raw error banner:\n%s", out)
	}
}

func TestAppsNonAuthErrorStillBanners(t *testing.T) {
	m := NewModel("pi4", "addr", false, fakeAPI{err: errors.New("dial tcp: refused")})
	m = pump(t, m, m.refresh())
	out := m.View()
	if strings.Contains(out, "press t to update") {
		t.Fatalf("a transport error must not show the credential hint:\n%s", out)
	}
	if !strings.Contains(out, "⚠") {
		t.Fatalf("expected an error banner, got:\n%s", out)
	}
}

// Login has no standalone view: credential repair lives behind t (LAN token)
// and g (relay), so the footer must not advertise a login key.
func TestAppsFooterDoesNotAdvertiseLogin(t *testing.T) {
	if f := newAppsView(false).footer(); strings.Contains(f, "login") {
		t.Fatalf("footer still advertises login: %q", f)
	}
}
