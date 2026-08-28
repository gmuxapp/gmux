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

**GET /meta** returns the full session state including internal title inputs (`shell_title`, `adapter_title`) and build identity (`binary_hash`, `runner_version`). gmuxd merges these runner facts into the session's durable SQLite row.

**SSE events** carry incremental updates:

| Event | Fields |
|-------|--------|
| `status` | `active`, `error`, `interrupted` |
| `meta` | `shell_title`, `adapter_title`, `subtitle`, `unread`, `slug` |
| `exit` | `exit_code` |
| `conversation_file` | `path` (legacy name `session_file` accepted until v2.1) |
| `terminal_resize` | `cols`, `rows` |
| `activity` | (no fields, signal only) |

### gmuxd → frontend

gmuxd exposes aggregated state to the browser via `GET /v1/events?session_stream=3` (SSE). Coalesced full replacements use bounded `snapshot.sessions.begin` / complete-row `.batch` / atomic `.ready` transactions (ADR 0001); a non-fatal `.error` identifies an omitted oversized row. `session-activity` is forwarded as a bare signal. Unversioned consumers temporarily retain legacy `snapshot.sessions`, and `GET /v1/sessions` remains for CLI/scripting. A wire-conversion layer controls which fields are serialized: internal fields are excluded, their derived outputs included instead.

### Field map

| Field | Runner sends | gmuxd stores | API sends | Frontend reads |
|-------|:---:|:---:|:---:|:---:|
| **Core identity** |
| `id` | ✓ | ✓ | ✓ | ✓ selection, WS URL |
| `created_at` | ✓ | ✓ | ✓ | ✓ age display |
| `command` | ✓ | ✓ | ✓ | title fallback only |
| `cwd` | ✓ | ✓ | ✓ | ✓ header, grouping |
| `adapter` | ✓ | ✓ | ✓ | ✓ adapter badge, URLs |
| `drive_mode` | ✓ | ✓ | ✓ | ✓ terminal vs. conversation view |
| `semantic_agent` | — | ✓ derived | ✓ | ✓ family-edge eligibility |
| `peer` | — | ✓ (hub) | ✓ | ✓ host attribution |
| `parent_session_id` | ✓ | ✓ | ✓ | ✓ family grouping, sidebar placement |
| `launched_from_session_id` | — | ✓ stamped | ✓ | ✓ “Return to family” |
| `workspace_root` | ✓ | ✓ | ✓ | ✓ project grouping |
| `remotes` | ✓ | ✓ | ✓ | ✓ project grouping |
| **Process state** |
| `alive` | ✓ | ✓ | ✓ | ✓ everywhere |
| `pid` | ✓ | ✓ | ✓ | — |
| `exit_code` | ✓ | ✓ | ✓ | — |
| `started_at` | ✓ | ✓ | ✓ | — |
| `exited_at` | ✓ | ✓ | ✓ | — |
| `last_output_at` | — | ✓ stamped | ✓ | ✓ activity-feed recency |
| **Display** |
| `title` | ✓ computed | ✓ re-resolved | ✓ | ✓ header, sidebar |
| `subtitle` | ✓ | ✓ | ✓ | — |
| `status` | ✓ | ✓ | ✓ | ✓ dots, header indicator |
| `unread` | ✓ | ✓ | ✓ | ✓ dots, tab badge |
| `unread_token` | ✓ | ✓ | ✓ | ✓ read acknowledgement identity |
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
| `id` | string | Unique session identifier (e.g. `16y0lfv7`) |
| `created_at` | ISO 8601 | When the session was created |
| `command` | string[] | The command the session was launched with. Preserved across exit; resume commands are derived separately. |
| `cwd` | string | Working directory |
| `adapter` | string | Adapter name: `"shell"`, `"claude"`, `"codex"`, `"pi"`, etc. |
| `workspace_root` | string? | Root of the workspace (jj/git), if detected. Used for project grouping. |
| `remotes` | map? | Git/jj remote URLs. Used for cross-machine project grouping. |
| `peer` | string? | Owning gmuxd instance; empty = local. Set by the hub for remote sessions, never by runners. |
| `drive_mode` | string | How gmux hosts the harness (ADR 0033): `"terminal"` (PTY) or `"acp"` (terminal-less). Absent on the wire means `terminal`, so older payloads normalize cleanly. |
| `semantic_agent` | boolean | The adapter exposes gmux's conversation-backed semantic-agent capability; both endpoints of a task-family edge must have it. |
| `launched_from_session_id` | string? | Immutable launch provenance: the session this one was launched from. Survives every parent mutation; powers “Return to family”. |

