---
title: Versioning
description: How gmux versions its artifacts and contracts.
---

## Principles

- Version user-facing artifacts, not internal implementation details.
- Keep release automation reviewable and reversible.

## What is versioned

- **`gmuxd`** and **`gmux`** binary versions (semver)
- gmuxd REST API via URL path (`/v1/...`)
- Session metadata schema (see [Session Schema](/develop/session-schema))

## Binary compatibility

gmuxd detects whether running gmux sessions match the current build using binary hash comparison. Mismatched sessions are marked **stale** in the UI — they still work, but may behave differently than newly launched sessions. See [Architecture](/architecture) for details.

## Automatic daemon upgrade

When `gmux` starts, it checks the running daemon's version via `/v1/health`. If the daemon reports a different version, `gmux` replaces it automatically. This happens transparently — existing sessions stay alive, and the new daemon rediscovers them.

All install methods handle this: Homebrew's postflight hook and the `curl | sh` installer both restart the daemon if it was running. Manual installs get the same behavior on the next `gmux` invocation.

Dev builds (`version=dev`) skip version checking and never replace — this avoids churn when running `dev-server.sh` alongside a production daemon.

## Contract versioning

Breaking API or schema changes require a new version prefix. Non-breaking additions (new optional fields) are fine within a version.

## What is NOT published

- `apps/gmux-web` is not a standalone npm package — it's embedded into gmuxd
- `packages/protocol` is internal to the monorepo
- If public SDK packages emerge later, they'll get their own versioning
