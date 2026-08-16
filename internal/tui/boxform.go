package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/piperbox/piper/internal/config"
)

// boxFormView adds or edits a box: name, addr, and a masked token. On submit it
// validates locally, then verifies the box is reachable (dial + ListApps) before
// writing the config. Editing preserves the box's wizard-managed relay fields.
type boxFormView struct {
	dial     Dialer
	existing []config.Box // for duplicate-name checks
	orig     config.Box   // the box being edited; zero value for add
	editing  bool
	name     textinput.Model
	addr     textinput.Model
	token    textinput.Model
	focus    int // 0 name, 1 addr, 2 token
	err      error
}

func newBoxForm(dial Dialer, boxes []config.Box) boxFormView {
	name := textinput.New()
	name.Placeholder = "name"
	name.Focus()
	addr := textinput.New()
	addr.Placeholder = "host:8088"
	token := textinput.New()
	token.Placeholder = "token"
	token.EchoMode = textinput.EchoPassword
	return boxFormView{dial: dial, existing: boxes, name: name, addr: addr, token: token}
}

func newBoxFormEdit(dial Dialer, boxes []config.Box, orig config.Box) boxFormView {
	v := newBoxForm(dial, boxes)
	v.orig, v.editing = orig, true
	v.name.SetValue(orig.Name)
	v.addr.SetValue(orig.Addr)
	v.token.SetValue(orig.Token)
	return v
}

func (v boxFormView) Init() tea.Cmd { return nil }

func (v boxFormView) title() string { return "box" }

func (v boxFormView) refresh(API) tea.Cmd { return nil }

func (v boxFormView) capturesText() bool { return true }

func (v *boxFormView) applyFocus() {
	inputs := []*textinput.Model{&v.name, &v.addr, &v.token}
	for i, in := range inputs {
		if i == v.focus {
			in.Focus()
		} else {
			in.Blur()
		}
	}
}

// validate checks the name is present and unique (an edit may keep its own name)
// and the addr is present; it returns the assembled box or an error.
func (v boxFormView) validate() (config.Box, error) {
	name := strings.TrimSpace(v.name.Value())
	if name == "" {
		return config.Box{}, fmt.Errorf("name is required")
	}
	for _, b := range v.existing {
		if b.Name == name && !(v.editing && b.Name == v.orig.Name) {
			return config.Box{}, fmt.Errorf("a box named %q already exists", name)
		}
	}
	addr := strings.TrimSpace(v.addr.Value())
	if addr == "" {
		return config.Box{}, fmt.Errorf("addr is required")
	}
	// Preserve wizard-managed relay fields on edit.
	return config.Box{
		Name:              name,
		Addr:              addr,
		Token:             strings.TrimSpace(v.token.Value()),
		BaseDomain:        v.orig.BaseDomain,
		RelayAPI:          v.orig.RelayAPI,
		AccountCredential: v.orig.AccountCredential,
	}, nil
}

func (v boxFormView) submit() (tea.Model, tea.Cmd) {
	box, err := v.validate()
	if err != nil {
		v.err = err
		return v, nil
	}
	dial, replacing := v.dial, ""
	if v.editing {
		replacing = v.orig.Name
	}
	return v, func() tea.Msg {
		c, _, _, err := dial(box)
		if err == nil {
			_, err = c.ListApps()
		}
		if err != nil {
			return errMsg{err}
		}
		if err := saveBox(box, replacing); err != nil {
			return errMsg{err}
		}
		return boxSavedMsg{box: box, replacing: replacing}
	}
}

func (v boxFormView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case errMsg:
		v.err = msg.err
		return v, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "tab", "down":
			v.focus = (v.focus + 1) % 3
			v.applyFocus()
			return v, nil
		case "up":
			v.focus = (v.focus + 2) % 3
			v.applyFocus()
			return v, nil
		case "enter":
			return v.submit()
		}
	}
	// Any editing keystroke clears a stale validation banner.
	v.err = nil
	var cmd tea.Cmd
	switch v.focus {
	case 0:
		v.name, cmd = v.name.Update(msg)
	case 1:
		v.addr, cmd = v.addr.Update(msg)
	default:
		v.token, cmd = v.token.Update(msg)
	}
	return v, cmd
}

func (v boxFormView) View() string {
	var b strings.Builder
	title := "add box"
	if v.editing {
		title = "edit box"
	}
	fmt.Fprintf(&b, "  %s\n\n", title)
	fmt.Fprintf(&b, "  name   %s\n", v.name.View())
	fmt.Fprintf(&b, "  addr   %s\n", v.addr.View())
	fmt.Fprintf(&b, "  token  %s\n\n", v.token.View())
	if v.err != nil {
		fmt.Fprintf(&b, " ⚠ %v\n\n", v.err)
	}
	b.WriteString("  ↵ verify & save   tab switch   esc cancel")
	return b.String()
}
