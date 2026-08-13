package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/piperbox/piper/internal/client"
	"github.com/piperbox/piper/internal/config"
	"github.com/piperbox/piper/internal/relayclient"
	"github.com/piperbox/piper/internal/store"
	"github.com/piperbox/piper/internal/tui"
	"github.com/piperbox/piper/internal/version"
)

var openBrowserFn = openBrowser

// stdinReader feeds the destructive-command confirmation prompts; a var so
// tests can substitute input.
var stdinReader io.Reader = os.Stdin

// dialClient returns a client for piperd's control API: loopback by default,
// or — when remote is a relay-connected box's base domain — through the
// relay's control plane at <RelayAPI>/agents/<base-domain>, authenticated by
// the account credential from `piper login`. The relay strips the prefix and
// swaps the credential for the box's own token, so the same Client works for
// both.
func dialClient(remote string, stderr io.Writer) (*client.Client, bool) {
	cc, err := config.LoadClient()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return nil, false
	}
	if remote != "" {
		if cc.RelayAPI == "" || cc.AccountCredential == "" {
			fmt.Fprintln(stderr, "error: remote target requires a relay login; run `piper login`")
			return nil, false
		}
		return client.New(strings.TrimRight(cc.RelayAPI, "/")+"/agents/"+remote, cc.AccountCredential), true
	}
	return client.New(cc.Addr, cc.Token), true
}

// printAgentVersion reports the build of the piperd actually serving this
// client — over the relay for a remote target, which is the case that matters:
// the box you are asking about is usually not the machine you are typing on,
// and its `piper` is a different build again (#375).
//
// Best-effort by design. A 404 means an agent old enough to predate the
// endpoint, which is worth saying out loud. Any other failure prints nothing:
// the ListApps that follows hits the same box and will report the real problem
// rather than this reporting it twice.
func printAgentVersion(c *client.Client, stdout io.Writer) {
	v, err := c.AgentVersion()
	switch {
	case errors.Is(err, client.ErrVersionUnsupported):
		fmt.Fprintln(stdout, "agent: unknown (too old to report its version)")
	case err == nil:
		fmt.Fprintf(stdout, "agent: %s\n", v)
	}
}

// appURL renders the URL an app is served on from its stored hostname and the
// scheme the daemon reports for it (api.App.Scheme). The daemon is the only
// party that can answer the scheme: this CLI reaches a relay-backed box over
// the relay and a directly-served box over the LAN (#507), so how it dialled
// says nothing about whether the app itself is on TLS. Empty hostname (never
// deployed) yields "".
func appURL(hostname, scheme string) string {
	if hostname == "" {
		return ""
	}
	return scheme + "://" + hostname
}

// isTerminal reports whether both stdout and stdin are interactive terminals;
// a func var so run() tests can force either mode. Requiring stdin too keeps
// a piped-but-drained stdin (e.g. `echo x | piper`) from launching the
// full-screen UI.
var isTerminal = func() bool {
	return term.IsTerminal(int(os.Stdout.Fd())) && term.IsTerminal(int(os.Stdin.Fd()))
}

// tuiRequestTimeout bounds each poll the TUI makes against piperd, so a
// blackholed box surfaces as unreachable instead of hanging the 2s poll loop.
const tuiRequestTimeout = 5 * time.Second

// launchTUI opens the interactive TUI against the current box (or the given
// relay-remote base domain); a func var so run() tests can stub it.
var launchTUI = func(remote string, stderr io.Writer) int {
	c, ok := dialClient(remote, stderr)
	if !ok {
		return 1
	}
	c = c.WithTimeout(tuiRequestTimeout)
	box, addr := "default", ""
	if cf, err := config.LoadClientFile(); err == nil {
		if b, ok := cf.CurrentBox(); ok {
			box = b.Name
		}
	}
	if cc, err := config.LoadClient(); err == nil {
		addr = cc.Addr // env overrides + localhost default applied
	}
	relay := remote != ""
	if relay {
		addr = remote // the relay base domain
	}
	if err := tui.Run(box, addr, relay, c, dialBox, relayDial); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}

