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

func TestUpsertAliveSessionRemovesDeadShadowWithSameSlug(t *testing.T) {
	s := New()
	s.Upsert(Session{
		ID: "dead-abc", Kind: "pi", Alive: false,
		Command: []string{"pi"}, Slug: "rk-1", AdapterTitle: "shadow",
	})
	s.Upsert(Session{
		ID: "sess-123", Kind: "pi", Alive: true,
		Slug: "rk-1", AdapterTitle: "live",
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
		Slug: "rk-1", AdapterTitle: "live",
	})
	s.Upsert(Session{
		ID: "dead-abc", Kind: "pi", Alive: false,
		Command: []string{"pi"}, Slug: "rk-1", AdapterTitle: "shadow",
	})

	items := s.List()
	if len(items) != 1 {
		t.Fatalf("expected 1 session, got %d: %+v", len(items), items)
	}
	if items[0].ID != "sess-123" {
		t.Fatalf("expected live session to remain, got %q", items[0].ID)
	}
}

func TestUpdateSlugOnAliveSessionRemovesDeadShadow(t *testing.T) {
	s := New()
	s.Upsert(Session{
		ID: "dead-abc", Kind: "pi", Alive: false,
		Command: []string{"pi"}, Slug: "rk-1", AdapterTitle: "shadow",
	})
	s.Upsert(Session{
		ID: "sess-123", Kind: "pi", Alive: true, AdapterTitle: "live",
	})

	ok := s.Update("sess-123", func(sess *Session) {
		sess.Slug = "rk-1"
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

func TestSlugDedup_ScopedToKindAndPeer(t *testing.T) {
	s := New()
	// Dead pi session with slug "fix-auth".
	s.Upsert(Session{ID: "dead-1", Kind: "pi", Alive: false, Command: []string{"pi"}, Slug: "fix-auth"})
	// Live claude session with the same slug — different kind, should NOT remove the dead pi session.
	s.Upsert(Session{ID: "live-1", Kind: "claude", Alive: true, Slug: "fix-auth"})

	if len(s.List()) != 2 {
		t.Fatalf("expected 2 sessions (different kinds coexist), got %d", len(s.List()))
	}

	// Now a live pi session arrives — should remove the dead pi session.
	s.Upsert(Session{ID: "live-2", Kind: "pi", Alive: true, Slug: "fix-auth"})

	items := s.List()
	if len(items) != 2 {
		t.Fatalf("expected 2 sessions (claude + live pi), got %d", len(items))
	}
	for _, item := range items {
		if item.ID == "dead-1" {
			t.Error("dead pi session should have been replaced by live pi session")
		}
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

// --- Slug uniqueness tests ---
//
// Slug is the single human-readable identifier used for both URL
// routing and session resumption. The store enforces uniqueness within
// (kind, peer). Sessions without a Slug (fresh launches before
// file attribution) are left alone; the frontend falls back to id[:8].

func TestSlugPreserved(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "s1", Kind: "pi", Slug: "fix-the-auth-bug"})
	got, _ := s.Get("s1")
	if got.Slug != "fix-the-auth-bug" {
		t.Fatalf("slug = %q, want %q", got.Slug, "fix-the-auth-bug")
	}
}

func TestSlugEmptyForFreshLaunch(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "s1", Kind: "pi"})
	got, _ := s.Get("s1")
	if got.Slug != "" {
		t.Fatalf("fresh launch should have empty slug, got %q", got.Slug)
	}
}

func TestSlugUniqueness(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "s1", Kind: "pi", Slug: "fix-auth"})
	s.Upsert(Session{ID: "s2", Kind: "pi", Slug: "fix-auth"})
	s.Upsert(Session{ID: "s3", Kind: "pi", Slug: "fix-auth"})

	s1, _ := s.Get("s1")
	s2, _ := s.Get("s2")
	s3, _ := s.Get("s3")

	if s1.Slug != "fix-auth" || s2.Slug != "fix-auth-2" || s3.Slug != "fix-auth-3" {
		t.Fatalf("expected suffixed slugs: %q, %q, %q", s1.Slug, s2.Slug, s3.Slug)
	}
}

