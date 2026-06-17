package ptyserver

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gmuxapp/gmux/cli/gmux/internal/session"
	"github.com/gmuxapp/gmux/packages/adapter"
)

// stubFiler implements adapter.FileMonitor + adapter.SessionFiler with
// scripted behavior, so the reader can be exercised without a real adapter.
type stubFiler struct {
	gotLines [][]string // ParseNewLines call log
	events   func(lines []string) []adapter.Event
	info     *adapter.SessionFileInfo
}

func (s *stubFiler) ParseNewLines(lines []string, _ string) []adapter.Event {
	s.gotLines = append(s.gotLines, lines)
	if s.events != nil {
		return s.events(lines)
	}
	return nil
}
func (s *stubFiler) SessionRootDir() string   { return "" }
func (s *stubFiler) SessionDir(string) string { return "" }
func (s *stubFiler) ParseSessionFile(string) (*adapter.SessionFileInfo, error) {
	return s.info, nil
}

func newReader(t *testing.T, stub *stubFiler) (*sessionFileReader, *session.State) {
	t.Helper()
	st := session.New(session.Config{ID: "s1", Kind: "pi"})
	return &sessionFileReader{state: st, parser: stub, filer: stub}, st
}

func TestSessionFileReaderReadAllThenIncremental(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess.jsonl")
	os.WriteFile(path, []byte("line1\nline2\n"), 0o644)

	stub := &stubFiler{
		info: &adapter.SessionFileInfo{ID: "tool-1", Slug: "my-slug", Title: "Header Title"},
		events: func(lines []string) []adapter.Event {
			// Title once a "user" line is present; unread on every new batch.
			return []adapter.Event{{Title: "Parsed Title", Unread: adapter.BoolPtr(true)}}
		},
	}
	r, st := newReader(t, stub)

	// First write: whole file parsed (readAll), metadata synced.
	r.onWrite(path)
	if len(stub.gotLines) != 1 || len(stub.gotLines[0]) != 2 {
		t.Fatalf("first read lines = %v, want 2 lines", stub.gotLines)
	}
	if st.Slug != "my-slug" {
		t.Errorf("slug = %q, want my-slug", st.Slug)
	}
	if st.AdapterTitle != "Parsed Title" {
		t.Errorf("title = %q, want Parsed Title", st.AdapterTitle)
	}
	// Unread must NOT be applied on the initial (readAll) pass.
	if st.Unread {
		t.Errorf("unread set on readAll; should be suppressed")
	}

	// Append one line: only the new line is parsed, and unread now applies.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("line3\n")
	f.Close()

	r.onWrite(path)
	if len(stub.gotLines) != 2 || len(stub.gotLines[1]) != 1 || stub.gotLines[1][0] != "line3" {
		t.Fatalf("incremental read = %v, want [line3]", stub.gotLines)
	}
	if !st.Unread {
		t.Errorf("unread not applied on incremental write")
	}
}

func TestSessionFileReaderRebindResetsOffset(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.jsonl")
	newPath := filepath.Join(dir, "new.jsonl")
	os.WriteFile(oldPath, []byte("a\nb\n"), 0o644)
	os.WriteFile(newPath, []byte("x\ny\nz\n"), 0o644)

	stub := &stubFiler{info: &adapter.SessionFileInfo{ID: "tool"}}
	r, _ := newReader(t, stub)

	r.onWrite(oldPath)
	r.onWrite(newPath) // /resume rebind: new path → readAll from offset 0

	if len(stub.gotLines) != 2 || len(stub.gotLines[1]) != 3 {
		t.Fatalf("rebind should re-read whole new file, got %v", stub.gotLines)
	}
	if r.path != newPath || r.offset != 6 {
		t.Errorf("after rebind path=%q offset=%d, want %q/6", r.path, r.offset, newPath)
	}
}

func TestSessionFileReaderNilSafe(t *testing.T) {
	var r *sessionFileReader
	r.onWrite("/whatever") // must not panic
}
