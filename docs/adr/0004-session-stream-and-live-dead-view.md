# ADR 0004: `SessionStream` and the live↔dead view abstraction

**Status:** Proposed
**Date:** 2026-05-04
**Related:** ADR 0003 (Daemon-initiated resume passes Session.ID to the runner)

## Context

A user attached to a session whose runner exits expects the
terminal view to **stay where it is**: the bytes already on screen
remain visible, the session transitions into a read-only state,
and an action bar appears offering Resume / Dismiss. Today the
behavior is the opposite: the active xterm is unmounted, a fresh
xterm is mounted by a separate `ReplayView` component, and that
component refetches the bytes the user already saw from
`/v1/sessions/<id>/scrollback`. The visual identity of the view
is destroyed and recreated across an alive→dead transition, with
flicker, sometimes-missing trailing bytes, and a re-instantiated
WebGL context.

The shape that produces this is two components for two transports:

- **`TerminalView`** owns the WebSocket attach for live sessions
  and writes incoming bytes into an xterm.
- **`ReplayView`** owns the HTTP scrollback fetch for dead sessions
  and writes the response into a different xterm.

`main.tsx` selects between them on `session.alive`. Each component
mounts its own xterm; each tears it down on the alive flip; each
maintains its own write path into a freshly-instantiated terminal.

This shape produces a recurring set of problems:

1. **Live↔dead transitions destroy view identity.** The xterm
   buffer, scroll position, selection, addon state, and WebGL
   context the user was looking at are all discarded on alive flip.
   The replacement is reconstructed from disk bytes that may not
   match what was last on screen (the runner's final flush may not
   have hit disk before exit).
2. **Two paths to "what the user sees".** Live = WS bytes from the
   runner's ptyserver. Dead = HTTP fetch from disk file. Maintained
   by different components, with different cap policies (runner
   ring buffer 128 KiB; disk file 2 MiB). Whatever the disk file
   contains is what dead-view shows; whatever the runner ring
   buffer contains is what late-attach to a live session shows.
   The two are designed to be consistent but maintained
   independently.
3. **Tab switches re-fetch.** Switching to a previously-viewed
   session unmounts its xterm. Switching back rebuilds from the
   network: scrollback re-fetched if dead, runner replay buffer
   re-streamed if alive. Image protocol state (sixel, iTerm
   protocol) is not safely replayable from a text-only ring buffer,
   and this is why the codebase carries a cols-±1 resize hack on
   tab-switch — to force TUIs to repaint from scratch since their
   image state was lost in the truncated replay.
4. **Activity-timing depends on detach.** gmuxd's
   `session-activity` SSE event fires when the runner emits output
   while no clients are attached. The unread/notify UX is built on
   this signal. The signal is approximate (it requires a viewer to
   detach to fire) and noisy (ring-buffer rotation moments
   coincidentally count as activity).
5. **Resume seam is unrepresentable.** When a dead session is
   resumed in place, the user has been looking at the dead view
   (in `ReplayView`), and the alive flip causes Preact to unmount
   it and mount a fresh `TerminalView` whose xterm is empty and
   whose WebSocket replay shows the new runner's first 128 KiB of
   output. There is no way to mark "here is where the old run
   ended and the new run began" because the two runs are rendered
   into two different xterms.

The root cause is that the bytes on screen are not a durable
artifact of the session. Today they are a downstream consequence
of whatever transport is active.

## Decision

Introduce **`SessionStream`**, a frontend abstraction that owns the
per-session byte stream as a first-class durable artifact, decouples
it from any particular xterm instance, and serves as the single seam
between transports (live WS, cold-load HTTP fetch) and presentation
(xterm subscriber).

### Shape

```ts
// apps/gmux-web/src/session-stream.ts

class SessionStreamRegistry {
    constructor(store: Store)
    get(sessionId: string): SessionStream  // lazy create
    // disposes when the store removes the session
}

class SessionStream {
    readonly state: ReadonlySignal<StreamState>
    onOutput(cb: (ev: OutputEvent) => void): () => void
    dispose(): void
}

type OutputEvent =
    | { kind: 'reset' }                        // clear xterm before next bytes
    | { kind: 'bytes'; data: Uint8Array }

type StreamState =
    | { phase: 'bootstrapping' }
    | { phase: 'streaming'; live: true }
    | { phase: 'frozen'; exitCode: number | null }
    | { phase: 'reattaching' }
    | { phase: 'error'; message: string }
```

`TerminalView` becomes a presentational subscriber: it instantiates
an xterm on mount, subscribes to `stream.onOutput`, and on each
event either calls `term.clear()` (reset) or `term.write(ev.data)`
(bytes). On unmount it unsubscribes and disposes the xterm. The
WebSocket and the HTTP fetch are no longer the component's concern.

