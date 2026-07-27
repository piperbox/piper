package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/go-acme/lego/v4/challenge"
	"github.com/go-acme/lego/v4/providers/dns/cloudflare"
	"github.com/piperbox/piper/internal/agent"
	"github.com/piperbox/piper/internal/api"
	"github.com/piperbox/piper/internal/caddy"
	"github.com/piperbox/piper/internal/certs"
	"github.com/piperbox/piper/internal/config"
	"github.com/piperbox/piper/internal/deploy"
	"github.com/piperbox/piper/internal/domain"
	"github.com/piperbox/piper/internal/runtime"
	"github.com/piperbox/piper/internal/source"
	"github.com/piperbox/piper/internal/source/github"
	"github.com/piperbox/piper/internal/store"
	"github.com/piperbox/piper/internal/tunnel"
	"github.com/piperbox/piper/internal/version"
	"github.com/piperbox/piper/internal/webhook"
)

const (
	drainTimeout    = 15 * time.Second
	shutdownTimeout = 20 * time.Second
)

type apiShutdowner interface {
	Shutdown(context.Context) error
	Close() error
}

type webhookLifecycle interface {
	stop(context.Context)
	close()
	wait(context.Context) bool
	cancel()
}

type listenerStopper interface{ Stop() }
type storeCloser interface {
	FailBuildingDeployments() (int64, error)
	Close() error
}

type tokenStore interface {
	CreateToken(label, scope string) (string, error)
	ListTokens() ([]store.Token, error)
	RevokeToken(label string) error
}

// relayTokenStore is the store slice relay-control provisioning needs.
type relayTokenStore interface {
	ListTokens() ([]store.Token, error)
	CreateToken(label, scope string) (string, error)
	DeleteToken(label string) error
}

// relayAppStore is the store slice the per-connect app re-push needs.
type relayAppStore interface {
	ListApps() ([]store.App, error)
	RunningPreviews() ([]store.Deployment, error)
}

// relayAppAnnouncer is the tunnel-client slice the per-connect app re-push
// needs.
type relayAppAnnouncer interface {
	BindRepo(app, repo, branch string) error
	Register(app string, pr int) (string, error)
}

// repushRelayApps re-announces this box's per-app relay state on every tunnel
// (re)connect. The relay's router is in-memory, and when a session registers it
// re-derives only the custom domains it can attribute to that agent. Two things
// have to come from the box instead:
//
//   - repo bindings, which live only in the box's store;
//   - relay-assigned hostnames (#369) — the relay does not re-derive an agent's
//     app hostnames when its session registers, the way it does custom domains.
//     (Since #405 the hostnames table names the agent, so it could; nothing has
//     been written to do it.) Without this re-push, every <hash>-<user> URL
//     drops TLS after a relay restart or a tunnel flap until the app is
//     redeployed, while custom domains keep serving.
//
// Only apps that already hold a hostname are re-registered, and registration is
// idempotent on the relay, so this claims no new hostname and consumes no app
// quota. terminated is false on a BYO/LAN box, which has no relay-assigned
// hostnames at all. One app's failure must not strand the rest, so errors are
// logged and the loop continues; the next connect retries.
func repushRelayApps(st relayAppStore, tc relayAppAnnouncer, terminated bool) {
	apps, err := st.ListApps()
	if err != nil {
		log.Printf("relay: re-push apps: %v", err)
		return
	}
	for _, a := range apps {
		if a.Repo != "" {
			if err := tc.BindRepo(a.Name, a.Repo, a.Branch); err != nil {
				log.Printf("relay: re-bind %s: %v", a.Name, err)
			}
		}
		if terminated && a.Hostname != "" {
			if _, err := tc.Register(a.Name, 0); err != nil {
				log.Printf("relay: re-register %s: %v", a.Name, err)
			}
		}
	}
	if !terminated {
		return
	}
	// Previews hold relay-assigned hostnames from the same register op, and are
	// missed by the loop above: they live on their own (app, pr) deployment rows,
	// not on the app row's single Hostname (#376). Without this a live PR-preview
	// URL goes dark on a relay restart and stays dark until the PR pushes again.
	// A failed lookup here must not cost the box the hostnames already restored.
	previews, err := st.RunningPreviews()
	if err != nil {
		log.Printf("relay: re-push previews: %v", err)
		return
	}
	for _, p := range previews {
		if _, err := tc.Register(p.App, p.PR); err != nil {
			log.Printf("relay: re-register %s PR %d: %v", p.App, p.PR, err)
		}
	}
}

