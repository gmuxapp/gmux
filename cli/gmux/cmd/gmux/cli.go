package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// mode is the top-level action gmux is being asked to perform.
type mode int

const (
	modeHelp       mode = iota // print usage and exit
	modeVersion                // print version and exit
	modeOpen                   // open the web UI
	modeRun                    // run a command in a new session (gmux -- <cmd>)
	modeList                   // gmux ls
	modeAttach                 // gmux attach <id>
	modeTail                   // gmux tail <id>
	modeKill                   // gmux kill <id>
	modeSend                   // gmux send <id> <text> [keys...]
	modeSendKeys               // gmux send-keys -t <id> ... (tmux-compat)
	modeWait                   // gmux wait <id>...
	modeAgent                  // gmux agent prompt|cancel|output <id>
	modeSession                // gmux session parent|promote|demote <id>
	modeEdit                   // gmux edit [file]
	modeEditChild              // (internal) gmux __edit-child [file]
	modeDaemon                 // gmux daemon <start|stop|restart|status|log-path|state ...>
	modeAuth                   // gmux auth
	modeRemote                 // gmux remote
	modeDumpEnv                // (internal) gmux __dump-env
	modeCodexHook              // (internal) gmux __codex-hook <Event>
	modeClaudeHook             // (internal) gmux __claude-hook
)

// command is the fully-parsed CLI invocation. One struct for every
// verb keeps dispatch in main.go a single switch with no flag-combo
// validation: each verb's parser only sets the fields it owns.
type command struct {
	mode mode

	// run (modeRun / internal __run)
	detach      bool
	runArgs     []string // the wrapped command, verbatim
	resumeID    string   // internal: reuse this session id
	initialCols int      // internal: pre-size PTY width
	initialRows int      // internal: pre-size PTY height

	// session-addressing verbs (attach/tail/kill/send/send-keys/wait)
	ref      string   // session reference; may carry an @peer suffix
	waitRefs []string // wait accepts one or more references, in argv order

	// ls
	all  bool
	json bool

	// tail (raw PTY output) and `agent logs` (conversation markdown).
	// One field, two units: tail counts lines, agent logs counts messages.
	tailLines int

	// send
	sendText *string  // literal text to type (nil = none)
	sendKeys []string // trailing key-name tokens (Enter, C-c, ...)
	sendWait bool     // --wait: block until the triggered turn completes

	// send-keys (tmux-compat)
	keysLiteral bool     // -l: treat args as literal text, not key names
	keys        []string // key/text arguments

	// agent (modeAgent)
	agentSub    string  // prompt|cancel|logs
	agentMode   string  // prompt|follow_up|steer (prompt verb only)
	agentNoWait bool    // --no-wait: return at the admission boundary
	promptText  *string // inline prompt text (nil = read stdin)
	agentNew    bool    // --new: launch the session this prompt starts
	agentModel  string  // --new only: model selector for the launch
	agentName   string  // --new only: session display name for the launch

	// session (modeSession)
	sessionSub  string // parent|promote|demote
	parentRef   string // parent target for `session parent`; empty with --clear
	clearParent bool

	// help
	helpTopic string // "agent", "agent prompt", ... ("" = full usage)

	// wait
	timeout  int    // --timeout seconds (0 = none)
	forText  string // --for-text: wait for substring in output
	forRegex string // --for-regex: wait for regex match in output
	quiet    bool   // --quiet: synchronize only, print no result

	// edit
	editFile string // file path to open

	// daemon
	daemonSub  string   // start|stop|restart|status|log-path|state
	daemonArgs []string // full argument list forwarded to gmuxd verbatim

	// codex hook (internal __codex-hook)
	codexHookEvent string // the codex hook event name (SessionStart, ...)
}

