---
title: Writing an Adapter
description: How to add first-class gmux support for a new tool.
---

An adapter is a single Go file that teaches gmux how to work with a specific tool. It lives in `packages/adapter/adapters/` and is compiled into both `gmux` and `gmuxd`.

Read this page if you are adding support for a new tool. If you want the system-level overview first, see [Adapter Architecture](/develop/adapter-architecture). This page stays focused on the implementation recipe.

## Minimal adapter

Create `packages/adapter/adapters/myapp.go`:

```go
package adapters

import (
    "path/filepath"

    "github.com/gmuxapp/gmux/packages/adapter"
)

func init() {
    All = append(All, &MyApp{})
}

type MyApp struct{}

func (m *MyApp) Name() string { return "myapp" }
func (m *MyApp) Discover() bool { return true }

func (m *MyApp) Match(cmd []string) bool {
    for _, arg := range cmd {
        if filepath.Base(arg) == "myapp" {
            return true
        }
        if arg == "--" { break }
    }
    return false
}

func (m *MyApp) Env(_ adapter.EnvContext) []string { return nil }

func (m *MyApp) Monitor(output []byte) *adapter.Event { return nil }
```

That's enough for a valid adapter. It:

- reports whether the tool is available on this machine with `Discover()`
- activates when the command matches `myapp`
- contributes no extra environment yet
- reports no events yet
- is available for richer optional capabilities later

Write tests in `myapp_test.go` alongside it.

## Optional launch menu support

If the adapter should appear in the UI launch menu, implement `Launchable` on the same struct:

```go
type Launchable interface {
    Launchers() []Launcher
}

func (m *MyApp) Launchers() []adapter.Launcher {
    return []adapter.Launcher{{
        ID:          "myapp",
        Label:       "MyApp",
        Command:     []string{"myapp"},
        Description: "My tool",
    }}
}
```

`gmuxd` derives the launch menu from the compiled adapter set by checking which adapters implement `Launchable`. It then filters that menu using the adapter's required `Discover()` method.

Adapters may expose zero, one, or many launch presets. The built-in shell fallback also implements `Launchable`, so shell appears in the menu without a separate special registry.

## The base interface

Every adapter implements five methods:

```go
type Adapter interface {
    Name() string
    Discover() bool
    Match(command []string) bool
    Env(ctx EnvContext) []string
    Monitor(output []byte) *Event
}
```

**`Name()`** returns a short identifier like `"pi"` or `"myapp"`.

**`Discover()`** reports whether the backing tool is available on the current machine. `gmuxd` runs this in parallel for all compiled adapters during startup and only includes launchers from adapters whose discovery succeeds. Keep it cheap and deterministic. For example, shell returns `true`, while pi checks that the `pi` binary is on `PATH` (`exec.LookPath`) without executing it — running the binary would be too slow.

**`Match(cmd)`** receives the full command array and decides whether this adapter should handle it. Match on `filepath.Base(arg)` so full paths and wrappers work. Stop scanning at `"--"`.

**`Env(ctx)`** returns extra environment variables for the child. The runner already sets `GMUX`, `GMUX_SOCKET`, `GMUX_SESSION_ID`, `GMUX_ADAPTER`, and `GMUX_RUNNER_VERSION`. Most adapters return `nil`.

**`Monitor(output)`** receives raw PTY bytes on every read. Return a `*Event` when something meaningful happens, `nil` otherwise. This runs frequently, so keep it cheap. The built-in agent adapters (pi/claude/codex) return `nil` here — their status comes from the agent hook, not PTY parsing.

### Important: adapters do not rewrite the user's command

The command the user typed is what runs. `Env()` can add environment variables; adapters don't inject flags or wrap the process. The only sanctioned argv change is the hook-injection seam (`SessionExtender`/`SessionHookCommand`, below), which the runner drives.

## Reporting events

Return an `Event` from `Monitor()` to update the session. Zero/nil fields are no-ops — only explicitly set fields are applied:

```go
type Event struct {
    Title  string  // non-empty: update the adapter title
    Status *Status // non-nil: update status; &Status{} clears it
    Unread *bool   // non-nil: set or clear the unread flag (adapter.BoolPtr)
    Cwd    string  // non-empty: update the session's canonical directory
}

type Status struct {
    Working bool // true while the tool is busy (pulsing dot)
    Error   bool // true on a retryable error (red dot)
}
```

