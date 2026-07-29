package session

import (
	"encoding/json"
	"testing"

	"github.com/gmuxapp/gmux/packages/adapter"
)

// drain reads everything currently queued on a subscription.
func drain(ch chan Event) []Event {
	var out []Event
	for {
		select {
		case e := <-ch:
			out = append(out, e)
		default:
			return out
		}
	}
}

// edgeFrame decodes the frame carried by a status event, or nil when the event
// carries none. It goes through JSON on purpose: what a subscriber (the daemon)
// can actually observe is the marshalled payload, not the Go value.
func edgeFrame(t *testing.T, e Event) *TurnFrame {
	t.Helper()
	raw, err := json.Marshal(e.Data)
	if err != nil {
		t.Fatalf("marshal %v: %v", e.Data, err)
	}
	var payload struct {
		Active bool       `json:"active"`
		Frame  *TurnFrame `json:"turn_frame"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return payload.Frame
}

// TestTurnEdgeCarriesItsFrameInOneEvent is the transport half of the scoped
// delivery invariant: a status edge and the frame it belongs to are ONE event, so
// no subscriber can observe a close without the frame that closed it.
//
// The event type stays "status" so a consumer that knows nothing about frames
// still sees the status event it always saw — asserted here, because that
// compatibility is what lets the runner ship this without a flag day.
func TestTurnEdgeCarriesItsFrameInOneEvent(t *testing.T) {
	st := New(Config{ID: "s1", Adapter: "pi"})
	ch := st.Subscribe()
	defer st.Unsubscribe(ch)

	st.OpenTurn(7, "what is 2+2?", 0)
	events := drain(ch)
	if len(events) != 1 || events[0].Type != "status" {
		t.Fatalf("open turn emitted %d event(s): %+v", len(events), events)
	}
	frame := edgeFrame(t, events[0])
	if frame == nil || frame.Current == nil || frame.Current.TurnSeq != 7 {
		t.Fatalf("the active edge did not carry its turn identity: %+v", frame)
	}

	st.CloseTurnFrame(TurnClose{TurnSeq: 7, Outcome: "completed", Output: "4"},
		&adapter.Status{Active: false})
	events = drain(ch)
	if len(events) != 1 || events[0].Type != "status" {
		t.Fatalf("close emitted %d event(s): %+v", len(events), events)
	}
	frame = edgeFrame(t, events[0])
	if frame == nil || frame.Last == nil || frame.Last.TurnSeq != 7 || frame.Last.Output != "4" {
		t.Fatalf("the close did not carry its result: %+v", frame)
	}
	// The status fields are still there, in place, for a frame-blind consumer.
	raw, _ := json.Marshal(events[0].Data)
	var status adapter.Status
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatalf("a status consumer could not decode the edge: %v (%s)", err, raw)
	}
	if status.Active {
		t.Fatalf("close edge decoded as active: %s", raw)
	}
}

// TestFullSubscriberNeverSeesACloseWithoutItsFrame is the failure mode this
// coupling exists for. The fan-out is lossy by design (a full buffer drops rather
// than stalling the runner), and two separate sends could have one dropped and
// the other delivered — a close a waiter cannot attribute, which resolves
// `completed` with no answer.
//
// The subscriber's buffer is filled first, so every send during the turn is
// dropped; then it is drained and the session runs another turn. The property:
// every status edge the subscriber DOES see carries its frame. Never a close
// without one.
func TestFullSubscriberNeverSeesACloseWithoutItsFrame(t *testing.T) {
	st := New(Config{ID: "s1", Adapter: "pi"})
	ch := st.Subscribe()
	defer st.Unsubscribe(ch)

	// Fill the 16-deep buffer, then overfill it: from here every emit drops.
	for i := 0; i < 32; i++ {
		st.SetSubtitle(string(rune('a' + i%26)))
	}
	st.OpenTurn(1, "first", 0)
	st.CloseTurnFrame(TurnClose{TurnSeq: 1, Outcome: "completed", Output: "first answer"},
		&adapter.Status{Active: false})

	// Whatever survived, no status event may be frameless.
	for _, e := range drain(ch) {
		if e.Type != "status" {
			continue
		}
		if edgeFrame(t, e) == nil {
			t.Fatalf("a status edge arrived without its frame: %+v", e)
		}
	}

	// And with room again, the next turn's close arrives whole.
	st.OpenTurn(2, "second", 0)
	st.CloseTurnFrame(TurnClose{TurnSeq: 2, Outcome: "completed", Output: "second answer"},
		&adapter.Status{Active: false})
	var closes int
	for _, e := range drain(ch) {
		if e.Type != "status" {
			continue
		}
		frame := edgeFrame(t, e)
		if frame == nil {
			t.Fatalf("a status edge arrived without its frame: %+v", e)
		}
		if frame.Last != nil && frame.Last.TurnSeq == 2 {
			closes++
			if frame.Last.Output != "second answer" {
				t.Fatalf("close carried %q", frame.Last.Output)
			}
		}
	}
	if closes != 1 {
		t.Fatalf("the recovered subscriber saw %d attributable closes, want 1", closes)
	}
}

// A stale end (one that closes no open turn) still publishes its assertion, and
// travels alone because there is no status transition to pair it with. The
// turn_seq match downstream is what decides whether anyone may use it.
func TestStaleCloseEmitsTheFrameAlone(t *testing.T) {
	st := New(Config{ID: "s1", Adapter: "pi"})
	ch := st.Subscribe()
	defer st.Unsubscribe(ch)

	if st.CloseTurnFrame(TurnClose{TurnSeq: 4, Outcome: "completed", Output: "late"},
		&adapter.Status{Active: false}) {
		t.Fatal("closed a turn that was never open")
	}
	events := drain(ch)
	if len(events) != 1 || events[0].Type != "turn_frame" {
		t.Fatalf("stale close emitted %+v", events)
	}
	if f := st.TurnFrameSnapshot(); f.Last == nil || f.Last.TurnSeq != 4 {
		t.Fatalf("stale close was not recorded: %+v", f)
	}
}

// An injection is the one frame update with no status transition to ride: the
// turn was already active and stays active. Pinned so a future refactor does not
// invent a spurious active edge for it — that edge would look like a new turn to
// the delivery reservation.
func TestInjectionEmitsFrameOnly(t *testing.T) {
	st := New(Config{ID: "s1", Adapter: "pi"})
	ch := st.Subscribe()
	defer st.Unsubscribe(ch)
	st.OpenTurn(3, "go", 0)
	drain(ch)

	st.NoteInjection(3, "actually, stop", 0)
	events := drain(ch)
	if len(events) != 1 || events[0].Type != "turn_frame" {
		t.Fatalf("injection emitted %+v", events)
	}
	if st.StatusSnapshot() == nil || !st.StatusSnapshot().Active {
		t.Fatal("the turn must still be active after an injection")
	}
}

// A raw `PUT /status` close (a script, a non-hook child) belongs to nobody's
// turn: it asserts no identity and no result. It must still leave the frame
// honest — an idle session may not advertise a turn that has ended — while NOT
// inventing a close record, because a turn_seq in `last` is something a waiter
// could match against an answer that does not exist.
func TestRawCloseAbandonsTheCurrentTurn(t *testing.T) {
	st := New(Config{ID: "s1", Adapter: "pi"})
	ch := st.Subscribe()
	defer st.Unsubscribe(ch)

	// A real turn ran and closed, so `last` holds a genuine result.
	st.OpenTurn(1, "first", 0)
	st.CloseTurnFrame(TurnClose{TurnSeq: 1, Outcome: "completed", Output: "first answer"},
		&adapter.Status{Active: false})
	// A second turn is running when a script writes the status by hand.
	st.OpenTurn(2, "second", 0)
	drain(ch)

	st.SetStatusAbandoningTurn(&adapter.Status{Active: false})

	frame := st.TurnFrameSnapshot()
	if frame.Current != nil {
		t.Fatalf("an idle session still advertises a running turn: %+v", frame.Current)
	}
	if frame.Last == nil || frame.Last.TurnSeq != 1 || frame.Last.Output != "first answer" {
		t.Fatalf("the raw close rewrote the last-closed record: %+v", frame.Last)
	}
	// Attribution is unchanged: the abandoned turn (2) has no close record at
	// all, so a waiter that observed it matches nothing and is served no answer —
	// rather than turn 1's, which is the failure this whole design prevents.
	if frame.Last.TurnSeq == 2 {
		t.Fatal("the raw close invented a close record for the abandoned turn")
	}
	// And the close reached the subscriber as one coupled event, like any edge.
	events := drain(ch)
	if len(events) != 1 || events[0].Type != "status" {
		t.Fatalf("raw close emitted %+v", events)
	}
	if f := edgeFrame(t, events[0]); f == nil || f.Current != nil {
		t.Fatalf("the raw close's event did not carry the abandoning frame: %+v", f)
	}

	// A raw write that keeps the session ACTIVE abandons nothing: the turn it
	// describes is still running.
	st.OpenTurn(3, "third", 0)
	drain(ch)
	st.SetStatusAbandoningTurn(&adapter.Status{Active: true, Error: true})
	if cur := st.TurnFrameSnapshot().Current; cur == nil || cur.TurnSeq != 3 {
		t.Fatalf("an active raw write abandoned a running turn: %+v", cur)
	}

	// A session that never asserted a turn (a shell, `PUT /status` only) keeps
	// the plain status shape — no frame is invented for it.
	plain := New(Config{ID: "s2", Adapter: "shell"})
	pch := plain.Subscribe()
	defer plain.Unsubscribe(pch)
	plain.SetStatusAbandoningTurn(&adapter.Status{Active: false})
	events = drain(pch)
	if len(events) != 1 || events[0].Type != "status" {
		t.Fatalf("shell status emitted %+v", events)
	}
	if f := edgeFrame(t, events[0]); f != nil {
		t.Fatalf("a frame was invented for a session that asserts no turns: %+v", f)
	}
	if plain.TurnFrameSnapshot() != nil {
		t.Fatalf("frame = %+v, want none", plain.TurnFrameSnapshot())
	}
}

// TestReplayUsesTheCoupledEdgeShape: a (re)connecting subscriber is replayed the
// status and the frame in ONE event, exactly as a live edge delivers them, from a
// single consistent snapshot. Two reads could straddle a turn edge, and the
// replay is where that costs a result: a wait armed in the reconnect window would
// bind turn_seq 0.
func TestReplayUsesTheCoupledEdgeShape(t *testing.T) {
	st := New(Config{ID: "s1", Adapter: "pi"})
	st.OpenTurn(5, "ask", 0)
	st.CloseTurnFrame(TurnClose{TurnSeq: 5, Outcome: "completed", Output: "the answer"},
		&adapter.Status{Active: false})

	typ, payload, ok := ReplayTurnEdge(st.TurnEdgeSnapshot())
	if !ok || typ != "status" {
		t.Fatalf("replay type=%q ok=%v, want a coupled status event", typ, ok)
	}
	frame := edgeFrame(t, Event{Type: typ, Data: payload})
	if frame == nil || frame.Last == nil || frame.Last.Output != "the answer" {
		t.Fatalf("the replayed status did not carry the frame: %+v", frame)
	}

	// A session with a frame but no status yet (the adapter asserted a turn
	// before any status was reported) sends the frame alone — there is no edge to
	// couple it to.
	noStatus := New(Config{ID: "s2", Adapter: "pi"})
	noStatus.CloseTurnFrame(TurnClose{TurnSeq: 1, Outcome: "completed", Output: "x"}, &adapter.Status{})
	if typ, _, ok := ReplayTurnEdge(noStatus.TurnEdgeSnapshot()); !ok || typ != "turn_frame" {
		t.Fatalf("frame-without-status replay type=%q ok=%v", typ, ok)
	}

	// Nothing asserted, nothing reported: nothing to replay.
	if _, _, ok := ReplayTurnEdge(New(Config{ID: "s3"}).TurnEdgeSnapshot()); ok {
		t.Fatal("a fresh session replayed a turn edge")
	}
}
