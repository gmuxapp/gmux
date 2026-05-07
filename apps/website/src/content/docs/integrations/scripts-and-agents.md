---
title: Scripts and agents
description: Drive gmux sessions from shell scripts, CI, and agent harnesses.
sidebar:
  order: 0
---

`gmux` was designed so that the same binary works whether you're attaching to a session by hand or driving sessions from a script. This page covers the scripted shape: how to start sessions non-interactively, how to send input and wait for output, and how to compose the primitives into agent-orchestration patterns.

:::tip[Driving gmux from an agent?]
Install the [gmux skill](https://github.com/gmuxapp/gmux/blob/main/skills/gmux/SKILL.md) so your agent picks up these patterns automatically:

```sh
npx skills add gmuxapp/gmux
```

The skill follows the [agentskills.io](https://agentskills.io/) standard and works with Claude Code, Codex, Cursor, Copilot, Gemini CLI, OpenCode, and 50+ other agents. Or drop the `SKILL.md` into your agent's skills directory by hand if you prefer not to install the CLI.
:::

## The piped flow

The most useful primitive for scripting is `gmux <cmd>` with stdin redirected away from a terminal:

```bash
gmux make build < /dev/null
gmux pi -p "summarize this PR" < /dev/null
```

The `-p` (print) flag tells `pi` to process the prompt and exit instead of staying interactive. Other agents have similar one-shot modes (`claude -p`, `codex exec`); without one, the agent stays running and `gmux <cmd>` blocks indefinitely. For multi-turn orchestration, see the [parallel orchestration](#parallel-orchestration) section below: spawn with `--no-attach`, then drive with `--send` / `--wait`.

When stdin is not a TTY, `gmux <cmd>`:

- **Blocks** until the child exits.
- **Streams bounded metadata** to stdout (session id, kind, exit status), not the full PTY output. Your script's logs stay readable.
- **Exits with the child's exit code**, so `gmux make build < /dev/null && deploy.sh` works.
- **Keeps the session in the UI** for the duration: a human can watch it live in the browser without affecting the script.

This is the shape every other line on this page builds on. It works the same in CI, in cron jobs, in agent harnesses (whose stdin is a pipe by default), and in any scripted invocation.

## Sending input

Use [`--send`](/reference/cli/#gmux---send---no-submit-id-text) to push input into an already-running session, as if the bytes had been typed at the keyboard. By default `--send` submits the input (appends the carriage return that signals Enter), so the canonical shape is just:

```bash
gmux --send <id> < prompt.txt
gmux --send <id> 'shorter inline message'
```

Use `--no-submit` for the rare case where you want to pre-fill the input box without dispatching, e.g. agent-assisted human authoring or sending a control character without an extra Enter:

```bash
printf '\x03' | gmux --send --no-submit <id>   # Ctrl-C, no extra Enter
gmux --send --no-submit <id> 'draft '          # leave "draft " in the input
```

`--send` is local-only and gated by Unix-socket file permissions (owner-only `0700`); see the CLI reference for the access-control story.

## Waiting for the turn to finish

`gmux --wait <id>` blocks until an agent session has finished its current turn. It's the primitive that turns sequential orchestration into a one-line-per-step pattern:

```bash
gmux --send <id> < step-1.txt
gmux --wait <id>

gmux --send <id> < step-2.txt
gmux --wait <id>

# extract the final answer
gmux --tail 200 <id>
```

The idle signal is the same `Status.Working` flag the UI's spinner consumes, so `--wait` returns the moment the agent emits its closing message for the turn. If the session dies first, `--wait` exits 2; if you set `--timeout N` and N seconds pass, it exits 3. Idle is exit 0.

`--wait` is for agent sessions (`claude`, `codex`, `pi`); shell sessions don't emit a working signal and are rejected with a clear error. To wait for a shell command to finish, run it through the piped flow above instead — the blocking shape is exactly what `gmux make build < /dev/null` already provides.

## Reading output

`gmux --tail N <id>` dumps the last N lines of a session's output as plain text (ANSI stripped). Pair it with `--wait` to capture the agent's final answer:

```bash
gmux --send <id> < ship-prompt.txt
gmux --wait --timeout 600 <id>
url=$(gmux --tail 50 <id> | grep -oE 'https://github\.com/[^ ]+/pull/[0-9]+' | tail -1)
echo "$url"
```

`--tail` is local-only today.

## Discovery and cleanup

```bash
gmux --list                  # all sessions, alive first, newest first
gmux --kill <id>             # SIGTERM the runner, normal exit lifecycle
```

`--list` accepts ID prefixes, full session IDs, or slugs anywhere a session is named, so the eight-character short form it prints can be passed straight back to `--kill`, `--send`, `--tail`, or `--wait`.

## Parallel orchestration

Spawn N agents in parallel, then wait for each in turn. Sequential waiting finishes when the slowest agent finishes — same wall-clock as backgrounding the `--wait` calls, but exit codes are per-session and the loop reads as a straight line:

```bash
ids=()
for ticket in fa-48 fa-49 fa-52; do
  prompt="Implement $ticket. Return when you're done."
  ids+=( "$(gmux --no-attach pi "$prompt")" )
done

for id in "${ids[@]}"; do
  gmux --wait --timeout 600 "$id" || echo "$id did not finish cleanly: $?"
done

for id in "${ids[@]}"; do
  echo "=== $id ==="
  gmux --tail 100 "$id"
done
```

The agents run concurrently because `gmux --no-attach pi <prompt>` returns as soon as the session registers (and `--no-attach` prints just the session id, no grep needed); the wait loop just gates the harvest step on every agent reaching idle. Background `&` + `wait` is only useful when you want to dispatch the next step as soon as **any** agent finishes (rare for orchestration, where you usually want all of them done before you act).

## Nested gmux

When `gmux <cmd>` is run inside an existing gmux session (detected via the `GMUX=1` env var), gmux auto-detaches into a headless background process so you don't end up with a PTY-within-PTY. Importantly, the auto-detach only triggers when stdin is a TTY: agent harnesses whose stdin is a pipe land in the piped / non-tty flow described above and behave normally, blocking with bounded output. You don't need to special-case nested invocations in your scripts.

## Agent-specific integrations

Each adapter has its own status and resumption story. See:

- [pi](/integrations/pi/)
- [Codex](/integrations/codex/)
- [Claude Code](/integrations/claude-code/)
