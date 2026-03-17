---
title: State Management
description: How session state flows between the daemon, runners, and the web UI.
---

Session state flows one way: runners and file monitors produce it, `gmuxd` aggregates it in a store, and the frontend renders it. The frontend never modifies session state — it sends actions and waits for the backend to broadcast the result.

## The store

`gmuxd` holds all sessions in an in-memory store. Every mutation goes through `store.Upsert(session)`, which:

1. Derives computed fields (`title`, `resumable`, `close_action`)
2. Writes the session under a lock
3. Broadcasts a `session-upsert` SSE event to all connected browsers

`store.Remove(id)` broadcasts `session-remove`. There are no other write paths.

## Who writes what

Each field on a session has a single owner. No two subsystems write the same field.

| Transition | Owner | Trigger |
|---|---|---|
| Session appears (live) | **Register** | Runner calls `POST /v1/register` |
| Session appears (from file) | **Scanner** | Periodic scan of adapter session directories |
| Metadata updates | **Subscription** | Runner SSE `status` / `meta` events |
| File attribution + title | **FileMonitor** | inotify on `.jsonl` files |
| Session dies (clean exit) | **Subscription** | Runner SSE `exit` event |
| Session dies (crash) | **Discovery Scan** | Socket file gone |
| Session removed | **Dismiss handler** | User clicks × |

### Register: single entry point for live sessions

All live session creation flows through `Register()`. It queries the runner's `/meta` endpoint, creates or merges the session, and starts an SSE subscription. Both the `POST /v1/register` HTTP handler and the discovery scan delegate to it.

For resumed sessions, `Register()` merges the new runner into the existing store entry (keeping the original ID and resume key) via the `PendingResumes` mechanism.

### Discovery Scan: consistency check, not session creator

Scan runs every 3 seconds and does two things:

1. **New sockets** → delegates to `Register()` (never creates sessions directly)
2. **Missing sockets** → marks alive sessions as dead

This means discovery can never race with Register to create duplicate sessions.

### FileMonitor: file-driven updates

Watches adapter session directories with inotify. When a `.jsonl` file is written:

1. Attributes the file to a live session via the adapter's `FileAttributor` interface (pi uses scrollback similarity; claude and codex use cwd + timestamp proximity)
2. Tracks the **active file** per session — when a different file is attributed (e.g. `/new` or `/resume` in the tool's TUI), the `resume_key` updates to the new file's session ID
3. Feeds new lines to the adapter's `ParseNewLines()` for title and status updates

### Scanner: file-discovered sessions

Runs every 30 seconds. Enumerates adapter session files on disk (e.g. `~/.claude/projects/`) and creates resumable entries for sessions not already in the store. Respects the dismissed set — sessions the user removed won't reappear.

## Session lifecycle

```
                    ┌──────────────────────────┐
                    │                          │
  Scanner ─────►  resumable  ◄──── exit+file   │
                    │                          │
                    │ user clicks resume        │
                    │ (launches runner,         │
                    │  no store change)         │
                    │                          │
                    ▼                          │
  Register ────►  alive  ──────► exit ────────┘
                    ▲                │
                    │                │ (no file)
  Register ────────┘                ▼
  (new launch)                   dead
                                    │
                              dismiss │
                                    ▼
                                 removed
```

**Key transitions:**

- **alive → dead:** Subscription receives exit event from the runner, or discovery finds the socket gone.
- **dead → resumable:** If the session has a `resume_key` (file was attributed) and the adapter implements `Resumer`, the exit handler derives the resume command. The session transitions directly — no intermediate "exited" limbo state.
- **dead → removed:** Non-resumable dead sessions are not shown in the sidebar. Resumable sessions in the "Resume previous" drawer can be dismissed with ×.
- **resumable → alive:** User clicks the session. The resume handler launches a runner with the resume command but does **not** modify the store. When the runner registers, `Register()` merges it back to alive.
- **removed (with file):** Dismissed resume keys are tracked in memory so the scanner doesn't re-add them. Restarting `gmuxd` clears this set — a fresh start shows all available sessions.

## Derived fields

These are computed in `Upsert()`, never set manually:

| Field | Derivation |
|---|---|
| `title` | `adapter_title` > `shell_title` > `CommandTitler` > adapter kind (see below) |
| `resumable` | `!alive && resume-capable kind && has resume_key && has command` |

A session is only resume-capable if its adapter implements the `Resumer` interface. The set of resume-capable kinds is built from the compiled adapter set at startup.

**Title priority:** `adapter_title` always wins over `shell_title`. An empty `adapter_title` from the runner never overwrites a non-empty one on the daemon — this preserves titles across resume, where the daemon knows the title from file attribution but the freshly-started runner doesn't yet. The next fallback is the adapter's `CommandTitler` interface (shell uses this to show `pytest -x`). The final fallback is the adapter kind name (e.g. "codex").

`resume_key` is set during file attribution — not at session creation. A session from a resumable adapter that exits before creating a file (e.g. opened and immediately closed) gets `close_action: "dismiss"` and is not resumable.

## Frontend architecture

The frontend is a pure projection of backend state. Session state arrives exclusively via:

1. `GET /v1/sessions` — initial fetch on page load
2. SSE `session-upsert` — real-time updates
3. SSE `session-remove` — real-time removals
4. SSE reconnect — re-fetches all sessions

There are **no optimistic updates**. When the user clicks dismiss, the frontend sends `POST /v1/sessions/{id}/dismiss` and waits for the `session-remove` SSE event. On localhost the round-trip is <10ms — imperceptible.

### UI state (frontend-owned)

Two pieces of state are local to the frontend and not part of the session model:

```typescript
selectedId: string | null   // which session the terminal shows
resumingId: string | null   // which session has a resume in flight
```

**`selectedId`** — set on click, cleared when the selected session dies. Only sessions with `alive && socket_path` can be selected (the terminal needs a socket to connect to). Auto-selected on initial load for the first attachable session.

**`resumingId`** — set when the user clicks a resumable session. Shows a pulsing dot on the sidebar row while waiting for the backend to confirm the session is alive. Cleared when the SSE upsert arrives with `alive: true` and a valid `socket_path`, or after a 10-second timeout.

### canAttach

The terminal renders when `selected.alive && selected.socket_path` is true. This means:

- Dead/resumable sessions: no terminal, empty state shown
- Alive but no socket yet: impossible — `Register()` always sets both `alive` and `socket_path` atomically
- Alive with socket: terminal connects via WebSocket proxy

## Status labels

Status is **null by default**. A label should only be set when it carries information the user can't already see from the session's visual state.

| State | What the UI shows | Status field |
|---|---|---|
| Alive, idle | Steady dot | `null` |
| Alive, working | Pulsing dot | `{ working: true }` (no label) |
| Dead, clean exit | Dimmed row | `null` |
| Dead, non-zero exit | Dimmed row + label | `{ label: "exited (1)" }` |
| Resumable | Normal row, clickable | `null` |

Don't set labels like "completed", "idle", or "working" — they repeat what the dot and alive/dead state already communicate. Labels are for genuinely informative states like `"exited (1)"` or `"tests: 3 failed"`.
