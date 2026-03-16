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
./dev
```

This starts:
- **gmuxd** (`:8790`) — Go, auto-restarts on `.go` changes via watchexec
- **gmux-web** (`:5173`) — Vite HMR, proxies `/v1/*` and `/ws/*` to gmuxd
- **gmuxr** — Go runner launching `pi`, auto-restarts via watchexec

Ctrl+C stops everything.

**No manual kill needed.** When gmuxd starts, it asks any existing instance on the same port to shut down gracefully (`POST /v1/shutdown`) before binding. Restarts via watchexec or re-running `./dev` are seamless.

To run services individually:

```bash
pnpm moon run gmuxd:dev        # just gmuxd with watchexec
pnpm moon run gmux-web:dev     # just vite
```

## Tests & linting

```bash
pnpm moon run :test    # all tests (Go + JS)
pnpm moon run :lint    # all lint/typecheck
```

## Project structure

See [README.md](README.md) for workspace layout and [docs/](docs/) for ADRs and protocol specs.
