package tui

import (
	"testing"
)

// The TUI's own opener honours PIPER_NO_BROWSER=1 too — it is a second copy of
// the same OS-dispatch (see manifest.go), so a knob on only one of them would
// still pop a tab through the other.
func TestBrowserCmdSuppressedByEnv(t *testing.T) {
	const url = "https://example.test/device"

	t.Setenv("PIPER_NO_BROWSER", "") // don't inherit a suppressed parent env
	if browserCmd(url) == nil {
		t.Fatal("browserCmd = nil without PIPER_NO_BROWSER; the TUI would never open a browser")
	}

	t.Setenv("PIPER_NO_BROWSER", "1")
	if cmd := browserCmd(url); cmd != nil {
		t.Fatalf("PIPER_NO_BROWSER=1 still builds a browser command: %v", cmd.Args)
	}
	if err := openBrowser(url); err != nil {
		t.Fatalf("openBrowser under PIPER_NO_BROWSER=1 = %v, want nil", err)
	}
}
