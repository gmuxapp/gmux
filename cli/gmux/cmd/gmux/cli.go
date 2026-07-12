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
	modeWait                   // gmux wait <id>
	modeEdit                   // gmux edit [file]
	modeEditChild              // (internal) gmux __edit-child [file]
	modeDaemon                 // gmux daemon <start|stop|restart|status|log-path>
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
	ref string // session reference; may carry an @peer suffix

	// ls
	all  bool
	json bool

	// tail
	tailLines int
	raw       bool

	// send
	sendText   *string  // literal text to type (nil = none)
	sendKeys   []string // trailing key-name tokens (Enter, C-c, ...)
	sendWait   bool     // --wait: block until the triggered turn completes
	sendSubmit string   // --follow-up/--steering: auto-append the adapter's submit key ("" = neither)

	// send-keys (tmux-compat)
	keysLiteral bool     // -l: treat args as literal text, not key names
	keys        []string // key/text arguments

	// wait
	timeout  int    // --timeout seconds (0 = none)
	forText  string // --for-text: wait for substring in output
	forRegex string // --for-regex: wait for regex match in output

	// edit
	editFile string // file path to open

	// daemon
	daemonSub string // start|stop|restart|status|log-path

	// codex hook (internal __codex-hook)
	codexHookEvent string // the codex hook event name (SessionStart, ...)
}

// reservedVerbs is the closed top-level namespace (ADR 0009). Growth
// happens under namespace groups, not new top-level verbs. Used to give
// "did you mean?" hints and to distinguish a removed flag from a stray
// command in the error-only migration shim.
var reservedVerbs = []string{
	"open", "ls", "attach", "tail", "kill", "send", "send-keys",
	"wait", "edit", "daemon", "auth", "remote", "version", "help",
}

// removedFlags maps every pre-2.0 action flag to the verb that replaced
// it. The migration shim (ADR 0009) recognizes these solely to print a
// precise error; no old behavior is carried.
var removedFlags = map[string]string{
	"--list": "gmux ls", "-l": "gmux ls",
	"--attach": "gmux attach <id>", "-a": "gmux attach <id>",
	"--tail": "gmux tail <id>", "-t": "gmux tail <id>",
	"--kill": "gmux kill <id>", "-k": "gmux kill <id>",
	"--send":      "gmux send <id> <text>",
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
	case "help", "-h", "--help":
		// Lenient: `gmux help` and `gmux help <anything>` both print the
		// full usage. Per-verb help is intentionally not implemented (see
		// ADR 0009); accepting a trailing word avoids an error on the
		// natural `gmux help send`.
		return &command{mode: modeHelp}, nil
	case "version", "--version":
		return &command{mode: modeVersion}, nil
	case "open":
		if len(rest) > 0 {
			return nil, errors.New("open takes no arguments")
		}
		return &command{mode: modeOpen}, nil
	case "ls":
		return parseLs(rest)
	case "attach":
		return parseRefOnly(modeAttach, "attach", rest)
	case "kill":
		return parseRefOnly(modeKill, "kill", rest)
	case "tail":
		return parseTail(rest)
	case "send":
		return parseSend(rest)
	case "send-keys":
		return parseSendKeys(rest)
	case "wait":
		return parseWait(rest)
	case "edit":
		return parseEdit(rest)
	case "daemon":
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
	if v := didYouMean(head); v != "" {
		return nil, fmt.Errorf("unknown command %q; did you mean %q? (%s)", head, v, runHint)
	}
	return nil, fmt.Errorf("unknown command %q; %s", head, runHint)
}

