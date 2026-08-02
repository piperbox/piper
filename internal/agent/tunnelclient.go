// Package agent holds piperd's relay-mode runtime helpers (the outbound tunnel
// client). It depends only on internal/tunnel and the standard library.
package agent

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/piperbox/piper/internal/tunnel"
)

// ErrNotConnected is returned by Register/Deregister when no relay session is live.
var ErrNotConnected = errors.New("relay tunnel not connected")

// TunnelClient maintains an outbound tunnel to the relay and exposes hostname
// registration over it. The current session is published under a mutex so the
// deploy path can open control streams on whatever session is live.
type TunnelClient struct {
	mu      sync.Mutex
	sess    *tunnel.Session
	running bool
	lastErr string

	// OnConnect, if set before Run, is invoked in its own goroutine each time a
	// relay session is established — piperd uses it to provision the relay's
	// control bearer (see the control-stream routing design).
	OnConnect func()
}

// Status reports the tunnel's state for the enrollment socket's status
// surface: "connected" with a live session, "retrying" while Run is looping
// without one (lastErr is the most recent dial/handshake failure — the only
// place a rejected enrollment token becomes visible outside the log), "off"
// when Run is not running.
func (c *TunnelClient) Status() (state, lastErr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.sess != nil:
		return "connected", ""
	case c.running:
		return "retrying", c.lastErr
	default:
		return "off", c.lastErr
	}
}

func (c *TunnelClient) setErr(err error) {
	c.mu.Lock()
	c.lastErr = err.Error()
	c.mu.Unlock()
}

func (c *TunnelClient) setSession(s *tunnel.Session) {
	c.mu.Lock()
	c.sess = s
	if s != nil {
		c.lastErr = ""
	}
	c.mu.Unlock()
}

func (c *TunnelClient) current() *tunnel.Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sess
}

// relayDialTimeout bounds one relay connect. piperd joins Run at shutdown
// (#242), so every blocking point in the loop must answer to ctx — and a
// blackholed relay (SYNs swallowed, no RST) would otherwise hold a plain
// net.Dial until the kernel's own connect timeout, minutes past the daemon's
// shutdown budget. DialContext also makes an in-flight dial abort the moment
// the shutdown signal lands.
const relayDialTimeout = 10 * time.Second

// Run maintains the tunnel to relayAddr, registering baseDomain, and forwards
// each relay-opened stream to dialLocal(kind, stream). dialLocal may peek
// (read) bytes from stream before choosing a backend; it must replay whatever
// it consumed into the returned conn. It reconnects with backoff until ctx is
// cancelled. Blocks.
func (c *TunnelClient) Run(ctx context.Context, relayAddr, token, baseDomain string, dialLocal func(kind byte, stream net.Conn) (net.Conn, error)) {
	c.mu.Lock()
	c.running = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.running = false
		c.mu.Unlock()
	}()
	backoff := time.Second
	for ctx.Err() == nil {
		conn, err := (&net.Dialer{Timeout: relayDialTimeout}).DialContext(ctx, "tcp", relayAddr)
		if err != nil {
			if ctx.Err() != nil {
				return // shutdown interrupted the dial; not a relay problem
			}
			c.setErr(err)
			log.Printf("tunnel: dial relay: %v (retry in %s)", err, backoff)
			sleep(ctx, backoff)
			backoff = nextBackoff(backoff)
			continue
		}
		sess, err := tunnel.Dial(conn, token, baseDomain)
		if err != nil {
			c.setErr(err)
			log.Printf("tunnel: handshake: %v (retry in %s)", err, backoff)
			conn.Close()
			sleep(ctx, backoff)
			backoff = nextBackoff(backoff)
			continue
		}
		log.Printf("tunnel: connected to relay %s as %s", relayAddr, baseDomain)
		c.setSession(sess)
		if c.OnConnect != nil {
			go c.OnConnect()
		}
		start := time.Now()
		serveStreams(ctx, sess, dialLocal)
		c.setSession(nil)
		if time.Since(start) > healthyThreshold {
			backoff = time.Second
		}
		sleep(ctx, backoff)
		backoff = nextBackoff(backoff)
	}
}