// reservedVerbs is the closed top-level namespace (ADR 0009). Growth
// happens under namespace groups, not new top-level verbs. Used to give
// "did you mean?" hints and to distinguish a removed flag from a stray
// command in the error-only migration shim.
//
// `agent` and `session` are namespace groups, not new bare action verbs: they
// grow under `gmux <group> <verb>` without reopening the frozen action list.
var reservedVerbs = []string{
	"open", "ls", "attach", "tail", "kill", "send", "send-keys",
	"wait", "agent", "session", "edit", "daemon", "auth", "remote", "version", "help",
}

// removedFlags maps every pre-2.0 action flag to the verb that replaced
// it. The migration shim (ADR 0009) recognizes these solely to print a
// precise error; no old behavior is carried.
var removedFlags = map[string]string{
	"--list": "gmux ls", "-l": "gmux ls",
	"--attach": "gmux attach <id>", "-a": "gmux attach <id>",
	"--tail": "gmux tail <id>", "-t": "gmux tail <id>",
	"--kill": "gmux kill <id>", "-k": "gmux kill <id>",
	"--send":      "gmux send <id> <text> Enter",
	"--no-submit": "gmux send <id> <text>  (omit a trailing Enter to not submit)",
	"--wait":      "gmux wait <id>",
	"--no-attach": "gmux -d -- <cmd>",
	"--host":      "address the session as <id>@<peer> instead",
	"--all":       "gmux ls --all",
}

