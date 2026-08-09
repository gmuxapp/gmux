---
name: gmux
description: Run commands and orchestrate AI agents in observable gmux sessions. Use for builds, servers, long-running commands, subagents, parallel delegation, interaction, waiting, or inspecting session output.
---

A gmux session is a durable, browser-visible process. It may run an ordinary command or an AI agent; both share the same IDs and observation commands. Details: `gmux --help` and `gmux <verb> --help`.

## Command sessions

Add `gmux --` before a command to make it observable:

```bash
gmux -- pnpm build | tail                 # foreground; full output stays visible in gmux
id=$(gmux -d -- pnpm dev)                 # detached; capture the session ID
gmux wait "$id" --for-text 'listening on' # readiness gate
gmux send "$id" C-c                       # interact (with a shell or TUI for example)
gmux kill "$id"                           # if no longer needed
```

For long builds and tests, `gmux -- <command> | tail` keeps the complete stream in gmux while returning only the useful final lines to your context. Foreground commands compose normally: `if gmux -- pytest; then ...`.

## Agent sessions

Launch agents with `gmux agent prompt --new`; the returned ID names an ordinary gmux session:

```bash
id=$(gmux agent prompt --new --no-wait 'Review the auth module')
gmux agent prompt "$id" 'Add tests for the race you found'
gmux agent logs "$id" -n 2
gmux wait "$id"
```

Use `--follow-up` to queue instructions after the current turn or `--steer` to redirect active work. Prefer a fresh isolated workspace for each agent that may inspect or modify code, including reviewers running probes.

### Safe multiline prompts

Pass nontrivial prompts on stdin using a **quoted heredoc**. The quoted delimiter keeps backticks, `$variables`, `$(commands)`, quotes, and other shell syntax literal without a temporary file:

```bash
gmux agent prompt --new --no-wait \
  --model openai-codex/gpt-5.6-sol:low \
  --name auth-review <<'PROMPT'
Review the `auth` package.

Check $(git status) before you begin. Work alone; do not spawn agents.
PROMPT
```

The same pattern works for an existing session:

```bash
gmux agent prompt --no-wait --steer a1b2c3d4 <<'PROMPT'
Focus on the race in `consumeResult`; do not change the public API.
PROMPT
```

Short static prompts may remain single-quoted positional arguments. Prompt text can also come from a file: `gmux agent prompt a1b2c3d4 < /tmp/instructions.md`.

## Waiting and observation

```bash
gmux wait "$id"                         # wait for process exit or agent turn settlement
gmux wait "$id1" "$id2"                 # observe several sessions concurrently
gmux wait "$id" --for-regex 'ready|ok'  # output predicate; one session only
gmux tail "$id" -n 50                   # terminal output
gmux agent logs "$id" -n 2              # structured agent exchanges
gmux ls                                 # discover sessions
```

A multi-session prints reports in argument order after every session settles. Always use `gmux wait` rather than sleeping or polling files. Interrupting a wait or synchronous launch leaves the observed session running.

Timeouts bound observation, not execution:

```bash
gmux wait "$id" --timeout 250
```

This returns after at most 250 seconds if the session has not settled, while the session keeps running. Use it when you want the completion report but need control back to inspect or intervene if the work takes too long.

## Interaction notes

- `gmux send "$id" 'text' Enter` types into a shell or TUI; hotkeys use names such as `C-c`.
- Agent instructions use `gmux agent prompt`, not terminal keystrokes.
- Interrupting a synchronous prompt or wait does not stop the agent. Resume observation with another `gmux wait`, or explicitly cancel/kill it.
- Servers and watchers should be launched detached, gated with `--for-text` or `--for-regex`, and killed when no longer needed.
