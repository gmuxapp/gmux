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

**GET /meta** returns the full session state including internal title inputs (`shell_title`, `adapter_title`) and build identity (`binary_hash`, `runner_version`). gmuxd deserializes this into `store.Session`.

**SSE events** carry incremental updates:

| Event | Fields |
|-------|--------|
| `status` | `working`, `error` |
| `meta` | `shell_title`, `adapter_title`, `subtitle`, `unread`, `slug` |
| `exit` | `exit_code` |
| `conversation_file` | `path` (legacy name `session_file` accepted until v2.1) |
| `terminal_resize` | `cols`, `rows` |
| `activity` | (no fields, signal only) |

### gmuxd → frontend

gmuxd exposes the aggregated store to the browser via `GET /v1/events` (SSE). Session state arrives as coalesced `snapshot.sessions` full-replacement snapshots (ADR 0001); `session-activity` is forwarded as a bare signal. `GET /v1/sessions` remains for CLI/scripting. A custom `MarshalJSON` on `store.Session` controls which fields are serialized in both. Internal fields are excluded; their derived outputs are included instead.

### Field map

| Field | Runner sends | gmuxd stores | API sends | Frontend reads |
|-------|:---:|:---:|:---:|:---:|
| **Core identity** |
| `id` | ✓ | ✓ | ✓ | ✓ selection, WS URL |
| `created_at` | ✓ | ✓ | ✓ | ✓ age display |
| `command` | ✓ | ✓ | ✓ | title fallback only |
| `cwd` | ✓ | ✓ | ✓ | ✓ header, grouping |
| `adapter` | ✓ | ✓ | ✓ | ✓ adapter badge, URLs |
| `peer` | — | ✓ (hub) | ✓ | ✓ host attribution |
| `parent_session_id` | ✓ | ✓ | ✓ | ✓ sidebar placement of editor children |
| `workspace_root` | ✓ | ✓ | ✓ | ✓ project grouping |
| `remotes` | ✓ | ✓ | ✓ | ✓ project grouping |
| **Process state** |
| `alive` | ✓ | ✓ | ✓ | ✓ everywhere |
| `pid` | ✓ | ✓ | ✓ | — |
| `exit_code` | ✓ | ✓ | ✓ | — |
| `started_at` | ✓ | ✓ | ✓ | — |
| `exited_at` | ✓ | ✓ | ✓ | — |
| `last_activity_at` | — | ✓ stamped | ✓ | ✓ home recency buckets |
| **Display** |
| `title` | ✓ computed | ✓ re-resolved | ✓ | ✓ header, sidebar |
| `subtitle` | ✓ | ✓ | ✓ | — |
| `status` | ✓ | ✓ | ✓ | ✓ dots, header indicator |
| `unread` | ✓ | ✓ | ✓ | ✓ dots, tab badge |
| **Resume & conversations** |
| `resumable` | — | ✓ derived | ✓ | ✓ sidebar |
| `conversation_file` | ✓ (hook) | ✓ | ✓ | ✓ duplicate-conversation warning |
| **Routing** |
| `slug` | ✓ opt | ✓ auto-derived | ✓ | ✓ URL routing |
| **Terminal** |
| `socket_path` | ✓ | ✓ | ✓ | truthiness only |
| `terminal_cols` | ✓ | ✓ | ✓ | ✓ initial size |
| `terminal_rows` | ✓ | ✓ | ✓ | ✓ initial size |
| **Build identity** |
| `runner_version` | ✓ | ✓ | ✓ | ✓ staleness input |
| `binary_hash` | ✓ | ✓ | ✓ | ✓ staleness input |
| **Project assignment (ADR 0002)** |
| `project_slug`, `project_index` | — | ✓ stamped | ✓ | ✓ project rendering |
| **Internal (not in API)** |
| `shell_title` | ✓ | ✓ | — | — |
| `adapter_title` | ✓ | ✓ | — | — |

Fields marked "—" in the "Frontend reads" column are sent by the API but not used by any rendering or logic code. They exist for future features or as defensive redundancy.

Internal fields are inputs to derived fields. The API only exposes the derived output: `shell_title` and `adapter_title` resolve into `title` (via `resolveTitle`). There is no `stale` field — the frontend derives staleness by comparing `runner_version`/`binary_hash` against `GET /v1/health`'s daemon version and `runner_hash`.

## Schema

### Core Identity (set at creation, immutable)

| Field | Type | Description |
|-------|------|-------------|
| `id` | string | Unique session identifier (e.g. `sess-abc123`) |
| `created_at` | ISO 8601 | When the session was created |
| `command` | string[] | The command being run. For resumed sessions, replaced with the resume command. |
| `cwd` | string | Working directory |
| `adapter` | string | Adapter name: `"shell"`, `"claude"`, `"codex"`, `"pi"`, etc. |
| `workspace_root` | string? | Root of the workspace (jj/git), if detected. Used for project grouping. |
| `remotes` | map? | Git/jj remote URLs. Used for cross-machine project grouping. |
| `peer` | string? | Owning gmuxd instance; empty = local. Set by the hub for remote sessions, never by runners. |
| `parent_session_id` | string? | Session this one was spawned from (`gmux edit` as `$EDITOR`); places the child next to its parent in the sidebar. |

### Process State (owned by gmux, authoritative)

| Field | Type | Description |
|-------|------|-------------|
| `alive` | boolean | Is the process running? Derived from socket reachability. |
| `pid` | number | Process ID when alive |
| `exit_code` | number? | Exit code when dead |
| `started_at` | ISO 8601 | When the process was started |
| `exited_at` | ISO 8601? | When the process exited |
| `last_activity_at` | ISO 8601? | Stamped by the owning daemon on noteworthy transitions (exit, unread on, working on, error on). Powers the home screen's recency buckets. |

