package sessioncoord

import (
	"context"
	"errors"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

var (
	ErrConvergenceOpen    = errors.New("sessioncoord: convergence window already open")
	ErrConvergenceClosed  = errors.New("sessioncoord: convergence window already closed")
	ErrConvergenceNotOpen = errors.New("sessioncoord: convergence window not open")
)

// BeginConvergence opens the startup convergence window. It records the
// durable rows that were recorded alive (no exited timestamp) when the
// daemon last ran. While the window is open those rows are "liveness
// unknown": runners re-registering through the ordinary Register path
// converge them, and everything else waits for FinishConvergence.
//
// It may be called exactly once, before any deadline-driven sweep.
func (c *Coordinator) BeginConvergence(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.convergeClosed {
		return ErrConvergenceClosed
	}
	if c.convergeCandidates != nil {
		return ErrConvergenceOpen
	}
	sessions, err := c.durable.ListSessions(ctx)
	if err != nil {
		return err
	}
	candidates := make(map[centralstore.SessionID]struct{})
	for _, s := range sessions {
		if s.ExitedAt == nil {
			candidates[s.ID] = struct{}{}
		}
	}
	c.convergeCandidates = candidates
	return nil
}

// FinishConvergence closes the window: every previously-alive row whose
// runner did not come back is marked dead in one durable sweep transaction
// with one invalidation.
//
// Two mechanisms make the sweep safe against concurrent registration:
//
//  1. The lifecycle mutex is held for the whole close, so no registration
//     can commit between the live-registry exclusion below and the sweep
//     transaction.
//  2. Every candidate whose session currently has an installed live
//     generation is excluded, and the store additionally skips any row
//     whose exit was durably recorded meanwhile (`exited_at IS NULL`
//     predicate).
//
// There is deliberately no row-version fence: version churn during the
// window does not resolve liveness (dead-row acknowledgement bumps the
// version without an exit; a re-registration with changed facts bumps it
// and the runner can still vanish stream-first without exit facts), and
// fencing on it would leave such rows alive-looking forever after this
// one-shot barrier.
//
// On success the barrier-completion signal (Converged) fires; snapshot
// serving readiness can be gated on it. On sweep failure the window stays
// open and the call may be retried.
func (c *Coordinator) FinishConvergence(ctx context.Context, at centralstore.UnixMillis) (centralstore.MutationResult, error) {
	c.mu.Lock()
	if c.convergeClosed {
		c.mu.Unlock()
		return centralstore.MutationResult{}, ErrConvergenceClosed
	}
	if c.convergeCandidates == nil {
		c.mu.Unlock()
		return centralstore.MutationResult{}, ErrConvergenceNotOpen
	}
	sweep := make([]centralstore.SessionID, 0, len(c.convergeCandidates))
	for id := range c.convergeCandidates {
		if _, live := c.registry.current(id); live {
			continue // converged: a live generation claimed this row
		}
		sweep = append(sweep, id)
	}
	result, err := c.durable.SweepDeadSessions(ctx, sweep, at)
	if err != nil {
		c.mu.Unlock()
		return centralstore.MutationResult{}, err
	}
	c.convergeClosed = true
	// Decide the degraded set HERE, once, and never re-derive it from live
	// state afterwards: it is the set of candidates this pass swept dead
	// without ever getting an answer about them (noted abandoned AND not
	// claimed by a live generation at close). Deriving "degraded" from a live
	// registry count instead would make an ordinary session exit — months of
	// uptime later, on a perfectly healthy daemon — look like a recovery
	// failure, because the candidate set is immutable startup provenance while
	// the registry is not.
	//
	// From here the set only shrinks (recoveryInstalledLocked), so the status
	// machine is monotone by construction: degraded → ready is one-way.
	//
	// Intersecting with `sweep` rather than taking the noted set wholesale is
	// load-bearing, not defensive: one session id can be reached through two
	// endpoints (a legacy socket directory still holds a stale pathname while
	// the runner serves the current one). The stale probe times out and notes
	// the id as abandoned, the live probe registers it — both before this
	// close. Such an id is not in `sweep`: it was recovered, nothing was
	// durably marked dead on no evidence, and the promotion hook has already
	// fired, so admitting it here would freeze the daemon in degraded for its
	// whole lifetime.
	if len(c.convergeAbandonedIDs) > 0 {
		for _, id := range sweep {
			if _, noted := c.convergeAbandonedIDs[id]; !noted {
				continue
			}
			if c.convergeUnresolved == nil {
				c.convergeUnresolved = make(map[centralstore.SessionID]struct{})
			}
			c.convergeUnresolved[id] = struct{}{}
		}
	}
	c.convergeAbandonedIDs = nil
	// Keep the candidate set as immutable startup provenance. Lifecycle code
	// keys window openness on convergeClosed, while health can continue to
	// derive how many of the runners expected at daemon replacement are
	// currently installed without maintaining a second counter.
	close(c.converged)
	seq := c.outcomes.allocSeq() // stamp before releasing c.mu
	c.mu.Unlock()

	c.publish(ctx, result)
	if result.Changed {
		c.emitOutcomes(ctx, seq, sweep...)
	}
	return result, nil
}

// NoteAbandonedRecovery records the recovery candidates the startup
// convergence pass gave up on for a transient, retryable reason: it asked the
// runner and never got an answer inside the budget. It is the one fact the
// coordinator cannot derive — only the pass knows the difference between
// "this runner is provably gone" (nothing listening, or a permanent protocol
// verdict) and "I ran out of time asking".
//
// Identity is the caller's attribution and is deliberately advisory: ids that
// are not recovery candidates are ignored, and a mis-attributed id can only
// mis-report a diagnostic — no lifecycle decision reads this set.
//
// It must be called before FinishConvergence, which freezes the resulting
// degraded set; notes arriving after the window closed are ignored, so the
// status machine can never move backwards.
func (c *Coordinator) NoteAbandonedRecovery(ids ...centralstore.SessionID) {
	if len(ids) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.convergeClosed {
		return
	}
	for _, id := range ids {
		if _, candidate := c.convergeCandidates[id]; !candidate {
			continue // not a recovery candidate: irrelevant to recovery health
		}
		if c.convergeAbandonedIDs == nil {
			c.convergeAbandonedIDs = make(map[centralstore.SessionID]struct{})
		}
		c.convergeAbandonedIDs[id] = struct{}{}
	}
}

// recoveryInstalledLocked is the one-way promotion hook: an abandoned
// candidate that comes back (the periodic discovery pass reaches its runner)
// leaves the degraded set for good. Caller must hold c.mu.
func (c *Coordinator) recoveryInstalledLocked(id centralstore.SessionID) {
	if len(c.convergeUnresolved) == 0 {
		return
	}
	delete(c.convergeUnresolved, id)
}

// RecoveryState is the restart recovery projection derived from the durable
// rows that were alive at bind time and the runners currently installed in the
// runtime registry. The existing convergence close is the bounded terminal
// transition: FinishConvergence marks every unclaimed row dead before Status
// leaves recovering.
//
// Status is a three-state, monotone machine. It never revisits an earlier
// state, so readiness cannot flap:
//
//	recovering — the convergence window is open and there is something to
//	             recover. Bounded: the window is closed by the one-shot
//	             FinishConvergence sweep, so this state cannot persist.
//	degraded   — the window is closed, and it swept candidates dead that the
//	             pass had abandoned transiently: it never got an answer about
//	             them. Recovery is finished but NOT complete. A consumer must
//	             treat it as terminal-and-unhealthy (fail an install gate,
//	             alert) rather than wait for it to clear. Exactly one thing
//	             promotes it to ready: the abandoned runner registering ALIVE
//	             again (the periodic discovery pass). A runner that answers
//	             dead, or never answers again, therefore leaves the daemon
//	             degraded until it is restarted — which is the honest report,
//	             because a session was durably marked dead on no evidence and
//	             a later death is not evidence about the sweep that guessed
//	             it.
//	ready      — recovery is complete or definitively finished: every expected
//	             runner is installed, or nothing was expected (an empty
//	             candidate set reports ready 0/0 immediately), or every
//	             candidate was resolved — recovered, or provably gone (nothing
//	             listening, or a permanent protocol verdict).
//
// Expected and Recovered are live counts and therefore informational: an
// ordinary session exit lowers Recovered long after recovery finished. The
// status is deliberately NOT derived from them (that is what made an exit
// look like a recovery failure); it is decided once at window close and only
// ever promoted.
type RecoveryState struct {
	Status    string `json:"status"`
	Expected  int    `json:"expected"`
	Recovered int    `json:"recovered"`
}

func (c *Coordinator) RecoveryState() RecoveryState {
	c.mu.Lock()
	defer c.mu.Unlock()
	recovered := 0
	for id := range c.convergeCandidates {
		if _, live := c.registry.current(id); live {
			recovered++
		}
	}
	expected := len(c.convergeCandidates)
	status := "ready"
	switch {
	// With no previously-alive local rows there is nothing to recover. Runner
	// endpoint discovery may still be in flight (for brand-new runners), but it
	// must not make an empty daemon advertise a recovery phase or flap readiness.
	case !c.convergeClosed && expected > 0:
		status = "recovering"
	// Frozen at window close and only ever shrunk by recoveryInstalledLocked;
	// never recomputed from the live registry.
	case len(c.convergeUnresolved) > 0:
		status = "degraded"
	}
	return RecoveryState{Status: status, Expected: expected, Recovered: recovered}
}

// Converged is the startup convergence barrier-completion signal. The
// channel closes when FinishConvergence has durably swept unclaimed rows;
// future production wiring gates first-snapshot SSE serving on it. It is
// safe to call before BeginConvergence.
func (c *Coordinator) Converged() <-chan struct{} { return c.converged }
