package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestParseAgent pins the `gmux agent` grammar: flags before the ref,
// verbatim prompt after it, mutual exclusion of the delivery-shaping flags,
// and a usage error for every shape that would otherwise guess at intent.
func TestParseAgent(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		check func(t *testing.T, c *command)
	}{
		{name: "plain prompt", args: []string{"agent", "prompt", "s1", "do it"},
			check: func(t *testing.T, c *command) {
				if c.agentSub != "prompt" || c.agentMode != agentModePrompt {
					t.Errorf("sub=%q mode=%q", c.agentSub, c.agentMode)
				}
				if c.ref != "s1" {
					t.Errorf("ref = %q", c.ref)
				}
				if c.promptText == nil || *c.promptText != "do it" {
					t.Errorf("promptText = %v", c.promptText)
				}
				if c.agentNoWait || c.timeout != 0 {
					t.Errorf("unexpected no-wait/timeout: %v %d", c.agentNoWait, c.timeout)
				}
			}},
		{name: "follow-up mode", args: []string{"agent", "prompt", "--follow-up", "s1", "then this"},
			check: func(t *testing.T, c *command) {
				if c.agentMode != agentModeFollowUp {
					t.Errorf("mode = %q", c.agentMode)
				}
			}},
		{name: "steer mode", args: []string{"agent", "prompt", "--steer", "s1", "no, that"},
			check: func(t *testing.T, c *command) {
				if c.agentMode != agentModeSteer {
					t.Errorf("mode = %q", c.agentMode)
				}
			}},
		{name: "no-wait keeps plain mode", args: []string{"agent", "prompt", "--no-wait", "s1", "go"},
			check: func(t *testing.T, c *command) {
				if !c.agentNoWait || c.agentMode != agentModePrompt {
					t.Errorf("noWait=%v mode=%q", c.agentNoWait, c.agentMode)
				}
			}},
		// --no-wait shapes the WAIT, the mode flags shape the DELIVERY, so
		// they compose: "redirect the running turn and don't block" is a real
		// thing a parent agent wants, and the wire model already carries it.
		{name: "no-wait composes with steer", args: []string{"agent", "prompt", "--no-wait", "--steer", "s1", "go"},
			check: func(t *testing.T, c *command) {
				if !c.agentNoWait || c.agentMode != agentModeSteer {
					t.Errorf("noWait=%v mode=%q", c.agentNoWait, c.agentMode)
				}
			}},
		{name: "no-wait composes with follow-up in either order", args: []string{"agent", "prompt", "--follow-up", "--no-wait", "s1", "go"},
			check: func(t *testing.T, c *command) {
				if !c.agentNoWait || c.agentMode != agentModeFollowUp || c.timeout != 0 {
					t.Errorf("noWait=%v mode=%q timeout=%d", c.agentNoWait, c.agentMode, c.timeout)
				}
			}},
		{name: "timeout separate value", args: []string{"agent", "prompt", "--timeout", "30", "s1", "go"},
			check: func(t *testing.T, c *command) {
				if c.timeout != 30 {
					t.Errorf("timeout = %d", c.timeout)
				}
			}},
		{name: "timeout equals form", args: []string{"agent", "prompt", "--timeout=5", "s1", "go"},
			check: func(t *testing.T, c *command) {
				if c.timeout != 5 {
					t.Errorf("timeout = %d", c.timeout)
				}
			}},
		{name: "timeout zero is indefinite", args: []string{"agent", "prompt", "--timeout=0", "s1", "go"},
			check: func(t *testing.T, c *command) {
				if c.timeout != 0 {
					t.Errorf("timeout = %d", c.timeout)
				}
			}},
		{name: "prompt omitted means stdin", args: []string{"agent", "prompt", "s1"},
			check: func(t *testing.T, c *command) {
				if c.promptText != nil {
					t.Errorf("promptText = %v, want nil (stdin)", *c.promptText)
				}
			}},
		{name: "dashed text after the ref is verbatim", args: []string{"agent", "prompt", "s1", "--steer"},
			check: func(t *testing.T, c *command) {
				if c.agentMode != agentModePrompt {
					t.Errorf("mode = %q, flag after the ref must be text", c.agentMode)
				}
				if c.promptText == nil || *c.promptText != "--steer" {
					t.Errorf("promptText = %v", c.promptText)
				}
			}},
		{name: "double dash guards a dashed ref", args: []string{"agent", "prompt", "--", "s1", "go"},
			check: func(t *testing.T, c *command) {
				if c.ref != "s1" || c.promptText == nil || *c.promptText != "go" {
					t.Errorf("ref=%q text=%v", c.ref, c.promptText)
				}
			}},
		{name: "cancel", args: []string{"agent", "cancel", "s1"},
			check: func(t *testing.T, c *command) {
				if c.agentSub != "cancel" || c.ref != "s1" {
					t.Errorf("sub=%q ref=%q", c.agentSub, c.ref)
				}
			}},
		{name: "status", args: []string{"agent", "status", "abc123"},
			check: func(t *testing.T, c *command) {
				if c.agentSub != "status" || c.ref != "abc123" {
					t.Errorf("sub=%q ref=%q", c.agentSub, c.ref)
				}
			}},
		{name: "peer-qualified ref parses (rejected at execution)", args: []string{"agent", "status", "abc@laptop"},
			check: func(t *testing.T, c *command) {
				if c.ref != "abc@laptop" {
					t.Errorf("ref = %q", c.ref)
				}
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := parseCLI(tt.args)
			if err != nil {
				t.Fatalf("parseCLI(%v) error: %v", tt.args, err)
			}
			if c.mode != modeAgent {
				t.Fatalf("mode = %v, want modeAgent", c.mode)
			}
			tt.check(t, c)
		})
	}
}

