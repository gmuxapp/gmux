package agentext

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runExtDriver materializes the shipped extension, runs an inline node driver
// against it with a collecting fake runner socket, and returns the posted hook
// events. body is JS executed with `handlers` (the extension's registered
// handlers) and `ctx` (a minimal sessionManager) in scope.
func runExtDriver(t *testing.T, body string) []map[string]any {
	t.Helper()
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping pi-ext behavior test")
	}
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "runner.sock")
	events, stop := hookEventCollector(t, sockPath)
	defer stop()

	extPath := filepath.Join(dir, "pi-ext.mjs")
	if err := os.WriteFile(extPath, extSource, 0o644); err != nil {
		t.Fatalf("materialize extension: %v", err)
	}
	driver := `
		const ext = (await import(process.argv[2])).default;
		const handlers = {};
		ext({ on: (ev, fn) => { handlers[ev] = fn; } });
		const ctx = { sessionManager: {
			getSessionFile: () => "/tmp/conv.jsonl",
			getSessionId: () => "id-1",
			getSessionName: () => "",
			getCwd: () => "/tmp",
		}};
` + body + `
		await new Promise((r) => setTimeout(r, 400));
	`
	driverPath := filepath.Join(dir, "driver.mjs")
	if err := os.WriteFile(driverPath, []byte(driver), 0o644); err != nil {
		t.Fatalf("write driver: %v", err)
	}
	cmd := exec.Command(nodeBin, driverPath, extPath)
	cmd.Env = append(os.Environ(), "GMUX_SESSION_SOCK="+sockPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node driver: %v\n%s", err, out)
	}
	return events()
}

// turnEvents filters to op "turn" events, in delivery order.
func turnEvents(evs []map[string]any) []map[string]any {
	var out []map[string]any
	for _, ev := range evs {
		if ev["op"] == "turn" {
			out = append(out, ev)
		}
	}
	return out
}

func seqOf(t *testing.T, ev map[string]any) float64 {
	t.Helper()
	v, ok := ev["turn_seq"].(float64)
	if !ok {
		t.Fatalf("event %v has no turn_seq", ev)
	}
	return v
}

// TestPiExtAssertsTurnFacts is the source-assertion contract (ADR 0027,
// 2026-07-28): one settled run is one turn, identified by turn_seq, carrying the
// trigger captured at before_agent_start and the final assistant prose of the
// settled run.
func TestPiExtAssertsTurnFacts(t *testing.T) {
	evs := turnEvents(runExtDriver(t, `
		handlers.before_agent_start({ prompt: "what is 2+2?" }, ctx);
		handlers.agent_start({}, ctx);
		handlers.agent_end({ messages: [
			{ role: "user", content: [{ type: "text", text: "what is 2+2?" }] },
			{ role: "assistant", content: [
				{ type: "thinking", text: "hmm" },
				{ type: "toolCall", name: "bash", arguments: {} },
				{ type: "text", text: "4" },
			], stopReason: "stop" },
		] }, ctx);
		handlers.agent_settled({}, ctx);
	`))
	if len(evs) != 2 {
		t.Fatalf("want start+end, got %v", evs)
	}
	start, end := evs[0], evs[1]
	if start["phase"] != "start" || start["trigger"] != "what is 2+2?" {
		t.Errorf("start = %v", start)
	}
	if end["phase"] != "end" || end["outcome"] != "completed" || end["output"] != "4" {
		t.Errorf("end = %v", end)
	}
	if end["truncated"] != nil {
		t.Errorf("truncated set for an uncapped output: %v", end)
	}
	if seqOf(t, start) != seqOf(t, end) || seqOf(t, start) != 1 {
		t.Errorf("turn_seq mismatch: %v / %v", start, end)
	}
}

