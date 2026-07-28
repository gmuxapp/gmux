package agentext

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gmuxapp/gmux/cli/gmux/internal/session"
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

// TestPiExtReportsInjections pins the steer/merged-follow-up report: a user
// message entering the RUNNING loop extends the open turn (it is not a new
// turn), while the loop's own opening prompt is the trigger, not an injection.
func TestPiExtReportsInjections(t *testing.T) {
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
	if _, ok := evs[1]["truncated"]; ok {
		// A whole message must not claim to be an excerpt: `truncated` is what
		// licenses the runner to match this text as a PREFIX of a delivery, so
		// asserting it here would hand a shorter message a longer delivery's
		// identity.
		t.Errorf("an uncapped injection reported truncated: %v", evs[1])
	}
	if seqOf(t, evs[1]) != seqOf(t, evs[0]) {
		t.Errorf("injection reported against another turn: %v", evs)
	}
}

// TestPiExtInjectionReportsItsOwnTruncation: the adapter is the only party that
// knows whether it capped an injection excerpt, so it says so as a FACT on the
// event. The runner's correlation grants the prefix rule on that flag alone —
// never on the text ending in an ellipsis, which a foreign message can also do.
func TestPiExtInjectionReportsItsOwnTruncation(t *testing.T) {
	// Longer than the extension's excerpt cap, so the report is genuinely a
	// prefix; and a second injection that merely ENDS in an ellipsis, which is
	// not one.
	evs := turnEvents(runExtDriver(t, `
		handlers.agent_start({}, ctx);
		handlers.message_start({ message: { role: "assistant", content: [{ type: "text", text: "working" }] } }, ctx);
		handlers.message_start({ message: { role: "user", content: "x".repeat(4000) } }, ctx);
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
	if capped["truncated"] != true {
		t.Errorf("a capped excerpt did not report its truncation: %v", capped)
	}
	if text, _ := capped["text"].(string); !strings.HasSuffix(text, "\u2026") {
		t.Errorf("a capped excerpt lost its ellipsis: %q", text)
	}
	if _, ok := plain["truncated"]; ok {
		t.Errorf("a whole message ending in an ellipsis claimed truncation: %v", plain)
	}
	if plain["text"] != "wait\u2026" {
		t.Errorf("text = %v", plain["text"])
	}
}

// TestPiExtNonCompletedCarriesNoOutput pins the polarity rule: an interrupted or
// errored turn has no answer, and an error close may carry a short diagnostic
// instead — the account channel, never the result.
func TestPiExtNonCompletedCarriesNoOutput(t *testing.T) {
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
	if _, ok := end["output"]; ok {
		t.Errorf("error close carried an output: %v", end)
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

// TestPiExtExcerptCap pins the excerpt cap on triggers (they ride stderr
// reports, so they are bounded independently of the output cap).
func TestPiExtExcerptCap(t *testing.T) {
	evs := turnEvents(runExtDriver(t, `
		handlers.before_agent_start({ prompt: "p".repeat(5000) }, ctx);
		handlers.agent_start({}, ctx);
	`))
	trigger, _ := evs[0]["trigger"].(string)
	if len(trigger) > 1024 || !strings.HasSuffix(trigger, "…") {
		t.Errorf("trigger excerpt = %d bytes, suffix %q", len(trigger), trigger[max(0, len(trigger)-3):])
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
		const { assistantProse, excerpt } = await import(process.argv[2]);
		process.stdout.write(JSON.stringify({
			blocks: assistantProse({ content: [
				{ type: "text", text: " one " },
				{ type: "thinking", text: "secret" },
				{ type: "text", text: "two" },
			]}),
			string: assistantProse({ content: "  plain  " }),
			none: assistantProse(undefined),
			collapsed: excerpt("a\n\n b\tc  "),
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
	want := map[string]string{"blocks": "one\n\ntwo", "string": "plain", "none": "", "collapsed": "a b c"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

// TestPiExtExcerptWhitespaceMatchesRunner pins the whitespace class as a
// CROSS-RUNTIME CONTRACT rather than as each side's local habit.
//
// The runner decides whether an injection belongs to a given gmux request by
// comparing the adapter's excerpt against the same normalization of the text it
// delivered (ADR 0027's steer self-exclusion). The two runtimes disagree out of
// the box — JavaScript's \s matches U+FEFF but not U+0085, Go's unicode.IsSpace
// the reverse — so a prompt containing either character would normalize
// differently on the two sides, fail to correlate, and silently downgrade its
// injector to an indeterminate result. Both sides collapse the union; this test
// is what keeps them collapsing the SAME union.
func TestPiExtExcerptWhitespaceMatchesRunner(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping pi-ext behavior test")
	}
	dir := t.TempDir()
	extPath := filepath.Join(dir, "pi-ext.mjs")
	if err := os.WriteFile(extPath, extSource, 0o644); err != nil {
		t.Fatalf("materialize extension: %v", err)
	}
	// One case per character the two runtimes disagree about, plus the ordinary
	// ones both already agreed on.
	inputs := []string{
		"a\n\n b\tc  ",
		"bom\uFEFFhere", // JS \s only
		"nel\u0085here", // Go IsSpace only
		"mixed\uFEFF\u0085 x",
		"\uFEFFleading and trailing\u0085",
	}
	driver := `
		const { excerpt } = await import(process.argv[2]);
		process.stdout.write(JSON.stringify(JSON.parse(process.argv[3]).map(excerpt)));
	`
	driverPath := filepath.Join(dir, "driver.mjs")
	if err := os.WriteFile(driverPath, []byte(driver), 0o644); err != nil {
		t.Fatalf("write driver: %v", err)
	}
	payload, err := json.Marshal(inputs)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(nodeBin, driverPath, extPath, string(payload)).Output()
	if err != nil {
		t.Fatalf("node driver: %v (%s)", err, out)
	}
	var got []string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if len(got) != len(inputs) {
		t.Fatalf("got %d excerpts for %d inputs", len(got), len(inputs))
	}
	for i, in := range inputs {
		if want := session.NormalizeExcerpt(in); got[i] != want {
			t.Errorf("excerpt(%q) = %q (node), but the runner normalizes it to %q; "+
				"the correlation compares these two strings, so they must agree", in, got[i], want)
		}
	}
}