// TestParseAgentErrors covers every rejected shape. Each of these has a
// plausible-looking wrong behavior (guessing a mode, joining words into a
// prompt, ignoring a flag) that would execute something the caller did not
// ask for.
func TestParseAgentErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"unknown verb", []string{"agent", "chat", "s1"}, "unknown agent verb"},
		{"prompt without ref", []string{"agent", "prompt"}, "requires a session id"},
		{"prompt with only flags", []string{"agent", "prompt", "--steer"}, "requires a session id"},
		{"steer and follow-up", []string{"agent", "prompt", "--steer", "--follow-up", "s1", "x"}, "mutually exclusive"},
		{"follow-up and steer", []string{"agent", "prompt", "--follow-up", "--steer", "s1", "x"}, "mutually exclusive"},
		// A repeated flag is a repetition, not a conflict: "--steer and
		// --steer are mutually exclusive" would be nonsense.
		{"repeated steer", []string{"agent", "prompt", "--steer", "--steer", "s1", "x"}, "given more than once"},
		{"repeated no-wait", []string{"agent", "prompt", "--no-wait", "--no-wait", "s1", "x"}, "given more than once"},
		// Last-wins on a repeated --timeout would let two generated arguments
		// disagree with the loser invisible.
		{"repeated timeout", []string{"agent", "prompt", "--timeout=5", "--timeout=9", "s1", "x"}, "given more than once"},
		{"unknown flag", []string{"agent", "prompt", "--quiet", "s1", "x"}, "unknown flag"},
		// --timeout bounds the wait, so with --no-wait it bounds nothing.
		// Silently ignoring it would let a caller believe they had capped a
		// call that in fact waits on a fixed admission window.
		{"no-wait with timeout", []string{"agent", "prompt", "--no-wait", "--timeout=5", "s1", "x"}, "cannot be combined with --no-wait"},
		{"timeout then no-wait", []string{"agent", "prompt", "--timeout", "5", "--no-wait", "s1", "x"}, "cannot be combined with --no-wait"},
		{"timeout without value", []string{"agent", "prompt", "--timeout"}, "--timeout requires"},
		{"negative timeout", []string{"agent", "prompt", "--timeout=-1", "s1", "x"}, "non-negative"},
		{"non-numeric timeout", []string{"agent", "prompt", "--timeout=soon", "s1", "x"}, "non-negative"},
		{"multiple prompt words", []string{"agent", "prompt", "s1", "do", "it"}, "single prompt argument"},
		{"cancel without ref", []string{"agent", "cancel"}, "requires a session id"},
		{"cancel with flags", []string{"agent", "cancel", "--force", "s1"}, "takes no flags"},
		// The message must name the real problem: a second id was given, not
		// zero ids; and a trailing flag is a misplaced flag, not a missing id.
		{"cancel with extra ref", []string{"agent", "cancel", "s1", "s2"}, "exactly one session id"},
		// The removed read verb fails by name, and the error names BOTH
		// replacements: the report and the answer-only read.
		{"output is removed", []string{"agent", "output", "s1"}, "gmux agent status <id>"},
		{"output names the answer read", []string{"agent", "output", "s1"}, "logs --agent -n 1"},
		{"status without ref", []string{"agent", "status"}, "requires a session id"},
		{"status with extra ref", []string{"agent", "status", "s1", "s2"}, "exactly one session id"},
		{"status unknown flag", []string{"agent", "status", "--raw", "s1"}, "agent status:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseCLI(tt.args)
			if err == nil {
				t.Fatalf("parseCLI(%v) = nil error", tt.args)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

// TestParseAgentHelp pins the help routing: the namespace, each verb, and the
// `gmux help agent ...` spelling all reach the agent help rather than the
// generic usage.
func TestParseAgentHelp(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"agent"}, "agent"}, // bare namespace is a question, not a mistake
		{[]string{"agent", "--help"}, "agent"},
		{[]string{"agent", "-h"}, "agent"},
		{[]string{"agent", "help"}, "agent"},
		{[]string{"agent", "?"}, "agent"},
		{[]string{"agent", "prompt", "?"}, "agent prompt"},
		{[]string{"agent", "cancel", "?"}, "agent cancel"},
		{[]string{"agent", "prompt", "--help"}, "agent prompt"},
		{[]string{"agent", "cancel", "--help"}, "agent cancel"},
		{[]string{"agent", "status", "-h"}, "agent status"},
		{[]string{"agent", "logs", "--help"}, "agent logs"},
		{[]string{"help", "agent"}, "agent"},
		{[]string{"help", "agent", "prompt"}, "agent prompt"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			c, err := parseCLI(tt.args)
			if err != nil {
				t.Fatalf("parseCLI: %v", err)
			}
			if c.mode != modeHelp || c.helpTopic != tt.want {
				t.Fatalf("mode=%v topic=%q, want modeHelp/%q", c.mode, c.helpTopic, tt.want)
			}
		})
	}
	// The namespace guide is the one dedicated help page for the domain: it
	// must cover the three verbs, the prompt modes, results/exit codes, the
	// safety boundary of retries, the scope, and the raw-send escape hatch.
	var b strings.Builder
	printAgentUsage(&b, "agent")
	guide := b.String()
	for _, want := range []string{
		"agent prompt", "agent cancel", "agent status", "agent logs",
		"--no-wait", "--follow-up", "--steer", "--timeout",
		"stdin",
		"0 completed", "2 intentionally interrupted",
		"indeterminate", "admission_timeout", "transport_error",
		"safe to retry", "incarnation_mismatch",
		"store-only snapshot", "never start or resume",
		"this host only", "pi only",
		"gmux send", "gmux tail",
		"--help",
	} {
		if !strings.Contains(guide, want) {
			t.Errorf("namespace guide missing %q:\n%s", want, guide)
		}
	}
	b.Reset()
	printAgentUsage(&b, "agent prompt")
	for _, want := range []string{"--no-wait", "--follow-up", "--steer", "--timeout", "gmux agent status"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("prompt help missing %q:\n%s", want, b.String())
		}
	}
	// `agent` is reserved so a typo suggests it instead of trying to run it.
	if v := didYouMean("agen"); v != "agent" {
		t.Errorf("didYouMean(agen) = %q, want agent", v)
	}
}

