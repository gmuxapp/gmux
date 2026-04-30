---
title: Scripts and agents
description: Drive gmux from shell scripts, CI pipelines, and coding-agent harnesses.
---

gmux is built around long-running processes that someone wants to supervise loosely. That makes it a natural fit not just for interactive use but for anything that *launches* long-running processes on a user's behalf: shell scripts, CI pipelines, and coding agents (pi, Codex, Claude Code, your own harness).

The primitives are already in the CLI; this page shows how they compose.

## The piped run: `gmux <cmd> | tail`

The single most useful pattern for scripts and agents:

```bash
gmux make build | tail -n 20
```

What happens, step by step:

1. Because gmux's stdin is a pipe (not a tty), it takes the [non-tty flow](/reference/cli/#gmux---no-attach-command-args): no raw mode, no PTY passthrough to stdout.
2. gmux prints a short metadata header (`session:`, `adapter:`, `command:`, `pid:`, `socket:`, `serving...`), then **blocks** until the child exits.
3. The child's actual output goes to the gmux session: visible live in the web UI, persisted in scrollback, and available to `gmux --tail`.
4. When the child exits, gmux prints `exited: N` and exits with the same code.

The caller (your script, your agent's shell tool) sees at most ~7 lines of bounded output plus the exit code. The user watches the real work happen in the gmux UI on their phone or laptop. Nothing is lost: the scrollback is retained in the session.

This is deliberately *not* how most CLI wrappers behave. `time`, `nohup`, `env`, etc. forward the child's stdout to the caller. gmux doesn't, on the theory that you already have a much better surface for watching a long-running process (a real terminal in a browser) and what the caller actually wants is a blocking wait with a predictable exit code.

### Why this shape is useful to agents

A coding agent that shells out to `gmux pytest` or `gmux cargo build` gets:

- **Blocking**, so the agent waits for completion before reasoning about the result.
- **Bounded output**, so a 10-minute test run that prints thousands of lines doesn't blow up the agent's context window.
- **Reliable exit code**, so `if gmux <cmd>; then ...` works.
- **Live visibility for the human**, who can open the gmux UI and see exactly what the agent is doing, intervene via `--send`, or kill it with `--kill`.

## Peeking without attaching: `gmux --tail`

When you need the actual output, either for a post-hoc log line or for an agent that wants to summarize what happened, use [`gmux --tail`](/reference/cli/#gmux---tail-n-id--t):

```bash
gmux --tail 100 a3f20187     # last 100 lines of scrollback + visible screen
```

`--tail` returns plain text with ANSI stripped, suitable for piping into `grep`, `jq`, or an LLM. It works on live sessions only; once a session exits and the scrollback is garbage-collected, use `--list` to confirm state and fall back to whatever log the child itself produced.

## Listing and discovering sessions

```bash
gmux --list
```

Prints one row per session (alive first, newest first) with short IDs, status, adapter kind, title, and cwd. Most management flags accept a unique prefix of the short ID or the slug, so `gmux --tail 20 a3f2` is usually enough.

## Driving a running session: `gmux --send`

When a script or agent needs to reply to a prompt inside a running session, [`gmux --send`](/reference/cli/#gmux---send-id-text) writes bytes straight into the session's PTY, indistinguishable from real keystrokes:

```bash
gmux --send a3f2 $'yes\n'                     # answer a yes/no prompt
printf '\x03' | gmux --send a3f2              # send Ctrl-C
cat script.txt | gmux --send a3f2             # pipe multi-line input
```

Access is scoped by the session socket's filesystem permissions (owner-only, 0700). Only the user that started the session can send to it, and remote peer sessions are rejected outright.

## Fire-and-forget: `gmux --no-attach`

Piped gmux blocks. If you want to start a session and return immediately, without waiting:

```bash
id=$(gmux --no-attach pytest --watch)
```

Detaches the child from the caller entirely. `gmux` blocks just long enough for the child to register with gmuxd, prints the new session id on stdout, and exits 0. Capture it directly into a shell variable as above and you can drive the session immediately, no `--list` polling required:

```bash
gmux --tail 50 "$id"
gmux --send "$id" $'q\n'
gmux --kill "$id"
```

If registration fails (handshake timeout or the child dies before registering), `gmux` prints a short reason on stderr and exits non-zero, so `set -e` scripts fail loudly instead of capturing an empty id.

Use `--no-attach` for watchers, dev servers, or anything the script should kick off and then move on from.

## Cleaning up: `gmux --kill`

```bash
gmux --kill a3f2
```

Sends SIGHUP to the session's process group, waits up to 2 s, and escalates to SIGKILL if needed. Same signal chain as the UI's kill button.

## End-to-end example: a build-and-report script

```bash
#!/usr/bin/env bash
set -euo pipefail

# Run the build in a gmux session; caller blocks, sees only metadata.
if gmux make release 2>&1 | tail -n 10; then
  echo "build succeeded"
  exit 0
fi

# Find the session by the command that launched it, grab its scrollback,
# and surface the last failure lines without re-running anything.
sid=$(gmux --list | awk '/make release/ {print $1; exit}')
echo "build failed; last 40 lines of the session:"
gmux --tail 40 "$sid"
exit 1
```

The human running this script sees one of two short outputs, plus a live gmux UI they can open at any time during the build to watch it streaming.

## Nested gmux

When gmux is invoked from inside an existing gmux session, the behavior depends on stdin:

- **Stdin is a terminal** (you opened a shell in the gmux UI and are typing commands): gmux auto-detaches. The nested call returns immediately on stderr with `started <cmd> in background (visible in gmux)`, avoiding PTY-within-PTY nesting. The new session appears in the UI but your typed `gmux <cmd>` call does *not* block.
- **Stdin is not a terminal** (a shell script running inside a gmux session, or an agent harness that pipes stdin to its child): gmux falls through to the non-tty flow and blocks normally, the same as from a fresh shell. This is what agents and scripts want and what they get automatically.

Practically: agent harnesses and piped scripts get the right behavior without thinking about it, because their own stdin is not a tty to begin with. Only hand-typed `gmux <cmd>` inside the UI's terminal switches into the fire-and-forget shape, and there you can always just use `--list` / the UI to reach the new session.

## Agent-specific integrations

For out-of-the-box integrations with specific agents, gmux ships dedicated adapters that layer status tracking and title extraction on top of these primitives:

- [pi](/integrations/pi/)
- [Codex](/integrations/codex/)
- [Claude Code](/integrations/claude-code/)

The patterns on this page apply to any shell script or harness, including ones that don't have a dedicated adapter.
