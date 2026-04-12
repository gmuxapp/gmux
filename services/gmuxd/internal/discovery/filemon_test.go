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
	t.Setenv("GMUX_SOCKET_DIR", filepath.Join(home, "gmux-sessions"))

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
	fm.syncFileMetadataLocked("sess-1", file1)

	sess, _ := s.Get("sess-1")
	if sess.Slug != "hello" {
		t.Fatalf("expected slug 'hello', got %q", sess.Slug)
	}
	if sess.AdapterTitle != "hello" {
		t.Fatalf("expected title 'hello', got %q", sess.AdapterTitle)
	}

	// Create second file (simulating /new command).
	file2 := filepath.Join(dir, "rollout-b.jsonl")
	content2 := `{"timestamp":"` + now.Format(time.RFC3339Nano) + `","type":"session_meta","payload":{"id":"session-bbb","timestamp":"` + now.Format(time.RFC3339Nano) + `","cwd":"/tmp"}}
{"timestamp":"` + now.Format(time.RFC3339Nano) + `","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"new topic"}]}}
`
	os.WriteFile(file2, []byte(content2), 0o644)

	// Attribute file2 to the same session.
	fm.attributions[file2] = "sess-1"
	fm.syncFileMetadataLocked("sess-1", file2)

	sess, _ = s.Get("sess-1")
	if sess.Slug != "new-topic" {
		t.Fatalf("expected slug updated to 'new-topic', got %q", sess.Slug)
	}
	if sess.AdapterTitle != "new topic" {
		t.Fatalf("expected title updated to 'new topic', got %q", sess.AdapterTitle)
	}

	// Same file again — should be a no-op.
	fm.syncFileMetadataLocked("sess-1", file2)
	sess, _ = s.Get("sess-1")
	if sess.Slug != "new-topic" {
		t.Fatalf("slug should still be 'new-topic', got %q", sess.Slug)
	}
	if sess.AdapterTitle != "new topic" {
		t.Fatalf("title should still be 'new topic', got %q", sess.AdapterTitle)
	}
}

func TestActiveFileChangeResetsTitleForPi(t *testing.T) {
	fm, s, dir := setupPiFileMonitor(t)

	// First session file with a user message.
	file1 := filepath.Join(dir, "session-aaa.jsonl")
	simulateFileWrite(t, fm, "sess-pi", file1,
		`{"type":"session","id":"aaa","cwd":"/home/user/dev/project","timestamp":"2026-04-12T10:00:00Z"}`,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"fix the auth bug"}]}}`,
	)
	sess, _ := s.Get("sess-pi")
	if sess.AdapterTitle != "fix the auth bug" {
		t.Fatalf("expected title 'fix the auth bug', got %q", sess.AdapterTitle)
	}

	// /new creates a second file. Simulate attribution via the throttled path.
	file2 := filepath.Join(dir, "session-bbb.jsonl")
	simulateFileWrite(t, fm, "sess-pi", file2,
		`{"type":"session","id":"bbb","cwd":"/home/user/dev/project","timestamp":"2026-04-12T11:00:00Z"}`,
		`{"type":"message","id":"u2","message":{"role":"user","content":[{"type":"text","text":"refactor the login flow"}]}}`,
	)
	sess, _ = s.Get("sess-pi")
	if sess.AdapterTitle != "refactor the login flow" {
		t.Fatalf("expected title 'refactor the login flow' after /new, got %q", sess.AdapterTitle)
	}
}

// --- Pi status lifecycle tests ---
// These test the full filemon pipeline: file write → ParseNewLines → store update.

// setupPiFileMonitor creates a FileMonitor with a pi session pre-registered,
// returns the monitor, store, session dir, and the session JSONL path.
func setupPiFileMonitor(t *testing.T) (*FileMonitor, *store.Store, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)

	cwd := "/home/user/dev/project"
	pi := adapters.NewPi()
	sessionDir := pi.SessionDir(cwd)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	s := store.New()
	s.Upsert(store.Session{
		ID:         "sess-pi",
		Cwd:        cwd,
		Kind:       "pi",
		Alive:      true,
		StartedAt:  time.Now().UTC().Format(time.RFC3339),
		SocketPath: "/tmp/fake.sock",
	})

	fm := NewFileMonitor(s)
	if fm.watcher != nil {
		defer fm.watcher.Close()
		fm.watcher.Close()
		fm.watcher = nil // disable real inotify for unit tests
	}

	fm.sessions["sess-pi"] = &monitoredSession{
		id:      "sess-pi",
		cwd:     cwd,
		kind:    "pi",
		adapter: pi,
		fileMon: pi,
		filer:   pi,
	}

	return fm, s, sessionDir
}

// simulateFileWrite writes lines to a file and calls handleFileChange to
// simulate what inotify would do. Pre-sets attribution if not already set.
func simulateFileWrite(t *testing.T, fm *FileMonitor, sessionID, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open file: %v", err)
	}
	for _, l := range lines {
		f.WriteString(l + "\n")
	}
	f.Close()
	// Pre-set attribution to bypass scrollback-based matching (no real runner).
	if _, ok := fm.attributions[path]; !ok {
		fm.attributions[path] = sessionID
	}
	fm.handleFileChange(path)
}

func TestPiAbortClearsWorking(t *testing.T) {
	fm, s, dir := setupPiFileMonitor(t)
	path := filepath.Join(dir, "test.jsonl")

	// Session header + user message → working.
	simulateFileWrite(t, fm, "sess-pi", path,
		`{"type":"session","id":"test-123","cwd":"/home/user/dev/project","timestamp":"2026-03-19T10:00:00Z"}`,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"fix bug"}]}}`,
	)
	sess, _ := s.Get("sess-pi")
	if sess.Status == nil || !sess.Status.Working {
		t.Fatal("expected working=true after user message")
	}

	// toolUse → still working.
	simulateFileWrite(t, fm, "sess-pi", path,
		`{"type":"message","id":"a1","message":{"role":"assistant","stopReason":"toolUse","content":[]}}`,
	)
	sess, _ = s.Get("sess-pi")
	if sess.Status == nil || !sess.Status.Working {
		t.Fatal("expected working=true after toolUse")
	}

	// User presses Esc → aborted → idle.
	simulateFileWrite(t, fm, "sess-pi", path,
		`{"type":"message","id":"a2","message":{"role":"assistant","stopReason":"aborted","content":[]}}`,
	)
	sess, _ = s.Get("sess-pi")
	if sess.Status != nil {
		t.Fatalf("expected nil status (idle) after abort, got %+v", sess.Status)
	}
}

