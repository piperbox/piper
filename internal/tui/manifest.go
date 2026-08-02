package tui

import (
	"context"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/piperbox/piper/internal/config"
)

// manifestView runs the GitHub App manifest flow: enter an org (blank = personal
// account), press ↵, and it serves a local auto-submitting form that POSTs the
// manifest to GitHub, catches the ?code= redirect, and exchanges it for App
// credentials on the box. It mirrors cmd/piper's `github setup`, bridged into a
// tea.Cmd. The socket plumbing runs in beginManifestFlow (below Update), so
// Update stays a pure (msg) -> (model, cmd) machine.
type manifestView struct {
	org     textinput.Model
	running bool
	formURL string
	spin    spinner.Model
	err     error
}

func newManifestView() manifestView {
	org := textinput.New()
	org.Placeholder = "org (blank for your personal account)"
	org.Focus()
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	return manifestView{org: org, spin: sp}
}

func (v manifestView) Init() tea.Cmd { return nil }

func (v manifestView) title() string { return "manifest" }

func (v manifestView) refresh(API) tea.Cmd { return nil }

func (v manifestView) capturesText() bool { return true }

func (v manifestView) footer() string { return "↵ start · esc cancel" }

func (v manifestView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case errMsg:
		v.err, v.running = msg.err, false
		return v, nil
	case githubDoneMsg:
		if msg.err != nil {
			v.err, v.running = msg.err, false
			return v, nil
		}
		return v, func() tea.Msg { return popMsg{n: 1} }
	case githubFormReadyMsg:
		v.formURL = msg.url
		return v, nil
	case spinner.TickMsg:
		var cmd tea.Cmd
		v.spin, cmd = v.spin.Update(msg)
		return v, cmd
	case tea.KeyMsg:
		if msg.String() == "enter" && !v.running {
			return v.start()
		}
	}
	if v.running {
		return v, nil // ignore field edits mid-flow
	}
	v.err = nil
	var cmd tea.Cmd
	v.org, cmd = v.org.Update(msg)
	return v, cmd
}

// start flips to the running state and signals the root to launch the manifest
// flow. It is not a method on API-only state: it needs the client for
// Manifest/ExchangeGitHub, which the view does not hold. The root owns the
// client, so it turns the githubStartMsg intent into the runManifestFlow cmd
// (see app.go).
func (v manifestView) start() (tea.Model, tea.Cmd) {
	v.running, v.err = true, nil
	v.formURL = ""
	org := strings.TrimSpace(v.org.Value())
	return v, tea.Batch(v.spin.Tick, func() tea.Msg { return githubStartMsg{org: org} })
}

func (v manifestView) View() string {
	var b strings.Builder
	b.WriteString("  configure a GitHub App\n\n")
	if v.running {
		fmt.Fprintf(&b, "  %s waiting for GitHub App approval…\n", v.spin.View())
		if v.formURL != "" {
			fmt.Fprintf(&b, "  %s\n", v.formURL)
		}
		b.WriteString("\n  esc cancel")
		if v.err != nil {
			fmt.Fprintf(&b, "\n\n ⚠ %v", v.err)
		}
		return b.String()
	}
	fmt.Fprintf(&b, "  org   %s\n\n", v.org.View())
	if v.err != nil {
		fmt.Fprintf(&b, " ⚠ %v\n\n", v.err)
	}
	b.WriteString("  ↵ start   esc cancel")
	return b.String()
}

// manifestActionURL is the GitHub endpoint the manifest form POSTs to: the
// personal-account creator, or the org creator when org is non-empty.
func manifestActionURL(org string) string {
	if org == "" {
		return "https://github.com/settings/apps/new"
	}
	return fmt.Sprintf("https://github.com/organizations/%s/settings/apps/new", url.PathEscape(org))
}

// browserCmd builds the OS command that opens rawURL, or nil when
// PIPER_NO_BROWSER=1 asks for the browser to stay shut. Duplicated from
// cmd/piper (that copy is unexported in package main).
func browserCmd(rawURL string) *exec.Cmd {
	if config.NoBrowser() {
		return nil
	}
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", rawURL)
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL)
	default:
		return exec.Command("xdg-open", rawURL)
	}
}

// openBrowser opens rawURL in the OS browser. A package var so tests can stub it.
var openBrowser = func(rawURL string) error {
	cmd := browserCmd(rawURL)
	if cmd == nil {
		return nil
	}
	return cmd.Start()
}

// beginManifestFlow starts the two-server manifest dance and returns once the
// servers are up: a githubFormReadyMsg carrying the form URL (for the running
// view to display as a manual-open fallback on headless boxes) plus the wait
// cmd that blocks on GitHub's callback, or a githubDoneMsg{err} if setup
// failed before the servers could start. It mirrors cmd/piper/main.go's
// githubSetup: a callback server catches GitHub's ?code=, a form server serves
// an auto-submitting POST of the manifest, and the browser opens at the form.
// It is exercised by the CLI's githubSetup tests + e2e; unit tests here drive
// the state machine directly (see github_test.go), not this function.
func beginManifestFlow(ctx context.Context, c API, org string) tea.Msg {
	codeCh := make(chan string, 1)
	cbLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return githubDoneMsg{err: err}
	}
	redirect := "http://" + cbLn.Addr().String() + "/cb"

	manifest, err := c.Manifest(redirect)
	if err != nil {
		cbLn.Close()
		return githubDoneMsg{err: err}
	}

	cbSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if code := r.URL.Query().Get("code"); code != "" {
			fmt.Fprintln(w, "Piper GitHub App created. You can close this tab.")
			select {
			case codeCh <- code:
			default:
			}
		}
	})}
	go cbSrv.Serve(cbLn)

	page := fmt.Sprintf(`<form id="f" action="%s" method="post">`+
		`<input type="hidden" name="manifest" value='%s'></form>`+
		`<script>document.getElementById('f').submit()</script>`,
		html.EscapeString(manifestActionURL(org)), html.EscapeString(manifest))
	formLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		cbSrv.Close()
		return githubDoneMsg{err: err}
	}
	formSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, page)
	})}
	go formSrv.Serve(formLn)

	formURL := "http://" + formLn.Addr().String()
	_ = openBrowser(formURL)

	wait := func() tea.Msg {
		defer cbSrv.Close()
		defer formSrv.Close()
		defer cbLn.Close()
		defer formLn.Close()
		select {
		case code := <-codeCh:
			_, err := c.ExchangeGitHub(code)
			return githubDoneMsg{err: err}
		case <-time.After(5 * time.Minute):
			return githubDoneMsg{err: fmt.Errorf("timed out waiting for GitHub App approval")}
		case <-ctx.Done():
			return githubDoneMsg{err: ctx.Err()}
		}
	}
	return githubFormReadyMsg{url: formURL, wait: wait}
}
