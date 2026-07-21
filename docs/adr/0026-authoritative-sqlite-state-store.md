# ADR 0026: Authoritative SQLite store for daemon-owned state

**Status:** Accepted
**Date:** 2026-07-15
**Accepted:** 2026-07-18
**Supersedes:** ADR 0012's decision to retain JSON stores
**Partially supersedes:** ADR 0016 (per-session `meta.json` lifecycle), ADR 0024 (`projects.json` membership representation)
**Related:** ADR 0001 (snapshot push protocol), ADR 0002 (share-nothing peers), ADR 0009 (CLI namespace), ADR 0011 (runner-owned session state), ADR 0022 (adapter-opaque conversation refs), ADR 0023 (unified turn model)

## Context

gmux persists daemon-owned state through several independent mechanisms:

- `internal/store.Store`, an authoritative in-memory session map;
- one `sessions/<id>/meta.json` per dead session;
- `projects.json`, containing project definitions, match rules, membership, and order;
- `peers.json`, containing manual peer configuration and tokens;
- asynchronous store subscribers that try to keep these representations aligned.

This split has repeatedly produced lifecycle seams. A representative bug is a dead session becoming unread again after daemon restart: selecting it clears `Unread` in the in-memory store, but no path rewrites its `meta.json`, so startup restores the older value. Dismissal, conversation takeover, project cleanup, retention, and resume have accumulated redundant synchronous and subscriber-driven cleanup to prevent similar resurrection and drift bugs.

ADR 0012 rejected SQLite because the then-motivating project-attribution bugs had been fixed without a relational store, and a migration would have bought mostly structural tidiness. The motivation is now different: the in-memory map and several independently persisted representations cannot provide one authoritative state transition across session lifecycle, project membership, and peer configuration. Fixing each missed write preserves the underlying failure mode.

The 2.0 rewrite permits a clean state start; importing the existing JSON and per-session metadata is not required.

## Decision

### 1. One authoritative database for daemon-owned structured state

gmuxd uses one local SQLite database as the immediate authority for:

- local durable sessions and their common state;
- project definitions and match rules;
- derived project placement and user-authored ordering;
- manual peer configuration, including peer tokens.

A successful domain mutation commits to SQLite before any snapshot invalidation or side effect is published. The database is not an asynchronous mirror of an authoritative Go map. “Authoritative” here means authoritative for the daemon's durable projection; it does not transfer ownership of live runner facts to gmuxd.

The implementation stack is:

- `modernc.org/sqlite` through `database/sql`;
- sqlc-generated typed queries;
- embedded, checked-in Goose SQL migrations;
- a small gmux-owned `internal/centralstore` domain API.

Generated sqlc models and generic query methods remain private to `centralstore`. Callers use transactionally meaningful operations such as runner registration, dismissal, project reorder, adapter reconciliation, and manual-peer update—not Ent entities or generic CRUD repositories.

Two independent spikes validated this direction. `modernc.org/sqlite` built with `CGO_ENABLED=0` for Linux and Darwin on amd64 and arm64, enforced foreign keys/WAL, and passed concurrent domain transactions under the race detector. sqlc and Goose additionally validated reproducible module-pinned generation, embedded fresh and upgrade migrations, rollback, DB-backed snapshots, and a much smaller generated surface than Ent. Ent and Atlas were rejected for this model: Ent generated a broad entity framework while the important ordered-membership invariant still required specialized SQL, and the tested Atlas CLI workflow was not source-pinnable through the expected Go module path.

Production starts with one database connection. Additional read pooling is introduced only if snapshot profiling demonstrates contention.

The accepted production boundary is `sessioncoord.Coordinator` for serialized lifecycle/domain operations, `centralstore.Store` for durable authority, and the central snapshot composer plus wire cache/fan-out for query-backed HTTP/SSE projections. The live registry and peering manager remain runtime projections only.

### 2. Query-backed snapshot protocol; no durable-state projection map

The existing SSE wire protocol remains unchanged:

