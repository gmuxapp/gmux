package main

// wait_result_central_test.go pins ADR 0027 §8/§11 on the daemon side: the
// universal wait reports the turn's CONCLUSION (not just idleness), carries the
// agent's answer only for a normal completion, and derives both through the
// same code the synchronous prompt path uses.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter"
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
	// marks counts result-window captures made by the handler under test, so a
	// test can order its appends strictly after the mark.
	marks atomic.Int64
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
	prev := markWaitWindow
	markWaitWindow = func(ctx context.Context, store *centralstore.Store, id string, turnInProgress bool) resultWindow {
		w := prev(ctx, store, id, turnInProgress)
		f.marks.Add(1)
		return w
	}
	t.Cleanup(func() { markWaitWindow = prev })
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

// registerWithConversation registers a session pointing at a conversation file
// the test can keep appending to, and returns the path.
func (f *waitFixture) registerWithConversation(t *testing.T, id, adapterName string, lines ...string) string {
	t.Helper()
	path := filepath.Join(f.dir, id+".jsonl")
	writeLines(t, path, lines...)
	ref := path
	if _, _, err := f.store.RegisterRunner(context.Background(), centralstore.RunnerRegistration{
		ID: centralstore.SessionID(id), Adapter: adapterName, Alive: true, CreatedAt: 1, ObservedAt: 1,
		Facts: centralstore.RunnerFacts{ConversationRef: &ref},
	}); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	for _, l := range lines {
		if _, err := fh.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

// waitForMark blocks until the handler under test has marked its result window,
// which is observable as the conversation having been rendered at least once.
// Polling a real signal beats sleeping: the interfering append must land
// strictly AFTER the mark for the test to mean anything.
func waitForMark(t *testing.T, f *waitFixture) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f.marks.Load() > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("handler never marked a result window")
}

// waitForMarks blocks until the agent harness has captured n result windows
// (one pre-delivery plus one per observed turn-start edge).
func waitForMarks(t *testing.T, h *agentHarness, n int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.markCalls.Load() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("only %d result windows were marked, wanted %d", h.markCalls.Load(), n)
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
		{"completed", wire.Session{ID: "s", Alive: true, Status: &wire.Status{Active: false}},
			"completed", "", true, "All green."},
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

// latestAgentMessageIn under an unbound (snapshot) window is the SAME selector scope=message serves, and every
// "nothing to show" shape collapses to "" so a result-bearing wait stays quiet
// instead of failing a legitimate wait.
func TestLatestAgentMessageIsTheSharedSelector(t *testing.T) {
	f := newConversationFixture(t)
	f.addPiSession(t, "sess-1", piResultLines...)
	viaHandler, _ := io.ReadAll(f.do(http.MethodGet, "sess-1", "scope=message").Body)
	direct := latestAgentMessageIn(context.Background(), f.sessions, "sess-1", snapshotWindow())
	if strings.TrimRight(string(viaHandler), "\n") != direct || direct != "All green." {
		t.Fatalf("selector drift: handler=%q direct=%q", viaHandler, direct)
	}

	// Tool-only current turn: the previous turn's answer must NOT leak out.
	f.addPiSession(t, "tool-only",
		piSessionHeader,
		`{"type":"message","id":"a0","message":{"role":"assistant","content":[{"type":"text","text":"old answer"}]}}`,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"again"}]}}`,
		`{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"toolCall","id":"t1","name":"bash","arguments":{}}]}}`,
	)
	if got := latestAgentMessageIn(context.Background(), f.sessions, "tool-only", snapshotWindow()); got != "" {
		t.Fatalf("stale prose leaked: %q", got)
	}
	f.addSession(t, "shellish", "shell", "/tmp/whatever.jsonl")
	f.addSession(t, "noref", "pi", "")
	f.addSession(t, "badref", "pi", filepath.Join(t.TempDir(), "missing.jsonl"))
	for _, id := range []string{"shellish", "noref", "badref", "no-such-session"} {
		if got := latestAgentMessageIn(context.Background(), f.sessions, id, snapshotWindow()); got != "" {
			t.Fatalf("%s: want quiet, got %q", id, got)
		}
	}
	if got := latestAgentMessageIn(context.Background(), nil, "sess-1", snapshotWindow()); got != "" {
		t.Fatalf("nil store: %q", got)
	}
}

// ── synchronous prompt ─────────────────────────────────────────────────────

// A completed synchronous prompt carries the answer; every other conclusion
// carries none and never even asks for one.
func TestSynchronousPromptCarriesTheResultOnlyOnCompletion(t *testing.T) {
	t.Run("completed", func(t *testing.T) {
		h := newAgentHarness(t, liveRow("s", false))
		h.resultText = "All green."
		get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modePrompt}))
		<-h.prompts
		h.nextTimer(t)
		h.publish(statusOutcome("s", true, false, false))
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
			h.resultText = "stale prior answer"
			get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modePrompt}))
			<-h.prompts
			h.nextTimer(t)
			h.publish(statusOutcome("s", true, false, false))
			h.publish(statusOutcome("s", false, tc.errored, tc.interrupted))
			got := get()
			if got.data()["outcome"] != tc.outcome {
				t.Fatalf("body=%v", got.body)
			}
			if _, ok := got.data()["output"]; ok {
				t.Fatalf("%s carried a result: %v", tc.name, got.body)
			}
			if n := h.resultCalls.Load(); n != 0 {
				t.Fatalf("result was selected %d times for a non-completed turn", n)
			}
		})
	}
	t.Run("nothing to render stays quiet", func(t *testing.T) {
		// No conversation on disk: the field is omitted, never empty.
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
		h.resultText = "All green."
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

// ── result windows: binding an answer to the turn that closed ──────────────

// The selector's two edges, in isolation. Every row is a way the unbounded
// "newest prose in the current turn" selector gets attribution wrong.
func TestAssistantProseInWindowBindsToOneTurn(t *testing.T) {
	msg := func(role, prose string) adapter.ConversationMessage {
		return adapter.ConversationMessage{Role: role, Text: prose, Prose: prose}
	}
	tool := func() adapter.ConversationMessage {
		return adapter.ConversationMessage{Role: "assistant", Text: "[tool] bash {}"}
	}
	conv := []adapter.ConversationMessage{
		msg("user", "first ask"), msg("assistant", "first answer"),
		msg("user", "second ask"), msg("assistant", "second answer"),
	}
	for _, tc := range []struct {
		name  string
		msgs  []adapter.ConversationMessage
		start int
		want  string
		found bool
	}{
		{"our turn is the tail", conv, 2, "second answer", true},
		// A newer turn landed before the read: its prose is NOT ours.
		{"later turn is excluded", conv, 0, "first answer", true},
		// Our turn produced nothing sayable; the previous turn's answer must
		// not be promoted into the gap.
		{"previous turn is excluded", []adapter.ConversationMessage{
			msg("user", "ask"), msg("assistant", "old answer"), msg("user", "again"), tool(),
		}, 2, "", false},
		// The prompt path marks before delivery, so its own user message is the
		// first thing inside the window and must not end it.
		{"leading user message is skipped", []adapter.ConversationMessage{
			msg("user", "old"), msg("assistant", "old answer"),
			msg("user", "ours"), msg("assistant", "our answer"),
		}, 2, "our answer", true},
		{"tool-only tail falls back within the turn", []adapter.ConversationMessage{
			msg("user", "ours"), msg("assistant", "our answer"), tool(),
		}, 0, "our answer", true},
		{"empty window", conv, len(conv), "", false},
		// A rotated/truncated conversation can no longer prove what our turn
		// said; silence beats a guess.
		{"watermark past the end", conv, len(conv) + 3, "", false},
		{"negative watermark is clamped", conv, -1, "first answer", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, found := assistantProseInWindow(tc.msgs, tc.start)
			if got != tc.want || found != tc.found {
				t.Fatalf("got %q,%v want %q,%v", got, found, tc.want, tc.found)
			}
		})
	}
}

// The race Sol identified, end to end through the real handler: a turn that
// lands between the close observation and the result read must neither replace
// our answer nor make us lose it.
//
// The schedule is real, not simulated: the handler marks its window on the
// initial Active=true observation, the test then rewrites the conversation, and
// only afterwards does the fanout report the turn closed — so the read happens
// strictly after the interfering append.
func TestGenericWaitBindsResultToTheTurnItObserved(t *testing.T) {
	const (
		userLine   = `{"type":"message","id":"u%d","message":{"role":"user","content":[{"type":"text","text":"ask %d"}]}}`
		answerLine = `{"type":"message","id":"a%d","message":{"role":"assistant","content":[{"type":"text","text":"%s"}]}}`
	)
	for _, tc := range []struct {
		name string
		// appended between the close observation and the read
		interfering []string
		want        string
	}{
		{
			// (b) a complete newer turn: its answer must not be reported as ours.
			name: "a whole new turn cannot replace our answer",
			interfering: []string{
				fmt.Sprintf(userLine, 2, 2),
				fmt.Sprintf(answerLine, 2, "second answer"),
			},
			want: "first answer",
		},
		{
			// (a) only a new user message: the snapshot selector would stop at
			// it and report nothing, silently losing our own answer.
			name:        "a new user message cannot lose our answer",
			interfering: []string{fmt.Sprintf(userLine, 2, 2)},
			want:        "first answer",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWaitFixture(t)
			// At turn start the conversation holds the prompt only — the state
			// the daemon actually sees when a turn opens.
			path := f.registerWithConversation(t, "s", "pi", piSessionHeader, fmt.Sprintf(userLine, 1, 1))
			fan := wf(wire.Session{ID: "s", Alive: true, Status: &wire.Status{Active: true}})

			rec := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				handleWaitCentral(rec, httptest.NewRequest(http.MethodPost, "/wait?timeout=5", nil),
					f.boot, fan, "s", func(string) string { return f.dir })
			}()

			// Let the handler observe the open turn and mark its window.
			waitForMark(t, f)

			// Our turn finishes, and a newer turn immediately lands.
			appendLines(t, path, append([]string{fmt.Sprintf(answerLine, 1, "first answer")}, tc.interfering...)...)
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
			if data["output"] != tc.want {
				t.Fatalf("output=%v want %q (data=%v)", data["output"], tc.want, data)
			}
		})
	}
}

// A wait that finds the turn already closed has no turn of its own to bind to
// and keeps snapshot semantics — the same answer `gmux agent output` gives.
func TestAlreadyClosedTurnKeepsSnapshotSemantics(t *testing.T) {
	f := newWaitFixture(t)
	f.register(t, "s", "pi", piResultLines...)
	data := f.wait(t, "s", "timeout=1", wire.Session{ID: "s", Alive: true, Status: &wire.Status{Active: false}})
	if data["output"] != "All green." {
		t.Fatalf("data=%v", data)
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

// The synchronous prompt is bound the same way: its window opens before
// delivery and is re-marked at its turn's start edge, so a turn appended before
// the read cannot be reported as this prompt's answer.
func TestSynchronousPromptBindsResultToItsTurn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conv.jsonl")
	writeLines(t, path,
		piSessionHeader,
		`{"type":"message","id":"u0","message":{"role":"user","content":[{"type":"text","text":"old ask"}]}}`,
		`{"type":"message","id":"a0","message":{"role":"assistant","content":[{"type":"text","text":"old answer"}]}}`,
	)
	row := liveRow("s", false)
	row.ConversationRef = path
	h := newAgentHarness(t, row)
	get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modePrompt}))
	<-h.prompts
	h.nextTimer(t)
	// Our turn opens, runs, and a THIRD turn lands before the daemon reads the
	// result. The binding is the PRE-DELIVERY watermark (taken before the bytes
	// went out, which <-h.prompts already proves happened): a plain prompt is
	// admitted only against an idle agent, so everything after that watermark
	// up to the next user message is our turn, and the third turn's answer sits
	// outside it.
	h.publish(statusOutcome("s", true, false, false))
	appendLines(t, path,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"ours"}]}}`,
		`{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"text","text":"our answer"}]}}`,
		`{"type":"message","id":"u2","message":{"role":"user","content":[{"type":"text","text":"somebody else"}]}}`,
		`{"type":"message","id":"a2","message":{"role":"assistant","content":[{"type":"text","text":"third answer"}]}}`,
	)
	h.publish(statusOutcome("s", false, false, false))
	got := get()
	if got.data()["output"] != "our answer" {
		t.Fatalf("output=%v want %q (body=%v)", got.data()["output"], "our answer", got.body)
	}
}

