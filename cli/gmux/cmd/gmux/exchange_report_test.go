package main

import (
	"bytes"
	"net/http"
	"strings"
	"syscall"
	"testing"

	"github.com/gmuxapp/gmux/packages/adapter"
)

func TestAgentExchangeGrammar(t *testing.T) {
	c, err := parseCLI([]string{"agent", "logs", "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if c.tailLines != 1 {
		t.Fatalf("default=%d", c.tailLines)
	}
	for _, args := range [][]string{{"agent", "status", "s1"}, {"agent", "logs", "--json", "s1"}, {"agent", "logs", "--user", "s1"}, {"agent", "prompt", "--json", "s1", "hi"}, {"wait", "--json", "s1"}} {
		if _, err := parseCLI(args); err == nil {
			t.Errorf("%v unexpectedly accepted", args)
		}
	}
	if _, err := parseCLI([]string{"agent", "logs", "-n", "0", "s1"}); err == nil || !strings.Contains(err.Error(), "exchanges") {
		t.Fatalf("bad -n: %v", err)
	}
}

func TestWaitReportAbbreviationExactRepeatsAndCaps(t *testing.T) {
	submitted := "one  two\nthree four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen seventeen eighteen nineteen twenty twenty-one"
	anchorText := "anchor two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen seventeen eighteen nineteen twenty twenty-one"
	res := waitResult{Outcome: waitOutcomeCompleted, AnchorOrdinal: 2, BaselineOrdinal: 2, Output: "ok", Exchanges: []adapter.Exchange{{Ordinal: 1, User: submitted, Iterations: 1}, {Ordinal: 2, User: anchorText}, {Ordinal: 3, User: submitted, Iterations: 2}}}
	var out bytes.Buffer
	if code := renderExchangeWait(res, false, 0, submitted, &out); code != 0 {
		t.Fatalf("exit=%d", code)
	}
	got := out.String()
	if strings.Count(got, "…") != 2 {
		t.Fatalf("expected anchor and post-baseline match abbreviated:\n%s", got)
	}
	if strings.Count(got, "[USER]: "+submitted) != 1 {
		t.Fatalf("pre-baseline identical text was abbreviated or duplicated: %q", got)
	}

	chars := strings.Repeat("界", 241)
	abbr := abbreviateUser(chars)
	if len([]rune(abbr)) != 241 || !strings.HasSuffix(abbr, "…") {
		t.Fatalf("char cap=%d %q", len([]rune(abbr)), abbr)
	}
}

func TestAgentPromptGrammarMatrix(t *testing.T) {
	invalid := [][]string{
		{"agent", "prompt", "--steer", "--follow-up", "s", "x"},
		{"agent", "prompt", "--new", "--steer", "x"},
		{"agent", "prompt", "--timeout", "1", "--timeout", "2", "s", "x"},
		{"agent", "prompt", "--timeout", "1", "--no-wait", "s", "x"},
		{"agent", "prompt", "--model", "m", "s", "x"},
		{"agent", "prompt", "--name", "n", "s", "x"},
		{"agent", "cancel", "a", "b"},
	}
	for _, args := range invalid {
		if _, err := parseCLI(args); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
	c, err := parseCLI([]string{"agent", "prompt", "s", "--timeout literal"})
	if err != nil || c.promptText == nil || *c.promptText != "--timeout literal" {
		t.Fatalf("post-ref text was parsed as flags: c=%+v err=%v", c, err)
	}
	c, err = parseCLI([]string{"agent", "prompt", "--new", "-"})
	if err != nil || !c.agentNew || c.promptText != nil {
		t.Fatalf("--new stdin form: c=%+v err=%v", c, err)
	}
}

func TestWaitVersionSkewTruncationDiagnosticsAndPartialLabels(t *testing.T) {
	var out bytes.Buffer
	stderr := captureStderr(t, func() {
		if code := reportTurnConclusion(waitResult{}, false, true, &out); code != waitExitError {
			t.Fatalf("missing outcome exit=%d", code)
		}
	})
	if out.Len() != 0 || !strings.Contains(stderr, "version skew") {
		t.Fatalf("stdout=%q stderr=%q", out.String(), stderr)
	}
	for _, tc := range []struct {
		name string
		run  func(*bytes.Buffer) int
	}{
		{"old timeout", func(out *bytes.Buffer) int {
			return renderExchangeWait(waitResult{Reason: "timeout"}, false, 3, "", out)
		}},
		{"old died", func(out *bytes.Buffer) int {
			return reportWaitResult(waitResult{Reason: "died"}, false, false, true, out)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var oldOut bytes.Buffer
			oldErr := captureStderr(t, func() {
				if code := tc.run(&oldOut); code != waitExitError {
					t.Fatalf("exit=%d", code)
				}
			})
			if oldOut.Len() != 0 || !strings.Contains(oldErr, "version skew") {
				t.Fatalf("stdout=%q stderr=%q", oldOut.String(), oldErr)
			}
		})
	}
	var predicateOut bytes.Buffer
	if code := reportWaitResult(waitResult{Reason: "matched"}, true, false, true, &predicateOut); code != waitExitOK || predicateOut.Len() != 0 {
		t.Fatalf("matched predicate exit=%d out=%q", code, predicateOut.String())
	}
	predicateErr := captureStderr(t, func() {
		if code := reportWaitResult(waitResult{Reason: "died"}, true, false, true, &predicateOut); code != waitExitError {
			t.Fatalf("died predicate exit=%d", code)
		}
	})
	if strings.Contains(predicateErr, "version skew") || !strings.Contains(predicateErr, "exited before") {
		t.Fatalf("predicate stderr=%q", predicateErr)
	}

	out.Reset()
	if code := reportTurnConclusion(waitResult{Outcome: waitOutcomeSnapshot, Exchanges: []adapter.Exchange{{User: "history"}}}, false, true, &out); code != waitExitOK || !strings.Contains(out.String(), "[USER]: history") {
		t.Fatalf("current snapshot exit=%d out=%q", code, out.String())
	}
	out.Reset()
	if code := reportTurnConclusion(waitResult{Outcome: waitOutcomeSnapshot}, false, true, &out); code != waitExitOK || out.String() != "[No exchanges yet]\n" {
		t.Fatalf("empty snapshot exit=%d out=%q", code, out.String())
	}

	out.Reset()
	res := waitResult{Outcome: waitOutcomeError, Diagnostic: "provider failed", TerminalPartial: true, Truncated: true,
		Exchanges: []adapter.Exchange{{Ordinal: 1, User: "ask", Iterations: 1, Terminal: " half\n"}}}
	if code := renderExchangeWait(res, false, 0, "", &out); code != waitExitError {
		t.Fatalf("exit=%d", code)
	}
	if got := out.String(); !strings.Contains(got, "[AGENT, partial, truncated]:  half\n") || !strings.Contains(got, "[Agent failed: provider failed]") {
		t.Fatalf("report=%q", got)
	}
}

func TestOldRunnerOutputCompatibility(t *testing.T) {
	for _, tt := range []struct {
		name string
		res  waitResult
		code int
		want string
	}{
		{"trigger exchange", waitResult{Outcome: waitOutcomeCompleted, Trigger: "old prompt", Output: "old answer"}, 0, "[USER]: old prompt\n\n[AGENT]: old answer"},
		{"unscoped truncated", waitResult{Outcome: waitOutcomeCompleted, Output: "old answer", Truncated: true}, 0, "[AGENT, compatibility, truncated]: old answer"},
		{"unscoped failed partial", waitResult{Outcome: waitOutcomeError, Output: "half", Diagnostic: "old failure"}, 1, "[AGENT, compatibility, partial]: half\n\n[Agent failed: old failure]"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			if code := renderExchangeWait(tt.res, false, 0, "", &out); code != tt.code || !strings.Contains(out.String(), tt.want) {
				t.Fatalf("exit=%d report=%q", code, out.String())
			}
		})
	}
}

func TestAgentLogsRequiresExchangeScopeMarker(t *testing.T) {
	for _, present := range []bool{true, false} {
		t.Run(map[bool]string{true: "present", false: "absent"}[present], func(t *testing.T) {
			d := startStubDaemon(t, localSession())
			d.on(func(w http.ResponseWriter, _ *http.Request) {
				if present {
					w.Header().Set(conversationScopeHeader, "exchanges")
				}
				_, _ = w.Write([]byte("[USER]: stored\n"))
			})
			var code int
			var stderr string
			stdout := captureStdout(t, func() { stderr = captureStderr(t, func() { code = cmdAgentLogs("1va8lvdv", 1) }) })
			if present {
				if code != 0 || stdout != "[USER]: stored\n" || stderr != "" {
					t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
				}
				d.mu.Lock()
				read := false
				for _, req := range d.requests {
					read = read || strings.HasSuffix(req.path, "/read")
				}
				d.mu.Unlock()
				if !read {
					t.Fatal("agent logs did not acknowledge unread")
				}
			} else if code != 1 || stdout != "" || !strings.Contains(stderr, "version skew") {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
		})
	}
}

func TestAgentLogsDelayedAcknowledgementCannotClearNewerCompletion(t *testing.T) {
	sessions := localSession()
	sessions[0].UnreadToken = "turn-1"
	d := startStubDaemon(t, sessions)
	currentToken := "turn-1"
	d.on(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/conversation"):
			w.Header().Set(conversationScopeHeader, "exchanges")
			_, _ = w.Write([]byte("[AGENT]: turn one\n"))
			// N+1 completes after logs observed N's report and token but before
			// the command sends its acknowledgement.
			currentToken = "turn-2"
		case strings.HasSuffix(r.URL.Path, "/read"):
			if r.URL.Query().Get("token") != "turn-1" || currentToken != "turn-2" {
				t.Fatalf("unexpected read schedule: %s current token=%s", r.URL.String(), currentToken)
			}
			writeErrEnvelope(w, http.StatusConflict, "result_changed", "newer result")
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	})
	var out bytes.Buffer
	if code := cmdAgentLogs("1va8lvdv", 1, &out); code != waitExitOK || out.String() != "[AGENT]: turn one\n" {
		t.Fatalf("exit=%d output=%q", code, out.String())
	}
	if currentToken != "turn-2" {
		t.Fatal("delayed agent-logs acknowledgement cleared the newer result")
	}
}

func TestAgentLogsOutputFailurePreservesUnread(t *testing.T) {
	d := startStubDaemon(t, localSession())
	d.on(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(conversationScopeHeader, "exchanges")
		_, _ = w.Write([]byte("[AGENT]: result\n"))
	})
	if code := cmdAgentLogs("1va8lvdv", 1, failingOutputWriter{}); code == 0 {
		t.Fatal("agent logs succeeded despite output failure")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, req := range d.requests {
		if strings.HasSuffix(req.path, "/read") {
			t.Fatalf("failed agent logs consumed unread: %+v", d.requests)
		}
	}
}

func TestWaitExitTaxonomyAndSignalFormatting(t *testing.T) {
	if waitExitOK != 0 || waitExitError != 1 || waitExitInterrupted != 2 {
		t.Fatalf("exit taxonomy=%d/%d/%d", waitExitOK, waitExitError, waitExitInterrupted)
	}
	if got := exitSignaled(syscall.SIGINT); got != 130 {
		t.Fatalf("SIGINT exit=%d", got)
	}
	report := string(adapter.RenderExchangeReport(adapter.ExchangeReport{Outcome: adapter.ExchangeWaitSignal}))
	if !strings.Contains(report, "[Wait interrupted; agent remains active") {
		t.Fatalf("signal report=%q", report)
	}
}

func TestWaitSessionClassification(t *testing.T) {
	for _, tc := range []struct {
		adapter string
		agent   bool
	}{{"pi", true}, {"claude", true}, {"codex", true}, {"shell", false}, {"editor", false}, {"", false}} {
		if got := isAgentSession(cliSession{Adapter: tc.adapter}); got != tc.agent {
			t.Errorf("adapter %q agent=%v, want %v", tc.adapter, got, tc.agent)
		}
	}
}

func TestWaitRunnerLossUsesSessionAppropriateLanguage(t *testing.T) {
	res := waitResult{Outcome: waitOutcomeError, Cause: causeRunnerDied}
	for _, tc := range []struct {
		agent bool
		want  string
	}{{true, "[Agent failed: agent activity was lost]"}, {false, "[Session activity failed: session process exited before the activity completed]"}} {
		var out bytes.Buffer
		if code := renderWait(res, false, 0, "", tc.agent, &out); code != waitExitError || !strings.Contains(out.String(), tc.want) {
			t.Errorf("agent=%v exit=%d report=%q, want %q", tc.agent, code, out.String(), tc.want)
		}
	}
}

func TestWaitRenderingUsesSessionAppropriateLanguage(t *testing.T) {
	outcomes := []struct {
		outcome      string
		agentMarker  string
		sharedMarker string
		code         int
	}{
		{waitOutcomeSnapshot, "[No exchanges yet]", "[Session inactive]", 0},
		{waitOutcomeCompleted, "[AGENT]: done", "[Session activity completed]", 0},
		{waitOutcomeInterrupted, "[Agent interrupted]", "[Session activity interrupted]", 2},
		{waitOutcomeError, "[Agent failed: provider failed]", "[Session activity failed: provider failed]", 1},
		{outcomeTimeout, "[Wait timed out after 9s; agent active", "[Wait timed out after 9s; session remains active]", 1},
	}
	for _, tc := range outcomes {
		for _, agent := range []bool{false, true} {
			name := map[bool]string{false: "command", true: "agent"}[agent] + "/" + tc.outcome
			t.Run(name, func(t *testing.T) {
				var out bytes.Buffer
				res := waitResult{Outcome: tc.outcome, Diagnostic: "provider failed"}
				if agent && tc.outcome != waitOutcomeSnapshot {
					res.Output = "done"
					res.Exchanges = []adapter.Exchange{{User: "ask", Iterations: 1}}
				}
				if code := renderWait(res, false, 9, "", agent, &out); code != tc.code {
					t.Fatalf("exit=%d want=%d", code, tc.code)
				}
				marker := tc.sharedMarker
				if agent {
					marker = tc.agentMarker
				}
				if !strings.Contains(out.String(), marker) {
					t.Fatalf("report=%q, want %q", out.String(), marker)
				}
				if !agent && strings.Contains(strings.ToLower(out.String()), "agent") {
					t.Fatalf("command report uses agent language: %q", out.String())
				}
			})
		}
	}
}

func TestWaitReportsUseStdoutForEveryDomainOutcomeAndQuiet(t *testing.T) {
	for _, tt := range []struct {
		outcome string
		code    int
		marker  string
		prose   string
	}{{waitOutcomeCompleted, 0, "[AGENT]: done", "[AGENT]: done"}, {waitOutcomeInterrupted, 2, "[Agent interrupted]", "[AGENT, partial]: done"}, {waitOutcomeError, 1, "[Agent failed:", "[AGENT, partial]: done"}, {outcomeTimeout, 1, "[Wait timed out", "[AGENT, partial]: done"}} {
		var out bytes.Buffer
		res := waitResult{Outcome: tt.outcome, Output: "done", Exchanges: []adapter.Exchange{{User: "ask", Iterations: 1}}}
		if code := renderExchangeWait(res, false, 9, "", &out); code != tt.code {
			t.Errorf("%s exit=%d", tt.outcome, code)
		}
		if !strings.Contains(out.String(), tt.marker) || !strings.Contains(out.String(), tt.prose) {
			t.Errorf("%s report=%q", tt.outcome, out.String())
		}
		out.Reset()
		if code := renderExchangeWait(res, true, 9, "", &out); code != tt.code || out.Len() != 0 {
			t.Errorf("quiet %s exit=%d out=%q", tt.outcome, code, out.String())
		}
	}
}
