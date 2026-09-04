package relay

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// hitLogin fires one request at a limited login endpoint from ip and returns
// the status code.
func hitLogin(t *testing.T, api http.Handler, method, target, ip string) int {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	req.RemoteAddr = ip + ":12345"
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	return rr.Code
}

func TestLoginDeviceRateLimited(t *testing.T) {
	api, _, _ := newTestAPI(t)
	for i := 0; i < loginLimitPerMin; i++ {
		if c := hitLogin(t, api, http.MethodPost, "/v1/login/device", "203.0.113.1"); c != http.StatusOK {
			t.Fatalf("device login #%d = %d, want 200", i+1, c)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/login/device", nil)
	req.RemoteAddr = "203.0.113.1:12345"
	rr := httptest.NewRecorder()
	api.ServeHTTP(rr, req)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("limit+1 device login = %d, want 429", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "rate limit") {
		t.Fatalf("429 body = %q, want a short plain explanation", rr.Body.String())
	}
}

func TestLoginWebRateLimited(t *testing.T) {
	api, _, _ := newWebTestAPI(t)
	target := "/v1/login/web?redirect_uri=" + url.QueryEscape("https://dash.getpiper.co/auth")
	for i := 0; i < loginLimitPerMin; i++ {
		if c := hitLogin(t, api, http.MethodGet, target, "203.0.113.2"); c != http.StatusFound {
			t.Fatalf("web login #%d = %d, want 302", i+1, c)
		}
	}
	if c := hitLogin(t, api, http.MethodGet, target, "203.0.113.2"); c != http.StatusTooManyRequests {
		t.Fatalf("limit+1 web login = %d, want 429", c)
	}
}

// Both unauthenticated login endpoints draw from the same per-IP window.
func TestLoginRateLimitSharedAcrossEndpoints(t *testing.T) {
	api, _, _ := newWebTestAPI(t)
	target := "/v1/login/web?redirect_uri=" + url.QueryEscape("https://dash.getpiper.co/auth")
	for i := 0; i < loginLimitPerMin/2; i++ {
		hitLogin(t, api, http.MethodPost, "/v1/login/device", "203.0.113.3")
		hitLogin(t, api, http.MethodGet, target, "203.0.113.3")
	}
	if c := hitLogin(t, api, http.MethodPost, "/v1/login/device", "203.0.113.3"); c != http.StatusTooManyRequests {
		t.Fatalf("device login after mixed window = %d, want 429", c)
	}
	if c := hitLogin(t, api, http.MethodGet, target, "203.0.113.3"); c != http.StatusTooManyRequests {
		t.Fatalf("web login after mixed window = %d, want 429", c)
	}
}

func TestLoginRateLimitPerIPIndependent(t *testing.T) {
	api, _, _ := newTestAPI(t)
	for i := 0; i < loginLimitPerMin; i++ {
		hitLogin(t, api, http.MethodPost, "/v1/login/device", "203.0.113.4")
	}
	if c := hitLogin(t, api, http.MethodPost, "/v1/login/device", "203.0.113.4"); c != http.StatusTooManyRequests {
		t.Fatalf("exhausted IP = %d, want 429", c)
	}
	if c := hitLogin(t, api, http.MethodPost, "/v1/login/device", "203.0.113.5"); c != http.StatusOK {
		t.Fatalf("fresh IP = %d, want 200", c)
	}
}

// A new window admits again. No sleeps — the limiter's now func is the seam.
func TestLoginRateLimitNewWindowAdmits(t *testing.T) {
	st := openTestStore(t)
	fakeNow := time.Now()
	l := loginLimiter{st: st, now: func() time.Time { return fakeNow }}
	for i := 0; i < loginLimitPerMin; i++ {
		if !l.allow("203.0.113.6") {
			t.Fatalf("request #%d rejected before the window filled", i+1)
		}
	}
	if l.allow("203.0.113.6") {
		t.Fatal("request past the window's limit allowed, want limited")
	}
	fakeNow = fakeNow.Add(loginWindow)
	if !l.allow("203.0.113.6") {
		t.Fatal("request in the next window rejected, want allowed")
	}
}

// rateLimitKey masks native IPv6 addresses to their /64 prefix so an
// attacker can't dodge the limiter by cycling addresses within their own
// allocation. IPv4 (including IPv4-mapped IPv6) is left as-is, and
// malformed input passes through unmasked rather than being dropped.
func TestRateLimitKey(t *testing.T) {
	tests := []struct {
		name string
		ip   string
		want string
	}{
		{"ipv4 unchanged", "203.0.113.1", "203.0.113.1"},
		{"ipv4 unchanged, different address", "198.51.100.9", "198.51.100.9"},
		{"ipv4-mapped ipv6 unchanged", "::ffff:203.0.113.1", "::ffff:203.0.113.1"},
		{"ipv6 masked to /64", "2001:db8:1234:5678::1", "2001:db8:1234:5678::/64"},
		{"ipv6 masked to /64, second address in same prefix", "2001:db8:1234:5678:aaaa:bbbb:cccc:dddd", "2001:db8:1234:5678::/64"},
		{"malformed input passes through unmasked", "not-an-ip", "not-an-ip"},
		{"empty string passes through unmasked", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rateLimitKey(tt.ip); got != tt.want {
				t.Fatalf("rateLimitKey(%q) = %q, want %q", tt.ip, got, tt.want)
			}
		})
	}
}

// Two IPv6 addresses in the same /64 share a window.
func TestLoginRateLimitIPv6SamePrefixSharesBucket(t *testing.T) {
	st := openTestStore(t)
	l := loginLimiter{st: st}
	for i := 0; i < loginLimitPerMin; i++ {
		if !l.allow("2001:db8:1234:5678::1") {
			t.Fatalf("request #%d from first address rejected before the window filled", i+1)
		}
	}
	if l.allow("2001:db8:1234:5678:ffff:ffff:ffff:ffff") {
		t.Fatal("second address in same /64 allowed, want limited (shared window)")
	}
}

// Two IPv6 addresses in different /64 prefixes get independent windows.
func TestLoginRateLimitIPv6DifferentPrefixIndependent(t *testing.T) {
	st := openTestStore(t)
	l := loginLimiter{st: st}
	for i := 0; i < loginLimitPerMin; i++ {
		l.allow("2001:db8:1111:1111::1")
	}
	if l.allow("2001:db8:1111:1111::1") {
		t.Fatal("request past the limit allowed for first prefix, want limited")
	}
	if !l.allow("2001:db8:2222:2222::1") {
		t.Fatal("request from a different /64 prefix rejected, want allowed")
	}
}

// Behind an edge the same client's requests are spread over every relay; the
// budget is one per client, not one per relay (#522).
func TestLoginRateLimitSharedAcrossRelays(t *testing.T) {
	st := openTestStore(t)
	st.Configure("public.getpiper.co", 3, 10, 5)
	fv := NewFakeVerifier()
	relayA, relayB := NewAPI(st, fv), NewAPI(st, fv)
	for i := 0; i < loginLimitPerMin/2; i++ {
		hitLogin(t, relayA, http.MethodPost, "/v1/login/device", "203.0.113.9")
		hitLogin(t, relayB, http.MethodPost, "/v1/login/device", "203.0.113.9")
	}
	if c := hitLogin(t, relayA, http.MethodPost, "/v1/login/device", "203.0.113.9"); c != http.StatusTooManyRequests {
		t.Fatalf("relay A after a full shared window = %d, want 429", c)
	}
	if c := hitLogin(t, relayB, http.MethodPost, "/v1/login/device", "203.0.113.9"); c != http.StatusTooManyRequests {
		t.Fatalf("relay B after a full shared window = %d, want 429", c)
	}
}

// A limiter that cannot reach its store fails closed.
func TestLoginRateLimitFailsClosedWithoutStore(t *testing.T) {
	st := openTestStore(t)
	st.Close()
	l := loginLimiter{st: st}
	if l.allow("203.0.113.10") {
		t.Fatal("limiter allowed a request it could not count")
	}
}