func TestPiSingleErrorKeepsWorking(t *testing.T) {
	fm, s, dir := setupPiFileMonitor(t)
	path := filepath.Join(dir, "test.jsonl")

	simulateFileWrite(t, fm, "sess-pi", path,
		`{"type":"session","id":"test-123","cwd":"/home/user/dev/project","timestamp":"2026-03-19T10:00:00Z"}`,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"fix bug"}]}}`,
	)
	sess, _ := s.Get("sess-pi")
	if sess.Status == nil || !sess.Status.Working {
		t.Fatal("expected working=true after user message")
	}

	// Single error → no state change (retry expected).
	simulateFileWrite(t, fm, "sess-pi", path,
		`{"type":"message","id":"a1","message":{"role":"assistant","stopReason":"error","content":[]}}`,
	)
	sess, _ = s.Get("sess-pi")
	if sess.Status == nil || !sess.Status.Working {
		t.Fatal("expected working=true after single error (retry pending)")
	}
}

func TestPiExhaustedErrorSetsErrorState(t *testing.T) {
	fm, s, dir := setupPiFileMonitor(t)
	path := filepath.Join(dir, "test.jsonl")

	simulateFileWrite(t, fm, "sess-pi", path,
		`{"type":"session","id":"test-123","cwd":"/home/user/dev/project","timestamp":"2026-03-19T10:00:00Z"}`,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"fix bug"}]}}`,
	)

	// 4 consecutive errors = retries exhausted → error state (not working, error=true).
	simulateFileWrite(t, fm, "sess-pi", path,
		`{"type":"message","id":"a1","message":{"role":"assistant","stopReason":"error","content":[]}}`,
	)
	simulateFileWrite(t, fm, "sess-pi", path,
		`{"type":"message","id":"a2","message":{"role":"assistant","stopReason":"error","content":[]}}`,
	)
	simulateFileWrite(t, fm, "sess-pi", path,
		`{"type":"message","id":"a3","message":{"role":"assistant","stopReason":"error","content":[]}}`,
	)
	simulateFileWrite(t, fm, "sess-pi", path,
		`{"type":"message","id":"a4","message":{"role":"assistant","stopReason":"error","content":[]}}`,
	)
	sess, _ := s.Get("sess-pi")
	if sess.Status == nil {
		t.Fatal("expected error status after exhausted retries, got nil")
	}
	if sess.Status.Working {
		t.Error("expected working=false after exhausted retries")
	}
	if !sess.Status.Error {
		t.Error("expected error=true after exhausted retries")
	}
}

