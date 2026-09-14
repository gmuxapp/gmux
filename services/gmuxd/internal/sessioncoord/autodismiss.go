package sessioncoord

import (
	"context"
	"sort"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

// AutoDismissPolicy is the retention policy the daemon applies to its OWN
// dead sessions, expressed as dismissal (ADR 0026 §6: hidden, not
// forgotten) rather than deletion. A dismissed row keeps its SQLite row,
// conversation identity, launch provenance, scrollback and resumability; it
// just stops riding the wire and the sidebar. The old sessionmeta.Sweep
// retention (ADR 0016) deleted whole session dirs and was silently lost
// when ADR 0026 moved authority to SQLite; this replaces it with a policy
// that cannot destroy anything.
type AutoDismissPolicy struct {
	// MaxIdle hides dead rows whose last activity (falling back to exit,
	// then creation) is older than this. Zero disables the sweep.
	MaxIdle time.Duration
	// IncludeUnread lets the sweep hide rows that still carry the unread
	// attention marker. Off by default: an unread row is a pending
	// notification, and hiding it silently is a product decision, not a
	// housekeeping one.
	IncludeUnread bool
	// DryRun computes and reports the candidate set without committing.
	DryRun bool
}

// AutoDismissReport is what one sweep decided.
type AutoDismissReport struct {
	// Candidates are the rows the policy selected (in deterministic order).
	Candidates []centralstore.SessionID
	// Dismissed are the rows actually stamped (empty on dry run or when
	// nothing qualified).
	Dismissed []centralstore.SessionID
	// Visible is the number of non-dismissed local rows examined.
	Visible int
	// KeptUnread counts rows that would have qualified by age but were
	// spared because they are unread and the policy keeps unread rows.
	KeptUnread int
	// KeptAncestors counts rows that qualified on their own but were spared
	// because a descendant stays visible (never a visible child under a
	// hidden parent).
	KeptAncestors int
}

// AutoDismiss runs one retention sweep under the lifecycle mutex.
//
// A row is a candidate iff ALL of:
//
//   - it is not already dismissed;
//   - it carries an exit fact (ExitedAt != nil) — an exit-less row is either
//     alive or liveness-unknown (startup convergence window), and neither
//     may be hidden;
//   - no live runner generation is installed for it (registry), it has no
//     in-flight lifecycle claim, and no active-subagent launch reservation
//     names it;
//   - it is read, or the policy includes unread rows;
//   - its retention timestamp (LastActivityAt → ExitedAt → CreatedAt) is
//     older than now − MaxIdle;
//   - every launch descendant is already dismissed or is itself a
//     candidate. This keeps the sweep subtree-closed: the sidebar never
//     shows an orphaned child whose parent silently vanished, and a family
//     with one recent or unread member keeps its whole ancestor chain.
//
// Peer-owned rows are never local rows, so the sweep is owner-only by
// construction: each host applies its own policy to its own store. Alive
// rows can never qualify (they have no exit fact).
func (c *Coordinator) AutoDismiss(ctx context.Context, policy AutoDismissPolicy) (AutoDismissReport, error) {
	var report AutoDismissReport
	if policy.MaxIdle <= 0 {
		return report, nil
	}
	c.mu.Lock()
	sessions, err := c.durable.ListSessions(ctx)
	if err != nil {
		c.mu.Unlock()
		return report, err
	}
	now := c.now()
	cutoff := int64(now) - policy.MaxIdle.Milliseconds()

	byID := make(map[centralstore.SessionID]centralstore.Session, len(sessions))
	children := make(map[centralstore.SessionID][]centralstore.SessionID)
	for _, s := range sessions {
		byID[s.ID] = s
		if s.ParentSessionID != nil {
			children[*s.ParentSessionID] = append(children[*s.ParentSessionID], s.ID)
		}
	}

	// Pass 1: per-row eligibility, ignoring family structure.
	eligible := make(map[centralstore.SessionID]bool, len(sessions))
	for _, s := range sessions {
		if s.DismissedAt != nil {
			continue
		}
		report.Visible++
		if s.ExitedAt == nil {
			continue // alive, or liveness unknown during convergence — never hidden
		}
		if _, live := c.registry.current(s.ID); live {
			continue
		}
		if _, busy := c.ops[s.ID]; busy {
			continue
		}
		if int64(retentionStamp(s)) >= cutoff {
			continue
		}
		if s.Unread && !policy.IncludeUnread {
			report.KeptUnread++
			continue
		}
		eligible[s.ID] = true
	}
	if c.activeSubagents != nil {
		for id := range eligible {
			if c.activeSubagents.hasLaunchFrom(map[centralstore.SessionID]bool{id: true}) {
				delete(eligible, id)
			}
		}
	}

	// Pass 2: subtree closure. A row stays visible if any descendant stays
	// visible. Memoized post-order over the launch forest.
	closed := make(map[centralstore.SessionID]bool, len(eligible))
	var subtreeHidden func(id centralstore.SessionID, seen map[centralstore.SessionID]bool) bool
	subtreeHidden = func(id centralstore.SessionID, seen map[centralstore.SessionID]bool) bool {
		if v, ok := closed[id]; ok {
			return v
		}
		if seen[id] {
			return false // corrupt cycle: keep everything visible
		}
		seen[id] = true
		s := byID[id]
		hidden := s.DismissedAt != nil || eligible[id]
		if hidden {
			for _, k := range children[id] {
				if !subtreeHidden(k, seen) {
					hidden = false
					break
				}
			}
		}
		closed[id] = hidden
		return hidden
	}
	for id := range eligible {
		if !subtreeHidden(id, map[centralstore.SessionID]bool{}) {
			report.KeptAncestors++
			continue
		}
		report.Candidates = append(report.Candidates, id)
	}
	sort.Slice(report.Candidates, func(a, b int) bool { return report.Candidates[a] < report.Candidates[b] })

	if policy.DryRun || len(report.Candidates) == 0 {
		c.mu.Unlock()
		return report, nil
	}
	dismissed, result, err := c.durable.DismissSessions(ctx, report.Candidates, now)
	seq := c.outcomes.allocSeq()
	c.mu.Unlock()
	if err != nil {
		return report, err
	}
	report.Dismissed = dismissed
	c.publish(ctx, result)
	c.emitOutcomes(ctx, seq, dismissed...)
	return report, nil
}

// retentionStamp ranks a dead row for retention: last activity, else exit,
// else creation. Mirrors sessionmeta.effectiveTime's preference order but
// leads with activity because dead-row acknowledgement and unread flips
// bump LastActivityAt and "touched recently" is what the sidebar sorts by.
func retentionStamp(s centralstore.Session) centralstore.UnixMillis {
	if s.LastActivityAt != nil {
		return *s.LastActivityAt
	}
	if s.ExitedAt != nil {
		return *s.ExitedAt
	}
	return s.CreatedAt
}
