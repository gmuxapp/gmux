package discovery

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

func writePiSession(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"session","version":3,"id":"abc-123","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp/test"}` + "\n" +
		`{"type":"message","id":"u1","timestamp":"2026-03-15T10:01:00Z","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveResumeCommand(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.jsonl")
	writePiSession(t, good)
	// Parses fine, has no messages: the adapter itself declares it
	// non-resumable, so derivation yields nil.
	empty := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(empty, []byte(`{"type":"session","version":3,"id":"e-1","timestamp":"2026-03-15T10:00:00Z","cwd":"/tmp/test"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Note: resumability is the adapter's own call inside ResumeCommand —
	// a described-but-empty conversation yields nil here too.
	tests := []struct {
		name    string
		sess    store.Session
		wantNil bool
	}{
		{"no conversation file", store.Session{Adapter: "pi"}, true},
		{"unknown adapter", store.Session{Adapter: "nope", ConversationRef: good}, true},
		{"unparseable file", store.Session{Adapter: "pi", ConversationRef: filepath.Join(dir, "ghost.jsonl")}, true},
		{"described but empty pi", store.Session{Adapter: "pi", ConversationRef: empty}, true},
		{"resumable pi", store.Session{Adapter: "pi", ConversationRef: good}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := ResolveResumeCommand(&tc.sess)
			if tc.wantNil {
				if cmd != nil {
					t.Fatalf("want nil, got %v", cmd)
				}
				return
			}
			if len(cmd) == 0 || cmd[0] != "pi" || !slices.Contains(cmd, good) {
				t.Fatalf("resume command %v does not reference %q via pi", cmd, good)
			}
		})
	}
}

// A shell session never carries a ConversationRef, so the guard returns before the
// adapter lookup — even though shell is itself a ConversationFiler/Resumer.
func TestResolveResumeCommandShellGuarded(t *testing.T) {
	if cmd := ResolveResumeCommand(&store.Session{Adapter: "shell"}); cmd != nil {
		t.Fatalf("shell session should not resolve a resume command, got %v", cmd)
	}
}
