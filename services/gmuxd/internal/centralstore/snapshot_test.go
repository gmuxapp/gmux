package centralstore

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestReadSnapshotJoinsPlacementsAndFiltersDismissed(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	cat := registrationCatalog(t, s)

	if _, _, err := s.RegisterRunner(ctx, registration("placed", "shell", "/one/src", true, 10)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RegisterRunner(ctx, registration("unplaced", "shell", "/elsewhere", true, 20)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RegisterRunner(ctx, registration("hidden", "shell", "/one", true, 30)); err != nil {
		t.Fatal(err)
	}
	// No dismissal domain operation exists in this slice; set the column
	// directly to characterize the wire-visibility filter.
	if _, err := s.database.ExecContext(ctx,
		`UPDATE local_sessions SET dismissed_at_ms = 40 WHERE id = 'hidden'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.database.ExecContext(ctx,
		`DELETE FROM project_placements WHERE local_session_id = 'hidden'`); err != nil {
		t.Fatal(err)
	}

	snap, err := s.ReadSnapshot(ctx, SnapshotQuery{IncludeSessions: true, IncludeProjects: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Sessions) != 2 {
		t.Fatalf("sessions=%d (%#v), dismissed row must be hidden", len(snap.Sessions), snap.Sessions)
	}
	// ListSessions orders by id: "placed" then "unplaced".
	placed, unplaced := snap.Sessions[0], snap.Sessions[1]
	if placed.ID != "placed" || unplaced.ID != "unplaced" {
		t.Fatalf("deterministic id order violated: %s, %s", placed.ID, unplaced.ID)
	}
	if placed.Placement == nil || placed.Placement.ProjectSlug != "one" ||
		placed.Placement.ProjectEntryID != cat[0].ID || placed.Placement.Position != 0 {
		t.Fatalf("placement join=%#v", placed.Placement)
	}
	if unplaced.Placement != nil {
		t.Fatalf("unmatched session must be visible but unplaced: %#v", unplaced.Placement)
	}
	if len(snap.Projects) != 3 || snap.Projects[0].Slug != "one" {
		t.Fatalf("projects=%#v", snap.Projects)
	}
	if snap.LocalPeerPlacements == nil || len(snap.LocalPeerPlacements) != 0 {
		t.Fatalf("local peer placements must be an empty non-nil slice: %#v", snap.LocalPeerPlacements)
	}
	if placed.Title != "sh" {
		t.Fatalf("derived title=%q", placed.Title)
	}
}

func TestReadSnapshotIncludesLocalPeerPlacements(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	cat := registrationCatalog(t, s)
	if _, err := s.UpsertLocalPeerPlacement(ctx, LocalPeerSubject{PeerKey: "laptop", SessionID: "peer-1"}, cat[1].ID); err != nil {
		t.Fatal(err)
	}
	snap, err := s.ReadSnapshot(ctx, SnapshotQuery{IncludeProjects: true})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Sessions != nil {
		t.Fatalf("sessions kind not requested, got %#v", snap.Sessions)
	}
	if len(snap.LocalPeerPlacements) != 1 {
		t.Fatalf("peer placements=%#v", snap.LocalPeerPlacements)
	}
	p := snap.LocalPeerPlacements[0]
	if p.PeerKey != "laptop" || p.SessionID != "peer-1" || p.ProjectSlug != "two" || p.Position != 0 {
		t.Fatalf("peer placement=%#v", p)
	}
}

func TestReadSnapshotNarrowQueries(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	registrationCatalog(t, s)
	if _, _, err := s.RegisterRunner(ctx, registration("a", "shell", "/one", true, 10)); err != nil {
		t.Fatal(err)
	}
	sessionsOnly, err := s.ReadSnapshot(ctx, SnapshotQuery{IncludeSessions: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessionsOnly.Sessions) != 1 || sessionsOnly.Projects != nil || sessionsOnly.LocalPeerPlacements != nil {
		t.Fatalf("sessions-only=%#v", sessionsOnly)
	}
	projectsOnly, err := s.ReadSnapshot(ctx, SnapshotQuery{IncludeProjects: true})
	if err != nil {
		t.Fatal(err)
	}
	if projectsOnly.Sessions != nil || len(projectsOnly.Projects) != 3 {
		t.Fatalf("projects-only=%#v", projectsOnly)
	}
	if _, err := s.ReadSnapshot(ctx, SnapshotQuery{}); err == nil {
		t.Fatal("empty query must be rejected")
	}
}

// TestReadSnapshotIsTransactionConsistent forces a writer between the
// snapshot's component queries and proves the composition observes the
// all-before state (never a mix), while the writer commits afterwards.
func TestReadSnapshotIsTransactionConsistent(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	registrationCatalog(t, s)
	if _, _, err := s.RegisterRunner(ctx, registration("existing", "shell", "/one", true, 10)); err != nil {
		t.Fatal(err)
	}

	var writerDone sync.WaitGroup
	writerStarted := make(chan struct{})
	var writerErr error
	s.betweenSnapshotQueries = func() {
		writerDone.Add(1)
		go func() {
			defer writerDone.Done()
			close(writerStarted)
			// A cross-kind mutation: inserts a session AND its placement.
			_, _, writerErr = s.RegisterRunner(ctx, registration("intruder", "shell", "/two", true, 20))
		}()
		<-writerStarted
		// Give the writer a real chance to commit if the read transaction
		// were not isolating it.
		time.Sleep(100 * time.Millisecond)
	}
	snap, err := s.ReadSnapshot(ctx, SnapshotQuery{IncludeSessions: true, IncludeProjects: true})
	s.betweenSnapshotQueries = nil
	if err != nil {
		t.Fatal(err)
	}
	writerDone.Wait()
	if writerErr != nil {
		t.Fatal(writerErr)
	}
	if len(snap.Sessions) != 1 || snap.Sessions[0].ID != "existing" {
		t.Fatalf("torn snapshot: %#v", snap.Sessions)
	}
	after, err := s.ReadSnapshot(ctx, SnapshotQuery{IncludeSessions: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Sessions) != 2 {
		t.Fatalf("writer commit lost: %#v", after.Sessions)
	}
}

func TestReadSnapshotEmptyStoreShape(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	snap, err := s.ReadSnapshot(ctx, SnapshotQuery{IncludeSessions: true, IncludeProjects: true})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Sessions == nil || len(snap.Sessions) != 0 {
		t.Fatalf("sessions must be an empty non-nil slice: %#v", snap.Sessions)
	}
	if len(snap.Projects) != 0 {
		t.Fatalf("projects=%#v", snap.Projects)
	}
	if snap.LocalPeerPlacements == nil || len(snap.LocalPeerPlacements) != 0 {
		t.Fatalf("peer placements must be an empty non-nil slice: %#v", snap.LocalPeerPlacements)
	}
}

// TestReadSnapshotRejectsOrphanPlacement covers the defensive error path for
// a placement whose project entry is missing. The schema's FK + triggers
// prevent this state through any domain operation, so it is manufactured
// with foreign keys disabled (simulating corruption).
func TestReadSnapshotRejectsOrphanPlacement(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	cat := registrationCatalog(t, s)
	if _, _, err := s.RegisterRunner(ctx, registration("placed", "shell", "/one", true, 10)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertLocalPeerPlacement(ctx, LocalPeerSubject{PeerKey: "laptop", SessionID: "peer-1"}, cat[1].ID); err != nil {
		t.Fatal(err)
	}
	// Orphan both the local placement (entry "one") and the Local-peer
	// placement (entry "two").
	if _, err := s.database.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []ProjectEntryID{cat[0].ID, cat[1].ID} {
		if _, err := s.database.ExecContext(ctx, "DELETE FROM project_match_rules WHERE project_entry_id = ?", int64(id)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.database.ExecContext(ctx, "DELETE FROM project_entries WHERE id = ?", int64(id)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.database.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadSnapshot(ctx, SnapshotQuery{IncludeSessions: true}); err == nil {
		t.Fatal("orphan placement must fail the snapshot read, not silently drop")
	}
	if _, err := s.ReadSnapshot(ctx, SnapshotQuery{IncludeProjects: true}); err == nil {
		t.Fatal("orphan Local-peer-side read must also fail")
	}
}

func TestReadSnapshotPerformsNoMutation(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	registrationCatalog(t, s)
	row, _, err := s.RegisterRunner(ctx, registration("a", "shell", "/one", true, 10))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReadSnapshot(ctx, SnapshotQuery{IncludeSessions: true, IncludeProjects: true}); err != nil {
		t.Fatal(err)
	}
	after := mustSession(t, s, "a")
	if after.Version != row.Version {
		t.Fatalf("snapshot read mutated the row: %d -> %d", row.Version, after.Version)
	}
}
