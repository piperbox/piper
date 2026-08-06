package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/piperbox/piper/internal/relay"
	"github.com/piperbox/piper/internal/version"
)

// shutdownGrace bounds how long a SIGTERM waits for in-flight webhook
// deliveries to finish or park. One attempt is capped at the delivery timeout,
// so this only has to cover that plus the parking write.
const shutdownGrace = 35 * time.Second

// versionRequested reports whether args ask for the build version. Kept
// separate so it can be unit-tested; it also gives piper-relay a symbol that
// imports internal/version so the release ldflags stamp actually lands.
func versionRequested(args []string) bool {
	return len(args) > 0 && (args[0] == "version" || args[0] == "--version")
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// readAppKey loads the GitHub App private key, refusing one any other user on
// the box could read. Only the world bits disqualify it: systemd stages
// LoadCredential= files at 0440 inside a per-unit tmpfs it grants nobody else
// access to, and that is how a DynamicUser= relay is meant to receive a key —
// the same path its TLS cert already takes. A world-readable key is still
// fatal, which catches the realistic mistake of scp'ing one in at 0644.
func readAppKey(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if perm := info.Mode().Perm(); perm&0o007 != 0 {
		return nil, fmt.Errorf("%s is world readable (mode %o); chmod 600 it", path, perm)
	}
	return os.ReadFile(path)
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

// adminStore is the slice of *relay.Store the admin subcommands need.
type adminStore interface {
	DisableAccount(username, accountType string) error
}

// apiAddrIsLoopback reports whether addr binds only the loopback interface.
// A bare ":8080" or "0.0.0.0:8080" binds all interfaces; "127.0.0.1:8080" /
// "[::1]:8080" / "localhost:8080" are loopback-only.
func apiAddrIsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// No port present; treat the whole string as the host.
		host = addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return false
	}
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// parseWebRedirects splits and validates the comma-separated redirect_uri
// prefix allowlist. Prefix matching is by strings.HasPrefix, so every prefix
// must pin the full host: absolute http(s) URL with a path ("https://host/...").
// Invalid entries are dropped with a log line rather than silently allowed.
func parseWebRedirects(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		u, err := url.Parse(p)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || !strings.HasPrefix(u.Path, "/") {
			log.Printf("piper-relay: ignoring invalid PIPER_RELAY_WEB_REDIRECTS entry %q (need https://host/path-prefix)", p)
			continue
		}
		out = append(out, p)
	}
	return out
}

const adminUsage = "usage: piper-relay admin disable [--org] <name>"

// runAdmin handles "piper-relay admin <cmd> ...". Currently:
// disable [--org] <name>. The --org flag picks the org holding <name> rather
// than the user — usernames are unique only per type (#411), so a bare name is
// ambiguous whenever both exist.
func runAdmin(st adminStore, args []string) error {
	if len(args) == 0 {
		return errors.New(adminUsage)
	}
	switch args[0] {
	case "disable":
		rest, accountType := args[1:], "user"
		if len(rest) > 0 && rest[0] == "--org" {
			rest, accountType = rest[1:], "org"
		}
		if len(rest) != 1 {
			return errors.New(adminUsage)
		}
		return st.DisableAccount(rest[0], accountType)
	default:
		return fmt.Errorf("unknown admin command %q", args[0])
	}
}

