package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/piperbox/piper/internal/client"
	"github.com/piperbox/piper/internal/config"
	"github.com/piperbox/piper/internal/enrollapi"
	"github.com/piperbox/piper/internal/relayclient"
)

// enrollFlowOpts parameterizes the claim half of the merged `piper login`.
type enrollFlowOpts struct {
	relayAPI string
	dataDir  string
	org      string
	noEnroll bool
	reEnroll bool
	relogin  bool
}

// Seams so tests run instantly.
var (
	enrollApplyTimeout = 60 * time.Second
	enrollPollInterval = time.Second
)

// enrollSocketDial probes one candidate socket and positively identifies
// piperd behind it (GET /v1/version); a var so tests can stub the probe.
var enrollSocketDial = func(path string) (*client.Client, bool) {
	c := client.NewUnix(path)
	if _, err := c.AgentVersion(); err != nil {
		return nil, false
	}
	return c, true
}

// findEnrollSocket returns a client on the first live enrollment socket.
func findEnrollSocket(dataDir string) (*client.Client, bool) {
	for _, p := range config.EnrollSocketCandidates(dataDir) {
		if c, ok := enrollSocketDial(p); ok {
			return c, true
		}
	}
	return nil, false
}

// enrollAfterLogin is the claim stage of the merged `piper login` (one-command
// login design). The caller has already persisted the account credential, so
// identity is durable no matter what happens here; this stage's own discipline
// is staged too — a hard claim failure exits 1 saying so, and once piperd has
// persisted the enrollment a pending tunnel is advisory (#297 extended).
func enrollAfterLogin(ctx context.Context, o enrollFlowOpts, cred string, stdout, stderr io.Writer) int {
	if o.noEnroll {
		return 0
	}
	c, ok := findEnrollSocket(o.dataDir)
	if !ok {
		if agentInstalled(o.dataDir) {
			fmt.Fprintln(stderr, "logged in, but this box is NOT connected — piperd is installed but not running.")
			fmt.Fprintln(stderr, "start it with `piper agent up`, then run `piper login` again.")
			return 1
		}
		fmt.Fprintln(stdout, "identity only — no piperd on this machine; run `piper login` on a box to connect it.")
		return 0
	}
	st, err := c.RelayStatus()
	if errors.Is(err, client.ErrRelayStatusUnsupported) {
		fmt.Fprintln(stderr, "logged in, but this piperd predates one-command login (a stale process or old install).")
		fmt.Fprintln(stderr, "restart it (`piper agent down`, then `piper agent up`) and run `piper login` again.")
		return 1
	}
	if err != nil {
		fmt.Fprintln(stderr, "error: cannot read piperd's relay status:", err)
		return 1
	}
	if st.EnvManaged {
		fmt.Fprintln(stdout, "this box's enrollment is operator-managed via "+config.SystemEnvFile()+" — nothing to do.")
		return 0
	}
	if st.Enrolled && !o.reEnroll {
		// Staleness cross-check: enrolled locally but unknown to the account
		// (piper box rm, account/relay switch) would otherwise deadlock — the
		// claim would be skipped forever with no verb left to fix it.
		agents, err := relayclient.New(o.relayAPI).Agents(ctx, cred)
		if err == nil {
			known := false
			for _, a := range agents {
				if a.BaseDomain == st.BaseDomain {
					known = true
					break
				}
			}
			if !known {
				fmt.Fprintf(stderr, "this box is enrolled as %s, but that box is not on your account (removed with `piper box rm`, or a different account/relay).\n", st.BaseDomain)
				fmt.Fprintln(stderr, "run `piper login --re-enroll` to claim it fresh.")
				return 1
			}
		}
		fmt.Fprintf(stdout, "already enrolled as %s\n", st.BaseDomain)
		return waitConnected(ctx, o.dataDir, st.BaseDomain, stdout, stderr)
	}
	fmt.Fprintln(stdout, "claiming this box…")
	resp, err := c.EnrollRelay(enrollapi.EnrollRequest{
		RelayAPI: o.relayAPI, AccountCredential: cred, Org: o.org, Replace: o.reEnroll,
	})
	var already *client.AlreadyEnrolledError
	switch {
	case errors.Is(err, client.ErrEnvManaged):
		fmt.Fprintln(stdout, "this box's enrollment is operator-managed via "+config.SystemEnvFile()+" — nothing to do.")
		return 0
	case errors.As(err, &already):
		// Raced with another login; treat like the enrolled path.
		fmt.Fprintf(stdout, "already enrolled as %s\n", already.BaseDomain)
		return waitConnected(ctx, o.dataDir, already.BaseDomain, stdout, stderr)
	case errors.Is(err, client.ErrEnrollQuota):
		fmt.Fprintln(stderr, "error: account agent quota exceeded")
		fmt.Fprintln(stderr, "run `piper box ls` to see your boxes, then `piper box rm <base-domain>` to free a slot")
		return 1
	case errors.Is(err, client.ErrEnrollCredential):
		fmt.Fprintln(stderr, "error: relay rejected your account credential; run `piper login` again")
		return 1
	case errors.Is(err, client.ErrEnrollBusy):
		fmt.Fprintln(stderr, "error: piperd is busy (a deployment is building, or another enrollment is in flight); retry shortly")
		return 1
	case err != nil:
		// A transport drop here can mean the apply already tore the listener
		// down; the status poll below settles whether the claim persisted.
		fmt.Fprintf(stderr, "note: enrollment response lost (%v); checking whether it applied…\n", err)
		return waitConnected(ctx, o.dataDir, "", stdout, stderr)
	}
	fmt.Fprintf(stdout, "enrolled as %s\napplying…\n", resp.BaseDomain)
	return waitConnected(ctx, o.dataDir, resp.BaseDomain, stdout, stderr)
}

