---
title: Quick Start
description: Install gmux and launch your first session in under a minute.
---

## Install

### Homebrew (macOS & Linux)

```bash
brew install gmuxapp/tap/gmux
```

This installs two binaries: `gmuxd` (the daemon) and `gmuxr` (the session launcher).

### Manual download

Grab the latest release from [GitHub Releases](https://github.com/gmuxapp/gmux/releases) and put both binaries somewhere on your `PATH`.

## Launch your first session

```bash
gmuxr pi
```

That's it. `gmuxr` auto-starts `gmuxd` in the background if it isn't already running, then launches `pi` as a managed session.

Open **[localhost:8790](http://localhost:8790)** in your browser — you'll see the session in the sidebar with a live terminal.

## More examples

```bash
gmuxr -- pytest --watch     # test watcher
gmuxr -- make build         # build process
gmuxr -- npm run dev        # dev server
gmuxr -- ssh prod-host      # remote shell
```

Everything after `--` is the command to run. Without `--`, gmuxr assumes you mean an adapter name (like `pi`).

## What you'll see

The browser UI shows:

- **Sidebar** — sessions grouped by project directory, with status dots:
  - 🟢 cyan pulse = working (actively running something)
  - 🟠 amber = unread (new output since you last looked)
- **Terminal** — click any session to attach a live terminal, same as if you ran the command locally
- **Header** — session title, working directory, status label, kill button

## Multiple sessions

Launch as many as you want:

```bash
gmuxr pi                    # coding agent
gmuxr -- pytest --watch     # test watcher in same project
cd ~/other-project
gmuxr -- make build         # different project — shows as separate folder
```

All sessions appear in the sidebar, grouped by the directory you launched them from.

## App mode

For a standalone window without browser chrome (no address bar, no tabs), launch with `--app`:

```bash
# Chrome / Chromium
google-chrome --app=http://localhost:8790
# or
chromium --app=http://localhost:8790

# macOS
open -na "Google Chrome" --args --app=http://localhost:8790

# Edge
msedge --app=http://localhost:8790
```

This gives gmux its own window and taskbar entry — feels like a native app. You can also "Install" it as a PWA from the browser menu (⋮ → Install gmux) for the same effect with a desktop shortcut.

## Mobile

Open the same URL on your phone. The terminal is fully interactive — tap `esc`, `tab`, `ctrl`, arrow keys, and `enter` from the bottom bar.

## Stopping

- **Kill a session:** click the `×` button in the header, or `Ctrl+C` in the terminal
- **Stop the daemon:** `kill $(pgrep gmuxd)` — or just leave it running, it uses minimal resources

## What's running

```
gmuxr (per session)  →  gmuxd (per machine)  →  browser
  PTY + WebSocket         discovery + proxy        sidebar + terminal
```

- **gmuxr** owns each child process — PTY, WebSocket server, adapter
- **gmuxd** discovers sessions, proxies connections, serves the web UI
- **Browser** renders the session list and attaches to terminals

## Development setup

If you're working on gmux itself, see [CONTRIBUTING.md](https://github.com/gmuxapp/gmux/blob/main/CONTRIBUTING.md) for the local development workflow.

## Next steps

- [Introduction](/introduction) — what gmux is and why
- [Architecture](/architecture) — how the pieces fit together
- [Adapters](/adapters) — how gmux understands different tools
