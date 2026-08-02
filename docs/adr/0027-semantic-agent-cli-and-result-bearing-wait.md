# ADR 0027: semantic agent CLI and result-bearing universal wait

**Status:** Accepted
**Date:** 2026-07-24
**Amended:** 2026-07-26
**Related:** ADR 0003 (resume by ID), ADR 0005 (CLI routes through gmuxd), ADR 0009 (verb-first CLI), ADR 0014 (adapter-owned conversation sources), ADR 0015 (hook translation at the agent side), ADR 0021 (ACP as the normalized conversation schema), ADR 0022 (adapter-opaque conversation refs), ADR 0023 (unified turn model), ADR 0026 (authoritative SQLite state store)
**Amends:** ADR 0009 (`agent` namespace, raw `send`, and `wait` output/exit contract), ADR 0021 (semantic write path and CLI relation), ADR 0023 (public active/inactive vocabulary)

> **Superseded in part (2026-07-29)** by [ADR 0028](0028-cli-output-channels.md)
> (output channels), [ADR 0029](0029-agent-sessions-abstract-runner-residency.md)
> (runner residency abstraction), and
> [ADR 0030](0030-exchange-oriented-agent-reads-and-observational-wait.md)
> (exchange-oriented reads, observational wait). Superseded here: §10 (`agent
> output`, later `status` — the verb is removed; `agent logs` is the semantic
> read), §11's rendering bullets, the 2026-07-27 `logs` amendment's `-n`
> unit/default, the `--new` amendment's output shape, and the 2026-07-28
> amendment's output routing, `--json`, "Retrieval verbs", and "Steering
> interrupts waits" sections. ADR 0030 lists the exact clauses. Inline notes
> mark the affected sections below.

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
gmux agent prompt [--no-wait] [--follow-up|--steer] [--timeout N] <ref> [prompt]
gmux agent cancel <ref>
gmux agent output <ref>
```

`--follow-up` and `--steer` are mutually exclusive: each names a different
delivery, and only one can happen. `--no-wait` is orthogonal to both — it
chooses whether the caller blocks, not what is delivered — so `--no-wait
--steer` ("redirect the turn, don't wait for it") is valid and maps onto the
same `{mode, wait}` pair the wire model already carries.

Prompt text may come from piped stdin when the positional prompt is omitted.
An interactive missing prompt and empty input are usage errors. Prompt bytes
that are not valid UTF-8 are refused rather than re-encoded: substituting
U+FFFD would run different text than the caller supplied.

`agent` names the user-facing domain, not a transport. A terminal adapter and
a future ACP-native runner expose the same CLI. ADR 0021's ACP-shaped schema
remains the normalized conversation model and future native-agent seam; this
ADR does not claim public ACP server conformance.

The initial tracer is deliberately **local-only and pi-only**. Commands check
the capability they use; complete adapter support is a release/documentation
policy, not a composite `AgentAdapter` marker. Peer forwarding and Claude/Codex
support are follow-ups. *(Amended 2026-08-02 by
[ADR 0033](0033-session-drive-modes-and-semantic-capability-boundaries.md):
the Claude/Codex follow-up is the ACP drive mode, not this terminal path —
their terminal-mode sessions stay interactive-only and refuse semantic
verbs, steer permanently.)*

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

> **Amended (2026-07-29) by ADR 0029:** the per-verb table below is
> generalized to a principle — delivering work transparently resumes, reads
> never do — and the "dead session" vocabulary is operational-only: on
> semantic surfaces a runnerless session is *inactive*, steer/cancel fail
> because **no activity is in progress**, and `output`'s row is inherited by
> `agent logs` (ADR 0030).

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

> **Superseded (2026-07-29) by ADR 0030:** the verb — and its later rename to
> `agent status` — is removed. The semantic read is `gmux agent logs`, which
> renders whole exchanges (default: the latest) rather than the bare final
> assistant message.

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
*(Amended 2026-07-27: tail is the universal **terminal** view; the transcript
view is `gmux agent logs` — see the amendment below.)*

### 11. `gmux wait` remains universal and becomes conditionally result-bearing

> **Superseded in part (2026-07-29) by ADR 0030:** a resolved wait renders the
> exchange-structured report of the activity it observed — on stdout for every
> outcome (ADR 0028) — not the bare final assistant message, and an
> interrupted/failed activity's terminal partial prose is included, labeled,
> when it ends the observed span. Universality, `--quiet`, predicate waits,
> and shell/process behavior stand.

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

## Amendment: where a result-bearing answer comes from

> **Note (2026-07-29):** the surviving parts of this amendment (server-side
> selection, one classification, global exit codes) stand, except that
> "omit-rather-than-empty" is now rendered explicitly by ADR 0030's
> `[Agent completed without a final response]` marker, and non-completed
> reports go to stdout per ADR 0028.

> **Superseded in part (2026-07-28)** by the amendment below: the conversation
> **watermark** described here is replaced by a source-asserted, event-carried
> result. The
> rest of this section — server-side selection at turn close inside the
> resolving request, omit-rather-than-empty, one classification, global exit
> codes — stands unchanged.

Recorded when §8/§11 were implemented. The decisions themselves stand; this
fixes two things the decision text left open.

**The result is selected server-side, at turn close, and bound to the turn that
closed.** Both the resolved universal wait (`POST /v1/sessions/{id}/wait`) and
the synchronous prompt response carry an `output` field holding the final
assistant message of *their* turn, produced by the same selector
`GET …/conversation?scope=message` uses. Having the CLI read the conversation
after the wait returns was rejected because it reopens the staleness hole the
barrier (§9) closes: between a wait resolving and a second request landing,
another actor can start a new turn. Selecting inside the resolving request also
keeps the read store-only (no runner, no resume), so it is safe on a session
that has just died.

Server-side selection alone is **not** sufficient, because "newest prose" still
carries no turn identity: a turn that lands between the close observation and
the read would have its answer reported as ours, and a newer turn that has so
far produced only a user message would make the snapshot selector report
nothing at all. The binding is therefore a **conversation watermark** taken when
the waiter first observes its turn, and which index it records depends on what
was observed:

- a turn seen **starting** (a fresh inactive→active edge, or a prompt delivered
  into an idle agent) is bounded by the **message count**: its content is what
  comes after, and the previous turn's answer — which a user-boundary bound would
  admit whenever our own user message has not been persisted yet — stays out;
- a turn **already in progress** (a wait that subscribes mid-turn, a steer, a
  follow-up queued behind a running turn) is bounded by that turn's **user
  boundary**, the index just past the newest user message. Prose the turn has
  already persisted is inside the window, which it must be: bounding such a wait
  by the message count loses the answer of any turn whose tail is tool-only, and
  reports nothing for a wait that completed perfectly well.

At close, the selector considers only messages after the bound and stops at the
first user message beyond it, so neither a later turn's prose nor an earlier
turn's can be reported. A window with no prose omits the field, like every other
"nothing to show" case. This needs no message IDs, no turn tokens and no stored
turn metadata — an index is enough.

A conversation that cannot be read **at mark time** offers no safe boundary, and
is recorded as such: no result is attributed, because binding it to 0 would admit
an earlier turn's answer as soon as storage became readable.

A wait that finds the turn already closed has no turn to bind to and keeps
snapshot semantics, as does `gmux agent output`, which is explicitly a snapshot.
Such a wait therefore reports the conclusion and result of the turn that has
**already** finished — correct, but a reason to arm waits before delivering
(`agent prompt`, `send --wait`) when gating on a turn yet to start.

A request that will not be shown a result marks nothing: a detached
(`--no-wait`) prompt and raw `send --wait` never render a conversation.

`output` is present only for `outcome: "completed"`, and is **omitted rather
than empty** when there is nothing to show (non-renderer adapter, no
conversation, no assistant prose in the current turn). Absence therefore never
reads as "the agent answered with silence".

**One classification, two waits.** The generic wait's terminal verdict and the
semantic prompt's outcome are computed by one function
(`classifyTurnClose`): terminal error → `error`, otherwise intentional stop →
`interrupted`, otherwise `completed`. Death while a turn was open — or before
any status was ever reported — is `error` with cause `runner_died`, never a
fourth outcome and never a completion. The two wait *endpoints* remain
separate: the generic wait gains no admission/reservation machinery.

**Exit codes** are now global (§8): `0` success, `1` error (usage, unsupported,
transport, timeout, death, terminal turn failure), `2` intentional
interruption. This retired `gmux wait`'s `2 = died` / `3 = timeout` and
`gmux agent`'s `3 = execution_timeout`, and moved parse-time CLI usage errors
from `2` to `1`. ADR 0009's decision 13 and its `send --wait` amendment are
superseded accordingly.

`send --wait` reports the same **conclusion** as `gmux wait` (through the same
classification and the same exit mapping), because an intentionally interrupted
turn must not exit 0 through the composition the docs recommend. It remains
result-free: raw keystrokes make no claim about which agent turn they belong to.

The taxonomy covers verdicts gmux itself reaches. Three verbs deliberately pass
a code through instead: `gmux -- <cmd>` and `gmux edit` return the child's, and
`gmux daemon|auth|remote` exec gmuxd and return its.

One consequence is deliberate and documented: a session whose turn is its whole
lifetime closes that turn with `Error` when the child exits non-zero, so
`gmux wait` on a failed `gmux -d -- make build` exits 1. "Finished" and
"succeeded" are the same question for such sessions, and a wait that returned 0
for a failed build would be the more dangerous answer.

## Amendment (2026-07-28): the result is asserted at the source

The watermark model above bound a turn's result window when the turn was first
observed, and reconstructed attribution downstream from the conversation file.
That decision is reversed: **the adapter asserts the turn's boundary, its
result, and its trigger at the source**, and gmux never reconstructs
attribution from the conversation at all.

### Why reconstruction lost, twice

The watermark forced gmuxd to bind **before the evidence existed**. Measured
against pi's actual persistence model — a fresh conversation's file is not
written at all until the turn's first assistant output, when pi writes the user message and assistant message
together — a window marked at admission time points into a conversation that
does not exist yet. For a fresh session the mark is taken before the adapter
has even connected, so it loses the race with conversation-ref discovery by
construction: the **first synchronous prompt of every fresh session completed
with an empty answer** while `gmux agent output` could read it one second
later. The machinery required to make early binding safe — two bind kinds, a
re-mark for queued follow-ups, first-look consumption, the refless refusal —
was itself the source of most of the defects found while hardening it.

The first replacement considered was a stateless close-time read of the
conversation file ("last completed exchange"). It fixes the first-turn bug and
deletes the same machinery, but its correctness rests on a timing argument —
no foreign assistant entry can persist within the sub-second close-to-read gap
— which is true today and **unfalsifiable as an adapter contract** tomorrow,
and it stays exposed to conversation-ref rotation in the gap and gives
late-arriving waits only snapshot guesses.

Both approaches reconstruct facts the adapter holds authoritatively at the
moment they are needed. Translation at the agent side (ADR 0015) says that is
where they should be extracted — and pi's extension API already carries all of
them; no upstream change is required.

### The boundary is `agent_settled`, and a turn is pi's merged loop

Two facts about pi, verified in its source, reshape the turn model:

- `agent_end` fires **per retry attempt**: a transient provider error emits an
  error-shaped `agent_end`, then pi retries. It is not a logical boundary.
  pi's **`agent_settled`** event — "fired after an agent run has fully settled
  and no automatic retry, compaction, or queued continuation will run" — is.
  The extension closes turns on `agent_settled`, using the message list
  captured at the last `agent_end`. (This also fixes a pre-existing status
  flap where a retried error briefly read as error-then-active.)
- **pi merges queued follow-ups into the running agent loop.** A follow-up
  delivered mid-turn is drained by the same loop at its would-stop boundary:
  no second `agent_start`, no second close, one settled event whose output is
  the *merged* turn's final prose. The previous model — two turns, a re-mark
  at the queued turn's start, `queued_turn_unobserved` — describes a two-close
  world pi does not implement, and its machinery (`spanQueuedTurn` and
  friends) is deleted with the watermark.

**One loop is one turn.** A user message injected into an *active* turn —
whether a steer or a "queued" follow-up, whether delivered by gmux or typed by
a human (observable via pi's `message_start`) — extends that turn and changes
what its answer means. The steer rule in the follow-up slices below therefore
covers both injection kinds; "a follow-up queued behind yours does not change
your answer" is true only of follow-ups delivered to an idle agent, which are
ordinary turns.

### The wire

The extension (ours, in-repo) reports; the runner forwards. Turn events gain
content:

```jsonc
{ "op": "turn", "phase": "start", "turn_seq": 7, "trigger": "<excerpt>" }  // from before_agent_start.prompt
{ "op": "turn", "phase": "steered", "turn_seq": 7, "text": "<excerpt>" }   // mid-turn user message_start
{ "op": "turn", "phase": "end", "turn_seq": 7, "outcome": "completed",
  "output": "<final assistant prose>", "truncated": false }                // on agent_settled
