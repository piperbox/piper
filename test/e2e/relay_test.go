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

// TestRelayLoopback proves the full relay path locally: browser→edge:8443
// (SNI)→relay→tunnel→piperd→Caddy:443(TLS)→container. Self-signed wildcard
// cert, no ACME, no real DNS. Uses :8443 and :7000 to avoid privileged :443.
func TestRelayLoopback(t *testing.T) {
	if os.Getenv("RUN_E2E") != "1" {
		t.Skip("set RUN_E2E=1 to run (needs Docker; Caddy is embedded)")
	}
	repoRoot, _ := filepath.Abs("../..")
	base := "alice.localhost"

	// Self-signed wildcard cert for *.alice.localhost.
	certFile, keyF := writeSelfSigned(t, base)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cl := startCluster(t, ctx, clusterOpts{apex: "localhost"})
	token := cl.enroll("alice", base)
	bin := bins(t)

	// Mint a control-API token before starting piperd, so there's only one
	// writer to piper.db at a time.
	piperdDataDir := t.TempDir()
	tokenCmd := exec.Command(filepath.Join(bin, "piperd"), "token", "create", "--name", "e2e")
	tokenCmd.Env = append(os.Environ(), "PIPER_DATA_DIR="+piperdDataDir)
	tokenOut, err := tokenCmd.Output()
	if err != nil {
		t.Fatalf("token create: %v", err)
	}
	apiToken := strings.TrimSpace(string(tokenOut))
	if apiToken == "" {
		t.Fatal("token create: empty token")
	}

	// Start piperd in relay mode with the static cert. Its relay address is
	// the edge's; a box never learns a relay's own address.
	startRelayPiperd(t, ctx, piperdDataDir, base, token, certFile, keyF)

	// Deploy the sample app.
	c := client.New("http://127.0.0.1:8088", apiToken)
	if err := c.CreateApp("blog", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if _, err := c.Deploy("blog", filepath.Join(repoRoot, "test/e2e/sampleapp")); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	// Fetch through the edge's TLS port by SNI blog.alice.localhost.
	dialer := &tls.Dialer{Config: &tls.Config{ServerName: "blog." + base, InsecureSkipVerify: true}}
	var body string
	for i := 0; i < 30; i++ {
		conn, err := dialer.DialContext(ctx, "tcp", edgeTLSAddr)
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

// TestRelayCluster is the topology under the failure it exists for: a box's
// two sessions (#530) land on two relays in two zones (#531), the app keeps
// serving when one of those relays is rolled, and the session it lost
// re-places when the relay comes back. None of it is observable with a
// single directly-dialled relay.
func TestRelayCluster(t *testing.T) {
	if os.Getenv("RUN_E2E") != "1" {
		t.Skip("set RUN_E2E=1 to run (needs Docker; Caddy is embedded)")
	}
	repoRoot, _ := filepath.Abs("../..")
	base := "alice.localhost"
	certFile, keyF := writeSelfSigned(t, base)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cl := startCluster(t, ctx, clusterOpts{apex: "localhost"})
	token := cl.enroll("alice", base)
	bin := bins(t)

	piperdDataDir := t.TempDir()
	tokenCmd := exec.Command(filepath.Join(bin, "piperd"), "token", "create", "--name", "e2e")
	tokenCmd.Env = append(os.Environ(), "PIPER_DATA_DIR="+piperdDataDir)
	tokenOut, err := tokenCmd.Output()
	if err != nil {
		t.Fatalf("token create: %v", err)
	}
	startRelayPiperd(t, ctx, piperdDataDir, base, token, certFile, keyF)

	c := client.New("http://127.0.0.1:8088", strings.TrimSpace(string(tokenOut)))
	if err := c.CreateApp("blog", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if _, err := c.Deploy("blog", filepath.Join(repoRoot, "test/e2e/sampleapp")); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	hc := sniClient()
	url := "https://blog." + base + "/"
	fetchVia(t, hc, url, "hello piper\n", 60*time.Second)

	// Both sessions placed, on different relays and different zones. Slot 0
	// is on relay 0: it dials first, and with both relays idle the pool
	// order is by start time. Slot 1 then excludes relay 0 as an owner and
	// prefers a zone nothing of this box's is in.
	owners := cl.waitOwners(base, 2, 30*time.Second)
	if got := relayIndexes(owners); got[0] == got[1] {
		t.Fatalf("both sessions placed on relay %v; #530 wants one each", got)
	}
	if owners[0].Zone == owners[1].Zone {
		t.Fatalf("both sessions in zone %q; #531 wants one per zone", owners[0].Zone)
	}

	// Roll the relay holding slot 0. It drains, leaves the pool, and the
	// visitor keeps being served — through the other owner, which is what
	// the second session is for.
	cl.stopRelay(0)
	cl.waitOwners(base, 1, 30*time.Second)
	fetchVia(t, hc, url, "hello piper\n", 30*time.Second)

	// The replacement joins the pool and the stranded slot lands on it: back
	// to two owners, and still serving.
	cl.startRelay(0)
	back := cl.waitOwners(base, 2, 60*time.Second)
	if got := relayIndexes(back); got[0] == got[1] {
		t.Fatalf("after the roll both sessions are on relay %v", got)
	}
	fetchVia(t, hc, url, "hello piper\n", 30*time.Second)
}

// startRelayPiperd boots a relay-mode piperd against the edge with a static
// wildcard cert, and returns once its control API accepts.
func startRelayPiperd(t *testing.T, ctx context.Context, dataDir, base, token, certFile, keyFile string) {
	t.Helper()
	pd := exec.CommandContext(ctx, filepath.Join(bins(t), "piperd"))
	pd.Env = append(os.Environ(),
		"PIPER_DATA_DIR="+dataDir,
		"PIPER_API_ADDR=127.0.0.1:8088",
		"PIPER_BASE_DOMAIN="+base,
		"PIPER_RELAY_ADDR="+edgeTunnelAddr,
		"PIPER_RELAY_TOKEN="+token,
		"PIPER_TLS_CERT_FILE="+certFile,
		"PIPER_TLS_KEY_FILE="+keyFile,
	)
	pd.Stdout, pd.Stderr = os.Stdout, os.Stderr
	if err := pd.Start(); err != nil {
		t.Fatalf("start piperd: %v", err)
	}
	killOnCleanup(t, pd)
	waitPort(t, "127.0.0.1:8088", 15*time.Second)
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
