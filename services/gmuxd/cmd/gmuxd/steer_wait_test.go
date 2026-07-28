package main

// steer_wait_test.go pins ADR 0027's "Steering interrupts waits" at the daemon
// boundary: an injection into an active turn resolves every OTHER armed wait
// early, the injecting request is excluded by delivery identity but only while
// its message is the loop's last, and an injection the loop never acknowledged
// makes the injector's own result indeterminate rather than the pre-injection
// answer.

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
)

// injected is the frame shape of a turn that is running and has taken user
// messages since it started.
func injectedFrame(seq uint64, trigger string, injections ...sessioncoord.TurnInjection) *sessioncoord.TurnFrame {
	return &sessioncoord.TurnFrame{Seq: seq, Current: &sessioncoord.TurnCurrent{
		TurnSeq: seq, Trigger: trigger, Injections: injections,
		// The runner's monotonic counter, which is what novelty is decided
		// against; here it happens to equal the retained list's length because
		// these turns are nowhere near the list's bound.
		InjectionCount: uint64(len(injections)),
	}}
}

// injectionSignal is how an injection reaches a waiter in production: a
// transient, frame-bearing outcome with no session row attached, because an
// injection changes no row.
func injectionSignal(id string, frame *sessioncoord.TurnFrame) sessioncoord.Outcome {
	return sessioncoord.Outcome{
		Type: sessioncoord.OutcomeActivity, ID: centralstore.SessionID(id), Alive: true,
		Generation: harnessGeneration, Frame: frame,
	}
}

// ── the unit rule ───────────────────────────────────────────────────────────

func TestInjectionWatchRules(t *testing.T) {
	mine := sessioncoord.TurnInjection{Text: "actually, do X", DeliveryID: "d-1"}
	theirs := sessioncoord.TurnInjection{Text: "no, do Y", DeliveryID: "d-2"}
	human := sessioncoord.TurnInjection{Text: "stop, I'll drive"}
	for _, tc := range []struct {
		name       string
		watch      injectionWatch
		injections []sessioncoord.TurnInjection
		want       steerVerdict
	}{
		{
			name:       "a bystander is interrupted by any injection",
			watch:      injectionWatch{turnSeq: 5},
			injections: []sessioncoord.TurnInjection{mine},
			want:       steerVerdict{Steered: true, Text: mine.Text},
		},
		{
			name:       "a human injection interrupts every waiter",
			watch:      injectionWatch{turnSeq: 5},
			injections: []sessioncoord.TurnInjection{human},
			want:       steerVerdict{Steered: true, Text: human.Text},
		},
		{
			name:       "the injector is not interrupted by its own message",
			watch:      injectionWatch{turnSeq: 5, deliveryID: "d-1"},
			injections: []sessioncoord.TurnInjection{mine},
			want:       steerVerdict{},
		},
		{
			name:       "a later injection supersedes the earlier injector",
			watch:      injectionWatch{turnSeq: 5, deliveryID: "d-1"},
			injections: []sessioncoord.TurnInjection{mine, theirs},
			want:       steerVerdict{Steered: true, Text: theirs.Text, Cause: causeSteeredAgain},
		},
		{
			name:       "a human superseding the injector is still steered_again",
			watch:      injectionWatch{turnSeq: 5, deliveryID: "d-1"},
			injections: []sessioncoord.TurnInjection{mine, human},
			want:       steerVerdict{Steered: true, Text: human.Text, Cause: causeSteeredAgain},
		},
		{
			name:       "a foreign injection before our own ack still interrupts",
			watch:      injectionWatch{turnSeq: 5, deliveryID: "d-1"},
			injections: []sessioncoord.TurnInjection{human},
			want:       steerVerdict{Steered: true, Text: human.Text},
		},
		{
			name:       "injections that predate the wait are part of the turn it chose",
			watch:      injectionWatch{turnSeq: 5, baseline: 1},
			injections: []sessioncoord.TurnInjection{human},
			want:       steerVerdict{},
		},
		{
			name:       "an injection on a DIFFERENT turn is somebody else's business",
			watch:      injectionWatch{turnSeq: 9},
			injections: []sessioncoord.TurnInjection{human},
			want:       steerVerdict{},
		},
		{
			name:  "a wait that identified no turn watches nothing",
			watch: injectionWatch{},
			// An empty delivery id must never match a human's id-less injection.
			injections: []sessioncoord.TurnInjection{human},
			want:       steerVerdict{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame := injectedFrame(5, "go", tc.injections...)
			if got := tc.watch.check(frame); got != tc.want {
				t.Fatalf("check=%+v want %+v", got, tc.want)
			}
			// The same rule must hold over the settled record: a waiter that
			// learns of the steer only at the close may not be handed the merged
			// answer either.
			close := &sessioncoord.TurnClose{TurnSeq: 5, Outcome: outcomeCompleted, Trigger: "go", Injections: tc.injections}
			if got := tc.watch.checkClose(close); got != tc.want {
				t.Fatalf("checkClose=%+v want %+v", got, tc.want)
			}
		})
	}
}