// dialBox builds a TUI client for an arbitrary saved box (LAN path), for the
// in-TUI box switcher. Relay boxes are switched via the phase-6 wizard, not here.
func dialBox(b config.Box) (tui.API, string, bool, error) {
	return client.New(b.Addr, b.Token).WithTimeout(tuiRequestTimeout), b.Addr, false, nil
}

// relayDial builds the TUI's relay client; a thin adapter over relayclient.
func relayDial(base string) tui.RelayAPI { return relayclient.New(base) }

// login verifies token against the target (GET /v1/apps) and, on success,
// saves it to ~/.piper/piper/config.json.
func login(addr, token string, stdout, stderr io.Writer) int {
	if token == "" {
		fmt.Fprintln(stderr, "usage: piper login --token <token>  (create one with `piperd token create`, prefixed with `sudo` on a systemd install)")
		return 2
	}
	cc, err := config.LoadClient()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if addr == "" {
		addr = cc.Addr
	}
	if _, err := client.New(addr, token).ListApps(); err != nil {
		var se *client.StatusError
		switch {
		case errors.As(err, &se) && se.Code == http.StatusUnauthorized:
			fmt.Fprintln(stderr, "error: token rejected:", err)
		case errors.As(err, &se):
			fmt.Fprintln(stderr, "error:", err)
		default:
			fmt.Fprintf(stderr, "error: cannot reach piperd at %s: %v\n", addr, err)
		}
		return 1
	}
	// Load-mutate-save (mirroring relayLogin): a fresh ClientConfig here would
	// drop any stored relay creds, so a LAN login after a relay login wiped
	// RelayAPI/AccountCredential and broke the next `piper login` (#84).
	cc.Addr = addr
	cc.Token = token
	if err := config.SaveClient(cc); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintf(stdout, "logged in to %s\n", addr)
	return 0
}

