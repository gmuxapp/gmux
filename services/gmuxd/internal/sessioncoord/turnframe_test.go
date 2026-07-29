package sessioncoord

import (
	"context"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

// frameOfEventually polls the retained frame, because a stream event is applied
// by the drain goroutine.
func frameOfEventually(t *testing.T, c *Coordinator, id centralstore.SessionID) *TurnFrame {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f := c.frameOf(id); f != nil {
			return f
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no frame was retained")
	return nil
}

// A turn frame is runtime state, not a fact: it is retained for the generation
// that sent it and written NOWHERE durable. Persisting it would make a dead
// session's stale answer outlive the runner that asserted it.
func TestTurnFrameIsRetainedNotPersisted(t *testing.T) {
	id := sid(1)
	client := newFakeClient(liveMeta(id, "pi", "conv.jsonl"))
	dur := newFakeDurable(0)
	coord := newCoord(client, dur, &fakeDirtySink{}, nil)
	defer coord.Close()
	if _, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep"}); err != nil {
		t.Fatal(err)
	}
	applied := len(dur.applied)

	// A frame-only event (a mid-turn injection, a rebind clear, a replay
	// snapshot) is retained and writes NOTHING: a durable observation for it
	// would churn the row version for a fact the store does not hold.
	client.stream.send(RunnerEvent{ObservedAt: ts(10), FrameOnly: true, Frame: &TurnFrame{
		Seq: 1, Current: &TurnCurrent{TurnSeq: 3, Exchanges: []TurnExchange{{Ordinal: 1, User: "what is 2+2?"}}},
	}})
	frame := frameOfEventually(t, coord, id)
	if frame.CurrentTurnSeq() != 3 {
		t.Fatalf("retained frame = %+v", frame)
	}
	dur.mu.Lock()
	after := len(dur.applied)
	dur.mu.Unlock()
	if after != applied {
		t.Fatalf("a frame-only event produced %d durable observations", after-applied)
	}

	// A turn EDGE carries both: its status facts are applied, and the frame it
	// carries is retained WITHOUT becoming a durable fact. Persisting it would
	// make a dead session's stale answer outlive the runner that asserted it.
	active := true
	client.stream.send(RunnerEvent{ObservedAt: ts(11), FrameOnly: false,
		Facts: centralstore.RunnerFacts{Active: &active},
		Frame: &TurnFrame{Seq: 2, Current: &TurnCurrent{TurnSeq: 4}},
	})
	deadline := time.Now().Add(2 * time.Second)
	for coord.frameOf(id).CurrentTurnSeq() != 4 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if seq := coord.frameOf(id).CurrentTurnSeq(); seq != 4 {
		t.Fatalf("the edge's frame was not retained (seq %d)", seq)
	}
	dur.mu.Lock()
	defer dur.mu.Unlock()
	if len(dur.applied) != after+1 {
		t.Fatalf("the edge produced %d durable observations, want 1", len(dur.applied)-after)
	}
	obs := dur.applied[len(dur.applied)-1]
	if obs.Facts.Active == nil || !*obs.Facts.Active {
		t.Fatalf("the edge's status facts did not reach the store: %+v", obs.Facts)
	}
}

// The frame a waiter must see is the one retained when the CLOSE was applied, so
// the close outcome carries it. The runner delivers the settled frame and the
// closing status as one event and drain retains the frame before applying its
// facts, so by the time the close commits the answer is already in hand — which
// is what makes "completed, exit 0, no answer" unreachable on this path.
//
// The frame is sent as a separate event here only because this test drives the
// coordinator directly; the wire shape is pinned in cmd/gmuxd
// (TestRunnerEventProjectionStatusCarriesTheTurnFrame).
func TestCloseOutcomeCarriesTheSettledFrame(t *testing.T) {
	id := sid(1)
	client := newFakeClient(liveMeta(id, "pi", "conv.jsonl"))
	dur := newFakeDurable(0)
	// The post-commit read the outcome publish performs. It carries the row, and
	// deliberately NOT the result: the frame is the result's carrier.
	dur.session = func(centralstore.SessionID) (centralstore.Session, bool, error) {
		return centralstore.Session{ID: id, StatusReported: true}, true, nil
	}
	coord := newCoord(client, dur, &fakeDirtySink{}, nil)
	defer coord.Close()
	if _, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep"}); err != nil {
		t.Fatal(err)
	}
	outcomes, cancel := coord.SubscribeOutcomes()
	defer cancel()

	client.stream.send(RunnerEvent{ObservedAt: ts(10), Frame: &TurnFrame{
		Seq: 2, Last: &TurnClose{TurnSeq: 3, Outcome: "completed", Output: "4"},
	}})
	frameOfEventually(t, coord, id) // the frame is applied before the status event
	inactive := false
	client.stream.send(RunnerEvent{ObservedAt: ts(11), Facts: centralstore.RunnerFacts{Active: &inactive}})

	deadline := time.After(2 * time.Second)
	for {
		select {
		case o := <-outcomes:
			if o.Type != OutcomeUpserted || o.ID != id {
				continue
			}
			if o.Frame == nil {
				continue // an earlier registration outcome, published before the frame
			}
			if got := o.Frame.ClosedTurn(3); got == nil || got.Output != "4" {
				t.Fatalf("close outcome carried %+v", o.Frame)
			}
			return
		case <-deadline:
			t.Fatal("no frame-carrying outcome was published")
		}
	}
}

