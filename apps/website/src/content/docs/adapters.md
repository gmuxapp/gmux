---
title: Adapters
description: How gmux understands different tools.
---

Adapters teach gmux how to interpret specific tools. When you launch a session, gmux automatically detects what you're running and applies the right adapter.

## What adapters do

An adapter watches the terminal output of your process and reports structured status to the sidebar:

- **Working** — the tool is busy (cyan pulsing dot)
- **Idle** — the tool is waiting (no dot)

Without an adapter, gmux still tracks whether the process is alive — but with one, you get meaningful at-a-glance status like "thinking" or "42/50 passed".

## Automatic detection

You don't configure adapters. gmux recognizes tools by their command name:

```bash
gmux claude        # → claude adapter
gmux codex         # → codex adapter
gmux pi            # → pi adapter
gmux bash          # → shell adapter (fallback)
gmux make build    # → shell adapter (fallback)
```

If no specific adapter matches, the **shell** adapter handles it.

## Built-in adapters

### Shell (default)

Always active. Tracks terminal title changes so your shell's `PS1` or tool-set window titles appear in the sidebar.

### Claude Code

Active when `claude` is installed. Provides:

- Live status detection (working while assistant responds, idle when done)
- Session titles from conversation files or Claude's auto-generated titles
- Resumable sessions — exited sessions stay in the sidebar, click to resume via `claude --resume`

See [Claude Code integration](/integrations/claude-code) for details.

### Codex

Active when `codex` is installed. Provides:

- Live status detection (working while agent responds, idle on task completion)
- Session titles from your first prompt (system context filtered out)
- Resumable sessions — exited sessions stay in the sidebar, click to resume via `codex resume`

See [Codex integration](/integrations/codex) for details.

### pi

Active when `pi` is installed. Provides:

- Live status detection (thinking, waiting for input, etc.)
- Session titles from conversation files
- Resumable sessions — exited sessions stay in the sidebar, click to resume

See [pi integration](/integrations/pi) for details.

## Self-reporting

Any process can report its own status without a custom adapter. `gmux` sets `$GMUX_SOCKET` in the child's environment:

```bash
curl -X PUT --unix-socket "$GMUX_SOCKET" \
  http://localhost/status \
  -H 'Content-Type: application/json' \
  -d '{"label": "building", "working": true}'
```

## Writing an adapter

Adapters are Go files in `packages/adapter/adapters/`. See [Writing an Adapter](/develop/writing-adapters) for the recipe, or [Adapter Architecture](/develop/adapter-architecture) for the runtime model.
