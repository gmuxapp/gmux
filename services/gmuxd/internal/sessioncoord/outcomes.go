package sessioncoord

import (
	"context"
	"fmt"
	"sync"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

// OutcomeType classifies one post-commit outcome (design §2.1).
type OutcomeType int

const (
	// OutcomeUpserted carries the committed durable row after any mutation
	// that left the row in place (registration, observation, exit repair,
	// acknowledge, sweep, dismissal).
	OutcomeUpserted OutcomeType = iota
	// OutcomeRemoved marks a row that no longer exists (hard deletion,
	// reconcile removal, takeover eviction).
	OutcomeRemoved
	// OutcomeActivity is the transient session-activity signal. It is never
	// durable and — alone among outcome types — lossy under backlog,
	// preserving production `session-activity` semantics.
	OutcomeActivity
)

// Outcome is one committed domain outcome with registry liveness stamped at
// publish time (ADR 0026 §9: runtime effects remain explicit consumers of
// committed domain outcomes; liveness is runtime-only and rides the outcome,
// never a row column).
type Outcome struct {
	Type       OutcomeType
	ID         centralstore.SessionID
	Session    *centralstore.Session // committed row for Upserted; nil otherwise
	Alive      bool                  // registry liveness at publish time
	Generation uint64                // 0 when not alive
}

// outcomeActivityBacklog bounds how many undelivered outcomes a subscriber
// may accumulate before incoming Activity outcomes are dropped. Upserted and
// Removed outcomes are never dropped (lossless), so the queue is unbounded
// for them — consumers are in-process and the store is sidebar-scale.
const outcomeActivityBacklog = 256

type outcomeSub struct {
	mu     sync.Mutex
	queue  []Outcome
	signal chan struct{} // 1-buffered wakeup for the pump
	done   chan struct{} // closed by unsubscribe
	ch     chan Outcome
	// seen is the per-session version watermark enforcing monotone Upserted
	// delivery (review H-1): publishes happen outside the lifecycle mutex,
	// so commit order and publish order can diverge — without the watermark
	// a subscriber's FINAL outcome for a session could be a stale row (e.g.
	// Register blocked in the dirty sink while a newer apply commits and
	// publishes first). Upserted outcomes carry the committed Session.Version;
	// any non-monotone Upserted is dropped at enqueue. Removed is never
	// version-gated and resets the watermark, because a post-removal
	// re-registration starts a fresh version sequence at 1.
	seen map[centralstore.SessionID]centralstore.RowVersion
}

type outcomeBus struct {
	mu   sync.Mutex
	subs map[*outcomeSub]struct{}
}

func (b *outcomeBus) hasSubscribers() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs) > 0
}

// publish appends the outcome to every subscriber queue. It never blocks:
// delivery runs on each subscriber's pump goroutine. Ordering is preserved
// per subscriber (single bus lock section per publish), and Upserted
// delivery is version-monotone per session (see outcomeSub.seen): a racing
// older commit published later is dropped, so the subscriber's final state
// for a session is always the newest delivered row.
func (b *outcomeBus) publish(o Outcome) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for sub := range b.subs {
		sub.mu.Lock()
		switch o.Type {
		case OutcomeActivity:
			if len(sub.queue) >= outcomeActivityBacklog {
				sub.mu.Unlock()
				continue // lossy by contract; Upserted/Removed always enqueue
			}
		case OutcomeUpserted:
			if o.Session != nil {
				if last, ok := sub.seen[o.ID]; ok && o.Session.Version <= last {
					sub.mu.Unlock()
					continue // stale: a newer row was already enqueued (H-1)
				}
				if sub.seen == nil {
					sub.seen = make(map[centralstore.SessionID]centralstore.RowVersion)
				}
				sub.seen[o.ID] = o.Session.Version
			}
		case OutcomeRemoved:
			// Never dropped by an older version; reset the watermark so a
			// post-removal re-registration's fresh version sequence (starting
			// over at 1) is not shadowed by the removed row's versions.
			delete(sub.seen, o.ID)
		}
		sub.queue = append(sub.queue, o)
		sub.mu.Unlock()
		select {
		case sub.signal <- struct{}{}:
		default:
		}
	}
}

// SubscribeOutcomes registers a post-commit outcome consumer. The returned
// channel delivers outcomes in publish order; Upserted/Removed are lossless
// (a slow consumer delays only itself), Activity is dropped under backlog.
// The cancel function must be called exactly once; it closes the channel
// after the pump stops. Subscribers see only outcomes published after
// subscription — initial state comes from a durable read plus a registry
// snapshot, never from replay (design §2.1 startup seeding).
func (c *Coordinator) SubscribeOutcomes() (<-chan Outcome, func()) {
	sub := newOutcomeSub()
	c.outcomes.mu.Lock()
	c.installOutcomeSubLocked(sub)
	c.outcomes.mu.Unlock()
	return startOutcomeSub(c, sub)
}

func newOutcomeSub() *outcomeSub {
	return &outcomeSub{signal: make(chan struct{}, 1), done: make(chan struct{}), ch: make(chan Outcome), seen: make(map[centralstore.SessionID]centralstore.RowVersion)}
}

func (c *Coordinator) installOutcomeSubLocked(sub *outcomeSub) {
	if c.outcomes.subs == nil {
		c.outcomes.subs = make(map[*outcomeSub]struct{})
	}
	c.outcomes.subs[sub] = struct{}{}
}

