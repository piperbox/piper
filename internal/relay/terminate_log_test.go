package relay

import (
	"bytes"
	"crypto/tls"
	"io"
	"log"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/piperbox/piper/internal/tunnel"
)

// captureStdLog redirects the standard logger into a buffer until the test
// ends and returns the buffer.
func captureStdLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

// resetTerminateHandshakeGate clears the rate-limit state so each test starts
// with the next failure guaranteed loggable.
func resetTerminateHandshakeGate(t *testing.T) {
	t.Helper()
	terminateHandshakeGate.mu.Lock()
	terminateHandshakeGate.last = time.Time{}
	terminateHandshakeGate.mu.Unlock()
}

// windTerminateHandshakeGate ages the gate's last-log stamp by d, standing in
// for the passage of time without making the test wait out the interval.
func windTerminateHandshakeGate(d time.Duration) {
	terminateHandshakeGate.mu.Lock()
	terminateHandshakeGate.last = terminateHandshakeGate.last.Add(d)
	terminateHandshakeGate.mu.Unlock()
}

// failOneHandshake runs terminate against a client that sends junk instead of
// a ClientHello and returns the peer address terminate saw. It returns only
// after terminate has returned, so any log line the failure produced is
// already written.
func failOneHandshake(t *testing.T, ln net.Listener, tlsCfg *tls.Config) string {
	t.Helper()
	cli, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	srv, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	go func() {
		// Junk from the first byte: the record header can't parse as TLS, so
		// the handshake fails deterministically ("tls: first record does not
		// look like a TLS handshake") before the client close matters.
		_, _ = cli.Write([]byte("not a TLS ClientHello"))
		cli.Close()
	}()
	terminate(srv, nil, nil, tlsCfg, nil)
	return srv.RemoteAddr().String()
}

// A failed terminate-path handshake is logged once with the peer address and
// the error — the shape the ALPN solver established in #242 — rate-limited
// because this path fronts the public listener (#496), and the limiter
// re-arms on time so a recurring failure is reported again (#497).
func TestTerminateHandshakeFailureLoggedOnce(t *testing.T) {
	cert, key := writeWildcard(t, "public.getpiper.co")
	tlsCfg, err := LoadWildcardConfig(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	resetTerminateHandshakeGate(t)
	buf := captureStdLog(t)

	peer := failOneHandshake(t, ln, tlsCfg)
	out := buf.String()
	if !strings.Contains(out, "handshake from "+peer) {
		t.Fatalf("log %q missing the failed handshake from %s", out, peer)
	}
	if !strings.Contains(out, "tls:") {
		t.Fatalf("log %q missing the handshake error", out)
	}

	// An immediate repeat from another connection is suppressed: one bad peer
	// (or a scanner) must not produce a line per connection.
	failOneHandshake(t, ln, tlsCfg)
	if n := strings.Count(buf.String(), "handshake from"); n != 1 {
		t.Fatalf("got %d handshake log lines, want 1 (rate-limited)", n)
	}

	// The gate re-arms on time alone: a failure still happening an interval
	// later is logged again, so a recurring outage is never hidden by its
	// first report.
	windTerminateHandshakeGate(-2 * terminateHandshakeLogInterval)
	peer3 := failOneHandshake(t, ln, tlsCfg)
	out = buf.String()
	if n := strings.Count(out, "handshake from"); n != 2 {
		t.Fatalf("got %d handshake log lines after the interval passed, want 2", n)
	}
	if !strings.Contains(out, "handshake from "+peer3) {
		t.Fatalf("log %q missing the re-armed failure from %s", out, peer3)
	}
}

// A completed handshake stays silent: the log is for failures, the happy path
// produces nothing (same contract as the ALPN solver).
func TestTerminateHandshakeSuccessSilent(t *testing.T) {
	cert, key := writeWildcard(t, "public.getpiper.co")
	tlsCfg, err := LoadWildcardConfig(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	resetTerminateHandshakeGate(t)
	buf := captureStdLog(t)

	// A real session pair: after the handshake terminate opens a KindHTTP
	// stream, and the agent side answers and drains it.
	p1, p2 := net.Pipe()
	served := make(chan *tunnel.Session, 1)
	go func() {
		if s, err := tunnel.Serve(p1, func(string, string) error { return nil }); err == nil {
			served <- s
		}
	}()
	agent, err := tunnel.Dial(p2, "token", "agent.getpiper.co")
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()
	sess := <-served
	defer sess.Close()
	go func() {
		for {
			kind, stream, err := agent.AcceptKind()
			if err != nil {
				return
			}
			if kind != tunnel.KindHTTP {
				stream.Close()
				continue
			}
			go func() { _, _ = io.Copy(io.Discard, stream); stream.Close() }()
		}
	}()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cli, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	srv, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	done := make(chan struct{})
	go func() { terminate(srv, nil, sess, tlsCfg, nil); close(done) }()

	client := tls.Client(cli, &tls.Config{InsecureSkipVerify: true, ServerName: "app.public.getpiper.co"})
	if err := client.Handshake(); err != nil {
		t.Fatal(err)
	}
	client.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("terminate did not return after the client hung up")
	}
	if out := buf.String(); strings.Contains(out, "handshake from") {
		t.Fatalf("completed handshake was logged: %q", out)
	}
}
