package main

import (
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
)

// TestParseAgentPromptNew pins the `--new` grammar: no ref, launch knobs only
// here, and the prompt as the sole positional.
func TestParseAgentPromptNew(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		check func(t *testing.T, c *command)
	}{
		{name: "bare new with prompt", args: []string{"agent", "prompt", "--new", "do it"},
			check: func(t *testing.T, c *command) {
				if !c.agentNew || c.ref != "" {
					t.Errorf("new=%v ref=%q", c.agentNew, c.ref)
				}
				if c.promptText == nil || *c.promptText != "do it" {
					t.Errorf("promptText = %v", c.promptText)
				}
				if c.agentMode != agentModePrompt {
					t.Errorf("mode = %q", c.agentMode)
				}
			}},
		{name: "model and name", args: []string{"agent", "prompt", "--new", "--model", "anthropic/sonnet", "--name", "review", "go"},
			check: func(t *testing.T, c *command) {
				if c.agentModel != "anthropic/sonnet" || c.agentName != "review" {
					t.Errorf("model=%q name=%q", c.agentModel, c.agentName)
				}
			}},
		{name: "equals forms", args: []string{"agent", "prompt", "--new", "--model=gpt", "--name=x", "go"},
			check: func(t *testing.T, c *command) {
				if c.agentModel != "gpt" || c.agentName != "x" {
					t.Errorf("model=%q name=%q", c.agentModel, c.agentName)
				}
			}},
		{name: "prompt from stdin when omitted", args: []string{"agent", "prompt", "--new"},
			check: func(t *testing.T, c *command) {
				if c.promptText != nil {
					t.Errorf("promptText = %v, want nil (stdin)", c.promptText)
				}
			}},
		{name: "explicit dash means stdin", args: []string{"agent", "prompt", "--new", "-"},
			check: func(t *testing.T, c *command) {
				if c.promptText != nil {
					t.Errorf("promptText = %v, want nil (stdin)", c.promptText)
				}
			}},
		{name: "no-wait composes", args: []string{"agent", "prompt", "--new", "--no-wait", "go"},
			check: func(t *testing.T, c *command) {
				if !c.agentNew || !c.agentNoWait {
					t.Errorf("new=%v noWait=%v", c.agentNew, c.agentNoWait)
				}
			}},
		{name: "timeout composes", args: []string{"agent", "prompt", "--new", "--timeout", "60", "go"},
			check: func(t *testing.T, c *command) {
				if c.timeout != 60 {
					t.Errorf("timeout = %d", c.timeout)
				}
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := parseCLI(tt.args)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if c.mode != modeAgent || c.agentSub != "prompt" {
				t.Fatalf("mode=%v sub=%q", c.mode, c.agentSub)
			}
			tt.check(t, c)
		})
	}
}

