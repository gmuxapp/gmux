package centralstore

import (
	"context"
	"errors"
	"fmt"
)

// SnapshotQuery selects which snapshot kinds one read transaction composes.
// A cross-kind invalidation must request both kinds so the matched pair is
// read from the same transaction and cannot be torn by a commit between two
// separate reads.
type SnapshotQuery struct {
	IncludeSessions bool
	IncludeProjects bool
}

// SessionPlacement is the placement join for one visible local session.
type SessionPlacement struct {
	ProjectEntryID ProjectEntryID
	ProjectSlug    string
	SiblingScope   string
	Position       int
}

// SessionView is one visible (non-dismissed) local session with its derived
// placement joined in. Runner-live facts (alive, PID, endpoint, runner
// version/hash, resumability) are deliberately absent: they are overlaid
// from the runtime registry at composition time and never read from SQLite.
type SessionView struct {
	Session
	Placement *SessionPlacement
	// Depth is the session's root-relative position in the durable parent
	// graph, computed over ALL local rows (including dismissed-but-retained
	// ancestors) via Lineage. It is therefore the same number the
	// active-subagent budget enforces, not just the visible-subset position.
	Depth int
}

// LocalPeerPlacementView is one durable parent-owned Local-peer placement
// row. The peer's session facts themselves stay ephemeral in the peer
// manager; composition joins these rows onto current peer projections.
type LocalPeerPlacementView struct {
	PeerKey         PeerKey
	SessionID       string
	ParentSessionID string
	ProjectEntryID  ProjectEntryID
	ProjectSlug     string
	SiblingScope    string
	Position        int
}

// StoreSnapshot is the durable half of a snapshot composition, read from one
// transaction. Slices for kinds not requested are nil.
type StoreSnapshot struct {
	Sessions            []SessionView
	Projects            ProjectCatalog
	LocalPeerPlacements []LocalPeerPlacementView
}

// lineageDepthMemo returns start's root-relative depth — identical to
// centralstore.Lineage for acyclic or dangling graphs — memoizing resolved
// depths in cache so walks from multiple visible sessions reuse shared ancestry
// (amortized linear across a projection). If a walk meets a cycle — unreachable
// through the API; the schema rejects it — it defers to centralstore.Lineage
// for that session's exact cycle semantics instead of poisoning the cache.
func lineageDepthMemo(lookup func(id string) (parent string, hasParent, present bool), start string, cache map[string]int) int {
	if d, ok := cache[start]; ok {
		return d
	}
	path := make([]string, 0, 8)
	visiting := make(map[string]bool)
	cur := start
	base := 0
	for {
		if d, ok := cache[cur]; ok {
			base = d
			break
		}
		if visiting[cur] {
			_, d := Lineage(lookup, start)
			return d
		}
		p, hasParent, present := lookup(cur)
		if !present || !hasParent {
			cache[cur] = 0 // its own root
			base = 0
			break
		}
		if _, _, ok := lookup(p); !ok {
			cache[cur] = 0 // parent absent: last present node is its own root
			base = 0
			break
		}
		visiting[cur] = true
		path = append(path, cur)
		cur = p
	}
	d := base
	for i := len(path) - 1; i >= 0; i-- {
		d++
		cache[path[i]] = d
	}
	return cache[start]
}

// ReadSnapshot composes every requested payload input inside one read
// transaction, so a cross-kind snapshot pair observes a single committed
// state. It performs no mutation; every query is transaction-bound.
func (s *Store) ReadSnapshot(ctx context.Context, query SnapshotQuery) (StoreSnapshot, error) {
	if !query.IncludeSessions && !query.IncludeProjects {
		return StoreSnapshot{}, errors.New("centralstore: snapshot query selects no kind")
	}
	tx, err := s.readDB.BeginTx(ctx, nil)
	if err != nil {
		return StoreSnapshot{}, err
	}
	defer tx.Rollback()
	q := s.readQ.WithTx(tx)

	var out StoreSnapshot
	// The catalog is needed by both kinds (sessions join project slugs), so
	// it is read once inside the same transaction.
	catalog, err := catalogFromQueries(ctx, q)
	if err != nil {
		return StoreSnapshot{}, err
	}
	slugByEntry := make(map[ProjectEntryID]string, len(catalog))
	for _, e := range catalog {
		slugByEntry[e.ID] = e.Slug
	}

	if s.betweenSnapshotQueries != nil {
		s.betweenSnapshotQueries()
	}
	placementRows, err := placements(ctx, q)
	if err != nil {
		return StoreSnapshot{}, err
	}

	if query.IncludeSessions {
		rows, listErr := q.ListSessions(ctx)
		if listErr != nil {
			return StoreSnapshot{}, listErr
		}
		placementBySession := make(map[SessionID]*SessionPlacement)
		for _, p := range placementRows {
			if p.local == "" {
				continue
			}
			slug, ok := slugByEntry[ProjectEntryID(p.project)]
			if !ok {
				return StoreSnapshot{}, fmt.Errorf("centralstore: placement references unknown project entry %d", p.project)
			}
			placementBySession[SessionID(p.local)] = &SessionPlacement{
				ProjectEntryID: ProjectEntryID(p.project), ProjectSlug: slug,
				SiblingScope: p.scope, Position: int(p.pos),
			}
		}
		// Hydrate every row (dismissed included) so lineage can count hops through
		// dismissed-but-retained ancestors — the graph that seeds the active-subagent
		// budget, so projected depth equals enforced depth. Depth is then resolved
		// only for VISIBLE sessions and memoized across them: shared ancestry is
		// walked once (a deep chain is linear, not quadratic) and fully-dismissed
		// subtrees, which contribute no visible row, are never walked.
		present := make(map[string]bool, len(rows))
		parent := make(map[string]string, len(rows))
		hasParent := make(map[string]bool, len(rows))
		visible := make([]Session, 0, len(rows))
		for _, r := range rows {
			v, convErr := sessionFromDB(r)
			if convErr != nil {
				return StoreSnapshot{}, convErr
			}
			id := string(v.ID)
			present[id] = true
			if v.ParentSessionID != nil {
				parent[id] = string(*v.ParentSessionID)
				hasParent[id] = true
			}
			if v.DismissedAt == nil {
				visible = append(visible, v)
			}
		}
		lookup := func(id string) (string, bool, bool) {
			return parent[id], hasParent[id], present[id]
		}
		depth := make(map[string]int, len(visible))
		out.Sessions = make([]SessionView, 0, len(visible))
		for _, v := range visible {
			out.Sessions = append(out.Sessions, SessionView{
				Session: v, Placement: placementBySession[v.ID],
				Depth: lineageDepthMemo(lookup, string(v.ID), depth),
			})
		}
	}

	if query.IncludeProjects {
		out.Projects = catalog
		out.LocalPeerPlacements = make([]LocalPeerPlacementView, 0)
		for _, p := range placementRows {
			if p.local != "" {
				continue
			}
			slug, ok := slugByEntry[ProjectEntryID(p.project)]
			if !ok {
				return StoreSnapshot{}, fmt.Errorf("centralstore: placement references unknown project entry %d", p.project)
			}
			out.LocalPeerPlacements = append(out.LocalPeerPlacements, LocalPeerPlacementView{
				PeerKey: PeerKey(p.peer), SessionID: p.session, ParentSessionID: p.parent,
				ProjectEntryID: ProjectEntryID(p.project), ProjectSlug: slug,
				SiblingScope: p.scope, Position: int(p.pos),
			})
		}
	}

	if err = tx.Commit(); err != nil {
		return StoreSnapshot{}, err
	}
	return out, nil
}