func TestPiExtStartCarriesPersistenceBaselineAndSourceBytes(t *testing.T) {
	evs := turnEvents(runExtDriver(t, `
		ctx.sessionManager.getBranch = () => [
			{ type: "message", message: { role: "user" } },
			{ type: "message", message: { role: "assistant" } },
			{ type: "message", message: { role: "user" } },
		];
		handlers.before_agent_start({ prompt: "  ask\n" }, ctx);
		handlers.agent_start({}, ctx);
	`))
	if len(evs) != 1 || evs[0]["trigger"] != "  ask\n" || evs[0]["source_bytes"] != float64(6) || evs[0]["previous_exchanges"] != float64(2) {
		t.Fatalf("start source facts=%v", evs)
	}
}

// TestPiExtHoldsTheTriggerUntilTheTurnStarts pins the ORDER of the two facts pi
// gives us separately: `before_agent_start` carries the prompt, `agent_start`
// raises the active edge, and the trigger must ride the START post rather than
// one of its own.
//
// Both halves are load-bearing:
//
//   - nothing may be posted at `before_agent_start`. pi's preflight throws
//     (no model, no credentials) happen BEFORE it, but extension-handler code
//     runs between it and the loop inside the same `try` and its throw is
//     re-raised — so a post there could announce a turn that never runs, and
//     gmux would report an active session with an idle agent.
//   - the start post must nevertheless carry the trigger, or the turn's report
//     could never say what the turn was asked to do.
func TestPiExtHoldsTheTriggerUntilTheTurnStarts(t *testing.T) {
	// A prompt that fails preflight after before_agent_start: no agent_start
	// follows, so no turn may be announced at all.
	if evs := turnEvents(runExtDriver(t, `
		handlers.before_agent_start({ prompt: "never runs" }, ctx);
	`)); len(evs) != 0 {
		t.Fatalf("before_agent_start announced a turn on its own: %v", evs)
	}

	// And when the loop does start, the held trigger arrives WITH the edge.
	evs := turnEvents(runExtDriver(t, `
		handlers.before_agent_start({ prompt: "the real prompt" }, ctx);
		handlers.agent_start({}, ctx);
	`))
	if len(evs) != 1 || evs[0]["phase"] != "start" {
		t.Fatalf("want exactly one start event, got %v", evs)
	}
	if evs[0]["trigger"] != "the real prompt" {
		t.Fatalf("the start post did not carry the held trigger: %v", evs[0])
	}
}

// TestPiExtRetryIsOneTurn pins the boundary that made agent_end unusable: pi
// emits an error-shaped agent_end per retry attempt and a fresh agent_start for
// the continuation, but exactly one agent_settled per run. One turn, one close,
// and the close reports the FINAL attempt's result — not the transient error.
func TestPiExtRetryIsOneTurn(t *testing.T) {
	evs := turnEvents(runExtDriver(t, `
		handlers.before_agent_start({ prompt: "retry me" }, ctx);
		handlers.agent_start({}, ctx);
		handlers.agent_end({ messages: [
			{ role: "assistant", content: [{ type: "text", text: "boom" }], stopReason: "error" },
		] }, ctx);
		handlers.agent_start({}, ctx);            // pi's retry continuation
		handlers.agent_end({ messages: [
			{ role: "assistant", content: [{ type: "text", text: "recovered" }], stopReason: "stop" },
		] }, ctx);
		handlers.agent_settled({}, ctx);
	`))
	if len(evs) != 2 {
		t.Fatalf("want exactly one start and one end, got %v", evs)
	}
	if evs[0]["phase"] != "start" || evs[1]["phase"] != "end" {
		t.Fatalf("events = %v", evs)
	}
	if evs[1]["outcome"] != "completed" || evs[1]["output"] != "recovered" {
		t.Errorf("end = %v", evs[1])
	}
	if seqOf(t, evs[0]) != 1 || seqOf(t, evs[1]) != 1 {
		t.Errorf("retry allocated a second turn_seq: %v", evs)
	}
}

