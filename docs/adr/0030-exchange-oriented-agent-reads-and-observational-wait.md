# ADR 0030: exchange-oriented agent reads and observational wait

**Status:** Accepted
**Date:** 2026-07-29
**Related:** ADR 0014 (adapter-owned conversation sources), ADR 0015 (hook translation at the agent side), ADR 0022 (adapter-opaque conversation refs), ADR 0023 (unified turn model), ADR 0028 (CLI output channels), ADR 0029 (agent sessions abstract runner residency)
**Supersedes in part:** ADR 0027 — §10, §11's rendering bullets, the 2026-07-27 `logs` amendment's `-n`/default, the `--new` amendment's output shape, and the 2026-07-28 amendment's output routing, `--json`, "Retrieval verbs", and "Steering interrupts waits" sections. Exact clauses are listed at the end.

## Context

The unreleased follow-up stack on ADR 0027's amendments grew three reads with
three shapes (`agent status`'s three-part summary, `agent logs` with five
message-type filters, wait/prompt's bare-answer-or-stderr-report), a wait that
steering *interrupts*, and a delivery-identity correlation machine whose whole
job was deciding which waiter may claim a merged answer — including a
documented irreducible ambiguity (a human typing text byte-identical to an
in-flight steer).

Using it revealed the misdesign underneath:

- **Wait-as-contract fought pi's physics.** pi merges queued follow-ups and
  steers into one running loop; one settled run is one turn. Making a wait's
  answer *owned* — resolved early with exit 2 when someone else's message
  entered "its" turn — meant every injection had to be attributed, which text
  cannot prove, hence the correlation machinery and its residue. But a parent
  agent's actual question is observational: *what happened since I asked?* A
  wait that reports the merged timeline honestly needs no ownership at all.
- **The bare answer was the wrong unit.** "Print the final assistant message"
  collapses a fifteen-iteration, twice-steered activity into its last
  paragraph, then needed `status`'s trigger echo, `steered_by`, and stderr
  reports to paper the context back in — three shapes for one story.
- **Message-count `logs -n` was unusable.** Message counts vary wildly with
  tool traffic; "the last exchange" is what callers want and cannot ask for.
- **`status` vs `logs` vs `output` was a vocabulary tax.** Three verbs whose
  differences were shape-based, not need-based.

This ADR replaces that stack with one renderer over one vocabulary, applied
at three scopes, and an observational wait. Everything removed here is
unreleased: verbs and flags disappear without migration errors.

## Vocabulary

- **Logical activity** (source boundary): one adapter-asserted span of agent
  work, opened at the loop start and closed at the adapter's settled
  assertion — for pi, `agent_settled` (ADR 0027's 2026-07-28 amendment). It
  is what a live wait observes. One activity may contain several user
  messages (merged follow-ups, steers, human TUI input).
- **Visible exchange** (user boundary): one user message and all activity
  after it up to, but not including, the next user message. A display
  boundary for rendering, deliberately *not* pi's `agent_settled`.
- **Iteration**: one completed assistant/model response — tool-use responses,
  retried/recovered attempts, and the terminal response all count. The honest
  unit of "how much work happened".

Activity is what you *wait on*; exchanges are what you *read*. The two
boundaries coincide only when no message enters a running loop.

## Decision

### 1. One renderer

Every semantic read renders the same human record (ADR 0028: one coherent
stdout document):

```
[37 previous exchanges]

[USER]: <message>

[Agent worked for 15 iterations]

[USER]: <additional instruction>

[Agent worked for 4 iterations]

[AGENT]: <terminal response in full>
```

Rules:

- Only **terminal** assistant prose is rendered — the prose that ends the
  scope's last exchange. Nonterminal assistant prose, tool calls, thinking,
  and recovered (retried) error prose are omitted and represented only by the
  iteration count. A retried error is never rendered as an error.
- Work markers say `[Agent worked for N iterations]` / `[Agent worked for 1
  iteration]` — singular and plural honestly. A zero-iteration span between
  two user messages produces no marker.
- A final exchange still in progress ends
  `[Agent active, N iterations so far...]`, or
  `[Agent active, no completed iterations yet...]`.
- A completed final exchange with no terminal prose ends
  `[Agent completed without a final response]` — stated, never silent, so an
  empty answer can't be confused with a lost one.
- Partial terminal prose is included **only when it ends the scope's last
  exchange**, labeled `[AGENT, partial]: …` and followed by
  `[Agent interrupted]` or `[Agent failed: <reason>]`. Runner loss during
  active work may make the latest persisted assistant prose terminal-partial,
  followed by the semantic failure line — never by "the process is dead"
  (ADR 0029). Such prose qualifies **only when it belongs to the latest open
  visible exchange**; the renderer never reaches backward to a prior
  exchange's prose to fill the slot. If no qualifying prose is available, the
  partial is omitted and the failure line stands alone.
- Empty scope: `[No exchanges yet]`.
- Exchanges before the scope are summarized as `[N previous exchange(s)]`,
  counting the **active conversation branch** only, not abandoned branches.
- Message content and whitespace are preserved. Labels prefix content as
  shown; multiline content stays multiline. Source-side caps (the extension's
  output/excerpt budgets) still apply and are marked honestly when they cut.

### 2. Three scopes, one shape

- **`gmux agent logs <id> [-n N]`** — history. `-n` counts *exchanges*,
  defaults to **1**, must be positive. The message-type filters (`--user`,
  `--agent`, `--tool`, `--thinking`, `--all`) and their API plumbing are
  removed. Logs never abbreviate. Store-only read, never resumes (ADR 0029);
  the 2026-07-27 amendment's error taxonomy and store-only shape stand, with
  one refinement: a **resolvable** conversation source whose timeline is
  simply empty is a valid report — `[No exchanges yet]`, stdout, exit 0.
  `no_conversation` remains the stderr error for a conversation source that
  cannot be resolved or read at all, not for an empty timeline.
- **`gmux wait [--quiet] [--timeout N] <ref>`** — renders the **activity
  span it observed**, exchange-structured. Details in decision 3.
- **Synchronous `gmux agent prompt`** — exactly the wait renderer applied to
  the activity its prompt started or joined. Under `--new`: the bare session
  id on stdout line 1 (unchanged: printed as soon as the session exists), a
  blank line, then the report. `--new --no-wait`: the id only. The `--new`
  amendment's other rules (admission gate, pre-validation, failed sessions
  are the caller's to keep) stand. *(Amended 2026-07-30 by ADR 0028's
  payload-rule amendment: the synchronous id line moves to stderr with no
  separator; stdout is the report alone. `--no-wait` keeps the id on
  stdout.)*
- **`gmux agent status` is removed entirely.** Its "what matters now"
  question is answered by `logs` (defaults to the latest exchange, and shows
  `[Agent active, …]` when work is in flight).
- **`gmux tail`** remains the raw terminal view, unchanged.
- **`--json` is removed** from wait/prompt/logs (ADR 0028 decision 4).

### 3. Wait is observational

`gmux wait` reports what happened; it holds no claim over the activity:

- **Nothing interrupts a wait but its own terminal conditions.** Steering,
  merged follow-ups, and human TUI messages never resolve a wait early — they
  appear in its report as new exchange boundaries. The "steering interrupts
  waits" model, its `steered` reason, and the delivery-identity claim rules
  are superseded; the runner's injection *recording* remains, as report
  material.
- **The wait terminates at the first source-asserted settle
  (`agent_settled`) of its observed activity.** A follow-up admitted into the
  loop *before* the settle belongs to that activity and thus to this wait's
  report; one admitted *after* the settle starts a new activity and belongs
  to the next wait.
- **A late wait travels back.** A wait that arms when no activity is open
  returns immediately, rendering the **latest visible exchange** and exiting
  by its settled outcome. The race-stability guarantee is scoped precisely:
  arriving just before or just after the settle yields the **same verdict
  (exit code) and the same terminal content** — the terminal response or
  partial, and the terminal state line. It does *not* promise an identical
  multi-exchange document: a live wait renders the whole activity span it
  observed, while a late wait renders the latest visible exchange. A
  conversation with no exchanges reports `[No exchanges yet]` and exits 0.
  A conversation with history but **no observed or recorded outcome** (e.g.
  foreign history gmux never watched settle) is reported as an inactive
  snapshot without an outcome claim — never a fabricated completed verdict.
- Shell/process sessions and `--for-text`/`--for-regex` predicate waits keep
  their ADR 0023/0027 behavior; this renderer applies to renderer-capable
  agent sessions.

### 4. Wait-only abbreviation

Wait and prompt reports abbreviate exactly two things:

1. the **anchor** user boundary — the latest user message already visible
   when a bare wait (or a wait joining an active activity) arms, or the
   command's own prompt when it starts the activity; and
2. every user message after the command's delivery baseline whose text is an
   **exact string match** for the text that synchronous command submitted —
   all of them, if repeated.

The match is presentation-only — no outcome or ownership semantics hang off
it — and deliberately strict: no normalization, no prefix matching. If caps
or transport made the stored text differ from the submitted text, the message
simply renders in full; nothing worse can happen. Abbreviation cuts at the
first 20 whitespace-delimited words or 240 Unicode characters, whichever
comes first, preserves retained whitespace exactly, and appends `…` iff
something was omitted. Every other user message, and the terminal response,
render in full. `logs` never abbreviates.

### 5. Outcomes, exit codes, channels

Per ADR 0028, the report goes to stdout in every case gmux can produce it:

| Wait/prompt observation | stdout | exit |
|---|---|---|
| activity completed (any number of additional instructions) | report | 0 |
| activity interrupted | report | 2 |
| activity failed, or `--timeout` elapsed | report | 1 |
| first local SIGINT/SIGTERM | best-effort report | 128+N |
| cannot identify/inspect the target | — (stderr account) | 1 |

- The timeout report ends
  `[Wait timed out after Ns; agent active, N iterations so far...]` (with the
  honest zero-iterations variant).
- The signal report ends
  `[Wait interrupted; agent remains active, N iterations so far...]`; a
  second signal terminates immediately (ADR 0028 decision 6).
- `--quiet` stays verdict-only, and wins on the first signal too: no report,
  exit 128+N.

### 6. Truth sources and persistence

- **No content persistence, no adapter writes** (ADR 0029 decision 7): no
  gmux markers in pi's JSONL, no conversation content in gmux stores.
  Non-content **terminal outcome metadata** already retained in the central
  session row (last outcome, interrupted/error facts, timestamps) survives
  runner cleanup and daemon restart; it is what a late wait's verdict reads.
- **While an incarnation exists**, active/inactive and the live wait span
  come from its source-asserted events and frame (ADR 0011; the turn-frame
  transport of ADR 0027's 2026-07-28 amendment, which this ADR keeps). The
  frame's carried facts grow to serve the renderer: the activity's user
  boundaries and per-segment iteration counts, asserted by the extension.
  Carried text is **verbatim up to an honest cap** — no whitespace collapsing
  or other normalization, every cut marked. The live budget is generous but
  **bounded**: an activity that exceeds it never loses its close or result —
  the report preserves the source-asserted outcome and terminal content and
  states explicitly what was dropped (omitted exchange/byte counts). This is
  a degraded *display*, never an attribution or outcome change; `logs` reads
  the full native content. Delivery-identity correlation
  (`NotePendingInjection` and friends) loses its consumer and is deleted with
  the ownership semantics.
- **The `[N previous exchange(s)]` count is presentation-only.** For a live
  wait it may be obtained from an adapter store read at render time; the
  result, outcome, and terminal content remain strictly frame-sourced. A
  count gmux cannot obtain is omitted, never guessed.
- **Without an incarnation**, semantic state is inactive and the latest
  exchanges are reconstructed on demand from the adapter's **native
  conversation branch** (the active branch; abandoned branches are neither
  rendered nor counted). For pi this means walking the parent chain back from
  the **last persisted entry** — the same leaf rule pi itself applies when
  opening a conversation. A branch cursor moved only in memory with no
  persisted append is intentionally invisible: the latest persisted branch
  wins.
- **Reconstruction promises exchange grouping, not activity grouping.**
  Adapter storage records no settled boundaries and gmux persists none, so
  `logs` does not promise to reproduce historical `agent_settled` grouping —
  the latest user boundary wins. `logs` is history when more context is
  needed; the wait report is the activity-faithful view, available exactly
  when the activity is observed.
- **Cleanup and convergence** follow ADR 0029 decisions 5–6: only
  source-settled runners with no pending semantic delivery or adapter queue
  may be collected, and startup convergence must prevent duplicate resume —
  without exposing residency.

## Superseded ADR 0027 clauses

- **§10 (`agent output`)** — superseded entirely. The verb (and its
  2026-07-28 rename to `status`) is removed; the semantic read is
  `agent logs` with the exchange renderer.
- **§11, rendering bullets** — "print the latest final assistant message" and
  "error or interruption: print no potentially stale/partial result" are
  replaced by decision 1's report (partial prose appears, labeled, only when
  terminal). §11's universality, `--quiet`, predicate waits, and shell
  behavior stand.
- **2026-07-27 `logs` amendment** — `-n` counts exchanges (default 1), not
  messages (default 100); the renderer is decision 1's, not the raw
  transcript scope. The verb's placement, store-only shape, error taxonomy,
  and the `tail` reversion stand.
- **2026-07-27 `--new` amendment, output contract** — "the agent's answer
  follows" becomes id line, blank line, report. Everything else stands.
- **2026-07-28 amendment, "Output routing"** — superseded by ADR 0028.
- **2026-07-28 amendment, "Admission windows and interrupted waits", last
  bullet** — superseded by ADR 0028 decision 6.
- **2026-07-28 amendment, "Retrieval verbs"** — superseded: no `status` verb,
  no logs filters, no post-filter `-n`.
- **2026-07-28 amendment, "Steering interrupts waits"** — superseded by
  decision 3: waits are observational; no early resolution, no `steered`
  reason, no injection claim/ambiguity rules, no indeterminate injector
  outcome. The injection *events* and their frame recording survive as
  renderer input.
- **2026-07-28 amendment, "the result is asserted at the source"** — stands
  (boundary = `agent_settled`, frame transport, one-event turn edges,
  source-side caps), with its carried facts widened from
  trigger/injections/output to the exchange structure of decision 6, and its
  "completed with no output omits the field" consequence now rendered as
  `[Agent completed without a final response]`.

## Consequences

- One renderer to implement and test; `status`'s three-part shape, the filter
  matrix, and the correlation machinery (`pendingInjections`,
  `matchPendingLocked`, the excerpt-normalization contract's evidence role)
  are deleted.
- The irreducible ambiguity ADR 0027 accepted (human text byte-identical to
  an in-flight steer) vanishes: nothing is claimed, so nothing can be
  misclaimed. Exact-match abbreviation can at worst render a message
  unabbreviated.
- A parent agent gets the full story — what it asked, what else entered, how
  much work happened, how it ended — from one invocation, in one document,
  with the verdict in `$?`.
- `output=$(gmux wait id)` no longer yields the bare answer; callers that
  need machine-tight output wait for the future JSON contract (ADR 0028).
- The extension/frame wire grows (exchange segments, iteration counts,
  fuller user texts) and the correlation surface shrinks; caps must be
  re-sized jointly against every hop, keeping the invariant that an oversized
  payload never costs the close.

## Alternatives considered

### Keep ownership semantics but fix the correlation

Rejected. Text cannot prove provenance; every strengthening (IDs through the
composer, markers in the JSONL) either changes pi or violates the
no-adapter-writes rule. Observational reporting answers the actual question
without needing provenance at all.

### Render every message (full transcript) instead of iteration counts

Rejected as the default read. Tool traffic dwarfs the conversation; the
renderer's job is the story, and `tail`/future filters remain the escape
hatch for forensics.

### Persist settled boundaries so `logs` can group by activity

Rejected (ADR 0029 decision 7). The user boundary is reconstructible from the
adapter's own storage forever; settled grouping is available live, from the
source, when it matters — to the wait that observed it.

### Make `wait` return only exchanges after its arm point

Rejected. A wait that arms late would render nothing useful; traveling back
to the latest visible exchange makes the race with settle invisible in
verdict and terminal content instead of merely small.
