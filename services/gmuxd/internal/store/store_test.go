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

// --- ResumeKey uniqueness tests ---
//
// ResumeKey is the single human-readable identifier used for both URL
// routing and session resumption. The store enforces uniqueness within
// (kind, peer). Sessions without a ResumeKey (fresh launches before
// file attribution) are left alone; the frontend falls back to id[:8].

func TestResumeKeyPreserved(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "s1", Kind: "pi", ResumeKey: "fix-the-auth-bug"})
	got, _ := s.Get("s1")
	if got.ResumeKey != "fix-the-auth-bug" {
		t.Fatalf("resume_key = %q, want %q", got.ResumeKey, "fix-the-auth-bug")
	}
}

func TestResumeKeyEmptyForFreshLaunch(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "s1", Kind: "pi"})
	got, _ := s.Get("s1")
	if got.ResumeKey != "" {
		t.Fatalf("fresh launch should have empty resume_key, got %q", got.ResumeKey)
	}
}

func TestResumeKeyUniqueness(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "s1", Kind: "pi", ResumeKey: "fix-auth"})
	s.Upsert(Session{ID: "s2", Kind: "pi", ResumeKey: "fix-auth"})
	s.Upsert(Session{ID: "s3", Kind: "pi", ResumeKey: "fix-auth"})

	s1, _ := s.Get("s1")
	s2, _ := s.Get("s2")
	s3, _ := s.Get("s3")

	if s1.ResumeKey != "fix-auth" || s2.ResumeKey != "fix-auth-2" || s3.ResumeKey != "fix-auth-3" {
		t.Fatalf("expected suffixed resume_keys: %q, %q, %q", s1.ResumeKey, s2.ResumeKey, s3.ResumeKey)
	}
}

func TestResumeKeyUniquenessAcrossKindsAllowed(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "s1", Kind: "pi", ResumeKey: "fix-auth"})
	s.Upsert(Session{ID: "s2", Kind: "claude", ResumeKey: "fix-auth"})

	s1, _ := s.Get("s1")
	s2, _ := s.Get("s2")

	if s1.ResumeKey != "fix-auth" || s2.ResumeKey != "fix-auth" {
		t.Fatalf("same key across kinds should be allowed: %q, %q", s1.ResumeKey, s2.ResumeKey)
	}
}

func TestResumeKeyStableOnUpdate(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "s1", Kind: "pi", ResumeKey: "fix-auth"})

	s.Update("s1", func(sess *Session) {
		sess.Subtitle = "something"
	})
	updated, _ := s.Get("s1")
	if updated.ResumeKey != "fix-auth" {
		t.Fatalf("resume_key changed on unrelated update: got %q", updated.ResumeKey)
	}
}

func TestResumeKeyFreedAfterRemove(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "s1", Kind: "pi", ResumeKey: "fix-auth"})
	s.Remove("s1")

	s.Upsert(Session{ID: "s2", Kind: "pi", ResumeKey: "fix-auth"})
	s2, _ := s.Get("s2")
	if s2.ResumeKey != "fix-auth" {
		t.Fatalf("expected resume_key reusable after remove, got %q", s2.ResumeKey)
	}
}

func TestResumeKeySetByAttribution(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "s1", Kind: "pi"})
	before, _ := s.Get("s1")
	if before.ResumeKey != "" {
		t.Fatalf("expected empty resume_key before attribution, got %q", before.ResumeKey)
	}

	// File attribution sets resume_key.
	s.Update("s1", func(sess *Session) {
		sess.ResumeKey = "fix-the-sidebar"
	})
	after, _ := s.Get("s1")
	if after.ResumeKey != "fix-the-sidebar" {
		t.Fatalf("resume_key = %q, want %q", after.ResumeKey, "fix-the-sidebar")
	}
}

func TestEmptyResumeKeysCoexist(t *testing.T) {
	s := New()
	// Two fresh launches, neither attributed yet.
	s.Upsert(Session{ID: "s1", Kind: "pi", Alive: true})
	s.Upsert(Session{ID: "s2", Kind: "pi", Alive: true})

	s1, _ := s.Get("s1")
	s2, _ := s.Get("s2")
	if s1.ResumeKey != "" || s2.ResumeKey != "" {
		t.Fatalf("fresh sessions should have empty resume_key, got %q and %q", s1.ResumeKey, s2.ResumeKey)
	}
}