func TestFirstPromptBindsBeforePiCreatesConversationFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-created-yet.jsonl")
	row := liveRow("s", false)
	row.ConversationRef = path // pi reports the path before creating the file
	h := newAgentHarness(t, row)
	get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modePrompt}))
	<-h.prompts
	h.nextTimer(t)

	// The pre-delivery watermark must treat the known-but-absent file as an
	// empty conversation. The first turn then creates it and must return the
	// answer synchronously, not only through a later `agent output` snapshot.
	writeLines(t, path,
		piSessionHeader,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`,
		`{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"text","text":"first answer"}]}}`,
	)
	h.publish(statusOutcome("s", true, false, false))
	h.publish(statusOutcome("s", false, false, false))

	got := get()
	if got.data()["output"] != "first answer" {
		t.Fatalf("output=%v want %q (body=%v)", got.data()["output"], "first answer", got.body)
	}
}

// TestPlainPromptIgnoresAStaleActiveSeedWhenBinding is the regression for the
// one place the seed's activity bit must NOT choose the bound.
//
// The seed can lag the runner: here it still reports active although the
// previous turn has closed. The runner admits a plain prompt only against
// authoritative idle, so the admission itself proves no turn was running — but
// a window bound taken on the stale bit would open at the PREVIOUS turn's user
// boundary, with that turn's answer inside it. With a tool-only tail of its own,
// this prompt would then hand back "old answer" as its result: a previous turn's
// reply presented as this one's, under exit 0.
func TestPlainPromptIgnoresAStaleActiveSeedWhenBinding(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conv.jsonl")
	writeLines(t, path,
		piSessionHeader,
		`{"type":"message","id":"u0","message":{"role":"user","content":[{"type":"text","text":"old ask"}]}}`,
		`{"type":"message","id":"a0","message":{"role":"assistant","content":[{"type":"text","text":"old answer"}]}}`,
	)
	row := liveRow("s", false)
	row.ConversationRef = path
	h := newAgentHarness(t, row)
	// The seed lags: it still describes the closed turn as running.
	h.seedStatus("s", statusOutcome("s", true, false, false))
	get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modePrompt}))
	<-h.prompts
	h.nextTimer(t)
	// The lagging close lands as a later-versioned outcome (this is what the
	// seed was too old to have seen), and then our turn opens and produces
	// nothing but a tool call.
	h.publish(statusOutcome("s", false, false, false))
	h.publish(statusOutcome("s", true, false, false))
	appendLines(t, path,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`,
		`{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"toolCall","id":"t1","name":"bash","arguments":{}}]}}`,
	)
	h.publish(statusOutcome("s", false, false, false))
	got := get()
	if got.data()["outcome"] != outcomeCompleted {
		t.Fatalf("body=%v", got.body)
	}
	if out, ok := got.data()["output"]; ok {
		t.Fatalf("the previous turn's answer was reported as this prompt's result: %v", out)
	}
}

