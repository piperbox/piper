package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// domainFormView attaches a custom domain to an app: one field, the domain.
// On submit it emits addDomainMsg; the root runs AddAppDomain and replaces the
// form with the domain detail view on success. Validation beyond non-empty
// stays server-side.
type domainFormView struct {
	app   string
	input textinput.Model
	err   error
}

func newDomainForm(app string) domainFormView {
	in := textinput.New()
	in.Placeholder = "blog.example.com"
	in.Focus()
	return domainFormView{app: app, input: in}
}

func (v domainFormView) Init() tea.Cmd { return nil }

func (v domainFormView) title() string { return "add domain" }

func (v domainFormView) refresh(API) tea.Cmd { return nil }

func (v domainFormView) capturesText() bool { return true }

func (v domainFormView) footer() string { return "↵ add · esc cancel" }

func (v domainFormView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case errMsg:
		v.err = msg.err
		return v, nil
	case tea.KeyMsg:
		if msg.Type == tea.KeyEnter {
			dom := strings.TrimSpace(v.input.Value())
			if dom == "" {
				v.err = fmt.Errorf("domain is required")
				return v, nil
			}
			app := v.app
			return v, func() tea.Msg { return addDomainMsg{app: app, domain: dom} }
		}
	}
	v.err = nil // any keystroke clears a stale banner
	var cmd tea.Cmd
	v.input, cmd = v.input.Update(msg)
	return v, cmd
}

func (v domainFormView) View() string {
	var b strings.Builder
	fmt.Fprintf(&b, "  attach a domain to %s\n\n", v.app)
	fmt.Fprintf(&b, "  domain  %s\n\n", v.input.View())
	if v.err != nil {
		fmt.Fprintf(&b, " ⚠ %v\n\n", v.err)
	}
	b.WriteString("  ↵ add   esc cancel")
	return b.String()
}