// The saturation case both anchors found: once the frame's bounded injection
// list stops growing, novelty must still be visible. Deciding it from the list's
// LENGTH made every later injection invisible to a wait that armed on a
// heavily-steered turn, and that wait was then served the merged answer under
// exit 0 — the inversion the rule exists to prevent.
func TestSaturatedInjectionListStillInterrupts(t *testing.T) {
	// A turn that has taken 64 injections; the list is capped there and its
	// length can never grow again.
	const cap = 64
	list := make([]sessioncoord.TurnInjection, cap)
	for i := range list {
		list[i] = sessioncoord.TurnInjection{Text: "old steer"}
	}
	w := injectionWatch{turnSeq: 5, baseline: cap}

	// Another injection arrives: the list evicts its oldest and stays at 64,
	// while the runner's monotonic counter advances.
	list[cap-1] = sessioncoord.TurnInjection{Text: "NEW STEER"}
	frame := &sessioncoord.TurnFrame{Seq: 2, Current: &sessioncoord.TurnCurrent{
		TurnSeq: 5, Trigger: "go", Injections: list, InjectionCount: cap + 1,
	}}
	if v := w.check(frame); !v.Steered || v.Text != "NEW STEER" {
		t.Fatalf("a saturated list hid a new injection: %+v", v)
	}
	// Same at the close, which is the backstop for a missed live signal.
	closed := &sessioncoord.TurnClose{
		TurnSeq: 5, Outcome: outcomeCompleted, Output: "merged answer",
		Injections: list, InjectionCount: cap + 1,
	}
	if v := w.checkClose(closed); !v.Steered {
		t.Fatalf("a saturated list hid a new injection at the close: %+v", v)
	}
	// And nothing new still means nothing new.
	quiet := &sessioncoord.TurnFrame{Seq: 3, Current: &sessioncoord.TurnCurrent{
		TurnSeq: 5, Trigger: "go", Injections: list, InjectionCount: cap,
	}}
	if v := w.check(quiet); v.Steered {
		t.Fatalf("the baseline injections interrupted the wait: %+v", v)
	}
}

// A runner that predates the counter reports none, and novelty then falls back
// to the retained list. Reading a missing counter as zero would make every
// injection invisible on that runner — strictly worse than the bounded list.
func TestMissingInjectionCountFallsBackToTheList(t *testing.T) {
	w := injectionWatch{turnSeq: 5, baseline: baselineInjections(&sessioncoord.TurnFrame{
		Current: &sessioncoord.TurnCurrent{TurnSeq: 5, Injections: []sessioncoord.TurnInjection{{Text: "already there"}}},
	}, 5)}
	if w.baseline != 1 {
		t.Fatalf("baseline=%d from a counter-less frame", w.baseline)
	}
	frame := &sessioncoord.TurnFrame{Current: &sessioncoord.TurnCurrent{TurnSeq: 5, Injections: []sessioncoord.TurnInjection{
		{Text: "already there"}, {Text: "new one"},
	}}}
	if v := w.check(frame); !v.Steered || v.Text != "new one" {
		t.Fatalf("counter-less frame lost its novelty: %+v", v)
	}
}

// The settle-before-ack race, which is the one case where the injector's result
// is neither the answer nor a steer: pi may have closed the loop without ever
// consuming the delivered text.
func TestUnacknowledgedInjectionIsIndeterminate(t *testing.T) {
	w := injectionWatch{turnSeq: 5, deliveryID: "d-1"}
	close := &sessioncoord.TurnClose{TurnSeq: 5, Outcome: outcomeCompleted, Output: "pre-steer answer"}
	outcome, cause, _ := w.closeVerdict(close)
	if outcome != outcomeIndeterminate || cause != causeInjectionUnacknowledged {
		t.Fatalf("outcome=%q cause=%q; the pre-injection answer must never be served as the injector's", outcome, cause)
	}
	// A non-injecting wait on the very same close is served normally.
	if outcome, _, _ := (injectionWatch{turnSeq: 5}).closeVerdict(close); outcome != "" {
		t.Fatalf("a bystander's ordinary completion was replaced by %q", outcome)
	}
}

