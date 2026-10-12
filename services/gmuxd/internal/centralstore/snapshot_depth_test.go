package centralstore

import (
	"context"
	"testing"
)

// The projected depth must equal the budget's ENFORCED depth, which counts
// dismissed-but-retained ancestors. A live session reparented under a retained
// dismissed parent therefore projects one deeper than its visible position —
// the case where a visible-only walk would wrongly read it as a root.
func TestReadSnapshotDepthFollowsFullDurableGraph(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	registrationCatalog(t, s)

	for _, id := range []string{"R", "P", "C", "S"} {
		if _, _, err := s.RegisterRunner(ctx, registration(id, "shell", "/one", true, 10)); err != nil {
			t.Fatal(err)
		}
	}
	link := func(child, parent string) {
		t.Helper()
		p := SessionID(parent)
		if _, err := s.SetSessionParent(ctx, SessionID(child), &p); err != nil {
			t.Fatalf("reparent %s→%s: %v", child, parent, err)
		}
	}
	link("P", "R")
	link("C", "P") // C ← P ← R

	// Retain-dismiss P (dismissal stamps dismissed_at and keeps the row). C is
	// NOT dismissed and stays reparented under it.
	if _, err := s.database.ExecContext(ctx,
		`UPDATE local_sessions SET dismissed_at_ms = 99 WHERE id = 'P'`); err != nil {
		t.Fatal(err)
	}

	snap, err := s.ReadSnapshot(ctx, SnapshotQuery{IncludeSessions: true})
	if err != nil {
		t.Fatal(err)
	}
	depth := map[SessionID]int{}
	for _, v := range snap.Sessions {
		depth[v.ID] = v.Depth
	}
	if _, ok := depth["P"]; ok {
		t.Fatal("dismissed P must be hidden from the projection")
	}
	// R is a root (0); C sits under the retained dismissed P, so it projects at
	// depth 2 (R→P→C), matching what the budget enforces — NOT the visible-only
	// depth of 0 it would get if the walk stopped at the hidden parent. S is a
	// plain root (0).
	for id, want := range map[SessionID]int{"R": 0, "C": 2, "S": 0} {
		got, ok := depth[id]
		if !ok {
			t.Fatalf("%s must be visible in the projection", id)
		}
		if got != want {
			t.Errorf("depth(%s) = %d, want %d", id, got, want)
		}
	}
}

// The memoized depth walk must match a per-session Lineage exactly across
// visible sessions that share ancestry (the amortized path), not just isolated
// chains.
func TestReadSnapshotDepthMemoizedVisibleChain(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	registrationCatalog(t, s)

	for _, id := range []string{"a", "b", "c", "d"} {
		if _, _, err := s.RegisterRunner(ctx, registration(id, "shell", "/one", true, 10)); err != nil {
			t.Fatal(err)
		}
	}
	link := func(child, parent string) {
		t.Helper()
		p := SessionID(parent)
		if _, err := s.SetSessionParent(ctx, SessionID(child), &p); err != nil {
			t.Fatalf("reparent %s→%s: %v", child, parent, err)
		}
	}
	link("b", "a")
	link("c", "b")
	link("d", "c")

	snap, err := s.ReadSnapshot(ctx, SnapshotQuery{IncludeSessions: true})
	if err != nil {
		t.Fatal(err)
	}
	depth := map[SessionID]int{}
	for _, v := range snap.Sessions {
		depth[v.ID] = v.Depth
	}
	for id, want := range map[SessionID]int{"a": 0, "b": 1, "c": 2, "d": 3} {
		if depth[id] != want {
			t.Errorf("memoized depth(%s) = %d, want %d", id, depth[id], want)
		}
	}
}
