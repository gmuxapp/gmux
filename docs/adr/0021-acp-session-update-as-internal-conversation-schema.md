# ADR 0021: ACP `session/update` as gmux's internal normalized conversation schema

**Status:** Proposed
**Date:** 2026-07-06
**Related:** ADR 0004 (SessionStream, live↔dead view), ADR 0009 (verb-first CLI, universal per-adapter verbs), ADR 0011 (runner-owned state; daemon is a read cache; agent-hook), ADR 0015 (hook translation at the agent side), ADR 0016 (retention/scrollback as cache)

## Context & Problem

gmux renders and drives agent conversations. Two forces now converge on the same design:

1. **Streaming UI.** The frontend wants a live, structured conversation view — assistant text as it streams, thinking, tool calls and their results — not the PTY byte stream with terminal chrome. Getting that requires extending the pi extension to forward more than today's turn-level hook (ADR 0011 reports only session identity, title, and turn start/end).
2. **ACP-native adapters.** We anticipate adapters that are **ACP-only and terminal-less** (an agent that speaks the Agent Client Protocol natively, with no PTY). gmux would be an ACP *client* to those.

Left unconnected, these produce two representations of "a conversation": a bespoke gmux event shape for terminal pi, and ACP for the native adapters — two schemas, two renderers, two sets of types. gmux is **not** interested in being *driven* by foreign ACP clients (Zed/acpx); the value here is **internal uniformity**, not external interop.

The open architectural risk is content ownership. ADR 0011 deliberately moved the daemon **out** of holding conversation content: the daemon `store.Store` is a read cache fed by runner `/events`; it does not parse files or hold message bodies. Streaming deltas *are* content, so a naive design would reverse 0011 and make the daemon a content store again.

## Decision

Adopt **ACP's `session/update` notification family and content-block types as gmux's internal normalized conversation schema**, produced by the runner and consumed by one UI client — and keep the daemon content-free.

### 1. The runner exposes an ACP-subset endpoint; the daemon can't tell real from translated

Each session's runner serves a small ACP surface on its existing per-session socket, alongside `/meta`, `/input`, `/events`, and the PTY WebSocket. Behind that endpoint the source is adapter-internal:

- **Terminal pi:** the runner *synthesizes* `session/update` notifications from the (extended) read-only pi extension.
- **ACP-native adapter:** the runner is a real ACP client to the agent process (JSON-RPC over stdio) and *re-publishes* the same notifications.

From the daemon's and frontend's point of view there is **one ACP-shaped stream per session**; whether a genuine ACP agent or gmux's translation sits behind it is invisible. This is the ADR 0011 principle applied to conversation content: the adapter owns translation in the runner; the daemon stays generic.

### 2. Share the *schema*, not necessarily the *transport*

- **JSON-RPC-over-stdio ACP** exists only at the one real boundary: runner ↔ an ACP-native agent process.
- **Everywhere else** — the runner's session endpoint and the frontend — carries ACP-shaped *objects*. The frontend is one ACP client with one renderer, regardless of adapter.

### 3. Streaming transport mirrors the PTY WebSocket (ADR 0004)

The conversation stream reuses the live↔dead shape already proven for PTYs. A client connects to the runner's ACP endpoint and:

