package main

// Concrete production trigger sources for the inert S5 bootstrap graph.
// Construction starts only the explicitly requested source; serve does not
// select any of these adapters before the authority switch.

import (
	"context"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/conversations"
)

// productionConversationDeletionSource adapts WatchSources callbacks to a
// bounded level-triggered channel. Reconcile operates over the complete store,
// so adapter/ref payloads are intentionally coalesced without information loss.
func productionConversationDeletionSource(ctx context.Context, idx *conversations.Index) <-chan struct{} {
	out := make(chan struct{}, 1)
	if idx == nil {
		close(out)
		return out
	}
	idx.WatchSources(ctx, func(string, string) {
		select {
		case out <- struct{}{}:
		default:
		}
	})
	return out
}

// productionEndpointSchedule is the periodic endpoint enumeration trigger.
// The ticker is stopped with ctx and its output is bounded by time.Ticker.
func productionEndpointSchedule(ctx context.Context, interval time.Duration) <-chan time.Time {
	out := make(chan time.Time, 1)
	if interval <= 0 {
		close(out)
		return out
	}
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case at := <-ticker.C:
				select {
				case out <- at:
				default:
				}
			}
		}
	}()
	return out
}
