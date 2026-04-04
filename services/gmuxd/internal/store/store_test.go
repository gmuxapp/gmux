package store

import (
	"encoding/json"
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

func TestUpsertAliveSessionRemovesDeadShadowWithSameResumeKey(t *testing.T) {
	s := New()
	s.Upsert(Session{
		ID: "file-abc", Kind: "pi", Alive: false,
		Command: []string{"pi"}, ResumeKey: "rk-1", AdapterTitle: "shadow",
	})
	s.Upsert(Session{
		ID: "sess-123", Kind: "pi", Alive: true,
		ResumeKey: "rk-1", AdapterTitle: "live",
	})

	items := s.List()
	if len(items) != 1 {
		t.Fatalf("expected 1 session, got %d: %+v", len(items), items)
	}
	if items[0].ID != "sess-123" {
		t.Fatalf("expected live session to remain, got %q", items[0].ID)
	}
}

func TestUpsertDeadShadowSkippedWhenAliveSessionExists(t *testing.T) {
	s := New()
	s.Upsert(Session{
		ID: "sess-123", Kind: "pi", Alive: true,
		ResumeKey: "rk-1", AdapterTitle: "live",
	})
	s.Upsert(Session{
		ID: "file-abc", Kind: "pi", Alive: false,
		Command: []string{"pi"}, ResumeKey: "rk-1", AdapterTitle: "shadow",
	})

	items := s.List()
	if len(items) != 1 {
		t.Fatalf("expected 1 session, got %d: %+v", len(items), items)
	}
	if items[0].ID != "sess-123" {
		t.Fatalf("expected live session to remain, got %q", items[0].ID)
	}
}

func TestUpdateResumeKeyOnAliveSessionRemovesDeadShadow(t *testing.T) {
	s := New()
	s.Upsert(Session{
		ID: "file-abc", Kind: "pi", Alive: false,
		Command: []string{"pi"}, ResumeKey: "rk-1", AdapterTitle: "shadow",
	})
	s.Upsert(Session{
		ID: "sess-123", Kind: "pi", Alive: true, AdapterTitle: "live",
	})

	ok := s.Update("sess-123", func(sess *Session) {
		sess.ResumeKey = "rk-1"
	})
	if !ok {
		t.Fatal("expected update to succeed")
	}

	items := s.List()
	if len(items) != 1 {
		t.Fatalf("expected 1 session, got %d: %+v", len(items), items)
	}
	if items[0].ID != "sess-123" {
		t.Fatalf("expected live session to remain, got %q", items[0].ID)
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

// --- Slug tests ---
//
// The slug model: resume_key === slug. Adapters produce human-readable
// resume_keys (via ParseSessionFile). The store ensures uniqueness
// within a kind. Sessions without a resume_key get a temporary slug
// from their kind name.

func TestSlugEqualsResumeKey(t *testing.T) {
	s := New()
	s.Upsert(Session{
		ID: "s1", Kind: "pi",
		ResumeKey: "fix-the-auth-bug",
	})
	got, _ := s.Get("s1")
	if got.Slug != "fix-the-auth-bug" {
		t.Fatalf("slug should equal resume_key, got %q", got.Slug)
	}
	if got.ResumeKey != got.Slug {
		t.Fatalf("resume_key and slug diverged: %q vs %q", got.ResumeKey, got.Slug)
	}
}

func TestSlugFallbackToKind(t *testing.T) {
	s := New()
	// No resume_key yet (fresh launch, before file attribution).
	s.Upsert(Session{ID: "s1", Kind: "pi"})
	got, _ := s.Get("s1")
	if got.Slug != "pi" {
		t.Fatalf("expected kind-based fallback slug, got %q", got.Slug)
	}
}

func TestSlugFallbackToSession(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "s1"})
	got, _ := s.Get("s1")
	if got.Slug != "session" {
		t.Fatalf("expected 'session' fallback, got %q", got.Slug)
	}
}

func TestSlugUniqueness(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "s1", Kind: "pi", ResumeKey: "fix-auth"})
	s.Upsert(Session{ID: "s2", Kind: "pi", ResumeKey: "fix-auth"})
	s.Upsert(Session{ID: "s3", Kind: "pi", ResumeKey: "fix-auth"})

	s1, _ := s.Get("s1")
	s2, _ := s.Get("s2")
	s3, _ := s.Get("s3")

	if s1.Slug != "fix-auth" || s2.Slug != "fix-auth-2" || s3.Slug != "fix-auth-3" {
		t.Fatalf("expected suffixed slugs: %q, %q, %q", s1.Slug, s2.Slug, s3.Slug)
	}
	// resume_key must stay in sync.
	if s2.ResumeKey != "fix-auth-2" || s3.ResumeKey != "fix-auth-3" {
		t.Fatalf("resume_key must match slug: %q, %q", s2.ResumeKey, s3.ResumeKey)
	}
}

