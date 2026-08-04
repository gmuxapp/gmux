package ptyserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gmuxapp/gmux/cli/gmux/internal/session"
)

// TestTurnFrameAssertedFacts pins the whole source-asserted turn record as the
// runner holds it: identity, trigger, injections while running, and the settled
// result moving from the current record to the last-closed one.
func TestTurnFrameAssertedFacts(t *testing.T) {
	st := session.New(session.Config{ID: "s1", Adapter: "pi"})
	srv := &Server{state: st}

	postHook(t, srv, []byte(`{"op":"turn","phase":"start","turn_seq":7,"trigger":"what is 2+2?"}`))
	f := st.TurnFrameSnapshot()
	if f == nil || f.Current == nil || f.Current.TurnSeq != 7 || len(f.Current.Exchanges) != 1 || f.Current.Exchanges[0].User != "what is 2+2?" {
		t.Fatalf("after start: %+v", f)
	}
	if f.Last != nil {
		t.Fatalf("a fresh runner must have no closed turn: %+v", f.Last)
	}
	if s := st.StatusSnapshot(); s == nil || !s.Active {
		t.Fatalf("start must open the turn: %+v", s)
	}

	postHook(t, srv, []byte(`{"op":"turn","phase":"steered","turn_seq":7,"text":"actually, stop"}`))
	f = st.TurnFrameSnapshot()
	if f.Current == nil || len(f.Current.Exchanges) != 2 || f.Current.Exchanges[1].User != "actually, stop" {
		t.Fatalf("after steer: %+v", f.Current)
	}

	postHook(t, srv, []byte(`{"op":"turn","phase":"end","turn_seq":7,"outcome":"completed","output":"4"}`))
	f = st.TurnFrameSnapshot()
	if f.Current != nil {
		t.Fatalf("a closed turn must leave no current record: %+v", f.Current)
	}
	if f.Last == nil || f.Last.TurnSeq != 7 || f.Last.Outcome != "completed" || f.Last.Output != "4" {
		t.Fatalf("after close: %+v", f.Last)
	}
	// The closed record carries the turn's inputs so a report can name what the
	// turn was asked to do without a second lookup.
	if len(f.Last.Exchanges) != 2 || f.Last.Exchanges[0].User != "what is 2+2?" {
		t.Fatalf("close lost the turn's inputs: %+v", f.Last)
	}
	if s := st.StatusSnapshot(); s == nil || s.Active {
		t.Fatalf("close must close the turn: %+v", s)
	}
}

// TestTurnFrameStaleInjectionIgnored: an injection reported against a turn that
// is not the open one cannot be attached to it. Pairing facts by turn_seq is the
// whole mechanism that stops one turn's steer from describing another's answer.
func TestTurnFrameStaleInjectionIgnored(t *testing.T) {
	st := session.New(session.Config{ID: "s1", Adapter: "pi"})
	srv := &Server{state: st}
	postHook(t, srv, []byte(`{"op":"turn","phase":"start","turn_seq":2}`))
	postHook(t, srv, []byte(`{"op":"turn","phase":"steered","turn_seq":1,"text":"late"}`))
	if exchanges := st.TurnFrameSnapshot().Current.Exchanges; len(exchanges) != 1 {
		t.Fatalf("stale boundary landed: %v", exchanges)
	}
}

// TestTurnFrameSeqAdvances: every published frame carries a higher sequence, so
// a consumer can tell a replayed frame from a stale one without comparing
// contents.
func TestTurnFrameSeqAdvances(t *testing.T) {
	st := session.New(session.Config{ID: "s1", Adapter: "pi"})
	srv := &Server{state: st}
	postHook(t, srv, []byte(`{"op":"turn","phase":"start","turn_seq":1}`))
	first := st.TurnFrameSnapshot().Seq
	postHook(t, srv, []byte(`{"op":"turn","phase":"end","turn_seq":1,"outcome":"completed","output":"hi"}`))
	if second := st.TurnFrameSnapshot().Seq; second <= first {
		t.Fatalf("frame seq did not advance: %d → %d", first, second)
	}
}

// TestTurnFrameClearedOnRebind: the frame is conversation-local. A pi
// switch/new/resume/fork makes the previous conversation's answer
// unattributable under the new ref, so it is dropped rather than left to be
// replayed to a late subscriber as the new conversation's result.
func TestTurnFrameClearedOnRebind(t *testing.T) {
	st := session.New(session.Config{ID: "s1", Adapter: "pi"})
	srv := &Server{state: st}
	postHook(t, srv, []byte(`{"op":"session","path":"/tmp/a.jsonl"}`))
	postHook(t, srv, []byte(`{"op":"turn","phase":"start","turn_seq":1}`))
	postHook(t, srv, []byte(`{"op":"turn","phase":"end","turn_seq":1,"outcome":"error","output":"old answer","diagnostic":"A failed"}`))
	if status := st.StatusSnapshot(); status == nil || !status.Error {
		t.Fatalf("A failure was not retained before rebind: %+v", status)
	}

	// Same conversation, re-reported (claude/codex do this on every turn end):
	// nothing is invalidated.
	postHook(t, srv, []byte(`{"op":"session","path":"/tmp/a.jsonl"}`))
	if f := st.TurnFrameSnapshot(); f.Last == nil || f.Last.Output != "old answer" {
		t.Fatalf("a same-conversation refresh cleared the frame: %+v", f)
	}

	postHook(t, srv, []byte(`{"op":"session","path":"/tmp/b.jsonl"}`))
	f := st.TurnFrameSnapshot()
	if f.Current != nil || f.Last != nil {
		t.Fatalf("rebind must clear both records: %+v", f)
	}
	if status := st.StatusSnapshot(); status != nil {
		t.Fatalf("B inherited A outcome status: %+v", status)
	}
}