// Register opens a control stream on the current session and asks the relay to
// assign/return the public hostname for app. pr is 0 for the app's production
// host and the PR number for a preview, which gets a hostname of its own so it
// never overwrites production's (#302).
func (c *TunnelClient) Register(app string, pr int) (string, error) {
	resp, err := c.control(tunnel.ControlRequest{Op: "register", App: app, PR: pr})
	if err != nil {
		return "", err
	}
	return resp.Hostname, nil
}

// Deregister asks the relay to drop hostname.
func (c *TunnelClient) Deregister(hostname string) error {
	_, err := c.control(tunnel.ControlRequest{Op: "deregister", Hostname: hostname})
	return err
}

// SyncApps announces every slot this box holds, so the relay can restore their
// routes and drop rows for the ones it no longer has (#418). Sent once per
// connect; an empty set is meaningful and prunes everything.
func (c *TunnelClient) SyncApps(apps []tunnel.AppRef) ([]tunnel.AppHost, error) {
	resp, err := c.control(tunnel.ControlRequest{Op: "sync-apps", Apps: apps})
	if err != nil {
		return nil, err
	}
	return resp.Apps, nil
}

// Provision hands the relay this box's control-API bearer for the enrollment.
// It rides the authenticated session, so it can only set this agent's token.
func (c *TunnelClient) Provision(token string) error {
	_, err := c.control(tunnel.ControlRequest{Op: "provision", Token: token})
	return err
}

// AddCustomDomain claims domain on the relay as a pending custom domain
// (#227): routable immediately so the TLS-ALPN-01 challenge can reach this
// box, evictable if not confirmed within the relay's pending TTL. It rides
// the authenticated session, so it can only ever claim for this agent.
func (c *TunnelClient) AddCustomDomain(domain string) error {
	_, err := c.control(tunnel.ControlRequest{Op: "add-domain", Domain: domain})
	return err
}

// RemoveCustomDomain drops this agent's claim on domain and its routing.
func (c *TunnelClient) RemoveCustomDomain(domain string) error {
	_, err := c.control(tunnel.ControlRequest{Op: "remove-domain", Domain: domain})
	return err
}

// ConfirmCustomDomain reports that this box holds an issued cert for domain,
// flipping the relay's pending claim to active (permanent, reconnect-safe).
func (c *TunnelClient) ConfirmCustomDomain(domain string) error {
	_, err := c.control(tunnel.ControlRequest{Op: "domain-active", Domain: domain})
	return err
}

// BindRepo tells the relay that app deploys from repo@branch on this box, so
// the relay can route that repository's webhooks here.
func (c *TunnelClient) BindRepo(app, repo, branch string) error {
	_, err := c.control(tunnel.ControlRequest{Op: "bind-repo", App: app, Repo: repo, Branch: branch})
	return err
}

// UnbindRepo drops an app's repo binding on the relay.
func (c *TunnelClient) UnbindRepo(app string) error {
	_, err := c.control(tunnel.ControlRequest{Op: "unbind-repo", App: app})
	return err
}

// GitHubToken asks the relay for an installation token scoped to repo. Brokered
// boxes hold no GitHub App key, so this is their only way to reach the repo.
func (c *TunnelClient) GitHubToken(repo string) (string, error) {
	resp, err := c.control(tunnel.ControlRequest{Op: "gh-token", Repo: repo})
	if err != nil {
		return "", err
	}
	return resp.Token, nil
}

// controlTimeout bounds one control round-trip. Without it the reply read was
// unbounded, so a relay that accepted the stream and then went quiet hung the
// caller until the session itself died — which yamux only detects on its 30s
// keepalive, or later still if the peer vanished without a clean close. That
// left every registrar-backed path (Stop, Start, Delete, ResumeRoutes, every
// deploy, since primaryHost registers) hanging on one wedged relay (#386).
//
// Generous because the slowest legitimate op is gh-token, where the relay
// mints an installation token against GitHub behind its own 30s HTTP timeout.
// Anything past that is the relay failing to answer, not working slowly.
// A var so tests can shrink it.
var controlTimeout = 45 * time.Second

