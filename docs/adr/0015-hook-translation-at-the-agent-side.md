# ADR 0015: Tool-event translation lives at the agent-side hook, not the runner

**Status:** Accepted
**Date:** 2026-06-23
**Related:** ADR 0011 (runner-owned session state), ADR 0013 (codex hooks),
`docs/runner-hook-protocol.md`

## Context

A recurring question when extending or debugging the hook path: should the
agent-side hook/extension be a **dumb forwarder** that posts the tool's *raw*
native events, leaving the gmux adapter (in the runner) to parse them into gmux
state? On its face that sounds like the cleaner "all tool-specific knowledge in
the adapter" split, and it reads as the natural end of ADR 0011's "adapter logic
in one place."

It is the wrong split, and this ADR records why so the question doesn't get
relitigated.

The three "hooks" are not the same kind of thing:

- **pi** — `agentext/pi-ext.mjs`, **JavaScript**, runs *inside* the pi process
  with first-class typed access to pi's API (`pi.on("agent_end")`,
  `ev.messages`, `ctx.sessionManager.getSessionName()`). It reaches the runner
  as JSON over a unix socket.
- **codex** — `gmux __codex-hook`, **the gmux Go binary** as a subcommand; codex
  pipes event JSON to its stdin.
- **claude** — `claude_hook.go`, **Go, already in the adapter package**.

So for codex and claude the translation is *already* adapter code, in Go,
sharing the adapter's types. There is nothing to move. The only odd one out is
pi, and it is JS because pi's extension API is JS — the translation lives at the
one place with typed access to pi's events.

## Decision

**Translate to gmux's stable hook protocol at the point of typed access to the
tool's native events; keep the runner tool-neutral.**

- The agent-side hook/extension is the adapter's **agent-side translation
  surface**, written in whatever language the tool dictates (JS for pi's
  extension; Go for codex/claude, where it lives in the adapter package).
- It emits the small, stable `{op:"session"|"turn"}` contract in
  `docs/runner-hook-protocol.md`. The runner's `handleHookEvent` stays
  tool-neutral and makes no per-adapter assumptions (ADR 0011).
- The runner does **not** receive raw tool events and does **not** dispatch them
  back into per-adapter parsers.

## Rationale

The hook→runner boundary is JSON across a process *and* language boundary
(JS↔Go for pi). No end-to-end type safety is achievable there regardless of
where translation sits. The only lever is **how wide** the untyped boundary is:

- **Translate at the source (chosen):** the untyped wire carries gmux's own
  small, stable protocol. The tool's churny, version-specific internal event
  shapes stay on the tool's side; the hook absorbs that churn.
- **Forward raw (rejected):** the untyped wire now carries the tool's *entire
  internal event model*, and the Go adapter re-parses version-specific shapes it
  cannot type — widening the untyped surface and coupling the adapter to tool
  internals.

So translating at the agent side is not a compromise forced by language
mismatch; it is the design that *minimizes* the untyped blast radius and the
adapter's coupling to tool internals.

## Consequences

- The extension/hook stays a thin, stable forwarder of a small contract; tool
  version churn is absorbed where the events are typed, not propagated across
  the wire into Go.
- `handleHookEvent` remains tool-neutral — adding an agent means writing its
  agent-side translator, not branching the runner.
- pi's title derivation (first user message until pi names the session) belongs
  in `pi-ext.mjs`, mirroring what the codex/claude Go hooks already do — not in
  the runner or daemon, and not via raw-event forwarding.

## Alternatives considered

- **Dumb hooks + raw-event forwarding, adapter parses in the runner.** Rejected:
  widens the untyped boundary to the tool's whole internal event model and
  couples the Go adapter to tool internals, for no type-safety gain (the
  boundary is untyped JSON either way). See Rationale.
- **A shared, generated protocol schema across JS and Go.** Possible future
  hardening for the `{op}` contract, but the contract is small and stable; not
  worth the build machinery now.
