package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"net"
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
}

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
	inst := &Instance{ID: hex.EncodeToString(raw[:]), StartedAt: time.Now().UTC()}
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

// row is the heartbeat payload: the identity plus the current session count.
func (i *Instance) row(sessions int) InstanceRow {
	return InstanceRow{ID: i.ID, StartedAt: i.StartedAt, Sessions: sessions,
		TLSAddr: i.TLSAddr, HTTPAddr: i.HTTPAddr, TunnelAddr: i.TunnelAddr, APIAddr: i.APIAddr}
}

// heartbeat upserts the instance row now and every heartbeatInterval until
// ctx is done, then deletes it: a clean stop leaves routing at once instead
// of after instanceTTL. The session count is the router's live agent total.
func (i *Instance) heartbeat(ctx context.Context, st *Store, router *Router) {
	beat := func() {
		agents, _, _ := router.Counts()
		if err := st.UpsertInstance(i.row(agents)); err != nil {
			log.Printf("relay: heartbeat: %v", err)
		}
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
