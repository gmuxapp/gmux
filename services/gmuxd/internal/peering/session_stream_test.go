package peering

import (
	"context"
	"encoding/json"
	"strings"
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
	// A rejected transaction is not permanent staleness: the next strictly
	// newer begin starts cleanly and can reach ready.
	p.handleStreamEvent(context.Background(), sessionstream.EventBegin, streamData(t, sessionstream.Begin{Version: 3, Epoch: 2}), &stage)
	p.handleStreamEvent(context.Background(), sessionstream.EventBatch, streamData(t, sessionstream.Batch[SessionProjection]{Epoch: 2, Sessions: []SessionProjection{{ID: "recovered"}}}), &stage)
	p.handleStreamEvent(context.Background(), sessionstream.EventReady, streamData(t, sessionstream.Ready{Epoch: 2}), &stage)
	if got := sink.replaced["spoke"]; len(got) != 1 || got[0].ID != "recovered@spoke" {
		t.Fatalf("recovery=%+v", got)
	}
}

func TestPeerDiagnosticDoesNotInvalidateReady(t *testing.T) {
	sink := newMockSink()
	p := &Peer{Config: config.PeerConfig{Name: "spoke"}, sink: sink}
	stage := peerSessionBootstrap{}
	ctx := context.Background()
	p.handleStreamEvent(ctx, sessionstream.EventBegin, streamData(t, sessionstream.Begin{Version: 3, Epoch: 1}), &stage)
	p.handleStreamEvent(ctx, sessionstream.EventBatch, streamData(t, sessionstream.Batch[SessionProjection]{Epoch: 1, Sessions: []SessionProjection{{ID: "good"}}}), &stage)
	p.handleStreamEvent(ctx, sessionstream.EventError, streamData(t, sessionstream.Error{Epoch: 1, Code: "row_too_large", ID: "bad", Message: "omitted", Count: 1}), &stage)
	if len(stage.diagnostics) != 1 || stage.diagnostics[0].ID != "bad" || stage.diagnostics[0].Count != 1 {
		t.Fatalf("retained diagnostics=%+v", stage.diagnostics)
	}
	p.handleStreamEvent(ctx, sessionstream.EventReady, streamData(t, sessionstream.Ready{Epoch: 1}), &stage)
	if got := sink.replaced["spoke"]; len(got) != 1 || got[0].ID != "good@spoke" {
		t.Fatalf("visible=%+v", got)
	}
}

func TestPeerRejectsNonIncreasingEpochWithoutRollback(t *testing.T) {
	sink := newMockSink()
	p := &Peer{Config: config.PeerConfig{Name: "spoke"}, sink: sink}
	stage := peerSessionBootstrap{}
	ctx := context.Background()
	commit := func(epoch uint64, id string) {
		p.handleStreamEvent(ctx, sessionstream.EventBegin, streamData(t, sessionstream.Begin{Version: 3, Epoch: epoch}), &stage)
		p.handleStreamEvent(ctx, sessionstream.EventBatch, streamData(t, sessionstream.Batch[SessionProjection]{Epoch: epoch, Sessions: []SessionProjection{{ID: id}}}), &stage)
		p.handleStreamEvent(ctx, sessionstream.EventReady, streamData(t, sessionstream.Ready{Epoch: epoch}), &stage)
	}
	commit(1, "old")
	commit(2, "new")
	commit(1, "replayed")
	if got := sink.replaced["spoke"]; len(got) != 1 || got[0].ID != "new@spoke" {
		t.Fatalf("projection rolled back: %+v", got)
	}

	// A stale begin must not destroy a newer in-flight epoch either.
	p.handleStreamEvent(ctx, sessionstream.EventBegin, streamData(t, sessionstream.Begin{Version: 3, Epoch: 3}), &stage)
	p.handleStreamEvent(ctx, sessionstream.EventBegin, streamData(t, sessionstream.Begin{Version: 3, Epoch: 2}), &stage)
	p.handleStreamEvent(ctx, sessionstream.EventBatch, streamData(t, sessionstream.Batch[SessionProjection]{Epoch: 3, Sessions: []SessionProjection{{ID: "newest"}}}), &stage)
	p.handleStreamEvent(ctx, sessionstream.EventReady, streamData(t, sessionstream.Ready{Epoch: 3}), &stage)
	if got := sink.replaced["spoke"]; len(got) != 1 || got[0].ID != "newest@spoke" {
		t.Fatalf("in-flight epoch destroyed: %+v", got)
	}
}