// TestParseAgentPromptNewExclusions walks every flag pair --new forbids. Each
// is refused by name: a new session has no turn to steer or queue behind, the
// launch knobs describe a launch that is not happening, and a ref alongside
// --new names a second session.
func TestParseAgentPromptNewExclusions(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"new and ref", []string{"agent", "prompt", "--new", "s1", "go"}, "no session id"},
		{"new and follow-up", []string{"agent", "prompt", "--new", "--follow-up", "go"}, "--follow-up"},
		{"follow-up and new", []string{"agent", "prompt", "--follow-up", "--new", "go"}, "--follow-up"},
		{"new and steer", []string{"agent", "prompt", "--new", "--steer", "go"}, "--steer"},
		{"steer and new", []string{"agent", "prompt", "--steer", "--new", "go"}, "--steer"},
		{"model without new", []string{"agent", "prompt", "--model", "m", "s1", "go"}, "--model only applies"},
		{"name without new", []string{"agent", "prompt", "--name", "n", "s1", "go"}, "--name only applies"},
		{"model equals without new", []string{"agent", "prompt", "--model=m", "s1", "go"}, "--model only applies"},
		{"new twice", []string{"agent", "prompt", "--new", "--new", "go"}, "--new given more than once"},
		{"model twice", []string{"agent", "prompt", "--new", "--model", "a", "--model", "b", "go"}, "--model given more than once"},
		{"name twice", []string{"agent", "prompt", "--new", "--name=a", "--name=b", "go"}, "--name given more than once"},
		{"empty model", []string{"agent", "prompt", "--new", "--model=", "go"}, "--model requires a non-empty value"},
		{"model without value", []string{"agent", "prompt", "--new", "--model"}, "--model requires a value"},
		{"empty name", []string{"agent", "prompt", "--new", "--name", " ", "go"}, "--name requires a non-empty value"},
		{"new with no-wait and timeout", []string{"agent", "prompt", "--new", "--no-wait", "-t", "5", "go"}, "--timeout bounds the wait"},
		// Neither a ref nor --new is still the pre-existing error.
		{"neither", []string{"agent", "prompt"}, "requires a session id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseCLI(tt.args)
			if err == nil {
				t.Fatal("want a usage error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

// TestAgentPromptDashIsRefPathUnchanged pins that `-` did NOT change meaning
// on the plain ref path when --new taught it to mean stdin.
//
// Both probes are pre-existing behaviour the ref path promises in its own help
// text ("everything after the id is the prompt, verbatim"): a trailing `-` is
// a literal one-character prompt, and a leading `-` is an unknown flag — never
// a session ref. --new's stdin spelling is scoped to the --new shape precisely
// so neither of these moves.
func TestAgentPromptDashIsRefPathUnchanged(t *testing.T) {
	c, err := parseCLI([]string{"agent", "prompt", "s1", "-"})
	if err != nil {
		t.Fatalf("`agent prompt s1 -` must parse: %v", err)
	}
	if c.agentNew || c.ref != "s1" {
		t.Fatalf("new=%v ref=%q", c.agentNew, c.ref)
	}
	if c.promptText == nil || *c.promptText != "-" {
		t.Errorf("promptText = %v, want the literal \"-\" (verbatim after the id)", c.promptText)
	}

	if _, err := parseCLI([]string{"agent", "prompt", "-", "x"}); err == nil {
		t.Error("`agent prompt - x` must stay a flag error, not resolve \"-\" as a session ref")
	}
}

// TestPiLaunchCommandArgv pins the adapter translation the CLI depends on:
// pi's real long flags, in a bare launch that carries no prompt.
func TestPiLaunchCommandArgv(t *testing.T) {
	pi := adapters.NewPi()
	tests := []struct {
		name string
		opts adapter.LaunchOptions
		want []string
	}{
		{"bare", adapter.LaunchOptions{}, []string{"pi"}},
		{"model", adapter.LaunchOptions{Model: "anthropic/sonnet"}, []string{"pi", "--model", "anthropic/sonnet"}},
		{"name", adapter.LaunchOptions{Name: "review"}, []string{"pi", "--name", "review"}},
		{"both", adapter.LaunchOptions{Model: "m", Name: "n"}, []string{"pi", "--model", "m", "--name", "n"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := pi.LaunchCommand(tt.opts)
			if !ok {
				t.Fatal("pi must support launching")
			}
			if strings.Join(got, "\x00") != strings.Join(tt.want, "\x00") {
				t.Errorf("argv = %q, want %q", got, tt.want)
			}
			for _, a := range got {
				if a == "-p" || a == "--print" {
					t.Errorf("argv must not carry a prompt: %q", got)
				}
			}
		})
	}
}

// nonLauncherAdapter is an adapter with no launch support, i.e. every adapter
// other than pi today.
type nonLauncherAdapter struct{}

func (nonLauncherAdapter) Name() string                    { return "claude" }
func (nonLauncherAdapter) Discover() bool                  { return true }
func (nonLauncherAdapter) Match([]string) bool             { return false }
func (nonLauncherAdapter) Env(adapter.EnvContext) []string { return nil }

// stubLaunch installs a fake launcher and returns the argv it was handed.
func stubLaunch(t *testing.T, id string, err error) *[]string {
	t.Helper()
	var got []string
	prev := agentLaunchSession
	agentLaunchSession = func(argv []string) (string, error) {
		got = argv
		return id, err
	}
	t.Cleanup(func() { agentLaunchSession = prev })
	return &got
}

// TestAgentPromptNewUnsupportedAdapter: an adapter with no LaunchCommand must
// fail in the namespace's established style, and must spawn nothing.
func TestAgentPromptNewUnsupportedAdapter(t *testing.T) {
	startStubDaemon(t, localSession())
	argv := stubLaunch(t, "sess-nope", nil)
	prev := agentLaunchAdapter
	agentLaunchAdapter = nonLauncherAdapter{}
	t.Cleanup(func() { agentLaunchAdapter = prev })

	text := "go"
	var code int
	stdout := captureStdout(t, func() {
		stderr := captureStderr(t, func() { code = cmdAgentPromptNew("", "", false, 0, &text) })
		if !strings.Contains(stderr, codeUnsupportedAdapter) {
			t.Errorf("stderr = %q, want the %s code", stderr, codeUnsupportedAdapter)
		}
	})
	if code != waitExitError {
		t.Errorf("exit = %d, want %d", code, waitExitError)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want nothing (no session was created)", stdout)
	}
	if *argv != nil {
		t.Errorf("nothing may be spawned for an unsupported adapter, got %q", *argv)
	}
}

// TestAgentPromptNewNoWaitPrintsBareID: the launch line of the handoff
// pattern. Exactly one line on stdout, the id, once the prompt is admitted.
func TestAgentPromptNewNoWaitPrintsBareID(t *testing.T) {
	d := startStubDaemon(t, localSession())
	d.on(func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, http.StatusAccepted, map[string]any{"admission": "accepted"})
	})
	argv := stubLaunch(t, "sess-abcd1234", nil)

	text := "start the refactor"
	var code int
	stdout := captureStdout(t, func() {
		code = cmdAgentPromptNew("sonnet", "refactor", true, 0, &text)
	})
	if code != waitExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if stdout != "sess-abcd1234\n" {
		t.Errorf("stdout = %q, want the bare id and nothing else", stdout)
	}
	if strings.Join(*argv, " ") != "pi --model sonnet --name refactor" {
		t.Errorf("spawned argv = %q", *argv)
	}
	req := d.lastRequest(t)
	if req.path != "/v1/sessions/sess-abcd1234/prompt" {
		t.Errorf("prompt went to %q", req.path)
	}
	if !strings.Contains(req.body, `"mode":"prompt"`) || !strings.Contains(req.body, `"wait":false`) {
		t.Errorf("body = %q", req.body)
	}
}

// TestAgentPromptNewSyncPrintsIDThenAnswer: under --new the id is stdout line
// 1 and the answer follows, so the completion signal is the exit code rather
// than non-empty stdout.
func TestAgentPromptNewSyncPrintsIDThenAnswer(t *testing.T) {
	d := startStubDaemon(t, localSession())
	d.on(func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, http.StatusOK, map[string]any{
			"admission": "accepted", "outcome": "completed", "output": "all done",
		})
	})
	stubLaunch(t, "sess-abcd1234", nil)

	text := "do it"
	var code int
	stdout := captureStdout(t, func() { code = cmdAgentPromptNew("", "", false, 0, &text) })
	if code != waitExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if stdout != "sess-abcd1234\nall done\n" {
		t.Errorf("stdout = %q, want the id line then the answer", stdout)
	}
	if got := d.lastRequest(t).body; !strings.Contains(got, `"wait":true`) {
		t.Errorf("body = %q", got)
	}
}