// parseCLI parses argv (without program name) into a command.
func parseCLI(args []string) (*command, error) {
	if len(args) == 0 {
		return &command{mode: modeHelp}, nil
	}

	// Consume leading global flags. Only -d/--detach is global, and it
	// is valid solely on the run form (gmux -d -- <cmd>).
	detach := false
	for len(args) > 0 && (args[0] == "-d" || args[0] == "--detach") {
		detach = true
		args = args[1:]
	}

	if len(args) == 0 {
		return nil, errors.New("-d/--detach requires a command: gmux -d -- <cmd>")
	}

	head := args[0]
	rest := args[1:]

	// `gmux -- <cmd>` (and `gmux -d -- <cmd>`): everything after -- is
	// the command verbatim.
	if head == "--" {
		if len(rest) == 0 {
			return nil, errors.New("gmux -- requires a command")
		}
		return &command{mode: modeRun, detach: detach, runArgs: rest}, nil
	}

	// Past this point -d makes no sense — it only pairs with `--`.
	if detach {
		return nil, errors.New("-d/--detach only applies to 'gmux -- <cmd>'")
	}

	switch head {
	case "help", "-h", "--help", "?":
		// `gmux help <command>` routes to that command's page when one
		// exists; unknown topics stay lenient and print the synopsis, so
		// `gmux help whatever` is never an error. Daemon help is served by
		// the gmuxd binary so the two can never drift apart.
		if len(rest) > 0 {
			switch {
			case rest[0] == "agent":
				return &command{mode: modeHelp, helpTopic: strings.TrimSpace("agent " + strings.Join(rest[1:], " "))}, nil
			case rest[0] == "session":
				return &command{mode: modeHelp, helpTopic: "session"}, nil
			case rest[0] == "daemon":
				return &command{mode: modeDaemon, daemonArgs: []string{"help"}}, nil
			case helpTopicExists(rest[0]):
				return &command{mode: modeHelp, helpTopic: rest[0]}, nil
			}
		}
		return &command{mode: modeHelp}, nil
	case "version", "--version", "-v":
		return &command{mode: modeVersion}, nil
	case "open":
		if len(rest) > 0 {
			return nil, errors.New("open takes no arguments")
		}
		return &command{mode: modeOpen}, nil
	case "ls":
		return dispatchVerb("ls", rest, parseLs)
	case "attach":
		return dispatchVerb("attach", rest, func(a []string) (*command, error) {
			return parseRefOnly(modeAttach, "attach", a)
		})
	case "kill":
		return dispatchVerb("kill", rest, func(a []string) (*command, error) {
			return parseRefOnly(modeKill, "kill", a)
		})
	case "tail":
		return dispatchVerb("tail", rest, parseTail)
	case "send":
		return dispatchVerb("send", rest, parseSend)
	case "send-keys":
		return dispatchVerb("send-keys", rest, parseSendKeys)
	case "wait":
		return dispatchVerb("wait", rest, parseWait)
	case "agent":
		c, err := parseAgent(rest)
		if err != nil {
			// A mistake inside the namespace prints the namespace guide,
			// not the top-level synopsis.
			return nil, &usageError{topic: "agent", err: err}
		}
		return c, nil
	case "session":
		c, err := parseSession(rest)
		if err != nil {
			return nil, &usageError{topic: "session", err: err}
		}
		return c, nil
	case "edit":
		return dispatchVerb("edit", rest, parseEdit)
	case "daemon":
		if len(rest) == 0 || isHelpToken(rest[0]) {
			// gmuxd owns the daemon help page; forward so `gmux daemon`,
			// `gmux daemon --help` and `gmuxd help` print the same text.
			return &command{mode: modeDaemon, daemonArgs: []string{"help"}}, nil
		}
		return parseDaemon(rest)
	case "auth":
		if len(rest) > 0 {
			return nil, errors.New("auth takes no arguments")
		}
		return &command{mode: modeAuth}, nil
	case "remote":
		if len(rest) > 0 {
			return nil, errors.New("remote takes no arguments")
		}
		return &command{mode: modeRemote}, nil
	case "__run":
		return parseInternalRun(rest)
	case "__dump-env":
		return &command{mode: modeDumpEnv}, nil
	case "__codex-hook":
		if len(rest) != 1 {
			return nil, errors.New("__codex-hook requires exactly one event name")
		}
		return &command{mode: modeCodexHook, codexHookEvent: rest[0]}, nil
	case "__claude-hook":
		return &command{mode: modeClaudeHook}, nil
	case "__edit-child":
		// Child process of an editor session: prompt (if needed) and exec
		// the fallback editor. Reuses the editFile field.
		if len(rest) > 1 {
			return nil, errors.New("__edit-child takes at most one file path")
		}
		c := &command{mode: modeEditChild}
		if len(rest) == 1 {
			c.editFile = rest[0]
		}
		return c, nil
	}

	// Error-only migration shim (ADR 0009): recognize removed forms and
	// the dropped bare-command shorthand to emit precise guidance. Strip
	// any =value so `--host=laptop` matches the `--host` key.
	flagKey := head
	if eq := strings.IndexByte(flagKey, '='); eq > 0 {
		flagKey = flagKey[:eq]
	}
	if repl, ok := removedFlags[flagKey]; ok {
		return nil, fmt.Errorf("%s was removed in 2.0; use: %s", flagKey, repl)
	}
	if strings.HasPrefix(head, "-") {
		return nil, fmt.Errorf("unknown flag %q", head)
	}
	// Unknown bare word: it could be a fat-fingered verb OR a real program
	// the user meant to run but forgot `--` (e.g. `gmux sed -i ...`). We
	// can't know which, so always surface the run form, and add a verb
	// suggestion only when one is close. Never replace the run hint with
	// the suggestion alone — that misleads when the word is a real command.
	runHint := "to run a command use: gmux -- " + strings.Join(args, " ")
	// Agent verbs typed without their namespace get the namespace guide:
	// `gmux prompt <id> ...` is far more likely a missing `agent` than a
	// program named prompt.
	if head == "prompt" || head == "cancel" || head == "logs" {
		return nil, &usageError{topic: "agent", err: fmt.Errorf(
			"unknown command %q; agent commands are namespaced: gmux agent %s ... (%s)", head, head, runHint)}
	}
	if v := didYouMean(head); v != "" {
		return nil, fmt.Errorf("unknown command %q; did you mean %q? (%s)", head, v, runHint)
	}
	return nil, fmt.Errorf("unknown command %q; %s", head, runHint)
}

