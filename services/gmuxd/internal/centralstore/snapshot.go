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

// ReadSnapshot composes every requested payload input inside one read
// transaction, so a cross-kind snapshot pair observes a single committed
// state. It performs no mutation; every query is transaction-bound.
func (s *Store) ReadSnapshot(ctx context.Context, query SnapshotQuery) (StoreSnapshot, error) {
	if !query.IncludeSessions && !query.IncludeProjects {
		return StoreSnapshot{}, errors.New("centralstore: snapshot query selects no kind")
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return StoreSnapshot{}, err
	}
	defer tx.Rollback()
	q := s.queries.WithTx(tx)

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
		out.Sessions = make([]SessionView, 0, len(rows))
		for _, r := range rows {
			v, convErr := sessionFromDB(r)
			if convErr != nil {
				return StoreSnapshot{}, convErr
			}
			if v.DismissedAt != nil {
				continue // dismissed means hidden: no wire visibility
			}
			out.Sessions = append(out.Sessions, SessionView{Session: v, Placement: placementBySession[v.ID]})
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
