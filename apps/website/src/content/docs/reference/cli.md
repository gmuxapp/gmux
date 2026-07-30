---
title: CLI
description: Command reference for gmux and gmuxd.
tableOfContents:
  maxHeadingLevel: 3
sidebar:
  order: 1
---

## Overview

**`gmux`** — the command you use. It runs commands in managed sessions and
drives them (list, attach, tail, send, wait, kill), drives agent sessions
(`gmux agent`), plus daemon control and pairing. It auto-starts the daemon when needed.

**`gmuxd`** — the daemon process. Serves the web UI, session history, and
optional remote access. You rarely invoke it directly; `gmux` starts it for
you, and `gmux daemon …` controls it.

`gmux` is verb-first: `gmux <verb> [args]`. Running a command is the one form
that isn't a verb — it uses an explicit `--` separator so gmux never has to
guess where its flags end and your command begins.

Coming from a pre-2.0 version? Every removed flag (`--list`, `--attach`,
`--send`, …) and the old bare-command shorthand print a precise error naming
the new form — see [Migrating to 2.0](/migrating-to-2/).

## Running a command

### `gmux -- <command> [args...]`

Run a command inside a gmux session. Everything after `--` is your command,
verbatim — including its own flags. The session registers with gmuxd and
appears in the web UI.

```bash
gmux -- bash
gmux -- python3 main.py
gmux -- pi "build the feature"
gmux -- pytest --watch        # --watch belongs to pytest, not gmux
```

There's no bare shorthand: `gmux pytest` is an "unknown command" error (gmux
suggests the `gmux -- pytest` form). If you run commands constantly,
`alias gm='gmux --'` gives you `gm pytest` back.

Behavior depends on whether stdin is a terminal and whether you're already
inside a gmux session:

| Stdin              | Inside `GMUX=1`? | Behavior                                                                                                |
| ------------------ | ---------------- | ------------------------------------------------------------------------------------------------------- |
| TTY                | no               | Attach: wire your terminal to the child PTY, forward Ctrl-C and resize, detach when the terminal closes. |
| TTY                | yes              | Auto-detach: spawn the session in the background and return immediately, so PTYs don't nest.            |
| Pipe / file / null | either           | Block, stream bounded metadata to stdout, exit with the child's exit code. The session keeps running for the UI to attach to. |

The pipe/file/null row is the canonical shape for scripts and agent harnesses:
a blocking call, bounded stdout (no full PTY noise in your logs), and reliable
exit-code propagation — so `if gmux -- pytest -q; then …` works.

See [Scripts and agents](/integrations/scripts-and-agents/) for the narrative
version with a worked build-and-report example.

### `gmux -d -- <command> [args...]`

Detached run. Spawns the session in the background and prints its session id on
stdout, so a script can capture it (`id=$(gmux -d -- pi "…")`) and drive it
without polling. `-d` must come before `--`.

## Managing sessions

Sessions are **local by default**: a bare id only ever matches a session on
this machine, so you can't accidentally act on another host. To target a peer,
suffix the id with `@<peer>` (see `gmux ls --all`). IDs are the 8-character
full eight-character form; verbs also accept a unique ID prefix or
the session's slug.

### `gmux ls`

List sessions, alive first, newest first. Local only unless `--all`.

```
$ gmux ls
ID        STATUS  ADAPTER  TITLE
a3f20187  alive   pi       fix auth bug  (/home/mg/dev/myapp)
be14b052  alive   shell    bash          (/home/mg/dev/gmux)
7d3304e9  dead    shell    make build    (/home/mg/dev/myapp)
```

- `--all` — include sessions from every connected peer (ids print as `<id>@<peer>`).
  Dead sessions are listed either way; `--all` widens the host scope, not the
  liveness filter.
- `--json` — emit a JSON array instead of the table, for scripts and agents.

The `--json` objects always carry `id`, `adapter` and `alive`, plus these when
they are set: `peer`, `cwd`, `pid`, `title`, `slug`, `parent_session_id`,
`socket_path`, `command`, `started_at`, `exited_at`, `exit_code`. Note there is
no `idle` field — whether an agent is mid-turn is a question for `gmux wait`,
not for a list snapshot.

### `gmux attach <id>`