func main() {
	if versionRequested(os.Args[1:]) {
		fmt.Println(version.String())
		return
	}
	dataDir := env("PIPER_RELAY_DATA_DIR", "./relay-data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		log.Fatalf("data dir: %v", err)
	}
	st, err := relay.Open(filepath.Join(dataDir, "relay.db"))
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	if len(os.Args) > 1 && os.Args[1] == "admin" {
		if err := runAdmin(st, os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		return
	}

	if len(os.Args) > 1 && os.Args[1] == "enroll" {
		fs := flag.NewFlagSet("enroll", flag.ExitOnError)
		domain := fs.String("domain", "", "base domain the agent may serve (e.g. alice.example.com)")
		// The flag package stops parsing at the first non-flag argument, so a
		// leading positional <name> before --domain would otherwise never be
		// recognized. Peel it off before handing the rest to fs.Parse.
		args := os.Args[2:]
		var name string
		if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
			name = args[0]
			args = args[1:]
		}
		fs.Parse(args)
		if name == "" || *domain == "" {
			log.Fatal("usage: piper-relay enroll <name> --domain <base-domain>")
		}
		tok, err := st.Enroll(name, *domain)
		if err != nil {
			log.Fatalf("enroll: %v", err)
		}
		fmt.Printf("enrolled %s for %s\ntoken: %s\n", name, *domain, tok)
		return
	}

	st.Configure(
		env("PIPER_RELAY_APEX", "public.getpiper.dev"),
		atoiOr(env("PIPER_RELAY_MAX_AGENTS", "3"), 3),
		atoiOr(env("PIPER_RELAY_MAX_APPS", "10"), 10),
		atoiOr(env("PIPER_RELAY_MAX_DOMAINS", "5"), 5),
	)

	tlsAddr := env("PIPER_RELAY_TLS_ADDR", ":443")
	httpAddr := env("PIPER_RELAY_HTTP_ADDR", ":80")
	tunnelAddr := env("PIPER_RELAY_TUNNEL_ADDR", ":7000")
	apiAddr := env("PIPER_RELAY_API_ADDR", ":8080")
	tunnelPublic := env("PIPER_RELAY_TUNNEL_PUBLIC", "")

	// Opt-in PROXY protocol v2 on the public listeners (#485), for a relay
	// behind an L4 load balancer / TCP reverse proxy. Off by default and must
	// stay off unless such a proxy is the only path to the ports: a listener
	// that trusts PROXY headers from arbitrary peers lets anyone spoof their
	// source IP.
	proxyProto := os.Getenv("PIPER_RELAY_PROXY_PROTOCOL") == "1"
	if proxyProto {
		log.Print("piper-relay: PIPER_RELAY_PROXY_PROTOCOL=1 — :443/:80/:7000 require a PROXY v2 header (trusted L4 proxy in front)")
	}

	// Self-service login needs a GitHub OAuth app; without one the relay runs
	// operator-enroll-only (existing behaviour) and login completes only via
	// test approval.
	var v relay.Verifier
	if id := env("PIPER_RELAY_GITHUB_CLIENT_ID", ""); id != "" {
		v = relay.NewGitHubVerifier(id, env("PIPER_RELAY_GITHUB_CLIENT_SECRET", ""))
	} else if env("PIPER_RELAY_FAKE_APPROVE", "") == "1" {
		log.Print("piper-relay: PIPER_RELAY_FAKE_APPROVE=1 — device login auto-approves (TEST ONLY)")
		v = relay.NewAutoApproveVerifier("e2e-sub", "e2e")
	} else {
		log.Print("piper-relay: no PIPER_RELAY_GITHUB_CLIENT_ID; self-service login disabled")
		v = relay.NewFakeVerifier() // login routes exist but complete only via test approval
	}
	appSlug := os.Getenv("PIPER_RELAY_GITHUB_APP_SLUG")

	var ghApp *relay.GitHubApp
	appID := os.Getenv("PIPER_RELAY_GITHUB_APP_ID")
	keyPath := os.Getenv("PIPER_RELAY_GITHUB_APP_KEY")
	if appID != "" && keyPath != "" {
		pemBytes, err := readAppKey(keyPath)
		if err != nil {
			log.Fatalf("github app key: %v", err)
		}
		ghApp, err = relay.NewGitHubApp(relay.GitHubAppConfig{
			AppID:         appID,
			PrivateKeyPEM: string(pemBytes),
			WebhookSecret: os.Getenv("PIPER_RELAY_GITHUB_WEBHOOK_SECRET"),
			Slug:          appSlug,
		})
		if err != nil {
			log.Fatalf("github app: %v", err)
		}
		log.Printf("relay: GitHub App %s configured (brokered git deploys enabled)", appID)
	}

	if !apiAddrIsLoopback(apiAddr) {
		log.Printf("piper-relay: WARNING control API %s is not loopback-only; it serves bearer credentials in cleartext HTTP and must be fronted with TLS", apiAddr)
	}

	// Browser (dashboard) login: allowed redirect_uri prefixes, comma-separated.
	// Empty — or a missing client secret — leaves web login disabled (503).
	webRedirects := parseWebRedirects(env("PIPER_RELAY_WEB_REDIRECTS", ""))
	if len(webRedirects) > 0 && env("PIPER_RELAY_GITHUB_CLIENT_SECRET", "") == "" {
		log.Print("piper-relay: PIPER_RELAY_WEB_REDIRECTS set but no PIPER_RELAY_GITHUB_CLIENT_SECRET; web login disabled")
		webRedirects = nil
	}

	router := relay.NewRouter()

	// Infra-only ops surface (metrics + log export). Isolation is the bind
	// address — loopback by default, a private VPC IP in production — never
	// the SNI dispatcher, so no public hostname can route here. Each endpoint
	// is off unless its toggle is set; with neither, nothing binds at all.
	opsAddr := env("PIPER_RELAY_OPS_ADDR", "127.0.0.1:9090")
	metricsOn := os.Getenv("PIPER_RELAY_METRICS") == "1"
	logsOn := os.Getenv("PIPER_RELAY_LOGS") == "1"
	var metrics *relay.Metrics
	var ring *relay.LogRing
	if metricsOn {
		metrics = relay.NewMetrics(router)
	}
	if logsOn {
		ring = relay.NewLogRing(1000)
		log.SetOutput(io.MultiWriter(os.Stderr, ring))
	}
	if metricsOn || logsOn {
		opsHandler := relay.NewOpsHandler(metrics, ring)
		go func() {
			log.Printf("piper-relay: ops endpoint %s (metrics=%v logs=%v)", opsAddr, metricsOn, logsOn)
			srv := &http.Server{Addr: opsAddr, Handler: opsHandler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
			if err := srv.ListenAndServe(); err != nil {
				log.Fatalf("ops endpoint: %v", err)
			}
		}()
	}

	apiHandler := relay.NewAPIWithTunnel(st, v, tunnelPublic, router, webRedirects, ghApp)

	ctrl := apiHandler
	var delivery *relay.TunnelDelivery
	if ghApp != nil {
		delivery = relay.NewTunnelDelivery(st, router)
		// Retries parked events for boxes that are connected but were not
		// accepting deliveries; without it such an event waits for a reconnect
		// or another webhook that may never come (#294).
		delivery.StartSweeper(0)
		outer := http.NewServeMux()
		outer.Handle("POST /gh", relay.NewGitHubIngress(st, ghApp, delivery))
		outer.Handle("/", apiHandler)
		ctrl = outer

		// A restart mid-burst used to drop every in-flight delivery: GitHub had
		// already been sent its 202, so nothing would ever retry them. Draining
		// the pool on SIGTERM/SIGINT lets each one park instead (#295).
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			s := <-sig
			log.Printf("piper-relay: %s — draining in-flight webhook deliveries", s)
			ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
			defer cancel()
			delivery.Shutdown(ctx)
			os.Exit(0)
		}()
	}

	go func() {
		log.Printf("piper-relay: control API %s", apiAddr)
		if err := http.ListenAndServe(apiAddr, apiHandler); err != nil {
			log.Fatalf("control API: %v", err)
		}
	}()

	tlsCfg, err := relay.LoadWildcardConfig(env("PIPER_RELAY_TLS_CERT", ""), env("PIPER_RELAY_TLS_KEY", ""))
	if err != nil {
		log.Fatalf("wildcard cert: %v", err)
	}
	if tlsCfg == nil {
		log.Print("piper-relay: no wildcard cert (PIPER_RELAY_TLS_CERT/KEY); passthrough-only, shared-domain termination disabled")
	}

	log.Printf("piper-relay: TLS %s, HTTP %s, tunnel %s", tlsAddr, httpAddr, tunnelAddr)
	log.Fatal(relay.Serve(tlsAddr, httpAddr, tunnelAddr, st, tlsCfg, router, ctrl, ghApp, delivery, metrics, proxyProto))
}