func main() {
	if code := run(os.Args[1:], os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

func run(args []string, stdout, stderr io.Writer) int {
	gfs := flag.NewFlagSet("piper", flag.ContinueOnError)
	gfs.SetOutput(stderr)
	remote := gfs.String("remote", os.Getenv("PIPER_REMOTE"), "base domain of a relay-connected box to drive through the relay")
	showVersion := gfs.Bool("version", false, "print the build version and exit")
	if err := gfs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Fprintln(stdout, version.String())
		return 0
	}
	args = gfs.Args()
	if len(args) == 0 {
		if isTerminal() {
			return launchTUI(*remote, stderr)
		}
		return usage(stderr)
	}
	remoteFlagSet := false
	gfs.Visit(func(f *flag.Flag) {
		if f.Name == "remote" {
			remoteFlagSet = true
		}
	})
	if remoteFlagSet {
		switch args[0] {
		case "version", "login", "agent":
			fmt.Fprintf(stderr, "error: --remote does not apply to %q\n", args[0])
			return 2
		}
	}
	switch args[0] {
	case "version":
		fmt.Fprintln(stdout, version.String())
		return 0
	case "login":
		fs := flag.NewFlagSet("login", flag.ContinueOnError)
		fs.SetOutput(stderr)
		token := fs.String("token", "", "API token from `piperd token create` (LAN login)")
		addr := fs.String("addr", "", "piperd address (LAN login)")
		relay := fs.String("relay", relayclient.DefaultAPI, "relay control API base URL")
		web := fs.Bool("web", false, "one-trip browser login through the relay's GitHub App")
		org := fs.String("org", "", "enroll this box for a GitHub org you own")
		noEnroll := fs.Bool("no-enroll", false, "stop after identity; do not claim this box")
		reEnroll := fs.Bool("re-enroll", false, "claim this box fresh even if already enrolled (after `piper box rm`, or switching accounts/relays)")
		relogin := fs.Bool("relogin", false, "authenticate again even if the saved credential still works (switching GitHub accounts)")
		dataDir := fs.String("data-dir", config.DefaultDataDir(), "piperd data directory (enrollment-socket probe)")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		o := enrollFlowOpts{relayAPI: *relay, dataDir: *dataDir, org: *org, noEnroll: *noEnroll, reEnroll: *reEnroll, relogin: *relogin}
		if *token != "" {
			return login(*addr, *token, stdout, stderr) // LAN login: identity only, unchanged
		}
		if *web {
			return relayLoginWeb(*relay, o, stdout, stderr)
		}
		return relayLogin(*relay, o, stdout, stderr)
	case "agent":
		return agent(args[1:], stdout, stderr)
	case "create":
		if len(args) < 2 {
			fmt.Fprintln(stderr, "usage: piper create <name> [--port N]")
			return 2
		}
		name := args[1]
		fs := flag.NewFlagSet("create", flag.ContinueOnError)
		fs.SetOutput(stderr)
		port := fs.Int("port", 8080, "container port the app listens on")
		if err := fs.Parse(args[2:]); err != nil {
			return 2
		}
		if fs.NArg() != 0 {
			fmt.Fprintln(stderr, "usage: piper create <name> [--port N]")
			return 2
		}
		c, ok := dialClient(*remote, stderr)
		if !ok {
			return 1
		}
		if err := c.CreateApp(name, *port); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintf(stdout, "created app %q (port %d)\n", name, *port)
		return 0
	case "deploy":
		if len(args) < 2 {
			fmt.Fprintln(stderr, "usage: piper deploy <name> [--path DIR] [--timeout DUR]")
			return 2
		}
		name := args[1]
		fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
		fs.SetOutput(stderr)
		path := fs.String("path", ".", "source directory containing a Dockerfile")
		timeout := fs.Duration("timeout", 15*time.Minute, "give up following the deploy after this long (0 waits forever)")
		if err := fs.Parse(args[2:]); err != nil {
			return 2
		}
		if fs.NArg() != 0 {
			fmt.Fprintln(stderr, "usage: piper deploy <name> [--path DIR] [--timeout DUR]")
			return 2
		}
		pathSet := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "path" {
				pathSet = true
			}
		})
		c, ok := dialClient(*remote, stderr)
		if !ok {
			return 1
		}
		// Without an explicit --path, a github-linked app deploys from its
		// repo; the launch directory need not hold the source at all.
		fromRepo := ""
		if !pathSet {
			app, err := c.App(name)
			var se *client.StatusError
			if errors.As(err, &se) && se.Code == http.StatusNotFound {
				fmt.Fprintf(stderr, "error: app %q does not exist — run 'piper create %s' first\n", name, name)
				return 1
			}
			if err != nil {
				fmt.Fprintln(stderr, "error:", err)
				return 1
			}
			if app.Repo != "" {
				fromRepo = app.Repo + "@" + app.Branch
			}
		}
		var dep store.Deployment
		var err error
		if fromRepo != "" {
			fmt.Fprintf(stderr, "deploying %s from %s\n", name, fromRepo)
			dep, err = c.DeployFromRepo(name)
		} else {
			dep, err = c.Deploy(name, *path)
		}
		if err != nil {
			var se *client.StatusError
			if errors.As(err, &se) && se.Code == http.StatusNotFound {
				fmt.Fprintf(stderr, "error: app %q does not exist — run 'piper create %s' first\n", name, name)
				return 1
			}
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		ctx := context.Background()
		if *timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, *timeout)
			defer cancel()
		}
		final, err := c.FollowDeploy(ctx, name, dep.ID, stderr)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				fmt.Fprintf(stderr, "error: gave up waiting for %s to finish after %s; it may still be building — check `piper app %s`\n", name, *timeout, name)
				return 1
			}
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		if final.Status != "running" {
			fmt.Fprintf(stderr, "deploy failed: %s (%s)\n", name, final.Status)
			return 1
		}
		app, err := c.App(name)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintf(stdout, "deployed %s: %s (%s)\n", name, appURL(app.Hostname, app.Scheme), final.Status)
		return 0
	case "list":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "usage: piper list")
			return 2
		}
		c, ok := dialClient(*remote, stderr)
		if !ok {
			return 1
		}
		apps, err := c.ListApps()
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		for _, app := range apps {
			if url := appURL(app.Hostname, app.Scheme); url != "" {
				fmt.Fprintf(stdout, "%s\tport=%d\t%s\n", app.Name, app.Port, url)
			} else {
				fmt.Fprintf(stdout, "%s\tport=%d\n", app.Name, app.Port)
			}
		}
		return 0
	case "status":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "usage: piper status")
			return 2
		}
		c, ok := dialClient(*remote, stderr)
		if !ok {
			return 1
		}
		if *remote != "" {
			live, err := c.Liveness()
			if err != nil {
				fmt.Fprintln(stderr, "error:", err)
				return 1
			}
			if !live.Connected {
				fmt.Fprintf(stdout, "box %s: offline\n", *remote)
				return 0
			}
			fmt.Fprintf(stdout, "box %s: connected\n", *remote)
		}
		printAgentVersion(c, stdout)
		apps, err := c.ListApps()
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		for _, app := range apps {
			status := app.Status
			if status == "" {
				status = "-"
			}
			if url := appURL(app.Hostname, app.Scheme); url != "" {
				fmt.Fprintf(stdout, "%s\tstatus=%s\tport=%d\t%s\n", app.Name, status, app.Port, url)
			} else {
				fmt.Fprintf(stdout, "%s\tstatus=%s\tport=%d\n", app.Name, status, app.Port)
			}
		}
		return 0
	case "stop":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "usage: piper stop <name>")
			return 2
		}
		c, ok := dialClient(*remote, stderr)
		if !ok {
			return 1
		}
		if err := c.StopApp(args[1]); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintf(stdout, "stopped %s\n", args[1])
		return 0
	case "start":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "usage: piper start <name>")
			return 2
		}
		c, ok := dialClient(*remote, stderr)
		if !ok {
			return 1
		}
		if err := c.StartApp(args[1]); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintf(stdout, "started %s\n", args[1])
		return 0
	case "delete":
		if len(args) < 2 {
			fmt.Fprintln(stderr, "usage: piper delete <name> [--yes]")
			return 2
		}
		name := args[1]
		if strings.HasPrefix(name, "-") {
			// e.g. `piper delete --yes blog`: the flag was taken as the name.
			fmt.Fprintln(stderr, "usage: piper delete <name> [--yes]  (the app name must come before flags)")
			return 2
		}
		fs := flag.NewFlagSet("delete", flag.ContinueOnError)
		fs.SetOutput(stderr)
		yes := fs.Bool("yes", false, "skip the confirmation prompt")
		if err := fs.Parse(args[2:]); err != nil {
			return 2
		}
		if fs.NArg() != 0 {
			fmt.Fprintln(stderr, "usage: piper delete <name> [--yes]")
			return 2
		}
		if !*yes && !confirmPrompt(stdout, fmt.Sprintf("delete app %q and all its history?", name)) {
			fmt.Fprintln(stdout, "aborted")
			return 0
		}
		c, ok := dialClient(*remote, stderr)
		if !ok {
			return 1
		}
		if err := c.DeleteApp(name); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintf(stdout, "deleted %s\n", name)
		return 0
	case "app":
		return cmdApp(*remote, args[1:], stdout, stderr)
	case "env":
		return cmdEnv(*remote, args[1:], stdout, stderr)
	case "domains":
		return cmdDomains(*remote, args[1:], stdout, stderr)
	case "github":
		return cmdGithub(*remote, args[1:], stdout, stderr)
	case "box":
		return cmdBox(args[1:], stdout, stderr)
	default:
		return usage(stderr)
	}
}

