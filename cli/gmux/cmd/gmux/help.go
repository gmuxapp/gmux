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
  gmux agent prompt <id> <prompt>   prompt an agent, wait, print its exchange report
  gmux agent logs <id> [-n N]       read visible conversation exchanges
  gmux agent --help                 all agent options

Sessions (local by default; address a peer with <id>@<peer>):
  gmux ls [--all|-a] [--json|-j]    list sessions
  gmux attach <id>                  reattach your terminal to a session
  gmux tail <id> [-n N]             print the last N lines of terminal output
  gmux send <id> <text> [Key...]    type text and keys into the terminal (raw)
  gmux wait <id>... [--timeout|-t N] block until turns end; print reports
  gmux read <id>...                  mark retained results consumed
  gmux kill <id>                    terminate a session
  gmux session --help               promote, demote, or reparent sessions

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
	"run": `gmux run: run a command in a managed session

  gmux -- <cmd> [args]
  gmux -d -- <cmd> [args]

A session receives input only through an interactive attach, 'gmux send', or
'gmux agent prompt'. The launching process's stdin is never forwarded. If stdin
has pending data, gmux refuses before launching a session; redirect </dev/null
to discard it explicitly.

The child runs on a terminal, so its stdout and stderr are one stream, as in
'ssh -t' or 'script'. That payload is written to stdout; gmux's own stderr
carries the session id and diagnostics.

To launch and then send input from a script:

  id=$(gmux -d -- cmd); gmux send "$id" 'input' Enter
`,

	"ls": `gmux ls: list sessions, alive first, newest first

  gmux ls [--all|-a] [--json|-j]

  --all/-a   include sessions from every connected peer
             (peer sessions print as <id>@<peer>)
  --json/-j  emit a JSON array instead of the table, for scripts and
             agents; includes the exit_code of dead sessions

IDs in the first column are the full 8-character form every other command
accepts (also: a unique ID prefix or the session's slug).
`,

	"attach": `gmux attach: reattach your terminal to a session

  gmux attach <id>

Replays scrollback, forwards resize, and detaches (without killing the
session) when your terminal closes. Requires an interactive terminal.
Peer sessions (<id>@<peer>) attach transparently through the daemon.
`,

	"tail": `gmux tail: print a snapshot of a session's terminal output

  gmux tail <id> [-n N]

  -n N         how many lines to print (default 100)

Always the raw view: the last N lines of rendered terminal output, ANSI
stripped, for every kind of session — shell, one-shot command or agent.
tail answers "what is on its screen".

For an agent session you usually want a semantic view instead:

  gmux agent logs <id> [-n N]    visible conversation exchanges (-n counts
                                 exchanges and defaults to one)

('gmux tail --raw' and its -e/-r aliases are gone: tail is raw by
definition, and the conversation view moved to 'gmux agent logs'.)
`,

	"read": `gmux read: mark session results consumed

  gmux read <id>...
  gmux read --family <root-id>

The family form marks the root and all transitive launch descendants read. Use
it once to clear retained child-session unread piles created by older gmux
versions whose wait command did not consume results.
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

	"wait": `gmux wait: observe activity until it settles

  gmux wait <id>... [--timeout|-t N] [--quiet|-q]
  gmux wait <id> --for-text <substring> [--timeout|-t N]
  gmux wait <id> --for-regex <pattern> [--timeout|-t N]

  --timeout/-t N  stop waiting after N seconds
  --quiet/-q      suppress the report; return only the verdict

Waits for multiple ids run concurrently under one timeout and print reports in
argument order, each headed by '=== <full-session-id> ==='. A single id keeps
its headerless output. Duplicate resolved ids are refused. --for-text and
--for-regex accept one id only.

For renderer-capable agents, prints an exchange-structured report on stdout.
Steers, follow-ups, and human instructions are additional user boundaries and
do not end the wait. A late wait immediately reports the latest exchange.
Timeout, interruption, and activity failure are valid stdout reports; the exit
code is the verdict. --quiet suppresses reports. Non-agent sessions use neutral
session-activity markers; predicate waits remain synchronization-only.

Exit codes: 0 completed/matched, 2 intentionally interrupted, 1 failure or
timeout. A local signal says the observed agent or sessions remain active and exits 128+N.
`,

	"kill": `gmux kill: terminate a session

  gmux kill <id>

Sends SIGHUP to the session's child process group, waits up to two seconds,
then escalates to SIGKILL. The session stays listed
('gmux ls') with its exit code, and its output remains readable.
`,

	"session": `gmux session: change task-family ownership

  gmux session promote <id>                present a child as a root
  gmux session demote <id>                 restore normal child presentation
  gmux session reparent <id> <parent-id>   assign a new direct parent
  gmux session reparent <id> --clear       clear the direct parent

Promotion is sticky presentation state: it preserves the organizational
parent while giving the session full root visibility and its own
active-subagent budget. Completion-notification suppression is unchanged
either way: it follows the immutable launch parent, not presentation.
Demotion clears that presentation override.

Reparenting changes the parent used by family grouping, recursive dismissal,
and the active-subagent budget. Both sessions must exist on this daemon;
self-parenting, cycles, and cross-peer reassignment are refused.
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
