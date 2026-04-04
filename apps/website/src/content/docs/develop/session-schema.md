---
title: Session Schema
description: The session metadata model shared between gmux, gmuxd, and the web UI.
---

> Application-agnostic session metadata. For how this state flows between components, see [State Management](/develop/state-management).

## Design Principles

1. **gmux owns process lifecycle; the child owns application state.** gmux knows if a process is alive. Only the child process knows if it's "thinking" or "waiting for input."

2. **Two-layer model.** Process state is authoritative and simple (alive/exited). Application state is advisory and rich (set by the child via well-known env/socket).

3. **Application-agnostic.** The schema must work for pi, Claude Code, Codex, opencode, a plain bash session, or any future tool. No field should assume a specific application.

4. **Sidebar-first.** Every field exists to answer: "what do I show in the sidebar?" If it doesn't affect the sidebar or terminal attachment, it doesn't belong here.

## Communication channels

Session data flows through three boundaries. Not every field crosses every boundary.

### Runner → gmuxd

Two paths: the runner's `GET /meta` endpoint (polled by discovery) and its SSE `/events` stream (subscribed for live updates).

**GET /meta** returns the full session state including internal title inputs (`shell_title`, `adapter_title`) and build identity (`binary_hash`). gmuxd deserializes this into `store.Session`.

**SSE events** carry incremental updates:

| Event | Fields |
|-------|--------|
| `status` | `label`, `working`, `error` |
| `meta` | `title`, `shell_title`, `adapter_title`, `subtitle`, `unread` |
| `exit` | `exit_code` |
| `terminal_resize` | `cols`, `rows` |
| `activity` | (no fields, signal only) |

### gmuxd → frontend

gmuxd exposes the aggregated store via `GET /v1/sessions` and `session-upsert` / `session-remove` SSE events. A custom `MarshalJSON` on `store.Session` controls which fields are serialized. Internal fields are excluded; their derived outputs are included instead.

### Field map

| Field | Runner sends | gmuxd stores | API sends | Frontend reads |
|-------|:---:|:---:|:---:|:---:|
| **Core identity** |
| `id` | ✓ | ✓ | ✓ | ✓ selection, WS URL |
| `created_at` | ✓ | ✓ | ✓ | ✓ age display |
| `command` | ✓ | ✓ | ✓ | title fallback only |
| `cwd` | ✓ | ✓ | ✓ | ✓ header, grouping |
| `kind` | ✓ | ✓ | ✓ | ✓ adapter badge |
| `workspace_root` | ✓ | ✓ | ✓ | ✓ folder grouping |
| `remotes` | ✓ | ✓ | ✓ | ✓ folder grouping |
| **Process state** |
| `alive` | ✓ | ✓ | ✓ | ✓ everywhere |
| `pid` | ✓ | ✓ | ✓ | — |
| `exit_code` | ✓ | ✓ | ✓ | — |
| `started_at` | ✓ | ✓ | ✓ | — |
| `exited_at` | ✓ | ✓ | ✓ | — |
| **Display** |
| `title` | ✓ computed | ✓ re-resolved | ✓ | ✓ header, sidebar |
| `subtitle` | ✓ | ✓ | ✓ | — |
| `status` | ✓ | ✓ | ✓ | ✓ dots, label |
| `unread` | ✓ | ✓ | ✓ | ✓ dots, tab badge |
| **Resume** |
| `resumable` | — | ✓ derived | ✓ | ✓ sidebar |
| **Terminal** |
| `socket_path` | ✓ | ✓ | ✓ | truthiness only |
| `terminal_cols` | ✓ | ✓ | ✓ | ✓ initial size |
| `terminal_rows` | ✓ | ✓ | ✓ | ✓ initial size |
| **Build identity** |
| `stale` | — | ✓ derived | ✓ | ✓ "outdated" badge |
| **Internal (not in API)** |
| `shell_title` | ✓ | ✓ | — | — |
| `adapter_title` | ✓ | ✓ | — | — |
| `resume_key` | — | ✓ | — | — |
| `binary_hash` | ✓ | ✓ | — | — |

Fields marked "—" in the "Frontend reads" column are sent by the API but not used by any rendering or logic code. They exist for future features (exit codes, process timing, subtitle display) or as defensive redundancy.

Internal fields are inputs to derived fields. The API only exposes the derived output:

| Internal input | Derived output |
|---|---|
| `shell_title`, `adapter_title` | `title` (via `resolveTitle`) |
| `resume_key` | `resumable` (via `Upsert`/`Update`) |
| `binary_hash` | `stale` (via `markStale`) |

## Schema

### Core Identity (set at creation, immutable)

| Field | Type | Description |
|-------|------|-------------|
| `id` | string | Unique session identifier (e.g. `sess-abc123`) |
| `created_at` | ISO 8601 | When the session was created |
| `command` | string[] | The command being run. For resumed sessions, replaced with the resume command. |
| `cwd` | string | Working directory |
| `kind` | string | Adapter kind: `"shell"`, `"claude"`, `"codex"`, `"pi"`, etc. |
| `workspace_root` | string? | Root of the workspace (jj/git), if detected. Used for folder grouping. |
| `remotes` | map? | Git/jj remote URLs. Used for cross-machine folder grouping. |

