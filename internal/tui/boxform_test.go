package tui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/piperbox/piper/internal/config"
)

// submitForm fills the fields and returns the cmd produced by Enter.
func submitForm(t *testing.T, v boxFormView, name, addr, token string) (boxFormView, tea.Cmd) {
	t.Helper()
	v.name.SetValue(name)
	v.addr.SetValue(addr)
	v.token.SetValue(token)
	m, cmd := v.Update(keyEnter())
	if cmd == nil {
		return m.(boxFormView), nil
	}
	return m.(boxFormView), cmd
}

func TestBoxFormValidSubmitVerifiesThenSaves(t *testing.T) {
	seedConfig(t, config.ClientFile{Boxes: []config.Box{{Name: "pi4", Addr: "a"}}, Current: "pi4"})
	v := newBoxForm(fakeDialer(fakeAPI{}, "", false, nil), []config.Box{{Name: "pi4", Addr: "a"}})
	_, cmd := submitForm(t, v, "blog", "192.168.1.9:8088", "tok")
	if cmd == nil {
		t.Fatal("valid submit should emit a verify+save cmd")
	}
	if _, ok := cmd().(boxSavedMsg); !ok {
		t.Fatalf("want boxSavedMsg on success, got %T", cmd())
	}
	cf, err := config.LoadClientFile()
	if err != nil {
		t.Fatal(err)
	}
	if len(cf.Boxes) != 2 || cf.Boxes[1].Name != "blog" || cf.Boxes[1].Addr != "192.168.1.9:8088" {
		t.Fatalf("box not saved: %+v", cf.Boxes)
	}
}

func TestBoxFormBadTokenBannersNoWrite(t *testing.T) {
	seedConfig(t, config.ClientFile{Boxes: []config.Box{{Name: "pi4", Addr: "a"}}, Current: "pi4"})
	v := newBoxForm(fakeDialer(fakeAPI{err: errors.New("401 unauthorized")}, "", false, nil), []config.Box{{Name: "pi4", Addr: "a"}})
	_, cmd := submitForm(t, v, "blog", "x", "bad")
	msg := cmd()
	if _, ok := msg.(errMsg); !ok {
		t.Fatalf("want errMsg on failed probe, got %T", msg)
	}
	cf, _ := config.LoadClientFile()
	if len(cf.Boxes) != 1 {
		t.Fatalf("failed probe must not write: %+v", cf.Boxes)
	}
}

func TestBoxFormRejectsEmptyAndDuplicateName(t *testing.T) {
	boxes := []config.Box{{Name: "pi4", Addr: "a"}}
	// empty name
	v := newBoxForm(fakeDialer(fakeAPI{}, "", false, nil), boxes)
	vv, cmd := submitForm(t, v, "", "x", "")
	if cmd != nil {
		t.Fatal("empty name must not submit")
	}
	if !strings.Contains(vv.View(), "name") {
		t.Fatalf("expected a name validation banner:\n%s", vv.View())
	}
	// duplicate name
	v = newBoxForm(fakeDialer(fakeAPI{}, "", false, nil), boxes)
	vv, cmd = submitForm(t, v, "pi4", "x", "")
	if cmd != nil {
		t.Fatal("duplicate name must not submit")
	}
	if !strings.Contains(vv.View(), "exists") {
		t.Fatalf("expected a duplicate-name banner:\n%s", vv.View())
	}
}

func TestBoxFormEditPreservesRelayFields(t *testing.T) {
	orig := boxWithBaseDomain(t, config.Box{Name: "cloud", Addr: "old", Token: "t1", RelayAPI: "https://relay.example", AccountCredential: "cred"}, "cloud.example")
	seedConfig(t, config.ClientFile{Boxes: []config.Box{orig}, Current: "cloud"})
	v := newBoxFormEdit(fakeDialer(fakeAPI{}, "", false, nil), []config.Box{orig}, orig)
	_, cmd := submitForm(t, v, "cloud", "new-addr", "t2")
	if _, ok := cmd().(boxSavedMsg); !ok {
		t.Fatalf("edit should save, got %T", cmd())
	}
	cf, _ := config.LoadClientFile()
	got := cf.Boxes[0]
	if got.Addr != "new-addr" || got.Token != "t2" {
		t.Fatalf("edit did not update addr/token: %+v", got)
	}
	if got.RelayAPI != "https://relay.example" || got.AccountCredential != "cred" {
		t.Fatalf("edit dropped relay fields: %+v", got)
	}
	if got := persistedBaseDomain(t, got); got != "cloud.example" {
		t.Fatalf("edit dropped relay identity: %q", got)
	}
}

func TestBoxesKeyOpensForms(t *testing.T) {
	v := newBoxesView(fakeDialer(fakeAPI{}, "", false, nil))
	vv, _ := v.Update(boxesLoadedMsg{boxes: []config.Box{{Name: "pi4", Addr: "a"}}, current: "pi4", viewID: v.viewID, requestID: 1})
	v = vv.(boxesView)

	_, cmd := v.Update(keyRunes('a'))
	if push, ok := cmd().(pushMsg); !ok {
		t.Fatalf("a should push a form, got %T", cmd())
	} else if _, ok := push.view.(boxFormView); !ok {
		t.Fatalf("a should push boxFormView, got %T", push.view)
	}

	_, cmd = v.Update(keyRunes('e'))
	if push, ok := cmd().(pushMsg); !ok {
		t.Fatalf("e should push a form, got %T", cmd())
	} else if _, ok := push.view.(boxFormView); !ok {
		t.Fatalf("e should push boxFormView, got %T", push.view)
	}
}

func TestRootCurrentBoxRenameRedials(t *testing.T) {
	m := NewModel("pi4", "192.168.1.6:8088", false, fakeAPI{}).WithDialer(fakeDialer(fakeAPI{}, "192.168.1.9:8088", false, nil))
	m2, _ := m.Update(boxSavedMsg{box: config.Box{Name: "pi4-new", Addr: "192.168.1.9:8088"}, replacing: "pi4"})
	m = m2.(Model)
	if m.box != "pi4-new" || m.addr != "192.168.1.9:8088" {
		t.Fatalf("renaming the current box should re-dial: got box=%q addr=%q", m.box, m.addr)
	}
}

func TestRootBoxSavedPopsToBoxes(t *testing.T) {
	m := NewModel("pi4", "a", false, fakeAPI{}).WithDialer(fakeDialer(fakeAPI{}, "", false, nil))
	m2, _ := m.Update(pushMsg{newBoxesView(m.dial)})
	m = m2.(Model)
	m2, _ = m.Update(pushMsg{newBoxForm(m.dial, nil)})
	m = m2.(Model)
	if len(m.stack) != 3 {
		t.Fatalf("setup: want depth 3, got %d", len(m.stack))
	}
	// saving a non-current box pops back to the boxes view
	m2, _ = m.Update(boxSavedMsg{box: config.Box{Name: "blog"}})
	m = m2.(Model)
	if len(m.stack) != 2 {
		t.Fatalf("box saved should pop to boxes view (depth 2), got %d", len(m.stack))
	}
}