func TestPiErrorAutoRetryResetsWorking(t *testing.T) {
	fm, s, dir := setupPiFileMonitor(t)
	path := filepath.Join(dir, "test.jsonl")

	simulateFileWrite(t, fm, "sess-pi", path,
		`{"type":"session","id":"test-123","cwd":"/home/user/dev/project","timestamp":"2026-03-19T10:00:00Z"}`,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"fix bug"}]}}`,
	)

	// Error followed by auto-retry (both in same batch).
	simulateFileWrite(t, fm, "sess-pi", path,
		`{"type":"message","id":"a1","message":{"role":"assistant","stopReason":"error","content":[]}}`,
		`{"type":"message","id":"a2","message":{"role":"assistant","stopReason":"toolUse","content":[]}}`,
	)
	sess, _ := s.Get("sess-pi")
	if sess.Status == nil || !sess.Status.Working {
		t.Fatal("expected working=true after error auto-retry (toolUse)")
	}
}

func TestPiNameDuringWorkPreservesStatus(t *testing.T) {
	fm, s, dir := setupPiFileMonitor(t)
	path := filepath.Join(dir, "test.jsonl")

	simulateFileWrite(t, fm, "sess-pi", path,
		`{"type":"session","id":"test-123","cwd":"/home/user/dev/project","timestamp":"2026-03-19T10:00:00Z"}`,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"fix bug"}]}}`,
	)

	// /name while working → title changes, working preserved.
	simulateFileWrite(t, fm, "sess-pi", path,
		`{"type":"session_info","name":"Bug fix session"}`,
	)
	sess, _ := s.Get("sess-pi")
	if sess.AdapterTitle != "Bug fix session" {
		t.Errorf("expected title 'Bug fix session', got %q", sess.AdapterTitle)
	}
	if sess.Status == nil || !sess.Status.Working {
		t.Fatal("expected working=true preserved after /name")
	}

	// Subsequent assistant turn should not overwrite the /name title.
	simulateFileWrite(t, fm, "sess-pi", path,
		`{"type":"message","id":"a1","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"Done."}]}}`,
	)
	sess, _ = s.Get("sess-pi")
	if sess.AdapterTitle != "Bug fix session" {
		t.Errorf("/name title should persist after assistant turn, got %q", sess.AdapterTitle)
	}
}

func TestPiFullLifecycle(t *testing.T) {
	fm, s, dir := setupPiFileMonitor(t)
	path := filepath.Join(dir, "test.jsonl")

	// 1. Session starts.
	simulateFileWrite(t, fm, "sess-pi", path,
		`{"type":"session","id":"test-123","cwd":"/home/user/dev/project","timestamp":"2026-03-19T10:00:00Z"}`,
	)
	sess, _ := s.Get("sess-pi")
	if sess.Status != nil {
		t.Fatalf("expected nil status on fresh session, got %+v", sess.Status)
	}

	// 2. User message → working.
	simulateFileWrite(t, fm, "sess-pi", path,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"fix bug"}]}}`,
	)
	sess, _ = s.Get("sess-pi")
	if sess.Status == nil || !sess.Status.Working {
		t.Fatal("expected working after user message")
	}
	if sess.AdapterTitle != "fix bug" {
		t.Errorf("expected title 'fix bug', got %q", sess.AdapterTitle)
	}

	// 3. Tool loop.
	simulateFileWrite(t, fm, "sess-pi", path,
		`{"type":"message","id":"a1","message":{"role":"assistant","stopReason":"toolUse","content":[]}}`,
		`{"type":"message","id":"tr1","message":{"role":"toolResult","content":""}}`,
	)
	sess, _ = s.Get("sess-pi")
	if sess.Status == nil || !sess.Status.Working {
		t.Fatal("expected working during tool loop")
	}

	// 4. Error + auto-retry.
	simulateFileWrite(t, fm, "sess-pi", path,
		`{"type":"message","id":"a2","message":{"role":"assistant","stopReason":"error","content":[]}}`,
		`{"type":"message","id":"a3","message":{"role":"assistant","stopReason":"toolUse","content":[]}}`,
		`{"type":"message","id":"tr2","message":{"role":"toolResult","content":""}}`,
	)
	sess, _ = s.Get("sess-pi")
	if sess.Status == nil || !sess.Status.Working {
		t.Fatal("expected working after error auto-retry")
	}

	// 5. Agent finishes.
	simulateFileWrite(t, fm, "sess-pi", path,
		`{"type":"message","id":"a4","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"Done."}]}}`,
	)
	sess, _ = s.Get("sess-pi")
	if sess.Status != nil {
		t.Fatalf("expected idle after stop, got %+v", sess.Status)
	}

	// 6. Second turn.
	simulateFileWrite(t, fm, "sess-pi", path,
		`{"type":"message","id":"u2","message":{"role":"user","content":[{"type":"text","text":"also fix login"}]}}`,
	)
	sess, _ = s.Get("sess-pi")
	if sess.Status == nil || !sess.Status.Working {
		t.Fatal("expected working after second user message")
	}
	// Title should still be from first user message.
	if sess.AdapterTitle != "fix bug" {
		t.Errorf("title should stay 'fix bug', got %q", sess.AdapterTitle)
	}

	// 7. User aborts.
	simulateFileWrite(t, fm, "sess-pi", path,
		`{"type":"message","id":"a5","message":{"role":"assistant","stopReason":"aborted","content":[]}}`,
	)
	sess, _ = s.Get("sess-pi")
	if sess.Status != nil {
		t.Fatalf("expected idle after abort, got %+v", sess.Status)
	}
}

