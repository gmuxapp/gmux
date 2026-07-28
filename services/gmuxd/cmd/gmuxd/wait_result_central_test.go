package main

// wait_result_central_test.go pins ADR 0027 §8/§11 on the daemon side: the
// universal wait reports the turn's CONCLUSION (not just idleness), carries the
// agent's answer only for a normal completion, and derives both through the
// same code the synchronous prompt path uses.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/packages/scrollback"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
)

// waitFixture runs handleWaitCentral against a real store with a real pi
// conversation on disk, so the result attached to a wait is produced by the
// production selector rather than a stub.
type waitFixture struct {
	store *centralstore.Store
	boot  *Bootstrap
	dir   string
	// frame is the turn frame gmuxd retains for this session's live generation:
	// the adapter's own assertion about its turns, which is the only thing a
	// wait may report as a result. nil (the default) models every session whose
	// adapter asserts nothing — a shell, Claude/Codex, a version-skewed runner —
	// whose closes are served result-free.
	frameMu sync.Mutex
	frame   *sessioncoord.TurnFrame
	// looks counts retained-frame reads, which is how a test orders itself
	// against the handler having observed the open turn.
	looks atomic.Int64
}

// setFrame installs the retained frame, as a runner's turn events would.
func (f *waitFixture) setFrame(frame *sessioncoord.TurnFrame) {
	f.frameMu.Lock()
	defer f.frameMu.Unlock()
	f.frame = frame
}

// openTurn/closeTurn are the two frame shapes every test needs.
func (f *waitFixture) openTurn(seq uint64, trigger string) {
	f.setFrame(&sessioncoord.TurnFrame{Seq: seq, Current: &sessioncoord.TurnCurrent{TurnSeq: seq, Trigger: trigger}})
}

func (f *waitFixture) closeTurn(seq uint64, outcome, output string, truncated bool) {
	f.setFrame(&sessioncoord.TurnFrame{Seq: seq + 100, Last: &sessioncoord.TurnClose{
		TurnSeq: seq, Outcome: outcome, Output: output, Truncated: truncated,
	}})
}

func newWaitFixture(t *testing.T) *waitFixture {
	t.Helper()
	st, err := centralstore.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	coord := sessioncoord.New(nil, &bootstrapRunners{metas: map[string]sessioncoord.RunnerMeta{}, blocked: map[string]bool{}}, st, nil, nil)
	t.Cleanup(coord.Close)
	f := &waitFixture{store: st, boot: &Bootstrap{Store: st, Coordinator: coord}, dir: t.TempDir()}
	prev := retainedTurnFrame
	retainedTurnFrame = func(boot *Bootstrap, id string) *sessioncoord.TurnFrame {
		f.looks.Add(1)
		f.frameMu.Lock()
		defer f.frameMu.Unlock()
		return f.frame
	}
	t.Cleanup(func() { retainedTurnFrame = prev })
	return f
}

// register inserts a session with the given adapter and, when lines are
// supplied, a pi JSONL conversation it points at.
func (f *waitFixture) register(t *testing.T, id, adapterName string, lines ...string) {
	t.Helper()
	reg := centralstore.RunnerRegistration{
		ID: centralstore.SessionID(id), Adapter: adapterName, Alive: true, CreatedAt: 1, ObservedAt: 1,
	}
	if len(lines) > 0 {
		path := filepath.Join(f.dir, id+".jsonl")
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		reg.Facts.ConversationRef = &path
	}
	if _, _, err := f.store.RegisterRunner(context.Background(), reg); err != nil {
		t.Fatal(err)
	}
}

// waitForLook blocks until the handler under test has read the retained frame at
// least once, i.e. has observed the open turn and recorded its identity. Polling
// a real signal beats sleeping: the close must land strictly AFTER that
// observation for the test to mean anything.
func waitForLook(t *testing.T, f *waitFixture) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f.looks.Load() > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("handler never looked at the turn frame")
}

func decodeWaitData(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return env.Data
}

// wait resolves a wait against the given visible session state and returns the
// decoded data object.
func (f *waitFixture) wait(t *testing.T, id, query string, visible wire.Session) map[string]any {
	t.Helper()
	url := "/wait"
	if query != "" {
		url += "?" + query
	}
	rec := httptest.NewRecorder()
	handleWaitCentral(rec, httptest.NewRequest(http.MethodPost, url, nil), f.boot, wf(visible), id,
		func(string) string { return f.dir })
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return env.Data
}

