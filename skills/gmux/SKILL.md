---
name: gmux
description: Drive long-running terminal commands and AI coding agents through gmux sessions. Use when the user asks to run a command in the background, send input to a running session, wait for an agent's turn to finish, orchestrate multiple agents in parallel, or capture output from a tmux/screen-style session.
---

# gmux

## Primitives

```bash
gmux --no-attach <cmd>       # spawn detached; prints the session id on stdout
gmux <cmd> < /dev/null       # spawn blocking; exits with the child's exit code
gmux --send <id> [text]      # send text (or stdin) to a session and submit
gmux --wait <id>             # block until the agent finishes its turn
gmux --tail N <id>           # last N lines of output (ANSI stripped)
gmux --list                  # all sessions
gmux --kill <id>             # SIGTERM the runner
```

`--list` IDs are 8-character prefixes; pass them directly to `--send` / `--wait` / `--tail` / `--kill`.

## Sequential orchestration

```bash
id=$(gmux --no-attach pi "implement the feature")
gmux --wait $id

gmux --send $id < review.txt
gmux --wait $id

gmux --tail 100 $id
```

## Parallel orchestration

```bash
ids=()
for ticket in fa-48 fa-49 fa-52; do
  ids+=( "$(gmux --no-attach pi "Implement $ticket. Return when done.")" )
done

for id in "${ids[@]}"; do
  gmux --wait --timeout 600 "$id" || echo "$id failed: $?"
done

for id in "${ids[@]}"; do
  echo "=== $id ==="
  gmux --tail 100 "$id"
done
```

## `--wait` exit codes

- `0` agent reached idle
- `2` session died
- `3` `--timeout` elapsed

`--wait` only works for agent sessions (`claude`, `codex`, `pi`). For shell commands use the blocking piped flow: `gmux make build < /dev/null`.

## Other agents have one-shot modes

Agents stay running by default. To make them exit after one prompt, use the agent's print mode: `pi -p`, `claude -p`, `codex exec`. Pair with the piped flow for fire-and-forget:

```bash
gmux pi -p "summarize this PR" < /dev/null
```

## Sending control characters

```bash
printf '\x03' | gmux --send --no-submit $id   # Ctrl-C without an extra Enter
```

## Reference

- <https://gmux.app/reference/cli/>
- <https://gmux.app/integrations/scripts-and-agents/>