// TestTopLevelUsageAgentSection pins the top-level presentation of the agent
// namespace: a prominent section surfacing the one command a first-time
// caller wants (prompt) plus the pointer to the namespace guide — and
// nothing else. Per-verb flag detail and the management verbs (cancel,
// status) live in 'gmux agent --help', keeping the synopsis scannable.
func TestTopLevelUsageAgentSection(t *testing.T) {
	var b strings.Builder
	printUsage(&b)
	usage := b.String()

	var agentLines []string
	for _, line := range strings.Split(usage, "\n") {
		if strings.Contains(line, "gmux agent") {
			agentLines = append(agentLines, line)
		}
	}
	if len(agentLines) != 2 {
		t.Errorf("top-level usage must mention 'gmux agent' on exactly two lines (prompt + help pointer), got %d:\n%s", len(agentLines), usage)
	}
	if !strings.Contains(usage, "gmux agent prompt <id> <prompt>") {
		t.Errorf("top-level usage must surface the prompt form:\n%s", usage)
	}
	if !strings.Contains(usage, "gmux agent --help") {
		t.Errorf("top-level usage must point at 'gmux agent --help':\n%s", usage)
	}
	// Detail stays in the namespace guide: no prompt flag soup and no
	// management verbs at top level.
	for _, stale := range []string{"agent cancel", "agent output", "--steer", "--follow-up", "--no-wait"} {
		if strings.Contains(usage, stale) {
			t.Errorf("top-level usage must not inline agent verb detail %q:\n%s", stale, usage)
		}
	}
}

// TestReadPromptText covers prompt sourcing: argument, piped stdin, and the
// refusals (interactive with no text would hang; blank input would submit an
// empty turn; an oversized prompt must be refused rather than truncated).
func TestReadPromptText(t *testing.T) {
	text := func(s string) *string { return &s }

	got, err := readPromptText(text("hello"), strings.NewReader("ignored"), true)
	if err != nil || got != "hello" {
		t.Fatalf("inline text: %q, %v", got, err)
	}
	got, err = readPromptText(nil, strings.NewReader("line one\nline two\n"), false)
	if err != nil {
		t.Fatalf("stdin: %v", err)
	}
	if got != "line one\nline two\n" {
		t.Errorf("multiline stdin must arrive as one prompt, got %q", got)
	}
	if _, err := readPromptText(nil, strings.NewReader("x"), true); err == nil {
		t.Error("interactive with no prompt text must be a usage error")
	}
	if _, err := readPromptText(nil, strings.NewReader("   \n\t\n"), false); err == nil {
		t.Error("whitespace-only stdin must be a usage error")
	}
	if _, err := readPromptText(text(""), nil, true); err == nil {
		t.Error("empty inline text must be a usage error")
	}
	big := strings.Repeat("a", maxPromptBytes+1)
	if _, err := readPromptText(nil, strings.NewReader(big), false); err == nil {
		t.Error("oversized stdin must be refused, not truncated")
	} else if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("oversized error = %v", err)
	}
	if _, err := readPromptText(text(big), nil, true); err == nil {
		t.Error("oversized inline prompt must be refused")
	}
	// The budget boundary itself is legal.
	if _, err := readPromptText(nil, strings.NewReader(strings.Repeat("a", maxPromptBytes)), false); err != nil {
		t.Errorf("prompt at the exact budget must be accepted: %v", err)
	}
}

