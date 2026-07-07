# ACP conversation stream: runner-synthesized `session/update`

**Status:** Tracer (streaming assistant text only) · **Related:** ADR 0021, ADR 0004, ADR 0011, ADR 0016, `cli/gmux/internal/acp`, `cli/gmux/internal/ptyserver`

gmux adopts a minimal subset of the Agent Client Protocol (ACP) as its internal
normalized conversation schema (ADR 0021). The runner produces it; one frontend
client consumes it. For terminal pi the runner **synthesizes** the stream from
the read-only pi extension; a future ACP-native adapter re-publishes the same
shapes from a real agent. Everything above the runner is adapter-agnostic.

This document is the wire contract for the **first tracer slice: streaming
assistant text**. Later slices extend it (thinking, tool calls, prompt/cancel);
they add variants without breaking these shapes.

## Two channels

### 1. Extension → runner ingest (`POST /acp/ingest`, one-way)

The read-only pi extension (`agentext/pi-ext.mjs`) forwards token-level
assistant text over the same `GMUX_SESSION_SOCK` it uses for `/hook/event`.
Fire-and-forget, but — unlike `/hook/event` — **ordered**: token deltas must
arrive in sequence, so the extension serializes these POSTs through a promise
chain (independent fire-and-forget requests can complete out of order and
corrupt the reassembled text).

```jsonc
{ "op": "message_start", "messageId": "m1" }            // assistant message begins
{ "op": "chunk", "messageId": "m1", "delta": "Hel" }     // one token-level text delta
{ "op": "message_end", "messageId": "m1" }               // pi finalized it to JSONL
```

`messageId` is a per-turn monotonic counter minted by the extension (pi's
in-memory `AssistantMessage` has no id). It correlates the deltas of one
message. Sourced from pi's `message_start` / `message_update`
(`assistantMessageEvent.type === "text_delta"`) / `message_end` events; only
`text_delta` is forwarded this slice.

### 2. Runner → client stream (`/acp` WebSocket, snapshot-then-stream)

Mirrors the PTY attach (ADR 0004): on connect the server pushes the history
**snapshot** first, then streams **live** notifications. Frames are JSON-RPC
2.0 objects as WebSocket **text** messages. This slice has no client→server
messages (the write path is keystrokes via `/input`, per ADR 0021 §6).

```jsonc
// First frame — the history snapshot (server-initiated, not a client request).
{ "jsonrpc": "2.0", "method": "session/load",
  "params": { "sessionId": "...", "messages": [
    { "role": "user", "content": [{ "type": "text", "text": "hi" }] },
    // The in-flight assistant tail (if any) carries its streaming messageId so a
    // mid-turn joiner keeps appending live deltas to the same message.
    { "role": "assistant", "messageId": "m1", "content": [{ "type": "text", "text": "Hel" }] }
  ] } }

// Live frames — token deltas.
{ "jsonrpc": "2.0", "method": "session/update",
  "params": { "sessionId": "...", "update": {
    "sessionUpdate": "agent_message_chunk", "messageId": "m1",
    "content": { "type": "text", "text": "lo" } } } }
```

## Content ownership (ADR 0011 / 0016 intact)

- The **daemon holds no conversation content** — gmuxd is a transparent
  WebSocket proxy (`/acp/{sessionId}` → runner `/acp`).
- The **runner holds only the unwritten tail**: the in-flight partial assistant
  message and its accumulated text. It is dropped on `message_end`, once pi has
  persisted the message to its JSONL. This mirrors the PTY ring-buffer +
  scrollback split (ADR 0016).
- `session/load` = durable JSONL history **+** the in-memory tail. Live stream =
  tokens as they arrive.

## Backpressure

Tokens are chatty. The runner broadcasts per-token and **drops frames for a
subscriber whose buffer is full** rather than stalling ingest; the frontend
coalesces deltas into one render per burst.

## Known limits (this slice)

- Only `agent_message_chunk` (assistant text). Thinking, tool calls,
  `session/prompt`, `session/cancel` are later slices.
- **Stitch-across-flush window:** between `message_end` (tail forgotten) and pi
  actually appending to JSONL, a client connecting in that gap can miss the
  just-finished message from its snapshot. Full parity hardening is a later
  slice (ADR 0021 migration plan step 3).
- Peer (remote-session) proxying of the ACP stream is not implemented; gmuxd
  returns `501` for remote sessions.