### Family edge (mutable)

| Field | Type | Description |
|-------|------|-------------|
| `parent_session_id` | string? | The session's *current* behavioral parent: family grouping, subagent budget depth, recursive dismissal, and notification suppression all follow this edge. Unlike the fields above it is mutable: `POST /v1/sessions/{id}/reparent` with `{"parent_session_id": "<id>"}` moves the session, and with `{"parent_session_id": null}` promotes it to a root (`gmux promote` / `gmux reparent` are thin wrappers over this endpoint). Local sessions only; cycles, self-parenting, and peer-owned targets are refused. |

### Process State (owned by gmux, authoritative)

| Field | Type | Description |
|-------|------|-------------|
| `alive` | boolean | Is the process running? Derived from socket reachability. |
| `pid` | number? | Process ID when alive. Diagnostic; may be absent on any row. |
| `exit_code` | number? | Exit code when dead |
| `started_at` | ISO 8601 | When the process was started |
| `exited_at` | ISO 8601? | When the process exited |
| `last_output_at` | ISO 8601? | Stamped by the owning daemon when `unread` turns on — the session produced output the user hasn't seen. Powers the activity feed's recency ordering. |

### Resume & conversations

| Field | Type | Description |
|-------|------|-------------|
| `resumable` | boolean | Derived, never set manually: the session is dead, has evidence it actually ran, its adapter has not declared the conversation gone, and a resume command can be derived. |
| `conversation_file` | string? | The agent's opaque conversation ref (for file-backed adapters, the transcript path), reported authoritatively by the agent hook (ADR 0011). Drives resume-command derivation; duplicate values across live sessions trigger a "conversation open in multiple tabs" warning. |

The stored launch `command` is preserved across exit. Resuming derives a tool-specific resume command from `(adapter, conversation_ref)` at spawn time. Dead conversations can be resolved via `GET /v1/conversations/{adapter}/{slug}`.

### Routing

