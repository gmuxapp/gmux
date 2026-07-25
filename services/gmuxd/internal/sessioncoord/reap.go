package sessioncoord

import (
	"context"
	"errors"
	"fmt"
)

// ReapOrphans terminates exactly one orphan class: a runner process whose
// Meta reports a session ID that already has an installed live generation on
// a DIFFERENT socket. "Different socket" means a different pathname, or the
// same pathname provably rebound to a different inode while the installed
// generation kept draining its own (unnamed) one. That is the
// lost-resume-race leftover the lifecycle slice documented — a last-wins Replace superseded the loser process, whose
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
// Termination goes through RunnerControl.Reap, never Terminate. A reap
// decision is made from a probe and executed later against a pathname, and
// pathnames change hands in between. Reap addresses a facility only a
// protocol-aware runner implements: such a runner compares the named
// incarnation with its own and refuses if it is not the process the decision
// was about, and an occupant that predates the protocol cannot act on the
// request at all (ErrReapUnsupported) and is left untouched. A candidate that
// cannot be named is never reaped either, for the same reason: the daemon
// would have no way to bound the blast radius of being wrong.
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

	installed := c.registry.InstalledSockets()

	var reaped []string
	for _, endpoint := range endpoints {
		// ── probe: I/O, no locks ─────────────────────────────────────────
		// Bracket the probe with the pathname's identity: probed describes
		// the socket that actually answered Meta, or is unknown when the
		// pathname moved underneath the probe.
		preProbe := socketIdent(endpoint)
		if _, isInstalled := installed[preProbe]; isInstalled && preProbe.Known() {
			// This pathname names a socket an installed generation is
			// subscribed to. It cannot be an orphan, so skip the probe
			// entirely: an unchanged fleet must cost no runner I/O per tick.
			continue
		}
		meta, err := c.runners.Meta(ctx, endpoint)
		if err != nil {
			continue // unreachable/unknown: discovery's problem, not reaping's
		}
		probed := settledIdent(preProbe, endpoint)
		id := meta.Registration.ID
		if id == "" {
			continue
		}
		if meta.Incarnation == "" {
			// Unidentifiable candidate: a runner from before the protocol, or
			// one whose transport dropped the field. Killing it would mean
			// addressing a pathname and hoping, which is the entire failure
			// mode this guard exists to remove.
			continue
		}
		_, release, err := c.claim(id, "reap")
		if err != nil {
			continue // a lifecycle op owns this session right now: skip
		}
		live, ok := c.registry.current(id)
		if !ok {
			// No live winner: discovery converges this endpoint via Register.
			release()
			continue
		}
		if live.Endpoint == endpoint {
			// Same pathname. Only a provably different socket is an orphan:
			// if either identity is unknown, or they match, this either IS
			// the installed generation or cannot be shown not to be — and
			// this branch ends in killing a process, so unproven means no.
			if !probed.Known() || !live.Socket.Known() || probed.Same(live.Socket) {
				release()
				continue
			}
			// A distinct process rebound the pathname while the installed
			// generation is still draining its own socket. Fall through.
		}
		// ── reap: I/O, no locks, no DB transaction ───────────────────────
		// Conditional on identity, and on a facility only a protocol-aware
		// runner has. A replacement that took the pathname over declines (or,
		// if it predates the protocol, cannot act at all) and stays alive; the
		// next sweep classifies whoever is there on its own merits.
		//
		// Re-stat'ing the pathname here instead would only move the same
		// check-then-act one instruction closer to the kill.
		err = c.control.Reap(ctx, endpoint, meta.Incarnation)
		switch {
		case errors.Is(err, ErrReapUnsupported):
			// The occupant predates conditional reaping and was left alone.
			// Not an error: discovery converges it the ordinary way.
			release()
			continue
		case err != nil:
			c.reportError(ctx, fmt.Errorf("sessioncoord: reap of %s (session %s): %w", endpoint, id, err))
			release()
			continue
		}
		reaped = append(reaped, endpoint)
		release()
	}
	return reaped, nil
}
