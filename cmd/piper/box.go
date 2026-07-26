package main

import (
	"context"
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
