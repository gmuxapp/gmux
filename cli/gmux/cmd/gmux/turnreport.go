package main

// turnreport.go — output routing for a blocking wait (ADR 0027, "Output
// routing: stdout is data, stderr is the account, the exit code is the
// verdict").
//
// One type, turnResolution, is what `gmux wait` and a synchronous `gmux agent
// prompt` both resolve to, and one renderer decides where each part goes:
//
//   - COMPLETED: stdout is the answer, alone. No report, no trigger echo — a
//     script piping stdout gets bytes it can use, and a human gets no noise for
//     a thing that worked.
//   - NON-COMPLETED: stdout is empty and stderr carries a status-shaped report
//     naming the outcome, the reason, what the turn was asked to do and (for a
//     steer) what was injected. The account arrives exactly when the caller
//     needs it, with no second command — which is the whole point: the previous
//     behavior made the caller run `gmux agent status` to find out what a
//     non-zero exit meant, by which time the session may have moved on.
//   - --json: one stable envelope on stdout regardless of outcome, and NO
//     stderr report, because the envelope is the account. It releases the human
//     default from purity pressure.
//
// Every field a report or an envelope prints comes from the RELAYED TURN FACTS
// the daemon carried with the resolution — never from a fresh conversation read.
// A re-read would reopen the staleness gap the source-asserted result closes,
// and would describe a session that has moved on rather than the turn that just
// ended. `gmux agent status` is the verb that reads the tape.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Outcome words a resolution can carry. completed/error/interrupted come from
// the daemon's turn classification; the rest name resolutions that are not turn
// conclusions at all.
const (
	// outcomeSteered — a user message entered the turn this wait was bound to,
	// so the pending answer no longer answers what was asked. The turn keeps
	// running.
	outcomeSteered = "steered"
	// outcomeIndeterminate — this request injected text into a running loop and
	// the loop settled without the adapter unambiguously acknowledging it
	// (either no report matched its text, or one did but so did somebody else's
	// pending delivery). Neither "here is your answer" nor "your steer failed"
	// is assertable.
	outcomeIndeterminate = "indeterminate"
	// outcomeTimeout — the wait ended, the turn did not.
	outcomeTimeout = "timeout"
	// outcomeDied — the session went away before its turn ended.
	outcomeDied = "died"
	// outcomeAccepted — the prompt was admitted and this response carries no
	// turn conclusion (a daemon-side 202 on a waiting request). It exists so the
	// --json contract has an envelope for every terminal path: a script must never
	// have to read "no output" as an outcome.
	outcomeAccepted = "accepted"
)

// turnResolution is what one blocking wait resolved to, in the vocabulary both
// verbs share.
type turnResolution struct {
	// Outcome is the verdict word and the only thing the exit code reads.
	Outcome string
	// Reason is the finer cause when one exists (steered_again,
	// injection_unacknowledged, runner_died, a daemon error code…). It is what a
	// script matches on without parsing prose.
	Reason string
	// Output is the agent's answer, present only for a completed turn.
	Output string
	// Trigger is the excerpt of what started the turn, as the adapter asserted
	// it.
	Trigger string
	// SteeredBy is the injected excerpt that ended this wait early.
	SteeredBy string
	// Truncated says the adapter capped Output at the source.
	Truncated bool
	// Message is the daemon's own prose for a failure it named (an error code's
	// message). It is passed through rather than reworded: it encodes facts this
	// process cannot re-derive, above all whether bytes reached the agent.
	Message string
	// Note is the one actionable next step for a human — a re-arm command, an
	// inspection verb. Report-only: it is advice, not a fact about the turn, so
	// it never enters the envelope.
	Note string
	// Tail is a labeled terminal-tail excerpt, appended to the report for the
	// one failure ADR 0027 cannot source-assert: a prompt that fails before any
	// turn starts (no API key, no model), where pi paints a banner and emits no
	// event at all. Best-effort diagnosis on the account channel, where
	// impurity is allowed — and labeled as the screen so it can never be
	// mistaken for an asserted fact.
	Tail []string
}

// exitCode maps an outcome onto ADR 0027 §8's taxonomy: 0 completed, 2 for an
// intentional stop (a stop and a steer are both expected coordination rather
// than faults, and both are routinely handled differently from a failure), 1 for
// everything else.
func (r turnResolution) exitCode() int {
	switch r.Outcome {
	case waitOutcomeCompleted, outcomeAccepted:
		return waitExitOK
	case waitOutcomeInterrupted, outcomeSteered:
		return waitExitInterrupted
	default:
		return waitExitError
	}
}

