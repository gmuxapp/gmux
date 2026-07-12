# Ubiquitous Language

## Session identity

| Term                | Definition                                                                                                | Aliases to avoid          |
| ------------------- | --------------------------------------------------------------------------------------------------------- | ------------------------- |
| **Session**         | The user-facing unit of work in a directory: a terminal pane plus everything we know about it             | Pane, terminal, tab       |
| **Conversation**    | An agent's own thread of dialogue (pi/claude/codex's "session"), identified by the agent's own **Conversation ID** (a UUID in the transcript header). It lives in adapter-owned storage, located by a **Conversation Ref**; the ref can change (resume/fork), the ID is the stable handle. A tool-backed **Session** corresponds to a Conversation; a **Runner** binds to one and may rebind (`/resume`) to another | Agent session, thread, session file |
| **Slug**            | A session's mutable, human-readable display name; persistent across runner restarts and resume, but renamable. Not identity (the immutable Conversation ID is) | Name                      |
| **Session ID**      | A specific runner instance's identifier; ephemeral for shell, stable (= conversation ID) for pi/claude    | Identifier (alone is too generic) |
| **Key**             | The single string projects.json uses per session: slug if attributed, session ID otherwise                |                           |
| **Conversation ID** | The agent's own identifier for a Conversation, extracted from the conversation's stored content by the adapter (`ConversationInfo.ID`). Used with the adapter name as the conversations-index key `(adapter, conversationID)` | Tool ID (the old name)    |
| **Conversation Ref** | An opaque, adapter-scoped locator for one stored Conversation (ADR 0022). Only the owning adapter interprets it: file-backed adapters use the transcript's absolute path; a DB-backed adapter may use a row key. The wire/persisted key `conversation_file` carries it for compatibility | Conversation file path (as an identity), session file |

## Session lifecycle

| Term                    | Definition                                                                                          | Aliases to avoid       |
| ----------------------- | --------------------------------------------------------------------------------------------------- | ---------------------- |
| **Alive**               | Has a live runner whose Unix socket is reachable                                                    | Running, active        |
| **Dead**                | No live runner; record exists in the store and possibly on disk                                     | Stopped, exited        |
| **Resumable**           | Dead session whose `Command` is set so a new runner can be spawned from it                          | Restartable            |
| **Pre-attribution**     | Transient phase for tool-backed sessions before their adapter file appears: has ID, no slug yet     | Ephemeral, unattributed |
| **Attributed**          | Tool-backed session whose adapter file exists, giving it a real slug                                | Named                  |
| **Fast-exit**           | A runner whose child finished before gmuxd's `queryMeta` reaches it; lands in the store as Alive=false directly, never as Alive=true | Quick-exit |

## Turn model (ADR 0023)

| Term                    | Definition                                                                                          | Aliases to avoid       |
| ----------------------- | --------------------------------------------------------------------------------------------------- | ---------------------- |
| **Turn**                | One unit of "the session is doing something": an agent turn (hook-delimited), a shell command (prompt-mark-delimited), or — default — the whole process lifetime. `Status.Working` is its open/closed state | Task, job              |
| **Turn source**         | Where a session's turn boundaries come from: agent hook for `HookDriven` adapters, otherwise the runner's default model (lifetime, upgraded by observed OSC 133 prompt marks) | —                      |
| **Active**              | Turn open (`Status.Working == true`); the blue dot. Distinct from *Alive* (process exists) and from the transient output-activity pulse | Busy, working (in prose) |
| **Idle**                | Turn closed; what `gmux wait` waits for. Includes a closed turn at process exit (one-shot completed, shell exited at its prompt) | Done, finished         |
| **Waiting on you**      | Unread flag set by a turn close; cleared when the session is viewed                                 | Unread (in UI prose)   |
| **Upgrade**             | A default-model session's switch from lifetime-as-turn to per-command turns on the first observed OSC 133 prompt mark; permanent for the session's life | Promotion              |

## Components

| Term            | Definition                                                                                       | Aliases to avoid            |
| --------------- | ------------------------------------------------------------------------------------------------ | --------------------------- |
| **Runner**      | A `gmux` process holding a child PTY and serving WS / scrollback over a per-session Unix socket  | gmux process, child         |
| **Daemon**      | The single per-host `gmuxd` process; central registry, broker, and proxy                         | Server, gmuxd (in prose)    |
| **Broker**      | The daemon's role serving readonly state (today: scrollback) sourced from disk for dead sessions | Replay server               |
| **Adapter**     | A plugin (pi, claude, shell, ...) that resolves commands, derives slugs, and may write tool files | Plugin, kind (the old wire field name) |
| **Agent-hook**  | A small extension the runner injects into the agent (pi: `-e <ext>`) that subscribes to the agent's own session/agent lifecycle and reports the held session file, title, and status to the runner authoritatively (ADR 0011). Supersedes the removed agent-shim fs preload (ADR 0010) | Hook, extension             |
| **Frontend**    | The web UI consuming `/v1/events` SSE and `/ws/<id>` WebSockets                                  | Client (overloaded), web    |