// TestAgentPromptSendsMultibytePromptByteExact is the counterweight to the
// UTF-8 refusal: valid non-ASCII text must arrive at the daemon EXACTLY as
// typed, through both the positional and the stdin path. A guard that refuses
// or normalizes (NFC-folding, escaping, transliterating) legitimate multibyte
// input would be a quieter version of the bug it was added to prevent — the
// agent running text the caller did not write.
func TestAgentPromptSendsMultibytePromptByteExact(t *testing.T) {
	// Accents, CJK, an emoji outside the BMP, a combining sequence, and an RTL
	// run: the shapes a naive sanitizer mangles.
	const prompt = "café 世界 🧪 é́ مرحبا — ¥€"

	for _, tt := range []struct {
		name       string
		positional bool
	}{
		{name: "positional argument", positional: true},
		{name: "piped stdin"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := startStubDaemon(t, localSession())
			d.on(func(w http.ResponseWriter, r *http.Request) {
				writeEnvelope(w, http.StatusOK, map[string]any{
					"admission": "accepted", "outcome": "completed",
				})
			})
			var code int
			if tt.positional {
				text := prompt
				code = cmdAgentPrompt("abcd1234", agentModePrompt, false, 0, &text)
			} else {
				code = agentPromptWithStdin(nil, prompt)
			}
			if code != waitExitOK {
				t.Fatalf("exit = %d, want 0", code)
			}
			var got map[string]any
			raw := d.lastRequest(t).body
			if err := json.Unmarshal([]byte(raw), &got); err != nil {
				t.Fatalf("body %q: %v", raw, err)
			}
			sent, _ := got["prompt"].(string)
			if sent != prompt {
				t.Errorf("prompt round-trip differs:\n sent %q\n want %q", sent, prompt)
			}
			// Byte-level equality, not just string equality after decoding:
			// this is what the agent ultimately receives.
			if !bytes.Equal([]byte(sent), []byte(prompt)) {
				t.Errorf("prompt bytes = % x, want % x", sent, prompt)
			}
			if strings.Contains(raw, "\ufffd") {
				t.Errorf("body carries a replacement character: %q", raw)
			}
		})
	}
}

// TestReadPromptTextRefusesInvalidUTF8 pins the encoding refusal. Both
// json.Marshal here and the daemon's decoder substitute U+FFFD for every
// invalid byte, so an accepted mis-encoded prompt makes the agent run
// DIFFERENT text than the caller supplied — silently, under exit 0.
func TestReadPromptTextRefusesInvalidUTF8(t *testing.T) {
	bad := "caf\xe9 pl\xfcs" // latin-1, as a shell or file can easily produce
	for _, tt := range []struct {
		name string
		run  func() (string, error)
	}{
		{"positional", func() (string, error) { return readPromptText(&bad, nil, true) }},
		{"stdin", func() (string, error) { return readPromptText(nil, strings.NewReader(bad), false) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.run()
			if err == nil {
				t.Fatalf("accepted invalid UTF-8 and returned %q", got)
			}
			if !strings.Contains(err.Error(), "UTF-8") {
				t.Errorf("error = %v, want it to name the encoding", err)
			}
		})
	}
}

// TestAgentPromptRefusesInvalidUTF8BeforeSending: the refusal happens before
// any request is issued, so a mis-encoded prompt cannot reach the agent even
// in a rewritten form.
func TestAgentPromptRefusesInvalidUTF8BeforeSending(t *testing.T) {
	for _, tt := range []struct {
		name  string
		text  *string
		stdin string
	}{
		{name: "positional", text: func() *string { s := "bad \xff byte"; return &s }()},
		{name: "stdin", stdin: "bad \xff byte"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := startStubDaemon(t, localSession())
			d.on(func(w http.ResponseWriter, r *http.Request) {
				t.Error("a request was issued for a prompt that is not valid UTF-8")
				writeEnvelope(w, http.StatusOK, map[string]any{"admission": "accepted", "outcome": "completed"})
			})
			stderr := captureStderr(t, func() {
				code := agentPromptWithStdin(tt.text, tt.stdin)
				if code != waitExitError {
					t.Errorf("exit = %d, want 1", code)
				}
			})
			if !strings.Contains(stderr, "UTF-8") {
				t.Errorf("stderr = %q", stderr)
			}
			d.mu.Lock()
			n := len(d.requests)
			d.mu.Unlock()
			if n != 0 {
				t.Errorf("%d request(s) reached the daemon", n)
			}
		})
	}
}

// agentPromptWithStdin runs cmdAgentPrompt with stdin replaced by a pipe
// carrying raw, so the stdin path of readPromptText runs for real (the tty
// guard keys on the actual os.Stdin).
func agentPromptWithStdin(text *string, raw string) int {
	if text != nil {
		return cmdAgentPrompt("abcd1234", agentModePrompt, false, 0, text)
	}
	r, w, err := os.Pipe()
	if err != nil {
		return -1
	}
	orig := os.Stdin
	os.Stdin = r
	go func() { _, _ = w.WriteString(raw); _ = w.Close() }()
	defer func() { os.Stdin = orig; _ = r.Close() }()
	return cmdAgentPrompt("abcd1234", agentModePrompt, false, 0, nil)
}

