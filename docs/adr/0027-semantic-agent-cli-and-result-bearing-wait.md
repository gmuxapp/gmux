# ADR 0027: semantic agent CLI and result-bearing universal wait

**Status:** Accepted
**Date:** 2026-07-24
**Amended:** 2026-07-26
**Related:** ADR 0003 (resume by ID), ADR 0005 (CLI routes through gmuxd), ADR 0009 (verb-first CLI), ADR 0014 (adapter-owned conversation sources), ADR 0015 (hook translation at the agent side), ADR 0021 (ACP as the normalized conversation schema), ADR 0022 (adapter-opaque conversation refs), ADR 0023 (unified turn model), ADR 0026 (authoritative SQLite state store)
**Amends:** ADR 0009 (`agent` namespace, raw `send`, and `wait` output/exit contract), ADR 0021 (semantic write path and CLI relation), ADR 0023 (public active/inactive vocabulary)

## Context

gmux's generic CLI can launch and drive any terminal:

```sh
id=$(gmux -d -- pi)
gmux send --wait "$id" 'review this branch' Enter
gmux tail "$id"
```

That universality is valuable, but terminal mechanics are a poor primary
interface for an agent orchestrating another agent:

- Submission is a key sequence rather than intent. In pi, Enter sends now
  while Alt+Enter queues a follow-up.
- A plain prompt should not accidentally steer active work merely because its
  caller did not know the session was active.
- Detached submission must establish that work began before a subsequent
  `wait`, or the latter can observe the previous inactive state.
- `wait` currently synchronizes but does not return the conclusion. Parent
  agents consequently launch subagents and then fail to consume their answer.
- `tail` is a terminal/transcript view. The common machine result is the final
  assistant message.
- A retained conversation may have no runner. Callers should not have to
  manually resume a process before continuing the conversation.

ACP informs the internal design but does not standardize all of this. It
specifies a client speaking to an already-running agent and provides
`session/prompt`, `session/cancel`, and `session/update`; it does not own
process supervision, transparent resume, follow-up queues, steering, or an
independent universal wait. gmux also initially implements terminal pi through
semantic-to-PTY translation rather than exposing a conformant public ACP
endpoint.

## Decision

### 1. Add an experimental `gmux agent` namespace

Reserve `agent` as a top-level namespace under ADR 0009's closed grammar:

```sh
gmux agent prompt [--no-wait|--follow-up|--steer] [--timeout N] <ref> [prompt]
gmux agent cancel <ref>
gmux agent output <ref>
```

Prompt text may come from piped stdin when the positional prompt is omitted.
An interactive missing prompt and empty input are usage errors.

`agent` names the user-facing domain, not a transport. A terminal adapter and
a future ACP-native runner expose the same CLI. ADR 0021's ACP-shaped schema
remains the normalized conversation model and future native-agent seam; this
ADR does not claim public ACP server conformance.

The initial tracer is deliberately **local-only and pi-only**. Commands check
the capability they use; complete adapter support is a release/documentation
policy, not a composite `AgentAdapter` marker. Peer forwarding and Claude/Codex
support are follow-ups.

### 2. `send` is raw; semantic actions belong to `agent`

`gmux send` and the runner's raw `/input` endpoint send exactly the bytes the
caller specifies. Remove adapter-aware `--steering` and `--follow-up` from
`send`; there is one semantic surface.

Adapters translate three parameterless turn-control actions:

```text
send
send after the current turn
interrupt
```

The initial terminal capability is a stateless action-to-input encoder. It does
not perform orchestration or hold runtime state. Prompt text remains verbatim
outside the encoder. Payload-bearing operations such as model selection are
designed separately when needed.

CLI intent maps to adapter action and runtime precondition:

| Intent | Adapter action | Required state |
|---|---|---|
| plain prompt | send | inactive |
| `--steer` | send | active |
| `--follow-up` | send after current turn | any |
| cancel | interrupt | active |

Thus plain prompt and steer are the same adapter action with different caller
policy. Follow-up on an inactive agent behaves like ordinary submission.

### 3. Raw input and semantic actions use distinct endpoints