// TestRebindClearsTheFrameBeforePublishingTheNewConversation pins the ORDER, not
// just the fact, of the rebind clear. The clear must reach a live subscriber
// BEFORE the new conversation ref: a subscriber that learned the new ref while
// the old frame was still current could attribute the previous conversation's
// answer to the new one.
//
// Position, not presence, is what this kills: TestTurnFrameClearedOnRebind fails
// if the clear is removed, but both survive if the clear moves after
// SetConversationRef — which is exactly the mutation this test rejects.
func TestRebindClearsTheFrameBeforePublishingTheNewConversation(t *testing.T) {
	st := session.New(session.Config{ID: "s1", Adapter: "pi"})
	srv := &Server{state: st}
	postHook(t, srv, []byte(`{"op":"session","path":"/tmp/a.jsonl","slug":"old chat"}`))
	postHook(t, srv, []byte(`{"op":"turn","phase":"end","turn_seq":1,"outcome":"completed","output":"old answer"}`))

	ch := st.Subscribe()
	defer st.Unsubscribe(ch)
	postHook(t, srv, []byte(`{"op":"session","path":"/tmp/b.jsonl"}`))

	cleared, ref := -1, -1
	for i, ev := range drainEvents(ch) {
		switch ev.Type {
		case "turn_frame":
			frame, ok := ev.Data.(*session.TurnFrame)
			if ok && cleared < 0 && frame.Current == nil && frame.Last == nil {
				cleared = i
			}
		case "conversation_file":
			if ref < 0 {
				ref = i
			}
		}
	}
	if cleared < 0 {
		t.Fatal("the rebind published no cleared frame")
	}
	if ref < 0 {
		t.Fatal("the rebind published no conversation ref")
	}
	if cleared > ref {
		t.Fatalf("the new conversation ref (%d) was published before the frame clear (%d)", ref, cleared)
	}
}

// drainEvents reads everything currently queued on a subscription.
func drainEvents(ch chan session.Event) []session.Event {
	var out []session.Event
	for {
		select {
		case e := <-ch:
			out = append(out, e)
		default:
			return out
		}
	}
}

// TestTurnFrameReplayedToSubscribers is the transport guarantee: the frame
// reaches a (re)connecting /events subscriber as a snapshot, so a wait armed
// after a daemon restart still learns what the last turn answered. Ordering
// matters too — the frame precedes the conversation ref it belongs to.
func TestTurnFrameReplayedToSubscribers(t *testing.T) {
	st := session.New(session.Config{ID: "s1", Adapter: "pi"})
	srv := &Server{state: st}
	postHook(t, srv, []byte(`{"op":"session","path":"/tmp/a.jsonl"}`))
	postHook(t, srv, []byte(`{"op":"turn","phase":"end","turn_seq":3,"outcome":"completed","output":"42"}`))

	req := httptest.NewRequest("GET", "http://unix/events", nil)
	ctx, cancel := contextWithImmediateCancel(req)
	defer cancel()
	rec := httptest.NewRecorder()
	srv.handleEvents(rec, req.WithContext(ctx))

	body := rec.Body.String()
	frameIdx := strings.Index(body, "event: turn_frame")
	refIdx := strings.Index(body, "event: conversation_file")
	if frameIdx < 0 {
		t.Fatalf("no frame replayed:\n%s", body)
	}
	if refIdx >= 0 && frameIdx > refIdx {
		t.Errorf("frame replayed after the conversation ref:\n%s", body)
	}
	var frame session.TurnFrame
	if err := json.Unmarshal([]byte(sseData(t, body, "turn_frame")), &frame); err != nil {
		t.Fatalf("decode replayed frame: %v", err)
	}
	if frame.Last == nil || frame.Last.TurnSeq != 3 || frame.Last.Output != "42" {
		t.Errorf("replayed frame = %+v", frame.Last)
	}
}

