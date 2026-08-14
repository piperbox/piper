package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/piperbox/piper/internal/store"
)

const pollInterval = 2 * time.Second

// Model is the root of the TUI: it owns the view stack, the poll tick, the
// active box's client, and the status bar. The top view handles its own
// messages; the root intercepts global keys, navigation, and connectivity.
type Model struct {
	box    string
	addr   string
	remote bool
	client API
	dial   Dialer
	relay  RelayDialer

	stack         []view
	loaded        bool // at least one successful poll
	down          bool // last poll failed (dial/timeout)
	unauthorized  bool // last poll was rejected 401 — reachable, needs a token
	width, height int

	githubCancel context.CancelFunc // cancels the in-flight github manifest flow on leave
}

func NewModel(box, addr string, remote bool, c API) Model {
	return Model{box: box, addr: addr, remote: remote, client: c, stack: []view{newAppsView(remote)}}
}

// WithDialer attaches the box-switch client factory and returns the model for
// chaining. Kept separate from NewModel so existing call sites (and tests that
// never switch boxes) stay four-argument.
func (m Model) WithDialer(d Dialer) Model { m.dial = d; return m }

// WithRelay attaches the relay client factory used by the github wizard and
// the link form's repo picker. Kept separate from NewModel so existing call
// sites and tests stay four-argument.
func (m Model) WithRelay(r RelayDialer) Model { m.relay = r; return m }

// Run starts the interactive TUI against c, identified as box/addr in the
// status bar. remote marks a relay-backed box (HTTPS URLs). dial builds clients
// for the box switcher; relay builds relay clients for the github wizard and
// repo picker. It blocks until quit.
func Run(box, addr string, remote bool, c API, dial Dialer, relay RelayDialer) error {
	m := NewModel(box, addr, remote, c).WithDialer(dial).WithRelay(relay)
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}

func (m Model) Init() tea.Cmd { return tea.Batch(m.refresh(), tick()) }

