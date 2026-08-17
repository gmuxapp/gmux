# Ubiquitous Language

This file defines current v2 terminology. Historical ADRs may use superseded `store.Store`, `sessionmeta`, or `projects.json` language; do not carry those terms into current implementation descriptions.

## Session identity

| Term | Definition | Avoid |
| --- | --- | --- |
| **Session** | The durable user-facing unit of work: identity, terminal history, lifecycle, and optional conversation binding. A runner may host it. | Pane, runner, process |
| **Session ID** | Immutable durable identity and project-placement/order key on the origin host. Remote addresses qualify it as `id@peer`. | Slug, key |
| **Session slug** | Mutable human-readable URL/display name allocated by the origin daemon. Never a placement key or durable identity. | ID, identity |
| **Conversation** | An adapter-owned thread of dialogue. It is not created by project matching or conversation indexing. | Session file |
| **Conversation ID** | Stable adapter-reported identity extracted by `DescribeConversation`; indexed with adapter as `(adapter, conversationID)`. | Tool ID |
| **Conversation Ref** | Opaque adapter-scoped locator persisted in `conversation_file` for compatibility. Only the adapter interprets it. | File path as identity |
| **Launch parent** | Immutable session ID of the session that launched this one. Records provenance independently of presentation promotion. | UI parent only |
| **Harness** | The agent tool a session runs — pi, claude, codex. Names the session identity in the canonical spec `model:effort@harness`; one adapter per harness. | Backend, adapter (as identity) |
| **Drive mode** | How gmux hosts and drives a harness: terminal (PTY) or ACP. Capabilities attach to the (harness, mode) pair; for driven launches the mode is capability-determined, never a preference. | Backend, session kind |
| **In-scope models** | The model list a harness's own picker offers, as configured in the harness itself and reported by its adapter. The shorthand-resolution corpus and web model picker source. | gmux-side model catalog, allowlist in gmux config |

## Session lifecycle

| Term | Definition | Avoid |
| --- | --- | --- |
| **Alive** | A current runner generation is present in the runtime registry. This is derived, never persisted. | Active |
| **Dead** | No current runner; a durable row may remain visible/resumable. Operational term only. | Deleted |
| **Resumable** | A dead, previously-run row for which its adapter verdict and derived command permit a new runner. The stored launch command alone is insufficient. | Every dead session |
| **Register** | Coordinator operation that probes a runner, validates its generation/identity, and transactionally merges facts into the durable row and placement. | In-memory upsert |
| **Resume** | Spawn a new runner for an existing dead session using an adapter-derived command while preserving session identity. | Rerun unconditionally |
| **Restart** | Stop a live runner and then resume the same session. | Resume |
| **Dismiss** | Recursively hide a launch subtree: stop live descendants, stamp `dismissed_at`, and remove placement. Rows, conversation identity, provenance, timestamps, and scrollback are retained. | Delete, forget |
| **Reconcile/remove** | Adapter-driven permanent removal of a retained row that is no longer resumable; surviving children are repaired/promoted. | Dismiss |
| **Fast-exit** | A child that exits during registration. It can still become a durable dead row. | Missed session |

## Turn and agent semantics

| Term | Definition | Avoid |
| --- | --- | --- |
| **Turn** | Generic unit of activity: hook-delimited agent work, prompt-mark-delimited shell command, or default process lifetime. `Status.Active` is its open/closed state. | Job |
| **Active** | Turn open. Distinct from **Alive** (runner residency) and output activity. | Running |
| **Inactive** | Turn closed; the state semantic waits observe. It does not imply the runner is dead. | Dead |
| **Interrupted** | Last turn intentionally stopped; orthogonal to error and cleared by the next terminal outcome. | Failed |
| **Error** | Adapter-reported failure/attention condition; orthogonal to Active. | Dead |
| **Unread** | New turn/output state not yet acknowledged by viewing the session. | Waiting section |
| **Logical activity** | Source-bounded span of agent work that `gmux wait` observes. | Exchange |
| **Visible exchange** | User-message-bounded display unit rendered by logs/reports. | Activity |
| **Iteration** | One completed assistant/model response, including tool-use or retried responses. | Turn |
| **Agent session** | Semantic resumable conversation addressed by `gmux agent`; runner residency is an implementation detail. | Agent process |
| **Runner incarnation** | One process generation hosting a session. Incarnation boundaries are not semantic events. | New session |

## Components and authority