1. a domain transaction commits;
2. it signals a coalesced `sessions` and/or `world` invalidation;
3. gmuxd queries a complete snapshot from SQLite;
4. it sends `snapshot.sessions` and/or `snapshot.world`.

Transient `session-activity` remains a lossy direct event. The database replaces `sessions.List()` and `projects.Manager.Load()` as the source for local snapshots. A second authoritative in-memory session map is not maintained unless future profiling proves a need.

A session-only or world-only invalidation may compose only its affected payload. A project or other cross-kind invalidation performs one composition pass that reads both session and world payloads in one SQLite read transaction, then queues/sends that matched pair in protocol order; a commit between the two network writes cannot change either payload. The existing invalidation mechanism remains level-triggered and coalescing: commit marks the affected snapshot kinds dirty; concurrent commits while a snapshot is composed leave another pass pending; dirty state is cleared only after a successful read/dispatch decision. No durable outbox or wire-visible revision is introduced because reconnect always receives a fresh initial snapshot.

### 2a. REST reads are store-direct; SSE reads are cache-composed

The coalesced async composer (§2) serves SSE subscriptions and their initial snapshots. REST GET endpoints that serve structured state (`/v1/sessions`, `/v1/projects`, `/v1/health`) read from the store directly at request time, applying the same runtime overlay and wire conversion as the composer. This ensures read-your-writes semantics for request-response clients (CLI, imperative web fetches, peering probes) without sacrificing SSE coalescing.

The composed fanout cache is not the authoritative read path for REST; it is an SSE delivery optimization. REST handlers must not read from the fanout cache for structured state that mutations have committed to the store.

### 3. Ownership determines persistence

SQLite contains only daemon-owned structured state.

The following remain outside it:

- **Runner runtime state:** current liveness, PID, socket path, subscriptions, and transient activity. `alive` is derived from the live runner registry and is not persisted.
- **Runner-authoritative common facts while live:** turn state (`working`, `unread`, and error), title/slug updates, conversation binding, lifecycle events, and terminal dimensions originate with the runner or its adapter. SQLite is their durable daemon projection, not a competing writer. Before serving the first startup snapshot, gmuxd discovers surviving runners, replays/merges their current authoritative facts transactionally, and only then completes startup reconciliation. Daemon-originated mutations that overlap runner ownership, such as read acknowledgement, are restricted to sessions with no live runner.
- **Runner-owned scrollback:** bounded `scrollback` files remain independently writable by runners, preserving the ownership and cache model of ADRs 0011 and 0016.
- **Adapter-owned conversations:** transcripts or database entries remain in their owning tools. SQLite stores only the adapter name and opaque `conversation_ref` defined by ADR 0022.
- **Peer-owned snapshots:** remote sessions/projects remain in-memory connection projections. Peers are share-nothing and re-announce their latest state; gmux does not create an offline replica. Only manual peer configuration is local durable state.
- **Transient notifications:** delivery timers, presence routing, and pending delivery state remain runtime-only until notification history receives a separate product design.
- **Bootstrap and runtime files:** Unix sockets, the auth token and similar cross-process bootstrap identity, and user-authored configuration remain dedicated files where their external/bootstrap contracts require it.

The state directory and database files are owner-only. Raw database backups contain manual peer tokens and must be treated as secrets. Export and diagnostics redact tokens by default.

### 4. Persist the common session model, not adapter internals

Adapters translate their own state into the common model gmux and its users consume. The initial schema has no opaque adapter payload.

Durable common state includes, where applicable:

- session ID, adapter, and opaque conversation reference;
- command and CWD;
- existing workspace-root and remotes values;
- title/subtitle and working/unread/error state;
- creation, start, exit, activity, and dismissal timestamps;
- exit code;
- last-known terminal columns and rows;
- parent-session provenance and root-promotion preference.

`command` and `remotes` may be JSON values because they are consumed as whole common values. Domain state and queried/constrained fields use ordinary columns. Timestamps are Unix milliseconds in SQLite and are converted to the existing wire representation at the API boundary.

Terminal dimensions are durable even though resize observations originate at runtime: dead replay rendering, CLI tail rendering, and resumed process initialization require the last-known geometry.

