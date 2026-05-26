# Running the Dev Frontend

How to run the gmux-web frontend against a live gmuxd daemon, covering both the standard host workflow and the sandbox/container workflow.

---

## Prerequisites

```bash
cd apps/gmux-web
pnpm install          # or npm install — installs vite and all JS deps
```

---

## Option A — Standard host workflow (recommended)

Use this when you have Go installed and want the full dev stack (gmuxd auto-reload on Go file changes):

```bash
just dev
# or equivalently:
bash scripts/dev-server.sh
```

This single command:
1. Builds `bin/gmux-dev` and `bin/gmuxd-dev` from source
2. Starts **vite** on `:5173` with HMR
3. Starts **gmuxd-dev** on `:8791`, proxying `/v1`, `/auth`, and `/ws` through to vite
4. Watches `services/gmuxd/`, `cli/gmux/`, and `packages/adapter/` — rebuilds and restarts gmuxd on `.go` changes

Open **http://localhost:8791** (not 5173 — gmuxd proxies vite and serves the API on the same port).

To launch sessions against this dev daemon:
```bash
source scripts/dev-session.sh
gmux-dev bash          # any command
gmux-dev pi            # coding agent session
```

---

## Option B — Frontend-only (attach to existing gmuxd)

Use this when gmuxd is already running (e.g. the installed production daemon on `:8790`) and you only need to iterate on the frontend. No Go required.

```bash
cd apps/gmux-web

# Point vite's proxy at the running gmuxd (default is 127.0.0.1:8790)
npx vite
```

Vite proxies all `/v1`, `/auth`, and `/ws` requests to `http://127.0.0.1:8790` (or whatever gmuxd is running on).

To override the target:

```bash
VITE_DEV_PROXY_HOST=127.0.0.1 VITE_DEV_PROXY_PORT=8790 npx vite
```

Open **http://localhost:5173**. The production gmuxd serves real sessions; the frontend builds and reloads via HMR.

> **Note:** Use `?mock` (`http://localhost:5173/?mock`) to load with canned mock data and no live daemon. Some UI chrome (e.g. the × close button on sessions) is hidden in mock mode by CSS — use the live mode to verify interactive behaviour.

---

## Option C — Sandbox/container workflow

Use this when developing inside a Docker sandbox that can't bind to the host's loopback directly.

**On the host**, expose the running gmuxd port to the sandbox:

```bash
# Allow the sandbox to reach host gmuxd
sbx policy allow network host.docker.internal:8790
```

**Inside the sandbox**, run vite pointing at the host:

```bash
cd apps/gmux-web
VITE_DEV_PROXY_HOST=host.docker.internal VITE_DEV_PROXY_PORT=8790 npx vite --host 0.0.0.0
```

Then **on the host**, expose the vite port back so your browser can reach it:

```bash
sbx ports <sandbox-name> --publish 5173:5173/tcp
```

Open **http://localhost:5173** in your browser on the host. The frontend talks to the sandbox vite server, which proxies API calls to the host's gmuxd.

---

## Quick reference

| Scenario | Command | Open |
|---|---|---|
| Full dev stack (Go + JS) | `just dev` | `http://localhost:8791` |
| Frontend only, prod gmuxd | `cd apps/gmux-web && npx vite` | `http://localhost:5173` |
| Mock mode (no daemon) | `cd apps/gmux-web && npx vite` → append `?mock` | `http://localhost:5173/?mock` |
| Sandbox → host gmuxd | `VITE_DEV_PROXY_HOST=host.docker.internal npx vite --host 0.0.0.0` | `http://localhost:5173` (after port-forward) |

---

## Verifying frontend changes against a live daemon

The mock (`?mock`) mode is useful for basic UI rendering checks, but it hides interactive chrome (close buttons, file tree actions) and can't exercise any feature that reads or writes real state. For meaningful verification:

1. Ensure gmuxd is running: `curl http://localhost:8790/v1/health`
2. Start vite in Option B or C mode
3. Exercise the feature with real sessions — launch one with `gmux bash` or `gmux pi`
4. For file-tree and markdown editor features, open a project with real `.md` files via the file tree

---

## Environment variables (vite.config.ts)

| Variable | Default | Purpose |
|---|---|---|
| `VITE_DEV_PROXY_HOST` | `127.0.0.1` | gmuxd hostname for vite proxy |
| `VITE_DEV_PROXY_PORT` | `8790` | gmuxd port for vite proxy |
| `VITE_DEV_TOKEN` | _(empty)_ | Bearer token forwarded to gmuxd (for auth-enabled instances) |
| `VERSION` | `dev-<git-hash>` | Version baked into the bundle (set automatically by `just dev`) |
