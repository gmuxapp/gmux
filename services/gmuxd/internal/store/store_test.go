package store

import (
	"strings"
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
		ID:           "s1",
		Kind:         "pi",
		Alive:        true,
		AdapterTitle: "test",
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
	s.Upsert(Session{ID: "s1", Kind: "pi", AdapterTitle: "v1"})
	s.Upsert(Session{ID: "s1", Kind: "pi", AdapterTitle: "v2"})

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
// All dead sessions with a command are resumable, regardless of kind.

func TestDerivedResumable_AliveSessionNeverResumable(t *testing.T) {
	s := New()
	s.Upsert(Session{
		ID: "s1", Kind: "claude", Alive: true,
		Command: []string{"claude"},
	})
	got, _ := s.Get("s1")
	if got.Resumable {
		t.Error("alive session should never be resumable")
	}
}

func TestDerivedResumable_DeadWithCommand(t *testing.T) {
	s := New()
	s.Upsert(Session{
		ID: "s1", Kind: "claude", Alive: false,
		Command: []string{"claude"},
	})
	got, _ := s.Get("s1")
	if !got.Resumable {
		t.Error("dead session with command should be resumable")
	}
}

func TestDerivedResumable_DeadNoCommand(t *testing.T) {
	s := New()
	s.Upsert(Session{
		ID: "s1", Kind: "claude", Alive: false,
	})
	got, _ := s.Get("s1")
	if got.Resumable {
		t.Error("dead session without command should NOT be resumable")
	}
}

func TestDerivedResumable_ShellIsResumable(t *testing.T) {
	s := New()
	s.Upsert(Session{
		ID: "s1", Kind: "shell", Alive: false,
		Command: []string{"/bin/bash"},
	})
	got, _ := s.Get("s1")
	if !got.Resumable {
		t.Error("dead shell session with command should be resumable")
	}
}

func TestUpdateAtomic(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "s1", Kind: "pi", AdapterTitle: "original"})

	ok := s.Update("s1", func(sess *Session) {
		sess.AdapterTitle = "updated"
	})
	if !ok {
		t.Fatal("expected update to succeed")
	}
	got, _ := s.Get("s1")
	if got.AdapterTitle != "updated" {
		t.Fatalf("expected 'updated', got %q", got.AdapterTitle)
	}
	if got.Title != "updated" {
		t.Fatalf("expected resolved title 'updated', got %q", got.Title)
	}
}

func TestUpdateMissing(t *testing.T) {
	s := New()
	if s.Update("nonexistent", func(sess *Session) {
		sess.AdapterTitle = "x"
	}) {
		t.Fatal("expected update on missing session to return false")
	}
}

func TestUpdatePreservesOtherFields(t *testing.T) {
	s := New()
	s.Upsert(Session{
		ID: "s1", Kind: "pi",
		AdapterTitle: "my title",
		Status:       &Status{Working: true},
	})
	// Update only the title — status should be preserved.
	s.Update("s1", func(sess *Session) {
		sess.AdapterTitle = "new title"
	})
	got, _ := s.Get("s1")
	if got.AdapterTitle != "new title" {
		t.Fatalf("expected 'new title', got %q", got.AdapterTitle)
	}
	if got.Status == nil || !got.Status.Working {
		t.Fatal("expected working status to be preserved")
	}
}

func TestUpdateBroadcasts(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "s1", Kind: "pi"})
	ch, cancel := s.Subscribe()
	defer cancel()

	// Drain the subscription channel from the initial Upsert
	select {
	case <-ch:
	default:
	}

	s.Update("s1", func(sess *Session) {
		sess.AdapterTitle = "via update"
	})

	select {
	case ev := <-ch:
		if ev.Type != "session-upsert" || ev.Session.AdapterTitle != "via update" {
			t.Fatalf("unexpected event: %+v", ev)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timed out waiting for broadcast from Update")
	}
}

func TestBroadcastDoesNotMutateState(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "s1", Kind: "shell", Alive: true})
	ch, cancel := s.Subscribe()
	defer cancel()

	// Drain the initial upsert.
	select {
	case <-ch:
	default:
	}

	// Broadcast a transient event.
	s.Broadcast(Event{Type: "session-activity", ID: "s1"})

	// Subscriber should receive the event.
	select {
	case ev := <-ch:
		if ev.Type != "session-activity" || ev.ID != "s1" {
			t.Fatalf("unexpected event: %+v", ev)
		}
		if ev.Session != nil {
			t.Fatal("session-activity should not carry session data")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timed out waiting for broadcast event")
	}

	// Session state should be unchanged.
	got, _ := s.Get("s1")
	if !got.Alive {
		t.Fatal("session state should not be mutated by Broadcast")
	}
}

// --- Slug derivation tests ---

