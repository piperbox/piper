package caddy

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
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// freeAddr returns a currently-free 127.0.0.1:port.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick free port: %v", err)
	}
	defer l.Close()
	return l.Addr().String()
}

// The base (LAN, non-WithHTTPS) config is plain HTTP: automatic HTTPS must be
// disabled so adding a host-matched route provisions no internal cert and
// stands up no :80 redirect server. Plan 1 is HTTP-only on :80.
func TestBaseConfigDisablesAutomaticHTTPSOnLAN(t *testing.T) {
	o := &managerOpts{httpListen: ":80"}
	base := o.baseConfig()

	srv := base["apps"].(map[string]any)["http"].(map[string]any)["servers"].(map[string]any)["piper"].(map[string]any)
	ah, ok := srv["automatic_https"].(map[string]any)
	if !ok {
		t.Fatalf("base config should set automatic_https on the piper server, got %v", srv["automatic_https"])
	}
	if ah["disable"] != true {
		t.Fatalf("automatic_https.disable should be true on the LAN path, got %v", ah["disable"])
	}
}

// A host-matched route must apply on a non-:80 http port without Caddy trying
// to bind :80 for an auto-HTTPS redirect server — the exact case #40's test had
// to sidestep. With automatic HTTPS disabled the reload succeeds regardless of
// whether :80 is bindable.
func TestUpsertRouteOnNon80PortApplies(t *testing.T) {
	t.Setenv("PATH", "")
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_DATA_HOME", dir)

	admin := "http://" + freeAddr(t)
	httpListen := freeAddr(t) // non-:80
	m, err := StartManager(admin, httpListen)
	if err != nil {
		t.Fatalf("StartManager: %v", err)
	}
	defer m.Stop()

	c := NewClient(admin)
	if err := c.UpsertRoute("app.piper.localhost", 8080); err != nil {
		t.Fatalf("UpsertRoute on non-:80 port: %v", err)
	}
}

// Caddy is a process-global singleton (caddy.Load/caddy.Stop act on it), so a
// second StartManager while one is live would silently clobber the first's
// config. StartManager must refuse it, and a fresh Start must succeed once the
// first is Stopped.
func TestStartManagerRefusesSecondWhileActive(t *testing.T) {
	t.Setenv("PATH", "")
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_DATA_HOME", dir)

	m, err := StartManager("http://"+freeAddr(t), freeAddr(t))
	if err != nil {
		t.Fatalf("first StartManager: %v", err)
	}

	if _, err := StartManager("http://"+freeAddr(t), freeAddr(t)); err == nil {
		m.Stop()
		t.Fatal("second StartManager while one is active should error, got nil")
	}

	m.Stop()

	// After Stop the invariant is cleared: a fresh Manager starts fine.
	m2, err := StartManager("http://"+freeAddr(t), freeAddr(t))
	if err != nil {
		t.Fatalf("StartManager after Stop: %v", err)
	}
	m2.Stop()
}

// StartManager must run Caddy in-process: with PATH emptied (so no external
// `caddy` binary is reachable) it still brings up a live admin API serving the
// base config we built.
func TestStartManagerRunsCaddyWithoutExternalBinary(t *testing.T) {
	// No external caddy anywhere on PATH; embedded Caddy must not care.
	t.Setenv("PATH", "")
	// Keep Caddy's autosave/storage out of the real home dir.
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_DATA_HOME", dir)

	admin := "http://" + freeAddr(t)
	httpListen := freeAddr(t)
	m, err := StartManager(admin, httpListen)
	if err != nil {
		t.Fatalf("StartManager: %v", err)
	}
	defer m.Stop()

	// The admin API is live and serving the config we built (piper server on
	// httpListen). This can only be true if Caddy is running in-process.
	resp, err := http.Get(admin + "/config/")
	if err != nil {
		t.Fatalf("GET admin /config/: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin /config/ status = %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), httpListen) {
		t.Fatalf("running config missing our http listener %q: %s", httpListen, body)
	}
}

// Caddy binds with SO_REUSEPORT, so a foreign process already holding a listen
// port never surfaces as a bind error — piperd would start split-brained, with
// the kernel delivering traffic to the squatter (#126). StartManager must
// detect the clash and fail loudly instead.
func TestStartManagerFailsWhenPortAlreadyHeld(t *testing.T) {
	t.Setenv("PATH", "")
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_DATA_HOME", dir)

	// The squatter binds with SO_REUSEPORT, like a real stray Caddy: that is
	// the case where Caddy's own bind succeeds silently instead of erroring.
	squat := func(t *testing.T) net.Listener {
		t.Helper()
		lc := net.ListenConfig{Control: func(network, address string, c syscall.RawConn) error {
			var soErr error
			err := c.Control(func(fd uintptr) {
				soErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
			})
			if err != nil {
				return err
			}
			return soErr
		}}
		l, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("squatter listen: %v", err)
		}
		t.Cleanup(func() { l.Close() })
		return l
	}

	t.Run("http listener held", func(t *testing.T) {
		l := squat(t)
		m, err := StartManager("http://"+freeAddr(t), l.Addr().String())
		if err == nil {
			m.Stop()
			t.Fatal("StartManager with http port held by a foreign listener should error, got nil")
		}
		if !strings.Contains(err.Error(), l.Addr().String()) {
			t.Fatalf("error should name the conflicting address %q, got: %v", l.Addr().String(), err)
		}
	})

	t.Run("admin address held", func(t *testing.T) {
		l := squat(t)
		m, err := StartManager("http://"+l.Addr().String(), freeAddr(t))
		if err == nil {
			m.Stop()
			t.Fatal("StartManager with admin port held by a foreign listener should error, got nil")
		}
		if !strings.Contains(err.Error(), l.Addr().String()) {
			t.Fatalf("error should name the conflicting address %q, got: %v", l.Addr().String(), err)
		}
	})

	t.Run("https listener held", func(t *testing.T) {
		l := squat(t)
		m, err := StartManager("http://"+freeAddr(t), freeAddr(t), WithHTTPS(l.Addr().String()))
		if err == nil {
			m.Stop()
			t.Fatal("StartManager with https port held by a foreign listener should error, got nil")
		}
		if !strings.Contains(err.Error(), l.Addr().String()) {
			t.Fatalf("error should name the conflicting address %q, got: %v", l.Addr().String(), err)
		}
	})
}