```

`turn_seq` is an extension-local monotonic counter binding one turn's start,
injections, and close together; every consumer that pairs facts across events
pairs them by it. Across a settled run's retries and continuations the facts
accumulate independently — the trigger and injection list span the whole run,
`output` and the stop reason come from the final attempt's message list —
rather than being replaced wholesale per `agent_end`.

- `output` is the settled turn's last assistant prose from its own message
  list. A tool-only turn omits the field: `completed` with no output.
- `interrupted` and `error` turns carry no `output`; an error close may carry
  a short `diagnostic` instead (the reason/account channel, never the result).
- Caps are applied **at the extension and sized jointly against every hop**:
  `output` generously (256 KiB pre-escape), `trigger`/`text` as short
  excerpts, and the runner body and daemon scanner limits are raised to fit
  the worst-case escaped payload. The invariant: **an oversized output never
  costs the close** — the event still closes the turn, `truncated` is set,
  and the conversation read serves the full text.

### The transport: a runner-held last-turn relay record

The naive version of this design — "the payload rides the event pipe" — fails
against what that pipe actually is: the coordinator's outcome publish re-reads
the durable row post-commit, subscriber watermarks coalesce overtaken
publishes, the runner's event fan-out drops under backlog, and the generic
wait has a row-snapshot ticker path that carries no event at all. Every one of
those is a place where an edge-scoped payload silently falls off while the row
state still converges — producing "completed, exit 0, no answer", the exact
phenotype this amendment exists to kill.

The runner therefore keeps the turn facts the same way it already keeps
status, conversation ref, and slug: a **turn frame**, replayed to every
`/events` subscriber on connect as one atomic, sequence-bearing snapshot. The
frame holds two records to keep the polarities apart — the **current turn**
(`turn_seq`, trigger, injections so far) and the **last closed turn**
(`turn_seq`, outcome, output, truncated, diagnostic) — so a reader can never
pair a running turn's trigger with the previous turn's answer. It is
**conversation-local, not merely generation-local**: an authoritative rebind
(pi switch/new/resume/fork, reported at `session_start`) clears it atomically,
ordered ahead of the rebind's `ready`/session replay, so a late subscriber can
never see the previous conversation's answer under the new ref. It is not row
state, not persistence (it dies with the runner), and not a tape read; it is
ADR 0011's runner-owned live truth, extended by one structure.

The frame does not travel alone: **a turn edge is one event carrying both the
status transition and its frame** — the scoped invariant below is a transport
property only because the close can never be delivered without the frame that
closed it, so re-splitting them into separate sends reopens the lost-answer
window this section exists to close. gmuxd retains the last applied frame per
generation in registry runtime and attaches close facts to the outcome **at
apply time** (not via the post-commit re-read). Every resolution path that can declare a wait closed — the outcome
channel, the initial fanout check, the 500 ms ticker, a reconnect replay —
serves from that retained frame, and **only when its `turn_seq` matches the
close being resolved**; a mismatch (two back-to-back turns between looks)
degrades honestly to a result-free close, never to an *earlier* turn's answer.
The guarantee is directional: when the row lags the runner, a wait can bind
the frame's newer turn and serve *that* turn's answer — the newest completed
turn of a genuinely idle session, which snapshot semantics would also report.
The dangerous direction — a stale answer presented as a newer turn's — is
closed by construction.

The delivery invariant is stated positively but **scoped**: for a live wait on
a result-bearing, frame-capable generation, no path may resolve `completed`
without the settled frame having been available — a genuinely absent `output`
then always means a tool-only turn, never transport loss. Outside that scope
the close is served result-free, exactly like a non-result-bearing adapter:
shell and hook-driven sessions, raw `PUT /status` closes, and version-skewed
runners that never sent a frame must complete normally, not hang on an
invariant they cannot satisfy. Nothing is written to the central store: a wait
that arrives after the close — even while the same runner still lives — keeps
snapshot semantics, as does `gmux agent status`; the frame serves only waits
that observed their close through it.

Two operational notes stated rather than implied: the extension→runner post is
fire-and-forget, so a lost settled post leaves the session semantically active
until reconnect replay or runner death repairs it — replay makes daemon-side
transport reliable, not the source hop, and the degraded state is visible as
such (`active` with an idle screen) rather than as a false success. And the
frame is bounded end-to-end: one capped frame per runner, one retained copy
per generation in gmuxd, shared rather than duplicated into each subscriber
queue, so the budget is per-session, not per-waiter.

### Failures before the turn starts

pi's prompt submission can fail before any agent loop event fires (no API
key, no model): the TUI paints a banner and **no turn or failure event exists
on that path** — the one fact in this design that cannot be source-asserted today. No
heroics: the admission wait times out as before (indeterminate, §7), and the
stderr report for that failure appends a **terminal-tail excerpt**, clearly
labeled as the screen — best-effort diagnosis on the account channel, where
impurity is allowed. If pi ever emits a prompt-failure event, this upgrades to
a source-asserted `error` close with a `diagnostic`; nothing else changes.

### Admission windows and interrupted waits

Two operational adjustments from live testing:

- **The admission window widens from 10s to 60s.** A slow-loading model can
  legitimately take more than 10s between delivery and its first agent event;
  a 10s indeterminate answer for a healthy session is worse than a longer
  wait. Admission remains unstored and its timeout remains indeterminate (§7).
- **`prompt --new` gates on admission even with `--no-wait`.** The launch
  line's exit 0 is the health event the handoff pattern relies on; returning
  at delivery let a session that never starts its turn masquerade as a
  healthy launch. `--no-wait` still prints the bare id immediately and never
  waits for the *turn*; it now waits for the turn to *begin*, bounded by the
  same admission window. The cost is stated where the handoff pattern is
  taught: `id=$(gmux agent prompt --new --no-wait …)` returns at process
  exit, so on a sick session the launch line can now block up to the window
  instead of returning at delivery — exit 0 buying the stronger claim is the
  point.
- **An interrupted `wait` says what it is not.** *(Superseded 2026-07-29 by
  ADR 0028 decision 6: the first signal now emits a best-effort **stdout**
  report ending `[Wait interrupted; agent remains active, …]`, then exits
  128+N; a second signal terminates immediately. The "no verdict, so neither
  1 nor 2" reasoning and the `$?`-is-the-contract rule stand.)*
  SIGINT/SIGTERM on a blocking
  `wait` (or `prompt`) prints one stderr line stating that only the wait was
  interrupted — the session and its turn keep running, and `gmux wait <id>`
  re-arms — then dies from the signal (the shell's 128+N; a backstop path may
exit with the same code rather than die signaled, so `$?` is the contract,
`WIFSIGNALED` is not). The §8 taxonomy
  deliberately does not apply: gmux reached no verdict about the turn, so both
  `1` and `2` would be lies. Without the notice, ^C reads like the agent was
  stopped.

### Output routing: stdout is data, stderr is the account, the exit code is the verdict

> **Superseded (2026-07-29) by ADR 0028:** valid domain reports go to stdout
> for every outcome; stderr is only for inability to produce the report;
> `--json` is removed (unreleased) pending a designed machine contract.
> `--quiet` stands.

- **Completed** (exit 0): stdout is the answer, alone — `wait`, sync `prompt`,
  and sync `--new` (after its id line) unchanged. Quiet success: no trigger
  echo, no report.
- **Non-completed** (exit 1/2): stdout empty, and a **status-shaped report on
  stderr** — outcome, reason (`canceled`, `steered`, `runner_died`, `timeout`,
  …), the trigger excerpt, and the injected text when a steer is the reason.
  The account arrives exactly when the caller needs it, with no second
  command.
- **`--json`** on `wait` and sync `prompt`: a stable envelope on stdout
  regardless of outcome (`outcome`, `reason`, `output`, `trigger`,
  `steered_by`, `truncated`). This is the machine contract, and it releases
  the human default from purity pressure.
- Reports and envelopes are rendered **from the relayed turn facts only**,
  never from a post-resolution conversation read — a re-read would reopen the
  §9 gap this amendment closes. `status`, the snapshot verb, is the one that
  reads the tape.
- `--quiet` unchanged (verdict only).

### The adapter contract

Result-bearing becomes an adapter property with a testable definition: **a
result-bearing adapter asserts its turn boundary, and delivers the turn's
outcome, final assistant message, and triggering excerpt in its own terminal
and start events.** pi implements it with existing API (`before_agent_start`,
`agent_end` + `agent_settled`, `message_start`). claude/codex do not become
result-bearing (unchanged). No tape-read fallback exists when the settled
event lacks `output`: absent means tool-only, and the conversation read
remains the explicit recourse.

One scope **widening** is deliberate: the settled event reports every turn's
result, however the turn started. An armed `gmux wait` is therefore
result-bearing for turns a human typed into the TUI or raw `send` initiated,
not only for semantically prompted ones. §11's condition widens accordingly.
`send --wait` itself remains result-free by its own contract.

### Follow-up slices recorded here, implemented separately

> **Superseded (2026-07-29) by ADR 0030:** both slices below shipped
> unreleased and are replaced. `agent status` and the `logs` message-type
> filters are removed (`logs -n` counts exchanges, default 1), and waits are
> **observational**: steering, merged follow-ups, and human messages never
> resolve a wait early, so the delivery-identity claim rules below — and
> their accepted ambiguity residue — are deleted with the ownership
> semantics. Injection events survive as report material.

**Retrieval verbs.** `gmux agent output` is renamed to **`gmux agent status`**
and answers "whatever is relevant now" with a fixed three-part shape: a state
line (alive/idle/active, last outcome), the triggering excerpt, and the
relevant content (final answer when idle; recent messages when active).
`gmux agent logs` gains message-type filters: no flags → user and agent
prose-bearing messages; explicit type flags (`--user`, `--agent`, `--tool`,
`--thinking` where rendered) **replace** the default set; `--all` is
everything; `-n` counts post-filter messages. `logs --agent -n 1` approximates
the old `output` read — as a snapshot it can be staler than a wait's carried
result, and the docs say so rather than claiming equality. The verb split is
cognitive, not shape-based: `status` = "I don't know what I want, show me what
matters"; `logs` = "I know the exact text I want"; `tail` = raw screen.

`--follow-up`'s help must state the mode split this model makes observable:
delivered to an **idle** agent it starts an ordinary turn; delivered into an
**active** turn it merges into that turn (pi's queue), which interrupts other
waiters like a steer and makes the merged close's answer the follow-up's.

**Steering interrupts waits.** A user message injected into a turn someone
else is waiting on — a steer, or a follow-up merged into the active loop —
changes the contract of the pending answer, so it resolves that wait early:
exit 2, reason `steered`, report on stderr carrying the injected text. The
turn itself keeps running; the waiter re-arms if it still cares. The injecting
request's own sync wait is excluded by runner-local correlation: the delivery
carries an id, and the injector may claim the merged close **only after a
`message_start` steer event was unambiguously correlated to it by text** —
exact equality under one shared normalization, or an extension-marked
truncation prefix. Ambiguity (identical pending texts, an unmarked prefix)
credits the acknowledgement to nobody: the injectors resolve indeterminate and
bystanders are interrupted by the id-less injection. One residue is accepted
rather than closed: a human typing text byte-identical to an in-flight steer
at the same moment is indistinguishable by construction — and the merged
answer then reflects that identical instruction, a distinction without a
difference. If the turn settles before that
acknowledgement arrives, pi may have closed without consuming the injection —
the injector's result is then indeterminate (reported as such), never the
pre-injection answer under exit 0. And the claim holds only while the
injector's message is the loop's **last** injection: a later injection — human
or semantic — supersedes it, and the earlier injector is interrupted like any
other waiter, with the report saying the turn was steered again after its
message. Human injections (detected via `message_start`) interrupt every
waiter: a human grabbed the wheel.

## Amendment (2026-07-27): `gmux agent logs`, and `tail` goes back to raw

> **Superseded in part (2026-07-29) by ADR 0030:** `logs` renders whole
> exchanges with the shared renderer — `-n` counts **exchanges** and defaults
> to **1**, not 100 messages — and the table's "What is the answer?" row is
> answered by `gmux agent logs` itself (`agent output`/`status` is removed).
> The verb's placement, store-only shape, error taxonomy, and the `tail`
> reversion stand.

The conversation-markdown view that ADR 0009's 2026-07-12 amendment made
`gmux tail`'s default for renderer-backed sessions moves into this namespace as
**`gmux agent logs <id> [-n N]`** (`-n` counts messages, default 100), and
**`gmux tail <id> [-n N]` reverts to decision 13a's unconditional raw PTY
view** (`-n` counts lines). `--raw` and its `-e`/`-r` aliases are removed from
`tail`; like the pre-2.0 action flags they are refused **by name**, with an
error naming `gmux agent logs`, rather than reported as unknown.

### Why: three questions, three verbs, no word doing double duty

| Question | Command | `-n` |
| --- | --- | --- |
| What is on its screen? | `gmux tail <id>` — any session | lines |
| What has it been doing? | `gmux agent logs <id>` — agents | messages |
| What is the answer? | `gmux agent output <id>` | — |

The old arrangement failed the same test this ADR applies to `send`: one verb
whose **output shape depended on the session's adapter** could not be scripted
without first knowing what was running inside the session — and `-n` silently
changed unit with it. That is the raw/semantic boundary this namespace exists
to draw, crossed by a flag. Splitting the views puts the adapter-aware read
where every other adapter-aware read already lives (local-only, pi-only,
store-only), and leaves `tail` a single answer it can give for a shell, a
one-shot command and an agent alike.

`logs` is the word the neighbours use for "what has this thing been doing"
(`docker logs`, `kubectl logs`), which also reserves the obvious next step:
`--follow` is deliberately **not** implemented, but the name now has a home
that can grow one, whereas ADR 0009 decision 13a had to ban a top-level `logs`
verb outright. 13a's ban stands: this is namespace growth under decision 9, not
a new bare verb. `gmux logs <id>` therefore prints the same
"agent commands are namespaced" hint `prompt`/`cancel`/`output` print.

### Shape

`agent logs` is a **store-only** read, exactly like `agent output`: one GET, no
runner, so it works on a dead retained session and can never start or resume
one. Local sessions only. Its error taxonomy is `agent output`'s verbatim — the
daemon's code and message printed as-is, an envelope-less 404 reported as
version skew rather than a missing session, and `unsupported_adapter` /
`no_conversation` carrying the read-side `gmux tail <id>` hint, because the raw
view is exactly what a caller who cannot have a transcript should reach for.

Unlike `agent output` it needs **no marker-header guard**: that guard exists
because an old daemon ignoring `scope=message` would answer with the whole
transcript, and the whole transcript is precisely what this verb asked for. The
only skew that can bite is a daemon with no `/conversation` route at all, which
arrives as the envelope-less 404 above.

### Server-side

The read is gmuxd's existing transcript scope, unchanged:
`GET /v1/sessions/{id}/conversation?tail=N`. One answer did change: a session
whose adapter is not a `ConversationRenderer` now gets **422
`unsupported_adapter`** instead of 404 `no_conversation`, with the adapter
check ordered before the conversation-ref check — the same ordering, and the
same reason, as the message scope. "This adapter has no conversation model" is
permanent and actionable; "nothing rendered yet" is transient. The two
collapsed into one code only because `gmux tail` keyed its scrollback fallback
on `no_conversation`, and that fallback no longer exists. Peers and older
daemons still answer `no_conversation` for the permanent case; the CLI treats
both identically (exit 1, `gmux tail` hint), so skew costs precision, not
correctness.

## Amendment (2026-07-27): `gmux agent prompt --new` launches the session

`gmux agent prompt --new [--model M] [--name N] [--timeout S] [--no-wait]
[<prompt>|-]` launches a session and sends it its first prompt in one command.
Exactly one of a positional session ref or `--new` may be given. `--new`
refuses `--follow-up` and `--steer` — a session that does not exist yet has no
turn to queue behind or steer — and `--model` / `--name` are usage errors
without it. There is no `--session` flag, no `--adapter` flag and no argv
passthrough: the launch is pi-only for now, exactly like the rest of this
namespace.

### Bare launch, and admission as the single health event

The spawned argv carries **no prompt**. gmux starts the session through the
existing `gmux -d` machinery (from the caller's env and cwd, local daemon
only — a remote-only daemon fails the way `gmux -d` fails), and then delivers
the prompt over the ordinary readiness-gated `POST /prompt`.

That is the whole point. A launch-time prompt would create a second, differently
shaped health event: "did the agent start with my text?" answered by process
exit and screen scraping, next to "was my prompt admitted?" answered by the
delivery reservation this ADR already defines. With a bare launch there is one
event. A session that never becomes ready fails its **first** prompt exactly as
it fails its tenth — `admission_timeout`, same code, same indeterminacy rules,
same retry advice — and the first turn needs no special case anywhere in the
CLI, the daemon or the runner.

`--timeout` keeps its meaning: it bounds the wait, never the launch. Readiness
stays on the adapter's fixed `ActionReadyTimeout` (10 s for pi), which is a
property of that agent's cold start, not of the caller's patience.

### Output contract: the bare id is always stdout line 1

> **Superseded in part (2026-07-30) by ADR 0028's payload-rule amendment:**
> the synchronous shape prints the bare id on **stderr** and its stdout is
> the exchange report alone; only `--no-wait` keeps the id on stdout, because
> there the id is the payload. The id is still emitted exactly once, the
> moment the session exists and before delivery, and nothing is emitted when
> no session exists. The rest of this section — what the id means, the
> exit-code contract, post-spawn ownership — stands.

Under `--new` the session id is written to stdout, on its own line, **the
moment the session exists** — after the spawn registers and *before* the prompt
is delivered. With `--no-wait` it is the only output and exit 0 means admitted:
that is the launch line of the handoff pattern, `id=$(gmux agent prompt --new
--no-wait 'go')`. Synchronously it is line 1 and the agent's answer follows, so
under `--new` **the completion signal is the exit code, not non-empty stdout** —
a deliberate difference from a plain sync prompt, where stdout is the answer
alone, and one the help page and `reference/cli.md` state explicitly.
*(Amended 2026-07-29 by ADR 0030: what follows the id line is a blank line and
the exchange report, and a plain sync prompt prints that report rather than
the answer alone; the id-first rule and the exit-code contract stand.)*

**The id line means exactly one thing: this session exists and is
addressable.** It is not an admission receipt, not a readiness signal, not a
claim that the prompt was delivered, and not a promise that the turn will run.
Every one of those verdicts is carried by the exit code, where the taxonomy in
§8 already defines them. Stating the negative matters because the line is
printed *before* the only event that could support those readings.

Printing the id before delivery rather than at admission was a live design
question — the slice brief said "at admission" — and is settled here in favour
of the earlier print. With a single synchronous POST there is no
client-observable admission moment for the `wait:true` shape at all; splitting
it into admit-then-wait would buy no health signal (the exit code carries
admission and completion either way, and `--no-wait`'s exit 0 remains
202-gated) while costing a second request and its skew edges. The watcher
use-case is served strictly better by the earlier id, which can attach or tail
*during* readiness.

It also resolves the pre-admission failure question. Anything can go wrong after the spawn: the
agent never becomes ready, the runner dies, the turn errors. In every one of
those cases the caller has already paid for a real session, and a session whose
id was never reported is a leak nothing can clean up. So the id goes to stdout
first, unconditionally and exactly once; failures after that point report on
stderr and exit per the taxonomy in §8. A failed launch — nothing registered,
no session — prints no id at all, because there is nothing to address. The rule
a script can rely on: **stdout line 1 is a session id whenever a session
exists, and stdout is empty whenever one does not.**

One observed consequence of not special-casing the first turn: a freshly
launched session's first prompt frequently completes with an **empty** result
field, because the daemon has not yet resolved that session's conversation ref
when the turn closes. This is pre-existing behaviour — `gmux -d -- pi` followed
by a synchronous prompt does exactly the same — and it is left alone here
rather than patched at the launch site, which would be precisely the
launch-shaped special case this amendment exists to avoid. The turn completed,
the exit code says so, and `gmux agent output <id>` reads the reply. Fixing it
belongs where the gap is: conversation-ref discovery at turn close.

**A post-spawn failure leaves the session behind, and the caller owns it.** The
session keeps existing and may well keep running — gmux does not tear down a
session because its first prompt failed, which would destroy the state and
scrollback the caller needs to diagnose it. The printed id is what that
ownership is made of: retry against it, read it, or `gmux kill` it.

`--new` must appear **before** the prompt. After a session id it is prompt text
like any other token — `gmux agent prompt s1 --new` prompts s1 with the literal
text `--new` — because "everything after the ref is verbatim" is a rule of this
grammar older than this flag, and a flag that clawed tokens back from it would
make prompt text depend on gmux's flag vocabulary. The `-` spelling for "prompt
on stdin" is scoped to `--new` for the same reason: on the ref path a trailing
`-` is a literal prompt and a leading `-` is an unknown flag, both unchanged.

Ordering follows from the same rule in the other direction: the prompt is
validated (non-empty, UTF-8, within budget) and the argv translated *before*
anything is spawned, so a usage error never leaves an orphan session behind.

### Adapter translation

`adapter.AgentLauncher` — `LaunchCommand(LaunchOptions) (argv []string, ok
bool)` — sits next to `AgentActionEncoder` as the launch-side twin: stateless
translation, no I/O, no session state. pi implements it (`pi [--model M]
[--name N]`, pi's real long flags; an empty option is omitted entirely rather
than passed empty, because `--model ""` is a different request than no
`--model`). Every other adapter does not implement it and the CLI reports
`unsupported_adapter` in the established style, pointing at `gmux -d -- <cmd>`
plus a prompt as the two-step route that still works and always will.
