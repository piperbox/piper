package relay

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/piperbox/piper/internal/tunnel"
)

// ErrAgentOffline is returned when a bound agent has no live tunnel session.
var ErrAgentOffline = errors.New("agent not connected")

// deliveryTimeout bounds one delivery attempt end to end.
const deliveryTimeout = 30 * time.Second

// TunnelDelivery hands a verified webhook to a box over its tunnel. It opens a
// KindHTTP stream — which the agent already pipes to Caddy on :80 — and speaks
// plain HTTP with Host hooks.<base>, so the box's existing webhook listener and
// Caddy route serve it exactly as a public request. Nothing new is exposed to
// the internet.
//
// Deliveries run on a bounded pool rather than a bare goroutine per binding:
// one webhook fans out to every binding for the repo, and a burst of pushes
// multiplies that. The pool is also what makes shutdown safe — every dispatched
// unit is tracked, so Shutdown can wait for it to park whatever it could not
// deliver instead of dropping it. GitHub already returned its 202, so an event
// lost here would never be retried by anyone (#295).
type TunnelDelivery struct {
	st     *Store
	router *Router

	sem    chan struct{} // delivery pool: one slot per concurrent unit
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex // guards closed against the wg.Add/Wait race
	closed bool
}

// maxConcurrentDeliveries bounds deliveries in flight relay-wide. The retry
// sweep dispatches through the same pool, so a sweep cannot itself become the
// burst the pool exists to contain (#294/#295).
const maxConcurrentDeliveries = 8

// defaultSweepInterval is how often parked events are re-checked. Retries are
// rate-limited by each event's own backoff, so this only sets the granularity.
const defaultSweepInterval = 1 * time.Minute

func NewTunnelDelivery(st *Store, router *Router) *TunnelDelivery {
	ctx, cancel := context.WithCancel(context.Background())
	return &TunnelDelivery{
		st: st, router: router,
		sem:    make(chan struct{}, maxConcurrentDeliveries),
		ctx:    ctx,
		cancel: cancel,
	}
}

// Dispatch runs fn on the delivery pool: bounded concurrency, tracked so
// Shutdown waits for it, and handed a context cancelled when the relay stops.
//
// After Shutdown, fn runs inline with an already-cancelled context instead of
// being dropped. Its delivery fails fast and its caller parks the event — the
// one thing that must not be skipped, since nobody else will retry it.
func (t *TunnelDelivery) Dispatch(fn func(ctx context.Context)) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		fn(t.ctx)
		return
	}
	t.wg.Add(1)
	t.mu.Unlock()

	go func() {
		defer t.wg.Done()
		// Always waits for a slot, including during shutdown. Bailing out of
		// the queue on cancellation would release the whole backlog at once —
		// unbounded, which is the burst the pool exists to prevent. Slots turn
		// over fast once ctx is cancelled, because the in-flight deliveries
		// holding them fail immediately; Shutdown's own deadline is the
		// backstop if they do not.
		t.sem <- struct{}{}
		defer func() { <-t.sem }()
		fn(t.ctx)
	}()
}

// Shutdown stops the sweeper, cancels in-flight deliveries so their I/O
// unblocks, and waits for every dispatched unit to finish parking what it
// could not deliver. It gives up waiting when ctx is done.
func (t *TunnelDelivery) Shutdown(ctx context.Context) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	t.mu.Unlock()
	t.cancel()

	done := make(chan struct{})
	go func() { t.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		log.Print("relay: shutdown timed out waiting for in-flight webhook deliveries")
	}
}

// StartSweeper runs the parked-event retry sweep until Shutdown. Without it a
// replay that fails to a *connected* box is re-parked and never retried: the
// box never disconnects, so no reconnect drain fires, and if no further webhook
// arrives for it no ingress re-drain fires either. The box would sit on an
// undelivered tip commit indefinitely (#294).
func (t *TunnelDelivery) StartSweeper(interval time.Duration) {
	if interval <= 0 {
		interval = defaultSweepInterval
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.wg.Add(1)
	t.mu.Unlock()

	go func() {
		defer t.wg.Done()
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-t.ctx.Done():
				return
			case <-tick.C:
				t.sweep()
			}
		}
	}()
}