// TestAgentEnvelopeLessNotFoundIsVersionSkew: a bare net/http 404 on an action
// route means the ROUTE does not exist, not that the session is missing — the
// CLI resolved that same session through this same daemon one request earlier.
// Reporting "session not found" would send the caller hunting for a session
// that is demonstrably there.
func TestAgentEnvelopeLessNotFoundIsVersionSkew(t *testing.T) {
	for _, tt := range []struct {
		name string
		run  func() int
	}{
		{"prompt", func() int { text := "go"; return cmdAgentPrompt("abcd1234", agentModePrompt, false, 0, &text) }},
		{"cancel", func() int { return cmdAgentCancel("abcd1234") }},
		{"status", func() int { return cmdAgentStatus("abcd1234", false) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := startStubDaemon(t, localSession())
			d.on(func(w http.ResponseWriter, r *http.Request) {
				// Exactly what Go's mux serves for an unregistered route.
				http.NotFound(w, r)
			})
			stderr := captureStderr(t, func() {
				if code := tt.run(); code != waitExitError {
					t.Errorf("exit = %d, want 1", code)
				}
			})
			if !strings.Contains(stderr, "predates") {
				t.Errorf("stderr = %q, want a version-skew report", stderr)
			}
			if strings.Contains(stderr, "not found") {
				t.Errorf("stderr = %q, must not claim the session is missing", stderr)
			}
		})
	}

	// An explicit not_found envelope still reports a missing session.
	d := startStubDaemon(t, localSession())
	d.on(func(w http.ResponseWriter, r *http.Request) {
		writeErrEnvelope(w, http.StatusNotFound, "not_found", "session not found")
	})
	stderr := captureStderr(t, func() {
		if code := cmdAgentCancel("abcd1234"); code != waitExitError {
			t.Errorf("exit = %d, want 1", code)
		}
	})
	if !strings.Contains(stderr, "not_found") || strings.Contains(stderr, "predates") {
		t.Errorf("stderr = %q", stderr)
	}
}

// TestAgentAnswerRejectsMarkedEmptyBody: a marked but empty 200 is not a
// silent success. The daemon cannot currently emit one (it answers
// no_message), so this pins the contract rather than a known case: treating
// silence as the answer is the one thing the message scope must never do.
func TestAgentAnswerRejectsMarkedEmptyBody(t *testing.T) {
	d := startStubDaemon(t, localSession())
	d.on(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(conversationScopeHeader, conversationScopeMessage)
		w.WriteHeader(http.StatusOK)
	})
	got := readAgentAnswer(cliSession{ID: "sess-abcd1234", Adapter: "pi", Alive: true})
	if !got.Failed || !strings.Contains(got.Report, "no content") {
		t.Errorf("readAgentAnswer = %+v, want a failed read naming the empty body", got)
	}
	_ = d
}

// TestAgentPromptAcceptedWithoutNoWait pins the 202 short-circuit for a
// waiting caller: if the daemon answers at the admission boundary anyway,
// there is no outcome to report, so the CLI must not fall through to the
// outcome switch and call a missing outcome unexpected.
func TestAgentPromptAcceptedWithoutNoWait(t *testing.T) {
	d := startStubDaemon(t, localSession())
	d.on(func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, http.StatusAccepted, map[string]any{"admission": "accepted"})
	})
	stderr := captureStderr(t, func() {
		text := "go"
		if code := cmdAgentPrompt("abcd1234", agentModePrompt, false, 0, &text); code != waitExitOK {
			t.Errorf("exit = %d, want 0", code)
		}
	})
	if strings.Contains(stderr, "unexpected") {
		t.Errorf("stderr = %q, a 202 is not an unexpected outcome", stderr)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(d.lastRequest(t).body), &body); err != nil {
		t.Fatal(err)
	}
	if body["wait"] != true {
		t.Errorf("wait = %v, want true (the caller did not pass --no-wait)", body["wait"])
	}
}

// TestAgentOutputSkewError pins the version-skew guard: an old daemon ignores
// scope=message and answers 200 with the whole transcript, which must not be
// printed as "the agent's latest message".
func TestAgentOutputSkewError(t *testing.T) {
	if msg := agentOutputSkewError(http.StatusOK, conversationScopeMessage); msg != "" {
		t.Errorf("marked response rejected: %q", msg)
	}
	if msg := agentOutputSkewError(http.StatusOK, ""); msg == "" {
		t.Error("unmarked 200 must be reported as an outdated daemon")
	}
	if msg := agentOutputSkewError(http.StatusNotFound, ""); msg != "" {
		t.Errorf("errors are left to the normal path, got %q", msg)
	}
}

// stubDaemon is an in-process gmuxd stub reachable over the real Unix socket
// path, so the CLI's own HTTP client, session resolution and error handling
// are exercised end to end without a daemon.
type stubDaemon struct {
	mu       sync.Mutex
	requests []recordedRequest
	handler  func(w http.ResponseWriter, r *http.Request)
	sessions []cliSession
	// sessionsRequests counts GET /v1/sessions calls: `agent status` must
	// classify liveness and turn flags from ONE listing, and a stub that only
	// records the sub-routes cannot see a second one.
	sessionsRequests int
	// rows, when set, replaces the /v1/sessions payload with raw JSON objects.
	// The session row carries fields cliSession deliberately does not (status,
	// last_output_at), and `agent status` reads them: pinning them needs a row
	// shape the CLI's pinned ls schema does not have.
	rows []map[string]any
}

