package relay

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"sync/atomic"
	"time"

	proxyproto "github.com/pires/go-proxyproto"
)

// EdgeConfig is what ServeEdge needs; cmd/piper-edge fills it from the
// PIPER_EDGE_* environment.
type EdgeConfig struct {
	Apex       string // recognises api.<apex>
	TLSAddr    string
	HTTPAddr   string
	TunnelAddr string
	ProxyProto bool // our own listeners expect PROXY v2 from a balancer in front
}

// edgeDialTimeout bounds a backend dial; a relay that does not answer in
// this long is treated as dead.
var edgeDialTimeout = 2 * time.Second

// edgePollInterval is the belt-and-braces refresh of the instance pool: it
// also evicts rows that expired silently, which no NOTIFY announces.
var edgePollInterval = 15 * time.Second

// errBackendDial marks a backend that could not be reached. forward has
// already evicted it when this is returned.
var errBackendDial = errors.New("edge: backend dial failed")

// edge is one running piper-edge: stateless apart from its copy of the
// cluster tables. It holds no cert, speaks no HTTP beyond peeking a Host
// line, and its only store writes delete dead instance rows.
type edge struct {
	cfg     EdgeConfig
	st      *Store
	state   *edgeState
	m       *Metrics
	apiHost string
	dbDown  atomic.Bool
}

// ServeEdge runs the public entrypoint: :443 by SNI, :80 by Host, :7000 by
// load, each spliced to the owning relay behind a PROXY v2 header that
// carries the client's address. Blocks until a listener fails or ctx is done.
func ServeEdge(ctx context.Context, cfg EdgeConfig, st *Store, m *Metrics) error {
	return newEdge(cfg, st, m).serve(ctx)
}

func newEdge(cfg EdgeConfig, st *Store, m *Metrics) *edge {
	return &edge{cfg: cfg, st: st, state: newEdgeState(), m: m, apiHost: "api." + cfg.Apex}
}

