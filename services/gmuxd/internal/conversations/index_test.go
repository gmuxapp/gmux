package conversations

import (
	"testing"
	"time"
)

func TestLookup_Empty(t *testing.T) {
	idx := New()
	_, ok := idx.Lookup("pi", "fix-auth")
	if ok {
		t.Error("expected miss on empty index")
	}
}

func TestUpsert_BasicLookup(t *testing.T) {
	idx := New()
	slug := idx.Upsert(Info{
		ToolID: "abc-123",
		Slug:   "fix-auth",
		Kind:   "pi",
		Title:  "fix auth bug",
	})
	if slug != "fix-auth" {
		t.Fatalf("expected slug fix-auth, got %q", slug)
	}
	info, ok := idx.Lookup("pi", "fix-auth")
	if !ok {
		t.Fatal("expected hit")
	}
	if info.Title != "fix auth bug" {
		t.Errorf("expected title 'fix auth bug', got %q", info.Title)
	}
	if idx.Count() != 1 {
		t.Errorf("expected count 1, got %d", idx.Count())
	}
}

func TestUpsert_UpdateInPlace(t *testing.T) {
	idx := New()
	idx.Upsert(Info{
		ToolID: "abc-123",
		Slug:   "fix-auth",
		Kind:   "pi",
		Title:  "old title",
	})
	idx.Upsert(Info{
		ToolID: "abc-123",
		Slug:   "fix-auth",
		Kind:   "pi",
		Title:  "new title",
	})
	if idx.Count() != 1 {
		t.Fatalf("expected count 1, got %d", idx.Count())
	}
	info, _ := idx.Lookup("pi", "fix-auth")
	if info.Title != "new title" {
		t.Errorf("expected updated title, got %q", info.Title)
	}
}

func TestUpsert_SlugCollision(t *testing.T) {
	idx := New()
	s1 := idx.Upsert(Info{ToolID: "aaa", Slug: "say-hi", Kind: "claude"})
	s2 := idx.Upsert(Info{ToolID: "bbb", Slug: "say-hi", Kind: "claude"})
	s3 := idx.Upsert(Info{ToolID: "ccc", Slug: "say-hi", Kind: "claude"})

	if s1 != "say-hi" {
		t.Errorf("first should be unsuffixed, got %q", s1)
	}
	if s2 != "say-hi-2" {
		t.Errorf("second should be -2, got %q", s2)
	}
	if s3 != "say-hi-3" {
		t.Errorf("third should be -3, got %q", s3)
	}
	if idx.Count() != 3 {
		t.Errorf("expected count 3, got %d", idx.Count())
	}
}

func TestUpsert_SlugCollisionAcrossKinds(t *testing.T) {
	idx := New()
	s1 := idx.Upsert(Info{ToolID: "aaa", Slug: "fix-auth", Kind: "pi"})
	s2 := idx.Upsert(Info{ToolID: "bbb", Slug: "fix-auth", Kind: "claude"})

	// Different kinds should not collide.
	if s1 != "fix-auth" || s2 != "fix-auth" {
		t.Errorf("cross-kind collision: pi=%q, claude=%q", s1, s2)
	}
}

func TestUpsert_UpdatePreservesSlug(t *testing.T) {
	idx := New()
	idx.Upsert(Info{ToolID: "aaa", Slug: "say-hi", Kind: "claude"})
	idx.Upsert(Info{ToolID: "bbb", Slug: "say-hi", Kind: "claude"})

	// Update the second one with a different base slug (e.g., title changed).
	// It should keep its assigned slug "say-hi-2", not get a new one.
	s := idx.Upsert(Info{ToolID: "bbb", Slug: "hello", Kind: "claude", Title: "updated"})
	if s != "say-hi-2" {
		t.Errorf("update should preserve assigned slug, got %q", s)
	}
}

func TestLookupByToolID(t *testing.T) {
	idx := New()
	idx.Upsert(Info{ToolID: "abc-123", Slug: "fix-auth", Kind: "pi"})

	slug := idx.LookupByToolID("pi", "abc-123")
	if slug != "fix-auth" {
		t.Errorf("expected fix-auth, got %q", slug)
	}

	slug = idx.LookupByToolID("pi", "unknown")
	if slug != "" {
		t.Errorf("expected empty for unknown, got %q", slug)
	}
}

func TestRemove(t *testing.T) {
	idx := New()
	idx.Upsert(Info{ToolID: "abc-123", Slug: "fix-auth", Kind: "pi"})

	if !idx.Remove("pi", "abc-123") {
		t.Error("expected remove to return true")
	}
	if idx.Count() != 0 {
		t.Errorf("expected count 0 after remove, got %d", idx.Count())
	}
	if _, ok := idx.Lookup("pi", "fix-auth"); ok {
		t.Error("expected miss after remove")
	}
	if idx.Remove("pi", "abc-123") {
		t.Error("double remove should return false")
	}
}

