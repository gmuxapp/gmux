---
name: dev-full
description: Start the full dev stack (vite + dev gmuxd + Go file watcher). Use when Go daemon code has changed and backend + frontend must run together.
---

# Dev: full stack (Go + frontend)

Builds `gmuxd-dev` from source, runs it on `:9790`, runs vite on `:5173`, and
watches Go files for automatic rebuild. Use this when you've changed Go code.

## Start the stack

```bash
cd /Users/james-carmody/james-agent-workspace/projects/james/james-gmux
just dev
```

This blocks — keep it running in a background session. The app is at **http://localhost:5173**; the dev daemon API is at `:9790`.

> `:9790` is the dev daemon. `:8790` is production. They are intentionally far apart.

## Authenticate and navigate

```bash
TOKEN=$(find ~/.local/state -name 'auth-token' -path '*/gmux-dev*' 2>/dev/null | head -1 | xargs cat)
agent-browser navigate "http://localhost:5173/auth/login?token=$TOKEN"
```

Then navigate and screenshot:

```bash
agent-browser navigate "http://localhost:5173/<slug>/<route>"
agent-browser screenshot <path>.png
```

> Use `localhost`, not `127.0.0.1` — Vite binds IPv6 (`[::1]`). Always auth and
> navigate via `:5173`, not `:9790` — the auth cookie is origin-scoped.

## Find your project slug

```bash
find ~/.local/state -name 'projects.json' -path '*/gmux-dev*' 2>/dev/null | head -1 | xargs cat
```

## Browser debug hooks

| Hook | Purpose |
|---|---|
| `__gmuxNavigateToSession(id)` | Route to a session by id |
| `__gmuxTerm` | Live terminal instance |
| `__gmuxInject(b64)` | Write base64 bytes into the terminal |
| `__gmuxDiag()` | Sync diagnostics (scrollback, ws state) |