func TestPiExhaustedErrorRecovery(t *testing.T) {
	fm, s, dir := setupPiFileMonitor(t)
	path := filepath.Join(dir, "test.jsonl")

	simulateFileWrite(t, fm, "sess-pi", path,
		`{"type":"session","id":"test-123","cwd":"/home/user/dev/project","timestamp":"2026-03-19T10:00:00Z"}`,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"fix bug"}]}}`,
	)

	// Exhaust retries.
	for i := range 4 {
		simulateFileWrite(t, fm, "sess-pi", path,
			`{"type":"message","id":"e`+string(rune('1'+i))+`","message":{"role":"assistant","stopReason":"error","content":[]}}`,
		)
	}
	sess, _ := s.Get("sess-pi")
	if sess.Status == nil || !sess.Status.Error {
		t.Fatal("expected error state after exhausted retries")
	}

	// User sends new message → error clears, working resumes.
	simulateFileWrite(t, fm, "sess-pi", path,
		`{"type":"message","id":"u2","message":{"role":"user","content":[{"type":"text","text":"continue"}]}}`,
	)
	sess, _ = s.Get("sess-pi")
	if sess.Status == nil || !sess.Status.Working {
		t.Fatal("expected working after user message")
	}
	if sess.Status.Error {
		t.Fatal("expected error=false after user message clears error state")
	}

	// Agent succeeds this time.
	simulateFileWrite(t, fm, "sess-pi", path,
		`{"type":"message","id":"a5","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"Done."}]}}`,
	)
	sess, _ = s.Get("sess-pi")
	if sess.Status != nil {
		t.Fatalf("expected nil status after stop, got %+v", sess.Status)
	}
}

func TestReadAllSuppressesUnread(t *testing.T) {
	fm, s, dir := setupPiFileMonitor(t)
	path := filepath.Join(dir, "test.jsonl")

	// Write a complete conversation to the file (simulating pre-existing content).
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"type":"session","id":"test-123","cwd":"/home/user/dev/project","timestamp":"2026-03-19T10:00:00Z"}` + "\n")
	f.WriteString(`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"fix bug"}]}}` + "\n")
	f.WriteString(`{"type":"message","id":"a1","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"Done."}]}}` + "\n")
	f.Close()

	// Mark readAll to simulate session restart (full file re-read).
	fm.sessions["sess-pi"].readAll = true
	fm.attributions[path] = "sess-pi"
	fm.handleFileChange(path)

	sess, _ := s.Get("sess-pi")
	if sess.Unread {
		t.Fatal("readAll parse should not set unread from historical assistant turns")
	}

	// Subsequent incremental writes should still set unread normally.
	simulateFileWrite(t, fm, "sess-pi", path,
		`{"type":"message","id":"u2","message":{"role":"user","content":[{"type":"text","text":"more work"}]}}`,
		`{"type":"message","id":"a2","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"Also done."}]}}`,
	)
	sess, _ = s.Get("sess-pi")
	if !sess.Unread {
		t.Fatal("incremental writes should still set unread")
	}
}

