package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// TestParseAgentLogs covers the verb's grammar: it joins prompt/cancel/output
// in the namespace, takes exactly one ref, and its only flag is -n — on
// either side of the ref, since there is no verbatim trailing content here to
// protect.
func TestParseAgentLogs(t *testing.T) {
	for _, args := range [][]string{
		{"agent", "logs", "s1"},
		{"agent", "logs", "s1", "-n", "100"},
		{"agent", "logs", "-n", "100", "s1"},
		{"agent", "logs", "-n=100", "s1"},
	} {
		c, err := parseCLI(args)
		if err != nil {
			t.Fatalf("parseCLI(%v): %v", args, err)
		}
		if c.mode != modeAgent || c.agentSub != "logs" || c.ref != "s1" || c.tailLines != 100 {
			t.Errorf("parseCLI(%v) = mode=%v sub=%q ref=%q n=%d", args, c.mode, c.agentSub, c.ref, c.tailLines)
		}
	}
	// -n is a message count, and the default matches tail's 100.
	c, err := parseCLI([]string{"agent", "logs", "s1", "-n", "3"})
	if err != nil || c.tailLines != 3 {
		t.Fatalf("explicit -n: (%+v, %v)", c, err)
	}

	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"no ref", []string{"agent", "logs"}, "requires a session id"},
		{"two refs", []string{"agent", "logs", "s1", "s2"}, "exactly one session id"},
		{"zero messages", []string{"agent", "logs", "-n", "0", "s1"}, "must be a positive number of messages"},
		{"negative messages", []string{"agent", "logs", "-n", "-2", "s1"}, "must be a positive number of messages"},
		{"non-numeric", []string{"agent", "logs", "-n", "many", "s1"}, "agent logs:"},
		{"unknown flag", []string{"agent", "logs", "--raw", "s1"}, "agent logs:"},
		// The view's own name is not a flag on it.
		{"follow is not implemented", []string{"agent", "logs", "--follow", "s1"}, "agent logs:"},
	} {
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

// TestAgentLogsHelpRouting: every help spelling reaches the verb's own page,
// and a bare `gmux logs <id>` gets the namespace hint the other agent verbs
// get rather than being read as a program to run.
func TestAgentLogsHelpRouting(t *testing.T) {
	for _, args := range [][]string{
		{"agent", "logs", "--help"},
		{"agent", "logs", "-h"},
		{"agent", "logs", "help"},
		{"agent", "logs", "?"},
		{"help", "agent", "logs"},
	} {
		c, err := parseCLI(args)
		if err != nil {
			t.Fatalf("parseCLI(%v): %v", args, err)
		}
		if c.mode != modeHelp || c.helpTopic != "agent logs" {
			t.Errorf("parseCLI(%v) = mode=%v topic=%q, want modeHelp/'agent logs'", args, c.mode, c.helpTopic)
		}
	}

	// `gmux logs abc` is a missing namespace, not a program named logs.
	_, err := parseCLI([]string{"logs", "abc"})
	if err == nil || !strings.Contains(err.Error(), "gmux agent logs") {
		t.Fatalf("parseCLI(logs abc) = %v, want the namespaced-command hint", err)
	}
	var ue *usageError
	if !errors.As(err, &ue) || ue.topic != "agent" {
		t.Errorf("parseCLI(logs abc) must carry the agent topic, got %v", err)
	}

	// The page itself must teach the three-questions split and its unit.
	var b strings.Builder
	printAgentUsage(&b, "agent logs")
	page := b.String()
	for _, want := range []string{
		"gmux agent logs <id> [-n N]",
		"MESSAGES",
		"store-only", "never starts or resumes",
		"Local sessions only",
		"unsupported_adapter",
		"gmux tail <id>",
		"gmux agent output <id>",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("agent logs page missing %q:\n%s", want, page)
		}
	}
	// The namespace guide and tail's page must both route to it.
	b.Reset()
	printAgentUsage(&b, "agent")
	if !strings.Contains(b.String(), "gmux agent logs") {
		t.Errorf("namespace guide does not mention agent logs:\n%s", b.String())
	}
	if !strings.Contains(verbHelpPages["tail"], "gmux agent logs") {
		t.Errorf("tail page does not redirect to agent logs:\n%s", verbHelpPages["tail"])
	}
}

