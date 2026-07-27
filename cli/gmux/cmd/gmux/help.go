package main

import (
	"fmt"
	"io"
)

// Help lives in three layers: the top-level synopsis (printUsage) surfaces
// what a first-time caller most likely wants — running commands and driving
// agents — and buries management detail behind per-command pages
// (printVerbUsage, printAgentUsage). Every page spells flags in their
// canonical long form, or as --long/-short when a short alias exists.
// `help`, `--help`, `-h` and `?` are interchangeable everywhere a help
// request is valid.

// printUsage writes the gmux usage synopsis.
func printUsage(w io.Writer) {
	fmt.Fprint(w, `gmux: wrap any command in a managed session you watch in a browser

Run a command:
  gmux -- <cmd> [args]              run a command in a new session
  gmux -d -- <cmd> [args]           ... detached; prints the session id

Drive an agent (semantic turn control):
  gmux agent prompt <id> <prompt>   prompt an agent, wait, print its answer
  gmux agent --help                 all agent commands and options

Sessions (local by default; address a peer with <id>@<peer>):
  gmux ls [--all|-a] [--json|-j]    list sessions
  gmux attach <id>                  reattach your terminal to a session
  gmux tail <id> [-n N] [--raw|-r]  print conversation or recent output
  gmux send <id> <text> [Key...]    type text and keys into the terminal (raw)
  gmux wait <id> [--timeout|-t N]   block until the turn ends; print the result
  gmux kill <id>                    terminate a session

Editing (usable as $EDITOR; blocks until the editor closes):
  gmux edit [file]                  open a file for the user to inspect or edit

Other:
  gmux open                         open the web UI
  gmux remote                       manage remote access (Tailscale)
  gmux daemon <command>             manage the background daemon
  gmux version                      print the gmux version

'gmux <command> --help' explains each command. Full docs: https://gmux.app
`)
}

// keyVocabulary is the key-name reference shared by the send and
// send-keys pages: whoever lands on either page gets the full vocabulary
// without being sent to a second help command.
const keyVocabulary = `Key names (tmux vocabulary):
  Enter  Tab  BTab/S-Tab  Space  Escape/Esc  BSpace/Backspace
  Up  Down  Left  Right  Home  End
  PageUp/PPage  PageDown/NPage  Insert/IC  Delete/DC  F1 ... F12
  C-<letter>     control chord: C-a ... C-z (case-insensitive)
  C-Space  C-@   NUL
  C-[  C-\  C-]  C-^  C-_  C-?
                 the remaining control bytes (ESC, SIGQUIT, GS, RS, US, DEL).
                 Those are the whole control set: no control byte exists for
                 a digit or for other punctuation, so C-1 and C-, are not
                 keys — send them as text.
  M-<char>       alt/meta chord, ESC + any single character: M-x, M-b, M-. ...
  C-M-<same>     both: ESC + the control byte, for exactly the C- forms above

Modifiers C-, M-, S- combine in any order on the keys that have a standard
modified encoding — the arrows, Home, End, PageUp/PageDown, Insert, Delete
and F1-F12:
  C-Left  S-Up  M-PageUp  C-S-Home  C-M-End  M-F5  C-F12 ...

Not supported, because no single encoding exists for them (they depend on
the terminal and on keyboard-protocol negotiation): C-Tab, M-Enter,
C-Enter, M-Escape, S-Space, F13 and up, the keypad. Shift on a plain
character is just the upper-case character (A, not S-a). gmux refuses
these rather than guessing bytes — see how each command treats an
unrecognized name.
`

