package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

// mode is the top-level action gmux is being asked to perform.
type mode int

const (
	modeHelp     mode = iota // print usage and exit
	modeVersion              // print version and exit
	modeOpen                 // open the web UI
	modeRun                  // run a command in a new session (gmux -- <cmd>)
	modeList                 // gmux ls
	modeAttach               // gmux attach <id>
	modeTail                 // gmux tail <id>
	modeKill                 // gmux kill <id>
	modeSend                 // gmux send <id> <text> [keys...]
	modeSendKeys             // gmux send-keys -t <id> ... (tmux-compat)
	modeWait                 // gmux wait <id>
	modeDaemon               // gmux daemon <start|stop|restart|status|log-path>
	modeAuth                 // gmux auth
	modeRemote               // gmux remote
	modeDumpEnv              // (internal) gmux __dump-env
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
	sendText *string  // literal text to type (nil = none)
	sendKeys []string // trailing key-name tokens (Enter, C-c, ...)

	// send-keys (tmux-compat)
	keysLiteral bool     // -l: treat args as literal text, not key names
	keys        []string // key/text arguments

	// wait
	forText  string // --for-text: fixed substring
	forRegex string // --for-regex: regular expression
	timeout  int    // --timeout seconds (0 = none)

	// daemon
	daemonSub string // start|stop|restart|status|log-path
}

// reservedVerbs is the closed top-level namespace (ADR 0009). Growth
// happens under namespace groups, not new top-level verbs. Used to give
// "did you mean?" hints and to distinguish a removed flag from a stray
// command in the error-only migration shim.
var reservedVerbs = []string{
	"open", "ls", "attach", "tail", "kill", "send", "send-keys",
	"wait", "daemon", "auth", "remote", "version", "help",
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

// parseSend handles `gmux send <id> [text] [Key...]`. The first
// positional is the session ref; an optional second positional is the
// literal text; any further bare tokens are key-name tokens. With no
// text and no keys, stdin supplies the text.
func parseSend(args []string) (*command, error) {
	if len(args) < 1 {
		return nil, errors.New("send requires a session id")
	}
	c := &command{mode: modeSend, ref: args[0]}
	rest := args[1:]
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
	fs.StringVar(&c.forText, "for-text", "", "wait until this fixed substring appears")
	fs.StringVar(&c.forRegex, "for-regex", "", "wait until this regex matches")
	fs.IntVar(&c.timeout, "timeout", 0, "fail after N seconds")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return nil, err
	}
	if len(pos) != 1 {
		return nil, errors.New("wait requires a session id")
	}
	if c.forText != "" && c.forRegex != "" {
		return nil, errors.New("--for-text and --for-regex are mutually exclusive")
	}
	if c.timeout < 0 {
		return nil, errors.New("--timeout must be a non-negative number of seconds")
	}
	c.ref = pos[0]
	return c, nil
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
  gmux send-keys -t <id> <keys...>  tmux-compatible key sending
  gmux wait <id> [--for-text S | --for-regex P] [--timeout N]
  gmux kill <id>                    terminate a session

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
