package store

import (
	"encoding/json"
	"reflect"
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

// recvEvent waits briefly for the next bus event, or returns ok=false
// if none arrives. Used to assert both presence and absence of a
// broadcast.
func recvEvent(t *testing.T, ch <-chan Event) (Event, bool) {
	t.Helper()
	select {
	case ev := <-ch:
		return ev, true
	case <-time.After(150 * time.Millisecond):
		return Event{}, false
	}
}

// TestUpsertIdenticalSuppressesBroadcast is the core regression guard
// against peer-snapshot amplification: re-storing a byte-identical
// session must not emit a second session-upsert. A spoke re-ships its
// whole snapshot on every change, so without this the hub fans every
// unchanged mirrored session out to every SSE subscriber at the
// spoke's cadence.
func TestUpsertIdenticalSuppressesBroadcast(t *testing.T) {
	s := New()
	ch, cancel := s.Subscribe()
	defer cancel()

	sess := Session{ID: "s1", Kind: "pi", Alive: true, Slug: "fix", Cwd: "/work"}
	s.Upsert(sess)
	if ev, ok := recvEvent(t, ch); !ok || ev.Type != "session-upsert" {
		t.Fatalf("first upsert should broadcast, got ok=%v ev=%+v", ok, ev)
	}

	// Identical re-upsert: no broadcast.
	s.Upsert(sess)
	if ev, ok := recvEvent(t, ch); ok {
		t.Fatalf("identical re-upsert should be silent, got %+v", ev)
	}
}

// TestUpsertRemoteIdenticalSuppressesBroadcast covers the actual peer
// mirroring path (UpsertRemote skips title/resumable derivation).
func TestUpsertRemoteIdenticalSuppressesBroadcast(t *testing.T) {
	s := New()
	ch, cancel := s.Subscribe()
	defer cancel()

	sess := Session{ID: "s1@peer", Kind: "pi", Peer: "peer", Alive: true, Slug: "fix", Title: "Fix auth"}
	s.UpsertRemote(sess)
	if _, ok := recvEvent(t, ch); !ok {
		t.Fatal("first UpsertRemote should broadcast")
	}
	s.UpsertRemote(sess)
	if ev, ok := recvEvent(t, ch); ok {
		t.Fatalf("identical UpsertRemote should be silent, got %+v", ev)
	}
}

// TestUpsertChangedFieldStillBroadcasts guards the other direction:
// dedup must not swallow real state changes. A single flipped field
// must produce exactly one broadcast carrying the new value.
func TestUpsertChangedFieldStillBroadcasts(t *testing.T) {
	s := New()
	ch, cancel := s.Subscribe()
	defer cancel()

	sess := Session{ID: "s1", Kind: "pi", Alive: true, Slug: "fix"}
	s.Upsert(sess)
	if _, ok := recvEvent(t, ch); !ok {
		t.Fatal("first upsert should broadcast")
	}

	sess.Alive = false // a real transition
	s.Upsert(sess)
	ev, ok := recvEvent(t, ch)
	if !ok {
		t.Fatal("changed upsert should broadcast")
	}
	if ev.Session == nil || ev.Session.Alive {
		t.Fatalf("broadcast should carry the new (dead) state, got %+v", ev.Session)
	}
	// And no spurious follow-up.
	if ev, ok := recvEvent(t, ch); ok {
		t.Fatalf("unexpected extra broadcast: %+v", ev)
	}
}

// TestUpdateNoOpSuppressesBroadcast: Update routes through the same
// dedup as Upsert. A modifier that leaves the session byte-identical
// (the file monitor re-reading unchanged metadata, a status handler
// re-stamping the same status) must not broadcast.
func TestUpdateNoOpSuppressesBroadcast(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "s1", Kind: "pi", Alive: true, Slug: "fix"})
	ch, cancel := s.Subscribe()
	defer cancel()

	// A modifier that changes nothing.
	if !s.Update("s1", func(*Session) {}) {
		t.Fatal("Update should return true for an existing session")
	}
	if ev, ok := recvEvent(t, ch); ok {
		t.Fatalf("no-op Update should be silent, got %+v", ev)
	}
}

