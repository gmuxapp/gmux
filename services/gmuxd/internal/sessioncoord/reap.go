package sessioncoord

import (
	"context"
	"fmt"
)

// ReapOrphans terminates exactly one orphan class: a runner process whose
// Meta reports a session ID that already has an installed live generation on
// a DIFFERENT endpoint. That is the lost-resume-race leftover the lifecycle
// slice documented — a last-wins Replace superseded the loser process, whose
// registration can never win without Replace provenance — so two processes
// claim one identity and the unregistered one is provably redundant.
//
// Every other unregistered process is deliberately NOT reaped: detection and
// convergence of unknown/dead/unclaimed runners belong to discovery and the
// ordinary Register path (production parity — discovery registers, never
// kills; ADR 0003: "the runner always starts"). Killing a process the daemon
// holds no durable claim about would be destructive guesswork.
//
// endpoints is the caller-enumerated probe set (socket enumeration is
// discovery's job). Per endpoint: Meta probe (I/O, no locks; failure skips),
// per-session lifecycle claim (busy skips — never race a stop/resume/
// restart), re-check under the claim, then RunnerControl.Terminate outside
// all locks. No durable write occurs: the orphan owns no row — its claimed
// identity's row belongs to the live winner. Death of the orphan is not
// waited for; it was never subscribed, so nothing observes it.
//
// Wiring constraint: the claim excludes Resume/Restart, but a bare
// Register{Replace: true} takes no claim — Replace registrations MUST go
// through claimed operations (Resume/Restart), otherwise one issued directly
// against a reaped endpoint between the re-check and Terminate could install
// a generation that is then killed.
//
// Like Reconcile, reaping is gated on the closed convergence barrier: while
// the window is open the registry is still converging and "live generation"
// is not yet trustworthy enough to kill against.
//
// Returns the endpoints that were terminated.
func (c *Coordinator) ReapOrphans(ctx context.Context, endpoints []string) ([]string, error) {
	if c.control == nil {
		return nil, ErrNoRunnerControl
	}
	c.mu.Lock()
	if !c.convergeClosed {
		c.mu.Unlock()
		return nil, fmt.Errorf("%w: reaping waits for the convergence barrier", ErrConvergencePending)
	}
	c.mu.Unlock()

	var reaped []string
	for _, endpoint := range endpoints {
		// ── probe: I/O, no locks ─────────────────────────────────────────
		meta, err := c.runners.Meta(ctx, endpoint)
		if err != nil {
			continue // unreachable/unknown: discovery's problem, not reaping's
		}
		id := meta.Registration.ID
		if id == "" {
			continue
		}
		release, err := c.claim(id, "reap")
		if err != nil {
			continue // a lifecycle op owns this session right now: skip
		}
		live, ok := c.registry.current(id)
		if !ok || live.Endpoint == endpoint {
			// No live winner (discovery converges this endpoint via
			// Register), or this IS the registered generation's endpoint.
			release()
			continue
		}
		// ── terminate: I/O, no locks, no DB transaction ──────────────────
		if err := c.control.Terminate(ctx, endpoint); err != nil {
			c.reportError(ctx, fmt.Errorf("sessioncoord: reap of %s (session %s): %w", endpoint, id, err))
			release()
			continue
		}
		reaped = append(reaped, endpoint)
		release()
	}
	return reaped, nil
}
