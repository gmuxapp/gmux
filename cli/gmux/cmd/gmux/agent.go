package main

// agent.go — the `gmux agent` namespace (ADR 0027): semantic, adapter-aware
// turn control for agent sessions.
//
//	gmux agent prompt [--no-wait] [--follow-up|--steer] [--timeout|-t N] <ref> [prompt]
//	gmux agent cancel <ref>
//	gmux agent status <ref> [--json]
//	gmux agent logs <ref> [-n N] [--user|--agent|--tool|--all] [--json]
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
// message would be a previous or partial turn's. `gmux agent status` shows
// what is there in those cases.
//
// The reading verbs split cognitively, not by shape (ADR 0027's 2026-07-28
// amendment): `status` is "I don't know what I want, show me what matters",
// `logs` is "I know the exact text I want", `tail` is the raw screen.

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
	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
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
	case "cancel":
		return parseAgentRefOnly(head, rest)
	case "status":
		return parseAgentStatus(rest)
	case "logs":
		return parseAgentLogs(rest)
	case "output":
		// Removed by name, with both replacements spelled out. Silently
		// aliasing it to `status` would print a three-part report where a
		// script expected one bare answer on stdout, so the verb fails.
		return nil, errors.New("agent output was replaced by 'gmux agent status <id>' (the state, the trigger and the relevant content); for the answer alone use 'gmux agent logs --agent -n 1 <id>'")
	}
	return nil, fmt.Errorf("unknown agent verb %q; expected prompt, logs, status or cancel", head)
}

// parseAgentRefOnly handles the two ref-only verbs. Flags are rejected rather
// than ignored, and a lone -h/--help prints the namespace help.
func parseAgentRefOnly(sub string, args []string) (*command, error) {
	if len(args) == 1 && isHelpToken(args[0]) {
		return &command{mode: modeHelp, helpTopic: "agent " + sub}, nil
	}
	// Report what is actually wrong. "requires a session id" for
	// `agent cancel s1 s2` is false (one was given) and for
	// `agent cancel s1 -h` it hides the misplaced flag.
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

// parseAgentStatus handles `gmux agent status <ref> [--json]`.
//
// Flags may sit on either side of the ref, like logs: there is no verbatim
// trailing content to protect here.
func parseAgentStatus(args []string) (*command, error) {
	if len(args) == 1 && isHelpToken(args[0]) {
		return &command{mode: modeHelp, helpTopic: "agent status"}, nil
	}
	c := &command{mode: modeAgent, agentSub: "status"}
	fs := newFlagSet("agent status")
	fs.BoolVar(&c.json, "json", false, "print the machine shape instead of the report")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return &command{mode: modeHelp, helpTopic: "agent status"}, nil
		}
		return nil, fmt.Errorf("agent status: %v", err)
	}
	if len(pos) == 0 {
		return nil, errors.New("agent status requires a session id")
	}
	if len(pos) > 1 {
		return nil, fmt.Errorf("agent status takes exactly one session id (got %d: %s)", len(pos), strings.Join(pos, " "))
	}
	c.ref = pos[0]
	return c, nil
}

// agentLogsTypes is the wire vocabulary of the transcript filter, shared with
// the daemon's `types=` parameter.
const (
	agentLogTypeUser  = "user"
	agentLogTypeAgent = "agent"
	agentLogTypeTool  = "tool"
)

