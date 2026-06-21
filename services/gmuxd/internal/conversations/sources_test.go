package conversations

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writePiFile(t *testing.T, path, id string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"session","version":3,"id":"` + id + `","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp/test"}` + "\n" +
		`{"type":"message","id":"u1","timestamp":"2026-03-15T10:01:00Z","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// End-to-end over the real pi ConversationSource: Snapshot indexes existing
// files; WatchSources picks up a file created after startup. HOME is redirected
// so every adapter root lives under the temp dir (codex/claude see nothing).
func TestSnapshotAndWatchSources(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(home, ".pi", "agent"))
	root := filepath.Join(home, ".pi", "agent", "sessions")

	writePiFile(t, filepath.Join(root, "--tmp-a--", "id-1.jsonl"), "id-1")

	idx := New()
	idx.Snapshot()
	if idx.LookupByToolID("pi", "id-1") == "" {
		t.Fatal("Snapshot did not index the pre-existing conversation")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	idx.WatchSources(ctx)
	time.Sleep(150 * time.Millisecond) // let the watchers establish

	writePiFile(t, filepath.Join(root, "--tmp-b--", "id-2.jsonl"), "id-2")

	deadline := time.After(3 * time.Second)
	for idx.LookupByToolID("pi", "id-2") == "" {
		select {
		case <-deadline:
			t.Fatal("WatchSources did not index a conversation created after startup")
		case <-time.After(20 * time.Millisecond):
		}
	}
}
