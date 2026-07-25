// Package caddy drives a running Caddy instance through its JSON admin API.
package caddy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
)

type Client struct {
	base string
	http *http.Client
}

func NewClient(adminBase string) *Client {
	return &Client{base: adminBase, http: &http.Client{}}
}

func routeID(host string) string { return "piper-" + host }

// redirectID is the :80 redirect's stable id for the same host. The ":" is
// what keeps the two namespaces apart: a hostname cannot contain one, so no
// host's routeID can ever collide with another host's redirectID (it is also
// a legal URL path character, and the id is addressed as /id/<id>).
func redirectID(host string) string { return "piper-redirect:" + host }

// UpsertRoute makes Caddy reverse-proxy host to 127.0.0.1:<port> over HTTP.
// The route carries a stable @id so re-deploys replace it: existing route is
// removed by id (404 ignored), then a fresh one is appended.
func (c *Client) UpsertRoute(host string, upstreamHostPort int) error {
	return c.upsertRoute("piper", host, upstreamHostPort)
}

// UpsertRouteTLS is UpsertRoute for the piper-tls server — the runtime-enabled
// HTTPS listener that serves the BYO custom domain (see EnsureHTTPS) — plus
// the :80 redirect that sends plaintext visitors to it.
//
// The redirect is armed here rather than left to callers because the two are
// never wanted apart: a host reachable only over https:// 404s for anyone who
// types the bare domain, and browsers default to http://. The plaintext
// "piper" server sets automatic_https.disable, so Caddy provisions no redirect
// of its own — it has to be an explicit route (#357).
func (c *Client) UpsertRouteTLS(host string, upstreamHostPort int) error {
	if err := c.upsertRoute("piper-tls", host, upstreamHostPort); err != nil {
		return err
	}
	return c.upsertRedirect(host)
}

// upsertRedirect appends the 308 to https:// for host on the plaintext "piper"
// server, under its own stable @id so redeploys replace it in place.
func (c *Client) upsertRedirect(host string) error {
	route := map[string]any{
		"@id":   redirectID(host),
		"match": []map[string]any{{"host": []string{host}}},
		"handle": []map[string]any{{
			"handler":     "static_response",
			"status_code": 308,
			// Placeholders keep the port and path/query of the original
			// request, so a redirect is never lossy.
			"headers": map[string][]string{
				"Location": {"https://{http.request.host}{http.request.uri}"},
			},
		}},
	}
	if err := c.removeByID(redirectID(host)); err != nil {
		return err
	}
	return c.write(http.MethodPost, "/config/apps/http/servers/piper/routes", route)
}

func (c *Client) upsertRoute(server, host string, upstreamHostPort int) error {
	route := map[string]any{
		"@id":   routeID(host),
		"match": []map[string]any{{"host": []string{host}}},
		"handle": []map[string]any{{
			"handler":   "reverse_proxy",
			"upstreams": []map[string]any{{"dial": fmt.Sprintf("127.0.0.1:%d", upstreamHostPort)}},
		}},
	}
	// Only this route's own id: the paired redirect has its own, and
	// UpsertRouteTLS replaces it separately.
	if err := c.removeByID(routeID(host)); err != nil {
		return err
	}
	return c.write(http.MethodPost, "/config/apps/http/servers/"+server+"/routes", route)
}

// EnsureHTTPS arms TLS serving at runtime for managers started HTTP-only:
// it creates the tls app (empty load_pem) and a dedicated "piper-tls" server
// on listen. A separate server, because tls_connection_policies applies to a
// whole server — the "piper" server must keep speaking plaintext on :80 for
// relay-terminated traffic. Idempotent.
func (c *Client) EnsureHTTPS(listen string) error {
	resp, err := c.http.Get(c.base + "/config/apps/")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var apps struct {
		TLS  json.RawMessage `json:"tls"`
		HTTP struct {
			Servers map[string]json.RawMessage `json:"servers"`
		} `json:"http"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&apps); err != nil {
		return fmt.Errorf("caddy read config: %w", err)
	}
	if len(apps.TLS) == 0 || string(apps.TLS) == "null" {
		tlsApp := map[string]any{"certificates": map[string]any{"load_pem": []any{}}}
		if err := c.write(http.MethodPut, "/config/apps/tls", tlsApp); err != nil {
			return err
		}
	}
	if _, ok := apps.HTTP.Servers["piper-tls"]; !ok {
		srv := map[string]any{
			"listen":                  []string{listen},
			"routes":                  []any{},
			"automatic_https":         map[string]any{"disable": true},
			"tls_connection_policies": []any{map[string]any{}},
		}
		if err := c.write(http.MethodPut, "/config/apps/http/servers/piper-tls", srv); err != nil {
			return err
		}
	}
	return nil
}

// RemoveRoute deletes the route addressed by the host's stable id, and the
// :80 redirect that UpsertRouteTLS pairs with it. A missing route (404) is not
// an error, so this is a no-op for the hosts that never had a redirect.
func (c *Client) RemoveRoute(host string) error {
	if err := c.removeByID(routeID(host)); err != nil {
		return err
	}
	return c.removeByID(redirectID(host))
}

// removeByID deletes one config object by its @id; 404 is not an error.
func (c *Client) removeByID(id string) error {
	url := c.base + "/id/" + id
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("caddy remove route: status %d", resp.StatusCode)
}

// CertPair is one PEM cert/key entry in Caddy's tls.certificates.load_pem.
type CertPair struct {
	CertPEM string
	KeyPEM  string
}

// ReplaceCerts replaces Caddy's complete load_pem certificate list with pairs.
// The domain manager owns the full set (box-wide wildcard + per-app exact-host
// certs) and re-syncs it whole on every change, so issue/renew/remove can never
// append duplicates or strand another domain's cert. An empty pairs unloads all.
func (c *Client) ReplaceCerts(pairs []CertPair) error {
	entries := make([]map[string]string, 0, len(pairs))
	for _, p := range pairs {
		entries = append(entries, map[string]string{
			"certificate": p.CertPEM,
			"key":         p.KeyPEM,
		})
	}
	body, _ := json.Marshal(entries)
	url := c.base + "/config/apps/tls/certificates/load_pem"
	req, _ := http.NewRequest(http.MethodPatch, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("caddy replace certs: status %d", resp.StatusCode)
	}
	return nil
}

// write sends a JSON body to the admin API and errors on non-2xx.
func (c *Client) write(method, path string, body any) error {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, c.base+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("caddy %s %s: status %d", method, path, resp.StatusCode)
	}
	return nil
}
