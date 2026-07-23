package sessioncoord

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

func recvOutcome(t *testing.T, ch <-chan Outcome) Outcome {
	t.Helper()
	select {
	case o, ok := <-ch:
		if !ok {
			t.Fatal("outcome channel closed")
		}
		return o
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for outcome")
	}
	panic("unreachable")
}

// TestOutcomeBusRegistrationUpserted verifies a live registration publishes
// one Upserted outcome carrying the committed row and registry-stamped
// liveness.
func TestOutcomeBusRegistrationUpserted(t *testing.T) {
	id := sid(220)
	meta := RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Alive: true}}
	client := newFakeClient(meta)
	dur := newFakeDurable(0)
	coord := newCoord(client, dur, &fakeDirtySink{}, nil)

	ch, cancel := coord.SubscribeOutcomes()
	defer cancel()

	runtime, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep220"})
	if err != nil {
		t.Fatal(err)
	}
	o := recvOutcome(t, ch)
	if o.Type != OutcomeUpserted || o.ID != id {
		t.Fatalf("outcome=%+v", o)
	}
	if o.Session == nil || o.Session.ID != id {
		t.Fatalf("expected committed row on Upserted, got %+v", o.Session)
	}
	if !o.Alive || o.Generation != runtime.Generation {
		t.Fatalf("liveness stamp: alive=%v gen=%d want gen=%d", o.Alive, o.Generation, runtime.Generation)
	}
}

// TestOutcomeBusExitStampsDead verifies an exit-carrying event publishes an
// Upserted outcome with Alive=false and Generation 0 (the entry left the
// registry before publish).
func TestOutcomeBusExitStampsDead(t *testing.T) {
	id := sid(221)
	meta := RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Alive: true}}
	client := newFakeClient(meta)
	dur := newFakeDurable(0)
	at := centralstore.UnixMillis(100)
	dur.session = func(centralstore.SessionID) (centralstore.Session, bool, error) {
		return centralstore.Session{ID: id, Version: 2, ExitedAt: &at}, true, nil
	}
	coord := newCoord(client, dur, &fakeDirtySink{}, nil)
	if _, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep221"}); err != nil {
		t.Fatal(err)
	}

	ch, cancel := coord.SubscribeOutcomes()
	defer cancel()

	client.stream.send(RunnerEvent{ObservedAt: at, Alive: aliveFalse, Facts: centralstore.RunnerFacts{ExitedAt: exitedAt(100)}})

	o := recvOutcome(t, ch)
	if o.Type != OutcomeUpserted || o.ID != id || o.Alive || o.Generation != 0 {
		t.Fatalf("outcome=%+v", o)
	}
	if o.Session == nil || o.Session.ExitedAt == nil {
		t.Fatalf("expected exited row, got %+v", o.Session)
	}
}

// TestOutcomeBusRemove verifies Remove publishes a Removed outcome (post-
// commit read finds no row).
func TestOutcomeBusRemove(t *testing.T) {
	dur := newFakeDurable(0)
	coord := New(nil, newFakeClient(RunnerMeta{}), dur, &fakeDirtySink{}, nil)

	ch, cancel := coord.SubscribeOutcomes()
	defer cancel()

	if err := coord.Remove(context.Background(), "sess-r", 1); err != nil {
		t.Fatal(err)
	}
	o := recvOutcome(t, ch)
	if o.Type != OutcomeRemoved || o.ID != "sess-r" || o.Session != nil || o.Alive {
		t.Fatalf("outcome=%+v", o)
	}
}

// TestOutcomeBusDismissUpsertsRetainedRows verifies dismissal publishes
// Upserted outcomes for the retained (hidden) rows.
func TestOutcomeBusDismissUpsertsRetainedRows(t *testing.T) {
	dur := newFakeDurable(0)
	dur.listSessions = func() ([]centralstore.Session, error) { return treeSessions(), nil }
	dismissedAt := centralstore.UnixMillis(777)
	dur.session = func(id centralstore.SessionID) (centralstore.Session, bool, error) {
		return centralstore.Session{ID: id, Version: 2, DismissedAt: &dismissedAt}, true, nil
	}
	dur.dismissResult = func(root centralstore.SessionID, at centralstore.UnixMillis) ([]centralstore.SessionID, centralstore.MutationResult, error) {
		return []centralstore.SessionID{"sess-p", "sess-c"}, centralstore.MutationResult{Changed: true, SessionsDirty: true, WorldDirty: true}, nil
	}
	coord := newDismissCoord(t, dur, &fakeDirtySink{})

	ch, cancel := coord.SubscribeOutcomes()
	defer cancel()

	if _, err := coord.Dismiss(context.Background(), "sess-p"); err != nil {
		t.Fatal(err)
	}
	first, second := recvOutcome(t, ch), recvOutcome(t, ch)
	if first.Type != OutcomeUpserted || first.ID != "sess-p" || first.Session == nil || first.Session.DismissedAt == nil {
		t.Fatalf("first=%+v", first)
	}
	if second.Type != OutcomeUpserted || second.ID != "sess-c" {
		t.Fatalf("second=%+v", second)
	}
}

