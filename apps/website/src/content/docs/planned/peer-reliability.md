---
title: Peer Reliability
description: Keepalives, silent-drop detection, and a unified session identity model.
---

> This feature is not yet implemented.

Remote sessions today work through a single hub-and-spoke protocol that goes through `sseclient` and `apiclient` (see [Multi-Machine](/multi-machine) and [Architecture](/architecture#shared-client-packages)). That refactor gave us one code path per protocol primitive, which is where two reliability improvements now want to live.

## Keepalives and silent-drop detection

Long-lived connections between gmuxd instances can die silently when a NAT rebinds, a Tailscale tunnel hiccups, or a mobile device suspends. The TCP socket stays open from both sides, but no bytes flow, and neither side notices until the next write fails (which on an idle SSE stream may never come).

The plan is to add two cheap mechanisms on top of the shared client packages:

### Server-sent SSE heartbeats

The spoke's SSE handler emits a `:\n\n` comment line every 30 seconds when the event queue is idle. Comment lines are part of the SSE spec, they produce no client-side events, and they work through any HTTP proxy that preserves chunked transfer-encoding. Ten lines of code on the spoke, already supported by `sseclient`.

### Ping ticker on the proxy WebSocket

`apiclient.ProxyWS` runs a 30-second ping ticker on the spoke connection and closes with a distinct error if no pong comes back within 60 seconds. The peering outer loop treats that close like any other disconnect, triggering the existing exponential-backoff reconnect. The browser side already relies on OS TCP keepalive, so nothing changes there.

### Read deadlines

`sseclient` sets a sliding read deadline (refreshed on every event or comment line) so an idle connection trips after the configured window even if the TCP socket never surfaces an error. Same mechanism, different layer, catches the case where the server stops sending heartbeats entirely.

### Tuning

Initial values: 30 s ping, 60 s idle deadline. These numbers are conservative starting points and benefit from real-tailnet testing before being locked in. Landing the keepalive work separately from the [peer refactor](/multi-machine) lets the numbers be tuned without reopening the refactor diff.

## Unified session identity

Session identity today uses several parallel concepts:

| origin | ID shape | stored where | notes |
|--------|----------|--------------|-------|
| live runner | UUID from runner | `store.sessions[id]` | |
| dead session from file | `file-<first8>` | `store.sessions[id]` | prefix avoids clash with live |
| remote | `<id>@<peer>` | `store.sessions[id]` | namespacing applied by hub |
| resumable routing | `ResumeKey` (slug) | `store.Session.ResumeKey` | frontend URL stability |

Plus three lifecycle flags that should really be one:

- `Session.Alive` (bool)
- `Session.Resumable` (bool, derived from `!Alive && len(Command) > 0`)
- `Store.dismissed` side-map

The dismissed side-map in particular exists because the scanner and the store disagree on whether a dismissed-then-removed session should come back. It's a patch, not a model.

### Target model

```go
type Session struct {
    OriginKey string       // immutable, the full UUID or file-backed ID
    Owner     string       // "" for local, peer name otherwise
    State     SessionState // alive | resumable | dismissed
    // ...other metadata...
}

type SessionState int

const (
    StateAlive     SessionState = iota // runner is up
    StateResumable                     // dead, but can be resumed
    StateDismissed                     // user dismissed, hidden
)

func (s Session) WireID() string {
    if s.Owner == "" {
        return s.OriginKey
    }
    return s.OriginKey + "@" + s.Owner
}
```

What this changes:

1. **The `file-` prefix disappears.** The scanner emits the full origin key directly. A one-time migration reads existing `file-<first8>` entries and restores the full UUID from the session file they point at.
2. **`Store.dismissed` disappears.** Dismissal sets `State = StateDismissed` and the store keeps the row; the scanner's "is this already known?" check naturally excludes it because the key is there. Dismiss emits `session-upsert` with the new state, the frontend filters `state === 'dismissed'` from the sidebar (with a "Show dismissed" toggle in settings).
3. **`Alive` and `Resumable` become derived methods** on `Session` for wire-format backward compatibility. They read from `State` instead of being independently stored. No new storage, just a projection.
4. **`peering.NamespaceID` / `ParseID` become thin helpers** around `Session.WireID` / `ParseWireID`. Nested peer chains (`sess-xyz@proj@server`) still work because `Owner` can include nested `@` and the split happens on the last one.
5. **The projects manager keys membership by `(OriginKey, Owner)`** instead of the wire ID string. This is the piece that actually fixes the "same dead session, different representation" bugs that motivated the current state sprawl.

### Why this eliminates the dismissed side-map

With dismissal-as-state, the store never forgets a dismissed session. The scanner sees it in the store, skips it. If the user clicks resume, the state flips back to `StateAlive` via the normal resume path. A subsequent exit takes it to `StateResumable`, not back to dismissed. No scanner special cases, no `Upsert` side effects, no patches.

## Scope and sequencing

The keepalive work and the identity rework are independent and can land in either order. Keepalives are small, mechanical, and benefit from landing first because they improve reliability on the same day they merge. The identity rework is larger, touches the scanner and the frontend, and supersedes the existing dismissed-session fix. They should not be bundled into one PR.

Explicitly out of scope:

- **Reconnect-UUID buffer pattern** (Coder-style). The runner already replays full scrollback on each WebSocket attach, which is the moral equivalent. Revisit only if browser flicker on reconnect becomes a user-visible issue.
- **Single-WebSocket protocol** that multiplexes state and terminal data. Mature terminal tools (ttyd, gotty, wetty, Coder) don't do this, and the duplication that motivated the refactor lived on the peer side, not the browser side.
- **Centrifuge or other real-time messaging libraries.** Wrong abstraction layer, large dependency, solves problems we don't have.
- **Hub-side caching of shared spoke WebSocket connections.** One WebSocket per attach stays. The resource cost is negligible for a self-hosted single-user app.
