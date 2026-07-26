package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/piperbox/piper/internal/config"
	"github.com/piperbox/piper/internal/relayclient"
)

// wizardState is where the github wizard stands in the onboarding story.
type wizardState int

const (
	wizLoading      wizardState = iota // probing config + relay status
	wizLogin                           // no credential: armed, ↵ starts the browser login
	wizLoginPolling                    // code + URL shown, polling CLILoginPoll
	wizByo                             // relay brokers no App; manifest flow is the path
	wizInstall                         // logged in, no installation: URL shown, polling status
	wizInstalled                       // installations listed; ↵ browses repos
)

// githubWizardView is the relay-brokered GitHub onboarding wizard behind `g`:
// login → App install → installations. It decides its state from client config
// and GET /v1/github/status, and rides the root's 2s tick for all polling —
// every cmd is a one-shot HTTP call, so esc simply pops the view and polling
// stops with it (an abandoned login handle expires on the relay).
type githubWizardView struct {
	relay      RelayDialer
	state      wizardState
	base       string // relay base in use: saved RelayAPI or relayclient.DefaultAPI
	cred       string // account credential once known
	handle     string // brokered-login poll handle
	code       string // user code the human enters in the browser
	installURL string
	insts      []relayclient.Installation
	sel        int
	err        error
}

func newGithubWizard(relay RelayDialer) githubWizardView {
	return githubWizardView{relay: relay, state: wizLoading}
}

func (v githubWizardView) Init() tea.Cmd { return nil }

func (v githubWizardView) title() string { return "github" }

// refresh ignores the box API: the wizard polls the relay. Only the states
// that are waiting on something return a cmd; settled states poll nothing.
func (v githubWizardView) refresh(API) tea.Cmd {
	relay := v.relay
	switch v.state {
	case wizLoading:
		return func() tea.Msg { return probeStatus(relay) }
	case wizLoginPolling:
		base, handle := v.base, v.handle
		return func() tea.Msg { return pollLogin(relay, base, handle) }
	case wizInstall:
		base, cred := v.base, v.cred
		return func() tea.Msg { return probeWith(relay, base, cred) }
	}
	return nil
}

func (v githubWizardView) footer() string {
	switch v.state {
	case wizLogin:
		return "↵ sign in · m manifest app · esc cancel"
	case wizInstall:
		return "o open install page · esc cancel"
	case wizInstalled:
		return "↑↓ move · ↵ repos · m manifest app · esc back"
	case wizByo:
		return "m manifest app · esc back"
	}
	return "esc cancel"
}

func (v githubWizardView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case wizStatusMsg:
		if msg.err != nil {
			if errors.Is(msg.err, relayclient.ErrBadCredential) {
				// The relay no longer knows this credential (e.g. re-provisioned
				// relay DB): retrying can never succeed, so fall back to sign-in.
				// A fresh login overwrites the stale credential on success.
				v.err, v.base, v.state = msg.err, msg.base, wizLogin
				return v, nil
			}
			v.err = msg.err // banner; the state keeps polling, so a transient error retries
			return v, nil
		}
		v.err = nil
		v.base, v.cred = msg.base, msg.cred
		switch {
		case msg.noCred:
			v.state = wizLogin
		case !msg.st.GitHubApp:
			v.state = wizByo
		case len(msg.st.Installations) == 0:
			v.state, v.installURL = wizInstall, msg.st.InstallURL
		default:
			v.state, v.insts = wizInstalled, msg.st.Installations
			if v.sel >= len(v.insts) {
				v.sel = 0
			}
		}
		return v, nil
	case wizLoginStartedMsg:
		if msg.err != nil {
			v.err, v.state = msg.err, wizLogin
			return v, nil
		}
		v.handle, v.code, v.err = msg.handle, msg.code, nil
		v.state = wizLoginPolling
		return v, nil
	case wizLoginDoneMsg:
		if msg.pending {
			return v, nil
		}
		if msg.err != nil {
			v.err = msg.err // banner; stay polling — the next tick retries
			return v, nil
		}
		v.err = nil
		v.cred = msg.acc.AccountCredential
		if msg.acc.InstallURL != "" {
			// One-trip carry-over: the relay already bounced the browser to the
			// install page; show the URL and watch status until it lands.
			v.state, v.installURL = wizInstall, msg.acc.InstallURL
			return v, nil
		}
		v.state = wizLoading // has installs already (or BYO relay): re-probe decides
		return v, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "enter":
			switch v.state {
			case wizLogin:
				return v.startLogin()
			case wizInstalled:
				if len(v.insts) > 0 {
					sub := wizardReposView{relay: v.relay, base: v.base, cred: v.cred, inst: v.insts[v.sel]}
					return v, func() tea.Msg { return pushMsg{view: sub} }
				}
			}
		case "m":
			if v.state == wizLogin || v.state == wizByo || v.state == wizInstalled {
				return v, func() tea.Msg { return pushMsg{view: newManifestView()} }
			}
		case "o":
			if v.state == wizInstall && v.installURL != "" {
				url := v.installURL
				return v, func() tea.Msg { _ = openBrowser(url); return nil }
			}
		case "up", "k":
			if v.state == wizInstalled && v.sel > 0 {
				v.sel--
			}
		case "down", "j":
			if v.state == wizInstalled && v.sel < len(v.insts)-1 {
				v.sel++
			}
		}
	}
	return v, nil
}

