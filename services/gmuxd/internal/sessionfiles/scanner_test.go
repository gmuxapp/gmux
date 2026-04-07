package sessionfiles

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

// writePiSession creates a minimal pi session JSONL file in the right directory structure.
func writePiSession(t *testing.T, homeDir, cwd, sessID, userMsg string) string {
	t.Helper()

	// Pi encodes cwd: strip leading /, replace / with -, wrap in --
	stripped := strings.TrimPrefix(cwd, "/")
	dirName := "--" + strings.ReplaceAll(stripped, "/", "-") + "--"
	encoded := filepath.Join(homeDir, ".pi", "agent", "sessions", dirName)
	os.MkdirAll(encoded, 0o755)

	header, _ := json.Marshal(map[string]string{
		"type": "session", "id": sessID, "cwd": cwd,
		"timestamp": "2026-03-15T10:00:00.000Z",
	})
	msg, _ := json.Marshal(map[string]any{
		"type":    "message",
		"message": map[string]any{"role": "user", "content": userMsg},
	})

	path := filepath.Join(encoded, "2026-03-15T10-00-00-000Z_"+sessID+".jsonl")
	content := string(header) + "\n" + string(msg) + "\n"
	os.WriteFile(path, []byte(content), 0o644)
	return path
}

func newTestStore() *store.Store {
	return store.New()
}

func TestScanDiscoversFromAllDirectories(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// Create sessions in two different cwds.
	writePiSession(t, tmpHome, "/tmp/project-a", "aaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "fix auth")
	writePiSession(t, tmpHome, "/tmp/project-b", "ffff-gggg-hhhh-iiii-jjjjjjjjjjjj", "add tests")

	// Empty store — no live sessions. Scanner should still find everything.
	s := newTestStore()
	sc := New(s)
	sc.Scan()

	sessions := s.List()
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(sessions))
	}

	titles := map[string]bool{}
	for _, sess := range sessions {
		titles[sess.Title] = true
		if !sess.Resumable {
			t.Errorf("session %s should be resumable", sess.ID)
		}
	}

	if !titles["fix auth"] {
		t.Error("missing session with title 'fix auth'")
	}
	if !titles["add tests"] {
		t.Error("missing session with title 'add tests'")
	}
}

func TestScanSkipsDuplicates(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	sessID := "aaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	writePiSession(t, tmpHome, "/tmp/project", sessID, "hello")

	s := newTestStore()
	// Pre-existing alive session with slug-based resume_key.
	// The scanner would derive the same slug ("hello") from the file,
	// so the store's dedup should skip the dead shadow.
	s.Upsert(store.Session{ID: "existing", Kind: "pi", Cwd: "/tmp/project", ResumeKey: "hello", Alive: true})

	sc := New(s)
	sc.Scan()

	// The alive session survives; the dead file-scanned shadow is skipped by dedup.
	alive := 0
	for _, sess := range s.List() {
		if sess.Alive {
			alive++
		}
	}
	if alive != 1 {
		t.Errorf("expected 1 alive session, got %d", alive)
	}
}

func TestScanRediscoversAfterRemove(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	sessID := "aaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	writePiSession(t, tmpHome, "/tmp/project", sessID, "hello")

	s := newTestStore()
	sc := New(s)
	sc.Scan()

	if len(s.List()) != 1 {
		t.Fatal("expected 1 session from initial scan")
	}

	// Remove the session — simulates user clicking ×.
	s.Remove(s.List()[0].ID)
	if len(s.List()) != 0 {
		t.Fatal("expected 0 sessions after remove")
	}

	// Rescan — session should come back. Project arrays (not the store)
	// control sidebar visibility; the scanner just discovers what exists.
	sc.Scan()
	if len(s.List()) != 1 {
		t.Errorf("expected 1 session after rescan, got %d", len(s.List()))
	}
}

func TestScanUsesFileHeaderCwd(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// The cwd in the file header is the source of truth.
	writePiSession(t, tmpHome, "/home/user/my-project", "aaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "hello")

	s := newTestStore()
	sc := New(s)
	sc.Scan()

	sessions := s.List()
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].Cwd != "/home/user/my-project" {
		t.Errorf("cwd = %q, want %q", sessions[0].Cwd, "/home/user/my-project")
	}
}

func TestScanRefreshesDead(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	sessID := "aaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	writePiSession(t, tmpHome, "/tmp/project", sessID, "fix auth")

	s := newTestStore()
	// Simulate a session that was resumed then exited: has resume_key,
	// dead, no command (so not resumable yet). Scanner should refresh it.
	s.Upsert(store.Session{
		ID:        "file-aaaa-bbb",
		Cwd:       "/tmp/project",
		ResumeKey: sessID,
		Alive:     false,
	})

	sc := New(s)
	sc.Scan()

	sessions := s.List()
	// Should have 2: the old dead one + the refreshed resumable one
	// (scanner creates file-aaaa-bbb as new ID from file-<first8>)
	resumable := 0
	for _, sess := range sessions {
		if sess.Resumable {
			resumable++
			if sess.Title != "fix auth" {
				t.Errorf("title = %q, want %q", sess.Title, "fix auth")
			}
		}
	}
	if resumable != 1 {
		t.Errorf("expected 1 resumable session, got %d", resumable)
	}
}

func TestScanSetsWorkspaceRoot(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// Create a fake git repo so workspace.DetectRoot resolves it.
	repoDir := filepath.Join(tmpHome, "projects", "myrepo")
	os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755)

	// Create a pi session file whose cwd is inside the repo.
	subdir := filepath.Join(repoDir, "src", "pkg")
	os.MkdirAll(subdir, 0o755)
	writePiSession(t, tmpHome, subdir, "aaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "refactor")

	s := newTestStore()
	sc := New(s)
	sc.Scan()

	sessions := s.List()
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	// The store canonicalizes paths: $HOME prefix becomes ~.
	wantRoot := "~/projects/myrepo"
	if sessions[0].WorkspaceRoot != wantRoot {
		t.Errorf("workspace_root = %q, want %q", sessions[0].WorkspaceRoot, wantRoot)
	}
}

func TestScanWorkspaceRootEmptyWithoutVCS(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// cwd with no .git or .jj — WorkspaceRoot should be empty.
	plainDir := filepath.Join(tmpHome, "no-vcs")
	os.MkdirAll(plainDir, 0o755)
	writePiSession(t, tmpHome, plainDir, "aaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "hello")

	s := newTestStore()
	sc := New(s)
	sc.Scan()

	sessions := s.List()
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].WorkspaceRoot != "" {
		t.Errorf("workspace_root = %q, want empty", sessions[0].WorkspaceRoot)
	}
}

func TestPurgeStaleSessions(t *testing.T) {
	s := newTestStore()

	s.Upsert(store.Session{
		ID:       "stale",
		Alive:    false,
		ExitedAt: time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339),
	})
	s.Upsert(store.Session{
		ID:       "fresh",
		Alive:    false,
		ExitedAt: time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339),
	})
	s.Upsert(store.Session{
		ID:        "resumable",
		Alive:     false,
		ResumeKey: "some-key",
		ExitedAt:  time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339),
	})

	sc := New(s)
	sc.PurgeStaleSessions(1 * time.Hour)

	ids := map[string]bool{}
	for _, sess := range s.List() {
		ids[sess.ID] = true
	}

	if ids["stale"] {
		t.Error("stale session should have been purged")
	}
	if !ids["fresh"] {
		t.Error("fresh session should still be present")
	}
	if !ids["resumable"] {
		t.Error("resumable session should still be present")
	}
}
