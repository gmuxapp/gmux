package main

// wait_test.go pins ADR 0027 §8/§11 on the CLI side: `gmux wait` reports the
// turn's conclusion, prints the agent's answer only for a normal completion,
// and every exit path speaks the global 0/1/2 taxonomy.

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The taxonomy itself. A test that merely checks each path's number would pass
// after someone reintroduced 3 for a new path, so this asserts the closed set.
func TestWaitExitTaxonomyIsClosed(t *testing.T) {
	if waitExitOK != 0 || waitExitError != 1 || waitExitInterrupted != 2 {
		t.Fatalf("taxonomy drifted: ok=%d error=%d interrupted=%d",
			waitExitOK, waitExitError, waitExitInterrupted)
	}
}

// The conclusion → (exit, stdout, stderr) matrix. The result is printed for a
// completion and for nothing else: an error, an interruption or a death would
// otherwise present a previous turn's answer as this turn's.
func TestWaitReportsConclusionAndSuppressesStaleResults(t *testing.T) {
	sess := cliSession{ID: "sess-abcd1234", Adapter: "pi", Alive: true}
	for _, tc := range []struct {
		name       string
		res        waitResult
		quiet      bool
		wantExit   int
		wantStdout string
		wantErrSub string
	}{
		{name: "completed prints the result",
			res:      waitResult{Reason: "idle", Outcome: "completed", Output: "All green."},
			wantExit: waitExitOK, wantStdout: "All green.\n"},
		{name: "completed multi-line is untruncated",
			res:      waitResult{Reason: "idle", Outcome: "completed", Output: "line one\nline two\n\nline four"},
			wantExit: waitExitOK, wantStdout: "line one\nline two\n\nline four\n"},
		{name: "quiet suppresses the result",
			res:   waitResult{Reason: "idle", Outcome: "completed", Output: "All green."},
			quiet: true, wantExit: waitExitOK},
		{name: "completed with nothing to render is quiet but successful",
			res:      waitResult{Reason: "idle", Outcome: "completed"},
			wantExit: waitExitOK},
		{name: "interrupted prints no result",
			res:      waitResult{Reason: "idle", Outcome: "interrupted", Output: "stale prior answer"},
			wantExit: waitExitInterrupted, wantErrSub: "interrupted"},
		{name: "terminal error prints no result",
			res:      waitResult{Reason: "idle", Outcome: "error", Output: "stale prior answer"},
			wantExit: waitExitError, wantErrSub: "ended in an error"},
		{name: "error names its cause",
			res:      waitResult{Reason: "idle", Outcome: "error", Cause: "runner_died"},
			wantExit: waitExitError, wantErrSub: "runner_died"},
		{name: "death prints no result",
			res:      waitResult{Reason: "died", Outcome: "error", Cause: "runner_died", Output: "stale prior answer"},
			wantExit: waitExitError, wantErrSub: "died before its turn ended"},
		{name: "unknown outcome fails loudly",
			res:      waitResult{Reason: "idle", Outcome: "wat"},
			wantExit: waitExitError, wantErrSub: "unexpected turn outcome"},
		{name: "missing outcome is version skew, not success",
			res:      waitResult{Reason: "idle"},
			wantExit: waitExitError, wantErrSub: "predates the turn conclusions 'gmux wait' needs"},
		{name: "unknown reason fails loudly",
			res:      waitResult{Reason: "wat"},
			wantExit: waitExitError, wantErrSub: "unexpected wait reason"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var code int
			stderr := captureStderr(t, func() {
				code = reportWaitResult(sess, "gmux wait", tc.res, false, tc.quiet, &stdout)
			})
			if code != tc.wantExit {
				t.Errorf("exit = %d, want %d (stderr=%q)", code, tc.wantExit, stderr)
			}
			if got := stdout.String(); got != tc.wantStdout {
				t.Errorf("stdout = %q, want %q", got, tc.wantStdout)
			}
			if tc.wantErrSub != "" && !strings.Contains(stderr, tc.wantErrSub) {
				t.Errorf("stderr = %q, want it to mention %q", stderr, tc.wantErrSub)
			}
			// Whatever happened, a non-completed turn's stdout stays empty:
			// this is the suppression rule, stated once more as an invariant.
			if tc.res.Outcome != "completed" && stdout.Len() != 0 {
				t.Errorf("non-completed conclusion wrote %q to stdout", stdout.String())
			}
		})
	}
}

