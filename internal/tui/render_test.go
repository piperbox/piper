package tui

import "testing"

func TestAppURL(t *testing.T) {
	if got := appURL("", "", false); got != "" {
		t.Fatalf("empty hostname: got %q", got)
	}
	if got := appURL("blog.piper.localhost", "http", false); got != "http://blog.piper.localhost" {
		t.Fatalf("got %q", got)
	}
	if got := appURL("blog.example.dev", "https", true); got != "https://blog.example.dev" {
		t.Fatalf("relay got %q", got)
	}
}

// A never-enrolled box that serves its own domain directly (#507) terminates
// TLS itself, and the CLI reaches it over the LAN — so remote is false while
// the app really is on HTTPS. Deciding the scheme from remote alone printed
// http:// for a URL that only answers on 443, which is why the daemon reports
// the scheme and the client follows it.
func TestAppURLFollowsTheDaemonNotTheDialPath(t *testing.T) {
	if got := appURL("blog.example.dev", "https", false); got != "https://blog.example.dev" {
		t.Fatalf("direct-served box: got %q, want https", got)
	}
	// A daemon that predates the field reports nothing; the dial path is then
	// the only signal left, and the old behaviour is what we fall back to.
	if got := appURL("blog.example.dev", "", true); got != "https://blog.example.dev" {
		t.Fatalf("no scheme reported, remote: got %q", got)
	}
	if got := appURL("blog.piper.localhost", "", false); got != "http://blog.piper.localhost" {
		t.Fatalf("no scheme reported, local: got %q", got)
	}
	// A never-deployed app has no URL whatever the daemon says.
	if got := appURL("", "https", true); got != "" {
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