### Pool bytes, not xterm instances

`SessionStream` holds a per-session byte buffer (`Uint8Array`,
ring-capped at 1 MiB, matching the disk scrollback cap). On a new
subscriber, the stream synthesizes one `bytes` event with the full
buffer's contents, so the subscriber's freshly-mounted xterm
renders the full historical view immediately, parsed by xterm's
own renderer at memory speed.

xterm instances are **not** pooled. Each `TerminalView` mount
creates and disposes its own xterm. Reasons:

- xterm is heavy: per-row × per-col cell metadata + 10000-line
  scrollback + WebGL textures. Pooling 31+ sessions' worth would
  consume hundreds of MiB.
- Browsers cap WebGL contexts at ~16 per page. Pooling more than a
  handful would force WebGL eviction and visible "context lost"
  warnings.
- The unit of pooling should be the *session-shaped* canonical
  state (bytes), not the *viewer-shaped* rendering of it
  (xterm).

### Always-on WebSocket

While a `SessionStream` exists, its WebSocket attachment to gmuxd
is held open, regardless of whether any subscriber is currently
mounted. Live bytes accumulate into the byte buffer in real time;
xterm subscribers come and go (on tab switches) and replay the
buffer instantly on each mount.

Consequences:

- **Tab switch is instant.** No network fetch, no replay buffer
  consumed, no TUI cols-±1 jiggle hack. The byte buffer the user
  switched away from is the byte buffer they switch back to, plus
  whatever streamed in while elsewhere.
- **Image protocol state is preserved.** Sixel / iTerm protocol
  bytes were rendered into the previous xterm in real time. A
  fresh xterm replaying the byte buffer parses the same escape
  sequences and renders the same images. The cols-±1 jiggle goes
  away.
- **Background sessions are observable.** A session whose runner
  is producing output while the user is on another tab streams
  bytes into its `SessionStream`'s buffer. The byte-count delta
  becomes a reliable signal for activity / unread indicators
  (see "Unread reworks downstream" below).

### Lazy create, dispose on session-remove

`SessionStreamRegistry.get(id)` instantiates a `SessionStream`
on first reference (typically when a `TerminalView` for that id
mounts) and caches it in a Map keyed by session id. The registry
subscribes to the store's session-remove events and calls
`stream.dispose()` when the session leaves the store, which closes
the WebSocket and frees the byte buffer.

No idle-eviction policy in v1. Memory at the v1 cap is bounded by
N_sessions × 1 MiB. For the intended scale (single user, single-
to-low-double-digit active sessions, single-digit "heavy"
hundreds), this is invisible memory pressure on a desktop browser.
Eviction policy can be added without API change if real usage
shows pressure.

No connection cap in v1. A heavy user with 100 sessions runs 100
WebSocket connections to a daemon on the same machine. Connection
count is the operational concern most likely to surface first; if
it does, the recovery path is a browser refresh (re-creates only
the streams the user actually re-references). Pool eviction with
LRU is a documented future option (the runner ring buffer's
replay-on-reconnect, designed for cold-attach, is what would make
that eviction recoverable).

### Resume separator: frontend-injected

When a `SessionStream` observes its session's `alive` flag flip
from `false` to `true` (a resume completed via ADR 0003's
mechanism), the stream emits a synthetic `bytes` event carrying a
formatted separator into its byte buffer:

```
\r\n\x1b[2m──── resumed at HH:MM:SS — <truncated command> ────\x1b[0m\r\n
```

The separator is **part of the byte buffer**: subsequent xterm
mounts (tab switches) replay it as part of the buffer's
contents. It is not persisted to disk: gmuxd's scrollback file is
written by the runner, which has no opinion on viewer-side seams.
A cold-load of the session via `/v1/sessions/<id>/scrollback`
after a daemon restart returns whatever the active runner has
written, with no separator artifacts in the on-disk record.

Resume semantics: the byte buffer is **preserved** across the
seam. The new runner's bytes append below the separator. The
xterm subscriber sees: previous-run-bytes, separator, new-run-
bytes, with normal xterm scrollback affordances over the whole
sequence. Per-kind variation (e.g., shell "Run again" → reset
buffer; agent "Resume" → preserve) is a defensible future
heuristic; v1 uses one policy uniformly because the alternate-
screen-buffer semantics most TUIs use already produce the right
visual effect (the old scrollback scrolls up off-screen, the
TUI repaints fresh, the user can scroll back to find history).

### Cold load of a dead session

