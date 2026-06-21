package discovery

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fsnotify/fsnotify"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/conversations"
)

// piRoot points pi's adapter at a temp dir and returns its session root.
func piRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", dir)
	root := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func writePiFile(t *testing.T, path, id string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"session","version":3,"id":"` + id + `","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp/test"}` + "\n" +
		`{"type":"message","id":"u1","timestamp":"2026-03-15T10:01:00Z","message":{"role":"user","content":[{"type":"text","text":"Fix the auth bug"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A .jsonl create/write feeds the conversations index; a remove drops it.
func TestNotifyConvIndexUpsertAndRemove(t *testing.T) {
	root := piRoot(t)
	idx := conversations.New()
	fm := NewFileMonitor()
	fm.SetConvIndex(idx)

	path := filepath.Join(root, "--tmp-test--", "abc-123.jsonl")
	writePiFile(t, path, "abc-123")

	fm.notifyConvIndex(fsnotify.Event{Name: path, Op: fsnotify.Create})
	if idx.LookupByToolID("pi", "abc-123") == "" {
		t.Fatal("expected conversation indexed after create")
	}
	if idx.Count() != 1 {
		t.Fatalf("count=%d, want 1", idx.Count())
	}

	fm.notifyConvIndex(fsnotify.Event{Name: path, Op: fsnotify.Remove})
	if idx.Count() != 0 {
		t.Fatalf("count=%d after remove, want 0", idx.Count())
	}
}

// Non-.jsonl events and paths outside any adapter root are ignored.
func TestNotifyConvIndexIgnoresIrrelevant(t *testing.T) {
	root := piRoot(t)
	idx := conversations.New()
	fm := NewFileMonitor()
	fm.SetConvIndex(idx)

	fm.notifyConvIndex(fsnotify.Event{Name: filepath.Join(root, "x.txt"), Op: fsnotify.Create})
	if idx.Count() != 0 {
		t.Fatalf("non-jsonl indexed; count=%d", idx.Count())
	}

	outside := filepath.Join(t.TempDir(), "y.jsonl")
	writePiFile(t, outside, "out-1")
	fm.notifyConvIndex(fsnotify.Event{Name: outside, Op: fsnotify.Create})
	if idx.Count() != 0 {
		t.Fatalf("file outside roots indexed; count=%d", idx.Count())
	}
}

func TestAdapterForPath(t *testing.T) {
	root := piRoot(t)
	fm := NewFileMonitor()

	if a := fm.adapterForPath(filepath.Join(root, "--p--", "s.jsonl")); a == nil || a.Name() != "pi" {
		t.Fatalf("expected pi adapter for path under pi root, got %v", a)
	}
	if a := fm.adapterForPath(filepath.Join(t.TempDir(), "s.jsonl")); a != nil {
		t.Fatalf("expected nil adapter outside roots, got %v", a.Name())
	}
}

// A file landing in a freshly-created subdir is caught up via the Create path.
func TestHandleFSEventCatchesUpNewSubdir(t *testing.T) {
	root := piRoot(t)
	idx := conversations.New()
	fm := NewFileMonitor()
	fm.SetConvIndex(idx)
	fm.WatchRoots() // root now watched

	sub := filepath.Join(root, "--tmp-test--")
	path := filepath.Join(sub, "late-1.jsonl")
	writePiFile(t, path, "late-1") // dir + file exist before we observe the Create

	fm.handleFSEvent(fsnotify.Event{Name: sub, Op: fsnotify.Create})
	if idx.LookupByToolID("pi", "late-1") == "" {
		t.Fatal("expected pre-existing file in new subdir to be caught up")
	}
}
