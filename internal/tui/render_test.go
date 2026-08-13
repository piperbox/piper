package tui

import "testing"

func TestAppURL(t *testing.T) {
	if got := appURL("", "https"); got != "" {
		t.Fatalf("empty hostname: got %q", got)
	}
	if got := appURL("blog.piper.localhost", "http"); got != "http://blog.piper.localhost" {
		t.Fatalf("got %q", got)
	}
	if got := appURL("blog.example.dev", "https"); got != "https://blog.example.dev" {
		t.Fatalf("relay got %q", got)
	}
}

// A never-enrolled box that serves its own domain directly (#507) terminates
// TLS itself, and the TUI reaches it over the LAN — so the dial path says
// "local" while the app really is on HTTPS. Deciding the scheme from the dial
// printed http:// for a URL that only answers on 443, which is why the daemon
// reports the scheme and every client follows it, with nothing left to infer
// it from.
func TestAppURLFollowsTheDaemonNotTheDialPath(t *testing.T) {
	if got := appURL("blog.example.dev", "https"); got != "https://blog.example.dev" {
		t.Fatalf("direct-served box: got %q, want https", got)
	}
	// A never-deployed app has no URL whatever the daemon says.
	if got := appURL("", "https"); got != "" {
		t.Fatalf("empty hostname: got %q", got)
	}
}

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
