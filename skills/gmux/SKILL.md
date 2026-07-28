---
name: gmux
description: Drive long-running terminal commands and AI coding agents through gmux sessions. Use when the user asks to run a command in the background, send input to a running session, wait for an agent's turn to finish, orchestrate multiple agents in parallel, or capture output from a tmux/screen-style session.
---

# gmux

A command run through gmux becomes a managed session the user can watch live
in a browser. The grammar is verb-first; **running a command always uses the
explicit `--` separator** so gmux never guesses where its own flags end and
the command begins.

## Primitives

```bash
gmux -- <cmd> [args]         # run blocking; exits with the child's exit code
gmux -d -- <cmd> [args]      # run detached; prints the session id on stdout
gmux agent prompt <id> 'text'   # prompt an AGENT session; blocks until the turn ends
gmux agent prompt --new 'text'  # launch a new pi session AND prompt it (prints the id first)
gmux agent status <id>          # what matters now: state, trigger, answer
gmux agent cancel <id>          # interrupt the running turn
gmux send <id> 'text' Enter  # RAW: type text and submit (Enter is explicit)
gmux send <id> C-c           # send a control key (interrupt), no text
gmux send --wait <id> 'text' Enter  # raw send AND block until the reply is done
gmux wait <id>               # block until the turn ends; prints an agent's answer
gmux wait --quiet <id>       # ...synchronize only, print nothing
gmux wait <id> --for-text S  # block until S appears in the output
gmux tail <id> [-n N]        # RAW: last N lines of terminal output (N=100)
gmux agent logs <id> [-n N] [--user|--agent|--tool|--all] [--json]
                             # an agent's conversation as markdown (N messages,
                             # counted after the type filter)
gmux agent logs --agent -n 1 <id>   # just the latest answer
gmux ls [--json]             # list sessions (--json for machine parsing)
gmux kill <id>               # SIGTERM the runner
```

**For agent sessions (pi), prompt with `gmux agent prompt`, not `gmux send`.**
`send` types raw keystrokes and cannot tell you whether the agent accepted
them; `agent prompt` waits until the agent can accept input, submits the way
that agent expects, and reports the turn's real outcome. Keep `send` for
shells, TUIs, and agents without semantic support (Claude Code, Codex — they
fail with `unsupported_adapter`).

`ls` IDs are 8-character prefixes; pass them directly to
`agent` / `send` / `wait` / `tail` / `kill`. Tip: `alias gm='gmux --'` makes
`gm pytest` shorthand for `gmux -- pytest`.

Because `gmux -- <cmd>` propagates the child's exit code, it composes:
`if gmux -- pytest -q; then ...`.

## Sending input and keys

`send` types literal text; any trailing token that is a key name is sent as a
key. **Enter is not implicit** — add it to submit:

```bash
gmux send $id 'pytest -q' Enter   # type and run
gmux send $id 'half a line'       # type without submitting
gmux send $id C-c                 # interrupt (Ctrl-C)
gmux send $id Escape              # send Escape
echo "$body" | gmux send $id Enter  # pipe stdin, then submit (Enter optional)
```

Key names follow tmux: `Enter`, `Tab`, `BTab`/`S-Tab`, `Escape`,
`Up`/`Down`/`Left`/`Right`, `Home`/`End`, `PageUp`/`PageDown`, `Insert`,
`Delete`, `F1`-`F12`, `C-c`, `M-x`, `C-M-a`, and modified special keys like
`C-Left` or `C-S-Home`. Run `gmux send --help` for the full list — including
the combinations gmux refuses (`C-Tab`, `M-Enter`, `F13`+ …) because no single
terminal encoding exists for them.

**Only the first token after the id is text; every later token must be a key.**
An unrecognized key name is an error, so a typo is never silently typed into
the session as prose. For verbatim tmux compatibility (unknown tokens typed
literally) there is `gmux send-keys -t <id> <keys...>`, with `-l` to force it.

**Everything after the id is verbatim** — including dash-leading tokens, so
`gmux send $id -v` sends a literal `-v` with no `--` guard needed. `send`'s own
flags (`--wait`, `--timeout`) only work *before* the id.

## Prompting an agent session

