# gmux

**Keep tabs on every AI agent, test runner, and long-running process across your machines. Work from your desktop, steer from your phone.**

Launch any command as a managed session. gmux gives you a live, interactive terminal for each one — grouped by project, with real-time status updates pushed to your browser. When an agent needs input, you'll know. When tests fail, you'll see it. Switch to your phone and the same view is there, ready for you to course-correct.

No Electron, no desktop app. Just a browser and two small binaries.

## Install

```bash
brew install gmuxapp/tap/gmux
```

Or download from [GitHub Releases](https://github.com/gmuxapp/gmux/releases).

## Quick start

```bash
gmux -- pi                 # launch a coding agent
gmux -- npm run dev       # launch any long-running command
gmux -d -- make build      # detached; prints the session id
gmux open                  # open the UI
```

Open the dashboard — all three sessions are there, grouped by project, with live status indicators. Click one to attach a full terminal. The same xterm.js that powers the VS Code terminal, running in your browser.

The daemon (`gmuxd`) starts automatically on first use. There's nothing else to set up. Power users alias `gm='gmux --'`. For daemon lifecycle commands, run `gmux daemon status` (see `gmux help`).

## How it works

```mermaid
graph LR
    subgraph "Per session"
        gmux1["gmux\nPTY · WebSocket · adapter"]
    end

    subgraph "Per machine"
        gmuxd["gmuxd\ndiscovery · cache · proxy"]
    end

    subgraph Browser
        web["gmux-web\nsidebar · terminal"]
    end

    gmux1 -- "Unix socket" --> gmuxd
    gmuxd -- "HTTP · SSE · WS" --> web
```

**`gmux`** wraps any command in a managed session. It allocates a PTY, serves a WebSocket for terminal access, and runs an **adapter** that understands what the child process is doing. For agent tools (pi, Claude Code, Codex), gmux installs a small hook into the agent so the agent itself reports its state — active, idle, which conversation it holds — authoritatively, with no output scraping. A generic command gets alive/dead/activity tracking out of the box.

**`gmuxd`** runs once per machine (auto-started by `gmux`). SQLite is authoritative for durable session rows, project placement, ordering, and manual peers. Live runners remain authoritative for runtime facts such as liveness and current status; gmuxd merges that runtime overlay into SQLite-backed snapshots, proxies WebSocket connections, and pushes updates via SSE. Authentication and bootstrap files also live under gmux's state/config directories.

**`gmux-web`** is the browser UI. The sidebar groups sessions into projects, with status dots that pulse when something needs attention. The terminal is xterm.js — the same battle-tested terminal emulator that powers VS Code's integrated terminal — with synchronized output for flicker-free session switching and ~1 MiB of persisted scrollback that replays instantly on reconnect.

## What you see

```
┌─────────────────────────────────────┐
│ gmux                             ⚙  │
│                                     │
│ ▼ myapp                        ● 2  │
│                                     │
│   ● fix auth bug               now  │
│     working · pi                    │
│                                     │
│   ● test watcher             2m ago │
│     npm run dev                     │
│                                     │
│ ▼ gmux                         ● 1  │
│                                     │
│   ● bootstrap                  5m   │
│     unread · pi                     │
│                                     │
│ ▸ docs                         ○ 1  │
└─────────────────────────────────────┘
```

Sessions are grouped into **projects** by working directory — manage the project list in Settings, where gmux also suggests directories it discovered from past sessions. Each project's status dot reflects the most urgent session inside it.

## Features

### Sessions
- **Launch anything** — `gmux -- <command>` wraps any process in a managed session
- **Full terminal** — xterm.js with WebSocket transport, the same terminal emulator as VS Code
- **~1 MiB persisted scrollback** — replays instantly on reconnect, survives runner exit, no lost context
- **Flicker-free switching** — DEC 2026 synchronized output renders session swaps in a single frame
- **Session lifecycle** — live status, exit codes, kill from the UI; dead sessions stay resumable (agent conversations continue, other commands re-run)
- **Reconnecting** — tab away, come back, the terminal is right where you left it
- **Editor tabs** — `gmux edit <file>` opens a managed editor session; works as `$EDITOR` (blocks and propagates the exit code, so `git commit` just works)

### Adapters — session-level intelligence
Adapters teach gmux how to work with specific tools. They're compiled into the binary and selected automatically by command name.

- **Auto-detection** — `gmux -- pi` recognizes pi and activates the pi adapter. No flags needed.
- **Authoritative agent status** — pi, Claude Code, and Codex report session identity, titles, and active/idle state through the tools' own hook mechanisms, injected per launch
- **Child awareness** — any tool can self-report status via `PUT /status` on `$GMUX_SOCKET`, no adapter required
- **Graceful fallback** — unknown commands get the shell adapter

### Scripting & agents
gmux is a full CLI, designed to compose into scripts and agent workflows:

```bash
id=$(gmux -d -- pi)                     # launch detached, capture the id
gmux send --wait --timeout 600 "$id" 'refactor the auth module' Enter  # send, block until the turn ends
gmux tail "$id" -n 50                   # read the plain-text terminal tail
```

`gmux ls --json` gives agents a machine-readable session list; `gmux send-keys -t` is tmux-compatible. See the [scripting guide](apps/website/src/content/docs/integrations/scripts-and-agents.md).

### Multi-machine
Run gmux on each machine, then connect them: `gmux auth` on the remote host prints a connect URL you paste into **Settings → Hosts → Connect to host**. Next add wanted projects under **Settings → Projects → From other hosts**; connecting a network host alone does not add its sessions to the sidebar. Peers authenticate with bearer tokens, and remote sessions are addressed as `<id>@<peer>`. Devcontainers running the gmux feature are discovered automatically. See [Multi-machine](apps/website/src/content/docs/multi-machine.md).

### UI
- **Activity by recency** — the home screen orders live sessions by recent output and groups them by calendar day
- **Project grouping** — sessions group into projects by working directory; manage the list in Settings
- **Find in terminal** — Cmd/Ctrl+F searches the terminal buffer
- **Mobile responsive** — same URL on your phone; a dedicated toolbar, keyboard handling, and long-press link actions
- **URL routing** — every project and session has a stable URL you can bookmark
- **Customizable theme** — dark theme with a Windows Terminal–compatible terminal palette (`theme.jsonc`)

### Architecture
- **Split authority** — SQLite owns durable daemon state; live runners own runtime facts that gmuxd overlays onto stored rows
- **No external dependencies** — no tmux, no screen, no abduco. Two Go binaries and a web app.
- **Web-first** — works on desktop, tablet, phone. Same URL everywhere.
- **Zero config** — run `gmux -- <command>`, open a browser

## Extensibility

- **Adapters** (Go, compiled in) — recognize commands, launch presets, titles, resume, hook-driven status
- **Child self-reporting** — any process can `PUT /status` on `$GMUX_SOCKET` to set active/error state, no adapter required

## Development

See [CONTRIBUTING.md](CONTRIBUTING.md) for prerequisites and setup.

```bash
pnpm install      # JS dependencies
pnpm dev          # start all services with watch/HMR
```

### Monorepo layout

```mermaid
graph TB
    subgraph "CLI"
        gmux2["cli/gmux\nGo — PTY, WebSocket, runner"]
    end

    subgraph "Daemon"
        gmuxd1["services/gmuxd\nGo — discovery, cache, proxy"]
    end

    subgraph "Web"
        web["apps/gmux-web\nPreact — sidebar, terminal"]
        proto["packages/protocol\nTypeScript — zod schemas"]
        proto --> web
    end

    gmux2 -- "Unix socket" --> gmuxd1
    gmuxd1 -- "REST + SSE + WS" --> web
```

| Path | Language | Purpose |
|------|----------|---------|
| `cli/gmux` | Go | Session launcher — PTY, WebSocket, runner |
| `services/gmuxd` | Go | Machine daemon — discovery, cache, WS proxy, embedded web UI |
| `packages/*` | Go | Shared libraries — adapters, paths, scrollback, session env |
| `apps/gmux-web` | TypeScript/Preact | Browser UI — sidebar, terminal, header bar |
| `packages/protocol` | TypeScript | Shared schemas, zod-validated |
| `apps/website` | Astro/Starlight | Documentation site |

## Docs

Documentation lives in the [website](apps/website/src/content/docs/):

- [Getting Started](apps/website/src/content/docs/getting-started.mdx) — install and first session
- [Architecture](apps/website/src/content/docs/architecture.md) — runtime structure (gmux, gmuxd, web UI)
- [CLI Reference](apps/website/src/content/docs/reference/cli.md) — every verb and flag
- [Multi-machine](apps/website/src/content/docs/multi-machine.md) — connecting hosts
- [Session Schema](apps/website/src/content/docs/develop/session-schema.md) — metadata model
- [Adapter Architecture](apps/website/src/content/docs/develop/adapter-architecture.md) — how adapters work
- [Security](apps/website/src/content/docs/security.md) — threat model and safeguards
- [Remote Access](apps/website/src/content/docs/remote-access.md) — `gmux remote` / Tailscale setup

## License

MIT
