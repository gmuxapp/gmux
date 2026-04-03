---
title: Session Schema
description: The session metadata model shared between gmux, gmuxd, and the web UI.
---

> Application-agnostic session metadata. Informed by research into Codex, Claude Code Desktop, T3 Code, and cmux sidebar APIs. For how this state flows between components, see [State Management](/develop/state-management).

## Design Principles

1. **gmux owns process lifecycle; the child owns application state.** gmux knows if a process is alive. Only the child process knows if it's "thinking" or "waiting for input."

2. **Two-layer model.** Process state is authoritative and simple (alive/exited). Application state is advisory and rich (set by the child via well-known env/socket).

3. **Application-agnostic.** The schema must work for pi, Claude Code, Codex, opencode, a plain bash session, or any future tool. No field should assume a specific application.

4. **Sidebar-first.** Every field exists to answer: "what do I show in the sidebar?" If it doesn't affect the sidebar or terminal attachment, it doesn't belong here.

## Schema

### Core Identity (set at creation, immutable)

| Field | Type | Description |
|-------|------|-------------|
| `id` | string | Unique session identifier (e.g. `sess-abc123`) |
| `created_at` | ISO 8601 | When the session was created |
| `command` | string[] | The command being run |
| `cwd` | string | Working directory |
| `kind` | string | Adapter kind: `"shell"`, `"claude"`, `"codex"`, `"pi"`, etc. |

### Process State (owned by gmux, authoritative)

| Field | Type | Description |
|-------|------|-------------|
| `alive` | boolean | Is the process running? Derived from socket reachability. |
| `pid` | number \| null | Process ID when alive, null when exited |
| `exit_code` | number \| null | Exit code when dead, null when alive |
| `started_at` | ISO 8601 | When the process was started |
| `exited_at` | ISO 8601 \| null | When the process exited |

### Resume (derived by gmuxd)

| Field | Type | Description |
|-------|------|-------------|
| `resumable` | boolean | Derived: `!alive && resume-capable kind && has resume_key && command present`. Never set manually. |
| `resume_key` | string \| null | Session file ID, set during file attribution. Required for a session to be resumable. |
| `command` | string[] | For resumable dead sessions, this is the resume command (e.g. `["claude", "--resume", "abc"]`). For alive sessions, the original launch command. |

### Display (set by child or gmux, mutable)

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `title` | string | yes | Primary display name. Default: first element of `command`. Can be overridden by child. Examples: `"gmux bootstrap"`, `"fix auth bug"`, `"pi"` |
| `subtitle` | string | no | Secondary context line. Examples: `"~/dev/gmux"`, `"iteration 3/10"`, `"waiting for build"` |
| `slug` | string | no | URL-safe stable identifier for the session. Set by the adapter via `/meta` or `GMUX_SOCKET`. Used as the trailing segment in URL routing (`/<folder>/<adapter>/<slug>`). See below. |
| `status` | Status | no | Application-reported status (see below) |
| `unread` | boolean | no | Whether this session has unseen activity. Default: false. Set true on output when not focused; cleared on focus. |

### Build Identity (set by gmux, used by gmuxd)

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `binary_hash` | string | no | SHA-256 hex digest of the gmux binary that owns this session. Computed once at startup from `os.Executable()`. |
| `stale` | boolean | no | Set by gmuxd when `binary_hash` doesn't match the gmux binary gmuxd would launch for new sessions. Indicates the session was started by a different build. Default: false. |

Stale detection allows the UI to show a visual indicator on sessions running an outdated gmux. This is important during development (frequent rebuilds) and after upgrades (old sessions survive daemon restart). The comparison is exact: any difference in binary content is a mismatch.

### Status Object (set by child process)

Status is **null by default** and should only be set when it carries meaningful information. The absence of status is the normal state — a session exists, it's alive or dead, and the dot color already communicates that.

```typescript
interface Status {
  label: string    // Short text, shown next to the dot. Only set when informative.
  working: boolean // Pulsing dot animation. Don't also set a label — the dot is enough.
}
```

#### Design principle: no status is the default

