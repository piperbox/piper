package e2e

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRelayCustomDomainDirectServe proves the direct-serve migration path
// (PIPER_SERVE) on top of the relay-terminated loop: piper login (device flow
// + claim-through-the-enrollment-socket, one command) → piper deploy (as in
// TestRelayCustomDomainSelfService), then PUT /v1/domain with "serve":"direct"
// drives DNS-01 issuance (stubbed via PIPER_TEST_ISSUER=selfsigned) → live
// Caddy activation, but with direct-shaped guidance: A records at the box's
// own relay-observed public IP (loopback in this harness) instead of the
// wildcard CNAME, and a green dns_ok because the selfsigned seam also stubs
// the resolver to loopback. A visitor reaches the app at the box's own :443
// directly — no relay in the path by construction — while the relay path
// keeps serving the same domain too, the migration property that lets an
// operator flip DNS without a window of downtime.
func TestRelayCustomDomainDirectServe(t *testing.T) {
	if os.Getenv("RUN_E2E") != "1" {
		t.Skip("set RUN_E2E=1 to run (needs Docker; Caddy is embedded)")
	}
	repoRoot, _ := filepath.Abs("../..")
	apex := "public.localhost"
	certFile, keyFile := writeSelfSigned(t, apex) // *.public.localhost

	binDir := t.TempDir()
	for _, c := range []string{"piperd", "piper-relay", "piper"} {
		b := exec.Command("go", "build", "-o", filepath.Join(binDir, c), "./cmd/"+c)
		b.Dir = repoRoot
		if out, err := b.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", c, err, out)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	relayData := t.TempDir()
	relay := exec.CommandContext(ctx, filepath.Join(binDir, "piper-relay"))
	relay.Env = append(os.Environ(),
		"PIPER_RELAY_DATA_DIR="+relayData,
		"PIPER_RELAY_TLS_ADDR=127.0.0.1:8443",
		"PIPER_RELAY_HTTP_ADDR=127.0.0.1:8880",
		"PIPER_RELAY_TUNNEL_ADDR=127.0.0.1:7000",
		"PIPER_RELAY_API_ADDR=127.0.0.1:8080",
		"PIPER_RELAY_TUNNEL_PUBLIC=127.0.0.1:7000",
		"PIPER_RELAY_APEX="+apex,
		"PIPER_RELAY_TLS_CERT="+certFile,
		"PIPER_RELAY_TLS_KEY="+keyFile,
		"PIPER_RELAY_FAKE_APPROVE=1",
	)
	relay.Stdout, relay.Stderr = os.Stdout, os.Stderr
	if err := relay.Start(); err != nil {
		t.Fatalf("start relay: %v", err)
	}
	killOnCleanup(t, relay)
	waitPort(t, "127.0.0.1:7000", 10*time.Second)
	waitPort(t, "127.0.0.1:8080", 10*time.Second)

	// Start piperd first, LAN-only (no PIPER_RELAY_* env): its enrollment
	// socket comes up so `piper login` below can claim it (one-command login).
	//
	// os.MkdirTemp, not t.TempDir(): t.TempDir() embeds the (long) test name in
	// the path, which blows darwin's ~104-byte sockaddr_un sun_path limit under
	// a normal $TMPDIR. MkdirTemp("", "p") drops that segment and stays well
	// under the limit on both darwin and linux.
	piperdData, err := os.MkdirTemp("", "p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(piperdData) })
	pd := exec.CommandContext(ctx, filepath.Join(binDir, "piperd"))
	pd.Env = append(os.Environ(),
		"PIPER_DATA_DIR="+piperdData,
		"PIPER_API_ADDR=127.0.0.1:8088",
		"PIPER_TEST_ISSUER=selfsigned",
	)
	pd.Stdout, pd.Stderr = os.Stdout, os.Stderr
	if err := pd.Start(); err != nil {
		t.Fatalf("start piperd: %v", err)
	}
	killOnCleanup(t, pd)
	waitPort(t, "127.0.0.1:8088", 15*time.Second)

	// piper login (device flow auto-approves, then claims this box through
	// piperd's enrollment socket) → writes ~/.piper/piper and piperd's
	// relay.json (terminated); piperd re-execs itself to apply it.
	home := t.TempDir()
	piperEnv := append(os.Environ(), "HOME="+home, "PIPER_ADDR=", "PIPER_TOKEN=", "PIPER_NO_BROWSER=1")
	login := exec.Command(filepath.Join(binDir, "piper"), "login", "--relay", "http://127.0.0.1:8080", "--data-dir", piperdData)
	login.Env = piperEnv
	if out, err := login.CombinedOutput(); err != nil {
		t.Fatalf("piper login: %v\n%s", err, out)
	}
	waitPort(t, "127.0.0.1:8088", 15*time.Second) // piperd re-exec'd to apply the enrollment

	// Mint a control-API token now that piperd is enrolled and running (safe
	// alongside the live daemon: store.Open uses WAL + busy_timeout).
	tokenCmd := exec.Command(filepath.Join(binDir, "piperd"), "token", "create", "--name", "e2e")
	tokenCmd.Env = append(os.Environ(), "PIPER_DATA_DIR="+piperdData)
	tokenOut, err := tokenCmd.Output()
	if err != nil {
		t.Fatalf("token create: %v", err)
	}
	apiToken := strings.TrimSpace(string(tokenOut))

	// Create the app, then deploy. Terminated deploy registers the hostname over
	// the tunnel, so retry until piperd's tunnel client has connected to the relay.
	create := exec.Command(filepath.Join(binDir, "piper"), "create", "blog", "--port", "8080")
	create.Env = append(piperEnv, "PIPER_ADDR=http://127.0.0.1:8088", "PIPER_TOKEN="+apiToken)
	if out, err := create.CombinedOutput(); err != nil {
		t.Fatalf("piper create: %v\n%s", err, out)
	}
	var deployErr string
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		dep := exec.Command(filepath.Join(binDir, "piper"),
			"deploy", "blog", "--path", filepath.Join(repoRoot, "test/e2e/sampleapp"))
		dep.Env = append(piperEnv, "PIPER_ADDR=http://127.0.0.1:8088", "PIPER_TOKEN="+apiToken)
		out, err := dep.CombinedOutput()
		if err == nil {
			deployErr = ""
			break
		}
		deployErr = fmt.Sprintf("%v\n%s", err, out)
		time.Sleep(1 * time.Second)
	}
	if deployErr != "" {
		t.Fatalf("piper deploy: %s", deployErr)
	}

	// ---- Direct-served custom domain ----
	custom := "shop.localhost"

	put := func() (*http.Response, error) {
		req, _ := http.NewRequest(http.MethodPut, "http://127.0.0.1:8088/v1/domain",
			strings.NewReader(`{"domain":"`+custom+`","dns_provider":"cloudflare","dns_token":"fake-for-selfsigned","serve":"direct"}`))
		req.Header.Set("Authorization", "Bearer "+apiToken)
		return http.DefaultClient.Do(req)
	}
	resp, err := put()
	if err != nil {
		t.Fatalf("PUT /v1/domain: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /v1/domain = %d: %s", resp.StatusCode, b)
	}

	// Poll until active; then the guidance must be direct-shaped: A records at
	// the box's relay-observed IP (loopback here) and a green dns_ok (the
	// selfsigned seam stubs the resolver to loopback too).
	deadline = time.Now().Add(30 * time.Second)
	var domBody string
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:8088/v1/domain", nil)
		req.Header.Set("Authorization", "Bearer "+apiToken)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			gb, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			domBody = string(gb)
			if strings.Contains(domBody, `"status":"active"`) {
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !strings.Contains(domBody, `"status":"active"`) {
		t.Fatalf("domain never became active: %s", domBody)
	}
	if !strings.Contains(domBody, `"serve":"direct"`) {
		t.Fatalf("GET /v1/domain missing serve mode: %s", domBody)
	}
	if !strings.Contains(domBody, `"type":"A"`) || !strings.Contains(domBody, `"value":"127.0.0.1"`) {
		t.Fatalf("direct mode must guide A records at the observed IP: %s", domBody)
	}
	if !strings.Contains(domBody, `"dns_ok":true`) {
		t.Fatalf("dns_ok not green in direct mode: %s", domBody)
	}

	// The point of direct mode: a visitor reaches the app at the BOX's :443 —
	// no relay in the path by construction.
	curlDirect := func() string {
		d := &tls.Dialer{Config: &tls.Config{ServerName: "blog." + custom, InsecureSkipVerify: true}}
		conn, err := d.DialContext(ctx, "tcp", "127.0.0.1:443")
		if err != nil {
			return ""
		}
		fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: blog.%s\r\nConnection: close\r\n\r\n", custom)
		cb, _ := io.ReadAll(conn)
		conn.Close()
		return string(cb)
	}
	var directResp string
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if directResp = curlDirect(); directResp != "" {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if directResp == "" {
		t.Fatal("no response dialing the box's :443 directly")
	}

	// Migration property: the relay claim is kept, so the same domain still
	// serves through the relay's public port while DNS is being flipped.
	d := &tls.Dialer{Config: &tls.Config{ServerName: "blog." + custom, InsecureSkipVerify: true}}
	conn, err := d.DialContext(ctx, "tcp", "127.0.0.1:8443")
	if err != nil {
		t.Fatalf("relay-path dial: %v", err)
	}
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: blog.%s\r\nConnection: close\r\n\r\n", custom)
	rb, _ := io.ReadAll(conn)
	conn.Close()
	if len(rb) == 0 {
		t.Fatal("relay path stopped serving the direct domain (migration property broken)")
	}
}
