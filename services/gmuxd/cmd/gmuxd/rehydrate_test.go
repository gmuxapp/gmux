package main

import (
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/conversations"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/projects"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

// TestRehydrateProjects_PreservesSessionmetaState is the regression
// guard for the bug where the projects.json rehydration loop
// clobbered runtime fields populated by the sessionmeta sweep.
//
// Reproduce: a session with rich runtime state (exit code, status,
// resolved title, exited-at timestamp) is loaded into the store first
// (mimicking sessionmeta.Sweep). projects.json lists the same session
// by slug. After rehydrateProjects runs, all runtime fields must
// remain intact.
func TestRehydrateProjects_PreservesSessionmetaState(t *testing.T) {
	sessions := store.New()

	// Sessionmeta.Sweep equivalent: full runtime state. ShellTitle
	// (set from runtime OSC sequences) is the field a real shell
	// session carries; resolveTitle promotes it to Title on Upsert.
	exitCode := 42
	exitedAt := "2024-01-01T12:00:00Z"
	loaded := store.Session{
		ID:         "tool-abc",
		Slug:       "fix-auth",
		Kind:       "shell",
		Cwd:        "/home/me/proj",
		Alive:      false,
		ExitCode:   &exitCode,
		ExitedAt:   exitedAt,
		Status:     &store.Status{Label: "exited (42)", Error: true},
		ShellTitle: "bash -c echo hi; exit 42",
		CreatedAt:  "2024-01-01T11:00:00Z",
	}
	sessions.Upsert(loaded)

	// convIndex carries the sparse, AdapterTitle-only view that comes
	// from scanning adapter state files. Pre-fix, this would feed
	// into Upsert via projects rehydration, take precedence over the
	// already-loaded ShellTitle (AdapterTitle > ShellTitle), and
	// clobber Title to the generic kind label.
	idx := conversations.New()
	idx.Upsert(conversations.Info{
		ToolID:  "tool-abc",
		Slug:    "fix-auth",
		Kind:    "shell",
		Title:   "shell", // becomes AdapterTitle on rehydrate
		Cwd:     "/home/me/proj",
		Created: time.Date(2024, 1, 1, 11, 0, 0, 0, time.UTC),
	})

	state := &projects.State{
		Items: []projects.Item{{
			Slug:     "proj",
			Sessions: []string{"fix-auth"},
		}},
	}

	rehydrateProjects(sessions, idx, state)

	got, ok := sessions.Get("tool-abc")
	if !ok {
		t.Fatal("session disappeared from store")
	}
	if got.ExitCode == nil || *got.ExitCode != 42 {
		t.Errorf("ExitCode lost: got %v, want 42", got.ExitCode)
	}
	if got.ExitedAt != exitedAt {
		t.Errorf("ExitedAt lost: got %q, want %q", got.ExitedAt, exitedAt)
	}
	if got.Status == nil || got.Status.Label != "exited (42)" {
		t.Errorf("Status lost: got %+v", got.Status)
	}
	if got.Title != "bash -c echo hi; exit 42" {
		t.Errorf("Title clobbered: got %q, want runtime title", got.Title)
	}
}

// TestRehydrateProjects_PreventsDuplicateForToolBackedAdapter is the
// regression guard for the pi/claude/codex case where sessionmeta
// and convIndex use different identifiers for the same logical
// session: sessionmeta keys by the runner's session ID (assigned at
// runner spawn), the conversations index keys by the adapter's own
// UUID (e.g. JSONL filename header). Without the slug-based skip,
// the sweep loads under one ID and the projects rehydration adds a
// parallel ghost entry under the other, surfacing as a duplicate in
// the sidebar.
func TestRehydrateProjects_PreventsDuplicateForToolBackedAdapter(t *testing.T) {
	sessions := store.New()

	// Sessionmeta-loaded entry: keyed by runner session ID.
	sessions.Upsert(store.Session{
		ID:    "sess-runner-A",
		Slug:  "fix-auth",
		Kind:  "pi",
		Cwd:   "/work",
		Alive: false,
	})

	// convIndex entry: same logical session, but ToolID is the JSONL
	// UUID, which is a different string from the runner ID.
	idx := conversations.New()
	idx.Upsert(conversations.Info{
		ToolID:  "jsonl-uuid-XYZ",
		Slug:    "fix-auth",
		Kind:    "pi",
		Title:   "(new)",
		Cwd:     "/work",
		Created: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	})

	state := &projects.State{
		Items: []projects.Item{{
			Slug:     "proj",
			Sessions: []string{"fix-auth"},
		}},
	}

	rehydrateProjects(sessions, idx, state)

	if got := len(sessions.List()); got != 1 {
		t.Fatalf("want 1 session after rehydrate, got %d (duplicate ghost not skipped)", got)
	}
	if _, ok := sessions.Get("sess-runner-A"); !ok {
		t.Error("runner-keyed entry from sessionmeta was lost")
	}
	if _, ok := sessions.Get("jsonl-uuid-XYZ"); ok {
		t.Error("convIndex ghost entry was created despite slug match")
	}
}

// TestRehydrateProjects_FallbackForMissingSessionmeta covers the
// pre-S2 migration path: a session is in projects.json + convIndex
// but has no sessionmeta record. Without rehydration it would not
// appear in the sidebar.
func TestRehydrateProjects_FallbackForMissingSessionmeta(t *testing.T) {
	sessions := store.New()

	idx := conversations.New()
	idx.Upsert(conversations.Info{
		ToolID:        "tool-old",
		Slug:          "legacy",
		Kind:          "shell",
		Title:         "shell",
		Cwd:           "/home/me/legacy",
		Created:       time.Date(2023, 6, 1, 0, 0, 0, 0, time.UTC),
		ResumeCommand: []string{"bash"},
	})

	state := &projects.State{
		Items: []projects.Item{{
			Slug:     "proj",
			Sessions: []string{"legacy"},
		}},
	}

	rehydrateProjects(sessions, idx, state)

	got, ok := sessions.Get("tool-old")
	if !ok {
		t.Fatal("fallback rehydration did not populate store")
	}
	if got.Slug != "legacy" {
		t.Errorf("Slug = %q, want %q", got.Slug, "legacy")
	}
	if got.Cwd != "/home/me/legacy" {
		t.Errorf("Cwd = %q, want /home/me/legacy", got.Cwd)
	}
	if got.Alive {
		t.Error("rehydrated session must land as Alive=false")
	}
	if !got.Resumable {
		t.Error("session with ResumeCommand should be marked Resumable")
	}
}

// TestRehydrateProjects_SkipsUnknownKey covers the case where
// projects.json references a key that resolves nowhere: neither
// sessionmeta nor convIndex knows about it. The function must leave
// the store untouched and not panic.
func TestRehydrateProjects_SkipsUnknownKey(t *testing.T) {
	sessions := store.New()
	idx := conversations.New()

	state := &projects.State{
		Items: []projects.Item{{
			Slug:     "proj",
			Sessions: []string{"orphan-key"},
		}},
	}

	rehydrateProjects(sessions, idx, state)

	if got := sessions.List(); len(got) != 0 {
		t.Errorf("expected empty store, got %d sessions", len(got))
	}
}
