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

// TestNeverEnrolledDirectServesHTTPS is the relay-less half of direct serve
// (#507): a box that has never been enrolled, told only what it is publicly
// called and to serve that name itself, terminates TLS for it. There is no
// relay process in this test at all, so nothing it proves can be coming from
// one — which is the whole point, and what every other e2e here cannot show.
//
// It stops at the TLS layer on purpose: the certificate the box serves under
// its own SNI is the thing the relay gates used to make unreachable. Routing
// an app onto that listener is the deploy path, already covered above.
func TestNeverEnrolledDirectServesHTTPS(t *testing.T) {
	if os.Getenv("RUN_E2E") != "1" {
		t.Skip("set RUN_E2E=1 to run (needs Docker; Caddy is embedded)")
	}
	repoRoot, _ := filepath.Abs("../..")
	binDir := t.TempDir()
	b := exec.Command("go", "build", "-o", filepath.Join(binDir, "piperd"), "./cmd/piperd")
	b.Dir = repoRoot
	if out, err := b.CombinedOutput(); err != nil {
		t.Fatalf("build piperd: %v\n%s", err, out)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// os.MkdirTemp, not t.TempDir(): see the note in the test above.
	piperdData, err := os.MkdirTemp("", "p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(piperdData) })

	const domain = "direct.localhost"
	pd := exec.CommandContext(ctx, filepath.Join(binDir, "piperd"))
	// Every PIPER_RELAY_* is cleared explicitly: inheriting one from the
	// environment would quietly restore the relay path this test exists to
	// prove is not needed. High loopback ports keep it off :80/:443, which the
	// relay-mode e2es above own.
	pd.Env = append(os.Environ(),
		"PIPER_DATA_DIR="+piperdData,
		"PIPER_API_ADDR=127.0.0.1:8188",
		"PIPER_HTTP_ADDR=127.0.0.1:8180",
		"PIPER_HTTPS_ADDR=127.0.0.1:8543",
		"PIPER_CADDY_ADMIN=http://127.0.0.1:2219",
		"PIPER_BASE_DOMAIN="+domain,
		"PIPER_SERVE=direct",
		"PIPER_TEST_ISSUER=selfsigned",
		"PIPER_RELAY_ADDR=",
		"PIPER_RELAY_TOKEN=",
		"PIPER_RELAY_TERMINATED=",
	)
	pd.Stdout, pd.Stderr = os.Stdout, os.Stderr
	if err := pd.Start(); err != nil {
		t.Fatalf("start piperd: %v", err)
	}
	killOnCleanup(t, pd)
	waitPort(t, "127.0.0.1:8188", 20*time.Second)
	waitPort(t, "127.0.0.1:8543", 20*time.Second)

	// The handshake is the assertion: before the hoist there was no TLS
	// listener here at all, and no cert for it to present.
	dialer := &tls.Dialer{Config: &tls.Config{
		ServerName: "blog." + domain, InsecureSkipVerify: true,
	}}
	var conn net.Conn
	deadline := time.Now().Add(20 * time.Second)
	for {
		conn, err = dialer.DialContext(ctx, "tcp", "127.0.0.1:8543")
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("TLS dial of the box's own HTTPS listener: %v", err)
	}
	defer conn.Close()

	state := conn.(*tls.Conn).ConnectionState()
	if len(state.PeerCertificates) == 0 {
		t.Fatal("no certificate presented on the box's HTTPS listener")
	}
	if err := state.PeerCertificates[0].VerifyHostname("blog." + domain); err != nil {
		t.Fatalf("served cert does not cover the box's own wildcard: %v (subject %q, SANs %v)",
			err, state.PeerCertificates[0].Subject, state.PeerCertificates[0].DNSNames)
	}

	// And it speaks HTTP over that TLS: Caddy is really serving, not just
	// completing a handshake.
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: blog.%s\r\nConnection: close\r\n\r\n", domain)
	body, _ := io.ReadAll(conn)
	if !strings.HasPrefix(string(body), "HTTP/1.1 ") {
		t.Fatalf("no HTTP response over the direct TLS listener: %q", string(body))
	}

	// The daemon must also tell its clients the apps are on HTTPS — a client
	// on the LAN cannot infer it from how it dialled (#507).
	tokenCmd := exec.Command(filepath.Join(binDir, "piperd"), "token", "create", "--name", "e2e")
	tokenCmd.Env = append(os.Environ(), "PIPER_DATA_DIR="+piperdData)
	tokenOut, err := tokenCmd.Output()
	if err != nil {
		t.Fatalf("token create: %v", err)
	}
	apiToken := strings.TrimSpace(string(tokenOut))

	create, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:8188/v1/apps",
		strings.NewReader(`{"name":"blog","port":8080}`))
	create.Header.Set("Authorization", "Bearer "+apiToken)
	resp, err := http.DefaultClient.Do(create)
	if err != nil {
		t.Fatalf("POST /v1/apps: %v", err)
	}
	cb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/apps = %d: %s", resp.StatusCode, cb)
	}
	if !strings.Contains(string(cb), `"Scheme":"https"`) {
		t.Fatalf("daemon must report the https scheme for its apps: %s", cb)
	}

	// Per-app custom domain on a never-enrolled box (#506): DNS-01 via the
	// box-wide token source (the selfsigned test issuer here), no relay
	// claim anywhere, exact-host cert on the box's own listener. Before
	// #506 this POST was refused with 409 "cannot activate on this box".
	addDom, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:8188/v1/apps/blog/domains",
		strings.NewReader(`{"domain":"shop.localhost"}`))
	addDom.Header.Set("Authorization", "Bearer "+apiToken)
	resp, err = http.DefaultClient.Do(addDom)
	if err != nil {
		t.Fatalf("POST /v1/apps/blog/domains: %v", err)
	}
	db, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/apps/blog/domains = %d: %s", resp.StatusCode, db)
	}

	statusReq, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:8188/v1/apps/blog/domains", nil)
	statusReq.Header.Set("Authorization", "Bearer "+apiToken)
	deadline = time.Now().Add(30 * time.Second)
	for {
		r, err := http.DefaultClient.Do(statusReq)
		if err == nil {
			sb, _ := io.ReadAll(r.Body)
			r.Body.Close()
			if strings.Contains(string(sb), `"status":"active"`) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("shop.localhost never activated: %s", sb)
			}
		} else if time.Now().After(deadline) {
			t.Fatalf("GET /v1/apps/blog/domains: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}

	appDialer := &tls.Dialer{Config: &tls.Config{
		ServerName: "shop.localhost", InsecureSkipVerify: true,
	}}
	appConn, err := appDialer.DialContext(ctx, "tcp", "127.0.0.1:8543")
	if err != nil {
		t.Fatalf("TLS dial for the per-app domain: %v", err)
	}
	defer appConn.Close()
	appState := appConn.(*tls.Conn).ConnectionState()
	if len(appState.PeerCertificates) == 0 {
		t.Fatal("no certificate presented for the per-app domain")
	}
	if err := appState.PeerCertificates[0].VerifyHostname("shop.localhost"); err != nil {
		t.Fatalf("per-app cert does not cover shop.localhost: %v (SANs %v)",
			err, appState.PeerCertificates[0].DNSNames)
	}
}