Keep raw input and semantic operations unmistakably separate.

Daemon routes:

```text
POST /v1/sessions/{id}/prompt
POST /v1/sessions/{id}/cancel
POST /v1/sessions/{id}/wait
GET  /v1/sessions/{id}/conversation
POST /v1/sessions/{id}/input        # raw bytes, unchanged
```

Runner routes:

```text
POST /prompt
POST /cancel
POST /input                         # raw bytes, unchanged
POST /hook/event
```

Flat daemon actions fit existing routing and eventual peer forwarding. A
separate runner endpoint also fails loudly against an old runner; overloading
`/input?action=...` could make an old runner ignore the query and silently
execute follow-up as ordinary raw input.

The public prompt request carries intent, for example:

```json
{
  "prompt": "review this branch",
  "mode": "prompt",
  "wait": true,
  "timeout_seconds": 0
}
```

`mode` is `prompt`, `follow_up`, or `steer`; `wait` defaults true; and
`timeout_seconds` bounds execution only (zero/absent is indefinite). Readiness
and admission deadlines are adapter/internal policy, not public knobs.

gmuxd maps public intent to stable runner mechanism:

```json
{
  "prompt": "review this branch",
  "delivery": "now",
  "require": "inactive"
}
```

`delivery` is `now` or `after_turn`; `require` is `inactive`, `active`, or
`any`. The runner waits for readiness, checks the requirement against its
authoritative state immediately before delivery, asks the adapter to encode or
invoke the action, and returns successful delivery. Unsupported actions and
failed preconditions are explicit errors; they never degrade to raw input.

`cancel` has separate daemon and runner routes, no prompt body, and an inherent
active precondition. Initially it returns after delivery rather than waiting
for the interrupted transition. An explicit waiter observes that transition.

### 4. Readiness is adapter-authoritative and generation-local

A registered runner or open socket does not prove that the underlying agent
can accept semantic input. Extend the tool-neutral hook protocol with:

```json
{"op":"ready"}
```

For pi this is emitted from `session_start` after its editor, key handlers,
submit handler, and UI have initialized. It is independent of conversation
binding so a fresh session can be ready before its first conversation file
exists.

Readiness belongs to the runner generation and is never durable. Semantic
runner endpoints wait for it according to adapter policy (initial pi timeout:
about ten seconds); raw `/input` never does. A readiness timeout occurs before
delivery and is safe to retry. Readiness may survive an in-process
conversation rebind but resets with a replacement runner.

Future ACP-native runners set ready after protocol initialization. The exact
Go readiness/encoder interface may be simplified during implementation; the
ownership and behavior above are the contract.

### 5. Plain prompt is safe by default

A plain prompt fails without delivery when the runner is active:

```text
gmux: reviewer is active; cancel it, or specify --follow-up or --steer
```

Steer and cancel fail when inactive. The CLI chooses the requirement, but the
runner enforces it close to the authoritative state and PTY/RPC delivery. This
avoids deciding from gmuxd's coalesced snapshot without introducing input
ownership or serialization. A genuinely concurrent human/agent input can
still race; gmux deliberately does not create an exclusive writer model.

### 6. Resume is transparent only for operations that need it

Plain and follow-up prompt transparently resume a retained dead session under
its existing gmux session ID, wait for the new runner's adapter readiness, and
then deliver. A dead row's historical active-at-death bit describes the prior
generation and does not block a new prompt.

Other commands do not resume:

| Operation on dead session | Behavior |
|---|---|
| plain prompt | resume, then send |
| follow-up | resume, then send (acts like ordinary send) |
| steer | fail; no active turn exists |
| cancel | fail; no active turn exists |
| output | read adapter storage without resume |
| wait | report retained state without resume |

Process supervision remains in gmuxd's lifecycle coordinator. Adapters only
describe how a stored conversation is resumed.

### 7. Admission and execution are distinct, unstored phases

Prompt command flow is:

```text
resume/readiness → delivery/admission → execution
```

These are command phases, not durable session states or operation resources.
No operation ID or queued-operation record is introduced.

