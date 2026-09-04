package relay

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/piperbox/piper/internal/tunnel"
)

// edgeRelay is one in-process relay standing behind the edge under test.
type edgeRelay struct {
	inst   *Instance
	router *Router
	apiURL string
	stop   func() // leaves the pool and closes every listener; idempotent
}

// startRelayBehindEdge runs a relay the way Serve does — tunnel, TLS and
// HTTP listeners wrapped for PROXY v2, control API on an httptest server —
// tagged with X-Relay so a response names the process that produced it, plus
// RunInstance so it heartbeats and owns. Returns once the pool lists it.
func startRelayBehindEdge(t *testing.T, st *Store, tlsCfg *tls.Config) *edgeRelay {
	t.Helper()
	router := NewRouter()
	delivery := NewTunnelDelivery(st, router)

	var api http.Handler
	var inst *Instance
	tagged := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("X-Relay", inst.ID)
		api.ServeHTTP(w, r)
	})
	apiSrv := httptest.NewServer(tagged)
	t.Cleanup(apiSrv.Close)

	tunLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	httpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tlsLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	inst, err = NewInstance("127.0.0.1", tlsLn.Addr().String(), httpLn.Addr().String(), tunLn.Addr().String(), apiSrv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	api = NewAPIWithTunnel(st, NewFakeVerifier(), "", router, nil, nil, inst)

	ctrlQ := newConnQueue()
	ctrlSrv := &http.Server{Handler: tagged, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = ctrlSrv.Serve(ctrlQ) }()
	ctrlHost := "api." + st.apexOrDefault()

	go acceptTunnels(proxyV2Listener(tunLn), st, router, nil, delivery, nil, inst)
	go acceptHTTP(proxyV2Listener(httpLn), router, nil)
	wrappedTLS := proxyV2Listener(tlsLn)
	go func() {
		for {
			c, err := wrappedTLS.Accept()
			if err != nil {
				return
			}
			go handlePublic(c, router, tlsCfg, ctrlHost, ctrlQ, nil)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	poolDone := make(chan struct{})
	go func() { RunInstance(ctx, st, inst, router, delivery); close(poolDone) }()
	waitCond(t, 5*time.Second, "relay "+inst.ID+" in the pool", func() bool {
		rows, _ := st.LiveInstances()
		for _, r := range rows {
			if r.ID == inst.ID {
				return true
			}
		}
		return false
	})

	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			<-poolDone
			tunLn.Close()
			httpLn.Close()
			tlsLn.Close()
			ctrlQ.Close()
		})
	}
	t.Cleanup(stop)
	return &edgeRelay{inst: inst, router: router, apiURL: apiSrv.URL, stop: stop}
}

