package relay

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/piperbox/piper/internal/tunnel"
)

// scrapeMetrics returns the text exposition of m's registry via NewOpsHandler.
func scrapeMetrics(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	NewOpsHandler(m, nil).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

func TestMetricsTopologyGauges(t *testing.T) {
	router := NewRouter()
	m := NewMetrics(router)
	router.Register(&tunnel.Session{BaseDomain: "alice.example.com"})
	s := &tunnel.Session{BaseDomain: "bob.example.com"}
	router.Register(s)
	router.RegisterHost("app-bob.public.getpiper.co", s)
	router.RegisterCustom("byo.example.org", s)

	body := scrapeMetrics(t, m)
	for _, want := range []string{
		"piper_relay_agents_connected 2",
		"piper_relay_hostnames_routed 1",
		"piper_relay_custom_domains_routed 1",
		"go_goroutines", // free Go collector present
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape missing %q\n%s", want, body)
		}
	}
}

func TestMetricsCounters(t *testing.T) {
	m := NewMetrics(NewRouter())
	m.ConnAccepted("tls")
	m.ConnAccepted("tls")
	m.ConnRouted("http")
	m.ConnUnrouted("tls")
	m.StreamStart()

	body := scrapeMetrics(t, m)
	for _, want := range []string{
		`piper_relay_conns_accepted_total{listener="tls"} 2`,
		`piper_relay_conns_routed_total{listener="http"} 1`,
		`piper_relay_conns_unrouted_total{listener="tls"} 1`,
		"piper_relay_active_streams 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape missing %q\n%s", want, body)
		}
	}
	m.StreamEnd()
	if body := scrapeMetrics(t, m); !strings.Contains(body, "piper_relay_active_streams 0") {
		t.Errorf("active_streams should return to 0\n%s", body)
	}
}

func TestMetricsNilReceiverIsNoOp(t *testing.T) {
	var m *Metrics // every instrument method must be safe on nil
	m.ConnAccepted("tls")
	m.ConnRouted("http")
	m.ConnUnrouted("http")
	m.StreamStart()
	m.StreamEnd()
}

func TestOpsHandlerLogs(t *testing.T) {
	ring := NewLogRing(8)
	ring.Write([]byte("first\nsecond\n"))
	rec := httptest.NewRecorder()
	NewOpsHandler(nil, ring).ServeHTTP(rec, httptest.NewRequest("GET", "/logs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /logs = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q, want text/plain", ct)
	}
	if got := rec.Body.String(); got != "first\nsecond\n" {
		t.Fatalf("body = %q, want %q", got, "first\nsecond\n")
	}
}

func TestOpsHandlerDisabledEndpoints404(t *testing.T) {
	cases := []struct {
		name string
		h    http.Handler
		path string
		want int
	}{
		{"metrics off", NewOpsHandler(nil, NewLogRing(8)), "/metrics", http.StatusNotFound},
		{"logs off", NewOpsHandler(NewMetrics(NewRouter()), nil), "/logs", http.StatusNotFound},
		{"metrics on", NewOpsHandler(NewMetrics(NewRouter()), nil), "/metrics", http.StatusOK},
		{"logs on", NewOpsHandler(nil, NewLogRing(8)), "/logs", http.StatusOK},
		{"unknown path", NewOpsHandler(NewMetrics(NewRouter()), NewLogRing(8)), "/nope", http.StatusNotFound},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		c.h.ServeHTTP(rec, httptest.NewRequest("GET", c.path, nil))
		if rec.Code != c.want {
			t.Errorf("%s: GET %s = %d, want %d", c.name, c.path, rec.Code, c.want)
		}
	}
}

// waitForScrape polls the scrape until it contains want or the deadline
// passes — counters increment on goroutines the test doesn't join.
func waitForScrape(t *testing.T, m *Metrics, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(scrapeMetrics(t, m), want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("scrape never contained %q\n%s", want, scrapeMetrics(t, m))
}

func TestCountersUnroutedTLS(t *testing.T) {
	router := NewRouter()
	m := NewMetrics(router)
	c1, c2 := net.Pipe()
	defer c1.Close()
	go handlePublic(c2, router, nil, "api.public.getpiper.co", nil, m)
	// A real ClientHello for an unknown SNI: drive a TLS client handshake at
	// the pipe; readSNI parses the hello, the router matches nothing.
	go tls.Client(c1, &tls.Config{ServerName: "unknown.example.com", InsecureSkipVerify: true}).Handshake()
	waitForScrape(t, m, `piper_relay_conns_unrouted_total{listener="tls"} 1`)
}

func TestCountersUnroutedHTTP(t *testing.T) {
	router := NewRouter()
	m := NewMetrics(router)
	c1, c2 := net.Pipe()
	defer c1.Close()
	go handleHTTP(c2, router, m)
	go c1.Write([]byte("GET / HTTP/1.1\r\nHost: nope.example.com\r\n\r\n"))
	waitForScrape(t, m, `piper_relay_conns_unrouted_total{listener="http"} 1`)
}

func TestCountersRoutedHTTPAndActiveStreams(t *testing.T) {
	router := NewRouter()
	m := NewMetrics(router)
	server, client := newSessionPair(t)
	router.RegisterCustom("byo.example.org", server)
	// The agent side accepts the spliced stream and drains it, like a box's
	// Caddy would.
	go func() {
		_, stream, err := client.AcceptKind()
		if err != nil {
			return
		}
		io.Copy(io.Discard, stream)
	}()
	c1, c2 := net.Pipe()
	go handleHTTP(c2, router, m)
	go c1.Write([]byte("GET / HTTP/1.1\r\nHost: byo.example.org\r\n\r\n"))
	waitForScrape(t, m, `piper_relay_conns_routed_total{listener="http"} 1`)
	waitForScrape(t, m, "piper_relay_active_streams 1")
	c1.Close()                                          // splice ends…
	waitForScrape(t, m, "piper_relay_active_streams 0") // …and the gauge returns to 0
}

func TestCountersAcceptedHTTP(t *testing.T) {
	router := NewRouter()
	m := NewMetrics(router)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go acceptHTTP(ln, router, m)
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	waitForScrape(t, m, `piper_relay_conns_accepted_total{listener="http"} 1`)
}

func TestEdgeMetricsUseTheEdgePrefixAndCountDialFailures(t *testing.T) {
	m := NewEdgeMetrics()
	m.ConnAccepted("tls")
	m.DialFailed("tunnel")
	rr := httptest.NewRecorder()
	NewOpsHandler(m, nil).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rr.Body.String()
	for _, want := range []string{
		`piper_edge_conns_accepted_total{listener="tls"} 1`,
		`piper_edge_backend_dial_failures_total{listener="tunnel"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
	if strings.Contains(body, "piper_edge_agents_connected") {
		t.Error("edge exposes a router gauge it has no router for")
	}
}