// A steer joins a turn already in flight, so the binding is the pre-delivery
// watermark taken at the RUNNING turn's user boundary. Note what that does and
// does not exclude: pre-steer prose from the same turn is INSIDE the window (by
// design — the amendment binds a steer to its turn, not to the instant of
// delivery), so this test passes because the post-steer answer is newer, not
// because earlier prose was excluded. A steer whose own tail is tool-only
// therefore reports the pre-steer prose.
func TestSteerBindsToContentAfterDelivery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conv.jsonl")
	writeLines(t, path,
		piSessionHeader,
		`{"type":"message","id":"u0","message":{"role":"user","content":[{"type":"text","text":"go"}]}}`,
		`{"type":"message","id":"a0","message":{"role":"assistant","content":[{"type":"text","text":"pre-steer prose"}]}}`,
	)
	row := liveRow("s", true) // a turn is already running
	row.ConversationRef = path
	h := newAgentHarness(t, row)
	get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "no, this way", "mode": modeSteer}))
	<-h.prompts
	appendLines(t, path,
		`{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"text","text":"post-steer answer"}]}}`,
	)
	h.publish(statusOutcome("s", false, false, false))
	got := get()
	if got.data()["output"] != "post-steer answer" {
		t.Fatalf("output=%v (body=%v)", got.data()["output"], got.body)
	}
}