// The pi conversation used throughout: one user turn, a tool call, then prose.
var piResultLines = []string{
	piSessionHeader,
	`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"run the tests"}]}}`,
	`{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"toolCall","id":"t1","name":"bash","arguments":{"command":"go test ./..."}},{"type":"text","text":"All green."}]}}`,
}

// The conclusion matrix: every way a turn can close, through the GENERIC wait,
// on an agent session. This is the gap ADR 0027 §8/§11 identified — the old
// wait could only say idle/died.
func TestGenericWaitReportsTheTurnsConclusion(t *testing.T) {
	exit := 0
	for _, tc := range []struct {
		name          string
		visible       wire.Session
		outcome       string
		cause         string
		wantOutput    bool
		wantOutputStr string
	}{
		// A wait that finds the turn ALREADY CLOSED never identified a turn of
		// its own, so it reports the conclusion without claiming an answer.
		{"completed", wire.Session{ID: "s", Alive: true, Status: &wire.Status{Active: false}},
			"completed", "", false, ""},
		{"terminal error", wire.Session{ID: "s", Alive: true, Status: &wire.Status{Active: false, Error: true}},
			"error", "", false, ""},
		{"interrupted", wire.Session{ID: "s", Alive: true, Status: &wire.Status{Active: false, Interrupted: true}},
			"interrupted", "", false, ""},
		{"death mid turn", wire.Session{ID: "s", Alive: false, ExitCode: &exit, Status: &wire.Status{Active: true}},
			"error", causeRunnerDied, false, ""},
		{"death before any status", wire.Session{ID: "s", Alive: false, StartedAt: "x"},
			"error", causeRunnerDied, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWaitFixture(t)
			f.register(t, "s", "pi", piResultLines...)
			data := f.wait(t, "s", "timeout=1", tc.visible)
			if data["outcome"] != tc.outcome {
				t.Fatalf("outcome=%v want %q (data=%v)", data["outcome"], tc.outcome, data)
			}
			if tc.cause == "" {
				if _, ok := data["cause"]; ok {
					t.Fatalf("unexpected cause: %v", data)
				}
			} else if data["cause"] != tc.cause {
				t.Fatalf("cause=%v want %q", data["cause"], tc.cause)
			}
			got, present := data["output"]
			if present != tc.wantOutput {
				// The suppression rule is the point: a stale prior-turn answer
				// must never be attached to a failed/interrupted/dead turn.
				t.Fatalf("output present=%v want %v (data=%v)", present, tc.wantOutput, data)
			}
			if tc.wantOutput && got != tc.wantOutputStr {
				t.Fatalf("output=%q want %q", got, tc.wantOutputStr)
			}
		})
	}
}

