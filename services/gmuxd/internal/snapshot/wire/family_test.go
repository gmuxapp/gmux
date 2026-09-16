package wire

import (
	"reflect"
	"testing"
)

// PROTO (3.0): the roots view re-ranks project_index over roots only.
func TestRootsViewReranksProjectIndexOverRootsOnly(t *testing.T) {
	rows := []Session{
		{ID: "a", ProjectSlug: "p", ProjectIndex: 0, SemanticAgent: true, CreatedAt: "2026-01-01T00:00:00Z"},
		{ID: "a1", ProjectSlug: "p", ProjectIndex: 1, ParentSessionID: "a", CreatedAt: "2026-01-02T00:00:00Z"},
		{ID: "a2", ProjectSlug: "p", ProjectIndex: 2, ParentSessionID: "a", CreatedAt: "2026-01-03T00:00:00Z"},
		{ID: "b", ProjectSlug: "p", ProjectIndex: 3, CreatedAt: "2026-01-04T00:00:00Z"},
		{ID: "c", ProjectSlug: "p", ProjectIndex: 4, CreatedAt: "2026-01-05T00:00:00Z"},
		{ID: "x", ProjectSlug: "q", ProjectIndex: 0, Peer: "hs", CreatedAt: "2026-01-05T00:00:00Z"},
		{ID: "y", ProjectSlug: "q", ProjectIndex: 1, Peer: "hs", CreatedAt: "2026-01-06T00:00:00Z"},
		{ID: "z", CreatedAt: "2026-01-06T00:00:00Z"}, // unplaced: index stays 0
	}
	idx := AnnotateDescendantCounts(rows)
	roots := RootsView(rows, idx)
	got := map[string]int{}
	for _, r := range roots {
		got[r.ID] = r.ProjectIndex
	}
	want := map[string]int{"a": 0, "b": 1, "c": 2, "x": 0, "y": 1, "z": 0}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reranked = %v, want %v", got, want)
	}
	// The annotated (full) rows keep the flat index.
	if rows[3].ProjectIndex != 3 || rows[4].ProjectIndex != 4 {
		t.Fatalf("full view mutated: b=%d c=%d", rows[3].ProjectIndex, rows[4].ProjectIndex)
	}

	// A child spawned under `a` renumbers b and c in the flat index but
	// leaves the roots view byte-identical except for a's counts.
	next := append([]Session(nil), rows...)
	for i := range next {
		next[i].DescendantCounts = nil // annotate is additive (hub composition); start clean
	}
	next = append(next, Session{ID: "a3", ProjectSlug: "p", ProjectIndex: 3, ParentSessionID: "a", CreatedAt: "2026-01-07T00:00:00Z"})
	next[3].ProjectIndex = 4
	next[4].ProjectIndex = 5
	idx2 := AnnotateDescendantCounts(next)
	roots2 := RootsView(next, idx2)
	for i := range roots {
		if roots[i].ID != roots2[i].ID {
			t.Fatalf("order changed at %d: %s vs %s", i, roots[i].ID, roots2[i].ID)
		}
		if roots[i].ID == "a" {
			if roots2[i].DescendantCounts == nil || roots2[i].DescendantCounts.Total != 3 {
				t.Fatalf("a counts = %+v", roots2[i].DescendantCounts)
			}
			continue
		}
		if !reflect.DeepEqual(roots[i], roots2[i]) {
			t.Fatalf("root %s changed on a child spawn:\n%+v\n%+v", roots[i].ID, roots[i], roots2[i])
		}
	}
}