For an inactive prompt, gmuxd subscribes before runner delivery and waits for
a fresh inactive→active transition. This is authoritative **acceptance**. The
initial admission deadline is a fixed ten seconds after delivery.

For active steer/follow-up and cancel, pi exposes no separate acknowledgement;
success means **delivered**, not authoritatively accepted. A future adapter ack
can strengthen that without changing the request shape.

A readiness timeout guarantees no delivery. An admission timeout is
indeterminate: bytes were delivered but activity was not observed, so blind
retry may duplicate input.

`--no-wait` returns at the admission boundary. Synchronous prompt keeps one
gmuxd subscription from before delivery through active→inactive completion,
with separate internal admission and user-facing execution deadlines.

These forms converge on the same completion/result semantics:

```sh
gmux agent prompt reviewer 'review this branch'
```

```sh
gmux agent prompt --no-wait reviewer 'review this branch'
gmux wait reviewer
```

### 8. Active/error/interrupted is the common status

The universal Turn axis is:

```text
active   = turn open
inactive = turn closed
```

Rename the common model and unreleased wire/schema from `working` to `active`.
This is an intentional compatibility break made before release so terminology
is consistent across runner, daemon, peers, frontend, and SQLite. Old runners
that lack semantic routes/status schema fail explicitly as outdated rather
than being silently interpreted as inactive.

`error` is orthogonal to activity:

- active + no error: ordinary work;
- active + error: adapter reports an attention-worthy retry/rate-limit state;
- inactive + error: terminal failure.

`interrupted` means a human or agent intentionally stopped the turn. It is
separate from error because expected cancellation must be observable to a
synchronous waiter without becoming a terminal red error or normal-completion
notification.

Normalized hook terminal events are:

```text
completed | interrupted | error
```

Each adapter maps native facts according to intent. For pi, explicit
`aborted` maps to interrupted; `stop` maps to completed; `error`, length
exhaustion, unexpected terminal tool-use, and unknown abnormal ends map to
error. A process dying mid-turn normally emits no terminal hook event and is
observed as death while active, hence error rather than interruption.

Starting a new turn clears the prior interruption. No general persisted
completion-outcome enum is required: active/error/interrupted plus liveness
and timeout facts answer the initial consumers.

CLI exit status is intentionally small and global:

```text
0 success
1 error (including timeout, death, usage, unsupported, or transport failure)
2 intentional interruption
```

This replaces the prior wait-specific died/timeout exit codes.

### 9. Turn-end is a result-consistency barrier

An adapter must not emit terminal turn-end until its authoritative
conversation reader can observe all finalized content from that turn:

```text
agent finalizes output
→ adapter-owned result source becomes readable
→ adapter emits terminal turn-end
→ runner publishes inactive
→ waits resolve and render
```

This is a normalized adapter contract, not polling duplicated in every waiter.
Pi already supplies the required ordering: final `message_end` is synchronously
appended to its JSONL before its later `agent_end` extension event. An adapter
whose native lifecycle does not guarantee this must settle or watermark at its
translation boundary before releasing turn-end. An ACP-native runner similarly
incorporates preceding conversation updates before publishing inactive.

Adapters and hooks are authoritative for readiness, activity, interruption,
and error. PTY output is presentation data and must not be scraped to invent
semantic verdicts.

### 10. `output` returns the latest final assistant message

The initial semantic read is:

```sh
gmux agent output reviewer
```

It reads adapter-owned conversation storage without resuming and prints the
latest final assistant message. It is a snapshot and does not imply success or
current-turn finality. `output` is preferable to `result` because it remains
valid for explicit inspection after partial/error/interrupted work.

The initial pi tracer reuses `ConversationRenderer`; richer exact turn and full
conversation scopes, tool inclusion, and ACP content blocks are deferred. No
public message IDs are introduced. Output is not truncated by gmux.

`gmux tail` remains the universal transcript/terminal debugging view.

### 11. `gmux wait` remains universal and becomes conditionally result-bearing

Keep one top-level wait:

```sh
gmux wait [--quiet] [--timeout N] <ref>
```

It waits for the active unit of work to become inactive. For the initial
tracer:

