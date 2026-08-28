package main

// agent.go implements semantic delivery and exchange-oriented reads.
//
//	gmux agent prompt [--no-wait] [--follow-up|--steer] [--timeout N] <ref> [prompt]
//	gmux agent cancel <ref>
//	gmux agent logs <ref> [-n N]
//
// Semantic actions never fall back to raw terminal input. Synchronous prompts
// and wait share one stdout exchange report; logs reads adapter storage only.

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
	case "logs":
		return parseAgentLogs(rest)
	}
	return nil, fmt.Errorf("unknown agent verb %q; expected prompt, logs or cancel", head)
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

// parseAgentLogs handles `gmux agent logs <ref> [-n N]`.
//
// Flags sit on either side of the ref (the interspersed convention `tail` and
// `wait` already use): there is no verbatim trailing content here to protect,
// and a caller who typed `gmux agent logs s1 -n 5` meant the count.
//
// -n counts user-bounded exchanges, not terminal lines. The daemon renders the
// selected native history through the same human formatter as waits/prompts.
func parseAgentLogs(args []string) (*command, error) {
	if len(args) == 1 && isHelpToken(args[0]) {
		return &command{mode: modeHelp, helpTopic: "agent logs"}, nil
	}
	c := &command{mode: modeAgent, agentSub: "logs", tailLines: agentLogsDefaultExchanges}
	fs := newFlagSet("agent logs")
	fs.IntVar(&c.tailLines, "n", agentLogsDefaultExchanges, "number of conversation exchanges to show")
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
		return nil, errors.New("agent logs: -n must be a positive number of exchanges")
	}
	c.ref = pos[0]
	return c, nil
}

// agentLogsDefaultExchanges is the default native-history tail.
const agentLogsDefaultExchanges = 1

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
	// surface follows: a caller who
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
			verb, sess.ID, sess.Peer)
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

// agentLaunchSession spawns a detached session with the daemon-issued launch
// receipt and returns its id. A variable keeps the real CLI/API path testable
// without forking a real agent.
var agentLaunchSession = launchDetachedSessionReserved

var agentReserveActiveSubagent = reserveActiveSubagent
var agentReleaseActiveSubagent = releaseActiveSubagent

type agentLaunchAdmissionError struct {
	code, message string
}

func (e *agentLaunchAdmissionError) Error() string {
	if e.code != "" && e.message != "" {
		return e.code + ": " + e.message
	}
	if e.message != "" {
		return e.message
	}
	return "active-subagent launch admission failed"
}

func reserveActiveSubagent(parent string) (string, error) {
	ensureGmuxd()
	body, _ := json.Marshal(map[string]any{"parent_session_id": func() any {
		if parent == "" {
			return nil
		}
		return parent
	}()})
	resp, err := gmuxdClient().Post(gmuxdBaseURL()+"/v1/agent-launch-reservations", "application/json", strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		code, message := errorCode(raw), extractMessage(raw)
		if resp.StatusCode == http.StatusNotFound && code == "" {
			message = "this gmuxd predates active-subagent launch admission; restart it with 'gmux daemon restart'"
		}
		return "", &agentLaunchAdmissionError{code: code, message: message}
	}
	var env struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.Data.Token == "" {
		return "", errors.New("invalid active-subagent reservation response from gmuxd")
	}
	return env.Data.Token, nil
}

