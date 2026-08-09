# Bounded semantic session stream — amended after review

## Protocol

A protocol-3 session replacement is one transaction:

1. `snapshot.sessions.begin {version:3,epoch}`
2. zero or more `snapshot.sessions.batch {epoch,sessions:[...]}` events
3. zero or more non-fatal `snapshot.sessions.error` row diagnostics
4. `snapshot.sessions.ready {epoch}`

Batches contain complete semantic rows and have a 48 KiB JSON payload maximum.
Receivers stage privately and replace the visible set only at the matching
`ready`. Initial hydration and later coalesced full replacements use the same
framing. This does not implement archive/live-set selection.

## Availability and bounds

A row larger than 48 KiB no longer aborts or closes the stream. The sender omits
only that row, emits a bounded diagnostic with a fixed reason and safe identity
(IDs over 128 bytes become `sha256:<digest>`), and completes `ready` for every
other row. The browser publishes a persistent exact omitted-session total with
at most 256 safe ID/reason details; the sender's counted summary cannot consume
or be dropped by the detail cap, and a later clean bootstrap clears both. Peers retain each bounded
diagnostic for the transaction and log it. This handles the confirmed old-spoke
amplification schedule: a new hub accepts a legacy row up to its 1 MiB
compatibility ceiling, then quarantines that row rather than bricking its
browser or downstream peer projection.

Likely row-size causes are `command`, `cwd`, `remotes`, `title`, `subtitle`,
`socket_path`, and `conversation_file`. Rows contain no scrollback/transcript.
They are never truncated or arbitrary-byte-split.

Sender and receiver use symmetric transaction limits:

- 100,000 staged rows
- 64 MiB receiver encoded staging
- sender accepts at most 63 MiB of row JSON, reserving 1 MiB for batch envelopes
- at most 256 individual diagnostics, followed by one bounded summary
- Go SSE line/event accumulation ceiling: 1 MiB for transitional protocol 2

A malformed/overflow transaction is abandoned without changing visible state.
A strictly newer begin starts cleanly, so rejection does not create permanent
staleness. Correct senders remain inside receiver bounds.

## Epoch and consistency schedules

`fanout.Subscribe` installs the subscriber and captures its baseline under the
same mutex as publication. A mutation is either included in baseline epoch N or
queued as a later full replacement. The handler serializes all of N through
`ready` before reading N+1.

Epochs must increase strictly within one transport in both browser and peer
receivers. The first accepted session event also locks the transport to legacy
or protocol 3, so a legacy injection cannot reset v3 replay protection and v3
cannot take over a legacy-only connection. Duplicate/older begin events are
ignored without destroying a newer in-flight transaction; stale batch/ready
cannot publish. Disconnect releases staging and resets mode/epoch history,
allowing the next transport to restart at 1.
Browser tests prove release by sending batch+ready without a new begin after an
error; the partial rows do not publish.

The complete session transaction has one 10-second write deadline. Cancellation
is checked between fragment writes. The deadline is cleared after completion,
so timeout no longer multiplies by event count.

## Compatibility

Protocol 3 is explicit for all consumers:

- current browser: `/v1/events?session_stream=3`
- current peer: `/v1/events?as=peer&session_stream=3`
- unversioned browser tab/custom consumer: transitional protocol-2
  `snapshot.sessions`
- old peer omitting the marker: protocol 2
- new hub to old spoke: old spoke ignores the marker; new hub accepts protocol 2
- unknown requested version: legacy fallback rather than guessing

The new browser also retains a legacy listener defensively. An old tab opened
across a daemon upgrade requests no marker and therefore continues receiving the
event it understands rather than silently freezing.

## SSE parser

The Go SSE client now follows event boundaries: all `data:` fields are
accumulated, joined with `\n`, and dispatched once at the blank line. Field order
is unconstrained, no-event blocks use type `message`, comments do not terminate
an event, and empty/no-data blocks do not dispatch. Both individual lines and
the accumulated event remain bounded.

## `snapshot.world` scope

World remains the pre-existing single event, separate from session rows. This PR
adds no world cap/error behavior and makes no endpoint-wide boundedness claim:
48 KiB applies only to protocol-3 session events. This preserves protocol-2 old
tabs and deep-link hydration exactly. The realistic shape fixture (1,000
memberships across 50 projects, match rules, 20 peers, health, launchers, peer
projects, and discovery) remains only a size characterization (**26,177
bytes**), not a supported maximum. World semantic framing is a separate future
design if production evidence demonstrates it.

## Reproduction and measurements

Pre-change realistic fixture:

```
legacy payload=860535 frame=860568 max_line=860541 events=1
lines=1 err=bufio.Scanner: token too long
```

Checked-in 1,000-row fixture:

| metric | old | protocol 3 |
|---|---:|---:|
| max session JSON event | 687,678 B | 48,889 B |
| total session SSE bytes | 687,711 B | 688,721 B |
| session event count | 1 | 17 |
| median serialization, 3 runs | ~1.30 ms | ~1.77 ms |

Wire overhead is 1,010 bytes (~0.15%). The measured local serialization delta is
~0.47 ms. Quarantine adds one bounded diagnostic per bad row (subject to the
diagnostic cap) and does not affect healthy fixtures.

## Evidence map

- bounds, empty/one/many, legacy Scanner reproduction, quarantine, safe IDs,
  sender/receiver limit symmetry, measurements:
  `internal/sessionstream/sessionstream_test.go`
- standard multiline SSE parsing and line/aggregate limits:
  `internal/sseclient/client_test.go`
- peer reconnect, retained diagnostics-through-ready, overflow recovery,
  monotonic epoch, mixed-mode rollback, old-spoke-large-row amplification:
  `internal/peering/session_stream_test.go`
- browser explicit negotiation, legacy-only fallback, protocol-mode lock, real
  disconnect release, monotonic epoch, persistent degraded-ready warning:
  `apps/gmux-web/src/sse-reconnect.test.ts`
- subscription boundary, compatibility matrix, realistic world-size
  characterization:
  `cmd/gmuxd/session_stream_boundary_test.go`
- transaction-wide deadline/cancellation:
  `cmd/gmuxd/sse_transaction_test.go`
- production initial event contract:
  `cmd/gmuxd/production_container_e2e_test.go`

## Open tradeoffs

- Every mutation still re-enumerates the full selected set; no delta/replay or
  archive/live-set policy is introduced.
- Quarantined rows are absent from the SSE projection until they shrink. They
  remain inspectable/actionable through one-shot CLI/REST paths.
- Transitional protocol-2 session frames are still single events for one
  release; the new client bounds them at 1 MiB.
- World framing and limits are deliberately unchanged and require a separate
  design if real-world evidence demonstrates an oversized world path.
