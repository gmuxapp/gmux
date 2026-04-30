package sessionmeta

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	return New(t.TempDir())
}

func sampleSession() store.Session {
	exit := 42
	return store.Session{
		ID:           "sess-test123",
		Kind:         "shell",
		Command:      []string{"bash", "-l"},
		Cwd:          "/home/u/proj",
		Alive:        false,
		ExitCode:     &exit,
		StartedAt:    "2026-04-26T10:00:00Z",
		ExitedAt:     "2026-04-26T10:05:00Z",
		Title:        "proj",
		ShellTitle:   "shell-title-internal",
		AdapterTitle: "adapter-title-internal",
		Slug:         "proj",
	}
}

// TestRoundTripPreservesInternalFields is the central correctness
// claim of this package: persisted sessions come back with the
// internal Title-precedence fields (ShellTitle, AdapterTitle) intact.
// Without this, a restored session loses its title resolution and
// shows up as the bare Kind ("shell") instead of "proj".
func TestRoundTripPreservesInternalFields(t *testing.T) {
	s := newStore(t)
	in := sampleSession()

	if err := s.Write(in); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out, err := s.Read(in.ID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if out.ShellTitle != in.ShellTitle {
		t.Errorf("ShellTitle: got %q, want %q", out.ShellTitle, in.ShellTitle)
	}
	if out.AdapterTitle != in.AdapterTitle {
		t.Errorf("AdapterTitle: got %q, want %q", out.AdapterTitle, in.AdapterTitle)
	}
	if out.ExitCode == nil || *out.ExitCode != *in.ExitCode {
		t.Errorf("ExitCode: got %v, want %v", out.ExitCode, in.ExitCode)
	}
	if out.Cwd != in.Cwd || out.Title != in.Title || out.Slug != in.Slug {
		t.Errorf("scalars mismatch: got %+v, want %+v", out, in)
	}
}

// TestRoundTripFullSession is the regression test for "someone added
// a field to store.Session without a json tag." By using
// reflect.DeepEqual against a fully-populated Session we don't have
// to enumerate field names: any silently-dropped field surfaces as
// a diff at this assertion. The persistedSession alias is supposed
// to make this guarantee automatic, but only if every field carries
// a json tag.
func TestRoundTripFullSession(t *testing.T) {
	s := newStore(t)
	exit := 7
	in := store.Session{
		ID:            "sess-full",
		CreatedAt:     "2026-04-26T10:00:00Z",
		Command:       []string{"bash", "-c", "echo hi"},
		Cwd:           "~/work",
		Kind:          "shell",
		WorkspaceRoot: "~/work/repo",
		Remotes:       map[string]string{"origin": "github.com/me/repo"},
		Alive:         false,
		Pid:           12345,
		ExitCode:      &exit,
		StartedAt:     "2026-04-26T10:00:01Z",
		ExitedAt:      "2026-04-26T10:05:00Z",
		Title:         "my title",
		Subtitle:      "my subtitle",
		Status:        &store.Status{Label: "working", Working: true, Error: false},
		Unread:        true,
		Resumable:     true,
		SocketPath:    "/tmp/gmux-sessions/sess-full.sock",
		TerminalCols:  120,
		TerminalRows:  40,
		Slug:          "my-slug",
		RunnerVersion: "v1.4.0",
		BinaryHash:    "abc123",
		ShellTitle:    "shell-title",
		AdapterTitle:  "adapter-title",
	}

	if err := s.Write(in); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out, err := s.Read(in.ID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round-trip diverged.\nin:  %+v\nout: %+v", in, out)
	}
}

// TestWriteAtomicNoTempLeftover pins down the rename-not-copy
// invariant: after a successful Write, no .tmp-* file lingers in
// the session directory. Without this, a partial-Write crash could
// leave files that would confuse Sweep on next startup.
func TestWriteAtomicNoTempLeftover(t *testing.T) {
	s := newStore(t)
	if err := s.Write(sampleSession()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	entries, err := os.ReadDir(s.SessionDir("sess-test123"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != metaFile {
			t.Errorf("unexpected file in session dir: %q", e.Name())
		}
	}
}

func TestWriteSkipsPeerSessions(t *testing.T) {
	s := newStore(t)
	sess := sampleSession()
	sess.Peer = "remote-host"
	if err := s.Write(sess); err != nil {
		t.Fatalf("Write returned error for peer session: %v", err)
	}
	if _, err := os.Stat(s.SessionDir(sess.ID)); !os.IsNotExist(err) {
		t.Fatalf("peer session should not have been persisted; got err=%v", err)
	}
}

func TestWriteRejectsEmptyID(t *testing.T) {
	s := newStore(t)
	sess := sampleSession()
	sess.ID = ""
	if err := s.Write(sess); err == nil {
		t.Fatalf("expected error for empty id, got nil")
	}
}

func TestWriteFilePermissions(t *testing.T) {
	s := newStore(t)
	if err := s.Write(sampleSession()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	info, err := os.Stat(filepath.Join(s.SessionDir("sess-test123"), metaFile))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != fileMode {
		t.Errorf("file mode: got %o, want %o", got, fileMode)
	}
	dirInfo, err := os.Stat(s.SessionDir("sess-test123"))
	if err != nil {
		t.Fatalf("Stat dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != dirMode {
		t.Errorf("dir mode: got %o, want %o", got, dirMode)
	}
}

func TestRemoveIsIdempotent(t *testing.T) {
	s := newStore(t)
	if err := s.Write(sampleSession()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := s.Remove("sess-test123"); err != nil {
		t.Fatalf("first Remove: %v", err)
	}
	if err := s.Remove("sess-test123"); err != nil {
		t.Fatalf("second Remove (must be idempotent): %v", err)
	}
	if err := s.Remove("sess-never-existed"); err != nil {
		t.Fatalf("Remove of non-existent: %v", err)
	}
}

func TestSweepLoadsAllSessionsAsAliveFalse(t *testing.T) {
	s := newStore(t)
	a := sampleSession()
	a.ID = "sess-aaa"
	b := sampleSession()
	b.ID = "sess-bbb"
	b.Alive = true // Sweep must downgrade — we only persist after death

	for _, sess := range []store.Session{a, b} {
		if err := s.Write(sess); err != nil {
			t.Fatalf("Write %s: %v", sess.ID, err)
		}
	}

	loaded, err := s.Sweep()
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("loaded %d sessions, want 2: %+v", len(loaded), loaded)
	}
	for _, sess := range loaded {
		if sess.Alive {
			t.Errorf("Sweep returned Alive=true for %s; should always be false", sess.ID)
		}
	}
}

func TestSweepRemovesOrphanDirs(t *testing.T) {
	s := newStore(t)
	// Orphan: dir exists with non-meta content (a scrollback file
	// the runner wrote) but no meta.json. Sweep should treat the
	// whole dir as orphan and clean it up.
	orphan := s.SessionDir("sess-orphan")
	if err := os.MkdirAll(orphan, dirMode); err != nil {
		t.Fatalf("mkdir orphan: %v", err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "scrollback"), []byte("xyz"), fileMode); err != nil {
		t.Fatalf("write orphan scrollback: %v", err)
	}

	if _, err := s.Sweep(); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan dir should have been removed; got err=%v", err)
	}
}

func TestSweepKeepsValidSessionsAlongsideOrphan(t *testing.T) {
	s := newStore(t)

	good := sampleSession()
	if err := s.Write(good); err != nil {
		t.Fatalf("Write good: %v", err)
	}
	orphan := s.SessionDir("sess-orphan")
	if err := os.MkdirAll(orphan, dirMode); err != nil {
		t.Fatalf("mkdir orphan: %v", err)
	}

	loaded, err := s.Sweep()
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(loaded) != 1 || loaded[0].ID != good.ID {
		t.Fatalf("Sweep should return just the good session; got %+v", loaded)
	}
	if _, err := os.Stat(filepath.Join(s.SessionDir(good.ID), metaFile)); err != nil {
		t.Errorf("good session was disturbed: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("orphan should have been swept: err=%v", err)
	}
}

// TestSweepSkipsUnparseableMeta verifies that a corrupted meta.json
// doesn't take down the whole sweep. Recovery posture: log, skip,
// keep going. Operator can inspect manually if curious.
func TestSweepSkipsUnparseableMeta(t *testing.T) {
	s := newStore(t)
	bad := s.SessionDir("sess-bad")
	if err := os.MkdirAll(bad, dirMode); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bad, metaFile), []byte("not json"), fileMode); err != nil {
		t.Fatalf("write bad meta: %v", err)
	}
	good := sampleSession()
	if err := s.Write(good); err != nil {
		t.Fatalf("Write good: %v", err)
	}

	loaded, err := s.Sweep()
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(loaded) != 1 || loaded[0].ID != good.ID {
		t.Fatalf("Sweep should return only the good session: %+v", loaded)
	}
	// Bad dir is intentionally left in place for inspection.
	if _, err := os.Stat(bad); err != nil {
		t.Errorf("bad dir should be left alone: %v", err)
	}
}

func TestSweepReturnsEmptyForMissingDir(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "does-not-exist"))
	loaded, err := s.Sweep()
	if err != nil {
		t.Fatalf("Sweep on missing dir should not error: %v", err)
	}
	if len(loaded) != 0 {
		t.Fatalf("expected zero sessions, got %d", len(loaded))
	}
}

// TestWatchRemovalsRemovesOnSessionRemoveEvent verifies the cleanup
// loop's contract end to end: a session-remove event drops the
// on-disk record; events of other types do not; the loop terminates
// cleanly when the channel closes.
//
// This is the catch-all for slug-takeover orphans (and any other
// store removal not paired with an explicit Remove call). A
// regression here would silently leak per-session directories.
func TestWatchRemovalsRemovesOnSessionRemoveEvent(t *testing.T) {
	s := newStore(t)
	target := sampleSession()
	target.ID = "sess-target"
	survivor := sampleSession()
	survivor.ID = "sess-survivor"

	for _, sess := range []store.Session{target, survivor} {
		if err := s.Write(sess); err != nil {
			t.Fatalf("Write %s: %v", sess.ID, err)
		}
	}

	events := make(chan store.Event, 4)
	done := make(chan struct{})
	go func() {
		s.WatchRemovals(events)
		close(done)
	}()

	// Non-removal events must not touch the disk.
	events <- store.Event{Type: "session-upsert", ID: target.ID}
	events <- store.Event{Type: "session-activity", ID: target.ID}

	// Removal of target should drop its dir.
	events <- store.Event{Type: "session-remove", ID: target.ID}

	// Removal of an ID we never persisted is a no-op (peer sessions,
	// sessions removed before any Alive=false upsert).
	events <- store.Event{Type: "session-remove", ID: "sess-never-existed"}

	// Closing the channel must terminate the loop.
	close(events)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WatchRemovals did not return after channel close")
	}

	if _, err := os.Stat(s.SessionDir(target.ID)); !os.IsNotExist(err) {
		t.Errorf("target dir should have been removed; err=%v", err)
	}
	if _, err := os.Stat(s.SessionDir(survivor.ID)); err != nil {
		t.Errorf("survivor dir should be untouched: %v", err)
	}
}

// TestWriteOverwritesExisting verifies the "every Alive=true→false
// transition rewrites meta.json" contract from the lifecycle plan.
// Subsequent Writes for the same id replace the previous file
// (e.g., an exit code arriving via a late SSE event after the
// socket-gone path already persisted a no-exit-code stub).
func TestWriteOverwritesExisting(t *testing.T) {
	s := newStore(t)
	first := sampleSession()
	first.Title = "first"
	if err := s.Write(first); err != nil {
		t.Fatalf("first Write: %v", err)
	}

	second := sampleSession()
	second.Title = "second"
	if err := s.Write(second); err != nil {
		t.Fatalf("second Write: %v", err)
	}

	out, err := s.Read(first.ID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if out.Title != "second" {
		t.Errorf("title: got %q, want %q (second Write should have overwritten)", out.Title, "second")
	}

	// Sanity: meta.json contains exactly the second payload.
	raw, err := os.ReadFile(filepath.Join(s.SessionDir(first.ID), metaFile))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var verify persistedSession
	if err := json.Unmarshal(raw, &verify); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if verify.Title != "second" {
		t.Errorf("on-disk title: got %q, want %q", verify.Title, "second")
	}
}
