package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/piperbox/piper/internal/api"
)

// appsView is the depth-0 home view: a table of apps.
type appsView struct {
	apps   []api.App
	err    error
	loaded bool
	remote bool
	cursor int
}

func newAppsView(remote bool) appsView { return appsView{remote: remote} }

func (v appsView) Init() tea.Cmd { return nil }

func (v appsView) title() string { return "apps" }

func (v appsView) count() int { return len(v.apps) }

func (v appsView) footer() string {
	return "n new · g github · t boxes · ↵ open · q quit"
}

func (v appsView) refresh(c API) tea.Cmd {
	return func() tea.Msg {
		apps, err := c.ListApps()
		if err != nil {
			return errMsg{err}
		}
		return appsLoadedMsg{apps}
	}
}

func (v appsView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case appsLoadedMsg:
		v.apps, v.err, v.loaded = msg.apps, nil, true
		if v.cursor >= len(v.apps) {
			v.cursor = max(0, len(v.apps)-1)
		}
	case errMsg:
		v.err = msg.err
	case tea.KeyMsg:
		switch msg.String() {
		case "up", "k":
			if v.cursor > 0 {
				v.cursor--
			}
		case "down", "j":
			if v.cursor < len(v.apps)-1 {
				v.cursor++
			}
		case "enter":
			if len(v.apps) > 0 {
				name := v.apps[v.cursor].Name
				return v, func() tea.Msg { return pushMsg{newAppDetailView(name)} }
			}
		case "n":
			return v, func() tea.Msg { return pushMsg{newFormView()} }
		}
	}
	return v, nil
}

func (v appsView) View() string {
	var b strings.Builder
	if v.err != nil {
		if isUnauthorized(v.err) {
			b.WriteString(" " + unauthorizedHint(v.remote) + "\n\n")
		} else {
			fmt.Fprintf(&b, " ⚠ %v\n\n", v.err)
		}
	}
	if !v.loaded {
		b.WriteString(" loading…")
		return b.String()
	}
	if len(v.apps) == 0 {
		b.WriteString(" no apps yet — create one with `piper create <name>`")
		return b.String()
	}
	fmt.Fprintf(&b, "  %-16s %-12s %s\n", "NAME", "STATUS", "URL")
	for i, a := range v.apps {
		cursor := "  "
		if i == v.cursor {
			cursor = "▸ "
		}
		status := strings.TrimSpace(statusIcon(a.Status) + " " + a.Status)
		fmt.Fprintf(&b, "%s%-16s %-12s %s\n", cursor, a.Name, status, appURL(a.Hostname, a.Scheme))
	}
	return b.String()
}
