package relay

import (
	"log"
	"net"
	"net/http"
	"net/netip"
	"time"
)

// Login rate limits (#106, #522): the unauthenticated login endpoints
// (POST /v1/login/device, GET /v1/login/web, the CLI start and confirm) share
// one per-client fixed window, counted in Postgres so N relays behind an edge
// enforce one budget, not N. Real logins are human-paced — single-digit
// attempts per minute even with retries — and every device-flow start also
// costs an upstream IdP call, so this cap gives honest users ample headroom
// while capping scripted abuse.
const (
	loginLimitPerMin = 30          // requests one client key may make per window
	loginWindow      = time.Minute // fixed window length
)

// loginLimiter is the per-client login rate limit over Store.LoginHit.
type loginLimiter struct {
	st  *Store
	now func() time.Time // test seam; nil ⇒ time.Now
}

// allow reports whether ip may make another login request now. A store error
// fails closed: these are the unauthenticated endpoints, and a relay that
// cannot reach Postgres cannot finish a login anyway.
func (l *loginLimiter) allow(ip string) bool {
	now := time.Now()
	if l.now != nil {
		now = l.now()
	}
	hits, err := l.st.LoginHit(rateLimitKey(ip), now, loginWindow)
	if err != nil {
		log.Printf("relay: login rate limit: %v; refusing", err)
		return false
	}
	return hits <= loginLimitPerMin
}

// rateLimitKey normalizes ip into the login rate limiter's bucket key. A
// typical residential IPv6 allocation is a /64, and an attacker on one
// machine can otherwise source each login attempt from a fresh address
// within their prefix — dodging the fixed window's 30-per-minute cap on each
// key. Native IPv6
// addresses are therefore masked to their /64 prefix; IPv4 (including
// IPv4-mapped IPv6, e.g. ::ffff:a.b.c.d) is keyed on the address as-is,
// since a /64 mask carries no meaning there. Malformed input (should not
// occur — ip comes from clientIP or a test) is used unmasked rather than
// dropped, so the limiter fails safe rather than exempting bad input from
// rate limiting entirely.
func rateLimitKey(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	if addr.Is4() || addr.Is4In6() {
		return ip
	}
	return netip.PrefixFrom(addr, 64).Masked().String()
}

// clientIP derives the rate-limit key from the request's RemoteAddr. Nothing
// in this codebase honors X-Forwarded-For, so RemoteAddr is the client IP —
// the direct peer by default, or, with PIPER_RELAY_PROXY_PROTOCOL=1 (#485),
// the source address the trusted L4 proxy's PROXY v2 header claimed for the
// connection (Serve wraps the public listeners, and net/http captures the
// wrapped conn's RemoteAddr into the request).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
