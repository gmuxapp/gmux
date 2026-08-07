package main

import (
	"fmt"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

func TestProjectSessionTreeRowsBreadthFirst(t *testing.T) {
	root := centralstore.SessionID("root")
	childB := centralstore.SessionID("child-b")
	childA := centralstore.SessionID("child-a")
	rows := []centralstore.Session{
		{ID: "unrelated"},
		{ID: childB, ParentSessionID: &root},
		{ID: "grandchild", ParentSessionID: &childA},
		{ID: root},
		{ID: childA, ParentSessionID: &root},
	}

	got, err := projectSessionTreeRows(rows, root)
	if err != nil {
		t.Fatal(err)
	}
	want := []centralstore.SessionID{root, childA, childB, "grandchild"}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Fatalf("row %d = %q, want %q", i, got[i].ID, want[i])
		}
	}
}

// BenchmarkProjectSessionTreeRows mirrors the browser repro corpus: 1,000
// stored sessions, 500 of them children of the selected root.
func BenchmarkProjectSessionTreeRows(b *testing.B) {
	root := centralstore.SessionID("root")
	rows := make([]centralstore.Session, 0, 1000)
	rows = append(rows, centralstore.Session{ID: root})
	for i := 0; i < 500; i++ {
		rows = append(rows, centralstore.Session{
			ID:              centralstore.SessionID(fmt.Sprintf("child-%04d", i)),
			ParentSessionID: &root,
		})
	}
	for i := 0; i < 499; i++ {
		rows = append(rows, centralstore.Session{ID: centralstore.SessionID(fmt.Sprintf("other-%04d", i))})
	}

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		got, err := projectSessionTreeRows(rows, root)
		if err != nil {
			b.Fatal(err)
		}
		if len(got) != 501 {
			b.Fatalf("got %d rows, want 501", len(got))
		}
	}
}
