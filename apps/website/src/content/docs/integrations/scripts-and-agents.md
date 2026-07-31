---
title: Scripts and agents
description: Drive gmux sessions from shell scripts, CI, and agent harnesses.
sidebar:
  order: 0
---

`gmux` is designed so the same binary works whether you attach to a session by hand or drive sessions from a script. This page covers the scripted shape for **raw sessions**: running commands non-interactively, sending input, waiting for output, and reading it back. For launching and driving AI agents semantically, see [Orchestrating agents](/orchestrating-agents/).

:::tip[Driving gmux from an agent?]
Install the gmux skills — [gmux](https://github.com/gmuxapp/gmux/blob/main/skills/gmux/SKILL.md) for running commands through sessions and [gmux-agent](https://github.com/gmuxapp/gmux/blob/main/skills/gmux-agent/SKILL.md) for orchestrating agents — so your agent picks up these patterns automatically:

```sh
npx skills add gmuxapp/gmux
```

The skills follow the [agentskills.io](https://agentskills.io/) standard and work with Claude Code, Codex, Cursor, Copilot, Gemini CLI, OpenCode, and 50+ other agents. Or drop the `SKILL.md` files into your agent's skills directory by hand if you prefer not to install the CLI.
:::

## The piped flow

The most useful primitive for scripting is `gmux -- <cmd>` with stdin redirected away from a terminal:

```bash
gmux -- make build < /dev/null
gmux -- pi -p "summarize this PR" < /dev/null
```

Running a command always uses the explicit `--` separator — there is no bare `gmux <cmd>` shorthand. The `-p` (print) flag tells `pi` to process the prompt and exit instead of staying interactive; other agents have similar one-shot modes (`claude -p`, `codex exec`). Without one, the agent stays running and the call blocks indefinitely — for multi-turn work, [orchestrate it as an agent session](/orchestrating-agents/) instead.

When stdin is not a TTY, `gmux -- <cmd>`:

- **Blocks** until the child exits.
- **Streams the child's output to stdout** — ANSI escapes stripped and CRLF normalised to LF, so `gmux -- make build | tail` reads the build's own last lines. The full escape stream still reaches the UI and scrollback.
- **Prints the session id on stderr** as soon as the child and PTY exist, so a watcher can attach or `gmux tail` the session while it runs without disturbing stdout. A fast child may write some or all of its stdout before the id appears on stderr.
- **Exits with the child's exit code**, so `gmux -- make build < /dev/null && deploy.sh` works.
- **Keeps the session in the UI** for the duration: a human can watch it live in the browser without affecting the script.

This is the shape every other line on this page builds on. It works the same in CI, cron jobs, and agent harnesses (whose stdin is a pipe by default).

One exception: recognized one-shot adapter subcommands (e.g. `pi update`, `pi --version`) are exec'd directly and never create a session.

## Spawning detached

To start a session and drive it later, spawn it detached with `-d`. It returns immediately and prints the session id:

```bash
id=$(gmux -d -- pi "build the feature")
```

Capture that id and pass it to `send`, `wait`, `tail`, and `kill`.

## Prompting an agent

For **agent sessions** (pi today), `gmux agent prompt` is the verb to script against: it waits until the agent can accept input, submits the way that agent expects, and prints an exchange report when the work settles. Launching, prompting, steering, reading conversations, and parallel fan-out are covered in [Orchestrating agents](/orchestrating-agents/).

## Sending input

`gmux send <id> [text] [keys]` pushes input into a running session, as if typed at the keyboard. Text is sent literally; trailing key names (`Enter`, `C-c`, …) are sent as keys. **Submission is explicit** — add a trailing `Enter` to dispatch a line:

```bash
gmux send <id> 'shorter inline message' Enter
gmux send <id> Enter < prompt.txt          # pipe a file, then submit
gmux send <id> C-c                          # interrupt, no Enter
```

When no text argument is given and stdin is a pipe, gmux reads stdin until EOF (capped at 1 MiB). `send` is gated by the daemon's owner-only Unix socket; see the [CLI reference](/reference/cli/) for the access-control story. For drop-in tmux compatibility, `gmux send-keys -t <id> [-l] <keys...>` is also supported.

## Waiting

`gmux wait <id>` blocks until a session's current activity settles — a shell finishing its command, a one-shot process exiting, an agent settling its work. For raw sessions, `send --wait` bundles the send and the wait:

```bash
gmux send --wait <id> Enter < step-2.txt # type, submit, wait
gmux wait --quiet "$id"                  # synchronize only
```

The signal is the same `Status.Active` flag the UI's spinner consumes. Exit codes: `0` the activity completed (or the output condition matched), `2` it was intentionally interrupted, `1` anything else — a failed activity or `--timeout N` elapsing — and `128+N` when a first local signal stopped the wait itself (the session keeps running). A non-quiet bare `wait` always writes report-format output: renderer-capable agent sessions get the full exchange report (see [Orchestrating agents](/orchestrating-agents/)), while shell sessions and agents without rendered history get minimal markers such as `[No exchanges yet]` and any applicable outcome marker. Output-condition waits are synchronization-only and print no report; use `--quiet` to suppress bare-wait output.

**A failed command fails its wait — for lifetime turns only.** For sessions whose turn is their whole lifetime (`gmux -d -- make build`, shells *without* OSC 133 integration) a non-zero child exit closes the turn with an error, so `gmux wait` exits `1`. That is deliberate — `gmux wait "$id" && deploy.sh` must not deploy a failed build — but it means "did it finish?" and "did it succeed?" are the same question here. If you only need the former, run it in the foreground (`gmux -- make build`, which propagates the child's code) or read `exit_code` from `gmux ls --json`.

**Per-command turns carry no result.** In a shell whose integration emits OSC 133 prompt marks (fish does by default) each command is its own turn, and gmux consumes the marks only as busy/idle transitions — the exit code in the `D` mark is deliberately dropped. `gmux wait` therefore exits `0` when the prompt returns, pass or fail, so `gmux wait "$shell_id" && deploy.sh` **does** deploy after a failed build. Run the command as its own session if you need its verdict.

**Prefer `send --wait` over `send` then `wait`.** The two-command form is subtly racy: `wait`'s opening snapshot can catch the *previous* turn's idle state before the send-induced `Active=true` has propagated, returning immediately. `gmux send --wait <id> … Enter` subscribes before delivering the input, so it always gates on a fresh turn. Bound it with `--timeout N`; exit codes match `wait` — `send --wait` reports the same turn conclusion (including `2` for an interrupted turn) but prints no agent result. `send --wait` shares the 0/1/2 verdict, but not the first-signal 128+N re-arm behavior: a local signal triggers ordinary process handling, not the `[Wait interrupted; agent remains active]` report that `gmux wait` installs. A standalone `gmux wait <id>` is still the right tool when you're waiting on a turn you didn't just trigger.

**Waiting on output.** `gmux wait <id> --for-text 'DONE'` (or `--for-regex 'error: \d+'`) blocks until text appears in the session's output instead of on the idle signal. The match runs server-side against gmuxd's scrollback (per rendered line), and — unlike the idle wait — works for **shell** sessions too:

```bash
gmux wait <id> --for-text 'Listening on' --timeout 30   # wait for a server to come up
```

Every session is waitable: agents get their idle signal from turn hooks, shells with OSC 133 prompt marks (fish out of the box) get per-command idle, and everything else — one-shot commands, shells without integration — is one lifetime-long turn that closes when the process exits. Careful with the last kind: an interactive shell without integration never exits on its own, so bound that wait with `--timeout` or an output condition. `wait` only works for sessions on the local host — peer sessions (`<id>@<peer>`) are rejected. To wait for a shell *command* to finish, running it as its own session through the blocking piped flow above (`gmux -- make build < /dev/null`) is still the simplest shape.

## Reading output

```bash
gmux tail <id> -n 50       # the raw screen: terminal text, any session
```

`gmux tail <id>` is always the raw view: the last `-n N` **lines** of rendered terminal output as plain text (default 100), for shells, one-shot commands and agents alike. For agent sessions there is also `gmux agent logs`, which renders the stored conversation instead of the screen — see [Orchestrating agents](/orchestrating-agents/).

## Discovery and cleanup

```bash
gmux ls            # all local sessions, alive first, newest first
gmux ls --all      # include sessions on paired peer hosts (ids print as <id>@<peer>)
gmux ls --json     # machine-readable, for parsing in scripts
gmux kill <id>     # SIGTERM the runner, normal exit lifecycle
```

When parsing JSON, use `.ref` as the session argument — never `.id`. It is the
authoritative address (`id` locally, `id@peer` remotely), so copying a peer row
cannot accidentally target a same-named local session:

```bash
ref=$(gmux ls --all --json | jq -r 'map(select(.alive))[0].ref')
gmux tail "$ref"
```

Rows are alive-first, newest-first. Even so, **`alive` is runner liveness only**:
it says nothing about activity/idle state, success, health, resumability, or
semantic support. Synchronize with `wait`, inspect a known `exit_code`, and
trust each action's stable result/error instead. Optional keys are absent rather
than `null` (an absent `exit_code` is unknown), peers may omit newer optional
keys, and scripts must ignore unknown keys added during 2.x. `command` is argv,
not shell text; timestamps are RFC 3339 and may contain fractional seconds.

`send`, `tail`, and `kill` accept `<id>@<peer>` refs; `wait` and `gmux agent`
(including `agent logs`) reject peer refs explicitly for now (run them on the
owning host instead). Passing `.ref` is still the safe rule for every verb: it
preserves owner scope instead of silently falling back to local.

Every verb also accepts id prefixes, full session ids, or slugs. For human
output, the full address printed in the ID column passes straight back to
`kill`, `send`, `tail`, or `wait`.

## Nested gmux

When `gmux -- <cmd>` runs inside an existing gmux session (detected via the `GMUX=1` env var), gmux auto-detaches into a headless background process so you don't get a PTY-within-PTY. The auto-detach only triggers when stdin is a TTY: agent harnesses whose stdin is a pipe land in the piped flow above and behave normally. You don't need to special-case nested invocations.

## Editor sessions as $EDITOR

`gmux edit <file>` opens a managed editor session, blocks until the editor closes, and propagates its exit code — so it works anywhere `$EDITOR` does (`git commit`, `crontab -e`). Inside gmux sessions, `EDITOR`/`VISUAL` already default to `gmux edit` when your dotfiles don't set them, so an agent asking git to open an editor gets a proper tab in the UI.

## Agent-specific integrations

Each adapter has its own status and resumption story. See:

- [pi](/integrations/pi/)
- [Codex](/integrations/codex/)
- [Claude Code](/integrations/claude-code/)