// provisionRelayControl mints a control-API token for the relay and pushes it
// over the tunnel, once per enrollment (agent-push Token B — see the
// control-stream routing design). The token row itself is the marker: any row
// labeled relay:<base>, live OR revoked, means "already provisioned" or "the
// owner cut the relay off" — never re-mint. A new `piper connect` creates a new
// enrollment (new base domain) and so a fresh mint. If the push fails, the
// just-minted row is deleted so the next connect retries.
//
// mu serializes the whole list-then-mint sequence across concurrent OnConnect
// callbacks: without it, a session that flaps before the first push completes
// could have two goroutines both read an empty token list and each mint a
// duplicate relay:<base> admin token (the label has no unique constraint). One
// shared mutex per box closes that TOCTOU.
func provisionRelayControl(mu *sync.Mutex, st relayTokenStore, push func(string) error, baseDomain string) {
	mu.Lock()
	defer mu.Unlock()
	label := "relay:" + baseDomain
	toks, err := st.ListTokens()
	if err != nil {
		log.Printf("relay control provision: list tokens: %v", err)
		return
	}
	for _, tk := range toks {
		if tk.Label == label {
			return
		}
	}
	tok, err := st.CreateToken(label, "admin")
	if err != nil {
		log.Printf("relay control provision: mint: %v", err)
		return
	}
	if err := push(tok); err != nil {
		log.Printf("relay control provision: push: %v (will retry next connect)", err)
		_ = st.DeleteToken(label)
		return
	}
	log.Printf("relay control provision: pushed control bearer for %s", baseDomain)
}

// apiServers folds the control API's two servers (local tokenless +
// authenticated) into the one apiShutdowner slot shutdown() has, so both go
// through the same graceful drain (#221).
type apiServers []apiShutdowner

func (s apiServers) Shutdown(ctx context.Context) error {
	var first error
	for _, srv := range s {
		if err := srv.Shutdown(ctx); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (s apiServers) Close() error {
	var first error
	for _, srv := range s {
		if err := srv.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// startAuthAPI serves handler wrapped in RequireToken on an ephemeral loopback
// listener and returns the bound address. It is the control API's authenticated
// entry point: the relay tunnel dials it for control streams, so the bearer
// keeps gating internet-originated requests while the local listener
// (cfg.APIAddr) serves the on-box CLI tokenless (#221).
func startAuthAPI(st *store.Store, handler http.Handler) (string, *http.Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	srv := &http.Server{Handler: api.RequireToken(st, handler)}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("auth api serve: %v", err)
		}
	}()
	return ln.Addr().String(), srv, nil
}

// newLocalHandler picks the auth mode for the local control-API listener from
// its bind address: loopback serves tokenless (the bind is the trust boundary),
// while a non-loopback bind (the documented PIPER_API_ADDR=0.0.0.0:8088 LAN
// flow) keeps requiring the bearer — otherwise that flow would expose an
// unauthenticated control API to the whole LAN (#221).
func newLocalHandler(st *store.Store, handler http.Handler, addr string) http.Handler {
	if loopbackAddr(addr) {
		return handler
	}
	return api.RequireToken(st, handler)
}

// loopbackAddr reports whether addr binds only the loopback interface.
func loopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// dialAddr turns one of Caddy's *listen* addresses into a dialable one. Listen
// addresses are routinely port-only (":8080") or wildcard ("0.0.0.0:8080"), and
// neither can be dialed as a destination, so an unspecified host becomes
// loopback. An address that already names a host is returned untouched.
func dialAddr(listenAddr string) string {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return listenAddr
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		return net.JoinHostPort("127.0.0.1", port)
	}
	return listenAddr
}

// newDialLocal maps relay tunnel stream kinds to local addresses. Control
// streams go to the authenticated listener (authAddr) — never the tokenless
// local one, or the relay path would silently lose its bearer gate (#221).
// KindHTTP is plaintext HTTP for the box's HTTP listener in every mode — Caddy
// listens there in terminated mode (relay-terminated shared-domain apps) and in
// BYO mode alike, which is what lets custom-domain port-80 traffic reach the box
// (#228). Passthrough streams whose ClientHello offers acme-tls/1 are
// TLS-ALPN-01 validations and are spliced to the in-process solver (alpnAddr)
// instead of Caddy (httpsAddr), with the peeked hello replayed into whichever
// backend is dialed (#226).
//
// httpAddr/httpsAddr come from cfg and are NOT assumed to be :80/:443: a
// rootless install cannot bind a low port, so the macOS LaunchAgent runs Caddy
// on :8080/:8443. Hardcoding the privileged ports here made every such box
// silently unroutable from the relay while its apps were perfectly healthy
// (#399).
func newDialLocal(authAddr, alpnAddr, httpAddr, httpsAddr string) func(kind byte, stream net.Conn) (net.Conn, error) {
	return func(kind byte, stream net.Conn) (net.Conn, error) {
		switch {
		case kind == tunnel.KindControlAPI:
			return net.Dial("tcp", authAddr)
		case kind == tunnel.KindHTTP:
			return net.Dial("tcp", httpAddr)
		default:
			acme, consumed := agent.PeekALPN(stream)
			addr := httpsAddr
			if acme && alpnAddr != "" {
				addr = alpnAddr
			}
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				return nil, err
			}
			if _, err := conn.Write(consumed); err != nil {
				conn.Close()
				return nil, err
			}
			return conn, nil
		}
	}
}

