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
