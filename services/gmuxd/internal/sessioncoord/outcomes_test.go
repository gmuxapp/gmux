package sessioncoord

import (
	"context"
	"fmt"
	"reflect"
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

	if err := coord.Remove(context.Background(), "11qpvu53", 1); err != nil {
		t.Fatal(err)
	}
	o := recvOutcome(t, ch)
	if o.Type != OutcomeRemoved || o.ID != "11qpvu53" || o.Session != nil || o.Alive {
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
		return []centralstore.SessionID{"151zemtf", "10cel6cx"}, centralstore.MutationResult{Changed: true, SessionsDirty: true, WorldDirty: true}, nil
	}
	coord := newDismissCoord(t, dur, &fakeDirtySink{})

	ch, cancel := coord.SubscribeOutcomes()
	defer cancel()

	if _, err := coord.Dismiss(context.Background(), "151zemtf"); err != nil {
		t.Fatal(err)
	}
	first, second := recvOutcome(t, ch), recvOutcome(t, ch)
	if first.Type != OutcomeUpserted || first.ID != "151zemtf" || first.Session == nil || first.Session.DismissedAt == nil {
		t.Fatalf("first=%+v", first)
	}
	if second.Type != OutcomeUpserted || second.ID != "10cel6cx" {
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
		coord.PublishActivity("1m2jkaw3")
	}
	// A durable outcome enqueues regardless of backlog.
	coord.outcomes.publish(Outcome{Type: OutcomeUpserted, ID: "1m6s8sy2"})

	activities := 0
	for {
		o := recvOutcome(t, ch)
		if o.Type == OutcomeActivity {
			activities++
			continue
		}
		if o.ID != "1m6s8sy2" {
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

	coord.outcomes.publish(Outcome{Type: OutcomeUpserted, ID: "1108gm0e"})
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
	coord.emitOutcomes(context.Background(), 0, "1mw5c5n9", "18wnzse2")
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

	v5 := centralstore.Session{ID: "1vbfhza6", Version: 5}
	coord.outcomes.publish(Outcome{Type: OutcomeUpserted, ID: "1vbfhza6", Session: &v5})
	coord.outcomes.publish(Outcome{Type: OutcomeRemoved, ID: "1vbfhza6"})
	v1 := centralstore.Session{ID: "1vbfhza6", Version: 1}
	coord.outcomes.publish(Outcome{Type: OutcomeUpserted, ID: "1vbfhza6", Session: &v1})

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
//  1. Remove commits (seq=1).
//  2. Register commits (seq=2) and publishes its Upserted immediately.
//  3. Remove's post-commit read finishes late; its Removed is published last.
//
// Without the watermark the subscriber's final state is "removed" (wrong).
// With the watermark the late seq=1 Removed is dropped (correct).
func TestOutcomeBusLateRemovedDroppedAfterNewerUpserted(t *testing.T) {
	dur := newFakeDurable(0)
	coord := New(nil, newFakeClient(RunnerMeta{}), dur, &fakeDirtySink{}, nil)

	ch, cancel := coord.SubscribeOutcomes()
	defer cancel()

	id := centralstore.SessionID("1b4k46rv")
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

// TestOutcomeBusOldGenerationUpsertDroppedAfterReregistered deterministically
// reproduces a three-publish composition of the removal-boundary race:
//
//  1. a fresh re-registration Upserted (v1, seq=2) arrives first;
//  2. the old generation's late Removed (seq=1) is dropped;
//  3. an even later captured old-generation Upserted (v5, seq=1) arrives.
//
// The stale Removed must not reset either watermark, and the stale Upserted
// must also be sequence-gated; otherwise v5 becomes the subscriber's final
// state despite belonging to the removed generation.
func TestOutcomeBusOldGenerationUpsertDroppedAfterReregistered(t *testing.T) {
	dur := newFakeDurable(0)
	coord := New(nil, newFakeClient(RunnerMeta{}), dur, &fakeDirtySink{}, nil)

	ch, cancel := coord.SubscribeOutcomes()
	defer cancel()

	id := centralstore.SessionID("1o6ocu1e")
	fresh := centralstore.Session{ID: id, Version: 1}
	old := centralstore.Session{ID: id, Version: 5}

	coord.outcomes.publish(Outcome{Type: OutcomeUpserted, ID: id, Session: &fresh, Sequence: 2})
	coord.outcomes.publish(Outcome{Type: OutcomeRemoved, ID: id, Sequence: 1})
	coord.outcomes.publish(Outcome{Type: OutcomeUpserted, ID: id, Session: &old, Sequence: 1})

	if o := recvOutcome(t, ch); o.Type != OutcomeUpserted || o.Session == nil || o.Session.Version != 1 || o.Sequence != 2 {
		t.Fatalf("expected fresh re-registration only, got %+v", o)
	}

	// Non-vacuously prove both stale domain outcomes were rejected: the next
	// delivery must be this sentinel rather than Removed or old-generation v5.
	coord.PublishActivity(id)
	if o := recvOutcome(t, ch); o.Type != OutcomeActivity {
		t.Fatalf("old-generation outcome delivered after re-registration: %+v", o)
	}
}

func assertOldWatermarkReregisterSchedule(t *testing.T, coord *Coordinator, ch <-chan Outcome, id centralstore.SessionID) {
	t.Helper()
	fresh := centralstore.Session{ID: id, Version: 1}
	coord.outcomes.publish(Outcome{Type: OutcomeUpserted, ID: id, Session: &fresh, Sequence: 3})
	coord.outcomes.publish(Outcome{Type: OutcomeRemoved, ID: id, Sequence: 2})

	if o := recvOutcome(t, ch); o.Type != OutcomeUpserted || o.Session == nil || o.Session.Version != 1 || o.Sequence != 3 {
		t.Fatalf("fresh re-registration was not delivered: %+v", o)
	}
	coord.PublishActivity(id)
	if o := recvOutcome(t, ch); o.Type != OutcomeActivity {
		t.Fatalf("delayed removal survived fresh re-registration: %+v", o)
	}
}

// TestOutcomeBusOldWatermarkAcceptsFreshGeneration pins the long-lived
// subscriber schedule: old v5 is already reflected, fresh v1 from a newer
// commit publishes before the intervening removal, and must supersede the old
// row-version generation before making that delayed removal stale.
func TestOutcomeBusOldWatermarkAcceptsFreshGeneration(t *testing.T) {
	coord := New(nil, newFakeClient(RunnerMeta{}), newFakeDurable(0), &fakeDirtySink{}, nil)
	ch, cancel := coord.SubscribeOutcomes()
	defer cancel()
	id := centralstore.SessionID("13y9ugnk")
	old := centralstore.Session{ID: id, Version: 5}
	coord.outcomes.publish(Outcome{Type: OutcomeUpserted, ID: id, Session: &old, Sequence: 1})
	if o := recvOutcome(t, ch); o.Session == nil || o.Session.Version != 5 {
		t.Fatalf("old generation setup: %+v", o)
	}
	assertOldWatermarkReregisterSchedule(t, coord, ch, id)
}

// TestOutcomeBusSeededOldWatermarkAcceptsFreshGeneration exercises the same
// schedule when old v5 came from SubscribeOutcomesSeed and therefore has a row
// watermark but no commit-sequence stamp.
func TestOutcomeBusSeededOldWatermarkAcceptsFreshGeneration(t *testing.T) {
	id := centralstore.SessionID("1ig01k68")
	dur := newFakeDurable(0)
	dur.listSessions = func() ([]centralstore.Session, error) {
		return []centralstore.Session{{ID: id, Version: 5}}, nil
	}
	coord := New(nil, newFakeClient(RunnerMeta{}), dur, &fakeDirtySink{}, nil)
	seed, ch, cancel, err := coord.SubscribeOutcomesSeed(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if len(seed) != 1 || seed[0].Session == nil || seed[0].Session.Version != 5 {
		t.Fatalf("seed=%+v", seed)
	}
	assertOldWatermarkReregisterSchedule(t, coord, ch, id)
}

// TestOutcomeBusNewerSequenceDeduplicatesIdenticalProjection proves a newer
// sequence advances stale-outcome protection without redelivering the exact
// same durable/runtime projection observed by a racing post-commit read.
func TestOutcomeBusNewerSequenceDeduplicatesIdenticalProjection(t *testing.T) {
	coord := New(nil, newFakeClient(RunnerMeta{}), newFakeDurable(0), &fakeDirtySink{}, nil)
	ch, cancel := coord.SubscribeOutcomes()
	defer cancel()
	id := centralstore.SessionID("1xf1h2dn")
	row := centralstore.Session{ID: id, Version: 5, Title: "same"}
	first := Outcome{Type: OutcomeUpserted, ID: id, Session: &row, Alive: true, Generation: 7, Sequence: 1}
	second := first
	second.Sequence = 3
	coord.outcomes.publish(first)
	coord.outcomes.publish(second)
	coord.outcomes.publish(Outcome{Type: OutcomeRemoved, ID: id, Sequence: 2})
	if o := recvOutcome(t, ch); o.Sequence != 1 {
		t.Fatalf("first projection=%+v", o)
	}
	coord.PublishActivity(id)
	if o := recvOutcome(t, ch); o.Type != OutcomeActivity {
		t.Fatalf("duplicate projection or stale removal delivered: %+v", o)
	}
}

// TestOutcomeBusSameVersionNewGenerationDelivered ensures projection dedup
// includes runtime generation identity: replacement generation 8 at the same
// durable v1 is a real state transition, not an identical row duplicate.
func TestOutcomeBusSameVersionNewGenerationDelivered(t *testing.T) {
	coord := New(nil, newFakeClient(RunnerMeta{}), newFakeDurable(0), &fakeDirtySink{}, nil)
	ch, cancel := coord.SubscribeOutcomes()
	defer cancel()
	id := centralstore.SessionID("1sutoynf")
	row1 := centralstore.Session{ID: id, Version: 1}
	row2 := row1
	coord.outcomes.publish(Outcome{Type: OutcomeUpserted, ID: id, Session: &row1, Alive: true, Generation: 7, Sequence: 1})
	coord.outcomes.publish(Outcome{Type: OutcomeUpserted, ID: id, Session: &row2, Alive: true, Generation: 8, Sequence: 2})
	if o := recvOutcome(t, ch); o.Generation != 7 {
		t.Fatalf("old generation=%+v", o)
	}
	if o := recvOutcome(t, ch); o.Generation != 8 || o.Session == nil || o.Session.Version != 1 {
		t.Fatalf("same-version replacement was deduplicated: %+v", o)
	}
}

// TestOutcomeBusSeedDeduplicatesReflectedCommit pins the startup contract: a
// stamped publisher for a commit already represented by Seed advances the
// sequence watermark but does not emit the identical projection again.
func TestOutcomeBusSeedDeduplicatesReflectedCommit(t *testing.T) {
	id := centralstore.SessionID("1c4is9hf")
	row := centralstore.Session{ID: id, Version: 5, Title: "seeded"}
	dur := newFakeDurable(0)
	dur.listSessions = func() ([]centralstore.Session, error) { return []centralstore.Session{row}, nil }
	coord := New(nil, newFakeClient(RunnerMeta{}), dur, &fakeDirtySink{}, nil)
	seed, ch, cancel, err := coord.SubscribeOutcomesSeed(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if len(seed) != 1 {
		t.Fatalf("seed=%+v", seed)
	}
	copyRow := row
	coord.outcomes.publish(Outcome{Type: OutcomeUpserted, ID: id, Session: &copyRow, Sequence: 3})
	coord.outcomes.publish(Outcome{Type: OutcomeRemoved, ID: id, Sequence: 2})
	coord.PublishActivity(id)
	if o := recvOutcome(t, ch); o.Type != OutcomeActivity {
		t.Fatalf("seed-reflected projection or stale removal delivered: %+v", o)
	}
}

func aliasTestSession(id centralstore.SessionID, version centralstore.RowVersion, title string) centralstore.Session {
	started, exited, activity, dismissed := centralstore.UnixMillis(1), centralstore.UnixMillis(2), centralstore.UnixMillis(3), centralstore.UnixMillis(4)
	exitCode, cols, rows := 5, uint16(80), uint16(24)
	parent, launchedFrom := centralstore.SessionID("parent"), centralstore.SessionID("launcher")
	return centralstore.Session{
		ID: id, Version: version, Title: title,
		Command: []string{"cmd", "arg"}, Remotes: map[string]string{"origin": "url"},
		StartedAt: &started, ExitedAt: &exited, LastActivityAt: &activity, DismissedAt: &dismissed,
		ExitCode: &exitCode, TerminalCols: &cols, TerminalRows: &rows, ParentSessionID: &parent, LaunchedFromSessionID: &launchedFrom,
	}
}

func corruptAliasSession(s *centralstore.Session) {
	s.Title = "consumer-local"
	s.Command[0] = "mutated"
	s.Remotes["origin"] = "mutated"
	*s.StartedAt = 11
	*s.ExitedAt = 12
	*s.LastActivityAt = 13
	*s.DismissedAt = 14
	*s.ExitCode = 15
	*s.TerminalCols = 16
	*s.TerminalRows = 17
	*s.ParentSessionID = "mutated-parent"
	*s.LaunchedFromSessionID = "mutated-launcher"
}

// TestOutcomeBusEventProjectionSnapshotOwned reproduces both event alias
// failure directions: consumer mutation must neither resurrect an unchanged
// projection nor pre-corrupt the baseline to suppress a real future update.
func TestOutcomeBusEventProjectionSnapshotOwned(t *testing.T) {
	t.Run("unchanged row remains deduplicated", func(t *testing.T) {
		id := centralstore.SessionID("15nm9eeg")
		coord := New(nil, newFakeClient(RunnerMeta{}), newFakeDurable(0), &fakeDirtySink{}, nil)
		ch, cancel := coord.SubscribeOutcomes()
		defer cancel()
		firstRow := aliasTestSession(id, 1, "real")
		coord.outcomes.publish(Outcome{Type: OutcomeUpserted, ID: id, Session: &firstRow, Sequence: 1})
		got := recvOutcome(t, ch)
		corruptAliasSession(got.Session)
		unchanged := aliasTestSession(id, 1, "real")
		coord.outcomes.publish(Outcome{Type: OutcomeUpserted, ID: id, Session: &unchanged, Sequence: 2})
		coord.PublishActivity(id)
		if o := recvOutcome(t, ch); o.Type != OutcomeActivity {
			t.Fatalf("consumer mutation resurrected unchanged event: %+v", o)
		}
	})

	t.Run("real update remains deliverable", func(t *testing.T) {
		id := centralstore.SessionID("1v2ulglf")
		coord := New(nil, newFakeClient(RunnerMeta{}), newFakeDurable(0), &fakeDirtySink{}, nil)
		ch, cancel := coord.SubscribeOutcomes()
		defer cancel()
		old := aliasTestSession(id, 1, "old")
		coord.outcomes.publish(Outcome{Type: OutcomeUpserted, ID: id, Session: &old, Sequence: 1})
		got := recvOutcome(t, ch)
		consumerFuture := aliasTestSession(id, 2, "new")
		*got.Session = consumerFuture
		future := aliasTestSession(id, 2, "new")
		coord.outcomes.publish(Outcome{Type: OutcomeUpserted, ID: id, Session: &future, Sequence: 2})
		if o := recvOutcome(t, ch); o.Session == nil || o.Session.Version != 2 || o.Session.Title != "new" {
			t.Fatalf("consumer mutation suppressed real update: %+v", o)
		}
	})
}

// TestOutcomeBusSeedProjectionSnapshotOwned applies the same ownership check
// to the Seed value, which escapes synchronously while its dedup baseline is
// retained by the subscriber.
func TestOutcomeBusSeedProjectionSnapshotOwned(t *testing.T) {
	id := centralstore.SessionID("1znq2hep")
	seedRow := aliasTestSession(id, 1, "real")
	dur := newFakeDurable(0)
	dur.listSessions = func() ([]centralstore.Session, error) { return []centralstore.Session{seedRow}, nil }
	coord := New(nil, newFakeClient(RunnerMeta{}), dur, &fakeDirtySink{}, nil)
	seed, ch, cancel, err := coord.SubscribeOutcomesSeed(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	corruptAliasSession(seed[0].Session)
	unchanged := aliasTestSession(id, 1, "real")
	coord.outcomes.publish(Outcome{Type: OutcomeUpserted, ID: id, Session: &unchanged, Sequence: 2})
	coord.PublishActivity(id)
	if o := recvOutcome(t, ch); o.Type != OutcomeActivity {
		t.Fatalf("seed mutation resurrected reflected row: %+v", o)
	}
}

func TestOutcomeBusConcurrentConsumerMutationDoesNotRaceProjection(t *testing.T) {
	coord := New(nil, newFakeClient(RunnerMeta{}), newFakeDurable(0), &fakeDirtySink{}, nil)
	ch, cancel := coord.SubscribeOutcomes()
	defer cancel()
	for i := range 100 {
		id := centralstore.SessionID(fmt.Sprintf("%08d", i))
		row := aliasTestSession(id, 1, "real")
		coord.outcomes.publish(Outcome{Type: OutcomeUpserted, ID: id, Session: &row, Sequence: uint64(i*2 + 1)})
		got := recvOutcome(t, ch)
		unchanged := aliasTestSession(id, 1, "real")
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); corruptAliasSession(got.Session) }()
		go func() {
			defer wg.Done()
			coord.outcomes.publish(Outcome{Type: OutcomeUpserted, ID: id, Session: &unchanged, Sequence: uint64(i*2 + 2)})
		}()
		wg.Wait()
	}
}

func TestOutcomeBusSeedMutationCannotSuppressRealUpdate(t *testing.T) {
	id := centralstore.SessionID("1v435n4r")
	seedRow := aliasTestSession(id, 1, "old")
	dur := newFakeDurable(0)
	dur.listSessions = func() ([]centralstore.Session, error) { return []centralstore.Session{seedRow}, nil }
	coord := New(nil, newFakeClient(RunnerMeta{}), dur, &fakeDirtySink{}, nil)
	seed, ch, cancel, err := coord.SubscribeOutcomesSeed(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	consumerFuture := aliasTestSession(id, 2, "new")
	*seed[0].Session = consumerFuture
	future := aliasTestSession(id, 2, "new")
	coord.outcomes.publish(Outcome{Type: OutcomeUpserted, ID: id, Session: &future, Sequence: 2})
	if o := recvOutcome(t, ch); o.Session == nil || o.Session.Version != 2 || o.Session.Title != "new" {
		t.Fatalf("seed mutation suppressed real update: %+v", o)
	}
}

// TestCloneOutcomeSessionOwnsEveryReferenceField documents the current
// centralstore.Session reference shape. If Session gains another reference
// field, this test fails until cloneOutcomeSession explicitly owns it.
func TestCloneOutcomeSessionOwnsEveryReferenceField(t *testing.T) {
	allowed := map[string]bool{
		"Command": true, "Remotes": true, "StartedAt": true, "ExitedAt": true,
		"LastActivityAt": true, "DismissedAt": true, "ExitCode": true,
		"TerminalCols": true, "TerminalRows": true, "ParentSessionID": true,
		"LaunchedFromSessionID": true,
	}
	typ := reflect.TypeOf(centralstore.Session{})
	for i := range typ.NumField() {
		field := typ.Field(i)
		switch field.Type.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Map, reflect.Interface:
			if !allowed[field.Name] {
				t.Fatalf("new Session reference field %s (%s) is not owned by cloneOutcomeSession", field.Name, field.Type)
			}
		}
	}
	if len(allowed) != 11 {
		t.Fatalf("reference-field inventory changed: %v", allowed)
	}
	original := aliasTestSession("1xke0d7j", 1, "real")
	clone := cloneOutcomeSession(&original)
	corruptAliasSession(&original)
	want := aliasTestSession("1xke0d7j", 1, "real")
	if !reflect.DeepEqual(clone, &want) {
		t.Fatalf("clone retained reference alias:\n got %+v\nwant %+v", clone, &want)
	}
}

func TestOutcomeProjectionPreservesCommandNilness(t *testing.T) {
	t.Run("identical empty non-nil deduplicates", func(t *testing.T) {
		coord := New(nil, newFakeClient(RunnerMeta{}), newFakeDurable(0), &fakeDirtySink{}, nil)
		ch, cancel := coord.SubscribeOutcomes()
		defer cancel()
		id := centralstore.SessionID("16rzmogz")
		first := centralstore.Session{ID: id, Version: 1, Command: []string{}}
		second := centralstore.Session{ID: id, Version: 1, Command: []string{}}
		coord.outcomes.publish(Outcome{Type: OutcomeUpserted, ID: id, Session: &first, Sequence: 1})
		coord.outcomes.publish(Outcome{Type: OutcomeUpserted, ID: id, Session: &second, Sequence: 2})
		if o := recvOutcome(t, ch); o.Session == nil || o.Session.Command == nil {
			t.Fatalf("first empty command projection=%+v", o)
		}
		coord.PublishActivity(id)
		if o := recvOutcome(t, ch); o.Type != OutcomeActivity {
			t.Fatalf("identical empty non-nil command redelivered: %+v", o)
		}
	})

	for _, tc := range []struct {
		name        string
		first, next []string
	}{
		{name: "empty to nil", first: []string{}, next: nil},
		{name: "nil to empty", first: nil, next: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			coord := New(nil, newFakeClient(RunnerMeta{}), newFakeDurable(0), &fakeDirtySink{}, nil)
			ch, cancel := coord.SubscribeOutcomes()
			defer cancel()
			id := centralstore.SessionID("1cb2gy89" + tc.name)
			first := centralstore.Session{ID: id, Version: 1, Command: tc.first}
			next := centralstore.Session{ID: id, Version: 1, Command: tc.next}
			coord.outcomes.publish(Outcome{Type: OutcomeUpserted, ID: id, Session: &first, Sequence: 1})
			coord.outcomes.publish(Outcome{Type: OutcomeUpserted, ID: id, Session: &next, Sequence: 2})
			_ = recvOutcome(t, ch)
			got := recvOutcome(t, ch)
			if got.Session == nil || (got.Session.Command == nil) != (tc.next == nil) {
				t.Fatalf("Command nilness transition lost: %+v", got)
			}
		})
	}

	nilCommand := centralstore.Session{Command: nil}
	emptyCommand := centralstore.Session{Command: []string{}}
	if got := cloneOutcomeSession(&nilCommand); got.Command != nil {
		t.Fatalf("nil Command cloned as non-nil: %#v", got.Command)
	}
	if got := cloneOutcomeSession(&emptyCommand); got.Command == nil || len(got.Command) != 0 {
		t.Fatalf("empty non-nil Command not preserved: %#v", got.Command)
	}
}

type delayedRemovalDurable struct {
	*fakeDurable
	stateMu           sync.Mutex
	row               *centralstore.Session
	blockRemovalRead  bool
	captureBeforeWait bool
	readEntered       chan struct{}
	releaseRead       chan struct{}
}

func (d *delayedRemovalDurable) RegisterRunner(_ context.Context, reg centralstore.RunnerRegistration) (centralstore.Session, centralstore.MutationResult, error) {
	d.stateMu.Lock()
	row := centralstore.Session{ID: reg.ID, Version: 1}
	d.row = &row
	d.stateMu.Unlock()
	return row, centralstore.MutationResult{Changed: true, SessionsDirty: true, SessionVersion: 1}, nil
}

func (d *delayedRemovalDurable) RemoveSessionAtVersion(_ context.Context, _ centralstore.SessionID, _ centralstore.RowVersion) (centralstore.MutationResult, error) {
	d.stateMu.Lock()
	d.row = nil
	d.blockRemovalRead = true
	d.stateMu.Unlock()
	return centralstore.MutationResult{Changed: true, SessionsDirty: true, WorldDirty: true}, nil
}

func (d *delayedRemovalDurable) Session(_ context.Context, _ centralstore.SessionID) (centralstore.Session, bool, error) {
	d.stateMu.Lock()
	block := d.blockRemovalRead
	if block {
		d.blockRemovalRead = false
	}
	captureBeforeWait := d.captureBeforeWait
	var row centralstore.Session
	ok := d.row != nil
	if ok {
		row = *d.row
	}
	d.stateMu.Unlock()
	if block {
		close(d.readEntered)
		<-d.releaseRead
		if !captureBeforeWait {
			d.stateMu.Lock()
			ok = d.row != nil
			if ok {
				row = *d.row
			}
			d.stateMu.Unlock()
		}
	}
	return row, ok, nil
}

func (d *delayedRemovalDurable) ListSessions(_ context.Context) ([]centralstore.Session, error) {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	if d.row == nil {
		return nil, nil
	}
	return []centralstore.Session{*d.row}, nil
}

// TestCoordinatorReregisterBeatsDelayedRemoval composes the production seam:
// a real Remove captures absence and blocks before publishing, then a real
// Register commits and publishes fresh v1 first. Besides the ordering
// invariant, the non-zero stamps make this test kill allocSeq=>0 and
// Sequence-propagation=>0 mutations.
func TestCoordinatorReregisterBeatsDelayedRemoval(t *testing.T) {
	id := centralstore.SessionID("1xmkuqe4")
	old := centralstore.Session{ID: id, Version: 5}
	dur := &delayedRemovalDurable{
		fakeDurable:       newFakeDurable(0),
		row:               &old,
		captureBeforeWait: true,
		readEntered:       make(chan struct{}),
		releaseRead:       make(chan struct{}),
	}
	client := newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Alive: true}})
	coord := New(nil, client, dur, &fakeDirtySink{}, nil)
	seed, ch, cancel, err := coord.SubscribeOutcomesSeed(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if len(seed) != 1 || seed[0].Session == nil || seed[0].Session.Version != 5 {
		t.Fatalf("seed=%+v", seed)
	}

	removeDone := make(chan error, 1)
	go func() { removeDone <- coord.Remove(context.Background(), id, 5) }()
	select {
	case <-dur.readEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("Remove did not reach post-commit read barrier")
	}

	if _, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep-production-order"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	fresh := recvOutcome(t, ch)
	if fresh.Type != OutcomeUpserted || fresh.Session == nil || fresh.Session.Version != 1 || fresh.Sequence == 0 {
		t.Fatalf("production registration outcome=%+v", fresh)
	}
	close(dur.releaseRead)
	if err := <-removeDone; err != nil {
		t.Fatalf("Remove: %v", err)
	}
	coord.PublishActivity(id)
	if o := recvOutcome(t, ch); o.Type != OutcomeActivity {
		t.Fatalf("production delayed removal delivered after fresh row: %+v", o)
	}
}

type registerBlockSink struct {
	entered chan struct{}
	release chan struct{}
}

func (s *registerBlockSink) Committed(_ context.Context, result centralstore.MutationResult) {
	if result.Changed && result.SessionsDirty && !result.WorldDirty {
		close(s.entered)
		<-s.release
	}
}

// TestCoordinatorRacingPublishersDeduplicateCurrentProjection composes the
// production duplicate seam. Remove commits first and pauses its post-commit
// read; Register commits v1 but pauses before emitUpserted; Remove then reads
// and publishes that same current v1 with its older stamp; finally Register's
// newer-stamped identical projection must advance sequence state but not emit.
func TestCoordinatorRacingPublishersDeduplicateCurrentProjection(t *testing.T) {
	id := centralstore.SessionID("1di0ttkr")
	old := centralstore.Session{ID: id, Version: 5}
	dur := &delayedRemovalDurable{
		fakeDurable: newFakeDurable(0),
		row:         &old,
		readEntered: make(chan struct{}),
		releaseRead: make(chan struct{}),
	}
	sink := &registerBlockSink{entered: make(chan struct{}), release: make(chan struct{})}
	client := newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Alive: true}})
	coord := New(nil, client, dur, sink, nil)
	ch, cancel := coord.SubscribeOutcomes()
	defer cancel()

	removeDone := make(chan error, 1)
	go func() { removeDone <- coord.Remove(context.Background(), id, 5) }()
	select {
	case <-dur.readEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("Remove did not reach pre-read barrier")
	}

	registerDone := make(chan error, 1)
	go func() {
		_, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep-production-dedup"})
		registerDone <- err
	}()
	select {
	case <-sink.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Register did not reach pre-publication barrier")
	}

	close(dur.releaseRead)
	if err := <-removeDone; err != nil {
		t.Fatalf("Remove: %v", err)
	}
	first := recvOutcome(t, ch)
	if first.Type != OutcomeUpserted || first.Session == nil || first.Session.Version != 1 || first.Sequence == 0 {
		t.Fatalf("older publisher did not carry current projection: %+v", first)
	}
	close(sink.release)
	if err := <-registerDone; err != nil {
		t.Fatalf("Register: %v", err)
	}
	coord.PublishActivity(id)
	if o := recvOutcome(t, ch); o.Type != OutcomeActivity {
		t.Fatalf("newer publisher redelivered identical projection: %+v", o)
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

	id := centralstore.SessionID("1mfaar53")
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
