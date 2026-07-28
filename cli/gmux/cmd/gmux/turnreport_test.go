package main

// turnreport_test.go pins ADR 0027's "Output routing" at the CLI boundary: what
// lands on stdout, what lands on stderr, what the exit code says, and the shape
// of the --json envelope.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func reportSession() cliSession {
	return cliSession{ID: "sess-abcd1234", Adapter: "pi", Alive: true}
}

// A steered wait: exit 2, no stdout, and a report that carries BOTH excerpts —
// what the turn was asked to do and what changed it — plus the re-arm move.
func TestSteeredWaitReportsWhatWentIn(t *testing.T) {
	res := waitResult{
		Reason: "steered", Trigger: "run the tests", SteeredBy: "stop, fix the lint first",
	}
	var stdout bytes.Buffer
	var code int
	stderr := captureStderr(t, func() {
		code = reportWaitResult(reportSession(), "gmux wait", res, false, false, false, &stdout)
	})
	if code != waitExitInterrupted {
		t.Fatalf("exit=%d (stderr=%q)", code, stderr)
	}
	if stdout.Len() != 0 {
		t.Fatalf("a steered wait printed data: %q", stdout.String())
	}
	for _, want := range []string{
		"did not complete: steered", "run the tests", "stop, fix the lint first", "gmux wait abcd1234",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr=%q missing %q", stderr, want)
		}
	}
}

// The superseded injector gets its own word: "somebody steered the turn" and
// "your steer went in and was then overridden" call for different next moves.
func TestSupersededInjectorIsNamedAsSuch(t *testing.T) {
	res := waitResult{Reason: "steered", Cause: causeSteeredAgain, SteeredBy: "no, revert it"}
	var stdout bytes.Buffer
	code := 0
	stderr := captureStderr(t, func() {
		code = reportWaitResult(reportSession(), "gmux agent prompt", res, false, false, false, &stdout)
	})
	if code != waitExitInterrupted || !strings.Contains(stderr, causeSteeredAgain) {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
}

// An injection the loop never acknowledged is exit 1 and says why: the answer
// that turn produced is not this command's result.
func TestIndeterminateInjectionIsExitOneAndExplained(t *testing.T) {
	res := waitResult{
		Reason: "idle", Outcome: outcomeIndeterminate, Cause: causeInjectionUnacknowledged,
		Trigger: "go", Output: "pre-steer answer",
	}
	var stdout bytes.Buffer
	code := 0
	stderr := captureStderr(t, func() {
		code = reportWaitResult(reportSession(), "gmux agent prompt", res, false, false, false, &stdout)
	})
	if code != waitExitError {
		t.Fatalf("exit=%d", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("an indeterminate resolution printed an answer: %q", stdout.String())
	}
	if !strings.Contains(stderr, causeInjectionUnacknowledged) {
		t.Fatalf("stderr=%q", stderr)
	}
}

// Every non-completed resolution reports, and a completed one stays silent.
// That pairing is the contract: stderr noise on success would poison logs, and
// silence on failure is what forced the caller to run a second command.
func TestReportsExistExactlyForNonCompletedResolutions(t *testing.T) {
	for _, tc := range []struct {
		name       string
		res        waitResult
		wantReport bool
	}{
		{"completed", waitResult{Reason: "idle", Outcome: "completed", Output: "x"}, false},
		{"interrupted", waitResult{Reason: "idle", Outcome: "interrupted"}, true},
		{"error", waitResult{Reason: "idle", Outcome: "error", Cause: causeRunnerDied}, true},
		{"steered", waitResult{Reason: "steered"}, true},
		{"indeterminate", waitResult{Reason: "idle", Outcome: outcomeIndeterminate}, true},
		{"died", waitResult{Reason: "died", Outcome: "error", Cause: causeRunnerDied}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			stderr := captureStderr(t, func() {
				reportWaitResult(reportSession(), "gmux wait", tc.res, false, false, false, &stdout)
			})
			if got := strings.Contains(stderr, "did not complete:"); got != tc.wantReport {
				t.Fatalf("report=%v want %v (stderr=%q)", got, tc.wantReport, stderr)
			}
		})
	}
}

// --quiet suppresses the ANSWER, never the account: a caller who wants no data
// still must not be lied to about what happened.
func TestQuietStillReports(t *testing.T) {
	var stdout bytes.Buffer
	stderr := captureStderr(t, func() {
		reportWaitResult(reportSession(), "gmux wait", waitResult{Reason: "steered", SteeredBy: "hold on"}, false, true, false, &stdout)
	})
	if !strings.Contains(stderr, "did not complete: steered") {
		t.Fatalf("--quiet swallowed the report: %q", stderr)
	}
}

// The --json envelope: one stable shape on stdout for every outcome, nothing on
// stderr, and the exit code unchanged.
func TestJSONEnvelopeShape(t *testing.T) {
	for _, tc := range []struct {
		name     string
		res      waitResult
		wantExit int
		want     map[string]any
	}{
		{
			name:     "completed",
			res:      waitResult{Reason: "idle", Outcome: "completed", Output: "All green.", Trigger: "run the tests", Truncated: true},
			wantExit: waitExitOK,
			want: map[string]any{
				"outcome": "completed", "output": "All green.", "trigger": "run the tests", "truncated": true,
			},
		},
		{
			name:     "steered",
			res:      waitResult{Reason: "steered", Trigger: "run the tests", SteeredBy: "stop"},
			wantExit: waitExitInterrupted,
			want: map[string]any{
				"outcome": "steered", "reason": "steered", "trigger": "run the tests", "steered_by": "stop",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var code int
			stderr := captureStderr(t, func() {
				code = reportWaitResult(reportSession(), "gmux wait", tc.res, false, false, true, &stdout)
			})
			if code != tc.wantExit {
				t.Fatalf("exit=%d want %d", code, tc.wantExit)
			}
			if stderr != "" {
				t.Fatalf("--json wrote a report to stderr as well: %q", stderr)
			}
			var got map[string]any
			if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
				t.Fatalf("stdout=%q: %v", stdout.String(), err)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("envelope[%q]=%v want %v (full=%v)", k, got[k], v, got)
				}
			}
			// Omit-not-empty: a field describing a fact the turn does not have
			// must be ABSENT, not blank, exactly like `agent status --json`.
			for _, k := range []string{"output", "trigger", "steered_by", "reason", "message"} {
				if v, ok := got[k]; ok && v == "" {
					t.Fatalf("envelope carried an empty %q instead of omitting it: %v", k, got)
				}
			}
		})
	}
}

