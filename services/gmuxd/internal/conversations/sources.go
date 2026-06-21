package conversations

import (
	"context"
	"errors"
	"log"

	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
)

// indexSink bridges one adapter's ConversationSource to the index.
type indexSink struct {
	idx *Index
	a   adapter.Adapter
}

func (s indexSink) Upsert(path string) { s.idx.ScanFile(s.a, path) }
func (s indexSink) Remove(path string) { s.idx.RemoveByPath(path) }

// Snapshot populates the index from every adapter ConversationSource.
// Synchronous; call once at startup before consumers read the index.
func (idx *Index) Snapshot() {
	for _, a := range adapters.AllAdapters() {
		if src, ok := a.(adapter.ConversationSource); ok {
			src.SnapshotConversations(indexSink{idx, a})
		}
	}
}

// WatchSources starts every adapter ConversationSource in its own goroutine,
// feeding the index until ctx is cancelled.
func (idx *Index) WatchSources(ctx context.Context) {
	for _, a := range adapters.AllAdapters() {
		src, ok := a.(adapter.ConversationSource)
		if !ok {
			continue
		}
		go func(a adapter.Adapter, src adapter.ConversationSource) {
			if err := src.WatchConversations(ctx, indexSink{idx, a}); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("conversations: source %s stopped: %v", a.Name(), err)
			}
		}(a, src)
	}
}