// runTokenCmd implements `piperd token <create|list|revoke>`, writing directly
// to the on-box store. It needs no auth: running it is proof of box ownership.
func runTokenCmd(st tokenStore, args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: piperd token <create|list|revoke>")
	}
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("token create", flag.ContinueOnError)
		name := fs.String("name", "", "label for the token")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *name == "" {
			return fmt.Errorf("token create: --name is required")
		}
		tok, err := st.CreateToken(*name, "admin")
		if err != nil {
			return err
		}
		fmt.Fprintln(out, tok)
		return nil
	case "list":
		toks, err := st.ListTokens()
		if err != nil {
			return err
		}
		for _, tk := range toks {
			status := "active"
			if tk.RevokedAt != nil {
				status = "revoked"
			}
			fmt.Fprintf(out, "%s\t%s\t%s\n", tk.Label, tk.Scope, status)
		}
		return nil
	case "revoke":
		if len(args) < 2 {
			return fmt.Errorf("usage: piperd token revoke <name>")
		}
		return st.RevokeToken(args[1])
	default:
		return fmt.Errorf("unknown token subcommand %q", args[0])
	}
}

// versionRequested reports whether args ask for the build version. Kept
// separate so it can be unit-tested; it also gives piperd/piper-relay a symbol
// that imports internal/version so the release ldflags stamp actually lands.
func versionRequested(args []string) bool {
	return len(args) > 0 && (args[0] == "version" || args[0] == "--version")
}

