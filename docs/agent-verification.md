# Agent Manual Verification

The three setups below are for **manual verification only**: reproducing bugs, confirming fixes, and taking screenshots. Pick based on what changed.

For automated E2E tests, see [docs/e2e.md](e2e.md).

---

## Which option to use?

| Scenario | Option |
|---|---|
| UI / React / CSS changes only; no Go changes | **A** — frontend dev against prod daemon |
| Go daemon code changed; need backend + frontend together | **B** — full dev stack |
| Need to reproduce or verify a bug in production | **C** — production stack via Chrome remote debugging |

**Default is Option A.** Use it unless Go code has changed. It starts instantly, uses real data, and requires no build step.

Option B is required when Go source has changed and the new daemon behaviour must be exercised together with the frontend.

Option C connects agent-browser directly to the production daemon at `:8790`, which serves its built-in embedded frontend. Use it to confirm a bug exists in production before working on a fix, or as a last resort to verify a fix after deploying. No build step — the production daemon is already running.

---

## Prerequisites

```bash
cd apps/gmux-web
pnpm install    # only needed once, or after lockfile changes
```

---

## Option A — Frontend dev against running prod daemon

The production daemon at `:8790` is already running with real data. Connect the vite dev server to it:

```bash
just dev-frontend
# or: cd apps/gmux-web && npx vite
```

Vite proxies `/v1`, `/auth`, `/ws` to `127.0.0.1:8790`. Override if needed:

```bash
VITE_DEV_PROXY_HOST=127.0.0.1 VITE_DEV_PROXY_PORT=8790 npx vite
```

App is at **http://localhost:5173**. Real sessions, real files, hot-reload.

---

## Option B — Full dev stack (Go + JS)

Use when Go daemon code has changed and backend + frontend must be tested together.

```bash
just dev
# or: bash scripts/dev-server.sh
```

This builds `bin/gmux-dev` and `bin/gmuxd-dev`, starts vite on `:5173`, starts gmuxd-dev on `:8791`, and watches Go sources for rebuild.

App is at **http://localhost:5173**. Use that URL for agent-browser (not `:8791` — the auth cookie is origin-scoped, see below). The daemon API is at `:8791`.

To launch sessions against the dev daemon:

```bash
source scripts/dev-session.sh
gmux-dev bash
```

Use real data. The dev daemon starts fresh, but point it at real workspace dirs via `projects.json`. Don't rely on fabricated fixtures for manual verification — real data surfaces real bugs.

---

## Option C — Production stack via Chrome remote debugging

The production daemon at `:8790` already serves the production-bundled frontend. No
build or server start needed. Use this option to:

- Confirm a bug exists in production before starting a fix (most common use)
- Verify a fix after deploying to production (last resort — prefer A or B first)

Agent-browser talks directly to the daemon. Auth cookie and app are both at `:8790`:

```bash
TOKEN=$(find ~/.local/state -name 'auth-token' -path '*/gmux*' 2>/dev/null | head -1 | xargs cat)
agent-browser navigate "http://localhost:8790/auth/login?token=$TOKEN"
agent-browser navigate "http://localhost:8790/<route>"
agent-browser screenshot <path>.png
```

Do not start vite for this option. Cookie and app must share the same origin (`:8790`).
---

## agent-browser flow (Options A and B)

The sandbox has no display, so headed Playwright doesn't work. Use `agent-browser` (Chrome remote debugging) against the vite dev server.

**Step 1** — Start the server (Option A or B above).

**Step 2** — Find the auth token:

```bash
TOKEN=$(find ~/.local/state -name 'auth-token' -path '*/gmux*' 2>/dev/null | head -1 | xargs cat)
```

Use `find` — don't hardcode the path. Token lives under the daemon's `XDG_STATE_HOME`.

**Step 3** — Authenticate via **localhost:5173**:

```bash
agent-browser navigate "http://localhost:5173/auth/login?token=$TOKEN"
# Browser lands on http://localhost:5173/ — you're now logged in
```

> **Why localhost, not 127.0.0.1?** Vite binds to `[::1]` (IPv6). `127.0.0.1` is IPv4 and gets `ERR_CONNECTION_REFUSED`.
>
> **Why port 5173, not 8790?** The auth cookie is origin-scoped. Logging in at `:8790` sets a cookie for `:8790` — the browser at `:5173` stays unauthenticated.

**Step 4** — Navigate and screenshot:

```bash
agent-browser navigate "http://localhost:5173/<slug>/<route>"
agent-browser screenshot tasks/james-gmux/ss-description.png
```

Replace `<slug>` with the project slug from `projects.json` (see below).

---

## Finding your project slug

The markdown editor URL is `/:slug/_md/:path`. The slug must match an entry in `projects.json`:

```bash
find ~/.local/state -name 'projects.json' -path '*/gmux*' 2>/dev/null | head -1 | xargs cat
```

A typical config:

```json
{
  "version": 2,
  "items": [
    { "slug": "james-gmux", "match": [{ "path": "/Users/james-carmody/james-agent-workspace/projects/james/james-gmux" }] },
    { "slug": "workspace",  "match": [{ "path": "/Users/james-carmody/james-agent-workspace" }] }
  ]
}
```

A session or file only appears in the UI if its path matches a configured project.

---

## Quick reference

| Option | Start command | agent-browser login URL |
|---|---|---|
| A — frontend + prod daemon | `just dev-frontend` | `http://localhost:5173` |
| B — full dev stack | `just dev` | `http://localhost:5173` |
| C — production stack | _(nothing to start)_ | `http://localhost:8790` |

---

## Environment variables (vite.config.ts)

| Variable | Default | Purpose |
|---|---|---|
| `VITE_DEV_PROXY_HOST` | `127.0.0.1` | gmuxd hostname for vite proxy |
| `VITE_DEV_PROXY_PORT` | `8790` | gmuxd port for vite proxy |
| `VITE_DEV_TOKEN` | _(empty)_ | Bearer token forwarded to gmuxd (for auth-enabled instances) |
| `VERSION` | `dev-<git-hash>` | Version baked into the bundle (set automatically by `just dev`) |

---

## Driving a session from the browser (debug hooks)

The running app exposes these globals on `window`:

| Hook | Purpose |
|---|---|
| `__gmuxNavigateToSession(id)` | Route to a session by id. Returns `true` once session and project have loaded; `false` if no matching project. |
| `__gmuxTerm` | Live `WTerm` instance — `__gmuxTerm.write(new TextEncoder().encode("..."))` injects bytes. |
| `__gmuxInject(b64)` | Write base64-encoded bytes into the real terminal (live sessions only). |
| `__gmuxDiag()` | Sync diagnostics (scrollback bytes, ws state, etc.). |

---

## Known issues

**`bin/gmux` may be the wrong architecture.** The checked-in `bin/gmux` symlink can point at a macOS binary; on a Linux host gmuxd's session-launch fails with `exec format error`. Run `scripts/build.sh` or use `just dev` (always builds from source) to fix this.