// waitConnected polls the enrollment socket (re-finding it: the re-exec
// replaces the listener) until the tunnel reports connected. Once piperd has
// persisted the enrollment a quiet deadline is ADVISORY — exit 0 with a note,
// the tunnel client retries in the background — while a recorded handshake
// rejection is definitive. A socket that never comes back is a hard failure
// with a per-platform diagnosis hint. ctx cancellation (Ctrl-C) is a
// cancellation point on every loop iteration (#297 precedent): once the
// enrollment has been observed persisted it is treated the same as the quiet
// deadline (advisory, exit 0); otherwise it is a hard interrupt (exit 1).
func waitConnected(ctx context.Context, dataDir, baseDomain string, stdout, stderr io.Writer) int {
	deadline := time.Now().Add(enrollApplyTimeout)
	sawStatus := false
	enrolled := false
	for ctx.Err() == nil && time.Now().Before(deadline) {
		if c, ok := findEnrollSocket(dataDir); ok {
			if st, err := c.RelayStatus(); err == nil {
				sawStatus = true
				enrolled = enrolled || st.Enrolled
				if st.BaseDomain != "" {
					baseDomain = st.BaseDomain
				}
				if st.Tunnel == "connected" {
					fmt.Fprintf(stdout, "piperd connected — this box is live (apps at https://<app>.%s)\n", baseDomain)
					return 0
				}
				if strings.Contains(st.LastTunnelError, "rejected") {
					fmt.Fprintf(stderr, "error: the relay rejected this box's enrollment: %s\n", st.LastTunnelError)
					fmt.Fprintln(stderr, "run `piper login --re-enroll` to claim it fresh.")
					return 1
				}
			}
		}
		// Read the seams on this goroutine, not the sleeper's: a cancelled wait
		// abandons the sleeper, which would otherwise still be reading these
		// package-level vars while a test's Cleanup restores them (#467).
		sleep, interval := pollSleep, enrollPollInterval
		slept := make(chan struct{})
		go func() { sleep(interval); close(slept) }()
		select {
		case <-ctx.Done():
		case <-slept:
		}
	}
	if ctx.Err() != nil {
		if enrolled {
			fmt.Fprintf(stdout, "enrollment applied; the tunnel is still retrying in the background — check later with `piper box ls`.\n")
			return 0
		}
		fmt.Fprintln(stderr, "interrupted before the enrollment was confirmed applied.")
		return 1
	}
	if !sawStatus {
		fmt.Fprintln(stderr, "error: piperd did not come back after applying the enrollment.")
		fmt.Fprintln(stderr, applyDiagnosisHint())
		return 1
	}
	if !enrolled {
		// piperd answered every poll and never claimed to be enrolled: the
		// daemon is fine, the claim is what failed (reachable via the
		// lost-response path, where the enroll POST's reply never arrived and
		// this loop is what settles whether it applied). Service logs are the
		// wrong place to send someone whose service is plainly up.
		fmt.Fprintln(stderr, "error: piperd is running, but the enrollment did not persist.")
		fmt.Fprintln(stderr, "run `piper login --re-enroll` to claim this box again; if it keeps failing, check piperd's log for the claim error.")
		return 1
	}
	fmt.Fprintf(stdout, "enrollment applied; the tunnel is still retrying in the background — check later with `piper box ls`.\n")
	return 0
}

// applyDiagnosisHint names where to look when the daemon vanished mid-apply.
func applyDiagnosisHint() string {
	if agentGOOS == "darwin" {
		return "check: brew services info piper — logs: $(brew --prefix)/var/log/piperd.err.log"
	}
	if out, err := systemctlRun("is-active", "piperd"); err == nil || strings.TrimSpace(out) != "" {
		return "piperd service state: " + strings.TrimSpace(out) + " — check: journalctl -u piperd -n 50"
	}
	return "check: systemctl status piperd; logs: journalctl -u piperd -n 50"
}
