package relay

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/piperbox/piper/internal/tunnel"
)

// proxyV2HeaderTCP4 hand-builds a PROXY protocol v2 header (12-byte signature,
// version/command, TCP4 address block) claiming src→dst. Built by hand so the
// tests do not rely on the parsing library to generate the bytes it then
// parses — a shared bug would otherwise cancel out.
func proxyV2HeaderTCP4(src string, srcPort uint16, dst string, dstPort uint16) []byte {
	h := []byte{
		0x0D, 0x0A, 0x0D, 0x0A, 0x00, 0x0D, 0x0A, 0x51, 0x55, 0x49, 0x54, 0x0A, // v2 signature
		0x21,       // version 2, command PROXY
		0x11,       // AF_INET over STREAM
		0x00, 0x0C, // 12-byte TCP4 address block
	}
	h = append(h, net.ParseIP(src).To4()...)
	h = append(h, net.ParseIP(dst).To4()...)
	h = binary.BigEndian.AppendUint16(h, srcPort)
	return binary.BigEndian.AppendUint16(h, dstPort)
}

type acceptResult struct {
	conn net.Conn
	err  error
}

func acceptOne(ln net.Listener) <-chan acceptResult {
	ch := make(chan acceptResult, 1)
	go func() {
		c, err := ln.Accept()
		ch <- acceptResult{c, err}
	}()
	return ch
}

func awaitAccept(t *testing.T, ch <-chan acceptResult) net.Conn {
	t.Helper()
	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatal(res.err)
		}
		return res.conn
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not accept the dialed connection")
		return nil
	}
}

