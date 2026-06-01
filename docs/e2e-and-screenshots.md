# gmux: E2E Tests & Screenshot Verification

## E2E Tests (Playwright)

### Run the full suite
```bash
cd projects/james/james-gmux
pnpm test:e2e
# or equivalently:
npx playwright test --config e2e/playwright.config.ts
```

The `global-setup.ts` does all isolation automatically:
- Builds the frontend + Go binaries (unless `E2E_SKIP_BUILD=1`)
- Allocates a free port
- Spins up an isolated `gmuxd run` with a fresh tmpdir for HOME, state, config, and sockets
- Seeds `projects.json` with a `test-project` entry pointing at a workspace dir
- Writes a `host.toml` with the allocated port and devcontainer/tailscale discovery disabled
- Sets `GMUX_CONFIG_DIR`, `XDG_CONFIG_HOME`, `XDG_STATE_HOME`, `HOME`, `GMUX_SOCKET_DIR`
- Spawns a `bash -c 'echo READY; while ...'` session as the test session

### Skip the build (fast iteration)
```bash
E2E_SKIP_BUILD=1 npx playwright test --config e2e/playwright.config.ts
```
Only valid if binaries are already current. If Go or frontend sources changed, drop `E2E_SKIP_BUILD`.

### Run a single spec
```bash
E2E_SKIP_BUILD=1 npx playwright test --config e2e/playwright.config.ts e2e/tests/terminal-scroll.spec.ts
```

### Run a single test by name
```bash
E2E_SKIP_BUILD=1 npx playwright test --config e2e/playwright.config.ts --grep "stays at bottom"
```

### Key env vars the suite sets (set these manually if you need to call helpers outside Playwright)
| Var | What it is |
|---|---|
| `GMUXD_TEST_PORT` | Port gmuxd is listening on |
| `GMUX_TEST_TOKEN` | Auth token (`e2e` + 61 zeros) |
| `GMUX_TEST_SESSION_ID` | Session ID of the test shell session |
| `GMUX_TEST_WORKSPACE` | Path to the test workspace dir (where files can be seeded) |
| `GMUX_TEST_HOME` | Fake HOME the daemon uses (for conversation fixtures) |

---

## Screenshots for visual verification

### The right way: dev server + agent-browser

The sandbox has no display, so `--headed` Playwright doesn't work. Start the vite
dev server and use `agent-browser` instead.

**Step 1** — Start vite against the already-running daemon at `:8790`:
```bash
cd projects/james/james-gmux/apps/gmux-web
npx vite &
# Vite starts on :5173 and proxies /v1, /auth, /ws to localhost:8790
```

**Step 2** — Find the auth token (path depends on which daemon is running):
```bash
TOKEN=$(find ~/.local/state -name 'auth-token' -path '*/gmux*' 2>/dev/null | head -1 | xargs cat)
```

**Step 3** — Log in via **localhost:5173** (not 8790):
```bash
agent-browser navigate "http://localhost:5173/auth/login?token=$TOKEN"
# Browser lands on http://localhost:5173/ — cookie is now set for port 5173
```

**Step 4** — Navigate and screenshot:
```bash
agent-browser navigate "http://localhost:5173/<slug>/_md/AGENTS.md"  # or any route
agent-browser screenshot tasks/james-gmux/ss-01-description.png
```

Replace `<slug>` with the project slug from `projects.json` — see
`docs/running-dev-frontend.md` for how to find it.

### To screenshot with mock data (no live daemon needed)
```bash
cd projects/james/james-gmux/apps/gmux-web
npx vite &
agent-browser navigate "http://localhost:5173/?mock"
agent-browser screenshot tasks/james-gmux/ss-mock.png
```
Note: mock mode hides some interactive chrome (close buttons, file tree actions are CSS-hidden).

---

## Caveats

- **`--headed` Playwright fails** in the sandbox — no display. Don't try it.
- **`agent-browser` uses a persistent Chromium process** — `agent-browser close` to restart it with different flags.
- **Use `localhost`, not `127.0.0.1`** — vite binds to `[::1]` (IPv6). `127.0.0.1` is IPv4 and gets `ERR_CONNECTION_REFUSED`.
- **Log in via port 5173, not 8790** — the auth cookie is origin-scoped. Logging in at `:8790` sets a cookie for `:8790`; the browser at `:5173` stays unauthenticated.
- **gmuxd token path varies** — use `find ~/.local/state -name 'auth-token' -path '*/gmux*' 2>/dev/null` rather than hardcoding the path. For the standard dev setup it is at `/home/agent/.local/state/gmux-dev/state/gmux/auth-token`.
- **The e2e test token** is always `e2e` padded to 64 chars (`e2e` + 61 zeros), but the test daemon only lives during a test run — it's gone once the suite finishes.
- **`E2E_SKIP_BUILD=1`** skips both vite build and `go build` — only use if the `bin/` binaries match the current source.
