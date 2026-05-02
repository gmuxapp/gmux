# Ubiquitous Language

## Session identity

| Term                | Definition                                                                                                | Aliases to avoid          |
| ------------------- | --------------------------------------------------------------------------------------------------------- | ------------------------- |
| **Session**         | The user-facing unit of work in a directory: a terminal pane plus everything we know about it             | Pane, terminal, tab       |
| **Slug**            | A session's stable, human-readable identity; persistent across runner restarts and resume                 | Name                      |
| **Session ID**      | A specific runner instance's identifier; ephemeral for shell, stable (= tool ID) for pi/claude            | Identifier (alone is too generic) |
| **Key**             | The single string projects.json uses per session: slug if attributed, session ID otherwise                |                           |
| **Tool ID**         | An adapter-managed file identifier (e.g. JSONL filename) used as session ID for tool-backed sessions      |                           |

## Session lifecycle

| Term                    | Definition                                                                                          | Aliases to avoid       |
| ----------------------- | --------------------------------------------------------------------------------------------------- | ---------------------- |
| **Alive**               | Has a live runner whose Unix socket is reachable                                                    | Running, active        |
| **Dead**                | No live runner; record exists in the store and possibly on disk                                     | Stopped, exited        |
| **Resumable**           | Dead session whose `Command` is set so a new runner can be spawned from it                          | Restartable            |
| **Pre-attribution**     | Transient phase for tool-backed sessions before their adapter file appears: has ID, no slug yet     | Ephemeral, unattributed |
| **Attributed**          | Tool-backed session whose adapter file exists, giving it a real slug                                | Named                  |
| **Fast-exit**           | A runner whose child finished before gmuxd's `queryMeta` reaches it; lands in the store as Alive=false directly, never as Alive=true | Quick-exit |

## Components

| Term            | Definition                                                                                       | Aliases to avoid            |
| --------------- | ------------------------------------------------------------------------------------------------ | --------------------------- |
| **Runner**      | A `gmux` process holding a child PTY and serving WS / scrollback over a per-session Unix socket  | gmux process, child         |
| **Daemon**      | The single per-host `gmuxd` process; central registry, broker, and proxy                         | Server, gmuxd (in prose)    |
| **Broker**      | The daemon's role serving readonly state (today: scrollback) sourced from disk for dead sessions | Replay server               |
| **Adapter**     | A plugin (pi, claude, shell, ...) that resolves commands, derives slugs, and may write tool files | Plugin, kind                |
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
| **Store**                | The daemon's in-memory `store.Store`: the live, authoritative session table                                  | Session table                 |
| **Sessionmeta**          | Per-session runtime persistence: `<state>/sessions/<id>/meta.json`. SOT for **runtime state** of dead sessions | Session metadata, meta files |
| **Scrollback**           | Per-session persisted PTY byte stream: `<state>/sessions/<id>/scrollback{,.0}`. SOT for terminal history     | History, log                  |
| **Projects.json**        | The on-disk SOT for **sidebar membership and ordering** (project rules + ordered key lists)                  | Project state, projects file  |
| **Conversations Index**  | In-memory id↔slug map rebuilt on startup by scanning **adapter state files**; serves URL resolution & search | Conv index, conversations DB  |
| **Adapter state files**  | Per-adapter on-disk records the conversations index reads (shell `<cwd>/<id>.json`, pi/claude JSONLs, ...)    | Session files                 |

## State separation

The four stores have orthogonal concerns. Mixing them is a smell:

| Store                  | Owns                                                              | Keyed by                          |
| ---------------------- | ----------------------------------------------------------------- | --------------------------------- |
| **Store**              | Live runtime state of every session the hub knows about           | Session ID                        |
| **Sessionmeta**        | Persisted runtime state (exit code, status, title, timestamps)    | Session ID                        |
| **Projects.json**      | Which sessions appear in the sidebar, in what project, what order | Project slug → list of **Key**s   |
| **Conversations Index**| Cross-reference between IDs and slugs for adapter-backed sessions | (kind, ID) and (kind, slug)       |

## Lifecycle events

| Term                  | Definition                                                                                                            | Aliases to avoid               |
| --------------------- | --------------------------------------------------------------------------------------------------------------------- | ------------------------------ |
| **Register**          | Runner POSTs to `/v1/register`; daemon queries the runner's `/meta` and Upserts the session                            | Announce                       |
| **Deregister**        | Runner POSTs to `/v1/deregister` on shutdown; daemon unsubscribes (does **not** remove)                                | Unregister                     |
| **Resume**            | User-initiated: spawn a new runner with the dead session's resume `Command`, merge new runner onto the existing ID      | Restart (overloaded — see below) |
| **Resume merge**      | The internal step inside `Register` where a pending resume's existing ID swallows the fresh runner's ID                | Reattach                       |
| **Restart**           | Like Resume but kills the live runner first; goes through Resume after exit                                            |                                |
| **Slug-takeover**     | A fresh live session evicting a dead one with the same `(kind, peer, slug)` from the store                              | Slug eviction                  |
| **Dismiss**           | Explicit user removal: runner killed if alive, store entry removed, sessionmeta + scrollback dropped                    | Delete, close                  |
| **Sweep**             | Daemon startup operation: read every `meta.json` and Upsert as `Alive=false` so dead sessions reappear in the sidebar   | Restore                        |
| **Attribution**       | The pre-attribution → attributed transition for tool-backed sessions when their adapter file appears                    | Naming                         |

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

> **Domain expert:** "Right. **Sessionmeta** is what makes dead sessions reappear in the sidebar after a daemon restart. The **Conversations Index** exists for things like `/v1/conversations/{kind}/{slug}` URL lookup and future search. They live in different concerns; conflating them is what made the old rehydration path overwrite runtime state."

## Flagged ambiguities

- **"Session"** is used colloquially for the in-memory record, the runner process, the user's mental model, and the on-disk meta.json. Prefer **Session** for the conceptual entity, **Runner** for the process, **Sessionmeta** for the on-disk record, **Store entry** for the in-memory record.

- **"Slug"** appears in two scopes: **Project slug** (a project's identifier in projects.json) and **session Slug** (a session's identity). When ambiguous, qualify: "project slug" vs "session slug".

- **"Restart"** has been used both for the explicit `restart` action (kill + resume) and informally for "resume". Prefer **Restart** only for the kill-then-resume operation; **Resume** for spawning a runner from a dead session.

- **"State"** has been overloaded across **Store** (in-memory), **Sessionmeta** (per-session runtime fields on disk), **adapter state files** (per-adapter on-disk records), and the runtime Alive/Dead/Resumable. Always qualify which one.

- **"Identity"**: in this codebase, **Slug** is identity, **Session ID** is instance. Treat them as distinct columns even though `SessionKey` coalesces them for projects.json's purposes.

- **"Local"** has two meanings: the local *machine* (where this gmuxd runs), and a **Local peer** (`PeerConfig.Local = true`, currently devcontainers only). Prefer **Local peer** for the latter to avoid collision with "the local daemon".

- **"Broker"** was introduced for the scrollback endpoint. Reserve it for read-only daemon endpoints serving disk-backed state for dead sessions; not a synonym for the daemon as a whole.