func startOutcomeSub(c *Coordinator, sub *outcomeSub) (<-chan Outcome, func()) {
	go func() {
		defer close(sub.ch)
		for {
			select {
			case <-sub.done:
				return
			case <-sub.signal:
			}
			for {
				sub.mu.Lock()
				if len(sub.queue) == 0 {
					sub.mu.Unlock()
					break
				}
				next := sub.queue[0]
				sub.queue = sub.queue[1:]
				sub.mu.Unlock()
				select {
				case sub.ch <- next:
				case <-sub.done:
					return
				}
			}
		}
	}()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			c.outcomes.mu.Lock()
			delete(c.outcomes.subs, sub)
			c.outcomes.mu.Unlock()
			close(sub.done)
		})
	}
	return sub.ch, cancel
}

// SubscribeOutcomesSeed installs a subscription and takes its durable/runtime
// seed under one lifecycle/publication fence. Commits cannot cross the seed,
// and publishers for already-reflected commits are deduplicated by the
// subscriber's row-version watermark. Consumers apply Seed first, then Events.
func (c *Coordinator) SubscribeOutcomesSeed(ctx context.Context) (seed []Outcome, events <-chan Outcome, cancel func(), err error) {
	sub := newOutcomeSub()
	c.mu.Lock()
	c.outcomes.mu.Lock()
	c.installOutcomeSubLocked(sub)
	rows, readErr := c.durable.ListSessions(ctx)
	if readErr == nil {
		seed = make([]Outcome, 0, len(rows))
		for i := range rows {
			row := rows[i]
			alive, generation := c.livenessOf(row.ID)
			sub.seen[row.ID] = row.Version
			seed = append(seed, Outcome{Type: OutcomeUpserted, ID: row.ID, Session: &row, Alive: alive, Generation: generation})
		}
	}
	c.outcomes.mu.Unlock()
	c.mu.Unlock()
	if readErr != nil {
		c.outcomes.mu.Lock()
		delete(c.outcomes.subs, sub)
		c.outcomes.mu.Unlock()
		return nil, nil, nil, readErr
	}
	events, cancel = startOutcomeSub(c, sub)
	return seed, events, cancel, nil
}

// PublishActivity forwards one transient session-activity signal onto the
// outcome bus, liveness-stamped. Production wiring calls it from the runner
// event transport; activity is never durable (design §2.1).
func (c *Coordinator) PublishActivity(id centralstore.SessionID) {
	if !c.outcomes.hasSubscribers() {
		return
	}
	alive, generation := c.livenessOf(id)
	c.outcomes.publish(Outcome{Type: OutcomeActivity, ID: id, Alive: alive, Generation: generation})
}

func (c *Coordinator) livenessOf(id centralstore.SessionID) (bool, uint64) {
	if e, ok := c.registry.current(id); ok {
		return true, e.Generation
	}
	return false, 0
}

// emitUpserted publishes an Upserted outcome for a row the caller already
// holds (the committed registration row). Callers must not hold the
// lifecycle mutex. Liveness is stamped at publish time (design M-3); the
// row may be older than the stamped world when a newer commit raced this
// publish, but then that newer commit's own outcome either already set the
// watermark (this one is dropped) or is still queued behind it (delivered
// after) — the subscriber's final state is the newest row either way
// (review M-2 rides the H-1 watermark).
func (c *Coordinator) emitUpserted(session centralstore.Session) {
	if !c.outcomes.hasSubscribers() {
		return
	}
	alive, generation := c.livenessOf(session.ID)
	s := session
	c.outcomes.publish(Outcome{Type: OutcomeUpserted, ID: session.ID, Session: &s, Alive: alive, Generation: generation})
}

// emitOutcomes publishes one outcome per ID after a commit: a post-commit
// row read decides Upserted (row present, committed state attached) versus
// Removed (row absent). The read races later commits by design — a newer
// row is safe to deliver, and the per-subscriber version watermark drops
// any older row published late, so delivery is monotone per session even
// though publishes run outside the lifecycle mutex. Callers must not hold
// the lifecycle mutex (the read is a short DB transaction; publish never
// blocks). Read failures are reported and the outcome is skipped; consumers
// converge on the next outcome for that row.
//
// Known residual (documented, not fixable locally with per-ID watermarks;
// it exists in BOTH directions across a removal boundary, fable delta
// review R-2):
//
//   - Removed then stale Upserted: a Removed outcome followed by a stale
//     captured-row Upserted for the SAME pre-removal generation (reachable
//     via a fast-dead Register racing a Remove) can deliver the stale row
//     after the watermark reset. Production consumers treat rows with exit
//     facts as dead either way.
//   - Upserted then late Removed: the durable read sites CAN produce the
//     inverse — a Remove commits, its post-commit read observes absence,
//     and before that Removed publishes, a re-registration commits and
//     publishes its fresh Upserted; the late Removed (never version-gated,
//     and it resets the watermark) then lands last, leaving a ghost
//     removal for a live session. A live session self-heals on its next
//     runner event; only a fast-dead re-registration immediately after a
//     Remove can leave the ghost as final state.
//
// Consumers keyed on Removed must therefore tolerate a row reappearing via
// the next Upserted (S5 consumer-wiring checklist). A real fix needs
// commit-ordered sequence stamping.
func (c *Coordinator) emitOutcomes(ctx context.Context, ids ...centralstore.SessionID) {
	if len(ids) == 0 || !c.outcomes.hasSubscribers() {
		return
	}
	for _, id := range ids {
		s, ok, err := c.durable.Session(ctx, id)
		if err != nil {
			c.reportError(ctx, fmt.Errorf("sessioncoord: outcome read for %s: %w", id, err))
			continue
		}
		alive, generation := c.livenessOf(id)
		if !ok {
			c.outcomes.publish(Outcome{Type: OutcomeRemoved, ID: id, Alive: alive, Generation: generation})
			continue
		}
		row := s
		c.outcomes.publish(Outcome{Type: OutcomeUpserted, ID: id, Session: &row, Alive: alive, Generation: generation})
	}
}
