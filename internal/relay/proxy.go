package relay

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/piperbox/piper/internal/tunnel"
)

// responseHeaderTimeout bounds how long the relay waits for a box to start
// sending response headers after a control request is forwarded. A wedged box
// must not hold a relay goroutine and the caller's connection indefinitely; the
// caller sees a 502 instead. Only header arrival is bounded — a long-lived
// response *body* (future SSE log streaming) is unaffected, since this timeout
// stops once the headers are in hand. It is a package var (cf. disabledPollInterval
// in server.go) so tests can drive the timeout with a short value; production
// leaves it at 30s.
var responseHeaderTimeout = 30 * time.Second

// errNoTunnelSession is returned by the transport's dialer when the request
// carries no tunnel session in its context — a programming error, never a
// caller-triggered path.
var errNoTunnelSession = errors.New("relay: control proxy request has no tunnel session")

// proxyRoute carries the per-request forwarding target through the request
// context, so a single ReverseProxy + Transport can be reused across every
// request instead of rebuilt per call (the session, path tail, and box token
// all vary per request but the wiring does not).
type proxyRoute struct {
	sess     *tunnel.Session
	base     string
	tail     string
	boxToken string
}

type routeCtxKey struct{}

func withRoute(ctx context.Context, rt *proxyRoute) context.Context {
	return context.WithValue(ctx, routeCtxKey{}, rt)
}

func routeFromContext(ctx context.Context) *proxyRoute {
	rt, _ := ctx.Value(routeCtxKey{}).(*proxyRoute)
	return rt
}

// hopHeaderName marks a request that has already been forwarded once by
// another relay's control hop. A router miss on a request carrying this
// header must not hop again — see the NewControlProxy doc comment.
const hopHeaderName = "X-Piper-Hop"

// hopCtxKey carries the owner relay's api_addr for a hopped request.
type hopCtxKey struct{}

func withHop(ctx context.Context, apiAddr string) context.Context {
	return context.WithValue(ctx, hopCtxKey{}, apiAddr)
}

func hopFromContext(ctx context.Context) string {
	addr, _ := ctx.Value(hopCtxKey{}).(string)
	return addr
}

// tunnelDialer opens a control-API stream on a live session. *tunnel.Session
// satisfies it; tests inject a blocking opener to exercise dial cancellation.
type tunnelDialer interface {
	OpenKind(kind byte) (net.Conn, error)
}

// dialControlStream opens a KindControlAPI stream while honoring ctx: if the
// caller disconnects (or otherwise cancels) while the open is still pending, it
// returns ctx.Err() promptly rather than blocking on the tunnel, and any stream
// the abandoned open later yields is closed so it can't leak.
func dialControlStream(ctx context.Context, d tunnelDialer) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		conn, err := d.OpenKind(tunnel.KindControlAPI)
		ch <- result{conn, err}
	}()
	select {
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.err == nil {
				r.conn.Close()
			}
		}()
		return nil, ctx.Err()
	case r := <-ch:
		return r.conn, r.err
	}
}