Status carries only booleans; any display text is derived by the frontend from these plus the exit code.

## Adapter resolution

When `gmux` launches a command, adapters are tried in this order:

1. **`GMUX_ADAPTER` env var** — explicit override, validated against `Match()`. If the named adapter doesn't match the command (or is unknown), resolution falls through — this prevents a `GMUX_ADAPTER` leaked from a parent session from forcing the wrong adapter.
2. **Registered adapters** — `Match()` in registration order; first match wins
3. **Shell fallback** — always matches

A false negative is low cost because the shell adapter still handles the session.

## Optional capabilities

The base interface covers command matching, env injection, and PTY monitoring. Additional opt-in interfaces add richer integration. Implement them on the same struct; `gmux` or `gmuxd` discover them via type assertion.

For the runtime behavior behind these interfaces, see [Adapter Architecture](/develop/adapter-architecture).

### `Launchable`

```go
type Launchable interface {
    Launchers() []Launcher
}
```

Implement this if the adapter should contribute launch presets to the UI.

- return one launcher for a simple tool entry
- return multiple launchers if one adapter supports multiple presets
- return none by not implementing the interface at all
- remember that launch availability is controlled separately by the required `Discover()` method

### `ConversationDescriber`

```go
type ConversationDescriber interface {
    DescribeConversation(ref string) (*ConversationInfo, error)
}
```