// A frame belongs to the generation that asserted it. An event for a replaced
// generation must not install one, or a taken-over session's successor would
// serve its predecessor's answer.
func TestFrameFromAReplacedGenerationIsDropped(t *testing.T) {
	id := sid(1)
	reg := NewRegistry()
	reg.install(registryEntry{Runtime: Runtime{SessionID: id, Generation: 2}, dead: make(chan struct{})})
	if reg.setFrame(id, 1, &TurnFrame{Last: &TurnClose{TurnSeq: 1, Output: "old"}}) {
		t.Fatal("a stale generation installed a frame")
	}
	if f := reg.Frame(id); f != nil {
		t.Fatalf("frame = %+v", f)
	}
	if !reg.setFrame(id, 2, &TurnFrame{Last: &TurnClose{TurnSeq: 9, Output: "ours"}}) {
		t.Fatal("the installed generation could not set its frame")
	}
	if got := reg.Frame(id).ClosedTurn(9); got == nil || got.Output != "ours" {
		t.Fatalf("frame = %+v", reg.Frame(id))
	}
	// A fenced entry (a replacement inside its commit-to-install window) serves
	// nothing: it is mid-replacement and no longer speaks for the session.
	reg.supersede(id, 2)
	if f := reg.Frame(id); f != nil {
		t.Fatalf("a fenced generation served %+v", f)
	}
}

// TestReplayedFrameSurvivesRegistration is the reconnect path, and it was a real
// hole: a runner's connect-time replay is the FIRST thing on a fresh stream, so it
// lands in Register's pre-registration drain — before any drain goroutine exists.
// That drain folds events with reduce(), which merges durable facts only, so the
// replayed frame was dropped and the entry installed with none.
//
// The cost was invisible in unit tests and obvious live: after a daemon restart,
// a wait armed against a turn that was still running resolved `completed` with no
// answer, because it had no turn_seq to match the close against. Staged live, that
// wait ran 141s and printed nothing.
func TestReplayedFrameSurvivesRegistration(t *testing.T) {
	id := sid(1)
	client := newFakeClient(liveMeta(id, "pi", "conv.jsonl"))
	// The replay, queued before Register reads the stream: one coupled edge
	// carrying the running turn's identity, exactly as handleEvents writes it.
	active := true
	client.stream.send(RunnerEvent{ObservedAt: ts(5),
		Facts: centralstore.RunnerFacts{Active: &active},
		Frame: &TurnFrame{Seq: 3, Current: &TurnCurrent{TurnSeq: 8, Exchanges: []TurnExchange{{Ordinal: 1, User: "ask"}}}},
	})

	coord := newCoord(client, newFakeDurable(0), &fakeDirtySink{}, nil)
	defer coord.Close()
	rt, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep"})
	if err != nil {
		t.Fatal(err)
	}
	if rt.Frame.CurrentTurnSeq() != 8 {
		t.Fatalf("the installed runtime lost the replayed frame: %+v", rt.Frame)
	}
	if got := coord.frameOf(id).CurrentTurnSeq(); got != 8 {
		t.Fatalf("frameOf = %d, want the replayed turn 8 — a wait armed after a reconnect "+
			"would bind 0 and resolve result-free", got)
	}
	// The durable half of the same event still landed: the replay is not special
	// beyond carrying the frame.
	if rt.Generation == 0 {
		t.Fatal("no generation installed")
	}
}

// And a session that replays a frame with no status yet (the standalone
// turn_frame shape) is retained the same way.
func TestReplayedFrameOnlyEventSurvivesRegistration(t *testing.T) {
	id := sid(1)
	client := newFakeClient(liveMeta(id, "pi", "conv.jsonl"))
	client.stream.send(RunnerEvent{ObservedAt: ts(5), FrameOnly: true,
		Frame: &TurnFrame{Seq: 2, Last: &TurnClose{TurnSeq: 4, Outcome: "completed", Output: "42"}},
	})
	coord := newCoord(client, newFakeDurable(0), &fakeDirtySink{}, nil)
	defer coord.Close()
	if _, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep"}); err != nil {
		t.Fatal(err)
	}
	if got := coord.frameOf(id).ClosedTurn(4); got == nil || got.Output != "42" {
		t.Fatalf("frameOf lost a replayed frame-only event: %+v", coord.frameOf(id))
	}
}
