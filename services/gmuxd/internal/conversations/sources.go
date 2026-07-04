package conversations

import (
	"context"
	"errors"
	"log"

	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
)

// indexSink bridges one adapter's ConversationSource to the index.
// onRemoved, if set, additionally observes every file-gone event —
// including files the index never held (parse failure, CanResume=false,
// empty cwd), which is why retirement hangs here and not off
// Index.RemoveByPath: a dead session bound to an unindexed conversation
// must still retire when that file is deleted.
type indexSink struct {
	idx       *Index
	a         adapter.Adapter
	onRemoved func(path string)
}

func (s indexSink) Upsert(path string) { s.idx.ScanFile(s.a, path) }
func (s indexSink) Remove(path string) {
	s.idx.RemoveByPath(path)
	if s.onRemoved != nil {
		s.onRemoved(path)
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
// feeding the index until ctx is cancelled. onFileRemoved, if non-nil,
// is invoked (from the watcher goroutines) with each conversation file
// path observed to disappear — whether or not it was indexed. cmd/gmuxd
// wires this to retire dead sessions backed by the deleted file.
func (idx *Index) WatchSources(ctx context.Context, onFileRemoved func(path string)) {
	for _, a := range adapters.AllAdapters() {
		src, ok := a.(adapter.ConversationSource)
		if !ok {
			continue
		}
		go func(a adapter.Adapter, src adapter.ConversationSource) {
			if err := src.WatchConversations(ctx, indexSink{idx: idx, a: a, onRemoved: onFileRemoved}); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("conversations: source %s stopped: %v", a.Name(), err)
			}
		}(a, src)
	}
}