```bash
id=$(gmux -d -- pi)
gmux agent prompt --timeout 600 $id 'run the suite and fix what fails'
gmux agent status $id                     # state, trigger, and the answer
gmux agent logs --agent -n 1 $id           # the answer alone, no [tool] lines
git diff | gmux agent prompt $id           # prompt from stdin (stays one prompt)
```

### Launch and prompt in one command: `--new`

```bash
id=$(gmux agent prompt --new --no-wait --name review 'review the diff on this branch')
gmux wait $id && gmux agent status $id

gmux agent prompt --new --model anthropic/sonnet 'summarize this repo'
```

`--new` launches a new **pi** session (same machinery as `gmux -d -- pi`, from
your env and cwd) and sends the prompt as its first turn. Pass either an id or
`--new`, never both.

**The session id is stdout line 1**, printed the moment the session exists and
before the prompt is delivered — so you can always address a session you just
created, even if admission or the turn then fails. It means only "this session
exists and is addressable": not admitted, not ready, not delivered — the exit
code says those. A failure after the launch leaves the session behind and **you
own it** (it may still be running): retry against the id, or `gmux kill $id`. Under `--new` the completion
signal is therefore the **exit code**, not non-empty stdout: a sync run prints
the id and then the answer; `--no-wait` prints the bare id only and exits 0 once
the turn was **admitted** (the agent started it) — so on a sick session that
launch line can block up to the 60 s admission window rather than returning at
delivery. A launch that never registers prints nothing on stdout and exits 1.

The first turn of a brand-new session prints its answer like any other: the agent
asserts its turn's result at the boundary, so gmux never has to reconstruct it
from the conversation file.

`--new` must come **before** the prompt: after an id it is prompt text like
anything else (`gmux agent prompt $id --new` sends the literal `--new`).

`--model`/`--name` are valid only with `--new`; `--follow-up`/`--steer` are
refused with it (there is no turn yet); `--timeout` bounds your wait, not the
launch. `--new` is pi-only — for any other command the two-step
`id=$(gmux -d -- <cmd>)` then `gmux agent prompt $id …` is still fully valid.

