package main

import (
	"os"
	"testing"
)

// TestMain keeps the browser shut for the whole test binary. The openBrowserFn
// seam is not enough on its own: it only helps tests that remember to set it,
// and the ones that do restore it to the *real* opener afterwards — so a test
// that forgot (TestRelayLoginRunsClaimStage) popped a real GitHub device-login
// tab on every `make verify`. Suppressing at the source is the one thing a
// restore cannot undo.
func TestMain(m *testing.M) {
	os.Setenv("PIPER_NO_BROWSER", "1")
	os.Exit(m.Run())
}

// PIPER_NO_BROWSER=1 must keep the browser shut. The e2e suite runs the real
// piper binary as a subprocess, where the in-process openBrowserFn seam cannot
// reach — so suppression has to live in the binary itself.
func TestBrowserCmdSuppressedByEnv(t *testing.T) {
	const url = "https://example.test/device"

	t.Setenv("PIPER_NO_BROWSER", "") // TestMain sets it for the whole binary
	if browserCmd(url) == nil {
		t.Fatal("browserCmd = nil without PIPER_NO_BROWSER; the CLI would never open a browser")
	}

	t.Setenv("PIPER_NO_BROWSER", "1")
	if cmd := browserCmd(url); cmd != nil {
		t.Fatalf("PIPER_NO_BROWSER=1 still builds a browser command: %v", cmd.Args)
	}
	if err := openBrowser(url); err != nil {
		t.Fatalf("openBrowser under PIPER_NO_BROWSER=1 = %v, want nil", err)
	}
}
