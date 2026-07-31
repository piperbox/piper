package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/piperbox/piper/internal/config"
	"github.com/piperbox/piper/internal/relayclient"
)

// pollSleep is the device-flow poll delay; a seam so tests don't really sleep.
var pollSleep = time.Sleep

// installPollTimeout bounds the advisory post-login install poll; a seam so
// tests can drive the timeout path without waiting the full ten minutes.
var installPollTimeout = 10 * time.Minute

// installPollInterval is the delay between advisory install polls; a seam so
// tests don't really wait out the full three seconds between polls.
var installPollInterval = 3 * time.Second

// errInstallTimeout reports the advisory install poll exhausting
// installPollTimeout without seeing an installation.
var errInstallTimeout = errors.New("timed out waiting for the GitHub App install")

// finishInstall runs the advisory post-login install poll. By the time it is
// called the account credential is already persisted, so the login has fully
// succeeded regardless of what the poll does. A timed-out, interrupted, or
// otherwise failed poll therefore must NOT turn a successful login into a
// non-zero exit (#297): it prints what is still outstanding and how to finish
// it, and the caller returns 0. Genuine login failures happen earlier and keep
// their non-zero exit.
func finishInstall(ctx context.Context, rc *relayclient.Client, acc relayclient.Account, stdout, stderr io.Writer) {
	if acc.InstallURL == "" {
		return
	}
	if err := waitForInstall(ctx, rc, acc.AccountCredential, acc.InstallURL); err != nil {
		fmt.Fprintf(stderr, "note: %v\n", err)
		fmt.Fprintf(stdout, "You are logged in. To deploy git repos, install the Piper GitHub App:\n  %s\nthen run `piper github repos` to see the result.\n", acc.InstallURL)
	}
}

