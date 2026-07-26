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
drives them (list, attach, tail, send, wait, kill), prompts agent sessions
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
- `--json` — emit a JSON array instead of the table, for scripts and agents (the adapter field is `"adapter"`).

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

Print the session's conversation as clean markdown. For agents that persist a
structured conversation file (pi), the transcript is reconstructed from that
file — the actual user/assistant messages plus compact one-line tool calls —
rather than the terminal rendering of the TUI, so the output is readable and
pipe-friendly. Thinking blocks and tool outputs are omitted.

Sessions without a conversation file (shells, plain commands) print recent PTY
output as plain text instead, exactly as before.

```bash
gmux tail a3f20187            # conversation markdown (last 100 messages),
                              # or last 100 lines of output for shells
gmux tail -n 5 a3f20187       # last 5 messages (or lines)
gmux tail --raw a3f20187      # force the PTY view: last N lines of terminal
                              # output, plain text (-e also works)
```

`-n` counts messages in the conversation view and lines in the PTY view. The
transcript is read from the conversation file the agent has flushed to disk,
so an assistant message that is still streaming appears once it completes.

It's a snapshot, not a stream — to watch a session live, attach to it or open
it in the browser.

### `gmux send [--wait [--timeout N]] <id> [text] [Key...]`

Inject input into a running session as if typed at the keyboard. The text is
sent literally; any trailing arguments that name keys (`Enter`, `C-c`,
`Escape`, `Up`, …) are sent as those keys. **Submission is explicit** — add a
trailing `Enter` to dispatch a line; omit it to leave the input unsent.

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

**Everything after the session id is verbatim** — including tokens that start
with a dash. `gmux send abc -v` sends the literal `-v`, and no `--` guard is
needed for dash-leading text. The trade-off is that `send`'s own flags are only
recognized *before* the id (the first non-flag token). A `--` before the id is
accepted as an explicit end-of-flags marker.

**`--wait`** fuses send-and-wait into one race-free step: deliver the input,
then block until the turn it triggers completes, with the same exit codes as
[`gmux wait`](#gmux-wait-id) (`0` idle, `2` died, `3` timeout). Bound it with
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
lifetime-long turn that closes when the process exits. Exit codes (so
scripts can branch on the outcome):

- `0` — the session reached idle (turn finished — including a one-shot
  command completing or a shell exiting at its prompt), or the output
  condition matched
- `2` — the session exited with its turn still open (crashed mid-command /
  mid-turn) / exited before its output matched
- `3` — `--timeout` elapsed

The verdict is stable across timing: a `wait` issued after the session
already exited answers the same as one that watched it live.

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
`gmux tail`.

### `gmux agent prompt [flags] <id> [prompt]`

Send a prompt to an agent session and, by default, block until the turn it
starts has ended.

```bash
gmux agent prompt a3f20187 'review the diff on this branch'
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
- `--follow-up` — queue the prompt so it submits after the current turn ends.
  On an idle agent it behaves like a plain prompt.
- `--steer` — redirect the turn that is running *right now*. Fails if the agent
  is idle or dead; steering nothing is not a thing.
- `--no-wait` — return as soon as the prompt is admitted instead of waiting for
  the turn.
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

Exit codes: `0` the turn completed, `2` the turn was **interrupted**, `3`
`--timeout` elapsed, `1` anything else (including a turn that ended in an error
and a runner that died). Note `2` differs from [`gmux wait`](#gmux-wait-id),
where it means the session died. A successful prompt is quiet — the agent's
answer is read separately with `gmux agent output`.

Failures name a stable code, and the wording distinguishes what is known about
delivery. `admission_timeout`, `delivery_timeout`, `queued_turn_unobserved` and
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
that text along with the new prompt, as one turn. Check with `gmux tail` if a
session has queued follow-ups you did not see run. And `--follow-up` and `cancel`
ride pi's *default* keybindings (alt+enter, escape); a session whose user remapped
those loses both silently — the bytes still arrive, they just no longer mean that
action. Plain prompts (Enter) are unaffected.

### `gmux agent output <id>`

Print the agent's latest message — its answer, verbatim and untruncated,
without the compact `[tool]` lines that appear in the transcript.

```bash
gmux agent prompt a3f20187 'summarize the failures' && gmux agent output a3f20187
```

Read from the agent's own stored conversation, so it never starts, resumes or
touches a process, and it works on a dead session. Exits `1` with a stable code
when there is nothing to print: `no_message` (the agent hasn't produced one
yet), `no_conversation` (no conversation on record), `unsupported_adapter`
(this agent has no conversation gmux can read). Use `gmux tail` for the whole
transcript.

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
name (it prints the full usage either way).

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