// TestAgentPromptNewIDPrintedBeforeDelivery is the load-bearing ordering
// guarantee (ADR 0027, the --new amendment): the id reaches stdout before the
// prompt request is issued. The line therefore says "this session exists and
// is addressable" and nothing more — no admission, no readiness, no delivery;
// those are the exit code's job — which is exactly what lets a watcher attach
// or tail while the agent is still coming up.
func TestAgentPromptNewIDPrintedBeforeDelivery(t *testing.T) {
	d := startStubDaemon(t, localSession())
	seen := make(chan string, 1)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	d.on(func(hw http.ResponseWriter, hr *http.Request) {
		// Read what stdout already holds at the moment the daemon is called.
		buf := make([]byte, len("sess-abcd1234\n"))
		n, _ := r.Read(buf)
		seen <- string(buf[:n])
		writeEnvelope(hw, http.StatusOK, map[string]any{"admission": "accepted", "outcome": "completed"})
	})
	stubLaunch(t, "sess-abcd1234", nil)

	orig := os.Stdout
	os.Stdout = w
	text := "go"
	code := cmdAgentPromptNew("", "", false, 0, &text)
	os.Stdout = orig
	_ = w.Close()
	if code != waitExitOK {
		t.Fatalf("exit = %d", code)
	}
	if got := <-seen; got != "sess-abcd1234\n" {
		t.Errorf("stdout at delivery time = %q, want the id already flushed", got)
	}
	_ = r.Close()
}

