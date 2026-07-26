package main

// agent.go — the `gmux agent` namespace (ADR 0027): semantic, adapter-aware
// turn control for agent sessions.
//
//	gmux agent prompt [--no-wait] [--follow-up|--steer] [--timeout N] <ref> [prompt]
//	gmux agent cancel <ref>
//	gmux agent output <ref>
//
// The distinction from `gmux send` is the whole point of the namespace: send
// types bytes at a terminal and tells you nothing about whether an agent read
// them, while agent expresses intent (start a turn, steer the one running,
// queue a follow-up, interrupt) and reports what the daemon actually observed.
// Nothing here may fall back to raw send/`/input`: a semantic action that
// silently degrades into keystrokes is precisely the failure mode ADR 0027's
// readiness gate and delivery reservation exist to prevent.
//
// This slice is deliberately local-and-pi-only, and deliberately quiet: the
// daemon's synchronous prompt response carries no output field yet (see
// agent_actions.go), so a completed prompt prints nothing rather than
// inventing a result. `gmux agent output` is the way to read what the agent
// said until the next slice makes waits result-bearing.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gmuxapp/gmux/cli/gmux/internal/localterm"
)

// Public prompt modes, mirroring the daemon's wire vocabulary. Duplicated as
// constants rather than imported: cli/gmux and services/gmuxd are separate
// modules and this is a wire contract.
const (
	agentModePrompt   = "prompt"
	agentModeFollowUp = "follow_up"
	agentModeSteer    = "steer"
)

// maxPromptBytes is the decoded prompt budget the daemon and runner both
// enforce (maxInputBytes). Checked client-side so an oversized prompt fails
// before any bytes are delivered instead of after a wasted round trip — and
// so the failure is a refusal, never a truncation: a silently truncated
// prompt is a different instruction than the one that was typed.
const maxPromptBytes = 1 << 20 // 1 MiB

// agentExit maps a daemon answer onto a process exit code.
//
// Mostly today's repo-wide wait codes (0 ok, 3 timeout, 1 everything else),
// reused so `gmux agent` does not introduce a second taxonomy mid-stack. Two
// deliberate departures:
//
//   - 2 means INTENTIONAL INTERRUPTION here, not "died". `gmux wait`'s 2 is
//     the death code, but a namespace that reports both a cancelled turn and a
//     dead runner as 2 cannot distinguish the one case scripts most need to
//     branch on. A dead runner is an error under both the current and the next
//     taxonomy, so it maps to 1 and nothing regresses.
//   - a turn that ends in `error` is 1, not 2.
//
// When the global 0/1/2 rework lands, the only change here is
// execution_timeout 3→1; interruption already exits 2.
const (
	agentExitOK          = 0
	agentExitError       = 1
	agentExitInterrupted = 2
	agentExitTimeout     = 3
)

// parseAgent handles `gmux agent <verb> ...`.
//
// Grammar follows the existing verb-first conventions exactly (ADR 0009
// decision 9): behaviour flags precede the session ref, the ref is the first
// non-flag token, and everything after the ref is verbatim content. That last
// rule is why prompt text needs no `--` guard: `gmux agent prompt s1 --help`
// prompts the agent with the literal text `--help`.
func parseAgent(args []string) (*command, error) {
	if len(args) == 0 {
		return nil, errors.New("agent requires one of: prompt, cancel, output")
	}
	head := args[0]
	rest := args[1:]
	switch head {
	case "help", "-h", "--help":
		return &command{mode: modeHelp, helpTopic: "agent"}, nil
	case "prompt":
		return parseAgentPrompt(rest)
	case "cancel", "output":
		return parseAgentRefOnly(head, rest)
	}
	return nil, fmt.Errorf("unknown agent verb %q; expected prompt, cancel or output", head)
}

// parseAgentRefOnly handles the two ref-only verbs. Flags are rejected rather
// than ignored, and a lone -h/--help prints the namespace help.
func parseAgentRefOnly(sub string, args []string) (*command, error) {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		return &command{mode: modeHelp, helpTopic: "agent " + sub}, nil
	}
	// Report what is actually wrong. "requires a session id" for
	// `agent cancel s1 s2` is false (one was given) and for
	// `agent output s1 -h` it hides the misplaced flag.
	if len(args) == 0 {
		return nil, fmt.Errorf("agent %s requires a session id", sub)
	}
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			return nil, fmt.Errorf("agent %s takes no flags (got %q)", sub, a)
		}
	}
	if len(args) > 1 {
		return nil, fmt.Errorf("agent %s takes exactly one session id (got %d: %s)",
			sub, len(args), strings.Join(args, " "))
	}
	return &command{mode: modeAgent, agentSub: sub, ref: args[0]}, nil
}

