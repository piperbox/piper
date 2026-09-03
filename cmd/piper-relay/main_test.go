package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/piperbox/piper/internal/relay"
	"github.com/piperbox/piper/internal/relay/relaytest"
	"github.com/piperbox/piper/internal/version"
)

func TestMain(m *testing.M) { os.Exit(relaytest.Main(m)) }

func TestRunAdminDisable(t *testing.T) {
	st, err := relay.Open(relaytest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	acc, err := st.UpsertAccount("sub-1", "leo")
	if err != nil {
		t.Fatal(err)
	}

	if err := runAdmin(st, []string{"disable", acc.Username}); err != nil {
		t.Fatalf("runAdmin disable: %v", err)
	}
	// Disabling again on a real account still succeeds (idempotent-ish); an
	// unknown username must error.
	if err := runAdmin(st, []string{"disable", "no-such-user"}); err == nil {
		t.Fatal("runAdmin disable unknown user succeeded, want error")
	}
}

func TestRunAdminDisableOrg(t *testing.T) {
	st, err := relay.Open(relaytest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	alice, err := st.UpsertAccount("sub-1", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateOrg(alice.ID, "acme"); err != nil {
		t.Fatal(err)
	}

	// A bare name means the user; --org picks the org of the same name (#411).
	if err := runAdmin(st, []string{"disable", "--org", "acme"}); err != nil {
		t.Fatalf("runAdmin disable --org: %v", err)
	}
	// The user "acme" is untouched, so disabling it must still find a row.
	if err := runAdmin(st, []string{"disable", "acme"}); err != nil {
		t.Fatalf("runAdmin disable user: %v", err)
	}
}

func TestRunAdminUsage(t *testing.T) {
	st, _ := relay.Open(relaytest.DSN(t))
	defer st.Close()
	if err := runAdmin(st, []string{"disable"}); err == nil {
		t.Fatal("runAdmin with no username succeeded, want usage error")
	}
}

func TestParseWebRedirects(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"root path kept", "https://dash.getpiper.co/", []string{"https://dash.getpiper.co/"}},
		{"subpath kept", "https://dash.getpiper.co/auth", []string{"https://dash.getpiper.co/auth"}},
		{"no path dropped", "https://dash.getpiper.co", nil},
		{"no scheme dropped", "dash.getpiper.co/", nil},
		{"empty dropped", "", nil},
		{"whitespace-only dropped", "   ", nil},
		{"mixed list", "https://dash.getpiper.co/, https://dash.getpiper.co, https://ok.example/x",
			[]string{"https://dash.getpiper.co/", "https://ok.example/x"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseWebRedirects(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("parseWebRedirects(%q) = %v, want %v", c.in, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("parseWebRedirects(%q) = %v, want %v", c.in, got, c.want)
				}
			}
		})
	}
}

func TestApiAddrIsLoopback(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{":8080", false},
		{"0.0.0.0:8080", false},
		{"127.0.0.1:8080", true},
		{"localhost:8080", true},
		{"[::1]:8080", true},
		{"192.168.1.5:8080", false},
	}
	for _, c := range cases {
		if got := apiAddrIsLoopback(c.addr); got != c.want {
			t.Errorf("apiAddrIsLoopback(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}

// piper-relay must have a version surface so the release ldflags stamp lands
// and the binary can report its build. #61.
func TestVersionRequested(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}} {
		if !versionRequested(args) {
			t.Errorf("versionRequested(%v) = false, want true", args)
		}
	}
	for _, args := range [][]string{nil, {"admin"}, {"enroll"}} {
		if versionRequested(args) {
			t.Errorf("versionRequested(%v) = true, want false", args)
		}
	}
	if version.String() == "" {
		t.Error("version.String() is empty")
	}
}

// TestReadAppKeyMode pins which file modes the GitHub App key may carry.
// systemd stages LoadCredential= files at 0440 inside a per-unit 0700 tmpfs —
// the documented way to hand a key to a DynamicUser= service, and the way the
// relay's own TLS cert already arrives — so group-readable must be accepted.
// World-readable must not be: `scp` defaults to 0644, which is the realistic
// way a key gets exposed on a shared box.
func TestReadAppKeyMode(t *testing.T) {
	for _, tc := range []struct {
		mode    os.FileMode
		wantErr bool
	}{
		{0o600, false},
		{0o400, false},
		{0o440, false}, // systemd credential
		{0o644, true},  // scp default
		{0o604, true},
		{0o666, true},
	} {
		path := filepath.Join(t.TempDir(), "github-app.pem")
		if err := os.WriteFile(path, []byte("-----BEGIN RSA PRIVATE KEY-----"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, tc.mode); err != nil {
			t.Fatal(err)
		}
		got, err := readAppKey(path)
		if tc.wantErr {
			if err == nil {
				t.Errorf("mode %o: accepted a world-readable key", tc.mode)
			}
			continue
		}
		if err != nil {
			t.Errorf("mode %o: %v", tc.mode, err)
		} else if len(got) == 0 {
			t.Errorf("mode %o: read no bytes", tc.mode)
		}
	}
}

func TestReadAppKeyMissing(t *testing.T) {
	if _, err := readAppKey(filepath.Join(t.TempDir(), "nope.pem")); err == nil {
		t.Fatal("missing key file accepted")
	}
}
