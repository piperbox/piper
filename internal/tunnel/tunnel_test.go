package tunnel

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

type deadlineRecordingConn struct {
	net.Conn
	mu           sync.Mutex
	readDeadline time.Time
}

func (c *deadlineRecordingConn) SetReadDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.readDeadline = deadline
	c.mu.Unlock()
	return c.Conn.SetReadDeadline(deadline)
}

func (c *deadlineRecordingConn) currentReadDeadline() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readDeadline
}

// handshake + a round-trip stream over an in-process pipe.
func TestDialServeRoundTrip(t *testing.T) {
	c, s := net.Pipe()
	t.Cleanup(func() { c.Close(); s.Close() })

	type res struct {
		sess *Session
		err  error
	}
	srvCh := make(chan res, 1)
	go func() {
		sess, err := Serve(s, func(token, base string) error {
			if token != "tok-123" || base != "alice.example.com" {
				t.Errorf("bad handshake: %q %q", token, base)
			}
			return nil
		})
		srvCh <- res{sess, err}
	}()

	cli, err := Dial(c, "tok-123", "alice.example.com")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	sr := <-srvCh
	if sr.err != nil {
		t.Fatalf("Serve: %v", sr.err)
	}
	if sr.sess.BaseDomain != "alice.example.com" {
		t.Fatalf("server BaseDomain = %q", sr.sess.BaseDomain)
	}

	// Server opens a stream; agent accepts and echoes.
	go func() {
		st, err := cli.Accept()
		if err != nil {
			return
		}
		io.Copy(st, st)
		st.Close()
	}()

	stream, err := sr.sess.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	stream.SetDeadline(time.Now().Add(2 * time.Second))
	stream.Write([]byte("ping"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(stream, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("echo = %q", buf)
	}
}

func TestServeRejectsBadAuth(t *testing.T) {
	c, s := net.Pipe()
	t.Cleanup(func() { c.Close(); s.Close() })
	go Dial(c, "wrong", "alice.example.com")
	_, err := Serve(s, func(token, base string) error {
		return io.EOF // any non-nil rejects
	})
	if err == nil {
		t.Fatal("expected Serve to reject bad auth")
	}
}

// Serve must bound its unauthenticated handshake read: a client that connects
// and sends nothing cannot pin a goroutine forever (slowloris / fd-exhaustion).
func TestServe_PreAuthDeadline(t *testing.T) {
	c, s := net.Pipe()
	t.Cleanup(func() { c.Close(); s.Close() })

	// Client never writes; the server-side read must time out, not block.
	prev := preAuthReadTimeout
	preAuthReadTimeout = 50 * time.Millisecond
	t.Cleanup(func() { preAuthReadTimeout = prev })

	start := time.Now()
	_, err := Serve(s, func(token, base string) error {
		t.Error("auth called on a silent client")
		return nil
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected Serve to time out on a silent client")
	}
	if elapsed > time.Second {
		t.Fatalf("Serve blocked %v (deadline not enforced)", elapsed)
	}
}

func TestServeClearsPreAuthDeadlineBeforeAuth(t *testing.T) {
	clientConn, rawServerConn := net.Pipe()
	serverConn := &deadlineRecordingConn{Conn: rawServerConn}
	t.Cleanup(func() { clientConn.Close(); serverConn.Close() })

	previous := preAuthReadTimeout
	preAuthReadTimeout = 20 * time.Millisecond
	t.Cleanup(func() { preAuthReadTimeout = previous })

	clientResult := make(chan *Session, 1)
	go func() {
		session, _ := Dial(clientConn, "token", "example.com")
		clientResult <- session
	}()

	errDeadlineActive := errors.New("pre-auth read deadline still active during auth")
	serverSession, err := Serve(serverConn, func(_, _ string) error {
		time.Sleep(2 * preAuthReadTimeout)
		if !serverConn.currentReadDeadline().IsZero() {
			return errDeadlineActive
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	t.Cleanup(func() { serverSession.Close() })

	clientSession := <-clientResult
	t.Cleanup(func() { clientSession.Close() })
}

func TestOpenAcceptKind(t *testing.T) {
	c1, c2 := net.Pipe()
	srvSess := make(chan *Session, 1)
	go func() {
		s, err := Serve(c2, func(_, _ string) error { return nil })
		if err != nil {
			t.Errorf("Serve: %v", err)
			return
		}
		srvSess <- s
	}()
	cli, err := Dial(c1, "tok", "base.example.com")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	srv := <-srvSess

	// Client opens a control stream; server accepts and sees the kind.
	go func() {
		stream, err := cli.OpenKind(KindControl)
		if err != nil {
			t.Errorf("OpenKind: %v", err)
			return
		}
		_ = WriteMsg(stream, ControlRequest{Op: "register", App: "blog"})
		var resp ControlResponse
		_ = ReadMsg(stream, &resp)
		if resp.Hostname != "blog-alice.public.getpiper.co" {
			t.Errorf("resp = %+v", resp)
		}
		stream.Close()
	}()

	kind, stream, err := srv.AcceptKind()
	if err != nil {
		t.Fatalf("AcceptKind: %v", err)
	}
	if kind != KindControl {
		t.Fatalf("kind = %q, want C", kind)
	}
	var req ControlRequest
	if err := ReadMsg(stream, &req); err != nil {
		t.Fatalf("ReadMsg: %v", err)
	}
	if req.Op != "register" || req.App != "blog" {
		t.Fatalf("req = %+v", req)
	}
	_ = WriteMsg(stream, ControlResponse{Hostname: "blog-alice.public.getpiper.co"})
}

// Dial must not report success on a handshake the relay rejected. Without an
// ack it returned a live-looking Session the instant the frame was written, so
// an agent whose enrollment the relay no longer knows logged "connected" every
// retry, forever, with the failure invisible on both ends (#400).
func TestDialFailsWhenServerRejectsAuth(t *testing.T) {
	c, s := net.Pipe()
	t.Cleanup(func() { c.Close(); s.Close() })

	go Serve(s, func(token, base string) error { return errors.New("unknown agent") })

	sess, err := Dial(c, "stale-token", "092942b4-alice.example.com")
	if err == nil {
		sess.Close()
		t.Fatal("Dial reported success on a rejected handshake")
	}
}

// The rejection must carry a reason the agent can log. "relay closed the
// connection" is what cost the debugging time; the agent needs to be told the
// enrollment is the problem.
func TestDialRejectionCarriesReason(t *testing.T) {
	c, s := net.Pipe()
	t.Cleanup(func() { c.Close(); s.Close() })

	go Serve(s, func(token, base string) error { return errors.New("unknown agent") })

	_, err := Dial(c, "stale-token", "092942b4-alice.example.com")
	if err == nil {
		t.Fatal("Dial reported success on a rejected handshake")
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("Dial error = %q, want it to say the relay rejected the handshake", err)
	}
}

// A relay that accepts the connection but never reads fills the kernel send
// buffer and the peer's receive window, after which the handshake write
// blocks with nothing to release it — pinning the reconnect loop exactly
// like an unbounded ack wait. net.Pipe has no buffer at all, so a peer that
// never reads makes the write block deterministically.
func TestDialBoundsTheHandshakeWrite(t *testing.T) {
	c, s := net.Pipe()
	t.Cleanup(func() { c.Close(); s.Close() })

	// Fake relay: holds its end but never reads a byte.

	prev := handshakeWriteTimeout
	handshakeWriteTimeout = 50 * time.Millisecond
	t.Cleanup(func() { handshakeWriteTimeout = prev })

	type dialRes struct {
		sess *Session
		err  error
	}
	resCh := make(chan dialRes, 1)
	start := time.Now()
	go func() {
		sess, err := Dial(c, "tok", "alice.example.com")
		resCh <- dialRes{sess, err}
	}()

	select {
	case res := <-resCh:
		if res.err == nil {
			res.sess.Close()
			t.Fatal("expected Dial to fail when the relay never reads the handshake")
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("Dial blocked %v on the handshake write (deadline not enforced)", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Dial hung on the handshake write (no write deadline)")
	}
}

// The handshake write deadline must be cleared once the frame is out. A stale
// deadline survives into the established session and the first yamux write
// after it expires (the keepalive at 30s) fails with an i/o timeout, killing
// an otherwise healthy session. The silent-relay test above can't see this:
// it never does I/O after Dial. Complete the handshake here, let the deadline
// window lapse, and then write on the very connection Dial returned.
func TestDialClearsTheHandshakeWriteDeadline(t *testing.T) {
	c, s := net.Pipe()
	t.Cleanup(func() { c.Close(); s.Close() })

	// Fake relay: read the handshake frame, ack it, then keep draining so
	// later writes don't block on the unbuffered pipe.
	go func() {
		if _, err := readFrame(s); err != nil {
			return
		}
		ackPayload, _ := json.Marshal(handshakeAck{})
		if err := writeFrame(s, ackPayload); err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, s)
	}()

	prev := handshakeWriteTimeout
	handshakeWriteTimeout = 50 * time.Millisecond
	t.Cleanup(func() { handshakeWriteTimeout = prev })

	sess, err := Dial(c, "tok", "alice.example.com")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { sess.Close() })

	// Let the handshake write deadline lapse, then write on the connection
	// Dial returned: a deadline that was never cleared fails this write.
	time.Sleep(2 * handshakeWriteTimeout)
	if _, err := c.Write([]byte("x")); err != nil {
		t.Fatalf("write on the established connection failed (handshake write deadline not cleared): %v", err)
	}
}

// A relay that accepts the connection but never acks must not pin the agent
// forever: the reconnect loop can only retry if Dial returns.
func TestDialBoundsTheAckWait(t *testing.T) {
	c, s := net.Pipe()
	t.Cleanup(func() { c.Close(); s.Close() })

	// Server reads the handshake frame and then goes silent — never acks.
	go func() {
		_, _ = readFrame(s)
		select {}
	}()

	prev := ackReadTimeout
	ackReadTimeout = 50 * time.Millisecond
	t.Cleanup(func() { ackReadTimeout = prev })

	start := time.Now()
	_, err := Dial(c, "tok", "alice.example.com")
	if err == nil {
		t.Fatal("expected Dial to time out waiting for the ack")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Dial blocked %v waiting for an ack (deadline not enforced)", elapsed)
	}
}