// parseAgentPrompt handles `gmux agent prompt [flags] <ref> [text]`.
//
// Flag independence, deliberately: `--follow-up` and `--steer` are mutually
// exclusive because each names a different DELIVERY intent and only one can
// happen. `--no-wait` is orthogonal to both — it chooses whether this process
// waits for the turn, not what is delivered — so `--no-wait --steer` is a
// legitimate "redirect the turn and don't block", which the wire model and the
// daemon already support.
//
// Every flag is single-use. A repeated `--timeout` under last-wins would let
// two generated arguments silently disagree, with the loser invisible.
func parseAgentPrompt(args []string) (*command, error) {
	c := &command{mode: modeAgent, agentSub: "prompt", agentMode: agentModePrompt}
	modeSet := ""
	noWaitSet, timeoutSet := false, false
	i := 0
	for i < len(args) {
		a := args[i]
		if a == "--" { // explicit end-of-flags; the ref follows
			i++
			break
		}
		if !strings.HasPrefix(a, "-") { // first non-flag token is the ref
			break
		}
		switch {
		case a == "-h" || a == "--help":
			return &command{mode: modeHelp, helpTopic: "agent prompt"}, nil
		case a == "--no-wait":
			if noWaitSet {
				return nil, agentRepeatedFlag(a)
			}
			noWaitSet = true
			c.agentNoWait = true
		case a == "--follow-up":
			if err := agentSetMode(&modeSet, a); err != nil {
				return nil, err
			}
			c.agentMode = agentModeFollowUp
		case a == "--steer":
			if err := agentSetMode(&modeSet, a); err != nil {
				return nil, err
			}
			c.agentMode = agentModeSteer
		case a == "--timeout" || strings.HasPrefix(a, "--timeout="):
			if timeoutSet {
				return nil, agentRepeatedFlag("--timeout")
			}
			timeoutSet = true
			val := strings.TrimPrefix(a, "--timeout")
			if val == "" {
				i++
				if i >= len(args) {
					return nil, errors.New("--timeout requires a number of seconds")
				}
				val = args[i]
			} else {
				val = strings.TrimPrefix(val, "=")
			}
			n, err := strconv.Atoi(val)
			if err != nil || n < 0 {
				return nil, errors.New("--timeout must be a non-negative number of seconds (0 means no timeout)")
			}
			c.timeout = n
		default:
			return nil, fmt.Errorf("agent prompt: unknown flag %q (flags go before the session id; text after the id is literal)", a)
		}
		i++
	}
	if i >= len(args) {
		return nil, errors.New("agent prompt requires a session id")
	}
	// --timeout bounds the wait, so it means nothing without one. Refusing the
	// combination rather than ignoring the flag is the same rule the rest of this
	// surface follows (a repeated flag, `?tail=` on a message scope): a caller who
	// set a bound and got none would believe they had constrained something. It
	// matters more here than elsewhere — `--no-wait` still waits internally for
	// admission, on a fixed window this flag cannot move.
	if c.agentNoWait && timeoutSet {
		return nil, errors.New("agent prompt: --timeout bounds the wait, so it cannot be combined with --no-wait")
	}
	c.ref = args[i]
	rest := args[i+1:]
	switch len(rest) {
	case 0:
		// Prompt text comes from piped stdin; the tty case is rejected at
		// execution time, where the CLI can see whether stdin is a pipe.
	case 1:
		t := rest[0]
		c.promptText = &t
	default:
		// Joining the words would guess at whitespace the shell already ate.
		// A prompt is one argument; quote it.
		return nil, errors.New("agent prompt takes a single prompt argument (quote the whole prompt), or pipe it on stdin")
	}
	return c, nil
}

