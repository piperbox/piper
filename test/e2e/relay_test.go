package e2e

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/piperbox/piper/internal/client"
)

// TestRelayLoopback proves the full relay path locally: browser→relay:8443
// (SNI)→tunnel→piperd→Caddy:443(TLS)→container. Self-signed wildcard cert, no
// ACME, no real DNS. Uses :8443 and :7000 to avoid privileged :443.
func TestRelayLoopback(t *testing.T) {
	if os.Getenv("RUN_E2E") != "1" {
		t.Skip("set RUN_E2E=1 to run (needs Docker; Caddy is embedded)")
	}
	repoRoot, _ := filepath.Abs("../..")
	base := "alice.localhost"

	// Self-signed wildcard cert for *.alice.localhost.
	certFile, keyF := writeSelfSigned(t, base)

	// Build both binaries.
	binDir := t.TempDir()
	for _, c := range []string{"piperd", "piper-relay"} {
		b := exec.Command("go", "build", "-o", filepath.Join(binDir, c), "./cmd/"+c)
		b.Dir = repoRoot
		if out, err := b.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", c, err, out)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Enroll an agent against the relay's store, capture the token.
	relayData := t.TempDir()
	enroll := exec.Command(filepath.Join(binDir, "piper-relay"), "enroll", "alice", "--domain", base)
	enroll.Env = append(os.Environ(), "PIPER_RELAY_DATA_DIR="+relayData)
	out, err := enroll.CombinedOutput()
	if err != nil {
		t.Fatalf("enroll: %v\n%s", err, out)
	}
	token := parseToken(t, string(out))

	// Start the relay (TLS :8443, tunnel :7000).
	relay := exec.CommandContext(ctx, filepath.Join(binDir, "piper-relay"))
	relay.Env = append(os.Environ(),
		"PIPER_RELAY_DATA_DIR="+relayData,
		"PIPER_RELAY_TLS_ADDR=127.0.0.1:8443",
		"PIPER_RELAY_HTTP_ADDR=127.0.0.1:8880",
		"PIPER_RELAY_TUNNEL_ADDR=127.0.0.1:7000",
	)
	relay.Stdout, relay.Stderr = os.Stdout, os.Stderr
	if err := relay.Start(); err != nil {
		t.Fatalf("start relay: %v", err)
	}
	killOnCleanup(t, relay)
	waitPort(t, "127.0.0.1:7000", 10*time.Second)

	// Mint a control-API token before starting piperd, so there's only one
	// writer to piper.db at a time.
	piperdDataDir := t.TempDir()
	tokenCmd := exec.Command(filepath.Join(binDir, "piperd"), "token", "create", "--name", "e2e")
	tokenCmd.Env = append(os.Environ(), "PIPER_DATA_DIR="+piperdDataDir)
	tokenOut, err := tokenCmd.Output()
	if err != nil {
		t.Fatalf("token create: %v", err)
	}
	apiToken := strings.TrimSpace(string(tokenOut))
	if apiToken == "" {
		t.Fatal("token create: empty token")
	}

	// Start piperd in relay mode with the static cert.
	pd := exec.CommandContext(ctx, filepath.Join(binDir, "piperd"))
	pd.Env = append(os.Environ(),
		"PIPER_DATA_DIR="+piperdDataDir,
		"PIPER_API_ADDR=127.0.0.1:8088",
		"PIPER_BASE_DOMAIN="+base,
		"PIPER_RELAY_ADDR=127.0.0.1:7000",
		"PIPER_RELAY_TOKEN="+token,
		"PIPER_TLS_CERT_FILE="+certFile,
		"PIPER_TLS_KEY_FILE="+keyF,
	)
	pd.Stdout, pd.Stderr = os.Stdout, os.Stderr
	if err := pd.Start(); err != nil {
		t.Fatalf("start piperd: %v", err)
	}
	killOnCleanup(t, pd)
	waitPort(t, "127.0.0.1:8088", 15*time.Second)

	// Deploy the sample app.
	c := client.New("http://127.0.0.1:8088", apiToken)
	if err := c.CreateApp("blog", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if _, err := c.Deploy("blog", filepath.Join(repoRoot, "test/e2e/sampleapp")); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	// Fetch through the relay's TLS port by SNI blog.alice.localhost.
	dialer := &tls.Dialer{Config: &tls.Config{ServerName: "blog." + base, InsecureSkipVerify: true}}
	var body string
	for i := 0; i < 30; i++ {
		conn, err := dialer.DialContext(ctx, "tcp", "127.0.0.1:8443")
		if err == nil {
			fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: blog.%s\r\nConnection: close\r\n\r\n", base)
			b, _ := io.ReadAll(conn)
			conn.Close()
			body = string(b)
			if body != "" {
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if body == "" {
		t.Fatal("no response through the relay")
	}
	fmt.Printf("relay e2e response:\n%s\n", body)
}

func writeSelfSigned(t *testing.T, base string) (certFile, keyF string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "*." + base},
		DNSNames:     []string{"*." + base, base},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyF = filepath.Join(dir, "key.pem")
	certOut, _ := os.Create(certFile)
	pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	certOut.Close()
	keyBytes, _ := x509.MarshalECPrivateKey(key)
	keyOut, _ := os.Create(keyF)
	pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})
	keyOut.Close()
	return certFile, keyF
}

func parseToken(t *testing.T, out string) string {
	t.Helper()
	const marker = "token: "
	i := strings.Index(out, marker)
	if i < 0 {
		t.Fatalf("no token in enroll output: %q", out)
	}
	return strings.TrimSpace(out[i+len(marker):])
}
