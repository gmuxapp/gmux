# ADR 0016: dead-session retention — scrollback is a cache, meta mirrors conversation existence

**Status:** Accepted
**Date:** 2026-06-23
**Related:** ADR 0011 (runner-owned session state), ADR 0014 (adapter-owned conversation sources)

## Context

Each dead, never-dismissed session leaves a directory under
`$XDG_STATE_HOME/gmux/sessions/<id>/`:

- `meta.json` (~1 KB) — written by `sessionmeta` on every `Alive=false`
  landing. Powers the sidebar entry and the resume affordance, and survives a
  daemon restart.
- `scrollback` + `scrollback.0` — written *live* by the runner
  (`packages/scrollback`), capped at `2 × MaxBytes` ≈ **2 MiB per session**.

After ADR 0014 retired the daemon's file monitor, this directory was the sole
remaining unbounded-growth surface: a user who never dismisses accumulates one
dir per dead session forever. The big artifact is scrollback; meta is tiny and
is the thing that makes a session resumable and visible.

## Decision

Two tiers, with different lifetimes for the two artifacts.

### 1. Scrollback is a cache (aggregate byte cap)

`PruneScrollback` sums scrollback bytes across all dead sessions and, when the
total exceeds `ScrollbackCacheBytes` (`GMUX_SCROLLBACK_CACHE_MB`, default
**256 MiB**), deletes scrollback files **oldest-first** (by newest scrollback
mtime) until back under the cap. `meta.json` is **kept** — the session stays in
the sidebar and resumable; replay simply has no terminal history. That is the
accepted tradeoff: scrollback is reconstructible-by-rerun context, not a
durable record.

**Liveness guard.** The runner writes scrollback live into the same dir, so a
live session's scrollback must never be evicted. The store is the authority on
liveness; the prune skips any session ID the store reports `Alive`. At startup
this runs only from discovery's first-scan hook, *after* live runners have
re-registered as alive. Because the alive set is a snapshot, a session resumed
*during* the prune (or a runner the store hasn't registered yet) is covered by
a write-freshness guard instead: a victim whose scrollback mtime advanced past
its scan-time value is skipped — any writer bumps the mtime.

**When it runs.** At startup (first-scan hook) and, throttled to ≥12 h since
the last run, when a session is **dismissed**. No background timer: a periodic
tick would cause disk churn at moments unrelated to anything the user did.
Dismiss is a natural, user-initiated moment; startup covers restarts. The
throttle is tracked by the mtime of a `.scrollback-prune-stamp` marker.

### 2. `meta.json` mirrors conversation existence

A dead session's *whole* entry is retired only when its backing conversation
file disappears. The adapter conversation watchers (ADR 0014) already observe
file removals; the watcher-level sink reports every file-gone event —
including files the index never held (parse failure, `CanResume=false`, empty
cwd) — and gmuxd retires the dead session(s) whose `SessionFile` matches
(skipping any alive or peer-owned session — the mapping is N:1).
`sessions.Remove` broadcasts `session-remove`, which the existing
`WatchRemovals` loop catches to drop the dir.

The index only observes removals while gmuxd runs, so a file deleted *while the
daemon is down* would otherwise leave an orphan entry. A **startup reconcile**
closes that gap: after the first discovery scan (live runners flagged), gmuxd
asks each owning adapter, via the `ConversationProber` capability, whether its
dead sessions' conversation files are gone. The adapter distinguishes a genuine
deletion from *unreachable storage* (unmounted home, tool never installed) —
this is adapter-owned because only the adapter knows which directory anchors
"storage is present" for its layout. The shared `ConversationGoneAtRoot` helper
implements the common rule: a file absent under a present `SessionRootDir` is
deleted; an absent/unreadable root, or any non-`NotExist` stat error, is
undeterminable and never retires. Only a confident "gone" retires the entry.

**Conversation-less corpses** (shells, and anything that never reported a
`session_file`) have no conversation whose removal could retire them, so they
fall back to a whole-dir age/count cap at startup `Sweep`:
`GMUX_SESSION_RETENTION_DAYS` (default 30) and `GMUX_SESSION_RETENTION_MAX`
(default 200, LRU by exit time). Sessions *with* a conversation file are exempt
from this cap — their lifecycle is the conversation's.

## Consequences

- Agent session, conversation file present: meta + sidebar entry persist
  ~indefinitely (cheap); scrollback reclaimed under space pressure.
- Agent session, conversation file deleted while running: index notices → entry
  retires.
- Agent session, conversation file deleted while gmuxd was down: startup
  reconcile retires it on next launch (when the adapter confirms deletion).
- Shell corpse: aged/counted out.
- Bound holds across restarts (startup prune) and within a long run (dismiss).
  A daemon that never restarts and whose user never dismisses is the one gap;
  judged acceptable, since that user isn't generating new dead sessions either.
- All knobs accept a non-negative integer; `0` disables that limit.

## Known limitation

- Reconcile relies on `SessionRootDir` presence as the storage-availability
  anchor. If a tool's root sits on local disk but an individual conversation
  lives on a *separate* mount that's down, a missing file under a present root
  would read as deleted. This is exotic — these tools keep all conversations
  under one tree below `$HOME` — and the cost is one retired (~1 KB) entry, so
  we accept it. An adapter with such a layout can implement `ConversationGone`
  with finer logic (e.g. sibling-file probing) instead of the shared helper.