// TestPiExtReportsAdditionalUserBoundaries pins that a user message entering
// the running loop extends the activity instead of opening another one.
func TestPiExtReportsAdditionalUserBoundaries(t *testing.T) {
	evs := turnEvents(runExtDriver(t, `
		handlers.before_agent_start({ prompt: "first" }, ctx);
		handlers.agent_start({}, ctx);
		handlers.message_start({ message: { role: "user", content: "first" } }, ctx);
		handlers.message_start({ message: { role: "assistant", content: [{ type: "text", text: "working" }] } }, ctx);
		handlers.message_start({ message: { role: "user", content: "actually, stop" } }, ctx);
		handlers.agent_end({ messages: [
			{ role: "assistant", content: [{ type: "text", text: "ok" }], stopReason: "stop" },
		] }, ctx);
		handlers.agent_settled({}, ctx);
	`))
	if len(evs) != 3 {
		t.Fatalf("want start+steered+end, got %v", evs)
	}
	if evs[1]["phase"] != "steered" || evs[1]["text"] != "actually, stop" {
		t.Errorf("steered event = %v", evs[1])
	}
	if seqOf(t, evs[1]) != seqOf(t, evs[0]) {
		t.Errorf("injection reported against another turn: %v", evs)
	}
}

// TestPiExtUserBoundarySourceLength survives the display cap independently of
// whether the original text itself ends in an ellipsis.
func TestPiExtUserBoundarySourceLength(t *testing.T) {
	evs := turnEvents(runExtDriver(t, `
		handlers.agent_start({}, ctx);
		handlers.message_start({ message: { role: "assistant", content: [{ type: "text", text: "working" }] } }, ctx);
		handlers.message_start({ message: { role: "user", content: "x".repeat(10000) } }, ctx);
		handlers.message_start({ message: { role: "user", content: "wait\u2026" } }, ctx);
		handlers.agent_end({ messages: [
			{ role: "assistant", content: [{ type: "text", text: "ok" }], stopReason: "stop" },
		] }, ctx);
		handlers.agent_settled({}, ctx);
	`))
	var steers []map[string]any
	for _, e := range evs {
		if e["phase"] == "steered" {
			steers = append(steers, e)
		}
	}
	if len(steers) != 2 {
		t.Fatalf("want two injections, got %v", evs)
	}
	capped, plain := steers[0], steers[1]
	if capped["source_bytes"] != float64(10000) {
		t.Errorf("capped boundary lost source byte length: %v", capped)
	}
	if text, _ := capped["text"].(string); !strings.HasSuffix(text, "\u2026") {
		t.Errorf("a capped boundary lost its ellipsis: %q", text)
	}
	if plain["source_bytes"] != float64(len("wait\u2026")) {
		t.Errorf("whole boundary source bytes = %v", plain)
	}
	if plain["text"] != "wait\u2026" {
		t.Errorf("text = %v", plain["text"])
	}
}

// TestPiExtNonCompletedCarriesPartialOutput pins terminal-partial transport.
func TestPiExtNonCompletedCarriesPartialOutput(t *testing.T) {
	evs := turnEvents(runExtDriver(t, `
		handlers.agent_start({}, ctx);
		handlers.agent_end({ messages: [
			{ role: "assistant", content: [{ type: "text", text: "half an answer" }],
			  stopReason: "error", errorMessage: "provider exploded" },
		] }, ctx);
		handlers.agent_settled({}, ctx);
	`))
	end := evs[len(evs)-1]
	if end["outcome"] != "error" {
		t.Fatalf("end = %v", end)
	}
	if end["output"] != "half an answer" {
		t.Errorf("error close partial output = %v", end)
	}
	if end["diagnostic"] != "provider exploded" {
		t.Errorf("diagnostic = %v", end["diagnostic"])
	}
}

// TestPiExtToolOnlyTurnOmitsOutput: a completed turn whose tail is tool-only
// omits the field rather than sending an empty string, so an absent output
// always means "the turn produced no prose" and never "the transport lost it".
func TestPiExtToolOnlyTurnOmitsOutput(t *testing.T) {
	evs := turnEvents(runExtDriver(t, `
		handlers.agent_start({}, ctx);
		handlers.agent_end({ messages: [
			{ role: "assistant", content: [{ type: "toolCall", name: "bash", arguments: {} }], stopReason: "stop" },
		] }, ctx);
		handlers.agent_settled({}, ctx);
	`))
	end := evs[len(evs)-1]
	if end["outcome"] != "completed" {
		t.Fatalf("end = %v", end)
	}
	if _, ok := end["output"]; ok {
		t.Errorf("tool-only turn carried an output: %v", end)
	}
}