// TestAttributionAcrossDirectories verifies that a session file in a
// directory other than SessionDir(cwd) is still attributed to the
// correct session. This simulates grove worktrees, /resume across
// cwds, or any scenario where the tool writes to a different dir.
func TestAttributionAcrossDirectories(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cwd := "/home/user/dev/gmux"
	otherCwd := "/home/user/dev/gmux/.grove/ws-1"
	pi := adapters.NewPi()

	// Create session dirs for both cwds.
	primaryDir := pi.SessionDir(cwd)
	otherDir := pi.SessionDir(otherCwd)
	if err := os.MkdirAll(primaryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(otherDir, 0o755); err != nil {
		t.Fatal(err)
	}

	s := store.New()
	s.Upsert(store.Session{
		ID:         "sess-ws1",
		Cwd:        cwd,
		Kind:       "pi",
		Alive:      true,
		StartedAt:  time.Now().UTC().Format(time.RFC3339),
		SocketPath: "/tmp/fake.sock",
	})

	fm := NewFileMonitor(s)
	if fm.watcher != nil {
		fm.watcher.Close()
		fm.watcher = nil
	}

	fm.sessions["sess-ws1"] = &monitoredSession{
		id:      "sess-ws1",
		cwd:     cwd,
		kind:    "pi",
		adapter: pi,
		fileMon: pi,
		filer:   pi,
	}

	// Write a session file in the OTHER dir (not the session's cwd dir).
	// Pre-set attribution to simulate what scrollback matching would do.
	path := filepath.Join(otherDir, "test.jsonl")
	simulateFileWrite(t, fm, "sess-ws1", path,
		`{"type":"session","id":"ws1-123","cwd":"`+otherCwd+`","timestamp":"2026-04-12T10:00:00Z"}`,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"fix the build"}]}}`,
	)

	sess, ok := s.Get("sess-ws1")
	if !ok {
		t.Fatal("session disappeared")
	}
	if sess.AdapterTitle != "fix the build" {
		t.Errorf("expected title 'fix the build', got %q", sess.AdapterTitle)
	}
	if sess.Status == nil || !sess.Status.Working {
		t.Fatal("expected working=true after user message from cross-dir file")
	}
}

// TestRecentFileScanSkipsOldFiles verifies that the initial dir scan
// on session registration only processes files modified after the
// session started, preventing stale title attribution.
func TestRecentFileScanSkipsOldFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GMUX_SOCKET_DIR", filepath.Join(home, "gmux-sessions"))

	cwd := "/home/user/dev/project"
	otherCwd := "/home/user/other/project"
	pi := adapters.NewPi()

	// Create dirs.
	for _, c := range []string{cwd, otherCwd} {
		if err := os.MkdirAll(pi.SessionDir(c), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Write an old file in a DIFFERENT session dir. Without the mtime
	// filter, the broader directory scanning would find this file and
	// attribute it to the new session.
	oldFile := filepath.Join(pi.SessionDir(otherCwd), "old.jsonl")
	os.WriteFile(oldFile, []byte(
		`{"type":"session","id":"old","cwd":"`+otherCwd+`","timestamp":"2026-03-01T10:00:00Z"}`+"\n"+
			`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"old task"}]}}`+"\n",
	), 0o644)
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
		SocketPath: "/tmp/fake.sock",
	})

	fm := NewFileMonitor(s)
	if fm.watcher != nil {
		defer fm.watcher.Close()
	}

	fm.NotifyNewSession("sess-new")
	time.Sleep(500 * time.Millisecond)

	sess, _ := s.Get("sess-new")
	if sess.Title != "pi" {
		t.Fatalf("title = %q, want %q (old file in different dir should not be attributed)", sess.Title, "pi")
	}
	if len(fm.attributions) != 0 {
		t.Fatalf("attributions = %v, want none", fm.attributions)
	}
}

// TestTryAttributeUnmatchedPicksUpCandidates verifies that files added
// to the candidate set (via handleFileChange on unattributed files) are
// picked up by tryAttributeUnmatched and attributed to the correct session.
func TestTryAttributeUnmatchedPicksUpCandidates(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cwd := "/home/user/dev/project"
	claude := adapters.NewClaude()
	sessionDir := claude.SessionDir(cwd)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}

	s := store.New()
	s.Upsert(store.Session{
		ID:         "sess-1",
		Cwd:        cwd,
		Kind:       "claude",
		Alive:      true,
		StartedAt:  time.Now().UTC().Format(time.RFC3339),
		SocketPath: "/tmp/fake.sock",
	})

	fm := NewFileMonitorWithAttributions(s, nil)
	if fm.watcher != nil {
		fm.watcher.Close()
		fm.watcher = nil
	}

	fm.sessions["sess-1"] = &monitoredSession{
		id:      "sess-1",
		cwd:     cwd,
		kind:    "claude",
		adapter: claude,
		fileMon: claude,
		filer:   claude,
		readAll: true,
	}

	// Write a file and trigger handleFileChange. Since the file is not
	// attributed, it should be added to candidates (not processed yet).
	path := filepath.Join(sessionDir, "test-session.jsonl")
	os.WriteFile(path, []byte(
		`{"type":"user","sessionId":"abc","timestamp":"2026-03-19T10:00:00Z","cwd":"/home/user/dev/project","message":{"role":"user","content":"fix bug"},"uuid":"u1"}`+"\n",
	), 0o644)

	fm.handleFileChange(path)

	// File should be in candidates, not in attributions.
	if _, ok := fm.attributions[path]; ok {
		t.Fatal("file should not be attributed yet")
	}
	if !fm.candidateFiles[path] {
		t.Fatal("file should be in candidate set")
	}

	// Now run tryAttributeUnmatched. Claude has no FileAttributor, so
	// trivial attribution to the single candidate session.
	fm.tryAttributeUnmatched()

	if fm.attributions[path] != "sess-1" {
		t.Fatalf("expected attribution to sess-1, got %q", fm.attributions[path])
	}
	if fm.candidateFiles[path] {
		t.Fatal("file should be removed from candidate set after attribution")
	}

	// Verify the file was processed (title/status extracted).
	sess, _ := s.Get("sess-1")
	if sess.Status == nil || !sess.Status.Working {
		t.Fatal("expected working=true after attribution + file processing")
	}
}

// TestHandleFileChangeProcessesAttributedImmediately verifies that
// already-attributed files are processed on the event loop without
// going through the throttled attribution path.
func TestHandleFileChangeProcessesAttributedImmediately(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cwd := "/home/user/dev/project"
	claude := adapters.NewClaude()
	sessionDir := claude.SessionDir(cwd)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}

	s := store.New()
	s.Upsert(store.Session{
		ID:         "sess-1",
		Cwd:        cwd,
		Kind:       "claude",
		Alive:      true,
		StartedAt:  time.Now().UTC().Format(time.RFC3339),
		SocketPath: "/tmp/fake.sock",
	})

	path := filepath.Join(sessionDir, "test-session.jsonl")
	// Pre-seed attribution.
	fm := NewFileMonitorWithAttributions(s, map[string]string{path: "sess-1"})
	if fm.watcher != nil {
		fm.watcher.Close()
		fm.watcher = nil
	}

	fm.sessions["sess-1"] = &monitoredSession{
		id:      "sess-1",
		cwd:     cwd,
		kind:    "claude",
		adapter: claude,
		fileMon: claude,
		filer:   claude,
		readAll: true,
	}

	// Write and handle. Since it's attributed, it should be processed
	// immediately without going through candidates.
	os.WriteFile(path, []byte(
		`{"type":"user","sessionId":"abc","timestamp":"2026-03-19T10:00:00Z","cwd":"/home/user/dev/project","message":{"role":"user","content":"fix bug"},"uuid":"u1"}`+"\n",
	), 0o644)

	fm.handleFileChange(path)

	// Should NOT be in candidates.
	if fm.candidateFiles[path] {
		t.Fatal("attributed file should not be added to candidates")
	}

	// Should be processed immediately.
	sess, _ := s.Get("sess-1")
	if sess.Status == nil || !sess.Status.Working {
		t.Fatal("expected working=true after immediate processing")
	}
}

// TestOrphanedCandidatesPrunedAfterSessionDeath verifies that candidate
// files are removed when no live session can claim them (the session died
// or was never relevant). Without this, tryAttributeUnmatched would
// retry forever.
func TestOrphanedCandidatesPrunedAfterSessionDeath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cwd := "/home/user/dev/project"
	pi := adapters.NewPi()
	sessionDir := pi.SessionDir(cwd)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}

	s := store.New()
	s.Upsert(store.Session{
		ID:         "sess-1",
		Cwd:        cwd,
		Kind:       "pi",
		Alive:      true,
		StartedAt:  time.Now().UTC().Format(time.RFC3339),
		SocketPath: "/tmp/fake.sock",
	})

	fm := NewFileMonitorWithAttributions(s, nil)
	if fm.watcher != nil {
		fm.watcher.Close()
		fm.watcher = nil
	}

	fm.sessions["sess-1"] = &monitoredSession{
		id:      "sess-1",
		cwd:     cwd,
		kind:    "pi",
		adapter: pi,
		fileMon: pi,
		filer:   pi,
	}

	// Add a candidate file.
	path := filepath.Join(sessionDir, "test.jsonl")
	os.WriteFile(path, []byte(`{"type":"session"}\n`), 0o644)
	fm.candidateFiles[path] = true

	// Session dies.
	fm.NotifySessionDied("sess-1")

	// tryAttributeUnmatched should prune the orphaned candidate
	// and return false (nothing left to retry).
	if fm.tryAttributeUnmatched() {
		t.Fatal("expected tryAttributeUnmatched to return false after session death")
	}
	if len(fm.candidateFiles) != 0 {
		t.Fatalf("expected empty candidateFiles, got %v", fm.candidateFiles)
	}
}

// TestStaleAttributionRequeuesAsCandidate verifies that when a file
// event arrives for a file attributed to a dead session, the stale
// attribution is cleared and the file is re-queued as a candidate.
func TestStaleAttributionRequeuesAsCandidate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cwd := "/home/user/dev/project"
	pi := adapters.NewPi()
	sessionDir := pi.SessionDir(cwd)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}

	s := store.New()
	s.Upsert(store.Session{
		ID:         "sess-2",
		Cwd:        cwd,
		Kind:       "pi",
		Alive:      true,
		StartedAt:  time.Now().UTC().Format(time.RFC3339),
		SocketPath: "/tmp/fake.sock",
	})

	// Pre-seed a stale attribution to sess-dead (not in sessions map).
	path := filepath.Join(sessionDir, "test.jsonl")
	fm := NewFileMonitorWithAttributions(s, map[string]string{path: "sess-dead"})
	if fm.watcher != nil {
		fm.watcher.Close()
		fm.watcher = nil
	}

	// Only sess-2 is live.
	fm.sessions["sess-2"] = &monitoredSession{
		id:      "sess-2",
		cwd:     cwd,
		kind:    "pi",
		adapter: pi,
		fileMon: pi,
		filer:   pi,
	}

	os.WriteFile(path, []byte(`{"type":"session"}\n`), 0o644)
	fm.handleFileChange(path)

	// Stale attribution should be cleared, file re-queued.
	if _, ok := fm.attributions[path]; ok {
		t.Fatal("stale attribution should have been cleared")
	}
	if !fm.candidateFiles[path] {
		t.Fatal("file should be in candidate set after stale attribution cleared")
	}
}

// --- Claude status lifecycle tests ---

func setupClaudeFileMonitor(t *testing.T) (*FileMonitor, *store.Store, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)

	cwd := "/home/user/dev/project"
	claude := adapters.NewClaude()
	sessionDir := claude.SessionDir(cwd)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	s := store.New()
	s.Upsert(store.Session{
		ID:         "sess-claude",
		Cwd:        cwd,
		Kind:       "claude",
		Alive:      true,
		StartedAt:  time.Now().UTC().Format(time.RFC3339),
		SocketPath: "/tmp/fake.sock",
	})

	fm := NewFileMonitor(s)
	if fm.watcher != nil {
		fm.watcher.Close()
		fm.watcher = nil
	}

	fm.sessions["sess-claude"] = &monitoredSession{
		id:      "sess-claude",
		cwd:     cwd,
		kind:    "claude",
		adapter: claude,
		fileMon: claude,
		filer:   claude,
	}

	return fm, s, sessionDir
}

func TestClaudeAbortClearsWorking(t *testing.T) {
	fm, s, dir := setupClaudeFileMonitor(t)
	path := filepath.Join(dir, "test-session.jsonl")

	// User message → working.
	simulateFileWrite(t, fm, "sess-claude", path,
		`{"type":"user","sessionId":"abc","timestamp":"2026-03-19T10:00:00Z","cwd":"/home/user/dev/project","message":{"role":"user","content":"fix bug"},"uuid":"u1"}`,
	)
	sess, _ := s.Get("sess-claude")
	if sess.Status == nil || !sess.Status.Working {
		t.Fatal("expected working=true after user message")
	}

	// Tool use → still working.
	simulateFileWrite(t, fm, "sess-claude", path,
		`{"type":"assistant","sessionId":"abc","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"bash","input":{}}],"stop_reason":null},"uuid":"a1"}`,
	)
	sess, _ = s.Get("sess-claude")
	if sess.Status == nil || !sess.Status.Working {
		t.Fatal("expected working=true after tool_use")
	}

	// User presses Esc → stop_sequence → idle.
	simulateFileWrite(t, fm, "sess-claude", path,
		`{"type":"assistant","sessionId":"abc","message":{"role":"assistant","content":[{"type":"text","text":"I was..."}],"stop_reason":"stop_sequence"},"uuid":"a2"}`,
	)
	sess, _ = s.Get("sess-claude")
	if sess.Status != nil {
		t.Fatalf("expected nil status (idle) after abort, got %+v", sess.Status)
	}
}