// ── the generic wait ────────────────────────────────────────────────────────

// A `gmux wait` armed on a running turn resolves as soon as somebody steers it:
// the turn keeps running, but its answer no longer answers what this wait asked.
func TestGenericWaitResolvesOnAForeignSteer(t *testing.T) {
	f := newWaitFixture(t)
	f.register(t, "s", "pi", piResultLines...)
	f.openTurn(7, "run the tests")
	// The turn NEVER closes here: a steer resolves the wait while the turn keeps
	// running, which is the whole point of the rule.
	data := runGenericWait(t, f, func() {
		// A human grabbed the wheel. The ticker re-reads the retained frame, so
		// no event is needed for this to be observed.
		f.setFrame(injectedFrame(7, "run the tests", sessioncoord.TurnInjection{Text: "stop, fix the lint first"}))
	})
	if data["reason"] != outcomeSteered || data["outcome"] != outcomeSteered {
		t.Fatalf("data=%v", data)
	}
	if data["steered_by"] != "stop, fix the lint first" || data["trigger"] != "run the tests" {
		t.Fatalf("the report lost its facts: %v", data)
	}
	if _, ok := data["output"]; ok {
		t.Fatalf("a steered wait must carry no answer: %v", data)
	}
}

// An injection that was already on the turn when the wait armed is part of the
// turn it chose to wait on, so it resolves normally and carries the answer — and
// the trigger excerpt rides the conclusion, which is what lets the CLI report a
// non-completed resolution without reading the tape.
func TestGenericWaitIgnoresInjectionsItArmedOn(t *testing.T) {
	f := newWaitFixture(t)
	f.register(t, "s", "pi", piResultLines...)
	f.setFrame(injectedFrame(7, "run the tests", sessioncoord.TurnInjection{Text: "also check the docs"}))
	data := runGenericWait(t, f, func() {
		f.setFrame(&sessioncoord.TurnFrame{Seq: 99, Last: &sessioncoord.TurnClose{
			TurnSeq: 7, Outcome: outcomeCompleted, Trigger: "run the tests", Output: "All green.",
			Injections: []sessioncoord.TurnInjection{{Text: "also check the docs"}},
		}})
	})
	if data["outcome"] != outcomeCompleted || data["output"] != "All green." {
		t.Fatalf("data=%v", data)
	}
	if data["trigger"] != "run the tests" {
		t.Fatalf("the conclusion lost the trigger excerpt a report needs: %v", data)
	}
}

// runGenericWait arms handleWaitCentral on a session that is active, runs settle
// once the wait has observed the open turn, and returns the resolved payload.
// The fanout's active→inactive flip is what closes the turn, so a test that only
// changes the frame (a steer) simply does not call it.
func runGenericWait(t *testing.T, f *waitFixture, settle func()) map[string]any {
	t.Helper()
	fan := wf(wire.Session{ID: "s", Alive: true, Status: &wire.Status{Active: true}})
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleWaitCentral(rec, httptest.NewRequest(http.MethodPost, "/wait?timeout=5", nil),
			f.boot, fan, "s", func(string) string { return f.dir })
	}()
	waitForLook(t, f)
	settle()
	fan.BroadcastFrames(wire.Frames{Sessions: &wire.SessionsPayload{
		Sessions: []wire.Session{{ID: "s", Alive: true, Status: &wire.Status{Active: false}}},
	}})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not resolve")
	}
	return decodeWaitData(t, rec)
}

// ── the synchronous prompt ──────────────────────────────────────────────────

// A sync prompt waiting on a turn somebody else steers is interrupted like any
// other waiter, and told what went in.
func TestSyncPromptInterruptedByAForeignSteer(t *testing.T) {
	h := newAgentHarness(t, liveRow("s", false))
	get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "what is 2+2?", "mode": modePrompt}))
	<-h.prompts
	h.nextTimer(t)
	h.openTurn(3, "what is 2+2?")
	h.publish(statusOutcome("s", true, false, false))
	steered := injectedFrame(3, "what is 2+2?", sessioncoord.TurnInjection{Text: "never mind, do Y"})
	h.setFrame(steered)
	h.publish(injectionSignal("s", steered))
	got := get()
	if got.code != http.StatusOK {
		t.Fatalf("status=%d body=%v", got.code, got.body)
	}
	if got.data()["outcome"] != outcomeSteered || got.data()["steered_by"] != "never mind, do Y" {
		t.Fatalf("body=%v", got.body)
	}
	if _, ok := got.data()["output"]; ok {
		t.Fatalf("a steered prompt must carry no answer: %v", got.body)
	}
}

