# ADR 0011: Runner-owned session state; the daemon is a read cache

**Status:** Proposed
**Date:** 2026-06-16
**Related:** ADR 0004 (SessionStream), ADR 0010 (agent-shim attribution)

## Context

After ADR 0010, attribution (which conversation file a runner holds) is
authoritative and **runner-owned**: the agent-shim reports the file to the
runner, the runner records it on `session.State` and replays it, and the
daemon's attribution map is a derived cache fed by that replay.

The rest of session state is **not** owned consistently. Today it splits by
adapter kind:

- **shell** (PTY-driven): the runner's `adapter.Monitor` sets runner
  `session.State` (status/title), which flows to the daemon over `/events`.
- **pi/claude/codex** (file-driven): the **daemon's** `FileMonitor` watches
  the file, runs `adapter.ParseNewLines`, and writes `store.Store` directly
  (`filemon.go` `store.Update`). The runner's state is bypassed for content.

So the daemon `store.Store` has **two independent writers** (the
`Subscriptions` consumer of runner `/events`, and `FileMonitor` parsing
files), and adapter logic lives in **two places** (the runner for PTY, the
daemon for files). This split is the root of the remaining attribution
machinery and the duplicated "session file" state between runner and daemon.

## Decision (proposed)

Invert ownership so the **runner's adapter owns all state for its session**,
and the **daemon is a read cache plus a raw file-event source**.

1. **Adapter parsing runs in the runner.** The runner feeds the shim delta
   (and, for unshimmed agents, raw file-changed events from the daemon) to
   its adapter, which updates `session.State`. `session.State` is the single
   source of truth for a live session.

2. **The daemon `store.Store` is a read model.** Its only live feed is the
   runner `/events` stream (already built via re-announce). It no longer
   parses files or writes session content. Dead sessions are the last-known
   runner snapshot persisted to `meta.json` — the one piece of state the
   daemon owns, because no runner can once it has exited.

3. **The daemon's file watching becomes a raw event source.** inotify emits
   `{path, bytes}` with no parsing, no adapter logic, no attribution. It is
   needed **only for unshimmed agents** (e.g. the Rust `codex`); shimmed
   agents get path + delta directly from their shim and need nothing from
   the daemon.

4. **Identity/attribution is adapter-owned, in the runner.** Raw file events
   are broadcast to same-kind runners; each adapter decides "is this mine?"
   (by the shim's authoritative path, by cwd, or — last resort — by content).
   Daemon-side attribution (`AttributeFromShim`, `tryAttributeUnmatched`, the
   `attributions`/`shimFiles`/`shimCovered` maps, the `FileAttributor`
   fallback) is removed.

This realises ADR 0009's "identity is adapter-owned; shells aren't special":
once the adapter owns state and identity in the runner, the daemon stops
making per-adapter decisions entirely.

## Migration plan (phased, each phase shippable)

- **Phase 0 — done (#315).** Runner owns *attribution*; daemon attribution
  is a re-announce-fed cache; `attributions.json` retired; scrollback
  demoted to a logged fallback.

- **Phase 1 — move file parsing into the runner.** For shimmed sessions the
  shim already delivers path + delta; the runner runs `ParseNewLines` and
  sets its own `session.State` (status/title/unread/slug). The daemon's
  `FileMonitor` stops writing `store.Store` for content; the store is fed
  only by runner `/events`. This unifies pi/claude onto the same path shell
  already uses and removes one of the two store writers. Highest-risk phase:
  needs status/title parity tests per adapter.

- **Phase 2 — daemon file-watch → raw broadcast.** The daemon emits raw
  file-changed events to same-kind runners; adapters claim/ignore. Delete
  daemon-side attribution and the scrollback content matcher (pi-only).
  Unshimmed agents (codex) now attribute + parse entirely in their runner
  (which already links the adapter).

- **Phase 3 — store is explicitly a cache.** Single live feed (runner
  `/events`), dead snapshots to `meta.json`. Collapse the remaining
  daemon-side session-state plumbing; the conversations index (URL/search,
  disk-scan) is unaffected.

## Consequences

- One owner per session (the runner), one store writer, adapter logic in one
  place. The "two update channels" and "duplicated truth" problems dissolve
  rather than being patched.
- Shimmed sessions are self-contained; the daemon does zero file work for
  them. The daemon shrinks toward registry + cache + broker + frontend.
- Loss/ordering of shim deltas becomes a runner-local concern: the runner
  reconciles deltas against its own on-disk file (it has the path), so disk
  stays the loss-proof source of truth inside the runner and the daemon
  never handles raw content.
- More processes do small parse work (one per session) instead of one daemon
  loop; file events are low-rate, so the fan-out from broadcasting is cheap.

## Alternatives considered

- **Keep daemon-side parsing, just collapse the shim/inotify channels.**
  Patches the symptom; leaves the daemon owning file-driven state and the
  split intact. Rejected as a stopping point.
- **Push deltas as the source of truth (no disk re-read).** Simpler channel
  count but bets session display state on an unordered, droppable stream.
  Rejected: keep disk as SOT, reconciled runner-side.
- **Route file events by daemon-side attribution instead of broadcasting.**
  Keeps an attribution step in the daemon; contradicts adapter-owned
  identity. Broadcast preferred given file events are low-rate.
