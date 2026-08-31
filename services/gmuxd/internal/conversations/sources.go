package conversations

import (
	"context"
	"errors"
	"log"
	"sync"

	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
)

// allAdapters is the adapter enumeration seam; overridden in tests to
// exercise snapshot scheduling with synthetic sources.
var allAdapters = adapters.AllAdapters

// indexSink bridges one adapter's ConversationSource to the index.
// onRemoved, if set, additionally observes every conversation-gone event —
// including refs the index never held (describe failure, non-resumable,
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
	for _, a := range allAdapters() {
		if src, ok := a.(adapter.ConversationSource); ok {
			src.SnapshotConversations(indexSink{idx: idx, a: a})
		}
	}
	idx.markSnapshotComplete()
}

// DefaultSnapshotWorkers bounds how many adapter snapshot scans run
// concurrently in StartSnapshot. Each scan is one adapter's full-history
// walk+parse (file I/O heavy); two in flight overlaps the large corpora
// without saturating the disk during daemon startup.
const DefaultSnapshotWorkers = 2

// StartSnapshot populates the index from every adapter ConversationSource in
// the background, with at most `workers` adapter scans in flight, and returns
// immediately. Conversations become visible progressively as each ref is
// scanned; onAdapterIndexed (if non-nil) runs after each adapter's scan
// completes, and onComplete (if non-nil) runs once after all adapters finish
// — at which point SnapshotComplete reports true. Callbacks run on snapshot
// goroutines and must be cheap and concurrency-safe.
//
// StartSnapshot is safe to run while WatchSources is feeding the same index:
// Index mutations are mutex-serialized and Upsert is keyed by adapter-native
// conversation ID, so a ref observed by both the scan and a watcher resolves
// to one entry (last describe wins — both describe the same file). Cancelling
// ctx before an adapter's scan starts skips it and leaves the snapshot
// incomplete; an in-progress scan is not interruptible (the adapter API has
// no context) but the daemon is exiting anyway.
func (idx *Index) StartSnapshot(ctx context.Context, workers int, onAdapterIndexed func(adapterName string), onComplete func()) {
	if workers < 1 {
		workers = 1
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for _, a := range allAdapters() {
		src, ok := a.(adapter.ConversationSource)
		if !ok {
			continue
		}
		wg.Add(1)
		go func(a adapter.Adapter, src adapter.ConversationSource) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			src.SnapshotConversations(indexSink{idx: idx, a: a})
			if onAdapterIndexed != nil {
				onAdapterIndexed(a.Name())
			}
		}(a, src)
	}
	go func() {
		wg.Wait()
		if ctx.Err() != nil {
			// Shutdown raced the scan: at least one adapter may have been
			// skipped, so the index must not advertise completeness.
			return
		}
		idx.markSnapshotComplete()
		if onComplete != nil {
			onComplete()
		}
	}()
}

// WatchSources starts every adapter ConversationSource in its own goroutine,
// feeding the index until ctx is cancelled. onRemoved, if non-nil, is
// invoked (from the source goroutines) with each (adapter, ref) observed
// to disappear — whether or not it was indexed. cmd/gmuxd wires this to
// retire dead sessions backed by the deleted conversation.
func (idx *Index) WatchSources(ctx context.Context, onRemoved func(adapterName, ref string)) {
	for _, a := range allAdapters() {
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
