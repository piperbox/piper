package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/piperbox/piper/internal/client"
	"github.com/piperbox/piper/internal/relay/relaytest"
	"github.com/piperbox/piper/internal/store"
)

// TestWebhookPushAndPreview proves Plan 3 end-to-end on the passthrough relay:
// an HMAC-signed synthetic GitHub delivery to hooks.<base> — TLS-dialed against
// the relay's public port by SNI, exactly as GitHub's delivery arrives — rides
// the tunnel to the box's webhook listener, which fetches the tarball from a
// stub GitHub API (via PIPER_GITHUB_API_BASE), builds, and serves the app.
// A pull_request opened event brings up pr-7-blog.<base>; closed tears it down.
func TestWebhookPushAndPreview(t *testing.T) {
	if os.Getenv("RUN_E2E") != "1" {
		t.Skip("set RUN_E2E=1 to run (needs Docker; Caddy is embedded)")
	}
	repoRoot, _ := filepath.Abs("../..")
	base := "alice.localhost"
	const (
		repo     = "alice/blog"
		secret   = "whsec-e2e"
		pushSHA  = "1111111111111111111111111111111111111111"
		prSHA    = "2222222222222222222222222222222222222222"
		pushBody = "push v1\n"
		prBody   = "pr preview\n"
	)

	certFile, keyF := writeSelfSigned(t, base)

	// Stub GitHub API: installation tokens, tarballs keyed by SHA, Deployments.
	var statusPosts atomic.Int32
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			io.WriteString(w, `{"token":"ghs_e2e"}`)
		case strings.Contains(r.URL.Path, "/tarball/"+pushSHA):
			w.Write(appTarball(t, pushBody))
		case strings.Contains(r.URL.Path, "/tarball/"+prSHA):
			w.Write(appTarball(t, prBody))
		case strings.HasSuffix(r.URL.Path, "/deployments") && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"id":1}`)
		case strings.HasSuffix(r.URL.Path, "/deployments") && r.Method == http.MethodGet:
			io.WriteString(w, `[{"id":1}]`)
		case strings.HasSuffix(r.URL.Path, "/statuses"):
			statusPosts.Add(1)
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{}`)
		default:
			t.Errorf("stub github: unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer gh.Close()

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

	// Enroll an agent, capture the token, start the relay (as TestRelayLoopback).
	relayData := t.TempDir()
	relayDB := relaytest.DSN(t)
	enroll := exec.Command(filepath.Join(binDir, "piper-relay"), "enroll", "alice", "--domain", base)
	enroll.Env = append(os.Environ(), "PIPER_RELAY_DATA_DIR="+relayData, "PIPER_RELAY_DB_URL="+relayDB)
	out, err := enroll.CombinedOutput()
	if err != nil {
		t.Fatalf("enroll: %v\n%s", err, out)
	}
	token := parseToken(t, string(out))

	relay := exec.CommandContext(ctx, filepath.Join(binDir, "piper-relay"))
	relay.Env = append(os.Environ(),
		"PIPER_RELAY_DATA_DIR="+relayData,
		"PIPER_RELAY_DB_URL="+relayDB,
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

	// Seed the BYO GitHub App row before piperd starts (one writer at a time).
	// store.Open runs the schema, so this also initializes a fresh piper.db.
	piperdData := t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
	st, err := store.Open(filepath.Join(piperdData, "piper.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.SaveGitHubApp(store.GitHubApp{
		AppID: 1, Slug: "e2e", PrivateKey: keyPEM, WebhookSecret: secret,
	}); err != nil {
		t.Fatalf("seed github app: %v", err)
	}
	st.Close()

	// Start piperd in relay mode with the static cert and the GitHub stub.
	pd := exec.CommandContext(ctx, filepath.Join(binDir, "piperd"))
	pd.Env = append(os.Environ(),
		"PIPER_DATA_DIR="+piperdData,
		"PIPER_API_ADDR=127.0.0.1:8088",
		"PIPER_BASE_DOMAIN="+base,
		"PIPER_RELAY_ADDR=127.0.0.1:7000",
		"PIPER_RELAY_TOKEN="+token,
		"PIPER_TLS_CERT_FILE="+certFile,
		"PIPER_TLS_KEY_FILE="+keyF,
		"PIPER_GITHUB_API_BASE="+gh.URL,
	)
	pd.Stdout, pd.Stderr = os.Stdout, os.Stderr
	if err := pd.Start(); err != nil {
		t.Fatalf("start piperd: %v", err)
	}
	killOnCleanup(t, pd)
	waitPort(t, "127.0.0.1:8088", 15*time.Second)

	// Create the app and link the repo (tokenless on loopback).
	c := client.New("http://127.0.0.1:8088", "")
	if err := c.CreateApp("blog", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if err := c.LinkApp("blog", repo, "main", ""); err != nil {
		t.Fatalf("LinkApp: %v", err)
	}

	hc := sniClient()

	// Push: signed delivery to hooks.<base> through the relay; retried until the
	// tunnel and webhook route are up. Then the app must serve the push body.
	pushPayload := `{"ref":"refs/heads/main","after":"` + pushSHA + `","repository":{"full_name":"` + repo + `"},"installation":{"id":99}}`
	deliver(t, hc, "https://hooks."+base+"/", "push", secret, pushPayload, 60*time.Second)
	fetchVia(t, hc, "https://blog."+base+"/", pushBody, 3*time.Minute)
	waitForStatus(t, &statusPosts, 30*time.Second)

	// PR opened → preview at pr-7-blog.<base> (flattened single label under the
	// wildcard); the production app keeps serving the push body.
	prOpened := `{"action":"opened","number":7,"pull_request":{"head":{"ref":"feature","sha":"` + prSHA + `"}},"repository":{"full_name":"` + repo + `"},"installation":{"id":99}}`
	deliver(t, hc, "https://hooks."+base+"/", "pull_request", secret, prOpened, 30*time.Second)
	fetchVia(t, hc, "https://pr-7-blog."+base+"/", prBody, 3*time.Minute)
	fetchVia(t, hc, "https://blog."+base+"/", pushBody, 20*time.Second)

	// PR closed → the preview route is torn down. The box's Caddy then answers
	// an empty 200 for the host (the route is gone, the TLS splice still
	// completes under the wildcard), so "gone" = anything but the preview body.
	prClosed := `{"action":"closed","number":7,"pull_request":{"head":{"ref":"feature","sha":"` + prSHA + `"}},"repository":{"full_name":"` + repo + `"},"installation":{"id":99}}`
	deliver(t, hc, "https://hooks."+base+"/", "pull_request", secret, prClosed, 30*time.Second)
	deadline := time.Now().Add(60 * time.Second)
	gone := false
	for time.Now().Before(deadline) {
		resp, err := hc.Get("https://pr-7-blog." + base + "/")
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(b) != prBody {
				gone = true
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !gone {
		t.Fatal("preview still serves its body after PR close")
	}
	fetchVia(t, hc, "https://blog."+base+"/", pushBody, 20*time.Second)
}

// appTarball is a gzipped tar in GitHub codeload shape — a single top-level
// dir wrapping a Dockerfile — serving body (see dockerfileFor).
func appTarball(t *testing.T, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	content := dockerfileFor(body)
	if err := tw.WriteHeader(&tar.Header{
		Name: "alice-blog-abc123/Dockerfile", Mode: 0o644,
		Size: int64(len(content)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte(content))
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// sniClient dials every request to the relay's public TLS port; the URL's
// hostname carries the SNI, exactly as a visitor (or GitHub) arrives once DNS
// points at the relay.
func sniClient() *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, "127.0.0.1:8443")
			},
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
			DisableKeepAlives: true,
		},
	}
}

// deliver POSTs an HMAC-signed webhook, retrying until the box accepts it with
// 202 (the tunnel, Caddy route, and listener all have to be up first).
func deliver(t *testing.T, hc *http.Client, url, event, secret, payload string, within time.Duration) {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	deadline := time.Now().Add(within)
	var last string
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(payload))
		req.Header.Set("X-GitHub-Event", event)
		req.Header.Set("X-Hub-Signature-256", sig)
		resp, err := hc.Do(req)
		if err != nil {
			last = err.Error()
		} else {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusAccepted {
				return
			}
			last = fmt.Sprintf("status %d body %q", resp.StatusCode, b)
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("webhook %s to %s never accepted: last %s", event, url, last)
}

// waitForStatus polls until at least one deployment status has reached the stub
// GitHub. The handler publishes the app's route and only then reports success
// (webhook.go: Deploy, then Report), so a fetch through the relay can land in
// the window before the /statuses POST — the count has to be polled, not read
// once.
func waitForStatus(t *testing.T, posts *atomic.Int32, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if posts.Load() > 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("no deployment status was ever reported to the stub GitHub")
}

// fetchVia polls url through the relay until it serves exactly want.
func fetchVia(t *testing.T, hc *http.Client, url, want string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	var last string
	for time.Now().Before(deadline) {
		resp, err := hc.Get(url)
		if err != nil {
			last = err.Error()
		} else {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && string(b) == want {
				return
			}
			last = fmt.Sprintf("status %d body %q", resp.StatusCode, b)
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("%s never served %q: last %s", url, want, last)
}