// TestPiExtOversizedOutputStillCloses is the cap invariant: an output beyond the
// source cap is truncated and flagged, and the close itself still lands. Losing
// the close would leave the session semantically active forever.
func TestPiExtOversizedOutputStillCloses(t *testing.T) {
	evs := turnEvents(runExtDriver(t, `
		handlers.agent_start({}, ctx);
		handlers.agent_end({ messages: [
			{ role: "assistant", content: [{ type: "text", text: "x".repeat(300 * 1024) }], stopReason: "stop" },
		] }, ctx);
		handlers.agent_settled({}, ctx);
	`))
	end := evs[len(evs)-1]
	if end["outcome"] != "completed" || end["truncated"] != true {
		t.Fatalf("end outcome=%v truncated=%v", end["outcome"], end["truncated"])
	}
	out, _ := end["output"].(string)
	if got, want := len(out), 256*1024; got != want {
		t.Errorf("output capped to %d bytes, want %d", got, want)
	}
}

// TestPiExtRebindAbandonsOpenTurn: a switch/new/resume/fork mid-turn means the
// settled event that follows belongs to a conversation gmux is no longer bound
// to, so it must not be reported as the new conversation's close.
func TestPiExtRebindAbandonsOpenTurn(t *testing.T) {
	evs := turnEvents(runExtDriver(t, `
		handlers.agent_start({}, ctx);
		handlers.session_start({ reason: "resume" }, ctx);
		handlers.agent_settled({}, ctx);
	`))
	for _, ev := range evs {
		if ev["phase"] == "end" {
			t.Fatalf("close posted across a rebind: %v", evs)
		}
	}
}

// TestPiExtUserBoundaryCap pins the live-frame user boundary budget.
func TestPiExtUserBoundaryCap(t *testing.T) {
	evs := turnEvents(runExtDriver(t, `
		handlers.before_agent_start({ prompt: "p".repeat(10000) }, ctx);
		handlers.agent_start({}, ctx);
	`))
	trigger, _ := evs[0]["trigger"].(string)
	if len(trigger) > 8192 || !strings.HasSuffix(trigger, "…") {
		t.Errorf("trigger boundary = %d bytes, suffix %q", len(trigger), trigger[max(0, len(trigger)-3):])
	}
}

// TestPiExtProseHelpers unit-tests the exported prose/cap helpers directly, so
// the block-selection rule (text blocks only, mirroring the Go renderer) is
// pinned without staging a whole run.
func TestPiExtProseHelpers(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping pi-ext behavior test")
	}
	dir := t.TempDir()
	extPath := filepath.Join(dir, "pi-ext.mjs")
	if err := os.WriteFile(extPath, extSource, 0o644); err != nil {
		t.Fatalf("materialize extension: %v", err)
	}
	driver := `
		const { assistantProse, boundedDiagnostic } = await import(process.argv[2]);
		process.stdout.write(JSON.stringify({
			blocks: assistantProse({ content: [
				{ type: "text", text: " one " },
				{ type: "thinking", text: "secret" },
				{ type: "text", text: "two" },
			]}),
			string: assistantProse({ content: "  plain  " }),
			none: assistantProse(undefined),
			collapsed: boundedDiagnostic("a\n\n b\tc  "),
		}));
	`
	driverPath := filepath.Join(dir, "driver.mjs")
	if err := os.WriteFile(driverPath, []byte(driver), 0o644); err != nil {
		t.Fatalf("write driver: %v", err)
	}
	out, err := exec.Command(nodeBin, driverPath, extPath).Output()
	if err != nil {
		t.Fatalf("node driver: %v (%s)", err, out)
	}
	var got map[string]string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	want := map[string]string{"blocks": " one \n\ntwo", "string": "  plain  ", "none": "", "collapsed": "a b c"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}