// startEdge runs the edge on ephemeral ports against st and returns its
// config plus the edge itself, once the TLS listener accepts. It runs the
// body ServeEdge runs, holding the edge so a test can wait on the cluster
// picture routing reads rather than on the store rows behind it.
func startEdge(t *testing.T, st *Store) (EdgeConfig, *edge) {
	t.Helper()
	cfg := EdgeConfig{Apex: "public.getpiper.co", TLSAddr: freeTCPAddr(t), HTTPAddr: freeTCPAddr(t), TunnelAddr: freeTCPAddr(t)}
	e := newEdge(cfg, st, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errc := make(chan error, 1)
	go func() { errc <- e.serve(ctx) }()
	waitCond(t, 5*time.Second, "edge listening", func() bool {
		select {
		case err := <-errc:
			t.Fatalf("edge serve returned: %v", err)
		default:
		}
		// The TLS listener is opened last, so it accepting means all three are up.
		c, err := net.DialTimeout("tcp", cfg.TLSAddr, 200*time.Millisecond)
		if err != nil {
			return false
		}
		c.Close()
		return true
	})
	return cfg, e
}

// dialAgentThroughEdge dials the edge's :7000 as a box would and returns the
// agent-side session plus the dialer's local address (what PROXY v2 must
// carry to the relay).
func dialAgentThroughEdge(t *testing.T, cfg EdgeConfig, en Enrollment) (*tunnel.Session, string) {
	t.Helper()
	conn, err := net.Dial("tcp", cfg.TunnelAddr)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := tunnel.Dial(conn, en.Token, en.BaseDomain)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess, conn.LocalAddr().String()
}

// fakeAgent answers the streams a box would: passthrough (report the SNI it
// peeked), KindHTTP (record the body, answer 202), control API (echo method
// and path, like fakeBox).
func fakeAgent(sess *tunnel.Session, snis, bodies chan string) {
	for {
		kind, stream, err := sess.AcceptKind()
		if err != nil {
			return
		}
		go func() {
			defer stream.Close()
			switch kind {
			case tunnel.KindPassthrough:
				sni, _, _ := readSNI(stream)
				snis <- sni
			case tunnel.KindHTTP:
				req, err := http.ReadRequest(bufio.NewReader(stream))
				if err != nil {
					return
				}
				body, _ := io.ReadAll(req.Body)
				bodies <- string(body)
				_, _ = io.WriteString(stream, "HTTP/1.1 202 Accepted\r\nContent-Length: 0\r\n\r\n")
			case tunnel.KindControlAPI:
				req, err := http.ReadRequest(bufio.NewReader(stream))
				if err != nil {
					return
				}
				body := req.Method + " " + req.URL.RequestURI()
				fmt.Fprintf(stream, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
			}
		}()
	}
}

// waitEdgeOwner blocks until the edge itself resolves base to a live owner
// and returns that row. The store row lands first (the relay writes it before
// the piper_owners NOTIFY reaches the edge), and routing reads the edge's
// copy, so this is the wait a caller about to send traffic needs. It is also
// the wait that proves the owning relay's router already holds the session:
// serveTunnel registers before it records ownership.
func waitEdgeOwner(t *testing.T, e *edge, base string) InstanceRow {
	t.Helper()
	var owner InstanceRow
	waitCond(t, 5*time.Second, "edge sees an owner for "+base, func() bool {
		r, ok := e.state.ownerOf(base)
		owner = r
		return ok
	})
	return owner
}

// waitEdgeSessions blocks until the edge's own copy of the pool reports id
// with n sessions. Two hops have to land before the next box can be balanced
// against this one: the owning relay's heartbeat writes the count, and the
// piper_instances NOTIFY it fires refreshes the edge. Waiting on the store
// row alone would race the second hop.
func waitEdgeSessions(t *testing.T, e *edge, id string, n int) {
	t.Helper()
	waitCond(t, 5*time.Second, fmt.Sprintf("edge sees %d sessions on %s", n, id), func() bool {
		e.state.mu.RLock()
		defer e.state.mu.RUnlock()
		r, ok := e.state.instances[id]
		return ok && r.Sessions == n
	})
}

// sendClientHello starts a TLS handshake to addr with the given SNI and
// returns the conn; the handshake never completes (only the ClientHello has
// to travel), so the caller reads the far side's reaction elsewhere.
func sendClientHello(t *testing.T, addr, sni string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	go tls.Client(conn, &tls.Config{ServerName: sni, InsecureSkipVerify: true}).Handshake()
	return conn
}

func expectString(t *testing.T, ch <-chan string, want, what string) {
	t.Helper()
	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("%s: got %q, want %q", what, got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: nothing arrived", what)
	}
}

// TestEdgePlacesAndRoutesAcrossTwoRelays is the test that earns the design
// its keep: two relays in one pool, one edge in front, two boxes dialled
// through it. Placement spreads them; passthrough follows ownership; the
// relay sees the box's own address through PROXY v2; api.<apex> is pinned.
func TestEdgePlacesAndRoutesAcrossTwoRelays(t *testing.T) {
	logged := captureRelayLog(t)
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	acc, err := st.UpsertAccount("sub-1", "alice")
	if err != nil {
		t.Fatal(err)
	}
	en1, err := st.EnrollForAccount(acc.ID, "box-1")
	if err != nil {
		t.Fatal(err)
	}
	en2, err := st.EnrollForAccount(acc.ID, "box-2")
	if err != nil {
		t.Fatal(err)
	}
	cert, key := writeWildcard(t, "public.getpiper.co")
	tlsCfg, err := LoadWildcardConfig(cert, key)
	if err != nil {
		t.Fatal(err)
	}

	rA := startRelayBehindEdge(t, st, tlsCfg)
	rB := startRelayBehindEdge(t, st, tlsCfg)
	cfg, e := startEdge(t, st)

	// Placement: the first box lands somewhere; once that relay reports the
	// session, the second box must land on the other relay.
	s1, _ := dialAgentThroughEdge(t, cfg, en1)
	owner1 := waitEdgeOwner(t, e, en1.BaseDomain)
	waitEdgeSessions(t, e, owner1.ID, 1)
	s2, _ := dialAgentThroughEdge(t, cfg, en2)
	owner2 := waitEdgeOwner(t, e, en2.BaseDomain)
	if owner1.ID == owner2.ID {
		t.Fatalf("both boxes placed on %s; least-sessions placement failed", owner1.ID)
	}

	// Passthrough follows ownership: each SNI reaches its own box, through
	// whichever relay holds it.
	snis1, snis2 := make(chan string, 4), make(chan string, 4)
	go fakeAgent(s1, snis1, make(chan string, 4))
	go fakeAgent(s2, snis2, make(chan string, 4))
	sendClientHello(t, cfg.TLSAddr, "app."+en1.BaseDomain)
	expectString(t, snis1, "app."+en1.BaseDomain, "box 1 passthrough")
	sendClientHello(t, cfg.TLSAddr, "app."+en2.BaseDomain)
	expectString(t, snis2, "app."+en2.BaseDomain, "box 2 passthrough")

	// PROXY v2: a rejected handshake is logged with the dialer's address,
	// not the edge's.
	bogus, err := net.Dial("tcp", cfg.TunnelAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer bogus.Close()
	_, _ = tunnel.Dial(bogus, "not-a-token", "nobody.public.getpiper.co")
	waitForLog(t, logged, "from "+bogus.LocalAddr().String())

	// api.<apex> is pinned to the earliest-started relay (rA) and reaches
	// its control plane: no bearer → 401 from that process.
	tc, err := tls.Dial("tcp", cfg.TLSAddr, &tls.Config{ServerName: "api.public.getpiper.co", InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	if _, err := io.WriteString(tc, "GET /agents HTTP/1.1\r\nHost: api.public.getpiper.co\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tc), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("X-Relay") != rA.inst.ID {
		t.Fatalf("api.<apex> via edge: %d from %q, want 401 from %s (rB is %s)", resp.StatusCode, resp.Header.Get("X-Relay"), rA.inst.ID, rB.inst.ID)
	}
}

// TestEdgeClusterControlHopWebhookDrainAndOwnershipMove is the rest of the
// cluster contract in one flow: a control call that lands on the wrong relay
// is answered through the owner, an event parked by any process wakes the
// owner that holds the box, and when the owner leaves the pool the box's
// reconnection is placed on the survivor and ownership follows it.
func TestEdgeClusterControlHopWebhookDrainAndOwnershipMove(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	acc, err := st.UpsertAccount("sub-1", "alice")
	if err != nil {
		t.Fatal(err)
	}
	aliceCred, err := st.MintAccountCredential(acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	en1, err := st.EnrollForAccount(acc.ID, "box-1")
	if err != nil {
		t.Fatal(err)
	}
	en2, err := st.EnrollForAccount(acc.ID, "box-2")
	if err != nil {
		t.Fatal(err)
	}

	rA := startRelayBehindEdge(t, st, nil)
	rB := startRelayBehindEdge(t, st, nil)
	cfg, e := startEdge(t, st)

	s1, _ := dialAgentThroughEdge(t, cfg, en1)
	owner1 := waitEdgeOwner(t, e, en1.BaseDomain)
	waitEdgeSessions(t, e, owner1.ID, 1)
	dialAgentThroughEdge(t, cfg, en2)
	waitEdgeOwner(t, e, en2.BaseDomain)

	bodies1 := make(chan string, 4)
	go fakeAgent(s1, make(chan string, 4), bodies1)

	ownerRelay, otherRelay := rA, rB
	if owner1.ID == rB.inst.ID {
		ownerRelay, otherRelay = rB, rA
	}

	// Control hop: a call that lands on the non-owner is answered by the
	// box through the owner. The response carries both relays' tags.
	req, err := http.NewRequest(http.MethodGet, otherRelay.apiURL+"/agents/"+en1.BaseDomain+"/v1/apps", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+aliceCred)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "GET /v1/apps" {
		t.Fatalf("hop via non-owner: %d %q", resp.StatusCode, body)
	}
	tags := strings.Join(resp.Header.Values("X-Relay"), ",")
	if !strings.Contains(tags, ownerRelay.inst.ID) {
		t.Fatalf("response tags %q do not name the owner %s", tags, ownerRelay.inst.ID)
	}
	if !strings.Contains(tags, otherRelay.inst.ID) {
		t.Fatalf("response tags %q do not name the relay that was called, %s", tags, otherRelay.inst.ID)
	}

	// Webhook drain: an event parked by any process wakes the owner.
	if err := st.ParkEvent(en1.BaseDomain, "blog", "main", "push", []byte(`{"after":"x"}`)); err != nil {
		t.Fatal(err)
	}
	expectString(t, bodies1, `{"after":"x"}`, "parked webhook drained by the owner")

	// Ownership move: the owner leaves the pool and its box reconnects;
	// the edge must place it on the survivor and the row must follow.
	ownerRelay.stop()
	// Wait on the edge, not the store: placement reads the edge's copy of
	// the pool, so the redial below is only deterministic once the NOTIFY
	// that removed the stopped relay has landed here.
	waitCond(t, 5*time.Second, "edge drops the stopped relay from its pool", func() bool {
		e.state.mu.RLock()
		defer e.state.mu.RUnlock()
		_, still := e.state.instances[ownerRelay.inst.ID]
		return !still
	})
	s1.Close()
	dialAgentThroughEdge(t, cfg, en1)
	waitCond(t, 5*time.Second, "edge routes box 1 to the survivor", func() bool {
		r, ok := e.state.ownerOf(en1.BaseDomain)
		return ok && r.ID == otherRelay.inst.ID
	})
	// The edge only learns an owner from the row, so the row already moved;
	// assert it directly so a future in-memory shortcut cannot hide that.
	if r, ok, err := st.OwnerOf(en1.BaseDomain); err != nil || !ok || r.ID != otherRelay.inst.ID {
		t.Fatalf("agent_owners after the move: %+v ok=%v err=%v, want %s", r, ok, err, otherRelay.inst.ID)
	}
}

// TestEdgeEvictsDeadRelayOnDialFailureAndRetriesOnceOnTunnel: :7000 placement
// is a choice among equals, so a relay that does not answer costs one retry —
// and the eviction that pays for it takes its ownership rows with it.
func TestEdgeEvictsDeadRelayOnDialFailureAndRetriesOnceOnTunnel(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	deadAddr := freeTCPAddr(t) // nothing listens: dial is refused at once
	stampInstance(t, st, "dead", deadAddr, time.Now().Add(-time.Minute))
	liveLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { liveLn.Close() })
	stampInstance(t, st, "live", liveLn.Addr().String(), time.Now())
	if err := st.SetOwner(en.BaseDomain, "dead"); err != nil {
		t.Fatal(err)
	}
	cfg, _ := startEdge(t, st)

	// :7000 — equal load, so the earliest (dead) is tried first; the refused
	// dial evicts it and the single retry lands on live.
	accepted := acceptOne(liveLn)
	conn, err := net.Dial("tcp", cfg.TunnelAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	awaitAccept(t, accepted).Close()
	waitCond(t, 5*time.Second, "dead row deleted", func() bool {
		rows, _ := st.LiveInstances()
		return len(rows) == 1 && rows[0].ID == "live"
	})
	if _, ok, _ := st.OwnerOf(en.BaseDomain); ok {
		t.Fatal("ownership survived its instance's eviction")
	}
}

// TestEdgeNeverRetriesOnTLS is the other half of that rule: an agent's tunnel
// lives in exactly one process, so :443 has no second candidate. A dead owner
// costs the client its connection — never a splice to a relay that would
// answer for a box it does not hold.
func TestEdgeNeverRetriesOnTLS(t *testing.T) {
	st := openTestStore(t)
	en := enrollTestAgent(t, st)
	deadAddr := freeTCPAddr(t)
	stampInstance(t, st, "dead", deadAddr, time.Now())
	liveLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { liveLn.Close() })
	stampInstance(t, st, "live", liveLn.Addr().String(), time.Now())
	if err := st.SetOwner(en.BaseDomain, "dead"); err != nil {
		t.Fatal(err)
	}
	cfg, _ := startEdge(t, st)

	accepted := acceptOne(liveLn)
	conn := sendClientHello(t, cfg.TLSAddr, "app."+en.BaseDomain)
	// The edge closes the client: the owner is dead and :443 has no second candidate.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("edge kept the connection open after the owner dial failed")
	}
	select {
	case r := <-accepted:
		if r.conn != nil {
			r.conn.Close()
		}
		t.Fatal(":443 retried onto a relay that does not own the agent")
	case <-time.After(300 * time.Millisecond):
	}
	waitCond(t, 5*time.Second, "dead row deleted", func() bool {
		rows, _ := st.LiveInstances()
		return len(rows) == 1 && rows[0].ID == "live"
	})
}
