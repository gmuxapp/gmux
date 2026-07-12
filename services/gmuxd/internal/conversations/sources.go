package conversations

import (
	"context"
	"errors"
	"log"

	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
)

// indexSink bridges one adapter's ConversationSource to the index.
// onRemoved, if set, additionally observes every conversation-gone event —
// including refs the index never held (describe failure, CanResume=false,
// empty cwd), which is why retirement hangs here and not off
// Index.RemoveByRef: a dead session bound to an unindexed conversation
// must still retire when that conversation is deleted.
type indexSink struct {
	idx       *Index
	a         adapter.Adapter
	onRemoved func(adapterName, ref string)
}

func (s indexSink) Upsert(ref string) { s.idx.Scan(s.a, ref) }

// Remove re-scopes the source's bare ref with the owning adapter before it
// touches anything: refs are only unique within an adapter (ADR 0022), so
// both the index removal and the retirement callback carry (adapter, ref).
func (s indexSink) Remove(ref string) {
	s.idx.RemoveByRef(s.a.Name(), ref)
	if s.onRemoved != nil {
		s.onRemoved(s.a.Name(), ref)
	}
}

// Snapshot populates the index from every adapter ConversationSource.
// Synchronous; call once at startup before consumers read the index.
func (idx *Index) Snapshot() {
	for _, a := range adapters.AllAdapters() {
		if src, ok := a.(adapter.ConversationSource); ok {
			src.SnapshotConversations(indexSink{idx: idx, a: a})
		}
	}
}

// WatchSources starts every adapter ConversationSource in its own goroutine,
// feeding the index until ctx is cancelled. onRemoved, if non-nil, is
// invoked (from the source goroutines) with each (adapter, ref) observed
// to disappear — whether or not it was indexed. cmd/gmuxd wires this to
// retire dead sessions backed by the deleted conversation.
func (idx *Index) WatchSources(ctx context.Context, onRemoved func(adapterName, ref string)) {
	for _, a := range adapters.AllAdapters() {
		src, ok := a.(adapter.ConversationSource)
		if !ok {
			continue
		}
		go func(a adapter.Adapter, src adapter.ConversationSource) {
			if err := src.WatchConversations(ctx, indexSink{idx: idx, a: a, onRemoved: onRemoved}); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("conversations: source %s stopped: %v", a.Name(), err)
			}
		}(a, src)
	}
}
