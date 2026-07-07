---
title: Integration Tests
description: End-to-end tests that launch real tools through gmuxd.
---

Integration tests verify the full pipeline — from launching a real tool through gmuxd to observing session state transitions, file attribution, title derivation, and resume. They catch issues that unit tests can't: hook/file attribution timing, TUI input handling, trust prompts, and title derivation against real conversation files. (pi's discovery is file-watch based; claude and codex report state via authoritative agent hooks.)

## Running

```bash
# Build first — tests use the compiled binaries
./scripts/build.sh

# All integration tests
go test -tags integration -v -timeout 300s ./packages/adapter/adapters/

# One adapter
go test -tags integration -v -timeout 120s -run TestPi ./packages/adapter/adapters/

# One test
go test -tags integration -v -timeout 120s -run TestPiTurnAndTitle ./packages/adapter/adapters/
```

Tests are gated by the `integration` build tag — `go test ./...` never runs them. Each adapter's tests also skip automatically if the required binary isn't on `PATH`.

:::caution[API costs]
These tests make real API calls. A full run costs roughly $0.01–0.05 depending on model response length. Don't run them in a tight loop.
:::

## What's tested

Each adapter has a consistent set of tests:

| Test | Verifies |
|------|----------|
| `TurnAndTitle` | Send a message → attribution (slug set) → title from first user message |
| `SecondTurnKeepsTitle` | Send two messages → title stays as first message |
| `Resumability` | Send message → kill → session becomes resumable with a resume command → resume → alive again with same title |
| `NameOverridesTitle` (pi only) | Use `/name` command → title updates to explicit name |

Shell has lifecycle tests (`WSInput`, `Kill`, `KillFish`, `Restart`) covering the WebSocket → PTY input pipeline and kill/restart semantics. A scrollback suite (`TestScrollback*`) verifies persisted scrollback captures conversation content and omits overwritten spinner frames.

## Test harness

Tests use a shared harness in `packages/adapter/adapters/testutil/`. The key components:

### `StartGmuxd(t)`

Launches an isolated gmuxd instance:
- Random free port, written into a temp `host.toml` (isolated `XDG_CONFIG_HOME`)
- Temp state dir (`XDG_STATE_HOME`) and temp socket dir (`GMUX_SOCKET_DIR`)
- HTTP calls go over the daemon's Unix socket, bypassing the TCP listener's token auth
- `PATH` includes the built `bin/gmux` binary
- Cleaned up automatically when the test ends

Launching and driving sessions goes through `g.Launch`, `g.Kill`, `g.Resume`, `g.Restart`, `g.Dismiss`.

### `ConnectSession(sessionID)`

Opens a WebSocket directly to the runner's Unix socket (bypassing gmuxd's WS proxy). Sends an initial resize message so TUI apps render properly. Returns `(send, close)`; cleanup is automatic via `t.Cleanup`, so the close function can usually be ignored.

### Polling helpers

| Helper | What it does |
|--------|-------------|
| `WaitForSession(id, pred, timeout, desc)` | Polls `GET /v1/sessions` until the predicate matches |
| `WaitForOutput(sessionID, timeout)` | Polls scrollback until the TUI has rendered (returns the text) |
| `WaitForScrollback(socketPath, substr, timeout)` | Polls scrollback for a specific string |
| `ReadScrollback(t, socketPath)` | One-shot scrollback read |

## Writing a test for a new adapter

Follow the pattern in the existing test files:

```go
//go:build integration

package adapters

func TestMyAppTurnAndTitle(t *testing.T) {
    testutil.RequireBinary(t, "myapp")

    g := testutil.StartGmuxd(t)
    cwd := t.TempDir()

    sess := g.Launch([]string{"myapp"}, cwd)
    send, _ := g.ConnectSession(sess.ID)
    g.WaitForOutput(sess.ID, 15*time.Second)

    // Handle any trust/setup prompts your tool shows.
    // send("\r")

    // Send input and wait for the tool to process it.
    time.Sleep(2 * time.Second)
    send("say hi\r")

    // Wait for attribution (slug is derived from the conversation title).
    g.WaitForSession(sess.ID, func(s testutil.Session) bool {
        return s.Slug != ""
    }, 60*time.Second, "attribution (slug)")

    // Verify title.
    updated := g.WaitForSession(sess.ID, func(s testutil.Session) bool {
        return s.Title != "" && s.Title != "myapp"
    }, 15*time.Second, "title")
    t.Logf("title=%q", updated.Title)
}
```

### Things to watch for

- **Trust prompts.** Claude Code and Codex both ask "do you trust this directory?" on first launch in a new workspace. Dismiss them by waiting for `"trust"` in the scrollback, then sending `\r`.
- **TUI readiness.** Ink-based TUIs (pi, codex) need a moment after rendering before they accept input. A 2-second sleep after `WaitForOutput` is usually enough.
- **Batch file writes.** Some tools write user + assistant messages in one batch after the turn completes (pi does this). You can't reliably observe transient `working=true` status via polling — wait for the final state instead.
- **Hook-driven adapters attribute fast.** Claude and Codex report session identity through an injected hook (ADR 0010/0011), so attribution appears as soon as the hook fires — no file-watcher race. The long attribution timeout is only needed for file-watch adapters like pi.
- **Not every adapter needs these tests.** Shell and editor sessions have no API cost and are covered by cheaper lifecycle tests; the API-cost suite is for agent adapters.

## Related docs

- [Writing an Adapter](/develop/writing-adapters) — adapter implementation recipe
- [Adapter Architecture](/develop/adapter-architecture) — runtime model
- [State Management](/develop/state-management) — how session state flows