// TestTopLevelSynopsisMatchesTheVerbs pins the highest-traffic help page
// against the commands it teaches: the synopsis may not advertise a flag or a
// view that this stack makes fail by name. It is the page most callers see
// first, so a stale line there costs more than anywhere else — and the removed
// tail flags survived one round of edits precisely here.
func TestTopLevelSynopsisMatchesTheVerbs(t *testing.T) {
	var b strings.Builder
	printUsage(&b)
	usage := b.String()

	// tail's removed view flags must not reappear in any spelling, and the
	// line must describe the raw view it actually gives.
	for _, stale := range []string{"--raw", "[-e]", "|-e", "|-r", "print conversation"} {
		if strings.Contains(usage, stale) {
			t.Errorf("top-level synopsis still advertises %q:\n%s", stale, usage)
		}
	}
	if !strings.Contains(usage, "gmux tail <id> [-n N]") {
		t.Errorf("synopsis must show tail's current form:\n%s", usage)
	}
	if !strings.Contains(usage, "terminal output") {
		t.Errorf("synopsis must describe tail as the terminal view:\n%s", usage)
	}
	// The reading split is the headline change, so the agent pointer names the
	// two semantic reads — without spending a third 'gmux agent' line, which
	// TestTopLevelUsageAgentSection budgets.
	for _, want := range []string{"logs", "output"} {
		if !strings.Contains(usage, want) {
			t.Errorf("synopsis must surface the %q read:\n%s", want, usage)
		}
	}

	// Every command form the synopsis shows must actually parse. This is what
	// makes the pin self-maintaining rather than another string to forget.
	for _, args := range [][]string{
		{"ls", "--all", "--json"},
		{"ls", "-a", "-j"},
		{"attach", "abc"},
		{"tail", "abc"},
		{"tail", "-n", "5", "abc"},
		{"send", "abc", "text", "Enter"},
		{"wait", "abc", "--timeout", "5"},
		{"wait", "abc", "-t", "5"},
		{"kill", "abc"},
		{"agent", "prompt", "abc", "a prompt"},
		{"agent", "logs", "abc"},
		{"agent", "output", "abc"},
	} {
		if _, err := parseCLI(args); err != nil {
			t.Errorf("the synopsis teaches %v, which does not parse: %v", args, err)
		}
	}
}

