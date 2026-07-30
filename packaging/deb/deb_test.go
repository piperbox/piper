package deb

import (
	"os"
	"strings"
	"testing"
)

// TestDebUnitContract pins the deb variant of the unit: identical hardening to
// the manual-install unit, but ExecStart points at the deb's /usr/bin.
func TestDebUnitContract(t *testing.T) {
	b, err := os.ReadFile("piperd.service")
	if err != nil {
		t.Fatal(err)
	}
	unit := string(b)
	if !strings.Contains(unit, "ExecStart=/usr/bin/piperd") {
		t.Error("deb unit must ExecStart=/usr/bin/piperd")
	}
	if strings.Contains(unit, "/usr/local/bin") {
		t.Error("deb unit must not reference /usr/local/bin")
	}
}

// TestDebUnitOnlyDiffersInExecStart guards the two units against drifting
// apart: any hardening change must land in both.
func TestDebUnitOnlyDiffersInExecStart(t *testing.T) {
	deb, err := os.ReadFile("piperd.service")
	if err != nil {
		t.Fatal(err)
	}
	manual, err := os.ReadFile("../systemd/piperd.service")
	if err != nil {
		t.Fatal(err)
	}
	norm := func(b []byte) string {
		return strings.ReplaceAll(string(b), "ExecStart=/usr/bin/piperd", "ExecStart=/usr/local/bin/piperd")
	}
	if norm(deb) != string(manual) {
		t.Error("deb unit and packaging/systemd/piperd.service differ beyond ExecStart — sync them")
	}
}

// TestMaintainerScripts pins the dpkg maintainer-script contract: fresh
// install enables+starts, upgrade restarts only a running service, removal
// disables — all skipped outside systemd (containers/chroots).
func TestMaintainerScripts(t *testing.T) {
	post, err := os.ReadFile("postinstall.sh")
	if err != nil {
		t.Fatal(err)
	}
	pre, err := os.ReadFile("preremove.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`[ "$1" = configure ]`,
		"[ -d /run/systemd/system ]",
		"systemctl daemon-reload",
		"systemctl enable --now piperd",
		"systemctl restart piperd",
	} {
		if !strings.Contains(string(post), want) {
			t.Errorf("postinstall.sh missing %q", want)
		}
	}
	for _, want := range []string{
		`[ "$1" = remove ]`,
		"[ -d /run/systemd/system ]",
		"systemctl disable --now piperd",
	} {
		if !strings.Contains(string(pre), want) {
			t.Errorf("preremove.sh missing %q", want)
		}
	}
}
