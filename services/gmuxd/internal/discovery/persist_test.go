package discovery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter/adapters"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

func TestSaveAttributionsPrunesStaleEntries(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "attributions.json")

	sessions := map[string]*monitoredSession{
		"sess-1": {id: "sess-1", kind: "pi"},
	}
	attributions := map[string]string{
		"/sessions/proj/active.jsonl": "sess-1",
		"/sessions/old/stale.jsonl":   "sess-gone", // not in sessions
	}

	saveAttributionsTo(tmp, attributions, sessions)

	data, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var raw map[string]persistedAttribution
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(raw) != 1 {
		t.Fatalf("expected 1 entry after pruning, got %d: %v", len(raw), raw)
	}
	if raw["/sessions/proj/active.jsonl"].SessionID != "sess-1" {
		t.Error("active attribution should survive pruning")
	}
	if raw["/sessions/proj/active.jsonl"].Kind != "pi" {
		t.Error("kind should be persisted")
	}
}

func TestLoadAttributionsReturnsNilForMissingFile(t *testing.T) {
	result := loadAttributionsFrom(filepath.Join(t.TempDir(), "nope.json"))
	if result != nil {
		t.Fatalf("expected nil, got %v", result)
	}
}

func TestLoadAttributionsReturnsNilForBadJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(path, []byte("not json"), 0o644)

	result := loadAttributionsFrom(path)
	if result != nil {
		t.Fatalf("expected nil for corrupt file, got %v", result)
	}
}

// TestPreSeededAttributionSkipsScrollback is the key behavioral test:
// when a FileMonitor is created with pre-seeded attributions (simulating
// gmuxd restart), handleFileChange should use the persisted attribution
// directly. This means the file's title and ResumeKey are restored
// without needing scrollback-based matching.
func TestPreSeededAttributionSkipsScrollback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cwd := "/home/user/dev/project"
	pi := adapters.NewPi()
	sessionDir := pi.SessionDir(cwd)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Create a session file that was written by a previous gmuxd lifetime.
	filePath := filepath.Join(sessionDir, "2026-03-24T10-00-00-000Z_test.jsonl")
	os.WriteFile(filePath, []byte(
		"{\"type\":\"session\",\"id\":\"test-123\",\"cwd\":\"/home/user/dev/project\",\"timestamp\":\"2026-03-24T10:00:00Z\"}\n"+
			"{\"type\":\"message\",\"id\":\"u1\",\"message\":{\"role\":\"user\",\"content\":[{\"type\":\"text\",\"text\":\"fix the login bug\"}]}}\n",
	), 0o644)
	// Give it an old mtime so it wouldn't match via the "fresh" fallback.
	old := time.Now().Add(-1 * time.Hour)
	os.Chtimes(filePath, old, old)

	// Create FileMonitor with pre-seeded attribution (as if loaded from disk).
	s := store.New()
	s.Upsert(store.Session{
		ID:         "sess-1",
		Cwd:        cwd,
		Kind:       "pi",
		Alive:      true,
		StartedAt:  time.Now().UTC().Format(time.RFC3339),
		SocketPath: "/tmp/fake.sock",
	})

	preSeeded := map[string]string{filePath: "sess-1"}
	fm := NewFileMonitorWithAttributions(s, preSeeded)
	if fm.watcher != nil {
		fm.watcher.Close()
		fm.watcher = nil
	}

	// Register the session for monitoring (simulates what NotifyNewSession does).
	fm.sessions["sess-1"] = &monitoredSession{
		id:      "sess-1",
		cwd:     cwd,
		kind:    "pi",
		adapter: pi,
		fileMon: pi,
		filer:   pi,
		readAll: true,
	}

	// Simulate what scanDirForSessions would do on startup: process the file.
	fm.handleFileChange(filePath)

	// The pre-seeded attribution should have been used. Verify:
	// 1. Title was derived from the file.
	sess, ok := s.Get("sess-1")
	if !ok {
		t.Fatal("session not found")
	}
	if sess.AdapterTitle != "fix the login bug" {
		t.Errorf("expected title 'fix the login bug', got %q", sess.AdapterTitle)
	}
	// 2. ResumeKey is the slug derived from the first user message.
	if sess.ResumeKey != "fix-the-login-bug" {
		t.Errorf("expected resume_key 'fix-the-login-bug', got %q", sess.ResumeKey)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "attributions.json")

	sessions := map[string]*monitoredSession{
		"s1": {id: "s1", kind: "pi"},
		"s2": {id: "s2", kind: "claude"},
	}
	original := map[string]string{
		"/a.jsonl": "s1",
		"/b.jsonl": "s2",
	}

	saveAttributionsTo(tmp, original, sessions)
	loaded := loadAttributionsFrom(tmp)

	if len(loaded) != len(original) {
		t.Fatalf("expected %d entries, got %d", len(original), len(loaded))
	}
	for k, v := range original {
		if loaded[k] != v {
			t.Errorf("key %q: expected %q, got %q", k, v, loaded[k])
		}
	}
}
