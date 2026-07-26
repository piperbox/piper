package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/piperbox/piper/internal/api"
	"github.com/piperbox/piper/internal/domain"
	"github.com/piperbox/piper/internal/store"
)

// appDetailView is the depth-1 view: an app header plus its deployment history.
// It re-polls every tick while on top, so a deploy started elsewhere surfaces
// live. Read-only; actions arrive in phase 4.
type appDetailView struct {
	name    string
	remote  bool
	app     api.App
	deps    []store.Deployment
	domains []domain.AppDomainStatus
	cursor  int
	loaded  bool
	err     error
}

func newAppDetailView(name string, remote bool) appDetailView {
	return appDetailView{name: name, remote: remote}
}

func (v appDetailView) Init() tea.Cmd { return nil }

func (v appDetailView) title() string { return v.name }

func (v appDetailView) footer() string {
	if _, ok := v.selectedDomain(); ok {
		return "a add domain · x remove · ↵ details · d deploy · esc back"
	}
	stopOrStart := "s stop"
	if v.app.Status == "stopped" {
		stopOrStart = "s start"
	}
	return "d deploy · " + stopOrStart + " · x delete · l link · a domain · ↵ logs · esc back"
}

// selectedDomain returns the domain row under the cursor, if the cursor is in
// the domains section (past the deployments).
func (v appDetailView) selectedDomain() (domain.AppDomainStatus, bool) {
	if i := v.cursor - len(v.deps); i >= 0 && i < len(v.domains) {
		return v.domains[i], true
	}
	return domain.AppDomainStatus{}, false
}

func (v appDetailView) refresh(c API) tea.Cmd {
	name := v.name
	return func() tea.Msg {
		app, err := c.App(name)
		if err != nil {
			return errMsg{err}
		}
		deps, err := c.Deployments(name)
		if err != nil {
			return errMsg{err}
		}
		domains, err := c.AppDomains(name)
		if err != nil {
			return errMsg{err}
		}
		return appDetailLoadedMsg{app: app, deps: deps, domains: domains}
	}
}

func (v appDetailView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case appDetailLoadedMsg:
		v.app, v.deps, v.domains, v.loaded, v.err = msg.app, msg.deps, msg.domains, true, nil
		if total := len(v.deps) + len(v.domains); v.cursor >= total {
			v.cursor = max(0, total-1)
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
			if v.cursor < len(v.deps)+len(v.domains)-1 {
				v.cursor++
			}
		case "enter":
			if d, ok := v.selectedDomain(); ok {
				app := v.name
				return v, func() tea.Msg { return pushMsg{newDomainDetailView(app, d)} }
			}
			if len(v.deps) > 0 && v.cursor < len(v.deps) {
				d := v.deps[v.cursor]
				return v, func() tea.Msg { return pushMsg{newLogsView(v.name, d.ID, d.Status)} }
			}
		case "d":
			if v.app.Repo != "" {
				repo, branch := v.app.Repo, v.app.Branch
				return v, func() tea.Msg { return pushMsg{newRepoDeployView(v.name, repo, branch)} }
			}
			cwd, _ := os.Getwd()
			_, statErr := os.Stat(filepath.Join(cwd, "Dockerfile"))
			hasDockerfile := statErr == nil
			return v, func() tea.Msg { return pushMsg{newDeployView(v.name, cwd, hasDockerfile)} }
		case "s":
			if v.app.Status == "stopped" {
				return v, func() tea.Msg { return pushMsg{newStartConfirm(v.name)} }
			}
			return v, func() tea.Msg { return pushMsg{newStopConfirm(v.name)} }
		case "x":
			if d, ok := v.selectedDomain(); ok {
				app := v.name
				return v, func() tea.Msg { return pushMsg{newRemoveDomainConfirm(app, d.Domain)} }
			}
			return v, func() tea.Msg { return pushMsg{newDeleteConfirm(v.name)} }
		case "l":
			return v, func() tea.Msg { return pushMsg{newLinkForm(v.name)} }
		case "a":
			return v, func() tea.Msg { return pushMsg{newDomainForm(v.name)} }
		}
	}
	return v, nil
}

func (v appDetailView) View() string {
	var b strings.Builder
	if v.err != nil {
		fmt.Fprintf(&b, " ⚠ %v\n\n", v.err)
	}
	url := appURL(v.app.Hostname, v.remote)
	if url == "" {
		url = "—"
	}
	fmt.Fprintf(&b, "  %s   %s   :%d   %s\n\n", v.name, url, v.app.Port, repoLine(v.app))
	if !v.loaded {
		b.WriteString(" loading…")
		return b.String()
	}
	if len(v.deps) == 0 {
		b.WriteString(" no deployments yet\n")
	} else {
		fmt.Fprintf(&b, "  %-14s %-12s %-10s %s\n", "DEPLOYMENT", "STATUS", "CREATED", "PR")
		for i, d := range v.deps {
			cursor := "  "
			if i == v.cursor {
				cursor = "▸ "
			}
			status := strings.TrimSpace(statusIcon(d.Status) + " " + d.Status)
			pr := ""
			if d.PR > 0 {
				pr = fmt.Sprintf("#%d", d.PR)
			}
			fmt.Fprintf(&b, "%s%-14s %-12s %-10s %s\n", cursor, shortID(d.ID), status, relTime(d.CreatedAt), pr)
		}
	}
	if len(v.domains) > 0 {
		fmt.Fprintf(&b, "\n  %-24s %-12s %-13s %s\n", "DOMAIN", "STATUS", "CERT EXPIRES", "DNS")
		for i, d := range v.domains {
			cursor := "  "
			if v.cursor == len(v.deps)+i {
				cursor = "▸ "
			}
			expires := "-"
			if d.CertNotAfter != nil {
				expires = d.CertNotAfter.Format("2006-01-02")
			}
			dns := "no"
			if d.DNSOK {
				dns = "ok"
			}
			status := strings.TrimSpace(domainStatusIcon(d.Status) + " " + d.Status)
			fmt.Fprintf(&b, "%s%-24s %-12s %-13s %s\n", cursor, d.Domain, status, expires, dns)
		}
	}
	return b.String()
}
