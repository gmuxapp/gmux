package centralstore

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore/internal/db"
)

// DismissSessionTree dismisses one launch subtree in a single transaction:
// every not-yet-dismissed member (the root and its recursive launch
// descendants) gets dismissed_at stamped, loses its project placement, and
// every affected sibling scope is rewritten to its canonical dense order.
// Dismissal is hidden-not-forgotten (ADR 0026 §6): rows, conversation
// identity, launch provenance, promotion preference, and timestamps are
// retained; only visibility and placement/order are removed.
//
// A root that is itself already dismissed still converges descendants that
// re-registered (and so became visible) since the earlier dismissal; a fully
// dismissed subtree is a silent no-op.
//
// The subtree walk happens in Go over one transaction-bound full read: the
// row counts are sidebar-scale and the placement kernel already uses the
// same whole-table-in-Go pattern, so a recursive CTE would only add a second
// walk implementation to keep consistent.
//
// Runner liveness is deliberately outside SQLite: the lifecycle coordinator
// must establish, under its serialization, that no subtree member has a live
// generation immediately before calling this conditional operation. There is
// no row-version fence — as with SweepDeadSessions, safety is the lifecycle
// mutex plus live-registry exclusion, and the per-row predicate is
// `dismissed_at IS NULL`.
func (s *Store) DismissSessionTree(ctx context.Context, root SessionID, at UnixMillis) ([]SessionID, MutationResult, error) {
	if root == "" {
		return nil, MutationResult{}, errors.New("centralstore: session id required")
	}
	if at < 0 {
		return nil, MutationResult{}, errors.New("centralstore: dismissal timestamp must be non-negative")
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return nil, MutationResult{}, err
	}
	defer tx.Rollback()
	q := s.queries.WithTx(tx)

	rows, err := q.ListSessions(ctx)
	if err != nil {
		return nil, MutationResult{}, err
	}
	byID := make(map[SessionID]Session, len(rows))
	children := make(map[SessionID][]SessionID)
	for _, r := range rows {
		v, convErr := sessionFromDB(r)
		if convErr != nil {
			return nil, MutationResult{}, convErr
		}
		byID[v.ID] = v
		if v.LaunchParentID != nil {
			children[*v.LaunchParentID] = append(children[*v.LaunchParentID], v.ID)
		}
	}
	if _, ok := byID[root]; !ok {
		return nil, MutationResult{}, ErrSessionNotFound
	}
	subtree := launchSubtree(children, root)

	toDismiss := make([]SessionID, 0, len(subtree))
	for _, id := range subtree {
		if byID[id].DismissedAt == nil {
			toDismiss = append(toDismiss, id)
		}
	}
	if len(toDismiss) == 0 {
		if err = tx.Commit(); err != nil {
			return nil, MutationResult{}, err
		}
		return nil, MutationResult{}, nil
	}

	placementsRemoved := false
	for _, id := range toDismiss {
		n, dismissErr := q.DismissSession(ctx, db.DismissSessionParams{DismissedAtMs: nullMillis(&at), ID: string(id)})
		if dismissErr != nil {
			return nil, MutationResult{}, dismissErr
		}
		if n != 1 {
			return nil, MutationResult{}, fmt.Errorf("centralstore: session %s disappeared during dismissal", id)
		}
		removed, deleteErr := q.DeleteLocalSessionPlacement(ctx, nullString(string(id)))
		if deleteErr != nil {
			return nil, MutationResult{}, deleteErr
		}
		placementsRemoved = placementsRemoved || removed > 0
	}

	normalized, err := normalizePlacements(ctx, q, s.beforePlacementFinalize)
	if err != nil {
		return nil, MutationResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return nil, MutationResult{}, err
	}
	worldDirty := placementsRemoved || normalized
	return toDismiss, MutationResult{Changed: true, SessionsDirty: true, WorldDirty: worldDirty}, nil
}

// launchSubtree returns root plus its recursive launch descendants in
// deterministic BFS order (children sorted by ID). The launch-parent cycle
// trigger guarantees the adjacency is acyclic, but the walk still guards
// against revisits so a corrupt store cannot loop.
func launchSubtree(children map[SessionID][]SessionID, root SessionID) []SessionID {
	out := []SessionID{root}
	seen := map[SessionID]bool{root: true}
	for i := 0; i < len(out); i++ {
		kids := append([]SessionID(nil), children[out[i]]...)
		sort.Slice(kids, func(a, b int) bool { return kids[a] < kids[b] })
		for _, k := range kids {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	return out
}
