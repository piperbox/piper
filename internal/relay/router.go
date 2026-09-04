package relay

import (
	"strings"
	"sync"

	"github.com/piperbox/piper/internal/tunnel"
)

// Router maps an incoming SNI hostname to the agent session whose base domain
// owns it (exact match or subdomain). Registrations are keyed by base domain.
type Router struct {
	mu     sync.RWMutex
	byBase map[string]*tunnel.Session
	byHost map[string]*tunnel.Session
	custom map[string]*tunnel.Session
}

func NewRouter() *Router {
	return &Router{
		byBase: map[string]*tunnel.Session{},
		byHost: map[string]*tunnel.Session{},
		custom: map[string]*tunnel.Session{},
	}
}

func (r *Router) Register(sess *tunnel.Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if sess.Closed() {
		return
	}
	r.byBase[sess.BaseDomain] = sess
}

// RegisterHost maps an exact relay-terminated hostname to a session.
func (r *Router) RegisterHost(hostname string, sess *tunnel.Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if sess.Closed() {
		return
	}
	r.byHost[hostname] = sess
}

// UnregisterHost removes a single terminated hostname.
func (r *Router) UnregisterHost(hostname string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byHost, hostname)
}

// LookupHost returns the session for an exact terminated hostname.
func (r *Router) LookupHost(hostname string) (*tunnel.Session, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.byHost[hostname]
	return s, ok
}

// RegisterCustom maps a BYO custom domain to sess. It shares byBase so
// Lookup's exact + subdomain matching applies unchanged, and also records the
// domain in custom so the :80 Host routing can match custom domains alone.
func (r *Router) RegisterCustom(domain string, sess *tunnel.Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if sess.Closed() {
		return
	}
	r.byBase[domain] = sess
	r.custom[domain] = sess
}

// UnregisterCustom removes a custom-domain mapping.
func (r *Router) UnregisterCustom(domain string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byBase, domain)
	delete(r.custom, domain)
}

func (r *Router) Unregister(sess *tunnel.Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for base, s := range r.byBase {
		if s == sess {
			delete(r.byBase, base)
		}
	}
	for host, s := range r.byHost {
		if s == sess {
			delete(r.byHost, host)
		}
	}
	for domain, s := range r.custom {
		if s == sess {
			delete(r.custom, domain)
		}
	}
}

// The register calls above refuse a closed session (sess.Closed()) under r.mu:
// because the session goroutine only calls Unregister after its session closes,
// a late in-flight register either takes r.mu before that Unregister (and its
// entry is swept by it) or observes the closed session here and no-ops — no
// permanent stale entry survives in any interleaving.

// Lookup returns the session for an SNI equal to, or a subdomain of, a
// registered base domain.
func (r *Router) Lookup(sni string) (*tunnel.Session, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if s, ok := r.byBase[sni]; ok {
		return s, true
	}
	for base, s := range r.byBase {
		if strings.HasSuffix(sni, "."+base) {
			return s, true
		}
	}
	return nil, false
}

// Holds reports the session registered for exactly base — no suffix walk.
// It is what cluster-wide bookkeeping keys on: a NOTIFY payload is an exact
// base domain, and a suffix match could name a different agent.
func (r *Router) Holds(base string) (*tunnel.Session, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.byBase[base]
	return s, ok
}

// Bases lists the agent base domains this router holds (custom domains,
// which share byBase, are excluded).
func (r *Router) Bases() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	for base := range r.byBase {
		if _, custom := r.custom[base]; !custom {
			out = append(out, base)
		}
	}
	return out
}

// LookupCustom is Lookup restricted to BYO custom domains — same exact +
// subdomain matching, but agent base domains and terminated shared hostnames
// never match. It is what keeps the :80 Host routing (#228) from serving
// shared-domain hosts over plain HTTP.
func (r *Router) LookupCustom(host string) (*tunnel.Session, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if s, ok := r.custom[host]; ok {
		return s, true
	}
	for domain, s := range r.custom {
		if strings.HasSuffix(host, "."+domain) {
			return s, true
		}
	}
	return nil, false
}

// Counts reports the router's live registration totals for the metrics
// gauges: agent sessions, relay-terminated hostnames, and custom domains.
// RegisterCustom stores a domain in both byBase and custom, so an agent is a
// byBase entry with no custom twin.
func (r *Router) Counts() (agents, hosts, custom int) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for k := range r.byBase {
		if _, ok := r.custom[k]; !ok {
			agents++
		}
	}
	return agents, len(r.byHost), len(r.custom)
}
