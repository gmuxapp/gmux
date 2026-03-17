package store

import (
	"testing"
	"time"
)

func TestListEmpty(t *testing.T) {
	s := New()
	if len(s.List()) != 0 {
		t.Fatal("expected empty list")
	}
}

func TestUpsertAndGet(t *testing.T) {
	s := New()
	s.Upsert(Session{
		ID:    "s1",
		Kind:  "pi",
		Alive: true,
		Title: "test",
	})

	got, ok := s.Get("s1")
	if !ok {
		t.Fatal("expected session to exist")
	}
	if !got.Alive {
		t.Fatal("expected alive")
	}
	if got.Title != "test" {
		t.Fatalf("expected title 'test', got %q", got.Title)
	}
}

func TestUpsertOverwrite(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "s1", Kind: "pi", Title: "v1"})
	s.Upsert(Session{ID: "s1", Kind: "pi", Title: "v2"})

	got, _ := s.Get("s1")
	if got.Title != "v2" {
		t.Fatalf("expected v2, got %q", got.Title)
	}
	if len(s.List()) != 1 {
		t.Fatal("expected 1 session after overwrite")
	}
}

func TestRemove(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "s1", Kind: "pi"})

	if !s.Remove("s1") {
		t.Fatal("expected remove to succeed")
	}
	if s.Remove("s1") {
		t.Fatal("expected second remove to return false")
	}
	if len(s.List()) != 0 {
		t.Fatal("expected empty list after remove")
	}
}

func TestSubscribe(t *testing.T) {
	s := New()
	ch, cancel := s.Subscribe()
	defer cancel()

	s.Upsert(Session{ID: "s1", Kind: "pi", Alive: true})

	select {
	case ev := <-ch:
		if ev.Type != "session-upsert" || ev.ID != "s1" {
			t.Fatalf("unexpected event: %+v", ev)
		}
		if ev.Session == nil || !ev.Session.Alive {
			t.Fatal("expected session in event")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timed out waiting for event")
	}

	s.Remove("s1")

	select {
	case ev := <-ch:
		if ev.Type != "session-remove" || ev.ID != "s1" {
			t.Fatalf("unexpected event: %+v", ev)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timed out waiting for remove event")
	}
}

// --- Derived field tests ---

func newStoreWithKinds() *Store {
	s := New()
	s.SetResumableKinds(map[string]bool{"claude": true, "codex": true, "pi": true})
	return s
}

func TestDerivedResumable_AliveSessionNeverResumable(t *testing.T) {
	s := newStoreWithKinds()
	s.Upsert(Session{
		ID: "s1", Kind: "claude", Alive: true,
		Command: []string{"claude", "--resume", "abc"}, ResumeKey: "abc",
	})
	got, _ := s.Get("s1")
	if got.Resumable {
		t.Error("alive session should never be resumable")
	}
}

func TestDerivedResumable_DeadWithFileAndCommand(t *testing.T) {
	s := newStoreWithKinds()
	s.Upsert(Session{
		ID: "s1", Kind: "claude", Alive: false,
		Command: []string{"claude", "--resume", "abc"}, ResumeKey: "abc",
	})
	got, _ := s.Get("s1")
	if !got.Resumable {
		t.Error("dead session with resume kind + file + command should be resumable")
	}
}

func TestDerivedResumable_DeadNoFile(t *testing.T) {
	s := newStoreWithKinds()
	// Session from resumable adapter but no file was ever attributed.
	s.Upsert(Session{
		ID: "s1", Kind: "claude", Alive: false,
		Command: []string{"claude"},
	})
	got, _ := s.Get("s1")
	if got.Resumable {
		t.Error("dead session without ResumeKey should NOT be resumable")
	}
}

func TestDerivedResumable_DeadNoCommand(t *testing.T) {
	s := newStoreWithKinds()
	s.Upsert(Session{
		ID: "s1", Kind: "claude", Alive: false, ResumeKey: "abc",
	})
	got, _ := s.Get("s1")
	if got.Resumable {
		t.Error("dead session without command should NOT be resumable")
	}
}

func TestDerivedResumable_ShellNeverResumable(t *testing.T) {
	s := newStoreWithKinds()
	s.Upsert(Session{
		ID: "s1", Kind: "shell", Alive: false,
		Command: []string{"/bin/bash"}, ResumeKey: "x",
	})
	got, _ := s.Get("s1")
	if got.Resumable {
		t.Error("shell sessions should never be resumable")
	}
}

func TestDerivedCloseAction_AliveWithFile(t *testing.T) {
	s := newStoreWithKinds()
	s.Upsert(Session{
		ID: "s1", Kind: "pi", Alive: true,
		Command: []string{"pi"}, ResumeKey: "sess-123",
	})
	got, _ := s.Get("s1")
	if got.CloseAction != "minimize" {
		t.Errorf("alive session with file should get minimize, got %q", got.CloseAction)
	}
}

func TestDerivedCloseAction_AliveNoFile(t *testing.T) {
	s := newStoreWithKinds()
	// Alive session from resumable adapter, but no file attributed yet.
	s.Upsert(Session{
		ID: "s1", Kind: "pi", Alive: true,
		Command: []string{"pi"},
	})
	got, _ := s.Get("s1")
	if got.CloseAction != "dismiss" {
		t.Errorf("alive session without file should get dismiss, got %q", got.CloseAction)
	}
}

func TestDerivedCloseAction_DeadResumable(t *testing.T) {
	s := newStoreWithKinds()
	s.Upsert(Session{
		ID: "s1", Kind: "claude", Alive: false,
		Command: []string{"claude", "--resume", "abc"}, ResumeKey: "abc",
	})
	got, _ := s.Get("s1")
	if got.CloseAction != "dismiss" {
		t.Errorf("dead resumable session should get dismiss (× to remove), got %q", got.CloseAction)
	}
}

func TestDerivedCloseAction_Shell(t *testing.T) {
	s := newStoreWithKinds()
	s.Upsert(Session{ID: "s1", Kind: "shell", Alive: true, Command: []string{"/bin/bash"}})
	got, _ := s.Get("s1")
	if got.CloseAction != "dismiss" {
		t.Errorf("shell session should always get dismiss, got %q", got.CloseAction)
	}
}

func TestDerivedCloseAction_TransitionOnFileAttribution(t *testing.T) {
	s := newStoreWithKinds()
	// Start alive with no file — dismiss.
	s.Upsert(Session{ID: "s1", Kind: "claude", Alive: true, Command: []string{"claude"}})
	got, _ := s.Get("s1")
	if got.CloseAction != "dismiss" {
		t.Errorf("before attribution: expected dismiss, got %q", got.CloseAction)
	}

	// File gets attributed — minimize.
	got.ResumeKey = "sess-abc"
	s.Upsert(got)
	got, _ = s.Get("s1")
	if got.CloseAction != "minimize" {
		t.Errorf("after attribution: expected minimize, got %q", got.CloseAction)
	}
}

func TestSetTerminalSize(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "s1", Kind: "shell", Alive: true})

	if !s.SetTerminalSize("s1", 120, 40) {
		t.Fatal("expected terminal size update to succeed")
	}

	got, ok := s.Get("s1")
	if !ok {
		t.Fatal("expected session to exist")
	}
	if got.TerminalCols != 120 || got.TerminalRows != 40 {
		t.Fatalf("expected terminal size 120x40, got %dx%d", got.TerminalCols, got.TerminalRows)
	}
	if s.SetTerminalSize("missing", 120, 40) {
		t.Fatal("expected missing session update to fail")
	}
}