`agent prompt` blocks until the turn ends and **prints the agent's answer** on
stdout (the turn's final message, no `[tool]` lines), as the agent asserted it
for that exact turn. A turn nobody could identify as yours prints nothing rather
than somebody else's answer, and a very long answer is capped at the agent with
a note on stderr — `gmux agent logs --agent -n 1 <id>` has the full text. Exit codes are
gmux's global taxonomy, identical for every verb that reports a gmux verdict:
`0` completed, `2` intentionally interrupted, `1` everything else (failed turn,
`--timeout`, dead runner, usage/transport error). The exceptions pass a code
through instead: `gmux -- <cmd>` and `gmux edit` return the child's, and
`gmux daemon|auth|remote` return gmuxd's. A turn that did not complete prints **no**
answer — a previous turn's reply must never be presented as this one's; read
what exists with `gmux agent status <id>` (works even after the session died).
Use `gmux agent logs <id>` when you want specific text: no flag gives the
conversation (user messages plus assistant prose), `--user`/`--agent`/`--tool`/
`--all` **replace** that set, `-n` counts messages after the filter, and
`--json` prints `{role, type, text, prose}` objects. Both reads are snapshots of
the stored conversation, so they can be staler than the answer a wait carries.

Other shapes, all with the flags **before** the id (text after the id is
verbatim):

```bash
gmux agent prompt --no-wait $id 'start the long refactor'   # return once admitted
gmux agent prompt --follow-up $id 'then update the docs'    # queue after this turn
gmux agent prompt --steer $id 'stop, use the sqlite path'   # redirect the live turn
gmux agent prompt --no-wait --steer $id 'and skip the migration'  # --no-wait composes
gmux agent cancel $id; gmux wait $id; [ $? = 2 ] && echo stopped   # interrupt, then wait
```

Do **not** chain cancel with `&& gmux wait`: an interrupted turn exits `2`, so the
wait "fails" on exactly the outcome you asked for (and aborts the script under
`set -e`).

`--follow-up` and `--steer` are mutually exclusive; `--no-wait` composes with
either (it only decides whether you block, so `--no-wait --timeout N` is a usage
error). What `--no-wait` returns at differs by mode: a plain prompt and a
`--follow-up` to an idle agent start a turn, so it returns once the agent has
actually begun it (exit 0 is a health event); `--steer` and a `--follow-up` that
merges into a running turn join a turn already under way, so there is nothing to
admit beyond delivery and it returns at delivery. A plain prompt restarts a dead retained session to deliver it; `--steer` and
`cancel` need a live, active turn and never resume. `cancel` returns when the
interrupt is *delivered*, so follow it with `gmux wait` if the next step needs
the turn actually stopped.

For pi, `cancel` also **restores queued follow-ups into the composer**: after a
cancel the composer may hold text nobody retyped, and the next prompt submits it
together with the new one. And `--follow-up`/`cancel` depend on pi's default
alt+enter/escape keybindings — a session whose user remapped them loses both
silently.

Errors carry a stable code. `admission_timeout`, `delivery_timeout` and
`transport_error` are **indeterminate** — the prompt
may already have landed, so do not blindly retry; inspect with `gmux agent
status`/`gmux agent logs` first. So is a bare transport failure with no code (a dropped
connection to gmuxd): the request may have been delivered before the connection
went away. `runner_outdated` (the session predates semantic actions: restart it),
`precondition_failed`, `delivery_pending`, `not_ready`, `not_running` and
`incarnation_mismatch` (the runner was replaced mid-flight and the replacement
refused an action meant for its predecessor) all guarantee nothing was delivered
and are safe to retry. Local sessions only (peer ids are refused).

## Sequential orchestration

```bash
id=$(gmux -d -- pi "implement the feature")
gmux wait $id

gmux agent prompt --timeout 900 $id "$(cat review.txt)"
gmux agent status $id
```

For **raw** (non-agent) sessions, **prefer `gmux send --wait` over `gmux send … && gmux wait`** for "send a
prompt and wait for the reply": the two-command composition can observe the
*previous* turn's idle state and return before the agent has even started,
while `--wait` is race-free (the daemon arms the wait before delivering the
input and requires a fresh active→idle transition). `--wait` requires the
input to submit (a trailing `Enter` or a `\r` in piped stdin) and accepts
`--timeout N`; exit codes match `gmux wait` below.

## Parallel orchestration

```bash
ids=()
for ticket in fa-48 fa-49 fa-52; do
  ids+=( "$(gmux agent prompt --new --no-wait --name "$ticket" "Implement $ticket. Return when done.")" )
done

for id in "${ids[@]}"; do
  gmux wait "$id" --timeout 600 || echo "$id failed: $?"
done

for id in "${ids[@]}"; do
  echo "=== $id ==="
  gmux agent logs --agent -n 1 "$id"   # each agent's final answer
  # ...or 'gmux agent status "$id"' for state + trigger + answer
done
```

## Waiting

`gmux wait <id>` blocks until the session goes **idle** — an agent finishing
its turn, a shell finishing its command and returning to a fresh prompt, or
a one-shot command's process exiting — optionally bounded by `--timeout N`.
Exit codes (the same global taxonomy `gmux agent` uses). A bare `wait` on an
already-idle session returns at once and reports the **last** turn's conclusion
and result — to gate on a turn you are about to trigger, use `gmux agent prompt`
or `gmux send --wait`, which arm the wait before delivering:

- `0` the turn completed normally (a one-shot that exited **0**, a shell back at
  its prompt)
- `2` the turn was intentionally interrupted
- `1` everything else: the turn ended in an error, the session died mid-turn,
  or `--timeout` elapsed

**A failed command fails its wait — for lifetime turns only**: a session whose
turn is its whole lifetime (`gmux -d -- pnpm test`, a shell *without* OSC 133
integration) closes that turn with an error on a non-zero child exit, so
`gmux wait` exits 1, and `gmux wait $id && next-step` cannot run after a failed
build. If you only need "it finished", read `exit_code` from `gmux ls --json` or
run it in the foreground (`gmux -- pnpm test`), which propagates the child's code.

**Per-command turns carry no result.** In a shell whose integration emits OSC 133
prompt marks (fish, out of the box), every command is its own turn and gmux reads
the marks purely as busy/idle — the exit code in the `D` mark is not consumed. So
`gmux wait` exits 0 when the prompt returns, pass or fail: `gmux wait $shell_id &&
deploy.sh` **will** deploy after a failed build. Run the command as its own
session (`gmux -- make build`) if you need its result.

For a pi session whose turn completed, `wait` also **prints the agent's latest
final message** — the same answer `gmux agent status` reports — so
`answer=$(gmux wait $id)` works. `--quiet` suppresses it. A failed, interrupted
or dead turn prints nothing (the stderr line says which); shell sessions,
Claude/Codex sessions and output-condition waits are synchronization-only.

Every session is waitable. **Shell sessions** get per-command idle from OSC
133 prompt marks (fish emits them by default; bash/zsh need shell
integration): `gmux send --wait <id> 'make build' Enter` blocks until the
command finishes and the prompt returns. Sessions that never emit marks —
one-shot `gmux -d -- <cmd>` runs, or shells without integration — are one
lifetime-long turn: `wait` blocks until the process exits (`gmux -d -- pnpm
test; gmux wait $id` waits for the test run). Careful: an interactive shell
without integration never exits on its own, so bound that wait with
`--timeout` or use an output condition below. Waits issued after the exit
answer the same as live ones.

To wait for specific **output** instead of idle, use `--for-text <substr>` or
`--for-regex <pattern>` (works for shell sessions too — no grep loop needed):

```bash
gmux wait $id --for-text 'listening on' --timeout 60
gmux wait $id --for-regex 'tests? passed: \d+' --timeout 120
```

Same exit codes (`0` matched, `1` the session exited first or the timeout
elapsed), and no result is printed. Matching
is line-wise against the rendered terminal output (ANSI stripped, same text
`gmux tail` shows), including output that appeared before the wait
started, so the pattern must fit on one terminal line.

## Reading a session: three commands, split by what you know you want

| You want | Command | `-n` counts |
| --- | --- | --- |
| The raw screen | `gmux tail <id>` (any session) | lines |
| The exact text you want | `gmux agent logs <id>` (agents) | messages (post-filter) |
| "Show me what matters" | `gmux agent status <id>` (agents) | — |

`gmux tail` is always raw: the last N lines of rendered terminal output, plain
text, for shells, one-shot commands and agents alike.

For agent sessions backed by a conversation file (pi), `gmux agent logs` prints
the conversation itself as markdown — `## User` / `## Assistant` messages with
compact `[tool] …` one-liners — instead of the TUI's terminal rendering. `-n`
counts messages there, not lines; thinking blocks and tool outputs are omitted.
Type filters pick what you get: no flag gives user messages plus assistant
prose, and `--user`/`--agent`/`--tool`/`--all` **replace** that default set
(`--json` prints `{role, type, text, prose}` objects). There is no `--thinking`:
nothing renders thinking blocks today, so the flag is refused by name.

`gmux agent status <id>` is the other end of the split — one fixed shape
whatever the session is doing: a state line (alive/dead, active/idle, the last
closed turn's outcome and rough recency), the triggering excerpt, and the
relevant content (the answer when idle; the last few messages and a working
indicator while a turn runs). A running turn reports no outcome, and a session
that died mid-turn reports `the turn never finished (runner died)` — the same
verdict `gmux wait` gives that row, never "completed". `--json` gives one object
with those parts and a `content.kind` of `"answer"`, `"recent"` or `"none"`;
fields for a turn that does not exist (`state.last_turn_outcome`,
`state.last_turn_cause`) are absent rather than empty.

Both agent reads are snapshots of the stored conversation: they never start or
resume anything, they work on a dead session (local sessions only), and they can
be staler than the answer a `wait` or a synchronous `prompt` carries. This is
the best way to read another agent's work:

```bash
gmux agent prompt $id 'summarize your findings'
gmux agent logs --agent -n 1 $id  # just the reply
gmux agent logs $id -n 2          # the prompt and the reply, clean markdown
gmux agent status $id             # state + trigger + answer, one report
```

Shell sessions, and agents with no readable conversation (`unsupported_adapter`),
are read with `gmux tail` instead.

## Other agents have one-shot modes

Agents stay running by default. To make them exit after one prompt, use the
agent's print mode: `pi -p`, `claude -p`, `codex exec`:

```bash
gmux -- pi -p "summarize this PR"
```

## Sessions on other machines

Sessions are **local by default** — bare IDs only ever match this host, so you
can't accidentally act on another machine. To address a peer session
explicitly, suffix the ID with `@<peer>` (see them with `gmux ls --all`):

```bash
gmux tail abc123@laptop
```

## Reference

- <https://gmux.app/reference/cli/>
- <https://gmux.app/integrations/scripts-and-agents/>
