package sessioncoord

import (
	"context"
	"fmt"
	"sort"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

// ErrSubtreeBusy marks a dismissal blocked because a subtree member has an
// in-flight lifecycle operation (stop/resume/restart claim). A resume
// mid-spawn would clear the dismissal moments after it committed; failing
// fast is less surprising than a dismiss-then-undismiss flicker. It wraps
// ErrLifecycleOpInFlight so errors.Is matches both and one sentinel
// suffices for UI busy-retry mapping.
var ErrSubtreeBusy = fmt.Errorf("%w: in the subtree", ErrLifecycleOpInFlight)

// Dismiss dismisses the session's entire launch subtree in one durable
// transaction (ADR 0026 §6: hidden, not forgotten). The whole operation —
// subtree read, liveness/claim/convergence checks, and the store commit —
// runs under the lifecycle mutex, so no registration (which commits under
// the same mutex) can interleave between the checks and the commit, and no
// fence window can be observed (fences are set and resolved within a single
// Register mutex hold).
//
// Dismissal is runner-boundary conservative and blocks rather than excludes:
//
//   - any subtree member with an installed live generation fails the whole
//     operation with ErrSessionAlive — every live local runner must stay
//     visible, and silently narrowing the scope the UI confirmed would be
//     worse than asking it to re-confirm;
//   - any member with an in-flight lifecycle claim fails with ErrSubtreeBusy;
//   - any exit-less member while the startup convergence window is open
//     fails with ErrConvergencePending — its liveness is unknown until a
//     surviving runner had the chance to claim it (same rule as Resume).
//
// It returns the IDs that were durably dismissed. A registration arriving
// after the commit clears that session's dismissal by design.
func (c *Coordinator) Dismiss(ctx context.Context, root centralstore.SessionID) ([]centralstore.SessionID, error) {
	if c.beforeDismissLock != nil {
		c.beforeDismissLock()
	}
	c.mu.Lock()
	sessions, err := c.durable.ListSessions(ctx)
	if err != nil {
		c.mu.Unlock()
		return nil, err
	}
	byID := make(map[centralstore.SessionID]centralstore.Session, len(sessions))
	children := make(map[centralstore.SessionID][]centralstore.SessionID)
	for _, s := range sessions {
		byID[s.ID] = s
		if s.LaunchParentID != nil {
			children[*s.LaunchParentID] = append(children[*s.LaunchParentID], s.ID)
		}
	}
	if _, ok := byID[root]; !ok {
		c.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", centralstore.ErrSessionNotFound, root)
	}
	windowOpen := c.convergeCandidates != nil && !c.convergeClosed
	members := []centralstore.SessionID{root}
	seen := map[centralstore.SessionID]bool{root: true}
	for i := 0; i < len(members); i++ {
		kids := append([]centralstore.SessionID(nil), children[members[i]]...)
		sort.Slice(kids, func(a, b int) bool { return kids[a] < kids[b] })
		for _, k := range kids {
			if !seen[k] {
				seen[k] = true
				members = append(members, k)
			}
		}
	}
	for _, id := range members {
		if op, busy := c.ops[id]; busy {
			c.mu.Unlock()
			return nil, fmt.Errorf("%w: %s (%s)", ErrSubtreeBusy, id, op)
		}
		if _, live := c.registry.current(id); live {
			c.mu.Unlock()
			return nil, fmt.Errorf("%w: subtree member %s", ErrSessionAlive, id)
		}
		if windowOpen && byID[id].ExitedAt == nil {
			c.mu.Unlock()
			return nil, fmt.Errorf("%w: subtree member %s", ErrConvergencePending, id)
		}
	}

	dismissed, result, err := c.durable.DismissSessionTree(ctx, root, c.now())
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	// Publish the committed outcome outside the mutex, same as every other
	// lifecycle operation.
	c.publish(ctx, result)
	return dismissed, nil
}

// Remove hard-deletes one retained session row (e.g. retention deciding a
// row is no longer resumable). The deletion transaction clears direct
// children's launch parents, making them genuine roots without fabricating
// the sticky promotion bit, and repairs all affected sibling orderings
// (centralstore.RemoveSessionAtVersion). The observed version makes the
// delete conditional: a row that changed since the caller's decision fails
// with ErrStaleVersion.
//
// The same runner-boundary rules as Dismiss apply to the target: no live
// generation, no in-flight lifecycle claim, and no exit-less row while the
// startup convergence window is open (its surviving runner could still
// claim the identity that deletion would forget).
func (c *Coordinator) Remove(ctx context.Context, id centralstore.SessionID, observed centralstore.RowVersion) error {
	c.mu.Lock()
	if op, busy := c.ops[id]; busy {
		c.mu.Unlock()
		return fmt.Errorf("%w (%s)", ErrLifecycleOpInFlight, op)
	}
	if _, live := c.registry.current(id); live {
		c.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrSessionAlive, id)
	}
	if c.convergeCandidates != nil && !c.convergeClosed {
		s, ok, err := c.durable.Session(ctx, id)
		if err != nil {
			c.mu.Unlock()
			return err
		}
		if ok && s.ExitedAt == nil {
			c.mu.Unlock()
			return fmt.Errorf("%w: %s", ErrConvergencePending, id)
		}
	}
	result, err := c.durable.RemoveSessionAtVersion(ctx, id, observed)
	if err == nil {
		c.invalidateVerdict(id) // the row is gone; its reconciliation verdict with it
	}
	c.mu.Unlock()
	if err != nil {
		return err
	}
	c.publish(ctx, result)
	return nil
}
