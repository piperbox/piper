package caddy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUpsertRoutePutsRouteByID(t *testing.T) {
	type req struct{ method, path, body string }
	var reqs []req
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		reqs = append(reqs, req{r.Method, r.URL.Path, string(b)})
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	if err := c.UpsertRoute("blog.piper.localhost", 40001); err != nil {
		t.Fatalf("UpsertRoute: %v", err)
	}

	// The route must be addressed by its stable id somewhere (the delete-by-id).
	var sawID bool
	var post *req
	for i := range reqs {
		if strings.Contains(reqs[i].path, "piper-blog.piper.localhost") {
			sawID = true
		}
		if reqs[i].method == http.MethodPost {
			post = &reqs[i]
		}
	}
	if !sawID {
		t.Errorf("no request referenced the route id; got %+v", reqs)
	}
	if post == nil {
		t.Fatalf("no POST to append the route; got %+v", reqs)
	}
	// POST body must be valid JSON carrying the @id and the upstream.
	var route map[string]any
	if err := json.Unmarshal([]byte(post.body), &route); err != nil {
		t.Fatalf("body not JSON: %v (%s)", err, post.body)
	}
	if !strings.Contains(post.body, "127.0.0.1:40001") {
		t.Errorf("body missing upstream: %s", post.body)
	}
	if !strings.Contains(post.body, "piper-blog.piper.localhost") {
		t.Errorf("body missing route @id: %s", post.body)
	}
}

func TestEnsureHTTPSCreatesTLSAppAndServer(t *testing.T) {
	var puts []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/config/apps/":
			// no tls app, no piper-tls server yet
			io.WriteString(w, `{"http":{"servers":{"piper":{"listen":[":80"]}}}}`)
		case r.Method == http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			puts = append(puts, r.URL.Path+" "+string(b))
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer ts.Close()

	if err := NewClient(ts.URL).EnsureHTTPS(":8443"); err != nil {
		t.Fatalf("EnsureHTTPS: %v", err)
	}
	if len(puts) != 2 {
		t.Fatalf("puts = %v, want tls app + piper-tls server", puts)
	}
	if !strings.Contains(puts[0], "/config/apps/tls ") || !strings.Contains(puts[0], "load_pem") {
		t.Fatalf("first put = %q, want tls app", puts[0])
	}
	if !strings.Contains(puts[1], "/config/apps/http/servers/piper-tls ") ||
		!strings.Contains(puts[1], `":8443"`) ||
		!strings.Contains(puts[1], "tls_connection_policies") {
		t.Fatalf("second put = %q, want piper-tls server on :8443", puts[1])
	}
}

func TestEnsureHTTPSIsIdempotent(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/config/apps/" {
			io.WriteString(w, `{"http":{"servers":{"piper":{},"piper-tls":{}}},"tls":{}}`)
			return
		}
		t.Errorf("unexpected write %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()
	if err := NewClient(ts.URL).EnsureHTTPS(":8443"); err != nil {
		t.Fatalf("EnsureHTTPS: %v", err)
	}
}

func TestUpsertRouteTLSTargetsTLSServer(t *testing.T) {
	var postPaths []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNotFound) // no prior route
			return
		}
		if r.Method == http.MethodPost {
			postPaths = append(postPaths, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	if err := NewClient(ts.URL).UpsertRouteTLS("blog.example.com", 40001); err != nil {
		t.Fatalf("UpsertRouteTLS: %v", err)
	}
	// The reverse proxy goes on piper-tls; the paired :80 redirect goes on
	// piper (asserted in TestUpsertRouteTLSArmsHTTPRedirect).
	var sawTLS bool
	for _, p := range postPaths {
		if p == "/config/apps/http/servers/piper-tls/routes" {
			sawTLS = true
		}
	}
	if !sawTLS {
		t.Fatalf("posted to %q, want a POST to piper-tls routes", postPaths)
	}
}

// recordingCaddy captures every admin call so the route/redirect pairing can
// be asserted as a whole.
type caddyCall struct{ method, path, body string }

func recordingCaddy(t *testing.T) (*httptest.Server, *[]caddyCall) {
	t.Helper()
	var calls []caddyCall
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		calls = append(calls, caddyCall{r.Method, r.URL.Path, string(b)})
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNotFound) // nothing there yet
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)
	return ts, &calls
}

