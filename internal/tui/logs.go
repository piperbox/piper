package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

// chromeHeight is the rows the root spends on header + footer + blank lines +
// status bar around a view; the log viewport takes the rest of the terminal
// height.
const chromeHeight = 8

// logsView is the depth-2 view: one deployment's build log in a scrollable
// viewport. Follow re-fetches the log tail each tick while the deployment is
// still building and auto-stops when it leaves that state (the log stops
// growing). Read-only.
type logsView struct {
	app    string
	id     string
	status string
	follow bool
	loaded bool
	err    error
	logs   string
	vp     viewport.Model
	ready  bool
}

func newLogsView(app, id, status string) logsView {
	return logsView{app: app, id: id, status: status, follow: status == "building"}
}

func (v logsView) Init() tea.Cmd { return nil }

func (v logsView) title() string { return "logs" }

func (v logsView) footer() string { return "f follow · ↑/k ↓/j scroll · esc back" }

// refresh fetches the full log once, then only while following. It also reports
// this deployment's current status so the view can auto-stop follow.
func (v logsView) refresh(c API) tea.Cmd {
	if v.loaded && !v.follow {
		return nil
	}
	app, id := v.app, v.id
	return func() tea.Msg {
		logs, err := c.DeploymentLogs(app, id)
		if err != nil {
			return errMsg{err}
		}
		status := ""
		if deps, err := c.Deployments(app); err == nil {
			for _, d := range deps {
				if d.ID == id {
					status = d.Status
					break
				}
			}
			if status == "" {
				status = "stopped"
			}
		}
		return logsLoadedMsg{logs: logs, status: status}
	}
}

func (v logsView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case logsLoadedMsg:
		v.loaded, v.err = true, nil
		if msg.status != "" {
			v.status = msg.status
		}
		if v.status != "building" {
			v.follow = false // build finished: the log is static
		}
		if len(msg.logs) > len(v.logs) || (len(msg.logs) == len(v.logs) && msg.logs != v.logs) {
			v.logs = msg.logs
			if v.ready {
				v.vp.SetContent(v.logs)
				v.vp.GotoBottom()
			}
		}
	case errMsg:
		v.err = msg.err
	case tea.KeyMsg:
		if msg.String() == "f" {
			v.follow = !v.follow
			return v, nil
		}
	case tea.WindowSizeMsg:
		h := msg.Height - chromeHeight
		if h < 1 {
			h = 1
		}
		if !v.ready {
			v.vp = viewport.New(msg.Width, h)
			v.ready = true
		} else {
			v.vp.Width, v.vp.Height = msg.Width, h
		}
		v.vp.SetContent(v.logs)
		if v.follow {
			v.vp.GotoBottom()
		}
	}
	if v.ready {
		var cmd tea.Cmd
		v.vp, cmd = v.vp.Update(msg)
		return v, cmd
	}
	return v, nil
}

func (v logsView) View() string {
	var b strings.Builder
	tag := ""
	if v.follow {
		tag = " · following…"
	}
	fmt.Fprintf(&b, "  %s · %s%s\n\n", v.app, shortID(v.id), tag)
	if v.err != nil {
		fmt.Fprintf(&b, " ⚠ %v\n\n", v.err)
	}
	if !v.loaded {
		b.WriteString(" loading…")
		return b.String()
	}
	if v.ready {
		b.WriteString(v.vp.View())
	} else {
		b.WriteString(v.logs)
	}
	return b.String()
}
