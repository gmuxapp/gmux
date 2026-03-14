# Migration plan v1 (scaffold phase)

## Principles

1. Reuse behavior, not accidental complexity.
2. Preserve proven reliability properties (server truth, SSE-driven updates, isolated UI state).
3. Move contracts first, implementation second.
4. Keep each migration step shippable and reversible.
5. Explicitly re-evaluate old decisions before porting code.

## Rethink checkpoints (must answer before code import)

- Is this logic still necessary with `gmuxr` as metadata authority?
- Is this concern in the right component boundary (web/api/gmuxd/gmuxr)?
- Can this be represented as a contract/type instead of ad-hoc behavior?
- Does this preserve single source of truth on the server side?
- Can we test this behavior in isolation (unit) and end-to-end (lifecycle)?

## Source repository

- `/home/mg/dev/agent-cockpit`

## Phase 0 — scaffold (this commit)

- [x] create moon workspace
- [x] create polyglot directory layout
- [x] import core ADRs and protocol drafts

## Phase 1 — contract hardening

- [x] establish gmuxd REST envelope patterns and error code scaffold in `packages/protocol`
- [ ] finalize gmuxd REST v1 endpoint envelopes and error codes
- [x] establish session state/schema runtime types in `packages/protocol`
- [ ] finalize session metadata schema v1 and state machine
- [x] generate TS runtime-validated contract types in `packages/protocol`
- [x] define compatibility/versioning policy (`plans/versioning-release-policy.md`)

## Phase 2 — API/web bootstrap (TS)

- [x] scaffold `apps/gmux-api` with Hono + tRPC
- [x] scaffold `apps/gmux-web` with Preact + Vite
- [x] implement minimal vertical slice:
  - [x] list sessions via typed tRPC -> gmuxd REST path
  - [x] subscribe to updates (SSE)
  - [x] preserve UI-only selected session semantics

## Phase 3 — gmuxd bootstrap (native)

- [x] implement gmuxd health endpoint
- [ ] implement gmuxd capabilities endpoint
- [x] implement basic session registry in-memory
- [x] add events SSE endpoint
- [ ] add local file-backed persistence/reconciliation

## Phase 4 — gmuxr bootstrap (native)

- [ ] implement adapter interface (pi + generic)
- [ ] implement metadata writer and transition updates
- [ ] connect gmuxd ingestion path

## Phase 5 — behavior migration from cockpit

Migrate only after tests/specs exist in new repo.

Candidate reuse areas:

- session tracker logic and state mapping
- watcher event fanout patterns
- terminal attach lifecycle semantics (`is_new`)

Candidate rewrite areas:

- code coupled to old naming/runtime assumptions
- implicit mapping heuristics now replaced by gmuxr metadata authority
- mixed UI/server state ownership patterns

## Phase 6 — end-to-end reliability

- [ ] lifecycle tests: launch/attach/reconnect/kill
- [ ] startup reconciliation tests
- [ ] multi-node routing tests (node_id + session_id)
- [ ] manual UX pass for switching and waiting notifications

## Phase 7 — packaging and distribution

- [ ] define gmuxd/gmuxr release pipeline (native artifacts + checksums)
- [ ] package web assets into gmuxd local distribution
- [ ] define gmux-api deployment packaging (container + node runtime path)
- [ ] integrate patched abduco distribution strategy
- [ ] ship curl bootstrap installer for zero-install path
- [ ] add `gmux open` convenience launcher (browser app-mode when supported)

## Explicitly deferred from v1

- [ ] Electron desktop app (re-evaluate only after real shortcut pain data)

## Immediate next implementation task

Bootstrap `packages/protocol` with runtime-validated shared types and fixtures, then build a tiny `gmux-api` + `gmux-web` vertical slice against those fixtures.
