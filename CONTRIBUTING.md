# Contributing to gmux

## Prerequisites

| Tool | Purpose | Install |
|------|---------|---------|
| **Node.js** ≥ 20 | JS/TS tooling | [nodejs.org](https://nodejs.org) |
| **pnpm** ≥ 9 | Package manager | `npm i -g pnpm` |
| **Go** ≥ 1.22 | Native services (gmuxd, gmux) | [go.dev](https://go.dev/dl/) |
| **watchexec** | Auto-rebuild Go on file change (dev mode) | `pacman -S watchexec` / `cargo install watchexec-cli` / [github.com/watchexec/watchexec](https://github.com/watchexec/watchexec/releases) |
| **jj** | Version control | [martinvonz.github.io/jj](https://martinvonz.github.io/jj/) |

Optional: **moon** is installed locally via pnpm (`@moonrepo/cli`), no global install needed.

## Getting started

```bash
pnpm install          # JS dependencies + moon
```

## Development

Run all services with watch/HMR:

```bash
moon run :dev
```

This starts:
- **gmuxd** (`:8790`) — Go, auto-restarts on `.go` changes via watchexec
- **gmux-web** (`:5173`) — Vite HMR, proxies `/v1/*` and `/ws/*` to gmuxd

**No manual kill needed.** When gmuxd starts, it asks any existing instance to shut down gracefully via the Unix socket before binding.

To run services individually:

```bash
moon run gmuxd:dev        # just gmuxd with watchexec
moon run gmux-web:dev     # just vite
```

## Tests & linting

```bash
moon run :test    # all tests (Go + JS)
moon run :lint    # all lint/typecheck
```

## Project structure

| Path | Language | Purpose |
|------|----------|---------|
| `cli/gmux` | Go | Session launcher — PTY, WebSocket, runner |
| `services/gmuxd` | Go | Machine daemon — discovery, state store, WS proxy, embedded web UI |
| `packages/*` | Go | Shared libraries — adapters, paths, scrollback, session env |
| `apps/gmux-web` | TypeScript/Preact | Browser UI — sidebar, terminal, header bar |
| `packages/protocol` | TypeScript | Shared schemas, zod-validated |
| `apps/website` | Astro/Starlight | Documentation site ([gmux.app](https://gmux.app)) |

See the [architecture docs](https://gmux.app/architecture/) and the [develop guides](apps/website/src/content/docs/develop/) for how the pieces fit together.