func tick() tea.Cmd {
	return tea.Tick(pollInterval, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m Model) top() view { return m.stack[len(m.stack)-1] }

// refresh polls the top view's data off the UI thread.
func (m Model) refresh() tea.Cmd { return m.top().refresh(m.client) }

// topCapturesText reports whether the top view wants raw keystrokes (a text
// field), so the root suppresses its single-letter shortcuts (q, t, …) for it.
func (m Model) topCapturesText() bool {
	if c, ok := m.top().(interface{ capturesText() bool }); ok {
		return c.capturesText()
	}
	return false
}

// footered is a view that offers a one-line key legend. The root renders it dim
// between the body and the status bar; views without it render no footer.
type footered interface{ footer() string }

var footerStyle = lipgloss.NewStyle().Faint(true)

// topFooter returns the dim key legend for the top view, or "" if it offers none.
func (m Model) topFooter() string {
	f, ok := m.top().(footered)
	if !ok {
		return ""
	}
	return footerStyle.Render(" " + f.footer())
}

// popN drops n views off the top of the stack without ever removing the root.
func (m Model) popN(n int) Model {
	if n > len(m.stack)-1 {
		n = len(m.stack) - 1
	}
	m.stack = m.stack[:len(m.stack)-n]
	if boxes, ok := m.top().(boxesView); ok {
		// A relay result can arrive while a child view is on top. Re-arm the
		// visible boxes view so its next reload fetches a fresh snapshot.
		boxes.relayFetchStarted = false
		boxes.relayRequestID = 0
		boxes.relayRetryCount = 0
		m.stack[len(m.stack)-1] = boxes
	}
	return m
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc":
			if len(m.stack) > 1 {
				if _, ok := m.top().(manifestView); ok && m.githubCancel != nil {
					m.githubCancel()
					m.githubCancel = nil
				}
				m = m.popN(1)
				return m, m.refresh()
			}
			return m, nil
		}
		if !m.topCapturesText() {
			switch msg.String() {
			case "q":
				if len(m.stack) == 1 {
					return m, tea.Quit
				}
				m = m.popN(1)
				return m, m.refresh()
			case "t":
				if _, ok := m.top().(boxesView); !ok {
					return m, func() tea.Msg {
						boxes := newBoxesView(m.dial)
						boxes.relay = m.relay
						return pushMsg{boxes}
					}
				}
				return m, nil
			case "g":
				if _, ok := m.top().(githubWizardView); !ok {
					return m, func() tea.Msg { return pushMsg{newGithubWizard(m.relay)} }
				}
				return m, nil
			}
		}
	case tickMsg:
		return m, tea.Batch(m.refresh(), tick())
	case pushMsg:
		if lf, ok := msg.view.(linkFormView); ok && lf.relay == nil {
			lf.relay = m.relay // the pushing view doesn't hold the factory; the root does
			msg.view = lf
		}
		m.stack = append(m.stack, msg.view)
		if m.width > 0 {
			seeded, _ := m.top().Update(tea.WindowSizeMsg{Width: m.width, Height: m.height})
			m.stack[len(m.stack)-1] = seeded.(view)
		}
		return m, m.refresh()
	case switchBoxMsg:
		c, addr, remote, err := m.dial(msg.box)
		if err != nil {
			next, _ := m.top().Update(errMsg{err})
			m.stack[len(m.stack)-1] = next.(view)
			return m, nil
		}
		m.client, m.box, m.addr, m.remote = c, msg.box.Name, addr, remote
		m.loaded, m.down, m.unauthorized = false, false, false
		m.stack = []view{newAppsView(remote)}
		if m.width > 0 {
			seeded, _ := m.top().Update(tea.WindowSizeMsg{Width: m.width, Height: m.height})
			m.stack[len(m.stack)-1] = seeded.(view)
		}
		return m, m.refresh()
	case boxSavedMsg:
		if msg.box.Name == m.box || msg.replacing == m.box {
			return m.Update(switchBoxMsg{box: msg.box}) // current box changed: re-dial
		}
		m = m.popN(1)
		return m, m.refresh()
	case removeBoxMsg:
		name := msg.name
		return m, func() tea.Msg {
			current, changed, err := removeBox(name)
			if err != nil {
				return errMsg{err}
			}
			return boxRemovedMsg{current: current, changed: changed}
		}
	case boxRemovedMsg:
		if msg.changed {
			return m.Update(switchBoxMsg{box: msg.current}) // current removed: switch to the promoted box
		}
		m = m.popN(1)
		return m, m.refresh()
	case createAppMsg:
		name, port, c := msg.name, msg.port, m.client
		return m, func() tea.Msg { return actionResultMsg{err: c.CreateApp(name, port), popLevels: 1} }
	case stopAppMsg:
		name, c := msg.name, m.client
		return m, func() tea.Msg { return actionResultMsg{err: c.StopApp(name), popLevels: 1} }
	case startAppMsg:
		name, c := msg.name, m.client
		return m, func() tea.Msg { return actionResultMsg{err: c.StartApp(name), popLevels: 1} }
	case deleteAppMsg:
		name, c := msg.name, m.client
		return m, func() tea.Msg { return actionResultMsg{err: c.DeleteApp(name), popLevels: 2} }
	case linkAppMsg:
		name, repo, branch, rootDir, c := msg.name, msg.repo, msg.branch, msg.rootDir, m.client
		return m, func() tea.Msg { return actionResultMsg{err: c.LinkApp(name, repo, branch, rootDir), popLevels: 1} }
	case removeDomainMsg:
		app, dom, c := msg.app, msg.domain, m.client
		return m, func() tea.Msg { return actionResultMsg{err: c.RemoveAppDomain(app, dom), popLevels: 1} }
	case addDomainMsg:
		app, dom, c := msg.app, msg.domain, m.client
		return m, func() tea.Msg {
			st, err := c.AddAppDomain(app, dom)
			return domainAddedMsg{app: app, st: st, err: err}
		}
	case domainAddedMsg:
		if _, ok := m.top().(domainFormView); !ok {
			return m, nil // user navigated away before the add returned
		}
		if msg.err != nil {
			next, _ := m.top().Update(errMsg{msg.err})
			m.stack[len(m.stack)-1] = next.(view)
			return m, nil
		}
		m.stack[len(m.stack)-1] = newDomainDetailView(msg.app, msg.st)
		if m.width > 0 {
			seeded, _ := m.top().Update(tea.WindowSizeMsg{Width: m.width, Height: m.height})
			m.stack[len(m.stack)-1] = seeded.(view)
		}
		return m, m.refresh()
	case githubStartMsg:
		org, c := msg.org, m.client
		ctx, cancel := context.WithCancel(context.Background())
		m.githubCancel = cancel
		return m, func() tea.Msg { return beginManifestFlow(ctx, c, org) }
	case githubFormReadyMsg:
		// Always schedule wait so the flow's servers are torn down on every
		// path — even if the user left the github view before the URL arrived
		// (otherwise wait, whose defers close the servers, is never scheduled).
		if gv, ok := m.top().(manifestView); ok {
			next, _ := gv.Update(msg) // sets formURL for the manual-open fallback
			m.stack[len(m.stack)-1] = next.(view)
		}
		return m, msg.wait
	case githubDoneMsg:
		if m.githubCancel != nil {
			m.githubCancel()
			m.githubCancel = nil
		}
		if gv, ok := m.top().(manifestView); ok {
			next, cmd := gv.Update(msg) // nil → pop back to apps; err → banner in the view
			m.stack[len(m.stack)-1] = next.(view)
			return m, cmd
		}
		return m, nil // user already navigated away; the flow was torn down via cancel
	case actionResultMsg:
		if msg.err != nil {
			next, _ := m.top().Update(errMsg{msg.err})
			m.stack[len(m.stack)-1] = next.(view)
			return m, nil
		}
		m = m.popN(msg.popLevels)
		return m, m.refresh()
	case popMsg:
		m = m.popN(msg.n)
		return m, m.refresh()
	case deployMsg:
		name, cwd, fromRepo, c := msg.name, msg.cwd, msg.fromRepo, m.client
		return m, func() tea.Msg {
			var dep store.Deployment
			var err error
			if fromRepo {
				dep, err = c.DeployFromRepo(name)
			} else {
				dep, err = c.Deploy(name, cwd)
			}
			return deployStartedMsg{app: name, id: dep.ID, err: err}
		}
	case deployStartedMsg:
		if _, ok := m.top().(deployView); !ok {
			return m, nil // user navigated away before the kickoff returned
		}
		if msg.err != nil {
			next, _ := m.top().Update(errMsg{msg.err})
			m.stack[len(m.stack)-1] = next.(view)
			return m, nil
		}
		m.stack[len(m.stack)-1] = newLogsView(msg.app, msg.id, "building")
		if m.width > 0 {
			seeded, _ := m.top().Update(tea.WindowSizeMsg{Width: m.width, Height: m.height})
			m.stack[len(m.stack)-1] = seeded.(view)
		}
		return m, m.refresh()
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		// no return: fall through to forward the size to the top view
	}
	if pr, ok := msg.(pollResult); ok {
		if pr.reachable() {
			m.loaded = true
		}
		// A 401 means the box is up and answering — an auth failure, not a
		// dial failure. Keep it out of the "unreachable — retrying" path so
		// the bar points the user at login instead of at ports/services.
		m.unauthorized = false
		if e, ok := msg.(errMsg); ok && isUnauthorized(e.err) {
			m.down, m.unauthorized = false, true
		} else {
			m.down = !pr.reachable()
		}
	}
	next, cmd := m.top().Update(msg)
	m.stack[len(m.stack)-1] = next.(view)
	return m, cmd
}

