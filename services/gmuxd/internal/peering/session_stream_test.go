package peering

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessionstream"
)

func streamData(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestPeerSessionBootstrapIsAtomicAndDisconnectLocal(t *testing.T) {
	sink := newMockSink()
	p := &Peer{Config: config.PeerConfig{Name: "spoke"}, sink: sink}
	ctx := context.Background()
	stage := peerSessionBootstrap{}

	p.handleStreamEvent(ctx, sessionstream.EventBegin, streamData(t, sessionstream.Begin{Version: 3, Epoch: 1}), &stage)
	p.handleStreamEvent(ctx, sessionstream.EventBatch, streamData(t, sessionstream.Batch[SessionProjection]{Epoch: 1, Sessions: []SessionProjection{{ID: "partial"}}}), &stage)
	if _, visible := sink.replaced["spoke"]; visible {
		t.Fatal("partial bootstrap became visible before ready")
	}

	// A disconnect drops the connection-local staging value. The next
	// connection starts from zero and cannot complete epoch 1.
	stage = peerSessionBootstrap{}
	p.handleStreamEvent(ctx, sessionstream.EventReady, streamData(t, sessionstream.Ready{Epoch: 1}), &stage)
	if _, visible := sink.replaced["spoke"]; visible {
		t.Fatal("interrupted bootstrap became visible")
	}

	p.handleStreamEvent(ctx, sessionstream.EventBegin, streamData(t, sessionstream.Begin{Version: 3, Epoch: 1}), &stage)
	p.handleStreamEvent(ctx, sessionstream.EventBatch, streamData(t, sessionstream.Batch[SessionProjection]{Epoch: 1, Sessions: []SessionProjection{{ID: "fresh"}}}), &stage)
	p.handleStreamEvent(ctx, sessionstream.EventReady, streamData(t, sessionstream.Ready{Epoch: 1}), &stage)
	got := sink.replaced["spoke"]
	if len(got) != 1 || got[0].ID != "fresh@spoke" {
		t.Fatalf("visible=%+v, want fresh replacement", got)
	}
}

func TestPeerMutationEpochAppliesAfterPriorReadyExactlyOnce(t *testing.T) {
	sink := newMockSink()
	p := &Peer{Config: config.PeerConfig{Name: "spoke"}, sink: sink}
	stage := peerSessionBootstrap{}
	ctx := context.Background()
	emit := func(epoch uint64, title string) {
		p.handleStreamEvent(ctx, sessionstream.EventBegin, streamData(t, sessionstream.Begin{Version: 3, Epoch: epoch}), &stage)
		p.handleStreamEvent(ctx, sessionstream.EventBatch, streamData(t, sessionstream.Batch[SessionProjection]{Epoch: epoch, Sessions: []SessionProjection{{ID: "s", Title: title}}}), &stage)
	}
	emit(1, "before")
	if len(sink.replaced) != 0 {
		t.Fatal("rows visible before first ready")
	}
	p.handleStreamEvent(ctx, sessionstream.EventReady, streamData(t, sessionstream.Ready{Epoch: 1}), &stage)
	if got := sink.replaced["spoke"]; len(got) != 1 || got[0].Title != "before" {
		t.Fatalf("first visible=%+v", got)
	}

	// A mutation captured by the fanout while epoch 1 was being written is
	// queued as epoch 2. It cannot overtake epoch 1's ready and stays staged
	// until its own ready.
	emit(2, "after")
	if got := sink.replaced["spoke"]; got[0].Title != "before" {
		t.Fatalf("mutation exposed before ready: %+v", got)
	}
	p.handleStreamEvent(ctx, sessionstream.EventReady, streamData(t, sessionstream.Ready{Epoch: 2}), &stage)
	got := sink.replaced["spoke"]
	if len(got) != 1 || got[0].Title != "after" {
		t.Fatalf("visible=%+v", got)
	}
}

func TestPeerRejectsBootstrapBeyondAggregateMemoryBound(t *testing.T) {
	sink := newMockSink()
	p := &Peer{Config: config.PeerConfig{Name: "spoke"}, sink: sink}
	stage := peerSessionBootstrap{active: true, epoch: 1, bytes: sessionstream.MaxStagedBytes}
	p.handleStreamEvent(context.Background(), sessionstream.EventBatch,
		streamData(t, sessionstream.Batch[SessionProjection]{Epoch: 1, Sessions: []SessionProjection{{ID: "overflow"}}}), &stage)
	if stage.active || len(stage.rows) != 0 {
		t.Fatalf("staging not discarded: %+v", stage)
	}
	if len(sink.replaced) != 0 {
		t.Fatal("overflow became visible")
	}
}

func TestPeerAcceptsLegacySnapshotFromOldSpoke(t *testing.T) {
	sink := newMockSink()
	p := &Peer{Config: config.PeerConfig{Name: "old"}, sink: sink}
	stage := peerSessionBootstrap{active: true, epoch: 9, rows: []SessionProjection{{ID: "partial"}}}
	p.handleStreamEvent(context.Background(), "snapshot.sessions", streamData(t, sseSnapshotSessions{Sessions: []SessionProjection{{ID: "legacy"}}}), &stage)
	if stage.active {
		t.Fatal("legacy replacement did not discard partial protocol-3 staging")
	}
	got := sink.replaced["old"]
	if len(got) != 1 || got[0].ID != "legacy@old" {
		t.Fatalf("visible=%+v", got)
	}
}
