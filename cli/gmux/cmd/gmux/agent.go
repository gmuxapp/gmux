package main

// agent.go — the `gmux agent` namespace (ADR 0027): semantic, adapter-aware
// turn control for agent sessions.
//
//	gmux agent prompt [--no-wait] [--follow-up|--steer] [--timeout|-t N] <ref> [prompt]
//	gmux agent cancel <ref>
//	gmux agent output <ref>
//	gmux agent logs <ref> [-n N]
//
// The distinction from `gmux send` is the whole point of the namespace: send
// types bytes at a terminal and tells you nothing about whether an agent read
// them, while agent expresses intent (start a turn, steer the one running,
// queue a follow-up, interrupt) and reports what the daemon actually observed.
// Nothing here may fall back to raw send/`/input`: a semantic action that
// silently degrades into keystrokes is precisely the failure mode ADR 0027's
// readiness gate and delivery reservation exist to prevent.
//
// This namespace is deliberately local-and-pi-only. A synchronous prompt that
// completes prints the agent's answer (the daemon selects it at turn close);
// a failed or interrupted turn prints nothing, because the newest stored
// message would be a previous or partial turn's. `gmux agent output` reads it
// explicitly in those cases.

import (
	"encoding/json"
	"errors"
	"flag"
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

// Exit codes here are the global taxonomy defined in wait.go (waitExit*, ADR
// 0027 §8), shared verbatim with `gmux wait` and `send --wait`: 0 success,
// 1 error (usage, unsupported, transport, timeout, death, terminal turn
// failure), 2 intentional interruption. There is no timeout code: a timeout is
// an error, and a caller that needs to tell timeouts apart reads the daemon's
// stable error code on stderr, which says far more than a number could.

// parseAgent handles `gmux agent <verb> ...`.
//
// Grammar follows the existing verb-first conventions exactly (ADR 0009
// decision 9): behaviour flags precede the session ref, the ref is the first
// non-flag token, and everything after the ref is verbatim content. That last
// rule is why prompt text needs no `--` guard: `gmux agent prompt s1 --help`
// prompts the agent with the literal text `--help`.
func parseAgent(args []string) (*command, error) {
	if len(args) == 0 {
		// A bare `gmux agent` is a question, not a mistake: print the
		// namespace guide, like `gmux` prints the synopsis.
		return &command{mode: modeHelp, helpTopic: "agent"}, nil
	}
	head := args[0]
	rest := args[1:]
	if isHelpToken(head) {
		return &command{mode: modeHelp, helpTopic: "agent"}, nil
	}
	switch head {
	case "prompt":
		return parseAgentPrompt(rest)
	case "cancel", "output":
		return parseAgentRefOnly(head, rest)
	case "logs":
		return parseAgentLogs(rest)
	}
	return nil, fmt.Errorf("unknown agent verb %q; expected prompt, logs, cancel or output", head)
}

// parseAgentRefOnly handles the two ref-only verbs. Flags are rejected rather
// than ignored, and a lone -h/--help prints the namespace help.
func parseAgentRefOnly(sub string, args []string) (*command, error) {
	if len(args) == 1 && isHelpToken(args[0]) {
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

// parseAgentLogs handles `gmux agent logs <ref> [-n N]`.
//
// The only knob is how much to print, so unlike prompt this verb takes flags
// on either side of the ref (the interspersed convention `tail` and `wait`
// already use): there is no verbatim trailing content here to protect, and a
// caller who typed `gmux agent logs s1 -n 5` meant the count.
//
// -n counts MESSAGES, not lines: the view is the rendered conversation, and
// counting its lines would cut a message in half. That difference in unit
// from `gmux tail -n` is the whole reason the two views stopped sharing one
// verb.
func parseAgentLogs(args []string) (*command, error) {
	if len(args) == 1 && isHelpToken(args[0]) {
		return &command{mode: modeHelp, helpTopic: "agent logs"}, nil
	}
	c := &command{mode: modeAgent, agentSub: "logs", tailLines: agentLogsDefaultMessages}
	fs := newFlagSet("agent logs")
	fs.IntVar(&c.tailLines, "n", agentLogsDefaultMessages, "number of conversation messages to show")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return &command{mode: modeHelp, helpTopic: "agent logs"}, nil
		}
		return nil, fmt.Errorf("agent logs: %v", err)
	}
	if len(pos) == 0 {
		return nil, errors.New("agent logs requires a session id")
	}
	if len(pos) > 1 {
		return nil, fmt.Errorf("agent logs takes exactly one session id (got %d: %s)", len(pos), strings.Join(pos, " "))
	}
	if c.tailLines <= 0 {
		return nil, errors.New("agent logs: -n must be a positive number of messages")
	}
	c.ref = pos[0]
	return c, nil
}

// agentLogsDefaultMessages is how many conversation messages `agent logs`
// prints when -n is absent — the same 100 `tail` defaults to, in messages.
const agentLogsDefaultMessages = 100

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
	// A leading help token asks about the verb. Only the leading position:
	// after the ref, every token is verbatim prompt text, so
	// `agent prompt s1 ?` prompts with a literal `?`.
	if len(args) > 0 && isHelpToken(args[0]) {
		return &command{mode: modeHelp, helpTopic: "agent prompt"}, nil
	}
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
		case a == "--timeout" || strings.HasPrefix(a, "--timeout=") ||
			a == "-t" || strings.HasPrefix(a, "-t="):
			// Single-use is a property of the FLAG, not of a spelling:
			// `--timeout=5 -t 9` is the same disagreement as `--timeout`
			// twice, and is reported under the canonical name.
			if timeoutSet {
				return nil, agentRepeatedFlag("--timeout")
			}
			timeoutSet = true
			val := strings.TrimPrefix(strings.TrimPrefix(a, "--timeout"), "-t")
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
	case "logs":
		return cmdAgentLogs(c.ref, c.tailLines)
	}
	fmt.Fprintf(os.Stderr, "gmux: unknown agent verb %q\n", c.agentSub)
	return waitExitError
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
		return waitExitError
	}
	sess, ok := resolveAgentSession(ref, "prompt")
	if !ok {
		return waitExitError
	}
	body, err := json.Marshal(map[string]any{
		"prompt":          prompt,
		"mode":            mode,
		"wait":            !noWait,
		"timeout_seconds": timeoutSecs,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return waitExitError
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
		return waitExitError
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
	// Output is the agent's latest final message, present only for a
	// completed turn on a session whose conversation gmux can read.
	Output string `json:"output"`
}

// reportAgentPromptSuccess turns a 2xx prompt response into output and an exit
// code.
//
// A completed turn prints the agent's answer, which the daemon selected at turn
// close with the same selector `gmux agent output` and `gmux wait` use. Every
// other conclusion prints NO result — a stale prior-turn answer must never be
// presented as this turn's — and reports the condition on stderr instead.
// `resumed` is worth a stderr line because it says the session was dead and has
// been restarted: a state change the caller did not ask for.
//
// One coupling to know about: the error hint below names `gmux agent output`
// unconditionally, where reportWaitResult decides from the response's
// `conversation_readable` whether to hint at `gmux tail` instead. The two agree
// today only because a session that can be prompted has an AgentActionEncoder,
// and the only adapter with one (pi) is also a ConversationRenderer. The moment
// an adapter implements EncodeAction without RenderConversation, this hint starts
// naming a route that 404s, and the prompt payload needs `conversation_readable`
// too.
func reportAgentPromptSuccess(sess cliSession, status int, body []byte, noWait bool) int {
	var env struct {
		Data agentPromptResult `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		fmt.Fprintln(os.Stderr, "gmux: decode prompt response:", err)
		return waitExitError
	}
	res := env.Data
	if res.Resumed {
		fmt.Fprintf(os.Stderr, "gmux: resumed session %s to deliver the prompt\n", displayID(sess))
	}
	if noWait || status == http.StatusAccepted {
		// Detached: admission is all that is known, and it is not a result.
		return waitExitOK
	}
	switch res.Outcome {
	case waitOutcomeCompleted:
		if res.Output != "" {
			// Verbatim and untruncated, with exactly one trailing newline.
			// An absent field means "nothing gmux can read" (a non-renderer
			// agent, or a turn with no prose), which stays quiet: the turn
			// really did complete.
			if _, err := io.WriteString(os.Stdout, strings.TrimRight(res.Output, "\n")+"\n"); err != nil {
				fmt.Fprintln(os.Stderr, "gmux:", err)
				return waitExitError
			}
		}
		return waitExitOK
	case waitOutcomeInterrupted:
		fmt.Fprintf(os.Stderr, "gmux: the turn was interrupted before it finished (session %s)\n", displayID(sess))
		return waitExitInterrupted
	case waitOutcomeError:
		if res.Cause != "" {
			fmt.Fprintf(os.Stderr, "gmux: the turn ended in an error (%s); see gmux agent output %s\n",
				res.Cause, shortID(sess.ID))
		} else {
			fmt.Fprintf(os.Stderr, "gmux: the turn ended in an error; see gmux agent output %s\n", shortID(sess.ID))
		}
		return waitExitError
	default:
		fmt.Fprintf(os.Stderr, "gmux: unexpected prompt outcome %q\n", res.Outcome)
		return waitExitError
	}
}

// cmdAgentCancel implements `gmux agent cancel`: deliver an interrupt to a
// live, active agent. It returns once the interrupt is delivered — the daemon
// deliberately does not wait for the agent to go idle, so use `gmux wait` when
// the next step depends on the turn having actually stopped.
func cmdAgentCancel(ref string) int {
	sess, ok := resolveAgentSession(ref, "cancel")
	if !ok {
		return waitExitError
	}
	client := gmuxdClient()
	client.Timeout = 0
	url := gmuxdBaseURL() + "/v1/sessions/" + sess.ID + "/cancel"
	resp, err := client.Post(url, "application/json", strings.NewReader("{}"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return waitExitError
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return reportAgentError(sess, "cancel", resp.StatusCode, raw)
	}
	// Delivered, not stopped: saying "cancelled" would claim more than the
	// daemon reported.
	fmt.Fprintf(os.Stderr, "gmux: interrupt delivered to %s\n", displayID(sess))
	return waitExitOK
}

// cmdAgentOutput implements `gmux agent output`: print the agent's latest
// final message, verbatim and untruncated, from the daemon's semantic
// conversation read. It never resumes and never needs a live runner, so it
// works on a dead retained session.
func cmdAgentOutput(ref string) int {
	sess, ok := resolveAgentSession(ref, "output")
	if !ok {
		return waitExitError
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
		return waitExitError
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return waitExitError
	}
	if msg := agentOutputSkewError(resp.StatusCode, resp.Header.Get(conversationScopeHeader)); msg != "" {
		fmt.Fprintln(os.Stderr, "gmux:", msg)
		return waitExitError
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
		return waitExitError
	}
	if _, err := os.Stdout.Write(raw); err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return waitExitError
	}
	return waitExitOK
}

// cmdAgentLogs implements `gmux agent logs`: print the session's conversation
// as markdown (last n messages), read from the agent's own stored
// conversation.
//
// Store-only, exactly like `agent output`: it never starts, resumes or even
// touches a runner, so it works on a dead retained session, and it is
// local-only for the same reason the rest of the namespace is. The daemon's
// transcript scope is the read (`?tail=N`, no scope parameter — that IS the
// transcript), so nothing new is asked of gmuxd.
//
// Error taxonomy is `agent output`'s, verbatim: the daemon's code and message
// are printed as-is by reportAgentError, an envelope-less 404 is reported as
// version skew rather than a missing session, and a non-renderer adapter's
// refusal carries the read-side 'gmux tail' hint — the raw view is exactly
// what a caller who cannot have a transcript should reach for.
//
// No marker-header guard here, unlike `agent output`: that guard exists
// because an old daemon ignoring `scope=message` would answer with the whole
// transcript, and the whole transcript is precisely what this verb asked for.
// The only skew that can bite is a daemon with no /conversation route at all,
// which arrives as an envelope-less 404 and is reported as such.
func cmdAgentLogs(ref string, n int) int {
	sess, ok := resolveAgentSession(ref, "logs")
	if !ok {
		return waitExitError
	}
	client := gmuxdClient()
	// No client deadline, for the same reason `agent output` drops it: the
	// daemon re-reads and renders the agent's whole stored conversation, which
	// on a long session and a cold filesystem can outlast the default 5s and
	// turn a readable transcript into a transport error.
	client.Timeout = 0
	url := fmt.Sprintf("%s/v1/sessions/%s/conversation?tail=%d", gmuxdBaseURL(), sess.ID, n)
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return waitExitError
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return waitExitError
	}
	if resp.StatusCode >= 300 {
		return reportAgentError(sess, "logs", resp.StatusCode, raw)
	}
	if len(raw) == 0 {
		// A 200 with no body. The daemon answers no_conversation instead of
		// serving an empty transcript, so this is a contract guard: printing
		// nothing under exit 0 would tell a script the agent has done nothing,
		// which is a claim only the daemon's explicit codes may make.
		fmt.Fprintf(os.Stderr, "gmux: the daemon reported a conversation for %s but sent no content\n", displayID(sess))
		return waitExitError
	}
	if _, err := os.Stdout.Write(raw); err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return waitExitError
	}
	return waitExitOK
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
	// Every failure code is 1 under the global taxonomy, timeout-shaped ones
	// included. That loses nothing a number could carry: admission_timeout,
	// delivery_timeout and queued_turn_unobserved describe an INDETERMINATE
	// delivery (retrying may duplicate the prompt) while execution_timeout means
	// the turn is still running — three meanings one "timeout" code would have
	// flattened into the bucket scripts blindly retry. The stable code printed
	// above separates them.
	return waitExitError
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
		if verb == "output" || verb == "logs" {
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
	codeUnsupportedAdapter = "unsupported_adapter"
	codeUnsupportedAction  = "unsupported_action"
	codeNoMessage          = "no_message"
	codeNoConversation     = "no_conversation"
)

// printAgentUsage writes help for the namespace, or for one verb when topic
// names it ("agent prompt").
func printAgentUsage(w io.Writer, topic string) {
	switch topic {
	case "agent prompt":
		fmt.Fprint(w, `gmux agent prompt — send a prompt to an agent session and wait for the turn

  gmux agent prompt [--no-wait] [--follow-up|--steer] [--timeout|-t N] <id> [prompt]

  <prompt>          the prompt text; omit it to read the prompt from stdin
  --no-wait         return as soon as the prompt is admitted, without waiting
  --follow-up       queue the prompt to submit after the current turn ends
  --steer           redirect the turn that is running right now (fails if idle)
  --timeout/-t N    give up waiting after N seconds (0/absent: wait indefinitely)

--follow-up and --steer are mutually exclusive; --no-wait composes with either.

Waits by default: the command returns when the turn ends and prints the agent's
answer. Exit status is 0 for a completed turn, 2 for an interrupted one, and 1
for anything else (a failed turn, a --timeout, a dead runner). A turn that did
not complete prints no answer — read what exists with 'gmux agent output <id>'.

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
works on a dead session. For the whole conversation use 'gmux agent logs <id>',
and for the terminal itself 'gmux tail <id>'.
`)
	case "agent logs":
		fmt.Fprint(w, `gmux agent logs — print what an agent session has been doing

  gmux agent logs <id> [-n N]

  -n N              how many conversation messages to print (default 100)

Prints the agent's conversation as markdown, read from the agent's own stored
conversation: '## User' / '## Assistant' messages with compact [tool]
one-liners — the actual exchange, not the TUI's box-drawing and spinners.
-n counts MESSAGES here, not lines.

A store-only snapshot, like 'gmux agent output': it never starts or resumes
anything, so it works on a dead retained session. Local sessions only, and
only for agents whose conversation gmux can read — anything else answers
unsupported_adapter, where 'gmux tail <id>' shows the terminal instead.

The three reading verbs answer three different questions:

  gmux tail <id>          what is on its screen (raw terminal, any session)
  gmux agent logs <id>    what it has been doing (this command)
  gmux agent output <id>  what the answer is (its latest message)
`)
	default:
		fmt.Fprint(w, `gmux agent — drive an agent session by intent instead of keystrokes

  gmux agent prompt [--no-wait] [--follow-up|--steer] [--timeout|-t N] <id> [prompt]
                                    send a prompt and wait for the turn to end
  gmux agent cancel <id>            interrupt the running turn
  gmux agent logs <id> [-n N]       print the conversation as markdown
  gmux agent output <id>            print the agent's latest message

Unlike 'gmux send', which types raw bytes at a terminal and cannot say whether
the agent read them, prompt and cancel wait until the agent can accept input,
submit the way that agent expects, and report what the daemon observed. logs
and output are store-only snapshots: they never start or resume the agent.

Prompting. A plain prompt starts a fresh turn (restarting a dead retained
session if needed); --steer redirects the turn running right now; --follow-up
queues text to submit after the current turn. --follow-up and --steer are
mutually exclusive; --no-wait composes with either and only decides whether
you block. Flags go before the id; everything after the id is the prompt,
verbatim. Omit the prompt to pipe it on stdin (it stays one prompt).

Reading. Three questions, three verbs, no word doing double duty: 'gmux tail'
shows what is on the session's screen (raw terminal, any session); 'gmux agent
logs' shows what the agent has been doing (the conversation as markdown, -n in
messages); 'gmux agent output' shows what the answer is (its latest message).
logs and output are store-only snapshots: they never start or resume the agent
and work on a dead retained session.

Results. A synchronous prompt — and 'gmux wait' — prints the agent's answer
when the turn completes. A failed or interrupted turn prints nothing (a stale
answer is worse than none); 'gmux agent output <id>' reads what exists, even
after the session died. Exit codes: 0 completed, 2 intentionally interrupted,
1 anything else (failed turn, timeout, dead runner, usage, transport).

Retrying. admission_timeout, delivery_timeout, queued_turn_unobserved, and
transport_error are indeterminate: the prompt may already have landed, so
inspect before resending. A dropped connection is indeterminate too. These
codes guarantee nothing was delivered and are safe to retry: runner_outdated,
precondition_failed, delivery_pending, not_ready, not_running, and
incarnation_mismatch.

Scope. Agent sessions on this host only, and pi only for now; other agents
fail with unsupported_adapter — drive those with 'gmux send' and read them
with 'gmux tail'.

  gmux agent prompt|logs|cancel|output --help   per-verb help
`)
	}
}
