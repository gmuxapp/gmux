package adapter

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConversationGoneAtRoot(t *testing.T) {
	root := t.TempDir()
	present := filepath.Join(root, "conv.jsonl")
	if err := os.WriteFile(present, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(root, "deleted.jsonl")

	t.Run("present file is not gone", func(t *testing.T) {
		gone, ok := ConversationGoneAtRoot(present, root)
		if gone || !ok {
			t.Errorf("present: got (gone=%v ok=%v), want (false true)", gone, ok)
		}
	})

	t.Run("missing file with live root is deleted", func(t *testing.T) {
		gone, ok := ConversationGoneAtRoot(missing, root)
		if !gone || !ok {
			t.Errorf("deleted: got (gone=%v ok=%v), want (true true)", gone, ok)
		}
	})

	t.Run("missing file with absent root is undeterminable", func(t *testing.T) {
		// Storage anchor doesn't exist (e.g. unmounted home): must NOT
		// claim deletion.
		absentRoot := filepath.Join(root, "nope", "sessions")
		gone, ok := ConversationGoneAtRoot(filepath.Join(absentRoot, "c.jsonl"), absentRoot)
		if gone || ok {
			t.Errorf("absent root: got (gone=%v ok=%v), want (false false)", gone, ok)
		}
	})

	t.Run("root that is a file (not dir) is undeterminable", func(t *testing.T) {
		gone, ok := ConversationGoneAtRoot(missing, present) // present is a file
		if gone || ok {
			t.Errorf("file-as-root: got (gone=%v ok=%v), want (false false)", gone, ok)
		}
	})

	t.Run("empty args are undeterminable", func(t *testing.T) {
		if g, ok := ConversationGoneAtRoot("", root); g || ok {
			t.Errorf("empty path: want (false false), got (%v %v)", g, ok)
		}
		if g, ok := ConversationGoneAtRoot(present, ""); g || ok {
			t.Errorf("empty root: want (false false), got (%v %v)", g, ok)
		}
	})
}
