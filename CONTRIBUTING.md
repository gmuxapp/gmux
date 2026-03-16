# Contributing to gmux

## Prerequisites

| Tool | Purpose | Install |
|------|---------|---------|
| **Node.js** ≥ 20 | JS/TS tooling | [nodejs.org](https://nodejs.org) |
| **pnpm** ≥ 9 | Package manager | `npm i -g pnpm` |
| **Go** ≥ 1.22 | Native services (gmuxd, gmuxr) | [go.dev](https://go.dev/dl/) |
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

**No manual kill needed.** When gmuxd starts, it asks any existing instance on the same port to shut down gracefully (`POST /v1/shutdown`) before binding.

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

See [README.md](README.md) for workspace layout and the [website docs](apps/website/src/content/docs/) for architecture, protocol specs, and guides.