const appLinkUsage = "usage: piper app link <name> --repo owner/name [--branch main] [--root-dir apps/web]"

func cmdApp(remote string, args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 || args[0] != "link" {
		fmt.Fprintln(stderr, appLinkUsage)
		return 2
	}
	if len(args) < 2 {
		fmt.Fprintln(stderr, appLinkUsage)
		return 2
	}
	name := args[1]
	fs := flag.NewFlagSet("link", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", "", "GitHub repo, owner/name")
	branch := fs.String("branch", "main", "tracked branch")
	rootDir := fs.String("root-dir", "", "monorepo build subpath (relative repo path)")
	if err := fs.Parse(args[2:]); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *repo == "" {
		fmt.Fprintln(stderr, appLinkUsage)
		return 2
	}
	c, ok := dialClient(remote, stderr)
	if !ok {
		return 1
	}
	if err := c.LinkApp(name, *repo, *branch, *rootDir); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if *rootDir != "" {
		fmt.Fprintf(stdout, "linked %s -> %s (%s) root %s\n", name, *repo, *branch, *rootDir)
	} else {
		fmt.Fprintf(stdout, "linked %s -> %s (%s)\n", name, *repo, *branch)
	}
	return 0
}

const githubUsage = "usage: piper github setup [--org <name>] | piper github repos | piper github reset [--yes]"

func cmdGithub(remote string, args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, githubUsage)
		return 2
	}
	switch args[0] {
	case "setup":
		fs := flag.NewFlagSet("setup", flag.ContinueOnError)
		fs.SetOutput(stderr)
		org := fs.String("org", "", "GitHub organization name")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if fs.NArg() != 0 {
			fmt.Fprintln(stderr, "usage: piper github setup [--org <name>]")
			return 2
		}
		return githubSetup(remote, *org, stdout, stderr)
	case "repos":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "usage: piper github repos")
			return 2
		}
		return githubRepos(stdout, stderr)
	case "reset":
		fs := flag.NewFlagSet("reset", flag.ContinueOnError)
		fs.SetOutput(stderr)
		yes := fs.Bool("yes", false, "skip the confirmation prompt")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if fs.NArg() != 0 {
			fmt.Fprintln(stderr, "usage: piper github reset [--yes]")
			return 2
		}
		return githubReset(remote, *yes, stdout, stderr)
	default:
		fmt.Fprintln(stderr, githubUsage)
		return 2
	}
}