### Resume & conversations

| Field | Type | Description |
|-------|------|-------------|
| `resumable` | boolean | Derived: `!alive && command present`. Never set manually. |
| `conversation_file` | string? | The agent's on-disk conversation file, reported authoritatively by the agent hook (ADR 0011). Drives resume-command derivation on exit; duplicate values across live sessions trigger a "conversation open in multiple tabs" warning. |

All dead sessions with a command are resumable. On exit, adapters with native resume (pi, claude, codex) get their command replaced with a tool-specific resume command derived from the conversation file; sessions without one keep the original command, so "resume" re-runs it in the same working directory. Dead conversations can be resolved via `GET /v1/conversations/{adapter}/{slug}`.

### Routing

| Field | Type | Description |
|-------|------|-------------|
| `slug` | string? | Stable URL-friendly identifier, unique within (adapter, peer). Reported by the agent hook (or set via the runner's `PUT /slug` endpoint); gmuxd enforces uniqueness; the frontend falls back to the short ID when empty. |

### Display (set by child or gmux, mutable)

| Field | Type | Description |
|-------|------|-------------|
| `title` | string | Primary display name. Resolved by gmuxd: adapter title > shell title > CommandTitler > adapter name. |
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
| `runner_version` | string | Version of the runner binary hosting the session. |
| `binary_hash` | string | sha256 of the runner binary. The frontend compares both against `GET /v1/health` to derive the "outdated" badge (version mismatch, or hash drift in dev). |

### Status Object (set by child process)

Status is **null by default** and should only be set when it carries meaningful information.

```typescript
interface Status {
  working: boolean // Pulsing dot animation.
  error?: boolean  // Red dot, treated as enhanced unread.
}
```

**Design principle: no status is the default.**

- **`null`** — normal. Alive sessions show a steady dot, dead sessions are dimmed.
- **`working: true`** — pulsing dot. The animation says "something is happening."
- **`error: true`** — red dot; the agent gave up and needs attention. A turn-end `unread` report clears it — error is treated as enhanced unread.
- Display text is the frontend's concern: it derives "Working…"/"Error" from the booleans and `exited (N)` from `exit_code`.

### How Children Set Status

**Option A — the agent hook** (agents; primary): gmux injects a hook into pi/claude/codex launches. The hook `POST`s to `/hook/event` on `$GMUX_SESSION_SOCK`:

- `op: "session"` — binds the session: `path` (conversation file), `name` (title), `slug`/`id`.
- `op: "turn"` — `phase: "start"` sets working; `phase: "end"` + `outcome` (`completed` → idle + unread, `error` → red dot, `aborted` → idle).

See `docs/runner-hook-protocol.md` in the repo and ADRs 0010/0011/0013/0015.

**Option B — `PUT /status` on `$GMUX_SOCKET`** (any process; generic fallback):
```bash
# gmux sets this in the child's environment
GMUX_SOCKET=~/.local/state/gmux/run/sessions/sess-abc123.sock

# Child (or a wrapper script) sets status via HTTP on the socket
curl --unix-socket $GMUX_SOCKET http://localhost/status \
  -X PUT -d '{"working":true}'    # 'null' clears
```

There is no OSC status channel; the PTY reader parses only OSC 0/2 titles (which set `shell_title`).

### Full Example

As served by `GET /meta` on a runner's Unix socket (runner → gmuxd):

```json
{
  "id": "sess-abc123",
  "created_at": "2026-03-14T10:00:00Z",
  "command": ["pi"],
  "cwd": "/home/user/dev/gmux",
  "adapter": "pi",
  "alive": true,
  "pid": 12345,
  "started_at": "2026-03-14T10:00:01Z",
  "title": "fix auth bug",
  "shell_title": "user@host:~/dev/gmux",
  "adapter_title": "fix auth bug",
  "status": { "working": true },
  "unread": false,
  "socket_path": "~/.local/state/gmux/run/sessions/sess-abc123.sock",
  "conversation_file": "/home/user/.pi/agent/sessions/…/abc.jsonl",
  "runner_version": "2.0.0",
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
  "adapter": "pi",
  "alive": true,
  "pid": 12345,
  "started_at": "2026-03-14T10:00:01Z",
  "title": "fix auth bug",
  "status": { "working": true },
  "unread": false,
  "socket_path": "~/.local/state/gmux/run/sessions/sess-abc123.sock",
  "slug": "fix-auth-bug",
  "conversation_file": "/home/user/.pi/agent/sessions/…/abc.jsonl",
  "last_activity_at": "2026-03-14T10:05:00Z",
  "runner_version": "2.0.0",
  "binary_hash": "a1b2c3d4e5f6..."
}
```

Note the differences: `shell_title` and `adapter_title` are absent from the API response — `title` is the resolved value. `runner_version` and `binary_hash` ride the wire so the frontend can derive staleness against `/v1/health`. `last_activity_at` is stamped by the daemon.

## Terminology (2.0)

Pre-2.0 docs and payloads used `kind` (now `adapter`), `session_file` (now `conversation_file`), and `resume_key` (gone — its roles are covered by `conversation_file` and `slug`). See [Migrating to 2.0](/migrating-to-2/).

## What's NOT in This Schema

- **Model/provider** — application-specific, not gmux's concern
- **Cost/tokens** — same
- **Git branch / PR status** — could be a future Status extension, not core
- **Conversation history** — belongs to the application, not the multiplexer
- **Progress bar** — deferred; Status carries only `working`/`error` booleans today
