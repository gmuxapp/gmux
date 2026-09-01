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

// RecoveryState is the restart recovery projection derived from the durable
// rows that were alive at bind time and the runners currently installed in the
// runtime registry. The existing convergence close is the bounded terminal
// transition: FinishConvergence marks every unclaimed row dead before Status
// becomes ready.
type RecoveryState struct {
	Status    string `json:"status"`
	Expected  int    `json:"expected"`
	Recovered int    `json:"recovered"`
}

func (c *Coordinator) RecoveryState() RecoveryState {
	c.mu.Lock()
	defer c.mu.Unlock()
	status := "ready"
	// With no previously-alive local rows there is nothing to recover. Runner
	// endpoint discovery may still be in flight (for brand-new runners), but it
	// must not make an empty daemon advertise a recovery phase or flap readiness.
	if !c.convergeClosed && len(c.convergeCandidates) > 0 {
		status = "recovering"
	}
	recovered := 0
	for id := range c.convergeCandidates {
		if _, live := c.registry.current(id); live {
			recovered++
		}
	}
	return RecoveryState{Status: status, Expected: len(c.convergeCandidates), Recovered: recovered}
}

// Converged is the startup convergence barrier-completion signal. The
// channel closes when FinishConvergence has durably swept unclaimed rows;
// future production wiring gates first-snapshot SSE serving on it. It is
// safe to call before BeginConvergence.
func (c *Coordinator) Converged() <-chan struct{} { return c.converged }