// TestUpdateRealChangeBroadcasts: a modifier that actually changes a
// field still broadcasts exactly once with the new value.
func TestUpdateRealChangeBroadcasts(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "s1", Kind: "pi", Alive: true, Slug: "fix"})
	ch, cancel := s.Subscribe()
	defer cancel()

	s.Update("s1", func(sess *Session) { sess.Unread = true })
	ev, ok := recvEvent(t, ch)
	if !ok {
		t.Fatal("real Update should broadcast")
	}
	if ev.Session == nil || !ev.Session.Unread {
		t.Fatalf("broadcast should carry the new unread state, got %+v", ev.Session)
	}
	if ev, ok := recvEvent(t, ch); ok {
		t.Fatalf("unexpected extra broadcast: %+v", ev)
	}
}

// TestSetTerminalSizeNoOpSuppressesBroadcast: the runner re-emits
// terminal_resize even when dimensions are unchanged; a same-size
// SetTerminalSize must not fan out a snapshot.
func TestSetTerminalSizeNoOpSuppressesBroadcast(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "s1", Kind: "pi", Alive: true})
	s.SetTerminalSize("s1", 80, 24)
	ch, cancel := s.Subscribe()
	defer cancel()

	// Same dimensions again: silent.
	if !s.SetTerminalSize("s1", 80, 24) {
		t.Fatal("SetTerminalSize should return true for an existing session")
	}
	if ev, ok := recvEvent(t, ch); ok {
		t.Fatalf("same-size SetTerminalSize should be silent, got %+v", ev)
	}

	// A genuine resize still broadcasts.
	s.SetTerminalSize("s1", 100, 30)
	ev, ok := recvEvent(t, ch)
	if !ok {
		t.Fatal("changed SetTerminalSize should broadcast")
	}
	if ev.Session == nil || ev.Session.TerminalCols != 100 || ev.Session.TerminalRows != 30 {
		t.Fatalf("broadcast should carry the new size, got %+v", ev.Session)
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

// internalSessionFields lists struct fields that intentionally do
// not appear on the wire. The MarshalJSON "wire" struct excludes
// these; they are kept on Session for internal resolution (Title
// merging) but never sent to clients. Add a field here if and only
// if you are deliberately keeping it server-side.
var internalSessionFields = map[string]struct{}{
	"ShellTitle":   {},
	"AdapterTitle": {},
}

// TestSessionMarshalJSON_AllFieldsAppearOnWire is the gotcha-catcher
// for "someone added a field to Session with a json tag, but forgot
// to plumb it through the explicit `wire` struct in MarshalJSON."
// This is exactly how `last_activity_at` was almost silently dropped
// during PR #229: the field had a tag on the struct but wasn't
// enumerated in the wire literal, so it would never have reached
// any API consumer.
//
// The test reflects over every json-tagged field on Session,
// populates each to a non-zero value, marshals, and asserts the
// tag's wire name appears in the output. New fields fall into one of
// two camps: either they belong on the wire (and the wire struct
// needs an entry, which this test forces) or they're internal (and
// they need to be added to internalSessionFields with a comment
// justifying the exclusion). Either way the failure mode is loud,
// not silent.
func TestSessionMarshalJSON_AllFieldsAppearOnWire(t *testing.T) {
	sess := fullyPopulatedSession()
	b, err := json.Marshal(sess)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	st := reflect.TypeOf(sess)
	for i := 0; i < st.NumField(); i++ {
		f := st.Field(i)
		tag := f.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		wireName := strings.SplitN(tag, ",", 2)[0]
		_, isInternal := internalSessionFields[f.Name]
		_, onWire := m[wireName]
		switch {
		case isInternal && onWire:
			t.Errorf("field %s (wire: %s) is listed as internal but appears in MarshalJSON output", f.Name, wireName)
		case !isInternal && !onWire:
			t.Errorf("field %s (wire: %s) has a json tag but is missing from MarshalJSON's wire struct; "+
				"either add it to the wire literal in store.Session.MarshalJSON or add %q to internalSessionFields",
				f.Name, wireName, f.Name)
		}
	}
}

// fullyPopulatedSession returns a Session with every json-tagged
// field set to a non-zero value. Used by the all-fields-on-wire
// test; centralised so adding a new field forces a compile-time
// touchpoint here too.
func fullyPopulatedSession() Session {
	exit := 0
	return Session{
		ID:             "sess-full",
		Peer:           "peer-x",
		CreatedAt:      "2026-01-01T00:00:00Z",
		Command:        []string{"bash"},
		Cwd:            "/tmp/work",
		Kind:           "pi",
		WorkspaceRoot:  "/tmp/work",
		Remotes:        map[string]string{"origin": "github.com/x/y"},
		Alive:          true,
		Pid:            42,
		ExitCode:       &exit,
		StartedAt:      "2026-01-01T00:00:01Z",
		ExitedAt:       "2026-01-01T00:00:02Z",
		Title:          "t",
		Subtitle:       "s",
		Status:         &Status{Label: "l", Working: true, Error: true},
		Unread:         true,
		LastActivityAt: "2026-01-01T00:00:03Z",
		Resumable:      true,
		SocketPath:     "/tmp/sock",
		TerminalCols:   80,
		TerminalRows:   24,
		Slug:           "slug",
		RunnerVersion:  "v1",
		BinaryHash:     "hash",
		ShellTitle:     "shell-internal",
		AdapterTitle:   "adapter-internal",
		ProjectSlug:    "proj",
		ProjectIndex:   3,
	}
}

// TestSessionMarshalJSON_LastActivityAt pins that LastActivityAt
// makes it onto the wire. The Session type has a custom MarshalJSON
// (an explicit `wire` struct, not the default struct-tag reflection),
// so adding a field to the struct alone is silently insufficient:
// the field has to be enumerated in the wire struct and the
// json.Marshal call. This test exists specifically to catch that
// class of regression, not to test the std library's JSON encoder.
func TestSessionMarshalJSON_LastActivityAt(t *testing.T) {
	ts := "2026-05-23T10:30:00Z"
	s := Session{ID: "sess-1", Kind: "pi", Alive: true, LastActivityAt: ts}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := m["last_activity_at"]; got != ts {
		t.Fatalf("last_activity_at on wire = %v, want %q", got, ts)
	}

	// And: when unset, the field is omitted (omitempty), not sent as
	// an empty string. Matters because the frontend treats "" and
	// absent identically today but might diverge later, and empty
	// strings on RFC3339 fields are a smell.
	s2 := Session{ID: "sess-2", Kind: "pi", Alive: true}
	b2, _ := json.Marshal(s2)
	var m2 map[string]any
	json.Unmarshal(b2, &m2)
	if _, present := m2["last_activity_at"]; present {
		t.Errorf("unset LastActivityAt should be omitted, got %v", m2["last_activity_at"])
	}
}

// TestSessionRoundTrip_LastActivityAt pins the peer-payload path:
// the hub receives a peer session as JSON and json.Unmarshals into
// store.Session, then UpsertRemote stores it. The default tag-based
// unmarshal populates LastActivityAt from the wire; this test exists
// because the symmetry between MarshalJSON (custom) and Unmarshal
// (default) is the kind of asymmetry that breaks silently when
// someone adds an UnmarshalJSON later.
func TestSessionRoundTrip_LastActivityAt(t *testing.T) {
	ts := "2026-05-23T10:30:00Z"
	original := Session{ID: "sess-1", Kind: "pi", Alive: true, LastActivityAt: ts}
	b, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded Session
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.LastActivityAt != ts {
		t.Fatalf("round-trip LastActivityAt = %q, want %q", decoded.LastActivityAt, ts)
	}
}

// --- Reconcile (project ownership stamping per ADR 0002) ---

func TestReconcile_StampsOwnedSessions(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "a", Slug: "alpha", Kind: "k", Alive: true})
	s.Upsert(Session{ID: "b", Slug: "beta", Kind: "k", Alive: true})

	s.Reconcile(func(sess Session) (string, int) {
		switch sess.Slug {
		case "alpha":
			return "gmux", 0
		case "beta":
			return "gmux", 1
		}
		return "", 0
	})

	got, _ := s.Get("a")
	if got.ProjectSlug != "gmux" || got.ProjectIndex != 0 {
		t.Errorf("a: got slug=%q index=%d, want gmux/0", got.ProjectSlug, got.ProjectIndex)
	}
	got, _ = s.Get("b")
	if got.ProjectSlug != "gmux" || got.ProjectIndex != 1 {
		t.Errorf("b: got slug=%q index=%d, want gmux/1", got.ProjectSlug, got.ProjectIndex)
	}
}

