package systemd

import (
	"os"
	"strings"
	"testing"
)

func TestPiperdServiceContract(t *testing.T) {
	b, err := os.ReadFile("piperd.service")
	if err != nil {
		t.Fatal(err)
	}
	unit := string(b)

	required := []string{
		"After=docker.service network-online.target",
		"Wants=docker.service network-online.target",
		"ExecStart=/usr/local/bin/piperd",
		"Environment=PIPER_DATA_DIR=/var/lib/piper",
		"Environment=XDG_DATA_HOME=/var/lib/piper",
		"Environment=XDG_CONFIG_HOME=/var/lib/piper",
		"EnvironmentFile=-/etc/piper/piperd.env",
		"DynamicUser=yes",
		"SupplementaryGroups=docker",
		"StateDirectory=piper",
		"RuntimeDirectory=piper",
		"RuntimeDirectoryMode=0755",
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

func TestPiperdEnvExample(t *testing.T) {
	b, err := os.ReadFile("piperd.env.example")
	if err != nil {
		t.Fatal(err)
	}
	env := string(b)
	for _, text := range []string{
		"PIPER_API_ADDR",
		"PIPER_BASE_DOMAIN",
	} {
		if !strings.Contains(env, text) {
			t.Errorf("env example missing %q", text)
		}
	}
}

// TestPiperdDocumentation pins the one-tier install story in the docs: the
// deleted daemonize/rootless verbs must not resurface, and each platform's
// managed path is named.
func TestPiperdDocumentation(t *testing.T) {
	gettingStarted := repositoryFile(t, "docs", "getting-started.md")
	manualSetup := repositoryFile(t, "docs", "manual-setup.md")
	readme := repositoryFile(t, "README.md")

	for name, doc := range map[string]string{"getting-started": gettingStarted, "manual-setup": manualSetup, "README": readme} {
		if strings.Contains(doc, "daemonize") {
			t.Errorf("%s still mentions daemonize", name)
		}
		if strings.Count(doc, "systemctl --user") > 1 {
			t.Errorf("%s documents the rootless user unit beyond the one-time cleanup note", name)
		}
	}
	for _, want := range []string{"apt install piperd", "brew services start piper"} {
		if !strings.Contains(gettingStarted, want) {
			t.Errorf("getting-started.md missing %q", want)
		}
	}
	if !strings.Contains(manualSetup, "systemctl enable --now piperd") {
		t.Errorf("manual-setup.md missing the manual unit-install command")
	}
}
