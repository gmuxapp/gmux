package main

import (
	"context"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/conversations"
)

type sourceAdapter struct{ baseOnlyAdapter }

func (sourceAdapter) SnapshotConversations(adapter.ConversationSink) {}
func (sourceAdapter) WatchConversations(ctx context.Context, sink adapter.ConversationSink) error {
	sink.Remove("deleted-ref")
	<-ctx.Done()
	return ctx.Err()
}

func TestProductionConversationDeletionSourceAndSchedule(t *testing.T) {
	withAdapters(t, sourceAdapter{baseOnlyAdapter{name: "source-test"}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deleted := productionConversationDeletionSource(ctx, conversations.New())
	select {
	case <-deleted:
	case <-time.After(time.Second):
		t.Fatal("WatchSources deletion was not adapted")
	}
	ticks := productionEndpointSchedule(ctx, time.Millisecond)
	select {
	case <-ticks:
	case <-time.After(time.Second):
		t.Fatal("periodic endpoint schedule did not tick")
	}
}
