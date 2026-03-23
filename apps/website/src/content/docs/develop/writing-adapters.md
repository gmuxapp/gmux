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

func (m *MyApp) Monitor(output []byte) *adapter.Status { return nil }
```

That's enough for a valid adapter. It:

- reports whether the tool is available on this machine with `Discover()`
- activates when the command matches `myapp`
- contributes no extra environment yet
- reports no custom status yet
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
    Monitor(output []byte) *Status
}
```

**`Name()`** returns a short identifier like `"pi"` or `"myapp"`.

**`Discover()`** reports whether the backing tool is available on the current machine. `gmuxd` runs this in parallel for all compiled adapters during startup and only includes launchers from adapters whose discovery succeeds. Keep it cheap and deterministic. For example, shell returns `true`, while pi runs `pi --version` and checks whether it succeeds.

**`Match(cmd)`** receives the full command array and decides whether this adapter should handle it. Match on `filepath.Base(arg)` so full paths and wrappers work. Stop scanning at `"--"`.

**`Env(ctx)`** returns extra environment variables for the child. The runner already sets `GMUX`, `GMUX_SOCKET`, `GMUX_SESSION_ID`, `GMUX_ADAPTER`, and `GMUX_VERSION`. Most adapters return `nil`.

**`Monitor(output)`** receives raw PTY bytes on every read. Return a `*Status` when something meaningful happens, `nil` otherwise. This runs frequently, so keep it cheap.

### Important: adapters never modify the command

The command the user typed is exactly what runs. `Env()` can add environment variables, but adapters do not inject flags, wrap the process, or rewrite argv.

## Reporting status

Return a `Status` from `Monitor()` to update the sidebar:

```go
type Status struct {
    Label   string // Short text: "thinking", "3/10 passed"
    Working bool   // true while the tool is busy (shows pulsing cyan dot)
    Title   string // Optional: if set, updates the session title
}
```

`Working` controls the sidebar dot (cyan pulse when true, hidden when false). `Label` appears as secondary text below the session title. Set `Title` if the PTY output should rename the session.

## Adapter resolution

When `gmux` launches a command, adapters are tried in this order:

1. **`GMUX_ADAPTER` env var** — explicit override
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

### `SessionFiler`

```go
type SessionFiler interface {
    SessionRootDir() string
    SessionDir(cwd string) string
    ParseSessionFile(path string) (*SessionFileInfo, error)
}
```

Implement this if your tool writes session or conversation files to disk.

- `SessionRootDir()` returns the root containing all session directories
- `SessionDir(cwd)` returns the directory for a particular working directory
- `ParseSessionFile(path)` extracts display metadata from one file

### `FileMonitor`

```go
type FileMonitor interface {
    ParseNewLines(lines []string, filePath string) []FileEvent
}
```

Implement this if appended file content should update the live sidebar. Return `FileEvent` values with a `Title` or `Status`.

The `filePath` parameter is the attributed session file being monitored. Adapters can read it to inspect preceding context when needed (e.g. counting consecutive errors to detect exhausted retries). Pass `""` in tests that don't need file context.

### `FileAttributor`

```go
type FileAttributor interface {
    AttributeFile(filePath string, candidates []FileCandidate) string
}
```

Implement this if multiple live sessions can share the same watch directory. The daemon calls this to determine which session owns a newly written file. Each candidate carries `SessionID`, `Cwd`, `StartedAt`, and `Scrollback` (recent terminal text). Return the matching session ID, or `""` to reject the file.

Common strategies:
- **Metadata matching** (codex, claude): parse the file header for cwd + timestamp, pick the candidate with the closest `StartedAt`
- **Content similarity** (pi): compare file text against each candidate's `Scrollback`

Without this interface, single-candidate directories use trivial attribution and multi-candidate directories fall back to the first candidate.

### `Resumer`

```go
type Resumer interface {
    ResumeCommand(info *SessionFileInfo) []string
    CanResume(path string) bool
}
```

Implement this if your tool supports resuming previous sessions.

- `CanResume()` filters out invalid or empty files
- `ResumeCommand()` tells gmux how to resume a valid session

A session only becomes resumable once a file is attributed to it (setting `resume_key`). Sessions that exit before creating a file are treated as non-resumable — the original launch command would start a fresh session, not resume the old one.

### `CommandTitler`

```go
type CommandTitler interface {
    CommandTitle(command []string) string
}
```

Implement this if your adapter needs custom fallback title display from the command array. Without it, the fallback title is the adapter name (e.g. "codex", "pi"). Shell implements this to show the full command with args (e.g. "pytest -x").

This only matters when no `adapter_title` or `shell_title` is set — which is rare for adapters that implement `FileMonitor` (titles come from file parsing) but common for plain shell sessions.

### Capability composition

An adapter implements only what it needs:

| Adapter | Base | Launchable | SessionFiler | FileMonitor | FileAttributor | Resumer |
|---------|------|------------|-------------|-------------|----------------|---------|
| Shell | ✓ | ✓ | — | — | — | — |
| Claude | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| Codex | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| Pi | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |

## Testing

Write unit tests in `myapp_test.go` next to your adapter. Test `Match()` with different command shapes and `Monitor()` with representative output.

If the adapter implements `Launchable`, test the returned launcher IDs, labels, and commands.

For adapters with `SessionFiler`, create temp files in your tool's format and verify `ParseSessionFile()` extracts the expected metadata.

For the full end-to-end pipeline (launch → file attribution → title → resume), add integration tests that run real processes through gmuxd. See [Integration Tests](/develop/integration-tests) for the harness, patterns, and gotchas.

## Related docs

- [Adapters](/adapters) — user-facing overview
- [Adapter Architecture](/develop/adapter-architecture) — runtime model
- [Integration Tests](/develop/integration-tests) — end-to-end testing with real tools
- [pi](/integrations/pi) — concrete built-in example