func TestClaudeFullLifecycle(t *testing.T) {
	fm, s, dir := setupClaudeFileMonitor(t)
	path := filepath.Join(dir, "test-session.jsonl")

	// 1. User message → working.
	simulateFileWrite(t, fm, "sess-claude", path,
		`{"type":"user","sessionId":"abc","timestamp":"2026-03-19T10:00:00Z","cwd":"/home/user/dev/project","message":{"role":"user","content":"fix bug"},"uuid":"u1"}`,
	)
	sess, _ := s.Get("sess-claude")
	if sess.Status == nil || !sess.Status.Working {
		t.Fatal("expected working after user message")
	}

	// 2. Tool loop.
	simulateFileWrite(t, fm, "sess-claude", path,
		`{"type":"assistant","sessionId":"abc","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"bash","input":{}}],"stop_reason":null},"uuid":"a1"}`,
	)
	sess, _ = s.Get("sess-claude")
	if sess.Status == nil || !sess.Status.Working {
		t.Fatal("expected working during tool loop")
	}

	// 3. Thinking (intermediate) — no change.
	simulateFileWrite(t, fm, "sess-claude", path,
		`{"type":"assistant","sessionId":"abc","message":{"role":"assistant","content":[{"type":"thinking","thinking":"let me think..."}],"stop_reason":null},"uuid":"a2"}`,
	)
	sess, _ = s.Get("sess-claude")
	if sess.Status == nil || !sess.Status.Working {
		t.Fatal("expected working preserved after thinking-only")
	}

	// 4. More tool use.
	simulateFileWrite(t, fm, "sess-claude", path,
		`{"type":"assistant","sessionId":"abc","message":{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"read","input":{}}],"stop_reason":null},"uuid":"a3"}`,
	)
	sess, _ = s.Get("sess-claude")
	if sess.Status == nil || !sess.Status.Working {
		t.Fatal("expected working during second tool use")
	}

	// 5. Agent finishes (end_turn).
	simulateFileWrite(t, fm, "sess-claude", path,
		`{"type":"assistant","sessionId":"abc","message":{"role":"assistant","content":[{"type":"text","text":"Done."}],"stop_reason":"end_turn"},"uuid":"a4"}`,
	)
	sess, _ = s.Get("sess-claude")
	if sess.Status != nil {
		t.Fatalf("expected idle after end_turn, got %+v", sess.Status)
	}

	// 6. Second turn.
	simulateFileWrite(t, fm, "sess-claude", path,
		`{"type":"user","sessionId":"abc","message":{"role":"user","content":"also fix login"},"uuid":"u2"}`,
	)
	sess, _ = s.Get("sess-claude")
	if sess.Status == nil || !sess.Status.Working {
		t.Fatal("expected working after second user message")
	}

	// 7. User aborts (stop_sequence).
	simulateFileWrite(t, fm, "sess-claude", path,
		`{"type":"assistant","sessionId":"abc","message":{"role":"assistant","content":[{"type":"text","text":"I was..."}],"stop_reason":"stop_sequence"},"uuid":"a5"}`,
	)
	sess, _ = s.Get("sess-claude")
	if sess.Status != nil {
		t.Fatalf("expected idle after stop_sequence, got %+v", sess.Status)
	}
}

