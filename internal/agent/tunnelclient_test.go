package agent

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/piperbox/piper/internal/tunnel"
)

// fakeRelay accepts one agent tunnel and exposes its session for the test to
// drive (open T/H streams, accept C streams).
func fakeRelay(t *testing.T) (addr string, sessCh chan *tunnel.Session) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	sessCh = make(chan *tunnel.Session, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		sess, err := tunnel.Serve(c, func(_, _ string) error { return nil })
		if err != nil {
			return
		}
		sessCh <- sess
	}()
	return ln.Addr().String(), sessCh
}

// The tunnel client forwards an accepted stream to the local dialer. We stand up
// a real relay-side listener + tunnel.Serve, run the client against it, open a
// passthrough stream from the server, and check bytes reach a fake "local Caddy".
func TestTunnelClientForwardsToLocal(t *testing.T) {
	// Fake local Caddy: echoes.
	local, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	go func() {
		c, err := local.Accept()
		if err != nil {
			return
		}
		io.Copy(c, c)
		c.Close()
	}()

	addr, sessCh := fakeRelay(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var c TunnelClient
	go c.Run(ctx, addr, "tok", "alice.example.com", func(byte, net.Conn) (net.Conn, error) {
		return net.Dial("tcp", local.Addr().String())
	})

	sess := <-sessCh
	stream, err := sess.OpenKind(tunnel.KindPassthrough)
	if err != nil {
		t.Fatalf("OpenKind: %v", err)
	}
	stream.SetDeadline(time.Now().Add(2 * time.Second))
	stream.Write([]byte("hello"))
	buf := make([]byte, 5)
	if _, err := io.ReadFull(stream, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "hello" {
		t.Fatalf("echo = %q", buf)
	}
}

func TestTunnelClientDialsByKind(t *testing.T) {
	// Two local listeners stand in for the box's :443 and :80.
	ln443, _ := net.Listen("tcp", "127.0.0.1:0")
	ln80, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln443.Close()
	defer ln80.Close()
	got := make(chan byte, 1)
	accept := func(ln net.Listener, mark byte) {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			got <- mark
			c.Close()
		}
	}
	go accept(ln443, 'T')
	go accept(ln80, 'H')

	addr, sessCh := fakeRelay(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var c TunnelClient
	go c.Run(ctx, addr, "tok", "base.example.com", func(kind byte, _ net.Conn) (net.Conn, error) {
		if kind == tunnel.KindHTTP {
			return net.Dial("tcp", ln80.Addr().String())
		}
		return net.Dial("tcp", ln443.Addr().String())
	})
	relaySess := <-sessCh

	// Relay opens an H stream → agent must dial :80.
	hs, _ := relaySess.OpenKind(tunnel.KindHTTP)
	hs.Close()
	if mark := <-got; mark != 'H' {
		t.Fatalf("H stream dialed %q, want :80", mark)
	}
	// Relay opens a T stream → agent must dial :443.
	ts, _ := relaySess.OpenKind(tunnel.KindPassthrough)
	ts.Close()
	if mark := <-got; mark != 'T' {
		t.Fatalf("T stream dialed %q, want :443", mark)
	}
}

func TestTunnelClientRegister(t *testing.T) {
	addr, sessCh := fakeRelay(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var c TunnelClient
	go c.Run(ctx, addr, "tok", "base.example.com", func(byte, net.Conn) (net.Conn, error) {
		return net.Dial("tcp", "127.0.0.1:9") // unused in this test
	})
	relaySess := <-sessCh

	// Relay control handler: answer register with a canned hostname.
	go func() {
		kind, stream, err := relaySess.AcceptKind()
		if err != nil || kind != tunnel.KindControl {
			return
		}
		var req tunnel.ControlRequest
		_ = tunnel.ReadMsg(stream, &req)
		_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{Hostname: req.App + "-alice.public.getpiper.co"})
		stream.Close()
	}()

	// Give Run a moment to publish its session.
	var host string
	var err error
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		host, err = c.Register("blog", 0)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || host != "blog-alice.public.getpiper.co" {
		t.Fatalf("Register = %q,%v", host, err)
	}
}

// TestTunnelClientControlTimesOut pins #386: a relay that accepts the control
// stream and then never answers must not hang the caller. The read had no
// deadline, so the only thing that ever unblocked it was the session dying —
// and yamux only notices a dead peer on its 30s keepalive, or later. That put
// every registrar-backed path (Stop, Start, Delete, ResumeRoutes, every deploy)
// one wedged relay away from hanging indefinitely.
func TestTunnelClientControlTimesOut(t *testing.T) {
	old := controlTimeout
	controlTimeout = 100 * time.Millisecond
	t.Cleanup(func() { controlTimeout = old })

	addr, sessCh := fakeRelay(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var c TunnelClient
	go c.Run(ctx, addr, "tok", "base.example.com", func(byte, net.Conn) (net.Conn, error) {
		return net.Dial("tcp", "127.0.0.1:9") // unused in this test
	})
	relaySess := <-sessCh

	// A relay that reads the request and then goes quiet — connected, but wedged.
	go func() {
		_, stream, err := relaySess.AcceptKind()
		if err != nil {
			return
		}
		var req tunnel.ControlRequest
		_ = tunnel.ReadMsg(stream, &req)
		<-ctx.Done()
		stream.Close()
	}()

	// Wait for Run to publish its session, then time the wedged call.
	deadline := time.Now().Add(2 * time.Second)
	var err error
	for time.Now().Before(deadline) {
		started := time.Now()
		_, err = c.Register("blog", 0)
		if errors.Is(err, ErrNotConnected) {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if err == nil {
			t.Fatal("Register succeeded against a relay that never answered")
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("Register took %v to give up, want ~%v", elapsed, controlTimeout)
		}
		return
	}
	t.Fatalf("Register never returned a timeout error: %v", err)
}

// TestTunnelClientRunExitsPromptlyOnCancel pins the property piperd's shutdown
// join relies on (#242): once ctx is cancelled, Run returns quickly whether it
// is serving a live session or sitting in reconnect backoff — so joining its
// goroutine before closing the ALPN solver cannot hang the daemon.
func TestTunnelClientRunExitsPromptlyOnCancel(t *testing.T) {
	dialLocal := func(byte, net.Conn) (net.Conn, error) {
		return nil, errors.New("no local dials expected")
	}
	t.Run("mid-session", func(t *testing.T) {
		addr, sessCh := fakeRelay(t)
		ctx, cancel := context.WithCancel(context.Background())
		var c TunnelClient
		done := make(chan struct{})
		go func() {
			defer close(done)
			c.Run(ctx, addr, "tok", "base.example.com", dialLocal)
		}()
		<-sessCh // session is up; Run is inside serveStreams
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Run did not return after cancel while serving a session")
		}
	})
	t.Run("mid-backoff", func(t *testing.T) {
		// Nothing listens on :1, so every dial fails fast and Run spends its
		// time in the backoff sleep.
		ctx, cancel := context.WithCancel(context.Background())
		var c TunnelClient
		done := make(chan struct{})
		go func() {
			defer close(done)
			c.Run(ctx, "127.0.0.1:1", "tok", "base.example.com", dialLocal)
		}()
		time.Sleep(100 * time.Millisecond) // fail once, settle into backoff
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Run did not return after cancel during reconnect backoff")
		}
	})
}

func TestServeStreamsStopsOnContextCancellation(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { clientConn.Close(); serverConn.Close() })

	serverResult := make(chan *tunnel.Session, 1)
	go func() {
		sess, _ := tunnel.Serve(serverConn, func(_, _ string) error { return nil })
		serverResult <- sess
	}()
	clientSession, err := tunnel.Dial(clientConn, "token", "example.com")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	serverSession := <-serverResult
	t.Cleanup(func() { serverSession.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		serveStreams(ctx, clientSession, func(byte, net.Conn) (net.Conn, error) {
			return nil, errors.New("unexpected local dial")
		})
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("serveStreams did not stop after context cancellation")
	}
}

// If the relay session dies immediately after tunnel.Dial succeeds (e.g. the
// relay rejects the token and drops the connection before any yamux traffic),
// the reconnect loop must still back off instead of hammering net.Dial in a
// tight spin. We simulate that by accepting and instantly closing every
// connection, then counting how many connection attempts land within a short
// window.
func TestTunnelClientBacksOffOnImmediateSessionDeath(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var accepted int64
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			atomic.AddInt64(&accepted, 1)
			conn.Close()
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var c TunnelClient
	go c.Run(ctx, ln.Addr().String(), "tok", "alice.example.com", func(byte, net.Conn) (net.Conn, error) {
		return nil, io.EOF // never actually reached; session dies before Accept
	})

	time.Sleep(500 * time.Millisecond)
	cancel()

	if n := atomic.LoadInt64(&accepted); n >= 5 {
		t.Fatalf("accepted %d connections in 500ms; reconnect loop is busy-spinning (want < 5)", n)
	}
}

func TestTunnelClientProvision(t *testing.T) {
	addr, sessCh := fakeRelay(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var c TunnelClient
	go c.Run(ctx, addr, "tok", "base.example.com", func(byte, net.Conn) (net.Conn, error) {
		return nil, errors.New("no local dials expected")
	})
	relaySess := <-sessCh

	got := make(chan tunnel.ControlRequest, 1)
	go func() {
		kind, stream, err := relaySess.AcceptKind()
		if err != nil || kind != tunnel.KindControl {
			return
		}
		var req tunnel.ControlRequest
		_ = tunnel.ReadMsg(stream, &req)
		got <- req
		_ = tunnel.WriteMsg(stream, tunnel.ControlResponse{})
		stream.Close()
	}()

	// Retry until Run publishes its session.
	var err error
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err = c.Provision("box-token"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	req := <-got
	if req.Op != "provision" || req.Token != "box-token" {
		t.Fatalf("relay saw %+v, want op=provision token=box-token", req)
	}
}

func TestTunnelClientOnConnectFires(t *testing.T) {
	addr, sessCh := fakeRelay(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fired := make(chan struct{}, 1)
	var c TunnelClient
	c.OnConnect = func() { fired <- struct{}{} }
	go c.Run(ctx, addr, "tok", "base.example.com", func(byte, net.Conn) (net.Conn, error) {
		return nil, errors.New("no local dials expected")
	})
	<-sessCh
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("OnConnect did not fire after session establishment")
	}
}

// The per-app domain ops (#227) are thin control-stream wrappers; assert each
// sends the right op + domain and surfaces relay errors.
func TestTunnelClientDomainOps(t *testing.T) {
	addr, sessCh := fakeRelay(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var c TunnelClient
	go c.Run(ctx, addr, "tok", "base.example.com", func(byte, net.Conn) (net.Conn, error) {
		return nil, errors.New("no local dials expected")
	})
	relaySess := <-sessCh

	got := make(chan tunnel.ControlRequest, 3)
	go func() {
		for {
			kind, stream, err := relaySess.AcceptKind()
			if err != nil {
				return
			}
			if kind != tunnel.KindControl {
				stream.Close()
				continue
			}
			var req tunnel.ControlRequest
			_ = tunnel.ReadMsg(stream, &req)
			got <- req
			resp := tunnel.ControlResponse{}
			if req.Domain == "taken.example.com" {
				resp.Error = "domain already in use"
			}
			_ = tunnel.WriteMsg(stream, resp)
			stream.Close()
		}
	}()

	calls := []struct {
		name   string
		call   func(string) error
		wantOp string
	}{
		{"AddCustomDomain", c.AddCustomDomain, "add-domain"},
		{"ConfirmCustomDomain", c.ConfirmCustomDomain, "domain-active"},
		{"RemoveCustomDomain", c.RemoveCustomDomain, "remove-domain"},
	}
	for _, tc := range calls {
		var err error
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if err = tc.call("shop.example.com"); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		req := <-got
		if req.Op != tc.wantOp || req.Domain != "shop.example.com" {
			t.Fatalf("%s sent %+v, want op=%s domain=shop.example.com", tc.name, req, tc.wantOp)
		}
	}

	err := c.AddCustomDomain("taken.example.com")
	if err == nil || !strings.Contains(err.Error(), "domain already in use") {
		t.Fatalf("AddCustomDomain(taken.example.com) = %v, want error containing %q", err, "domain already in use")
	}
	<-got
}

func TestTunnelClientRepoOps(t *testing.T) {
	addr, sessCh := fakeRelay(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var c TunnelClient
	go c.Run(ctx, addr, "tok", "base.example.com", func(byte, net.Conn) (net.Conn, error) {
		return nil, errors.New("no local dials expected")
	})
	relaySess := <-sessCh

	got := make(chan tunnel.ControlRequest, 3)
	go func() {
		for {
			kind, stream, err := relaySess.AcceptKind()
			if err != nil {
				return
			}
			if kind != tunnel.KindControl {
				stream.Close()
				continue
			}
			var req tunnel.ControlRequest
			_ = tunnel.ReadMsg(stream, &req)
			got <- req
			resp := tunnel.ControlResponse{}
			if req.Op == "gh-token" {
				resp.Token = "ghs_x"
			}
			_ = tunnel.WriteMsg(stream, resp)
			stream.Close()
		}
	}()

	// Retry until the session is up, exactly as TestTunnelClientDomainOps does.
	var err error
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err = c.BindRepo("blog", "alice/blog", "main"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("BindRepo: %v", err)
	}
	if req := <-got; req.Op != "bind-repo" || req.App != "blog" || req.Repo != "alice/blog" || req.Branch != "main" {
		t.Fatalf("BindRepo sent %+v", req)
	}

	tok, err := c.GitHubToken("alice/blog")
	if err != nil {
		t.Fatalf("GitHubToken: %v", err)
	}
	if tok != "ghs_x" {
		t.Fatalf("token = %q, want ghs_x", tok)
	}
	if req := <-got; req.Op != "gh-token" || req.Repo != "alice/blog" {
		t.Fatalf("GitHubToken sent %+v", req)
	}

	if err := c.UnbindRepo("blog"); err != nil {
		t.Fatalf("UnbindRepo: %v", err)
	}
	if req := <-got; req.Op != "unbind-repo" || req.App != "blog" {
		t.Fatalf("UnbindRepo sent %+v", req)
	}
}