// A follow-up queued behind a running turn is bound to the QUEUED turn, which
// is why the window is re-marked at each turn-start edge rather than only taken
// before delivery: the running turn's own answer lands inside the pre-delivery
// window and would otherwise be reported as the follow-up's.
func TestQueuedFollowUpBindsToTheQueuedTurn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conv.jsonl")
	writeLines(t, path,
		piSessionHeader,
		`{"type":"message","id":"u0","message":{"role":"user","content":[{"type":"text","text":"first ask"}]}}`,
	)
	row := liveRow("s", true) // a turn is already running
	row.ConversationRef = path
	h := newAgentHarness(t, row)
	get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "and then", "mode": modeFollowUp}))
	<-h.prompts
	// The running turn finishes with its own answer.
	appendLines(t, path,
		`{"type":"message","id":"a0","message":{"role":"assistant","content":[{"type":"text","text":"running turn answer"}]}}`,
	)
	h.publish(statusOutcome("s", false, false, false))
	h.nextTimer(t) // queued-turn admission window
	// The queued turn opens (re-marking the window), then answers.
	h.publish(statusOutcome("s", true, false, false))
	waitForMarks(t, h, 2)
	appendLines(t, path,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"and then"}]}}`,
		`{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"text","text":"queued answer"}]}}`,
	)
	h.publish(statusOutcome("s", false, false, false))
	got := get()
	if got.data()["outcome"] != outcomeCompleted || got.data()["output"] != "queued answer" {
		t.Fatalf("body=%v windows=%v", got.body, h.resultWindows)
	}
}

// A wait that attaches to a turn ALREADY RUNNING must keep that turn's answer,
// including when the turn's tail is tool-only. Binding such a wait to the
// message count excluded prose the turn had already persisted, so a wait that
// completed perfectly well reported nothing.
func TestMidTurnWaitKeepsItsOwnTurnsProse(t *testing.T) {
	f := newWaitFixture(t)
	path := f.registerWithConversation(t, "s", "pi",
		piSessionHeader,
		`{"type":"message","id":"u0","message":{"role":"user","content":[{"type":"text","text":"older ask"}]}}`,
		`{"type":"message","id":"a0","message":{"role":"assistant","content":[{"type":"text","text":"older answer"}]}}`,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"ask"}]}}`,
		// Prose this turn produced BEFORE the wait subscribed.
		`{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"text","text":"the answer"}]}}`,
	)
	fan := wf(wire.Session{ID: "s", Alive: true, Status: &wire.Status{Active: true}})
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleWaitCentral(rec, httptest.NewRequest(http.MethodPost, "/wait?timeout=5", nil),
			f.boot, fan, "s", func(string) string { return f.dir })
	}()
	waitForMark(t, f)
	// The turn ends on a tool call, which carries no prose.
	appendLines(t, path,
		`{"type":"message","id":"a2","message":{"role":"assistant","content":[{"type":"toolCall","id":"t1","name":"bash","arguments":{}}]}}`,
	)
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

