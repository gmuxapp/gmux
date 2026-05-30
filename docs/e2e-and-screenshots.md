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

The sandbox has no display, so `--headed` Playwright doesn't work. Use the dev server instead:

**Step 1** — Start the frontend dev server (Option B: frontend-only against the real daemon on `:8790`):
```bash
cd projects/james/james-gmux/apps/gmux-web
npx vite &
# Vite starts on :5173 and proxies /v1, /auth, /ws to localhost:8790
```

The host gmuxd is already running on `:8790` with a real token at `~/.local/state/gmux/auth-token`.

**Step 2** — Navigate and screenshot:
```bash
TOKEN=$(cat /home/agent/.local/state/gmux/auth-token)
agent-browser navigate "http://127.0.0.1:8790/auth/login?token=$TOKEN"
agent-browser navigate "http://127.0.0.1:8790"         # or whatever URL you want
agent-browser screenshot tasks/james-gmux/ss-01-home.png
```

To screenshot the test-branch build specifically, start vite pointing at a test gmuxd (see below).

### Serving the test-branch build for screenshots

The e2e test suite leaves a live gmuxd running only for the duration of the test run. To get a persistent instance from the `test/integration` branch build for screenshots:

**Step 1** — Start the dev frontend against the host's running gmuxd:
```bash
cd projects/james/james-gmux
# The branch's built frontend is already in bin/gmuxd (embedded)
# OR serve it via vite to get the test-branch bundle:
E2E_SKIP_BUILD=1 npx playwright test --config e2e/playwright.config.ts \
  e2e/tests/harness.spec.ts   # runs global-setup, leaves daemon alive briefly
```

Actually simpler: **just run `npx vite` from the branch** — it serves the in-tree source against the host's gmuxd on `:8790`, which is always running.

```bash
cd projects/james/james-gmux/apps/gmux-web
npx vite
# Open http://127.0.0.1:5173 — serves test/integration frontend, real data
```

Then screenshot via `agent-browser`:
```bash
TOKEN=$(cat /home/agent/.local/state/gmux/auth-token)
agent-browser navigate "http://127.0.0.1:5173/auth/login?token=$TOKEN"
agent-browser navigate "http://127.0.0.1:5173/test-project"   # or any route
agent-browser screenshot tasks/james-gmux/ss-01-description.png
```

### To screenshot with mock data (no live daemon needed)
```bash
cd projects/james/james-gmux/apps/gmux-web
npx vite &
agent-browser navigate "http://127.0.0.1:5173/?mock"
agent-browser screenshot tasks/james-gmux/ss-mock.png
```
Note: mock mode hides some interactive chrome (close buttons, file tree actions are CSS-hidden).

---

## Caveats

- **`--headed` Playwright fails** in the sandbox — no display. Don't try it.
- **`agent-browser` uses a persistent Chromium process** — `agent-browser close` to restart it with different flags.
- **gmuxd token** for the dev instance is always at `/home/agent/.local/state/gmux/auth-token` — this is NOT the e2e test token.
- **The e2e test token** is always `e2e` padded to 64 chars (`e2e` + 61 zeros), but the test daemon only lives during a test run — it's gone once the suite finishes.
- **`E2E_SKIP_BUILD=1`** skips both vite build and `go build` — only use if the `bin/` binaries match the current source.