// githubSetup drives the GitHub App manifest flow: it asks piperd for a manifest,
// serves a tiny auto-submitting form that POSTs it to GitHub, catches the
// redirect ?code=, and exchanges it for App credentials stored on the box.
func githubSetup(remote, org string, stdout, stderr io.Writer) int {
	c, ok := dialClient(remote, stderr)
	if !ok {
		return 1
	}

	codeCh := make(chan string, 1)
	cbLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	defer cbLn.Close()
	redirect := "http://" + cbLn.Addr().String() + "/cb"

	manifest, err := c.Manifest(redirect)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	cbSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if code := r.URL.Query().Get("code"); code != "" {
			fmt.Fprintln(w, "Piper GitHub App created. You can close this tab.")
			codeCh <- code
		}
	})}
	go cbSrv.Serve(cbLn)
	defer cbSrv.Close()

	actionURL := "https://github.com/settings/apps/new"
	if org != "" {
		actionURL = fmt.Sprintf("https://github.com/organizations/%s/settings/apps/new", url.PathEscape(org))
	}

	// Auto-submitting form that POSTs the manifest to GitHub.
	page := fmt.Sprintf(`<form id="f" action="%s" method="post">`+
		`<input type="hidden" name="manifest" value='%s'></form><script>document.getElementById('f').submit()</script>`,
		html.EscapeString(actionURL), htmlEscape(manifest))
	formLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	formSrv := &http.Server{Handler: manifestFormHandler(page)}
	go formSrv.Serve(formLn)
	defer formSrv.Close()

	formURL := "http://" + formLn.Addr().String()
	fmt.Fprintf(stdout, "Opening %s — approve the App in your browser...\n", formURL)
	_ = openBrowserFn(formURL)

	var code string
	select {
	case code = <-codeCh:
	case <-time.After(5 * time.Minute):
		fmt.Fprintln(stderr, "error: timed out waiting for GitHub App approval")
		return 1
	}
	slug, err := c.ExchangeGitHub(code)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if slug != "" {
		fmt.Fprintf(stdout, "GitHub App configured. Install it: https://github.com/apps/%s/installations/new\n", url.PathEscape(slug))
	} else {
		fmt.Fprintln(stdout, "GitHub App configured. Install it on your repo.")
	}
	fmt.Fprintln(stdout, "Then run: piper app link <name> --repo owner/name")
	return 0
}

