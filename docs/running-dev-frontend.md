# Running the Dev Frontend

How to run the gmux-web frontend against a live gmuxd daemon.

---

## Prerequisites

```bash
cd apps/gmux-web
pnpm install    # only needed once, or after lockfile changes
```

---

## Option A — Frontend only, against a running daemon (recommended for UI work)

The PRODUCTION daemon at `:8790` is typically already running with real data and is available in the sandboxed workspace. Connect the gmux-web frontend to it:

```bash
cd apps/gmux-web
npx vite
```

Vite defaults to proxying `127.0.0.1:8790`. Override if needed:

```bash
VITE_DEV_PROXY_HOST=127.0.0.1 VITE_DEV_PROXY_PORT=8790 npx vite
```

Open **http://localhost:5173**. Real sessions, real files, hot-reload.

> **Mock mode** — `http://localhost:5173/?mock` loads canned data without a daemon.
> Useful for basic rendering checks, but it hides interactive chrome (close buttons,
> file tree actions). Don't use it for meaningful feature verification.

---

## Option B — Full dev stack (Go + JS, needed only for backend changes)

Use this only when you've changed Go code and need backend + frontend together.

```bash
just dev
# or: bash scripts/dev-server.sh
```

This builds `bin/gmux-dev` and `bin/gmuxd-dev`, starts vite on `:5173`, starts
gmuxd-dev on `:8791`, and watches Go sources for rebuild.

Open **http://localhost:8791** (gmuxd-dev proxies vite and serves the API on one port).

To launch sessions against the dev daemon:

```bash
source scripts/dev-session.sh
gmux-dev bash
```

**Use real data.** The dev daemon at `:8791` starts fresh, but you can point it at
real workspace dirs via `projects.json`. Don't rely on fabricated test fixtures for
manual verification — real data surfaces real bugs.

---

## Verifying frontend changes visually (agent-browser)

The sandbox has no display, so headed Playwright doesn't work. Use the dev server
and `agent-browser` instead.

**Step 1** — Start vite (Option A above).

**Step 2** — Find the auth token for the running daemon:

```bash
TOKEN=$(find ~/.local/state -name 'auth-token' -path '*/gmux*' 2>/dev/null | head -1 | xargs cat)
```

The token lives under the daemon's `XDG_STATE_HOME`. For the standard dev setup it
is at `/home/agent/.local/state/gmux-dev/state/gmux/auth-token`. Don't hardcode
the path — use `find` so this works regardless of which daemon is running.

**Step 3** — Authenticate via **localhost:5173** (not 8790).

```bash
agent-browser navigate "http://localhost:5173/auth/login?token=$TOKEN"
# Browser lands on http://localhost:5173/ — you're now logged in
```

> **Why localhost, not 127.0.0.1?** Vite binds to `[::1]` (IPv6). `agent-browser`
> resolves `localhost` to `[::1]` but `127.0.0.1` goes to IPv4 and gets refused.
>
> **Why port 5173, not 8790?** The auth cookie is scoped to the origin. Logging in
> via `:8790` sets a cookie for `:8790` — the browser at `:5173` stays unauthenticated.
> Log in via `:5173` so the cookie covers the vite dev server.

**Step 4** — Navigate and screenshot:

```bash
agent-browser navigate "http://localhost:5173/<slug>/_md/AGENTS.md"
agent-browser screenshot tasks/james-gmux/ss-feature.png
```

Replace `<slug>` with the project slug from `projects.json` (see below).

---

## Finding your project slug

The markdown editor URL is `/:slug/_md/:path`. The slug must match an entry in
`projects.json`. Find it:

```bash
# Path depends on XDG_STATE_HOME of the running daemon — find it dynamically:
find ~/.local/state -name 'projects.json' -path '*/gmux*' 2>/dev/null | head -1 | xargs cat
```

For the standard dev setup, the file is at
`/home/agent/.local/state/gmux-dev/state/gmux/projects.json`. A typical config:

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
Non-matching sessions show in counts but have no navigable URL.

---

## Quick reference

| Scenario | Command | Open |
|---|---|---|
| Frontend only (UI changes) | `cd apps/gmux-web && npx vite` | `http://localhost:5173` |
| Full dev stack (Go changes) | `just dev` | `http://localhost:8791` |
| Mock mode (no daemon) | `cd apps/gmux-web && npx vite` → append `?mock` | `http://localhost:5173/?mock` |

Screenshot flow (always):
```bash
TOKEN=$(find ~/.local/state -name 'auth-token' -path '*/gmux*' 2>/dev/null | head -1 | xargs cat)
agent-browser navigate "http://localhost:5173/auth/login?token=$TOKEN"
agent-browser navigate "http://localhost:5173/<route>"
agent-browser screenshot <path>.png
```

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

**`bin/gmux` may be the wrong architecture.** The checked-in `bin/gmux` symlink can
point at a macOS binary; on a Linux host gmuxd's session-launch fails with
`exec format error`. Run `scripts/build.sh` or use `just dev` (always builds from
source) to fix this.
