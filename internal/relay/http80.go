package relay

import (
	"bufio"
	"net"
	"net/http"
	"time"

	"github.com/piperbox/piper/internal/tunnel"
)

// Port-80 routing for BYO custom domains (#228). Custom domains are :443
// SNI-passthrough, so the relay never terminates them — and without this
// listener plain http://<custom-domain> would die at the relay. The relay
// peeks the Host header, matches it against registered custom domains only
// (exact or subdomain, same matching as the :443 SNI path), and pumps the
// connection down the owning agent's tunnel as a KindHTTP stream — the same
// stream kind the terminate path uses; the agent pipes it to the box's :80,
// where its Caddy answers. Pending claims route like on :443: the domain may
// hold no cert yet, and plain HTTP reaching the box pre-issuance is what
// makes HTTP-01 a usable fallback challenge. Shared-domain hostnames are
// deliberately never matched, so their HTTP behavior is unchanged.

// acceptHTTP dispatches connections from the public plain-HTTP listener.
func acceptHTTP(ln net.Listener, router *Router, m *Metrics) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		m.ConnAccepted("http")
		go handleHTTP(conn, router, m)
	}
}

// handleHTTP peeks the Host of one plain-HTTP connection and, when it names a
// registered custom domain, splices the connection down that agent's tunnel.
// Anything else — shared-domain hosts, unknown hosts, non-HTTP bytes — is
// dropped without a reply, exactly as before the listener existed.
func handleHTTP(conn net.Conn, router *Router, m *Metrics) {
	defer conn.Close()
	host, buffered, err := readHost(conn)
	if err != nil {
		return
	}
	if sess, ok := router.LookupCustom(host); ok {
		m.ConnRouted("http")
		pump(conn, buffered, sess, tunnel.KindHTTP, m)
		return
	}
	m.ConnUnrouted("http")
}

// readHost peeks the head of the HTTP request on conn and returns its Host
// (any port stripped) plus the raw bytes consumed, to be replayed down the
// tunnel — the :80 twin of readSNI. The relay parses only enough to route;
// the request itself is answered by the box.
func readHost(conn net.Conn) (string, []byte, error) {
	// Deadline the unauthenticated head read (cf. readSNI); clear it once
	// captured so the established pipe isn't killed mid-traffic.
	_ = conn.SetReadDeadline(time.Now().Add(preAuthReadTimeout))
	defer conn.SetReadDeadline(time.Time{})

	rec := &recordingConn{Conn: conn}
	req, err := http.ReadRequest(bufio.NewReader(rec))
	if err != nil {
		return "", rec.buf, err
	}
	host := req.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return host, rec.buf, nil
}
