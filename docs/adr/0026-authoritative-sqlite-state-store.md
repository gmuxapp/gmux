# ADR 0026: Authoritative SQLite store for daemon-owned state

**Status:** Proposed
**Date:** 2026-07-15
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

A successful domain mutation commits to SQLite before any snapshot invalidation or side effect is published. The database is not an asynchronous mirror of an authoritative Go map.

The implementation stack is:

- `modernc.org/sqlite` through `database/sql`;
- sqlc-generated typed queries;
- embedded, checked-in Goose SQL migrations;
- a small gmux-owned `internal/centralstore` domain API.

Generated sqlc models and generic query methods remain private to `centralstore`. Callers use transactionally meaningful operations such as runner registration, dismissal, project reorder, adapter reconciliation, and manual-peer update—not Ent entities or generic CRUD repositories.

Two independent spikes validated this direction. `modernc.org/sqlite` built with `CGO_ENABLED=0` for Linux and Darwin on amd64 and arm64, enforced foreign keys/WAL, and passed concurrent domain transactions under the race detector. sqlc and Goose additionally validated reproducible module-pinned generation, embedded fresh and upgrade migrations, rollback, DB-backed snapshots, and a much smaller generated surface than Ent. Ent and Atlas were rejected for this model: Ent generated a broad entity framework while the important ordered-membership invariant still required specialized SQL, and the tested Atlas CLI workflow was not source-pinnable through the expected Go module path.

Production starts with one database connection. Additional read pooling is introduced only if snapshot profiling demonstrates contention.

### 2. Query-backed snapshot protocol; no durable-state projection map

The existing SSE wire protocol remains unchanged:

1. a domain transaction commits;
2. it signals a coalesced `sessions` and/or `world` invalidation;
3. gmuxd queries a complete snapshot from SQLite;
4. it sends `snapshot.sessions` and/or `snapshot.world`.

Transient `session-activity` remains a lossy direct event. The database replaces `sessions.List()` and `projects.Manager.Load()` as the source for local snapshots. A second authoritative in-memory session map is not maintained unless future profiling proves a need.

### 3. Ownership determines persistence

SQLite contains only daemon-owned structured state.

The following remain outside it:

- **Runner runtime state:** current liveness, PID, socket path, subscriptions, and transient activity. `alive` is derived from the live runner registry and is not persisted.
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

Adapters reconcile their retained candidates in batches and return a disposition equivalent to retain, remove, or unknown. This permits adapter-specific policy—conversation existence, temporary storage unavailability, shell age/LRU limits—without embedding those rules in the central store. Unknown results retain conservatively. Adapters return decisions and never write SQLite directly.

Confirmed removals are applied transactionally. Removing one parent does not remove otherwise resumable descendants; surviving direct children become roots.

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

Which project contains a session remains derived from the existing project matcher and its existing inputs (`cwd`, `workspace_root`, remotes, and host rules). This ADR does not redesign project identity or matching semantics.

Placement is recalculated at existing discovery/registration points and, in a later feature, when runner/adapter common state reports a CWD/workspace change or project rules change. A project change removes the old placement and appends the session to its newly derived project transactionally. Users do not manually move sessions between projects.

Ordering within a project is durable user state. It uses dense, zero-based integer positions among display siblings. Every affected sibling group is rewritten to its canonical `0..n-1` order in one collision-safe transaction. Fractional ranks and linked-list ordering are rejected: they optimize writes that are negligible at sidebar scale while adding exhaustion/rebalancing or integrity complexity.

Dismissed sessions have no placement or preserved order. If they register again, they are newly assigned at the normal bottom.

### 8. Parent-child provenance supports arbitrary depth

A nullable self-reference records the session that launched a session. Every genuinely new nested `gmux` launch automatically captures the inherited `GMUX_SESSION_ID`; resume/restart retain their existing identity semantics.

The adjacency model permits arbitrary depth because subagents may launch subagents. Initial product behavior only passively groups recorded relationships; no closure table, materialized path, or general subtree editor is introduced.

Parentage is provenance. `promoted_to_root` is separate, sticky, user-authored presentation state:

- when true, the session renders as a project root without losing its launch parent;
- when false, it groups beneath its parent only when both are visible in the same derived project;
- sessions whose parent resolves to another project render as roots while retaining provenance.

A parent and child derive projects independently; parentage does not override CWD/workspace matching. Dragging can reorder siblings and promote/unpromote relative to the original parent, but arbitrary adoption under an unrelated parent is out of scope.

Dismissing a parent recursively dismisses its descendants. Permanently reconciling away a parent does not delete resumable descendants; direct children are promoted to genuine roots and their missing parent reference is cleared.

### 9. Cross-entity changes are domain transactions

Operations whose invariants cross tables execute in one transaction. Examples include:

- runner registration plus dismissal clearing and conditional project assignment;
- dismissal plus recursive placement removal;
- project reorder;
- conversation-lineage takeover plus placement cleanup;
- adapter reconciliation deletion plus surviving-child promotion;
- project-rule changes plus affected placement updates;
- manual-peer identity deduplication and credential update.

Persistence cleanup is no longer driven by lossy generic store subscribers. Post-commit snapshot invalidation may be lossy/coalesced because a later signal reads the same authoritative state. Runtime effects such as notifications and waits remain explicit consumers of committed domain outcomes and are characterized during migration.

### 10. Migrations and failure behavior

SQL migrations are immutable, reviewed source files embedded into gmuxd and applied by Goose before state-serving startup. They are the schema history and sqlc schema input. sqlc is pinned through Go's module-managed tool mechanism; CI regenerates queries and fails on a diff. CI also migrates fresh and supported upgrade fixtures and verifies foreign keys.

A database open, integrity, or migration failure stops normal daemon startup. gmuxd never silently creates replacement state, falls back to JSON, or publishes an empty world after such a failure. The database, WAL, and SHM files are preserved for diagnosis.

The administrative namespace is backend-neutral:

```text
gmux daemon state check
gmux daemon state backup <path>
gmux daemon state export
```

`check` verifies migration status, SQLite integrity, and foreign keys; it may operate offline only after confirming the daemon does not own the database. `backup` uses a consistent SQLite backup mechanism rather than blindly copying a live main file. `export` emits deterministic JSON and redacts secrets by default. Exact online/offline mechanics are implementation details.

## Consequences

### Positive

- One committed representation owns daemon state; restart no longer exposes missed JSON writes.
- Session lifecycle, project placement, and peer updates can be atomic.
- Foreign keys prevent stale project membership.
- Versioned SQL migrations replace several bespoke format migrations.
- Snapshot reads remain simple and compatible with the current frontend protocol.
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

1. Add the isolated central-store foundation: opener, permissions/configuration, Goose migrations, sqlc generation, and fresh/upgrade/FK/rollback/cross-build tests.
2. Move local durable sessions and DB-backed snapshot reads while preserving runner ownership and SSE protocol.
3. Move projects, matching rules, placement, dense ordering, dismissal, and transactionally coupled session lifecycle behavior.
4. Move manual peers and tokens; preserve peer-owned runtime projections.
5. Replace adapter retention and conversation-deletion cleanup with batch reconciliation transactions while retaining runner scrollback files.
6. Add state check/backup/export, failure/recovery tests, race/stress tests, and full release size/build measurements.
7. Remove superseded in-memory-authority and JSON persistence paths. Do not add an old-state importer.
8. Add dynamic CWD/workspace reporting and reassignment as a separate follow-up after the storage cutover.