// TestAgentPromptNewFailureAfterSpawnStillPrintsID: once the session exists,
// the caller owns it — it keeps existing, and may keep running, after the
// prompt fails. Admission failures, dead runners and never-ready agents all
// report on stderr and exit 1 with the id already on stdout, because a session
// the caller cannot address is a leak they cannot even kill.
func TestAgentPromptNewFailureAfterSpawnStillPrintsID(t *testing.T) {
	tests := []struct {
		name   string
		status int
		code   string
		msg    string
	}{
		{"never ready", http.StatusGatewayTimeout, "admission_timeout", "the agent never reported itself ready"},
		{"runner died", http.StatusBadGateway, "not_running", "the runner is gone"},
		{"unsupported action", 422, codeUnsupportedAction, "this adapter cannot be prompted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := startStubDaemon(t, localSession())
			d.on(func(w http.ResponseWriter, r *http.Request) {
				writeErrEnvelope(w, tt.status, tt.code, tt.msg)
			})
			stubLaunch(t, "sess-abcd1234", nil)
			text := "go"
			var code int
			var stderr string
			stdout := captureStdout(t, func() {
				stderr = captureStderr(t, func() { code = cmdAgentPromptNew("", "", false, 0, &text) })
			})
			if code != waitExitError {
				t.Errorf("exit = %d, want %d", code, waitExitError)
			}
			if stdout != "sess-abcd1234\n" {
				t.Errorf("stdout = %q, want the id so the caller can still address the session", stdout)
			}
			if !strings.Contains(stderr, tt.code) {
				t.Errorf("stderr = %q, want the daemon's code %q", stderr, tt.code)
			}
		})
	}
}

// TestAgentPromptNewSpawnFailurePrintsNoID: a launch that never registers
// produced no session, so there is nothing to address and stdout stays empty.
func TestAgentPromptNewSpawnFailurePrintsNoID(t *testing.T) {
	d := startStubDaemon(t, localSession())
	stubLaunch(t, "", errors.New("child process exited before registering"))
	text := "go"
	var code int
	var stderr string
	stdout := captureStdout(t, func() {
		stderr = captureStderr(t, func() { code = cmdAgentPromptNew("", "", false, 0, &text) })
	})
	if code != waitExitError {
		t.Errorf("exit = %d, want %d", code, waitExitError)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want nothing: no session exists", stdout)
	}
	if !strings.Contains(stderr, "child process exited before registering") {
		t.Errorf("stderr = %q, want the launch failure verbatim", stderr)
	}
	if len(d.requests) != 0 {
		t.Errorf("no prompt may be attempted after a failed launch, got %d request(s)", len(d.requests))
	}
}

// TestAgentPromptNewRejectsBadPromptBeforeSpawning: usage errors must never
// leave an orphan session behind, so the prompt is validated first.
func TestAgentPromptNewRejectsBadPromptBeforeSpawning(t *testing.T) {
	startStubDaemon(t, localSession())
	argv := stubLaunch(t, "sess-abcd1234", nil)
	empty := "   "
	var code int
	captureStderr(t, func() { code = cmdAgentPromptNew("", "", false, 0, &empty) })
	if code != waitExitError {
		t.Errorf("exit = %d, want %d", code, waitExitError)
	}
	if *argv != nil {
		t.Errorf("an invalid prompt must not spawn anything, got %q", *argv)
	}
}

// TestAgentPromptRefPathPrintsNoIDLine: the pre-existing shape is untouched —
// a plain `gmux agent prompt <ref>` still prints the answer alone.
func TestAgentPromptRefPathPrintsNoIDLine(t *testing.T) {
	d := startStubDaemon(t, localSession())
	d.on(func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, http.StatusOK, map[string]any{
			"admission": "accepted", "outcome": "completed", "output": "all done",
		})
	})
	text := "go"
	var code int
	stdout := captureStdout(t, func() { code = cmdAgentPrompt("abcd1234", agentModePrompt, false, 0, &text) })
	if code != waitExitOK {
		t.Fatalf("exit = %d", code)
	}
	if stdout != "all done\n" {
		t.Errorf("stdout = %q, want the answer alone", stdout)
	}
	if got := d.lastRequest(t).path; got != "/v1/sessions/sess-abcd1234/prompt" {
		t.Errorf("path = %q", got)
	}
}