func TestResumeKeyStableOnTitleChange(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "s1", Kind: "pi", ResumeKey: "fix-auth"})

	s.Update("s1", func(sess *Session) {
		sess.AdapterTitle = "completely different title"
	})
	updated, _ := s.Get("s1")
	if updated.ResumeKey != "fix-auth" {
		t.Fatalf("resume_key should not change when title changes: got %q", updated.ResumeKey)
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

// ── Peer-scoped ResumeKey uniqueness ──

func TestResumeKeyUniqueness_ScopedToPeer(t *testing.T) {
	s := New()

	// Local session with resume_key "fix-auth".
	s.Upsert(Session{ID: "s1", Kind: "pi", Alive: true, ResumeKey: "fix-auth"})

	// Remote session from "server" with the same resume_key. Should NOT be
	// renamed because it's in a different (kind, peer) scope.
	s.Upsert(Session{ID: "s2@server", Kind: "pi", Alive: true, ResumeKey: "fix-auth", Peer: "server"})

	got, _ := s.Get("s2@server")
	if got.ResumeKey != "fix-auth" {
		t.Errorf("remote resume_key = %q, want %q (should not conflict with local)", got.ResumeKey, "fix-auth")
	}
}

func TestResumeKeyUniqueness_WithinSamePeer(t *testing.T) {
	s := New()

	s.Upsert(Session{ID: "s1@server", Kind: "pi", Alive: true, ResumeKey: "fix-auth", Peer: "server"})
	s.Upsert(Session{ID: "s2@server", Kind: "pi", Alive: true, ResumeKey: "fix-auth", Peer: "server"})

	got, _ := s.Get("s2@server")
	if got.ResumeKey != "fix-auth-2" {
		t.Errorf("resume_key = %q, want %q", got.ResumeKey, "fix-auth-2")
	}
}

// ── RemoveByPeer / ListByPeer ──

func TestRemoveByPeer(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "local-1", Kind: "pi", Alive: true})
	s.Upsert(Session{ID: "s1@server", Kind: "pi", Alive: true, Peer: "server"})
	s.Upsert(Session{ID: "s2@server", Kind: "shell", Alive: true, Peer: "server"})
	s.Upsert(Session{ID: "s3@dev", Kind: "pi", Alive: true, Peer: "dev"})

	removed := s.RemoveByPeer("server")
	if len(removed) != 2 {
		t.Fatalf("removed %d, want 2", len(removed))
	}

	// Local and other peer sessions should remain.
	if _, ok := s.Get("local-1"); !ok {
		t.Error("local session should not be removed")
	}
	if _, ok := s.Get("s3@dev"); !ok {
		t.Error("dev session should not be removed")
	}

	// Server sessions should be gone.
	if _, ok := s.Get("s1@server"); ok {
		t.Error("server session should be removed")
	}
}

func TestListByPeer(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "local-1", Kind: "pi", Alive: true})
	s.Upsert(Session{ID: "s1@server", Kind: "pi", Alive: true, Peer: "server"})
	s.Upsert(Session{ID: "s2@server", Kind: "shell", Alive: true, Peer: "server"})

	ids := s.ListByPeer("server")
	if len(ids) != 2 {
		t.Fatalf("ListByPeer = %d, want 2", len(ids))
	}

	// Empty peer matches local sessions.
	ids = s.ListByPeer("")
	if len(ids) != 1 {
		t.Errorf("ListByPeer('') = %d, want 1 (local session)", len(ids))
	}
}

// ── Peer field in MarshalJSON ──

func TestMarshalJSON_PeerField(t *testing.T) {
	// Peer field present.
	s := Session{ID: "s1@server", Kind: "pi", Alive: true, Peer: "server"}
	data, _ := json.Marshal(s)
	var wire map[string]interface{}
	json.Unmarshal(data, &wire)
	if wire["peer"] != "server" {
		t.Errorf("peer = %v, want %q", wire["peer"], "server")
	}

	// Local session: peer should be omitted.
	s2 := Session{ID: "s2", Kind: "pi", Alive: true}
	data2, _ := json.Marshal(s2)
	var wire2 map[string]interface{}
	json.Unmarshal(data2, &wire2)
	if _, ok := wire2["peer"]; ok {
		t.Errorf("local session should not have peer field in JSON")
	}
}

func TestMarshalJSON_FrontendFields(t *testing.T) {
	s := Session{
		ID:        "s1",
		Kind:      "pi",
		Alive:     false,
		ResumeKey: "fix-auth",
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]interface{}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["resume_key"] != "fix-auth" {
		t.Errorf("resume_key missing from wire JSON: %s", data)
	}
	if _, ok := wire["slug"]; ok {
		t.Errorf("slug should no longer be in wire JSON: %s", data)
	}
	// Internal fields the frontend doesn't need should be excluded.
	if _, ok := wire["shell_title"]; ok {
		t.Errorf("shell_title should be excluded from wire JSON: %s", data)
	}
	if _, ok := wire["binary_hash"]; ok {
		t.Errorf("binary_hash should be excluded from wire JSON: %s", data)
	}
}

func TestUpsertCanonicalizesPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	s := New()
	s.Upsert(Session{
		ID:            "s1",
		Kind:          "pi",
		Alive:         true,
		Cwd:           home + "/dev/gmux/src",
		WorkspaceRoot: home + "/dev/gmux",
	})

	got, ok := s.Get("s1")
	if !ok {
		t.Fatal("expected session to exist")
	}
	if got.Cwd != "~/dev/gmux/src" {
		t.Errorf("Cwd = %q, want %q", got.Cwd, "~/dev/gmux/src")
	}
	if got.WorkspaceRoot != "~/dev/gmux" {
		t.Errorf("WorkspaceRoot = %q, want %q", got.WorkspaceRoot, "~/dev/gmux")
	}
}

func TestUpdateCanonicalizesPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	s := New()
	s.Upsert(Session{ID: "s1", Kind: "pi", Alive: true, Cwd: "/tmp"})

	s.Update("s1", func(sess *Session) {
		sess.Cwd = home + "/projects/app"
	})

	got, _ := s.Get("s1")
	if got.Cwd != "~/projects/app" {
		t.Errorf("Cwd after Update = %q, want %q", got.Cwd, "~/projects/app")
	}
}
