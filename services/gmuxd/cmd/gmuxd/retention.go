package main

import "github.com/gmuxapp/gmux/services/gmuxd/internal/store"

// reconcileDeletedConversations is the startup counterpart to the
// runtime conversations-index removal callback: it retires dead,
// locally-owned sessions whose backing conversation an adapter
// confidently reports deleted. It covers the gap the index can't —
// a conversation removed while gmuxd was down emits no event.
//
// probe answers "is this adapter's conversation at ref gone?" as
// (gone, known). Retirement happens only on (known && gone): when the
// adapter can't tell (known=false, e.g. its storage is unreachable
// because home isn't mounted), the entry is kept. That gate is the
// whole point — it must never retire on undeterminable storage — so it
// lives in one tested place rather than inline at the call site.
//
// Each distinct conversation ref is probed once (the conversation→session
// mapping is N:1); retire is expected to drop every dead session for
// that ref. Alive, peer-owned, and conversation-less sessions are skipped.
func reconcileDeletedConversations(
	list []store.Session,
	probe func(adapter, ref string) (gone, known bool),
	retire func(ref string),
) {
	seen := map[string]bool{}
	for _, sess := range list {
		if sess.Alive || sess.Peer != "" || sess.ConversationRef == "" || seen[sess.ConversationRef] {
			continue
		}
		seen[sess.ConversationRef] = true
		if gone, known := probe(sess.Adapter, sess.ConversationRef); known && gone {
			retire(sess.ConversationRef)
		}
	}
}