func TestSlugUniquenessAcrossKindsAllowed(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "s1", Kind: "pi", Slug: "fix-auth"})
	s.Upsert(Session{ID: "s2", Kind: "claude", Slug: "fix-auth"})

	s1, _ := s.Get("s1")
	s2, _ := s.Get("s2")

	if s1.Slug != "fix-auth" || s2.Slug != "fix-auth" {
		t.Fatalf("same key across kinds should be allowed: %q, %q", s1.Slug, s2.Slug)
	}
}

func TestSlugStableOnUpdate(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "s1", Kind: "pi", Slug: "fix-auth"})

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
	s.Upsert(Session{ID: "s1", Kind: "pi", Slug: "fix-auth"})
	s.Remove("s1")

	s.Upsert(Session{ID: "s2", Kind: "pi", Slug: "fix-auth"})
	s2, _ := s.Get("s2")
	if s2.Slug != "fix-auth" {
		t.Fatalf("expected slug reusable after remove, got %q", s2.Slug)
	}
}

func TestSlugSetByAttribution(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "s1", Kind: "pi"})
	before, _ := s.Get("s1")
	if before.Slug != "" {
		t.Fatalf("expected empty slug before attribution, got %q", before.Slug)
	}

	// File attribution sets slug.
	s.Update("s1", func(sess *Session) {
		sess.Slug = "fix-the-sidebar"
	})
	after, _ := s.Get("s1")
	if after.Slug != "fix-the-sidebar" {
		t.Fatalf("slug = %q, want %q", after.Slug, "fix-the-sidebar")
	}
}

func TestEmptySlugsCoexist(t *testing.T) {
	s := New()
	// Two fresh launches, neither attributed yet.
	s.Upsert(Session{ID: "s1", Kind: "pi", Alive: true})
	s.Upsert(Session{ID: "s2", Kind: "pi", Alive: true})

	s1, _ := s.Get("s1")
	s2, _ := s.Get("s2")
	if s1.Slug != "" || s2.Slug != "" {
		t.Fatalf("fresh sessions should have empty slug, got %q and %q", s1.Slug, s2.Slug)
	}
}

func TestSlugStableOnTitleChange(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "s1", Kind: "pi", Slug: "fix-auth"})

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

// ── Peer-scoped Slug uniqueness ──

func TestSlugUniqueness_ScopedToPeer(t *testing.T) {
	s := New()

	// Local session with slug "fix-auth".
	s.Upsert(Session{ID: "s1", Kind: "pi", Alive: true, Slug: "fix-auth"})

	// Remote session from "server" with the same slug. Should NOT be
	// renamed because it's in a different (kind, peer) scope.
	s.Upsert(Session{ID: "s2@server", Kind: "pi", Alive: true, Slug: "fix-auth", Peer: "server"})

	got, _ := s.Get("s2@server")
	if got.Slug != "fix-auth" {
		t.Errorf("remote slug = %q, want %q (should not conflict with local)", got.Slug, "fix-auth")
	}
}

