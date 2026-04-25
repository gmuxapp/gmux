# e2e harness

Playwright suite that drives a real `gmuxd` + `gmux` against a real
chromium. CI runs it on every PR via the `E2E (Playwright)` job in
`.github/workflows/ci.yml`. Locally: `pnpm test:e2e`.

## Isolation contract

The harness must produce a daemon whose state is fully derived from
this directory's setup, with no leakage from the operator's
environment. Three paths can leak and have to stay plugged:

1. **Socket discovery.** Sockets live in `$GMUX_SOCKET_DIR` (default
   `/tmp/gmux-sessions`). `gmuxd start` re-execs to daemonize and
   strips every `GMUX_*` env var on the way, so a `start`-launched
   daemon falls back to the operator's `/tmp/gmux-sessions`.
   **Fix:** spawn `gmuxd run` directly from `global-setup.ts`. Keeps
   the env intact and gives us a real PID for teardown. Don't switch
   back to `start` without first solving the env-strip.

2. **Adapter session-file scanning.** The pi/claude/codex/shell
   adapters resolve their session roots via `os.UserHomeDir()`, so a
   real `$HOME` makes them index every conversation the operator
   has on disk. **Fix:** override `HOME` to a fresh dir under
   `tmpDir`.

3. **Devcontainer discovery.** Reads the host's docker socket and
   would enumerate operator containers. **Fix:** seed
   `[discovery] devcontainers = false` in `host.toml`. Tailscale is
   off by default but pinned explicitly in the same file so a
   future default flip doesn't silently re-enable it for tests.

`tests/harness.spec.ts` asserts the socket-discovery contract
directly (one alive session, no peers, expected cwd). It does **not**
catch HOME regressions: the symptom there (operator pi/claude
conversations entering the index) doesn't currently surface in a
public API in a way the harness can probe. HOME isolation is
defense-in-depth for future tests that spawn agents like `pi` or
`claude` (which would otherwise read the operator's real
`~/.pi/agent/` etc.); audit it manually when touching this setup.

## Adapter-session fixtures

`fixtures.ts` knows how to write minimally valid JSONL session files
for pi/claude/codex into `$GMUX_TEST_HOME` (set by global-setup). Two
specs use it:

- `tests/conversation-fixtures.spec.ts` is a drift-detector. It
  pre-seeds one fixture per adapter via global-setup *before* gmuxd
  starts, then asserts each is reachable through `/v1/conversations/`
  once bootstrap completes. If a Go parser grows a new required
  field and the TS fixture lags, this spec fails first with a clear
  signal pointing at fixture validity, before anything else.
- `tests/conversation-discovery.spec.ts` exercises the watcher path:
  writes JSONL files at test time and asserts the index reflects
  them within 2s. Tight timeout intentionally: watcher-driven
  discovery is sub-second; longer would mask regression to a
  periodic scan. The harness's only alive session is `shell`, so
  these tests implicitly verify always-on root watches — if root
  watches were ever re-gated on a live session of the same kind,
  the discovery tests would time out.

When adding a test that needs a session file on disk, prefer
`writeFakeSession` over hand-crafted JSONL. Each call needs a unique
synthetic `cwd` so encoded paths and slugs don't collide between
tests; `uniqueFixture(kind, label)` in the discovery spec is a good
pattern.

## Test hooks on `window`

Two hooks are exposed by the app for the harness; both are
intentionally global because Playwright drives the page from the
outside:

- `__gmuxTerm` — the live xterm instance, exposed for assertions on
  rows/cols/scroll state.
- `__gmuxNavigateToSession(id)` — routes to a session by ID via the
  store's `navigateToSession`. Returns `false` until the store has
  loaded sessions and projects; the helper polls until it returns
  `true`. Needed because the home page no longer auto-selects.

Hooks should never gate or change product behavior. If a hook ever
needs to do more than read state or call an existing public API,
that's a sign the production API is missing something; fix the API
instead of growing the hook.

## API helpers

`helpers.ts` exposes two helpers for tests that talk to the daemon
directly (without a browser context):

- `apiGet<T>(urlPath)` — bearer-auth wrapper around `fetch` against
  `127.0.0.1:${GMUXD_TEST_PORT}`. Returns `{ status, body }`. Body is
  parsed JSON or undefined for non-JSON responses (404 etc.).
- `pollUntil(fn, opts)` — polls until `fn` returns truthy or the
  default 2s ceiling elapses. Use for assertions that wait on async
  work ("file written, index updated, API reflects it"). Tight
  default timeout is intentional: a longer wait masks regressions
  that re-introduce polling somewhere in the stack.

## Adding tests

- Reach for `gotoTestSession(page)` to navigate. It handles auth,
  store warm-up, navigation, and waits for the terminal.
- Use `__gmuxTerm` for assertions about terminal state. Don't poke
  at xterm internals without a hook; add one and document it here.
- The test session is `bash -c 'echo READY; while true; do sleep 60; done'`
  with `cwd=$tmpDir/workspace`. Don't depend on anything more
  specific than that without changing the harness.
- For tests that exercise daemon behavior without a browser (HTTP
  endpoints, watcher paths), drop into `apiGet` + `pollUntil`. No
  need to touch a `Page` if nothing in the UI changes.