type recordedRequest struct {
	method string
	path   string
	query  string
	body   string
}

func startStubDaemon(t *testing.T, sessions []cliSession) *stubDaemon {
	t.Helper()
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	dir := filepath.Join(state, "gmux")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	sock := filepath.Join(dir, "gmuxd.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	d := &stubDaemon{sessions: sessions}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"data":{"version":"dev"}}`))
	})
	mux.HandleFunc("/v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		d.sessionsRequests++
		rows := d.rows
		d.mu.Unlock()
		if rows != nil {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": rows})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": d.sessions})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		d.mu.Lock()
		d.requests = append(d.requests, recordedRequest{r.Method, r.URL.Path, r.URL.RawQuery, string(body)})
		handler := d.handler
		d.mu.Unlock()
		if handler == nil {
			w.WriteHeader(http.StatusNotImplemented)
			return
		}
		handler(w, r)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); _ = ln.Close() })
	return d
}

func (d *stubDaemon) on(handler func(w http.ResponseWriter, r *http.Request)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.handler = handler
}

func (d *stubDaemon) lastRequest(t *testing.T) recordedRequest {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.requests) == 0 {
		t.Fatal("no request reached the daemon")
	}
	return d.requests[len(d.requests)-1]
}

func localSession() []cliSession {
	return []cliSession{{ID: "sess-abcd1234", Adapter: "pi", Alive: true, Slug: "work"}}
}

func writeEnvelope(w http.ResponseWriter, status int, data map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": data})
}

func writeErrEnvelope(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok": false, "error": map[string]string{"code": code, "message": msg},
	})
}

// TestAgentPromptRequestBody pins the request the CLI builds for each public
// mode, plus the --no-wait and --timeout mappings. The daemon rejects unknown
// modes and defaults nothing, so a wrong mode string here is an unrequested
// intent, not a degraded one.
func TestAgentPromptRequestBody(t *testing.T) {
	tests := []struct {
		name        string
		mode        string
		noWait      bool
		timeout     int
		wantMode    string
		wantWait    bool
		wantTimeout float64
	}{
		{"plain", agentModePrompt, false, 0, "prompt", true, 0},
		{"follow-up", agentModeFollowUp, false, 0, "follow_up", true, 0},
		{"steer", agentModeSteer, false, 0, "steer", true, 0},
		{"detached", agentModePrompt, true, 0, "prompt", false, 0},
		{"timeout", agentModePrompt, false, 45, "prompt", true, 45},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := startStubDaemon(t, localSession())
			d.on(func(w http.ResponseWriter, r *http.Request) {
				writeEnvelope(w, http.StatusOK, map[string]any{
					"admission": "accepted", "outcome": "completed",
				})
			})
			text := "review this branch"
			if code := cmdAgentPrompt("abcd1234", tt.mode, tt.noWait, tt.timeout, &text); code != waitExitOK {
				t.Fatalf("exit = %d, want 0", code)
			}
			req := d.lastRequest(t)
			if req.method != http.MethodPost || req.path != "/v1/sessions/sess-abcd1234/prompt" {
				t.Fatalf("request = %s %s", req.method, req.path)
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(req.body), &got); err != nil {
				t.Fatalf("body %q: %v", req.body, err)
			}
			if got["prompt"] != text {
				t.Errorf("prompt = %v", got["prompt"])
			}
			if got["mode"] != tt.wantMode {
				t.Errorf("mode = %v, want %v", got["mode"], tt.wantMode)
			}
			if got["wait"] != tt.wantWait {
				t.Errorf("wait = %v, want %v", got["wait"], tt.wantWait)
			}
			if got["timeout_seconds"] != tt.wantTimeout {
				t.Errorf("timeout_seconds = %v, want %v", got["timeout_seconds"], tt.wantTimeout)
			}
		})
	}
}

// TestAgentPromptOutcomeExits pins the synchronous outcome → exit mapping. The
// interrupted case is the one that matters most: an intentionally stopped turn
// must never exit 0.
func TestAgentPromptOutcomeExits(t *testing.T) {
	tests := []struct {
		name string
		data map[string]any
		want int
	}{
		{"completed", map[string]any{"admission": "accepted", "outcome": "completed"}, waitExitOK},
		{"interrupted", map[string]any{"admission": "accepted", "outcome": "interrupted"}, waitExitInterrupted},
		{"error", map[string]any{"admission": "accepted", "outcome": "error"}, waitExitError},
		{"error with cause", map[string]any{"admission": "delivered", "outcome": "error", "cause": "runner_died"}, waitExitError},
		{"unknown outcome", map[string]any{"admission": "accepted", "outcome": "banana"}, waitExitError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := startStubDaemon(t, localSession())
			d.on(func(w http.ResponseWriter, r *http.Request) { writeEnvelope(w, http.StatusOK, tt.data) })
			captureStderr(t, func() {
				text := "go"
				if code := cmdAgentPrompt("abcd1234", agentModePrompt, false, 0, &text); code != tt.want {
					t.Errorf("exit = %d, want %d", code, tt.want)
				}
			})
		})
	}
}