// dispatchVerb gives a verb with a dedicated help page uniform help
// handling: a leading help token (help, --help, -h, ?) prints the page, a
// -h/--help caught by the flag parser anywhere does too, and any parse
// error is tagged with the verb's topic so the error message is followed
// by that page rather than the top-level synopsis.
func dispatchVerb(topic string, args []string, parse func([]string) (*command, error)) (*command, error) {
	if len(args) > 0 && isHelpToken(args[0]) {
		return &command{mode: modeHelp, helpTopic: topic}, nil
	}
	c, err := parse(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return &command{mode: modeHelp, helpTopic: topic}, nil
		}
		return nil, &usageError{topic: topic, err: err}
	}
	return c, nil
}

func parseSession(args []string) (*command, error) {
	if len(args) == 0 || isHelpToken(args[0]) {
		return &command{mode: modeHelp, helpTopic: "session"}, nil
	}
	c := &command{mode: modeSession, sessionSub: args[0]}
	switch c.sessionSub {
	case "promote", "demote":
		if len(args) != 2 {
			return nil, fmt.Errorf("session %s requires a session id", c.sessionSub)
		}
		c.ref = args[1]
	case "parent":
		pos := make([]string, 0, 2)
		for _, arg := range args[1:] {
			if arg == "--clear" {
				if c.clearParent {
					return nil, errors.New("session parent: --clear specified more than once")
				}
				c.clearParent = true
				continue
			}
			if strings.HasPrefix(arg, "-") {
				return nil, fmt.Errorf("session parent: unknown flag %q", arg)
			}
			pos = append(pos, arg)
		}
		if c.clearParent {
			if len(pos) != 1 {
				return nil, errors.New("session parent --clear requires one session id and no parent id")
			}
			c.ref = pos[0]
		} else {
			if len(pos) != 2 {
				return nil, errors.New("session parent requires a session id and parent id (or --clear)")
			}
			c.ref, c.parentRef = pos[0], pos[1]
		}
	default:
		return nil, fmt.Errorf("unknown session command %q (expected parent, promote, or demote)", c.sessionSub)
	}
	return c, nil
}

func parseLs(args []string) (*command, error) {
	c := &command{mode: modeList}
	fs := newFlagSet("ls")
	fs.BoolVar(&c.all, "all", false, "include sessions from all peers")
	fs.BoolVar(&c.all, "a", false, "alias of --all")
	fs.BoolVar(&c.json, "json", false, "emit a JSON array")
	fs.BoolVar(&c.json, "j", false, "alias of --json")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if len(fs.Args()) > 0 {
		return nil, errors.New("ls takes no positional arguments")
	}
	return c, nil
}

func parseRefOnly(m mode, name string, args []string) (*command, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("%s requires a session id", name)
	}
	return &command{mode: m, ref: args[0]}, nil
}

// parseTail handles `gmux tail [-n N] <id>`: always the raw PTY view,
// -n counting lines.
//
// tail answers "what is on its screen" and nothing else. The
// conversation-markdown view it used to default to moved to
// `gmux agent logs` ("what has it been doing"), so no flag on this verb
// crosses the raw/semantic boundary any more — and the flags that used
// to select the view are refused by name rather than reported as
// unknown, in the spirit of the top-level removedFlags shim.
func parseTail(args []string) (*command, error) {
	c := &command{mode: modeTail, tailLines: 100}
	for _, a := range args {
		switch strings.SplitN(a, "=", 2)[0] {
		case "--raw", "-r", "-e":
			return nil, fmt.Errorf("tail: %s was removed; tail is always the raw terminal view, and the conversation view moved to: gmux agent logs <id>",
				strings.SplitN(a, "=", 2)[0])
		}
	}
	fs := newFlagSet("tail")
	fs.IntVar(&c.tailLines, "n", 100, "number of terminal lines to show")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return nil, err
	}
	if len(pos) != 1 {
		return nil, errors.New("tail requires a session id")
	}
	if c.tailLines <= 0 {
		return nil, errors.New("-n must be a positive line count")
	}
	c.ref = pos[0]
	return c, nil
}

