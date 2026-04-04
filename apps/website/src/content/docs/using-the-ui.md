---
title: Using the UI
description: What you see in gmux and how to work with it.
---

Open **[localhost:8790](http://localhost:8790)** after launching your first session. This page explains what you're looking at.

## URL routing

Every session has a stable URL:

```
http://localhost:8790/<project>/<adapter>/<slug>
```

For example, `/gmux/pi/fix-auth-bug` links directly to a Pi session in the gmux project. These URLs are bookmarkable, shareable, and stable across session resume. External tools (notifications, CI, scripts) can link directly to a specific session.

The project segment is the project slug (shown in the sidebar header). The adapter segment is the session kind (`pi`, `shell`, `claude`). The slug is an adapter-provided identifier for the specific session.

## The sidebar

The left panel lists your sessions grouped into projects.

### Projects

Projects group sessions by repository. When you first launch a session, gmuxd discovers it and offers to add it as a project. Click **Add project** in the empty state, or use the **Manage projects** button at the bottom of the sidebar.

Two clones of the same repo (different paths, different machines) are grouped under one project as long as they share a git remote URL. Projects without remotes match by filesystem path, with the longest prefix winning.

### Session indicators

Each session has a dot on the left edge:

| Indicator | Meaning |
|-----------|---------|
| **Pulsing ring** | The tool is actively working (building, thinking, running tests) |
| **Blue dot** | New output you haven't seen yet (viewing the session clears it) |
| **Muted ring** (brief) | Transient terminal activity in a shell session, fades after a few seconds |
| **No dot** | Idle or waiting for input |

Agent sessions (Pi, Claude, Codex) only trigger the blue unread dot when the assistant completes a turn, not on every line of output.

### Session states

| Visual | State | What you can do |
|--------|-------|-----------------|
| Normal text | Running | Click to attach, use the terminal |
| Dimmed text | Exited (not resumable) | Dismiss with × |
| Normal text, not alive | Resumable | Click to resume |

### Close button

Hover over a session to reveal the **×** button:

- **Live sessions** — kills the process. If the adapter supports resume, the session moves to the "Resume previous" drawer.
- **Resumable sessions** — dismisses the entry (it can still be found in the drawer until gmuxd restarts).

### Resuming sessions

Below the live sessions, a **"Resume previous"** button expands to show resumable sessions from previous runs. Click one to resume; the drawer collapses and the session appears as a live entry.

## The terminal

Click a session to attach. You get a full interactive terminal powered by [xterm.js](https://xtermjs.org/). Colors, cursor positioning, mouse support, and images all work.

### Header bar

Above the terminal, the header shows:

- **Session title** — extracted from the tool (Pi's first message, shell's window title)
- **Status label** — adapter-reported state like "working" or "completed"
- **Working indicator** — pulsing ring when the tool is busy

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

Click the **+** button on a project header to launch a new session in that directory. A dropdown shows the available launchers (e.g. "Pi", "Shell"). The default launcher runs on click; others appear in the dropdown.

The empty state (when no session is selected) also shows launch buttons.

## Keyboard shortcuts

gmux ships a complete default keymap. Every shortcut is explicit; nothing relies on implicit browser or xterm.js passthrough. Keys not listed here go straight to the terminal.

### All platforms

| Shortcut | Action |
|----------|--------|
| **Shift+Enter** | Sends a plain newline (`\n`) instead of Enter |
| **Ctrl+C** | If text is selected: copy to clipboard. Otherwise: sends SIGINT |

### Linux / Windows

| Shortcut | Action |
|----------|--------|
| **Ctrl+Shift+C** | Copy selection to clipboard |
| **Ctrl+V** | Paste from clipboard |
| **Ctrl+Shift+V** | Paste from clipboard |
| **Ctrl+Alt+T** | Sends Ctrl+T (transpose-chars; browser steals Ctrl+T) |
| **Ctrl+Alt+N** | Sends Ctrl+N (next-history; browser steals Ctrl+N) |
| **Ctrl+Alt+W** | Sends Ctrl+W (backward-kill-word; browser steals Ctrl+W) |

### Mac

| Shortcut | Action |
|----------|--------|
| **Cmd+C** | Copy selection to clipboard |
| **Cmd+V** | Paste from clipboard |
| **Cmd+A** | Select all terminal content |
| **Cmd+Left** | Home (beginning of line) |
| **Cmd+Right** | End (end of line) |
| **Cmd+Backspace** | Delete to start of line (sends Ctrl+U) |
| **Cmd+K** | Clear screen (sends Ctrl+L) |

:::note[macCommandIsCtrl]
If you prefer every Cmd+character to send its Ctrl equivalent (Cmd+A = beginning of line, Cmd+K = kill to end, Cmd+R = reverse search, etc.), set `macCommandIsCtrl` in your keybinds config. See [settings.jsonc reference](/reference/settings/#maccommandisctrl).
:::

### Why explicit bindings?

Browsers sit between the keyboard and the terminal, and different platforms have different conventions:

- **Ctrl+C** could mean copy (browser) or SIGINT (terminal). gmux checks for a selection first.
- **Ctrl+V** on Linux: without interception, xterm.js sends `\x16` (quoted-insert) to the terminal *before* the browser fires the paste event. gmux intercepts the keydown to avoid this.
- **Cmd+C/V** on Mac: without interception, these take a three-hop path through the browser's clipboard DOM events into xterm.js. gmux handles them directly.
- **Ctrl+Shift+C/V**: browsers do NOT map these to copy/paste. They must be explicit keybinds.

This is why gmux owns the full keymap rather than relying on passthrough.

### Customizing keybinds

All defaults can be overridden or disabled via the `keybinds` array in `~/.config/gmux/settings.jsonc`. See [settings.jsonc reference](/reference/settings/#keybinds-guide) for the full reference, actions list, and starter templates.

:::tip[App mode]
Browsers reserve many shortcuts (Ctrl+T, Ctrl+N, Ctrl+W, etc.) that don't reach the terminal. Run gmux as a standalone app to get full keyboard pass-through:

```bash
google-chrome --app=http://localhost:8790
```

Or install it as a PWA from the browser menu (⋮ → Install gmux).
:::

## Mobile

Open the same URL on your phone (or via [remote access](/remote-access) on another device). The UI adapts to touch devices:

- The sidebar slides in from the left; tap the menu button (☰) to show it
- A bottom bar provides essential keys that phones don't have:

| Button | Sends |
|--------|-------|
| **esc** | Escape |
| **tab** | Tab |
| **ctrl** | Arms Ctrl for the next key you type (tap ctrl, then tap c = Ctrl+C) |
| **↑ ↓** | Arrow keys |
| **↵** | Enter (separate from the keyboard's Return, which inserts a newline) |

The ctrl button highlights when armed and disarms after the next keypress or after a timeout. The mobile toolbar appears based on touch capability, not screen width, so tablets in landscape get touch controls while desktop users with narrow windows keep the sidebar.

## Self-reporting status

Any process can update its own sidebar entry without a custom adapter. gmux sets `$GMUX_SOCKET` in the child's environment:

```bash
# Show "building" with a working indicator
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