func TestRemoveByPath(t *testing.T) {
	idx := New()
	idx.Upsert(Info{ToolID: "a", Slug: "one", Kind: "pi", FilePath: "/x/a.jsonl"})
	idx.Upsert(Info{ToolID: "b", Slug: "two", Kind: "pi", FilePath: "/x/b.jsonl"})
	idx.Upsert(Info{ToolID: "c", Slug: "three", Kind: "claude", FilePath: "/y/c.jsonl"})

	if !idx.RemoveByPath("/x/b.jsonl") {
		t.Fatal("expected RemoveByPath to return true for indexed path")
	}
	if idx.Count() != 2 {
		t.Errorf("expected count 2 after remove, got %d", idx.Count())
	}
	if _, ok := idx.Lookup("pi", "two"); ok {
		t.Error("expected miss for removed slug")
	}
	// Sibling entries with same kind unaffected.
	if _, ok := idx.Lookup("pi", "one"); !ok {
		t.Error("sibling pi entry was incorrectly removed")
	}
	// Cross-kind unaffected.
	if _, ok := idx.Lookup("claude", "three"); !ok {
		t.Error("cross-kind entry was incorrectly removed")
	}
	// Reverse-lookup (byToolID) cleared too: Upserting the same toolID
	// at a new slug should yield the new slug, not update-in-place at
	// the old one.
	slug := idx.Upsert(Info{ToolID: "b", Slug: "different", Kind: "pi", FilePath: "/x/b2.jsonl"})
	if slug != "different" {
		t.Errorf("expected fresh slug 'different' (byToolID was cleared), got %q", slug)
	}
	if idx.RemoveByPath("/x/nope.jsonl") {
		t.Error("expected RemoveByPath to return false for unknown path")
	}
}

func TestSlugExists(t *testing.T) {
	idx := New()
	idx.Upsert(Info{ToolID: "abc", Slug: "fix-auth", Kind: "pi"})

	if !idx.SlugExists("pi", "fix-auth") {
		t.Error("expected true")
	}
	if idx.SlugExists("pi", "other") {
		t.Error("expected false for unknown slug")
	}
	if idx.SlugExists("claude", "fix-auth") {
		t.Error("expected false for wrong kind")
	}
}

func TestFindByPrefix(t *testing.T) {
	idx := New()
	idx.Upsert(Info{ToolID: "abc-123", Slug: "fix-auth-bug", Kind: "pi"})
	idx.Upsert(Info{ToolID: "def-456", Slug: "fix-typo", Kind: "pi"})

	info, ok := idx.FindByPrefix("pi", "fix-auth")
	if !ok {
		t.Fatal("expected hit for prefix fix-auth")
	}
	if info.Slug != "fix-auth-bug" {
		t.Errorf("expected fix-auth-bug, got %q", info.Slug)
	}

	_, ok = idx.FindByPrefix("pi", "nonexist")
	if ok {
		t.Error("expected miss for nonexistent prefix")
	}
}

func TestLookupBySlug_AcrossKinds(t *testing.T) {
	idx := New()
	idx.Upsert(Info{ToolID: "aaa", Slug: "fix-auth", Kind: "pi", Title: "pi session"})
	idx.Upsert(Info{ToolID: "bbb", Slug: "add-tests", Kind: "claude", Title: "claude session"})

	info, ok := idx.LookupBySlug("fix-auth")
	if !ok {
		t.Fatal("expected hit for fix-auth")
	}
	if info.Kind != "pi" {
		t.Errorf("expected kind pi, got %q", info.Kind)
	}

	info, ok = idx.LookupBySlug("add-tests")
	if !ok {
		t.Fatal("expected hit for add-tests")
	}
	if info.Kind != "claude" {
		t.Errorf("expected kind claude, got %q", info.Kind)
	}

	_, ok = idx.LookupBySlug("nonexistent")
	if ok {
		t.Error("expected miss for nonexistent slug")
	}

	// "auth" should not match "fix-auth" (substring, not exact slug).
	_, ok = idx.LookupBySlug("auth")
	if ok {
		t.Error("substring should not match")
	}
}

func TestAll(t *testing.T) {
	idx := New()
	idx.Upsert(Info{ToolID: "a", Slug: "one", Kind: "pi", Created: time.Now()})
	idx.Upsert(Info{ToolID: "b", Slug: "two", Kind: "claude", Created: time.Now()})

	all := idx.All()
	if len(all) != 2 {
		t.Fatalf("expected 2, got %d", len(all))
	}
}


