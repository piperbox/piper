package e2e

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRelayCustomDomainSelfService proves the free-tier box can self-serve a
// BYO custom domain (#102) on top of the relay-terminated loop: piper login
// (device flow + claim-through-the-enrollment-socket, one command) → piper
// deploy (as in TestRelayTerminatedSelfService), then
// PUT /v1/domain on the box's control API drives DNS-01 issuance (stubbed via
// PIPER_TEST_ISSUER=selfsigned) → live Caddy activation → relay SNI splice,
// and a visitor reaches the app on the custom domain through the relay while
// the shared-domain URL keeps serving.
func TestRelayCustomDomainSelfService(t *testing.T) {
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
	piperEnv := append(os.Environ(), "HOME="+home, "PIPER_ADDR=", "PIPER_TOKEN=")
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

	// ---- Custom domain via the control API (#102) ----
	custom := "shop.localhost"

	// PUT /v1/domain on the box's local control API.
	put := func() (*http.Response, error) {
		req, _ := http.NewRequest(http.MethodPut, "http://127.0.0.1:8088/v1/domain",
			strings.NewReader(`{"domain":"`+custom+`","dns_provider":"cloudflare","dns_token":"fake-for-selfsigned"}`))
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

	// Poll GET /v1/domain until active; assert the token never leaks.
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
	if strings.Contains(domBody, "fake-for-selfsigned") {
		t.Fatalf("GET /v1/domain leaks the dns token: %s", domBody)
	}
	if !strings.Contains(domBody, `"dns_records"`) || !strings.Contains(domBody, `"*.`+custom+`"`) {
		t.Fatalf("GET /v1/domain missing guided-setup records: %s", domBody)
	}

	// Visitor on the custom domain: TLS SNI blog.shop.localhost → relay:8443
	// splices passthrough → box :443 terminates. E2E TLS: relay never decrypts.
	var customResp string
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		d := &tls.Dialer{Config: &tls.Config{ServerName: "blog." + custom, InsecureSkipVerify: true}}
		conn, err := d.DialContext(ctx, "tcp", "127.0.0.1:8443")
		if err == nil {
			fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: blog.%s\r\nConnection: close\r\n\r\n", custom)
			cb, _ := io.ReadAll(conn)
			conn.Close()
			if len(cb) > 0 {
				customResp = string(cb)
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if customResp == "" {
		t.Fatal("no response on the custom domain through the relay")
	}

	// Coexistence: the shared-domain URL still serves.
	hostname := terminatedHostname(t, relayData)
	d := &tls.Dialer{Config: &tls.Config{ServerName: hostname, InsecureSkipVerify: true}}
	conn, err := d.DialContext(ctx, "tcp", "127.0.0.1:8443")
	if err != nil {
		t.Fatalf("shared-domain dial after custom domain: %v", err)
	}
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", hostname)
	sb, _ := io.ReadAll(conn)
	conn.Close()
	if len(sb) == 0 {
		t.Fatal("shared-domain URL broke after adding the custom domain")
	}
}

// TestRelayPerAppCustomDomain proves the per-app BYO domain flow (#224/#231):
// on the relay-terminated loop, POST /v1/apps/<app>/domains claims the domain
// on the relay and drives tokenless issuance (stubbed via
// PIPER_TEST_ISSUER=selfsigned, which also stubs the DNS gate) to active, a
// visitor reaches the app on the exact-host domain through the relay while the
// shared URL keeps serving, DELETE tears everything down, and deleting the app
// itself tears down its domains too (#267).
func TestRelayPerAppCustomDomain(t *testing.T) {
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
	piperEnv := append(os.Environ(), "HOME="+home, "PIPER_ADDR=", "PIPER_TOKEN=")
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

	// ---- Per-app custom domain via the control API (#231) ----
	custom := "myshop.localhost"
	api := func(method, path, body string) (int, string) {
		t.Helper()
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req, _ := http.NewRequest(method, "http://127.0.0.1:8088"+path, rd)
		req.Header.Set("Authorization", "Bearer "+apiToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}
	attachAndWaitActive := func() {
		t.Helper()
		if code, body := api(http.MethodPost, "/v1/apps/blog/domains", `{"domain":"`+custom+`"}`); code != http.StatusCreated {
			t.Fatalf("POST domains = %d: %s", code, body)
		}
		deadline := time.Now().Add(30 * time.Second)
		var listBody string
		for time.Now().Before(deadline) {
			_, listBody = api(http.MethodGet, "/v1/apps/blog/domains", "")
			if strings.Contains(listBody, `"status":"active"`) {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if !strings.Contains(listBody, `"status":"active"`) {
			t.Fatalf("domain never became active: %s", listBody)
		}
		if !strings.Contains(listBody, `"dns_records"`) || !strings.Contains(listBody, `"name":"`+custom+`"`) {
			t.Fatalf("GET domains missing guided-setup record: %s", listBody)
		}
	}
	attachAndWaitActive()

	// Visitor on the exact-host domain: TLS SNI myshop.localhost → relay:8443
	// splices passthrough → box :443 terminates.
	curlCustom := func() string {
		d := &tls.Dialer{Config: &tls.Config{ServerName: custom, InsecureSkipVerify: true}}
		conn, err := d.DialContext(ctx, "tcp", "127.0.0.1:8443")
		if err != nil {
			return ""
		}
		fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", custom)
		cb, _ := io.ReadAll(conn)
		conn.Close()
		return string(cb)
	}
	var customResp string
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if customResp = curlCustom(); customResp != "" {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if customResp == "" {
		t.Fatal("no response on the per-app custom domain through the relay")
	}

	// Browsers default to http://, so the bare domain must not dead-end: the
	// relay's :80 listener carries the request over the tunnel to the box's
	// plaintext Caddy, which answers 308 to https:// (#357). Without the
	// redirect route nothing on that server matches the host and the visitor
	// never reaches the app.
	httpConn, err := net.Dial("tcp", "127.0.0.1:8880")
	if err != nil {
		t.Fatalf("dial relay :80: %v", err)
	}
	fmt.Fprintf(httpConn, "GET /pricing HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", custom)
	hb, _ := io.ReadAll(httpConn)
	httpConn.Close()
	redirResp := string(hb)
	if !strings.Contains(redirResp, "308") {
		t.Fatalf("http:// on the custom domain did not redirect:\n%s", redirResp)
	}
	if !strings.Contains(redirResp, "Location: https://"+custom+"/pricing") {
		t.Fatalf("redirect must preserve host and path; got:\n%s", redirResp)
	}

	// Coexistence: the shared-domain URL still serves.
	hostname := terminatedHostname(t, relayData)
	d := &tls.Dialer{Config: &tls.Config{ServerName: hostname, InsecureSkipVerify: true}}
	conn, err := d.DialContext(ctx, "tcp", "127.0.0.1:8443")
	if err != nil {
		t.Fatalf("shared-domain dial after custom domain: %v", err)
	}
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", hostname)
	sb, _ := io.ReadAll(conn)
	conn.Close()
	if len(sb) == 0 {
		t.Fatal("shared-domain URL broke after adding the per-app domain")
	}

	// DELETE tears down: row gone from the list, cert dir removed, and the
	// relay stops routing the domain.
	certDir := filepath.Join(piperdData, "appdomains", custom)
	if _, err := os.Stat(certDir); err != nil {
		t.Fatalf("cert dir missing while active: %v", err)
	}
	if code, body := api(http.MethodDelete, "/v1/apps/blog/domains/"+custom, ""); code != http.StatusNoContent {
		t.Fatalf("DELETE domain = %d: %s", code, body)
	}
	if _, body := api(http.MethodGet, "/v1/apps/blog/domains", ""); strings.TrimSpace(body) != "[]" {
		t.Fatalf("domains after DELETE = %s, want []", body)
	}
	if _, err := os.Stat(certDir); !os.IsNotExist(err) {
		t.Fatalf("cert dir survived DELETE: %v", err)
	}
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if curlCustom() == "" {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if resp := curlCustom(); resp != "" {
		t.Fatalf("custom domain still served after DELETE: %q", resp)
	}

	// #267: deleting the app itself tears down its re-attached domain too.
	attachAndWaitActive()
	if code, body := api(http.MethodDelete, "/v1/apps/blog", ""); code != http.StatusNoContent {
		t.Fatalf("DELETE app = %d: %s", code, body)
	}
	if _, err := os.Stat(certDir); !os.IsNotExist(err) {
		t.Fatalf("cert dir survived app delete: %v", err)
	}
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if curlCustom() == "" {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if resp := curlCustom(); resp != "" {
		t.Fatalf("custom domain still served after app delete: %q", resp)
	}
}
