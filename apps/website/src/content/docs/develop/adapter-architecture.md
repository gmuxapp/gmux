---
title: Adapter Architecture
description: How adapters, gmux, and gmuxd work together at runtime.
---

This page describes the runtime architecture behind gmux adapters: which component does what, how sessions are discovered, how file-backed integrations work, and how children can report status back to gmux.

Read this page if you are working on gmux internals, debugging adapter behavior, or trying to understand how a launched process becomes a live or resumable sidebar entry.

If you want the user-facing overview, see [Adapters](/adapters). If you want to add support for a new tool, see [Writing an Adapter](/develop/writing-adapters).

## Two processes, one adapter system

Adapters are defined once in `packages/adapter` and used by both `gmux` and `gmuxd`.

- **`gmux`** is per-session. It launches the child, owns the PTY, injects environment variables, and interprets live output.
- **`gmuxd`** is per-machine. It discovers running sessions, watches adapter-owned files, and surfaces resumable sessions.

That split is why the adapter system is a set of small interfaces instead of one giant one.

## Responsibility split

| Concern | Component | How |
|---|---|---|
| Adapter availability detection | `gmuxd` | `Adapter.Discover()` |
| Command matching | `gmux` | `Adapter.Match()` |
| Child env injection | `gmux` | `Adapter.Env()` |
| Turn state (default model) | `gmux` | launch/exit lifecycle + OSC 133 prompt marks (non-hook-driven adapters) |
| Child self-report API | `gmux` | Unix socket HTTP endpoints |
| Launch menu discovery | `gmuxd` | `Launchable` + compiled adapter set |
| Conversation metadata resolution | `gmuxd` | `ConversationDescriber` |
| Live session state | runner → `gmuxd` | agent hook (`SessionExtender` / `SessionHookCommand`) |
| Conversation discovery | `gmuxd` | `ConversationSource` (snapshot + watch) |
| Resume command derivation | `gmuxd` | `ConversationDescriber` + `Resumer` on the session's `ConversationRef` |

## Launch lifecycle

When you run a command through `gmux`:

1. `gmux` resolves the adapter
   - `GMUX_ADAPTER=<name>` override, if set
   - otherwise first matching registered adapter wins
   - otherwise shell fallback
2. `gmux` starts the child under a PTY
3. `gmux` injects the standard `GMUX_*` environment variables
4. `gmux` applies the default turn model for non-hook-driven adapters (active from launch; OSC 133 prompt marks upgrade to per-command turns)
5. `gmux` serves the session on its Unix socket (`/meta`, `/events`, terminal attach, child callbacks)
6. `gmuxd` discovers the runner socket (the runner registers itself; a periodic scan is the fallback), queries `/meta`, and subscribes to `/events`

Step 0, before any of this: `PassthroughDetector` adapters can mark one-shot invocations (e.g. `pi update`) that gmux execs directly without creating a session at all.

The user's command semantics are preserved: adapters never change *what* runs. But for hooked adapters the runner splices the gmux agent hook into the launch argv (`SessionExtender` / `SessionHookCommand`), and `GMUX_NO_AGENT_HOOK=1` disables that injection. Per-session sockets live under `~/.local/state/gmux/run/sessions` (override: `GMUX_SOCKET_DIR`).

## Adapter resolution

Adapter selection happens entirely in `gmux`:

1. **Explicit override**: `GMUX_ADAPTER=<name>`, honored only if that adapter's `Match()` accepts the command (a leaked env var can't hijack an unrelated command)
2. **Registered adapters in order**: first `Match()` wins
3. **Shell fallback**: always matches, always last

This keeps matching cheap and predictable. A false negative is low-cost because the shell adapter still gives basic behavior.

## Adapter discovery and available launchers

Every adapter implements a required discovery probe:

```go
type Adapter interface {
    Name() string
    Discover() bool
    Match(command []string) bool
    Env(ctx EnvContext) []string
}
```

`gmuxd` runs `Discover()` for every compiled adapter in parallel during startup.
That tells gmux which adapters are actually usable on the current machine.

For the built-in adapters:

- **shell** always returns `true`
- **pi**, **claude**, **codex** check for their binary on `PATH` (`exec.LookPath`; executing the binary would be too slow)
- **editor** returns `true` when a usable fallback editor resolves

