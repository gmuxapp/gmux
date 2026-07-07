---
title: Using the UI
description: What you see in gmux and how to work with it.
---

Running `gmux open` opens the dashboard in a dedicated browser window. You can also navigate to **[localhost:8790](http://localhost:8790)** directly; the first time you'll need to authenticate by visiting the login URL from `gmux auth`.

## The sidebar

The left panel lists your sessions grouped into projects.

### Logo

Click the **gmux** logo at the top of the sidebar to return to the home screen. The logo doubles as a cue: it lights up when a session elsewhere is waiting on you. The gear button next to it opens **Settings**; a red pip on the gear flags unresolved host references.

## Home: the Activity dashboard

The home screen is a pure overview of your sessions across all hosts, newest-first:

- **Waiting** — sessions with unread output that need your attention.
- **Active** — sessions where the agent or command is currently working.
- **Recency buckets** — everything else, grouped by last activity (last hour, earlier today, yesterday, earlier this week…).

An **Enable notifications** pill in the Activity header opts into browser notifications. Project and host management live in **Settings** (gear button in the sidebar header).

## Settings → Hosts

Hosts you add via **Settings → Hosts → Connect to host** persist across restarts and reconnect automatically. gmux does not auto-discover tailnet machines — adding one is an explicit, token-authenticated step (see [Multi-Machine](/multi-machine/) and [ADR 0008](https://github.com/gmuxapp/gmux/blob/main/docs/adr/0008-peer-authentication-via-token.md)).

In **Settings → Hosts**, each host shows an explicit status:

| Status | Meaning |
|--------|---------|
| **Online** | Connected and authenticated |
| **Connecting…** | Handshake in progress |
| **Auth needed** | Reachable, but the token is missing or wrong — an **Add token** button pre-fills the connect form so you can supply it |
| **Offline** | Unreachable right now (shows the connection error); it reconnects on its own when the host comes back |

Removing a host also clears the project references that pointed at it, so it leaves nothing behind under **Referenced but not found**.

**Upgrading to 2.0:** hosts you had projects on — that earlier versions auto-discovered on your tailnet — are migrated into the roster as **Auth needed**. Click **Add token** on each and paste its token (run `gmux auth` on that host) to bring it back online. Other tailnet machines aren't carried over; re-add them with **Connect to host** if you want them. Your `projects.json` is backed up to `projects.json.bak` before the upgrade rewrites it. See the [migration guide](/migrating-to-2/) for the full 2.0 upgrade story.

## Projects

Sessions don't appear in the sidebar until you add a project. The first time you open the dashboard, click the **+** button to launch a session. gmux creates a default "home" project that catches sessions started in your home directory itself. As you work in more repositories, open **Settings → Projects** to organize sessions by repo.

Click a **project name** to open the [project hub](#project-hub), an overview of all sessions in that project. The active project is highlighted in the sidebar.

Each project has **match rules** that determine which sessions belong to it. Rules can match by filesystem path (`~/dev/gmux` and its subdirectories) or by git remote URL (grouping clones across machines). See [`projects.json`](/reference/projects-json/) for the full reference on rules, precedence, and advanced options like exact matching.

You can manage projects at any time in **Settings → Projects** (gear button in the sidebar header, or the `?settings` URL parameter):

- **Your projects**: configured projects with their match rules. Drag to reorder, click **×** to remove.
- **Discovered**: directories gmux noticed sessions in that don't match any project — including directories advertised by peer hosts. Type to filter, click **Add**, or enter a local path manually.

## Sessions

Each session has a dot on the left edge:

| Indicator | Meaning |
|-----------|---------|
| **Pulsing ring** | The tool is actively working (building, thinking, running tests) |
| **Cyan dot** | New output you haven't seen yet (viewing the session clears it) |
| **Red dot** | The agent reported an error |
| **Muted ring** (brief) | Transient terminal activity, fades after a few seconds |
| **No dot** | Idle or waiting for input |

Agent sessions (pi, Claude, Codex) only trigger the unread dot when the assistant completes a turn, not on every line of output.

Hover over a session to reveal the **×** button. This dismisses the session: live runners are killed, the sidebar/project membership is removed, and persisted runtime metadata is dropped so the session does not come back as resumable. Use **Resume** from a dead session view only when you want to continue it.

## Project hub

Click a project name in the sidebar (or navigate to `/:project`) to see the project hub — an overview of every session in the project, mirroring the home layout: **Waiting**, **Active**, and **All sessions** rows, newest-first.

When every session shares the same working directory, it appears once as a subtitle; otherwise each row shows its own directory. On the home dashboard, a session card surfaces its working directory only when it differs from the project's canonical folder — a subfolder or worktree shows as a relative `./sub/dir` badge, an unrelated path as its absolute `~/…` form — so sessions launched somewhere other than the project root are easy to spot. If the project spans hosts, rows carry an `@host` suffix (devcontainer sessions get a container icon). The **+** in the hub header launches a new session in the project's canonical directory — for a referenced project this routes to the owning machine. If a project has no sessions yet, the hub shows the project's configured path with a **+** launcher to get started.

## The terminal

Click a session to attach. You get a full interactive terminal powered by [xterm.js](https://xtermjs.org/). Colors, cursor positioning, mouse support, and images all work. The header bar shows the session title and a status chip: **Working…**/**Error** while an agent is busy, **Exited (N)** for dead sessions, **Resuming…** during a resume.

### Find in terminal

Press **Cmd/Ctrl+F** (or use the session **⋮** menu → *Find in terminal*) to open a floating find bar over the terminal. Search is incremental; step through matches with Enter/Shift+Enter or the ‹ › buttons, and press Escape to close. This replaces the browser's in-page find, which can't see into a canvas-rendered terminal.

### Session menu

The **⋮** menu in the terminal header offers *Find in terminal*, one lifecycle action (**Restart** for alive sessions; **Resume** or **Rerun** for dead ones — dead sessions also show the same action as a primary button over the replay), and session info (adapter, version, host). An **outdated** badge appears when the session's runner binary is stale relative to the daemon — restart the session to pick up the new version.

Backend or action failures surface as error toasts.

## Launching sessions

### From the command line

```bash
gmux -- pi              # coding agent
gmux -- pytest --watch  # any command
```

```bash
gmux -d -- make build   # detached; prints the session id
gmux edit notes.md      # editor session; also works as $EDITOR
```

### From the UI

There are two places to launch:

- **Sidebar project**: hover a project name to reveal a **+** button. It launches in the project's own directory — the first configured path for a project you own, or the upstream directory for a [referenced](/multi-machine) project (which routes to the owning machine) — regardless of which session you're currently viewing.
- **Project hub**: the **+** in the header launches in that same project directory, routing to the owning machine for referenced projects.

Launch menus show the adapters available on that host (by default: Shell, pi, Claude Code, Codex, Editor — whichever are installed). The first item aligns with the **+** button so a double-click launches the default adapter instantly.

## URL routing

Every view has a stable URL:

| URL pattern | What it shows |
|-------------|---------------|
| `/` | Home: the Activity dashboard |
| `/:project` | Project hub overview |
| `/:project/:adapter/:slug` | A specific session's terminal |
| `/@:owner/:project/...` | A project owned by a peer host |

For example, `/gmux/pi/fix-auth-bug` links directly to a pi session in the gmux project. URLs update as you navigate, work with browser back/forward, and are bookmarkable. Session slugs remain stable across kill and resume.

## Keyboard shortcuts

gmux ships a complete default keymap. Keys not listed here go straight to the terminal.

### All platforms

| Shortcut | Action |
|----------|--------|
| **Shift+Enter** | Sends a plain newline (`\n`) instead of Enter |
| **Ctrl+C** | If text is selected: copy to clipboard. Otherwise: sends SIGINT |
| **Cmd/Ctrl+F** | Open find-in-terminal (replaces the browser's in-page find) |

### Linux / Windows

| Shortcut | Action |
|----------|--------|
| **Ctrl+Shift+C** | Copy selection to clipboard |
| **Ctrl+V** | Paste from clipboard |
| **Ctrl+Shift+V** | Paste from clipboard |
| **Ctrl+Alt+T** | Sends Ctrl+T (browser steals Ctrl+T) |
| **Ctrl+Alt+N** | Sends Ctrl+N (browser steals Ctrl+N) |
| **Ctrl+Alt+W** | Sends Ctrl+W (browser steals Ctrl+W) |
| **Ctrl+Backspace** | Delete word backward |
| **Ctrl+Delete** | Delete word forward |

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

Open the same URL on your phone (or via [remote access](/remote-access)). The sidebar slides in from the left (tap ☰ — a badge on it flags waiting sessions), and a bottom toolbar provides keys that phones don't have:

| Button | Sends |
|--------|-------|
| **☰** | Opens the sidebar |
| **esc** | Escape |
| **tab** | Tab |
| **ctrl** | Arms Ctrl for the next key (tap ctrl, then c = Ctrl+C) |
| **alt** | Arms Alt for the next key |
| **← ↑ ↓ →** | Arrow keys (hold to repeat) |
| **⇤ ⇥** | Word-jump left / right |
| **▶** | Send (Enter; Alt+Enter when alt is armed) |

When **ctrl** or **alt** is armed, the key highlights and applies to the next key you press — on the toolbar or the on-screen keyboard — then disarms. Keys never change meaning. When you've scrolled up, an extra key jumps back to the bottom. The toolbar works with the keyboard closed, and on narrow phones it wraps into two rows.

Long-press a link in the terminal to copy it or open it in a new tab. Paste goes through the paste keybind or long-press, not a toolbar key.