func (m Model) View() string {
	crumbs := make([]string, len(m.stack))
	for i, v := range m.stack {
		crumbs[i] = v.title()
	}
	header := lipgloss.NewStyle().Bold(true).Render(" piper ") + "· " + strings.Join(crumbs, " › ")
	body := header + "\n\n" + m.top().View()
	if f := m.topFooter(); f != "" {
		body += "\n\n" + f
	}
	return body + "\n\n" + m.statusBar()
}

// unauthorizedHint names the repair path for a 401: a relay-backed box needs a
// fresh account credential (the github wizard re-logs-in), a LAN box needs a
// new agent token pasted into its box entry.
func unauthorizedHint(remote bool) string {
	if remote {
		return "unauthorized — press g to log in again"
	}
	return "unauthorized — press t to update this box's token"
}

func (m Model) statusBar() string {
	loc := fmt.Sprintf("%s · %s", m.box, m.addr)
	switch {
	case m.unauthorized:
		return fmt.Sprintf(" ○ %s · %s", loc, unauthorizedHint(m.remote))
	case m.down:
		return fmt.Sprintf(" ○ %s · unreachable — retrying…", loc)
	case !m.loaded:
		return fmt.Sprintf(" … %s · connecting…", loc)
	default:
		return fmt.Sprintf(" ● %s · %s", loc, pluralApps(m.stack[0].(appsView).count()))
	}
}