// Predicate waits are synchronization-only and never print an agent result,
// even if a daemon were to attach one.
func TestWaitPredicateStaysQuiet(t *testing.T) {
	sess := cliSession{ID: "sess-abcd1234", Adapter: "pi", Alive: true}
	var stdout bytes.Buffer
	code := reportWaitResult(sess, "gmux wait", waitResult{Reason: "matched", Output: "not mine to print"}, true, false, &stdout)
	if code != waitExitOK || stdout.Len() != 0 {
		t.Fatalf("matched: exit=%d stdout=%q", code, stdout.String())
	}
	stdout.Reset()
	stderr := captureStderr(t, func() {
		code = reportWaitResult(sess, "gmux wait", waitResult{Reason: "died"}, true, false, &stdout)
	})
	if code != waitExitError || stdout.Len() != 0 || !strings.Contains(stderr, "before its output matched") {
		t.Fatalf("died: exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr)
	}
	// A predicate wait must never be handed a turn conclusion; if the daemon
	// answers "idle" here it answered a different question.
	stdout.Reset()
	stderr = captureStderr(t, func() {
		code = reportWaitResult(sess, "gmux wait", waitResult{Reason: "idle", Outcome: "completed", Output: "x"}, true, false, &stdout)
	})
	if code != waitExitError || stdout.Len() != 0 || !strings.Contains(stderr, "output condition") {
		t.Fatalf("idle on a predicate wait: exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr)
	}
}

// End to end over the real socket: the flags map onto the wait query, the
// completion result reaches stdout, and a timeout is an error (exit 1), not a
// code of its own any more.
func TestWaitEndToEndAgainstAStubDaemon(t *testing.T) {
	t.Run("completed prints the result", func(t *testing.T) {
		d := startStubDaemon(t, localSession())
		d.on(func(w http.ResponseWriter, r *http.Request) {
			writeEnvelope(w, http.StatusOK, map[string]any{
				"reason": "idle", "outcome": "completed", "output": "All green.",
			})
		})
		var code int
		out := captureStdout(t, func() { code = cmdWait("sess-abcd1234", 30, "", "", false) })
		if code != waitExitOK || out != "All green.\n" {
			t.Fatalf("exit=%d stdout=%q", code, out)
		}
		req := d.lastRequest(t)
		if req.method != http.MethodPost || req.path != "/v1/sessions/sess-abcd1234/wait" || req.query != "timeout=30" {
			t.Fatalf("request = %+v", req)
		}
	})
	t.Run("quiet stays silent", func(t *testing.T) {
		d := startStubDaemon(t, localSession())
		d.on(func(w http.ResponseWriter, r *http.Request) {
			writeEnvelope(w, http.StatusOK, map[string]any{
				"reason": "idle", "outcome": "completed", "output": "All green.",
			})
		})
		var code int
		out := captureStdout(t, func() { code = cmdWait("sess-abcd1234", 0, "", "", true) })
		if code != waitExitOK || out != "" {
			t.Fatalf("exit=%d stdout=%q", code, out)
		}
	})
	t.Run("timeout is exit 1", func(t *testing.T) {
		d := startStubDaemon(t, localSession())
		d.on(func(w http.ResponseWriter, r *http.Request) {
			writeErrEnvelope(w, http.StatusRequestTimeout, "timeout", "session did not become idle within timeout")
		})
		var code int
		stderr := captureStderr(t, func() { code = cmdWait("sess-abcd1234", 5, "", "", false) })
		if code != waitExitError {
			t.Fatalf("exit=%d stderr=%q", code, stderr)
		}
		if !strings.Contains(stderr, "timed out after 5s") {
			t.Fatalf("stderr=%q", stderr)
		}
	})
	t.Run("interrupted is exit 2 with no stdout", func(t *testing.T) {
		d := startStubDaemon(t, localSession())
		d.on(func(w http.ResponseWriter, r *http.Request) {
			writeEnvelope(w, http.StatusOK, map[string]any{
				"reason": "idle", "outcome": "interrupted", "output": "stale",
			})
		})
		var code int
		var stderr string
		out := captureStdout(t, func() {
			stderr = captureStderr(t, func() { code = cmdWait("sess-abcd1234", 0, "", "", false) })
		})
		if code != waitExitInterrupted || out != "" {
			t.Fatalf("exit=%d stdout=%q stderr=%q", code, out, stderr)
		}
	})
	t.Run("predicate wait sends its condition and prints nothing", func(t *testing.T) {
		d := startStubDaemon(t, localSession())
		d.on(func(w http.ResponseWriter, r *http.Request) {
			writeEnvelope(w, http.StatusOK, map[string]any{"reason": "matched"})
		})
		var code int
		out := captureStdout(t, func() { code = cmdWait("sess-abcd1234", 0, "BUILD DONE", "", false) })
		if code != waitExitOK || out != "" {
			t.Fatalf("exit=%d stdout=%q", code, out)
		}
		if req := d.lastRequest(t); req.query != "for_text=BUILD+DONE" {
			t.Fatalf("query=%q", req.query)
		}
	})
	t.Run("transport-level failures are exit 1", func(t *testing.T) {
		for _, status := range []int{http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError} {
			d := startStubDaemon(t, localSession())
			d.on(func(w http.ResponseWriter, r *http.Request) {
				writeErrEnvelope(w, status, "nope", "no")
			})
			var code int
			captureStderr(t, func() { code = cmdWait("sess-abcd1234", 0, "", "", false) })
			if code != waitExitError {
				t.Fatalf("status %d gave exit %d", status, code)
			}
		}
	})
}

// --quiet parses, and only as a flag before or after the ref (the existing
// interspersed convention).
func TestParseWaitQuiet(t *testing.T) {
	for _, args := range [][]string{{"--quiet", "s1"}, {"s1", "--quiet"}} {
		c, err := parseWait(args)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !c.quiet || c.ref != "s1" {
			t.Fatalf("%v: quiet=%v ref=%q", args, c.quiet, c.ref)
		}
	}
	c, err := parseWait([]string{"s1"})
	if err != nil || c.quiet {
		t.Fatalf("default: quiet=%v err=%v", c.quiet, err)
	}
}

// The retired codes must not come back — neither under their old names nor as
// a bare literal. A grep-shaped test is unusual, but the failure it guards
// against is textual: a NEW exit path choosing 3 (or reviving
// `waitExitTimeout`) restores the per-verb dialect the taxonomy exists to
// remove, and no behavioural test over the EXISTING paths would notice.
//
// The literal check covers gmux's own verdicts only. Child/editor exit-code
// pass-through returns a variable (`exitCode`), never a literal, so it is
// unaffected; if a pass-through path ever needs a literal, it must be listed
// here deliberately rather than silently allowed.
func TestNoRetiredExitCodesRemain(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	banned := []string{"waitExitTimeout", "waitExitDied", "agentExitTimeout"}
	// `return 3`, `return 3 // …`, `os.Exit(3)` — an exit verdict of 3 in any
	// spelling the package actually uses.
	literalThree := regexp.MustCompile(`(?m)(?:return\s+3\s*(?://.*)?$|os\.Exit\(\s*3\s*\))`)
	checked := 0
	for _, f := range files {
		if f == "wait_test.go" {
			continue // this file names them on purpose
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for _, name := range banned {
			if bytes.Contains(src, []byte(name)) {
				t.Errorf("%s still references %s; the taxonomy is 0/1/2 (ADR 0027 §8)", f, name)
			}
		}
		if strings.HasSuffix(f, "_test.go") {
			// Tests may legitimately assert on the number 3 (e.g. a child's exit
			// code); only production exit paths are constrained.
			continue
		}
		if loc := literalThree.FindString(string(src)); loc != "" {
			t.Errorf("%s exits with a literal 3 (%q); gmux's own verdicts are 0/1/2 (ADR 0027 §8)",
				f, strings.TrimSpace(loc))
		}
	}
	if checked == 0 {
		// A glob that matched nothing would make this test vacuously green.
		t.Fatal("scanned no files")
	}
}

// The hint must name a verb that can actually answer. `gmux agent status` is
// meaningless for a failed one-shot command or a Claude/Codex session (it 404s
// there), which is exactly the population that now exits 1 because a non-zero
// child exit closes its lifetime turn with Error=true.
func TestWaitHintMatchesWhatCanBeRead(t *testing.T) {
	sess := cliSession{ID: "sess-abcd1234", Alive: false}
	var stdout bytes.Buffer
	for _, tc := range []struct {
		name       string
		res        waitResult
		wantSub    string
		notWantSub string
	}{
		{"readable agent points at agent status",
			waitResult{Reason: "idle", Outcome: "error", ConversationReadable: true},
			"gmux agent status abcd1234", "gmux tail"},
		{"failed one-shot points at tail",
			waitResult{Reason: "idle", Outcome: "error"},
			"gmux tail abcd1234", "gmux agent status"},
		{"interrupted shell points at tail",
			waitResult{Reason: "idle", Outcome: "interrupted"},
			"gmux tail abcd1234", "gmux agent status"},
		{"interrupted agent points at agent status",
			waitResult{Reason: "idle", Outcome: "interrupted", ConversationReadable: true},
			"gmux agent status abcd1234", "gmux tail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout.Reset()
			stderr := captureStderr(t, func() { reportWaitResult(sess, "gmux wait", tc.res, false, false, &stdout) })
			if !strings.Contains(stderr, tc.wantSub) {
				t.Errorf("stderr = %q, want it to mention %q", stderr, tc.wantSub)
			}
			if strings.Contains(stderr, tc.notWantSub) {
				t.Errorf("stderr = %q, must not mention %q", stderr, tc.notWantSub)
			}
		})
	}
}

// `send --wait` shares `gmux wait`'s exit mapping — an intentionally
// interrupted turn must not exit 0 through the composition the docs call
// preferred — while printing no result, because raw input makes no claim about
// which agent turn its bytes belong to.
func TestSendWaitSharesTheExitMappingWithoutPrintingResults(t *testing.T) {
	for _, tc := range []struct {
		name string
		data map[string]any
		want int
	}{
		{"completed", map[string]any{"reason": "idle", "outcome": "completed", "output": "not mine to print"}, waitExitOK},
		{"interrupted", map[string]any{"reason": "idle", "outcome": "interrupted"}, waitExitInterrupted},
		{"error", map[string]any{"reason": "idle", "outcome": "error"}, waitExitError},
		{"died", map[string]any{"reason": "died", "outcome": "error", "cause": "runner_died"}, waitExitError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := startStubDaemon(t, localSession())
			d.on(func(w http.ResponseWriter, r *http.Request) { writeEnvelope(w, http.StatusOK, tc.data) })
			var code int
			out := captureStdout(t, func() {
				captureStderr(t, func() { code = cmdSend("sess-abcd1234", strp("go"), []string{"Enter"}, true, 0) })
			})
			if code != tc.want {
				t.Errorf("exit = %d, want %d", code, tc.want)
			}
			if out != "" {
				t.Errorf("send --wait printed %q; raw sends carry no result", out)
			}
			if req := d.lastRequest(t); !strings.Contains(req.query, "wait=idle") {
				t.Errorf("query = %q", req.query)
			}
		})
	}
	t.Run("timeout is exit 1", func(t *testing.T) {
		d := startStubDaemon(t, localSession())
		d.on(func(w http.ResponseWriter, r *http.Request) {
			writeErrEnvelope(w, http.StatusRequestTimeout, "timeout", "nope")
		})
		var code int
		captureStderr(t, func() { code = cmdSend("sess-abcd1234", strp("go"), []string{"Enter"}, true, 5) })
		if code != waitExitError {
			t.Errorf("exit = %d, want 1", code)
		}
	})
}

func strp(s string) *string { return &s }

// The version-skew diagnostic names the command the caller actually ran. Telling
// a `send --wait` user that their daemon "predates result-bearing waits" would
// describe a feature that path deliberately does not use.
func TestSkewMessageNamesTheCommand(t *testing.T) {
	sess := cliSession{ID: "sess-abcd1234"}
	var stdout bytes.Buffer
	for _, verb := range []string{"gmux wait", "gmux send --wait"} {
		stdout.Reset()
		var code int
		stderr := captureStderr(t, func() {
			code = reportWaitResult(sess, verb, waitResult{Reason: "idle"}, false, true, &stdout)
		})
		if code != waitExitError {
			t.Errorf("%s: exit = %d, want 1", verb, code)
		}
		if !strings.Contains(stderr, "'"+verb+"'") {
			t.Errorf("%s: stderr = %q, want it to name the command", verb, stderr)
		}
		// Positively pin the shared wording: naming the verb inside the OLD
		// sentence ("predates result-bearing waits") would satisfy the check
		// above while still telling a send --wait caller about a feature that
		// path does not use.
		if !strings.Contains(stderr, "predates the turn conclusions") {
			t.Errorf("%s: stderr = %q, want the shared 'turn conclusions' wording", verb, stderr)
		}
		if strings.Contains(stderr, "result-bearing") {
			t.Errorf("%s: stderr = %q, must not describe result-bearing waits", verb, stderr)
		}
		if !strings.Contains(stderr, "gmux daemon restart") {
			t.Errorf("%s: stderr = %q, want the fix", verb, stderr)
		}
	}
	// End to end on the raw path: an old daemon's bare `idle` must not read as
	// success there either.
	d := startStubDaemon(t, localSession())
	d.on(func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, http.StatusOK, map[string]any{"reason": "idle"})
	})
	var code int
	stderr := captureStderr(t, func() { code = cmdSend("sess-abcd1234", strp("go"), []string{"Enter"}, true, 0) })
	if code != waitExitError {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "predates the turn conclusions 'gmux send --wait' needs") {
		t.Fatalf("stderr = %q, want the verb-aware conclusion wording", stderr)
	}
	if strings.Contains(stderr, "result-bearing") {
		t.Fatalf("stderr = %q: the raw path must not claim result-bearing waits", stderr)
	}
}
