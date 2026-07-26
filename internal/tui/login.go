package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/piperbox/piper/internal/config"
)

// loginTarget selects the login flow. Only targetLAN exists today: the wizard
// takes a token from `piperd token create` and saves it to the current box.
// Relay login (GitHub device-flow → account credential) is a later phase; it
// slots in as a second target here without changing this view's shape.
type loginTarget int

const targetLAN loginTarget = iota

// loginView authenticates the current box. LAN only for now: enter a token,
// verify it with an authenticated ListApps probe against the current box's
// addr, and — on success — persist it to that box and re-dial the session.
type loginView struct {
	dial   Dialer
	box    string // current box name: display + config lookup
	target loginTarget
	token  textinput.Model
	err    error
}

func newLoginView(dial Dialer, box string) loginView {
	token := textinput.New()
	token.Placeholder = "token"
	token.EchoMode = textinput.EchoPassword
	token.Focus()
	return loginView{dial: dial, box: box, target: targetLAN, token: token}
}

func (v loginView) Init() tea.Cmd { return nil }

func (v loginView) title() string { return "login" }

func (v loginView) refresh(API) tea.Cmd { return nil }

func (v loginView) capturesText() bool { return true }

func (v loginView) footer() string { return "↵ verify & save · esc cancel" }

// submit verifies the token against the current box and, on success, saves it
// and emits boxSavedMsg{replacing: box} — the root re-dials the current box via
// the same path a phase-5 box edit uses.
func (v loginView) submit() (tea.Model, tea.Cmd) {
	token := strings.TrimSpace(v.token.Value())
	if token == "" {
		v.err = fmt.Errorf("token required")
		return v, nil
	}
	dial, name := v.dial, v.box
	return v, func() tea.Msg {
		cf, err := config.LoadClientFile()
		if err != nil {
			return errMsg{err}
		}
		box, ok := currentBox(cf, name)
		if !ok {
			return errMsg{fmt.Errorf("box %q not found in config", name)}
		}
		box.Token = token
		c, _, _, err := dial(box)
		if err == nil {
			_, err = c.ListApps()
		}
		if err != nil {
			return errMsg{err}
		}
		if err := saveBox(box, box.Name); err != nil {
			return errMsg{err}
		}
		return boxSavedMsg{box: box, replacing: box.Name}
	}
}

func (v loginView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case errMsg:
		v.err = msg.err
		return v, nil
	case tea.KeyMsg:
		if msg.String() == "enter" {
			return v.submit()
		}
	}
	v.err = nil // any keystroke clears a stale banner
	var cmd tea.Cmd
	v.token, cmd = v.token.Update(msg)
	return v, cmd
}

func (v loginView) View() string {
	var b strings.Builder
	fmt.Fprintf(&b, "  log in to %s\n\n", v.box)
	fmt.Fprintf(&b, "  token  %s\n\n", v.token.View())
	if v.err != nil {
		fmt.Fprintf(&b, " ⚠ %v\n\n", v.err)
	}
	b.WriteString("  ↵ verify & save   esc cancel")
	return b.String()
}

// currentBox returns the box named name from cf (falling back to cf.Current),
// so login edits the box the user is authenticated against.
func currentBox(cf config.ClientFile, name string) (config.Box, bool) {
	for _, b := range cf.Boxes {
		if b.Name == name {
			return b, true
		}
	}
	for _, b := range cf.Boxes {
		if b.Name == cf.Current {
			return b, true
		}
	}
	return config.Box{}, false
}