// parseSend handles `gmux send [--wait] [--timeout N] [--] <id> [text]
// [Key...]`. The first positional is the session ref; an optional
// second positional is the literal text; any further bare tokens are
// key-name tokens. With no text and no keys, stdin supplies the text.
//
// send is raw: it types exactly what the caller specifies and never
// appends an adapter-derived keystroke. Semantic, adapter-aware prompt
// submission lives behind the agent layer (ADR 0027), not here.
//
// Grammar (ADR 0009 decision 9, verbatim-content rule): behavior
// modifiers precede the session ref; the ref is the first non-flag
// token, and *everything after it is verbatim* — including tokens that
// start with a dash. So `gmux send abc -v` sends the literal `-v`, and
// `gmux send abc '--weird text'` needs no `--` guard. This deliberately
// diverges from parsing flags anywhere: send's trailing content is
// arbitrary user text, so taxing the common case with a `--` guard to
// support two rare flags is the wrong trade. To wait, put the flag
// before the id: `gmux send --wait abc 'do it' Enter`. A `--` before
// the ref is accepted as an explicit end-of-flags marker.
func parseSend(args []string) (*command, error) {
	c := &command{mode: modeSend}
	i := 0
	for i < len(args) {
		a := args[i]
		if a == "--" { // explicit end-of-flags; ref follows
			i++
			break
		}
		if !strings.HasPrefix(a, "-") { // first non-flag token is the ref
			break
		}
		switch {
		case a == "--wait" || a == "-w":
			c.sendWait = true
		case a == "--timeout" || strings.HasPrefix(a, "--timeout=") ||
			a == "-t" || strings.HasPrefix(a, "-t="):
			// -t takes its value the same three ways --timeout does:
			// separate argument, --timeout=N, -t=N.
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
			if err != nil || n <= 0 {
				// -t is a universal tmux habit for "target session", and on this
				// verb it is --timeout, so a non-numeric value is far more likely
				// a misremembered target than a typo'd duration. Say which flag
				// the caller actually reached.
				if strings.HasPrefix(a, "-t") {
					return nil, fmt.Errorf("send: -t is --timeout here and takes a positive number of seconds (got %q); the session id is positional: gmux send <id> ... (tmux's '-t <id>' target is on 'gmux send-keys')", val)
				}
				return nil, errors.New("send: --timeout must be a positive number of seconds")
			}
			c.timeout = n
		case a == "--steering" || strings.HasPrefix(a, "--steering="):
			// Removed in favour of the semantic verb, and named rather than
			// reported as "unknown": the replacement is one rename away, and
			// every script that learned the old flag deserves to be told it.
			return nil, errors.New("send: --steering was replaced by: gmux agent prompt --steer <id> <prompt>")
		case a == "--follow-up" || strings.HasPrefix(a, "--follow-up="):
			return nil, errors.New("send: --follow-up was replaced by: gmux agent prompt --follow-up <id> <prompt>")
		default:
			return nil, fmt.Errorf("send: unknown flag %q (flags go before the session id; text after the id is literal)", a)
		}
		i++
	}
	if c.timeout > 0 && !c.sendWait {
		return nil, errors.New("send: --timeout only applies with --wait")
	}
	if i >= len(args) {
		return nil, errors.New("send requires a session id")
	}
	c.ref = args[i]
	rest := args[i+1:]
	if len(rest) > 0 {
		// Heuristic: the first non-key token is the literal text; the
		// rest are keys. If the first token is itself a key name, there
		// is no text and everything is keys.
		if !isKeyName(rest[0]) {
			t := rest[0]
			c.sendText = &t
			rest = rest[1:]
		}
		// Past the text, every token must name a key. Typing an unrecognized
		// name as literal text (which is what tmux send-keys does, and what
		// this verb used to do) is the worst available outcome: `send <id>
		// 'make test' Etner` would type "Etner" into the terminal, exit 0, and
		// report the input as delivered — a silent text injection presented as
		// success, for exactly the tokens a caller is most likely to get wrong
		// (a typo, or a key gmux deliberately does not encode). Refusing costs
		// nothing: literal text belongs in the text argument, and send-keys -l
		// still exists for the tmux contract.
		for _, k := range rest {
			if !isKeyName(k) {
				return nil, fmt.Errorf("send: %q is not a key name (only the first token after the id is literal text; run 'gmux send --help' for the key vocabulary)", k)
			}
		}
		c.sendKeys = rest
	}
	return c, nil
}