### Process State (owned by gmux, authoritative)

| Field | Type | Description |
|-------|------|-------------|
| `alive` | boolean | Is the process running? Derived from socket reachability. |
| `pid` | number | Process ID when alive |
| `exit_code` | number? | Exit code when dead |
| `started_at` | ISO 8601 | When the process was started |
| `exited_at` | ISO 8601? | When the process exited |

### Resume (derived by gmuxd)

| Field | Type | Description |
|-------|------|-------------|
| `resumable` | boolean | Derived: `!alive && command present`. Never set manually. |

All dead sessions with a command are resumable. Adapters with native resume (pi, claude, codex) provide a tool-specific resume command derived from the session file. Adapters without native resume (shell) keep the original command, so "resume" re-runs it in the same working directory.

### Display (set by child or gmux, mutable)

| Field | Type | Description |
|-------|------|-------------|
| `title` | string | Primary display name. Resolved by gmuxd: adapter title > shell title > CommandTitler > adapter kind. |
| `subtitle` | string? | Secondary context line. |
| `status` | Status? | Application-reported status (see below). |
| `unread` | boolean | Whether this session has unseen activity. |

### Terminal

| Field | Type | Description |
|-------|------|-------------|
| `socket_path` | string | Runner's Unix socket. The frontend uses this as a truthiness check for attachability; the actual path is unused by the browser. |
| `terminal_cols` | number? | Current terminal width. Used for initial sizing on attach. |
| `terminal_rows` | number? | Current terminal height. |

### Build Identity

| Field | Type | Description |
|-------|------|-------------|
| `stale` | boolean | True when the session's binary hash doesn't match the current gmux binary. Derived from the internal `binary_hash` field. |

### Status Object (set by child process)

Status is **null by default** and should only be set when it carries meaningful information.

```typescript
interface Status {
  label: string    // Short text, shown next to the dot.
  working: boolean // Pulsing dot animation.
  error?: boolean  // Red dot, treated as enhanced unread.
}
```

**Design principle: no status is the default.**

- **`null`** — normal. Alive sessions show a steady dot, dead sessions are dimmed.
- **`working: true`** — pulsing dot, no label needed. The animation says "something is happening."
- **`label` without `working`** — informational text like `"exited (1)"` or `"tests: 3 failed"`. Use sparingly.
- **Don't set `"completed"`, `"idle"`, or `"working"` as labels.** These repeat what the dot and alive/dead state already show.

### How Children Set Status

**Option A — Environment variable + HTTP** (preferred):
```bash
# gmux sets this in the child's environment
GMUX_SOCKET=/tmp/gmux-sessions/sess-abc123.sock

# Child (or a hook) sets status via HTTP on the same socket
curl --unix-socket $GMUX_SOCKET http://localhost/status \
  -X PUT -d '{"label":"thinking","working":true}'
```

**Option B — OSC escape sequences** (terminal-native):
```bash
# OSC 7777 ; json ST  (custom, parsed by gmux's PTY reader)
printf '\e]7777;{"label":"waiting","working":false}\e\\'
```

### Full Example

As served by `GET /meta` on a runner's Unix socket (runner → gmuxd):

```json
{
  "id": "sess-abc123",
  "created_at": "2026-03-14T10:00:00Z",
  "command": ["pi"],
  "cwd": "/home/user/dev/gmux",
  "kind": "pi",
  "alive": true,
  "pid": 12345,
  "started_at": "2026-03-14T10:00:01Z",
  "title": "fix auth bug",
  "shell_title": "user@host:~/dev/gmux",
  "adapter_title": "fix auth bug",
  "status": { "label": "thinking", "working": true },
  "unread": false,
  "socket_path": "/tmp/gmux-sessions/sess-abc123.sock",
  "binary_hash": "a1b2c3d4e5f6..."
}
```

As served by `GET /v1/sessions` (gmuxd → frontend):

```json
{
  "id": "sess-abc123",
  "created_at": "2026-03-14T10:00:00Z",
  "command": ["pi"],
  "cwd": "/home/user/dev/gmux",
  "kind": "pi",
  "alive": true,
  "pid": 12345,
  "started_at": "2026-03-14T10:00:01Z",
  "title": "fix auth bug",
  "status": { "label": "thinking", "working": true },
  "unread": false,
  "socket_path": "/tmp/gmux-sessions/sess-abc123.sock",
  "stale": false
}
```

Note the differences: `shell_title`, `adapter_title`, and `binary_hash` are absent from the API response. `title` is the resolved value. `stale` is derived from `binary_hash`.

## What's NOT in This Schema

- **Model/provider** — application-specific, not gmux's concern
- **Cost/tokens** — same
- **Git branch / PR status** — could be a future Status extension, not core
- **Conversation history** — belongs to the application, not the multiplexer
- **Progress bar** — deferred; `Status.label` like `"3/10 tests"` is sufficient
