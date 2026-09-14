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
		if v.ParentSessionID != nil {
			children[*v.ParentSessionID] = append(children[*v.ParentSessionID], v.ID)
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

	_, result, err := dismissRows(ctx, q, s.beforePlacementFinalize, toDismiss, at, true)
	if err != nil {
		return nil, MutationResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return nil, MutationResult{}, err
	}
	return toDismiss, result, nil
}

// DismissSessions dismisses an explicit set of rows in one transaction with
// the same per-row semantics as DismissSessionTree (dismissed_at stamped,
// placement removed, sibling scopes re-normalized; hidden-not-forgotten).
// Unlike DismissSessionTree it does NOT walk descendants: the caller names
// exactly the rows to hide. It backs the retention auto-dismiss sweep, whose
// candidate rule is "dead ∧ read ∧ idle > W ∧ every descendant already
// dismissed or also a candidate" — i.e. the caller has already ensured that
// no visible row is left under a hidden ancestor.
//
// Rows already dismissed, or that disappeared meanwhile, are silently
// skipped (`dismissed_at IS NULL` predicate). Liveness exclusion is the
// lifecycle coordinator's obligation, exactly as for DismissSessionTree and
// SweepDeadSessions.
func (s *Store) DismissSessions(ctx context.Context, ids []SessionID, at UnixMillis) ([]SessionID, MutationResult, error) {
	if at < 0 {
		return nil, MutationResult{}, errors.New("centralstore: dismissal timestamp must be non-negative")
	}
	for _, id := range ids {
		if id == "" {
			return nil, MutationResult{}, errors.New("centralstore: session id required")
		}
	}
	if len(ids) == 0 {
		return nil, MutationResult{}, nil
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return nil, MutationResult{}, err
	}
	defer tx.Rollback()
	q := s.queries.WithTx(tx)

	dismissed, result, err := dismissRows(ctx, q, s.beforePlacementFinalize, ids, at, false)
	if err != nil {
		return nil, MutationResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return nil, MutationResult{}, err
	}
	return dismissed, result, nil
}

// dismissRows stamps dismissed_at on rows, drops their placements and
// re-normalizes sibling scopes; the caller commits. With strict set, a row
// that is no longer visible is an error (the subtree caller just read it in
// this transaction); otherwise it is skipped (the sweep caller selected it
// outside this transaction). Returns the rows actually stamped.
func dismissRows(ctx context.Context, q *db.Queries, fault func() error, ids []SessionID, at UnixMillis, strict bool) ([]SessionID, MutationResult, error) {
	dismissed := make([]SessionID, 0, len(ids))
	placementsRemoved := false
	for _, id := range ids {
		n, dismissErr := q.DismissSession(ctx, db.DismissSessionParams{DismissedAtMs: nullMillis(&at), ID: string(id)})
		if dismissErr != nil {
			return nil, MutationResult{}, dismissErr
		}
		if n != 1 {
			if strict {
				return nil, MutationResult{}, fmt.Errorf("centralstore: session %s disappeared during dismissal", id)
			}
			continue // already dismissed or gone: not this sweep's row any more
		}
		dismissed = append(dismissed, id)
		removed, deleteErr := q.DeleteLocalSessionPlacement(ctx, nullString(string(id)))
		if deleteErr != nil {
			return nil, MutationResult{}, deleteErr
		}
		placementsRemoved = placementsRemoved || removed > 0
	}
	if len(dismissed) == 0 {
		return nil, MutationResult{}, nil
	}
	normalized, err := normalizePlacements(ctx, q, fault)
	if err != nil {
		return nil, MutationResult{}, err
	}
	return dismissed, MutationResult{Changed: true, SessionsDirty: true, WorldDirty: placementsRemoved || normalized}, nil
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
