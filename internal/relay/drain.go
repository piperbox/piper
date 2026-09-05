package relay

import (
	"context"
	"log"
	"time"
)

// drainTick is how often Drain re-reads each held session's stream count.
// A package var (cf. heartbeatInterval) so tests can reason in ticks;
// production leaves it at 100ms.
var drainTick = 100 * time.Millisecond

// Drain is the first half of a relay's exit (#523). It marks inst draining —
// which makes acceptTunnels refuse new sessions and, through the pool row,
// makes edges place nothing new here — then closes each held session the
// moment it has no open streams, so in-flight splices finish. Owned
// hostnames keep routing here meanwhile: the pool row stays until the caller
// cancels the pool context, after Drain returns. It returns when the router
// holds no agents or ctx is done; in the second case every remaining session
// is closed and the number that still had streams open is returned.
func Drain(ctx context.Context, st *Store, inst *Instance, router *Router) (forced int) {
	inst.MarkDraining()
	agents, _, _ := router.Counts()
	if err := st.UpsertInstance(inst.row(agents)); err != nil {
		log.Printf("relay: mark draining: %v", err)
	}
	tick := time.NewTicker(drainTick)
	defer tick.Stop()
	for {
		sessions := router.Sessions()
		if len(sessions) == 0 {
			return 0
		}
		for _, sess := range sessions {
			if sess.NumStreams() == 0 {
				sess.Close()
			}
		}
		select {
		case <-ctx.Done():
			for _, sess := range router.Sessions() {
				if sess.NumStreams() > 0 {
					forced++
				}
				sess.Close()
			}
			return forced
		case <-tick.C:
		}
	}
}
