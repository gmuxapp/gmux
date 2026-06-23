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

Run the same entrypoints CI does — local and CI are kept in sync by
construction (CI just runs these):

```bash
pnpm lint    # biome + all lint/typecheck (Go vet + tsc)
pnpm build   # build everything
pnpm test    # all tests (Go + JS)
```

These wrap moon (`moon run :lint` / `:build` / `:test`). Prefer the `pnpm`
scripts: `pnpm lint` also runs biome with `--error-on-warnings`, which
bare `moon run :lint` does not — running moon directly will silently skip
the biome lint that CI enforces.

Note: moon does not manage the JS toolchain or auto-install dependencies.
Run `pnpm install` first, or tasks fail with "command not found".

To target one project: `moon run gmux-web:test`, `moon run gmuxd:lint`, etc.

## Project structure

See [README.md](README.md) for workspace layout and the [website docs](apps/website/src/content/docs/) for architecture, protocol specs, and guides.
