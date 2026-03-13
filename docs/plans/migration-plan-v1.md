# Migration plan v1 (scaffold phase)

## Principles

1. Reuse behavior, not accidental complexity.
2. Preserve proven reliability properties (server truth, SSE-driven updates, isolated UI state).
3. Move contracts first, implementation second.
4. Keep each migration step shippable and reversible.

## Source repository

- `/home/mg/dev/agent-cockpit`

## Phase 0 — scaffold (this commit)

- [x] create moon workspace
- [x] create polyglot directory layout
- [x] import core ADRs and protocol drafts

## Phase 1 — contract hardening

- [ ] finalize gmuxd REST v1 endpoint envelopes and error codes
- [ ] finalize session metadata schema v1 and state machine
- [ ] generate TS types from schema/contracts in `packages/protocol`
- [ ] define compatibility/versioning policy

## Phase 2 — API/web bootstrap (TS)

- [ ] scaffold `apps/gmux-api` with Hono + tRPC
- [ ] scaffold `apps/gmux-web` with Preact + Vite
- [ ] implement minimal vertical slice:
  - [ ] list sessions (mock from protocol fixtures)
  - [ ] subscribe to updates (SSE)
  - [ ] preserve UI-only selected session semantics

## Phase 3 — gmuxd bootstrap (native)

- [ ] implement gmuxd health/capabilities endpoints
- [ ] implement basic session registry in-memory
- [ ] add events SSE endpoint
- [ ] add local file-backed persistence/reconciliation

## Phase 4 — gmux-run bootstrap (native)

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
- implicit mapping heuristics now replaced by gmux-run metadata authority
- mixed UI/server state ownership patterns

## Phase 6 — end-to-end reliability

- [ ] lifecycle tests: launch/attach/reconnect/kill
- [ ] startup reconciliation tests
- [ ] multi-node routing tests (node_id + session_id)
- [ ] manual UX pass for switching and waiting notifications

## Phase 7 — packaging and distribution

- [ ] define gmuxd/gmux-run release pipeline
- [ ] integrate patched abduco distribution strategy
- [ ] ship curl bootstrap installer for zero-install path

## Immediate next implementation task

Bootstrap `packages/protocol` with runtime-validated shared types and fixtures, then build a tiny `gmux-api` + `gmux-web` vertical slice against those fixtures.