Implement this if your tool persists conversations gmux should be able to inspect. Conversations are located by an opaque, adapter-scoped **ref** string: *you* pick the locator (the file-backed adapters use the transcript's absolute path; a database-backed tool would use a row key or UUID), and gmux never interprets it — it only stores refs and hands them back to you.

`DescribeConversation(ref)` resolves a ref to display metadata: ID, title, slug, cwd, created time, `LastActivity` (freshness — file-backed adapters report the transcript's mtime), and message count. Set `Ref` to the ref you were given so resume commands that embed a locator can use it.

### `ConversationSource`

```go
type ConversationSource interface {
    SnapshotConversations(sink ConversationSink)
    WatchConversations(ctx context.Context, sink ConversationSink) error
}
```

Implement this if your tool's conversations live in files (or anywhere else) that gmux should index for URL resolution and search. The adapter owns *how* it discovers them: `SnapshotConversations` reports everything that exists now (synchronous, at startup), and `WatchConversations` streams changes until `ctx` is cancelled. Both report opaque refs to a `ConversationSink` (`Upsert(ref)` / `Remove(ref)`); the daemon resolves each via your `DescribeConversation`.

File-backed adapters build both on `packages/adapter/filewatch`, a reusable recursive tree watcher, in a few lines each — see `pi.go`. A non-file source (e.g. a database) implements the same interface with a poller or subscription instead.

Note: live session state — title, status, and the held conversation — is **not** reported here. That flows authoritatively from the agent hook (`SessionExtender` for pi, `SessionHookCommand` for codex/claude); see below and [Adapter Architecture](/develop/adapter-architecture).

### `SessionExtender` / `SessionHookCommand`

These are *the* way an agent adapter reports authoritative session identity, titles, and turn status (ADR 0010/0011/0013; protocol in `docs/runner-hook-protocol.md`). The runner splices the hook into the launch argv:

- **`SessionExtender.ExtendCommand`** — injects an extension file into the agent's own extension mechanism (pi: `-e <ext.mjs>`).
- **`SessionHookCommand.HookCommand`** — injects config overrides that make the agent run `gmux __<agent>-hook` on lifecycle events (claude: `--settings`, codex: `-c hooks.…`).

Only opt in if gmux fully controls the argv — a shell-wrapped launch (`bash -c 'claude'`) can't be extended and simply runs without live state. `GMUX_NO_AGENT_HOOK` disables injection.

### `ConversationProber`

Optional. Lets the startup retention reconcile distinguish a *deleted* conversation from *unavailable* storage, so dead sessions are only retired when their conversation is genuinely gone (ADR 0016). The file-backed agent adapters implement it via the shared `ConversationGoneAtRoot` helper.

### `ConversationOpener`

```go
type ConversationOpener interface {
    OpenConversation(ref string) (io.ReadCloser, error)
}
```

Optional. Streams a conversation's raw, adapter-native content (e.g. the JSONL transcript) for derived consumers such as the fulltext search index. The file-backed adapters implement it as `os.Open(ref)`.

### `SessionRegistrar` / `SessionFinalizer`

Optional lifecycle callbacks: `OnRegister` runs at registration time (return a slug, write a state file — shell and editor use this), `OnDismiss` cleans up when a session is dismissed.

### `PassthroughDetector`

Optional. Marks one-shot invocations (e.g. `pi update`, `pi --version`) that should be exec'd directly instead of wrapped in a session. Implement this for CLI tools with management subcommands.

### `Resumer`

```go
type Resumer interface {
    ResumeCommand(info *ConversationInfo) []string
    CanResume(ref string) bool
}
```

Implement this if your tool supports resuming previous sessions.

- `CanResume(ref)` filters out invalid or empty conversations
- `ResumeCommand()` tells gmux how to resume a valid session

All dead sessions are resumable. When a session exits, gmuxd checks whether the adapter implements `Resumer` **and** the session has a recorded conversation ref (`ConversationRef`, reported by the agent hook). If so, the session's command is replaced with the adapter's resume command (e.g. `["claude", "--resume", "abc"]`). If not, the original launch command is kept as-is, so "resume" simply re-runs the command in the same working directory.

This means adapters that don't implement `Resumer` still get resume for free: the user clicks resume, a new session starts with the same command and cwd. This is the right behavior for simple tools. Only implement `Resumer` when your tool has native resume support that you want to use instead.

### `CommandTitler`

```go
type CommandTitler interface {
    CommandTitle(command []string) string
}
```

Implement this to customize the fallback title derived from the command array. Without it, the store joins the full command (e.g. `pytest -x`). Adapters that use resume commands implement this to avoid titles like `codex resume 019cfb54-…`; the editor adapter shows the edited file's basename.

This only matters when no adapter or shell title has been set yet, which is rare for agent adapters (titles come from the agent hook or stored conversation) but common for plain shell sessions.

### Capability composition

An adapter implements only what it needs:

| Adapter | Base | Launchable | ConversationDescriber | ConversationSource | Resumer | Other |
|---------|------|------------|-----------------------|--------------------|---------|-------|
| Shell | ✓ | ✓ | ✓ | — | ✓ | CommandTitler, SessionRegistrar, SessionFinalizer |
| Editor | ✓ | ✓ | — | — | — | CommandTitler, SessionRegistrar |
| Claude | ✓ | ✓ | ✓ | ✓ | ✓ | ConversationOpener, ConversationProber, SessionHookCommand |
| Codex | ✓ | ✓ | ✓ | ✓ | ✓ | ConversationOpener, ConversationProber, SessionHookCommand |
| Pi | ✓ | ✓ | ✓ | ✓ | ✓ | ConversationOpener, ConversationProber, SessionExtender, PassthroughDetector |

Shell writes a small JSON state file per session under `~/.local/state/gmux/shell-sessions/` and resumes by launching `$SHELL` in the original cwd. The editor adapter (backing `gmux edit`) is a good minimal non-agent example: it matches an internal sentinel command and is ephemeral (auto-dismissed on close).

A house pattern worth copying: assert your capabilities at compile time — `var _ adapter.Resumer = (*MyApp)(nil)`.

## Testing

Write unit tests in `myapp_test.go` next to your adapter. Test `Match()` with different command shapes and `Monitor()` with representative output (if it does anything).

If the adapter implements `Launchable`, test the returned launcher IDs, labels, and commands.

For adapters with `ConversationDescriber`, create temp conversations in your tool's format and verify `DescribeConversation()` extracts the expected metadata.

For the full end-to-end pipeline (launch → file attribution → title → resume), add integration tests that run real processes through gmuxd. See [Integration Tests](/develop/integration-tests) for the harness, patterns, and gotchas.

## Related docs

- [Adapters](/adapters) — user-facing overview
- [Adapter Architecture](/develop/adapter-architecture) — runtime model
- [Integration Tests](/develop/integration-tests) — end-to-end testing with real tools
- [pi](/integrations/pi) — concrete built-in example