- renderer-capable agent + normal completion: print the latest final assistant
  message;
- `--quiet`: suppress result rendering;
- error or interruption: print no potentially stale/partial result, report the
  condition, and exit 1 or 2;
- shell/process sessions: retain synchronization-only output behavior;
- `--for-text`/`--for-regex`: retain predicate-wait behavior and print no agent
  result.

Generic shell command/lifetime output rendering remains the eventual universal
contract but is not required for the pi tracer.

Synchronous prompt and ordinary wait use the same completion classification and
result selector. A failed/interrupted caller can explicitly invoke
`gmux agent output` to inspect available content.

### 12. Waiter-owned attention is deferred

Eventually, a waiter registered before active→inactive should consume the
attention consequence of completion: all waiters receive completion, while
unread and user notification are suppressed because a parent agent is already
reacting. Later waiters should be told that another waiter may own coordination.

This is runtime-only waiter presence, not stored ownership or a lifecycle
phase. It is explicitly outside the initial implementation.

## Required implementation capabilities

The initial work must provide, without one broad stateful adapter interface:

1. **Resume derivation:** an already-described conversation returns a resume
   command or nil when non-resumable; the redundant `CanResume` probe is
   removed.
2. **Action translation:** adapter semantic send/send-after-turn/interrupt to
   its terminal or native mechanism, including readiness policy.
3. **Authoritative readiness and turn events:** ready, active/inactive, error,
   and intentional interruption.
4. **Result barrier and rendering:** terminal turn-end releases only after the
   adapter-owned final message is readable.
5. **Explicit unsupported behavior:** semantic endpoints reject unsupported
   adapters/actions and old runners; they never fall back to guessed keys or
   raw input.

Prefer small optional capabilities. Adapters remain stateless translators and
storage interpreters; runner and gmuxd own runtime truth and orchestration.

## Consequences

- Parent agents can prompt, steer, queue, cancel, wait for, and consume pi
  subagent output without key or file-layout knowledge.
- Raw terminal control remains available and unsurprising through `send`.
- Transparent resume hides process residency only where semantically useful.
- Readiness and preconditions are checked at the runner, while gmuxd retains
  lifecycle, peer-routing, subscription, and result responsibilities.
- Separate semantic runner routes prepare for terminal-less ACP-native agents
  and make old-runner incompatibility fail loudly.
- The common status can represent active retry errors and intentional
  interruption without an over-general completion enum.
- No prompt operation IDs, persisted admission phases, daemon-owned transcript,
  or gmux-owned follow-up queue are introduced.

## Alternatives considered

### Public `gmux acp`

Rejected for the CLI namespace. The initial implementation is a gmux semantic
orchestration surface backed by PTY pi, not a conformant public ACP endpoint.
ACP remains the internal normalized schema and future native transport.

### Keep only `send`, `wait`, and `tail`

Rejected as the sole machine interface. They expose terminal keys and
line/transcript views where adapter semantics exist.

### Overload raw `/input` with semantic query parameters

Rejected. Raw input has byte bodies and unconditional delivery semantics. An
old runner may ignore an unknown `action` query and silently execute the body
as ordinary input, changing follow-up into steer. Distinct semantic routes
fail loudly and support JSON/native ACP mechanisms.

### Put prompt policy only in the CLI

Rejected. The CLI reads a coalesced snapshot and can be stale before delivery.
The CLI chooses intent; the runner enforces the condition against authoritative
state. This does not create an exclusive input owner.

### Make adapters stateful runtime controllers

Rejected. gmuxd owns resume/subscriptions and the runner owns live state and
transport. Adapters remain stateless translation/storage capabilities.

### Split agent wait from generic wait

Rejected. ADR 0023 already defines one Turn abstraction. Result sources differ;
the synchronization primitive does not.

### Store resume/admission/execution or queued follow-ups

Rejected. They are command flow or adapter-owned queue state, not durable gmux
session facts.

### Make all waits quiet and require `output`

Rejected as the eventual default because parent agents routinely omit the
second read. `--quiet` preserves synchronization-only use; initial result
rendering is intentionally limited to supported agent adapters.
