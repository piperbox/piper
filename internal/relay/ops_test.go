package relay

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/piperbox/piper/internal/tunnel"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// scrapeMetrics returns the text exposition of m's registry via promhttp.
// (Task 4 will rewrite this to use NewOpsHandler instead.)
func scrapeMetrics(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{}).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
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
