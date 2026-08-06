package relay

import (
	"log"
	"net"
	"sync"
	"time"
)

// terminateHandshakeLogInterval caps the terminate-path handshake-failure log
// at one line per interval. The terminate path fronts the PUBLIC listener, so
// an unthrottled line per failed handshake lets any scanner fill the log
// (#496); one line per interval is still enough to see an expired or
// mismatched cert, a bad SNI, or a truncated ClientHello.
const terminateHandshakeLogInterval = 30 * time.Second

// terminateHandshakeGate records when the last terminate handshake failure was
// logged. It re-arms on time alone: a failure still happening an interval
// later is logged again, so a recurring outage is never hidden by its first
// report — the #497 lesson is that dedup which never re-arms silently
// swallows repeat outages.
var terminateHandshakeGate struct {
	mu   sync.Mutex
	last time.Time
}

// logTerminateHandshakeFailure logs a failed terminate-path handshake with the
// peer address and the error — the same shape as the ALPN solver's handshake
// log (#242): the failure is otherwise invisible server-side, and a completed
// handshake stays quiet. Rate-limited per terminateHandshakeLogInterval.
func logTerminateHandshakeFailure(peer net.Addr, err error) {
	terminateHandshakeGate.mu.Lock()
	defer terminateHandshakeGate.mu.Unlock()
	now := time.Now()
	if now.Sub(terminateHandshakeGate.last) < terminateHandshakeLogInterval {
		return
	}
	terminateHandshakeGate.last = now
	log.Printf("relay: terminate: handshake from %s: %v", peer, err)
}