// render writes one resolution to the right streams and returns the exit code.
//
// quiet suppresses the answer (pure synchronization) but never the report: the
// caller asked for no DATA, not to be lied to about what happened.
func (r turnResolution) render(stdout, stderr io.Writer, sess cliSession, verb string, quiet, asJSON bool) int {
	if asJSON {
		return r.renderJSON(stdout, stderr)
	}
	if r.Outcome == waitOutcomeCompleted || r.Outcome == outcomeAccepted {
		if quiet || r.Output == "" {
			// Nothing to print: --quiet, a tool-only turn, or a session whose
			// adapter asserts no result. Silence under exit 0 is the
			// pre-existing synchronization behavior, not a failure.
			return waitExitOK
		}
		// Verbatim, with exactly one trailing newline so the answer is usable in
		// a shell without swallowing a final one.
		if _, err := io.WriteString(stdout, strings.TrimRight(r.Output, "\n")+"\n"); err != nil {
			fmt.Fprintln(stderr, "gmux:", err)
			return waitExitError
		}
		if r.Truncated {
			fmt.Fprintf(stderr, "gmux: the answer was truncated at the agent; read it in full with 'gmux agent logs --agent -n 1 %s'\n",
				shortID(sess.ID))
		}
		return waitExitOK
	}
	r.writeReport(stderr, sess, verb)
	return r.exitCode()
}

// writeReport renders the status-shaped stderr account of a non-completed
// resolution. Every line is prefixed like every other gmux diagnostic, so a
// caller's log stays greppable and nothing here can be confused for data.
func (r turnResolution) writeReport(stderr io.Writer, sess cliSession, verb string) {
	head := r.Outcome
	if r.Reason != "" && r.Reason != r.Outcome {
		head += " (" + r.Reason + ")"
	}
	fmt.Fprintf(stderr, "gmux: %s did not complete: %s\n", verb, head)
	field := func(label, value string) {
		if value == "" {
			return
		}
		fmt.Fprintf(stderr, "gmux:   %-9s %s\n", label+":", value)
	}
	field("session", displayID(sess))
	field("trigger", quoteExcerpt(r.Trigger))
	field("injected", quoteExcerpt(r.SteeredBy))
	field("detail", r.Message)
	if len(r.Tail) > 0 {
		// Labeled as what it is: the screen, not an assertion.
		fmt.Fprintf(stderr, "gmux:   last lines of the session's terminal (not an agent report):\n")
		for _, line := range r.Tail {
			fmt.Fprintf(stderr, "gmux:   | %s\n", line)
		}
	}
	if r.Note != "" {
		fmt.Fprintf(stderr, "gmux:   %s\n", r.Note)
	}
}

// renderJSON prints the machine contract: one envelope on stdout for every
// outcome, and nothing on stderr.
func (r turnResolution) renderJSON(stdout, stderr io.Writer) int {
	env := struct {
		Outcome   string `json:"outcome"`
		Reason    string `json:"reason,omitempty"`
		Output    string `json:"output,omitempty"`
		Trigger   string `json:"trigger,omitempty"`
		SteeredBy string `json:"steered_by,omitempty"`
		Truncated bool   `json:"truncated,omitempty"`
		Message   string `json:"message,omitempty"`
	}{
		Outcome: r.Outcome, Reason: r.Reason, Output: r.Output, Trigger: r.Trigger,
		SteeredBy: r.SteeredBy, Truncated: r.Truncated, Message: r.Message,
	}
	// Omit-not-empty throughout (the struct tags), for the same reason `agent
	// status --json` does it: an absent field says "this turn has no such fact",
	// while an empty string would claim the fact exists and is blank.
	out, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		fmt.Fprintln(stderr, "gmux:", err)
		return waitExitError
	}
	if _, err := fmt.Fprintln(stdout, string(out)); err != nil {
		fmt.Fprintln(stderr, "gmux:", err)
		return waitExitError
	}
	return r.exitCode()
}

// fetchScrollbackQuiet is the tail read with no diagnostics of its own: it feeds
// a report that already names a failure, so its own failure must be silence
// rather than a second, louder error about scrollback.
func fetchScrollbackQuiet(sess cliSession, lines int) []byte {
	client := gmuxdClient()
	resp, err := client.Get(fmt.Sprintf("%s/v1/sessions/%s/scrollback?tail=%d", gmuxdBaseURL(), sess.ID, lines))
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}
	return data
}

// quoteExcerpt renders an excerpt on one line. The daemon already collapsed the
// whitespace at the source; quoting is what keeps an empty-looking or
// leading-space excerpt visible as a value rather than as a formatting accident.
func quoteExcerpt(s string) string {
	if s == "" {
		return ""
	}
	return `"` + s + `"`
}

// steerNote is the re-arm advice a steered wait ends with. It is the difference
// between "something went wrong" and "somebody redirected the work you were
// waiting on, and here is how to wait for the new answer".
func steerNote(sess cliSession) string {
	return "the turn is still running under its new instructions; wait for it again with 'gmux wait " + shortID(sess.ID) + "'"
}

// terminalTailExcerpt reads the last few lines the session painted, for the
// failed-to-start report. Best-effort by contract: any failure yields no lines
// and no diagnostic of its own, because this is a garnish on an error that has
// already been reported and a second failure message would bury it.
func terminalTailExcerpt(sess cliSession, lines int) []string {
	data := fetchScrollbackQuiet(sess, lines)
	if len(data) == 0 {
		return nil
	}
	var out []string
	for _, line := range strings.Split(strings.TrimRight(string(stripANSI(data)), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue // the TUI pads generously; blank lines carry no diagnosis
		}
		out = append(out, strings.TrimRight(line, " "))
	}
	if len(out) > lines {
		out = out[len(out)-lines:]
	}
	return out
}
