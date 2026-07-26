package centralstore

import (
	"context"
	"database/sql"
	"errors"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/projectmatch"
)

// PlaceUnplacedSessions places the given local sessions that are currently
// UNPLACED and match an owned catalog entry, with registration-placement
// semantics: same shared matcher (matchCatalog), canonical append at the
// bottom of the derived sibling scope, affected scopes densified. It is the
// coordinator's live auto-assign pass after a catalog change (design §4
// checklist item 1): ReplaceProjectCatalogAndRematch deliberately rematches
// previously PLACED subjects only, because liveness is not represented in
// SQLite — the coordinator proves liveness and supplies the candidate IDs.
//
// Per-candidate skips are silent by design: an unknown ID (row removed since
// the caller's registry snapshot), an already-placed session, a dismissed
// row, and a session matching no owned project all leave the store
// untouched — never an error, exactly like registration-time non-placement.
func (s *Store) PlaceUnplacedSessions(ctx context.Context, ids []SessionID, at UnixMillis) (MutationResult, error) {
	if at < 0 {
		return MutationResult{}, errors.New("centralstore: placement timestamp must be non-negative")
	}
	if len(ids) == 0 {
		return MutationResult{}, nil
	}

	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return MutationResult{}, err
	}
	defer tx.Rollback()
	q := s.queries.WithTx(tx)

	catalog, err := catalogFromQueries(ctx, q)
	if err != nil {
		return MutationResult{}, err
	}
	all, err := placements(ctx, q)
	if err != nil {
		return MutationResult{}, err
	}
	placed := make(map[string]bool, len(all))
	for _, r := range all {
		if r.local != "" {
			placed[r.local] = true
		}
	}

	appended := false
	for _, id := range ids {
		if id == "" || placed[string(id)] {
			continue
		}
		raw, rowErr := q.GetSession(ctx, string(id))
		if errors.Is(rowErr, sql.ErrNoRows) {
			continue // row vanished since the caller's snapshot
		}
		if rowErr != nil {
			return MutationResult{}, rowErr
		}
		sess, convErr := sessionFromDB(raw)
		if convErr != nil {
			return MutationResult{}, convErr
		}
		if sess.DismissedAt != nil {
			continue // hidden rows stay unplaced until they re-register
		}
		project, matched := matchCatalog(catalog, projectmatch.Inputs{CWD: sess.CWD, WorkspaceRoot: sess.WorkspaceRoot, Remotes: sess.Remotes})
		if !matched {
			continue
		}
		rec := &placementRec{project: int64(project), local: string(sess.ID), parent: func() string {
			if sess.LaunchParentID == nil {
				return ""
			}
			return string(*sess.LaunchParentID)
		}(), created: int64(sess.CreatedAt), promoted: sess.PromotedToRoot, isNew: true}
		all = append(all, rec)
		placed[string(sess.ID)] = true
		appended = true
	}

	if !appended {
		if err = tx.Commit(); err != nil {
			return MutationResult{}, err
		}
		return MutationResult{}, nil
	}
	changed, err := rewritePlacements(ctx, q, all, nil, s.beforePlacementFinalize)
	if err != nil {
		return MutationResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return MutationResult{}, err
	}
	if !changed {
		return MutationResult{}, nil
	}
	// Placement joins into the session wire rows (project slug) and the
	// world payload (project membership): both payloads are marked.
	return MutationResult{Changed: true, SessionsDirty: true, WorldDirty: true}, nil
}
