---
name: dev-frontend
description: Start the vite dev server connected to the already-running prod daemon. Use for UI/React/CSS changes when no Go code has changed.
---

# Dev: frontend against prod daemon

The production daemon is already running at `:8790` with real data. This starts a
hot-reload vite dev server that proxies API calls to it. No build step needed.

## Start the server

```bash
cd /Users/james-carmody/james-agent-workspace/projects/james/james-gmux
just dev-frontend
```

Leave it running. The app is at **http://localhost:5173**.

## Authenticate and navigate

```bash
TOKEN=$(find ~/.local/state -name 'auth-token' -path '*/gmux*' 2>/dev/null | grep -v gmux-dev | head -1 | xargs cat)
agent-browser navigate "http://localhost:5173/auth/login?token=$TOKEN"
```

Then navigate to whatever route you need:

```bash
agent-browser navigate "http://localhost:5173/<slug>/<route>"
agent-browser screenshot <path>.png
```

> Use `localhost`, not `127.0.0.1` — Vite binds IPv6 (`[::1]`).

## Find your project slug

```bash
find ~/.local/state -name 'projects.json' -path '*/gmux*' 2>/dev/null | grep -v gmux-dev | head -1 | xargs cat
```

## Browser debug hooks

| Hook | Purpose |
|---|---|
| `__gmuxNavigateToSession(id)` | Route to a session by id |
| `__gmuxTerm` | Live terminal instance |
| `__gmuxInject(b64)` | Write base64 bytes into the terminal |
| `__gmuxDiag()` | Sync diagnostics (scrollback, ws state) |
