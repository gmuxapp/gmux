# Runner hook protocol: authoritative agent activity

**Status:** Stable · **Related:** ADR 0011, ADR 0015, ADR 0029, ADR 0030

The runner exposes an owner-only Unix socket in `GMUX_SESSION_SOCK`. An agent
adapter posts fire-and-forget JSON to `POST /hook/event`. Events are translated
at the adapter's typed API boundary; the runner never parses adapter-native
conversation files or events.

## Events

```jsonc
{ "op": "ready" }

{ "op": "session", "path": "/opaque/conversation/ref", "id": "native-id",
  "slug": "title", "name": "title", "cwd": "/work",
  "reason": "startup|new|resume|fork|activity" }

{ "op": "turn", "phase": "start", "turn_seq": 7,
  "trigger": "verbatim user text up to the source cap",
  "source_bytes": 42, "previous_exchanges": 3 }
{ "op": "turn", "phase": "iteration", "turn_seq": 7 }
{ "op": "turn", "phase": "steered", "turn_seq": 7,
  "text": "verbatim additional user instruction", "source_bytes": 39 }
{ "op": "turn", "phase": "end", "turn_seq": 7,
  "outcome": "completed|interrupted|error",
  "output": "terminal or terminal-partial assistant prose",
  "truncated": false, "diagnostic": "short failure reason", "title": "title" }
```

`turn_seq` is source-asserted and monotonic within one extension incarnation.
For pi, the logical activity opens on the first `agent_start` and closes only on
`agent_settled`; retried `agent_end` events do not close it. Every completed
assistant `message_end` is an iteration, including tool-use and recovered
attempts. A user message entering the running loop is a `steered` event and a
visible exchange boundary. It does **not** settle the activity or resolve a
wait.

`output` is prose from the activity's latest assistant response. For a
non-completed outcome it is partial and consumers must label it as such. An
absent output means there was no terminal prose. It is never reconstructed as a
completed result by the runner or daemon.

## Turn frame

The runner retains one generation-local frame and replays it to `/events`:

```jsonc
{
  "seq": 12,
  "current": {
    "turn_seq": 7,
    "previous_exchanges": 3,
    "exchanges": [
      { "ordinal": 1, "user": "first request", "source_bytes": 13, "iterations": 3 },
      { "ordinal": 2, "user": "follow-up", "source_bytes": 9, "iterations": 1 }
    ],
    "omitted_exchanges": 0,
    "omitted_bytes": 0
  },
  "last": {
    "turn_seq": 6,
    "outcome": "completed",
    "previous_exchanges": 2,
    "exchanges": [{ "ordinal": 1, "user": "request", "source_bytes": 7, "iterations": 4 }],
    "output": "answer"
  }
}
```

A turn start or close is transported atomically with its status edge under the
`turn_frame` key. Mid-activity frame changes use `event: turn_frame`. Replay
uses the same coupled shape, so a consumer observes an edge with its frame or
neither. A conversation rebind clears current, last, and conversation-local outcome
metadata before publishing the new ref.

The frame is bounded and never persisted. pi caps terminal prose at 256 KiB and
each live user boundary at 8 KiB, both at a UTF-8 boundary and with an ellipsis
when cut. The runner retains the newest 65 user boundaries (anchor plus 64
additional instructions) and carries exact omitted exchange and byte counts.
The daemon runner-SSE scanner accepts 8 MiB lines; the hook endpoint accepts
4 MiB events. A display overflow can degrade text only: it never drops the
source close, outcome, `turn_seq`, or terminal output. Historical `agent logs`
reads full native content rather than this frame.

`source_bytes` is the original UTF-8 byte length before capping;
`previous_exchanges` is the native branch count before the activity; and
`ordinal` is monotonic within the activity even when old boundaries are evicted.
Together these permit exact omission accounting, persistence reconciliation,
and presentation-only abbreviation without text ownership claims.

No delivery IDs, text normalization, pending correlation, or ownership claims
exist on this wire. Additional user-boundary events are report material only.

## Readiness and delivery

`ready` means the agent's composer and semantic input handlers are installed.
`POST /prompt` and `POST /cancel` wait for it and require
`X-Gmux-Expect-Incarnation`; a mismatch is refused before any bytes are written.
Prompt delivery uses one ordered PTY write and a reservation released only by a
fresh inactive-to-active source edge. Raw `/input` remains unconditional and
outside these guarantees.

## Outcome vocabulary

| Outcome | Meaning |
|---|---|
| `completed` | Agent settled normally. |
| `interrupted` | Work was intentionally stopped. |
| `error` | Agent could not settle successfully. |

A late duplicate end cannot overwrite a closed outcome. Process exit after a
source close likewise cannot overwrite completed/interrupted/error. Loss while
an activity is open is projected by the daemon as an activity failure; semantic
reports never expose runner residency vocabulary.
