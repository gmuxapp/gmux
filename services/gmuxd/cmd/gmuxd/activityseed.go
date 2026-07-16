package main

import (
	"os"
	"path/filepath"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
	"github.com/gmuxapp/gmux/packages/scrollback"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

// activitySeedFor returns an RFC3339 timestamp to seed a rehydrated
// session's LastOutputAt from the newest durable activity proxy. Two
// sources, newest wins:
//
//   - the adapter-reported conversation freshness (primary): the owning
//     adapter's DescribeConversation(sess.ConversationRef).LastActivity
//     (ADR 0022). For file-backed adapters that is the transcript's
//     mtime — updated on every message — but that is the adapter's
//     detail; the daemon never stats the ref itself, so a DB-backed
//     adapter reseeds identically.
//   - the runner's scrollback tee (generic fallback): every session has
//     one under the per-session dir, written live by the runner and
//     owned by the daemon (ADR 0016), so a direct stat is legitimate.
//     Covers plain shells and agent sessions whose conversation hasn't
//     been reported yet.
//
// Returns "" when neither source yields a usable timestamp.
//
// This recovers "last activity" across a daemon restart. An alive
// session's LastOutputAt is never persisted (only dead sessions land
// in sessionmeta), so on re-register the store would otherwise leave it
// empty and the UI would fall back to created_at ("3 days ago"). The
// store applies this value only as a floor for empty stamps, so it can
// never overwrite a live or persisted timestamp.
//
// Note: the store can't tell a restart-rehydrate from a first-ever
// registration — both arrive with an empty stamp — so a brand-new
// session is seeded too. That is harmless: its activity proxies are
// ~now, which is the same instant the UI would show via the created_at
// fallback anyway, and the first real state transition bumps the stamp
// forward. The one visibly-odd case is resuming an *old* conversation
// when the scrollback tee failed to open: the seed would then be the old
// conversation's LastActivity (days ago) until the first transition
// corrects it. Rare and self-healing, so we accept rather than guard —
// there is no reliable restart-vs-fresh signal at this layer.
func activitySeedFor(sess store.Session, sessionDir func(string) string) string {
	var newest time.Time
	if sess.ConversationRef != "" {
		if desc, ok := adapters.FindByAdapter(sess.Adapter).(adapter.ConversationDescriber); ok {
			if info, err := desc.DescribeConversation(sess.ConversationRef); err == nil && info.LastActivity.After(newest) {
				newest = info.LastActivity
			}
		}
	}
	if sessionDir != nil && sess.ID != "" {
		if fi, err := os.Stat(filepath.Join(sessionDir(sess.ID), scrollback.ActiveName)); err == nil && fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
	}
	if newest.IsZero() {
		return ""
	}
	// Cap at now: a conversation on a skewed clock (or a remote
	// filesystem) must not stamp activity in the future.
	if now := time.Now(); newest.After(now) {
		newest = now
	}
	return newest.UTC().Format(time.RFC3339)
}
