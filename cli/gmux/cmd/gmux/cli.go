package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
)

// mode is the top-level action gmux is being asked to perform.
type mode int

const (
	modeUI     mode = iota // no args → open the web UI
	modeRun                // run a command in a new session
	modeList               // list known sessions
	modeAttach             // reattach to an existing session
	modeTail               // dump recent output from a session
	modeKill               // terminate a session
	modeSend               // inject input into a running session
	modeWait               // block until session reaches idle / dies
	modeHelp               // print usage and exit
)

// flags captures the parsed gmux-level options. Anything that influences
// the run path ("flags the runner cares about") or triggers a management
// action ("flags that replace the runner") lives here. The trailing
// positional command or session id is returned separately as rest.
type flags struct {
	noAttach    bool
	list        bool
	attach      bool
	kill        bool
	send        bool
	noSubmit    bool // suppresses the trailing carriage return on --send
	wait        bool
	waitTimeout int // 0 means no timeout
	tail        int // >=0 when set (flag default is -1)
	help        bool
}

// parseCLI parses argv (without program name) and decides which mode to
// dispatch. Flag parsing stops at the first positional argument, matching
// the POSIX runner convention (env/nohup/time/screen): everything from
// the first bare word onwards is the command, verbatim, including its
// own flags.
//
// A literal `--` also terminates flag parsing, for the rare case where
// the command itself starts with a dash.
func parseCLI(args []string) (mode, *flags, []string, error) {
	f := &flags{tail: -1}
	fs := flag.NewFlagSet("gmux", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // we print our own usage on error

	fs.BoolVar(&f.noAttach, "no-attach", false, "run the command detached from the terminal")
	fs.BoolVar(&f.list, "list", false, "list known sessions")
	fs.BoolVar(&f.list, "l", false, "list known sessions (short)")
	fs.BoolVar(&f.attach, "attach", false, "reattach to an existing session")
	fs.BoolVar(&f.attach, "a", false, "reattach to an existing session (short)")
	fs.BoolVar(&f.kill, "kill", false, "kill a running session")
	fs.BoolVar(&f.kill, "k", false, "kill a running session (short)")
	fs.BoolVar(&f.send, "send", false, "send input to a running session")
	fs.BoolVar(&f.noSubmit, "no-submit", false, "with --send, do not append the carriage return that submits the input")
	fs.BoolVar(&f.wait, "wait", false, "block until a session is idle (agent finished its turn)")
	fs.IntVar(&f.waitTimeout, "timeout", 0, "with --wait, fail after N seconds (default: no timeout)")
	fs.IntVar(&f.tail, "tail", -1, "dump the last N lines of a session")
	fs.IntVar(&f.tail, "t", -1, "dump the last N lines of a session (short)")
	fs.BoolVar(&f.help, "help", false, "show help")
	fs.BoolVar(&f.help, "h", false, "show help (short)")

	if err := fs.Parse(args); err != nil {
		return modeHelp, nil, nil, err
	}
	rest := fs.Args()

	if f.help {
		return modeHelp, f, rest, nil
	}

	// In management modes there is no wrapped command, only a bounded
	// number of positionals (id, optional text for --send). The POSIX
	// runner stop-at-first-positional rule that protects `gmux <cmd>
	// --cmd-flag` from having gmux eat --cmd-flag does nothing useful
	// here — it just turns `gmux --wait <id> --timeout 60` into a
	// silent foot-trap where --timeout becomes a positional. Re-parse
	// any flags interleaved with positionals so flag order doesn't
	// matter for management actions.
	if isManagementMode(f) {
		rest = parseInterspersedFlags(fs, rest)
	}

	// At most one management action at a time.
	actions := 0
	if f.list {
		actions++
	}
	if f.attach {
		actions++
	}
	if f.kill {
		actions++
	}
	if f.send {
		actions++
	}
	if f.wait {
		actions++
	}
	if f.tail >= 0 {
		actions++
	}
	if actions > 1 {
		return modeHelp, nil, nil, errors.New("--list, --attach, --tail, --kill, --send, --wait are mutually exclusive")
	}

	// --no-submit only changes the bytes --send writes; with anything
	// else it would silently do nothing, so reject it loudly.
	if f.noSubmit && !f.send {
		return modeHelp, nil, nil, errors.New("--no-submit only applies with --send")
	}
	// --timeout is meaningless without --wait. (Once we add other
	// time-bounded actions it can grow into a shared option.)
	if f.waitTimeout != 0 && !f.wait {
		return modeHelp, nil, nil, errors.New("--timeout only applies with --wait")
	}

	// Management actions take a single session id (except --list and --send).
	switch {
	case f.list:
		if len(rest) > 0 {
			return modeHelp, nil, nil, errors.New("--list takes no arguments")
		}
		if f.noAttach {
			return modeHelp, nil, nil, errors.New("--no-attach has no effect with --list")
		}
		return modeList, f, nil, nil
	case f.attach:
		if len(rest) != 1 {
			return modeHelp, nil, nil, errors.New("--attach requires a session id")
		}
		if f.noAttach {
			return modeHelp, nil, nil, errors.New("--no-attach conflicts with --attach")
		}
		return modeAttach, f, rest, nil
	case f.kill:
		if len(rest) != 1 {
			return modeHelp, nil, nil, errors.New("--kill requires a session id")
		}
		if f.noAttach {
			return modeHelp, nil, nil, errors.New("--no-attach has no effect with --kill")
		}
		return modeKill, f, rest, nil
	case f.send:
		// --send takes a session id and either an inline text arg or
		// stdin (when no text is given).
		if len(rest) < 1 || len(rest) > 2 {
			return modeHelp, nil, nil, errors.New("--send takes a session id and optional text (stdin is used if no text is given)")
		}
		if f.noAttach {
			return modeHelp, nil, nil, errors.New("--no-attach has no effect with --send")
		}
		return modeSend, f, rest, nil
	case f.wait:
		if len(rest) != 1 {
			return modeHelp, nil, nil, errors.New("--wait requires a session id")
		}
		if f.noAttach {
			return modeHelp, nil, nil, errors.New("--no-attach has no effect with --wait")
		}
		if f.waitTimeout < 0 {
			return modeHelp, nil, nil, errors.New("--timeout must be a non-negative number of seconds")
		}
		return modeWait, f, rest, nil
	case f.tail >= 0:
		if len(rest) != 1 {
			return modeHelp, nil, nil, errors.New("--tail requires a session id")
		}
		if f.tail == 0 {
			return modeHelp, nil, nil, errors.New("--tail needs a positive line count")
		}
		if f.noAttach {
			return modeHelp, nil, nil, errors.New("--no-attach has no effect with --tail")
		}
		return modeTail, f, rest, nil
	}

	// No management action. If there's a command, run it; otherwise UI.
	if len(rest) == 0 {
		if f.noAttach {
			return modeHelp, nil, nil, errors.New("--no-attach requires a command")
		}
		return modeUI, f, nil, nil
	}
	return modeRun, f, rest, nil
}

// isManagementMode reports whether the parsed flags request a
// management action (no wrapped command). Run mode is everything
// else — a command with optional --no-attach.
func isManagementMode(f *flags) bool {
	return f.list || f.attach || f.kill || f.send || f.wait || f.tail >= 0
}

// parseInterspersedFlags walks `rest` consuming any further flags via
// fs.Parse and collecting non-flag tokens as positionals. The default
// flag.FlagSet behavior stops at the first positional; iterating lets
// us pick up flags that appear after positionals too. Used only in
// management modes, where positionals are bounded and there is no
// risk of swallowing flags meant for a wrapped child command.
func parseInterspersedFlags(fs *flag.FlagSet, rest []string) []string {
	var positionals []string
	remaining := rest
	for len(remaining) > 0 {
		if err := fs.Parse(remaining); err != nil {
			// fs.Parse already wrote into the same flags struct on the
			// first call; any error here is from re-parsing an unknown
			// flag after a positional. Surface the leftover args as-is
			// so the caller's validation produces a sensible message.
			return append(positionals, remaining...)
		}
		newRest := fs.Args()
		if len(newRest) == 0 {
			break
		}
		if len(newRest) == len(remaining) {
			// fs.Parse stopped without consuming anything: first token
			// is a positional. Take it and resume on the suffix.
			positionals = append(positionals, newRest[0])
			remaining = newRest[1:]
			continue
		}
		// fs.Parse consumed at least one flag; loop on the new tail.
		remaining = newRest
	}
	return positionals
}

// printUsage writes the gmux usage synopsis. Shown on --help, on parse
// errors (with an error message prefix), and nowhere else.
func printUsage(w io.Writer) {
	fmt.Fprint(w, `gmux — wrap any command in a managed session

Usage:
  gmux                              open the web UI
  gmux [--no-attach] <cmd> [args]   run a command in a new session
  gmux -- <cmd> [args]              use -- if <cmd> starts with a dash

Session management:
  gmux --list                       list known sessions
  gmux --attach <id>                reattach to an existing session
  gmux --tail <N> <id>              print the last N lines of a session
  gmux --kill <id>                  terminate a session
  gmux --send <id> [text]           send text (or stdin) to a session and submit it
  gmux --send --no-submit <id> ...  send without the trailing carriage return
  gmux --wait <id>                  block until session is idle (agent finished its turn)
  gmux --wait --timeout N <id>      ... or fail after N seconds

Flags before the command apply to gmux itself. Once the first positional
argument is seen, everything after is the command to run, verbatim.

For daemon management see 'gmuxd --help'.
`)
}