// ...and the previous turn still stays outside that window: a mid-turn
// attachment to a turn that has said nothing yet reports nothing, not the answer
// before it.
func TestMidTurnWaitStillExcludesThePreviousTurn(t *testing.T) {
	f := newWaitFixture(t)
	path := f.registerWithConversation(t, "s", "pi",
		piSessionHeader,
		`{"type":"message","id":"u0","message":{"role":"user","content":[{"type":"text","text":"older ask"}]}}`,
		`{"type":"message","id":"a0","message":{"role":"assistant","content":[{"type":"text","text":"older answer"}]}}`,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"ask"}]}}`,
	)
	fan := wf(wire.Session{ID: "s", Alive: true, Status: &wire.Status{Active: true}})
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleWaitCentral(rec, httptest.NewRequest(http.MethodPost, "/wait?timeout=5", nil),
			f.boot, fan, "s", func(string) string { return f.dir })
	}()
	waitForMark(t, f)
	appendLines(t, path,
		`{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"toolCall","id":"t1","name":"bash","arguments":{}}]}}`,
	)
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
		t.Fatalf("previous turn's answer leaked: %v", got)
	}
}

// A conversation that could not be read when the turn was first observed offers
// no safe boundary, so no result is attributed even if storage becomes readable
// (with history) before the read. Binding to 0 instead would hand back an
// earlier turn's answer.
func TestUnreadableConversationAtMarkYieldsNoResult(t *testing.T) {
	f := newWaitFixture(t)
	f.register(t, "s", "pi") // renderer adapter, but no conversation ref yet
	fan := wf(wire.Session{ID: "s", Alive: true, Status: &wire.Status{Active: true}})
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleWaitCentral(rec, httptest.NewRequest(http.MethodPost, "/wait?timeout=5", nil),
			f.boot, fan, "s", func(string) string { return f.dir })
	}()
	waitForMark(t, f)
	// Storage shows up late, carrying somebody else's turn.
	f.registerWithConversation(t, "s", "pi",
		piSessionHeader,
		`{"type":"message","id":"u0","message":{"role":"user","content":[{"type":"text","text":"older ask"}]}}`,
		`{"type":"message","id":"a0","message":{"role":"assistant","content":[{"type":"text","text":"older answer"}]}}`,
	)
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
		t.Fatalf("unreadable-at-mark window produced a result: %v", got)
	}
	if got := latestAgentMessageIn(context.Background(), f.store, "s",
		resultWindow{bound: true, unreadable: true}); got != "" {
		t.Fatalf("unreadable window selected %q", got)
	}
}

// Nobody renders a conversation for a caller who will not be shown one.
func TestNoResultMeansNoRender(t *testing.T) {
	t.Run("detached prompt marks nothing", func(t *testing.T) {
		h := newAgentHarness(t, liveRow("s", false))
		get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{
			"prompt": "hi", "mode": modePrompt, "wait": false,
		}))
		<-h.prompts
		h.nextTimer(t)
		h.publish(statusOutcome("s", true, false, false))
		if got := get(); got.code != http.StatusAccepted {
			t.Fatalf("code=%d body=%v", got.code, got.body)
		}
		if n := h.markCalls.Load(); n != 0 {
			t.Fatalf("detached prompt marked %d result windows", n)
		}
	})
	t.Run("send --wait marks nothing", func(t *testing.T) {
		f := newWaitFixture(t)
		f.register(t, "s", "pi", piResultLines...)
		// The turn is already running when the raw wait attaches (the fused
		// path's own subscribe-then-deliver ordering is covered elsewhere); it
		// resolves on the close below.
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
		if n := f.marks.Load(); n != 0 {
			t.Fatalf("raw send --wait marked %d result windows", n)
		}
	})
}

// A resumed runner's first observation carries no status yet. That still counts
// as having looked BEFORE the turn opened, so the genuine turn-start edge that
// follows must bind as "starting" — binding it to the previous turn's user
// boundary would report a pre-resume answer as this turn's result, under exit 0.
func TestStatuslessFirstLookStillBindsTheNextEdgeAsStarting(t *testing.T) {
	f := newWaitFixture(t)
	path := f.registerWithConversation(t, "s", "pi",
		piSessionHeader,
		`{"type":"message","id":"u0","message":{"role":"user","content":[{"type":"text","text":"before the restart"}]}}`,
		`{"type":"message","id":"a0","message":{"role":"assistant","content":[{"type":"text","text":"pre-resume answer"}]}}`,
	)
	// First look: alive, but the resumed runner has published no status.
	fan := wf(wire.Session{ID: "s", Alive: true})
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleWaitCentral(rec, httptest.NewRequest(http.MethodPost, "/wait?timeout=5", nil),
			f.boot, fan, "s", func(string) string { return f.dir })
	}()
	// Let the handler take its statusless first look before the turn opens.
	// This barrier only prevents a flaky FAILURE, never a false pass: if the
	// active frame won the race, the handler's first look would itself be the
	// open turn, it would bind to the user boundary, and the assertion below
	// would fire.
	time.Sleep(300 * time.Millisecond)
	// The turn genuinely starts now.
	fan.BroadcastFrames(wire.Frames{Sessions: &wire.SessionsPayload{
		Sessions: []wire.Session{{ID: "s", Alive: true, Status: &wire.Status{Active: true}}},
	}})
	waitForMark(t, f)
	// It produces only a tool call, then closes: there is nothing of ITS OWN to
	// report, and the pre-resume answer is not it.
	appendLines(t, path,
		`{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"toolCall","id":"t1","name":"bash","arguments":{}}]}}`,
	)
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
		t.Fatalf("pre-resume answer reported as this turn's result: %v", got)
	}
}