// NewControlProxy serves /agents and /agents/<base-domain>/v1/*: it
// authenticates the caller's relay account credential, authorizes that the
// account owns the agent or is a member of the owning org (#104), and
// reverse-proxies the request over the agent's tunnel as a KindControlAPI
// stream — swapping the caller's credential for the box's stored control
// bearer. The box still validates that bearer on every request (#77); the
// relay hop grants nothing at the box. Unknown and unowned agents are both
// 404 so existence is never leaked across tenants. Bare /agents lists the
// caller's own enrolled agents with liveness (#98).
//
// With N relays an agent's tunnel may live in another process. self is this
// process's pool identity; when the router misses and Postgres names a live
// owner that is not self, the request is forwarded unchanged — original
// path, original Authorization — to that relay's control API over plain
// HTTP on the internal network (:8080 is documented as "front with TLS"),
// where it is authenticated and rewritten exactly as here. The hop marks the
// outbound request with the hopHeaderName header; a relay that sees that
// header on a router miss never hops it again, answering 503 instead
// exactly like the dead-owner case. That header — not the owner.ID != self.ID
// check alone — is what bounds the hop to one level: the owner row is a live
// upsert a reconnecting agent can move at any moment, so by the time a hop
// lands the owner may no longer name the sender, and without the header a
// flapping agent could ping-pong the request between relays indefinitely.
// Liveness answers (GET /agents, GET /agents/<base>, the DELETE refusal)
// count a live owner row as connected, and fail closed — 500, not a false
// "not connected" — when the owner lookup itself errors.
// self == nil is single-process mode: no hop, router-only liveness.
func NewControlProxy(st *Store, router *Router, self *Instance) http.Handler {
	// One Transport + ReverseProxy for the whole control proxy, reused across
	// every request (the control proxy is built once per relay API / Router, so
	// this is effectively a per-router singleton). The transport carries no
	// per-request state — the target session, path tail, and box token all ride
	// the request context via proxyRoute — so a single instance serves every
	// caller. DisableKeepAlives keeps the one-stream-per-request invariant: a
	// pooled stream must never outlive its session; reusing the Transport pools
	// nothing because keep-alives stay off.
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			rt := routeFromContext(ctx)
			if rt == nil {
				return nil, errNoTunnelSession
			}
			return dialControlStream(ctx, rt.sess)
		},
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: responseHeaderTimeout,
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			rt := routeFromContext(pr.Out.Context())
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = rt.base
			pr.Out.URL.Path = "/" + rt.tail
			// Never forward the caller's account credential to the box.
			// Inject the box's own bearer; if the box never provisioned one,
			// forward bare and let its auth gate answer 401.
			pr.Out.Header.Del("Authorization")
			if rt.boxToken != "" {
				pr.Out.Header.Set("Authorization", "Bearer "+rt.boxToken)
			}
			// Relay-internal bookkeeping; the box has no use for it.
			pr.Out.Header.Del(hopHeaderName)
		},
		Transport:     transport,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// Log the detail server-side; return a generic body so no transport
			// internals leak to the caller.
			base := "?"
			if rt := routeFromContext(r.Context()); rt != nil {
				base = rt.base
			}
			log.Printf("relay: control proxy to %s failed: %v", base, err)
			http.Error(w, "box unreachable", http.StatusBadGateway)
		},
	}
	hop := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = hopFromContext(pr.Out.Context())
			pr.Out.Host = pr.In.Host
			pr.Out.Header.Set(hopHeaderName, "1")
			// Everything else — path, query, Authorization — travels as is.
		},
		// The dial needs its own bound: ResponseHeaderTimeout starts once the
		// connection is up, so a hop to a node that is gone (SYN blackholed)
		// would otherwise hold the caller for the OS SYN-retry budget — the
		// same 2 s the edge gives a relay.
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: edgeDialTimeout}).DialContext,
			ResponseHeaderTimeout: responseHeaderTimeout,
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("relay: control hop to %s failed: %v", hopFromContext(r.Context()), err)
			http.Error(w, "owner relay unreachable", http.StatusBadGateway)
		},
	}

	// liveOwner is the cluster half of liveness: the live instance holding
	// base, if it is not this process. The error surfaces a broken lookup to
	// the caller instead of swallowing it — a query-level failure here must
	// not be silently read as "nobody owns it".
	liveOwner := func(base string) (InstanceRow, bool, error) {
		if self == nil {
			return InstanceRow{}, false, nil
		}
		owner, ok, err := st.OwnerOf(base)
		if err != nil {
			return InstanceRow{}, false, err
		}
		return owner, ok && owner.ID != self.ID, nil
	}
	connected := func(base string) (bool, error) {
		if _, ok := router.Lookup(base); ok {
			return true, nil
		}
		_, ok, err := liveOwner(base)
		if err != nil {
			return false, err
		}
		return ok, nil
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cred, ok := bearerToken(r)
		if !ok {
			http.Error(w, "missing bearer credential", http.StatusUnauthorized)
			return
		}
		acc, err := st.AuthenticateAccount(cred)
		if err != nil {
			http.Error(w, "bad credential", http.StatusUnauthorized)
			return
		}

		// Path shape: /agents[/<base-domain>[/v1/...]]
		rest := strings.TrimPrefix(r.URL.Path, "/agents")
		rest = strings.TrimPrefix(rest, "/")
		base, tail, _ := strings.Cut(rest, "/")
		if base == "" {
			// List the account's own agents, each with liveness from the
			// in-memory session map — same answers as the per-agent endpoint,
			// and only ever the caller's rows, so nothing can leak.
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			visible, err := st.AgentsVisibleTo(acc.ID)
			if err != nil {
				http.Error(w, "list failed", http.StatusInternalServerError)
				return
			}
			agents := make([]map[string]any, 0, len(visible))
			for _, a := range visible {
				up, err := connected(a.BaseDomain)
				if err != nil {
					log.Printf("relay: liveness check for %s: %v", a.BaseDomain, err)
					http.Error(w, "list failed", http.StatusInternalServerError)
					return
				}
				agents = append(agents, map[string]any{"agent": a.BaseDomain, "name": a.Name, "owner": a.Owner, "connected": up})
			}
			writeJSON(w, http.StatusOK, map[string]any{"agents": agents})
			return
		}

		ownerID, _, _, err := st.AgentAccount(base)
		if err != nil {
			// Unknown agent and disabled owner both 404: no existence leak.
			http.NotFound(w, r)
			return
		}
		if ok, err := st.CanControl(acc.ID, ownerID); err != nil || !ok {
			http.NotFound(w, r)
			return
		}

		if tail == "" {
			switch r.Method {
			case http.MethodGet:
				// Liveness: answered by the relay itself from its in-memory
				// session map — never opens a tunnel stream. Offline is an
				// answer, not an error: 200 with connected:false. A broken
				// lookup is not offline, though — fail closed instead of
				// reporting a possibly-live agent as disconnected.
				up, err := connected(base)
				if err != nil {
					log.Printf("relay: liveness check for %s: %v", base, err)
					http.Error(w, "liveness check failed", http.StatusInternalServerError)
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{"agent": base, "connected": up})
			case http.MethodDelete:
				// Enrolling a box is owner-only (api.go); removing one must be
				// too, or a plain member could permanently destroy a box
				// nobody can re-create without shell access to the hardware.
				// CanControl (checked above) is deliberately looser — it grants
				// drive rights to every member — so removal needs its own,
				// stricter gate.
				if ok, err := st.CanManage(acc.ID, ownerID); err != nil || !ok {
					http.Error(w, "owner role required", http.StatusForbidden)
					return
				}
				// Refuse while the box is live. Removal is irreversible — the
				// enrollment token is gone and the box must re-enroll — so a
				// mistyped base domain must not be able to retire a running
				// box. The caller stops piperd on it and retries. A broken
				// liveness lookup must fail closed here too: proceeding to
				// delete on an error could destroy a box that is live on
				// another relay.
				up, err := connected(base)
				if err != nil {
					log.Printf("relay: liveness check for %s: %v", base, err)
					http.Error(w, "liveness check failed", http.StatusInternalServerError)
					return
				}
				if up {
					http.Error(w, "box is connected; stop piperd on it first", http.StatusConflict)
					return
				}
				if err := st.DeleteAgent(base); err != nil {
					if errors.Is(err, ErrUnknownAgent) {
						http.NotFound(w, r)
						return
					}
					log.Printf("relay: remove agent %s: %v", base, err)
					http.Error(w, "remove failed", http.StatusInternalServerError)
					return
				}
				log.Printf("agent removed: %s", base)
				w.WriteHeader(http.StatusNoContent)
			default:
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}
		if !strings.HasPrefix(tail, "v1/") {
			http.NotFound(w, r)
			return
		}

		sess, ok := router.Lookup(base)
		if !ok {
			// A request that already hopped once must never hop again — see
			// the doc comment above. Answer exactly like the dead-owner case.
			if r.Header.Get(hopHeaderName) == "" {
				owner, live, err := liveOwner(base)
				if err != nil {
					log.Printf("relay: owner lookup for %s: %v", base, err)
					http.Error(w, "agent lookup failed", http.StatusInternalServerError)
					return
				}
				if live {
					hop.ServeHTTP(w, r.WithContext(withHop(r.Context(), owner.APIAddr)))
					return
				}
			}
			http.Error(w, "agent not connected", http.StatusServiceUnavailable)
			return
		}
		boxToken, err := st.ControlToken(base)
		if err != nil {
			http.Error(w, "agent lookup failed", http.StatusInternalServerError)
			return
		}

		rt := &proxyRoute{sess: sess, base: base, tail: tail, boxToken: boxToken}
		rp.ServeHTTP(w, r.WithContext(withRoute(r.Context(), rt)))
	})
}