func TestReconcile_CallerControlsPeerStampPreservation(t *testing.T) {
	// Reconcile no longer auto-skips peer sessions; the caller decides
	// per session. The canonical caller (reconcileProjectStamps in
	// main.go) preserves stamps for network peers and re-stamps Local
	// peers. This test verifies the contract: assignFn sees every
	// session and its return value is honoured.
	s := New()
	s.UpsertRemote(Session{ID: "p1", Slug: "peer-sess", Kind: "k", Peer: "tower", Alive: true, ProjectSlug: "from-origin", ProjectIndex: 3})

	s.Reconcile(func(sess Session) (string, int) {
		if sess.Peer != "" {
			return sess.ProjectSlug, sess.ProjectIndex // network peer: preserve
		}
		return "local", 0
	})

	got, _ := s.Get("p1")
	if got.ProjectSlug != "from-origin" || got.ProjectIndex != 3 {
		t.Errorf("peer session stamps lost: got slug=%q index=%d, want from-origin/3", got.ProjectSlug, got.ProjectIndex)
	}
}

func TestReconcile_DoesNotBroadcast(t *testing.T) {
	// Reconcile is intentionally silent until the snapshot protocol
	// commit makes the stamps wire-visible. Verify the contract.
	s := New()
	ch, cancel := s.Subscribe()
	defer cancel()
	s.Upsert(Session{ID: "a", Slug: "alpha", Kind: "k", Alive: true})
	// Drain the upsert event from setup.
	<-ch

	done := make(chan struct{})
	go func() {
		s.Reconcile(func(sess Session) (string, int) {
			return "gmux", 0
		})
		close(done)
	}()
	<-done

	select {
	case ev := <-ch:
		t.Fatalf("Reconcile broadcast unexpectedly: %+v", ev)
	default:
	}
}