func TestClaudeCustomTitleDuringWorkPreservesStatus(t *testing.T) {
	fm, s, dir := setupClaudeFileMonitor(t)
	path := filepath.Join(dir, "test-session.jsonl")

	simulateFileWrite(t, fm, "sess-claude", path,
		`{"type":"user","sessionId":"abc","timestamp":"2026-03-19T10:00:00Z","cwd":"/home/user/dev/project","message":{"role":"user","content":"fix bug"},"uuid":"u1"}`,
	)

	// Custom title while working — working should be preserved.
	simulateFileWrite(t, fm, "sess-claude", path,
		`{"type":"custom-title","customTitle":"Bug fix session","sessionId":"abc"}`,
	)
	sess, _ := s.Get("sess-claude")
	if sess.AdapterTitle != "Bug fix session" {
		t.Errorf("expected title 'Bug fix session', got %q", sess.AdapterTitle)
	}
	if sess.Status == nil || !sess.Status.Working {
		t.Fatal("expected working=true preserved after custom-title")
	}
}

// --- Codex status lifecycle tests ---

func setupCodexFileMonitor(t *testing.T) (*FileMonitor, *store.Store, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)

	cwd := "/home/user/dev/project"
	codex := adapters.NewCodex()
	// Codex uses date-nested dirs, create today's.
	sessionDir := codex.SessionDir(cwd)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	s := store.New()
	s.Upsert(store.Session{
		ID:         "sess-codex",
		Cwd:        cwd,
		Kind:       "codex",
		Alive:      true,
		StartedAt:  time.Now().UTC().Format(time.RFC3339),
		SocketPath: "/tmp/fake.sock",
	})

	fm := NewFileMonitor(s)
	if fm.watcher != nil {
		fm.watcher.Close()
		fm.watcher = nil
	}

	fm.sessions["sess-codex"] = &monitoredSession{
		id:      "sess-codex",
		cwd:     cwd,
		kind:    "codex",
		adapter: codex,
		fileMon: codex,
		filer:   codex,
	}

	return fm, s, sessionDir
}

