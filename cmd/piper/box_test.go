package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestCmdBoxRejectsUnknownSubcommand(t *testing.T) {
	var out, errb bytes.Buffer
	if code := cmdBox([]string{"frobnicate"}, &out, &errb); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "usage: piper box") {
		t.Fatalf("stderr = %q, want the box usage line", errb.String())
	}
}

func TestCmdBoxNoArgsPrintsUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := cmdBox(nil, &out, &errb); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "usage: piper box") {
		t.Fatalf("stderr = %q, want the box usage line", errb.String())
	}
}

func TestBoxListRendersAgents(t *testing.T) {
	rendered := renderBoxes([]boxRow{
		{BaseDomain: "a1.example", Owner: "alice", Connected: true},
		{BaseDomain: "a2.example", Owner: "alice", Connected: false},
	})
	for _, want := range []string{"a1.example", "connected", "a2.example", "offline", "alice"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered listing missing %q:\n%s", want, rendered)
		}
	}
}

func TestBoxListSaysSoWhenEmpty(t *testing.T) {
	if got := renderBoxes(nil); !strings.Contains(got, "no boxes") {
		t.Fatalf("empty listing = %q, want it to say there are no boxes", got)
	}
}

func TestCmdBoxRmNeedsABaseDomain(t *testing.T) {
	var out, errb bytes.Buffer
	if code := cmdBox([]string{"rm"}, &out, &errb); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "usage: piper box rm") {
		t.Fatalf("stderr = %q, want the rm usage line", errb.String())
	}
}

// The documented invocation order — positional base-domain, then --yes — must
// actually parse: Go's flag package stops at the first non-flag argument, so
// naively doing fs.Parse(args[1:]) leaves --yes in NArg() and the command
// wrongly bails out to usage.
func TestCmdBoxRmAcceptsPositionalThenFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var out, errb bytes.Buffer
	code := cmdBox([]string{"rm", "b.example", "--yes"}, &out, &errb)
	if code != 1 {
		t.Fatalf("code = %d, want 1 (should reach boxRemove and fail for lack of login)", code)
	}
	if strings.Contains(errb.String(), "usage: piper box rm") {
		t.Fatalf("stderr = %q, must not print usage for a valid invocation", errb.String())
	}
	if !strings.Contains(errb.String(), "not logged in") {
		t.Fatalf("stderr = %q, want the not-logged-in message proving boxRemove ran with yes=true (no confirmation prompt)", errb.String())
	}
}

// Declining the prompt must make no request at all — the check has to happen
// before the relay is dialed, or a "no" would still have removed the box.
func TestBoxRemoveAbortsOnDeclinedPrompt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	oldStdin := stdinReader
	stdinReader = strings.NewReader("n\n")
	defer func() { stdinReader = oldStdin }()

	var out, errb bytes.Buffer
	if code := boxRemove("a1.example", false, &out, &errb); code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "aborted") {
		t.Fatalf("stdout = %q, want an abort notice", out.String())
	}
	if errb.Len() != 0 {
		t.Fatalf("stderr = %q, want empty (no relay call attempted)", errb.String())
	}
}