func parseSendKeys(args []string) (*command, error) {
	c := &command{mode: modeSendKeys}
	fs := newFlagSet("send-keys")
	var target string
	fs.StringVar(&target, "t", "", "target session id")
	fs.BoolVar(&c.keysLiteral, "l", false, "treat arguments as literal text")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if target == "" {
		return nil, errors.New("send-keys requires -t <id>")
	}
	c.ref = target
	c.keys = fs.Args()
	if len(c.keys) == 0 {
		return nil, errors.New("send-keys requires at least one key or string")
	}
	return c, nil
}

func parseWait(args []string) (*command, error) {
	c := &command{mode: modeWait}
	fs := newFlagSet("wait")
	fs.IntVar(&c.timeout, "timeout", 0, "fail after N seconds")
	fs.IntVar(&c.timeout, "t", 0, "alias of --timeout")
	fs.StringVar(&c.forText, "for-text", "", "wait for this substring in the session's output")
	fs.StringVar(&c.forRegex, "for-regex", "", "wait for a regex match in the session's output")
	fs.BoolVar(&c.quiet, "quiet", false, "do not print the agent's result; synchronize only")
	fs.BoolVar(&c.quiet, "q", false, "alias of --quiet")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return nil, err
	}
	if len(pos) == 0 {
		return nil, errors.New("wait requires at least one session id")
	}
	if c.timeout < 0 {
		return nil, errors.New("--timeout must be a non-negative number of seconds")
	}
	if c.forText != "" && c.forRegex != "" {
		return nil, errors.New("--for-text and --for-regex are mutually exclusive")
	}
	if len(pos) > 1 && (c.forText != "" || c.forRegex != "") {
		return nil, errors.New("--for-text and --for-regex require exactly one session id")
	}
	if c.forRegex != "" {
		// Validate here so a typo fails as a usage error instead of a
		// daemon round-trip; the daemon validates again server-side.
		if _, err := regexp.Compile(c.forRegex); err != nil {
			return nil, fmt.Errorf("--for-regex: %v", err)
		}
	}
	c.ref = pos[0]
	c.waitRefs = append([]string(nil), pos...)
	return c, nil
}

// parseEdit handles `gmux edit [file]`: at most one file path. The verb
// is designed to be usable as $EDITOR (git commit, etc.): it blocks
// until the editor session exits and propagates its exit code. With no
// path (the + launcher menu can't parameterize one) the session prompts
// for a path interactively. Today it opens a fallback terminal editor
// in a managed session; a future release renders a browser-based editor
// tab instead, keeping this interface (0-1 paths, blocking, exit code)
// unchanged.
func parseEdit(args []string) (*command, error) {
	if len(args) > 1 {
		return nil, errors.New("edit takes at most one file path")
	}
	if len(args) == 1 {
		if strings.HasPrefix(args[0], "-") {
			return nil, fmt.Errorf("edit takes no flags (got %q)", args[0])
		}
		return &command{mode: modeEdit, editFile: args[0]}, nil
	}
	return &command{mode: modeEdit}, nil
}

