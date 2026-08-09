# Bounded semantic session stream

## Summary

Protocol 3 replaces each unbounded `snapshot.sessions` SSE event with a
transaction of complete semantic rows:

1. `snapshot.sessions.begin {version:3,epoch}`
2. zero or more `snapshot.sessions.batch {epoch,sessions:[...]}`
3. `snapshot.sessions.ready {epoch}`

Receivers stage batches privately and replace the visible projection only at
`ready`. This applies to initial hydration and the existing coalesced full-list
replacements after mutations. It deliberately does not select an archive or a
bounded live set; a future implementation can change which rows are enumerated
without changing framing.

## Bounds and failure behavior

- Maximum JSON payload per session-stream event: **48 KiB (49,152 bytes)**.
  This leaves 16 KiB below the default 64 KiB Scanner token for SSE syntax,
  envelope growth, and intermediary metadata.
- Rows are JSON-encoded independently and never byte-split.
- One row which cannot fit causes encoding to fail before `begin` is sent. The
  server emits a bounded `snapshot.sessions.error`, logs the row ID, and closes
  that response. The receiver discards staging and retains the previous ready
  set. Likely unbounded fields are `command`, `cwd`, `remotes`, `title`,
  `subtitle`, `socket_path`, and `conversation_file`. Session rows contain no
  scrollback or transcript content.
- Receiver staging is capped at 100,000 rows and 64 MiB.
- The Go SSE Scanner line ceiling is bounded at 1 MiB, up from 256 KiB. This
  admits the measured 1,000-row protocol-2 frame during mixed-version operation.
  Protocol 3 does not rely on that increase. Lines above it return a protocol
  error before unbounded growth.

## Consistency boundary

`sseFanout.Subscribe` takes the same mutex as `BroadcastFrames`, installs the
subscriber, and copies the current full projection before releasing it. A
mutation therefore falls on exactly one side of the boundary:

- before capture: reflected by baseline epoch N; or
- after capture: queued as a later full replacement epoch N+1.

The HTTP handler serially writes all events for one epoch, including `ready`,
before reading the next fanout message. A concurrent mutation cannot overtake
baseline readiness. The bounded fanout may drop intermediate full replacements
for a slow subscriber, but retains a later authoritative replacement, matching
ADR 0001's existing latest-only semantics. Activity remains intentionally
lossy.

Staging belongs to one transport connection in the Go peer. Browser staging is
cleared on EventSource error and reset by every begin marker. Disconnect before
ready therefore cannot expose or carry partial rows into reconnect.

## Compatibility

Browser assets are daemon-served/version-locked and always receive protocol 3.
New peer clients request `?as=peer&session_stream=3`.

- New hub -> old spoke: the old daemon ignores the query parameter and sends
  legacy `snapshot.sessions`; the new hub still handles it authoritatively.
- Old hub -> new spoke: absence of `session_stream=3` selects the legacy event.
- New hub -> new spoke: bounded begin/batch/ready events.
- Unknown requested versions select legacy fallback rather than guessing.

Thus mixed peers do not silently ignore/misparse new event names. The legacy
fallback remains intrinsically unbounded and can still exceed an old peer's
Scanner limit for very large sets; retaining wire compatibility and fixing an
old binary's parser cannot both be guaranteed by the new spoke.

## Oversized-path inspection

`snapshot.sessions` and `snapshot.world` are distinct frames. The actual large
row projection is `snapshot.sessions`; peer subscriptions suppress world
entirely. `snapshot.world` carries project membership IDs but no session rows.
With 1,000 eight-character IDs in one project its encoded payload measured
**11,110 bytes**, below the session event budget. World framing was therefore
left unchanged.

## Reproduction and measurements

Before code changes, a realistic 1,000-row projection (commands, cwd/workspace,
remotes, title/subtitle, socket path, conversation reference, runner/project
metadata; no transcripts) produced:

```
legacy payload=860535 frame=860568 max_line=860541 events=1
lines=1 err=bufio.Scanner: token too long
```

The checked-in deterministic Go fixture (slightly narrower but still realistic)
measures old versus protocol 3:

| metric | old full event | protocol 3 |
|---|---:|---:|
| rows | 1,000 | 1,000 |
| maximum JSON event payload | 687,678 B | 48,889 B |
| total SSE bytes | 687,711 B | 688,721 B |
| event count | 1 | 17 |
| serialization latency (median of 5 benchmark runs) | ~1.47 ms | ~2.71 ms |

Total wire overhead is 1,010 bytes (~0.15%). Serialization adds ~1.24 ms on the
fixture. On a local/in-memory initial synchronization this is the measured
latency delta; real links are dominated by transferring the same ~689 KiB, while
the receiver intentionally exposes the set at the final ready marker rather
than incrementally. Benchmark command:

```
go test -run '^$' -bench BenchmarkInitialSync1000 -benchmem -count=5 ./internal/sessionstream
```

## Evidence map

- Legacy >64 KiB reproduction, empty/one/many rows, event bounds,
  oversized-single-row failure, byte/event measurements:
  `internal/sessionstream/sessionstream_test.go`
- Scanner compatibility acceptance and hard ceiling:
  `internal/sseclient/client_test.go`
- Browser disconnect/reconnect and mutation staging:
  `apps/gmux-web/src/sse-reconnect.test.ts`
- Peer disconnect, reconnect, atomic readiness, mutation epoch, legacy spoke:
  `internal/peering/session_stream_test.go`
- Subscribe/publication boundary, browser/peer version selection, world size:
  `cmd/gmuxd/session_stream_boundary_test.go`
- Real server event order and matched world:
  `cmd/gmuxd/serve_switch_integration_test.go`

## Open tradeoffs

- Full session sets are still re-enumerated after mutations. This PR bounds
  transport frames and atomic application; it does not implement deltas,
  archive/live-set selection, resumable epochs, or server-side replay.
- A single semantic row over 48 KiB is rejected rather than split. A future
  protocol could model specific large fields semantically, but arbitrary-byte
  checkpointing is intentionally avoided.
- `snapshot.world` remains one event. The measured many-session path is small;
  independently unbounded project/peer metadata may warrant its own semantic
  framing if real deployments approach the Scanner ceiling.
- Browser EventSource provides no custom negotiation headers; this is safe only
  because the browser bundle and daemon are served/versioned together.
