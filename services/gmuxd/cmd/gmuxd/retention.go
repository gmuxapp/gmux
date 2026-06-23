package main

import "github.com/gmuxapp/gmux/services/gmuxd/internal/store"

// reconcileDeletedConversations is the startup counterpart to the
// runtime conversations-index removal callback: it retires dead,
// locally-owned sessions whose backing conversation file an adapter
// confidently reports deleted. It covers the gap the index can't —
// a conversation file removed while gmuxd was down emits no event.
//
// probe answers "is this kind's conversation file at path gone?" as
// (gone, known). Retirement happens only on (known && gone): when the
// adapter can't tell (known=false, e.g. its storage root is unreachable
// because home isn't mounted), the entry is kept. That gate is the
// whole point — it must never retire on undeterminable storage — so it
// lives in one tested place rather than inline at the call site.
//
// Each distinct conversation file is probed once (the file→session
// mapping is N:1); retire is expected to drop every dead session for
// that path. Alive, peer-owned, and file-less sessions are skipped.
func reconcileDeletedConversations(
	list []store.Session,
	probe func(kind, sessionFile string) (gone, known bool),
	retire func(sessionFile string),
) {
	seen := map[string]bool{}
	for _, sess := range list {
		if sess.Alive || sess.Peer != "" || sess.SessionFile == "" || seen[sess.SessionFile] {
			continue
		}
		seen[sess.SessionFile] = true
		if gone, known := probe(sess.Kind, sess.SessionFile); known && gone {
			retire(sess.SessionFile)
		}
	}
}