func TestPeerLocksProtocol3AgainstLegacyInjectionAndStaleReplay(t *testing.T) {
	sink := newMockSink()
	p := &Peer{Config: config.PeerConfig{Name: "spoke"}, sink: sink}
	stage := peerSessionBootstrap{}
	ctx := context.Background()
	commit := func(epoch uint64, id string) {
		p.handleStreamEvent(ctx, sessionstream.EventBegin, streamData(t, sessionstream.Begin{Version: 3, Epoch: epoch}), &stage)
		p.handleStreamEvent(ctx, sessionstream.EventBatch, streamData(t, sessionstream.Batch[SessionProjection]{Epoch: epoch, Sessions: []SessionProjection{{ID: id}}}), &stage)
		p.handleStreamEvent(ctx, sessionstream.EventReady, streamData(t, sessionstream.Ready{Epoch: epoch}), &stage)
	}
	commit(1, "old")
	commit(2, "new")
	p.handleStreamEvent(ctx, "snapshot.sessions", streamData(t, sseSnapshotSessions{Sessions: []SessionProjection{{ID: "legacy-current"}}}), &stage)
	commit(1, "replayed-old")
	if got := sink.replaced["spoke"]; len(got) != 1 || got[0].ID != "new@spoke" {
		t.Fatalf("mixed-mode rollback: %+v", got)
	}
}

func TestOldSpokeLargeRowIsQuarantinedWhenNewHubStreamsToBrowser(t *testing.T) {
	sink := newMockSink()
	p := &Peer{Config: config.PeerConfig{Name: "old"}, sink: sink}
	large := SessionProjection{ID: "large", Command: []string{strings.Repeat("x", 60*1024)}}
	stage := peerSessionBootstrap{}
	p.handleStreamEvent(context.Background(), "snapshot.sessions", streamData(t, sseSnapshotSessions{Sessions: []SessionProjection{large}}), &stage)
	mirrored := sink.replaced["old"]
	if len(mirrored) != 1 {
		t.Fatalf("old-spoke projection=%+v", mirrored)
	}
	rows := append([]SessionProjection{{ID: "local"}}, mirrored...)
	events, err := sessionstream.Encode(1, rows, func(row SessionProjection) string { return row.ID })
	if err != nil {
		t.Fatal(err)
	}
	var batches, diagnostics, ready int
	for _, event := range events {
		switch event.Type {
		case sessionstream.EventBatch:
			batches++
		case sessionstream.EventError:
			diagnostics++
		case sessionstream.EventReady:
			ready++
		}
	}
	if batches != 1 || diagnostics != 1 || ready != 1 {
		t.Fatalf("events=%v", eventTypesForPeerTest(events))
	}
}

func eventTypesForPeerTest(events []sessionstream.Event) []string {
	out := make([]string, len(events))
	for i := range events {
		out[i] = events[i].Type
	}
	return out
}

func TestPeerAcceptsLegacySnapshotFromOldSpoke(t *testing.T) {
	sink := newMockSink()
	p := &Peer{Config: config.PeerConfig{Name: "old"}, sink: sink}
	stage := peerSessionBootstrap{active: true, epoch: 9, rows: []SessionProjection{{ID: "partial"}}}
	p.handleStreamEvent(context.Background(), "snapshot.sessions", streamData(t, sseSnapshotSessions{Sessions: []SessionProjection{{ID: "legacy"}}}), &stage)
	if stage.active || stage.mode != sessionStreamLegacy {
		t.Fatalf("legacy replacement did not lock legacy mode: %+v", stage)
	}
	// A protocol-3 begin on the same legacy connection is ignored.
	p.handleStreamEvent(context.Background(), sessionstream.EventBegin, streamData(t, sessionstream.Begin{Version: 3, Epoch: 1}), &stage)
	p.handleStreamEvent(context.Background(), sessionstream.EventBatch, streamData(t, sessionstream.Batch[SessionProjection]{Epoch: 1, Sessions: []SessionProjection{{ID: "replayed"}}}), &stage)
	p.handleStreamEvent(context.Background(), sessionstream.EventReady, streamData(t, sessionstream.Ready{Epoch: 1}), &stage)
	got := sink.replaced["old"]
	if len(got) != 1 || got[0].ID != "legacy@old" {
		t.Fatalf("visible=%+v", got)
	}
}