// startLogin asks the relay for a login handle + user code and opens the
// browser at the verify page. The human side happens there; the wizard then
// polls the handle on the tick.
func (v githubWizardView) startLogin() (tea.Model, tea.Cmd) {
	relay, base := v.relay, v.base
	v.err = nil
	return v, func() tea.Msg {
		handle, code, err := relay(base).CLILoginStart(context.Background())
		if err != nil {
			return wizLoginStartedMsg{err: err}
		}
		_ = openBrowser(verifyURL(base))
		return wizLoginStartedMsg{handle: handle, code: code}
	}
}

func verifyURL(base string) string { return strings.TrimRight(base, "/") + "/v1/login/cli" }

// probeStatus loads client config and, when a credential exists, asks the
// relay for GitHub App status. Runs inside a tea.Cmd, off the UI thread.
func probeStatus(relay RelayDialer) tea.Msg {
	cc, err := config.LoadClient()
	if err != nil {
		return wizStatusMsg{err: err}
	}
	base := cc.RelayAPI
	if base == "" {
		base = relayclient.DefaultAPI
	}
	if cc.AccountCredential == "" {
		return wizStatusMsg{noCred: true, base: base}
	}
	return probeWith(relay, base, cc.AccountCredential)
}

func probeWith(relay RelayDialer, base, cred string) tea.Msg {
	st, err := relay(base).GitHubStatus(context.Background(), cred)
	if err != nil {
		return wizStatusMsg{base: base, cred: cred, err: err}
	}
	return wizStatusMsg{base: base, cred: cred, st: st}
}

// pollLogin polls one brokered-login round and, on success, saves the
// credential + relay base to client config before reporting — all off the UI
// thread, mirroring the CLI's relayLoginWeb.
func pollLogin(relay RelayDialer, base, handle string) tea.Msg {
	acc, err := relay(base).CLILoginPoll(context.Background(), handle)
	if errors.Is(err, relayclient.ErrAuthPending) {
		return wizLoginDoneMsg{pending: true}
	}
	if err != nil {
		return wizLoginDoneMsg{err: err}
	}
	cc, err := config.LoadClient()
	if err != nil {
		return wizLoginDoneMsg{err: err}
	}
	cc.RelayAPI = base
	cc.AccountCredential = acc.AccountCredential
	if err := config.SaveClient(cc); err != nil {
		return wizLoginDoneMsg{err: err}
	}
	return wizLoginDoneMsg{acc: acc}
}

