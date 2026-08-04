package relay

import (
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Login rate limits (#106): the two unauthenticated login endpoints
// (POST /v1/login/device, GET /v1/login/web) share one per-IP token bucket.
// Real logins are human-paced — single-digit attempts per minute even with
// retries — and every device-flow start also costs an upstream IdP call, so
// these values give honest users ample headroom while capping scripted abuse.
const (
	loginLimitBurst   = 10               // requests one IP may fire at once
	loginLimitPerMin  = 30               // sustained refill
	loginLimitMaxIdle = 10 * time.Minute // idle buckets evicted (mirrors the web-state TTL)
)

// loginLimiter is a per-IP token bucket for the unauthenticated login
// endpoints. The zero value is ready to use. Buckets are swept inline on each
// check — the same opportunistic pattern as the web-state sweep in loginWeb —
// so there is no janitor goroutine.
type loginLimiter struct {
	mu      sync.Mutex
	buckets map[string]*loginBucket
	now     func() time.Time // test seam; nil ⇒ time.Now
}

type loginBucket struct {
	lim  *rate.Limiter
	seen time.Time // last request, for idle eviction
}

// allow reports whether ip may make another login request at this moment.
func (l *loginLimiter) allow(ip string) bool {
	key := rateLimitKey(ip)
	now := time.Now()
	if l.now != nil {
		now = l.now()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, b := range l.buckets {
		if now.Sub(b.seen) > loginLimitMaxIdle {
			delete(l.buckets, k)
		}
	}
	b, ok := l.buckets[key]
	if !ok {
		if l.buckets == nil {
			l.buckets = map[string]*loginBucket{}
		}
		b = &loginBucket{lim: rate.NewLimiter(rate.Every(time.Minute/loginLimitPerMin), loginLimitBurst)}
		l.buckets[key] = b
	}
	b.seen = now
	return b.lim.AllowN(now, 1)
}

// rateLimitKey normalizes ip into the login rate limiter's bucket key. A
// typical residential IPv6 allocation is a /64, and an attacker on one
// machine can otherwise source each login attempt from a fresh address
// within their prefix — a fresh burst-10 bucket every time. Native IPv6
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
