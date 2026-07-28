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
short form the UI shows; verbs also accept a unique id prefix, the full id, or
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

For an agent session, the semantic views usually answer the question better:
[`gmux agent logs`](#gmux-agent-logs-id) prints the exact conversation text you
ask for (filterable by message type) and
[`gmux agent status`](#gmux-agent-status-id) shows what matters right now: state
line, trigger excerpt, and the answer (or the last few messages while a turn is
running).

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
failure). `send --wait` reports that shared conclusion but prints no agent result;
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
gmux wait [--quiet] [--json] [--timeout N] [--for-text S|--for-regex P] <id>
```

Block until the session is **idle**, optionally bounded by `--timeout N`. For
an agent session, idle means its current turn finished (the spinner stops);
for a shell session, that the command finished and the shell is back at a
fresh prompt; for a one-shot command (`gmux -d -- pnpm test`), that the
process exited.

```bash
gmux send a3f20187 'do the thing' Enter
gmux wait a3f20187
gmux wait a3f20187 --timeout 600   # or fail after 600s
```

The idle signal is the same `Status.Active` flag the UI's spinner consumes
— the session's **turn state**. Agents source it from their turn hooks, so
`wait` returns the moment the agent emits its closing message; shells source
it from OSC 133 prompt marks (see below); every other session is one
lifetime-long turn that closes when the process exits.

Exit codes are gmux's global taxonomy — the same three codes every verb that
reports a gmux verdict uses. (Three verbs deliberately don't: `gmux -- <cmd>`
and `gmux edit` propagate the child's own exit code, and
`gmux daemon|auth|remote` forward gmuxd's, whose usage and state failures still
use its own codes.)

- `0` — the turn completed normally (including a one-shot command that exited
  **successfully**, or a shell returning to its prompt), or the output condition
  matched
- `2` — the turn was **intentionally interrupted** (a human or another agent
  stopped it), or the wait was resolved early because somebody **steered** the
  turn it was waiting on
- `1` — anything else: the turn ended in an error, the session died, the
  `--timeout` elapsed, the session exited before its output matched, or the
  command was misused. The stderr line names which.

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

The verdict is stable across timing: a `wait` issued after the session
already exited answers the same as one that watched it live. It is a verdict
about the **most recent** turn, though — a bare `wait` on an already-idle
session returns immediately and reports the conclusion (and the result) of the
turn that has already finished, not of a turn yet to come. To gate on a turn you
are about to trigger, use `gmux agent prompt` or `gmux send --wait`, which arm
the wait before delivering anything.

**Results.** When the session is a **result-bearing** agent (pi today — one that
asserts its own turn boundary, outcome and final message) and the turn it
observed **completed normally**, `wait` prints that turn's final message on
stdout. The answer comes from the agent's assertion for that exact turn, not
from a transcript read, so it can never be another turn's:

```bash
answer=$(gmux wait a3f20187)      # the agent's reply, or empty for a shell
gmux wait --quiet a3f20187        # synchronize only, print nothing
```

Nothing is printed when the turn ended in an error, was interrupted, or the
session died: a previous or partial turn's message presented as this turn's
answer would be worse than silence. The condition is **reported on stderr**
instead — outcome, reason, the excerpt the turn was triggered with, and the
injected text when somebody steered — so you never need a second command to
learn what a non-zero exit meant. Everything in that report comes from the facts
the agent asserted when it closed the turn; `gmux agent status <id>` is the
snapshot verb that reads the transcript, and can be staler. Also result-free, and
perfectly waitable: shell/process sessions, agents that assert no turn identity
(Claude, Codex), output-condition waits, and a wait that arrives after the turn
has already closed — it never observed that turn, so it reports the conclusion
without claiming its answer.

Pressing `^C` on a blocking wait stops **the wait**, not the session: gmux says
so on stderr, and `gmux wait <id>` re-arms.

**Steering interrupts waits.** A user message injected into the turn you are
waiting on — somebody else's `--steer`, a `--follow-up` merged into the running
loop, or a human typing into the TUI — changes what the pending answer answers,
so the wait resolves early: **exit 2**, reason `steered`, with the injected text
in the stderr report. The turn itself keeps running, so re-arm when you still
want its result:

```bash
until answer=$(gmux wait "$id"); do
  echo "somebody redirected the turn; waiting for the new answer" >&2
done
```

The injecting command is excluded from the interruption it causes — its own
synchronous prompt gets the merged close — but only while its message is the
loop's **last**: a later injection supersedes it, and it is then interrupted like
everybody else (reason `steered_again`). And if the turn settles before the agent
acknowledges the injected text, that command reports `indeterminate` (exit 1)
rather than the answer, because the close may predate its message entirely.

**`--json`.** One envelope on stdout for every outcome, and no stderr report —
the envelope is the account:

```bash
gmux wait --json "$id"
{
  "outcome": "steered",
  "reason": "steered",
  "trigger": "run the tests",
  "steered_by": "stop, fix the lint first"
}
```

Fields: `outcome` (`completed`, `steered`, `interrupted`, `error`,
`indeterminate`, `timeout`, `died`, `accepted`), `reason`, `output`, `trigger`,
`steered_by`, `truncated`, `message`. Fields describing a fact the resolution
does not have are **absent**, not empty. **Every** terminal path prints exactly
one envelope, success included — an output condition that matched is
`{"outcome":"completed","reason":"matched"}` (no `output`: matched bytes are
terminal text, not an agent result), and a prompt the daemon accepted without a
turn conclusion is `{"outcome":"accepted", ...}`. Silence is never an outcome, so
a script never has to guess whether empty stdout meant success or a crash. The exit code is unchanged, and
`--json --quiet` is a usage error (they ask for opposite things; the exit code
alone needs neither).

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

Three reading commands split by **what you know you want**, not by the shape
that comes back:

| You want | Command | Unit of `-n` |
| --- | --- | --- |
| The raw screen | [`gmux tail <id>`](#gmux-tail-id) (any session) | lines |
| The exact text you want | [`gmux agent logs <id>`](#gmux-agent-logs-id) | messages (post-filter) |
| "Show me what matters" | [`gmux agent status <id>`](#gmux-agent-status-id) | — |

:::note[Changed]
`gmux agent output` was replaced by `gmux agent status` (the report) and
`gmux agent logs --agent -n 1 <id>` (the answer alone). The old verb fails by
name and prints both replacements.
:::

### `gmux agent prompt [flags] <id> [prompt]`

Send a prompt to an agent session and, by default, block until the turn it
starts has ended.

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

- **no flag** — start a fresh turn. Requires an idle agent; a dead but retained
  session is restarted transparently to deliver the prompt (you'll see a note
  on stderr when that happens).
- `--follow-up` — submit after the current turn. Two modes, and the difference is
  observable: delivered to an **idle** agent it starts an ordinary turn (like a
  plain prompt); delivered into a **running** turn it **merges into that turn**
  (pi drains its queue at the loop's next stopping point), so there is no second
  turn — the merged close's answer is this follow-up's answer, and it interrupts
  anybody else waiting on that turn exactly like a `--steer` does.
- `--steer` — redirect the turn that is running *right now*. Fails if the agent
  is idle or dead; steering nothing is not a thing.
- `--no-wait` — return as soon as the prompt is **admitted** instead of waiting
  for the turn to finish. What that means depends on whether the prompt starts a
  turn: a plain prompt (and `--follow-up` to an *idle* agent) starts one, so
  `--no-wait` returns once the agent has actually begun it and exit `0` is a
  health event rather than a delivery receipt. `--steer` and a `--follow-up` that
  merges into a *running* turn join a turn that was admitted before this prompt
  existed, so there is nothing to admit beyond delivery and `--no-wait` returns
  as soon as the text is delivered.
- `--timeout N` — stop waiting after N seconds (the turn keeps running). Absent
  or `0` waits indefinitely. It bounds *your* wait, so `--no-wait --timeout N` is
  refused as a usage error rather than silently ignored.

`--follow-up` and `--steer` are mutually exclusive — each names a different
delivery. `--no-wait` composes with either: it only decides whether *you* block.
No flag may be repeated. Flags go **before** the id; everything after the id is
the prompt, verbatim, so `gmux agent prompt a3f20187 --steer` prompts with the
literal text `--steer`. The prompt is one argument — quote it — or pipe it on
stdin (multi-line piped input stays one prompt and is not submitted line by
line). Prompts are capped at 1 MiB, and an oversized prompt or one that isn't
valid UTF-8 is refused before anything is sent — never truncated, never
re-encoded.

- `--json` — print one envelope on stdout for the turn's resolution instead of
  the answer-plus-report shape, and nothing on stderr. Same fields as
  [`gmux wait --json`](#gmux-wait-id); under `--new` it follows the session-id
  line. Refused with `--no-wait`, which never waits for a resolution to describe.

Exit codes are the global taxonomy shared with [`gmux wait`](#gmux-wait-id):
`0` the turn completed, `2` the turn was **intentionally interrupted or
steered**, `1` anything else (a turn that ended in an error, a `--timeout`, a
dead runner, a transport failure, an injection the agent never acknowledged).
Timeouts have no code of their own — the stable error code on stderr says far
more than a number could.

**Steering, and being steered.** A message injected into the turn you are
waiting on resolves your wait early with exit `2` and reason `steered` (see
[`gmux wait`](#gmux-wait-id)); the turn keeps running, so re-arm with
`gmux wait <id>` if you still want the result. Your own `--steer` (or merged
`--follow-up`) does **not** interrupt you: gmux matches the agent's report of a
message entering the loop against the text it delivered for you, and hands you
the merged close. That claim holds only while your message is the loop's last
injection — a later one supersedes it (`steered_again`) — and if the turn settles
before the agent acknowledges your text at all, the result is `indeterminate`
(exit `1`) rather than an answer that may predate your message.

The acknowledgement is correlated **by text**, because the agent's report carries
the message and nothing else. gmux therefore requires an exact match — or a prefix,
but only for an excerpt the agent explicitly reported as truncated — and refuses
to guess when two in-flight deliveries could explain the same message: an ambiguous report is
credited to nobody, so both callers get `indeterminate` and every bystander is
interrupted. The residue is a human typing text identical to your steer at that
moment — indistinguishable in principle, and it degrades to `indeterminate`
rather than to a wrong answer.

On normal completion the agent's answer is printed on stdout. It is the
agent's OWN assertion about that turn, carried out of the agent with the turn's
close, not a conversation read: a result is only ever served to the turn it
belongs to, and a turn nobody could identify is served none. A turn that failed
or was interrupted prints **nothing** on stdout and a short status-shaped report
on stderr — outcome, reason, the trigger excerpt, and the injected text when
somebody steered — because a previous or partial turn's message presented as this
one's answer is worse than silence, and a bare non-zero exit is worse than an
account. Read what exists with
`gmux agent status <id>`, or the answer alone with
`gmux agent logs --agent -n 1 <id>` — both are snapshot reads of the transcript,
and the latter is where a truncated answer's full text lives.

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
gmux wait "$id" && gmux agent status "$id"

# synchronous: the id, then the answer
gmux agent prompt --new --model anthropic/sonnet 'summarize this repo'
```

`--new` launches a **new pi session** the same way `gmux -d -- pi` does — from
this shell's env and cwd, on the local daemon only — and sends the prompt as
its first turn. Pass either a session id or `--new`, never both.

- `--model M` / `--name N` — pi's `--model` and `--name`. Valid **only** with
  `--new`; without it they are usage errors.
- `--follow-up` and `--steer` are **refused** with `--new`: a session that does
  not exist yet has no turn to queue behind or steer.
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
  stdout.** A successful synchronous run prints the id, then the answer; a
  failed one prints the id and exits non-zero. This differs from a plain sync
  prompt, where stdout is the answer alone.
- With `--no-wait`, the bare id is the only output and exit `0` means the turn
  was **admitted**: the id prints immediately, but the process returns only once
  the agent has started the turn (or the admission window expires). On a sick
  session `id=$(gmux agent prompt --new --no-wait …)` can therefore block up to
  that window instead of returning at delivery — exit `0` buying the stronger
  claim is the point.

The first turn of a freshly launched session prints its answer like any other.
(It did not always: while a wait's answer was reconstructed from the transcript,
the first turn of a fresh session completed with no inline answer because the
conversation file did not exist yet when the boundary was set. The agent now
asserts its own result, so there is no such window.)

If anything fails **after** the launch — admission, readiness, the turn itself —
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

Interrupt the turn an agent is running.

```bash
gmux agent cancel a3f20187

# ...and wait for it to stop. `wait` exits 2 for an interrupted turn — which is
# exactly what you asked for — so do not chain it with `&&`.
gmux agent cancel a3f20187
gmux wait a3f20187; [ $? = 2 ] && echo stopped
```

Returns once the interrupt is **delivered**, not once the agent has stopped —
otherwise a wedged agent would hang the command. Follow with `gmux wait` when
the next step needs the turn to be over. Requires a live, active session.

**Two pi-specific caveats.** Cancelling is not a clean stop: pi's interrupt
handler *restores any queued follow-ups into the composer*, so after a cancel the
composer may hold text nobody retyped — and the next `gmux agent prompt` submits
that text along with the new prompt, as one turn. Check with `gmux agent logs` if a
session has queued follow-ups you did not see run. And `--follow-up` and `cancel`
ride pi's *default* keybindings (alt+enter, escape); a session whose user remapped
those loses both silently — the bytes still arrive, they just no longer mean that
action. Plain prompts (Enter) are unaffected.

### `gmux agent logs <id>`

Print the exact conversation text you want, as clean markdown. For
agents that persist a structured conversation file (pi), the transcript is
reconstructed from that file — the actual user/assistant messages plus compact
one-line tool calls — rather than the terminal rendering of the TUI, so the
output is readable and pipe-friendly. Thinking blocks and tool outputs are
omitted.

```bash
gmux agent logs a3f20187              # last 100 messages (user + agent prose)
gmux agent logs -n 2 a3f20187         # the prompt and the reply
gmux agent logs --agent -n 1 a3f20187 # the latest answer, alone
gmux agent logs --tool -n 20 a3f20187 # what it has been running
gmux agent logs --all a3f20187        # every rendered message type
gmux agent logs --json --user a3f20187
```

**Message-type filters.** With no flag you get the conversation without the
machinery: user messages and assistant messages that actually said something
(tool-call-only messages and tool output are excluded). Explicit type flags
**replace** that default set and compose with each other:

| Flag | Shows |
| --- | --- |
| `--user` | user messages (prompts, steers, merged follow-ups) |
| `--agent` | assistant messages that carry prose |
| `--tool` | messages that are only tool calls |
| `--all` | every type gmux renders |

There is no `--thinking`: no adapter renders thinking blocks today (pi's own
transcript drops them), so the flag is refused by name rather than answering
with silence — `gmux tail <id>` shows whatever the TUI painted.

`--json` prints a JSON array of `{role, type, text, prose}` on stdout. That
array is the machine contract for this read; `text` is the message as rendered
(tool one-liners included) and `prose` is only what the agent said.

`-n` counts **messages** (default 100), not lines — that is the unit difference
that earned this view its own command — and it counts them **after** the type
filter, so `--tool -n 1` is the last tool call rather than "the last message, if
it happens to be one". The transcript is read from the
conversation file the agent has flushed to disk, so an assistant message that is
still streaming appears once it completes.

Read from the agent's own stored conversation, so like `gmux agent status` it
never starts, resumes or touches a process, and it works on a dead retained
session. Local sessions only. Exits `1` with a stable code when there is nothing
to print: `no_message` (nothing of the requested types), `no_conversation`
(nothing rendered yet) or `unsupported_adapter` (this agent has no conversation
gmux can read) — use `gmux tail <id>` for those, which shows the terminal itself.

`gmux agent logs --agent -n 1 <id>` approximates the old `gmux agent output`. It
is a **snapshot** read, so it can be staler than the answer a `gmux wait` or a
synchronous `gmux agent prompt` carries: those report what the adapter asserted
at turn close, while this reads the conversation the agent has flushed to disk.

### `gmux agent status <id>`

Show what matters about an agent session right now — the verb for "I don't know
what I want, show me what matters". Always the same three parts, whatever the
session is doing:

```bash
gmux agent status a3f20187
gmux agent status --json a3f20187
```

```
## State

a3f20187 pi — alive, idle; last turn completed 2m ago

## Triggered by

fix the failing test in internal/store

## Answer

Fixed: the fixture wrote the row before the migration ran.
```

- **State** — short id and adapter, alive/dead, active/idle, and the last
  *closed* turn's outcome (`completed`, `interrupted`, `error`) with a rough
  recency. A turn that is still running gets **no** outcome, in the report and
  in `--json` alike: it has not concluded, and the flags describe the previous
  turn. A session that **died mid-turn** reads `the turn never finished (runner
  died)` — the same verdict `gmux wait` reaches for that row (`error`, cause
  `runner_died`), never `completed`.
- **Triggered by** — the first lines of the user message that started the
  current (or last) turn, labeled so it can never be read as the answer. It is
  the newest user message in the whole conversation, so a turn with hundreds of
  tool calls cannot crowd it out. When gmux cannot read the conversation at all,
  this part carries the daemon's reason instead of claiming nothing was asked.
- **Answer** — the agent's final message when the session is idle. While a turn
  is running this becomes **Recent**: the last few messages plus a working
  indicator, because the previous turn's answer is not this turn's.

A **snapshot**: store-only, no runner contact, no resume, so it works on a dead
retained session — and it can be staler than the answer a `wait` or a
synchronous prompt carries. The State part is adapter-independent, so a session
whose conversation gmux cannot read (Claude, Codex, a shell) still gets it, with
a note instead of the content rather than an error. Local sessions only.

`--json` prints one object mirroring the three parts:
`{state: {...}, trigger: {text, truncated}, content: {kind, ...}}`, where
`content.kind` is `"answer"`, `"recent"` or `"none"`. Fields that describe a
turn which does not exist are **absent** rather than empty:
`state.last_turn_outcome` (and `state.last_turn_cause`, today only
`runner_died`) appear only for a concluded turn, so `state.turn_recorded` and
`state.active` are what a script branches on. `trigger.code` appears only when
the conversation could not be read. That object is the machine contract for this
read. For the answer and nothing else, use `gmux agent logs --agent -n 1 <id>`.

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
and `gmux agent prompt|cancel|output --help` (or `gmux help agent …`) prints
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
