---
title: Using the UI
description: What you see in gmux and how to work with it.
---

Running `gmux` with no arguments opens the dashboard in a dedicated browser window. You can also navigate to **[localhost:8790](http://localhost:8790)** directly; the first time you'll need to authenticate by visiting the login URL from `gmuxd auth`.

## The sidebar

The left panel lists your sessions grouped into projects.

### Projects

Sessions don't appear in the sidebar until you add a project. The first time you open the dashboard, click **Add project** to choose which directories to track. gmuxd discovers directories that have running sessions and offers them as suggestions. Once a project is added, any session launched in that directory appears automatically.

You can manage projects at any time via the **Manage projects** button at the bottom of the sidebar. A badge shows when there are running sessions in directories that aren't part of any project yet.

Two clones of the same repo (different paths, different machines) are grouped under one project as long as they share a git remote URL. Projects without remotes match by filesystem path.

### Sessions

Each session has a dot on the left edge:

| Indicator | Meaning |
|-----------|---------|
| **Pulsing ring** | The tool is actively working (building, thinking, running tests) |
| **Blue dot** | New output you haven't seen yet (viewing the session clears it) |
| **Muted ring** (brief) | Transient terminal activity, fades after a few seconds |
| **No dot** | Idle or waiting for input |

Agent sessions (pi, Claude, Codex) only trigger the blue unread dot when the assistant completes a turn, not on every line of output.

Hover over a session to reveal the **×** button. For live sessions this kills the process; if the adapter supports resume, the session moves to the **Resume previous** drawer at the bottom of the sidebar. Exited sessions that aren't resumable can be dismissed with ×.

## The terminal

Click a session to attach. You get a full interactive terminal powered by [xterm.js](https://xtermjs.org/). Colors, cursor positioning, mouse support, and images all work. The header bar shows the session title, status label, and a working indicator.

## Launching sessions

### From the command line

```bash
gmux pi              # coding agent
gmux pytest --watch  # any command
```

### From the UI

Click the **+** button on a project header. A dropdown shows the available launchers (adapters that can start new sessions in that directory).

When no session is selected, the main area shows launch buttons for the current project.

## URL routing

Every session has a stable URL:

```
http://localhost:8790/<project>/<adapter>/<slug>
```

For example, `/gmux/pi/fix-auth-bug` links directly to a pi session in the gmux project. URLs update as you navigate, work with browser back/forward, and are bookmarkable. They remain stable across session kill and resume.

## Keyboard shortcuts

gmux ships a complete default keymap. Keys not listed here go straight to the terminal.

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
| **Ctrl+Alt+T** | Sends Ctrl+T (browser steals Ctrl+T) |
| **Ctrl+Alt+N** | Sends Ctrl+N (browser steals Ctrl+N) |
| **Ctrl+Alt+W** | Sends Ctrl+W (browser steals Ctrl+W) |

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

All defaults can be overridden or disabled in [`settings.jsonc`](/reference/settings/#keybinds-guide).

:::note[macCommandIsCtrl]
If you prefer every Cmd+character to send its Ctrl equivalent (Cmd+A = beginning of line, Cmd+K = kill to end, Cmd+R = reverse search), enable [`macCommandIsCtrl`](/reference/settings/#maccommandisctrl).
:::

:::tip[App mode]
`gmux` tries to open in Chrome/Chromium `--app` mode for a standalone window with full keyboard access. If it falls back to a regular browser tab, shortcuts like Ctrl+T, Ctrl+N, and Ctrl+W are intercepted by the browser. The Ctrl+Alt workarounds in the table above cover this case. You can also install gmux as a PWA from the browser menu (⋮ → Install gmux).
:::

## Mobile

Open the same URL on your phone (or via [remote access](/remote-access)). The sidebar slides in from the left (tap ☰), and a bottom bar provides keys that phones don't have:

| Button | Sends |
|--------|-------|
| **esc** | Escape |
| **tab** | Tab |
| **ctrl** | Arms Ctrl for the next key (tap ctrl, then c = Ctrl+C) |
| **alt** | Arms Alt for the next key |
| **← →** | Arrow keys (hold to repeat) |
| **▶** | Send (Enter) |

When **ctrl** is armed, the toolbar transforms: **esc** and **tab** become **↑** and **↓** arrow keys, **▶** (send) becomes **paste**, and **← →** switch to word-jump navigation. The toolbar returns to normal after the next keypress or when you tap ctrl again.

