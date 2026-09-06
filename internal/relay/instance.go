package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"net"
	"sync/atomic"
	"time"
)

// heartbeatInterval is how often a relay refreshes its relay_instances row —
// a third of instanceTTL, so two missed beats still read as live. A package
// var (cf. disabledPollInterval) so tests tick fast; production leaves it at 5s.
var heartbeatInterval = 5 * time.Second

// Instance is this relay process's identity in the pool: a random id per
// start plus the addresses an edge dials to reach each of its listeners.
type Instance struct {
	ID         string
	StartedAt  time.Time
	TLSAddr    string
	HTTPAddr   string
	TunnelAddr string
	APIAddr    string
	// Zone is the failure zone from PIPER_RELAY_ZONE; "" when unset (#531).
	Zone string
	// Ready is what /readyz reports: set on the first successful heartbeat
	// upsert, cleared for good by MarkDraining (#552).
	Ready *Readiness
	// draining is set once, on SIGTERM, and never cleared: from then on the
	// heartbeat row says so and acceptTunnels refuses new sessions (#523).
	draining atomic.Bool
}

// MarkDraining flips this instance into its final state: every heartbeat
// from now on carries draining=true and acceptTunnels refuses new sessions.
func (i *Instance) MarkDraining() {
	i.draining.Store(true)
	i.Ready.SetDraining()
}

// Draining reports whether MarkDraining has been called.
func (i *Instance) Draining() bool { return i.draining.Load() }

// NewInstance mints an instance identity. advertiseHost is the host an edge
// dials ("" ⇒ the first non-loopback IPv4, which in a container or pod is
// the address an edge can actually reach); the four addrs are the relay's
// own listener addresses, of which only the port is kept.
func NewInstance(advertiseHost, tlsAddr, httpAddr, tunnelAddr, apiAddr string) (*Instance, error) {
	if advertiseHost == "" {
		h, err := defaultAdvertiseHost()
		if err != nil {
			return nil, err
		}
		advertiseHost = h
	}
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, err
	}
	inst := &Instance{ID: hex.EncodeToString(raw[:]), StartedAt: time.Now().UTC(), Ready: &Readiness{}}
	for _, a := range []struct {
		dst  *string
		addr string
	}{{&inst.TLSAddr, tlsAddr}, {&inst.HTTPAddr, httpAddr}, {&inst.TunnelAddr, tunnelAddr}, {&inst.APIAddr, apiAddr}} {
		_, port, err := net.SplitHostPort(a.addr)
		if err != nil {
			return nil, err
		}
		*a.dst = net.JoinHostPort(advertiseHost, port)
	}
	return inst, nil
}

// defaultAdvertiseHost picks the first non-loopback IPv4 on any interface.
func defaultAdvertiseHost() (string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() {
			continue
		}
		if ip4 := ipn.IP.To4(); ip4 != nil {
			return ip4.String(), nil
		}
	}
	return "", errors.New("no non-loopback IPv4 address; set PIPER_RELAY_ADVERTISE_HOST")
}

// row is the heartbeat payload: the identity plus the current session count
// and the drain flag.
func (i *Instance) row(sessions int) InstanceRow {
	return InstanceRow{ID: i.ID, StartedAt: i.StartedAt, Sessions: sessions,
		TLSAddr: i.TLSAddr, HTTPAddr: i.HTTPAddr, TunnelAddr: i.TunnelAddr, APIAddr: i.APIAddr,
		Draining: i.draining.Load(), Zone: i.Zone}
}

// heartbeat upserts the instance row now and every heartbeatInterval until
// ctx is done, then deletes it: a clean stop leaves routing at once instead
// of after instanceTTL. The session count is the router's live agent total.
func (i *Instance) heartbeat(ctx context.Context, st *Store, router *Router) {
	beat := func() {
		agents, _, _ := router.Counts()
		if err := st.UpsertInstance(i.row(agents)); err != nil {
			log.Printf("relay: heartbeat: %v", err)
		} else {
			i.Ready.SetReady()
		}
		i.reassertOwnership(st, router)
	}
	beat()
	tick := time.NewTicker(heartbeatInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := st.DeleteInstance(i.ID); err != nil {
				log.Printf("relay: leave pool: %v", err)
			}
			return
		case <-tick.C:
			beat()
		}
	}
}

// reassertOwnership records this instance as an owner of every base its
// router holds. SetOwners is idempotent and silent for rows that exist, so
// this is one statement per beat however many agents are held (#540), and
// a relay whose rows were cascaded away — by an edge that found it
// undialable for one dial, say — gets every owner row back on the next
// beat instead of staying dark until each agent reconnects. It runs inside
// the beat, after the upsert, so our own row is live before the owner rows
// point at it.
func (i *Instance) reassertOwnership(st *Store, router *Router) {
	bases := router.Bases()
	if len(bases) == 0 {
		return
	}
	if err := st.SetOwners(bases, i.ID); err != nil {
		log.Printf("re-record %d owner rows: %v", len(bases), err)
	}
}

// basesToResync names the held agents a piper_hostnames payload can
// concern (#540). The payload is one hostname or custom domain, never a
// base, so it maps to at most two agents: the one the store now gives it
// to (an add), and the one whose router entry still carries it (a drop, or
// the loser of a move). An empty payload is the listener's resync and a
// store read failure loses nothing by sweeping: both return every held base.
func basesToResync(st *Store, router *Router, payload string) []string {
	if payload == "" {
		return router.Bases()
	}
	seen := map[string]bool{}
	var out []string
	add := func(base string) {
		if _, held := router.Holds(base); held && !seen[base] {
			seen[base] = true
			out = append(out, base)
		}
	}
	base, ok, err := st.AgentForHost(payload)
	if err != nil {
		return router.Bases()
	}
	if ok {
		add(base)
	}
	if sess, ok := router.LookupHost(payload); ok {
		add(sess.BaseDomain)
	}
	if sess, ok := router.LookupCustom(payload); ok {
		add(sess.BaseDomain)
	}
	return out
}

// RunInstance keeps this relay in the pool and reacts to the cluster. It
// heartbeats until ctx is done, then leaves. Meanwhile it LISTENs on two
// channels: a piper_events payload naming an agent this process holds
// drains its parked webhooks, and a piper_hostnames payload re-derives the
// routes of the held agents it can concern from Postgres (#530, #540). On
// every listener (re)connect both run over every held agent, so a NOTIFY
// missed while disconnected is caught up. Ownership needs no listener: two
// owners is the normal state, so nothing here reacts to another instance
// taking a row. delivery may be nil (no GitHub App).
func RunInstance(ctx context.Context, st *Store, inst *Instance, router *Router, delivery *TunnelDelivery) {
	handle := func(channel, payload string) {
		switch channel {
		case chanEvents:
			if _, ok := router.Holds(payload); !ok || delivery == nil {
				return
			}
			delivery.Dispatch(func(ctx context.Context) { delivery.DrainFor(ctx, payload) })
		case chanHostnames:
			for _, base := range basesToResync(st, router, payload) {
				if sess, ok := router.Holds(base); ok {
					syncRoutes(st, router, base, sess)
				}
			}
		}
	}
	resync := func() {
		for _, base := range router.Bases() {
			handle(chanEvents, base)
		}
		handle(chanHostnames, "")
	}
	go listen(ctx, st.dsn, []string{chanEvents, chanHostnames}, resync, handle)
	inst.heartbeat(ctx, st, router)
}
