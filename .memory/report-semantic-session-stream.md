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
other row. This handles the confirmed old-spoke amplification schedule: a new
hub accepts a legacy row up to its 1 MiB compatibility ceiling, then quarantines
that row rather than bricking its browser or downstream peer projection.

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
receivers. Duplicate/older begin events are ignored without destroying a newer
in-flight transaction; stale batch/ready cannot publish. Disconnect releases
staging and resets epoch history, allowing the next transport to restart at 1.
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

World remains a single semantic object, separate from session rows. It now has
an explicit **512 KiB JSON sender maximum**, below the Go transport's 1 MiB
ceiling. Oversize/encode failure emits a small `snapshot.world.error`; no
unbounded world line is written. Browser updates retain the previous world; an
initial error exposes safe empty defaults so session availability is preserved.

The realistic transport-seam fixture includes 1,000 memberships across 50
projects, path/remote match rules, 20 peers, health, launchers, peer projects,
and peer discovery. It measures **26,177 bytes**, about 5% of the explicit
maximum. The earlier synthetic ID-only fixture was rejected as insufficient.

I did not semantically fragment world: at gmux's documented supported scale
(single user, dozens of sessions/peers; ADR 0001 explicitly says the snapshot
model is unsuitable for thousands), the composed stress fixture is 20× below
the sender maximum. Arbitrarily large operator metadata is now rejected before
the transport rather than producing an unbounded SSE line. Claims are narrowed:
48 KiB applies to protocol-3 session events; world has its separate 512 KiB
maximum; transitional protocol-2 sessions retain the 1 MiB client ceiling.

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
- peer reconnect, diagnostics-through-ready, overflow recovery, monotonic epoch,
  old-spoke-large-row amplification:
  `internal/peering/session_stream_test.go`
- browser explicit negotiation, legacy fallback, real disconnect release,
  monotonic epoch, degraded ready:
  `apps/gmux-web/src/sse-reconnect.test.ts`
- subscription boundary, compatibility matrix, realistic world transport bound,
  oversized-world diagnostic:
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
- World is bounded rejection rather than fragmentation. If supported deployment
  scale grows near 512 KiB, it should gain its own semantic row transaction.
