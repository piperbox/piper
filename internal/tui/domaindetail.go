package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/piperbox/piper/internal/domain"
)

// domainDetailView shows one per-app custom domain: live status, the CNAME the
// user must create, and the issuance note. Reached from enter on a domain row
// and from the add flow (the form is replaced with it on success). It re-polls
// on every tick, so pending → issuing → active surfaces without leaving the TUI.
type domainDetailView struct {
	app string
	st  domain.AppDomainStatus
	err error
}

func newDomainDetailView(app string, st domain.AppDomainStatus) domainDetailView {
	return domainDetailView{app: app, st: st}
}

func (v domainDetailView) Init() tea.Cmd { return nil }

func (v domainDetailView) title() string { return "domain" }

func (v domainDetailView) footer() string { return "esc back" }

func (v domainDetailView) refresh(c API) tea.Cmd {
	app, dom := v.app, v.st.Domain
	return func() tea.Msg {
		ds, err := c.AppDomains(app)
		if err != nil {
			return errMsg{err}
		}
		for _, d := range ds {
			if d.Domain == dom {
				return domainDetailLoadedMsg{st: d, found: true}
			}
		}
		return domainDetailLoadedMsg{}
	}
}

func (v domainDetailView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case domainDetailLoadedMsg:
		if msg.found {
			v.st = msg.st
		}
		v.err = nil
	case errMsg:
		v.err = msg.err
	}
	return v, nil
}

func (v domainDetailView) View() string {
	var b strings.Builder
	if v.err != nil {
		fmt.Fprintf(&b, " ⚠ %v\n\n", v.err)
	}
	st := v.st
	dns := "no"
	if st.DNSOK {
		dns = "ok"
	}
	expires := "-"
	if st.CertNotAfter != nil {
		expires = st.CertNotAfter.Format("2006-01-02")
	}
	fmt.Fprintf(&b, "  %s → %s\n\n", st.Domain, st.App)
	status := strings.TrimSpace(domainStatusIcon(st.Status) + " " + st.Status)
	fmt.Fprintf(&b, "  status  %s   cert expires  %s   dns  %s\n", status, expires, dns)
	if st.Status == domain.StatusFailed && st.Error != "" {
		fmt.Fprintf(&b, "\n ⚠ %s\n", st.Error)
	}
	if len(st.DNSRecords) > 0 {
		b.WriteString("\n  create this record at your DNS host:\n")
		for _, rec := range st.DNSRecords {
			fmt.Fprintf(&b, "    %s\t%s\t%s\n", rec.Name, rec.Type, rec.Value)
		}
	}
	if st.Status != domain.StatusActive {
		b.WriteString("\n  issuance starts once DNS points at the relay; status updates live\n")
	}
	return b.String()
}