// TestOutcomeBusLosslessOrderedDelivery verifies Upserted/Removed outcomes
// are delivered losslessly and in publish order even to a consumer that
// starts reading late.
func TestOutcomeBusLosslessOrderedDelivery(t *testing.T) {
	dur := newFakeDurable(0)
	coord := New(nil, newFakeClient(RunnerMeta{}), dur, &fakeDirtySink{}, nil)

	ch, cancel := coord.SubscribeOutcomes()
	defer cancel()

	const n = 500
	for i := range n {
		coord.outcomes.publish(Outcome{Type: OutcomeUpserted, ID: sid(i)})
	}
	for i := range n {
		o := recvOutcome(t, ch)
		if o.ID != sid(i) {
			t.Fatalf("out of order at %d: got %s", i, o.ID)
		}
	}
}

// TestOutcomeBusActivityLossyUnderBacklog verifies Activity outcomes drop
// once the subscriber backlog exceeds the bound, while durable outcomes are
// never dropped.
func TestOutcomeBusActivityLossyUnderBacklog(t *testing.T) {
	dur := newFakeDurable(0)
	coord := New(nil, newFakeClient(RunnerMeta{}), dur, &fakeDirtySink{}, nil)

	ch, cancel := coord.SubscribeOutcomes()
	defer cancel()

	for range outcomeActivityBacklog + 100 {
		coord.PublishActivity("sess-act")
	}
	// A durable outcome enqueues regardless of backlog.
	coord.outcomes.publish(Outcome{Type: OutcomeUpserted, ID: "sess-last"})

	activities := 0
	for {
		o := recvOutcome(t, ch)
		if o.Type == OutcomeActivity {
			activities++
			continue
		}
		if o.ID != "sess-last" {
			t.Fatalf("unexpected outcome %+v", o)
		}
		break
	}
	// The pump may drain a few items concurrently with publishing, so the
	// delivered count can slightly exceed the enqueue bound; it must stay
	// far below the published total (bounded, lossy).
	if activities > outcomeActivityBacklog+16 {
		t.Fatalf("activity backlog exceeded bound: %d", activities)
	}
	if activities == 0 {
		t.Fatal("expected some activity outcomes delivered")
	}
}

// TestOutcomeBusUnsubscribeClosesChannel verifies cancel stops delivery and
// closes the channel; publishing after cancel is safe.
func TestOutcomeBusUnsubscribeClosesChannel(t *testing.T) {
	dur := newFakeDurable(0)
	coord := New(nil, newFakeClient(RunnerMeta{}), dur, &fakeDirtySink{}, nil)

	ch, cancel := coord.SubscribeOutcomes()
	cancel()
	cancel() // idempotent

	coord.outcomes.publish(Outcome{Type: OutcomeUpserted, ID: "sess-x"})
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected closed channel after cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel not closed after cancel")
	}
	if coord.outcomes.hasSubscribers() {
		t.Fatal("subscriber not removed")
	}
}

// TestOutcomeBusNoSubscribersSkipsReads verifies emitOutcomes performs no
// durable reads when nobody subscribed.
func TestOutcomeBusNoSubscribersSkipsReads(t *testing.T) {
	dur := newFakeDurable(0)
	reads := 0
	dur.session = func(centralstore.SessionID) (centralstore.Session, bool, error) {
		reads++
		return centralstore.Session{}, false, nil
	}
	coord := New(nil, newFakeClient(RunnerMeta{}), dur, &fakeDirtySink{}, nil)
	coord.emitOutcomes(context.Background(), 0, "sess-a", "sess-b")
	if reads != 0 {
		t.Fatalf("expected no reads without subscribers, got %d", reads)
	}
}