A `SessionStream` instantiated for a dead session (no live runner)
with an empty byte buffer transitions through `bootstrapping`:
fetches `/v1/sessions/<id>/scrollback` once, populates the byte
buffer with the response, transitions to `frozen`. Subsequent
subscribers see the full buffer immediately as in any other case.

If the session is later resumed (alive flips), the stream
transitions through `reattaching` to `streaming`, emits the
synthetic separator, and opens the WebSocket. The cold-loaded
disk bytes precede the separator in the buffer.

### State machine

```
                ┌────────────────┐
                │ bootstrapping  │  initial state for any new stream
                └───────┬────────┘
                        │
         alive=true     │      alive=false
         (open WS)      │      (HTTP fetch /scrollback)
                ┌───────┴───────┐
                ▼               ▼
        ┌──────────────┐   ┌──────────┐
        │  streaming   │──▶│  frozen  │   alive: true → false
        │  (WS open)   │◀──│          │   alive: false → true
        └──────────────┘   └──────────┘   (via reattaching)
                ▲
                │ recover (auto)
        ┌───────┴────────┐
        │  reattaching   │   transient: WS opened, awaiting
        │                │   first frame after a resume
        └────────────────┘
```

`error` is reachable from any state on unrecoverable transport
failure (e.g., 401 on attach) and is observable by the action bar
to surface a "Reconnect" affordance.

### Unread reworks downstream

gmuxd currently emits `session-activity` SSE events when a runner
produces output while no clients are attached. Under always-on
WebSocket, every session always has a (frontend-side) subscriber,
and that signal goes silent.