func TestSlugUniqueness_WithinSamePeer(t *testing.T) {
	s := New()

	s.Upsert(Session{ID: "s1@server", Kind: "pi", Alive: true, Slug: "fix-auth", Peer: "server"})
	s.Upsert(Session{ID: "s2@server", Kind: "pi", Alive: true, Slug: "fix-auth", Peer: "server"})

	got, _ := s.Get("s2@server")
	if got.Slug != "fix-auth-2" {
		t.Errorf("slug = %q, want %q", got.Slug, "fix-auth-2")
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
		Slug: "fix-auth",
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
	if _, ok := wire["resume_key"]; ok {
		t.Errorf("old resume_key key should not be in wire JSON: %s", data)
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

// ── UpsertRemote ───────────────────────────────────────────────
//
// UpsertRemote writes a session that was already fully resolved on a
// peer. Title and Resumable must be preserved verbatim; canonicalization,
// dedup, and broadcast must still run.

func TestUpsertRemote_PreservesTitle(t *testing.T) {
	s := New()
	// A remote session arrives with Title already set (spoke
	// resolved it) but internal ShellTitle/AdapterTitle empty (those
	// are off-wire). Upsert would overwrite Title with Kind; UpsertRemote
	// must not.
	s.UpsertRemote(Session{
		ID:    "sess-123@server",
		Kind:  "codex",
		Alive: true,
		Peer:  "server",
		Title: "fix remote bug",
	})

	got, ok := s.Get("sess-123@server")
	if !ok {
		t.Fatal("expected session to exist")
	}
	if got.Title != "fix remote bug" {
		t.Errorf("Title = %q, want %q (UpsertRemote must not re-resolve)", got.Title, "fix remote bug")
	}
}

func TestUpsertRemote_PreservesResumableFromSpoke(t *testing.T) {
	s := New()
	// A dead remote session with Resumable explicitly set to false
	// by the spoke (e.g. a shell session with no command recorded).
	// Upsert would derive Resumable from !Alive && len(Command) > 0;
	// UpsertRemote must preserve the spoke's value.
	s.UpsertRemote(Session{
		ID:        "sess-1@server",
		Kind:      "pi",
		Alive:     false,
		Command:   []string{"pi"},
		Peer:      "server",
		Resumable: false, // spoke says not resumable despite command
		Title:     "archived",
	})
	got, _ := s.Get("sess-1@server")
	if got.Resumable {
		t.Errorf("Resumable = true, want false (UpsertRemote must not re-derive)")
	}
}

func TestUpsertRemote_CanonicalizesPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s := New()
	s.UpsertRemote(Session{
		ID:    "sess-path@server",
		Kind:  "shell",
		Alive: true,
		Peer:  "server",
		Title: "ok",
		Cwd:   home + "/projects/app",
	})
	got, _ := s.Get("sess-path@server")
	if got.Cwd != "~/projects/app" {
		t.Errorf("Cwd = %q, want %q (canonicalization must still run)", got.Cwd, "~/projects/app")
	}
}

func TestUpsertRemote_DedupsSlug(t *testing.T) {
	s := New()
	s.UpsertRemote(Session{
		ID:        "sess-1@server",
		Kind:      "codex",
		Alive:     true,
		Peer:      "server",
		Title:     "a",
		Slug: "fix-bug",
	})
	s.UpsertRemote(Session{
		ID:        "sess-2@server",
		Kind:      "codex",
		Alive:     true,
		Peer:      "server",
		Title:     "b",
		Slug: "fix-bug",
	})

	got, _ := s.Get("sess-2@server")
	if got.Slug == "fix-bug" {
		t.Errorf("Slug = %q, want a de-duplicated value (e.g. fix-bug-2)", got.Slug)
	}
}

func TestUpsertRemote_BroadcastsEvent(t *testing.T) {
	s := New()
	ch, cancel := s.Subscribe()
	defer cancel()

	s.UpsertRemote(Session{
		ID:    "sess-1@server",
		Kind:  "codex",
		Alive: true,
		Peer:  "server",
		Title: "hello from spoke",
	})

	select {
	case ev := <-ch:
		if ev.Type != "session-upsert" {
			t.Errorf("event type = %q, want session-upsert", ev.Type)
		}
		if ev.Session == nil || ev.Session.Title != "hello from spoke" {
			t.Errorf("broadcast session title wrong; got %+v", ev.Session)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("no broadcast")
	}
}




func TestSessionMarshalJSON_WireFormat(t *testing.T) {
	s := Session{
		ID:            "sess-abc",
		Kind:          "pi",
		Alive:         true,
		RunnerVersion: "1.2.0",
		BinaryHash:    "aabbccdd",
		ShellTitle:    "internal-only",
		AdapterTitle:  "internal-only",
		Slug:          "my-slug",
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Fields that must appear on the wire.
	if got := m["runner_version"]; got != "1.2.0" {
		t.Errorf("runner_version = %v, want 1.2.0", got)
	}
	if got := m["binary_hash"]; got != "aabbccdd" {
		t.Errorf("binary_hash = %v, want aabbccdd", got)
	}
	if got := m["slug"]; got != "my-slug" {
		t.Errorf("slug = %v, want my-slug", got)
	}

	// Internal fields must not appear on the wire.
	if _, ok := m["shell_title"]; ok {
		t.Error("shell_title must not appear in wire JSON")
	}
	if _, ok := m["adapter_title"]; ok {
		t.Error("adapter_title must not appear in wire JSON")
	}
	if _, ok := m["stale"]; ok {
		t.Error("stale was removed; must not appear in wire JSON")
	}
}