// The wrapped listener's accepted conn must report the header's source
// address as its RemoteAddr — that one method is what the login rate limiter
// (via http.Request.RemoteAddr) and the tunnel rejection logs key on — and
// must deliver only the bytes that follow the header.
func TestProxyV2ListenerThreadsHeaderSource(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := proxyV2Listener(raw)
	defer ln.Close()

	ch := acceptOne(ln)
	cli, err := net.Dial("tcp", raw.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	hdr := proxyV2HeaderTCP4("203.0.113.9", 40000, "192.0.2.1", 443)
	if _, err := cli.Write(append(hdr, []byte("hello")...)); err != nil {
		t.Fatal(err)
	}

	srv := awaitAccept(t, ch)
	defer srv.Close()
	if got := srv.RemoteAddr().String(); got != "203.0.113.9:40000" {
		t.Fatalf("RemoteAddr = %q, want PROXY header source 203.0.113.9:40000", got)
	}
	payload := make([]byte, 5)
	if _, err := io.ReadFull(srv, payload); err != nil {
		t.Fatal(err)
	}
	if string(payload) != "hello" {
		t.Fatalf("payload = %q, want %q — header bytes leaked into the stream", payload, "hello")
	}
}

// Fail-closed: with the listener wrapped, a connection that opens with
// anything but a PROXY header (here a plain HTTP request, i.e. a client that
// reached the port directly, bypassing the L4 proxy) fails its first read.
// Silently falling back to the direct peer address would be a spoof vector.
func TestProxyV2ListenerFailClosedWithoutHeader(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := proxyV2Listener(raw)
	defer ln.Close()

	ch := acceptOne(ln)
	cli, err := net.Dial("tcp", raw.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	if _, err := io.WriteString(cli, "GET / HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}

	srv := awaitAccept(t, ch)
	defer srv.Close()
	if _, err := srv.Read(make([]byte, 16)); err == nil {
		t.Fatal("first read succeeded without a PROXY header; want fail-closed rejection")
	}
}

// A v2 signature followed by garbage (here: an unknown address family) is a
// malformed header, not an absent one — same fail-closed rejection.
func TestProxyV2ListenerFailClosedOnMalformedHeader(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := proxyV2Listener(raw)
	defer ln.Close()

	ch := acceptOne(ln)
	cli, err := net.Dial("tcp", raw.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	bogus := []byte{
		0x0D, 0x0A, 0x0D, 0x0A, 0x00, 0x0D, 0x0A, 0x51, 0x55, 0x49, 0x54, 0x0A,
		0x21, 0xFF, // PROXY command, nonsense address family
		0x00, 0x00,
	}
	if _, err := cli.Write(bogus); err != nil {
		t.Fatal(err)
	}

	srv := awaitAccept(t, ch)
	defer srv.Close()
	if _, err := srv.Read(make([]byte, 16)); err == nil {
		t.Fatal("first read succeeded on a malformed v2 header; want fail-closed rejection")
	}
}

// The relay speaks PROXY v2 only (#485): a syntactically valid v1 header is
// rejected rather than honored, so the listener's contract stays exactly
// "v2, always".
func TestProxyV2ListenerRejectsV1Header(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := proxyV2Listener(raw)
	defer ln.Close()

	ch := acceptOne(ln)
	cli, err := net.Dial("tcp", raw.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	if _, err := io.WriteString(cli, "PROXY TCP4 203.0.113.9 192.0.2.1 40000 443\r\n"); err != nil {
		t.Fatal(err)
	}

	srv := awaitAccept(t, ch)
	defer srv.Close()
	// Deadline the read: if the v2-only validator ever regresses, the v1
	// header parses and consumes the client's bytes, and an undeadlined read
	// would hang to the package timeout instead of failing this assertion.
	if err := srv.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Read(make([]byte, 16)); !errors.Is(err, errProxyHeaderNotV2) {
		t.Fatalf("read error = %v, want errProxyHeaderNotV2", err)
	}
}

// The relay's listeners are TCP-only: a v2 PROXY header naming any other
// address family (here AF_UNIX) is rejected. Accepting it would make the
// conn's RemoteAddr an arbitrary header-controlled string (up to 108 bytes,
// newlines included) that flows unquoted into the tunnel rejection log and
// into the login rate limiter's bucket keyspace.
func TestProxyV2ListenerRejectsUnixFamilyHeader(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := proxyV2Listener(raw)
	defer ln.Close()

	ch := acceptOne(ln)
	cli, err := net.Dial("tcp", raw.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	hdr := []byte{
		0x0D, 0x0A, 0x0D, 0x0A, 0x00, 0x0D, 0x0A, 0x51, 0x55, 0x49, 0x54, 0x0A, // v2 signature
		0x21,       // version 2, command PROXY
		0x31,       // AF_UNIX over STREAM
		0x00, 0xD8, // 216-byte UNIX address block
	}
	block := make([]byte, 216)
	copy(block, "/x\nFORGED LOG LINE") // the payload a naive accept would turn into RemoteAddr
	if _, err := cli.Write(append(hdr, block...)); err != nil {
		t.Fatal(err)
	}

	srv := awaitAccept(t, ch)
	defer srv.Close()
	if err := srv.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Read(make([]byte, 16)); !errors.Is(err, errProxyHeaderNotTCP) {
		t.Fatalf("read error = %v, want errProxyHeaderNotTCP", err)
	}
}

// freeTCPAddr reserves an ephemeral loopback port and releases it, so a test
// can point Serve at explicit addresses — Serve takes address strings, so
// ":0" would leave the chosen ports unknown. The close→rebind race is
// inherent to testing an API that binds its own listeners.
func freeTCPAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// The production wiring for #485: with proxyProto set, Serve must wrap ALL
// THREE public listeners. Every other test here builds proxyV2Listener by
// hand, so a refactor that drops the wraps in Serve — leaving
// PIPER_RELAY_PROXY_PROTOCOL=1 a no-op that silently keeps bucketing every
// user under the proxy IP — would otherwise ship with a green suite. This
// test drives Serve itself on explicit ports and probes each listener from
// the outside, both ways: a header-less connection must be dropped, and a
// connection preceded by a valid v2 header must be served.
func TestServeWrapsPublicListenersWhenProxyProtocolEnabled(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	cert, key := writeWildcard(t, "public.getpiper.co")
	tlsCfg, err := LoadWildcardConfig(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	router := NewRouter()
	// A routed custom domain gives the :80 probe a dropped-vs-answered
	// signal: unrouted :80 connections are dropped header or not, but a
	// routed one is spliced down the tunnel and stays open.
	relaySess, _ := pipeSession(t, "box.public.getpiper.co")
	router.RegisterCustom("app.example.com", relaySess)
	ctrl := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	ctrlHost := "api." + st.apexOrDefault()

	tlsAddr, httpAddr, tunnelAddr := freeTCPAddr(t), freeTCPAddr(t), freeTCPAddr(t)
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- Serve(tlsAddr, httpAddr, tunnelAddr, st, tlsCfg, router, ctrl, nil, nil, nil, testInstance(t, st), true)
	}()

	// dial waits out Serve's startup and fails fast if Serve itself died. It
	// takes the subtest's t (as does expectDropped) so a failure Goexit kills
	// the subtest, not the parent.
	dial := func(t *testing.T, addr string) net.Conn {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
			if err == nil {
				return conn
			}
			select {
			case err := <-serveErr:
				t.Fatalf("Serve returned before the probes ran: %v", err)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("Serve never started listening on %s: %v", addr, err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	// expectDropped asserts the relay closed conn without answering: on every
	// listener the wrapped read fails before a byte reaches the serving path,
	// so the client sees EOF. A read that blocks to the deadline means the
	// connection was served as if no PROXY header were required.
	expectDropped := func(t *testing.T, conn net.Conn, what string) {
		t.Helper()
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, err := conn.Read(make([]byte, 1))
		if err == nil {
			t.Fatalf("%s answered a header-less connection: the listener is not wrapped", what)
		}
		var nerr net.Error
		if errors.As(err, &nerr) && nerr.Timeout() {
			t.Fatalf("%s kept a header-less connection open: the listener is not wrapped", what)
		}
	}

	// :7000, header-less: the handshake frame must never be processed. The
	// relay distinguishes "never read" (EOF while awaiting the ack) from
	// "read and rejected" (a rejection ack) — only the former is acceptable.
	t.Run("tunnel without header is dropped", func(t *testing.T) {
		conn := dial(t, tunnelAddr)
		defer conn.Close()
		_, err := tunnel.Dial(conn, "stale-token", "box.public.getpiper.co")
		if err == nil {
			t.Fatal("tunnel handshake succeeded with a token the relay never issued")
		}
		if !strings.Contains(err.Error(), "awaiting relay handshake ack") {
			t.Fatalf("header-less tunnel handshake was processed (%v): the tunnel listener is not wrapped", err)
		}
	})

	// :7000, with a valid v2 header: the handshake IS processed — and now
	// fails only because the token is unknown, proving the header was
	// consumed and the conn served.
	t.Run("tunnel with v2 header is served", func(t *testing.T) {
		conn := dial(t, tunnelAddr)
		defer conn.Close()
		if _, err := conn.Write(proxyV2HeaderTCP4("203.0.113.30", 40000, "192.0.2.1", 7000)); err != nil {
			t.Fatal(err)
		}
		_, err := tunnel.Dial(conn, "stale-token", "box.public.getpiper.co")
		if err == nil || !strings.Contains(err.Error(), "relay rejected") {
			t.Fatalf("want the relay to process the handshake after a valid v2 header and reject the token; got %v", err)
		}
	})

	// :80, header-less: a routed-Host request must be dropped, not spliced.
	t.Run("http without header is dropped", func(t *testing.T) {
		conn := dial(t, httpAddr)
		if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: app.example.com\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		expectDropped(t, conn, ":80")
	})

	// :80, with a valid v2 header: the request routes to the custom domain
	// and is pumped down the tunnel, so the connection stays open (nobody
	// answers — the pipe session's agent side is silent — but nothing closes
	// it either).
	t.Run("http with v2 header is served", func(t *testing.T) {
		conn := dial(t, httpAddr)
		defer conn.Close()
		if _, err := conn.Write(proxyV2HeaderTCP4("203.0.113.31", 40000, "192.0.2.1", 80)); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: app.example.com\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, err := conn.Read(make([]byte, 1))
		var nerr net.Error
		if !errors.As(err, &nerr) || !nerr.Timeout() {
			t.Fatalf(":80 with a valid v2 header: read = %v, want the connection held open (routed down the tunnel)", err)
		}
	})

	// :443, header-less: the SNI dispatch must never see the ClientHello, so
	// the TLS handshake to the control plane cannot complete.
	t.Run("tls without header is dropped", func(t *testing.T) {
		tr := &http.Transport{
			TLSClientConfig: &tls.Config{ServerName: ctrlHost, InsecureSkipVerify: true},
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", tlsAddr)
			},
		}
		defer tr.CloseIdleConnections()
		resp, err := (&http.Client{Transport: tr, Timeout: 5 * time.Second}).Get("https://" + ctrlHost + "/")
		if err == nil {
			resp.Body.Close()
			t.Fatal(":443 answered a request whose connection carried no PROXY header: the listener is not wrapped")
		}
	})

	// :443, with a valid v2 header: full path — header consumed, SNI dispatch
	// to api.<apex>, TLS terminated, control handler answers 200.
	t.Run("tls with v2 header is served", func(t *testing.T) {
		tr := &http.Transport{
			TLSClientConfig: &tls.Config{ServerName: ctrlHost, InsecureSkipVerify: true},
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				c, err := (&net.Dialer{}).DialContext(ctx, "tcp", tlsAddr)
				if err != nil {
					return nil, err
				}
				if _, err := c.Write(proxyV2HeaderTCP4("203.0.113.32", 40000, "192.0.2.1", 443)); err != nil {
					c.Close()
					return nil, err
				}
				return c, nil
			},
		}
		defer tr.CloseIdleConnections()
		resp, err := (&http.Client{Transport: tr, Timeout: 5 * time.Second}).Get("https://" + ctrlHost + "/")
		if err != nil {
			t.Fatalf(":443 did not serve a connection carrying a valid v2 header: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf(":443 with a valid v2 header: status = %d, want 200", resp.StatusCode)
		}
	})
}

func captureRelayLog(t *testing.T) *syncLogBuffer {
	t.Helper()
	var logged syncLogBuffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&logged)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })
	return &logged
}

func waitForLog(t *testing.T, logged *syncLogBuffer, want string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s := logged.String(); strings.Contains(s, want) {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("log never contained %q; log was %q", want, logged.String())
	return ""
}

// With the tunnel listener wrapped, the handshake rejection log must name the
// PROXY header's source address, not the L4 proxy's — the #485 report.
func TestProxyProtocolTunnelRejectionNamesHeaderSource(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	logged := captureRelayLog(t)

	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	go acceptTunnels(proxyV2Listener(raw), st, NewRouter(), nil, nil, nil, testInstance(t, st))

	conn, err := net.Dial("tcp", raw.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write(proxyV2HeaderTCP4("203.0.113.12", 40000, "192.0.2.1", 7000)); err != nil {
		t.Fatal(err)
	}
	if _, err := tunnel.Dial(conn, "stale-token", "092942b4-alice.public.getpiper.co"); err == nil {
		t.Fatal("Dial succeeded with a token the relay never issued")
	}

	waitForLog(t, logged, "from 203.0.113.12:40000")
}

// Spoof-safety with the feature OFF: an unwrapped listener (the default, env
// unset) must not consume PROXY-looking bytes — they are just garbage in
// front of the handshake — and the rejection log must name the direct peer,
// never the address the bytes tried to claim. This pins byte-identical
// pre-#485 behaviour.
func TestProxyProtocolDisabledIgnoresSpoofedHeader(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	logged := captureRelayLog(t)

	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	go acceptTunnels(raw, st, NewRouter(), nil, nil, nil, testInstance(t, st)) // unwrapped: feature off

	conn, err := net.Dial("tcp", raw.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	// A spoof attempt: PROXY v2 bytes claiming 203.0.113.13, then EOF so the
	// server's handshake frame read fails immediately.
	if _, err := conn.Write(proxyV2HeaderTCP4("203.0.113.13", 40000, "192.0.2.1", 7000)); err != nil {
		t.Fatal(err)
	}
	conn.Close()

	s := waitForLog(t, logged, "tunnel handshake rejected")
	if strings.Contains(s, "203.0.113.13") {
		t.Fatalf("unwrapped listener honored a spoofed PROXY header; log was %q", s)
	}
	if !strings.Contains(s, "from 127.0.0.1:") {
		t.Fatalf("rejection log does not name the direct peer; log was %q", s)
	}
}

// The #485 failure mode end to end: behind an L4 proxy every login used to
// share one bucket keyed on the proxy's IP. With the listener wrapped, the
// rate limiter must bucket on the PROXY header's source address, so one
// abuser's budget is theirs alone. Drives the real :443 path — PROXY header,
// SNI dispatch to the control plane, TLS termination, HTTP login handler.
func TestProxyProtocolLoginRateLimitKeysHeaderSource(t *testing.T) {
	cert, key := writeWildcard(t, "public.getpiper.co")
	tlsCfg, err := LoadWildcardConfig(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	api, st, _ := newTestAPI(t)

	ctrlQ := newConnQueue()
	srv := &http.Server{Handler: api, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	go func() { _ = srv.Serve(ctrlQ) }()
	t.Cleanup(func() { _ = srv.Close() })

	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := proxyV2Listener(raw)
	defer ln.Close()
	ctrlHost := "api." + st.apexOrDefault()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go handlePublic(c, NewRouter(), tlsCfg, ctrlHost, ctrlQ, nil)
		}
	}()

	// loginFrom fires one device-login request whose connection arrives
	// through the "L4 proxy": plain TCP to the listener, preceded by a PROXY
	// v2 header claiming claimedIP as the client.
	loginFrom := func(claimedIP string) int {
		tr := &http.Transport{
			TLSClientConfig: &tls.Config{ServerName: ctrlHost, InsecureSkipVerify: true},
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				c, err := (&net.Dialer{}).DialContext(ctx, "tcp", raw.Addr().String())
				if err != nil {
					return nil, err
				}
				if _, err := c.Write(proxyV2HeaderTCP4(claimedIP, 40000, "192.0.2.1", 443)); err != nil {
					c.Close()
					return nil, err
				}
				return c, nil
			},
		}
		defer tr.CloseIdleConnections()
		resp, err := (&http.Client{Transport: tr}).Post("https://"+ctrlHost+"/v1/login/device", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	for i := 0; i < loginLimitBurst; i++ {
		if c := loginFrom("203.0.113.10"); c != http.StatusOK {
			t.Fatalf("login #%d from claimed 203.0.113.10 = %d, want 200", i+1, c)
		}
	}
	if c := loginFrom("203.0.113.10"); c != http.StatusTooManyRequests {
		t.Fatalf("burst+1 login from claimed 203.0.113.10 = %d, want 429 (bucket keyed on the header source)", c)
	}
	// A second client behind the same proxy — identical direct peer — has its
	// own bucket: the abuser's exhausted budget is not shared.
	if c := loginFrom("203.0.113.11"); c != http.StatusOK {
		t.Fatalf("login from claimed 203.0.113.11 = %d, want 200 (independent bucket behind one proxy)", c)
	}
}
