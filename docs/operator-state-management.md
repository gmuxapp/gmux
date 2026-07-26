# Operator: State Management

This page covers gmuxd state database administration, failure behavior, and E2E validation. For the design rationale, see [ADR 0026](./adr/0026-authoritative-sqlite-state-store.md).

## Fresh 2.0 state

gmux 2.0 starts with a clean SQLite database. **No JSON migration is performed.**

- The migration floor is the first 2.0 SQLite schema.
- Existing `meta.json`, `projects.json`, `peers.json` are **not imported**.
- On first run, gmuxd creates the database and seeds a default "home" project.
- If you have existing 1.x state, manually re-add peers via "Connect to host" and reconfigure projects via the UI.

## Fail-closed recovery

A database open, integrity, or migration failure **stops normal daemon startup**. gmuxd never silently creates replacement state, falls back to JSON, or publishes an empty world.

- The database, WAL, and SHM files are preserved for diagnosis.
- Run `gmux daemon state check` to diagnose the issue.
- If the database is corrupt, restore from backup (see below).

## State administration commands

All commands are available via `gmux daemon state <subcommand>` or `gmuxd state <subcommand>`.

### `gmux daemon state check`

Verifies migration status, SQLite integrity, and foreign keys.

- Can operate **offline** only after confirming the daemon does not own the database (no lock file).
- Reports any integrity violations or migration gaps.
- Exit code 0 on success, non-zero on failure.

### `gmux daemon state backup <path>`

Creates a consistent SQLite backup at the given path.

- Uses SQLite's online backup API for consistency (not a blind file copy).
- Can run while the daemon is active.
- **The backup contains manual peer tokens and must be treated as a secret.**
- File permissions are set to 0600 (owner-only).

### `gmux daemon state export`

Emits deterministic JSON to stdout.

- **Redacts peer tokens by default** (shows `token_present: true` instead of the value).
- Suitable for diagnostics, migration planning, or archival.
- Not a restoreable format; use `backup` for recovery.

## Backup secrets and permissions

- The state directory and database files are **owner-only** (mode 0700/0600).
- Raw database backups contain **manual peer tokens** (bearer credentials for remote hosts).
- Treat backups as secrets: do not store them in version control or shared locations.
- Use `gmux daemon state export` for diagnostics (redacts tokens by default).

## Container E2E commands

The production E2E suite runs inside a network-isolated Docker container. It builds the gmuxd binary, runs it against private state/runtime directories, and validates all acceptance scenarios.

### Fast profile (~11s)

Core scenarios only. Suitable for pre-commit checks.

```bash
GMUX_E2E_PROFILE=fast ./services/gmuxd/tools/production-e2e.sh
```

### Extended profile (~14s)

Adds 50 stress cycles. Suitable for CI merge gates.

```bash
GMUX_E2E_PROFILE=extended ./services/gmuxd/tools/production-e2e.sh
```

### Host isolation

The container runs with:
- `--network=none` — no network access
- `--read-only` — read-only root filesystem
- `--tmpfs` mounts for state, config, and temp directories
- `--pids-limit=512` — process limit

The host gmuxd daemon, state, and socket are never accessed.

### Via moon

```bash
moon run gmuxd:e2e-fast
moon run gmuxd:e2e-extended
```
