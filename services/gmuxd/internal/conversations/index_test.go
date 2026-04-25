package conversations

import (
	"os"
	"path/filepath"
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

// TestScan_SkipsUnchangedFiles verifies that Scan() does not re-parse
// session files whose (mtime, size) match the last indexed snapshot.
// Without this cache, Scan re-reads every JSONL session file every
// interval, which on real systems with hundreds of MB of pi/claude/
// codex history pegs a CPU core.
//
// Strategy: index a real pi session file, then mutate the in-memory
// Info via Upsert (a sentinel marker). A re-Scan of the unchanged
// file must keep the sentinel (cache hit, no re-parse). Bumping the
// file's mtime must invalidate the cache and overwrite the sentinel.
func TestScan_SkipsUnchangedFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // claude/codex roots resolve under HOME
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(home, ".pi", "agent"))

	cwd := t.TempDir()
	piDir := filepath.Join(home, ".pi", "agent", "sessions", "--"+cwdEncode(cwd)+"--")
	if err := os.MkdirAll(piDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionFile := filepath.Join(piDir, "sess.jsonl")
	content := `{"type":"session","version":3,"id":"abc-123","timestamp":"2026-03-15T10:00:00Z","cwd":"` + cwd + `"}` + "\n" +
		`{"type":"message","id":"u1","timestamp":"2026-03-15T10:01:00Z","message":{"role":"user","content":[{"type":"text","text":"original title"}]}}` + "\n"
	if err := os.WriteFile(sessionFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	idx := New()
	idx.Scan()

	info, ok := idx.Lookup("pi", "original-title")
	if !ok {
		t.Fatalf("expected initial scan to index session file; got entries: %v", idx.All())
	}

	// Stamp a sentinel via Upsert. If the next Scan re-parses the file,
	// the sentinel is overwritten with the on-disk title.
	info.Title = "SENTINEL"
	idx.Upsert(info)

	idx.Scan()

	after, _ := idx.Lookup("pi", "original-title")
	if after.Title != "SENTINEL" {
		t.Errorf("expected unchanged file to be skipped (title still SENTINEL), got %q", after.Title)
	}

	// Bump mtime; cache must invalidate and re-parse, overwriting the
	// sentinel with the on-disk title.
	future := time.Now().Add(time.Minute)
	if err := os.Chtimes(sessionFile, future, future); err != nil {
		t.Fatal(err)
	}

	idx.Scan()

	after, _ = idx.Lookup("pi", "original-title")
	if after.Title == "SENTINEL" {
		t.Errorf("expected mtime change to invalidate cache and re-parse; sentinel still present")
	}
	if after.Title != "original title" {
		t.Errorf("expected re-parsed title %q, got %q", "original title", after.Title)
	}
}

// cwdEncode mirrors pi's filesystem encoding (strip leading /, replace
// remaining / with -). Duplicating it here avoids a test dependency on
// the adapters package's unexported helpers.
func cwdEncode(cwd string) string {
	s := cwd
	if len(s) > 0 && s[0] == '/' {
		s = s[1:]
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			out = append(out, '-')
		} else {
			out = append(out, s[i])
		}
	}
	return string(out)
}