The follow-up rework: unread state is computed in the frontend
from byte-count deltas. `SessionStream` exposes a
`bytesEmitted` count; the route layer records `lastViewedOffset`
when a session is the active route; unread = `bytesEmitted >
lastViewedOffset`. This is more accurate than the current
detach-based heuristic (a session whose runner emits output while
the user is on a sibling tab is unread regardless of whether
gmuxd's activity event fires) and is local to the frontend, with
no protocol churn.

This is documented as a follow-up, not part of the v1
`SessionStream` ship; the existing activity events continue to
fire while their original triggering condition (no detached
clients) still holds for sessions whose stream has not yet been
instantiated by any tab.

## Consequences

### Positive

- **Live→dead is invisible to the viewer.** The xterm and its
  contents persist across the alive flip; only the action bar
  changes.
- **Tab-switch is instant and faithful.** No network round-trip,
  no transport replay, image protocol state preserved.
- **Cols-±1 resize jiggle hack deletes.** Always-on WS makes the
  TUI repaint workaround unnecessary; bytes flowed in real time
  and the buffer reflects current TUI state.
- **Resume seam is representable.** The separator lives in the
  byte buffer alongside the bytes it separates and persists across
  tab switches as long as the byte buffer does.
- **Path divergence and merge bookkeeping shrink.** The
  `SessionStream` knows nothing about the runner's native id,
  scrollback file path, or merge state. Combined with ADR 0003,
  there is one path for one session's bytes, owned by one
  abstraction at each side of the WS.
- **Unread/notify accuracy improves.** Byte-offset deltas are a
  truer signal than detach-based activity events. Implementation
  is local to the frontend.
- **Foundation for future work.** `SessionStream` is the natural
  home for "skip-replay-up-to-offset" reconnect handshakes,
  per-tab read cursors for cross-tab unread synchronization, and
  speculative-attach for cross-host migration.

### Negative

- **Connection count grows linearly with session count.** Heavy
  users (~100 sessions) sustain ~100 WS connections. Acceptable
  on local IPC; if real usage shows pressure, LRU eviction is the
  documented escape. Browser refresh fully resets connection
  count to "currently-viewed sessions only."
- **Memory grows linearly with session count.** ~1 MiB per active
  session for the byte buffer cap. Tractable; comparable scale
  to today's xterm scrollback retention.
- **gmuxd's `session-activity` event becomes a degenerate
  signal.** Once every session has a stream, the "no detached
  clients" condition is rare; the event fires almost never. The
  event is not removed in v1 (back-compat with any consumer that
  hasn't migrated to byte-offset deltas) but loses operational
  meaning. Cleanup belongs to the unread-rework follow-up.
- **Reconnect after WS drop replays the runner ring buffer.**
  Without a "skip-replay-up-to-offset" handshake, transient WS
  disconnects produce a visible duplicate of the last 128 KiB.
  Acceptable for v1 (transient WS drops to a local daemon are
  rare); polishable later by extending the runner protocol with a
  cursor parameter.

### Breaking

None on the wire. Frontend-internal refactor; HTTP and SSE
surfaces are unchanged. Pre-existing dead sessions on disk
continue to cold-load via the existing `/scrollback` endpoint,
which is now consumed only by `SessionStream`'s bootstrapping
phase rather than by a standalone `ReplayView` component.

## Alternatives considered

### A. Status quo: `TerminalView` + `ReplayView`, switch on `alive`

Rejected as the source of the issues catalogued in Context. The
two-component shape destroys view identity on every alive flip
and forces two paths for the same bytes.

### B. Server-orchestrated WS: one transport, server holds session-byte state

Rejected. Architecturally appealing — eliminates the frontend
seam by moving it server-side; the WS endpoint serves the byte
stream regardless of runner alive/dead. Concretely, gmuxd would
hold the WS open across runner death, transparently switch the
source from runner socket to disk file, and forward new-runner
bytes after resume. Three reasons against:

1. **wsproxy becomes substantially stateful.** Today it is a dumb
   byte relay. Under model B it must hold post-death connections,
   replay scrollback to new attachments, switch sources on resume,
   and track per-attachment byte cursors for clean reconnect.
2. **Reconnect-on-net-blip needs cursors regardless.** The "WS
   drop produces 128 KiB duplicate" issue exists in B too; it is
   not eliminated by moving state server-side, only relocated.
3. **Multi-tab cost.** Per-attachment cursors in the daemon
   amplify state with tab count.

The frontend with a well-designed `SessionStream` achieves the
same correctness properties (one path for bytes, no seam-induced
divergence) at lower migration cost and without piling complexity
onto wsproxy. Revisit if cross-host session migration or strict
server-authoritative byte ordering becomes a hard requirement.

### C. Hybrid: transports as views over a single server-side byte stream

Rejected. Has B's wsproxy-stateful cost; also keeps an HTTP
endpoint as a debug surface; offers no advantage over B. If
server-authoritative ordering is the goal, B is the cleaner
shape; if frontend-side ordering is the goal, this ADR's decision
is the cleaner shape. The hybrid pays for both.

### D. Pool xterm instances instead of byte buffers

Rejected. WebGL context cap (~16/page) makes pooling more than a
small handful infeasible without visible "context lost" warnings;
memory cost is an order of magnitude higher than byte pooling
(per-row × per-col cell metadata + 10000-line scrollback + WebGL
textures vs. raw bytes). Pooling the wrong unit (the rendering)
rather than the right one (the canonical session state).

### E. Subscriber-bound WebSocket: open on subscribe, close on unsubscribe

Rejected. Each tab switch closes and reopens the WS; the runner
replays its 128 KiB ring buffer on each reattach; the byte buffer
either accepts a duplicate at the join (visible artifact) or
discards-and-replaces (loses the seam state). Cols-±1 jiggle
remains needed for image protocol state. Activity-timing remains
detach-based (no improvement). The savings (no idle WS
connections) are not worth the recurring re-attach cost on every
tab switch.

### F. `SessionStream` lives inside `store.ts`

Rejected. The store is a state container (sessions list,
projects, peers, computed views). `SessionStream` owns side
effects (WebSocket lifecycle, fetch cancellation, byte buffer).
Mixing imperative IO into the state container reintroduces the
effect-spaghetti the abstraction is meant to escape. Two modules
with one-way dataflow (registry observes store events) is testable
without a store fake; tests for the store don't mock WebSockets.

### G. `SessionStream` as a workspace package (`packages/session-stream/`)

Deferred. A standalone package is the right home if a second
consumer materializes (a `gmux --tail` CLI, a future native
client, etc.). For v1 there is one consumer (the web app), and
the abstraction is small. Promote to a package when a second
consumer surfaces; refactor cost is "move the file."

### H. Push API: `SessionStream` calls `term.write` directly

Rejected. Couples the stream to xterm; tests need a fake
terminal. The pull/subscribe API in the decision keeps the stream
ignorant of consumers — tests collect emitted events into arrays
and assert.

### I. Per-kind reset/preserve heuristic on resume

Deferred. `kind === 'shell'` → reset, agent kinds → preserve is a
defensible future evolution. v1 picks "preserve" uniformly
because (a) the alternate-screen-buffer behavior of TUIs already
gives the right visual effect for agent kinds, (b) shell users
who want a clean slate can scroll the previous output up, (c)
encoding the heuristic in `kind` couples the data model to a UX
policy that may evolve. Add a `resumeBehavior` adapter capability
or per-session preference if real usage shows the heuristic
mattering.

### J. Frontend stores xterm instances in a hidden DOM tree (display:none)

Rejected. Same WebGL-cap and memory-cost issues as D; also adds
a hack-shaped "the component is mounted but you can't see it"
state to the component tree, complicating Preact rendering and
focus management.
