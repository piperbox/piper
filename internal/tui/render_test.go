package tui

import "testing"

func TestStatusIcon(t *testing.T) {
	cases := map[string]string{
		"running": "●", "building": "◐", "failed": "✗", "stopped": "○", "": "—",
	}
	for status, want := range cases {
		if got := statusIcon(status); got != want {
			t.Fatalf("statusIcon(%q) = %q, want %q", status, got, want)
		}
	}
}
