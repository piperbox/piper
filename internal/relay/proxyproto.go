package relay

import (
	"errors"
	"net"

	proxyproto "github.com/pires/go-proxyproto"
)

// errProxyHeaderNotV2 rejects any PROXY header that is not version 2: the
// relay's contract for PIPER_RELAY_PROXY_PROTOCOL=1 is v2 only (#485), and a
// v1 header from a misconfigured proxy must fail loudly, not be silently
// honored under a contract the operator never opted into.
var errProxyHeaderNotV2 = errors.New("relay: PROXY header is not version 2")

// errProxyHeaderNotTCP rejects a v2 PROXY header whose address family is not
// TCP-over-IP. The wrapped listeners are TCP-only, and anything else the
// library accepts — e.g. AF_UNIX — would make the accepted conn's RemoteAddr
// an arbitrary header-controlled string (a *net.UnixAddr whose name is up to
// 108 verbatim header bytes) that flows unquoted into the tunnel rejection
// log and into the login rate limiter's bucket keyspace.
var errProxyHeaderNotTCP = errors.New("relay: PROXY header address family is not TCP")

// proxyV2Listener wraps one of the public listeners (:443 SNI, :80 HTTP,
// :7000 tunnel) for PIPER_RELAY_PROXY_PROTOCOL=1 (#485). The first bytes on
// every accepted connection must be a PROXY protocol v2 header written by
// the trusted L4 proxy in front of the relay; the accepted conn's RemoteAddr
// then reports the header's source address instead of the proxy's own, which
// is what the per-IP login rate limiter (login_rate.go) and the tunnel
// handshake rejection logs (serveTunnel) key on.
//
// Fail-closed by construction: the REQUIRE policy means a connection with no
// header, a malformed header, or (via the validator) a v1 header or a non-TCP
// address family fails its first read and is dropped by the serving path. A
// listener that silently fell back to the direct peer address would let any
// client able to reach the port strip or spoof its source IP, so there is no
// fallback.
//
// The parse itself is lazy — it runs on the first Read/RemoteAddr of the
// accepted conn — but it always precedes any TLS, tunnel-handshake, or HTTP
// byte delivered to the serving path, because every one of those reads goes
// through the wrapper. With the env unset Serve never wraps, and the byte
// stream is exactly as before.
func proxyV2Listener(ln net.Listener) net.Listener {
	return &proxyproto.Listener{
		Listener: ln,
		ConnPolicy: func(proxyproto.ConnPolicyOptions) (proxyproto.Policy, error) {
			return proxyproto.REQUIRE, nil
		},
		ValidateHeader: func(h *proxyproto.Header) error {
			if h.Version != 2 {
				return errProxyHeaderNotV2
			}
			if h.Command.IsProxy() && h.TransportProtocol != proxyproto.TCPv4 && h.TransportProtocol != proxyproto.TCPv6 {
				return errProxyHeaderNotTCP
			}
			return nil
		},
	}
}
