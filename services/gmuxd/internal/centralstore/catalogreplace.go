package centralstore

import (
	"context"
	"errors"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore/internal/db"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/projectmatch"
)

// LocalPeerMatchInput is one connected Local-peer session's point-in-time
// match input, supplied by the coordinator (which owns peer connectivity and
// the ephemeral session projections; SQLite knows neither). The parent
// linkage travels with the input because it is peer-reported runtime truth,
// not durable local state.
type LocalPeerMatchInput struct {
	Subject       LocalPeerSubject
	CWD           string
	WorkspaceRoot string
	Remotes       map[string]string
}

// ReplaceProjectCatalogAndRematch is the authoritative full-configuration
// replacement operation (ADR 0026 §7/§9 "project-rule changes plus affected
// placement updates"). In one transaction it:
//
//   - applies the new catalog and rules with the same identity-immutability
//     contract as ReplaceProjectCatalog (shared replaceCatalogInTx);
//   - re-derives membership for every subject that was PLACED at the start
//     of the operation, via the single shared matcher (matchCatalog):
//     a subject whose derived project is unchanged keeps its placement row
//     untouched (durable ordering preserved); a subject whose project moved
//     is retargeted and appended canonically at the bottom of its new
//     derived scope; a subject matching no owned project loses its placement
//     and becomes unplaced/discovered — never dismissed;
//   - re-places subjects whose placement was cascade-deleted with their
//     project entry when they match a surviving or new owned entry;
//   - normalizes every affected sibling scope densely.
//
// Deliberately conservative (flagged in the design doc): sessions that were
// UNPLACED before the call are never placed here, because production only
// auto-assigns live sessions and liveness is not represented in SQLite. The
// coordinator owns that follow-up through the ordinary registration-time
// rematch or an explicit assignment.
//
// localPeers is the point-in-time input set for connected Local-peer
// subjects (ADR 0025 exception: the parent owns their placement). A placed
// Local-peer subject without a supplied input keeps its placement when its
// project entry survived; its stale row stays invisible until the peer
// reconnects or PruneLocalPeer removes it.
func (s *Store) ReplaceProjectCatalogAndRematch(ctx context.Context, input []ProjectEntrySpec, localPeers []LocalPeerMatchInput, at UnixMillis) (ProjectCatalog, MutationResult, error) {
	if at < 0 {
		return nil, MutationResult{}, errors.New("centralstore: catalog timestamp must be non-negative")
	}
	in, err := normalizeSpecs(input)
	if err != nil {
		return nil, MutationResult{}, err
	}
	peerInputs := map[string]LocalPeerMatchInput{}
	for _, p := range localPeers {
		if err := validateSubject(SubjectRef{LocalPeer: &p.Subject}); err != nil {
			return nil, MutationResult{}, err
		}
		key := subjectKey(SubjectRef{LocalPeer: &p.Subject})
		if _, dup := peerInputs[key]; dup {
			return nil, MutationResult{}, errors.New("centralstore: duplicate Local-peer match input")
		}
		peerInputs[key] = p
	}

	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return nil, MutationResult{}, err
	}
	defer tx.Rollback()
	q := s.queries.WithTx(tx)

	// The placed-subject set is captured BEFORE the catalog swap: deleting a
	// project entry cascade-deletes its placements, and a subject placed in
	// a deleted project is still "previously placed" and eligible for
	// re-placement under the new rules.
	pre, err := placements(ctx, q)
	if err != nil {
		return nil, MutationResult{}, err
	}
	prePlaced := make(map[string]bool, len(pre))
	for _, r := range pre {
		prePlaced[recKey(r)] = true
	}

	catalog, catalogChanged, err := replaceCatalogInTx(ctx, q, in, at)
	if err != nil {
		return nil, MutationResult{}, err
	}

	all, err := placements(ctx, q)
	if err != nil {
		return nil, MutationResult{}, err
	}
	survivors := make(map[string]*placementRec, len(all))
	for _, r := range all {
		survivors[recKey(r)] = r
	}

	sessions, err := q.ListSessions(ctx)
	if err != nil {
		return nil, MutationResult{}, err
	}
	sessionByID := make(map[string]Session, len(sessions))
	for _, raw := range sessions {
		v, convErr := sessionFromDB(raw)
		if convErr != nil {
			return nil, MutationResult{}, convErr
		}
		sessionByID[string(v.ID)] = v
	}

	placementChanged := false
	peerParentChanged := false

	removeRec := func(rec *placementRec) {
		kept := all[:0]
		for _, r := range all {
			if r != rec {
				kept = append(kept, r)
			}
		}
		all = kept
	}

	// Local sessions: every subject placed before the swap is re-derived.
	for _, r := range pre {
		if r.local == "" {
			continue
		}
		sess, ok := sessionByID[r.local]
		if !ok {
			// FK-impossible inside one transaction; fail loudly rather than
			// silently dropping a subject.
			return nil, MutationResult{}, errors.New("centralstore: placed session row missing during rematch")
		}
		project, matched := matchCatalog(catalog, projectmatch.Inputs{CWD: sess.CWD, WorkspaceRoot: sess.WorkspaceRoot, Remotes: sess.Remotes})
		survivor := survivors["l:"+r.local]
		switch {
		case survivor != nil && matched && survivor.project == int64(project):
			// Project survives with the same derived entry: durable ordering
			// preserved by leaving the row untouched.
		case survivor != nil && matched:
			survivor.project = int64(project)
			placementChanged = true
		case survivor != nil:
			n, deleteErr := q.DeleteLocalSessionPlacement(ctx, nullString(r.local))
			if deleteErr != nil {
				return nil, MutationResult{}, deleteErr
			}
			if n != 1 {
				return nil, MutationResult{}, errors.New("centralstore: placement disappeared during catalog rematch")
			}
			removeRec(survivor)
			delete(survivors, "l:"+r.local)
			placementChanged = true
		case matched:
			// Placement was cascade-deleted with its entry; re-place.
			rec := &placementRec{project: int64(project), local: r.local, parent: func() string {
				if sess.LaunchParentID == nil {
					return ""
				}
				return string(*sess.LaunchParentID)
			}(), created: int64(sess.CreatedAt), promoted: sess.PromotedToRoot, isNew: true}
			all = append(all, rec)
			survivors["l:"+r.local] = rec
			placementChanged = true
		}
	}

	// Local-peer subjects: only supplied inputs for previously placed
	// subjects are re-derived; the parent linkage is refreshed from the
	// point-in-time input.
	for key, p := range peerInputs {
		if !prePlaced[key] {
			continue // this op rematches placed subjects only
		}
		project, matched := matchCatalog(catalog, projectmatch.Inputs{CWD: p.CWD, WorkspaceRoot: p.WorkspaceRoot, Remotes: p.Remotes})
		survivor := survivors[key]
		switch {
		case survivor != nil && matched:
			if survivor.project != int64(project) {
				survivor.project = int64(project)
				placementChanged = true
			}
			if survivor.parent != p.Subject.ParentSessionID {
				survivor.parent = p.Subject.ParentSessionID
				peerParentChanged = true
				placementChanged = true
			}
		case survivor != nil:
			n, deleteErr := q.DeleteLocalPeerPlacement(ctx, db.DeleteLocalPeerPlacementParams{LocalPeerKey: nullString(string(p.Subject.PeerKey)), PeerSessionID: nullString(p.Subject.SessionID)})
			if deleteErr != nil {
				return nil, MutationResult{}, deleteErr
			}
			if n != 1 {
				return nil, MutationResult{}, errors.New("centralstore: Local-peer placement disappeared during catalog rematch")
			}
			removeRec(survivor)
			delete(survivors, key)
			placementChanged = true
		case matched:
			rec := &placementRec{project: int64(project), peer: string(p.Subject.PeerKey), session: p.Subject.SessionID, parent: p.Subject.ParentSessionID, isNew: true}
			all = append(all, rec)
			survivors[key] = rec
			placementChanged = true
			// A re-placed subject introduces its input's parent linkage just
			// like a survivor whose parent changed; without this flag a cyclic
			// parent graph arriving entirely via cascade-deleted re-placed
			// subjects would commit — and then permanently brick every later
			// Local-peer placement op behind the same guard (fable MEDIUM-1).
			peerParentChanged = true
		}
	}
	if peerParentChanged {
		if err = validateLocalPeerParentGraph(all); err != nil {
			return nil, MutationResult{}, err
		}
	}

	changed := catalogChanged || placementChanged
	if changed {
		// One global rewrite: movers append canonically, survivors keep
		// their durable order, deleted parents' children regroup, and every
		// affected scope densifies to 0..n-1.
		rewritten, rewriteErr := rewritePlacements(ctx, q, all, nil, s.beforePlacementFinalize)
		if rewriteErr != nil {
			return nil, MutationResult{}, rewriteErr
		}
		changed = catalogChanged || placementChanged || rewritten
	}
	if err = tx.Commit(); err != nil {
		return nil, MutationResult{}, err
	}
	if !changed {
		return catalog, MutationResult{}, nil
	}
	// The sessions payload embeds the placement/slug join (SessionView), so
	// any catalog or placement change may alter session wire rows; both
	// kinds are marked (level-triggered, never under-invalidates).
	return catalog, MutationResult{Changed: true, SessionsDirty: true, WorldDirty: true}, nil
}