// relayLogin runs the GitHub device flow against the relay, printing the
// verification URL + user code, polling to completion, and storing the returned
// account credential (and relay API base) in the CLI config.
func relayLogin(relayAPI string, o enrollFlowOpts, stdout, stderr io.Writer) int {
	// Interrupt-aware: Ctrl-C during the poll loop cancels the in-flight
	// request instead of waiting out the 30s client timeout.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	rc := relayclient.New(relayAPI)
	da, err := rc.LoginDevice(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintf(stdout, "To log in, open:\n\n    %s\n\nand enter the code: %s\n\n", da.VerificationURI, da.UserCode)
	_ = openBrowserFn(da.VerificationURI)

	interval := da.Interval
	if interval <= 0 {
		interval = 5
	}
	expires := da.ExpiresIn
	if expires <= 0 {
		expires = 300
	}
	deadline := time.Now().Add(time.Duration(expires) * time.Second)
	for {
		if time.Now().After(deadline) {
			fmt.Fprintln(stderr, "error: login timed out; run `piper login` again")
			return 1
		}
		pollSleep(time.Duration(interval) * time.Second)
		acc, err := rc.LoginPoll(ctx, da.DeviceCode)
		if errors.Is(err, relayclient.ErrAuthPending) {
			continue
		}
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		cc, err := config.LoadClient()
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		cc.RelayAPI = relayAPI
		cc.AccountCredential = acc.AccountCredential
		if err := config.SaveClient(cc); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintf(stdout, "logged in to relay as %s\n", acc.Username)
		if code := enrollAfterLogin(ctx, o, acc.AccountCredential, stdout, stderr); code != 0 {
			return code // identity is durable; re-running login resumes at the claim
		}
		finishInstall(ctx, rc, acc, stdout, stderr)
		return 0
	}
}

// relayLoginWeb runs the one-trip brokered browser login (#291): the relay mints
// a handle + user code, the user enters the code in the browser and authorizes,
// and — for a first-timer — the same browser session is bounced to the install
// page. The box holds no loopback listener; it only polls the handle. Unlike the
// device flow, this ends with the install already underway, so a first-timer's
// login and install are one browser trip.
func relayLoginWeb(relayAPI string, o enrollFlowOpts, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	rc := relayclient.New(relayAPI)
	handle, code, err := rc.CLILoginStart(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	verifyURL := strings.TrimRight(relayAPI, "/") + "/v1/login/cli"
	fmt.Fprintf(stdout, "To sign in, open:\n\n    %s\n\nand enter the code: %s\n\n", verifyURL, code)
	_ = openBrowserFn(verifyURL)

	deadline := time.Now().Add(10 * time.Minute)
	for {
		if time.Now().After(deadline) {
			fmt.Fprintln(stderr, "error: login timed out; run `piper login --web` again")
			return 1
		}
		pollSleep(2 * time.Second)
		acc, err := rc.CLILoginPoll(ctx, handle)
		if errors.Is(err, relayclient.ErrAuthPending) {
			continue
		}
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		cc, err := config.LoadClient()
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		cc.RelayAPI = relayAPI
		cc.AccountCredential = acc.AccountCredential
		if err := config.SaveClient(cc); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintf(stdout, "logged in to relay as %s\n", acc.Username)
		if code := enrollAfterLogin(ctx, o, acc.AccountCredential, stdout, stderr); code != 0 {
			return code // identity is durable; re-running login resumes at the claim
		}
		finishInstall(ctx, rc, acc, stdout, stderr)
		return 0
	}
}

// waitForInstall polls the relay until the account's GitHub App installation is
// on record. Both login flows end here when the account has no installation yet:
// the device flow (which cannot install) and `login --web` (where the browser
// was already bounced to the install page). It is how the CLI learns the user
// finished installing — in another tab, or on another device for a headless box.
// The caller's interrupt-aware context drives both the requests and the wait
// between them, so Ctrl-C ends the poll promptly instead of after the current
// interval; installPollTimeout caps the total wait on top of that.
func waitForInstall(ctx context.Context, rc *relayclient.Client, cred, installURL string) error {
	fmt.Printf("Install the Piper GitHub App on the repos you want to deploy:\n  %s\n\nWaiting…", installURL)
	ctx, cancel := context.WithTimeout(ctx, installPollTimeout)
	defer cancel()
	for {
		st, err := rc.GitHubStatus(ctx, cred)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return errInstallTimeout
			}
			return err
		}
		if len(st.Installations) > 0 {
			n := 0
			for _, in := range st.Installations {
				// Best-effort repo count for the message; a transient error here
				// must not fail a login whose install already succeeded.
				if repos, err := rc.GitHubRepos(ctx, cred, in.ID); err == nil {
					n += len(repos)
				}
			}
			fmt.Printf("\rInstalled — %d repo(s) available.\n", n)
			return nil
		}
		fmt.Print(".")
		timer := time.NewTimer(installPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return errInstallTimeout
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// githubRepos lists the repositories the logged-in account's GitHub App
// installations can reach, read live from the relay across every installation.
func githubRepos(stdout, stderr io.Writer) int {
	cc, err := config.LoadClient()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if cc.RelayAPI == "" || cc.AccountCredential == "" {
		fmt.Fprintln(stderr, "error: not logged in to a relay; run `piper login` first")
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	rc := relayclient.New(cc.RelayAPI)
	st, err := rc.GitHubStatus(ctx, cc.AccountCredential)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if len(st.Installations) == 0 {
		fmt.Fprintln(stdout, "No repositories yet — run `piper login` to install the Piper GitHub App on the repos you want to deploy.")
		return 0
	}
	for _, in := range st.Installations {
		repos, err := rc.GitHubRepos(ctx, cc.AccountCredential, in.ID)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		for _, r := range repos {
			if r.Visibility != "" && r.Visibility != "public" {
				fmt.Fprintf(stdout, "%s (%s)\n", r.FullName, r.Visibility)
				continue
			}
			fmt.Fprintln(stdout, r.FullName)
		}
	}
	return 0
}

// agentInstalled reports whether this box has piperd at all — the login probe
// matrix must fail loudly when pointed at a box with no agent (#173). One
// tier: an existing data dir (an enrolled box), a system-managed install, or a
// resolvable piperd binary (deb/brew/manual) all count.
func agentInstalled(dataDir string) bool {
	if _, err := os.Stat(dataDir); err == nil {
		return true
	}
	if config.SystemManaged() {
		return true
	}
	_, err := piperdPath()
	return err == nil
}
