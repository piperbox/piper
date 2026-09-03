package systemd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPiperRelayServiceContract(t *testing.T) {
	b, err := os.ReadFile("piper-relay.service")
	if err != nil {
		t.Fatal(err)
	}
	unit := string(b)

	required := []string{
		"After=network-online.target",
		"Wants=network-online.target",
		// Ordering hint for a single-host layout where Postgres runs on the
		// same box; harmless (systemd ignores it) when the unit doesn't exist.
		"After=postgresql.service",
		// The relay log.Fatals if it can't reach Postgres at startup; without
		// this, systemd's default StartLimitBurst gives up restarting after a
		// handful of tries within StartLimitIntervalSec, and a DB that's merely
		// slow to come up on boot leaves the unit permanently failed.
		"StartLimitIntervalSec=0",
		"ExecStart=/usr/local/bin/piper-relay",
		"Environment=PIPER_RELAY_DATA_DIR=/var/lib/piper-relay",
		"EnvironmentFile=-/etc/piper-relay.env",
		"DynamicUser=yes",
		"StateDirectory=piper-relay",
		"AmbientCapabilities=CAP_NET_BIND_SERVICE",
		"CapabilityBoundingSet=CAP_NET_BIND_SERVICE",
		"NoNewPrivileges=yes",
		"ProtectSystem=strict",
		"ProtectHome=yes",
		"Restart=on-failure",
		"RestartSec=2s",
		"WantedBy=multi-user.target",
	}
	for _, directive := range required {
		if !strings.Contains(unit, directive) {
			t.Errorf("unit missing %q", directive)
		}
	}
}

func repositoryFile(t *testing.T, parts ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{"..", ".."}, parts...)...)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestServiceDocumentation(t *testing.T) {
	manual := repositoryFile(t, "docs", "manual-setup.md")
	for _, text := range []string{
		"packaging/systemd/piper-relay.service",
		"systemctl enable --now piper-relay",
	} {
		if !strings.Contains(manual, text) {
			t.Errorf("docs/manual-setup.md missing %q", text)
		}
	}

	runbook := repositoryFile(t, "docs", "runbooks", "git-deploy-e2e.md")
	for _, text := range []string{
		"systemd-run",
		"PIPER_RELAY_DATA_DIR=/var/lib/piper-relay",
		"systemctl enable --now piper-relay",
		"systemctl clean --what=state piper-relay",
		"journalctl -u piper-relay",
		"ss -lnt",
	} {
		if !strings.Contains(runbook, text) {
			t.Errorf("runbook missing %q", text)
		}
	}
}