Reattach your local terminal to a session. Scrollback is replayed on connect,
SIGWINCH is forwarded, and closing the terminal detaches without killing the
session. Requires an interactive terminal. Peer sessions work transparently —
gmuxd proxies the WebSocket to the owning host.

```bash
gmux attach a3f20187
gmux attach fix-auth-bug      # slug also works
gmux attach a3f20187@desktop  # a session on a peer
```

### `gmux tail <id>`

Print the last lines of the session's **terminal output** as plain text — what
is on its screen. Always the raw view, for every kind of session: shell,
one-shot command, or agent.

```bash
gmux tail a3f20187            # last 100 lines of terminal output
gmux tail -n 5 a3f20187       # last 5 lines
```

`-n` counts lines (default 100). ANSI escapes are stripped: the daemon renders
scrollback through a terminal emulator, so what you get is the visible text.

It's a snapshot, not a stream — to watch a session live, attach to it or open
it in the browser.

For an agent session, the semantic view usually answers the question better:
[`gmux agent logs`](#gmux-agent-logs-id) renders the conversation as
exchanges — what was asked, how much work happened, how it ended — and defaults
to the latest exchange, which also shows `[Agent active, …]` while work is in
flight.

:::note[Changed]
`gmux tail` briefly defaulted to the conversation transcript for agent
sessions. That view now has its own command, `gmux agent logs`, and `--raw`
(with its `-e`/`-r` aliases) is gone — tail is raw by definition. The flags
still print an error naming the replacement.
:::

### `gmux send [--wait [--timeout N]] <id> [text] [Key...]`

Inject input into a running session as if typed at the keyboard. The first
argument after the id is literal text (unless it names a key); every argument
after it must name a key (`Enter`, `C-c`, `Escape`, `Up`, …) and is sent as
that key. **Submission is explicit** — add a trailing `Enter` to dispatch a
line; omit it to leave the input unsent.

```bash
gmux send a3f20187 'describe yourself' Enter   # type and submit
gmux send a3f20187 'half a thought'            # type, leave it unsent
gmux send a3f20187 C-c                          # interrupt (Ctrl-C)
gmux send a3f20187 Escape                       # send Escape
echo "$body" | gmux send a3f20187 Enter         # pipe stdin, then submit
```

When no text is given and stdin is a pipe, gmux reads stdin until EOF (capped
at 1 MiB) and sends it — the natural shape for files and heredocs. Include a
trailing `Enter` to submit piped input.

**An unrecognized name in key position is an error**, not text: `gmux send abc
'make test' Etner` fails naming `Etner` instead of typing it, so a typo (or a
key gmux deliberately does not encode) can never be delivered as prose under a
success exit code. Literal text belongs in the text argument — quote it as one
argument — and `gmux send-keys` keeps tmux's literal fallback for scripts that
rely on it. `gmux send --help` lists the full key vocabulary, including which
modified keys are unsupported and why.

**Everything after the session id is verbatim** — including tokens that start
with a dash. `gmux send abc -v` sends the literal `-v`, and no `--` guard is
needed for dash-leading text. The trade-off is that `send`'s own flags are only
recognized *before* the id (the first non-flag token). A `--` before the id is
accepted as an explicit end-of-flags marker.

**`--wait`** fuses send-and-wait into one race-free step: deliver the input,
then block until the turn it triggers completes, with the same three exit codes as
[`gmux wait`](#gmux-wait-id): `0` the turn completed, `2` it was **intentionally
interrupted**, `1` anything else (an errored turn, a death, a timeout, a transport
failure). `send --wait` reports that shared conclusion but prints no report;
use
[`gmux agent prompt`](#gmux-agent-prompt-flags-id-prompt) or `gmux wait` for those. Bound it with
`--timeout N`. Because gmuxd subscribes to the session's events *before*
forwarding the bytes, it can't mistake the previous turn's idle state for the
reply — unlike the racy `gmux send X Enter && gmux wait X` composition. The
flags precede the id:

```bash
gmux send --wait a3f20187 'do the thing' Enter        # block until the reply lands
gmux send --wait --timeout 600 a3f20187 'go' Enter    # ...or fail after 600s
```

Like `gmux wait`, `--wait` works for every session (see [`gmux
wait`](#gmux-wait-id)): for an agent it blocks until the reply lands; for a
shell whose integration emits OSC 133 prompt marks, until the command you
sent finishes and the prompt returns; for anything else, until the process
exits. Local sessions only for now.

For verbatim tmux compatibility there's also `gmux send-keys -t <id> <keys...>`
(all arguments are key names by default; `-l` sends them as literal text).
Use plain `send` for everyday use; `send-keys` only when porting tmux commands.

**Access control.** `send` is powerful — anything you send lands in the
session's PTY, indistinguishable from keyboard input. Access is gated by
gmuxd's local IPC: the daemon's Unix socket is owner-only, so only your user
can send to sessions on this host. Peer sessions are reached through gmuxd's
authenticated proxy.

### `gmux wait <id>`

```
gmux wait [--quiet] [--timeout N] [--for-text S|--for-regex P] <id>
```

Block until the session settles, optionally bounded by `--timeout N`. For an
agent session that means the current **activity** ends — the agent itself
asserts the boundary; for a shell session, that the command finished and the
shell is back at a fresh prompt; for a one-shot command
(`gmux -d -- pnpm test`), that the process exited.

```bash
gmux agent prompt --no-wait a3f20187 'do the thing'
gmux wait a3f20187
gmux wait a3f20187 --timeout 600   # or fail after 600s
```

The signal is the same `Status.Active` flag the UI's spinner consumes — the
session's **activity state**. Agents source it from their own hooks, so `wait`
returns the moment the agent asserts its work settled; shells source it from
OSC 133 prompt marks (see below); every other session is one lifetime-long
turn that closes when the process exits.

Exit codes are gmux's global taxonomy — the same codes every verb that
reports a gmux verdict uses. (Three verbs deliberately don't: `gmux -- <cmd>`
and `gmux edit` propagate the child's own exit code, and
`gmux daemon|auth|remote` forward gmuxd's, whose usage and state failures still
use its own codes.)

- `0` — the activity completed normally (including a one-shot command that
  exited **successfully**, or a shell returning to its prompt), or the output
  condition matched
- `2` — the activity was **intentionally interrupted** (a human or another
  agent stopped it)
- `1` — anything else: the activity failed, the `--timeout` elapsed, the
  session exited before its output matched, or the command was misused
- `128+N` — a first local SIGINT/SIGTERM stopped **the wait**, not the agent
  (see below)

**A failed command fails its wait — only for lifetime turns.** A session whose
turn is its whole lifetime (`gmux -d -- make build`, a shell *without* OSC 133
integration) closes that turn with an error when the child exits non-zero, so
`gmux wait` on it exits `1`: `gmux wait $id && deploy.sh` cannot deploy a failed
build there. Use the exit code, not the wait, if you only care that the process
*finished*: `gmux -- make build` propagates the child's own code, and
`gmux ls --json` reports `exit_code`.

**Per-command turns carry no success or failure.** In a shell whose integration
emits OSC 133 prompt marks (fish does by default), each command is its own turn,
and gmux uses the marks only as busy/idle transitions — the exit code the `D`
mark carries is deliberately not consumed. So `gmux wait` exits `0` when the
prompt comes back, whether the command succeeded or failed, and
`gmux wait $shell_id && deploy.sh` deploys after a failed build. To gate on a
shell command's result, run it as its own session (`gmux -- make build`, whose
exit code is propagated) rather than typing it into an interactive shell.

**The report.** For a renderer-capable agent (pi today), `wait` prints an
**exchange report** on stdout — the same document `agent prompt` and
`agent logs` render (see [Agent sessions](#agent-sessions) for the format):
what was asked, every further user message that entered the loop, how many
iterations of work each caused, and the terminal response.

A live report is carried within an honest transport budget. An activity that
outgrows it never loses its outcome or terminal content, but display material
can be cut, and every cut is marked: an opening
`[N exchange(s) and M bytes omitted from live report]` line when early user
boundaries were dropped, and an `[AGENT, truncated]:` label when terminal
prose was capped. When you see either marker, `gmux agent logs -n N` has the
full text — it reads the complete native history and is never subject to the
live budget.

```bash
report=$(gmux wait a3f20187)      # the exchange report; check $? for the verdict
gmux wait --quiet a3f20187        # synchronize only, print nothing
```

**The exit code is the verdict; stdout is the report.** The report is printed
for **every** outcome gmux can observe — completed, interrupted, failed, and
timed out alike. A nonzero exit with a stdout report means "the wait did its
job; here is the bad news": an interrupted activity's report ends
`[Agent interrupted]` (exit 2), a failed one ends `[Agent failed: <reason>]`
(exit 1), and a timeout ends
`[Wait timed out after Ns; agent active, N iterations so far...]` (exit 1) —
the agent keeps running. stderr is reserved for gmux's **inability** to produce
the report at all: an unknown session, an unsupported adapter, a daemon or
protocol failure. Those exit 1 with an empty stdout. There is no `--json` on
`wait` today; script against the exit code and the report.

**The wait is observational.** Nothing that enters the running loop ends the
wait early: somebody else's `--steer`, a `--follow-up` merged into the loop, a
human typing into the TUI — all of them simply appear in the report as new
`[USER]:` boundaries. The wait ends only when the agent asserts the activity
settled (or on your timeout/signal), and the report tells the whole merged
story, so every waiter on the same activity gets the same account.

**A late wait travels back.** A bare `wait` on a session with no activity in
progress returns immediately, renders the **latest visible exchange**, and
exits by that activity's settled outcome. Arriving just before or just after
the settle yields the same verdict and the same terminal content. A
conversation with no exchanges yet reports `[No exchanges yet]` and exits 0;
history gmux never observed settle is reported as a plain snapshot, without a
fabricated verdict. To gate on an activity you are about to trigger, use
`gmux agent prompt` or `gmux send --wait`, which arm the wait before
delivering anything.

**Signals.** A first `^C` (or SIGTERM) stops **the wait**, not the agent: gmux
prints exactly

```
[Wait interrupted; agent remains active]
```

and exits `128+N`. The line is deliberately fact-free — until the wait
completes, the CLI has received no exchange facts to report, and it states
only what it knows rather than guessing; read the conversation with
`gmux agent logs` afterwards. A second signal terminates immediately. Under
`--quiet` the first signal prints nothing either — exit `128+N`,
verdict-only. `gmux wait <id>` re-arms.

Also result-free, and perfectly waitable: shell/process sessions, agents
without a renderer (Claude, Codex), and output-condition waits — they
synchronize and exit by the verdict, printing no report.

**Output conditions.** Instead of the idle signal, wait until specific text
appears in the session's output:

```bash
gmux wait a3f20187 --for-text 'BUILD OK'          # substring match
gmux wait a3f20187 --for-regex 'error: \d+'       # Go regexp match
```

`--for-text` and `--for-regex` are mutually exclusive, and an invalid regexp
is a usage error. The match runs **server-side** against gmuxd's on-disk
scrollback (matched per rendered, ANSI-stripped line), so nothing scrolls past
unseen between polls (loss is bounded by the scrollback cap, not a poll
interval).

**Shell sessions.** A shell's per-command idle signal comes from OSC 133
prompt marks ("semantic prompt" sequences): the runner flips the session
busy when a command starts executing and idle when the next prompt is
drawn. The marks come from your shell's integration — **fish** emits them
out of the box; bash and zsh need an integration snippet (the same one used
by kitty, VS Code, or WezTerm semantic prompts — e.g. for zsh, emit
`\e]133;A\a` in `precmd` and `\e]133;C\a` in `preexec`). A session whose
output never carries the marks — a one-shot `gmux -- <cmd>`, or an
interactive shell without integration — stays on the lifetime turn: `wait`
blocks until the process exits. For an interactive shell that can mean
"forever" (it is never provably idle), so bound the wait with `--timeout`
or use `--for-text`/`--for-regex`. Idle wait is local-only for now (peer
support is pending).

## Agent sessions

`gmux send` types bytes at a terminal. The `gmux agent` verbs speak to the
agent itself: they wait until it can accept input, submit the way that agent
expects, and report what the daemon actually observed. Use them for agent
sessions and keep `send`/`send-keys` for raw keystrokes.

Agent sessions on **this host** only, and **pi** only for now. Other agents
(Claude Code, Codex) report an explicit "unsupported" error rather than
quietly typing something — drive those with `gmux send` and read them with
`gmux tail`. `gmux agent help` prints the namespace guide in the terminal;
each verb also answers `--help`.

**The handle is a conversation, not a process.** An agent session names a
conversation that can always be continued; whether a process currently hosts
it is gmux's problem. Semantic surfaces report the agent as **active** (work
in progress) or **inactive** (settled) — never "alive" or "dead". Prompting an
inactive conversation transparently resumes it; reading one never does.

**Vocabulary.** A **visible exchange** is one user message and everything the
agent did up to the next user message. An **iteration** is one completed
assistant/model response — tool-use rounds included — so the iteration count
is the honest measure of how much work happened. Every semantic read renders
the same document:

```
[37 previous exchanges]

[USER]: run the full suite and fix what fails

[Agent worked for 15 iterations]

[USER]: also fix the lint warnings

[Agent worked for 4 iterations]

[AGENT]: All 212 tests pass and the linter is clean. Two fixtures needed …
```

Only the **terminal** response is printed; tool calls, thinking, and
intermediate prose are represented by the iteration counts. A final exchange
still in progress ends `[Agent active, N iterations so far...]`; a completed
one with nothing to say ends `[Agent completed without a final response]`;
partial terminal prose is labeled `[AGENT, partial]:` and followed by
`[Agent interrupted]` or `[Agent failed: <reason>]`. An empty conversation is
`[No exchanges yet]`.

Live wait/prompt reports are **bounded**: content cut by a cap is marked —
`[AGENT, truncated]:` on capped prose, and an opening
`[N exchange(s) and M bytes omitted from live report]` line when early user
boundaries fell out of the transport window. The outcome and terminal content
survive every cut. `gmux agent logs` reads the full native history, so it is
where the complete text of anything marked omitted or truncated lives.

Two reading commands split by **scope**, not by shape:

| You want | Command | Unit of `-n` |
| --- | --- | --- |
| The raw screen | [`gmux tail <id>`](#gmux-tail-id) (any session) | lines |
| The conversation | [`gmux agent logs <id>`](#gmux-agent-logs-id) | exchanges |

`gmux wait` and a synchronous `gmux agent prompt` render the same document
scoped to the **activity they observed** — the live, settle-faithful view.
`agent logs` is history: as many exchanges back as you ask for.

### `gmux agent prompt [flags] <id> [prompt]`

Send a prompt to an agent session and, by default, block until the activity it
starts (or joins) has settled, then print the exchange report.

```bash
gmux agent prompt a3f20187 'review the diff on this branch'
gmux agent prompt --new 'review the diff on this branch'     # launch, then prompt
gmux agent prompt --timeout 600 a3f20187 'run the full suite'
gmux agent prompt --no-wait a3f20187 'start the refactor'   # return once admitted
gmux agent prompt --follow-up a3f20187 'then update the docs'
gmux agent prompt --steer a3f20187 'stop, use the sqlite path instead'
gmux agent prompt --no-wait --steer a3f20187 'and skip the migration'
git diff | gmux agent prompt a3f20187                        # prompt from stdin
```

- **no flag** — start fresh work. Requires an inactive agent; an inactive
  conversation with no resident process is resumed transparently to deliver
  the prompt.
- `--follow-up` — submit after the current model response. Delivered to an
  **inactive** agent it starts ordinary work (like a plain prompt); delivered
  into **running** work it **merges into that activity** (pi drains its queue
  at the loop's next stopping point) — the merged activity settles once, and
  everybody waiting on it gets the same report with your follow-up as one of
  its `[USER]:` boundaries.
- `--steer` — redirect the activity in progress *right now*. Fails when no
  activity is in progress; steering nothing is not a thing.
- `--no-wait` — return as soon as the prompt is **admitted** instead of waiting
  for the activity to settle; print no report. A plain prompt (and
  `--follow-up` to an *inactive* agent) starts work, so `--no-wait` returns
  once the agent has actually begun it and exit `0` is a health event rather
  than a delivery receipt. `--steer` and a `--follow-up` that merges into
  *running* work join an activity that was admitted before this prompt
  existed, so there is nothing to admit beyond delivery and `--no-wait`
  returns as soon as the text is delivered.
- `--timeout N` — stop waiting after N seconds (the work keeps running).
  Absent or `0` waits indefinitely. It bounds *your* wait, so
  `--no-wait --timeout N` is refused as a usage error rather than silently
  ignored.

`--follow-up` and `--steer` are mutually exclusive — each names a different
delivery. `--no-wait` composes with either: it only decides whether *you* block.
No flag may be repeated. Flags go **before** the id; everything after the id is
the prompt, verbatim, so `gmux agent prompt a3f20187 --steer` prompts with the
literal text `--steer`. The prompt is one argument — quote it — or pipe it on
stdin (multi-line piped input stays one prompt and is not submitted line by
line). Prompts are capped at 1 MiB, and an oversized prompt or one that isn't
valid UTF-8 is refused before anything is sent — never truncated, never
re-encoded.

**The report.** A synchronous prompt prints the exchange report of the
activity its prompt started or joined — exactly the [`gmux wait`
document](#gmux-wait-id): your prompt (abbreviated, since you already know
it), every other user message that entered the loop, the iteration counts,
and the terminal response — within the live transport budget, whose cuts are
always marked (`[AGENT, truncated]:`,
`[N exchange(s) and M bytes omitted from live report]`); `gmux agent logs`
has the full text. Wait/prompt reports abbreviate exactly two
things — the anchoring user message and any later user message whose text is
an exact match for what this command submitted (cut at 20 words or 240
characters, marked with `…`). `agent logs` never abbreviates.

Exit codes are the global taxonomy shared with [`gmux wait`](#gmux-wait-id):
`0` the activity completed, `2` it was **intentionally interrupted**, `1`
anything else (a failed activity, a `--timeout`, a transport failure), and
`128+N` for a first local signal (which prints only the
`[Wait interrupted; agent remains active]` line; a second signal kills
immediately). The report is printed on stdout for every one of those
outcomes; stderr only ever explains why no report could be produced.

**Steering does not interrupt anybody.** A `--steer` or merged `--follow-up`
(or a human typing into the TUI) becomes a new `[USER]:` boundary in the
report of every wait observing that activity — no wait resolves early, nobody
has to re-arm, and there is no ownership question to adjudicate: the activity
settles once and everyone reads the same merged story.

A prompt gmux could not deliver at all — an unknown session, an unsupported
adapter, a transport failure — prints nothing on stdout and a concise account
on stderr instead.

Failures name a stable code, and the wording distinguishes what is known about
delivery. `admission_timeout`, `delivery_timeout` and
`transport_error` mean the prompt may already have reached the agent: inspect the
session before resending, because a retry can duplicate it. A transport failure
with no code at all — a dropped connection to gmuxd, a daemon restarted
mid-prompt — is indeterminate for the same reason: the request may have been
delivered before the connection went away.

The codes that guarantee **nothing** was delivered, and are therefore safe to
retry as-is: `runner_outdated` (the session started before semantic actions
existed — restart it, or drive it with `gmux send`), `precondition_failed`,
`delivery_pending`, `not_ready`, `not_running`, and `incarnation_mismatch` (the
session's runner was replaced while the prompt was on its way, and the
replacement refused an action meant for its predecessor).

#### `--new`: launch a session and prompt it in one command

```bash
gmux agent prompt --new [--model M] [--name N] [--timeout N] [--no-wait] [prompt|-]

# handoff pattern, one command instead of two
id=$(gmux agent prompt --new --no-wait --name review 'review the diff on this branch')
gmux wait "$id"

# synchronous: the id, a blank line, then the report
gmux agent prompt --new --model anthropic/sonnet 'summarize this repo'
```

`--new` launches a **new pi session** the same way `gmux -d -- pi` does — from
this shell's env and cwd, on the local daemon only — and sends the prompt as
its first work. Pass either a session id or `--new`, never both.

- `--model M` / `--name N` — pi's `--model` and `--name`. Valid **only** with
  `--new`; without it they are usage errors.
- `--follow-up` and `--steer` are **refused** with `--new`: a session that does
  not exist yet has no work to queue behind or steer.
- `--no-wait` and `--timeout` mean what they always mean. `--timeout` bounds
  your wait, never the launch: agent readiness runs on pi's own fixed 10 s
  window, and admission on the daemon's fixed 60 s one.
- The prompt is the single positional argument, or `-`/nothing to read stdin.
- `--new` must come **before** the prompt. After a session id it is prompt text
  like any other token, so `gmux agent prompt a3f20187 --new` prompts that
  session with the literal text `--new`. The `-`-means-stdin spelling is
  likewise `--new`-only: after a session id, `-` is a literal prompt.
- Other agents answer `unsupported_adapter`: launch those with `gmux -d -- <cmd>`
  and prompt the id it prints. That two-step route stays valid for pi too.

**The session id is stdout line 1**, printed the moment the session exists and
*before* the prompt is delivered — so a watcher can attach or tail while the
agent is still coming up, and so you can always address the session you just
paid for even when admission or the turn then fails.

The line means exactly one thing: **the session exists and is addressable.** It
is not an admission receipt, not a readiness signal, and not a claim that the
prompt was delivered — the exit code carries all of those. Two consequences
worth pinning:

- Under `--new`, **the completion signal is the exit code, not non-empty
  stdout.** A successful synchronous run prints the id, a blank line, then the
  exchange report; a failed one prints the id and exits non-zero.
- With `--no-wait`, the bare id is the only output and exit `0` means the work
  was **admitted**: the id prints immediately, but the process returns only once
  the agent has started it (or the admission window expires). On a sick
  session `id=$(gmux agent prompt --new --no-wait …)` can therefore block up to
  that window instead of returning at delivery — exit `0` buying the stronger
  claim is the point.

If anything fails **after** the launch — admission, readiness, the work itself —
the session stays behind and it is **yours**: gmux does not tear it down, and it
may still be running. Retry against the printed id, read it with `gmux agent
logs`, or `gmux kill <id>` it.

If the launch itself fails (nothing registered with gmuxd), **nothing** is
printed on stdout and the command exits `1`: there is no session to address.
The rule scripts can rely on is that stdout line 1 is a session id whenever a
session exists, and stdout is empty whenever one does not. A prompt that is
empty, oversized or not valid UTF-8 is refused before anything is spawned, so a
usage error never leaves an orphan session behind.

### `gmux agent cancel <id>`

Interrupt the work an agent is doing.

```bash
gmux agent cancel a3f20187

# ...and wait for it to stop. `wait` exits 2 for interrupted work — which is
# exactly what you asked for — so do not chain it with `&&`.
gmux agent cancel a3f20187
gmux wait a3f20187; [ $? = 2 ] && echo stopped
```

Returns once the interrupt is **delivered**, not once the agent has stopped —
otherwise a wedged agent would hang the command. Follow with `gmux wait` when
the next step needs the work to be over. Fails when no activity is in
progress: cancelling nothing is not a thing.

**Two pi-specific caveats.** Cancelling is not a clean stop: pi's interrupt
handler *restores any queued follow-ups into the composer*, so after a cancel the
composer may hold text nobody retyped — and the next `gmux agent prompt` submits
that text along with the new prompt, as one piece of work. Check with `gmux agent
logs` if a session has queued follow-ups you did not see run. And `--follow-up`
and `cancel` ride pi's *default* keybindings (alt+enter, escape); a session whose
user remapped those loses both silently — the bytes still arrive, they just no
longer mean that action. Plain prompts (Enter) are unaffected.

### `gmux agent logs <id>`

Render the conversation's stored history as visible exchanges — the same
document the wait report uses, read from the adapter's own conversation file
(pi's JSONL) rather than the terminal rendering of the TUI.

```bash
gmux agent logs a3f20187              # the latest exchange
gmux agent logs -n 3 a3f20187         # the last three exchanges
```

- `-n N` counts **visible exchanges** (default 1) and must be positive. Each
  exchange starts at a user message; the work it caused shows as
  `[Agent worked for N iterations]`, and only the terminal response is printed
  in full. Earlier history is summarized as `[N previous exchanges]`, counting
  the conversation's active branch only. The flag may sit on either side of
  the id.
- `logs` never abbreviates — user messages print in full, unlike the
  wait/prompt report's anchor abbreviation.
- While the agent is active, the latest exchange ends
  `[Agent active, N iterations so far...]` — this is how you check on running
  work without waiting on it.
- A **store-only read**: it never starts or resumes anything, so it is always
  safe to look. Local sessions only.
- A readable conversation with nothing in it prints `[No exchanges yet]` and
  exits 0. Exit 1 with a stderr account when there is no report to give:
  `no_conversation` (the conversation source cannot be resolved or read) or
  `unsupported_adapter` (this agent has no conversation gmux can read) — use
  `gmux tail <id>` for those, which shows the terminal itself.

`logs` groups by **user message**, not by settled activity: adapter storage
records no activity boundaries, so a historical activity that contained two
user messages reads as two exchanges. The wait report is the activity-faithful
view, available exactly when the activity is observed.

### `gmux kill <id>`

Terminate a running session: `SIGTERM` to the child, normal exit lifecycle,
session marked dead — the same path as the UI's kill button.

```bash
gmux kill a3f20187
```

### `gmux edit [file]`

Open a file in a managed **editor session** — a first-class tab in the UI.
Blocks until the editor exits and propagates its exit code, so it works as
`$EDITOR` (git commit, etc.), even from inside another gmux session. With no
file, the session prompts for a path (`~` expands).

```bash
gmux edit notes.md
export EDITOR='gmux edit'   # git commit opens an editor tab
```

Inside gmux sessions, `EDITOR`/`VISUAL` already default to `gmux edit` when
your dotfiles don't set them. Today the session runs a fallback terminal
editor: `$GMUX_EDIT_FALLBACK` if set (may include flags, e.g. `vim -u NONE`),
otherwise the first of `nano`, `vim`, `vi` on PATH. `edit` takes at most one
path and no flags.

## UI, pairing, and the daemon

### `gmux open`

Open the gmux UI in a browser, starting gmuxd if needed. Prefers Chrome/Chromium
in app mode for a standalone window; falls back to the default browser. (Bare
`gmux` with no arguments prints help — use `gmux open` to launch the UI.)

### `gmux auth`

Print this host's login URL and token — plus, when remote access is enabled,
a connect URL and QR code for pairing another machine. This reveals a secret — run it deliberately, not as a
status check.

### `gmux remote`

Set up or check Tailscale remote access. Walks you through enabling it the
first time, then reports connection status on later runs. See
[Remote Access](/remote-access/). (Shows connection *state* only; it never
prints the token — use `gmux auth` for that.)

### `gmux daemon <command>`

Control the daemon process. This is the canonical front for daemon lifecycle;
the underlying `gmuxd` binary keeps the same verbs for service managers.

```bash
gmux daemon status     # health, session counts, peer status
gmux daemon start      # start in the background (replaces a running instance)
gmux daemon stop       # stop the running daemon
gmux daemon restart    # restart; active sessions survive and are rediscovered
gmux daemon log-path   # print the log file path (for scripting)
gmux daemon state check       # verify database integrity (migrations, FK, SQLite integrity)
gmux daemon state backup <path>  # consistent online backup (contains peer tokens — treat as secret)
gmux daemon state export      # redacted JSON dump for bug reports
```

`gmux daemon status` example:

```
gmuxd 2.0.0 (ready)
  tcp:    127.0.0.1:8790
  socket: /home/user/.local/state/gmux/gmuxd.sock
  remote: https://gmux.tailnet.ts.net

Sessions: 3 alive (2 local, 1 remote), 12 dead (15 total)

Peers:
  • desktop (1 session)
    https://gmux-desktop.tailnet.ts.net
  ○ gmux-server (offline)
    https://gmux-server.tailnet.ts.net
  ✗ manual-peer (connection refused)
    https://peer.example.com
```

### `gmux version` · `gmux help`

Print the version, or the usage summary. `gmux help` accepts a trailing verb
name (it prints the full usage either way) — except the `agent` namespace,
which has real per-verb help: `gmux agent help` prints the namespace guide,
and `gmux agent prompt|logs|cancel --help` (or `gmux help agent …`) prints
per-verb detail.

## gmuxd

The daemon process. You normally start and control it through `gmux` (which
auto-starts it) and `gmux daemon …`. Invoke `gmuxd` directly only when a service
manager needs to own the process:

```bash
gmuxd run      # run in the foreground (for systemd, Docker, debugging)
gmuxd run --replace  # shut down a healthy same-version daemon first
```

`gmuxd run` refuses to replace a healthy same-version daemon (exits 0 "already running"). Use `--replace` to force replacement. Version upgrades still replace automatically.

Foreground `gmuxd` reads [`host.toml`](/reference/host-toml/), binds
`127.0.0.1` on the configured port (default 8790), and creates a Unix socket
for local IPC. For background start/stop/status/restart and the log path, use
`gmux daemon …` above. `gmuxd --help` lists the binary's own verbs and points
back to the `gmux daemon` equivalents.
