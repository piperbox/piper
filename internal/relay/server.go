package relay

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/piperbox/piper/internal/tunnel"
)

// connQueue adapts SNI-dispatched control-plane connections into a
// net.Listener so one http.Server can serve them all. handlePublic pushes each
// terminated TLS conn; the server owns its lifetime from there.
type connQueue struct {
	ch        chan net.Conn
	done      chan struct{}
	closeOnce sync.Once
}

func newConnQueue() *connQueue {
	return &connQueue{ch: make(chan net.Conn), done: make(chan struct{})}
}

func (q *connQueue) Accept() (net.Conn, error) {
	select {
	case c := <-q.ch:
		return c, nil
	case <-q.done:
		return nil, net.ErrClosed
	}
}

// Close is safe under concurrent calls: sync.Once guarantees the done channel
// is closed exactly once, so racing callers can never double-close it.
func (q *connQueue) Close() error {
	q.closeOnce.Do(func() { close(q.done) })
	return nil
}

func (q *connQueue) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4zero} }

func (q *connQueue) push(c net.Conn) {
	select {
	case q.ch <- c:
	case <-q.done:
		c.Close()
	}
}

// Serve runs the relay: it accepts agent tunnels on tunnelAddr, public TLS
// traffic on tlsAddr (routed by SNI), and public plain HTTP on httpAddr
// (routed by Host, custom domains only — #228). tlsCfg is the wildcard
// config for relay-terminated shared-domain apps; nil ⇒ passthrough-only.
// ctrl, when non-nil and a wildcard cert is armed, is the relay's own HTTP API,
// served TLS-terminated at SNI "api.<apex>" (#73). ghApp, when non-nil, lets the
// relay broker GitHub tokens over the control channel (#289); nil means BYO-only.
// delivery, when non-nil, drains any webhooks parked for a box while it was
// disconnected as soon as its tunnel reconnects; nil (no App configured) skips
// the drain. m, when non-nil, receives traffic counters; nil disables
// instrumentation. Blocks until a listener fails.
func Serve(tlsAddr, httpAddr, tunnelAddr string, st *Store, tlsCfg *tls.Config, router *Router, ctrl http.Handler, ghApp *GitHubApp, delivery *TunnelDelivery, m *Metrics) error {
	var ctrlQ *connQueue
	if ctrl != nil && tlsCfg != nil {
		ctrlQ = newConnQueue()
		srv := &http.Server{Handler: ctrl, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
		go func() { _ = srv.Serve(ctrlQ) }()
		// Tie the control server's lifetime to Serve's: when Serve returns (a
		// listener failed), stop the control server so its goroutine doesn't
		// outlive us. srv.Close closes ctrlQ, which unblocks its Accept and ends
		// srv.Serve. Latent while main log.Fatals on Serve's return, but correct
		// the moment Serve is reused.
		defer func() { _ = srv.Close() }()
	}
	ctrlHost := "api." + st.apexOrDefault()

	tunLn, err := net.Listen("tcp", tunnelAddr)
	if err != nil {
		return err
	}
	go acceptTunnels(tunLn, st, router, ghApp, delivery, m)

	httpLn, err := net.Listen("tcp", httpAddr)
	if err != nil {
		return err
	}
	go acceptHTTP(httpLn, router, m)

	tlsLn, err := net.Listen("tcp", tlsAddr)
	if err != nil {
		return err
	}
	for {
		conn, err := tlsLn.Accept()
		if err != nil {
			return err
		}
		m.ConnAccepted("tls")
		go handlePublic(conn, router, tlsCfg, ctrlHost, ctrlQ, m)
	}
}

// disabledPollInterval is how often each live session's watchdog re-reads its
// account's kill-switch flag from the store. It is a package var (cf. pollSleep
// in cmd/piper) so tests can drive eviction with a short tick instead of the
// production interval; production leaves it at 5s.
var disabledPollInterval = 5 * time.Second

// tunnelAuth is the relay's handshake authorizer: the presented token must
// resolve to a live (non-disabled) agent whose enrolled base domain matches the
// one it claims. A disabled account fails here (Authenticate returns ErrBadToken)
// — this is the auth-layer rejection that turns a disabled account away at
// reconnect, before any session or watchdog exists.
func tunnelAuth(st *Store) tunnel.Auth {
	return func(token, base string) error {
		ag, err := st.Authenticate(token)
		if err != nil {
			return err
		}
		if ag.BaseDomain != base {
			return ErrBadToken // token may only claim its enrolled base domain
		}
		return nil
	}
}

func acceptTunnels(ln net.Listener, st *Store, router *Router, ghApp *GitHubApp, delivery *TunnelDelivery, m *Metrics) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		m.ConnAccepted("tunnel")
		go serveTunnel(conn, st, router, st.AgentDisabled, ghApp, delivery)
	}
}