Neither `alive` nor `resumable` is persisted. Once initial runner discovery completes, liveness is derived from the runner registry. A retained row without a live runner is a resume candidate; an adapter-confirmed non-resumable row is removed.

### 5. Adapters own resumability and retention policy

The daemon does not distinguish “conversation-backed” from “conversation-less” sessions for retention. Every session has an adapter, including the default shell adapter.

Adapters reconcile their retained candidates in bounded batches and return a disposition equivalent to retain, remove, or unknown. This permits adapter-specific policy—conversation existence, temporary storage unavailability, shell age/LRU limits—without embedding those rules in the central store. Unknown results and partial/timeout failures retain conservatively. Adapters return decisions and never write SQLite directly.

Adapter I/O occurs outside database transactions. Each decision carries the observed row version and is applied conditionally; a stale decision or a candidate that has become live cannot be removed. Removing one parent does not remove otherwise resumable descendants; surviving direct children become roots. The editor adapter remains explicitly ephemeral and removes its session on close.

### 6. Dismissal means hidden, not forgotten

A dead runner or daemon restart does not imply that the user has finished with a session. A session remains visible and resumable while its adapter retains it. Scrollback continues to support seamless continuation.

Dismissal:

- sets nullable `dismissed_at`;
- removes project placement/order;
- retains the session row, conversation identity, parent provenance, promotion preference, and scrollback;
- recursively dismisses descendants after UI confirmation makes that scope clear.

There is no default destructive “forget completely” operation. Adapter reconciliation eventually removes sessions that are no longer resumable.

Every live local runner must be visible. Registering a genuinely new, previously dismissed, or surviving runner clears its dismissal. A session without current placement is assigned by the normal project matcher and appended using ordinary ordering rules. Re-registration after daemon restart preserves existing visible placement and ordering, making restart invisible apart from the client's reconnecting state.

The current shell `OnDismiss` behavior that destroys its rediscovery state is incompatible with this meaning and will be removed or reserved for a future explicit destructive operation.

### 7. Project membership is derived; ordering is durable

Which owned project contains a local session remains derived from the existing project matcher and its existing inputs (`cwd`, `workspace_root`, and remotes). This ADR does not redesign project identity or matching semantics.

ADR 0025's complete host-ownership model is preserved:

- ordered project entries may be owned projects or references identified by `(peer identity, remote slug)`; owned and referenced entries with the same slug may coexist;
- network-peer session/project placement is an ephemeral projection stamped by its origin and is never rematched or persisted locally;
- a `Local` peer remains the narrow exception: the parent applies its owned-project matcher and owns the resulting placement/order for namespaced ephemeral session keys. Those placement rows may be durable daemon-owned state while connected, but are pruned when the Local peer disappears and do not make the peer's session snapshot durable.

Placement is recalculated at existing discovery/registration points and, in a later feature, when runner/adapter common state reports a CWD/workspace change or project rules change. A project change removes the old placement and appends the session to its newly derived project transactionally. Users do not manually move sessions between projects.

Ordering within a project is durable user state. It uses dense, zero-based integer positions among display siblings. The schema uses an explicit non-null sibling-scope key for roots and grouped children, avoiding SQLite's nullable-unique-index hole. Promotion, unpromotion, project reassignment, dismissal, and parent deletion rewrite every affected old/new scope to its canonical `0..n-1` order in one collision-safe transaction. Fractional ranks and linked-list ordering are rejected: they optimize writes that are negligible at sidebar scale while adding exhaustion/rebalancing or integrity complexity.

Dismissed sessions have no placement or preserved order. If they register again, they are newly assigned at the normal bottom.

### 8. Parent-child provenance supports arbitrary depth

A nullable parent-session ID records the session that launched a session. Every genuinely new nested `gmux` launch automatically captures the inherited `GMUX_SESSION_ID`; resume/restart retain their existing identity semantics. This is intentionally not a foreign key: the PTY child starts before the parent's asynchronous daemon registration completes, so a child may durably register first. The private domain API keeps the relationship immutable, rejects cycles as missing parents arrive, and owns deletion repair.

