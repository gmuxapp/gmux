# E2E Tests

Automated Playwright tests. These are distinct from manual agent verification — see [docs/agent-verification.md](agent-verification.md) for reproducing bugs and confirming fixes.

The suite manages its own isolated daemon. You don't need to start any server manually.

---

## Run the full suite

```bash
cd projects/james/james-gmux
pnpm test:e2e
# or equivalently:
npx playwright test --config e2e/playwright.config.ts
```

`global-setup.ts` handles all isolation automatically:

- Builds the frontend + Go binaries (unless `E2E_SKIP_BUILD=1`)
- Allocates a free port
- Spins up an isolated `gmuxd run` with a fresh tmpdir for HOME, state, config, and sockets
- Seeds `projects.json` with a `test-project` entry pointing at a workspace dir
- Writes a `host.toml` with the allocated port and devcontainer/tailscale discovery disabled
- Sets `GMUX_CONFIG_DIR`, `XDG_CONFIG_HOME`, `XDG_STATE_HOME`, `HOME`, `GMUX_SOCKET_DIR`
- Spawns a `bash -c 'echo READY; while ...'` session as the test session

---

## Skip the build (fast iteration)

```bash
E2E_SKIP_BUILD=1 npx playwright test --config e2e/playwright.config.ts
```

Only valid if binaries are already current. If Go or frontend sources changed, drop `E2E_SKIP_BUILD`.

---

## Run a single spec

```bash
E2E_SKIP_BUILD=1 npx playwright test --config e2e/playwright.config.ts e2e/tests/terminal-scroll.spec.ts
```

## Run a single test by name

```bash
E2E_SKIP_BUILD=1 npx playwright test --config e2e/playwright.config.ts --grep "stays at bottom"
```

---

## Test data

The suite spins up a fresh, isolated daemon with no sessions or conversation history. For tests that exercise session state, conversation rendering, or scrollback behaviour, seed fixtures from real prod session data rather than inventing synthetic values. Copy prod session data into `GMUX_TEST_HOME` in global setup, or seed project files via `GMUX_TEST_WORKSPACE`. Real data catches edge cases that synthetic fixtures miss.

---

## Key env vars

| Var | What it is |
|---|---|
| `GMUXD_TEST_PORT` | Port gmuxd is listening on |
| `GMUX_TEST_TOKEN` | Auth token (`e2e` + 61 zeros) |
| `GMUX_TEST_SESSION_ID` | Session ID of the test shell session |
| `GMUX_TEST_WORKSPACE` | Path to the test workspace dir (where files can be seeded) |
| `GMUX_TEST_HOME` | Fake HOME the daemon uses (for conversation fixtures) |

---

## Caveats

- **`--headed` Playwright fails** in the sandbox — no display. Don't try it.
- **`E2E_SKIP_BUILD=1`** skips both vite build and `go build` — only use if the `bin/` binaries match the current source.
- **The e2e test token** is always `e2e` padded to 64 chars (`e2e` + 61 zeros), but the test daemon only lives during a test run.