func TestExitMetadataReplayedToSubscribers(t *testing.T) {
	st := session.New(session.Config{ID: "s1", Adapter: "shell"})
	st.SetExited(23)
	srv := &Server{state: st}
	req := httptest.NewRequest("GET", "http://unix/events", nil)
	ctx, cancel := contextWithImmediateCancel(req)
	defer cancel()
	rec := httptest.NewRecorder()
	srv.handleEvents(rec, req.WithContext(ctx))

	var replay struct {
		ExitCode int    `json:"exit_code"`
		ExitedAt string `json:"exited_at"`
	}
	if err := json.Unmarshal([]byte(sseData(t, rec.Body.String(), "exit")), &replay); err != nil {
		t.Fatalf("decode replayed exit: %v", err)
	}
	if replay.ExitCode != 23 || replay.ExitedAt == "" {
		t.Fatalf("replayed exit = %+v", replay)
	}
}

// contextWithImmediateCancel gives handleEvents a context that is already done,
// so it writes its replay snapshot and returns instead of streaming forever.
func contextWithImmediateCancel(req *http.Request) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	return ctx, cancel
}

// sseData extracts the data payload of the first SSE frame of the named type.
func sseData(t *testing.T, body, typ string) string {
	t.Helper()
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		if l == "event: "+typ && i+1 < len(lines) {
			return strings.TrimPrefix(lines[i+1], "data: ")
		}
	}
	t.Fatalf("no %s frame in:\n%s", typ, body)
	return ""
}

// TestReplayedCloseCarriesItsFrameOverSSE is the wire half of the replay
// coupling: a subscriber connecting after a turn closed gets ONE status frame on
// the wire carrying the settled result, so the daemon parses the same shape live
// and replayed, and a wait armed in a reconnect window can still attribute its
// turn.
func TestReplayedCloseCarriesItsFrameOverSSE(t *testing.T) {
	st := session.New(session.Config{ID: "s1", Adapter: "pi"})
	srv := &Server{state: st}
	postHook(t, srv, []byte(`{"op":"session","path":"/tmp/a.jsonl"}`))
	postHook(t, srv, []byte(`{"op":"turn","phase":"start","turn_seq":6,"trigger":"ask"}`))
	postHook(t, srv, []byte(`{"op":"turn","phase":"end","turn_seq":6,"outcome":"completed","output":"42"}`))

	req := httptest.NewRequest("GET", "http://unix/events", nil)
	ctx, cancel := contextWithImmediateCancel(req)
	defer cancel()
	rec := httptest.NewRecorder()
	srv.handleEvents(rec, req.WithContext(ctx))
	body := rec.Body.String()

	// One status event, carrying the frame — not a status followed by a separate
	// frame, which is what a torn replay looked like.
	if n := strings.Count(body, "event: status"); n != 1 {
		t.Fatalf("replay wrote %d status events:\n%s", n, body)
	}
	if strings.Contains(body, "event: turn_frame") {
		t.Fatalf("replay still writes a separate frame event:\n%s", body)
	}
	var edge struct {
		Active bool `json:"active"`
		Frame  *struct {
			Last *session.TurnClose `json:"last"`
		} `json:"turn_frame"`
	}
	if err := json.Unmarshal([]byte(sseData(t, body, "status")), &edge); err != nil {
		t.Fatalf("decode replayed edge: %v", err)
	}
	if edge.Active {
		t.Fatalf("replayed a closed turn as active:\n%s", body)
	}
	if edge.Frame == nil || edge.Frame.Last == nil || edge.Frame.Last.Output != "42" {
		t.Fatalf("the replayed status did not carry the settled result:\n%s", body)
	}
	// And it still precedes the conversation ref.
	if si, ci := strings.Index(body, "event: status"), strings.Index(body, "event: conversation_file"); ci >= 0 && si > ci {
		t.Errorf("the conversation ref was replayed before the turn state:\n%s", body)
	}
}

// TestRawStatusCloseLeavesNoRunningTurnInTheFrame is F7 end to end through the
// runner's route: a script closing a pi turn with `PUT /status` must not leave the
// frame advertising a turn that has ended. NoteInjection gates on that record, and
// the report verbs read its trigger — a stale `current` grows teeth later.
func TestRawStatusCloseLeavesNoRunningTurnInTheFrame(t *testing.T) {
	st := session.New(session.Config{ID: "s1", Adapter: "pi"})
	srv := &Server{state: st}
	postHook(t, srv, []byte(`{"op":"turn","phase":"start","turn_seq":4,"trigger":"ask"}`))

	rec := httptest.NewRecorder()
	srv.handlePutStatus(rec, httptest.NewRequest("PUT", "http://unix/status",
		strings.NewReader(`{"active":false}`)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("PUT /status → %d", rec.Code)
	}
	frame := st.TurnFrameSnapshot()
	if frame == nil || frame.Current != nil {
		t.Fatalf("an idle session still advertises a running turn: %+v", frame)
	}
	if frame.Last != nil {
		t.Fatalf("the raw close invented an asserted result: %+v", frame.Last)
	}
	// A later injection cannot attach to the abandoned turn.
	postHook(t, srv, []byte(`{"op":"turn","phase":"steered","turn_seq":4,"text":"too late"}`))
	if cur := st.TurnFrameSnapshot().Current; cur != nil {
		t.Fatalf("an injection resurrected the abandoned turn: %+v", cur)
	}
}