func (c *TunnelClient) control(req tunnel.ControlRequest) (tunnel.ControlResponse, error) {
	sess := c.current()
	if sess == nil {
		return tunnel.ControlResponse{}, ErrNotConnected
	}
	stream, err := sess.OpenKind(tunnel.KindControl)
	if err != nil {
		return tunnel.ControlResponse{}, err
	}
	defer stream.Close()
	// Covers the write too: a stalled relay can also stop reading.
	if err := stream.SetDeadline(time.Now().Add(controlTimeout)); err != nil {
		return tunnel.ControlResponse{}, err
	}
	if err := tunnel.WriteMsg(stream, req); err != nil {
		return tunnel.ControlResponse{}, err
	}
	var resp tunnel.ControlResponse
	if err := tunnel.ReadMsg(stream, &resp); err != nil {
		return tunnel.ControlResponse{}, err
	}
	if resp.Error != "" {
		return tunnel.ControlResponse{}, errors.New(resp.Error)
	}
	return resp, nil
}

// healthyThreshold is how long a session must stay up before a subsequent
// disconnect is treated as "was fine" and resets backoff to the floor. A
// session that dies before this (e.g. relay rejects auth immediately) keeps
// the backoff growing so a misconfigured token doesn't busy-spin reconnects.
const healthyThreshold = 10 * time.Second

// handlerJoinTimeout bounds how long serveStreams waits for its in-flight
// stream handlers after the session dies. A handler still in its local-dial
// phase (PeekALPN, net.Dial) must finish before Run returns — piperd closes
// the ALPN solver right after joining Run, and a handler dialing a just-closed
// solver gets connection refused, the exact shutdown race #242 asks to remove.
// Bounded so a wedged handler can't pin the join past the daemon's shutdown
// budget (piperd's overall shutdown timeout is 20s; this sits well inside it).
// Atomic (nanoseconds) so a test can shrink it while other tests' tunnel
// goroutines are still draining — every serveStreams exit reads it.
var handlerJoinTimeout atomic.Int64

func init() { handlerJoinTimeout.Store(int64(5 * time.Second)) }

func serveStreams(ctx context.Context, sess *tunnel.Session, dialLocal func(kind byte, stream net.Conn) (net.Conn, error)) {
	defer sess.Close()
	stopCancel := context.AfterFunc(ctx, func() { _ = sess.Close() })
	defer stopCancel()
	var handlers sync.WaitGroup
	for {
		kind, stream, err := sess.AcceptKind()
		if err != nil {
			break // session died; join handlers, then caller reconnects
		}
		handlers.Add(1)
		go func() {
			defer handlers.Done()
			defer stream.Close()
			local, err := dialLocal(kind, stream)
			if err != nil {
				log.Printf("tunnel: dial local (kind %q): %v", kind, err)
				return
			}
			defer local.Close()
			done := make(chan struct{}, 2)
			go func() { io.Copy(local, stream); done <- struct{}{} }()
			go func() { io.Copy(stream, local); done <- struct{}{} }()
			<-done
		}()
	}
	// The accept loop is over, so the session is already dead or dying; make
	// sure of it so handlers blocked on stream I/O unblock, then join them.
	// The join is bounded: a handler whose local dial outlives the grace is
	// abandoned rather than allowed to stall Run past the shutdown budget.
	_ = sess.Close()
	joinTimeout := time.Duration(handlerJoinTimeout.Load())
	joined := make(chan struct{})
	go func() { handlers.Wait(); close(joined) }()
	select {
	case <-joined:
	case <-time.After(joinTimeout):
		log.Printf("tunnel: stream handlers still running after %s; continuing without them", joinTimeout)
	}
}

func nextBackoff(d time.Duration) time.Duration {
	if d >= 30*time.Second {
		return 30 * time.Second
	}
	return d * 2
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