## Launchers and `Launchable`

Launch menu entries are still derived from adapter instances instead of a parallel global launcher registry.

Adapters that want to appear in the UI implement:

```go
type Launchable interface {
    Launchers() []Launcher
}
```

`gmuxd` aggregates launchers from the compiled adapter set by checking which adapters implement `Launchable`, then filters that list based on each adapter's `Discover()` result.

A few important consequences:

- launch menu support is optional, like other adapter capabilities
- adapter availability is mandatory, because every adapter must implement `Discover()`
- one adapter can expose zero, one, or many launch presets
- the shell fallback also implements `Launchable`, so shell appears in the UI without a separate special-case launcher registry
- unavailable launchers are omitted from the launch config entirely

The current built-in launcher ordering is simple:

- non-fallback adapters contribute launchers in adapter registration order
- shell is appended last

## Conversation-storing adapters

Some tools persist conversations (pi/claude/codex write JSONL files; a future tool might use a database). Those integrations use optional capabilities discovered by `gmuxd`, all keyed by an opaque, adapter-scoped **conversation ref** (ADR 0022): the adapter picks the locator — file-backed adapters use the transcript's absolute path — and everything above the adapter just stores and round-trips the string.

### `ConversationDescriber`

```go
type ConversationDescriber interface {
    DescribeConversation(ref string) (*ConversationInfo, error)
}
```

Use this when a tool stores conversations that gmux should be able to inspect. `DescribeConversation(ref)` resolves a ref to display metadata: ID, title, cwd, created time, last activity, and message count. How the adapter reads its storage (JSONL file, database row) is private to the adapter — including freshness: file-backed adapters report `LastActivity` from the transcript's mtime.

### `ConversationSource`

```go
type ConversationSource interface {
    SnapshotConversations(sink ConversationSink)
    WatchConversations(ctx context.Context, sink ConversationSink) error
}
```

Use this so `gmuxd` can index your tool's conversations (for URL resolution and search). The adapter owns discovery: `SnapshotConversations` reports everything that exists now, `WatchConversations` streams changes until `ctx` ends. Both report refs to a `ConversationSink` (`Upsert(ref)` / `Remove(ref)`); the daemon resolves each via `DescribeConversation`. File-backed adapters build on the shared `filewatch` tree watcher; a database-backed adapter would poll or subscribe instead.

### `ConversationOpener`

```go
type ConversationOpener interface {
    OpenConversation(ref string) (io.ReadCloser, error)
}
```

Use this to let derived consumers (e.g. the fulltext search index) stream a conversation's raw, adapter-native content.

### `Resumer`

```go
type Resumer interface {
    ResumeCommand(info *ConversationInfo) []string
    CanResume(ref string) bool
}
```

Use this when a finished session can be resumed later.