// TestAgentPromptDetachedIsQuietSuccess: a 202 carries admission only. There
// is no outcome to report, and the CLI must not manufacture one.
func TestAgentPromptDetachedIsQuietSuccess(t *testing.T) {
	d := startStubDaemon(t, localSession())
	d.on(func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, http.StatusAccepted, map[string]any{"admission": "accepted", "resumed": true})
	})
	stderr := captureStderr(t, func() {
		text := "go"
		if code := cmdAgentPrompt("abcd1234", agentModePrompt, true, 0, &text); code != waitExitOK {
			t.Errorf("exit = %d, want 0", code)
		}
	})
	if !strings.Contains(stderr, "resumed") {
		t.Errorf("a transparent resume must be reported: stderr = %q", stderr)
	}
}

// TestAgentErrorCodeSurfacing walks every stable daemon code the namespace can
// receive and requires the code and the daemon's own message to reach stderr
// with the documented exit code. The daemon's wording encodes whether bytes
// were delivered; rewriting it here is how an indeterminate delivery starts
// looking like a safe retry.
func TestAgentErrorCodeSurfacing(t *testing.T) {
	tests := []struct {
		code   string
		status int
		want   int
	}{
		{"runner_outdated", http.StatusBadGateway, waitExitError},
		{"admission_timeout", http.StatusGatewayTimeout, waitExitError},
		{"delivery_timeout", http.StatusGatewayTimeout, waitExitError},
		{"execution_timeout", http.StatusGatewayTimeout, waitExitError},
		{"runner_died", http.StatusBadGateway, waitExitError},
		{"precondition_failed", http.StatusConflict, waitExitError},
		{"delivery_pending", http.StatusConflict, waitExitError},
		{"not_ready", http.StatusGatewayTimeout, waitExitError},
		{codeUnsupportedAdapter, http.StatusUnprocessableEntity, waitExitError},
		{codeUnsupportedAction, http.StatusUnprocessableEntity, waitExitError},
		{"not_running", http.StatusConflict, waitExitError},
		{"local_only", http.StatusUnprocessableEntity, waitExitError},
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			d := startStubDaemon(t, localSession())
			message := "daemon says: " + tt.code
			d.on(func(w http.ResponseWriter, r *http.Request) {
				writeErrEnvelope(w, tt.status, tt.code, message)
			})
			stderr := captureStderr(t, func() {
				text := "go"
				if code := cmdAgentPrompt("abcd1234", agentModePrompt, false, 0, &text); code != tt.want {
					t.Errorf("exit = %d, want %d", code, tt.want)
				}
			})
			if !strings.Contains(stderr, tt.code) || !strings.Contains(stderr, message) {
				t.Errorf("stderr = %q, want the code and the daemon message", stderr)
			}
		})
	}
}

// TestAgentCancelRequest: cancel posts to /cancel with no options and reports
// delivery, not a stopped turn.
func TestAgentCancelRequest(t *testing.T) {
	d := startStubDaemon(t, localSession())
	d.on(func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, http.StatusAccepted, map[string]any{"admission": "delivered"})
	})
	var stderr string
	stderr = captureStderr(t, func() {
		if code := cmdAgentCancel("abcd1234"); code != waitExitOK {
			t.Errorf("exit = %d, want 0", code)
		}
	})
	req := d.lastRequest(t)
	if req.method != http.MethodPost || req.path != "/v1/sessions/sess-abcd1234/cancel" {
		t.Fatalf("request = %s %s", req.method, req.path)
	}
	if strings.TrimSpace(req.body) != "{}" {
		t.Errorf("cancel body = %q, want {}", req.body)
	}
	if !strings.Contains(stderr, "delivered") || strings.Contains(strings.ToLower(stderr), "stopped") {
		t.Errorf("stderr = %q, want a delivery claim only", stderr)
	}

	// A refusal is passed through with its code.
	d.on(func(w http.ResponseWriter, r *http.Request) {
		writeErrEnvelope(w, http.StatusConflict, "not_running", "session is not running")
	})
	stderr = captureStderr(t, func() {
		if code := cmdAgentCancel("abcd1234"); code != waitExitError {
			t.Errorf("exit = %d, want 1", code)
		}
	})
	if !strings.Contains(stderr, "not_running") {
		t.Errorf("stderr = %q", stderr)
	}
}

