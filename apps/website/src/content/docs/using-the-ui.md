---
title: Using the UI
description: What you see in gmux and how to work with it.
---

Running `gmux` with no arguments opens the dashboard in a dedicated browser window. You can also navigate to **[localhost:8790](http://localhost:8790)** directly; the first time you'll need to authenticate by visiting the login URL from `gmuxd auth`.

## The sidebar

The left panel lists your sessions grouped into projects.

### Logo

Click the **gmux** logo at the top of the sidebar to return to the home screen.

## Home screen

The home screen shows your hosts, projects, and quick-launch buttons.

### Host cards

Each machine (local and remote) gets a card with a status indicator, session count, and launch buttons.

| Indicator | Meaning |
|-----------|---------||
| **Green dot** | Connected, sessions visible |
| **Pulsing dot** | Connecting to peer |
| **Red ✗** | Peer disconnected (shows error reason) |
| **Dimmed card** | Offline: a tailnet device that looks like a gmux instance but is currently unreachable |

Offline cards appear for tailnet devices whose hostname matches the configured tsnet prefix (e.g. `gmux-dev`). They're informational only: no launch buttons, no session count. Once the device comes online and is confirmed as gmux, it becomes a full peer and persists across restarts.

Connected peers show launch buttons for each configured adapter, just like the local host.

### Projects

Sessions don't appear in the sidebar until you add a project. The first time you open the dashboard, click the **+** button to launch a session. gmux creates a default "home" project that catches sessions started from your home directory. As you work in more repositories, use **Manage projects** to organize sessions by repo.

Click a **project name** to open the [project hub](#project-hub), an overview of all sessions in that project grouped by host and working directory. The active project is highlighted in the sidebar.

Each project has **match rules** that determine which sessions belong to it. Rules can match by filesystem path (`~/dev/gmux` and its subdirectories) or by git remote URL (grouping clones across machines). See [`projects.json`](/reference/projects-json/) for the full reference on rules, precedence, and advanced options like exact matching and host scoping.

You can manage projects at any time via the **Manage projects** button at the bottom of the sidebar. A badge shows when there are running sessions that don't match any project yet. The modal has two sections:

- **Your projects**: configured projects with their match rules. Drag to reorder, click **×** to remove.
- **Discovered**: directories with running sessions that don't match any project. Type to filter, or enter a path to add directly.

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

## Project hub

Click a project name in the sidebar (or navigate to `/:project`) to see the project hub. This is an overview of every session in the project, grouped first by host, then by working directory.

### Host sections

Each host gets a section with a status indicator and a breadcrumb path showing the topology chain. For example, a devcontainer running on a remote peer might show `workstation › alpine-dev`. Status indicators:

| Indicator | Meaning |
|-----------|---------|
| **Accent dot** | Local host |
| **Green dot** | Connected remote peer |
| **Pulsing yellow dot** | Reconnecting to peer |
| **Red dot** | Peer disconnected |

### Folder rows

Within each host, sessions are grouped by their working directory. Each folder row shows a path label and the session cards underneath. A **+** button on each row lets you launch a new session in that directory on that host.

### Session cards

Each card shows a status dot and the session title. Click a card to attach to that session's terminal. The **×** button kills alive sessions or dismisses dead ones.

### Empty projects

If a project has no sessions yet, the hub shows the project's configured path with a **+** launcher to get started.

## The terminal

Click a session to attach. You get a full interactive terminal powered by [xterm.js](https://xtermjs.org/). Colors, cursor positioning, mouse support, and images all work. The header bar shows the session title, status label, and a working indicator.

## Launching sessions

### From the command line

```bash
gmux pi              # coding agent
gmux pytest --watch  # any command
```

### From the UI

There are several places to launch:

- **Sidebar header**: click the **+** button at the top of the sidebar to launch in the default directory.
- **Sidebar project**: hover a project name to reveal a **+** button. This launcher is context-aware: it targets the host and directory of whatever you're currently looking at. Select a session on a remote peer, and the **+** targets that peer. Switch to a local session, it targets local.
- **Project hub**: click the **+** on any folder row to launch in that specific directory on that host. For projects with [peers](/multi-machine), the per-host launcher routes the session to the correct machine.
- **Home screen**: quick-launch buttons for starting a session without any project context.

All launch menus show the available adapters (Shell, pi, Claude Code, Codex). The first item aligns with the **+** button so a double-click launches the default adapter instantly.

## URL routing

Every view has a stable URL:

| URL pattern | What it shows |
|-------------|---------------|
| `/` | Home: host status, projects, quick launch |
| `/:project` | Project hub overview |
| `/:project/:adapter/:slug` | A specific session's terminal |

For example, `/gmux/pi/fix-auth-bug` links directly to a pi session in the gmux project. URLs update as you navigate, work with browser back/forward, and are bookmarkable. Session slugs remain stable across kill and resume.

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