- `CanResume(ref)` filters out invalid or empty conversations
- `ResumeCommand(info)` tells gmux how to resume the session when the user clicks it — if the command embeds a locator (pi's `--session <path>`), take it from `info.Ref`

## Live state and conversation discovery

These are two separate concerns, with two different owners.

### Live session state comes from the agent hook

Which running session holds which conversation — and its title and status —
is **not** inferred by the daemon. The runner injects a gmux agent hook
(`SessionExtender` for pi's `-e` extension; `SessionHookCommand` for codex/claude
hooks), and the agent reports the held conversation, title, and status
authoritatively over the runner socket (`POST /hook/event`; see
`docs/runner-hook-protocol.md` in the repo). `gmuxd` records the ref on the
session (`ConversationRef`); a `/resume` rebind to a different conversation is
just another hook report. When a tool can't be hooked, the session runs without
daemon-reported live state — there is no metadata-matching fallback.

### Conversation discovery via `ConversationSource`

Independently, `gmuxd` indexes every stored conversation — including dead
conversations with no running session — for URL resolution and search. Each
`ConversationDescriber` adapter also implements `ConversationSource`: it reports
a startup snapshot and then streams changes, and the daemon resolves each ref via
`DescribeConversation` into the index. File-backed adapters share the `filewatch`
tree watcher; a database-backed tool would poll or subscribe instead. There is
no daemon-global file monitor.

## Resumable sessions

Sessions transition seamlessly between alive and resumable states.

### Live → resumable transition

When a session exits, `gmuxd` checks whether its adapter implements `ConversationDescriber` + `Resumer` and whether the session has a recorded `ConversationRef` (reported by the agent hook). If so:

1. the resume command is derived from the adapter's `ResumeCommand()`
2. `command` is set to the resume command
3. `resumable` is derived automatically (`!alive && command present`)
4. the session appears in the sidebar as clickable to resume — no intermediate "exited" state

Sessions without a recorded conversation keep their original command, so "resume" re-runs it.

### Dead sessions survive daemon restarts

Dead sessions are not rediscovered from conversation files. Instead, dead sessions are rows in the SQLite database (`state.db`, ADR 0026) and survive daemon restarts by construction. Adapters own retention policy: each adapter reconciles its retained candidates and returns a disposition (retain, remove, or unknown). Dead-session scrollback is an evictable cache on disk. The conversations index (from `ConversationSource`) serves URL resolution and search, not sidebar entries.

For concrete examples, see [Claude Code](/integrations/claude-code), [Codex](/integrations/codex), or [pi](/integrations/pi).

## Child awareness protocol

Every child launched by `gmux` gets a small protocol for detecting gmux and reporting back.

### Environment variables

| Variable | Purpose |
|---|---|
| `GMUX` | Simple detection flag (`1`) |
| `GMUX_SOCKET` | Unix socket path for callbacks to the runner |
| `GMUX_SESSION_ID` | Unique session identifier |
| `GMUX_ADAPTER` | Name of the matched adapter |
| `GMUX_RUNNER_VERSION` | Version of the gmux runner hosting the session |
| `GMUX_SESSION_SOCK` | Same socket as `GMUX_SOCKET`; set only when an agent hook was injected — the socket the hook posts to (the hook contract is deliberately independent of the general child env) |

Most tools ignore these. gmux-aware tools, wrappers, or hooks can use them to integrate directly.

### Child-to-runner endpoints

Served by `gmux` on the session socket:

| Endpoint | Method | Purpose |
|---|---|---|
| `/meta` | `GET` | Read current session metadata |
| `/hook/event` | `POST` | Authoritative agent-hook events: conversation bind, title, slug, turn status |
| `/status` | `PUT` | Set or clear application status (`null` clears) |
| `/slug` | `PUT` | Set the session slug |
| `/events` | `GET` | Subscribe to live state changes (SSE) |

There is no child title endpoint — titles come from the hook or from OSC 0/2 title sequences, which the runner parses centrally for all sessions.

Example:

```bash
curl --unix-socket "$GMUX_SOCKET" http://localhost/status \
  -X PUT -H 'Content-Type: application/json' \
  -d '{"working":true}'
```

`Status` carries only `working`/`error` booleans; display text is derived in the frontend.

This is the escape hatch for tools that want native gmux integration without needing a custom PTY parser.

## Status sources

A session's displayed state can come from multiple places:

- the runner's default turn model for non-hook-driven adapters: active from launch, per-command turns once OSC 133 prompt marks are observed (shell: busy on command start, idle when the prompt returns), turn closed by process exit otherwise
- authoritative agent-hook reports (held file, title, status) for hook-driven adapters
- direct child callbacks via `PUT /status`

There is deliberately no per-byte PTY inference by adapter code; status sources are declarative (hooks, marks, lifecycle) so they cannot flicker on TUI redraws.

The important design point is that adapters do not own the whole session model. They contribute structured hints into a runner-owned session state that `gmuxd` then aggregates and serves.

## Built-in examples

- **Shell**: fallback adapter; contributes the default shell launcher, keeps per-session state files, and resumes as `$SHELL` in the original cwd (terminal title parsing is handled centrally in the runner, for all sessions)
- **Editor**: backs `gmux edit`; matches an internal sentinel command, ephemeral (auto-dismissed on close)
- **Claude Code**: hook-driven agent adapter (status/title/attribution via injected `--settings` hooks); conversation files feed discovery and `claude --resume`
- **Codex**: hook-driven agent adapter with date-nested conversation storage; resume via `codex resume`
- **pi**: agent adapter using a pi extension (`-e`) for authoritative state; conversation files feed discovery and resume

See [Adapters](/adapters) for the high-level overview and the [Integrations](/integrations/claude-code) section for concrete integration behavior.