// TestAgentAnswerRead covers the semantic message-scope read that backs
// `agent status`'s Answer part (and, in spirit, `logs --agent -n 1`): the
// request shape, the verbatim text, and the three distinguishable "nothing to
// show" answers, which are reasons rather than failures.
func TestAgentAnswerRead(t *testing.T) {
	sess := cliSession{ID: "sess-abcd1234", Adapter: "pi", Alive: true}
	body := "Here is the answer.\n\n```go\nfunc main() {}\n```\n"
	d := startStubDaemon(t, localSession())
	d.on(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(conversationScopeHeader, conversationScopeMessage)
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = w.Write([]byte(body))
	})
	got := readAgentAnswer(sess)
	if got.Failed || got.Code != "" || got.Text != strings.TrimRight(body, "\n") {
		t.Errorf("readAgentAnswer = %+v, want the message verbatim", got)
	}
	req := d.lastRequest(t)
	if req.method != http.MethodGet || req.path != "/v1/sessions/sess-abcd1234/conversation" || req.query != "scope=message" {
		t.Fatalf("request = %s %s?%s", req.method, req.path, req.query)
	}

	// A daemon "nothing to read" code is a REASON, not a failure: status
	// reports it as a note beside a perfectly good state line.
	for _, code := range []string{codeNoMessage, codeNoConversation, codeUnsupportedAdapter} {
		d.on(func(w http.ResponseWriter, r *http.Request) {
			status := http.StatusNotFound
			if code == codeUnsupportedAdapter {
				status = http.StatusUnprocessableEntity
			}
			writeErrEnvelope(w, status, code, "nothing for you")
		})
		got := readAgentAnswer(sess)
		if got.Failed || got.Code != code || got.Message == "" {
			t.Errorf("%s: readAgentAnswer = %+v, want the daemon's code carried as a reason", code, got)
		}
	}

	// Version skew: an unmarked 200 must fail loudly instead of presenting a
	// whole transcript as the latest message.
	d.on(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("## User\n\nhi\n\n## Assistant\n\nhello\n"))
	})
	got = readAgentAnswer(sess)
	if !got.Failed || !strings.Contains(got.Report, "predates") {
		t.Errorf("skewed daemon: readAgentAnswer = %+v, want a version-skew failure", got)
	}
}

// TestAgentRefusesPeerSessions: every verb refuses a peer-owned session before
// issuing a request. Semantic actions are local-only in this slice, and a ref
// that silently resolved to a peer would drive somebody else's agent.
func TestAgentRefusesPeerSessions(t *testing.T) {
	peer := []cliSession{{ID: "sess-c0b3c1a1", Peer: "laptop", Adapter: "pi", Alive: true}}
	for _, tt := range []struct {
		name string
		run  func() int
	}{
		{"prompt", func() int { text := "go"; return cmdAgentPrompt("c0b3c1a1@laptop", agentModePrompt, false, 0, &text) }},
		{"cancel", func() int { return cmdAgentCancel("c0b3c1a1@laptop") }},
		{"status", func() int { return cmdAgentStatus("c0b3c1a1@laptop", false) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := startStubDaemon(t, peer)
			stderr := captureStderr(t, func() {
				if code := tt.run(); code != waitExitError {
					t.Errorf("exit = %d, want 1", code)
				}
			})
			if !strings.Contains(stderr, "local sessions") {
				t.Errorf("stderr = %q", stderr)
			}
			d.mu.Lock()
			n := len(d.requests)
			d.mu.Unlock()
			if n != 0 {
				t.Errorf("%d request(s) reached the daemon for a peer ref", n)
			}
		})
	}
}

// captureStdout / captureStderr run fn with the stream redirected to a pipe.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	return captureStream(t, &os.Stdout, fn)
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	return captureStream(t, &os.Stderr, fn)
}

func captureStream(t *testing.T, target **os.File, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := *target
	*target = w
	done := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(r)
		done <- string(data)
	}()
	fn()
	*target = orig
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

// TestAgentPromptPrintsTheResultOnlyOnCompletion: a synchronous prompt now
// prints the agent's answer (the daemon selects it at turn close with the same
// selector `gmux agent output` and `gmux wait` use), and prints nothing for any
// other conclusion — a stale prior-turn answer must never be presented as this
// turn's result.
func TestAgentPromptPrintsTheResultOnlyOnCompletion(t *testing.T) {
	for _, tt := range []struct {
		name       string
		data       map[string]any
		wantExit   int
		wantStdout string
	}{
		{"completed", map[string]any{"admission": "accepted", "outcome": "completed", "output": "All green."},
			waitExitOK, "All green.\n"},
		{"completed multi-line untruncated",
			map[string]any{"admission": "accepted", "outcome": "completed", "output": "a\nb\n\nc"},
			waitExitOK, "a\nb\n\nc\n"},
		{"completed with nothing to render", map[string]any{"admission": "accepted", "outcome": "completed"},
			waitExitOK, ""},
		{"interrupted", map[string]any{"admission": "accepted", "outcome": "interrupted", "output": "stale"},
			waitExitInterrupted, ""},
		{"error", map[string]any{"admission": "accepted", "outcome": "error", "output": "stale"},
			waitExitError, ""},
		{"detached", map[string]any{"admission": "accepted", "output": "stale"}, waitExitOK, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := startStubDaemon(t, localSession())
			status := http.StatusOK
			noWait := false
			if tt.name == "detached" {
				status, noWait = http.StatusAccepted, true
			}
			d.on(func(w http.ResponseWriter, r *http.Request) { writeEnvelope(w, status, tt.data) })
			var code int
			out := captureStdout(t, func() {
				captureStderr(t, func() {
					text := "go"
					code = cmdAgentPrompt("abcd1234", agentModePrompt, noWait, 0, &text)
				})
			})
			if code != tt.wantExit {
				t.Errorf("exit = %d, want %d", code, tt.wantExit)
			}
			if out != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", out, tt.wantStdout)
			}
		})
	}
}