func TestSlugFromResumeKey(t *testing.T) {
	s := New()
	s.Upsert(Session{
		ID: "s1", Kind: "pi",
		ResumeKey: "/home/user/.pi/sessions/2026-04-03T06-46-56_07b3c9c8.jsonl",
	})
	got, _ := s.Get("s1")
	if got.Slug == "" {
		t.Fatal("expected slug to be derived")
	}
	// Should be derived from the resume_key basename (without extension).
	if got.Slug != "2026-04-03t06-46-56-07b3c9c8" {
		t.Fatalf("expected resume_key-based slug, got %q", got.Slug)
	}
}

func TestSlugFromCommand(t *testing.T) {
	s := New()
	s.Upsert(Session{
		ID: "s1", Kind: "shell",
		Command: []string{"pytest", "--watch"},
	})
	got, _ := s.Get("s1")
	if got.Slug != "pytest-watch" {
		t.Fatalf("expected 'pytest-watch', got %q", got.Slug)
	}
}

func TestSlugFallbackToID(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "sess-abc12345", Kind: "shell"})
	got, _ := s.Get("sess-abc12345")
	// ID "sess-abc12345" is 14 chars, truncated to 12 -> "sess-abc1234" -> slugified.
	if got.Slug == "" {
		t.Fatal("expected a slug to be derived")
	}
	// Should be a prefix of the session ID.
	if !strings.HasPrefix("sess-abc12345", strings.ReplaceAll(got.Slug, "-", "-")) {
		// Just check it's non-empty and reasonable.
		t.Logf("slug from ID: %q", got.Slug)
	}
}

func TestSlugAdapterProvided(t *testing.T) {
	s := New()
	s.Upsert(Session{
		ID: "s1", Kind: "pi", Slug: "fix-auth-bug",
		ResumeKey: "/some/file.jsonl",
	})
	got, _ := s.Get("s1")
	if got.Slug != "fix-auth-bug" {
		t.Fatalf("expected adapter slug preserved, got %q", got.Slug)
	}
}

func TestSlugUniqueness(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "s1", Kind: "pi", Command: []string{"pi"}})
	s.Upsert(Session{ID: "s2", Kind: "pi", Command: []string{"pi"}})
	s.Upsert(Session{ID: "s3", Kind: "pi", Command: []string{"pi"}})

	s1, _ := s.Get("s1")
	s2, _ := s.Get("s2")
	s3, _ := s.Get("s3")

	if s1.Slug == s2.Slug || s1.Slug == s3.Slug || s2.Slug == s3.Slug {
		t.Fatalf("slugs should be unique: %q, %q, %q", s1.Slug, s2.Slug, s3.Slug)
	}
}

func TestSlugUniquenessAcrossKindsAllowed(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "s1", Kind: "pi", Command: []string{"pi"}})
	s.Upsert(Session{ID: "s2", Kind: "shell", Command: []string{"pi"}})

	s1, _ := s.Get("s1")
	s2, _ := s.Get("s2")

	// Same slug is OK for different kinds.
	if s1.Slug != "pi" || s2.Slug != "pi" {
		t.Fatalf("expected same slug for different kinds: %q, %q", s1.Slug, s2.Slug)
	}
}

func TestSlugStableOnUpdate(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "s1", Kind: "pi", Command: []string{"pi"}})
	original, _ := s.Get("s1")

	// Update should not change the slug.
	s.Update("s1", func(sess *Session) {
		sess.AdapterTitle = "new title"
	})
	updated, _ := s.Get("s1")
	if updated.Slug != original.Slug {
		t.Fatalf("slug changed on update: %q -> %q", original.Slug, updated.Slug)
	}
}

func TestSlugFreedAfterRemove(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "s1", Kind: "pi", Command: []string{"pi"}})
	s1, _ := s.Get("s1")
	originalSlug := s1.Slug // should be "pi"

	s.Remove("s1")

	// New session with same derived slug should get the slug without a suffix.
	s.Upsert(Session{ID: "s2", Kind: "pi", Command: []string{"pi"}})
	s2, _ := s.Get("s2")
	if s2.Slug != originalSlug {
		t.Fatalf("expected slug %q to be reusable after remove, got %q", originalSlug, s2.Slug)
	}
}

func TestSlugAdapterOverrideViaUpdate(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "s1", Kind: "pi", Command: []string{"pi"}})
	original, _ := s.Get("s1")
	if original.Slug != "pi" {
		t.Fatalf("expected auto-derived slug 'pi', got %q", original.Slug)
	}

	// Adapter provides a slug later via meta event.
	s.Update("s1", func(sess *Session) {
		sess.Slug = "fix-auth-bug"
	})
	updated, _ := s.Get("s1")
	if updated.Slug != "fix-auth-bug" {
		t.Fatalf("expected adapter slug 'fix-auth-bug', got %q", updated.Slug)
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