// verbHelpPages maps a top-level verb to its dedicated help page. Verbs
// without a page (open, remote, auth, version) are self-describing
// one-liners in the synopsis. The agent namespace has its own pages in
// printAgentUsage, and daemon help is served by the gmuxd binary itself.
var verbHelpPages = map[string]string{
	"ls": `gmux ls: list sessions, alive first, newest first

  gmux ls [--all|-a] [--json|-j]

  --all/-a   include sessions from every connected peer
             (peer sessions print as <id>@<peer>)
  --json/-j  emit a JSON array instead of the table, for scripts and
             agents; includes the exit_code of dead sessions

IDs in the first column are the 8-character short form every other command
accepts (also: a unique id prefix, the full id, or the session's slug).
`,

	"attach": `gmux attach: reattach your terminal to a session

  gmux attach <id>

Replays scrollback, forwards resize, and detaches (without killing the
session) when your terminal closes. Requires an interactive terminal.
Peer sessions (<id>@<peer>) attach transparently through the daemon.
`,

	"tail": `gmux tail: print a snapshot of a session's conversation or output

  gmux tail <id> [-n N] [--raw|-r]

  -n N         how much to print (default 100)
  --raw/-r     force the terminal-output view (-e is a tmux-compat alias)

For agent sessions that persist a conversation (pi), prints the
conversation as markdown: '## User' / '## Assistant' messages with compact
[tool] one-liners; -n counts messages. For everything else (and with
--raw), prints the last N lines of rendered terminal output, ANSI
stripped.

To read just an agent's latest answer, prefer 'gmux agent output <id>'.
`,

	"send": `gmux send: type raw text and keys into a session's terminal

  gmux send [--wait|-w] [--timeout|-t N] <id> [text] [Key...]

send is raw: it types exactly the bytes you name, nothing more. Enter is
never implied — append it to submit. For agent sessions prefer
'gmux agent prompt', which delivers semantically and reports the outcome.

  gmux send a3f2 'make test' Enter     type a command and run it
  gmux send a3f2 'partial input'       type without submitting
  gmux send a3f2 C-c                   interrupt (Ctrl-C)
  echo "$text" | gmux send a3f2 Enter  text from stdin (up to 1 MiB)

Flags go before the id; everything after the id is verbatim, so
dash-leading text needs no guard. The first token after the id is the
literal text (unless it is a key name); every further token must be a key
— an unrecognized name there is an error, not text: 'gmux send a3f2 hi Etner'
fails instead of typing 'Etner'. (send-keys differs: for tmux
compatibility it types unknown tokens literally, and -l forces that.)

  --wait/-w      block until the turn this input triggers ends; requires
                 the input to submit (trailing Enter, or \r in stdin)
  --timeout/-t N with --wait: give up after N seconds
                 (-t is --timeout here; send's target is positional. The
                 tmux-style '-t <id>' target lives on send-keys.)

` + keyVocabulary + `
Exit codes: 0 delivered (with --wait: the turn completed), 2 with --wait
when the turn was interrupted, 1 anything else.

tmux compatibility: 'gmux send-keys -t <id> [-l] <keys...>' is accepted
verbatim ('gmux send-keys --help').
`,

	"send-keys": `gmux send-keys: tmux-compatible key sending

  gmux send-keys -t <id> [-l] <keys...>

  -t <id>    target session (tmux's target flag; on 'gmux send', -t is
             --timeout and the id is positional)
  -l         treat every argument as literal text, not key names

Provided for tmux muscle memory and script compatibility; the native form
is 'gmux send'. Like tmux — and unlike 'gmux send' — an argument that is
not a recognized key name is typed as literal text rather than refused.

` + keyVocabulary,

	"wait": `gmux wait: block until a session's turn ends, or until output appears

  gmux wait <id> [--timeout|-t N] [--quiet|-q]
  gmux wait <id> --for-text <substring> [--timeout|-t N]
  gmux wait <id> --for-regex <pattern> [--timeout|-t N]

Blocks until the session goes idle: an agent finishing its turn, a shell
back at its prompt (OSC 133 marks), or a one-shot command exiting. For
agent sessions whose turn completed, prints the agent's latest final message
on stdout — the same text 'gmux agent output' returns. --quiet suppresses
it. A failed, interrupted or dead turn prints nothing (richer failure
detail is planned).

A wait on an already-idle session returns at once and reports the LAST
turn's conclusion; to gate on a turn you are about to trigger, use
'gmux agent prompt' or 'gmux send --wait', which arm the wait first.

  --timeout/-t N give up after N seconds (exit 1)
  --quiet/-q     synchronize only; print no result
  --for-text S   resolve when S appears in the output instead of on idle
  --for-regex P  ... or when P (RE2, line-wise) matches; works for shell
                 sessions too, and prints no result

Exit codes: 0 the turn completed (or the output matched), 2 the turn was
intentionally interrupted, 1 anything else (error, death, timeout).
`,

	"kill": `gmux kill: terminate a session

  gmux kill <id>

Sends SIGTERM to the session's process. The session stays listed
('gmux ls') with its exit code, and its output remains readable.
`,

	"edit": `gmux edit: open a file in a managed editor session

  gmux edit [file]

Usable as $EDITOR: blocks until the editor closes and propagates its exit
code. With no file, prompts for a path.
`,
}

// printVerbUsage writes the dedicated help page for a verb; callers
// guarantee the page exists (helpTopicExists).
func printVerbUsage(w io.Writer, verb string) {
	fmt.Fprint(w, verbHelpPages[verb])
}

// helpTopicExists reports whether topic names a dedicated help page.
func helpTopicExists(topic string) bool {
	_, ok := verbHelpPages[topic]
	return ok
}

// isHelpToken reports whether s is one of the interchangeable help
// spellings accepted wherever a help request is valid.
func isHelpToken(s string) bool {
	return s == "help" || s == "-h" || s == "--help" || s == "?"
}

// usageError carries the help topic whose page should follow the error
// message, so a mistake inside a verb prints that verb's help instead of
// the full synopsis.
type usageError struct {
	topic string // "" = top-level synopsis; "agent..." = agent pages
	err   error
}

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

// printHelpTopic routes a help topic to the page that owns it.
func printHelpTopic(w io.Writer, topic string) {
	switch {
	case topic == "":
		printUsage(w)
	case topic == "agent" || len(topic) > 6 && topic[:6] == "agent ":
		printAgentUsage(w, topic)
	case helpTopicExists(topic):
		printVerbUsage(w, topic)
	default:
		printUsage(w)
	}
}

// helpHint names the help invocation that explains a failed command.
// Errors print this one-liner instead of a full help page, so repeated
// mistakes don't fill the terminal (or an agent's context) with usage
// text nobody asked for.
func helpHint(topic string) string {
	switch {
	case topic == "":
		return "run 'gmux --help' for usage"
	case topic == "agent" || len(topic) > 6 && topic[:6] == "agent ":
		return "run 'gmux agent --help' for usage"
	default:
		return fmt.Sprintf("run 'gmux %s --help' for usage", topic)
	}
}
