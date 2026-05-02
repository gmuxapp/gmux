package main

import (
	"log"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/conversations"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/projects"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

// rehydrateProjects ensures every key listed in projects.json appears
// in the session store, so the sidebar shows project-tracked sessions
// after a daemon restart.
//
// Identity model: projects.json is the SOT for sidebar membership and
// ordering; sessionmeta is the SOT for per-session runtime fields.
// The two stores have orthogonal concerns. This function honours that
// split: it only fabricates a Session record when neither the
// sessionmeta sweep nor any other path has put a session in the
// store for this slot.
//
// Two skip checks, in priority order:
//
//  1. Same instance ID present (sessions.Get(info.ToolID)). This is
//     the steady-state for shell sessions, where the runner ID and
//     the conversations-index ToolID are the same string.
//
//  2. Same (kind, slug) present (sessions.HasLocalSlug). This is
//     critical for tool-backed adapters (pi, claude, codex) where
//     the conversations index uses the adapter's own UUID (e.g. the
//     JSONL filename) as ToolID, while sessionmeta keys by the
//     runner-generated session ID. Without this check, a single
//     pi/claude session restored from sessionmeta would gain a
//     parallel ghost entry from convIndex, surfacing as a duplicate
//     in the sidebar until slug-takeover at next attribution.
//
// The convIndex fallback exists only for the migration window where
// pre-S2 sessions referenced in projects.json have no sessionmeta
// record yet. The aggregated log surfaces lingering use so that
// decision can be informed.
func rehydrateProjects(sessions *store.Store, convIndex *conversations.Index, state *projects.State) {
	var fallbacks int
	for _, item := range state.Items {
		for _, key := range item.Sessions {
			info, ok := convIndex.LookupBySlug(key)
			if !ok {
				// Either an ephemeral pre-attribution session ID
				// (already loaded by sessionmeta keyed by ID) or a
				// stale entry whose adapter file is gone. The
				// orphan-cleanup pass below removes the latter.
				continue
			}
			if _, exists := sessions.Get(info.ToolID); exists {
				continue
			}
			if sessions.HasLocalSlug(info.Kind, info.Slug) {
				continue
			}
			fallbacks++
			sessions.Upsert(store.Session{
				ID:           info.ToolID,
				CreatedAt:    info.Created.UTC().Format(time.RFC3339),
				Command:      info.ResumeCommand,
				Cwd:          info.Cwd,
				Kind:         info.Kind,
				Alive:        false,
				AdapterTitle: info.Title,
				Slug:         info.Slug,
			})
		}
	}
	if fallbacks > 0 {
		// Aggregated: a 50-session pre-S2 install would otherwise
		// emit 50 lines on every startup. The count is what we use
		// to gauge when this fallback path can be retired.
		log.Printf("rehydrate: %d session(s) restored from convIndex (no sessionmeta record)", fallbacks)
	}
}