func parseLs(args []string) (*command, error) {
	c := &command{mode: modeList}
	fs := newFlagSet("ls")
	fs.BoolVar(&c.all, "all", false, "include sessions from all peers")
	fs.BoolVar(&c.json, "json", false, "emit a JSON array")
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

func parseTail(args []string) (*command, error) {
	c := &command{mode: modeTail, tailLines: 100}
	fs := newFlagSet("tail")
	fs.IntVar(&c.tailLines, "n", 100, "number of lines to show")
	fs.BoolVar(&c.raw, "raw", false, "preserve ANSI escapes")
	fs.BoolVar(&c.raw, "e", false, "preserve ANSI escapes (short)")
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

// parseSend handles `gmux send [--wait] [--timeout N]
// [--follow-up|--steering] [--] <id> [text] [Key...]`. The first
// positional is the session ref; an optional second positional is the
// literal text; any further bare tokens are key-name tokens. With no
// text and no keys, stdin supplies the text.
//
// --follow-up and --steering auto-append the session adapter's submit
// keystroke (pi: Alt+Enter vs Enter), so they are mutually exclusive
// with each other AND with trailing key tokens: the flag owns
// submission, and `--follow-up ... Enter` would double-submit.
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
		case a == "--wait":
			c.sendWait = true
		case a == "--follow-up" || a == "--steering":
			mode := strings.TrimPrefix(a, "--")
			if c.sendSubmit != "" && c.sendSubmit != mode {
				return nil, errors.New("send: --follow-up and --steering are mutually exclusive")
			}
			c.sendSubmit = mode
		case a == "--timeout" || strings.HasPrefix(a, "--timeout="):
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
			if err != nil || n <= 0 {
				return nil, errors.New("--timeout must be a positive number of seconds")
			}
			c.timeout = n
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
		c.sendKeys = rest
	}
	if c.sendSubmit != "" && len(c.sendKeys) > 0 {
		return nil, fmt.Errorf("send: --%s appends the adapter's submit keystroke itself; pass the message as a single argument with no trailing key tokens", c.sendSubmit)
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
	fs.StringVar(&c.forText, "for-text", "", "wait for this substring in the session's output")
	fs.StringVar(&c.forRegex, "for-regex", "", "wait for a regex match in the session's output")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return nil, err
	}
	if len(pos) != 1 {
		return nil, errors.New("wait requires a session id")
	}
	if c.timeout < 0 {
		return nil, errors.New("--timeout must be a non-negative number of seconds")
	}
	if c.forText != "" && c.forRegex != "" {
		return nil, errors.New("--for-text and --for-regex are mutually exclusive")
	}
	if c.forRegex != "" {
		// Validate here so a typo fails as a usage error instead of a
		// daemon round-trip; the daemon validates again server-side.
		if _, err := regexp.Compile(c.forRegex); err != nil {
			return nil, fmt.Errorf("--for-regex: %v", err)
		}
	}
	c.ref = pos[0]
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

func parseDaemon(args []string) (*command, error) {
	if len(args) != 1 || !daemonSubs[args[0]] {
		return nil, errors.New("daemon requires one of: start, stop, restart, status, log-path")
	}
	return &command{mode: modeDaemon, daemonSub: args[0]}, nil
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

// printUsage writes the gmux usage synopsis.
func printUsage(w io.Writer) {
	fmt.Fprint(w, `gmux — wrap any command in a managed session you watch in a browser

Run a command:
  gmux -- <cmd> [args]              run a command in a new session
  gmux -d -- <cmd> [args]           ... detached; prints the session id
  (tip: alias gm='gmux --')

Sessions (local by default; address a peer with <id>@<peer>):
  gmux ls [--all] [--json]          list sessions
  gmux attach <id>                  reattach to a session
  gmux tail <id> [-n N] [--raw]     print recent output (snapshot)
  gmux send <id> <text> [Key...]    type text and/or send keys (e.g. Enter, C-c)
  gmux send --wait [--timeout N] <id> <text> [Key...]
                                    ... and block until the triggered turn ends
  gmux send --follow-up|--steering <id> <text>
                                    ... auto-append the adapter's submit key
                                    (--follow-up queues after the current turn;
                                     --steering interjects now; composes with --wait)
  gmux send-keys -t <id> <keys...>  tmux-compatible key sending
  gmux wait <id> [--timeout N]      block until the session is idle
       [--for-text S|--for-regex P] ... or until output matches S / P
  gmux kill <id>                    terminate a session

Editing (usable as $EDITOR; blocks until the editor closes):
  gmux edit [file]                  open a file in a managed editor session
                                    (no file: prompts for a path)

UI & pairing:
  gmux open                         open the web UI
  gmux remote                       set up / check remote access
  gmux auth                         reveal this host's login token

Daemon:
  gmux daemon start|stop|restart|status|log-path

  gmux version · gmux help [verb]

For the daemon process itself, see 'gmuxd --help'.
`)
}