// firstBlockSink blocks only the FIRST Committed call until released,
// reproducing fable H-1's schedule: Register parks inside its dirty-sink
// publish while a newer apply commits and publishes first.
type firstBlockSink struct {
	entered chan struct{}
	release chan struct{}
	first   sync.Mutex
	used    bool
}

func (s *firstBlockSink) Committed(_ context.Context, _ centralstore.MutationResult) {
	s.first.Lock()
	isFirst := !s.used
	s.used = true
	s.first.Unlock()
	if isFirst {
		close(s.entered)
		<-s.release
	}
}

// TestOutcomeBusMonotoneDeliveryDropsStaleRow deterministically reproduces
// fable H-1: Register commits v1, blocks in the dirty sink; a runner event
// commits v2 and its outcome is delivered first; the sink is released and
// Register's captured v1 row is published LAST — the watermark must drop it
// so the subscriber's final state is the newest row.
func TestOutcomeBusMonotoneDeliveryDropsStaleRow(t *testing.T) {
	id := sid(230)
	meta := RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Alive: true}}
	client := newFakeClient(meta)
	dur := newFakeDurable(0)
	dur.registerResult = func(centralstore.RunnerRegistration) (centralstore.Session, centralstore.MutationResult, error) {
		return centralstore.Session{ID: id, Version: 1, Unread: false},
			centralstore.MutationResult{Changed: true, SessionsDirty: true, SessionVersion: 1}, nil
	}
	dur.applyResult = func(obs centralstore.RunnerObservation) (centralstore.MutationResult, error) {
		return centralstore.MutationResult{Changed: true, SessionsDirty: true, SessionVersion: 2}, nil
	}
	// The apply's post-commit read returns the newer committed row (v2).
	dur.session = func(centralstore.SessionID) (centralstore.Session, bool, error) {
		return centralstore.Session{ID: id, Version: 2, Unread: true}, true, nil
	}
	sink := &firstBlockSink{entered: make(chan struct{}), release: make(chan struct{})}
	coord := New(nil, client, dur, sink, nil)

	ch, cancel := coord.SubscribeOutcomes()
	defer cancel()

	regDone := make(chan error, 1)
	go func() {
		_, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep230"})
		regDone <- err
	}()
	<-sink.entered // Register is parked inside its dirty-sink publish (v1 not yet emitted)

	// A newer observation commits and publishes while Register is blocked.
	unread := true
	client.stream.send(RunnerEvent{ObservedAt: ts(10), Alive: aliveTrue, Facts: centralstore.RunnerFacts{Unread: &unread}})
	newer := recvOutcome(t, ch)
	if newer.Type != OutcomeUpserted || newer.Session == nil || newer.Session.Version != 2 || !newer.Session.Unread {
		t.Fatalf("expected v2 unread row first, got %+v", newer)
	}

	// Release Register: its stale v1 emit runs LAST and must be dropped.
	close(sink.release)
	if err := <-regDone; err != nil {
		t.Fatalf("Register: %v", err)
	}
	coord.PublishActivity(id) // sentinel: next delivery must be this, not v1
	final := recvOutcome(t, ch)
	if final.Type != OutcomeActivity {
		t.Fatalf("stale v1 row delivered after v2 (final outcome %+v)", final)
	}
}

// TestOutcomeBusRemovedResetsWatermark verifies a Removed outcome is never
// version-gated and resets the per-session watermark so a post-removal
// re-registration's fresh version sequence (starting at 1) is delivered.
func TestOutcomeBusRemovedResetsWatermark(t *testing.T) {
	dur := newFakeDurable(0)
	coord := New(nil, newFakeClient(RunnerMeta{}), dur, &fakeDirtySink{}, nil)

	ch, cancel := coord.SubscribeOutcomes()
	defer cancel()

	v5 := centralstore.Session{ID: "sess-w", Version: 5}
	coord.outcomes.publish(Outcome{Type: OutcomeUpserted, ID: "sess-w", Session: &v5})
	coord.outcomes.publish(Outcome{Type: OutcomeRemoved, ID: "sess-w"})
	v1 := centralstore.Session{ID: "sess-w", Version: 1}
	coord.outcomes.publish(Outcome{Type: OutcomeUpserted, ID: "sess-w", Session: &v1})

	if o := recvOutcome(t, ch); o.Type != OutcomeUpserted || o.Session.Version != 5 {
		t.Fatalf("first=%+v", o)
	}
	if o := recvOutcome(t, ch); o.Type != OutcomeRemoved {
		t.Fatalf("second=%+v", o)
	}
	if o := recvOutcome(t, ch); o.Type != OutcomeUpserted || o.Session.Version != 1 {
		t.Fatalf("fresh sequence after removal must deliver: %+v", o)
	}
}