// parseAgentLogs handles
// `gmux agent logs <ref> [-n N] [--user|--agent|--tool|--all] [--json]`.
//
// Flags sit on either side of the ref (the interspersed convention `tail` and
// `wait` already use): there is no verbatim trailing content here to protect,
// and a caller who typed `gmux agent logs s1 -n 5` meant the count.
//
// -n counts MESSAGES, not lines: the view is the rendered conversation, and
// counting its lines would cut a message in half. That difference in unit
// from `gmux tail -n` is the whole reason the two views stopped sharing one
// verb. It counts POST-FILTER messages, because the filter is what the caller
// asked to see: `--tool -n 1` is the last tool call, not "the last message if
// it happens to be one".
//
// Type flags REPLACE the default set rather than adding to it, so the printed
// view is always exactly the union of the types named — and `--all` is a name
// for "every type gmux renders" rather than a magic widening of a default.
// `--thinking` is refused by name: no adapter renders thinking blocks today
// (pi's renderer drops them), so accepting it would answer with silence, which
// reads as "the agent thought nothing".
func parseAgentLogs(args []string) (*command, error) {
	if len(args) == 1 && isHelpToken(args[0]) {
		return &command{mode: modeHelp, helpTopic: "agent logs"}, nil
	}
	c := &command{mode: modeAgent, agentSub: "logs", tailLines: agentLogsDefaultMessages}
	fs := newFlagSet("agent logs")
	fs.IntVar(&c.tailLines, "n", agentLogsDefaultMessages, "number of conversation messages to show")
	var user, agentProse, tool, all, thinking bool
	fs.BoolVar(&user, "user", false, "show user messages")
	fs.BoolVar(&agentProse, "agent", false, "show assistant messages that carry prose")
	fs.BoolVar(&tool, "tool", false, "show tool-call-only messages")
	fs.BoolVar(&all, "all", false, "show every rendered message type")
	fs.BoolVar(&thinking, "thinking", false, "show thinking blocks (not rendered by any adapter)")
	fs.BoolVar(&c.json, "json", false, "print the machine shape instead of markdown")
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
	if thinking {
		return nil, errors.New("agent logs: --thinking is not rendered by this adapter (pi's transcript drops thinking blocks); 'gmux tail <id>' shows whatever the TUI painted")
	}
	if all && (user || agentProse || tool) {
		// --all IS the full set, so naming a subset beside it either repeats it
		// or contradicts it. Picking a winner would print a view the caller did
		// not ask for.
		return nil, errors.New("agent logs: --all already selects every message type, so it cannot be combined with a type flag")
	}
	switch {
	case all:
		c.logTypes = []string{agentLogTypeUser, agentLogTypeAgent, agentLogTypeTool}
	case user || agentProse || tool:
		// Explicit types REPLACE the default pair.
		if user {
			c.logTypes = append(c.logTypes, agentLogTypeUser)
		}
		if agentProse {
			c.logTypes = append(c.logTypes, agentLogTypeAgent)
		}
		if tool {
			c.logTypes = append(c.logTypes, agentLogTypeTool)
		}
	default:
		// The conversation without the machinery: what was asked and what was
		// said. nil means "the daemon's default", which is this same pair.
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
	newSet, modelSet, nameSet := false, false, false
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
		// A bare `-` means "the prompt is on stdin" — but ONLY under --new,
		// where there is no ref and the positional is the prompt. On the ref
		// path `-` keeps its pre-existing meanings exactly: an unknown flag
		// before the ref, and verbatim prompt text after it. Treating it as a
		// positional here would let `agent prompt - x` resolve `-` as a
		// session ref, which the ref path never did.
		if a == "-" && newSet {
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
		case a == "--new":
			if newSet {
				return nil, agentRepeatedFlag(a)
			}
			newSet = true
			c.agentNew = true
		case a == "--model" || strings.HasPrefix(a, "--model="):
			if modelSet {
				return nil, agentRepeatedFlag("--model")
			}
			modelSet = true
			v, next, err := agentFlagValue(args, i, "--model")
			if err != nil {
				return nil, err
			}
			c.agentModel, i = v, next
		case a == "--name" || strings.HasPrefix(a, "--name="):
			if nameSet {
				return nil, agentRepeatedFlag("--name")
			}
			nameSet = true
			v, next, err := agentFlagValue(args, i, "--name")
			if err != nil {
				return nil, err
			}
			c.agentName, i = v, next
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
	// --new launches the session this prompt starts, so it conflicts with
	// everything that presumes an existing one. Each conflict is refused by
	// name rather than resolved: a new session has no running turn to steer
	// and no queue to sit behind, and picking a winner between a ref and
	// --new would address a session the caller did not name.
	if c.agentNew {
		if modeSet != "" {
			return nil, fmt.Errorf("agent prompt: %s needs a turn to act on, so it cannot be combined with --new", modeSet)
		}
	} else {
		if modelSet {
			return nil, errors.New("agent prompt: --model only applies to a session gmux is launching; pass it with --new")
		}
		if nameSet {
			return nil, errors.New("agent prompt: --name only applies to a session gmux is launching; pass it with --new")
		}
	}
	if i >= len(args) && !c.agentNew {
		return nil, errors.New("agent prompt requires a session id")
	}
	// --timeout bounds the wait, so it means nothing without one. Refusing the
	// combination rather than ignoring the flag is the same rule the rest of this
	// surface follows (a repeated flag, `?tail=` on a message scope): a caller who
	// set a bound and got none would believe they had constrained something. It
	// matters more here than elsewhere — `--no-wait` still blocks internally,
	// for admission (a plain prompt, an idle follow-up) or for delivery (a steer,
	// a merged follow-up), and neither window is something this flag can move.
	if c.agentNoWait && timeoutSet {
		return nil, errors.New("agent prompt: --timeout bounds the wait, so it cannot be combined with --no-wait")
	}
	var rest []string
	if c.agentNew {
		// No ref to consume: everything left is the prompt.
		rest = args[i:]
	} else {
		c.ref = args[i]
		rest = args[i+1:]
	}
	if c.agentNew && len(rest) == 1 && rest[0] == "-" {
		// The conventional spelling of "the prompt is on stdin". Only
		// meaningful as the whole prompt: a literal "-" prompt is not a
		// prompt anyone means. Scoped to --new: on the ref path everything
		// after the id is verbatim prompt text, `-` included, and that
		// promise predates this flag.
		rest = nil
	}
	switch len(rest) {
	case 0:
		// Prompt text comes from piped stdin; the tty case is rejected at
		// execution time, where the CLI can see whether stdin is a pipe.
	case 1:
		t := rest[0]
		c.promptText = &t
	default:
		if c.agentNew {
			// The overwhelmingly likely typo: a caller who wrote
			// `--new <id> <prompt>` out of habit. --new IS the session
			// selection, so naming one alongside it addresses two sessions.
			return nil, errors.New("agent prompt: --new starts a new session, so it takes no session id — pass only the prompt (quote the whole prompt), or pipe it on stdin")
		}
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

// agentFlagValue reads the value of a `--flag value` / `--flag=value` pair at
// args[i], returning the value and the index of the token it consumed.
//
// An empty value is refused rather than treated as absence: `--model=` asks
// for a model named "", which the adapter would silently drop, leaving the
// caller believing they had pinned a model.
func agentFlagValue(args []string, i int, name string) (string, int, error) {
	a := args[i]
	val := ""
	if strings.HasPrefix(a, name+"=") {
		val = strings.TrimPrefix(a, name+"=")
	} else {
		i++
		if i >= len(args) {
			return "", i, fmt.Errorf("%s requires a value", name)
		}
		val = args[i]
	}
	if strings.TrimSpace(val) == "" {
		return "", i, fmt.Errorf("%s requires a non-empty value", name)
	}
	return val, i, nil
}

func agentRepeatedFlag(flag string) error {
	return fmt.Errorf("agent prompt: %s given more than once", flag)
}

// cmdAgent dispatches the namespace's verbs.
func cmdAgent(c *command) int {
	switch c.agentSub {
	case "prompt":
		if c.agentNew {
			return cmdAgentPromptNew(c.agentModel, c.agentName, c.agentNoWait, c.timeout, c.promptText)
		}
		return cmdAgentPrompt(c.ref, c.agentMode, c.agentNoWait, c.timeout, c.promptText)
	case "cancel":
		return cmdAgentCancel(c.ref)
	case "status":
		return cmdAgentStatus(c.ref, c.json)
	case "logs":
		return cmdAgentLogs(c.ref, c.tailLines, c.logTypes, c.json)
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

// cmdAgentPrompt implements `gmux agent prompt <ref>`.
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
	return deliverPrompt(sess, mode, noWait, timeoutSecs, prompt)
}

// agentLaunchSession spawns a detached session and returns its id. A variable
// so tests can drive `--new`'s output contract without forking a real agent.
var agentLaunchSession = launchDetachedSession

// agentLaunchAdapter is the adapter `--new` launches through. The launch is
// pi-only for now (there is no --adapter flag), exactly like the rest of this
// namespace; a variable so tests can substitute a non-launcher adapter.
var agentLaunchAdapter adapter.Adapter = adapters.NewPi()

// cmdAgentPromptNew implements `gmux agent prompt --new`: launch a session and
// send it its first prompt, in one command.
//
// Ordering is the whole design. The prompt is read and the argv translated
// BEFORE anything is spawned, so a usage error never leaves an orphan session
// behind. Once the spawn succeeds the session id is written to stdout
// immediately — before the prompt is even delivered — because from that
// moment the caller owns a session they must be able to address no matter
// what happens next. Everything after that point (admission failure, a turn
// that errors, a --timeout) reports on stderr and exits per the taxonomy,
// leaving stdout's first line exactly one bare session id.
//
// So the id line means one thing only: the session exists and is addressable.
// It is not an admission receipt, not a readiness signal and not a claim that
// the prompt was delivered. A post-spawn failure leaves that session alive (or
// dead-retained) and the caller owning it: retry against the printed id, or
// kill it.
//
// The prompt itself travels the ordinary readiness-gated /prompt transport, so
// a session that never becomes ready fails its first prompt exactly as it
// would fail its tenth: admission is the single health event, and there is no
// launch-shaped special case to keep in sync.
func cmdAgentPromptNew(model, name string, noWait bool, timeoutSecs int, text *string) int {
	prompt, err := readPromptText(text, os.Stdin, localterm.IsInteractive())
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return waitExitError
	}
	argv, ok := agentLaunchArgv(agentLaunchAdapter, adapter.LaunchOptions{Model: model, Name: name})
	if !ok {
		fmt.Fprintf(os.Stderr, "gmux: %s: %s cannot be launched by gmux agent prompt --new\n",
			codeUnsupportedAdapter, agentLaunchAdapter.Name())
		fmt.Fprintln(os.Stderr, "gmux: start it yourself with 'gmux -d -- <command>' and prompt the id it prints")
		return waitExitError
	}
	id, err := agentLaunchSession(argv)
	if err != nil {
		// Nothing was registered, so there is no id to hand back: the caller
		// paid for nothing and has nothing to clean up.
		fmt.Fprintf(os.Stderr, "gmux: could not start %s: %s\n", strings.Join(argv, " "), err)
		return waitExitError
	}
	// Line 1 of stdout, unconditionally and before delivery (ADR 0027, the
	// 2026-07-27 --new amendment: "stdout line 1 means the session exists and
	// is addressable"). It asserts NOTHING about admission, readiness or the
	// turn — the exit code carries every one of those verdicts. Printing it
	// here rather than at admission is what makes the guarantee
	// unconditional: whatever fails next, the caller can address, retry
	// against or kill the session it just paid for. os.Stdout is unbuffered,
	// so a watcher reading the pipe can attach or tail during readiness.
	if _, err := fmt.Fprintln(os.Stdout, id); err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return waitExitError
	}
	return deliverPrompt(cliSession{ID: id}, agentModePrompt, noWait, timeoutSecs, prompt)
}

// agentLaunchArgv translates launch options into an argv, or reports that this
// adapter has no launch support.
func agentLaunchArgv(a adapter.Adapter, opts adapter.LaunchOptions) ([]string, bool) {
	l, ok := a.(adapter.AgentLauncher)
	if !ok {
		return nil, false
	}
	argv, ok := l.LaunchCommand(opts)
	if !ok || len(argv) == 0 {
		return nil, false
	}
	return argv, true
}

// deliverPrompt performs the POST /prompt round trip shared by both prompt
// shapes. sess must already be known local.
func deliverPrompt(sess cliSession, mode string, noWait bool, timeoutSecs int, prompt string) int {
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
	// Same notice as `gmux wait`, around the blocking call only: a ^C here stops
	// the wait, not the turn. `--no-wait` still blocks — on admission, or on
	// delivery for a mode that joins a running turn — and the notice is just as
	// true for it: the session keeps running either way.
	stopNotice := noticeInterruptedWait(os.Stderr, sess.ID)
	resp, err := client.Post(url, "application/json", strings.NewReader(string(body)))
	stopNotice()
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
	// Output is the turn's final assistant message as the adapter asserted it,
	// present only for a completed turn on a result-bearing session.
	Output string `json:"output"`
	// Truncated says the adapter capped Output at the source.
	Truncated bool `json:"truncated"`
}

// reportAgentPromptSuccess turns a 2xx prompt response into output and an exit
// code.
//
// A completed turn prints the agent's answer, which the daemon selected at turn
// close with the same selector `gmux agent status` and `gmux wait` use. Every
// other conclusion prints NO result — a stale prior-turn answer must never be
// presented as this turn's — and reports the condition on stderr instead.
// `resumed` is worth a stderr line because it says the session was dead and has
// been restarted: a state change the caller did not ask for.
//
// One coupling to know about: the error hint below names `gmux agent status`
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
			noteTruncatedAnswer(sess, res.Truncated)
		}
		return waitExitOK
	case waitOutcomeInterrupted:
		fmt.Fprintf(os.Stderr, "gmux: the turn was interrupted before it finished (session %s)\n", displayID(sess))
		return waitExitInterrupted
	case waitOutcomeError:
		if res.Cause != "" {
			fmt.Fprintf(os.Stderr, "gmux: the turn ended in an error (%s); see gmux agent status %s\n",
				res.Cause, shortID(sess.ID))
		} else {
			fmt.Fprintf(os.Stderr, "gmux: the turn ended in an error; see gmux agent status %s\n", shortID(sess.ID))
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

// agentAnswerRead is the outcome of the daemon's semantic message-scope read:
// the latest final assistant message, or the reason there is none.
//
// The reason is kept as the daemon's own code (and message) rather than
// collapsed into an error: `status` reports "no answer yet" as part of a
// perfectly good snapshot, while a transport failure is a failure.
type agentAnswerRead struct {
	Text string
	// Code is the daemon's stable error code when there is no answer
	// (no_message, no_conversation, unsupported_adapter, ...), "" on success.
	Code string
	// Message is the daemon's prose for Code.
	Message string
	// Failed marks a read that could not be performed at all (transport,
	// version skew, an empty 200): not "there is no answer", but "gmux does not
	// know". Report is the line to print for it.
	Failed bool
	Report string
}

// readAgentAnswer performs the store-only message-scope read for sess. It never
// resumes and never needs a live runner, so it works on a dead retained
// session.
func readAgentAnswer(sess cliSession) agentAnswerRead {
	client := gmuxdClient()
	// No client deadline: the daemon re-reads and renders the agent's whole
	// stored conversation to select one message, which on a long session and a
	// cold filesystem can outlast the default 5s and turn a readable answer
	// into a transport error.
	client.Timeout = 0
	url := gmuxdBaseURL() + "/v1/sessions/" + sess.ID + "/conversation?scope=message"
	resp, err := client.Get(url)
	if err != nil {
		return agentAnswerRead{Failed: true, Report: err.Error()}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil && !errors.Is(err, io.EOF) {
		return agentAnswerRead{Failed: true, Report: err.Error()}
	}
	if msg := agentOutputSkewError(resp.StatusCode, resp.Header.Get(conversationScopeHeader)); msg != "" {
		return agentAnswerRead{Failed: true, Report: msg}
	}
	if resp.StatusCode >= 300 {
		code, msg := errorCode(raw), extractMessage(raw)
		if code == "" {
			return agentAnswerRead{Failed: true, Report: agentSkewOrRawReport("status", resp.StatusCode, raw)}
		}
		return agentAnswerRead{Code: code, Message: msg}
	}
	if len(raw) == 0 {
		// A marked but empty 200. The daemon cannot currently produce one (it
		// answers no_message instead), so this is a contract guard rather than a
		// known case: reporting silence as the answer is the one thing the
		// message scope exists to never do.
		return agentAnswerRead{Failed: true, Report: fmt.Sprintf("the daemon reported a message for %s but sent no content", displayID(sess))}
	}
	return agentAnswerRead{Text: strings.TrimRight(string(raw), "\n")}
}

// agentSkewOrRawReport describes an envelope-less error response: a bare 404 is
// a missing ROUTE (the session was resolved through this same daemon one
// request earlier), i.e. version skew; anything else is passed through.
func agentSkewOrRawReport(verb string, status int, body []byte) string {
	if status == http.StatusNotFound {
		return fmt.Sprintf("this gmuxd predates 'gmux agent %s' (no such route); restart the daemon with 'gmux daemon restart'", verb)
	}
	return fmt.Sprintf("agent %s failed: %s", verb, strings.TrimSpace(string(body)))
}

// cmdAgentLogs implements `gmux agent logs`: print the session's conversation
// as markdown (the last n messages of the requested types), read from the
// agent's own stored conversation. With json, the same selection is printed as
// the machine shape instead: a JSON array of {role, type, text, prose}.
//
// The filter travels as `types=` and the serialization as `format=`, so the
// daemon serves exactly the messages asked for and -n counts them post-filter.
// An empty types list means the daemon's default (user + prose-bearing agent
// messages), which is deliberately the same default as no flags at all.
//
// Store-only, exactly like `agent status`: it never starts, resumes or even
// touches a runner, so it works on a dead retained session, and it is
// local-only for the same reason the rest of the namespace is. The daemon's
// transcript scope is the read (`?tail=N`, no scope parameter — that IS the
// transcript), so nothing new is asked of gmuxd.
//
// Error taxonomy is the message scope's, verbatim: the daemon's code and message
// are printed as-is by reportAgentError, an envelope-less 404 is reported as
// version skew rather than a missing session, and a non-renderer adapter's
// refusal carries the read-side 'gmux tail' hint — the raw view is exactly
// what a caller who cannot have a transcript should reach for.
//
// No marker-header guard here, unlike the message-scope read: that guard exists
// because an old daemon ignoring `scope=message` would answer with the whole
// transcript, and the whole transcript is precisely what this verb asked for.
// The only skew that can bite is a daemon with no /conversation route at all,
// which arrives as an envelope-less 404 and is reported as such.
func cmdAgentLogs(ref string, n int, types []string, asJSON bool) int {
	sess, ok := resolveAgentSession(ref, "logs")
	if !ok {
		return waitExitError
	}
	client := gmuxdClient()
	// No client deadline, for the same reason the message-scope read drops it: the
	// daemon re-reads and renders the agent's whole stored conversation, which
	// on a long session and a cold filesystem can outlast the default 5s and
	// turn a readable transcript into a transport error.
	client.Timeout = 0
	url := fmt.Sprintf("%s/v1/sessions/%s/conversation?tail=%d", gmuxdBaseURL(), sess.ID, n)
	if len(types) > 0 {
		url += "&types=" + strings.Join(types, ",")
	}
	if asJSON {
		url += "&format=json"
	}
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
	if asJSON {
		// The daemon wraps its JSON in the house envelope; the CLI's contract is
		// the bare array, so the messages are unwrapped and re-emitted. An
		// undecodable body is a contract breach, not an empty conversation.
		var env struct {
			Data struct {
				Messages []json.RawMessage `json:"messages"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &env); err != nil || env.Data.Messages == nil {
			fmt.Fprintf(os.Stderr, "gmux: this gmuxd cannot serve 'gmux agent logs --json' (it answered with something else); restart the daemon with 'gmux daemon restart'\n")
			return waitExitError
		}
		out, err := json.MarshalIndent(env.Data.Messages, "", "  ")
		if err != nil {
			fmt.Fprintln(os.Stderr, "gmux:", err)
			return waitExitError
		}
		if _, err := fmt.Fprintln(os.Stdout, string(out)); err != nil {
			fmt.Fprintln(os.Stderr, "gmux:", err)
			return waitExitError
		}
		return waitExitOK
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
		return "this gmuxd predates 'gmux agent status' (it answered with the whole transcript); restart the daemon with 'gmux daemon restart', or use 'gmux tail'"
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
	// delivery_timeout describe an INDETERMINATE delivery (retrying may duplicate
	// the prompt) while execution_timeout means the turn is still running — three
	// meanings one "timeout" code would have flattened into the bucket scripts
	// blindly retry. The stable code printed above separates them.
	return waitExitError
}

// agentErrorHint adds the one actionable next step the daemon's message does
// not already carry. Hints never soften an indeterminate outcome.
//
// The unsupported hint is verb-aware: on a write verb (prompt, cancel) the
// fallback is raw input, while on the read verb the caller wants to READ, and
// telling them to send keystrokes answers a question they did not ask.
// `logs` is the only read that routes through here — `status` reports a
// non-renderer adapter as part of its own report instead of as an error.
func agentErrorHint(code, verb string, sess cliSession) string {
	tailHint := "'gmux tail " + shortID(sess.ID) + "' shows this session directly"
	switch code {
	case codeUnsupportedAdapter, codeUnsupportedAction:
		if verb == "logs" {
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
  gmux agent prompt --new [--model M] [--name N] [--no-wait] [--timeout N] [prompt]

  <prompt>          the prompt text; omit it to read the prompt from stdin
                    (under --new, a lone - means stdin too; after a session
                    id, - is literal prompt text like everything else)
  --new             launch a new pi session and send this as its first prompt
  --model M         --new only: the model to launch with (pi's --model)
  --name N          --new only: the session display name (pi's --name)
  --no-wait         return as soon as the prompt is admitted, without waiting
                    for the turn to finish (see "What --no-wait waits for")
  --follow-up       queue the prompt to submit after the current turn ends
  --steer           redirect the turn that is running right now (fails if idle)
  --timeout/-t N    give up waiting after N seconds (0/absent: wait indefinitely)

--follow-up and --steer are mutually exclusive; --no-wait composes with either.
Pass either a session id or --new, never both. --new rejects --follow-up and
--steer (a session that does not exist yet has no turn to queue behind or
steer), and --model/--name mean nothing without it.

What --no-wait waits for depends on whether the prompt STARTS a turn:

  - a plain prompt, and --follow-up delivered to an IDLE agent, start one, so
    admission is an observable event: --no-wait returns once the agent has
    actually begun the turn (bounded by the 60s admission window), and exit 0
    is a health signal about this session.
  - --steer, and --follow-up that merges into a RUNNING turn, join a turn that
    is already under way. There is nothing to admit beyond delivery — the turn
    was admitted before this prompt existed — so --no-wait returns as soon as
    the text is delivered, and exit 0 claims delivery, not a fresh turn.

Launching (--new). gmux starts the session the same way 'gmux -d -- pi' does,
from this shell's env and cwd (local daemon only), then delivers the prompt
over the ordinary readiness-gated path — so a session that never becomes
ready fails its first prompt exactly like any later one.

  THE SESSION ID IS ALWAYS STDOUT LINE 1, printed the moment the session
  exists and before the prompt is delivered. It means exactly one thing:
  the session exists and is addressable. It is NOT an admission receipt,
  not a readiness signal and not a claim that the prompt was delivered —
  the exit code carries all of those. Under --new the completion signal is
  therefore the EXIT CODE, not non-empty stdout: a successful sync run
  prints the id, then the answer. With --no-wait the bare id is the only
  output, and exit 0 does mean the prompt was admitted.

  A failure AFTER the launch leaves the session behind, and it is yours:
  it may still be running. Retry against the printed id, or 'gmux kill' it.
  A failed launch prints no id, because no session exists.

  --new must come before the prompt. After a session id it is prompt text
  like any other token, so 'gmux agent prompt s1 --new' prompts s1 with the
  literal text '--new'.

  --timeout still bounds only the wait, never the launch: readiness runs on
  the adapter's own fixed window (10s for pi), and admission on the daemon's
  (60s).

  --no-wait gates exit 0 on ADMISSION, not on delivery: the id still prints
  immediately, but the process returns only once the agent has actually
  started the turn (or the admission window expires). That is the health
  event the handoff pattern relies on — id=$(gmux agent prompt --new
  --no-wait …) returns at process exit, so on a sick session the launch line
  can block up to that window instead of returning at delivery. Exit 0 buying
  the stronger claim is the point. It holds unconditionally here because a
  --new prompt always starts the session's first turn; --steer and --follow-up
  are refused with --new, so the delivery-only case above cannot arise.

Waits by default: the command returns when the turn ends and prints the agent's
answer. Exit status is 0 for a completed turn, 2 for an interrupted one, and 1
for anything else (a failed turn, a --timeout, a dead runner). A turn that did
not complete prints no answer — see what happened with 'gmux agent status <id>'.

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
	case "agent status":
		fmt.Fprint(w, `gmux agent status — show what matters about an agent session right now

  gmux agent status <id> [--json]

  --json            print the machine shape instead of the report

The verb for "I don't know what I want, show me what matters". Always the same
three parts, whatever the session is doing:

  ## State         short id and adapter, alive/dead, active/idle, and the last
                   CLOSED turn's outcome (completed/interrupted/error) with a
                   rough recency. A running turn has no outcome yet, and a
                   session that died mid-turn reads "the turn never finished
                   (runner died)" — the verdict 'gmux wait' reaches for that
                   same row, never "completed"
  ## Triggered by  the first lines of the user message that started the
                   current (or last) turn — labeled so it can never be
                   mistaken for the answer, and found however many tool
                   calls the turn has made since
  ## Answer        the agent's final message when the session is idle
  ## Recent        instead of Answer while a turn is running: the last few
                   messages and a working indicator

A SNAPSHOT: store-only, no runner contact, no resume, so it works on a dead
retained session — and it can be staler than the answer a 'gmux wait' or a
synchronous 'gmux agent prompt' carries, because those report what the adapter
asserted at turn close while this reads the stored conversation.

The State part is adapter-independent, so a session whose conversation gmux
cannot read (claude, codex, a shell) still gets it, with a note instead of the
content rather than an error. Local sessions only.

--json prints one object mirroring the three parts: {state:{...},
trigger:{text,truncated}, content:{kind,...}}, where content.kind is "answer",
"recent" or "none". Fields describing a turn that does not exist are ABSENT
rather than empty: state.last_turn_outcome (and state.last_turn_cause, today
only runner_died) appear only for a concluded turn, and trigger.code only when
the conversation could not be read. That object is the machine contract for
this read.

The answer ALONE, for a script that wants nothing else:

  gmux agent logs --agent -n 1 <id>
`)
	case "agent logs":
		fmt.Fprint(w, `gmux agent logs — print the exact conversation text you want

  gmux agent logs <id> [-n N] [--user] [--agent] [--tool] [--all] [--json]

  -n N              how many messages to print (default 100), counted AFTER
                    the type filter
  --user            user messages (prompts, steers, merged follow-ups)
  --agent           assistant messages that carry prose
  --tool            messages that are only tool calls
  --all             every type gmux renders
  --json            print the machine shape instead of markdown

Prints the agent's conversation as markdown, read from the agent's own stored
conversation: '## User' / '## Assistant' messages with compact [tool]
one-liners — the actual exchange, not the TUI's box-drawing and spinners.
-n counts MESSAGES here, not lines.

With no type flag you get the conversation without the machinery: user
messages and assistant messages that actually said something. Type flags
REPLACE that default rather than adding to it, and they compose
('--user --tool'), so the view is always exactly the types you named.

  gmux agent logs --agent -n 1 <id>   the latest answer, alone
  gmux agent logs --tool -n 20 <id>   what it has been running

There is no --thinking: no adapter renders thinking blocks today (pi's own
transcript drops them), so the flag is refused by name instead of answering
with silence. 'gmux tail <id>' shows whatever the TUI painted.

--json prints a JSON array of {role, type, text, prose} — the machine contract
for this read.

A store-only snapshot, like 'gmux agent status': it never starts or resumes
anything, so it works on a dead retained session. Local sessions only, and
only for agents whose conversation gmux can read — anything else answers
unsupported_adapter, where 'gmux tail <id>' shows the terminal instead.

The reading verbs split by what you know you want:

  gmux tail <id>          the raw screen (any session)
  gmux agent logs <id>    the exact text you want (this command)
  gmux agent status <id>  show me what matters, I'll decide after

'gmux agent logs --agent -n 1' approximates the old 'gmux agent output'. It is
a snapshot read, so it can be staler than the answer a 'gmux wait' or a
synchronous prompt carries — those report the adapter's assertion at turn
close.
`)
	default:
		fmt.Fprint(w, `gmux agent — drive an agent session by intent instead of keystrokes

  gmux agent prompt [--no-wait] [--follow-up|--steer] [--timeout|-t N] <id> [prompt]
                                    send a prompt and wait for the turn to end
  gmux agent prompt --new [--model M] [--name N] [prompt]
                                    launch a session and send its first prompt
  gmux agent cancel <id>            interrupt the running turn
  gmux agent status <id> [--json]   state, trigger and the content that matters
  gmux agent logs <id> [-n N] [--user|--agent|--tool|--all] [--json]
                                    print the conversation, filtered

Unlike 'gmux send', which types raw bytes at a terminal and cannot say whether
the agent read them, prompt and cancel wait until the agent can accept input,
submit the way that agent expects, and report what the daemon observed. status
and logs are store-only snapshots: they never start or resume the agent.

Prompting. A plain prompt starts a fresh turn (restarting a dead retained
session if needed); --new launches a new pi session first and prints its id as
stdout line 1 before the answer; --steer redirects the turn running right now;
--follow-up queues text to submit after the current turn (neither applies to
--new). --follow-up and --steer are mutually exclusive; --no-wait composes
with either and only decides whether you block. Flags go before the id;
everything after the id is the prompt, verbatim. Omit the prompt to pipe it on
stdin (it stays one prompt).

Reading. The split is what you know you want, not what shape comes back:
'gmux tail' is the raw screen (any session); 'gmux agent logs' is the exact
text you want (filter by --user/--agent/--tool/--all, -n counts messages
post-filter); 'gmux agent status' is "show me what matters" — state line,
trigger excerpt, and the answer or the last few messages. Both agent reads are
store-only snapshots: they never start or resume the agent, they work on a dead
retained session, and both take --json for a stable machine shape.

Results. A synchronous prompt — and 'gmux wait' — prints the agent's answer
when the turn completes. A failed or interrupted turn prints nothing (a stale
answer is worse than none); 'gmux agent status <id>' shows what exists, even
after the session died — as a snapshot it can be staler than the answer a wait
carries. Exit codes: 0 completed, 2 intentionally interrupted,
1 anything else (failed turn, timeout, dead runner, usage, transport).

Retrying. admission_timeout, delivery_timeout, and
transport_error are indeterminate: the prompt may already have landed, so
inspect before resending. A dropped connection is indeterminate too. These
codes guarantee nothing was delivered and are safe to retry: runner_outdated,
precondition_failed, delivery_pending, not_ready, not_running, and
incarnation_mismatch.

Scope. Agent sessions on this host only, and pi only for now; other agents
fail with unsupported_adapter — drive those with 'gmux send' and read them
with 'gmux tail'.

  gmux agent prompt|logs|status|cancel --help   per-verb help
`)
	}
}
