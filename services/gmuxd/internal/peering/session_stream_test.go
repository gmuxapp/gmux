package peering

import (
	"context"
	"encoding/json"
	"fmt"
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

// countWorldEvents returns how many PeerWorldChanged notifications the mock
// sink captured for a peer name.
func countWorldEvents(sink *mockSink, name string) int {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	n := 0
	for _, ev := range sink.worldEvents {
		if ev == name {
			n++
		}
	}
	return n
}

// TestPeerOmissionAccountingIsExactAndReachesWorldProjection reproduces the
// aggregate-overflow shape end to end on the peer seam: 256 individual
// diagnostics plus a counted diagnostics_suppressed summary (the sender's
// 257th error event) must commit alongside the surviving rows and surface as
// an exact per-peer omission count in both world projections, instead of
// dying in a log line.
func TestPeerOmissionAccountingIsExactAndReachesWorldProjection(t *testing.T) {
	sink := newMockSink()
	mgr := NewProjectionManager([]config.PeerConfig{{Name: "spoke", URL: "http://spoke"}}, "test-host", sink, EventHooks{})
	p := mgr.GetPeer("spoke")
	stage := peerSessionBootstrap{}
	ctx := context.Background()

	p.handleStreamEvent(ctx, sessionstream.EventBegin, streamData(t, sessionstream.Begin{Version: 3, Epoch: 1}), &stage)
	p.handleStreamEvent(ctx, sessionstream.EventBatch, streamData(t, sessionstream.Batch[SessionProjection]{Epoch: 1, Sessions: []SessionProjection{{ID: "survivor"}}}), &stage)
	for i := 0; i < 256; i++ {
		p.handleStreamEvent(ctx, sessionstream.EventError, streamData(t, sessionstream.Error{Epoch: 1, Code: "transaction_limit", ID: "gone", Message: "omitted", Count: 1}), &stage)
	}
	p.handleStreamEvent(ctx, sessionstream.EventError, streamData(t, sessionstream.Error{Epoch: 1, Code: "diagnostics_suppressed", Message: "6031 additional omitted session rows were not individually reported", Count: 6031}), &stage)

	// Nothing published before ready: no partial projection, no omission marker.
	if total, _ := p.SessionOmissions(); total != 0 {
		t.Fatalf("omissions visible before ready: %d", total)
	}
	p.handleStreamEvent(ctx, sessionstream.EventReady, streamData(t, sessionstream.Ready{Epoch: 1}), &stage)

	if got := sink.replaced["spoke"]; len(got) != 1 || got[0].ID != "survivor@spoke" {
		t.Fatalf("survivors=%+v", got)
	}
	total, codes := p.SessionOmissions()
	if total != 256+6031 {
		t.Fatalf("total=%d, want exact 6287", total)
	}
	if codes["transaction_limit"] != 256 || codes["diagnostics_suppressed"] != 6031 {
		t.Fatalf("codes=%v", codes)
	}
	for _, info := range mgr.WorldProjection().Peers {
		if info.Name == "spoke" && (info.SessionsOmitted != 6287 || info.SessionsOmittedCodes["transaction_limit"] != 256) {
			t.Fatalf("world projection=%+v", info)
		}
	}
	for _, info := range mgr.PeerStatus() {
		if info.Name == "spoke" && info.SessionsOmitted != 6287 {
			t.Fatalf("peer status=%+v", info)
		}
	}
	if countWorldEvents(sink, "spoke") != 1 {
		t.Fatalf("worldEvents=%v, want exactly one change notification", sink.worldEvents)
	}

	// A clean later transaction clears the incompleteness marker and notifies
	// the world exactly once more; a second identical clean ready is silent.
	commitClean := func(epoch uint64) {
		p.handleStreamEvent(ctx, sessionstream.EventBegin, streamData(t, sessionstream.Begin{Version: 3, Epoch: epoch}), &stage)
		p.handleStreamEvent(ctx, sessionstream.EventBatch, streamData(t, sessionstream.Batch[SessionProjection]{Epoch: epoch, Sessions: []SessionProjection{{ID: "survivor"}}}), &stage)
		p.handleStreamEvent(ctx, sessionstream.EventReady, streamData(t, sessionstream.Ready{Epoch: epoch}), &stage)
	}
	commitClean(2)
	if total, codes := p.SessionOmissions(); total != 0 || codes != nil {
		t.Fatalf("omissions not cleared: %d %v", total, codes)
	}
	after := countWorldEvents(sink, "spoke")
	commitClean(3)
	if countWorldEvents(sink, "spoke") != after {
		t.Fatalf("unchanged clean ready fired a world notification")
	}
}

// TestPeerOmissionCountsAreBoundedAgainstHostileSenders clamps count and code
// abuse from a remote sender: negative/zero counts count as one row, huge and
// repeated counts saturate at the transaction row bound, code cardinality
// folds into "other", oversized code strings are truncated, and the per-code
// breakdown always sums exactly to the published total.
func TestPeerOmissionCountsAreBoundedAgainstHostileSenders(t *testing.T) {
	sink := newMockSink()
	p := &Peer{Config: config.PeerConfig{Name: "spoke"}, sink: sink}
	stage := peerSessionBootstrap{}
	ctx := context.Background()
	longCode := strings.Repeat("c", 500)
	p.handleStreamEvent(ctx, sessionstream.EventBegin, streamData(t, sessionstream.Begin{Version: 3, Epoch: 1}), &stage)
	p.handleStreamEvent(ctx, sessionstream.EventError, streamData(t, sessionstream.Error{Epoch: 1, Code: "neg", Count: -5}), &stage)
	p.handleStreamEvent(ctx, sessionstream.EventError, streamData(t, sessionstream.Error{Epoch: 1, Code: longCode, Count: 1}), &stage)
	for i := 0; i < 20; i++ {
		p.handleStreamEvent(ctx, sessionstream.EventError, streamData(t, sessionstream.Error{Epoch: 1, Code: fmt.Sprintf("code-%d", i), Count: 1}), &stage)
	}
	// A hostile sender streaming huge counts indefinitely: every event past
	// saturation must be a no-op for total AND codes alike.
	for i := 0; i < 3; i++ {
		p.handleStreamEvent(ctx, sessionstream.EventError, streamData(t, sessionstream.Error{Epoch: 1, Code: longCode, Count: 1 << 60}), &stage)
	}
	p.handleStreamEvent(ctx, sessionstream.EventReady, streamData(t, sessionstream.Ready{Epoch: 1}), &stage)

	total, codes := p.SessionOmissions()
	if total != sessionstream.MaxStagedRows {
		t.Fatalf("total=%d, want saturation at MaxStagedRows", total)
	}
	if codes["neg"] != 1 {
		t.Fatalf("negative count not clamped to 1: %v", codes)
	}
	if len(codes) > maxPeerOmissionCodes+1 { // bounded set + "other" bucket
		t.Fatalf("code cardinality unbounded: %v", codes)
	}
	if codes["other"] == 0 {
		t.Fatalf("overflow codes not folded into other: %v", codes)
	}
	sum := 0
	for code, count := range codes {
		if len(code) > maxPeerOmissionCodeLen {
			t.Fatalf("code %q exceeds length bound", code)
		}
		if count < 0 || count > sessionstream.MaxStagedRows {
			t.Fatalf("code %q count %d outside [0, MaxStagedRows]", code, count)
		}
		sum += count
	}
	if sum != total {
		t.Fatalf("per-code sum %d != total %d (breakdown must be internally consistent)", sum, total)
	}
	// The oversized code string was truncated into a bounded named entry, and
	// the repeated huge-count events for the same code received exactly the
	// remaining headroom once — not 3× an unclamped accumulation.
	if got := codes[longCode[:maxPeerOmissionCodeLen]]; got != sessionstream.MaxStagedRows-21 {
		t.Fatalf("saturating truncated-code count=%d, want %d", got, sessionstream.MaxStagedRows-21)
	}
	// Codes past the cardinality cap folded into "other" (code-6..19).
	if codes["other"] != 14 {
		t.Fatalf("other bucket=%d, want 14: %v", codes["other"], codes)
	}
}

// TestPeerOmissionChangeFiresPeerWorldDirtyHook wires the manager exactly like
// the production central path (sink == nil, EventHooks only) and commits a
// lossy transaction whose survivor set is identical to the previous epoch, so
// the ReplacePeerSessions no-op suppression cannot recompose the world. Both
// the SET and the CLEAR of the omission marker must fire PeerWorldDirty, or
// browsers on a quiet hub never see the marker change.
func TestPeerOmissionChangeFiresPeerWorldDirtyHook(t *testing.T) {
	worldDirty := 0
	mgr := NewProjectionManager([]config.PeerConfig{{Name: "spoke", URL: "http://spoke"}}, "test-host",
		nil, EventHooks{PeerWorldDirty: func() { worldDirty++ }})
	p := mgr.GetPeer("spoke")
	stage := peerSessionBootstrap{}
	ctx := context.Background()
	commit := func(epoch uint64, lossy bool) {
		p.handleStreamEvent(ctx, sessionstream.EventBegin, streamData(t, sessionstream.Begin{Version: 3, Epoch: epoch}), &stage)
		p.handleStreamEvent(ctx, sessionstream.EventBatch, streamData(t, sessionstream.Batch[SessionProjection]{Epoch: epoch, Sessions: []SessionProjection{{ID: "survivor"}}}), &stage)
		if lossy {
			p.handleStreamEvent(ctx, sessionstream.EventError, streamData(t, sessionstream.Error{Epoch: epoch, Code: "row_too_large", ID: "big", Count: 1}), &stage)
		}
		p.handleStreamEvent(ctx, sessionstream.EventReady, streamData(t, sessionstream.Ready{Epoch: epoch}), &stage)
	}

	commit(1, false) // baseline projection, complete
	base := worldDirty
	commit(2, true) // identical survivors, marker SET
	if worldDirty != base+1 {
		t.Fatalf("omission marker SET fired PeerWorldDirty %d times, want 1 (production hooks path)", worldDirty-base)
	}
	for _, info := range mgr.WorldProjection().Peers {
		if info.Name == "spoke" && info.SessionsOmitted != 1 {
			t.Fatalf("marker not visible in world projection: %+v", info)
		}
	}
	commit(3, true) // unchanged marker: silent, no redundant recompose
	if worldDirty != base+1 {
		t.Fatalf("unchanged marker fired PeerWorldDirty (%d extra)", worldDirty-base-1)
	}
	commit(4, false) // identical survivors, marker CLEAR
	if worldDirty != base+2 {
		t.Fatalf("omission marker CLEAR fired PeerWorldDirty %d times, want 1 (production hooks path)", worldDirty-base-1)
	}
	if total, _ := p.SessionOmissions(); total != 0 {
		t.Fatalf("marker not cleared: %d", total)
	}
}

// TestPeerOmissionMarkerLifecycle: an abandoned transaction leaks nothing, a
// legacy snapshot (complete by definition) clears a stale marker.
func TestPeerOmissionMarkerLifecycle(t *testing.T) {
	sink := newMockSink()
	p := &Peer{Config: config.PeerConfig{Name: "spoke"}, sink: sink}
	stage := peerSessionBootstrap{}
	ctx := context.Background()

	// Diagnostics inside a transaction that never reaches ready must not
	// surface: the committed projection they described never became visible.
	p.handleStreamEvent(ctx, sessionstream.EventBegin, streamData(t, sessionstream.Begin{Version: 3, Epoch: 1}), &stage)
	p.handleStreamEvent(ctx, sessionstream.EventError, streamData(t, sessionstream.Error{Epoch: 1, Code: "row_too_large", ID: "x", Count: 1}), &stage)
	stage = peerSessionBootstrap{lastEpoch: stage.lastEpoch} // disconnect drops staging
	if total, _ := p.SessionOmissions(); total != 0 {
		t.Fatalf("abandoned transaction leaked omissions: %d", total)
	}

	// A committed marker survives until replaced…
	p.handleStreamEvent(ctx, sessionstream.EventBegin, streamData(t, sessionstream.Begin{Version: 3, Epoch: 2}), &stage)
	p.handleStreamEvent(ctx, sessionstream.EventError, streamData(t, sessionstream.Error{Epoch: 2, Code: "row_too_large", ID: "x", Count: 1}), &stage)
	p.handleStreamEvent(ctx, sessionstream.EventReady, streamData(t, sessionstream.Ready{Epoch: 2}), &stage)
	if total, _ := p.SessionOmissions(); total != 1 {
		t.Fatalf("marker not committed: %d", total)
	}

	// …and a legacy single-frame snapshot on a fresh connection clears it.
	stage = peerSessionBootstrap{}
	p.handleStreamEvent(ctx, "snapshot.sessions", streamData(t, sseSnapshotSessions{Sessions: []SessionProjection{{ID: "legacy"}}}), &stage)
	if total, codes := p.SessionOmissions(); total != 0 || codes != nil {
		t.Fatalf("legacy snapshot did not clear marker: %d %v", total, codes)
	}
}