// githubReset drops the box's own GitHub App. A stored App is treated as a
// deliberate operator override and outranks any App a relay brokers, so a box
// that ever ran `piper github setup` keeps failing brokered deliveries until
// the row goes (#299). The provider only takes effect at start, hence the
// restart line.
func githubReset(remote string, yes bool, stdout, stderr io.Writer) int {
	c, ok := dialClient(remote, stderr)
	if !ok {
		return 1
	}
	if !yes && !confirmPrompt(stdout, "remove this box's own GitHub App? its private key is not recoverable") {
		fmt.Fprintln(stdout, "aborted")
		return 0
	}
	provider, err := c.ResetGitHub()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintln(stdout, "removed this box's own GitHub App.")
	switch provider {
	case "brokered":
		fmt.Fprintln(stdout, "webhooks will use the relay's brokered GitHub App after a restart of piperd.")
	case "none":
		fmt.Fprintln(stdout, "no GitHub App is configured now; git deploys stay off until you run `piper github setup`")
		fmt.Fprintln(stdout, "or connect to a relay that brokers one. Restart piperd to apply.")
	default:
		fmt.Fprintf(stdout, "next webhook provider: %s. Restart piperd to apply.\n", provider)
	}
	return 0
}

// manifestFormHandler serves the auto-submitting manifest form. The Content-Type
// is set explicitly: the page starts with <form>, which Go's content sniffer does
// not recognize as HTML, so it would otherwise be served as text/plain and the
// browser would show the source instead of submitting it.
func manifestFormHandler(page string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, page)
	}
}

func htmlEscape(s string) string { return strings.ReplaceAll(s, "'", "&#39;") }

// browserCmd builds the OS command that opens url, or nil when the browser must
// stay shut: PIPER_NO_BROWSER=1 for headless boxes, SSH sessions, and the e2e
// suite, which drives the real binary and so cannot reach the openBrowserFn
// test seam. Split out from openBrowser so the knob is testable without
// spawning anything.
func browserCmd(url string) *exec.Cmd {
	if config.NoBrowser() {
		return nil
	}
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url)
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return exec.Command("xdg-open", url)
	}
}

func openBrowser(url string) error {
	cmd := browserCmd(url)
	if cmd == nil {
		return nil
	}
	return cmd.Start()
}

// confirmPrompt guards a destructive command; only "y"/"yes" proceeds.
func confirmPrompt(stdout io.Writer, question string) bool {
	fmt.Fprintf(stdout, "%s [y/N] ", question)
	sc := bufio.NewScanner(stdinReader)
	if !sc.Scan() {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(sc.Text()))
	return answer == "y" || answer == "yes"
}

func usage(w io.Writer) int {
	fmt.Fprintln(w, "usage: piper [--remote <base-domain>] [--version] <version|login|create|deploy|list|status|stop|start|delete|app|env|domains|github|box|agent> [args]")
	fmt.Fprintln(w, "       piper                # no subcommand in a terminal: interactive TUI")
	fmt.Fprintln(w, "connect is gone — piper login claims the box too")
	return 2
}