// EnsureHTTPS at runtime must leave :80-style plaintext serving intact while
// the new piper-tls server terminates TLS with a load_pem cert (coexistence).
func TestEnsureHTTPSServesTLSAlongsidePlaintext(t *testing.T) {
	t.Setenv("PATH", "")
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_DATA_HOME", dir)

	admin := "http://" + freeAddr(t)
	httpListen := freeAddr(t)
	httpsListen := freeAddr(t)

	m, err := StartManager(admin, httpListen)
	if err != nil {
		t.Fatalf("StartManager: %v", err)
	}
	defer m.Stop()

	c := NewClient(admin)
	if err := c.EnsureHTTPS(httpsListen); err != nil {
		t.Fatalf("EnsureHTTPS: %v", err)
	}
	// Idempotent second call.
	if err := c.EnsureHTTPS(httpsListen); err != nil {
		t.Fatalf("EnsureHTTPS twice: %v", err)
	}

	certPEM, keyPEM := selfSignedPEM(t, "shop.example.com")
	if err := c.ReplaceCerts([]CertPair{{CertPEM: string(certPEM), KeyPEM: string(keyPEM)}}); err != nil {
		t.Fatalf("ReplaceCerts: %v", err)
	}

	// Plaintext HTTP on the original listener still answers.
	retryBriefly(t, "plaintext GET after EnsureHTTPS", func() error {
		resp, err := http.Get("http://" + httpListen + "/")
		if err != nil {
			return err
		}
		resp.Body.Close()
		return nil
	})

	// TLS handshake on the new listener serves the loaded cert.
	retryBriefly(t, "TLS dial piper-tls", func() error {
		conn, err := tls.Dial("tcp", httpsListen, &tls.Config{
			ServerName: "blog.shop.example.com", InsecureSkipVerify: true,
		})
		if err != nil {
			return err
		}
		conn.Close()
		return nil
	})
}

// The unit tests assert the JSON we send; this proves a real Caddy accepts it
// and actually serves the redirect — a visitor typing the bare custom domain
// gets a 308 to https:// on the plaintext server, path and query preserved,
// rather than the 404 that automatic_https.disable leaves behind (#357).
func TestUpsertRouteTLSRedirectServes308OnPlaintext(t *testing.T) {
	t.Setenv("PATH", "")
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_DATA_HOME", dir)

	admin := "http://" + freeAddr(t)
	httpListen := freeAddr(t)
	httpsListen := freeAddr(t)

	m, err := StartManager(admin, httpListen)
	if err != nil {
		t.Fatalf("StartManager: %v", err)
	}
	defer m.Stop()

	c := NewClient(admin)
	if err := c.EnsureHTTPS(httpsListen); err != nil {
		t.Fatalf("EnsureHTTPS: %v", err)
	}
	if err := c.UpsertRouteTLS("myshop.example.com", 40001); err != nil {
		t.Fatalf("UpsertRouteTLS: %v", err)
	}

	// Never follow the redirect: the target is a domain with no DNS here.
	cl := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	var resp *http.Response
	retryBriefly(t, "plaintext GET on the custom domain", func() error {
		req, _ := http.NewRequest(http.MethodGet, "http://"+httpListen+"/pricing?ref=x", nil)
		req.Host = "myshop.example.com"
		r, err := cl.Do(req)
		if err != nil {
			return err
		}
		resp = r
		return nil
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPermanentRedirect {
		t.Fatalf("status = %d, want 308", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "https://myshop.example.com/pricing?ref=x" {
		t.Fatalf("Location = %q, want the same host, path and query over https", got)
	}

	// Teardown drops the redirect with the route: :80 must stop redirecting to
	// a domain that no longer serves.
	if err := c.RemoveRoute("myshop.example.com"); err != nil {
		t.Fatalf("RemoveRoute: %v", err)
	}
	retryBriefly(t, "plaintext GET after RemoveRoute", func() error {
		req, _ := http.NewRequest(http.MethodGet, "http://"+httpListen+"/", nil)
		req.Host = "myshop.example.com"
		r, err := cl.Do(req)
		if err != nil {
			return err
		}
		defer r.Body.Close()
		if r.StatusCode == http.StatusPermanentRedirect {
			return fmt.Errorf("still redirecting after RemoveRoute")
		}
		return nil
	})
}

// retryBriefly retries op until it succeeds or ~2s passes, then fails the
// test with op's last error. Caddy's admin load returns before the previous
// server's listener is fully torn down, so a request issued right after a
// reload can land in the swap window and get reset (#251). A genuinely dead
// listener still fails: it keeps erroring past the deadline.
func retryBriefly(t *testing.T, desc string, op func() error) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := op()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %v", desc, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func selfSignedPEM(t *testing.T, base string) (certPEM, keyPEM []byte) {
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
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	kb, _ := x509.MarshalECPrivateKey(key)
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	return certPEM, keyPEM
}
