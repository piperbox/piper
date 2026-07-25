package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/piperbox/piper/internal/config"
	"github.com/piperbox/piper/internal/source"
)

// pickRepo is one repo-picker entry: the owner/name to link and the
// installation target it came from (shown only when >1 installation, #322).
type pickRepo struct{ fullName, target string }

// linkFormView attaches a git repo to an app: repo (owner/name), branch
// (default main), and an optional monorepo root dir. When the CLI is logged in
// to a relay, the repo field doubles as a filter over the account's
// installation repos; free text always submits, so LAN/BYO boxes and relay
// outages never block linking by hand. On submit it emits linkAppMsg; the root
// runs LinkApp and pops back to app detail on success.
type linkFormView struct {
	app     string
	relay   RelayDialer // injected by the root on push; nil in bare tests → free text
	repo    textinput.Model
	branch  textinput.Model
	rootDir textinput.Model
	focus   int        // 0 repo, 1 branch, 2 root dir
	repos   []pickRepo // nil until loaded; non-nil (possibly empty) stops reloading
	multi   bool       // >1 installation: matches carry their target label
	noCred  bool       // not logged in to a relay: show the g hint
	sel     int        // selected match; -1 = none (enter submits free text)
	err     error
}

func newLinkForm(app string) linkFormView {
	repo := textinput.New()
	repo.Placeholder = "owner/name"
	repo.Focus()
	branch := textinput.New()
	branch.Placeholder = "main"
	branch.SetValue("main")
	rootDir := textinput.New()
	rootDir.Placeholder = "(repo root)"
	return linkFormView{app: app, repo: repo, branch: branch, rootDir: rootDir, sel: -1}
}

func (v linkFormView) Init() tea.Cmd { return nil }

func (v linkFormView) title() string { return "link" }

// refresh loads the picker list once. It rides the root's tick, so it retries
// until the load lands; a non-nil (even empty) repos stops it.
func (v linkFormView) refresh(API) tea.Cmd {
	if v.relay == nil || v.repos != nil || v.noCred {
		return nil
	}
	relay := v.relay
	return func() tea.Msg { return loadLinkRepos(relay) }
}

// loadLinkRepos fetches every installation's repos as one flat picker list.
// All failures degrade to free text — a LAN/BYO box or an unreachable relay
// must never block linking by hand.
func loadLinkRepos(relay RelayDialer) tea.Msg {
	cc, err := config.LoadClient()
	if err != nil || cc.RelayAPI == "" || cc.AccountCredential == "" {
		return linkReposMsg{noCred: true}
	}
	rc := relay(cc.RelayAPI)
	st, err := rc.GitHubStatus(context.Background(), cc.AccountCredential)
	if err != nil {
		return linkReposMsg{repos: []pickRepo{}}
	}
	repos := []pickRepo{}
	for _, in := range st.Installations {
		rs, err := rc.GitHubRepos(context.Background(), cc.AccountCredential, in.ID)
		if err != nil {
			continue
		}
		for _, r := range rs {
			repos = append(repos, pickRepo{fullName: r.FullName, target: in.TargetLogin})
		}
	}
	return linkReposMsg{repos: repos, multi: len(st.Installations) > 1}
}

func (v linkFormView) capturesText() bool { return true }

func (v linkFormView) footer() string {
	return "↵ link · tab switch · ↑↓ pick · esc cancel · ? help"
}

func (v *linkFormView) applyFocus() {
	v.repo.Blur()
	v.branch.Blur()
	v.rootDir.Blur()
	switch v.focus {
	case 0:
		v.repo.Focus()
	case 1:
		v.branch.Focus()
	default:
		v.rootDir.Focus()
	}
}

// matches returns up to six loaded repos whose full name contains the typed
// filter, case-insensitively. An empty filter matches the first six.
func (v linkFormView) matches() []pickRepo {
	q := strings.ToLower(strings.TrimSpace(v.repo.Value()))
	var out []pickRepo
	for _, r := range v.repos {
		if strings.Contains(strings.ToLower(r.fullName), q) {
			out = append(out, r)
			if len(out) == 6 {
				break
			}
		}
	}
	return out
}

func (v linkFormView) submit() (tea.Model, tea.Cmd) {
	repo := strings.TrimSpace(v.repo.Value())
	if repo == "" {
		v.err = fmt.Errorf("repo is required")
		return v, nil
	}
	// Free text always submits (a relay outage must never block linking by
	// hand), so this is where a bare name is caught: the agent rejects it too,
	// but naming the shape here saves a round trip (#333).
	if !source.ValidRepo(repo) {
		v.err = fmt.Errorf("repo must be %s (e.g. octocat/hello-world)", source.RepoShape)
		return v, nil
	}
	branch := strings.TrimSpace(v.branch.Value())
	if branch == "" {
		branch = "main"
	}
	name, rootDir := v.app, strings.TrimSpace(v.rootDir.Value())
	return v, func() tea.Msg { return linkAppMsg{name: name, repo: repo, branch: branch, rootDir: rootDir} }
}

func (v linkFormView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case errMsg:
		v.err = msg.err
		return v, nil
	case linkReposMsg:
		v.repos, v.multi, v.noCred = msg.repos, msg.multi, msg.noCred
		return v, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "tab":
			v.focus = (v.focus + 1) % 3
			v.sel = -1
			v.applyFocus()
			return v, nil
		case "up":
			if v.focus == 0 && v.sel >= 0 {
				v.sel--
			}
			return v, nil
		case "down":
			if v.focus == 0 && v.sel < len(v.matches())-1 {
				v.sel++
			}
			return v, nil
		case "enter":
			if v.focus == 0 && v.sel >= 0 {
				v.repo.SetValue(v.matches()[v.sel].fullName)
				v.repo.CursorEnd()
				v.sel = -1
				v.focus = 1
				v.applyFocus()
				return v, nil
			}
			return v.submit()
		}
	}
	var cmd tea.Cmd
	switch v.focus {
	case 0:
		v.repo, cmd = v.repo.Update(msg)
	case 1:
		v.branch, cmd = v.branch.Update(msg)
	default:
		v.rootDir, cmd = v.rootDir.Update(msg)
	}
	// Only a keystroke clears a stale banner or resets the pick (typing means
	// enter now submits free text). Non-key messages — the root's ~2s poll,
	// cursor blinks — must not wipe the selection out from under the user (#334).
	if _, ok := msg.(tea.KeyMsg); ok {
		v.err = nil
		if v.focus == 0 {
			v.sel = -1
		}
	}
	return v, cmd
}

func (v linkFormView) View() string {
	var b strings.Builder
	fmt.Fprintf(&b, "  link %s to a repo\n\n", v.app)
	fmt.Fprintf(&b, "  repo      %s\n", v.repo.View())
	if v.focus == 0 {
		for i, r := range v.matches() {
			marker := "    "
			if i == v.sel {
				marker = "  ▸ "
			}
			line := r.fullName
			if v.multi {
				line += footerStyle.Render(" · " + r.target)
			}
			fmt.Fprintf(&b, "%s%s\n", marker, line)
		}
	}
	fmt.Fprintf(&b, "  branch    %s\n", v.branch.View())
	fmt.Fprintf(&b, "  root dir  %s\n\n", v.rootDir.View())
	if v.noCred {
		b.WriteString(footerStyle.Render("  press g to connect GitHub") + "\n\n")
	}
	if v.err != nil {
		fmt.Fprintf(&b, " ⚠ %v\n\n", v.err)
	}
	b.WriteString("  ↵ link   tab switch   esc cancel")
	return b.String()
}
