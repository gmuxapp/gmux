# Central store schema and ordering primitives

This package is an isolated SQLite implementation slice related to ADR 0026. It
is **not** wired into gmuxd and is **not an authoritative domain kernel yet**.
The current public surface provides schema-backed session fact/version
primitives, a conditional dead-session acknowledgement tracer, an atomic
nonproduction runner-registration and runner-observation operations, bootstrap
project-catalog construction, and collision-safe placement ordering primitives.
The sibling `sessioncoord` package provides a test-only subscribe-first
coordinator and runtime-only generation registry; neither package is wired to
production.

Important scope limits:

- `ReplaceProjectCatalog` is bootstrap-only. It returns
  `ErrCatalogHasPlacements` once any local or Local-peer placement exists,
  because this slice does not own the match inputs needed to rematch subjects
  authoritatively after rule changes.
- Direct caller-selected placement is an ordering primitive, not proof of
  derived project membership.
- `AcknowledgeDeadSession` is an isolated, nonproduction tracer. SQLite cannot
  determine runner liveness: a lifecycle coordinator must establish that no
  runner is live immediately before calling it.
- `RegisterRunner` atomically merges a caller-proven runner observation with
  durable history and derived owned-project placement. It performs no runner
  I/O. Immediately before calling it, the singleton lifecycle coordinator must
  hold lifecycle serialization, validate that its reserved runtime generation
  is still current, and supply `NewGeneration` provenance for replacement,
  resume, or restart observations. Generation is deliberately not persisted.
  The operation is nonproduction until that coordinator owns registration,
  resume/restart, dismissal, and event ordering. Production integration,
  recursive dismissal, reconciliation batching, and lifecycle liveness checks
  remain future work. Callers must not fill those gaps with raw
  access to the private generated queries. `ApplyRunnerObservation` uses the
  coordinator's observed row-version token, preserves daemon-owned history,
  and advances activity only for runner activity/death transitions.
- Peer snapshots, tokens, waits/notifications, adapter batching/takeover, and
  dynamic CWD reporting remain outside this slice.

From the repository root:

```sh
# Regenerate checked-in private sqlc code with the module-pinned tool.
moon run gmuxd:generate-centralstore

# Fail when the checked-in generated code drifts from a fresh regeneration.
moon run gmuxd:check-centralstore-generated

# Validate the CGO-free package for release platforms.
moon run gmuxd:check-centralstore-cross-build

# Package checks.
cd services/gmuxd
go test -race ./internal/centralstore
```

The `check-centralstore-generated` and `check-centralstore-cross-build` tasks
are not yet reachable from the CI task graph; wiring them into CI belongs to
the operational integration slice.

Migrations under `migrations/` are immutable schema source. Stable query SQL and
checked-in generated files live under `internal/db`; transaction orchestration
uses generated `Queries.WithTx`, and Go structurally prevents imports of those
primitives from outside `centralstore`.
