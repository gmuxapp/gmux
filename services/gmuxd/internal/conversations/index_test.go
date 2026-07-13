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
		ConversationID: "abc-123",
		Slug:           "fix-auth",
		Adapter:        "pi",
		Title:          "fix auth bug",
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
		ConversationID: "abc-123",
		Slug:           "fix-auth",
		Adapter:        "pi",
		Title:          "old title",
	})
	idx.Upsert(Info{
		ConversationID: "abc-123",
		Slug:           "fix-auth",
		Adapter:        "pi",
		Title:          "new title",
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
	s1 := idx.Upsert(Info{ConversationID: "aaa", Slug: "say-hi", Adapter: "claude"})
	s2 := idx.Upsert(Info{ConversationID: "bbb", Slug: "say-hi", Adapter: "claude"})
	s3 := idx.Upsert(Info{ConversationID: "ccc", Slug: "say-hi", Adapter: "claude"})

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
	s1 := idx.Upsert(Info{ConversationID: "aaa", Slug: "fix-auth", Adapter: "pi"})
	s2 := idx.Upsert(Info{ConversationID: "bbb", Slug: "fix-auth", Adapter: "claude"})

	// Different kinds should not collide.
	if s1 != "fix-auth" || s2 != "fix-auth" {
		t.Errorf("cross-adapter collision: pi=%q, claude=%q", s1, s2)
	}
}

func TestUpsert_RenameFollowsSlug(t *testing.T) {
	idx := New()
	idx.Upsert(Info{ConversationID: "aaa", Slug: "say-hi", Adapter: "claude"})
	idx.Upsert(Info{ConversationID: "bbb", Slug: "say-hi", Adapter: "claude"})

	// A title change re-keys so the displayed slug always resolves (ADR
	// 0023 §5, the #348 slug-follows-rename semantics). The old suffixed
	// key is released; conversation-ID lookups track the move.
	s := idx.Upsert(Info{ConversationID: "bbb", Slug: "hello", Adapter: "claude", Title: "updated"})
	if s != "hello" {
		t.Errorf("rename should re-key to the new slug, got %q", s)
	}
	if _, ok := idx.Lookup("claude", "say-hi-2"); ok {
		t.Error("old suffixed key should be released on rename")
	}
	if info, ok := idx.Lookup("claude", "bbb"); !ok || info.Key != "hello" {
		t.Errorf("conversation-ID lookup should track the rename, got %+v ok=%v", info, ok)
	}
	// The first conversation's key is untouched.
	if info, ok := idx.Lookup("claude", "say-hi"); !ok || info.ConversationID != "aaa" {
		t.Errorf("unrelated conversation must keep its key, got %+v ok=%v", info, ok)
	}
}

func TestLookupByConversationID(t *testing.T) {
	idx := New()
	idx.Upsert(Info{ConversationID: "abc-123", Slug: "fix-auth", Adapter: "pi"})

	slug := idx.LookupByConversationID("pi", "abc-123")
	if slug != "fix-auth" {
		t.Errorf("expected fix-auth, got %q", slug)
	}

	slug = idx.LookupByConversationID("pi", "unknown")
	if slug != "" {
		t.Errorf("expected empty for unknown, got %q", slug)
	}
}

func TestRemove(t *testing.T) {
	idx := New()
	idx.Upsert(Info{ConversationID: "abc-123", Slug: "fix-auth", Adapter: "pi"})

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

func TestRemoveByRef(t *testing.T) {
	idx := New()
	idx.Upsert(Info{ConversationID: "a", Slug: "one", Adapter: "pi", Ref: "/x/a.jsonl"})
	idx.Upsert(Info{ConversationID: "b", Slug: "two", Adapter: "pi", Ref: "/x/b.jsonl"})
	idx.Upsert(Info{ConversationID: "c", Slug: "three", Adapter: "claude", Ref: "/y/c.jsonl"})
	// Same ref string under a different adapter: refs are only unique
	// within an adapter (ADR 0022), so pi's removal must not touch it.
	idx.Upsert(Info{ConversationID: "d", Slug: "four", Adapter: "claude", Ref: "/x/b.jsonl"})

	if !idx.RemoveByRef("pi", "/x/b.jsonl") {
		t.Fatal("expected RemoveByRef to return true for indexed path")
	}
	if idx.Count() != 3 {
		t.Errorf("expected count 3 after remove, got %d", idx.Count())
	}
	if _, ok := idx.Lookup("pi", "two"); ok {
		t.Error("expected miss for removed slug")
	}
	// Sibling entries with same adapter unaffected.
	if _, ok := idx.Lookup("pi", "one"); !ok {
		t.Error("sibling pi entry was incorrectly removed")
	}
	// Cross-adapter unaffected.
	if _, ok := idx.Lookup("claude", "three"); !ok {
		t.Error("cross-adapter entry was incorrectly removed")
	}
	// Cross-adapter entry with the SAME ref unaffected.
	if _, ok := idx.Lookup("claude", "four"); !ok {
		t.Error("cross-adapter entry sharing the ref string was incorrectly removed")
	}
	// Reverse-lookup (byConversationID) cleared too: Upserting the same conversationID
	// at a new slug should yield the new slug, not update-in-place at
	// the old one.
	slug := idx.Upsert(Info{ConversationID: "b", Slug: "different", Adapter: "pi", Ref: "/x/b2.jsonl"})
	if slug != "different" {
		t.Errorf("expected fresh slug 'different' (byConversationID was cleared), got %q", slug)
	}
	if idx.RemoveByRef("pi", "/x/nope.jsonl") {
		t.Error("expected RemoveByRef to return false for unknown path")
	}
}

func TestSlugExists(t *testing.T) {
	idx := New()
	idx.Upsert(Info{ConversationID: "abc", Slug: "fix-auth", Adapter: "pi"})

	if !idx.SlugExists("pi", "fix-auth") {
		t.Error("expected true")
	}
	if idx.SlugExists("pi", "other") {
		t.Error("expected false for unknown slug")
	}
	if idx.SlugExists("claude", "fix-auth") {
		t.Error("expected false for wrong adapter")
	}
}

func TestUntitledConversationKeepsUUIDKeyButNoDisplaySlug(t *testing.T) {
	idx := New()
	const id = "7e41c769-5efc-4d31-b5f4-4b2a7e800a81"
	key := idx.Upsert(Info{
		ConversationID: id,
		Key:            id,
		Adapter:        "pi",
	})
	if key != id {
		t.Fatalf("Key = %q, want %q", key, id)
	}
	info, ok := idx.Lookup("pi", id)
	if !ok {
		t.Fatal("Lookup by UUID key failed")
	}
	if info.Slug != "" {
		t.Errorf("Slug = %q, want empty for untitled conversation", info.Slug)
	}
	info, ok = idx.FindByPrefix("pi", "7e41c769")
	if !ok || info.ConversationID != id {
		t.Errorf("FindByPrefix(UUID) = %+v, %v; want conversation %q", info, ok, id)
	}
}

func TestFindByPrefix(t *testing.T) {
	idx := New()
	idx.Upsert(Info{ConversationID: "abc-123", Slug: "fix-auth-bug", Adapter: "pi"})
	idx.Upsert(Info{ConversationID: "def-456", Slug: "fix-typo", Adapter: "pi"})

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
	idx.Upsert(Info{ConversationID: "aaa", Slug: "fix-auth", Adapter: "pi", Title: "pi session"})
	idx.Upsert(Info{ConversationID: "bbb", Slug: "add-tests", Adapter: "claude", Title: "claude session"})

	info, ok := idx.LookupBySlug("fix-auth")
	if !ok {
		t.Fatal("expected hit for fix-auth")
	}
	if info.Adapter != "pi" {
		t.Errorf("expected adapter pi, got %q", info.Adapter)
	}

	info, ok = idx.LookupBySlug("add-tests")
	if !ok {
		t.Fatal("expected hit for add-tests")
	}
	if info.Adapter != "claude" {
		t.Errorf("expected adapter claude, got %q", info.Adapter)
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
	idx.Upsert(Info{ConversationID: "a", Slug: "one", Adapter: "pi", Created: time.Now()})
	idx.Upsert(Info{ConversationID: "b", Slug: "two", Adapter: "claude", Created: time.Now()})

	all := idx.All()
	if len(all) != 2 {
		t.Fatalf("expected 2, got %d", len(all))
	}
}

// TestSinkRemoveFiresOnRemovedEvenWhenUnindexed pins the retirement
// seam: the watcher-level sink reports every file-gone event to
// onRemoved — including files the index never held (parse failure,
// CanResume=false, empty cwd) — because a dead session bound to an
// unindexed conversation must still retire when its file is deleted.
// It also still removes indexed entries from the index.
func TestSinkRemoveFiresOnRemovedEvenWhenUnindexed(t *testing.T) {
	idx := New()
	idx.Upsert(Info{ConversationID: "a", Slug: "one", Adapter: "pi", Ref: "/x/a.jsonl"})

	var got []string
	sink := indexSink{idx: idx, a: &dbAdapter{name: "pi"}, onRemoved: func(_, ref string) { got = append(got, ref) }}

	sink.Remove("/x/never-indexed.jsonl")
	sink.Remove("/x/a.jsonl")

	want := []string{"/x/never-indexed.jsonl", "/x/a.jsonl"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("onRemoved should fire for every removal event, got %v want %v", got, want)
	}
	if _, ok := idx.Lookup("pi", "one"); ok {
		t.Error("indexed entry should have been removed from the index")
	}
	if idx.Count() != 0 {
		t.Errorf("index should be empty, count=%d", idx.Count())
	}

	// nil callback must not panic.
	indexSink{idx: idx, a: &dbAdapter{name: "pi"}}.Remove("/x/whatever.jsonl")
}

// TestUpsertUpgradesUntitledKeyWhenTitled pins the titled-key upgrade
// (PR #405 review): a conversation first indexed untitled gets the UUID
// fallback key; when the title arrives, the key must upgrade so the displayed
// slug resolves — while old UUID deep links keep working through the
// conversation-ID fallbacks in Lookup and FindByPrefix.
func TestUpsertUpgradesUntitledKeyWhenTitled(t *testing.T) {
	idx := New()
	const conv = "44444444-4444-4444-8444-444444444444"

	key := idx.Upsert(Info{ConversationID: conv, Key: conv, Adapter: "pi"})
	if key != conv {
		t.Fatalf("untitled key = %q, want UUID fallback", key)
	}

	key = idx.Upsert(Info{ConversationID: conv, Key: "fix-the-bug", Slug: "fix-the-bug", Adapter: "pi", Title: "Fix the bug"})
	if key != "fix-the-bug" {
		t.Fatalf("titled key = %q, want upgraded slug key", key)
	}

	// The displayed slug resolves.
	if info, ok := idx.Lookup("pi", "fix-the-bug"); !ok || info.ConversationID != conv {
		t.Fatalf("Lookup by titled slug failed: ok=%v info=%+v", ok, info)
	}
	// Old UUID deep links keep resolving (exact and prefix).
	if info, ok := idx.Lookup("pi", conv); !ok || info.Key != "fix-the-bug" {
		t.Fatalf("Lookup by conversation ID failed: ok=%v info=%+v", ok, info)
	}
	if info, ok := idx.FindByPrefix("pi", conv[:8]); !ok || info.Key != "fix-the-bug" {
		t.Fatalf("FindByPrefix by conversation-ID prefix failed: ok=%v info=%+v", ok, info)
	}
	// The stale UUID key entry is gone from the key namespace (a NEW
	// conversation could claim it), yet resolution above still works.
	if n := len(idx.All()); n != 1 {
		t.Fatalf("index should hold exactly 1 conversation, got %d", n)
	}

	// A RENAME re-keys again: the displayed slug must always resolve.
	key = idx.Upsert(Info{ConversationID: conv, Key: "ship-the-fix", Slug: "ship-the-fix", Adapter: "pi", Title: "Ship the fix"})
	if key != "ship-the-fix" {
		t.Fatalf("renamed key = %q, want re-keyed slug", key)
	}
	if info, ok := idx.Lookup("pi", "ship-the-fix"); !ok || info.ConversationID != conv {
		t.Fatalf("Lookup by renamed slug failed: ok=%v info=%+v", ok, info)
	}
	if _, ok := idx.Lookup("pi", "fix-the-bug"); ok {
		t.Fatal("old titled key should be released on rename")
	}
	if info, ok := idx.Lookup("pi", conv); !ok || info.Key != "ship-the-fix" {
		t.Fatal("conversation-ID lookup should track the renamed key")
	}

	// A transiently empty slug (parse hiccup) must NOT downgrade the key.
	key = idx.Upsert(Info{ConversationID: conv, Key: "", Adapter: "pi"})
	if key != "ship-the-fix" {
		t.Fatalf("untitled refresh re-keyed to %q, want stable ship-the-fix", key)
	}
}

// TestRenameIntoCollidingSlugKeepsDisplayResolvable pins the PR #405 review
// finding "Collision Key Diverges From Slug": renaming a conversation onto a
// slug another conversation already holds must suffix BOTH the key and the
// displayed slug — otherwise the row advertises a URL that resolves the other
// conversation.
func TestRenameIntoCollidingSlugKeepsDisplayResolvable(t *testing.T) {
	idx := New()
	idx.Upsert(Info{ConversationID: "aaa", Slug: "fix-auth", Adapter: "pi"})
	idx.Upsert(Info{ConversationID: "bbb", Slug: "hello", Adapter: "pi"})

	key := idx.Upsert(Info{ConversationID: "bbb", Slug: "fix-auth", Adapter: "pi", Title: "fix auth"})
	if key != "fix-auth-2" {
		t.Fatalf("colliding rename key = %q, want fix-auth-2", key)
	}
	info, ok := idx.Lookup("pi", "fix-auth-2")
	if !ok || info.ConversationID != "bbb" {
		t.Fatalf("suffixed key lookup: ok=%v info=%+v", ok, info)
	}
	if info.Slug != "fix-auth-2" {
		t.Fatalf("displayed slug = %q, must equal the deduped key", info.Slug)
	}
	// The unsuffixed URL still resolves the ORIGINAL holder.
	if orig, ok := idx.Lookup("pi", "fix-auth"); !ok || orig.ConversationID != "aaa" {
		t.Fatalf("unsuffixed slug must keep resolving aaa: ok=%v info=%+v", ok, orig)
	}
	// A same-content refresh is a stable no-op (no key churn).
	if again := idx.Upsert(Info{ConversationID: "bbb", Slug: "fix-auth", Adapter: "pi", Title: "fix auth"}); again != "fix-auth-2" {
		t.Fatalf("refresh re-keyed to %q, want stable fix-auth-2", again)
	}
}
