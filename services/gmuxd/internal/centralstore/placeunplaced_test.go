package centralstore

import (
	"context"
	"path/filepath"
	"testing"
)

func placeUnplacedStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestPlaceUnplacedSessionsPlacesMatchingAndSkipsRest(t *testing.T) {
	ctx := context.Background()
	s := placeUnplacedStore(t)
	cat, _, err := s.ReplaceProjectCatalog(ctx, []ProjectEntrySpec{owned("a", "/a")}, 0)
	if err != nil {
		t.Fatal(err)
	}
	project := cat[0].ID

	// Matching unplaced, non-matching, already-placed, dismissed.
	addSessionCwd(t, s, "16ojhy50", "", "/a/x", 1)
	addSessionCwd(t, s, "1uz7l5ot", "", "/b/x", 2)
	addSessionCwd(t, s, "1pknzlg8", "", "/a/y", 3)
	placeLocal(t, s, "1pknzlg8", project)
	addSessionCwd(t, s, "1j0ydg3f", "", "/a/z", 4)
	if _, _, err := s.DismissSessionTree(ctx, "1j0ydg3f", 5); err != nil {
		t.Fatal(err)
	}

	placedBefore := localPlacementOf(t, s, "1pknzlg8")
	result, err := s.PlaceUnplacedSessions(ctx, []SessionID{"16ojhy50", "1uz7l5ot", "1pknzlg8", "1j0ydg3f", "1ve25bnc"}, 6)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || !result.SessionsDirty || !result.WorldDirty {
		t.Fatalf("result=%+v, want changed+both dirty", result)
	}

	if rec := localPlacementOf(t, s, "16ojhy50"); rec == nil || rec.project != int64(project) {
		t.Fatalf("16ojhy50 not placed: %+v", rec)
	}
	if rec := localPlacementOf(t, s, "1uz7l5ot"); rec != nil {
		t.Fatal("non-matching session must stay unplaced")
	}
	if rec := localPlacementOf(t, s, "1j0ydg3f"); rec != nil {
		t.Fatal("dismissed session must stay unplaced")
	}
	placedAfter := localPlacementOf(t, s, "1pknzlg8")
	if placedAfter == nil || placedAfter.project != placedBefore.project {
		t.Fatal("already-placed session must keep its placement")
	}
	// Registration-placement semantics: the newcomer appends at the bottom
	// of its root scope, after the pre-existing placement.
	if !(placedAfter.pos < localPlacementOf(t, s, "16ojhy50").pos) {
		t.Fatalf("expected append at bottom: placed=%d match=%d", placedAfter.pos, localPlacementOf(t, s, "16ojhy50").pos)
	}
}

func TestPlaceUnplacedSessionsNoOpWhenNothingMatches(t *testing.T) {
	ctx := context.Background()
	s := placeUnplacedStore(t)
	if _, _, err := s.ReplaceProjectCatalog(ctx, []ProjectEntrySpec{owned("a", "/a")}, 0); err != nil {
		t.Fatal(err)
	}
	addSessionCwd(t, s, "1uz7l5ot", "", "/b/x", 1)

	result, err := s.PlaceUnplacedSessions(ctx, []SessionID{"1uz7l5ot"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if result.Changed {
		t.Fatalf("no-op pass must not report change: %+v", result)
	}
	if result, err = s.PlaceUnplacedSessions(ctx, nil, 2); err != nil || result.Changed {
		t.Fatalf("empty id set: result=%+v err=%v", result, err)
	}
}

func TestPlaceUnplacedSessionsChildJoinsParentScope(t *testing.T) {
	ctx := context.Background()
	s := placeUnplacedStore(t)
	cat, _, err := s.ReplaceProjectCatalog(ctx, []ProjectEntrySpec{owned("a", "/a")}, 0)
	if err != nil {
		t.Fatal(err)
	}
	project := cat[0].ID

	addSessionCwd(t, s, "1rz9lyqa", "", "/a/x", 1)
	placeLocal(t, s, "1rz9lyqa", project)
	addSessionCwd(t, s, "10yeqnxg", "1rz9lyqa", "/a/x", 2)

	if _, err := s.PlaceUnplacedSessions(ctx, []SessionID{"10yeqnxg"}, 3); err != nil {
		t.Fatal(err)
	}
	rec := localPlacementOf(t, s, "10yeqnxg")
	if rec == nil || rec.project != int64(project) || rec.parent != "1rz9lyqa" {
		t.Fatalf("child placement: %+v", rec)
	}
}

func TestPlaceUnplacedSessionsRejectsNegativeTimestamp(t *testing.T) {
	s := placeUnplacedStore(t)
	if _, err := s.PlaceUnplacedSessions(context.Background(), []SessionID{"1108gm0e"}, -1); err == nil {
		t.Fatal("expected error for negative timestamp")
	}
}
