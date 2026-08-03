package main

import (
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

func formatAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return d.Round(time.Second).String()
}

const envUsage = "usage: piper env <app> <set KEY=VALUE [KEY2=VALUE2 ...] | ls [--show] | rm KEY>"

// cmdEnv drives per-app environment variables: a thin client over
// /v1/apps/<app>/env. Vercel semantics — changes are saved immediately and
// applied on the app's next deploy or restart, never by bouncing the
// running container.
func cmdEnv(remote string, args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintln(stderr, envUsage)
		return 2
	}
	app, sub, rest := args[0], args[1], args[2:]
	c, ok := dialClient(remote, stderr)
	if !ok {
		return 1
	}
	switch sub {
	case "set":
		if len(rest) == 0 {
			fmt.Fprintln(stderr, envUsage)
			return 2
		}
		// Parse everything before posting anything, so a malformed later arg
		// can't leave the app half-updated.
		type pair struct{ key, value string }
		pairs := make([]pair, 0, len(rest))
		for _, kv := range rest {
			key, value, found := strings.Cut(kv, "=")
			if !found || key == "" {
				fmt.Fprintf(stderr, "error: %q is not KEY=VALUE\n", kv)
				return 2
			}
			pairs = append(pairs, pair{key, value})
		}
		for _, p := range pairs {
			if err := c.SetAppEnv(app, p.key, p.value); err != nil {
				fmt.Fprintln(stderr, "error:", err)
				return 1
			}
		}
		fmt.Fprintf(stdout, "saved %d var(s) — applied on the next deploy or restart of %s\n", len(rest), app)
		return 0
	case "ls":
		fs := flag.NewFlagSet("env ls", flag.ContinueOnError)
		fs.SetOutput(stderr)
		show := fs.Bool("show", false, "print values instead of masking them")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if fs.NArg() != 0 {
			fmt.Fprintln(stderr, "usage: piper env <app> ls [--show]")
			return 2
		}
		env, updated, err := c.AppEnvWithTimestamps(app)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		keys := make([]string, 0, len(env))
		for k := range env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := "******"
			if *show {
				v = env[k]
			}
			age := formatAge(time.Since(updated[k]))
			fmt.Fprintf(stdout, "%s=%s  (%s)\n", k, v, age)
		}
		return 0
	case "rm":
		if len(rest) != 1 {
			fmt.Fprintln(stderr, envUsage)
			return 2
		}
		if err := c.DeleteAppEnv(app, rest[0]); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintf(stdout, "removed %s — applied on the next deploy or restart of %s\n", rest[0], app)
		return 0
	default:
		fmt.Fprintln(stderr, envUsage)
		return 2
	}
}
