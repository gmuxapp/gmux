# ADR 0029: agent sessions abstract runner residency

**Status:** Accepted
**Date:** 2026-07-29
**Related:** ADR 0011 (runner-owned session state), ADR 0014 (adapter-owned conversation sources), ADR 0016 (session retention, scrollback as cache), ADR 0023 (unified turn model), ADR 0027 (semantic agent CLI), ADR 0030 (exchange-oriented agent reads and observational wait)
**Amends:** ADR 0027 §6 (transparent resume — generalized from a per-verb table to a principle), ADR 0023 (public vocabulary for agent sessions)

## Context

gmux's operational model is process-shaped: a session is *alive* when its
runner's socket answers and *dead* otherwise, and ADR 0016 made dead sessions
first-class retained objects. ADR 0027 §6 then made `agent prompt`
transparently resume a dead session — but only prompt; every other semantic
surface still leaked residency. `gmux wait` on a runnerless session reported
"retained state", reads were documented as working "on a dead retained
session", and the public status axis carried an alive/dead shadow next to
active/inactive.

For the caller of the semantic surface — a parent agent orchestrating a
subagent — residency is an implementation detail with no decision value. The
thing it holds a handle to is a *conversation* that can always be continued;
whether a process currently hosts that conversation is gmux's problem. Worse,
"dead" is actively misleading vocabulary for a healthy, settled, resumable
conversation: it invites exactly the manual pre-resume choreography ADR 0027
§6 set out to delete.

Separately, resident runners of settled conversations cost memory and file
descriptors forever, because nothing may reap them as long as "process exists"
is a public fact someone might depend on.

## Decision

### 1. The agent session is a resumable conversation, not a process

The semantic handle addressed by `gmux agent …` and `gmux wait` names a
**conversation that can be continued** — ADR 0014/0022's adapter-owned
conversation plus gmux's session identity around it. A **runner incarnation**
(one runner process generation serving that session) is a hosting detail. The
handle, the slug, and the conversation identity are stable across
incarnations; incarnation boundaries are not semantic events.

### 2. Public agent state is active / inactive — never "dead"

Semantic surfaces expose exactly the ADR 0023 turn axis: **active** (work
open) or **inactive** (work settled), plus the orthogonal error/interrupted
facts of the last settled activity. **No resident runner means inactive**,
indistinguishable in vocabulary from a resident-but-idle agent. The words
"dead", "alive", "runner", and "resume" do not appear in semantic reports.

Operational and raw surfaces — `gmux ls`, `gmux tail`, `gmux kill`,
`gmux daemon …`, the web UI's residency indicators — legitimately expose
residency and keep the alive/dead vocabulary. The split is by surface, not by
session kind: the same session shows residency to an operator and hides it
from a semantic caller.

### 3. Prompt transparently resumes; reads never do

Delivering work (`agent prompt`, plain or `--follow-up`) to a session with no
resident runner spawns an incarnation under the existing session ID, waits
for adapter readiness, and delivers — invisibly, as ADR 0027 §6 already
decided. Semantic reads (`agent logs`, a wait that finds no live activity)
answer from adapter-owned conversation storage without resuming; observing a
conversation must never restart it. Steer and cancel still fail without a
resident active turn — not because the runner is gone, but because **no
activity is in progress**, which is how the error is worded.

### 4. Runner loss during active work is a failed activity

When an incarnation is lost while its activity is open, the activity's
outcome is **error** (the semantic fact: the work did not settle), reported
in domain vocabulary — `[Agent failed: …]` — without claiming a process died.
Runner exit *after* a settled activity is not an event at all on the semantic
surface: the settled outcome (completed/interrupted/error, ADR 0023's
turn-state-at-death) is durable and is never overwritten by the later process
exit. A wait issued before or after the loss reaches the same verdict.

### 5. Settled runners may be collected invisibly

gmux may automatically reap a runner incarnation when — and only when — its
activity is **source-settled** (the adapter asserted the terminal boundary;
never inferred from silence), and no pending semantic delivery and no
adapter-asserted queued work exists for it. Because prompt transparently
resumes and public state is active/inactive, collection is semantically
invisible: the session reads inactive before and after. Scrollback and
sessionmeta retention (ADR 0016) are unaffected; `gmux tail` still answers
from the broker.

Collection policy (whether, when, how aggressively) is deliberately not fixed
here; the *safety precondition* above is. A runner that a human is attached
to, or whose composer may hold an unsubmitted draft, is a policy concern the
implementation must resolve conservatively before enabling collection by
default.

### 6. Startup convergence must precede transparent resume

After a daemon restart, a surviving runner may be temporarily undiscovered.
Transparent resume must not race that window: spawning a second incarnation
for a conversation whose first is still alive would fork the conversation or
double-deliver. The daemon must converge discovery (or positively establish
absence) for a session before resuming it. Residency remains unexposed; the
convergence cost surfaces only as first-prompt latency after a daemon
restart.

### 7. gmux persists no conversation content and writes none

gmux never appends markers, boundaries, or any entries to adapter storage
(pi's JSONL stays exclusively pi's), and never persists conversation content
in its own stores. Non-content **terminal outcome metadata** already retained
in the central session row — last outcome, interrupted/error facts,
timestamps — survives runner collection and daemon restart; conversation
history gmux never observed settle carries no outcome and reads as an
inactive snapshot, never a fabricated completed verdict. While an incarnation exists, live truth is its
source-asserted events and frame (ADR 0011); without one, history is
reconstructed on demand from the adapter's native conversation storage. The
store, sessionmeta, and SQLite hold runtime facts only.

## Consequences

- Orchestration scripts never branch on residency: prompt when you want work,
  read when you want history, wait when you want a verdict.
- "Semantic state is inactive" becomes reachable without any process: a
  freshly swept, runnerless conversation is a first-class inactive agent.
- Resource use for idle fleets stops growing monotonically once collection
  ships; nothing observable changes for semantic callers.
- Error messages on semantic surfaces need re-wording from process vocabulary
  to activity vocabulary (no "session is dead", no "runner died" as a public
  phrase; the reason is a failed activity).
- ADR 0023's state machine, sources, and waitability rules are untouched;
  this ADR only fixes which vocabulary each *surface* speaks.

## Alternatives considered

### Expose residency with softer words ("suspended", "parked")

Rejected. Any third public state re-imports the manual choreography: callers
would special-case it. Two states plus transparent resume is the whole point.

### Keep runners forever, skip collection

Rejected as the steady state (unbounded residency cost), but collection ships
behind the safety precondition and may lag this ADR.

### Persist gmux-side activity boundaries to make history exact

Rejected. Appending to adapter JSONL corrupts another program's storage
format; persisting content in gmux duplicates the adapter's source of truth
and violates ADR 0014. ADR 0030 instead scopes what history reconstruction
promises (latest user boundary wins; exact historical settled grouping is not
promised).
