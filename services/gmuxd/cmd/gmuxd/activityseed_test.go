package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/packages/scrollback"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

func writeFileWithMtime(t *testing.T, path string, mt time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mt, mt); err != nil {
		t.Fatal(err)
	}
}

func TestActivitySeedFor_ConversationFileMtime(t *testing.T) {
	dir := t.TempDir()
	conv := filepath.Join(dir, "conv.jsonl")
	want := time.Now().Add(-90 * time.Minute).Truncate(time.Second)
	writeFileWithMtime(t, conv, want)

	got := activitySeedFor(store.Session{ID: "sess-abc", ConversationFile: conv}, nil)
	if got != want.UTC().Format(time.RFC3339) {
		t.Fatalf("seed = %q, want %q", got, want.UTC().Format(time.RFC3339))
	}
}

func TestActivitySeedFor_ScrollbackFallback(t *testing.T) {
	root := t.TempDir()
	sessionDir := func(id string) string { return filepath.Join(root, id) }
	want := time.Now().Add(-30 * time.Minute).Truncate(time.Second)
	writeFileWithMtime(t, filepath.Join(sessionDir("sess-abc"), scrollback.ActiveName), want)

	// No conversation file: falls back to scrollback tee mtime.
	got := activitySeedFor(store.Session{ID: "sess-abc"}, sessionDir)
	if got != want.UTC().Format(time.RFC3339) {
		t.Fatalf("seed = %q, want %q", got, want.UTC().Format(time.RFC3339))
	}
}

func TestActivitySeedFor_NewestWins(t *testing.T) {
	root := t.TempDir()
	sessionDir := func(id string) string { return filepath.Join(root, id) }
	conv := filepath.Join(root, "conv.jsonl")
	older := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	newer := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	writeFileWithMtime(t, conv, older)
	writeFileWithMtime(t, filepath.Join(sessionDir("sess-abc"), scrollback.ActiveName), newer)

	got := activitySeedFor(store.Session{ID: "sess-abc", ConversationFile: conv}, sessionDir)
	if got != newer.UTC().Format(time.RFC3339) {
		t.Fatalf("seed = %q, want newer %q", got, newer.UTC().Format(time.RFC3339))
	}
}

func TestActivitySeedFor_NoFilesReturnsEmpty(t *testing.T) {
	root := t.TempDir()
	sessionDir := func(id string) string { return filepath.Join(root, id) }
	got := activitySeedFor(store.Session{ID: "sess-missing", ConversationFile: "/nope/nope.jsonl"}, sessionDir)
	if got != "" {
		t.Fatalf("expected empty seed when no files exist, got %q", got)
	}
}

func TestActivitySeedFor_CapsFutureMtimeAtNow(t *testing.T) {
	dir := t.TempDir()
	conv := filepath.Join(dir, "conv.jsonl")
	writeFileWithMtime(t, conv, time.Now().Add(48*time.Hour))

	got := activitySeedFor(store.Session{ID: "sess-abc", ConversationFile: conv}, nil)
	parsed, err := time.Parse(time.RFC3339, got)
	if err != nil {
		t.Fatalf("unparseable seed %q: %v", got, err)
	}
	if parsed.After(time.Now().Add(time.Minute)) {
		t.Fatalf("future mtime not capped at now: %q", got)
	}
}