func releaseActiveSubagent(token string) {
	if token == "" {
		return
	}
	req, err := http.NewRequest(http.MethodDelete, gmuxdBaseURL()+"/v1/agent-launch-reservations/"+token, nil)
	if err != nil {
		return
	}
	resp, err := gmuxdClient().Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

// agentLaunchAdapter is the adapter `--new` launches through. The launch is
// pi-only for now (there is no --adapter flag), exactly like the rest of this
// namespace; a variable so tests can substitute a non-launcher adapter.
var agentLaunchAdapter adapter.Adapter = adapters.NewPi()

// cmdAgentPromptNew implements `gmux agent prompt --new`: launch a session and
// send it its first prompt, in one command.
//
// Ordering is the whole design. The prompt is read and the argv translated
// BEFORE anything is spawned, so a usage error never leaves an orphan session
// behind. Once the spawn succeeds its id is emitted immediately — before the
// prompt is delivered — because from that moment the caller owns a session it
// must be able to address no matter what happens next. Which channel carries
// it follows the payload rule (ADR 0028 amendment): --no-wait prints the bare
// id on stdout because the id IS its payload; a synchronous run prints the
// bare id on stderr, keeping stdout for the exchange report alone. Everything
// after that point exits per the taxonomy without retracting that address.
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
	reservation, err := agentReserveActiveSubagent(os.Getenv("GMUX_SESSION_ID"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return waitExitError
	}
	defer agentReleaseActiveSubagent(reservation)
	id, err := agentLaunchSession(argv, reservation)
	if err != nil {
		// Nothing was registered, so there is no id to hand back: the caller
		// paid for nothing and has nothing to clean up.
		fmt.Fprintf(os.Stderr, "gmux: could not start %s: %s\n", strings.Join(argv, " "), err)
		return waitExitError
	}
	// Print as soon as the session exists, before delivery. --no-wait keeps
	// its command-substitution-friendly bare id on stdout; a synchronous run
	// puts the bare id on stderr so stdout stays the report alone. Either form
	// asserts only that the session is addressable — the exit code carries the
	// delivery verdict.
	if noWait {
		_, err = fmt.Fprintln(os.Stdout, id)
	} else {
		_, err = fmt.Fprintln(os.Stderr, id)
	}
	if err != nil {
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
	stopNotice := noticeInterruptedWait(os.Stdout, false)
	resp, err := client.Post(url, "application/json", strings.NewReader(string(body)))
	stopNotice()
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return waitExitError
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var domain struct {
			Data agentPromptResult `json:"data"`
		}
		if json.Unmarshal(raw, &domain) == nil && domain.Data.Outcome != "" {
			return reportAgentPromptSuccess(sess, resp.StatusCode, raw, noWait, prompt)
		}
		return reportAgentError(sess, "prompt", resp.StatusCode, raw)
	}
	return reportAgentPromptSuccess(sess, resp.StatusCode, raw, noWait, prompt)
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
	Admission        string             `json:"admission"`
	Exchanges        []adapter.Exchange `json:"exchanges"`
	OmittedExchanges int                `json:"omitted_exchanges"`
	OmittedBytes     int                `json:"omitted_bytes"`
	Previous         *int               `json:"previous_exchanges"`
	AnchorOrdinal    uint64             `json:"anchor_ordinal"`
	BaselineOrdinal  uint64             `json:"baseline_ordinal"`
	TimeoutSeconds   int                `json:"timeout_seconds"`
	Outcome          string             `json:"outcome"`
	Cause            string             `json:"cause"`
	Diagnostic       string             `json:"diagnostic"`
	Trigger          string             `json:"trigger"` // compatibility with pre-exchange runner frames
	TerminalPartial  bool               `json:"terminal_partial"`
	Resumed          bool               `json:"resumed"`
	// Output is terminal assistant prose asserted by the source. The renderer
	// labels it partial for non-completed outcomes.
	Output string `json:"output"`
	// Truncated says the adapter capped Output at the source.
	Truncated bool `json:"truncated"`
}

// reportAgentPromptSuccess routes every observed domain outcome through the
// shared exchange renderer. Detached prompts report admission only.
func reportAgentPromptSuccess(sess cliSession, status int, body []byte, noWait bool, submitted ...string) int {
	var env struct {
		Data agentPromptResult `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		fmt.Fprintln(os.Stderr, "gmux: decode prompt response:", err)
		return waitExitError
	}
	res := env.Data
	if noWait || status == http.StatusAccepted {
		// Detached: admission is all that is known, and it is not a result.
		return waitExitOK
	}
	switch res.Outcome {
	case waitOutcomeCompleted, waitOutcomeInterrupted, waitOutcomeError, outcomeTimeout:
		text := ""
		if len(submitted) > 0 {
			text = submitted[0]
		}
		return renderExchangeWait(res.waitResult(), false, res.TimeoutSeconds, text, os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "gmux: unexpected prompt outcome %q\n", res.Outcome)
		return waitExitError
	}
}

// waitResult projects the prompt payload onto the wait payload, which is the
// shape the shared reporter consumes. The two routes carry the same turn facts
// under the same names; keeping ONE reporter is what stops `gmux wait` and a
// synchronous prompt from growing different accounts of the same turn.
func (r agentPromptResult) waitResult() waitResult {
	return waitResult{
		Reason: "idle", Outcome: r.Outcome, Cause: r.Cause, Output: r.Output,
		Exchanges: r.Exchanges, OmittedExchanges: r.OmittedExchanges, OmittedBytes: r.OmittedBytes, Previous: r.Previous,
		AnchorOrdinal: r.AnchorOrdinal, BaselineOrdinal: r.BaselineOrdinal, Diagnostic: r.Diagnostic, Trigger: r.Trigger, TerminalPartial: r.TerminalPartial,
		Truncated: r.Truncated,
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

// cmdAgentLogs prints the last n native conversation exchanges.
//
// Store-only semantic history works on dead retained sessions. While an
// activity is live, the daemon reconciles persisted history with source frames
// by user-boundary position. The response marker prevents an older daemon's
// incompatible conversation shape from being printed as exchanges.
func cmdAgentLogs(ref string, n int, writers ...io.Writer) int {
	stdout := io.Writer(os.Stdout)
	if len(writers) > 0 && writers[0] != nil {
		stdout = writers[0]
	}
	sess, ok := resolveAgentSession(ref, "logs")
	if !ok {
		return waitExitError
	}
	client := gmuxdClient()
	// No client deadline: the daemon re-reads and renders stored history, which
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
	if resp.Header.Get(conversationScopeHeader) != "exchanges" {
		fmt.Fprintln(os.Stderr, "gmux: daemon conversation response predates exchange reports; restart gmuxd to resolve version skew")
		return waitExitError
	}
	if len(raw) == 0 {
		// A 200 with no body. The daemon answers no_conversation instead of
		// serving an empty transcript, so this is a contract guard: printing
		// nothing under exit 0 would tell a script the agent has done nothing,
		// which is a claim only the daemon's explicit codes may make.
		fmt.Fprintf(os.Stderr, "gmux: the daemon reported a conversation for %s but sent no content\n", displayID(sess))
		return waitExitError
	}
	tokens := resp.Header.Values(unreadTokenHeader)
	if len(tokens) == 0 {
		fmt.Fprintln(os.Stderr, "gmux: daemon response has no unread token; restart gmuxd to resolve version skew")
		return waitExitError
	}
	if _, err := stdout.Write(raw); err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return waitExitError
	}
	if err := consumeSession(sess, tokens[0]); err != nil {
		fmt.Fprintf(os.Stderr, "gmux: agent logs could not mark %s read: %v\n", displayID(sess), err)
		return waitExitError
	}
	return waitExitOK
}

// conversationScopeHeader must match the daemon's marker header.
const conversationScopeHeader = "X-Gmux-Conversation-Scope"

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
	// The one failure ADR 0027 cannot source-assert: pi's prompt submission can
	// fail before any agent loop event exists (no API key, no model), painting a
	// banner and emitting nothing. The admission wait times out as designed, and
	// the report appends what the screen says — labeled as the screen, on the
	// account channel, where best-effort diagnosis is allowed.
	if tail := failedToStartTail(code, sess); len(tail) > 0 {
		fmt.Fprintln(os.Stderr, "gmux:   last lines of the session's terminal (not an agent report):")
		for _, line := range tail {
			fmt.Fprintln(os.Stderr, "gmux:   |", line)
		}
	}
	// Every failure code is 1 under the global taxonomy, timeout-shaped ones
	// included. That loses nothing a number could carry: admission_timeout,
	// delivery_timeout describe an INDETERMINATE delivery (retrying may duplicate
	// the prompt) while execution_timeout means the turn is still running — three
	// meanings one "timeout" code would have flattened into the bucket scripts
	// blindly retry. The stable code printed above separates them.
	return waitExitError
}

// failedToStartTail returns the terminal-tail excerpt for the failure that has
// no turn behind it, and nothing for every other code: a tail attached to, say,
// an execution timeout would show a turn that is running perfectly well and
// invite the reader to diagnose a problem that is not there.
func failedToStartTail(code string, sess cliSession) []string {
	if code != codeAdmissionTimeout {
		return nil
	}
	return terminalTailExcerpt(sess, failedToStartTailLines)
}

// failedToStartTailLines is how much screen the failed-to-start report shows:
// enough for pi's banner and the line above it, not enough to bury the report.
const failedToStartTailLines = 8

// Daemon failure codes the CLI reacts to by name for reporting purposes. They
// are the daemon's vocabulary, duplicated across the module boundary like the
// rest of the wire words.
const (
	codeAdmissionTimeout = "admission_timeout"
	codeExecutionTimeout = "execution_timeout"
)

// agentErrorHint adds the one actionable next step the daemon's message does
// not already carry. Hints never soften an indeterminate outcome.
//
// The unsupported hint is verb-aware: on a write verb (prompt, cancel) the
// fallback is raw input, while on the read verb the caller wants to READ, and
// telling them to send keystrokes answers a question they did not ask.
// `logs` is the read path that routes through here.
func agentErrorHint(code, verb string, sess cliSession) string {
	tailHint := "'gmux tail " + sess.ID + "' shows this session directly"
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
		fmt.Fprint(w, `gmux agent prompt — send instructions and report the observed activity

  gmux agent prompt [--no-wait] [--follow-up|--steer] [--timeout|-t N] <id> [prompt]
  gmux agent prompt --new [--model M] [--name N] [--no-wait] [--timeout N] [prompt]

  --new             launch a new pi conversation
  --model M         --new only: model passed to pi
  --name N          --new only: conversation name passed to pi
  --no-wait         return after admission; print no activity report
  --follow-up       submit after the current model response
  --steer           redirect activity that is currently in progress
  --timeout/-t N    stop waiting after N seconds (0 means no timeout)

A synchronous prompt prints the same exchange report as 'gmux wait'. Additional
instructions never end another observer's wait; all user boundaries admitted
before the source settles appear in the report. Exit 0 means completed, 2 means
intentionally interrupted, and 1 means failure or timeout.

With synchronous --new, the bare session id is printed on stderr the moment the
session exists and stdout is the report alone. With --new --no-wait, stdout is
the bare id only. Before creating a runner, --new reserves one live semantic-
agent slot at its depth below the caller's current behavioral root. The host
default allows unlimited direct children, eight shared grandchildren, and no
deeper descendants. Promoted roots and independent top-level launches have
independent budgets. 'gmux ls' shows the sessions to inspect when the daemon
refuses a launch. Semantic reads and delivery hide runner residency:
an inactive conversation is resumed automatically when prompted.
`)
	case "agent cancel":
		fmt.Fprint(w, `gmux agent cancel — interrupt active agent work

  gmux agent cancel <id>

Returns after the interrupt is delivered. Use 'gmux wait <id>' if the next step
requires the activity to have settled.
`)
	case "agent logs":
		fmt.Fprint(w, `gmux agent logs — render stored conversation exchanges

  gmux agent logs <id> [-n N]

  -n N              positive number of visible exchanges (default 1)

Reads the adapter's native conversation branch without starting a process.
Each exchange begins at a user message; assistant/model responses are counted
as iterations and only terminal assistant prose is shown. Earlier exchanges
are summarized by count. An empty, resolvable timeline prints
[No exchanges yet]. A successful read marks the session's result consumed.
For the raw terminal view use 'gmux tail <id>'.
`)
	default:
		fmt.Fprint(w, `gmux agent — drive and read resumable agent conversations

  gmux agent prompt [flags] <id> [prompt]   send instructions and wait
  gmux agent prompt --new [flags] [prompt] launch and prompt a conversation
  gmux agent cancel <id>                   interrupt active work
  gmux agent logs <id> [-n N]              render stored exchanges

Prompt and wait use one exchange report on stdout for completed, interrupted,
failed, and timed-out activity; the exit code is the verdict. --quiet on wait
suppresses the report. logs is store-only and never starts a runner. 'gmux tail'
is the raw terminal view.

  gmux agent prompt|logs|cancel --help
`)
	}
}