// TestAgentPromptNewHelpStatesTheIDContract pins the RATIFIED WORDING of the
// --new output contract on the help page, negative clauses included.
//
// Mentioning an id line is not enough. The contract ADR 0027's amendment
// ratified is a meaning plus a non-meaning — "stdout line 1 says the session
// exists and is addressable; it says nothing about admission, readiness or
// delivery, which the exit code carries" — and the non-meaning is the half a
// caller gets wrong, because the line is printed before the only events that
// could support the other readings. A help page that quietly loses "not an
// admission receipt" starts promising a health signal gmux does not give, so
// each clause is asserted on its own and deleting any one of them fails here.
//
// Matching is against the help text with its line wrapping collapsed, so
// reflowing a paragraph is allowed but rewording a clause is not.
func TestAgentPromptNewHelpStatesTheIDContract(t *testing.T) {
	var sb strings.Builder
	printAgentUsage(&sb, "agent prompt")
	help := strings.Join(strings.Fields(sb.String()), " ")

	tests := []struct {
		clause string
		why    string
	}{
		{"--new", "the flag itself"},
		{"--model", "the launch knobs are --new-only and must be documented"},
		{"--name", "the launch knobs are --new-only and must be documented"},
		{"THE SESSION ID IS ALWAYS STDOUT LINE 1", "the positive half of the contract"},
		{"the session exists and is addressable", "what the id line DOES mean"},
		{"NOT an admission receipt", "the ratified negative clause"},
		{"not a readiness signal", "the ratified negative clause"},
		{"not a claim that the prompt was delivered", "the ratified negative clause"},
		{"the exit code carries all of those", "where the verdicts actually live"},
		{"the completion signal is therefore the EXIT CODE, not non-empty stdout",
			"the difference from a plain sync prompt"},
		{"exit 0 does mean the prompt was admitted", "the one admission claim --no-wait DOES make"},
		{"A failure AFTER the launch leaves the session behind, and it is yours",
			"post-spawn ownership"},
		{"A failed launch prints no id", "stdout is empty when no session exists"},
		{"--new must come before the prompt", "after a ref it is verbatim prompt text"},
	}
	for _, tt := range tests {
		if !strings.Contains(help, tt.clause) {
			t.Errorf("agent prompt help no longer states %q (%s)", tt.clause, tt.why)
		}
	}
}

// TestAgentPromptHelpDistinguishesAdmissionFromDelivery pins the mode split in
// `--no-wait`'s documented meaning, because the unqualified version of that
// sentence was wrong for half the modes.
//
// What the daemon actually does (services/gmuxd agent_actions.go: runAgentWait's
// `admitted := !spec.requireAcceptance`, and requireAcceptance is set only for a
// plain prompt or a follow-up delivered to an idle agent):
//
//   - plain prompt / follow-up into an IDLE agent — a fresh turn is observable,
//     so `--no-wait` blocks until the agent starts it and exit 0 is a claim about
//     this session's health;
//   - `--steer` / follow-up merged into a RUNNING turn — the turn was admitted
//     before this prompt existed, so there is nothing to admit and the call
//     returns at delivery.
//
// Both halves must be on the page. A caller who reads "returns once the agent has
// actually started the turn" and applies it to `--steer` believes exit 0 proves
// something about a turn nobody restarted; a page that drops the admission half
// loses the guarantee the handoff pattern is built on. Deleting either clause
// fails here.
//
// Matching is against the help text with its line wrapping collapsed, so
// reflowing a paragraph is allowed but rewording a clause is not.
func TestAgentPromptHelpDistinguishesAdmissionFromDelivery(t *testing.T) {
	var sb strings.Builder
	printAgentUsage(&sb, "agent prompt")
	help := strings.Join(strings.Fields(sb.String()), " ")

	for _, tt := range []struct {
		clause string
		why    string
	}{
		{"What --no-wait waits for depends on whether the prompt STARTS a turn",
			"the split itself, named where the flag is documented"},
		{"--no-wait returns once the agent has actually begun the turn",
			"the admission half: what exit 0 buys for a plain prompt / idle follow-up"},
		{"bounded by the 60s admission window",
			"the cost of that stronger claim, and the current window"},
		{"--steer, and --follow-up that merges into a RUNNING turn",
			"names the modes the admission claim does NOT cover"},
		{"There is nothing to admit beyond delivery",
			"why those modes return early: the turn was already admitted"},
		{"exit 0 claims delivery, not a fresh turn",
			"what exit 0 means for those modes"},
		{"--steer and --follow-up are refused with --new",
			"why --new's unconditional admission claim stays true"},
	} {
		if !strings.Contains(help, tt.clause) {
			t.Errorf("agent prompt help no longer states %q (%s)", tt.clause, tt.why)
		}
	}
}
