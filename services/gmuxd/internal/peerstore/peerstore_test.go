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

func TestAddPersistsAndReloads(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	if _, err := s.Add(Record{Name: "laptop", URL: "https://gmux-laptop.ts.net", Token: "secret", NodeID: "node_a"}); err != nil {
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

func TestAddRejectsBadURL(t *testing.T) {
	s, _ := Open(t.TempDir())
	if _, err := s.Add(Record{Name: "x", URL: "ftp://nope"}); err == nil {
		t.Fatal("expected error for non-http url")
	}
}

func TestFindByNodeID(t *testing.T) {
	s, _ := Open(t.TempDir())
	s.Add(Record{Name: "a", URL: "http://h:8790", NodeID: "node_x"})

	if _, ok := s.FindByNodeID("node_x"); !ok {
		t.Fatal("should find by node id")
	}
	if _, ok := s.FindByNodeID("node_y"); ok {
		t.Fatal("should not find unknown node id")
	}
	// Empty node id never matches (undedupable peer is always new).
	if _, ok := s.FindByNodeID(""); ok {
		t.Fatal("empty node id must not match")
	}
}

func TestAddSuffixesNameCollision(t *testing.T) {
	s, _ := Open(t.TempDir())
	s.Add(Record{Name: "server", URL: "http://a:8790", NodeID: "node_1"})
	got, err := s.Add(Record{Name: "server", URL: "http://b:8790", NodeID: "node_2"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "server-2" {
		t.Fatalf("collision name = %q, want server-2", got.Name)
	}
	third, _ := s.Add(Record{Name: "server", URL: "http://c:8790", NodeID: "node_3"})
	if third.Name != "server-3" {
		t.Fatalf("third name = %q, want server-3", third.Name)
	}
}

func TestRemove(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	s.Add(Record{Name: "gone", URL: "http://h:8790"})
	rec, ok, err := s.Remove("gone")
	if err != nil || !ok || rec.Name != "gone" {
		t.Fatalf("remove = %+v ok=%v err=%v", rec, ok, err)
	}
	if _, ok, _ := s.Remove("gone"); ok {
		t.Fatal("second remove should report not found")
	}
	// Persisted empty.
	s2, _ := Open(dir)
	if len(s2.List()) != 0 {
		t.Fatalf("want empty after remove, got %v", s2.List())
	}
}