// agentSetMode records a delivery-mode flag, refusing a second one.
//
// Two different modes are mutually exclusive because each names a different
// intent and picking a winner would run something the caller did not ask for.
// The same flag twice is a repetition, not a conflict: reporting "--steer and
// --steer are mutually exclusive" would be nonsense.
func agentSetMode(current *string, flag string) error {
	switch *current {
	case "":
		*current = flag
		return nil
	case flag:
		return agentRepeatedFlag(flag)
	}
	return fmt.Errorf("agent prompt: %s and %s are mutually exclusive", *current, flag)
}

func agentRepeatedFlag(flag string) error {
	return fmt.Errorf("agent prompt: %s given more than once", flag)
}

// cmdAgent dispatches the namespace's verbs.
func cmdAgent(c *command) int {
	switch c.agentSub {
	case "prompt":
		return cmdAgentPrompt(c.ref, c.agentMode, c.agentNoWait, c.timeout, c.promptText)
	case "cancel":
		return cmdAgentCancel(c.ref)
	case "output":
		return cmdAgentOutput(c.ref)
	}
	fmt.Fprintf(os.Stderr, "gmux: unknown agent verb %q\n", c.agentSub)
	return agentExitError
}

// resolveAgentSession resolves a ref and refuses peer-owned sessions.
//
// Semantic actions are local-only in this slice, exactly as the daemon
// enforces (codeLocalOnly). Refusing here as well means a peer ref fails with
// a clear message instead of resolving and then appearing to work.
func resolveAgentSession(ref, verb string) (cliSession, bool) {
	sess, err := resolveSession(ref)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return cliSession{}, false
	}
	if sess.Peer != "" {
		fmt.Fprintf(os.Stderr, "gmux: agent %s is only supported for local sessions (%s is on peer %q); run gmux agent in a session on that host instead\n",
			verb, shortID(sess.ID), sess.Peer)
		return cliSession{}, false
	}
	return sess, true
}

// cmdAgentPrompt implements `gmux agent prompt`.
func cmdAgentPrompt(ref, mode string, noWait bool, timeoutSecs int, text *string) int {
	prompt, err := readPromptText(text, os.Stdin, localterm.IsInteractive())
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return agentExitError
	}
	sess, ok := resolveAgentSession(ref, "prompt")
	if !ok {
		return agentExitError
	}
	body, err := json.Marshal(map[string]any{
		"prompt":          prompt,
		"mode":            mode,
		"wait":            !noWait,
		"timeout_seconds": timeoutSecs,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return agentExitError
	}

	client := gmuxdClient()
	// A synchronous prompt blocks for as long as the agent's turn takes; the
	// only deadline that may end it is the caller's --timeout, enforced
	// server-side.
	client.Timeout = 0
	url := gmuxdBaseURL() + "/v1/sessions/" + sess.ID + "/prompt"
	resp, err := client.Post(url, "application/json", strings.NewReader(string(body)))
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return agentExitError
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return reportAgentError(sess, "prompt", resp.StatusCode, raw)
	}
	return reportAgentPromptSuccess(sess, resp.StatusCode, raw, noWait)
}

// readPromptText resolves the prompt text: the positional argument, else
// piped stdin. interactive reports whether stdin is a terminal.
//
// Every refusal is a usage error, raised before any request is issued, not a
// convenience to paper over:
//   - no text with a terminal on stdin would otherwise block the process
//     reading the human's keyboard, which looks like a hang;
//   - empty or whitespace-only input would deliver a submit keystroke with
//     nothing to submit, which on most agents starts an empty turn;
//   - invalid UTF-8 must be refused rather than encoded: json.Marshal (like
//     the daemon's decoder) substitutes U+FFFD for every bad byte, so an
//     accepted mis-encoded prompt would run a DIFFERENT instruction than the
//     caller supplied — quietly, under a success exit code. Refusing the
//     caller's bytes beats rewriting them. (Both paths need it: a shell can
//     hand latin-1 bytes to argv just as easily as to a pipe.)
func readPromptText(text *string, stdin io.Reader, interactive bool) (string, error) {
	var prompt string
	switch {
	case text != nil:
		prompt = *text
	case interactive:
		return "", errors.New("agent prompt requires prompt text (pass it as an argument or pipe it on stdin)")
	default:
		// One byte past the cap: overflow must be refused, never truncated.
		raw, err := io.ReadAll(io.LimitReader(stdin, maxPromptBytes+1))
		if err != nil {
			return "", fmt.Errorf("read prompt from stdin: %w", err)
		}
		// The cap itself is enforced once, below, for argv and stdin alike.
		prompt = string(raw)
	}
	if strings.TrimSpace(prompt) == "" {
		return "", errors.New("agent prompt requires non-empty prompt text")
	}
	if !utf8.ValidString(prompt) {
		return "", errors.New("prompt is not valid UTF-8 (gmux will not re-encode it: the agent would receive different text)")
	}
	if len(prompt) > maxPromptBytes {
		return "", fmt.Errorf("prompt exceeds %d bytes", maxPromptBytes)
	}
	return prompt, nil
}