// The steerer's own sync wait is excluded: its delivery id is on the injection,
// so the merged close is its result.
func TestSteererClaimsTheMergedClose(t *testing.T) {
	h := newAgentHarness(t, liveRow("s", true))
	h.openTurn(5, "go")
	get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "use the other API", "mode": modeSteer}))
	call := <-h.prompts
	if call.deliveryID != harnessDeliveryID {
		t.Fatalf("a steer must carry a delivery identity, got %q", call.deliveryID)
	}
	// The adapter acknowledges the injection, then the merged loop settles.
	acked := injectedFrame(5, "go", sessioncoord.TurnInjection{Text: "use the other API", DeliveryID: harnessDeliveryID})
	h.setFrame(acked)
	h.publish(injectionSignal("s", acked))
	h.setFrame(&sessioncoord.TurnFrame{Seq: 90, Last: &sessioncoord.TurnClose{
		TurnSeq: 5, Outcome: outcomeCompleted, Trigger: "go", Output: "used the other API",
		Injections: []sessioncoord.TurnInjection{{Text: "use the other API", DeliveryID: harnessDeliveryID}},
	}})
	h.publish(statusOutcome("s", false, false, false))
	got := get()
	if got.data()["outcome"] != outcomeCompleted || got.data()["output"] != "used the other API" {
		t.Fatalf("the steerer lost the close it caused: %v", got.body)
	}
}

// ...but only while its message is the LAST injection: a later one supersedes it
// and it is interrupted like everybody else.
func TestSupersededSteererIsInterrupted(t *testing.T) {
	h := newAgentHarness(t, liveRow("s", true))
	h.openTurn(5, "go")
	get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "use the other API", "mode": modeSteer}))
	<-h.prompts
	frame := injectedFrame(5, "go",
		sessioncoord.TurnInjection{Text: "use the other API", DeliveryID: harnessDeliveryID},
		sessioncoord.TurnInjection{Text: "no, revert it"})
	h.setFrame(frame)
	h.publish(injectionSignal("s", frame))
	got := get()
	if got.data()["outcome"] != outcomeSteered || got.data()["cause"] != causeSteeredAgain {
		t.Fatalf("body=%v", got.body)
	}
	if got.data()["steered_by"] != "no, revert it" {
		t.Fatalf("body=%v", got.body)
	}
}

// A steer whose text the loop never acknowledged before settling is
// INDETERMINATE: the pre-injection answer is not this request's result, and
// exit 0 for it would be the exact lie this design exists to prevent.
func TestUnacknowledgedSteerReportsIndeterminate(t *testing.T) {
	h := newAgentHarness(t, liveRow("s", true))
	h.openTurn(5, "go")
	get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "use the other API", "mode": modeSteer}))
	<-h.prompts
	h.setFrame(&sessioncoord.TurnFrame{Seq: 90, Last: &sessioncoord.TurnClose{
		TurnSeq: 5, Outcome: outcomeCompleted, Trigger: "go", Output: "pre-steer answer",
	}})
	h.publish(statusOutcome("s", false, false, false))
	got := get()
	if got.data()["outcome"] != outcomeIndeterminate || got.data()["cause"] != causeInjectionUnacknowledged {
		t.Fatalf("body=%v", got.body)
	}
	if _, ok := got.data()["output"]; ok {
		t.Fatalf("an unacknowledged steer was handed the pre-injection answer: %v", got.body)
	}
}

// A plain prompt starts its own turn, so it needs no delivery identity: nothing
// of its text is an injection into anybody's wait.
func TestPlainPromptCarriesNoDeliveryIdentity(t *testing.T) {
	h := newAgentHarness(t, liveRow("s", false))
	get := runPromptAsync(t, h, promptBodyJSON(t, map[string]any{"prompt": "hi", "mode": modePrompt, "wait": false}))
	call := <-h.prompts
	if call.deliveryID != "" {
		t.Fatalf("a plain prompt sent a delivery id (%q); the runner would arm a correlation window for a turn nobody is joining", call.deliveryID)
	}
	h.nextTimer(t)
	h.openTurn(1, "hi")
	h.publish(statusOutcome("s", true, false, false))
	get()
}
