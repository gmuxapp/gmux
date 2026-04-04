---
title: Introduction
description: What gmux is and why it exists.
---

gmux is a browser-based session manager for AI agents, test runners, and long-running processes. It gives you a live sidebar of everything running across a machine, so you can notice what needs attention without cycling through terminal tabs.

## Why it exists

Long-running command-line work is easy to start and annoying to supervise. AI agents, watchers, builds, and shells end up buried in tabs or tmux panes. gmux makes them visible from a browser.

## What it does

- Launches commands as managed sessions through `gmux`
- Groups sessions into projects in a sidebar
- Shows live status: working (pulsing ring) or unread output (blue dot)
- Provides a full interactive terminal in the browser via xterm.js
- Uses adapters to extract tool-specific status (e.g. Pi's thinking/waiting states)
- Supports resumable sessions for tools with file-backed state

## Core concepts

### Sessions

A session is any command launched through `gmux`:

```bash
gmux pi
gmux make build
gmux pytest --watch
```

Each session gets a PTY, a WebSocket server, and an adapter for status extraction.

### Projects

Sessions are grouped into projects by repository. Two clones of the same repo on different machines (or with different directory names) appear under one project heading. You choose which projects appear in the sidebar.

### Adapters

Adapters teach gmux how to interpret specific tools:

- **shell** — terminal title tracking (default fallback)
- **pi** — live status detection, file-backed titles, and session resume
- **claude** — live status, conversation titles, and resume via `claude --resume`
- **codex** — live status, prompt-based titles, and resume via `codex resume`

See [Adapters](/adapters) for details.

### Architecture

```
gmux (per session) → gmuxd (per machine) → browser
```

- **gmux** owns the child process and its live state
- **gmuxd** discovers sessions, proxies connections, and serves the web UI
- **Browser** renders the sidebar and attaches to terminals

See [Architecture](/architecture) for the full picture.

## Next steps

- [Quick Start](/quick-start) — install and run in under a minute
- [Using the UI](/using-the-ui) — what you see and how to work with it
- [Architecture](/architecture) — how the pieces fit together
- [Adapters](/adapters) — how gmux understands different tools