// agentPromptResult is the daemon's success payload for prompt/cancel.
type agentPromptResult struct {
	Admission string `json:"admission"`
	Outcome   string `json:"outcome"`
	Cause     string `json:"cause"`
	Resumed   bool   `json:"resumed"`
}

// reportAgentPromptSuccess turns a 2xx prompt response into output and an exit
// code.
//
// Quiet on success by design. The daemon does not return the agent's answer
// yet, and printing a cheerful "ok" for a turn whose text nobody has seen
// trains callers to trust an absence of information. `resumed` is worth a
// stderr line because it says the session was dead and has been restarted —
// a state change the caller did not ask for.
func reportAgentPromptSuccess(sess cliSession, status int, body []byte, noWait bool) int {
	var env struct {
		Data agentPromptResult `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		fmt.Fprintln(os.Stderr, "gmux: decode prompt response:", err)
		return agentExitError
	}
	res := env.Data
	if res.Resumed {
		fmt.Fprintf(os.Stderr, "gmux: resumed session %s to deliver the prompt\n", displayID(sess))
	}
	if noWait || status == http.StatusAccepted {
		// Detached: admission is all that is known, and it is not a result.
		return agentExitOK
	}
	switch res.Outcome {
	case "completed":
		return agentExitOK
	case "interrupted":
		fmt.Fprintf(os.Stderr, "gmux: the turn was interrupted before it finished (session %s)\n", displayID(sess))
		return agentExitInterrupted
	case "error":
		if res.Cause != "" {
			fmt.Fprintf(os.Stderr, "gmux: the turn ended in an error (%s); see gmux agent output %s\n",
				res.Cause, shortID(sess.ID))
		} else {
			fmt.Fprintf(os.Stderr, "gmux: the turn ended in an error; see gmux agent output %s\n", shortID(sess.ID))
		}
		return agentExitError
	default:
		fmt.Fprintf(os.Stderr, "gmux: unexpected prompt outcome %q\n", res.Outcome)
		return agentExitError
	}
}

// cmdAgentCancel implements `gmux agent cancel`: deliver an interrupt to a
// live, active agent. It returns once the interrupt is delivered — the daemon
// deliberately does not wait for the agent to go idle, so use `gmux wait` when
// the next step depends on the turn having actually stopped.
func cmdAgentCancel(ref string) int {
	sess, ok := resolveAgentSession(ref, "cancel")
	if !ok {
		return agentExitError
	}
	client := gmuxdClient()
	client.Timeout = 0
	url := gmuxdBaseURL() + "/v1/sessions/" + sess.ID + "/cancel"
	resp, err := client.Post(url, "application/json", strings.NewReader("{}"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return agentExitError
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return reportAgentError(sess, "cancel", resp.StatusCode, raw)
	}
	// Delivered, not stopped: saying "cancelled" would claim more than the
	// daemon reported.
	fmt.Fprintf(os.Stderr, "gmux: interrupt delivered to %s\n", displayID(sess))
	return agentExitOK
}

// cmdAgentOutput implements `gmux agent output`: print the agent's latest
// final message, verbatim and untruncated, from the daemon's semantic
// conversation read. It never resumes and never needs a live runner, so it
// works on a dead retained session.
func cmdAgentOutput(ref string) int {
	sess, ok := resolveAgentSession(ref, "output")
	if !ok {
		return agentExitError
	}
	client := gmuxdClient()
	// No client deadline: the daemon re-reads and renders the agent's whole
	// stored conversation to select one message, which on a long session and a
	// cold filesystem can outlast the default 5s and turn a readable answer
	// into a transport error.
	client.Timeout = 0
	url := gmuxdBaseURL() + "/v1/sessions/" + sess.ID + "/conversation?scope=message"
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return agentExitError
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return agentExitError
	}
	if msg := agentOutputSkewError(resp.StatusCode, resp.Header.Get(conversationScopeHeader)); msg != "" {
		fmt.Fprintln(os.Stderr, "gmux:", msg)
		return agentExitError
	}
	if resp.StatusCode >= 300 {
		return reportAgentError(sess, "output", resp.StatusCode, raw)
	}
	if len(raw) == 0 {
		// A marked but empty 200. The daemon cannot currently produce one (it
		// answers no_message instead), so this is a contract guard rather than a
		// known case: printing nothing under exit 0 would tell a script the
		// agent answered with silence, which is the one thing the message scope
		// exists to never say.
		fmt.Fprintf(os.Stderr, "gmux: the daemon reported a message for %s but sent no content\n", displayID(sess))
		return agentExitError
	}
	if _, err := os.Stdout.Write(raw); err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return agentExitError
	}
	return agentExitOK
}

// conversationScopeHeader must match the daemon's marker header.
const conversationScopeHeader = "X-Gmux-Conversation-Scope"

// agentOutputSkewError detects a daemon that predates scope=message and
// therefore answered 200 with the entire transcript. Returns "" when the
// response can be trusted.
//
// Printing that transcript as "the agent's latest message" is worse than
// failing: it is plausible, wrong, and unbounded. An old daemon's error
// responses are handled by reportAgentError, which recognizes an
// envelope-less status as version skew too.
func agentOutputSkewError(status int, scopeHeader string) string {
	if status == http.StatusOK && scopeHeader != conversationScopeMessage {
		return "this gmuxd predates 'gmux agent output' (it answered with the whole transcript); restart the daemon with 'gmux daemon restart', or use 'gmux tail'"
	}
	return ""
}

// conversationScopeMessage is the scope value the daemon echoes back.
const conversationScopeMessage = "message"

// reportAgentError surfaces a daemon error envelope and picks the exit code.
//
// The daemon's code and message are printed as-is, code first. They encode
// facts this process cannot re-derive — above all whether bytes reached the
// agent — so rewording them risks turning an indeterminate delivery into
// something that reads like a safe retry. The code prefix is deliberate:
// scripts get a stable token without parsing prose.
func reportAgentError(sess cliSession, verb string, status int, body []byte) int {
	code := errorCode(body)
	msg := extractMessage(body)
	switch {
	case code != "" && msg != "":
		fmt.Fprintf(os.Stderr, "gmux: %s: %s\n", code, msg)
	case status == http.StatusNotFound:
		// A 404 with no gmux error envelope is Go's net/http "404 page not
		// found": the route does not exist. It cannot mean the session is
		// missing — the CLI resolved that session through this same daemon one
		// request earlier — so reporting "not found" would send the caller
		// hunting for a session that is demonstrably there. It means this
		// daemon is older than the route.
		fmt.Fprintf(os.Stderr, "gmux: this gmuxd predates 'gmux agent %s' (no such route); restart the daemon with 'gmux daemon restart'\n", verb)
	default:
		fmt.Fprintf(os.Stderr, "gmux: agent %s failed: %s\n", verb, strings.TrimSpace(string(body)))
	}
	if hint := agentErrorHint(code, verb, sess); hint != "" {
		fmt.Fprintln(os.Stderr, "gmux:", hint)
	}
	return agentExitForCode(code)
}

// agentErrorHint adds the one actionable next step the daemon's message does
// not already carry. Hints never soften an indeterminate outcome.
//
// The unsupported hint is verb-aware: on a write verb the fallback is raw
// input, while on `output` the caller wants to READ, and telling them to send
// keystrokes answers a question they did not ask.
func agentErrorHint(code, verb string, sess cliSession) string {
	tailHint := "'gmux tail " + shortID(sess.ID) + "' shows this session directly"
	switch code {
	case codeUnsupportedAdapter, codeUnsupportedAction:
		if verb == "output" {
			return "this session's agent exposes no conversation gmux can read; " + tailHint
		}
		return "this session's agent has no semantic support yet; drive it with 'gmux send' and read it with 'gmux tail'"
	case codeNoMessage, codeNoConversation:
		return "nothing has been recorded for this session yet; " + tailHint
	}
	return ""
}

// Daemon error codes this CLI reacts to by name. Everything else is passed
// through verbatim with exit 1 — an unknown code is still the daemon's answer,
// and inventing a friendlier interpretation of it is how optimism leaks in.
const (
	codeRunnerOutdated       = "runner_outdated"
	codeAdmissionTimeout     = "admission_timeout"
	codeDeliveryTimeout      = "delivery_timeout"
	codeExecutionTimeout     = "execution_timeout"
	codeQueuedTurnUnobserved = "queued_turn_unobserved"
	codeRunnerDied           = "runner_died"
	codeUnsupportedAdapter   = "unsupported_adapter"
	codeUnsupportedAction    = "unsupported_action"
	codeNoMessage            = "no_message"
	codeNoConversation       = "no_conversation"
)

// agentExitForCode maps a daemon error code to an exit code under the current
// (pre-taxonomy-rework) conventions.
//
// Only execution_timeout is a timeout: the other timeout-shaped codes
// (admission_timeout, delivery_timeout, queued_turn_unobserved) describe an
// INDETERMINATE delivery, and mapping them to the timeout code would put them
// in the same bucket scripts routinely retry. They are errors, and their
// messages say why retrying can duplicate work.
//
// runner_died is 1, not 2: 2 is reserved for intentional interruption in this
// namespace (see the agentExit* block), and a runner dying is the opposite of
// intentional.
func agentExitForCode(code string) int {
	if code == codeExecutionTimeout {
		return agentExitTimeout
	}
	return agentExitError
}

// printAgentUsage writes help for the namespace, or for one verb when topic
// names it ("agent prompt").
func printAgentUsage(w io.Writer, topic string) {
	switch topic {
	case "agent prompt":
		fmt.Fprint(w, `gmux agent prompt — send a prompt to an agent session and wait for the turn

  gmux agent prompt [--no-wait] [--follow-up|--steer] [--timeout N] <id> [prompt]

  <prompt>          the prompt text; omit it to read the prompt from stdin
  --no-wait         return as soon as the prompt is admitted, without waiting
  --follow-up       queue the prompt to submit after the current turn ends
  --steer           redirect the turn that is running right now (fails if idle)
  --timeout N       give up waiting after N seconds (0/absent: wait indefinitely)

--follow-up and --steer are mutually exclusive; --no-wait composes with either.

Waits by default: the command returns when the turn ends. Exit status is 0 for
a completed turn, 2 for an interrupted one, 3 on --timeout, 1 otherwise (a
failed turn or a dead runner included). The agent's answer is not printed yet —
read it with 'gmux agent output <id>'.

A plain prompt (no flag) restarts a dead retained session to deliver the
prompt; --steer never does. Local sessions only.
`)
	case "agent cancel":
		fmt.Fprint(w, `gmux agent cancel — interrupt the turn an agent session is running

  gmux agent cancel <id>

Delivers an interrupt to a live, active agent and returns once it is
delivered — not once the agent has stopped. Follow with 'gmux wait <id>' when
the next step needs the turn to be over. Local sessions only.
`)
	case "agent output":
		fmt.Fprint(w, `gmux agent output — print an agent session's latest message

  gmux agent output <id>

Prints the agent's latest final message, verbatim and untruncated, read from
the agent's own stored conversation. Never starts or resumes anything, so it
works on a dead session. For the full transcript use 'gmux tail <id>'.
`)
	default:
		fmt.Fprint(w, `gmux agent — drive an agent session by intent instead of keystrokes

  gmux agent prompt [--no-wait] [--follow-up|--steer] [--timeout N] <id> [prompt]
                                    send a prompt and wait for the turn to end
  gmux agent cancel <id>            interrupt the running turn
  gmux agent output <id>            print the agent's latest message

Unlike 'gmux send', which types raw bytes at the terminal, these wait until
the agent can accept input, submit the way that agent expects, and report what
was actually observed. Agent sessions on this host only (pi today).

  gmux help agent prompt|cancel|output   per-verb help
`)
	}
}