// serveTunnel authenticates one agent tunnel, registers it, and runs the
// per-session kill-switch watchdog until the session ends. disabled reports
// whether the session's account has been disabled; it is a parameter (defaulting
// to st.AgentDisabled) so tests can inject store-read failures. ghApp is
// threaded through to serveControl for token-brokering control ops. delivery,
// when non-nil, drains any webhooks parked while this box was disconnected.
func serveTunnel(conn net.Conn, st *Store, router *Router, disabled func(string) (bool, error), ghApp *GitHubApp, delivery *TunnelDelivery) {
	// Record what the peer claimed so a rejection can name it. Without this the
	// relay closed the connection silently and a box with a stale enrollment was
	// invisible from both sides at once — the agent reported itself connected
	// (#400) and the relay's own logs never mentioned it. The claimed base
	// domain is unauthenticated peer input, so it is logged quoted.
	var claimedBase string
	auth := func(token, base string) error {
		claimedBase = base
		return tunnelAuth(st)(token, base)
	}
	sess, err := tunnel.Serve(conn, auth)
	if err != nil {
		log.Printf("tunnel handshake rejected for %q from %s: %v", claimedBase, conn.RemoteAddr(), err)
		conn.Close()
		return
	}
	router.Register(sess)
	if delivery != nil {
		delivery.Dispatch(func(ctx context.Context) { delivery.DrainFor(ctx, sess.BaseDomain) })
	}
	// Re-derive every live custom domain (active + unexpired pending);
	// expired pending squats are filtered by the store, so they also die
	// here even if never contested by a rival claim (#227).
	if domains, err := st.CustomDomains(sess.BaseDomain); err == nil {
		for _, d := range domains {
			router.RegisterCustom(d, sess)
		}
	}
	// Post-register re-check closes the handshake race deterministically: auth
	// may have passed before DisableAccount committed, landing Register after
	// the flag flip. Evict on an affirmative kill read — disabled=true, or
	// ErrUnknownAccount (the agent row is gone). A transient store error leaves
	// the fresh session up; the watchdog re-checks next tick.
	if off, err := disabled(sess.BaseDomain); (err == nil && off) || errors.Is(err, ErrUnknownAccount) {
		router.Unregister(sess)
		sess.Close()
		return
	}
	log.Printf("agent registered: %s", sess.BaseDomain)
	go serveControl(sess, st, router, ghApp)

	ticker := time.NewTicker(disabledPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-sess.CloseChan():
			router.Unregister(sess)
			log.Printf("agent gone: %s", sess.BaseDomain)
			return
		case <-ticker.C:
			// Transient vs. permanent split. The read has three outcomes:
			//   - a bare store error is TRANSIENT — a DB blip must not kill a
			//     healthy session, so log and retry next tick;
			//   - disabled=true (operator kill-switch) and ErrUnknownAccount
			//     (the agent's row is gone — e.g. a future account-deletion
			//     path) are PERMANENT kill signals. Both are affirmative reads
			//     that the account can no longer serve; a gone row is at least
			//     as strong a signal as disabled=true, so it evicts too rather
			//     than retrying forever.
			// Close() drives normal teardown; CloseChan then fires and the next
			// iteration unregisters, mirroring an agent-initiated close.
			off, err := disabled(sess.BaseDomain)
			if err != nil && !errors.Is(err, ErrUnknownAccount) {
				log.Printf("agent %s: disabled watchdog read failed: %v", sess.BaseDomain, err)
				continue
			}
			if off || errors.Is(err, ErrUnknownAccount) {
				log.Printf("agent gone or disabled, evicting: %s", sess.BaseDomain)
				sess.Close()
			}
		}
	}
}

// serveControl accepts the agent's control streams (KindControl) for the life of
// the session and dispatches each. Non-control streams are ignored (the agent
// never opens them). Returns when the session dies.
func serveControl(sess *tunnel.Session, st *Store, router *Router, ghApp *GitHubApp) {
	for {
		kind, stream, err := sess.AcceptKind()
		if err != nil {
			return
		}
		if kind != tunnel.KindControl {
			stream.Close()
			continue
		}
		go handleControl(stream, sess, st, router, ghApp)
	}
}

