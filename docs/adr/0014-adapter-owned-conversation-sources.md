# ADR 0014: conversation discovery is adapter-owned; no daemon-global file monitor

**Status:** Accepted
**Date:** 2026-06-21
**Related:** ADR 0011 (runner-owned session state), ADR 0013 (codex hooks), ADR 0012 (relational storage a non-goal)

## Context

ADRs 0011/0013 made live session state (held file, title, status) **runner-owned**:
pi, codex, and claude each report authoritatively via an agent hook over the
runner socket. ADR 0013 named the precondition for retiring the daemon's
metadata-attribution + file-parse engine — *"removed only once a hooks-capable
codex is the supported floor"* — and deferred that `filemon-simplify` work.

With all three agent adapters hooked, that precondition holds. The daemon's
`discovery/filemon.go` (1075 lines) was then doing two unrelated jobs:

1. **Attribution + status parsing** — guessing which live session owned a file
   (`FileAttributor`) and parsing JSONL for title/status (`FileMonitor.ParseNewLines`).
   Hooks made this dead: the runner reports the file (recorded on
   `store.Session.SessionFile`) and the state, and the parse path was already
   guarded off for hooked sessions.
2. **Conversation discovery** — watching every adapter root for `.jsonl`
   changes to keep the `conversations` index current (URL resolution for dead
   conversations, and future search). This is the only durable need: a dead
   conversation has no runner, so it can never be hook-driven.

The daemon "knowing" each tool's on-disk layout (codex's `YYYY/MM/DD` nesting
via `SessionFileLister`, pi/claude per-cwd dirs) was the smell — discovery
mechanics lived in the daemon while the layout knowledge lived in adapters.

## Decision

**Delete the daemon-global file monitor. Conversation discovery is an adapter
capability.**

- Job (1) is removed outright: `FileAttributor`, `FileMonitor`/`ParseNewLines`,
  the metadata matcher, and the retry/stickiness/mtime machinery are gone. When
  an adapter can't be hooked (codex below the hook floor, a shell-wrapped argv
  where injection is lost), the session simply runs without daemon-reported live
  state — **there is no fallback**.
- Job (2) is inverted into `adapter.ConversationSource`: an adapter emits a
  startup `SnapshotConversations` and ctx-scoped incremental `WatchConversations`
  changes to a `ConversationSink`. The adapter owns the *mechanism*; the daemon
  owns nothing tool-specific. File-backed adapters build on `packages/adapter/filewatch`,
  a reusable recursive tree watcher; the daemon (`conversations.Index.Snapshot` /
  `WatchSources`) just supervises sources and feeds the index.
- Resume no longer needs the watcher: `ResolveResumeCommand` is a stateless
  function keyed off `store.Session.SessionFile`, which persists across restarts
  (the old in-memory attribution map did not).

This collapses `SessionFiler`+`SessionFileLister`+`FileMonitor`+`FileAttributor`
(and the daemon's path→adapter map) toward a single adapter-owned source: a
recursive `.jsonl` walk covers both pi/claude's per-cwd layout and codex's
date-nested one, so `SessionFileLister` and the per-adapter `ListSessionFiles`
are gone.

## Consequences

- **`fsnotify` moves from `services/gmuxd` into `packages/adapter`** (via
  `filewatch`). This is the deliberate cost of adapter-owned monitoring: a
  file-backed adapter legitimately owns a file watcher, and co-locating it is
  what lets the daemon stop understanding tool layouts. The runner imports
  `adapter` and so gains the dependency transitively.
- **The conversations index now takes concurrent writes** — one source
  goroutine per adapter, where `filemon` was a single goroutine. The index is
  mutex-guarded, so this is safe (verified under `-race`).
- **Search drops in cleanly** (ADR 0012): a fulltext index subscribes to the
  same `ConversationSource` event stream rather than a daemon-global watcher,
  consistent with the index being a derived, rebuildable structure.
- **Non-file adapters fit without new daemon code**: a future DB-backed adapter
  implements `ConversationSource` with a poller/subscription; the daemon's sink
  is unchanged ("watcher *or otherwise*").

## Alternatives

- **Keep a daemon-side generic watcher fed by `SessionFiler` roots.** Less
  inversion, keeps `fsnotify` out of the adapter module — but leaves the daemon
  knowing tool layouts and keeps the four-interface capability zoo. Rejected:
  the present-day payoff (collapsing interfaces, deleting the god-module) is
  real, not speculative.
- **Adapter emits parsed `ConversationInfo` instead of file paths.** Moves
  parsing ownership too and duplicates the index's `Info` type across the module
  boundary. Rejected: the daemon already owns `ParseSessionFile` via the index;
  a path-level sink keeps the seam thin.
