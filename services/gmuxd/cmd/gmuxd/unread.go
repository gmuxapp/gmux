package main

import "github.com/gmuxapp/gmux/services/gmuxd/internal/store"

// clearDeadSessionUnread clears the "waiting on you" flag on a dead
// session when a client views it. Live sessions don't need this: the
// runner clears unread itself when a terminal attaches (WS connect →
// SetUnread(false) → meta event), and a daemon-side clear would race
// that authoritative path. Dead sessions have no runner, and under the
// unified turn model (ADR 0023) every one-shot dies unread — the exit
// closes its lifetime turn — so viewing must be able to acknowledge it.
//
// The error flag rides the unread lifecycle, mirroring the runner-path
// rule in subscribe.go's meta handling: acknowledging the output also
// acknowledges the red dot. Working is left untouched — it is the turn
// state at death, which post-mortem waits resolve by.
//
// Status is replaced, not mutated in place: store.Update's copy is
// shallow, so writing through the shared *Status would also rewrite
// the store's previous snapshot and defeat its no-op change detection.
func clearDeadSessionUnread(sessions *store.Store, sessionID string) {
	sess, ok := sessions.Get(sessionID)
	if !ok || sess.Alive || (!sess.Unread && (sess.Status == nil || !sess.Status.Error)) {
		return
	}
	sessions.Update(sessionID, func(s *store.Session) {
		s.Unread = false
		if s.Status != nil && s.Status.Error {
			s.Status = &store.Status{Working: s.Status.Working}
		}
	})
}
