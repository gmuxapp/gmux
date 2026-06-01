---
name: dev-prod
description: Connect agent-browser directly to the production daemon at :8790. Use to confirm a bug exists in production before starting a fix, or to verify a deployed fix.
---

# Dev: production stack (no build)

The production daemon at `:8790` already serves its embedded frontend. Nothing to
start. Use this to confirm a bug exists before touching code, or to verify a fix
after `just install`.

## Authenticate and navigate

```bash
TOKEN=$(find ~/.local/state -name 'auth-token' -path '*/gmux*' 2>/dev/null | grep -v gmux-dev | head -1 | xargs cat)
agent-browser navigate "http://localhost:8790/auth/login?token=$TOKEN"
agent-browser navigate "http://localhost:8790/<route>"
agent-browser screenshot <path>.png
```

> Do **not** start vite. Cookie and app must share the same origin (`:8790`).

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