var daemonSubs = map[string]bool{
	"start": true, "stop": true, "restart": true, "status": true, "log-path": true,
}

var daemonStateSubs = map[string]bool{"check": true, "backup": true, "export": true, "reset": true}

func parseDaemon(args []string) (*command, error) {
	if len(args) > 0 && args[0] == "state" {
		// `gmux daemon state check|backup|export|reset [...]` forwards to gmuxd
		// verbatim (validation and help live server-side in the gmuxd
		// binary, mirroring the other daemon verbs). Accept -h/--help and
		// backup's target path as pass-through arguments.
		if len(args) >= 2 && (args[1] == "-h" || args[1] == "--help") {
			return &command{mode: modeDaemon, daemonSub: "state", daemonArgs: args}, nil
		}
		if len(args) < 2 || !daemonStateSubs[args[1]] {
			return nil, errors.New("daemon state requires one of: check, backup <path>, export, reset --yes")
		}
		return &command{mode: modeDaemon, daemonSub: "state", daemonArgs: args}, nil
	}
	if len(args) != 1 || !daemonSubs[args[0]] {
		return nil, errors.New("daemon requires one of: start, stop, restart, status, log-path, state")
	}
	return &command{mode: modeDaemon, daemonSub: args[0], daemonArgs: args}, nil
}

// parseInternalRun handles the hidden `gmux __run [directives] -- <cmd>`
// form the daemon uses to fork a runner. Directives precede `--`; the
// command follows it verbatim.
func parseInternalRun(args []string) (*command, error) {
	c := &command{mode: modeRun}
	fs := newFlagSet("__run")
	fs.StringVar(&c.resumeID, "resume-id", "", "reuse this session id")
	fs.IntVar(&c.initialCols, "initial-cols", 0, "pre-size PTY width")
	fs.IntVar(&c.initialRows, "initial-rows", 0, "pre-size PTY height")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if c.initialCols < 0 || c.initialRows < 0 {
		return nil, errors.New("--initial-cols and --initial-rows must be non-negative")
	}
	c.runArgs = fs.Args()
	if len(c.runArgs) == 0 {
		return nil, errors.New("__run requires a command")
	}
	return c, nil
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// parseInterspersed parses flags that may appear before or after the
// positional arguments. Go's flag package stops at the first
// positional; for management verbs (bounded positionals, no wrapped
// child command) we want `gmux wait abc --timeout 30` to work the same
// as `gmux wait --timeout 30 abc`. A literal `--` ends flag parsing.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positionals []string
	remaining := args
	for len(remaining) > 0 {
		if remaining[0] == "--" {
			return append(positionals, remaining[1:]...), nil
		}
		if err := fs.Parse(remaining); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		if len(rest) == len(remaining) {
			// fs.Parse consumed nothing: first token is a positional.
			positionals = append(positionals, rest[0])
			remaining = rest[1:]
			continue
		}
		remaining = rest
	}
	return positionals, nil
}

// didYouMean returns the closest reserved verb to head, or "" if none is
// close. A cheap edit-distance-1 check covers the common typo cases.
func didYouMean(head string) string {
	for _, v := range reservedVerbs {
		if editDistanceLE1(head, v) {
			return v
		}
	}
	return ""
}

func editDistanceLE1(a, b string) bool {
	if a == b {
		return true
	}
	la, lb := len(a), len(b)
	if la > lb {
		a, b = b, a
		la, lb = lb, la
	}
	if lb-la > 1 {
		return false
	}
	// At most one insertion/substitution.
	i, j, diffs := 0, 0, 0
	for i < la && j < lb {
		if a[i] == b[j] {
			i++
			j++
			continue
		}
		diffs++
		if diffs > 1 {
			return false
		}
		if la == lb {
			i++
			j++
		} else {
			j++
		}
	}
	return true
}