// sweep retries the due parked events of every agent that is currently
// connected, and reclaims the slots of ones that are not coming back.
func (t *TunnelDelivery) sweep() {
	if n, err := t.st.PurgeExpiredPending(); err != nil {
		log.Printf("relay: purge expired pending events: %v", err)
	} else if n > 0 {
		log.Printf("relay: purged %d pending events past their TTL", n)
	}
	agents, err := t.st.AgentsWithDuePending()
	if err != nil {
		log.Printf("relay: list agents with due pending events: %v", err)
		return
	}
	for _, name := range agents {
		if _, ok := t.router.Lookup(name); !ok {
			continue // offline: the reconnect drain will pick it up
		}
		t.Dispatch(func(ctx context.Context) { t.drain(ctx, name, true) })
	}
}

func (t *TunnelDelivery) Deliver(ctx context.Context, b Binding, eventType string, payload []byte) error {
	// Checked up front so a delivery dispatched during shutdown fails fast and
	// its caller parks it, rather than opening a stream that cannot finish.
	if err := ctx.Err(); err != nil {
		return err
	}
	sess, ok := t.router.Lookup(b.AgentName)
	if !ok {
		return ErrAgentOffline
	}
	secret, err := t.st.AgentWebhookSecret(b.AgentName)
	if err != nil {
		return err
	}

	stream, err := sess.OpenKind(tunnel.KindHTTP)
	if err != nil {
		return fmt.Errorf("open delivery stream: %w", err)
	}
	defer stream.Close()
	deadline := time.Now().Add(deliveryTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = stream.SetDeadline(deadline)
	// The stream I/O below does not watch ctx, so without this a cancellation
	// (shutdown) would still wait out the full deadline. Expiring the deadline
	// unblocks the read/write immediately and the attempt fails into a park.
	defer context.AfterFunc(ctx, func() { _ = stream.SetDeadline(time.Now()) })()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://hooks."+b.AgentName+"/", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", eventType)
	// GitHub's own signature is never forwarded: the box shares no secret with
	// GitHub in brokered mode. Re-sign with the per-agent secret so the agent's
	// existing verification path is unchanged and the tunnel is not treated as
	// authenticating on its own.
	req.Header.Set("X-Hub-Signature-256", signPayload(secret, payload))
	req.ContentLength = int64(len(payload))

	if err := req.Write(stream); err != nil {
		return fmt.Errorf("write delivery: %w", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(stream), req)
	if err != nil {
		return fmt.Errorf("read delivery response: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("box rejected delivery: %s", resp.Status)
	}
	return nil
}

func signPayload(secret string, payload []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(payload)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// maxPendingPerAgent bounds the parked-event table for one box. A PR-heavy repo
// creates one slot per open PR, so the cap is what stops an offline box from
// growing the table without limit. Oldest slots are evicted first.
const maxPendingPerAgent = 50

// pendingEventTTL ages parked events out. maxPendingPerAgent bounds how many
// slots one box holds, but a box that never comes back would hold them for
// good; the TTL is what reclaims them. It is deliberately long — a park must
// survive a box being off for a weekend, since the whole point is that the tip
// commit deploys when it returns.
const pendingEventTTL = 7 * 24 * time.Hour

// Retry backoff for a parked event, applied by the sweep. A box that is up but
// rejecting deliveries (Caddy reloading, webhook listener restarting, app
// wedged) would otherwise be retried at sweep frequency indefinitely, each
// attempt costing a stream and up to deliveryTimeout.
const (
	retryBackoffBase = 1 * time.Minute
	retryBackoffMax  = 1 * time.Hour
)

// retryDelay is capped exponential backoff on the number of failed attempts.
func retryDelay(attempts int) time.Duration {
	d := retryBackoffBase
	for i := 1; i < attempts; i++ {
		d *= 2
		if d >= retryBackoffMax {
			return retryBackoffMax
		}
	}
	return d
}

// PendingEvent is a webhook parked for a box that was offline when it arrived.
type PendingEvent struct {
	AgentName string
	App       string
	Ref       string
	Event     string
	Payload   []byte
	CreatedAt string
	Attempts  int
}

// ParkEvent stores an undelivered event, coalescing by (agent, app, ref): a
// newer event for the same ref replaces the older one. Deploys are
// last-write-wins, so replaying intermediate commits on reconnect would be
// actively wrong — a box that was off overnight should deploy the tip, once.
// pendingTimeLayout is fixed-width, unlike RFC3339Nano (which trims trailing
// fractional zeros, so "…:05Z" sorts lexicographically *after* "…:05.4Z").
// The eviction and drain ordering below compare created_at as strings and
// depend on lexicographic == chronological.
const pendingTimeLayout = "2006-01-02T15:04:05.000000000Z"

// A park always follows a failed delivery, so the slot starts at one attempt
// and is not due again until that attempt's backoff has elapsed. A genuinely
// new event taking the slot resets both: it has its own delivery to fail.
func (s *Store) ParkEvent(agentName, app, ref, event string, payload []byte) error {
	now := time.Now().UTC()
	stamp := now.Format(pendingTimeLayout)
	next := now.Add(retryDelay(1)).Format(pendingTimeLayout)
	if _, err := s.db.Exec(
		`INSERT INTO pending_events(agent_name, app, ref, event, payload, created_at, attempts, next_try_at)
		 VALUES($1,$2,$3,$4,$5,$6,1,$7)
		 ON CONFLICT(agent_name, app, ref) DO UPDATE SET
		     event = excluded.event, payload = excluded.payload, created_at = excluded.created_at,
		     attempts = 1, next_try_at = excluded.next_try_at`,
		agentName, app, ref, event, payload, stamp, next); err != nil {
		return err
	}
	if err := s.evictOldestPending(agentName); err != nil {
		return err
	}
	return notify(s.db, chanEvents, agentName)
}

// evictOldestPending trims agentName's parked events down to the newest
// maxPendingPerAgent. Both ParkEvent and ReparkEvent can grow the table (a
// re-park is itself an insert), so both must run this or the cap can be
// exceeded by the concurrent window where DrainFor empties the table for a
// replay batch while fresh ParkEvent calls refill it before the replay
// re-parks — see ReparkEvent's doc comment.
func (s *Store) evictOldestPending(agentName string) error {
	_, err := s.db.Exec(
		`DELETE FROM pending_events
		  WHERE agent_name = $1
		    AND id NOT IN (
		        SELECT id FROM pending_events WHERE agent_name = $2
		         ORDER BY created_at DESC LIMIT $3)`,
		agentName, agentName, maxPendingPerAgent)
	return err
}

// ReparkEvent restores a replay that failed delivery, using the event's
// ORIGINAL created_at rather than now: a re-park is not a new arrival, and
// must not win the coalescing slot by park recency when a genuinely newer
// event took the slot while the replay was in flight (DrainFor drains,
// delivers, and re-parks in three separate steps, so that window is real).
// The WHERE clause makes the overwrite conditional on the re-park still
// being the newest thing for this ref; if it lost the race, the INSERT is
// silently dropped, which is correct by design — the newer event already
// in the slot supersedes it under last-write-wins.
// attempts is the count *after* this failure, so the row comes back with its
// history intact and its next sweep pushed out by that many failures' backoff.
func (s *Store) ReparkEvent(agentName, app, ref, event string, payload []byte, createdAt string, attempts int) error {
	next := time.Now().UTC().Add(retryDelay(attempts)).Format(pendingTimeLayout)
	if _, err := s.db.Exec(
		`INSERT INTO pending_events(agent_name, app, ref, event, payload, created_at, attempts, next_try_at)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8)
		 ON CONFLICT(agent_name, app, ref) DO UPDATE SET
		     event = excluded.event, payload = excluded.payload, created_at = excluded.created_at,
		     attempts = excluded.attempts, next_try_at = excluded.next_try_at
		 WHERE excluded.created_at > pending_events.created_at`,
		agentName, app, ref, event, payload, createdAt, attempts, next); err != nil {
		return err
	}
	// A re-park is itself an insert (it can land under a ref that fresh
	// ParkEvent calls never touched), so it must enforce the same cap: a
	// batch of replays failing while fresh events refill the table would
	// otherwise leave the agent over maxPendingPerAgent until the next
	// arrival happened to trim it back down.
	return s.evictOldestPending(agentName)
}

// DrainEvents returns and removes every parked event for agentName. The read
// locks the rows it returns (FOR UPDATE SKIP LOCKED) and the delete names
// exactly those rows, so two drains of one agent — on one relay or two — split
// the events between them and never both deliver the same one: a row the
// other drain holds is skipped here and returned there. A concurrent
// ParkEvent either commits before the read and is returned, or after the
// delete and survives for the next drain.
// Events older than pendingEventTTL are deleted with the rest but never
// returned: a box gone for a week should not have a week-old push replayed at
// it, and the delete is what reclaims its capped slots.
func (s *Store) DrainEvents(agentName string) ([]PendingEvent, error) {
	return s.drainEvents(agentName, "")
}

// DrainDueEvents is DrainEvents restricted to the events whose retry backoff
// has elapsed — the sweep's view. Rows not yet due are left untouched, so a
// backing-off slot is not consumed and silently re-parked.
func (s *Store) DrainDueEvents(agentName string) ([]PendingEvent, error) {
	return s.drainEvents(agentName, time.Now().UTC().Format(pendingTimeLayout))
}

// drainEvents reads, locks, and deletes agentName's parked events in one
// transaction. dueBy empty means every event regardless of backoff.
func (s *Store) drainEvents(agentName, dueBy string) ([]PendingEvent, error) {
	where, args := `agent_name=$1`, []any{agentName}
	if dueBy != "" {
		where += ` AND next_try_at<=$2`
		args = append(args, dueBy)
	}
	expiry := time.Now().UTC().Add(-pendingEventTTL).Format(pendingTimeLayout)

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(
		`SELECT id, app, ref, event, payload, created_at, attempts FROM pending_events
		  WHERE `+where+` ORDER BY created_at FOR UPDATE SKIP LOCKED`, args...)
	if err != nil {
		return nil, err
	}
	var out []PendingEvent
	var ids []int64
	for rows.Next() {
		var id int64
		ev := PendingEvent{AgentName: agentName}
		if err := rows.Scan(&id, &ev.App, &ev.Ref, &ev.Event, &ev.Payload, &ev.CreatedAt, &ev.Attempts); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
		if ev.CreatedAt < expiry {
			continue // deleted below with the rest, but too stale to replay
		}
		out = append(out, ev)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, tx.Commit()
	}
	if _, err := tx.Exec(`DELETE FROM pending_events WHERE id = ANY($1)`, ids); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// PurgeExpiredPending drops parked events past pendingEventTTL, so a box that
// never reconnects stops holding its slots. Returns how many were removed.
func (s *Store) PurgeExpiredPending() (int64, error) {
	expiry := time.Now().UTC().Add(-pendingEventTTL).Format(pendingTimeLayout)
	res, err := s.db.Exec(`DELETE FROM pending_events WHERE created_at < $1`, expiry)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// AgentsWithDuePending lists the agents holding at least one parked event whose
// backoff has elapsed — the sweep's work list.
func (s *Store) AgentsWithDuePending() ([]string, error) {
	now := time.Now().UTC().Format(pendingTimeLayout)
	rows, err := s.db.Query(
		`SELECT DISTINCT agent_name FROM pending_events WHERE next_try_at<=$1`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// DrainFor replays every parked event for agentName. Called on reconnect, and
// after a park to close the race where the box came back between a delivery
// failing and the park landing. It bails while the agent is offline — the
// destructive drain must not run when nothing can be delivered — and a replay
// that fails is re-parked, never dropped: GitHub already got its 202, so a
// lost event here would never be retried by anyone.
func (t *TunnelDelivery) DrainFor(ctx context.Context, agentName string) {
	t.drain(ctx, agentName, false)
}

// drain replays agentName's parked events. dueOnly restricts it to the ones
// whose retry backoff has elapsed — the sweep's view. Reconnect and post-park
// drains take everything: the box coming back, or a webhook arriving for it,
// is a better signal than any timer, and both are already rate-limited by the
// event that triggered them.
func (t *TunnelDelivery) drain(ctx context.Context, agentName string, dueOnly bool) {
	if _, ok := t.router.Lookup(agentName); !ok {
		return // events stay parked for the reconnect drain
	}
	drainFn := t.st.DrainEvents
	if dueOnly {
		drainFn = t.st.DrainDueEvents
	}
	events, err := drainFn(agentName)
	if err != nil {
		log.Printf("relay: drain pending for %s: %v", agentName, err)
		return
	}
	for _, ev := range events {
		b := Binding{AgentName: ev.AgentName, App: ev.App}
		if err := t.Deliver(ctx, b, ev.Event, ev.Payload); err != nil {
			log.Printf("relay: replay %s to %s/%s (attempt %d): %v (re-parking)",
				ev.Event, ev.AgentName, ev.App, ev.Attempts+1, err)
			if perr := t.st.ReparkEvent(ev.AgentName, ev.App, ev.Ref, ev.Event, ev.Payload, ev.CreatedAt, ev.Attempts+1); perr != nil {
				log.Printf("relay: re-park %s for %s/%s: %v", ev.Event, ev.AgentName, ev.App, perr)
			}
		}
	}
}