## Peering

| Term            | Definition                                                                                                  | Aliases to avoid           |
| --------------- | ----------------------------------------------------------------------------------------------------------- | -------------------------- |
| **Hub**         | The local daemon a frontend is connected to; proxies and aggregates                                         | Master                     |
| **Spoke**       | A remote daemon whose sessions are forwarded through a hub                                                  | Slave, child daemon        |
| **Peer**        | A connection from a hub to another daemon                                                                   |                            |
| **Local peer**  | A peer where `PeerConfig.Local = true` — currently only Docker devcontainers; sessions count as "owned"      | Devcontainer (the *connection*, not the *machine*) |
| **Network peer**| A non-Local peer (Tailscale or manual); its sessions are excluded from the hub's `/v1/events` stream         | Remote peer                |

## Persistence stores

| Term                     | Definition                                                                                                   | Aliases to avoid              |
| ------------------------ | ------------------------------------------------------------------------------------------------------------ | ----------------------------- |
| **Store**                | The daemon's in-memory `store.Store`: a **read cache** of live session state, fed by runner `/events` (ADR 0011). The runner owns live state; the store no longer parses files or writes session content                                  | Session table                 |
| **Sessionmeta**          | Per-session runtime persistence: `<state>/sessions/<id>/meta.json`. SOT for **runtime state** of dead sessions | Session metadata, meta files |
| **Scrollback**           | Per-session persisted PTY byte stream: `<state>/sessions/<id>/scrollback{,.0}`. SOT for terminal history     | History, log                  |
| **Projects.json**        | The on-disk SOT for **sidebar membership and ordering** (project rules + ordered key lists)                  | Project state, projects file  |
| **Conversations Index**  | In-memory id↔slug map rebuilt on startup from each adapter's **ConversationSource** (refs resolved via `DescribeConversation`); serves URL resolution & search | Conv index, conversations DB  |
| **Adapter state files**  | Per-adapter on-disk records behind file-backed ConversationSources (shell `<cwd>/<id>.json`, pi/claude JSONLs, ...) — an adapter implementation detail    | Session files                 |

## State separation

The four stores have orthogonal concerns. Mixing them is a smell:

| Store                  | Owns                                                              | Keyed by                          |
| ---------------------- | ----------------------------------------------------------------- | --------------------------------- |
| **Store**              | Caches live runtime state (the runner owns it; ADR 0011)          | Session ID                        |
| **Sessionmeta**        | Persisted runtime state (exit code, status, title, timestamps)    | Session ID                        |
| **Projects.json**      | Which sessions appear in the sidebar, in what project, what order | Project slug → list of **Key**s   |
| **Conversations Index**| Cross-reference between IDs and slugs for adapter-backed sessions | (adapter, conversationID) and (adapter, slug) |

## Lifecycle events

| Term                  | Definition                                                                                                            | Aliases to avoid               |
| --------------------- | --------------------------------------------------------------------------------------------------------------------- | ------------------------------ |
| **Register**          | Runner POSTs to `/v1/register`; daemon queries the runner's `/meta` and Upserts the session                            | Announce                       |
| **Deregister**        | Runner POSTs to `/v1/deregister` on shutdown; daemon unsubscribes (does **not** remove)                                | Unregister                     |
| **Resume**            | User-initiated: spawn a new runner with the dead session's resume `Command`, merge new runner onto the existing ID      | Restart (overloaded — see below) |
| **Resume merge**      | The internal step inside `Register` where a pending resume's existing ID swallows the fresh runner's ID                | Reattach                       |
| **Restart**           | Like Resume but kills the live runner first; goes through Resume after exit                                            |                                |
| **Slug-takeover**     | A fresh live session evicting a dead one with the same `(adapter, peer, slug)` from the store                              | Slug eviction                  |
| **Dismiss**           | Explicit user removal: runner killed if alive, store entry removed, sessionmeta + scrollback dropped                    | Delete, close                  |
| **Sweep**             | Daemon startup operation: read every `meta.json` and Upsert as `Alive=false`, so previously-seen dead sessions reappear. Whether each then renders depends on project membership and match rules — `State.Discovered()` also surfaces unfiled sessions, so visibility is not strictly gated by `projects.json` | Restore                        |
| **Attribution**       | Binding a tool-backed session to its adapter session file. **Primary:** authoritative, reported by the **Agent-hook** (pi names the held file directly, including cache-served `/resume` rebinds). **Fallback:** metadata matching (cwd + start time) for agents with no hook (codex). See ADR 0011 | Naming                         |

