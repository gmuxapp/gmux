---
title: Session Schema
description: The session metadata model shared between gmux, gmuxd, and the web UI.
---

> Application-agnostic session metadata. Informed by research into Codex, Claude Code Desktop, T3 Code, and cmux sidebar APIs.

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
| `resume_key` | string \| null | Session file ID, set during file attribution. Required for a session to be resumable or show the minimize button. |
| `close_action` | string | Derived: `"minimize"` for alive sessions with a resume-capable kind **and** an attributed file (`resume_key`), `"dismiss"` otherwise. Before file attribution, even resume-capable sessions show ×. |
| `command` | string[] | For resumable dead sessions, this is the resume command (e.g. `["claude", "--resume", "abc"]`). For alive sessions, the original launch command. |

### Display (set by child or gmux, mutable)

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `title` | string | yes | Primary display name. Default: first element of `command`. Can be overridden by child. Examples: `"gmux bootstrap"`, `"fix auth bug"`, `"pi"` |
| `subtitle` | string | no | Secondary context line. Examples: `"~/dev/gmux"`, `"iteration 3/10"`, `"waiting for build"` |
| `status` | Status | no | Application-reported status (see below) |
| `unread` | boolean | no | Whether this session has unseen activity. Default: false. Set true on output when not focused; cleared on focus. |

### Build Identity (set by gmux, used by gmuxd)

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `binary_hash` | string | no | SHA-256 hex digest of the gmux binary that owns this session. Computed once at startup from `os.Executable()`. |
| `stale` | boolean | no | Set by gmuxd when `binary_hash` doesn't match the gmux binary gmuxd would launch for new sessions. Indicates the session was started by a different build. Default: false. |

Stale detection allows the UI to show a visual indicator on sessions running an outdated gmux. This is important during development (frequent rebuilds) and after upgrades (old sessions survive daemon restart). The comparison is exact: any difference in binary content is a mismatch.

### Status Object (set by child process)

The child process can report its own status via a well-known mechanism (env variable pointing to gmux's socket, or escape sequences). This is advisory — gmux renders it but doesn't interpret it.

```typescript
interface Status {
  label: string       // Short text: "thinking", "waiting", "error", "building"
  state: StatusState  // Semantic bucket for styling
  icon?: string       // Optional icon hint (emoji or icon name)
}

type StatusState =
  | "active"    // Working, processing (pulsing/animated indicator)
  | "attention" // Needs user input, approval needed (alert indicator)
  | "success"   // Completed successfully (green check)
  | "error"     // Something went wrong (red indicator)
  | "paused"    // Idle but resumable (dim/grey indicator)
  | "info"      // Informational, no urgency (subtle indicator)
```

#### Why these six states?

Research across all apps shows the same fundamental question: **what color/animation should the dot be?**

- **active** = Codex `in-progress`, T3 `running`, Claude Code's "agent working" spinner
- **attention** = Codex approval prompts, pi `waiting_for_user`, cmux notification rings
- **success** = Codex `completed`, T3 turn `completed`
- **error** = Codex `failed`/`error`, T3 turn `failed`
- **paused** = Claude Code `archived`, T3 `ready`, idle sessions
- **info** = cmux `info` log level, general status display

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
  "status": {
    "label": "thinking",
    "state": "active",
    "icon": "🤔"
  },
  "unread": false,
  "binary_hash": "a1b2c3d4e5f6..."
}
```

## What's NOT in This Schema

- **Model/provider** — application-specific, not gmux's concern
- **Cost/tokens** — same
- **Git branch / PR status** — could be a future Status extension, not core
- **Conversation history** — belongs to the application, not the multiplexer
- **Progress bar** — deferred; Status.label like `"3/10 tests"` is sufficient for v1
