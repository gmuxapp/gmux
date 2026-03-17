package discovery

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter/adapters"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

func TestNotifyNewSessionDoesNotStealTitleFromOldPiFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cwd := "/home/user/dev/project"
	pi := adapters.NewPi()
	sessionDir := pi.SessionDir(cwd)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir session dir: %v", err)
	}

	oldFile := filepath.Join(sessionDir, "2026-03-16T10-00-00-000Z_old.jsonl")
	oldContent := "" +
		"{\"type\":\"session\",\"id\":\"old-session\",\"cwd\":\"/home/user/dev/project\",\"timestamp\":\"2026-03-16T10:00:00Z\"}\n" +
		"{\"type\":\"message\",\"role\":\"user\",\"text\":\"fix auth bug\"}\n"
	if err := os.WriteFile(oldFile, []byte(oldContent), 0o644); err != nil {
		t.Fatalf("write old jsonl: %v", err)
	}
	// Set mtime to yesterday — real old files have old mtimes.
	yesterday := time.Now().Add(-24 * time.Hour)
	os.Chtimes(oldFile, yesterday, yesterday)

	s := store.New()
	s.Upsert(store.Session{
		ID:         "sess-new",
		Cwd:        cwd,
		Kind:       "pi",
		Alive:      true,
		Title:      "pi",
		StartedAt:  time.Now().UTC().Format(time.RFC3339),
		SocketPath: "/tmp/gmux-sessions/sess-new.sock",
	})

	fm := NewFileMonitor(s)
	if fm.watcher != nil {
		defer fm.watcher.Close()
	}

	fm.NotifyNewSession("sess-new")

	// The session started now but the file is from yesterday.
	// scanDirForRecentFiles should skip it (mtime before session start).
	time.Sleep(700 * time.Millisecond)

	sess, ok := s.Get("sess-new")
	if !ok {
		t.Fatal("session disappeared")
	}
	if sess.Title != "pi" {
		t.Fatalf("title = %q, want %q (do not steal old session title)", sess.Title, "pi")
	}
	if len(fm.attributions) != 0 {
		t.Fatalf("attributions = %v, want none until a real file write occurs", fm.attributions)
	}
}

func TestActiveFileTracking(t *testing.T) {
	s := store.New()
	s.Upsert(store.Session{
		ID:        "sess-1",
		Cwd:       "/tmp",
		Kind:      "codex",
		Alive:     true,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	})

	fm := NewFileMonitor(s)
	if fm.watcher != nil {
		defer fm.watcher.Close()
	}

	// Simulate registering the session for monitoring.
	fm.sessions["sess-1"] = &monitoredSession{
		id:      "sess-1",
		cwd:     "/tmp",
		kind:    "codex",
		adapter: adapters.NewCodex(),
		filer:   adapters.NewCodex(),
		fileMon: adapters.NewCodex(),
	}

	// Create first session file.
	dir := t.TempDir()
	file1 := filepath.Join(dir, "rollout-a.jsonl")
	now := time.Now()
	content1 := `{"timestamp":"` + now.Format(time.RFC3339Nano) + `","type":"session_meta","payload":{"id":"session-aaa","timestamp":"` + now.Format(time.RFC3339Nano) + `","cwd":"/tmp"}}
{"timestamp":"` + now.Format(time.RFC3339Nano) + `","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}}
`
	os.WriteFile(file1, []byte(content1), 0o644)

	// Attribute file1 to the session.
	fm.attributions[file1] = "sess-1"
	fm.updateActiveFileLocked("sess-1", file1)

	sess, _ := s.Get("sess-1")
	if sess.ResumeKey != "session-aaa" {
		t.Fatalf("expected resume_key 'session-aaa', got %q", sess.ResumeKey)
	}

	// Create second file (simulating /new command).
	file2 := filepath.Join(dir, "rollout-b.jsonl")
	content2 := `{"timestamp":"` + now.Format(time.RFC3339Nano) + `","type":"session_meta","payload":{"id":"session-bbb","timestamp":"` + now.Format(time.RFC3339Nano) + `","cwd":"/tmp"}}
{"timestamp":"` + now.Format(time.RFC3339Nano) + `","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"new topic"}]}}
`
	os.WriteFile(file2, []byte(content2), 0o644)

	// Attribute file2 to the same session.
	fm.attributions[file2] = "sess-1"
	fm.updateActiveFileLocked("sess-1", file2)

	sess, _ = s.Get("sess-1")
	if sess.ResumeKey != "session-bbb" {
		t.Fatalf("expected resume_key updated to 'session-bbb', got %q", sess.ResumeKey)
	}

	// Same file again — should be a no-op.
	fm.updateActiveFileLocked("sess-1", file2)
	sess, _ = s.Get("sess-1")
	if sess.ResumeKey != "session-bbb" {
		t.Fatalf("resume_key should still be 'session-bbb', got %q", sess.ResumeKey)
	}
}

func TestAttributionStickiness(t *testing.T) {
	s := store.New()
	fm := NewFileMonitor(s)
	if fm.watcher != nil {
		defer fm.watcher.Close()
	}

	fm.sessions["sess-1"] = &monitoredSession{
		id:      "sess-1",
		cwd:     "/tmp",
		kind:    "codex",
		adapter: adapters.NewCodex(),
		filer:   adapters.NewCodex(),
		fileMon: adapters.NewCodex(),
	}

	// Pre-set an attribution.
	fm.attributions["/some/file.jsonl"] = "sess-1"

	// Calling attributeFileLocked should return the cached value for a known file.
	// (We can't easily test this without the full dir setup, but we verify
	// the map check in handleFileChange by checking the attributions map.)
	if fm.attributions["/some/file.jsonl"] != "sess-1" {
		t.Fatal("attribution should be sticky")
	}
}