func (v githubWizardView) View() string {
	var b strings.Builder
	switch v.state {
	case wizLoading:
		b.WriteString("  checking GitHub status…\n")
	case wizLogin:
		fmt.Fprintf(&b, "  sign in with GitHub via %s\n\n", v.base)
		b.WriteString("  ↵ opens your browser; you'll enter a short code there.\n")
	case wizLoginPolling:
		b.WriteString("  finish signing in — enter this code in your browser:\n\n")
		fmt.Fprintf(&b, "      %s\n\n      %s\n\n", v.code, verifyURL(v.base))
		b.WriteString("  waiting…\n")
	case wizByo:
		b.WriteString("  this relay doesn't broker a GitHub App.\n\n")
		b.WriteString("  press m to create a self-held App on this box (manifest flow).\n")
	case wizInstall:
		b.WriteString("  install the Piper GitHub App on the repos you want to deploy:\n\n")
		fmt.Fprintf(&b, "      %s\n\n", v.installURL)
		b.WriteString("  waiting for the install…\n")
	case wizInstalled:
		b.WriteString("  GitHub connected — installations:\n\n")
		for i, in := range v.insts {
			marker := "  "
			if i == v.sel {
				marker = "▸ "
			}
			fmt.Fprintf(&b, "  %s%s (%s)\n", marker, in.TargetLogin, in.TargetType)
		}
	}
	if v.err != nil {
		fmt.Fprintf(&b, "\n ⚠ %v\n", v.err)
	}
	return b.String()
}

// wizardReposView is the wizard's read-only repo listing for one installation,
// pushed by ↵ on the installations list; esc pops back to the wizard. Repos
// load once, not per tick — GitHubRepos proxies to GitHub's API and the
// listing has no live state worth the rate-limit spend.
type wizardReposView struct {
	relay      RelayDialer
	base, cred string
	inst       relayclient.Installation
	repos      []relayclient.Repo
	loaded     bool
	err        error
}

func (v wizardReposView) Init() tea.Cmd { return nil }

func (v wizardReposView) title() string { return "repos" }

func (v wizardReposView) footer() string {
	if v.loaded && v.err != nil {
		return "r retry · esc back"
	}
	return "esc back"
}

// retry clears a completed error load so the next refresh re-arms the request.
// A successful load is left untouched — GitHubRepos loads once, not on a timer,
// so r has nothing to do there.
func (v wizardReposView) retry() wizardReposView {
	if v.loaded && v.err != nil {
		v.loaded, v.err = false, nil
	}
	return v
}

func (v wizardReposView) refresh(API) tea.Cmd {
	if v.loaded {
		return nil
	}
	relay, base, cred, id := v.relay, v.base, v.cred, v.inst.ID
	return func() tea.Msg {
		repos, err := relay(base).GitHubRepos(context.Background(), cred, id)
		return wizReposMsg{repos: repos, err: err}
	}
}

func (v wizardReposView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case wizReposMsg:
		v.loaded = true
		v.repos, v.err = m.repos, m.err
	case tea.KeyMsg:
		// r is this view's alone: the repos load once, so a failed load has no
		// tick to rescue it.
		if m.String() == "r" {
			v = v.retry()
			return v, v.refresh(nil)
		}
	}
	return v, nil
}

func (v wizardReposView) View() string {
	var b strings.Builder
	fmt.Fprintf(&b, "  %s (%s) — repositories:\n\n", v.inst.TargetLogin, v.inst.TargetType)
	switch {
	case !v.loaded:
		b.WriteString("  loading…\n")
	case len(v.repos) == 0 && v.err == nil:
		b.WriteString("  no repositories\n")
	}
	for _, r := range v.repos {
		if r.Visibility != "" && r.Visibility != "public" {
			fmt.Fprintf(&b, "  %s (%s)\n", r.FullName, r.Visibility)
		} else {
			fmt.Fprintf(&b, "  %s\n", r.FullName)
		}
	}
	if v.err != nil {
		fmt.Fprintf(&b, "\n ⚠ %v\n", v.err)
	}
	return b.String()
}