| Term | Definition | Avoid |
| --- | --- | --- |
| **Runner** | A `gmux` process holding a child PTY, runner-authoritative live facts, a Unix socket, and scrollback files. | Session |
| **Daemon** | The per-host `gmuxd`: lifecycle coordinator, durable store owner, snapshot composer, broker, and peer client. | Cache only |
| **Central store** | Authoritative SQLite database for daemon-owned durable local sessions, projects, placements/order, and manual peers. | `store.Store`, JSON mirror |
| **Runtime registry** | In-memory overlay of live runner generations, PIDs, endpoints, and other non-durable runtime facts. | Durable store |
| **Coordinator** | `sessioncoord.Coordinator`, the serialization boundary for lifecycle operations crossing SQLite and runtime effects. | Endpoint mutex |
| **Snapshot composer** | Queries central state and combines runtime/peer overlays for REST and SSE wire projections. | Authority |
| **Adapter** | Tool-specific capabilities for launch, conversation lookup/rendering, resumability, and reconciliation. | Kind, plugin state store |
| **Conversation index** | Rebuildable in-memory lookup/search index populated by adapter `ConversationSource`s. It does not create durable/sidebar sessions. | Session discovery store |
| **Scrollback** | Runner-owned bounded PTY byte cache in `scrollback{,.0}` files. | Structured state, transcript |

### Ownership rules

- SQLite is authoritative for durable daemon-owned structured state.
- A live runner is authoritative for current runner facts; registration projects those facts into SQLite.
- Liveness and runner generation exist only in the runtime registry.
- Adapter conversations remain in adapter-owned storage; SQLite stores only adapter and opaque ref.
- Peer session/project snapshots are ephemeral projections, not offline replicas.
- Scrollback is a runner-owned cache outside SQLite.
- `projects.json`, `meta.json`, and `peers.json` are 1.x historical/compatibility formats, not v2 production authorities.

## Projects and peering

| Term | Definition | Avoid |
| --- | --- | --- |
| **Owned project** | Project whose rules and local-session placement are owned by this daemon. | Global project |
| **Project reference** | Viewer-side `(peer, slug, node ID)` entry opting into a network peer's advertised project. Connecting a peer alone does not create references. | Cross-host match rule |
| **Placement** | Derived membership of a full session ID in a project, with durable sibling order. | Slug membership |
| **Match rule** | Host-local path or remote criterion. Rules are ORed; longest path wins, then path over remote, then catalog order. | Fleet-wide grouping |
| **Hub** | Daemon the frontend uses; connects outward to spokes. | Master |
| **Spoke** | Peer daemon whose current projection is consumed by a hub. | Slave |
| **Network peer** | Token-authenticated remote host. Its snapshots disappear on disconnect and require project references for sidebar display. | Offline replica |
| **Local peer** | Narrow `Local=true` peer, currently a devcontainer. Parent daemon owns matching/placement for its namespaced ephemeral sessions. | Any local machine |

## Relationships and invariants

- A Session ID is durable identity; a session slug is mutable presentation.
- Project placement and order use full Session IDs, including `id@peer` for Local-peer placements.
- Network peers own their project stamps; viewers never rematch those sessions into local owned projects.
- The **parent** or **family edge** (`parent_session_id`) is the current behavioral parent; promotion severs it and reparenting replaces it.
- The **launch parent** (`launched_from_session_id`) is immutable provenance and drives nothing automatically; the web may suggest it as a safe Return to family target.
- Dismissing a parent recursively dismisses descendants but does not hard-delete their rows.
- Permanently removing a parent clears surviving direct children's parent IDs; it does not delete otherwise resumable descendants.
- Conversation takeover is based on adapter conversation lineage/ref semantics, not slug collision.
- `last_output_at` is the public recency field. Terminal outcome status can remain after runner death for wait/report semantics.

## Flagged ambiguities

- Use **Session** for the durable conceptual entity and **Runner** for a hosting process.
- Qualify **project slug** versus **session slug**.
- Use **Restart** only for stop-then-resume and **Resume** for spawning a dead session.
- Qualify **durable state**, **runtime state**, **adapter state**, or **wire projection** instead of saying only “state.”
- Use **Local peer** for the devcontainer-style ownership exception; “local host” means the daemon's own machine.
- “Session file” is retired. Say **conversation ref**, or the adapter's native term at its API boundary.
- Operational surfaces may say alive/dead. Semantic agent surfaces should say Active/Inactive plus the last outcome.