func TestCodexAbortClearsWorking(t *testing.T) {
	fm, s, dir := setupCodexFileMonitor(t)
	path := filepath.Join(dir, "test.jsonl")

	simulateFileWrite(t, fm, "sess-codex", path,
		`{"type":"session_meta","payload":{"id":"abc","timestamp":"2026-03-19T10:00:00Z","cwd":"/home/user/dev/project"}}`,
		`{"type":"event_msg","payload":{"type":"user_message"}}`,
	)
	sess, _ := s.Get("sess-codex")
	if sess.Status == nil || !sess.Status.Working {
		t.Fatal("expected working=true after user_message")
	}

	// User aborts.
	simulateFileWrite(t, fm, "sess-codex", path,
		`{"type":"event_msg","payload":{"type":"turn_aborted"}}`,
	)
	sess, _ = s.Get("sess-codex")
	if sess.Status != nil {
		t.Fatalf("expected nil status (idle) after turn_aborted, got %+v", sess.Status)
	}
}

func TestCodexFullLifecycle(t *testing.T) {
	fm, s, dir := setupCodexFileMonitor(t)
	path := filepath.Join(dir, "test.jsonl")

	// 1. Session starts + user message → working.
	simulateFileWrite(t, fm, "sess-codex", path,
		`{"type":"session_meta","payload":{"id":"abc","timestamp":"2026-03-19T10:00:00Z","cwd":"/home/user/dev/project"}}`,
		`{"type":"event_msg","payload":{"type":"user_message"}}`,
	)
	sess, _ := s.Get("sess-codex")
	if sess.Status == nil || !sess.Status.Working {
		t.Fatal("expected working after user_message")
	}

	// 2. Agent works (function calls — no status events).
	simulateFileWrite(t, fm, "sess-codex", path,
		`{"type":"response_item","payload":{"type":"function_call"}}`,
		`{"type":"response_item","payload":{"type":"function_call_output"}}`,
	)
	sess, _ = s.Get("sess-codex")
	if sess.Status == nil || !sess.Status.Working {
		t.Fatal("expected working preserved during function calls")
	}

	// 3. Task complete → idle.
	simulateFileWrite(t, fm, "sess-codex", path,
		`{"type":"event_msg","payload":{"type":"task_complete"}}`,
	)
	sess, _ = s.Get("sess-codex")
	if sess.Status != nil {
		t.Fatalf("expected idle after task_complete, got %+v", sess.Status)
	}

	// 4. Second turn.
	simulateFileWrite(t, fm, "sess-codex", path,
		`{"type":"event_msg","payload":{"type":"user_message"}}`,
	)
	sess, _ = s.Get("sess-codex")
	if sess.Status == nil || !sess.Status.Working {
		t.Fatal("expected working after second user_message")
	}

	// 5. Turn cancelled.
	simulateFileWrite(t, fm, "sess-codex", path,
		`{"type":"event_msg","payload":{"type":"turn_cancelled"}}`,
	)
	sess, _ = s.Get("sess-codex")
	if sess.Status != nil {
		t.Fatalf("expected idle after turn_cancelled, got %+v", sess.Status)
	}
}
