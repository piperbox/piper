package relay

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Metrics owns the relay's Prometheus instruments on a private registry — the
// library's global default registry would leak state between tests and panic
// on duplicate registration. Topology is exposed as GaugeFuncs reading live
// Router state at scrape time (no inc/dec bookkeeping to drift); traffic
// counters are incremented by the accept/route paths through the nil-safe
// methods below, so a relay running without metrics passes nil and the data
// path never branches on config.
type Metrics struct {
	reg           *prometheus.Registry
	connsAccepted *prometheus.CounterVec
	connsRouted   *prometheus.CounterVec
	connsUnrouted *prometheus.CounterVec
	activeStreams prometheus.Gauge
}

func NewMetrics(router *Router) *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "piper_relay_agents_connected",
		Help: "Agent tunnel sessions currently registered.",
	}, func() float64 { a, _, _ := router.Counts(); return float64(a) }))
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "piper_relay_hostnames_routed",
		Help: "Relay-terminated app hostnames currently routed.",
	}, func() float64 { _, h, _ := router.Counts(); return float64(h) }))
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "piper_relay_custom_domains_routed",
		Help: "BYO custom domains currently routed.",
	}, func() float64 { _, _, c := router.Counts(); return float64(c) }))

	m := &Metrics{
		reg: reg,
		connsAccepted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "piper_relay_conns_accepted_total",
			Help: "Connections accepted, by public listener.",
		}, []string{"listener"}),
		connsRouted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "piper_relay_conns_routed_total",
			Help: "Connections whose SNI/Host matched a registered session.",
		}, []string{"listener"}),
		connsUnrouted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "piper_relay_conns_unrouted_total",
			Help: "Connections that completed a head read but matched no session.",
		}, []string{"listener"}),
		activeStreams: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "piper_relay_active_streams",
			Help: "Connections currently being spliced down an agent tunnel.",
		}),
	}
	reg.MustRegister(m.connsAccepted, m.connsRouted, m.connsUnrouted, m.activeStreams)
	return m
}

// The instrument methods are nil-safe: a relay with metrics disabled threads a
// nil *Metrics through the data path, and every call below no-ops.

func (m *Metrics) ConnAccepted(listener string) {
	if m == nil {
		return
	}
	m.connsAccepted.WithLabelValues(listener).Inc()
}

func (m *Metrics) ConnRouted(listener string) {
	if m == nil {
		return
	}
	m.connsRouted.WithLabelValues(listener).Inc()
}

func (m *Metrics) ConnUnrouted(listener string) {
	if m == nil {
		return
	}
	m.connsUnrouted.WithLabelValues(listener).Inc()
}

func (m *Metrics) StreamStart() {
	if m == nil {
		return
	}
	m.activeStreams.Inc()
}

func (m *Metrics) StreamEnd() {
	if m == nil {
		return
	}
	m.activeStreams.Dec()
}