func TestSlugUniquenessAcrossKindsAllowed(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "s1", Kind: "pi", ResumeKey: "fix-auth"})
	s.Upsert(Session{ID: "s2", Kind: "claude", ResumeKey: "fix-auth"})

	s1, _ := s.Get("s1")
	s2, _ := s.Get("s2")

	if s1.Slug != "fix-auth" || s2.Slug != "fix-auth" {
		t.Fatalf("same slug across kinds should be allowed: %q, %q", s1.Slug, s2.Slug)
	}
}

func TestSlugStableOnUpdate(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "s1", Kind: "pi", ResumeKey: "fix-auth"})

	s.Update("s1", func(sess *Session) {
		sess.Subtitle = "something"
	})
	updated, _ := s.Get("s1")
	if updated.Slug != "fix-auth" {
		t.Fatalf("slug changed on unrelated update: got %q", updated.Slug)
	}
}

func TestSlugFreedAfterRemove(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "s1", Kind: "pi", ResumeKey: "fix-auth"})
	s.Remove("s1")

	// Same resume_key should get the unsuffixed slug.
	s.Upsert(Session{ID: "s2", Kind: "pi", ResumeKey: "fix-auth"})
	s2, _ := s.Get("s2")
	if s2.Slug != "fix-auth" {
		t.Fatalf("expected slug reusable after remove, got %q", s2.Slug)
	}
}

func TestSlugUpdatedWhenResumeKeyArrives(t *testing.T) {
	s := New()
	// Fresh session, no resume_key.
	s.Upsert(Session{ID: "s1", Kind: "pi"})
	before, _ := s.Get("s1")
	if before.Slug != "pi" {
		t.Fatalf("expected kind fallback, got %q", before.Slug)
	}

	// File attribution sets resume_key.
	s.Update("s1", func(sess *Session) {
		sess.ResumeKey = "fix-the-sidebar"
	})
	after, _ := s.Get("s1")
	if after.Slug != "fix-the-sidebar" {
		t.Fatalf("expected slug from resume_key, got %q", after.Slug)
	}
}

func TestSlugNotOverriddenByTitleChange(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "s1", Kind: "pi", ResumeKey: "fix-auth"})

	// Title changes don't affect slug (resume_key is the source of truth).
	s.Update("s1", func(sess *Session) {
		sess.AdapterTitle = "completely different title"
	})
	updated, _ := s.Get("s1")
	if updated.Slug != "fix-auth" {
		t.Fatalf("slug should not change when title changes: got %q", updated.Slug)
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

// The frontend needs slug (URL routing) and resume_key (project session array
// membership for dead sessions). Verify they survive MarshalJSON.
func TestMarshalJSON_FrontendFields(t *testing.T) {
	s := Session{
		ID:        "s1",
		Kind:      "pi",
		Alive:     false,
		Slug:      "fix-auth",
		ResumeKey: "2026-04-03T06-46-56_07b3c9c8",
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]interface{}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["slug"] != "fix-auth" {
		t.Errorf("slug missing from wire JSON: %s", data)
	}
	if wire["resume_key"] != "2026-04-03T06-46-56_07b3c9c8" {
		t.Errorf("resume_key missing from wire JSON: %s", data)
	}
	// Internal fields the frontend doesn't need should be excluded.
	if _, ok := wire["shell_title"]; ok {
		t.Errorf("shell_title should be excluded from wire JSON: %s", data)
	}
	if _, ok := wire["binary_hash"]; ok {
		t.Errorf("binary_hash should be excluded from wire JSON: %s", data)
	}
}