1. calls **`session/load`** → receives the conversation **history snapshot** (analogous to `renderScreen`'s scrollback+screen for the PTY), then
2. receives live **`session/update`** notifications (analogous to live PTY bytes).

Late-join, reconnect, and the dead-session read-only view work exactly as ADR 0004 describes for terminals: one view abstraction, one snapshot-then-stream path, no separate live/dead components. Framing is **ACP JSON-RPC 2.0 over WebSocket** — the ACP analog of the existing `nhooyr.io/websocket` PTY attach — so both of the runner's streams (bytes and conversation) share transport machinery and the peering/hub proxy path.

### 4. Content ownership is unchanged: the daemon holds none; the runner holds only the unwritten tail

Per ADR 0011 the daemon stays a content-free read cache (summary state only: status, title, attribution). The **durable** conversation is pi's JSONL file, which the runner already owns and attributes.

The runner holds **in memory only the streaming output not yet flushed to JSONL** — the in-flight partial assistant message and its tokens. Once pi appends a finalized message to the JSONL, the runner **forgets** the in-memory copy. So:

- `session/load` history snapshot = **JSONL** (durable, complete) **+** the in-memory unwritten tail (the current partial message).
- live `session/update` stream = tokens as they arrive.

This is the exact analog of the PTY ring buffer + persistent scrollback (ADR 0016): a small live buffer for what hasn't been persisted, disk for the rest. No new durable content store, no reversal of ADR 0011.

### 5. Deltas are token-level

`session/update` carries **token-by-token** `agent_message_chunk` / `agent_thought_chunk`, sourced from the extension's `message_update.assistantMessageEvent`. Tool activity streams via `tool_call` / `tool_call_update`.

### 6. The write path is keystrokes; the extension is read-only

Consistent with ADR 0009's universal, raw `send`/`send-keys`, inbound requests are translated by the runner into the **existing keystroke input path** (`/input`), not a second actuator:

- **`session/prompt`** → `send "<text>" Enter` (equivalently `gmux send <ref> "<text>" Enter`; `send --follow-up` for the queued variant).
- **`session/cancel`** → `send Esc`.
- **model / mode changes** (later) → `send "/model <name>" Enter`.

Consequently the pi extension is a pure **observer/producer** of `session/update`, and the **extension→runner channel stays one-way** (higher-volume token deltas, but no duplex). This removes the duplex/back-channel design entirely and keeps the one destructive write path universal across adapters.

### 7. Committed method subset

| Piece | Verdict | Notes |
|---|---|---|
| Content blocks (text / image / resource) | **Adopt (schema)** | Message representation the UI renders |
| `session/update`: `agent_message_chunk`, `agent_thought_chunk`, `tool_call`, `tool_call_update` | **Adopt — core** | Token-level; the live feed |
| `session/prompt` | **Adopt** | Translated to keystrokes (§6) |
| `session/cancel` | **Adopt** | Translated to `Esc` |
| `session/load` / resume | **Adopt** | History snapshot then stream (§3) |
| `initialize` + capabilities | **Runner-internal** | Static/synthetic for pi; real for ACP-native; not a user surface |
| `session/new`, teardown | **Runner-internal** | Driven by gmux `open`/`kill`, not exposed as ACP surface |
| `auth/*` | **Pass-through, capability-gated** | Only ACP-native agents that need it |
| `session/request_permission` | **Drop** | pi does not gate; permissions out of scope |
| `session/set_mode`, `set_config_option` | **Defer** | Model/thinking selection; later, via keystroke `/model` |
| `session/list`, `delete` | **Drop as surface** | gmux ref system (`ls`/`kill`) owns lifecycle |
| `plan` | **Optional** | Render if an adapter emits it; pi has none |

### 8. CLI relation

The lightweight verbs are the CLI view of the same schema, not a second implementation:

- **`tail --clean` / `--json`** emits the `session/update` stream (ADR-pending B-lite tail).
- **`send --follow-up`** is a `session/prompt`.

## Consequences

- One conversation schema, one frontend ACP client, one renderer across terminal pi and future ACP-native adapters. The frontend can use an off-the-shelf ACP-aware UI (e.g. `assistant-ui`, proven clean in `mgabor3141/hakanai`).
- The daemon stays content-free (ADR 0011 intact); the only new state is a bounded per-runner in-memory unwritten-tail buffer, mirroring the PTY ring buffer.
- The extension stays read-only and its channel one-way — no duplex back-channel to design or secure.
- The "GUI view" is just another surface over one live session (like the phone), with **no pi mode switch and no restart**: it consumes the same `session/load`+`session/update` stream while the terminal remains attached. Multi-surface concurrency is preserved.
- We gain a clean seam for ACP-native adapters later: they slot in behind the same runner endpoint with a real JSON-RPC client, and nothing above the runner changes.
- Token-level deltas raise stream volume vs. the current turn-level hook; the transport must tolerate it (WebSocket backpressure) and the frontend coalesces for rendering.

## Alternatives considered

- **Bespoke gmux conversation events + ACP only for ACP-native adapters.** Two schemas, two renderers; rejected once ACP-native adapters make the ACP schema unavoidable in the codebase anyway.
- **Full ACP JSON-RPC server on the runner (spec-complete, for foreign clients).** Over-built: we don't want to be driven by Zed/acpx. We adopt the schema and a subset, not a conformant external server.
- **Daemon holds streaming content.** Reverses ADR 0011; makes the daemon a content store again with two writers. Rejected — the runner's unwritten-tail buffer + JSONL covers `session/load` without it.
- **Duplex extension channel with `sendUserMessage`/`abort` actuators.** More fidelity (in-process steer/follow-up), but forks the write path away from universal keystrokes (against ADR 0009) and requires a back-channel. Rejected in favor of keystroke translation; the extension stays a pure observer.
- **`pi --mode rpc` (pi-acp) as a second pi mode.** Headless-only, so restart-to-switch, no live dual-view, breaks universal verbs (`attach`/`send`) for those sessions. Kept only as a possible future *headless* session type, not the path to a GUI view.

## Migration / tracer plan

1. **This ADR** settles the schema, transport, and content ownership.
2. **Vertical tracer (single author):** terminal pi → extended read-only extension emits the four core `session/update` variants (token-level) → one-way channel → runner synthesizes ACP endpoint → `session/load`+`session/update` over WebSocket → frontend renders streaming assistant text. Prove schema + transport end-to-end on the narrowest slice. **Landed** across three slices: assistant text (`agent_message_chunk`), thinking (`agent_thought_chunk`), and tool calls (`tool_call` / `tool_call_update`, sourced from pi's `tool_execution_start` / `tool_execution_end` events and correlated from JSONL `toolCall` / `toolResult` records for the snapshot). See `docs/acp-conversation-stream.md`.
3. **Fan out once proven:** `tail --clean/--json`, `send --follow-up`, dead/reconnect parity with ADR 0004, and the first real ACP-native adapter (validates the schema against a genuine ACP producer rather than only the pi translation).