// serve is ServeEdge's body, split off so a test can hold the edge and wait
// on the cluster picture its routing decisions actually read — the store row
// lands before the NOTIFY that refreshes it here.
func (e *edge) serve(ctx context.Context) error {
	e.resync()
	go listen(ctx, e.st.dsn, []string{chanInstances, chanOwners, chanHostnames}, e.resync, e.onNotify)
	go e.poll(ctx)

	errc := make(chan error, 3)
	var lns []net.Listener
	for _, l := range []struct {
		name   string
		addr   string
		handle func(net.Conn)
	}{
		{"tunnel", e.cfg.TunnelAddr, e.handleTunnel},
		{"http", e.cfg.HTTPAddr, e.handleHTTP},
		{"tls", e.cfg.TLSAddr, e.handleTLS},
	} {
		ln, err := net.Listen("tcp", l.addr)
		if err != nil {
			for _, o := range lns {
				o.Close()
			}
			return err
		}
		if e.cfg.ProxyProto {
			ln = proxyV2Listener(ln)
		}
		lns = append(lns, ln)
		go func(ln net.Listener, name string, handle func(net.Conn)) {
			for {
				conn, err := ln.Accept()
				if err != nil {
					errc <- err
					return
				}
				e.m.ConnAccepted(name)
				go handle(conn)
			}
		}(ln, l.name, l.handle)
	}
	defer func() {
		for _, ln := range lns {
			ln.Close()
		}
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// resync reloads the pool and the owner map and drops the name cache. It
// runs at start, on every listener (re)connect, and from poll.
func (e *edge) resync() {
	rows, err := e.st.LiveInstances()
	if err != nil {
		e.dbLost(err)
		return
	}
	owners, err := e.st.Owners()
	if err != nil {
		e.dbLost(err)
		return
	}
	e.dbBack()
	e.state.setInstances(rows)
	e.state.setOwners(owners)
	e.state.clearNames()
}

func (e *edge) onNotify(channel, payload string) {
	switch channel {
	case chanInstances:
		if rows, err := e.st.LiveInstances(); err != nil {
			e.dbLost(err)
		} else {
			e.dbBack()
			e.state.setInstances(rows)
		}
	case chanOwners:
		owner, ok, err := e.st.OwnerOf(payload)
		if err != nil {
			e.dbLost(err)
			return
		}
		e.dbBack()
		if ok {
			e.state.setOwner(payload, owner.ID)
		} else {
			e.state.setOwner(payload, "")
		}
	case chanHostnames:
		e.state.clearNames()
	}
}

// poll is the fallback for a NOTIFY the listener missed and the only thing
// that removes rows which expired without anyone deleting them.
func (e *edge) poll(ctx context.Context) {
	tick := time.NewTicker(edgePollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := e.st.PurgeDeadInstances(); err != nil {
				e.dbLost(err)
				continue
			}
			e.resync()
		}
	}
}

// dbLost/dbBack log an outage once each way. During one the edge keeps
// serving from memory and lets the name cache go stale rather than empty.
func (e *edge) dbLost(err error) {
	if e.dbDown.CompareAndSwap(false, true) {
		log.Printf("edge: postgres unreachable (%v); serving from memory until it returns", err)
	}
}

func (e *edge) dbBack() {
	if e.dbDown.CompareAndSwap(true, false) {
		log.Print("edge: postgres reachable again")
	}
}

// resolveAgent maps a public name to the base domain of the agent serving
// it, through the cache. customOnly is the :80 rule (custom domains only).
// When Postgres is unreachable a stale entry is served rather than nothing.
func (e *edge) resolveAgent(host string, customOnly bool) (string, bool) {
	key := host
	lookup := e.st.AgentForHost
	if customOnly {
		key = "custom:" + host
		lookup = e.st.AgentForCustomHost
	}
	agent, cached, fresh := e.state.cachedName(key)
	if cached && fresh {
		return agent, agent != ""
	}
	got, ok, err := lookup(host)
	if err != nil {
		e.dbLost(err)
		return agent, cached && agent != ""
	}
	e.dbBack()
	if !ok {
		got = ""
	}
	e.state.cacheName(key, got)
	return got, ok
}

func (e *edge) handleTLS(conn net.Conn) {
	defer conn.Close()
	sni, buffered, err := readSNI(conn)
	if err != nil {
		return
	}
	var target InstanceRow
	var ok bool
	if sni == e.apiHost {
		// Login-flow state is per-process until its follow-up lands: pin the
		// control plane to one relay so every poll sees the same memory.
		target, ok = e.state.pickAPI()
	} else if agent, found := e.resolveAgent(sni, false); found {
		target, ok = e.state.ownerOf(agent)
	}
	if !ok {
		e.m.ConnUnrouted("tls")
		return
	}
	// No retry here: the owner is unique. If it is dead the agent will
	// reconnect and a new owner row will arrive.
	_ = e.forward("tls", conn, buffered, target, target.TLSAddr)
}

func (e *edge) handleHTTP(conn net.Conn) {
	defer conn.Close()
	host, buffered, err := readHost(conn)
	if err != nil {
		return
	}
	var target InstanceRow
	var ok bool
	if agent, found := e.resolveAgent(host, true); found {
		target, ok = e.state.ownerOf(agent)
	}
	if !ok {
		e.m.ConnUnrouted("http")
		return
	}
	_ = e.forward("http", conn, buffered, target, target.HTTPAddr)
}

// handleTunnel places a new agent on the least-loaded relay. A dial failure
// evicts that relay and retries the next candidate exactly once; the pool is
// small and a second failure means something bigger is wrong.
func (e *edge) handleTunnel(conn net.Conn) {
	defer conn.Close()
	exclude := map[string]bool{}
	for attempt := 0; attempt < 2; attempt++ {
		target, ok := e.state.pickTunnel(exclude)
		if !ok {
			break
		}
		if err := e.forward("tunnel", conn, nil, target, target.TunnelAddr); !errors.Is(err, errBackendDial) {
			return
		}
		exclude[target.ID] = true
	}
	e.m.ConnUnrouted("tunnel")
}

// forward dials addr on target, writes a PROXY v2 header naming the client,
// replays the bytes consumed while peeking, then splices both ways until one
// side ends. A refused or timed-out dial marks target dead: dropped from
// memory and its row deleted, which cascades its ownership rows, so nothing
// routes there again until it heartbeats afresh.
func (e *edge) forward(listener string, conn net.Conn, buffered []byte, target InstanceRow, addr string) error {
	backend, err := net.DialTimeout("tcp", addr, edgeDialTimeout)
	if err != nil {
		e.m.DialFailed(listener)
		log.Printf("edge: %s: dial relay %s at %s: %v; evicting it", listener, target.ID, addr, err)
		e.evict(target.ID)
		return errBackendDial
	}
	defer backend.Close()
	e.m.ConnRouted(listener)
	if _, err := proxyproto.HeaderProxyFromAddrs(2, conn.RemoteAddr(), conn.LocalAddr()).WriteTo(backend); err != nil {
		return nil
	}
	if len(buffered) > 0 {
		if _, err := backend.Write(buffered); err != nil {
			return nil
		}
	}
	e.m.StreamStart()
	defer e.m.StreamEnd()
	done := make(chan struct{}, 2)
	go func() { io.Copy(backend, conn); done <- struct{}{} }()
	go func() { io.Copy(conn, backend); done <- struct{}{} }()
	<-done
	return nil
}

func (e *edge) evict(id string) {
	e.state.evict(id)
	if err := e.st.DeleteInstance(id); err != nil {
		log.Printf("edge: delete dead instance %s: %v", id, err)
	}
}
