# Central store foundation

This package is the isolated SQLite foundation described by ADR 0026. It is not
used by gmuxd startup yet. The initial schema contains only a small metadata
key/value table; durable sessions, projects, and peers belong to later slices.

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

Migrations under `migrations/` are immutable schema source. Query SQL and
checked-in generated files live under `internal/db`, so Go structurally prevents
imports from outside `centralstore`.
