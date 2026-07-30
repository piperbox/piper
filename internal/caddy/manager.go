package caddy

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/caddyserver/caddy/v2"
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp"              // http app, server, host matcher
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy" // reverse_proxy handler
	_ "github.com/caddyserver/caddy/v2/modules/caddytls"               // tls app, load_pem
)

// Manager owns the in-process Caddy instance. Caddy is embedded as a library,
// so no external `caddy` binary is required.
//
// Caddy runs as a process-global singleton: caddy.Load and caddy.Stop act on
// one shared instance, so there can be at most one live Manager per process. A
// second StartManager would clobber the first's config, and any Stop tears down
// the shared global — so StartManager enforces the invariant (see managerActive).
type Manager struct{}

// managerActive is true while a Manager is live. It guards the process-global
// Caddy singleton so a second StartManager fails loudly instead of silently
// replacing a running Manager's config.
var managerActive atomic.Bool

type managerOpts struct {
	httpListen  string
	httpsListen string // "" ⇒ no TLS listener
	adminAddr   string
}

// Option configures StartManager.
type Option func(*managerOpts)

// WithHTTPS adds a TLS listener on listen, disables Caddy's automatic HTTPS
// (piperd owns certs), and enables the tls app so load_pem certs are served.
func WithHTTPS(listen string) Option {
	return func(o *managerOpts) { o.httpsListen = listen }
}

// baseConfig builds the Caddy JSON bootstrap config for these options.
func (o *managerOpts) baseConfig() map[string]any {
	listens := []string{o.httpListen}
	// LAN mode is plain HTTP on :80: disable automatic HTTPS so a host-matched
	// route provisions no internal cert and stands up no :80 redirect server.
	// The relay/TLS path (WithHTTPS) owns certs and disables it for the same
	// reason.
	piper := map[string]any{
		"listen":          listens,
		"routes":          []any{},
		"automatic_https": map[string]any{"disable": true},
	}
	apps := map[string]any{"http": map[string]any{"servers": map[string]any{"piper": piper}}}
	if o.httpsListen != "" {
		piper["listen"] = []string{o.httpListen, o.httpsListen}
		piper["tls_connection_policies"] = []any{map[string]any{}}
		apps["tls"] = map[string]any{"certificates": map[string]any{"load_pem": []any{}}}
	}
	return map[string]any{
		"admin": map[string]any{"listen": o.adminAddr},
		"apps":  apps,
	}
}

// StartManager runs Caddy in-process with an admin-enabled base config: one HTTP
// server named "piper" on httpListen with empty routes. Options can add a TLS
// listener (WithHTTPS). Teardown is via Manager.Stop.
//
// Caddy is process-global, so at most one Manager may be live at a time;
// StartManager returns an error if another is already active (see Manager).
func StartManager(adminBase, httpListen string, opts ...Option) (*Manager, error) {
	if !managerActive.CompareAndSwap(false, true) {
		return nil, fmt.Errorf("caddy Manager already active: Caddy is process-global, at most one per process")
	}
	o := &managerOpts{httpListen: httpListen, adminAddr: strings.TrimPrefix(adminBase, "http://")}
	for _, opt := range opts {
		opt(o)
	}
	addrs := []string{o.adminAddr, o.httpListen}
	if o.httpsListen != "" {
		addrs = append(addrs, o.httpsListen)
	}
	if err := preflightListen(addrs); err != nil {
		managerActive.Store(false)
		return nil, err
	}
	cfg, _ := json.Marshal(o.baseConfig())
	if err := caddy.Load(cfg, true); err != nil {
		managerActive.Store(false)
		return nil, fmt.Errorf("start embedded caddy: %w", err)
	}
	m := &Manager{}
	if err := waitAdmin(adminBase, 10*time.Second); err != nil {
		m.Stop()
		return nil, err
	}
	return m, nil
}

// preflightListen verifies no other process already holds any of addrs. Caddy
// binds with SO_REUSEPORT, so a foreign listener never surfaces as a bind
// error — the kernel silently splits traffic between the two processes (#126).
// A plain net.Listen (no SO_REUSEPORT) does fail against any existing binder,
// which is exactly the signal Caddy's own bind hides; on success the probe is
// closed and Caddy binds the port for real.
func preflightListen(addrs []string) error {
	for _, addr := range addrs {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("listen address %s is already held by another process (another piperd instance, or a stray caddy?) — piperd would start but receive no traffic; stop it first (`sudo systemctl stop piperd`, or `brew services stop piper` on macOS): %w", addr, err)
		}
		l.Close()
	}
	return nil
}

func waitAdmin(base string, d time.Duration) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/config/")
		if err == nil {
			resp.Body.Close()
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("caddy admin API not ready at %s", base)
}

func (m *Manager) Stop() {
	_ = caddy.Stop()
	managerActive.Store(false)
}