func TestReconcile_NoOpWhenStampsUnchanged(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "a", Slug: "alpha", Kind: "k", Alive: true})

	calls := 0
	assignFn := func(sess Session) (string, int) {
		calls++
		return "", 0
	}
	s.Reconcile(assignFn)
	first := calls
	s.Reconcile(assignFn)
	if calls-first != 1 {
		t.Errorf("expected one assignFn call on second Reconcile, got %d", calls-first)
	}

	got, _ := s.Get("a")
	if got.ProjectSlug != "" || got.ProjectIndex != 0 {
		t.Errorf("disclaimed stamps drifted: slug=%q index=%d", got.ProjectSlug, got.ProjectIndex)
	}
}

func TestSession_MarshalEmitsProjectStamps(t *testing.T) {
	sess := Session{
		ID:           "a",
		Kind:         "k",
		Alive:        true,
		ProjectSlug:  "gmux",
		ProjectIndex: 4,
	}
	b, err := json.Marshal(sess)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got, _ := m["project_slug"].(string); got != "gmux" {
		t.Errorf("project_slug on wire: got %v, want %q", m["project_slug"], "gmux")
	}
	// Numbers decode as float64 in untyped maps.
	if got, _ := m["project_index"].(float64); got != 4 {
		t.Errorf("project_index on wire: got %v, want 4", m["project_index"])
	}
}

