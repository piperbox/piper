package relay

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