func main() {
	if versionRequested(os.Args[1:]) {
		fmt.Println(version.String())
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "token" {
		dataDir, owner, err := resolveTokenDataDir(os.Args[2:])
		if err != nil {
			log.Fatalf("token: %v", err)
		}
		if err := os.MkdirAll(dataDir, 0o755); err != nil {
			log.Fatalf("data dir: %v", err)
		}
		st, err := store.Open(filepath.Join(dataDir, "piper.db"))
		if err != nil {
			log.Fatalf("store: %v", err)
		}
		if err := runTokenCmd(st, os.Args[2:], os.Stdout); err != nil {
			st.Close()
			log.Fatalf("token: %v", err)
		}
		// Close before chowning so any -wal/-shm are flushed to their final
		// state, then hand the DB files to the service's DynamicUser (#134).
		st.Close()
		if owner != nil {
			if err := chownDataFiles(dataDir, owner.uid, owner.gid); err != nil {
				log.Fatalf("data dir: chown: %v", err)
			}
		}
		return
	}

	cfg := config.Load()
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatalf("data dir: %v", err)
	}

	st, err := store.Open(filepath.Join(cfg.DataDir, "piper.db"))
	if err != nil {
		log.Fatalf("store: %v", err)
	}

	rt, err := runtime.NewDockerRuntime()
	if err != nil {
		log.Fatalf("docker: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Unless PIPER_SKIP_CADDY is set (e.g. a caddy is already running), manage one.
	var mgr *caddy.Manager
	if os.Getenv("PIPER_SKIP_CADDY") == "" {
		opts := []caddy.Option{}
		if cfg.RelayAddr != "" && !cfg.Terminated {
			opts = append(opts, caddy.WithHTTPS(cfg.HTTPSAddr))
		}
		mgr, err = caddy.StartManager(cfg.CaddyAdmin, cfg.HTTPAddr, opts...)
		if err != nil {
			log.Fatalf("caddy: %v", err)
		}
	}

	dep := deploy.New(st, rt, caddy.NewClient(cfg.CaddyAdmin), cfg.BaseDomain)

	var domMgr *domain.Manager
	var alpnSolver *certs.ALPNSolver
	if cfg.RelayAddr != "" {
		relayHost := cfg.RelayAddr
		if h, _, err := net.SplitHostPort(cfg.RelayAddr); err == nil {
			relayHost = h
		}
		// The TLS-ALPN-01 solver runs whenever relay mode is up: idle it is one
		// dormant loopback listener. The relay splices acme-tls/1 ClientHellos
		// down the tunnel to it (see newDialLocal); the per-app domain
		// lifecycle drives issuance against it.
		alpnSolver, err = certs.NewALPNSolver("127.0.0.1:0")
		if err != nil {
			log.Fatalf("alpn solver: %v", err)
		}
		opts := domain.Options{
			Store: st, Proxy: caddy.NewClient(cfg.CaddyAdmin), Router: dep,
			DataDir: cfg.DataDir, RelayHost: relayHost, HTTPSListen: ":443",
			Issuer: func(provider, token string) (domain.Issuer, error) {
				if os.Getenv("PIPER_TEST_ISSUER") == "selfsigned" {
					return testSelfSignedIssuer{}, nil
				}
				key, err := certs.LoadOrCreateAccountKey(filepath.Join(cfg.DataDir, "acme_account.key"))
				if err != nil {
					return nil, err
				}
				return certs.NewCloudflareIssuer(cfg.ACMEEmail, cfg.ACMECA, token, key)
			},
			AppIssuer: func() (domain.Issuer, error) {
				if os.Getenv("PIPER_TEST_ISSUER") == "selfsigned" {
					return testSelfSignedIssuer{}, nil
				}
				key, err := certs.LoadOrCreateAccountKey(filepath.Join(cfg.DataDir, "acme_account.key"))
				if err != nil {
					return nil, err
				}
				return certs.New(certs.Config{
					Email: cfg.ACMEEmail, CADirURL: cfg.ACMECA,
					ALPNSolver: alpnSolver, AccountKey: key,
				})
			},
		}
		if !cfg.Terminated {
			opts.EnvDomain = cfg.BaseDomain // env-managed BYO: API writes are 409
		}
		if os.Getenv("PIPER_TEST_ISSUER") == "selfsigned" {
			// E2E: the fake issuer implies the test domains have no real DNS
			// either. Resolve every name to loopback so the per-app DNS gate
			// (and dns_ok) sees them pointing at the loopback relay.
			opts.Resolve = func(context.Context, string) ([]net.IP, error) {
				return []net.IP{net.ParseIP("127.0.0.1")}, nil
			}
		}
		domMgr = domain.New(opts)
	}

	// The control API mux, shared by both listeners. wh is assigned below in
	// relay mode; the onGitHubApp closure captures the variable so `piper github
	// setup` can start serving webhooks without a restart.
	var wh *webhookStarter
	var dm api.DomainManager
	if domMgr != nil {
		dm = domMgr
	}
	// The tunnel client is created here, ahead of api.New, so the link handler
	// can push repo bindings to the relay; Run and the rest of its wiring still
	// start later, in the relay block below. binder is declared as the
	// api.RepoBinder interface (not a *agent.TunnelClient) so that on a
	// LAN-only box it stays genuinely nil — a nil *agent.TunnelClient boxed into
	// the interface would be a non-nil interface value and would defeat the
	// "binder != nil" guard in the link handler.
	var binder api.RepoBinder
	var tc *agent.TunnelClient
	if cfg.RelayAddr != "" {
		tc = &agent.TunnelClient{}
		binder = tc
	}
	var ghTokenFn func(repo string) (string, error)
	if tc != nil {
		ghTokenFn = tc.GitHubToken
	}
	apiHandler := api.New(st, dep, cfg.BaseDomain, cfg.GitHubAPIBase, func() {
		if wh != nil {
			wh.start()
		}
	}, dm, binder, func() string {
		// What `piper github reset` leaves behind: the same decision, re-run as
		// if the row it just deleted had never been there.
		return decideWebhookProvider(store.ErrNotFound, cfg, wh != nil && wh.ghToken != nil).name()
	}, newRepoFetcher(st, cfg, ghTokenFn))

	// The authenticated entry point. Always on, so LAN-only and relay-connected
	// boxes run the identical listener topology; the relay tunnel below is its
	// consumer (#221).
	authAddr, authSrv, err := startAuthAPI(st, apiHandler)
	if err != nil {
		log.Fatalf("auth api listen: %v", err)
	}

	// Relay mode: dial the relay and forward its streams. Terminated (free-tier)
	// mode holds no box cert and serves apps on :80; the relay terminates TLS and
	// opens KindHTTP streams. Non-terminated (BYO-domain) mode obtains a wildcard
	// cert, serves :443, and answers KindPassthrough streams. Control streams go
	// to the authenticated listener — never the tokenless local one.
	if cfg.RelayAddr != "" {
		dialLocal := newDialLocal(authAddr, alpnSolver.Addr(), dialAddr(cfg.HTTPAddr), dialAddr(cfg.HTTPSAddr))
		if !cfg.Terminated {
			if cfg.TLSCertFile != "" {
				certPEM, err := os.ReadFile(cfg.TLSCertFile)
				if err != nil {
					log.Fatalf("relay tls: %v", err)
				}
				keyPEM, err := os.ReadFile(cfg.TLSKeyFile)
				if err != nil {
					log.Fatalf("relay tls: %v", err)
				}
				// Through the manager's cert set, not straight into Caddy, so
				// a per-app domain sync can't clobber the file-provided cert.
				if err := domMgr.LoadStaticCert(certPEM, keyPEM); err != nil {
					log.Fatalf("relay tls: %v", err)
				}
			} else {
				iss, err := newEnvIssuer(cfg)
				if err != nil {
					log.Fatalf("relay tls: %v", err)
				}
				if err := domMgr.RunEnv(ctx, iss); err != nil {
					log.Fatalf("relay tls: %v", err)
				}
			}
		}
		// One mutex shared by every OnConnect callback, so overlapping
		// (re)connects can't race the list-then-mint and double-provision.
		var provisionMu sync.Mutex
		tc.OnConnect = func() {
			provisionRelayControl(&provisionMu, st, tc.Provision, cfg.BaseDomain)
			if cfg.Terminated {
				domMgr.OnRelayConnect() // gated like Resume: box-wide API configs exist only here
			}
			repushRelayApps(st, tc, cfg.Terminated)
			if cfg.Terminated {
				// Caddy comes up with a bare config, so a running app's own
				// host route needs re-arming too — and in terminated mode
				// primaryHost resolves through the registrar, so this can only
				// run once the tunnel is up (#371).
				dep.ResumeRoutes()
			}
		}
		// The registrar must be installed BEFORE the tunnel starts: OnConnect
		// calls dep.ResumeRoutes, which reads it. Starting tc.Run first both
		// races that read and — if the connect won — would resolve primaryHost
		// with a nil registrar and arm "<app>.<base>" instead of the
		// relay-assigned hostname, leaving the real URL unrouted. Setting it
		// here gives the goroutine a happens-before edge (#373).
		if cfg.Terminated {
			dep.SetHostnameRegistrar(tc)
		}
		go tc.Run(ctx, cfg.RelayAddr, cfg.RelayToken, cfg.BaseDomain, dialLocal)
		domMgr.SetRelay(tc)
		if cfg.Terminated {
			domMgr.Resume() // box-wide API-managed config; env mode has none
		}
		domMgr.ResumeAppDomains()
		go domMgr.StartRenewals(ctx)

		// The webhook deployer must carry the registrar on exactly the same
		// condition as the API deployer above: a git-push deploy and an API
		// deploy have to land on the same hostname. Declared as the interface
		// so a LAN-only box gets a genuinely nil registrar rather than a
		// typed-nil that would slip past the deployer's own guard.
		var whRegistrar deploy.HostnameRegistrar
		if cfg.Terminated {
			whRegistrar = tc
		}
		wh = newWebhookStarter(cfg, st, rt, tc.GitHubToken, whRegistrar)
		_, err := st.GetGitHubApp()
		switch decideWebhookProvider(err, cfg, wh.ghToken != nil) {
		case webhookProviderNone:
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				log.Printf("github app lookup: %v; webhook listener not started", err)
			} else {
				log.Printf("no GitHub App configured; run `piper github setup` to enable git deploys")
			}
		default:
			wh.start()
		}
	}

	// A box without relay-terminated hostnames re-arms at startup instead: its
	// hosts are "<app>.<base>", so primaryHost needs no registrar and there is
	// no tunnel to wait for (#371).
	if !cfg.Terminated {
		dep.ResumeRoutes()
	}

	// The local listener: tokenless on a loopback bind — whoever can run the
	// CLI on the box already owns the Docker socket piperd drives. A LAN bind
	// keeps the bearer (see newLocalHandler). Internet-originated
	// (relay-proxied) requests never land here; they go to the authenticated
	// listener above (#221).
	srv := &http.Server{Addr: cfg.APIAddr, Handler: newLocalHandler(st, apiHandler, cfg.APIAddr)}
	go func() {
		log.Printf("piperd listening on %s (apps at *.%s)", cfg.APIAddr, cfg.BaseDomain)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down")
	var mgrStop listenerStopper
	if mgr != nil {
		mgrStop = mgr
	}
	var whLifecycle webhookLifecycle
	if wh != nil {
		whLifecycle = wh
	}
	if alpnSolver != nil {
		_ = alpnSolver.Close()
	}
	shutdown(apiServers{srv, authSrv}, whLifecycle, mgrStop, st)
	os.Exit(0)
}

func shutdown(api apiShutdowner, wh webhookLifecycle, mgr listenerStopper, st storeCloser) {
	shutdownWithTimeouts(api, wh, mgr, st, drainTimeout, shutdownTimeout)
}

func shutdownWithTimeouts(api apiShutdowner, wh webhookLifecycle, mgr listenerStopper, st storeCloser, drain, overall time.Duration) {
	overallCtx, cancelOverall := context.WithTimeout(context.Background(), overall)
	defer cancelOverall()
	drainCtx, cancelDrain := context.WithTimeout(overallCtx, drain)
	defer cancelDrain()

	var calls sync.WaitGroup
	if api != nil {
		calls.Add(1)
		go func() { defer calls.Done(); _ = api.Shutdown(drainCtx) }()
	}
	if wh != nil {
		calls.Add(1)
		go func() { defer calls.Done(); wh.stop(drainCtx) }()
	}
	entryDone := make(chan struct{})
	go func() { calls.Wait(); close(entryDone) }()

	entryDrained := false
	select {
	case <-entryDone:
		entryDrained = true
	case <-drainCtx.Done():
	}

	workDrained := entryDrained
	if wh != nil && entryDrained {
		workDrained = wh.wait(drainCtx)
	}
	if !workDrained {
		if api != nil {
			_ = api.Close()
		}
		if wh != nil {
			wh.close()
		}
	}
	if wh != nil {
		wh.cancel()
		if !workDrained {
			_ = wh.wait(overallCtx)
		}
	}
	if !workDrained {
		// API handlers are cancelled by Close but are not tracked separately.
		// Keep shared infrastructure alive for their reserved cleanup window.
		<-overallCtx.Done()
	}
	if mgr != nil {
		mgr.Stop()
	}
	if st != nil {
		// A deploy started over the API runs in a goroutine this drain does not
		// track, and a Docker build routinely outlasts the drain window, so its
		// row can still be "building" here. Finalize it "failed" while the store
		// is open — otherwise the row survives shutdown as a permanent "building"
		// (#158). Any deploy that finished during the drain is no longer building.
		if n, err := st.FailBuildingDeployments(); err != nil {
			log.Printf("shutdown: fail building deployments: %v", err)
		} else if n > 0 {
			log.Printf("shutdown: marked %d in-flight deploy(s) failed", n)
		}
		_ = st.Close()
	}
}

// webhookStarter brings up the webhook listener and its Caddy route exactly
// once, from stored GitHub App creds. start() is safe to call both at boot (if
// creds already exist) and later from the exchange endpoint.
type webhookStarter struct {
	cfg     config.Config
	st      *store.Store
	rt      *runtime.DockerRuntime
	ghToken func(repo string) (string, error) // nil unless brokered
	// registrar assigns each app its relay-terminated public hostname. Nil on a
	// LAN-only box; non-nil whenever the API deployer carries one, and it must
	// be the same one — see newWebhookDeployer.
	registrar deploy.HostnameRegistrar
	once      sync.Once
	srv       *http.Server
	handler   *webhook.Handler
}

func newWebhookStarter(cfg config.Config, st *store.Store, rt *runtime.DockerRuntime, ghToken func(repo string) (string, error), registrar deploy.HostnameRegistrar) *webhookStarter {
	return &webhookStarter{cfg: cfg, st: st, rt: rt, ghToken: ghToken, registrar: registrar}
}

// newWebhookDeployer builds the deployer that serves git-push deploys. It must
// carry the same hostname registrar as the API deployer: without one, routing
// falls back to <app>.<baseDom>, which on a relay-terminated box sits two
// labels under the apex — outside the relay's single-label wildcard
// certificate and unknown to its router — so the app is unreachable however
// healthy the container is. reg is nil on a LAN-only box, which keeps that
// local convention deliberately.
func newWebhookDeployer(st *store.Store, rt runtime.Runtime, routes deploy.RouteSetter, baseDomain string, reg deploy.HostnameRegistrar) *deploy.Deployer {
	d := deploy.New(st, rt, routes, baseDomain)
	if reg != nil {
		d.SetHostnameRegistrar(reg)
	}
	return d
}

// webhookProvider is the outcome of applying the BYO-over-brokered
// precedence rule: which GitHub credential source, if any, the webhook
// listener should use.
type webhookProvider int

const (
	webhookProviderNone webhookProvider = iota
	webhookProviderBYO
	webhookProviderBrokered
)

// decideWebhookProvider is the one place the precedence rule lives, so the
// boot gate and webhookStarter.run can't drift apart on it (they both call
// this instead of each re-deriving their own guard). ghErr is the error from
// st.GetGitHubApp(): a locally stored App is an explicit BYO override and
// always wins over the relay's offer, so ghErr == nil (a row exists) wins
// regardless of cfg. Only store.ErrNotFound means "no local row, brokered
// may proceed" — any other error means we could not determine whether this
// box has its own credentials, so we fail closed rather than silently
// switching it to trusting the relay.
func decideWebhookProvider(ghErr error, cfg config.Config, hasGHToken bool) webhookProvider {
	switch {
	case ghErr == nil:
		return webhookProviderBYO
	case errors.Is(ghErr, store.ErrNotFound) && cfg.GitHubBrokered && cfg.WebhookSecret != "" && hasGHToken:
		return webhookProviderBrokered
	default:
		return webhookProviderNone
	}
}

// name is the wire spelling the control API reports to `piper github reset`.
func (p webhookProvider) name() string {
	switch p {
	case webhookProviderBYO:
		return "byo"
	case webhookProviderBrokered:
		return "brokered"
	default:
		return "none"
	}
}

// shadowWarning is the line that makes a passed-over brokered App visible.
// The precedence rule is right — a locally stored App is a deliberate trust
// boundary and must win — but a box that once ran `piper github setup` then
// enrolled on a brokering relay silently verifies deliveries against the wrong
// secret, and the only signal is the absence of the brokered log line (#299).
func shadowWarning(prov webhookProvider, cfg config.Config) string {
	if prov != webhookProviderBYO || !cfg.GitHubBrokered {
		return ""
	}
	return "webhook: the relay offers a brokered GitHub App, shadowed by this box's own; " +
		"run `piper github reset` to use the relay's"
}

// newRepoFetcher builds the FetchRepoFunc behind POST /v1/apps/{name}/deploy-
// from-repo: a manual deploy of a linked app fetches the repo the same way a
// webhook push would. The provider is re-derived per call (it is cheap to
// build) so a `piper github setup` run after boot takes effect without a
// restart. ghToken is nil unless a brokering relay is connected.
func newRepoFetcher(st *store.Store, cfg config.Config, ghToken func(repo string) (string, error)) api.FetchRepoFunc {
	return func(ctx context.Context, repo, ref, destDir string) error {
		gh, err := st.GetGitHubApp()
		var prov source.Provider
		switch decideWebhookProvider(err, cfg, ghToken != nil) {
		case webhookProviderBYO:
			p, err := github.New(github.Config{
				AppID: gh.AppID, PrivateKeyPEM: gh.PrivateKey, WebhookSecret: gh.WebhookSecret,
				APIBase: cfg.GitHubAPIBase,
			})
			if err != nil {
				return fmt.Errorf("github provider: %w", err)
			}
			prov = p
		case webhookProviderBrokered:
			prov = github.NewWithTokens(
				github.Config{WebhookSecret: cfg.WebhookSecret},
				github.RelayTokens{Ask: ghToken},
			)
		default:
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("github app lookup: %w", err)
			}
			return api.ErrNoGitHubApp
		}
		return prov.Fetch(ctx, source.Event{Repo: repo, SHA: ref}, destDir)
	}
}

func (w *webhookStarter) start() { w.once.Do(w.run) }

func (w *webhookStarter) run() {
	var prov source.Provider

	gh, err := w.st.GetGitHubApp()
	switch decideWebhookProvider(err, w.cfg, w.ghToken != nil) {
	case webhookProviderBYO:
		p, err := github.New(github.Config{
			AppID: gh.AppID, PrivateKeyPEM: gh.PrivateKey, WebhookSecret: gh.WebhookSecret,
			APIBase: w.cfg.GitHubAPIBase,
		})
		if err != nil {
			log.Printf("webhook: github provider: %v", err)
			return
		}
		prov = p
		log.Printf("webhook: using this box's own GitHub App %d", gh.AppID)
		if warn := shadowWarning(webhookProviderBYO, w.cfg); warn != "" {
			log.Print(warn)
		}
	case webhookProviderBrokered:
		prov = github.NewWithTokens(
			github.Config{WebhookSecret: w.cfg.WebhookSecret},
			github.RelayTokens{Ask: w.ghToken},
		)
		log.Printf("webhook: using the relay's GitHub App (brokered)")
	default:
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			log.Printf("webhook: local GitHub App lookup: %v; not starting a listener", err)
		} else {
			log.Printf("webhook: no GitHub App configured")
		}
		return
	}

	wdep := newWebhookDeployer(w.st, w.rt, caddy.NewClient(w.cfg.CaddyAdmin), w.cfg.BaseDomain, w.registrar)
	w.handler = webhook.New(prov, w.st, wdep, w.cfg.BaseDomain)
	w.srv = &http.Server{Addr: w.cfg.WebhookAddr, Handler: w.handler}
	go func() {
		if err := w.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("webhook serve: %v", err)
		}
	}()
	_, portStr, _ := net.SplitHostPort(w.cfg.WebhookAddr)
	port, _ := strconv.Atoi(portStr)
	if err := caddy.NewClient(w.cfg.CaddyAdmin).UpsertRoute("hooks."+w.cfg.BaseDomain, port); err != nil {
		log.Printf("webhook route: %v", err)
	}
	log.Printf("webhook listening on %s", w.cfg.WebhookAddr)
}

