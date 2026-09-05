package compose

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func repositoryFile(t *testing.T, parts ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{"..", ".."}, parts...)...)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDockerfileContract(t *testing.T) {
	dockerfile := repositoryFile(t, "Dockerfile")

	required := []string{
		"FROM --platform=$BUILDPLATFORM golang:1.26 AS build",
		"ARG TARGETOS",
		"ARG TARGETARCH",
		"GOOS=$TARGETOS GOARCH=$TARGETARCH",
		"CGO_ENABLED=0",
		"./cmd/piperd",
		"FROM gcr.io/distroless/static-debian12",
		"ENV PIPER_DATA_DIR=/var/lib/piper",
		"VOLUME /var/lib/piper",
		`ENTRYPOINT ["/usr/local/bin/piperd"]`,
	}
	for _, directive := range required {
		if !strings.Contains(dockerfile, directive) {
			t.Errorf("Dockerfile missing %q", directive)
		}
	}
}

func TestComposeContract(t *testing.T) {
	b, err := os.ReadFile("docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	compose := string(b)

	required := []string{
		"network_mode: host",
		"/var/run/docker.sock:/var/run/docker.sock",
		"piper_data:/var/lib/piper",
		"PIPER_DATA_DIR=/var/lib/piper",
		"../../packaging/systemd/piperd.env.example",
	}
	for _, directive := range required {
		if !strings.Contains(compose, directive) {
			t.Errorf("docker-compose.yml missing %q", directive)
		}
	}
}

func TestDockerDocumentation(t *testing.T) {
	manual := repositoryFile(t, "docs", "manual-setup.md")
	for _, text := range []string{
		"docker compose -f deploy/compose/docker-compose.yml up -d --build",
		"network_mode: host",
		"root-equivalent",
	} {
		if !strings.Contains(manual, text) {
			t.Errorf("docs/manual-setup.md missing %q", text)
		}
	}

	// The Compose pointer moved from the README into the getting-started
	// guide when the README was slimmed to a quick start (see #181).
	guide := repositoryFile(t, "docs", "getting-started.md")
	if !strings.Contains(guide, "run piperd in Docker via Compose") {
		t.Errorf("docs/getting-started.md missing pointer phrase %q", "run piperd in Docker via Compose")
	}
}

// The relay drains on SIGTERM (#523): up to 20 s for open streams, 5 s to
// leave the pool, 35 s for webhook deliveries to park. Compose's default
// 10 s stop grace would SIGKILL it mid-drain.
func TestRelayComposeGivesTheDrainItsBudget(t *testing.T) {
	compose := repositoryFile(t, "deploy", "compose", "relay", "docker-compose.yml")
	if !strings.Contains(compose, "stop_grace_period: 60s") {
		t.Error("relay/docker-compose.yml missing \"stop_grace_period: 60s\" on the relay service")
	}
}