// Active+error is an attention-worthy retry state, not a closed turn: the wait
// must not resolve, and must time out instead.
func TestGenericWaitDoesNotResolveOnActiveError(t *testing.T) {
	f := newWaitFixture(t)
	f.register(t, "s", "pi", piResultLines...)
	rec := httptest.NewRecorder()
	handleWaitCentral(rec, httptest.NewRequest(http.MethodPost, "/wait?timeout=1", nil), f.boot,
		wf(wire.Session{ID: "s", Alive: true, Status: &wire.Status{Active: true, Error: true}}), "s",
		func(string) string { return f.dir })
	if rec.Code != http.StatusRequestTimeout {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// A session that never reported a status keeps its pre-existing meaning: alive
// proves neither a turn nor its absence (so the wait blocks), while a death
// with run evidence is a death.
func TestGenericWaitStatuslessSessionKeepsItsOldMeaning(t *testing.T) {
	f := newWaitFixture(t)
	f.register(t, "s", "pi", piResultLines...)
	rec := httptest.NewRecorder()
	handleWaitCentral(rec, httptest.NewRequest(http.MethodPost, "/wait?timeout=1", nil), f.boot,
		wf(wire.Session{ID: "s", Alive: true}), "s", func(string) string { return f.dir })
	if rec.Code != http.StatusRequestTimeout {
		t.Fatalf("statusless live session resolved: %d %s", rec.Code, rec.Body.String())
	}
	data := f.wait(t, "s", "timeout=1", wire.Session{ID: "s", Alive: false, StartedAt: "x"})
	if data["reason"] != "died" || data["outcome"] != outcomeError {
		t.Fatalf("data=%v", data)
	}
}

// Shell/process sessions and non-renderer agents stay quiet but successful: the
// wait synchronizes exactly as before and simply has no result to attach.
func TestGenericWaitQuietForNonRendererSessions(t *testing.T) {
	for _, adapterName := range []string{"shell", "claude", "codex"} {
		t.Run(adapterName, func(t *testing.T) {
			f := newWaitFixture(t)
			// A conversation ref is registered on purpose: quietness must come
			// from the adapter having no readable conversation model, not from
			// an accidentally missing ref.
			f.register(t, "s", adapterName, piResultLines...)
			data := f.wait(t, "s", "timeout=1", wire.Session{ID: "s", Alive: true, Status: &wire.Status{Active: false}})
			if data["reason"] != "idle" || data["outcome"] != outcomeCompleted {
				t.Fatalf("data=%v", data)
			}
			if _, ok := data["output"]; ok {
				t.Fatalf("%s session carried a result: %v", adapterName, data)
			}
		})
	}
}

// Predicate waits stay synchronization-only: reason matched/died, no turn
// conclusion, no result. They answer a question about bytes on a terminal, not
// about a turn.
func TestPredicateWaitStaysSynchronizationOnly(t *testing.T) {
	f := newWaitFixture(t)
	f.register(t, "s", "pi", piResultLines...)
	if err := os.WriteFile(filepath.Join(f.dir, scrollback.ActiveName), []byte("BUILD DONE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	data := f.wait(t, "s", "for_text=BUILD+DONE&timeout=2",
		wire.Session{ID: "s", Alive: true, TerminalCols: 80, TerminalRows: 24, Status: &wire.Status{Active: false}})
	if data["reason"] != "matched" {
		t.Fatalf("data=%v", data)
	}
	for _, k := range []string{"outcome", "cause", "output"} {
		if _, ok := data[k]; ok {
			t.Fatalf("predicate wait carried %q: %v", k, data)
		}
	}
}

// A predicate wait on a session that exits without matching still reports died
// and no conclusion.
func TestPredicateWaitDeathHasNoConclusion(t *testing.T) {
	f := newWaitFixture(t)
	f.register(t, "s", "pi", piResultLines...)
	exit := 0
	data := f.wait(t, "s", "for_text=never&timeout=2",
		wire.Session{ID: "s", Alive: false, ExitCode: &exit, TerminalCols: 80, TerminalRows: 24})
	if data["reason"] != "died" {
		t.Fatalf("data=%v", data)
	}
	if _, ok := data["outcome"]; ok {
		t.Fatalf("predicate death carried an outcome: %v", data)
	}
}

// The snapshot selector behind `gmux agent output` (scope=message) survives the
// amendment untouched: it is explicitly a SNAPSHOT read of the tape, and it is
// the documented recourse for everything the source assertion deliberately does
// not serve (a late wait, a truncated answer's full text, a non-asserting
// adapter). What it must not do is leak a previous turn's prose when the current
// turn has produced none.
func TestSnapshotSelectorStillAnswersForOutput(t *testing.T) {
	f := newConversationFixture(t)
	f.addPiSession(t, "sess-1", piResultLines...)
	viaHandler, _ := io.ReadAll(f.do(http.MethodGet, "sess-1", "scope=message").Body)
	if got := strings.TrimRight(string(viaHandler), "\n"); got != "All green." {
		t.Fatalf("scope=message returned %q", got)
	}
	// Tool-only current turn: the previous turn's answer must NOT leak out.
	f.addPiSession(t, "tool-only",
		piSessionHeader,
		`{"type":"message","id":"a0","message":{"role":"assistant","content":[{"type":"text","text":"old answer"}]}}`,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"again"}]}}`,
		`{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"toolCall","id":"t1","name":"bash","arguments":{}}]}}`,
	)
	if code := f.do(http.MethodGet, "tool-only", "scope=message").StatusCode; code != http.StatusNotFound {
		t.Fatalf("tool-only turn answered %d; stale prose must not be served", code)
	}
}

// ── synchronous prompt ─────────────────────────────────────────────────────

// A completed synchronous prompt carries the answer; every other conclusion
// carries none and never even asks for one.
func TestSynchronousPromptCarriesTheResultOnlyOnCompletion(t *testing.T) {
	t.Run("completed", func(t *testing.T) {
		h := newAgentHarness(t, liveRow("s", false))
		get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modePrompt}))
		<-h.prompts
		h.nextTimer(t)
		h.openTurn(1, "hi")
		h.publish(statusOutcome("s", true, false, false))
		h.closeTurn(1, outcomeCompleted, "All green.")
		h.publish(statusOutcome("s", false, false, false))
		got := get()
		if got.data()["outcome"] != outcomeCompleted || got.data()["output"] != "All green." {
			t.Fatalf("body=%v", got.body)
		}
	})
	for _, tc := range []struct {
		name                 string
		errored, interrupted bool
		outcome              string
	}{
		{"error", true, false, outcomeError},
		{"interrupted", false, true, outcomeInterrupted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newAgentHarness(t, liveRow("s", false))
			get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modePrompt}))
			<-h.prompts
			h.nextTimer(t)
			h.openTurn(1, "hi")
			h.publish(statusOutcome("s", true, false, false))
			// The adapter's close for a non-completed turn carries no output at
			// all; a frame still holding a PRIOR turn's answer must not be
			// mined for one either.
			h.closeTurn(1, tc.outcome, "")
			h.publish(statusOutcome("s", false, tc.errored, tc.interrupted))
			got := get()
			if got.data()["outcome"] != tc.outcome {
				t.Fatalf("body=%v", got.body)
			}
			if _, ok := got.data()["output"]; ok {
				t.Fatalf("%s carried a result: %v", tc.name, got.body)
			}
		})
	}
	t.Run("a turn with nothing to say stays quiet", func(t *testing.T) {
		// The adapter asserted nothing: the field is omitted, never empty.
		h := newAgentHarness(t, liveRow("s", false))
		get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modePrompt}))
		<-h.prompts
		h.nextTimer(t)
		h.publish(statusOutcome("s", true, false, false))
		h.publish(statusOutcome("s", false, false, false))
		got := get()
		if got.data()["outcome"] != outcomeCompleted {
			t.Fatalf("body=%v", got.body)
		}
		if _, ok := got.data()["output"]; ok {
			t.Fatalf("unexpected output: %v", got.body)
		}
	})
	t.Run("detached prompt carries neither outcome nor result", func(t *testing.T) {
		h := newAgentHarness(t, liveRow("s", false))
		get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modePrompt, "wait": false}))
		<-h.prompts
		h.nextTimer(t)
		h.publish(statusOutcome("s", true, false, false))
		got := get()
		if got.code != http.StatusAccepted {
			t.Fatalf("code=%d body=%v", got.code, got.body)
		}
		for _, k := range []string{"outcome", "output"} {
			if _, ok := got.data()[k]; ok {
				t.Fatalf("detached response carried %q: %v", k, got.body)
			}
		}
	})
}

// Guard against the two paths drifting: the generic wait and the synchronous
// prompt must classify an identical set of terminal flags identically.
func TestWaitAndPromptShareOneClassification(t *testing.T) {
	for _, tc := range []struct {
		errored, interrupted bool
		want                 string
	}{
		{false, false, outcomeCompleted},
		{true, false, outcomeError},
		{false, true, outcomeInterrupted},
		{true, true, outcomeError},
	} {
		viaWait, done := terminalReason(compatSession{
			Alive: true, Status: &compatStatus{Error: tc.errored, Interrupted: tc.interrupted},
		}, false)
		if !done || viaWait.Outcome != tc.want {
			t.Fatalf("wait(%v,%v)=%+v", tc.errored, tc.interrupted, viaWait)
		}
		if got := classifyTurnClose(tc.errored, tc.interrupted); got != tc.want {
			t.Fatalf("classify(%v,%v)=%q", tc.errored, tc.interrupted, got)
		}
	}
	// And the prompt path routes through the same helper for a real schedule.
	h := newAgentHarness(t, liveRow("s", false))
	get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modePrompt}))
	<-h.prompts
	h.nextTimer(t)
	h.publish(statusOutcome("s", true, false, false))
	h.publish(statusOutcome("s", false, true, true))
	if got := get(); got.data()["outcome"] != outcomeError {
		t.Fatalf("error must win over interruption: %v", got.body)
	}
}

// ── turn identity: serving an answer only to the turn that closed ──────────

// The identity rule in isolation, on the frame itself: a result is served only
// when the settled record names the exact turn the waiter observed. Every other
// shape — an unknown turn, a different turn, no close at all — is result-free.
func TestTurnFrameServesOnlyTheMatchingTurn(t *testing.T) {
	frame := &sessioncoord.TurnFrame{Last: &sessioncoord.TurnClose{TurnSeq: 7, Outcome: outcomeCompleted, Output: "ours"}}
	for _, tc := range []struct {
		name     string
		frame    *sessioncoord.TurnFrame
		observed uint64
		want     string
	}{
		{"exact match", frame, 7, "ours"},
		// Two back-to-back turns between looks: the newer close is not ours, and
		// degrading to result-free is the whole point.
		{"a later turn's close is not ours", frame, 6, ""},
		{"an earlier turn's close is not ours", frame, 8, ""},
		// 0 is "we never identified a turn" — an adapter that asserts no
		// identity, a raw PUT /status close, a version-skewed runner.
		{"unknown observed turn", frame, 0, ""},
		{"no close yet", &sessioncoord.TurnFrame{Current: &sessioncoord.TurnCurrent{TurnSeq: 7}}, 7, ""},
		{"no frame at all", nil, 7, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.frame.ClosedTurn(tc.observed)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("served %+v, want result-free", got)
				}
				return
			}
			if got == nil || got.Output != tc.want {
				t.Fatalf("served %+v, want %q", got, tc.want)
			}
		})
	}
	if seq := frame.CurrentTurnSeq(); seq != 0 {
		t.Fatalf("a frame with no open turn reported seq %d", seq)
	}
	var nilFrame *sessioncoord.TurnFrame
	if seq := nilFrame.CurrentTurnSeq(); seq != 0 {
		t.Fatalf("nil frame reported seq %d", seq)
	}
}

// The generic wait, end to end: it records the running turn's identity when it
// first sees the turn open, and serves that turn's asserted answer at the close.
//
// Note which transport this exercises: the resolution arrives through the fanout
// snapshot, which carries no event and no payload at all. That path is exactly
// as result-bearing as the outcome path because the frame is RETAINED rather
// than ridden on an edge — the failure mode ("completed, exit 0, no answer") this
// design exists to kill.
func TestGenericWaitServesTheAssertedResult(t *testing.T) {
	f := newWaitFixture(t)
	f.register(t, "s", "pi", piResultLines...)
	f.openTurn(11, "run the tests")
	fan := wf(wire.Session{ID: "s", Alive: true, Status: &wire.Status{Active: true}})
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleWaitCentral(rec, httptest.NewRequest(http.MethodPost, "/wait?timeout=5", nil),
			f.boot, fan, "s", func(string) string { return f.dir })
	}()
	waitForLook(t, f)
	// The turn settles. A truncated flag rides along: the caller must be able to
	// tell a capped answer from a complete one.
	f.closeTurn(11, outcomeCompleted, "All green.", true)
	fan.BroadcastFrames(wire.Frames{Sessions: &wire.SessionsPayload{
		Sessions: []wire.Session{{ID: "s", Alive: true, Status: &wire.Status{Active: false}}},
	}})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not resolve")
	}
	data := decodeWaitData(t, rec)
	if data["outcome"] != outcomeCompleted || data["output"] != "All green." || data["truncated"] != true {
		t.Fatalf("data=%v", data)
	}
}

// A turn that closes while the frame's newest close names ANOTHER turn (a second
// turn started and settled between the waiter's looks) resolves result-free. The
// old reconstruction model reported the wrong turn's prose here; nothing is
// worse than a confidently wrong answer under exit 0.
func TestGenericWaitRefusesAnotherTurnsAnswer(t *testing.T) {
	f := newWaitFixture(t)
	f.register(t, "s", "pi", piResultLines...)
	f.openTurn(11, "ours")
	fan := wf(wire.Session{ID: "s", Alive: true, Status: &wire.Status{Active: true}})
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleWaitCentral(rec, httptest.NewRequest(http.MethodPost, "/wait?timeout=5", nil),
			f.boot, fan, "s", func(string) string { return f.dir })
	}()
	waitForLook(t, f)
	f.closeTurn(12, outcomeCompleted, "somebody else's answer", false)
	fan.BroadcastFrames(wire.Frames{Sessions: &wire.SessionsPayload{
		Sessions: []wire.Session{{ID: "s", Alive: true, Status: &wire.Status{Active: false}}},
	}})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not resolve")
	}
	data := decodeWaitData(t, rec)
	if data["outcome"] != outcomeCompleted {
		t.Fatalf("data=%v", data)
	}
	if got, ok := data["output"]; ok {
		t.Fatalf("another turn's answer was served: %v", got)
	}
}

// A generation that never asserted anything must still WAIT normally: shell
// sessions, hook-driven agents and version-skewed runners complete result-free
// rather than hanging on an invariant they cannot satisfy. This is the scope of
// the delivery invariant, stated as a test.
func TestFrameLessGenerationCompletesResultFree(t *testing.T) {
	f := newWaitFixture(t)
	f.register(t, "s", "pi", piResultLines...) // renderer adapter, but no frame ever sent
	fan := wf(wire.Session{ID: "s", Alive: true, Status: &wire.Status{Active: true}})
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleWaitCentral(rec, httptest.NewRequest(http.MethodPost, "/wait?timeout=5", nil),
			f.boot, fan, "s", func(string) string { return f.dir })
	}()
	waitForLook(t, f)
	fan.BroadcastFrames(wire.Frames{Sessions: &wire.SessionsPayload{
		Sessions: []wire.Session{{ID: "s", Alive: true, Status: &wire.Status{Active: false}}},
	}})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a frame-less close must resolve, not hang")
	}
	data := decodeWaitData(t, rec)
	if data["outcome"] != outcomeCompleted {
		t.Fatalf("data=%v", data)
	}
	if _, ok := data["output"]; ok {
		t.Fatalf("a frame-less close carried a result: %v", data)
	}
}

// A tool-only turn asserts `completed` with NO output, and that is reported as
// an absent field rather than an empty one: absence means "the turn produced no
// prose", never "the transport lost it".
func TestToolOnlyTurnCompletesWithoutOutput(t *testing.T) {
	f := newWaitFixture(t)
	f.register(t, "s", "pi", piResultLines...)
	f.openTurn(3, "do a thing")
	fan := wf(wire.Session{ID: "s", Alive: true, Status: &wire.Status{Active: true}})
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleWaitCentral(rec, httptest.NewRequest(http.MethodPost, "/wait?timeout=5", nil),
			f.boot, fan, "s", func(string) string { return f.dir })
	}()
	waitForLook(t, f)
	f.closeTurn(3, outcomeCompleted, "", false)
	fan.BroadcastFrames(wire.Frames{Sessions: &wire.SessionsPayload{
		Sessions: []wire.Session{{ID: "s", Alive: true, Status: &wire.Status{Active: false}}},
	}})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not resolve")
	}
	data := decodeWaitData(t, rec)
	if data["outcome"] != outcomeCompleted {
		t.Fatalf("data=%v", data)
	}
	if got, ok := data["output"]; ok {
		t.Fatalf("tool-only turn carried %q", got)
	}
}

// The prompt path resolves from the frame stamped on the OUTCOME that declares
// the close — the other carrier, and the one that matters when the retained frame
// has already moved on to a newer turn.
func TestPromptResolvesFromTheOutcomeCarriedFrame(t *testing.T) {
	h := newAgentHarness(t, liveRow("s", false))
	get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modePrompt}))
	<-h.prompts
	h.nextTimer(t)
	open := statusOutcome("s", true, false, false)
	open.Frame = &sessioncoord.TurnFrame{Current: &sessioncoord.TurnCurrent{TurnSeq: 21, Trigger: "hi"}}
	h.publish(open)
	// By the time the close is delivered the runner has moved on: the RETAINED
	// frame describes a newer turn, and only the frame stamped on this outcome
	// can attribute the answer correctly.
	h.closeTurn(22, outcomeCompleted, "a newer turn's answer")
	closed := statusOutcome("s", false, false, false)
	closed.Frame = &sessioncoord.TurnFrame{Last: &sessioncoord.TurnClose{
		TurnSeq: 21, Outcome: outcomeCompleted, Output: "our answer",
	}}
	h.publish(closed)
	got := get()
	if got.data()["output"] != "our answer" {
		t.Fatalf("output=%v (body=%v)", got.data()["output"], got.body)
	}
}

// The acceptance test of the whole amendment, at the daemon boundary: the FIRST
// synchronous prompt of a fresh session returns its answer. Under the watermark
// model this case failed by construction — the window was marked before pi had
// even created the conversation file — and it is the reason attribution moved to
// the source. Note there is no conversation on disk at all here: the answer can
// only come from the adapter's assertion.
func TestFirstPromptOfAFreshSessionReturnsItsAnswer(t *testing.T) {
	h := newAgentHarness(t, liveRow("s", false))
	get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "what is 2+2?", "mode": modePrompt}))
	<-h.prompts
	h.nextTimer(t)
	h.openTurn(1, "what is 2+2?")
	h.publish(statusOutcome("s", true, false, false))
	h.closeTurn(1, outcomeCompleted, "4")
	h.publish(statusOutcome("s", false, false, false))
	got := get()
	if got.data()["outcome"] != outcomeCompleted || got.data()["output"] != "4" {
		t.Fatalf("the first prompt of a fresh session lost its answer: %v", got.body)
	}
}

// A steer joins a turn that is already open, so its identity is knowable before
// delivery: the steer may claim the merged close of THAT turn, and nothing else.
func TestSteerClaimsTheTurnItJoined(t *testing.T) {
	h := newAgentHarness(t, liveRow("s", true))
	h.openTurn(5, "go")
	get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "no, this way", "mode": modeSteer}))
	<-h.prompts
	h.closeTurn(5, outcomeCompleted, "post-steer answer")
	h.publish(statusOutcome("s", false, false, false))
	got := get()
	if got.data()["output"] != "post-steer answer" {
		t.Fatalf("output=%v (body=%v)", got.data()["output"], got.body)
	}
}

// A plain prompt binds at the turn-start EDGE, never to whatever is running when
// it is admitted. The seed can lag the runner (it still reads active here) while
// the runner admits a plain prompt only against authoritative idle, so binding on
// that stale bit would hand this prompt the PREVIOUS turn's answer under exit 0.
func TestPlainPromptIgnoresAStaleActiveSeed(t *testing.T) {
	h := newAgentHarness(t, liveRow("s", false))
	h.seedStatus("s", statusOutcome("s", true, false, false))
	h.openTurn(1, "old ask") // the previous turn, still described as running
	get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modePrompt}))
	<-h.prompts
	h.nextTimer(t)
	// The lagging close lands, then our turn genuinely opens.
	h.closeTurn(1, outcomeCompleted, "old answer")
	h.publish(statusOutcome("s", false, false, false))
	h.openTurn(2, "hi")
	h.publish(statusOutcome("s", true, false, false))
	// Ours produces nothing sayable (a tool-only turn).
	h.closeTurn(2, outcomeCompleted, "")
	h.publish(statusOutcome("s", false, false, false))
	got := get()
	if got.data()["outcome"] != outcomeCompleted {
		t.Fatalf("body=%v", got.body)
	}
	if out, ok := got.data()["output"]; ok {
		t.Fatalf("the previous turn's answer was reported as this prompt's result: %v", out)
	}
}

// A wait that arrives after the close never observed a turn of its own, so it
// resolves result-free even though a settled frame is sitting right there: the
// frame serves waits that observed their close through it, and nothing else.
// `gmux agent output` is the snapshot read for this case, and says so.
func TestAlreadyClosedTurnIsResultFree(t *testing.T) {
	f := newWaitFixture(t)
	f.register(t, "s", "pi", piResultLines...)
	f.closeTurn(4, outcomeCompleted, "All green.", false)
	data := f.wait(t, "s", "timeout=1", wire.Session{ID: "s", Alive: true, Status: &wire.Status{Active: false}})
	if data["outcome"] != outcomeCompleted {
		t.Fatalf("data=%v", data)
	}
	if got, ok := data["output"]; ok {
		t.Fatalf("a late wait claimed a turn it never observed: %v", got)
	}
}

// conversation_readable travels with every turn conclusion so the CLI can point
// a failed shell at `gmux tail` instead of at `gmux agent output`, which 404s
// for it.
func TestWaitReportsWhetherAConversationIsReadable(t *testing.T) {
	for _, tc := range []struct {
		adapterName string
		withRef     bool
		want        bool
	}{
		{"pi", true, true},
		{"pi", false, false}, // renderer, but nothing recorded yet
		{"shell", true, false},
		{"claude", true, false},
	} {
		t.Run(tc.adapterName+"/ref="+strconv.FormatBool(tc.withRef), func(t *testing.T) {
			f := newWaitFixture(t)
			if tc.withRef {
				f.register(t, "s", tc.adapterName, piResultLines...)
			} else {
				f.register(t, "s", tc.adapterName)
			}
			exit := 1
			data := f.wait(t, "s", "timeout=1", wire.Session{
				ID: "s", Alive: false, ExitCode: &exit, Status: &wire.Status{Active: false, Error: true},
			})
			if data["outcome"] != outcomeError {
				t.Fatalf("data=%v", data)
			}
			if data["conversation_readable"] != tc.want {
				t.Fatalf("conversation_readable=%v want %v", data["conversation_readable"], tc.want)
			}
		})
	}
	// Predicate waits carry no conclusion, so they carry no readability claim.
	f := newWaitFixture(t)
	f.register(t, "s", "pi", piResultLines...)
	if err := os.WriteFile(filepath.Join(f.dir, scrollback.ActiveName), []byte("MATCH\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	data := f.wait(t, "s", "for_text=MATCH&timeout=2",
		wire.Session{ID: "s", Alive: true, TerminalCols: 80, TerminalRows: 24, Status: &wire.Status{Active: false}})
	if _, ok := data["conversation_readable"]; ok {
		t.Fatalf("predicate wait claimed readability: %v", data)
	}
}

// A failed one-shot / mark-less shell closes its lifetime turn with Error=true
// (run.go's finalizeSessionState), so its wait is deliberately nonzero. This is
// the row that used to be missing from the table.
func TestFailedOneShotIsAnErrorConclusion(t *testing.T) {
	exit := 2
	verdict, done := terminalReason(compatSession{ExitCode: &exit, Status: &compatStatus{Error: true}}, false)
	if !done || verdict.Reason != "idle" || verdict.Outcome != outcomeError || verdict.Cause != "" {
		t.Fatalf("got %+v,%v", verdict, done)
	}
	// A zero-exit one-shot still completes.
	ok := 0
	verdict, done = terminalReason(compatSession{ExitCode: &ok, Status: &compatStatus{}}, false)
	if !done || verdict.Outcome != outcomeCompleted {
		t.Fatalf("got %+v,%v", verdict, done)
	}
}

// A wait that attaches MID-TURN identifies that turn from the frame's current
// record on its very first look, so it is served that turn's answer when it
// settles. This is the case the watermark model had to special-case with a
// second bind kind; identity makes it fall out.
func TestMidTurnWaitClaimsTheRunningTurn(t *testing.T) {
	f := newWaitFixture(t)
	f.register(t, "s", "pi", piResultLines...)
	f.openTurn(8, "ask") // already running when the wait attaches
	fan := wf(wire.Session{ID: "s", Alive: true, Status: &wire.Status{Active: true}})
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleWaitCentral(rec, httptest.NewRequest(http.MethodPost, "/wait?timeout=5", nil),
			f.boot, fan, "s", func(string) string { return f.dir })
	}()
	waitForLook(t, f)
	f.closeTurn(8, outcomeCompleted, "the answer", false)
	fan.BroadcastFrames(wire.Frames{Sessions: &wire.SessionsPayload{
		Sessions: []wire.Session{{ID: "s", Alive: true, Status: &wire.Status{Active: false}}},
	}})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not resolve")
	}
	data := decodeWaitData(t, rec)
	if data["outcome"] != outcomeCompleted || data["output"] != "the answer" {
		t.Fatalf("data=%v", data)
	}
}

// Two waits that make no claim about an agent turn stay result-free by contract,
// however good a frame is sitting in the registry: a detached prompt (its caller
// is never shown a result) and raw `send --wait` (keystrokes make no claim about
// which turn they belong to).
func TestResultFreeByContract(t *testing.T) {
	t.Run("detached prompt", func(t *testing.T) {
		h := newAgentHarness(t, liveRow("s", false))
		get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{
			"prompt": "hi", "mode": modePrompt, "wait": false,
		}))
		<-h.prompts
		h.nextTimer(t)
		h.openTurn(1, "hi")
		h.publish(statusOutcome("s", true, false, false))
		got := get()
		if got.code != http.StatusAccepted {
			t.Fatalf("code=%d body=%v", got.code, got.body)
		}
		for _, k := range []string{"outcome", "output"} {
			if _, ok := got.data()[k]; ok {
				t.Fatalf("detached response carried %q: %v", k, got.body)
			}
		}
	})
	t.Run("raw send --wait", func(t *testing.T) {
		f := newWaitFixture(t)
		f.register(t, "s", "pi", piResultLines...)
		f.openTurn(4, "typed by hand")
		fan := wf(wire.Session{ID: "s", Alive: true, Status: &wire.Status{Active: true}})
		rec := httptest.NewRecorder()
		done := make(chan struct{})
		delivered := make(chan struct{})
		go func() {
			defer close(done)
			handleInputWaitCentral(rec, httptest.NewRequest(http.MethodPost, "/input?wait=idle&timeout=5", nil),
				f.boot, fan, "s", []byte("go\r"), func() error { close(delivered); return nil })
		}()
		// Deliver first: the handler subscribes before sending, so a close
		// published before delivery would be the stale pre-delivery state the
		// fused path exists to skip.
		<-delivered
		f.closeTurn(4, outcomeCompleted, "an answer nobody may attribute to keystrokes", false)
		fan.BroadcastFrames(wire.Frames{Sessions: &wire.SessionsPayload{
			Sessions: []wire.Session{{ID: "s", Alive: true, Status: &wire.Status{Active: false}}},
		}})
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("input wait did not resolve")
		}
		data := decodeWaitData(t, rec)
		if data["outcome"] != outcomeCompleted {
			t.Fatalf("data=%v", data)
		}
		if _, ok := data["output"]; ok {
			t.Fatalf("raw send --wait carried a result: %v", data)
		}
	})
}
