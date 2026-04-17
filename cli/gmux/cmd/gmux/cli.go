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
	modeHelp               // print usage and exit
)

// flags captures the parsed gmux-level options. Anything that influences
// the run path ("flags the runner cares about") or triggers a management
// action ("flags that replace the runner") lives here. The trailing
// positional command or session id is returned separately as rest.
type flags struct {
	noAttach bool
	list     bool
	attach   bool
	kill     bool
	tail     int // >=0 when set (flag default is -1)
	help     bool
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
	if f.tail >= 0 {
		actions++
	}
	if actions > 1 {
		return modeHelp, nil, nil, errors.New("--list, --attach, --tail, --kill are mutually exclusive")
	}

	// Management actions take a single session id (except --list).
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

Flags before the command apply to gmux itself. Once the first positional
argument is seen, everything after is the command to run, verbatim.

For daemon management see 'gmuxd --help'.
`)
}
