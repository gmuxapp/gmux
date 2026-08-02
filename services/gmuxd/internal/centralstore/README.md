# Central store schema and domain operations

This package implements the authoritative SQLite store for daemon-owned state described by ADR 0026. Production opens it during gmuxd bootstrap and uses it through the singleton `sessioncoord.Coordinator`; the central snapshot composer and wire converter query it for REST and SSE projections.

SQLite owns durable local session rows, project definitions and rules, placement/order, and manual peers. It does not own live runner liveness, peer snapshots, adapter transcripts, or scrollback bytes:

- `sessioncoord.Registry` provides the runtime liveness/generation overlay.
- live runners supply runner-authoritative common facts through registration and observation;
- peer sessions/projects remain ephemeral connection projections;
- adapters own conversations and return bounded reconciliation decisions;
- runner-owned scrollback remains a file cache.

Domain operations preserve cross-entity invariants transactionally. Callers must use operations such as runner registration, recursive dismissal, reconciliation deletion, catalog replacement, and reorder rather than importing the private generated query package. Runner, adapter, socket, and process I/O occurs outside database transactions and is serialized/fenced by `sessioncoord` where required.

Important boundaries:

- SQLite cannot establish runner liveness by itself. Lifecycle operations that require a dead session rely on coordinator serialization and registry exclusion.
- `RegisterRunner` merges a validated runner observation with durable history and derived placement. Runtime generation is deliberately not persisted.
- Dismissal is recursive and hidden-not-forgotten: it stamps `dismissed_at` and removes placement while retaining session identity and provenance.
- Reconciliation deletion, unlike dismissal, permanently removes rows judged no longer resumable and repairs surviving child relationships.
- Project compatibility JSON is an API boundary, not the persistence schema. Relational project entries, rules, references, and placements are authoritative.

From the repository root:

```sh
# Regenerate checked-in private sqlc code with the module-pinned tool.
moon run gmuxd:generate-centralstore

# Fail when checked-in generated code drifts from fresh generation.
moon run gmuxd:check-centralstore-generated

# Validate the CGO-free package for release platforms.
moon run gmuxd:check-centralstore-cross-build

# Package checks.
cd services/gmuxd
go test -race ./internal/centralstore
```

Migrations under `migrations/` are immutable schema source. The released v1 baseline checksum is pinned by `TestReleasedV1MigrationChecksum`; change the schema only by adding a numbered migration. Upgrade tests retain sessions, projects, placements, and manual-peer secrets, verify integrity and foreign keys, and pin idempotent reopen behavior.

Goose atomicity is per migration, not across the whole pending set. Before applying pending migrations to an existing non-empty database, startup retains a timestamped owner-only `VACUUM INTO` backup under `backups/`. Fresh creation needs no backup. Stable SQL and generated files live under `internal/db`; Go's internal-package boundary prevents callers outside `centralstore` from using those primitives directly.
