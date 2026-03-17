# State Architecture

## Problem

Session state is written by 6 independent backend writers and 2 frontend writers.
Races between them cause duplicate sessions, flash-then-deselect on resume, and
zombie "exited" entries that won't go away. Every fix for one race exposes another
because the system has no single owner for state transitions.

## Current writers (backend → store.Upsert)

| Writer | When | What it sets |
|---|---|---|
| **Discovery Scan** | Every 3s, iterates socket files | Creates new sessions, marks unreachable ones dead |
| **Discovery Scan (stale)** | Socket file gone | Sets `alive=false`, resolves resume command |
| **Register handler** | Runner calls POST /register | Creates session or merges into existing (resume) |
| **Resume handler** | User clicks resume | Sets `alive=true`, clears socket (optimistic) |
| **Subscription (status)** | Runner SSE "status" event | Updates `Status` |
| **Subscription (meta)** | Runner SSE "meta" event | Updates title, subtitle, unread |
| **Subscription (exit)** | Runner SSE "exit" event | Sets `alive=false`, resolves resume command |
| **FileMonitor** | inotify on .jsonl files | Updates title, status, sets ResumeKey |
| **FileMonitor (attribution)** | First file write | Sets ResumeKey on session |
| **Session file scanner** | Every 30s | Creates resumable sessions from files on disk |

## Current writers (frontend → setSessions)

| Writer | When | What it does |
|---|---|---|
| **Initial fetch** | Page load | Replaces all sessions |
| **SSE upsert** | Backend broadcasts | Updates or adds one session |
| **SSE remove** | Backend broadcasts | Removes one session |
| **SSE reconnect** | Connection restored | Re-fetches all sessions |
| **Optimistic dismiss** | User clicks × | Removes session locally before backend confirms |

## The races

### 1. Resume: discovery vs register
Resume handler sets `alive=true` and launches the runner. The runner creates a
socket and calls POST /register. If discovery scans between socket creation and
registration, it creates a **duplicate session** with the runner's new ID.

### 2. Resume: stale socket
Resume handler kept the old `socket_path`. Discovery found the stale socket file
gone and marked the session **dead again**, undoing the resume.

### 3. Resume: premature terminal attach
Frontend saw `alive=true` and tried to connect the WebSocket. The runner hadn't
registered yet — no valid socket to proxy to.

### 4. Optimistic dismiss divergence
Frontend removes the session locally on dismiss. If the backend SSE remove event
arrives late or the dismiss fails, the frontend and backend are out of sync.

### 5. Exit status clobbered
OnExit hook cleared status to signal "resumable", but the exit handler checked
`status == nil` and overwrote it with "exited (N)".

## Root cause

**No single owner for lifecycle transitions.** The same field (`alive`, `status`,
`socket_path`, `command`) is written by multiple independent paths that don't
coordinate. "Optimistic" backend updates (resume sets alive before the runner
exists) create states that other writers don't expect.

---

## Proposed architecture: backend is truth, frontend is projection

### Principle 1: Backend never lies

Don't set `alive=true` until the runner has actually registered. The session stays
in its current state (dead/resumable) until the runner calls POST /register and
the merge completes. This eliminates the "alive but no socket" transient state
and all three resume races.

### Principle 2: Frontend never writes session state

Remove all optimistic updates. The frontend's `sessions` array is only written by:
1. Initial fetch (GET /v1/sessions)
2. SSE `session-upsert` events
3. SSE `session-remove` events

The frontend sends actions (dismiss, kill, resume, launch) and waits for the
backend to broadcast the result. On localhost, SSE latency is <10ms — imperceptible.

### Principle 3: One owner per transition

| Transition | Owner | Mechanism |
|---|---|---|
| new session appears | Register handler | Runner calls POST /register |
| session metadata updates | Subscription | Runner SSE events |
| session file attribution | FileMonitor | inotify |
| session dies | Subscription | Runner SSE "exit" event |
| session dies (crash) | Discovery Scan | Socket file gone |
| resumable session discovered | Scanner | Session file on disk |
| session removed | Dismiss handler | User action |

Discovery Scan **no longer creates sessions**. It only:
- Detects dead sessions (socket gone) for sessions already in the store
- Cleans up stale socket files

New sessions come exclusively from Register (live) or Scanner (file-discovered).

### Principle 4: Resume is not a state change

The resume handler:
1. Records the pending resume
2. Launches the runner
3. Returns `{ ok: true }` to the frontend

It does NOT modify the session in the store. The session stays dead/resumable.
When the runner registers, Register merges it into the existing session — that's
when `alive` becomes true, `socket_path` is set, and the SSE upsert fires.

The frontend shows the session's current state (resumable) until the SSE upsert
arrives with `alive=true` + valid `socket_path`. A brief "resuming" indicator
can be shown client-side (local UI state, not session state).

### Principle 5: Discovery is read-only for known sessions

Discovery Scan iterates sockets and:
- **Unknown socket, not pending resume**: calls Register (which does queryMeta + Upsert)
- **Unknown socket, pending resume**: skip (Register will handle it)
- **Known socket**: skip (already tracked)
- **Missing socket for alive session**: mark dead

This makes Scan a consistency check, not a session creator. Session creation
flows through Register exclusively.

## State diagram

```
                    ┌─────────────────────────┐
                    │                         │
  Scanner ─────►  resumable  ◄──── exit+file  │
                    │                         │
                    │ user clicks             │
                    │ (resume handler         │
                    │  launches runner,       │
                    │  no state change)       │
                    │                         │
                    ▼                         │
  Register ────►  alive  ─────► exit ────────┘
                    ▲               │
                    │               │ (no file)
  Register ────────┘               ▼
  (new launch)                  exited/dead
                                    │
                              dismiss │
                                    ▼
                                 removed
```

## Frontend state

```typescript
// Session state: backend-owned, arrives via SSE
const [sessions, setSessions] = useState<Session[]>([])

// UI state: frontend-owned, not synced
const [selectedId, setSelectedId] = useState<string | null>(null)
const [resumingId, setResumingId] = useState<string | null>(null)
```

`resumingId` is pure UI — shows a spinner on the clicked session while waiting
for the SSE upsert. It's not session state; it's "I clicked this and I'm waiting."
Cleared when the session becomes alive or after a timeout.

## Migration

1. Remove optimistic dismiss (frontend)
2. Remove `alive=true` from resume handler (backend)
3. Remove "Starting" interstitial (frontend) — replaced by `resumingId` spinner
4. Collapse Discovery Scan: unknown sockets → call Register()
5. Remove `pendingResumeRef` and `pendingResumes.Has()` — no longer needed
6. Document the owner table above in the codebase
