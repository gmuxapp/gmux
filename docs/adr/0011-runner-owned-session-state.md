# ADR 0011: Runner-owned session state; the daemon is a read cache

**Status:** Accepted
**Date:** 2026-06-16
**Related:** ADR 0004 (SessionStream), ADR 0010 (the agent-shim this supersedes)
**Partially superseded by:** ADR 0013 (codex authoritative state via hooks) and ADR 0014 (adapter-owned conversation sources)

> **Superseded prose (2026-07):** references below to the **daemon's**
> `FileMonitor` / inotify file-watch and to codex as "the only remaining
> file-watch consumer" are historical. ADR 0013 moved codex onto
> authoritative hooks, and ADR 0014 deleted daemon-global attribution
> entirely — conversation watching now lives in the adapter
> (`packages/adapter/filewatch`) and the daemon consumes adapter
> `ConversationSource`s (there is **no** daemon file-watch fallback). The
> core decision of this ADR — runner/adapter owns state, daemon is a read
> cache — stands; only the file-watch ownership detail changed.

> **Outcome.** Built and shipped. The decisive change beyond the original plan:
> the fragile fs signal (ADR 0010's shim) was replaced by an **agent-hook** —
> a pi extension (`pi -e <ext>`) that subscribes to pi's own session/agent
> lifecycle and reports the held file, title, and status *directly*. Because
> the hook reports everything, pi needs no runner-side file parsing and no
> daemon file-watch at all. The agent-shim, its read-inference debouncer, and
> the pi scrollback content matcher have been **removed**; codex (no hook) is
> the only remaining file-watch + metadata-attribution consumer. The phased
> plan below records how we got here; notes mark what the hook changed.

## Context

After ADR 0010, attribution (which conversation file a runner holds) is
authoritative and **runner-owned**: the agent-hook reports the file to the
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

1. **Live state is set in the runner.** A hooked agent (pi) reports its title
   and status to the runner directly, which sets `session.State` — no file
   parsing. An unhooked, file-driven agent (codex) is parsed from raw
   file-changed events; whether that parse runs in the runner or the daemon's
   file-watch, `session.State`/the store is the single source of truth for a
   live session. (As built, pi is fully hook-driven and codex still parses via
   the daemon file-watch.)

2. **The daemon `store.Store` is a read model.** Its only live feed is the
   runner `/events` stream (already built via re-announce). It no longer
   parses files or writes session content. Dead sessions are the last-known
   runner snapshot persisted to `meta.json` — the one piece of state the
   daemon owns, because no runner can once it has exited.

3. **File watching is fallback-only.** The daemon's inotify file-watch is
   needed **only for unhooked agents** (e.g. the Rust `codex`, which can't
   load a node/bun hook); hooked agents (pi) report path, title, and status
   directly and need nothing from the daemon's file machinery.

4. **Identity/attribution is adapter-owned, from the strongest available
   signal.** Two tiers, in order of authority:
   1. **Agent-hook** (primary). An agent with an extension API tells us the
      active session directly. The gmux pi hook (`pi -e <path>`, package
      `agentext`, capability `adapter.SessionExtender`) subscribes to pi's
      `session_start`/`session_switch`/`session_fork` and posts an
      authoritative `session` event (`getSessionFile()`) on every bind — the
      only signal that survives pi's cache-served `/resume`-select, where no
      file is read for an fs probe to observe. It also posts title and status
      from `agent_start`/`agent_end`.
   2. **Metadata** (fallback). For agents with no hook (codex), the daemon
      matches a changed file to a candidate session by cwd + start time
      (`adapter.FileAttributor`). A lone fresh candidate is attributed by
      mtime; ambiguous sets wait.

   The earlier ADR 0010 **agent-shim** (fs preload) and the **pi scrollback**
   content matcher were intermediate tiers; both are now removed. Daemon-side
   attribution survives only as `AttributeFromHook` (recording the hook's
   authoritative `session → file` and suppressing daemon parsing for that
   session) plus the codex metadata fallback.

5. **Attribution is keyed `session → file`, not `file → session` (N:1).** A
   conversation file can legitimately be open in more than one runner (you
   resume a session that's already running in another tab). The old daemon
   map `file → one session` makes the last writer clobber the previous one,
   so attribution "jumps" between tabs and an aborted runner leaves a stale
   binding. Because each runner authoritatively owns the one file it writes,
   the natural key is `session → file`: every session carries its own file
   and nothing collides. The daemon (or frontend) derives the inverse view
   `file → {sessions}`; when that set has more than one live session, both
   stay attributed and the UI shows a "this conversation is open in N tabs"
   warning rather than fighting over a single slot. Rebind becomes "a session
   updates its own file," eliminating the clear-other-files logic in
   `AttributeFromHook`.

This realises the principle that identity is adapter-owned and shells aren't special:
once the adapter owns state and identity in the runner, the daemon stops
making per-adapter decisions entirely.

## Migration plan (phased, each phase shippable)

- **Phase 0 — done (#315).** Runner owns *attribution*; daemon attribution
  is a re-announce-fed cache; `attributions.json` retired; scrollback
  demoted to a logged fallback.

- **Phase 1 — move file parsing into the runner.** Done, then largely
  obsoleted: the runner first parsed the file from shim deltas, but once the
  **hook** reported title/status directly (`agent_start`/`agent_end`), pi
  stopped parsing the file at all. The daemon's `FileMonitor` no longer writes
  `store.Store` for hooked sessions; the store is fed by runner `/events`.

- **Phase 1b — session-keyed attribution + duplicate-open warning (N:1).**
  The runner already owns `session → file` (`State.SessionFile`); surface it
  on the store session and let the frontend derive `file → {sessions}` and
  warn when a conversation is live in more than one runner. Fixes the
  attribution "jump" and the stale binding an aborted duplicate leaves
  behind. Lands ahead of Phase 2 because it only adds a field; the
  collision-prone `file → session` maps are deleted in Phase 2.

- **Phase 2 — remove the inference machinery.** Done: the agent-shim
  (fs preload + read-inference debouncer) and the pi scrollback content
  matcher are deleted. The daemon file-watch remains as the codex-only
  fallback (metadata attribution + parse); broadcasting raw file events to
  runners was unnecessary once pi went fully hook-driven.

- **Phase 3 — store is explicitly a cache.** Single live feed (runner
  `/events`), dead snapshots to `meta.json`. Collapse the remaining
  daemon-side session-state plumbing; the conversations index (URL/search,
  disk-scan) is unaffected.

## Consequences

- One owner per session (the runner), one store writer, adapter logic in one
  place. The "two update channels" and "duplicated truth" problems dissolve
  rather than being patched.
- Hooked sessions (pi) are self-contained; the daemon does zero file work for
  them. The daemon shrinks toward registry + cache + broker + frontend.
- pi state is push-based from the agent's own lifecycle, so there is no
  syscall inference to get wrong and no scrollback to fetch or match.
- codex remains daemon-parsed via the file-watch fallback until it grows a
  comparable hook (its own Rust process can't load a node/bun extension).

## Alternatives considered

- **Keep daemon-side parsing, just collapse the hook/inotify channels.**
  Patches the symptom; leaves the daemon owning file-driven state and the
  split intact. Rejected as a stopping point.
- **Push deltas as the source of truth (no disk re-read).** Simpler channel
  count but bets session display state on an unordered, droppable stream.
  Rejected: keep disk as SOT, reconciled runner-side.
- **Route file events by daemon-side attribution instead of broadcasting.**
  Keeps an attribution step in the daemon; contradicts adapter-owned
  identity. Broadcast preferred given file events are low-rate.