// A host served over the box-terminated :443 must also answer on :80, or a
// visitor typing the bare domain gets a 404 — browsers default to http://.
// The "piper" server has automatic_https disabled, so Caddy provisions no
// redirect of its own and it has to be an explicit route (#357).
func TestUpsertRouteTLSArmsHTTPRedirect(t *testing.T) {
	ts, calls := recordingCaddy(t)
	if err := NewClient(ts.URL).UpsertRouteTLS("myshop.com", 40001); err != nil {
		t.Fatalf("UpsertRouteTLS: %v", err)
	}

	var tlsPost, redirPost *caddyCall
	for i := range *calls {
		c := &(*calls)[i]
		if c.method != http.MethodPost {
			continue
		}
		switch c.path {
		case "/config/apps/http/servers/piper-tls/routes":
			tlsPost = c
		case "/config/apps/http/servers/piper/routes":
			redirPost = c
		}
	}
	if tlsPost == nil {
		t.Fatalf("no reverse-proxy route posted to piper-tls; got %+v", *calls)
	}
	if redirPost == nil {
		t.Fatalf("no redirect route posted to the plaintext piper server; got %+v", *calls)
	}

	var route struct {
		ID     string                    `json:"@id"`
		Match  []struct{ Host []string } `json:"match"`
		Handle []struct {
			Handler    string              `json:"handler"`
			StatusCode int                 `json:"status_code"`
			Headers    map[string][]string `json:"headers"`
		} `json:"handle"`
	}
	if err := json.Unmarshal([]byte(redirPost.body), &route); err != nil {
		t.Fatalf("redirect body not JSON: %v (%s)", err, redirPost.body)
	}
	if len(route.Match) != 1 || len(route.Match[0].Host) != 1 || route.Match[0].Host[0] != "myshop.com" {
		t.Fatalf("redirect must match the exact host; got %+v", route.Match)
	}
	if len(route.Handle) != 1 || route.Handle[0].Handler != "static_response" {
		t.Fatalf("want a static_response handler, got %+v", route.Handle)
	}
	if route.Handle[0].StatusCode != 308 {
		t.Fatalf("status_code = %d, want 308", route.Handle[0].StatusCode)
	}
	loc := route.Handle[0].Headers["Location"]
	if len(loc) != 1 || !strings.HasPrefix(loc[0], "https://") {
		t.Fatalf("Location = %v, want a single https:// redirect", loc)
	}

	// The two routes live on different servers under the SAME host, so they
	// must not share an @id — routeID is keyed on host alone and upsertRoute
	// deletes by id first, so a shared id would make them evict each other.
	if route.ID == routeID("myshop.com") {
		t.Fatalf("redirect reuses the reverse-proxy route id %q; they would evict each other", route.ID)
	}
	if !strings.Contains(tlsPost.body, routeID("myshop.com")) {
		t.Fatalf("tls route missing its own id: %s", tlsPost.body)
	}
}

// Re-deploying a custom domain must replace both routes in place, never leave
// a second copy behind: each upsert deletes its own id first.
func TestUpsertRouteTLSDeletesBothIDsBeforeAppending(t *testing.T) {
	ts, calls := recordingCaddy(t)
	if err := NewClient(ts.URL).UpsertRouteTLS("myshop.com", 40001); err != nil {
		t.Fatalf("UpsertRouteTLS: %v", err)
	}
	deleted := map[string]bool{}
	for _, c := range *calls {
		if c.method == http.MethodDelete {
			deleted[strings.TrimPrefix(c.path, "/id/")] = true
		}
	}
	if !deleted[routeID("myshop.com")] {
		t.Errorf("reverse-proxy route id never deleted; deletes = %v", deleted)
	}
	if !deleted[redirectID("myshop.com")] {
		t.Errorf("redirect route id never deleted; deletes = %v", deleted)
	}
}

// Stop/delete/teardown drop a host through RemoveRoute; the redirect must go
// with it or :80 keeps redirecting to a domain that no longer serves.
func TestRemoveRouteDropsTheRedirectToo(t *testing.T) {
	ts, calls := recordingCaddy(t)
	if err := NewClient(ts.URL).RemoveRoute("myshop.com"); err != nil {
		t.Fatalf("RemoveRoute: %v", err)
	}
	deleted := map[string]bool{}
	for _, c := range *calls {
		if c.method == http.MethodDelete {
			deleted[strings.TrimPrefix(c.path, "/id/")] = true
		}
	}
	if !deleted[routeID("myshop.com")] || !deleted[redirectID("myshop.com")] {
		t.Fatalf("RemoveRoute deleted %v, want both the route and the redirect id", deleted)
	}
}

// The two id namespaces must not be able to collide: without a separator that
// cannot occur in a hostname, the redirect for "example.com" and the route for
// a host literally named "redirect-example.com" would share an id.
func TestRouteAndRedirectIDsCannotCollide(t *testing.T) {
	if redirectID("example.com") == routeID("redirect-example.com") {
		t.Fatalf("id collision: redirectID(example.com) == routeID(redirect-example.com) == %q", redirectID("example.com"))
	}
	if strings.ContainsAny(redirectID("example.com"), "/") {
		t.Fatalf("redirect id %q contains a slash; it is used as a URL path segment", redirectID("example.com"))
	}
}
