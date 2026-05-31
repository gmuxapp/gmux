package peerstore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenEmptyWhenAbsent(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.List()) != 0 {
		t.Fatalf("want empty, got %v", s.List())
	}
}

func TestAddOrGetPersistsAndReloads(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	if _, _, err := s.AddOrGet(Record{Name: "laptop", URL: "https://gmux-laptop.ts.net", Token: "secret", NodeID: "node_a"}); err != nil {
		t.Fatal(err)
	}

	// File is 0600 (carries a token).
	info, err := os.Stat(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %o, want 600", info.Mode().Perm())
	}

	// Reloads identically.
	s2, _ := Open(dir)
	recs := s2.List()
	if len(recs) != 1 || recs[0].Name != "laptop" || recs[0].Token != "secret" || recs[0].NodeID != "node_a" {
		t.Fatalf("reloaded = %+v", recs)
	}
}

func TestAddOrGetRejectsBadURL(t *testing.T) {
	s, _ := Open(t.TempDir())
	if _, _, err := s.AddOrGet(Record{Name: "x", URL: "ftp://nope", NodeID: "n"}); err == nil {
		t.Fatal("expected error for non-http url")
	}
}

// Re-adding the same node_id returns the existing record (existed=true)
// rather than creating a duplicate — and does so under one lock, so it's
// safe against concurrent connects (no check-then-act race).
func TestAddOrGetDedupsByNodeID(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	first, _, _ := s.AddOrGet(Record{Name: "laptop", URL: "http://a:8790", NodeID: "node_x"})

	got, existed, err := s.AddOrGet(Record{Name: "laptop-again", URL: "http://b:8790", NodeID: "node_x"})
	if err != nil {
		t.Fatal(err)
	}
	if !existed {
		t.Fatal("re-adding same node_id should report existed=true")
	}
	if got.Name != first.Name || got.URL != "http://a:8790" {
		t.Fatalf("dedup should return the original record, got %+v", got)
	}
	if len(s.List()) != 1 {
		t.Fatalf("dedup must not append, got %d records", len(s.List()))
	}
}

// An empty node_id is undedupable: each add is a distinct host.
func TestAddOrGetEmptyNodeIDNotDeduped(t *testing.T) {
	s, _ := Open(t.TempDir())
	s.AddOrGet(Record{Name: "a", URL: "http://a:8790"})
	s.AddOrGet(Record{Name: "b", URL: "http://b:8790"})
	if len(s.List()) != 2 {
		t.Fatalf("empty node_id must not dedup, got %d", len(s.List()))
	}
}

func TestAddOrGetSlugifiesAndSuffixes(t *testing.T) {
	s, _ := Open(t.TempDir())
	// Non-slug self-reported name is slugified rather than rejected.
	got, _, err := s.AddOrGet(Record{Name: "My-Laptop.local", URL: "http://a:8790", NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "my-laptop-local" {
		t.Fatalf("slugified name = %q, want my-laptop-local", got.Name)
	}
	// A different node reporting a name that slugifies to the same value
	// is suffixed.
	got2, _, _ := s.AddOrGet(Record{Name: "my-laptop-local", URL: "http://b:8790", NodeID: "n2"})
	if got2.Name != "my-laptop-local-2" {
		t.Fatalf("collision name = %q, want my-laptop-local-2", got2.Name)
	}
}

func TestAddOrGetRejectsUnslugifiableName(t *testing.T) {
	s, _ := Open(t.TempDir())
	if _, _, err := s.AddOrGet(Record{Name: "...", URL: "http://a:8790", NodeID: "n"}); err == nil {
		t.Fatal("expected error for a name with no usable slug characters")
	}
}

func TestSlugify(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"My-Laptop.local", "my-laptop-local"},
		{"gmux-server", "gmux-server"},
		{"  Spaces  ", "spaces"},
		{"...", ""},
		{"", ""},
	} {
		if got := Slugify(tt.in); got != tt.want {
			t.Errorf("Slugify(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestRemove(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	s.AddOrGet(Record{Name: "gone", URL: "http://h:8790", NodeID: "n"})
	rec, ok, err := s.Remove("gone")
	if err != nil || !ok || rec.Name != "gone" {
		t.Fatalf("remove = %+v ok=%v err=%v", rec, ok, err)
	}
	if _, ok, _ := s.Remove("gone"); ok {
		t.Fatal("second remove should report not found")
	}
	s2, _ := Open(dir)
	if len(s2.List()) != 0 {
		t.Fatalf("want empty after remove, got %v", s2.List())
	}
}