// TestOutcomeBusLateRemovedDroppedAfterNewerUpserted is a deterministic
// regression for R-2: an older Removed (commit-seq=N) that arrives AFTER a
// newer re-registration Upserted (commit-seq=N+1) for the same session must
// be dropped by the per-session commit-seq watermark.
//
// The schedule reproduced here (publish order differs from commit order):
//
//	1. Remove commits (seq=1).
//	2. Register commits (seq=2) and publishes its Upserted immediately.
//	3. Remove's post-commit read finishes late; its Removed is published last.
//
// Without the watermark the subscriber's final state is "removed" (wrong).
// With the watermark the late seq=1 Removed is dropped (correct).
func TestOutcomeBusLateRemovedDroppedAfterNewerUpserted(t *testing.T) {
	dur := newFakeDurable(0)
	coord := New(nil, newFakeClient(RunnerMeta{}), dur, &fakeDirtySink{}, nil)

	ch, cancel := coord.SubscribeOutcomes()
	defer cancel()

	id := centralstore.SessionID("sess-r2")
	v1 := centralstore.Session{ID: id, Version: 1}

	// Publish in arrival order: newer Upserted (seq=2) first, stale Removed
	// (seq=1) second — exactly the out-of-order window the fix closes.
	coord.outcomes.publish(Outcome{Type: OutcomeUpserted, ID: id, Session: &v1, Sequence: 2})
	coord.outcomes.publish(Outcome{Type: OutcomeRemoved, ID: id, Sequence: 1})

	// Must receive the Upserted.
	if o := recvOutcome(t, ch); o.Type != OutcomeUpserted || o.Sequence != 2 {
		t.Fatalf("expected Upserted seq=2, got %+v", o)
	}

	// The stale Removed (seq=1 < seenSeq=2) must be dropped; the next
	// delivery must be the Activity sentinel, not the Removed.
	coord.PublishActivity(id)
	if o := recvOutcome(t, ch); o.Type != OutcomeActivity {
		t.Fatalf("stale Removed seq=1 delivered after Upserted seq=2 (got %+v)", o)
	}
}

// TestOutcomeBusLateRemovedNormalOrderDelivered verifies that a Removed
// with a higher commit-seq than the preceding Upserted is delivered normally
// (i.e. the watermark only drops truly stale outcomes).
func TestOutcomeBusLateRemovedNormalOrderDelivered(t *testing.T) {
	dur := newFakeDurable(0)
	coord := New(nil, newFakeClient(RunnerMeta{}), dur, &fakeDirtySink{}, nil)

	ch, cancel := coord.SubscribeOutcomes()
	defer cancel()

	id := centralstore.SessionID("sess-r2b")
	v1 := centralstore.Session{ID: id, Version: 1}

	// Normal order: Upserted (seq=1) then Removed (seq=2).
	coord.outcomes.publish(Outcome{Type: OutcomeUpserted, ID: id, Session: &v1, Sequence: 1})
	coord.outcomes.publish(Outcome{Type: OutcomeRemoved, ID: id, Sequence: 2})

	if o := recvOutcome(t, ch); o.Type != OutcomeUpserted {
		t.Fatalf("expected Upserted, got %+v", o)
	}
	if o := recvOutcome(t, ch); o.Type != OutcomeRemoved {
		t.Fatalf("expected Removed, got %+v", o)
	}
	// After a delivered Removed, a fresh v1 re-registration must still pass.
	v1b := centralstore.Session{ID: id, Version: 1}
	coord.outcomes.publish(Outcome{Type: OutcomeUpserted, ID: id, Session: &v1b, Sequence: 3})
	if o := recvOutcome(t, ch); o.Type != OutcomeUpserted || o.Session.Version != 1 {
		t.Fatalf("fresh re-registration must deliver after Removed: %+v", o)
	}
}
