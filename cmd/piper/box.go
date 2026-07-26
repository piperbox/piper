package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/piperbox/piper/internal/config"
	"github.com/piperbox/piper/internal/relayclient"
)

const boxUsage = "usage: piper box ls | piper box rm <base-domain> [--yes]"

// boxRow is one line of `piper box ls`. It exists so rendering can be tested
// without a relay: renderBoxes is pure, and the command is the thin part.
type boxRow struct {
	BaseDomain string
	Owner      string
	Connected  bool
}

func cmdBox(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, boxUsage)
		return 2
	}
	switch args[0] {
	case "ls":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "usage: piper box ls")
			return 2
		}
		return boxList(stdout, stderr)
	case "rm":
		fs := flag.NewFlagSet("rm", flag.ContinueOnError)
		fs.SetOutput(stderr)
		yes := fs.Bool("yes", false, "skip the confirmation prompt")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if fs.NArg() != 1 {
			fmt.Fprintln(stderr, "usage: piper box rm <base-domain> [--yes]")
			return 2
		}
		return boxRemove(fs.Arg(0), *yes, stdout, stderr)
	default:
		fmt.Fprintln(stderr, boxUsage)
		return 2
	}
}

// renderBoxes formats the listing. Connectedness is the relay's live view, so
// "offline" here is exactly what makes a box removable.
func renderBoxes(rows []boxRow) string {
	if len(rows) == 0 {
		return "no boxes on this account — run `piper connect` on a box to claim one\n"
	}
	var b strings.Builder
	for _, r := range rows {
		state := "offline"
		if r.Connected {
			state = "connected"
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\n", r.BaseDomain, r.Owner, state)
	}
	return b.String()
}

// relayAccount loads the relay API base and account credential the box
// commands need, or explains that a login is missing.
func relayAccount(stderr io.Writer) (api, cred string, ok bool) {
	cc, err := config.LoadClient()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return "", "", false
	}
	if cc.RelayAPI == "" || cc.AccountCredential == "" {
		fmt.Fprintln(stderr, "error: not logged in to a relay; run `piper login` first")
		return "", "", false
	}
	return cc.RelayAPI, cc.AccountCredential, true
}

func boxList(stdout, stderr io.Writer) int {
	api, cred, ok := relayAccount(stderr)
	if !ok {
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	agents, err := relayclient.New(api).Agents(ctx, cred)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	rows := make([]boxRow, 0, len(agents))
	for _, a := range agents {
		rows = append(rows, boxRow{BaseDomain: a.BaseDomain, Owner: a.Owner, Connected: a.Connected})
	}
	fmt.Fprint(stdout, renderBoxes(rows))
	return 0
}

// boxRemove retires a box, freeing its agent slot. The confirmation comes
// before the relay is dialed: removal cannot be undone — the enrollment token
// is gone and the box must run `piper connect` again — so a declined prompt
// must not have sent anything.
//
// Removing a box does not free its app-cap slots. The relay's hostnames table
// keys on the account, not the agent, so it cannot tell which URLs were this
// box's; saying so here is better than a user inferring it from a still-full
// app quota.
func boxRemove(baseDomain string, yes bool, stdout, stderr io.Writer) int {
	if !yes && !confirmPrompt(stdout, "remove "+baseDomain+"? it must run `piper connect` again to come back") {
		fmt.Fprintln(stdout, "aborted")
		return 0
	}
	api, cred, ok := relayAccount(stderr)
	if !ok {
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	switch err := relayclient.New(api).RemoveAgent(ctx, cred, baseDomain); {
	case err == nil:
		fmt.Fprintf(stdout, "removed %s\n", baseDomain)
		fmt.Fprintln(stdout, "its app URLs stay reserved on the account; only the box slot is freed.")
		return 0
	case errors.Is(err, relayclient.ErrAgentConnected):
		fmt.Fprintf(stderr, "error: %s is still connected — stop piperd on that box, then retry\n", baseDomain)
		return 1
	case errors.Is(err, relayclient.ErrNoAgent):
		fmt.Fprintf(stderr, "error: no box %s on this account — run `piper box ls` to see them\n", baseDomain)
		return 1
	default:
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
}