The adjacency model permits arbitrary depth because subagents may launch subagents. Initial product behavior only passively groups recorded relationships; no closure table, materialized path, or general subtree editor is introduced.

Parentage records launch provenance while both rows are retained; it is immutable during that lifetime and dismissal does not alter it. `promoted_to_root` is separate, sticky, user-authored presentation state:

- when true, the session renders as a project root without losing its launch parent;
- when false, it groups beneath its parent only when both are visible in the same derived project;
- sessions whose parent resolves to another project render as roots while retaining provenance.

A parent and child derive projects independently; parentage does not override CWD/workspace matching. Dragging can reorder siblings and promote/unpromote relative to the original parent, but arbitrary adoption under an unrelated parent is out of scope.

Dismissing a parent recursively dismisses its descendants. Permanently reconciling away a parent does not delete resumable descendants; the deletion transaction explicitly clears its direct children's parent IDs, makes them genuine roots, and intentionally forgets that deleted relationship. Their own descendant relationships remain intact.

### 9. Cross-entity changes are domain transactions

Operations whose invariants cross tables execute in one transaction. Examples include:

- runner registration plus dismissal clearing and conditional project assignment;
- dismissal plus recursive placement removal;
- project reorder;
- conversation-lineage takeover plus placement cleanup;
- adapter reconciliation deletion plus surviving-child promotion;
- project-rule changes plus affected placement updates;
- manual-peer identity deduplication and credential update.

Persistence cleanup is no longer driven by lossy generic store subscribers. Post-commit snapshot invalidation is level-triggered/coalesced because each pass reads the same authoritative state. Runtime effects such as notifications and waits remain explicit consumers of committed domain outcomes and are characterized during migration.

A singleton-wide lifecycle coordinator serializes operations that cross the database/runtime boundary—registration, resume, restart, dismissal, reconciliation, and takeover—rather than relying on endpoint-specific mutexes. Database changes use conditional predicates; adapter, process, socket, and kill/wait I/O never occurs inside a database transaction. Startup discovery reconciles surviving runners and repairs a crash between launch and registration. Durable pending-operation claims are deferred unless gmux later permits multiple daemon writers or requires exactly-once launches across daemon crashes.

Manual-peer rows are likewise durable authority while the peering manager is an idempotent runtime projection. Startup and post-commit reconciliation converge runtime connections from committed rows; a commit followed by reconnect failure does not roll durable configuration back.

### 10. Migrations and failure behavior

SQL migrations are immutable, reviewed source files embedded into gmuxd and applied by Goose before state-serving startup. They are the schema history and sqlc schema input. sqlc is pinned through Go's module-managed tool mechanism; CI regenerates queries and fails on a diff. The migration floor is the first 2.0 SQLite schema—existing JSON state is intentionally outside the upgrade graph. CI migrates fresh and every released SQLite schema fixture and verifies foreign keys.

A database open, integrity, or migration failure stops normal daemon startup. gmuxd never silently creates replacement state, falls back to JSON, or publishes an empty world after such a failure. The database, WAL, and SHM files are preserved for diagnosis.

The administrative namespace is backend-neutral:

```text
gmux daemon state check
gmux daemon state backup <path>
gmux daemon state export
```

`check` verifies migration status, SQLite integrity, and foreign keys; it may operate offline only after confirming the daemon does not own the database. `backup` uses a consistent SQLite backup mechanism rather than blindly copying a live main file. `export` emits deterministic JSON and redacts secrets by default. Exact online/offline mechanics are implementation details.

For operational procedures (fresh 2.0 state, fail-closed recovery, backup secrets, E2E commands), see [Operator: State Management](../operator-state-management.md).

## Consequences

### Positive

- One committed representation owns daemon state; restart no longer exposes missed JSON writes.
- Session lifecycle, project placement, and peer updates can be atomic.
- Foreign keys prevent stale project membership.
- Versioned SQL migrations replace several bespoke format migrations.
- Snapshot reads remain simple and compatible with the current frontend protocol.
- REST reads provide read-your-writes consistency by construction; the eventual-consistency window is confined to the SSE push channel where it is the expected delivery model.
- The domain API contains SQLite/sqlc details and remains replaceable.
- The schema naturally supports future nested-agent sidebar grouping.

