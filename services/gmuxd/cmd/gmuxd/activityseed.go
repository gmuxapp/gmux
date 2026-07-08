package main

import (
	"os"
	"path/filepath"
	"time"

	"github.com/gmuxapp/gmux/packages/scrollback"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

// activitySeedFor returns an RFC3339 timestamp to seed a rehydrated
// session's LastActivityAt from the newest durable on-disk activity
// proxy. Two sources, newest wins:
//
//   - the adapter's conversation file (primary): appended on every
//     message, so its mtime is an accurate, restart-surviving proxy for
//     when the session last did something. The path is adapter-supplied
//     (session.ConversationFile, reported by the agent hook per ADR
//     0011); the daemon only stats it, so no adapter-specific reseed
//     logic is needed.
//   - the runner's scrollback tee (generic fallback): every session has
//     one under the per-session dir, written live by the runner. Covers
//     plain shells and agent sessions whose conversation file hasn't
//     appeared yet.
//
// Returns "" when neither file yields a usable mtime.
//
// This recovers "last activity" across a daemon restart. An alive
// session's LastActivityAt is never persisted (only dead sessions land
// in sessionmeta), so on re-register the store would otherwise leave it
// empty and the UI would fall back to created_at ("3 days ago"). The
// store applies this value only as a floor for empty stamps, so it can
// never overwrite a live or persisted timestamp.
func activitySeedFor(sess store.Session, sessionDir func(string) string) string {
	var newest time.Time
	consider := func(path string) {
		if path == "" {
			return
		}
		info, err := os.Stat(path)
		if err != nil {
			return
		}
		if mt := info.ModTime(); mt.After(newest) {
			newest = mt
		}
	}
	consider(sess.ConversationFile)
	if sessionDir != nil && sess.ID != "" {
		consider(filepath.Join(sessionDir(sess.ID), scrollback.ActiveName))
	}
	if newest.IsZero() {
		return ""
	}
	// Cap at now: a conversation file with a skewed clock (or one on a
	// remote filesystem) must not stamp activity in the future.
	if now := time.Now(); newest.After(now) {
		newest = now
	}
	return newest.UTC().Format(time.RFC3339)
}