func (w *webhookStarter) stop(ctx context.Context) {
	if w == nil {
		return
	}
	w.once.Do(func() {})
	if w.handler != nil {
		w.handler.StopAccepting()
	}
	if w.srv != nil {
		_ = w.srv.Shutdown(ctx)
	}
}

func (w *webhookStarter) close() {
	if w != nil && w.srv != nil {
		_ = w.srv.Close()
	}
}

func (w *webhookStarter) wait(ctx context.Context) bool {
	return w == nil || w.handler == nil || w.handler.WaitContext(ctx)
}

func (w *webhookStarter) cancel() {
	if w != nil && w.handler != nil {
		w.handler.Cancel()
	}
}

func newDNSProvider(name string) (challenge.Provider, error) {
	switch name {
	case "", "cloudflare":
		return cloudflare.NewDNSProvider()
	default:
		return nil, fmt.Errorf("unsupported DNS provider %q", name)
	}
}

// testSelfSignedIssuer is an e2e hook (PIPER_TEST_ISSUER=selfsigned): it
// issues a self-signed wildcard cert instead of ACME so end-to-end tests can
// exercise the domain-config flow without a CA or real DNS.
type testSelfSignedIssuer struct{}

func (testSelfSignedIssuer) Obtain(domains []string) ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: domains[0]},
		DNSNames:     domains,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	return certPEM, keyPEM, nil
}

// newEnvIssuer builds the env-managed issuer: DNS provider by name with creds
// from the provider's own env vars (the pre-#102 path), ACME account key
// persisted in the data dir.
func newEnvIssuer(cfg config.Config) (domain.Issuer, error) {
	if os.Getenv("PIPER_TEST_ISSUER") == "selfsigned" {
		return testSelfSignedIssuer{}, nil
	}
	provider, err := newDNSProvider(cfg.DNSProvider)
	if err != nil {
		return nil, err
	}
	key, err := certs.LoadOrCreateAccountKey(filepath.Join(cfg.DataDir, "acme_account.key"))
	if err != nil {
		return nil, err
	}
	return certs.New(certs.Config{
		Email: cfg.ACMEEmail, CADirURL: cfg.ACMECA,
		DNSProvider: provider, AccountKey: key,
	})
}