func TestSession_MarshalOmitsDisclaimedStamps(t *testing.T) {
	// A disclaimed session (no project match) leaves slug="" and
	// index=0. Both fields use omitempty so neither appears on the
	// wire; viewers fall through to their own match rules.
	sess := Session{ID: "a", Kind: "k", Alive: true}
	b, err := json.Marshal(sess)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["project_slug"]; ok {
		t.Error("project_slug should be omitted when empty")
	}
	if _, ok := m["project_index"]; ok {
		t.Error("project_index should be omitted when zero alongside empty slug")
	}
}

func TestSession_WireRoundTripPreservesStamps(t *testing.T) {
	// What an origin emits, a peer mirror unmarshals back into a
	// store.Session. The stamps must round-trip so the receiver can
	// render (peer, slug) folders without re-deriving project
	// membership.
	cases := []struct {
		name  string
		slug  string
		index int
	}{
		{"first-in-array", "gmux", 0},
		{"middle", "gmux", 2},
		{"disclaimed", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orig := Session{
				ID:           "a",
				Kind:         "k",
				Alive:        true,
				ProjectSlug:  tc.slug,
				ProjectIndex: tc.index,
			}
			b, err := json.Marshal(orig)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got Session
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got.ProjectSlug != tc.slug {
				t.Errorf("slug round-trip: got %q want %q", got.ProjectSlug, tc.slug)
			}
			if got.ProjectIndex != tc.index {
				t.Errorf("index round-trip: got %d want %d", got.ProjectIndex, tc.index)
			}
		})
	}
}

// TestLastActivityAt_NewSessionDoesNotBump pins option (a): brand-new
// sessions arrive with LastActivityAt unset. This is what keeps
// sessionmeta rehydrate at daemon startup from timestamping every
// resumable dead session to "now".
func TestLastActivityAt_NewSessionDoesNotBump(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "s1", Kind: "pi", Alive: true})
	got, _ := s.Get("s1")
	if got.LastActivityAt != "" {
		t.Fatalf("new session should not bump LastActivityAt, got %q", got.LastActivityAt)
	}
}

// TestLastActivityAt_BumpOnTransitions pins the exact set of state
// transitions that bump: exited, unread on, working on, error on.
// Each subtest starts from an alive-and-idle baseline and applies
// one transition.
func TestLastActivityAt_BumpOnTransitions(t *testing.T) {
	cases := []struct {
		name string
		next func(*Session)
	}{
		{"exited", func(s *Session) { s.Alive = false }},
		{"unread", func(s *Session) { s.Unread = true }},
		{"working", func(s *Session) { s.Status = &Status{Working: true} }},
		{"error", func(s *Session) { s.Status = &Status{Error: true} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New()
			s.Upsert(Session{ID: "s1", Kind: "pi", Alive: true})
			before, _ := s.Get("s1")
			if before.LastActivityAt != "" {
				t.Fatalf("baseline should be unset, got %q", before.LastActivityAt)
			}
			ok := s.Update("s1", tc.next)
			if !ok {
				t.Fatal("update failed")
			}
			after, _ := s.Get("s1")
			if after.LastActivityAt == "" {
				t.Fatalf("transition %q should bump LastActivityAt", tc.name)
			}
			if _, err := time.Parse(time.RFC3339, after.LastActivityAt); err != nil {
				t.Fatalf("LastActivityAt %q is not RFC3339: %v", after.LastActivityAt, err)
			}
		})
	}
}

