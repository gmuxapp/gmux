---
title: Using the UI
description: What you see in gmux and how to work with it.
---

Open **[localhost:8790](http://localhost:8790)** after launching your first session. This page explains what you're looking at.

## The sidebar

The left panel lists all sessions, grouped by working directory. Each folder shows the project path, and sessions within it are sorted by creation time.

### Session indicators

Each session has a colored dot on the left:

| Dot | Meaning |
|-----|---------|
| **Pulsing cyan** | The tool is actively working (building, thinking, running tests) |
| **Amber** | Something happened that you haven't seen yet (new output while viewing another session) |
| **No dot** | Idle or waiting for input |

The working/idle detection comes from [adapters](/adapters). Without a specific adapter, gmux only knows whether the process is alive.

### Session states

| Visual | State | What you can do |
|--------|-------|-----------------|
| Normal text | Running | Click to attach, use the terminal |
| Dimmed text | Exited (not resumable) | Dismiss with × |
| Normal text, not alive | Resumable | Click to resume |

### Close button

Hover over a session to reveal the **×** button:

- **Live sessions** — kills the process. If the adapter supports resume and a session file was attributed, the session moves to the "Resume previous" drawer.
- **Resumable sessions** — dismisses the entry (it can still be found in the drawer until gmuxd restarts).

### Resuming sessions

Below the live sessions, a **"Resume previous"** button expands to show resumable sessions from previous runs. Click one to resume — the drawer collapses and the session appears as a live entry.

## The terminal

Click a session to attach. You get a full interactive terminal powered by [xterm.js](https://xtermjs.org/) — colors, cursor positioning, mouse support, and images all work.

### Header bar

Above the terminal, the header shows:

- **Session title** — extracted from the tool (pi's first message, shell's window title)
- **Status label** — adapter-reported state like "working" or "completed"
- **Working indicator** — pulsing cyan dot when the tool is busy

## Launching sessions

There are two ways to start a new session:

### From the command line

```bash
gmux pi                    # coding agent
gmux pytest --watch     # any command
gmux make build
```

The session appears in the sidebar immediately.

### From the UI

Click the **+** button on a folder header to launch a new session in that directory. A dropdown shows the available launchers (e.g. "Pi", "Shell"). The default launcher runs on click; others appear in the dropdown.

The empty state (when no session is selected) also shows launch buttons.

## Keyboard shortcuts

Most keys pass straight through to the terminal. A few are intercepted by default, with different bindings per platform:

### All platforms

| Shortcut | Action |
|----------|--------|
| **Ctrl+C** | If text is selected: copy to clipboard. Otherwise: sends SIGINT (normal Ctrl+C) |
| **Ctrl+V** / **Cmd+V** | Paste from clipboard |
| **Shift+Enter** | Sends a plain newline (blocks Kitty keyboard protocol sequence) |

### Linux / Windows

Browsers reserve Ctrl+T, Ctrl+N, and Ctrl+W for tab management. These Ctrl+Alt workarounds send the original key to the terminal:

| Shortcut | Sends |
|----------|-------|
| **Ctrl+Alt+T** | Ctrl+T (transpose-chars in readline) |
| **Ctrl+Alt+N** | Ctrl+N (next-history in readline) |
| **Ctrl+Alt+W** | Ctrl+W (backward-kill-word in readline) |

### Mac

On Mac the browser steals Cmd+T/N/W instead, leaving Ctrl+T/N/W free. These bindings match iTerm2 and macOS Terminal conventions:

| Shortcut | Action |
|----------|--------|
| **Cmd+Left** | Home (beginning of line) |
| **Cmd+Right** | End (end of line) |
| **Cmd+Backspace** | Delete to start of line |
| **Cmd+K** | Clear screen |

### Customizing keybinds

All default keybinds can be overridden or disabled via `~/.config/gmux/keybinds.jsonc`. See [Configuration](/configuration#terminal-keybinds) for details.

:::tip[App mode]
Browsers reserve many shortcuts (Ctrl+T, Ctrl+N, Ctrl+W, etc.) that don't reach the terminal. Run gmux as a standalone app to get full keyboard pass-through:

```bash
google-chrome --app=http://localhost:8790
```

Or install it as a PWA from the browser menu (⋮ → Install gmux).
:::

## Mobile

Open the same URL on your phone (or via [remote access](/remote-access) on another device). The UI adapts to small screens:

- The sidebar slides in from the left — tap the menu button (☰) to show it
- A bottom bar provides essential keys that phones don't have:

| Button | Sends |
|--------|-------|
| **esc** | Escape |
| **tab** | Tab |
| **ctrl** | Arms Ctrl for the next key you type (tap ctrl, then tap c = Ctrl+C) |
| **↑ ↓** | Arrow keys |
| **↵** | Enter |

The ctrl button highlights when armed and disarms after the next keypress or after a timeout.

## Self-reporting status

Any process can update its own sidebar entry without a custom adapter. gmux sets `$GMUX_SOCKET` in the child's environment:

```bash
# Show "building" with a working dot
curl -X PUT --unix-socket "$GMUX_SOCKET" \
  http://localhost/status \
  -H 'Content-Type: application/json' \
  -d '{"label": "building", "working": true}'

# Clear status
curl -X PUT --unix-socket "$GMUX_SOCKET" \
  http://localhost/status \
  -H 'Content-Type: application/json' \
  -d '{"label": "", "working": false}'
```

See [Adapter Architecture](/develop/adapter-architecture) for the full child protocol.
