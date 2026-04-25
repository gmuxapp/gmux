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

## Adding tests

- Reach for `gotoTestSession(page)` to navigate. It handles auth,
  store warm-up, navigation, and waits for the terminal.
- Use `__gmuxTerm` for assertions about terminal state. Don't poke
  at xterm internals without a hook; add one and document it here.
- The test session is `bash -c 'echo READY; while true; do sleep 60; done'`
  with `cwd=$tmpDir/workspace`. Don't depend on anything more
  specific than that without changing the harness.