- **`null`** — normal. Alive sessions show a steady dot, dead sessions are dimmed.
- **`working: true`** — pulsing dot, no label needed. The animation says "something is happening."
- **`label` without `working`** — informational text like `"exited (1)"` or `"tests: 3 failed"`. Use sparingly — only when the label tells the user something they can't already see.
- **Don't set `"completed"`, `"idle"`, or `"working"` as labels.** These repeat what the dot and alive/dead state already show.

The `label` is the human-readable text. The `state` determines the visual treatment. This decouples "what the app says" from "how gmux renders it."

### How Children Set Status

**Option A — Environment variable + HTTP** (preferred):
```bash
# gmux sets this in the child's environment
GMUX_SOCKET=/tmp/gmux-sessions/sess-abc123.sock

# Child (or a hook) sets status via HTTP on the same socket
curl --unix-socket $GMUX_SOCKET http://localhost/status \
  -X PUT -d '{"label":"thinking","state":"active"}'
```

**Option B — OSC escape sequences** (terminal-native):
```bash
# OSC 7777 ; json ST  (custom, parsed by gmux's PTY reader)
printf '\e]7777;{"label":"waiting","state":"attention"}\e\\'
```

**Option C — Both.** HTTP for programmatic use (hooks, extensions), OSC for terminal-native tools.

### Dot/Indicator Logic

| Process alive? | status.state | Indicator |
|---------------|-------------|-----------|
| yes | `active` | Green pulsing dot |
| yes | `attention` | Orange/amber dot (possibly animated) |
| yes | `success` | Green check |
| yes | `error` | Red dot |
| yes | `paused` | Grey dot |
| yes | `info` | Blue dot |
| yes | (none) | Dim green dot (alive, no status reported) |
| no | (any/none) | Grey hollow dot or ✕ |

### Full Example

As served by `GET /meta` on a runner's Unix socket:

```json
{
  "id": "sess-abc123",
  "created_at": "2026-03-14T10:00:00Z",
  "command": ["pi"],
  "cwd": "/home/user/dev/gmux",
  "kind": "pi",
  "alive": true,
  "pid": 12345,
  "exit_code": null,
  "started_at": "2026-03-14T10:00:01Z",
  "exited_at": null,
  "title": "gmux bootstrap",
  "subtitle": "~/dev/gmux",
  "slug": "fix-auth-bug",
  "status": {
    "label": "thinking",
    "state": "active",
    "icon": "🤔"
  },
  "unread": false,
  "binary_hash": "a1b2c3d4e5f6..."
}
```

### URL Slug (set by child, stable across resume)

The `slug` field provides a stable, human-readable identifier for URL routing. It is set by the adapter and should be derived from something that persists across kill and resume.

**Adapter examples:**

| Adapter | Slug source | Example |
|---------|-------------|---------|
| pi | Conversation ID or first-message summary | `fix-auth-bug` |
| claude | Session file basename | `abc123` |
| codex | Session file basename | `def456` |
| shell | Sanitized command or counter | `pytest-watch`, `shell-3` |

**Rules:**

- URL-safe characters only (lowercase alphanumeric, hyphens). gmux sanitizes the adapter's input.
- Unique within the adapter's namespace for that folder. gmux appends a disambiguator (`-2`, `-3`) on collision.
- Falls back to a truncated session ID (e.g. `sess-abc12`) if the adapter doesn't provide one.
- Stable across resume: the slug is tied to the logical session (conversation ID, session file), not the process. A resumed session keeps the same slug.

The slug is part of the hierarchical URL path: `/<folder>/<adapter>/<slug>`. Each adapter gets its own namespace within a folder, so adapters don't need to coordinate with each other. See [Folder Management](/planned/folder-management#step-3-url-routing) for the full URL routing design.

**Setting the slug:**

```bash
# Via GMUX_SOCKET HTTP (preferred)
curl --unix-socket $GMUX_SOCKET http://localhost/meta \
  -X PATCH -d '{"slug": "fix-auth-bug"}'
```

Or include it in the `/meta` response from the runner. The slug can be set at any time; the URL updates and the old URL redirects.

## What's NOT in This Schema

- **Model/provider** — application-specific, not gmux's concern
- **Cost/tokens** — same
- **Git branch / PR status** — could be a future Status extension, not core
- **Conversation history** — belongs to the application, not the multiplexer
- **Progress bar** — deferred; Status.label like `"3/10 tests"` is sufficient for v1