## Relationships

- A **Session** has exactly one **Slug** (after Attribution) and one **Session ID**.
- A **Slug** is identity; a **Session ID** is instance. Over time a single **Slug** in a project may host multiple **Session IDs** (e.g., dismiss, then re-run in the same cwd).
- **Projects.json** stores **Keys**, never `(id, slug)` pairs: the **Key** is the slug if attributed, the ID otherwise.
- **Sessionmeta** is keyed by **Session ID** and is the SOT for runtime fields. **Projects.json** is keyed by project slug and is the SOT for sidebar membership.
- A **Local peer** owns its sessions (counted as local for SSE forwarding); a **Network peer** does not.
- The **Broker** reads from **Scrollback** files and the **Store**; it never writes either.

## Example dialogue

> **Dev:** "User dismisses a session, then runs `gmux bash` in the same cwd. New session, same slug. What happens?"

> **Domain expert:** "It's a fresh **Session** with a new **Session ID** but the same **Slug** — same cwd, same derivation. The **Store** does **Slug-takeover**: evicts the old dead one if it's still around, inserts the new live one. **Projects.json** doesn't notice — its **Key** for that slot is the slug, which is unchanged."

> **Dev:** "So the **Sessionmeta** for the old session — does that get dropped?"

> **Domain expert:** "Yes. **Slug-takeover** broadcasts `session-remove` for the evicted ID, the cleanup goroutine catches it and removes that ID's **Sessionmeta** directory. The new session writes its own meta on first `Alive=false` landing."

> **Dev:** "What if it's a pi session that's still **Pre-attribution** when it hits the project?"

> **Domain expert:** "Then `AutoAssignSession` puts the **Session ID** in **Projects.json** as the **Key**. Once the JSONL appears and the session becomes **Attributed**, it gets a real **Slug**, and the projects code rewrites that array entry in place — same slot, ID swapped for slug."

> **Dev:** "So the **Conversations Index** matters here for resolving URLs to dead pi sessions, but not for the sidebar?"

> **Domain expert:** "Right. **Sessionmeta** makes dead session runtime state survive a daemon restart. **Projects.json** decides whether that dead session is still sidebar-visible. The **Conversations Index** exists for things like `/v1/conversations/{kind}/{slug}` URL lookup and future search. They live in different concerns; conflating them is what made the old rehydration path overwrite runtime state."

## Flagged ambiguities

- **"Session"** is used colloquially for the in-memory record, the runner process, the user's mental model, and the on-disk meta.json. Prefer **Session** for the conceptual entity, **Runner** for the process, **Sessionmeta** for the on-disk record, **Store entry** for the in-memory record.

- **"Slug"** appears in two scopes: **Project slug** (a project's identifier in projects.json) and **session Slug** (a session's identity). When ambiguous, qualify: "project slug" vs "session slug".

- **"Restart"** has been used both for the explicit `restart` action (kill + resume) and informally for "resume". Prefer **Restart** only for the kill-then-resume operation; **Resume** for spawning a runner from a dead session.

- **"State"** has been overloaded across **Store** (in-memory), **Sessionmeta** (per-session runtime fields on disk), **adapter state files** (per-adapter on-disk records), and the runtime Alive/Dead/Resumable. Always qualify which one.

- **"Identity"**: a **Session ID** is the durable identity of a session and the projects.json membership key. A **session slug** is a mutable display/URL name; treat them as distinct columns.

- **"Local"** has two meanings: the local *machine* (where this gmuxd runs), and a **Local peer** (`PeerConfig.Local = true`, currently devcontainers only). Prefer **Local peer** for the latter to avoid collision with "the local daemon".

- **"Session file"** is banned: say **conversation file** (gmux's term) — or use the agent's own term only at its API boundary (ADR 0015: agent-native JSON tags like `sessionId`/`transcript_path` keep the agent's language).

- **"Broker"** was introduced for the scrollback endpoint. Reserve it for read-only daemon endpoints serving disk-backed state for dead sessions; not a synonym for the daemon as a whole.