// TestAgentLogs covers the read end to end against the daemon stub: the
// request shape (transcript scope with a message count), verbatim stdout, and
// the failure taxonomy it shares with `agent output`.
func TestAgentLogs(t *testing.T) {
	body := "## User\n\nfix the test\n\n## Assistant\n\n[tool] bash\n\ndone\n"
	d := startStubDaemon(t, localSession())
	d.on(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = w.Write([]byte(body))
	})
	stdout := captureStdout(t, func() {
		if code := cmdAgentLogs("abcd1234", 7); code != waitExitOK {
			t.Errorf("exit = %d, want 0", code)
		}
	})
	if stdout != body {
		t.Errorf("stdout = %q, want the transcript verbatim (%q)", stdout, body)
	}
	req := d.lastRequest(t)
	if req.method != http.MethodGet || req.path != "/v1/sessions/sess-abcd1234/conversation" || req.query != "tail=7" {
		t.Fatalf("request = %s %s?%s, want the transcript read with tail=7", req.method, req.path, req.query)
	}

	// Failure codes are the daemon's, printed as-is with the read-side hint.
	for _, tt := range []struct {
		code   string
		status int
	}{
		{codeUnsupportedAdapter, http.StatusUnprocessableEntity},
		{codeNoConversation, http.StatusNotFound},
	} {
		d.on(func(w http.ResponseWriter, r *http.Request) {
			writeErrEnvelope(w, tt.status, tt.code, "nothing to render")
		})
		stderr := captureStderr(t, func() {
			out := captureStdout(t, func() {
				if code := cmdAgentLogs("abcd1234", 100); code != waitExitError {
					t.Errorf("%s: exit = %d, want 1", tt.code, code)
				}
			})
			if out != "" {
				t.Errorf("%s: stdout = %q, want nothing printed", tt.code, out)
			}
		})
		if !strings.Contains(stderr, tt.code) {
			t.Errorf("%s: stderr = %q, want the daemon's code", tt.code, stderr)
		}
		// logs is a READ, like output: the hint must point at the other read
		// (gmux tail), never at sending keystrokes.
		if !strings.Contains(stderr, "gmux tail abcd1234") {
			t.Errorf("%s: stderr = %q, want the 'gmux tail <id>' hint", tt.code, stderr)
		}
		if strings.Contains(stderr, "gmux send") {
			t.Errorf("%s: stderr = %q, a read must not suggest sending keystrokes", tt.code, stderr)
		}
	}

	// Version skew: a daemon with no /conversation route answers Go's plain
	// 404, which means the ROUTE is missing — not the session the CLI just
	// resolved through this same daemon.
	d.on(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	stderr := captureStderr(t, func() {
		if code := cmdAgentLogs("abcd1234", 100); code != waitExitError {
			t.Errorf("skew: exit = %d, want 1", code)
		}
	})
	if !strings.Contains(stderr, "predates") || !strings.Contains(stderr, "agent logs") {
		t.Errorf("skew stderr = %q, want a version-skew report naming the verb", stderr)
	}
	if strings.Contains(stderr, "session not found") {
		t.Errorf("skew stderr = %q, must not claim the session is missing", stderr)
	}

	// A 200 with no body is a contract breach, not "the agent did nothing".
	d.on(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	stderr = captureStderr(t, func() {
		out := captureStdout(t, func() {
			if code := cmdAgentLogs("abcd1234", 100); code != waitExitError {
				t.Errorf("empty 200: exit = %d, want 1", code)
			}
		})
		if out != "" {
			t.Errorf("empty 200 printed %q", out)
		}
	})
	if !strings.Contains(stderr, "no content") {
		t.Errorf("empty 200 stderr = %q", stderr)
	}
}

// TestAgentLogsIsStoreOnly: like `agent output`, logs must work on a dead
// retained session and must never issue anything that could start or resume
// one — one GET, no writes.
func TestAgentLogsIsStoreOnly(t *testing.T) {
	dead := []cliSession{{ID: "sess-abcd1234", Adapter: "pi", Alive: false, Slug: "work"}}
	d := startStubDaemon(t, dead)
	d.on(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("## User\n\nhi\n"))
	})
	stdout := captureStdout(t, func() {
		if code := cmdAgentLogs("abcd1234", 100); code != waitExitOK {
			t.Errorf("dead session: exit = %d, want 0", code)
		}
	})
	if stdout == "" {
		t.Error("a dead retained session must still print its conversation")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, r := range d.requests {
		if r.method != http.MethodGet {
			t.Errorf("agent logs issued a %s to %s; the read must be store-only", r.method, r.path)
		}
		if strings.Contains(r.path, "/prompt") || strings.Contains(r.path, "/resume") || strings.Contains(r.path, "/input") {
			t.Errorf("agent logs touched %s", r.path)
		}
	}
}

// TestAgentLogsRefusesPeerSessions: local-only, refused before any request,
// exactly like the rest of the namespace.
func TestAgentLogsRefusesPeerSessions(t *testing.T) {
	peer := []cliSession{{ID: "sess-c0b3c1a1", Peer: "laptop", Adapter: "pi", Alive: true}}
	d := startStubDaemon(t, peer)
	stderr := captureStderr(t, func() {
		if code := cmdAgentLogs("c0b3c1a1@laptop", 100); code != waitExitError {
			t.Errorf("exit = %d, want 1", code)
		}
	})
	if !strings.Contains(stderr, "local sessions") {
		t.Errorf("stderr = %q", stderr)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.requests) != 0 {
		t.Errorf("%d request(s) reached the daemon for a peer ref", len(d.requests))
	}
}
