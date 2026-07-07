---
title: Adapters
description: How gmux understands different tools.
---

Adapters teach gmux how to interpret specific tools. When you launch a session, gmux automatically detects what you're running and applies the right adapter.

## What adapters do

An adapter teaches gmux how to launch, title, resume, and track a tool. For agent tools (Claude Code, Codex, pi), gmux installs a small hook into the agent when it launches; the agent itself reports session state authoritatively — no output scraping. The sidebar shows:

- **Working** — the tool is busy (cyan pulsing dot)
- **Error** — the tool reported an error (red dot)
- **Idle** — the tool is waiting (no dot)

Without a specific adapter, gmux still tracks whether the process is alive — but with one, you get at-a-glance working/error state, unread markers, meaningful session titles, and resumable conversations.

Terminal titles (OSC 0/2, e.g. your shell's `PS1`) work for every session regardless of adapter.

## Automatic detection

You don't configure adapters. gmux recognizes tools by their command name:

```bash
gmux -- claude       # → claude adapter
gmux -- codex        # → codex adapter
gmux -- pi           # → pi adapter
gmux edit README.md  # → editor adapter
gmux -- bash         # → shell adapter (fallback)
gmux -- make build   # → shell adapter (fallback)
```

If no specific adapter matches, the **shell** adapter handles it. Setting `GMUX_ADAPTER=<name>` forces an adapter, but only when that adapter also matches the command — a leaked variable can't hijack an unrelated command.

## Built-in adapters

### Shell (default)

The catch-all. Titles sessions by their command (e.g. `pytest -x`), keeps them resumable (resume reopens `$SHELL` in the original directory), and picks up terminal title changes from your shell's `PS1` or tool-set window titles.

### Claude Code

Matched whenever you run `claude`; its launcher appears in the UI when `claude` is on PATH. Provides:

- Live status via Claude Code hooks (working while a turn runs, idle when done)
- Session titles reported live by the hook (follows `/rename`)
- Resumable sessions — exited sessions stay in the sidebar, click to resume via `claude --resume`

See [Claude Code integration](/integrations/claude-code) for details.

### Codex

Matched whenever you run `codex`; its launcher appears when `codex` is on PATH. Provides:

- Live status via Codex hooks (Codex CLI ≥ 0.135.0)
- Session titles from your first prompt
- Resumable sessions — exited sessions stay in the sidebar, click to resume via `codex resume`

See [Codex integration](/integrations/codex) for details.

### pi

Matched whenever you run `pi`; its launcher appears when `pi` is on PATH. Provides:

- Live status via a gmux pi extension (turn start/end, working/idle)
- Session titles from pi's session name or your first message
- Resumable sessions — exited sessions stay in the sidebar, click to resume

See [pi integration](/integrations/pi) for details.

### Editor

Backs `gmux edit [file]` — editor sessions as a first-class tab type, usable as `$EDITOR`. Editor sessions are ephemeral: they're dismissed automatically when the editor closes. Plain `gmux -- nano file` stays on the shell adapter.

## Agent hooks

For claude, codex, and pi, gmux injects a lightweight hook (a pi extension, or command hooks for claude/codex) into each launch. The agent reports which conversation it holds, turn boundaries (working/idle), titles, and slugs directly to gmux — the launch is otherwise unmodified, and nothing is written to the tools' config directories. Set `GMUX_NO_AGENT_HOOK=1` to launch the agent completely unmodified; the session then runs without hook-driven title/status/attribution.

The hook mechanism is documented in depth in [Adapter Architecture](/develop/adapter-architecture/#live-session-state-comes-from-the-agent-hook); per-tool specifics are on the [pi](/integrations/pi/), [Claude Code](/integrations/claude-code/), and [Codex](/integrations/codex/) pages.

## Self-reporting

Any process or script can report its own status without a custom adapter. `gmux` sets `$GMUX_SOCKET` in the child's environment:

```bash
curl -X PUT --unix-socket "$GMUX_SOCKET" \
  http://localhost/status \
  -H 'Content-Type: application/json' \
  -d '{"working": true}'
```

Status carries only `working` and `error` booleans — display text is derived by the UI. Send `null` to clear the status. See [Scripts & agents](/integrations/scripts-and-agents) for how this composes with `gmux wait`.

## Writing an adapter

Adapters are Go files in `packages/adapter/adapters/`. See [Writing an Adapter](/develop/writing-adapters) for the recipe, or [Adapter Architecture](/develop/adapter-architecture) for the runtime model.