// TestLastActivityAt_NoBumpOnNoise pins that benign updates do not
// bump: title changes, slug changes, cwd changes, status label-only
// changes. Without this, the recency list would jitter on every
// adapter title refresh.
func TestLastActivityAt_NoBumpOnNoise(t *testing.T) {
	cases := []struct {
		name string
		next func(*Session)
	}{
		{"title", func(s *Session) { s.AdapterTitle = "renamed" }},
		{"slug", func(s *Session) { s.Slug = "new-slug" }},
		{"cwd", func(s *Session) { s.Cwd = "/tmp/elsewhere" }},
		{"status-label", func(s *Session) { s.Status = &Status{Label: "running"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New()
			s.Upsert(Session{ID: "s1", Kind: "pi", Alive: true, AdapterTitle: "orig"})
			// Pre-seed a LastActivityAt by transitioning to unread.
			s.Update("s1", func(sess *Session) { sess.Unread = true })
			seeded, _ := s.Get("s1")
			stamp := seeded.LastActivityAt
			if stamp == "" {
				t.Fatal("expected baseline bump from unread transition")
			}
			// Sleep one second so any erroneous bump would produce
			// a distinct RFC3339 timestamp.
			time.Sleep(1100 * time.Millisecond)
			s.Update("s1", tc.next)
			after, _ := s.Get("s1")
			if after.LastActivityAt != stamp {
				t.Fatalf("noise update %q should not bump (was %q, now %q)", tc.name, stamp, after.LastActivityAt)
			}
		})
	}
}

// TestLastActivityAt_OnlyTransitionEdgeBumps pins that staying in
// the same noteworthy state does not re-bump on every Update. A
// session that is unread, gets another Update while still unread,
// should keep its existing timestamp.
func TestLastActivityAt_OnlyTransitionEdgeBumps(t *testing.T) {
	s := New()
	s.Upsert(Session{ID: "s1", Kind: "pi", Alive: true})
	s.Update("s1", func(sess *Session) { sess.Unread = true })
	first, _ := s.Get("s1")
	time.Sleep(1100 * time.Millisecond)
	s.Update("s1", func(sess *Session) { sess.AdapterTitle = "still unread" })
	second, _ := s.Get("s1")
	if second.LastActivityAt != first.LastActivityAt {
		t.Fatalf("staying unread should not re-bump (was %q, now %q)", first.LastActivityAt, second.LastActivityAt)
	}
}

// TestLastActivityAt_UpsertOnExistingBumps pins that the bump
// machinery fires on Upsert (not just Update). Adapters call Upsert
// repeatedly with the same ID as they refresh metadata; when one of
// those refreshes carries a noteworthy transition (e.g. status going
// to working), it must stamp LastActivityAt the same way Update does.
func TestLastActivityAt_UpsertOnExistingBumps(t *testing.T) {
	cases := []struct {
		name string
		mutate func(*Session)
	}{
		{"exited", func(s *Session) { s.Alive = false }},
		{"unread", func(s *Session) { s.Unread = true }},
		{"working", func(s *Session) { s.Status = &Status{Working: true} }},
		{"error", func(s *Session) { s.Status = &Status{Error: true} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New()
			baseline := Session{ID: "s1", Kind: "pi", Alive: true}
			s.Upsert(baseline)
			before, _ := s.Get("s1")
			if before.LastActivityAt != "" {
				t.Fatalf("baseline should be unset, got %q", before.LastActivityAt)
			}
			next := baseline
			tc.mutate(&next)
			s.Upsert(next)
			after, _ := s.Get("s1")
			if after.LastActivityAt == "" {
				t.Fatalf("Upsert transition %q should bump LastActivityAt", tc.name)
			}
			if _, err := time.Parse(time.RFC3339, after.LastActivityAt); err != nil {
				t.Fatalf("LastActivityAt %q is not RFC3339: %v", after.LastActivityAt, err)
			}
		})
	}
}

// TestLastActivityAt_UpsertNoBumpPreservesStamp pins that a no-bump
// Upsert (routine title / cwd / slug refresh from an adapter) does
// NOT clobber a previously stamped LastActivityAt. Adapters build
// fresh Session structs from runner state without LastActivityAt;
// without an explicit carry-forward we'd silently zero the field on
// every metadata refresh and break Recent ordering.
func TestLastActivityAt_UpsertNoBumpPreservesStamp(t *testing.T) {
	s := New()
	// Seed with a stamped session via a transition.
	s.Upsert(Session{ID: "s1", Kind: "pi", Alive: true})
	s.Update("s1", func(sess *Session) { sess.Unread = true })
	before, _ := s.Get("s1")
	stamp := before.LastActivityAt
	if stamp == "" {
		t.Fatal("baseline transition should have stamped LastActivityAt")
	}
	// Routine refresh: same state, only adapter title changed. No
	// transition fires; LastActivityAt is absent from the payload.
	s.Upsert(Session{ID: "s1", Kind: "pi", Alive: true, Unread: true, AdapterTitle: "renamed"})
	after, _ := s.Get("s1")
	if after.LastActivityAt != stamp {
		t.Fatalf("no-bump Upsert must preserve LastActivityAt (was %q, now %q)", stamp, after.LastActivityAt)
	}
}

// TestLastActivityAt_PointerAliasingDoesNotHideTransition is a
// regression test for a real bug in an earlier draft: Update did
// `sess := prev` (shallow copy), so prev.Status and sess.Status
// pointed to the same Status struct. A mutator that wrote through
// the pointer (e.g. flipped sess.Status.Working from false to true)
// would silently mutate prev too, hiding the false→true transition
// from the bump check. This test pins that the bump fires correctly
// when a mutator constructs a new Status pointer in place of a nil
// one (the common pattern in subscribe.go), AND would have failed
// under the previous implementation if the mutator modified the
// pointer's pointee directly. Snapshotting the activity booleans
// before fn runs sidesteps the alias entirely.
func TestLastActivityAt_PointerAliasingDoesNotHideTransition(t *testing.T) {
	s := New()
	// Seed with a Status pointer present (label only, not working).
	s.Upsert(Session{ID: "s1", Kind: "pi", Alive: true, Status: &Status{Label: "idle"}})

	// Mutator writes Working=true through the existing Status pointer
	// rather than allocating a new Status. This is the alias-risk
	// shape.
	ok := s.Update("s1", func(sess *Session) {
		if sess.Status != nil {
			sess.Status.Working = true
		}
	})
	if !ok {
		t.Fatal("update failed")
	}
	after, _ := s.Get("s1")
	if after.LastActivityAt == "" {
		t.Fatal("working false→true through aliased pointer must still bump LastActivityAt")
	}
}

// TestLastActivityAt_UpsertRemotePreserves pins that peer payloads
// (UpsertRemote) carry through LastActivityAt as-received: the
// owning daemon stamped it, the hub must not recompute.
func TestLastActivityAt_UpsertRemotePreserves(t *testing.T) {
	s := New()
	wired := "2026-01-01T12:00:00Z"
	s.UpsertRemote(Session{ID: "s1", Kind: "pi", Peer: "remote", Alive: true, LastActivityAt: wired})
	got, _ := s.Get("s1")
	if got.LastActivityAt != wired {
		t.Fatalf("UpsertRemote should preserve LastActivityAt (want %q, got %q)", wired, got.LastActivityAt)
	}
	// Even a transition arriving via UpsertRemote must not recompute:
	// it's the spoke's job to stamp, not ours.
	s.UpsertRemote(Session{ID: "s1", Kind: "pi", Peer: "remote", Alive: false, LastActivityAt: wired})
	got2, _ := s.Get("s1")
	if got2.LastActivityAt != wired {
		t.Fatalf("UpsertRemote alive→false should preserve peer-supplied timestamp (want %q, got %q)", wired, got2.LastActivityAt)
	}
}