// --json and --quiet are opposite answers to the same question, so the
// combination is a usage error rather than a silent choice between them.
func TestJSONAndQuietIsAUsageError(t *testing.T) {
	if _, err := parseWait([]string{"--json", "--quiet", "s1"}); err == nil {
		t.Fatal("wait accepted --json --quiet")
	}
	if _, err := parseAgentPrompt([]string{"--json", "--no-wait", "s1", "hi"}); err == nil {
		t.Fatal("agent prompt accepted --json --no-wait, which never observes a resolution")
	}
	if _, err := parseAgentPrompt([]string{"--json", "s1", "hi"}); err != nil {
		t.Fatalf("agent prompt --json: %v", err)
	}
	if _, err := parseAgentPrompt([]string{"--json", "--json", "s1", "hi"}); err == nil {
		t.Fatal("agent prompt accepted a repeated --json")
	}
}

// The failed-to-start report (ADR 0027, "Failures before the turn starts"): pi's
// prompt submission can fail with no turn and no event at all — no API key, no
// model — so the admission timeout appends what the screen says, labeled as the
// screen. It is the one impure ingredient in this design, and it is confined to
// the account channel and to this one code.
func TestAdmissionTimeoutAppendsTheTerminalTail(t *testing.T) {
	d := startStubDaemon(t, localSession())
	d.on(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/scrollback") {
			_, _ = w.Write([]byte("some earlier line\n\nerror: model \"nope\" is not available\n"))
			return
		}
		writeErrEnvelope(w, http.StatusGatewayTimeout, codeAdmissionTimeout,
			"prompt was delivered but the agent did not start a turn within 1m0s")
	})
	text := "hi"
	var code int
	stderr := captureStderr(t, func() { code = cmdAgentPrompt("sess-abcd1234", agentModePrompt, false, 0, &text, false) })
	if code != waitExitError {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	for _, want := range []string{
		codeAdmissionTimeout,
		"not an agent report", // the label: this is the screen, not an assertion
		`error: model "nope" is not available`,
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr=%q missing %q", stderr, want)
		}
	}
	// Blank padding from the TUI carries no diagnosis and is dropped.
	if strings.Contains(stderr, "|  \n") {
		t.Fatalf("blank screen padding leaked into the report: %q", stderr)
	}

	// A failure that is NOT "the turn never started" gets no tail: it would show
	// a turn running perfectly well and invite a diagnosis of a problem that is
	// not there.
	d.on(func(w http.ResponseWriter, r *http.Request) {
		writeErrEnvelope(w, http.StatusGatewayTimeout, codeExecutionTimeout, "the turn did not complete within the requested timeout")
	})
	stderr = captureStderr(t, func() { cmdAgentPrompt("sess-abcd1234", agentModePrompt, false, 0, &text, false) })
	if strings.Contains(stderr, "not an agent report") {
		t.Fatalf("an execution timeout showed the screen: %q", stderr)
	}
}

