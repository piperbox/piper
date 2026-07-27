package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/piperbox/piper/internal/config"
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

// A relay rejecting the stored account credential must point at `piper
// login`, the same remedy `connect` already gives — not the bare
// "error: relay rejected account credential" a generic error branch would print.
func TestBoxListBadCredentialSuggestsLogin(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	if err := config.SaveClient(config.ClientConfig{
		Addr: "http://127.0.0.1:8088", RelayAPI: srv.URL, AccountCredential: "stale-cred",
	}); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := boxList(&out, &errb); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "piper login") {
		t.Fatalf("stderr = %q, want a `piper login` hint", errb.String())
	}
}

// Same remedy for `box rm` hitting a rejected credential.
func TestBoxRemoveBadCredentialSuggestsLogin(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	if err := config.SaveClient(config.ClientConfig{
		Addr: "http://127.0.0.1:8088", RelayAPI: srv.URL, AccountCredential: "stale-cred",
	}); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := boxRemove("b.example", true, &out, &errb); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "piper login") {
		t.Fatalf("stderr = %q, want a `piper login` hint", errb.String())
	}
}

// DeleteAgent also clears custom_domains now, so the success message must not
// claim custom domains stay attached — only the relay-assigned app URLs do.
func TestBoxRemoveSuccessMessageReflectsReleasedCustomDomains(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	if err := config.SaveClient(config.ClientConfig{
		Addr: "http://127.0.0.1:8088", RelayAPI: srv.URL, AccountCredential: "cred-xyz",
	}); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := boxRemove("b.example", true, &out, &errb); code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	if strings.Contains(out.String(), "only the box slot is freed") {
		t.Fatalf("stdout = %q, must not claim custom domains stay attached (DeleteAgent releases them)", out.String())
	}
	if !strings.Contains(out.String(), "custom domain") {
		t.Fatalf("stdout = %q, want it to say custom domains are released", out.String())
	}
}

// Since #405 removal reclaims the box's app slots, so the message must not
// still tell users their app URLs stay reserved on the account.
func TestBoxRemoveSuccessMessageReflectsReclaimedAppSlots(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	if err := config.SaveClient(config.ClientConfig{
		Addr: "http://127.0.0.1:8088", RelayAPI: srv.URL, AccountCredential: "cred-xyz",
	}); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := boxRemove("b.example", true, &out, &errb); code != 0 {
		t.Fatalf("code = %d, err = %s", code, errb.String())
	}
	if strings.Contains(out.String(), "stay reserved") {
		t.Fatalf("stdout = %q, must not claim app URLs stay reserved (DeleteAgent now frees them)", out.String())
	}
	if !strings.Contains(out.String(), "released") {
		t.Fatalf("stdout = %q, want it to say the app URLs and domains are released", out.String())
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