| Field | Type | Description |
|-------|------|-------------|
| `slug` | string? | Stable URL-friendly identifier, unique within (adapter, peer). Reported by the agent hook (or set via the runner's `PUT /slug` endpoint); gmuxd enforces uniqueness; the frontend falls back to the `~<full-id>` URL form when empty. |

### Display (set by child or gmux, mutable)

| Field | Type | Description |
|-------|------|-------------|
| `title` | string | Primary display name. Resolved by gmuxd: adapter title > shell title > CommandTitler > adapter name. |
| `subtitle` | string? | Secondary context line. |
| `status` | Status? | Application-reported status (see below). |
| `unread` | boolean | Whether this session has unseen activity. |
| `unread_token` | string | Opaque runner-owned identity of the unseen result. Read acknowledgements name the token they observed so a delayed read cannot clear a newer completion, including across runner replacement. Treat as opaque; compare only for equality. |

### Terminal

| Field | Type | Description |
|-------|------|-------------|
| `socket_path` | string? | Runner's Unix socket. The frontend uses this as a truthiness check for attachability; the actual path is unused by the browser. Diagnostic; may be absent on any row. |
| `terminal_cols` | number? | Current terminal width. Used for initial sizing on attach. |
| `terminal_rows` | number? | Current terminal height. |

### Build Identity

| Field | Type | Description |
|-------|------|-------------|
| `runner_version` | string? | Version of the runner binary hosting the session. |
| `binary_hash` | string? | sha256 of the runner binary. The frontend compares both against `GET /v1/health` to derive the "outdated" badge (version mismatch, or hash drift in dev). |

`pid`, `socket_path`, `runner_version`, and `binary_hash` are origin-local,
best-effort diagnostics: any of them may be absent on any row, and they carry
no meaning outside the owning host. They are not covenanted scripting inputs
— see [Interface stability](/reference/stability/#diagnostic-session-fields).

### Status Object (set by child process)

Status is **null by default** and should only be set when it carries meaningful information.

```typescript
interface Status {
  active: boolean       // Pulsing dot animation: a turn is open.
  error?: boolean       // Red dot, treated as enhanced unread.
  interrupted?: boolean // Last turn was intentionally stopped.
}
```

**Design principle: no status is the default.**

- **`null`** — normal. Alive sessions show a steady dot, dead sessions are dimmed.
- **`active: true`** — pulsing dot. The animation says "something is happening."
- **`error: true`** — red dot; the agent gave up and needs attention. A turn-end `unread` report clears it — error is treated as enhanced unread.
- **`interrupted: true`** — the last turn was stopped on purpose (human or agent). Not an error: the sidebar just goes idle, but a synchronous wait reports the stop.
- Display text is the frontend's concern: it derives "Working…"/"Error" from the booleans and `exited (N)` from `exit_code`.

### How Children Set Status

**Option A — the agent hook** (agents; primary): gmux injects a hook into pi/claude/codex launches. The hook `POST`s to `/hook/event` on `$GMUX_SESSION_SOCK`:

- `op: "session"` — binds the session: `path` (conversation file), `name` (title), `slug`/`id`.
- `op: "turn"` — `phase: "start"` sets active; `phase: "end"` + `outcome` (`completed` → idle + unread, `error` → red dot, `interrupted` → idle + interrupted).

See `docs/runner-hook-protocol.md` in the repo and ADRs 0010/0011/0013/0015.

**Option B — `PUT /status` on `$GMUX_SOCKET`** (any process; generic fallback):
```bash
# gmux sets this in the child's environment
GMUX_SOCKET=~/.local/state/gmux/run/sessions/16y0lfv7.sock

# Child (or a wrapper script) sets status via HTTP on the socket
curl --unix-socket $GMUX_SOCKET http://localhost/status \
  -X PUT -d '{"active":true}'    # 'null' clears
```

There is no OSC status channel; the PTY reader parses only OSC 0/2 titles (which set `shell_title`).

### Full Example

As served by `GET /meta` on a runner's Unix socket (runner → gmuxd):

```json
{
  "id": "16y0lfv7",
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
  "status": { "active": true },
  "unread": false,
  "socket_path": "~/.local/state/gmux/run/sessions/16y0lfv7.sock",
  "conversation_file": "/home/user/.pi/agent/sessions/…/abc.jsonl",
  "runner_version": "2.0.0",
  "binary_hash": "a1b2c3d4e5f6..."
}
```

As served by `GET /v1/sessions` (gmuxd → frontend):

```json
{
  "id": "16y0lfv7",
  "created_at": "2026-03-14T10:00:00Z",
  "command": ["pi"],
  "cwd": "/home/user/dev/gmux",
  "adapter": "pi",
  "alive": true,
  "pid": 12345,
  "started_at": "2026-03-14T10:00:01Z",
  "title": "fix auth bug",
  "status": { "active": true },
  "unread": false,
  "socket_path": "~/.local/state/gmux/run/sessions/16y0lfv7.sock",
  "slug": "fix-auth-bug",
  "conversation_file": "/home/user/.pi/agent/sessions/…/abc.jsonl",
  "last_output_at": "2026-03-14T10:05:00Z",
  "runner_version": "2.0.0",
  "binary_hash": "a1b2c3d4e5f6..."
}
```

Note the differences: `shell_title` and `adapter_title` are absent from the API response — `title` is the resolved value. `runner_version` and `binary_hash` ride the wire so the frontend can derive staleness against `/v1/health`. `last_output_at` is stamped by the daemon.

## Terminology (2.0)

Pre-2.0 docs and payloads used `kind` (now `adapter`), `session_file` (now `conversation_file`), and `resume_key` (gone — its roles are covered by `conversation_file` and `slug`). See [Migrating to 2.0](/migrating-to-2/).

## What's NOT in This Schema

- **Model/provider** — application-specific, not gmux's concern
- **Cost/tokens** — same
- **Git branch / PR status** — could be a future Status extension, not core
- **Conversation history** — belongs to the application, not the multiplexer
- **Progress bar** — deferred; Status carries only `active`/`error`/`interrupted` booleans today