// --json swaps the report for an envelope even for a daemon-reported failure, and
// keeps the daemon's own stable code as the reason: that code is the token a
// script would otherwise have grepped out of the stderr line.
func TestJSONEnvelopeForADaemonFailure(t *testing.T) {
	d := startStubDaemon(t, localSession())
	d.on(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/scrollback") {
			_, _ = w.Write([]byte("banner\n"))
			return
		}
		writeErrEnvelope(w, http.StatusGatewayTimeout, codeExecutionTimeout, "the turn did not complete within the requested timeout")
	})
	text := "hi"
	var code int
	var stderr string
	out := captureStdout(t, func() {
		stderr = captureStderr(t, func() { code = cmdAgentPrompt("sess-abcd1234", agentModePrompt, false, 30, &text, true) })
	})
	if code != waitExitError || stderr != "" {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("stdout=%q: %v", out, err)
	}
	if env["outcome"] != outcomeTimeout || env["reason"] != codeExecutionTimeout {
		t.Fatalf("envelope=%v", env)
	}
}

// --json must emit an envelope on EVERY terminal path. A contract that prints an
// object for failure and nothing for success forces a script to treat "empty
// stdout" as an undocumented third outcome — and to guess whether it means
// success or a crash.
func TestJSONEnvelopeOnEveryTerminalPath(t *testing.T) {
	t.Run("predicate wait success", func(t *testing.T) {
		var stdout bytes.Buffer
		var code int
		stderr := captureStderr(t, func() {
			code = reportWaitResult(reportSession(), "gmux wait",
				waitResult{Reason: "matched"}, true, false, true, &stdout)
		})
		if code != waitExitOK || stderr != "" {
			t.Fatalf("exit=%d stderr=%q", code, stderr)
		}
		var env map[string]any
		if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
			t.Fatalf("a matched predicate wait printed %q under --json: %v", stdout.String(), err)
		}
		if env["outcome"] != waitOutcomeCompleted || env["reason"] != "matched" {
			t.Fatalf("envelope=%v", env)
		}
		// The matched bytes are terminal output, not an agent result, so the
		// envelope carries no output — exactly like the human shape.
		if _, ok := env["output"]; ok {
			t.Fatalf("a predicate wait's envelope claimed an agent result: %v", env)
		}
	})

	t.Run("without --json a matched wait stays silent", func(t *testing.T) {
		var stdout bytes.Buffer
		stderr := captureStderr(t, func() {
			reportWaitResult(reportSession(), "gmux wait", waitResult{Reason: "matched"}, true, false, false, &stdout)
		})
		if stdout.Len() != 0 || stderr != "" {
			t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr)
		}
	})

	t.Run("daemon-side 202 on a waiting request", func(t *testing.T) {
		body := []byte(`{"ok":true,"data":{"admission":"delivered","resumed":false}}`)
		var code int
		var stderr string
		out := captureStdout(t, func() {
			stderr = captureStderr(t, func() {
				// noWait=false: this process WAS waiting; the daemon answered 202.
				code = reportAgentPromptSuccess(reportSession(), http.StatusAccepted, body, false, true)
			})
		})
		if code != waitExitOK || stderr != "" {
			t.Fatalf("exit=%d stderr=%q", code, stderr)
		}
		var env map[string]any
		if err := json.Unmarshal([]byte(out), &env); err != nil {
			t.Fatalf("a 202 printed %q under --json: %v", out, err)
		}
		if env["outcome"] != outcomeAccepted || env["reason"] != "delivered" {
			t.Fatalf("envelope=%v", env)
		}
		if _, ok := env["output"]; ok {
			t.Fatalf("an admission-only envelope claimed a result: %v", env)
		}
	})
}