// handleControl serves one control request: register or deregister a hostname
// for this session's account.
func handleControl(stream net.Conn, sess *tunnel.Session, st *Store, router *Router, ghApp *GitHubApp) {
	defer stream.Close()
	var req tunnel.ControlRequest
	if err := tunnel.ReadMsg(stream, &req); err != nil {
		return
	}
	switch req.Op {
	case "register":
		host, err := st.RegisterHostname(sess.BaseDomain, req.App, req.PR)
		if err != nil {
			_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{Error: err.Error()})
			return
		}
		router.RegisterHost(host, sess)
		_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{Hostname: host})
	case "deregister":
		_ = st.DeregisterHostname(sess.BaseDomain, req.Hostname)
		router.UnregisterHost(req.Hostname)
		_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{Hostname: req.Hostname})
	case "provision":
		// The box hands the relay its control-API bearer (agent-push Token B).
		// The op rides the authenticated session, so it can only ever set the
		// token for the session's own agent.
		if req.Token == "" {
			_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{Error: "provision: empty token"})
			return
		}
		if err := st.SetControlToken(sess.BaseDomain, req.Token); err != nil {
			_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{Error: err.Error()})
			return
		}
		_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{})
	case "add-domain":
		// Per-app custom domain claim (#227): pending, routable immediately —
		// that is what lets the TLS-ALPN-01 challenge reach the box before any
		// cert exists. RegisterCustom overwrites any evicted squatter's mapping
		// (the router is keyed by domain), so its routing dies with the claim.
		if err := st.AddCustomDomain(sess.BaseDomain, req.Domain); err != nil {
			_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{Error: err.Error()})
			return
		}
		router.RegisterCustom(req.Domain, sess)
		_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{})
	case "domain-active":
		// The box reports it holds the issued cert; the claim becomes
		// permanent. Routing is already live, so the router is untouched.
		if err := st.ConfirmCustomDomain(sess.BaseDomain, req.Domain); err != nil {
			_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{Error: err.Error()})
			return
		}
		_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{})
	case "remove-domain":
		// Idempotent at the store layer: a caller removing a domain it
		// never held is a no-op there. But the router must not be touched
		// in that case either — otherwise any authenticated agent could
		// unroute another tenant's live domain by naming it (cross-tenant
		// DoS). Only unroute when this session actually held the row that
		// got deleted.
		held, err := st.removeCustomDomainOwned(sess.BaseDomain, req.Domain)
		if err != nil {
			_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{Error: err.Error()})
			return
		}
		if held {
			router.UnregisterCustom(req.Domain)
		}
		_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{})
	case "bind-repo":
		if req.App == "" || req.Repo == "" || req.Branch == "" {
			_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{Error: "bind-repo: app, repo and branch required"})
			return
		}
		if err := st.BindRepo(sess.BaseDomain, req.App, req.Repo, req.Branch); err != nil {
			_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{Error: err.Error()})
			return
		}
		_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{})
	case "unbind-repo":
		if req.App == "" {
			_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{Error: "unbind-repo: app required"})
			return
		}
		if err := st.UnbindRepo(sess.BaseDomain, req.App); err != nil {
			_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{Error: err.Error()})
			return
		}
		_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{})
	case "gh-token":
		if req.Repo == "" {
			_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{Error: "gh-token: repo required"})
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		tok, exp, err := st.GitHubTokenFor(ctx, ghApp, sess.BaseDomain, req.Repo)
		cancel()
		if err != nil {
			// The detail stays server-side: a box must not learn whether a repo
			// exists, only that it is not authorized for it here.
			log.Printf("relay: gh-token for %s repo %s: %v", sess.BaseDomain, req.Repo, err)
			_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{Error: "github token unavailable"})
			return
		}
		_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{
			Token: tok, Expires: exp.UTC().Format(time.RFC3339),
		})
	default:
		_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{Error: "unknown op"})
	}
}

func handlePublic(conn net.Conn, router *Router, tlsCfg *tls.Config, ctrlHost string, ctrlQ *connQueue, m *Metrics) {
	sni, buffered, err := readSNI(conn)
	if err != nil {
		conn.Close()
		return
	}
	// Control plane: api.<apex> is the relay's own TLS-terminated HTTP API,
	// checked before app routing so no app registration can ever shadow it.
	if ctrlQ != nil && sni == ctrlHost {
		ctrlQ.push(tls.Server(&prefixConn{Conn: conn, prefix: buffered}, tlsCfg))
		return
	}
	defer conn.Close()
	if sess, ok := router.LookupHost(sni); ok {
		m.ConnRouted("tls")
		if tlsCfg == nil {
			return // terminated hostname but no wildcard cert configured
		}
		terminate(conn, buffered, sess, tlsCfg, m)
		return
	}
	if sess, ok := router.Lookup(sni); ok {
		m.ConnRouted("tls")
		pump(conn, buffered, sess, tunnel.KindPassthrough, m)
		return
	}
	m.ConnUnrouted("tls")
}

// pump is the shared byte-splice: open a stream of the given kind on sess,
// replay the bytes already consumed from conn, then pipe raw bytes both ways.
// The :443 path uses it as the Plan-2 SNI passthrough (KindPassthrough, the
// box terminates TLS); the :80 path pumps custom-domain plaintext HTTP down a
// KindHTTP stream to the box's :80 (#228).
func pump(conn net.Conn, buffered []byte, sess *tunnel.Session, kind byte, m *Metrics) {
	stream, err := sess.OpenKind(kind)
	if err != nil {
		return
	}
	m.StreamStart()
	defer m.StreamEnd()
	defer stream.Close()
	if _, err := stream.Write(buffered); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { io.Copy(stream, conn); done <- struct{}{} }()
	go func() { io.Copy(conn, stream); done <- struct{}{} }()
	<-done
}