### Costs and risks

- The pure-Go SQLite driver adds roughly 5–6 MiB to a minimal stripped binary and increases cold build time; full release measurements remain required.
- SQLite serializes writes. gmux begins with one connection and must add telemetry/profile before introducing pooling.
- The rewrite touches session lifecycle, projects, peers, startup, waits, notifications, snapshots, retention, and operational tooling.
- SQL migrations and generated queries add developer tooling and CI requirements.
- A single corrupted database has a wider blast radius than one malformed JSON file, making fail-closed startup, backup, and diagnostics necessary.
- Existing 2.0 development state is not imported.

## Rejected alternatives

- **Patch individual `meta.json` write omissions.** Fixes known call sites but preserves the multiple-authority design and future recurrence.
- **Periodically serialize the in-memory store.** Simpler than subscribers but cannot atomically maintain projects/peers and leaves a crash window between memory and disk.
- **A single JSON snapshot.** Removes scattered metadata but provides no foreign keys or cross-entity transactions and turns unrelated updates into whole-world rewrites.
- **Reliable persistence subscriber.** Still makes durable state a reaction to another authority and couples correctness to event delivery.
- **Ent + Atlas.** Strong generated entity API, but disproportionate generated/runtime machinery for a small, operation-oriented schema; specialized ordering SQL remains necessary, and the validated CLI pinning path failed.
- **Raw `database/sql` only.** Viable but retains manual scan/type plumbing that sqlc removes at low generated cost.
- **GORM/Bun/other runtime ORMs.** Weaker compile-time query guarantees or broader runtime abstraction than this SQL-first model requires.
- **Turso/libSQL.** Replication, credentials, service availability, CGO/runtime complexity, and conflict semantics provide no benefit to the local, share-nothing ownership model.
- **Persist scrollback in SQLite.** Would give runners shared database-write responsibility and put high-churn terminal bytes into the structured state store.
- **Persist peer-owned snapshots.** Creates an offline replica contrary to share-nothing ownership and introduces stale-state semantics.
- **Fractional or linked-list ordering.** Optimizes insignificant row writes at the cost of occasional rebalance paths or graph-integrity problems.

## Implementation outline

The cutover uses endpoint-sized tracer bullets rather than horizontal “sessions first, projects later” phases. No shipped intermediate state may split one cross-entity invariant between SQLite and JSON or rely on durable dual writes.

1. Add the isolated central-store foundation and domain interface: opener, owner-only modes, connection configuration, Goose migrations, sqlc generation, transaction-bound query tests, and fresh/FK/rollback/cross-build gates. Production authority is unchanged.
2. Move dead-session read acknowledgement end to end: durable mutation, committed outcome, snapshot invalidation, and restart characterization. This proves the motivating seam with minimal live-runner interaction.
3. Move new local registration, owned-project placement, startup runner convergence, and transaction-consistent snapshot reads as one vertical slice.
4. Move exit, durable resume candidacy, serialized resume/restart, and same-ID re-registration, including crash/race characterization around external process effects.
5. Move dismissal and parent hierarchy: dense scoped ordering, promotion, recursive dismissal, wait/notification outcomes, and retained scrollback.
6. Move conversation takeover and bounded adapter reconciliation, applying external decisions conditionally and protecting live/changed rows.
7. Move project editing, ordering, references, and the Local-peer exception after hierarchy scope invariants are executable.
8. Move manual peers/tokens and idempotent runtime peering reconciliation while preserving peer-owned ephemeral projections.
9. Remove the superseded in-memory/JSON authorities only after every production read/write route uses the domain interface. Add state check/backup/export, restore and failure drills, race/stress tests, and full release size/build measurements before enabling the new path by default. Do not add an old-state importer.
10. Add dynamic CWD/workspace reporting and reassignment as a separate follow-up after the storage cutover.
