# ADR 0033: session drive modes and semantic capability boundaries

**Status:** Accepted
**Date:** 2026-08-02
**Related:** ADR 0009 (verb-first CLI), ADR 0013 (codex authoritative state via hooks), ADR 0015 (hook translation at the agent side), ADR 0021 (ACP as the normalized conversation schema), ADR 0022 (adapter-opaque conversation refs), ADR 0023 (unified turn model), ADR 0027 (semantic agent CLI and result-bearing wait), ADR 0028 (CLI output channels), ADR 0029 (agent sessions abstract runner residency), ADR 0030 (exchange-oriented reads and observational wait), ADR 0031 (session ID issuance), ADR 0032 (session input doors)
**Amends:** ADR 0027 §1 (the "Claude/Codex support are follow-ups" clause — the follow-up is the ACP drive mode, not the terminal path)

## Context

ADR 0027–0032 define gmux's semantic agent contract: readiness-gated prompt
delivery with `--follow-up`/`--steer` preconditions, cancel, source-asserted
turn boundaries, result-bearing observational wait, and exchange-structured
reads. Pi implements the full contract through its terminal runner. ADR 0027
§1 recorded Claude and Codex support as follow-ups, and two PRs attempted to
deliver exactly that — the same contract, through each tool's interactive
terminal plus its hook system and native conversation storage:

- **PR #438 (Claude, `feat/claude-agent-interface`, head `14034503`).** The
  code passed adversarial review and CI: four review rounds closed
  active-branch reconstruction (parent-linked `uuid`/`parentUuid` traversal
  with abandoned-branch and sidechain privacy), image-only exchange
  boundaries, non-destructive settings injection, and reachable launch
  routing. It was then **closed at a live manual gate**. Driving the real
  Claude TUI showed the interactive terminal is not an automation transport:
  trust/onboarding modals appear before the composer accepts input at all;
  readiness could only be approximated with arbitrary elapsed-time delays,
  because no event distinguishes "painted" from "accepting input"; whether
  Enter submits depends on focus, vim-mode, and rebindable keybindings;
  permission dialogs change what the same keys mean mid-turn; and obtaining
  the result required a compensating second observational wait after the hook
  reported the turn closed. Every one of these is a property of a UI built
  for a human with eyes, not a defect gmux code could fix.
- **PR #439 (Codex, `feat/agent-interface-codex`, head `96edd0d9`).** Blocked
  in review on two structural findings, both deterministic from the
  production event translation and verified against upstream Codex source:
  Codex's hooks **never emit semantic readiness** (no event corresponds to
  "the composer will accept a prompt"), so every readiness-gated action times
  out before delivery; and its turn events carry **no identity, boundary, or
  output facts** — no `turn_seq`, no trigger, no iteration or user-boundary
  events, no terminal output, and upstream `StopRequest` carries no outcome —
  so the daemon correctly serves such live waits result-free and a
  synchronous `gmux agent prompt` can never render the exchange report the
  public contract promises. These are upstream interface properties, not
  implementation bugs.

The two failures have one shape. The semantic contract needs facts the
transport must assert: readiness before delivery, submission as intent rather
than a keystroke that might mean something else today, and a settled boundary
that carries the result (ADR 0027's 2026-07-28 amendment). Pi's terminal
satisfies this because **gmux controls both sides of that interface** — the
in-repo pi extension asserts readiness, boundaries, injections, and results
from inside the agent, and pi's composer semantics are ours to keep stable.
Claude's and Codex's terminals are foreign UIs: their keybindings, modals,
and hook vocabularies are owned upstream, optimized for humans, and free to
change without notice. Emulating a user against them yields a contract whose
every clause rests on an unfalsifiable timing or keybinding assumption.

ACP is the interface Claude and Codex *do* offer for automation. ADR 0021
already anticipated ACP-native, terminal-less operation, and draft PR #388
holds the gmux-native conversation UI seam (streaming
text/thinking/tool-calls). An ACP contract spike (report of 2026-08-02,
`.grove/acp-contract-spike/`) then verified the ground truth against the
protocol (v1 stable, v2 draft) and both current first-party adapters
(`claude-agent-acp` 0.64.0, `codex-acp` 1.1.8, live `initialize` handshakes
plus pinned-source analysis): ACP as implemented can express essentially the
whole `gmux agent` contract — **including mid-turn steering distinct from a
queued follow-up**, via the `_session/steering` extension both adapters
implement and advertise. Readiness is the `initialize` response instead of
elapsed-time guesses; submission is a JSON-RPC request instead of
focus-dependent keystrokes; turn completion is the `session/prompt` response
instead of a compensating wait; and permission dialogs are requests gmux
answers instead of key semantics changing underneath a blind writer. Every
failure cited in the #438/#439 post-mortems has a structural fix here.

## Decision

### 1. The harness is the identity; capabilities attach to the (harness, mode) pair

A session's identity is its **harness** — pi, claude, codex — plus its gmux
session id and the harness's native conversation. There is one adapter per
harness. What varies is the **drive mode**: how gmux hosts and drives that
harness. Two drive modes exist:

- **Terminal mode** — the existing PTY session: a real process on a durable
  PTY, attachable, raw-sendable, tailable, with whatever *observational*
  facts the adapter's hook/extension and native storage trustworthily
  provide (ADR 0011/0013/0015). Semantic control in this mode requires the
  ADR 0027 contract to be source-asserted, which today only pi's in-repo
  extension does.
- **ACP mode (planned)** — a runner speaking the Agent Client Protocol to a
  terminal-less agent process: `initialize` as the readiness barrier,
  `session/new`/`session/load`, structured prompt turns, streaming
  `session/update` (natively ADR 0021's schema), tool calls, permission
  requests, cancellation, and the `session/prompt` response as the stable
  completion boundary, rendered in gmux-native UI (PR #388 is the seam). No
  PTY exists; there is no screen to tail and nothing to attach to.

Capabilities are a property of the **(harness, mode) pair**, not of the
harness name alone and not of the mode alone. "A Claude session" is one
identity — one harness, one native conversation store, one gmux session id —
that at any moment is being driven in one mode; which verbs it supports
follows from the pair. Adapter names stop implying capabilities.

Internally this is a per-adapter seam, not a second adapter: alongside the
existing terminal seams (argv/hook translation, storage renderers), an
adapter that supports ACP mode implements a "how to drive this harness via
ACP" interface — adapter process/argv, initialize expectations, extension
capabilities to honor (`_meta.steering.supported`), and native-storage
mapping so both modes read the same conversations. Modeling `claude` and
`claude-acp` as separate adapters or session kinds is explicitly rejected:
it would fragment one conversation identity across two names, duplicate
catalogs and configuration, and turn mode conversion (decision 6) into a
cross-identity migration.

### 2. Claude and Codex terminal-mode sessions are interactive-only

`gmux -- claude` and `gmux -- codex` remain plain interactive terminal
sessions. They keep everything terminal mode honestly provides: launch,
attach, raw `send`, `tail`, hook-driven active/idle status, titles,
attribution, retention, resume-by-relaunch, and observational transcript
reads from native storage (the review-hardened renderers salvaged from the
closed PRs — see Consequences). They gain no semantic control in this mode:
`gmux agent prompt` (plain and `--follow-up`), `gmux agent cancel`, and
driven launches targeting terminal-mode Claude/Codex refuse, and `--steer`
refuses **permanently** (decision 3).

`gmux wait` remains universal (ADR 0027 §11): it synchronizes on any
session's activity, including hook-observed Claude/Codex turns, with the
fidelity the hooks actually provide (Codex's outcome-less `Stop` stays
coarse, per ADR 0013). It is result-bearing only where the mode asserts
results; on Claude/Codex terminal sessions it never fabricates an exchange
report from a screen.

### 3. Refusal is explicit, names the boundary, and states the remedy

A semantic action against a terminal-mode session whose adapter does not
source-assert the contract fails **before any delivery**, with an error that
names the mode boundary rather than a transient condition:

```text
gmux: claude terminal sessions are interactive-only; semantic control
requires the session in ACP mode. Use gmux send / gmux tail to drive this
session, or gmux attach to take over.
```

The refusal is a capability fact in the `unsupported_adapter` family of the
established error taxonomy (ADR 0027's 2026-07-27 amendment): checked and
reported before readiness, activity, or residency, because "this session in
this mode cannot do that" is permanent-shaped and actionable while the
others are transient. It never degrades to raw input — a semantic verb must
not silently become keystrokes (ADR 0027 §3's loud-failure rule applies at
this boundary too).

The refusals are not all the same shape:

- **`prompt` and `--follow-up` are refused initially, with a documented
  future path.** Delivering work has a correct mode available; once mode
  conversion (decision 6) and the ACP runner exist, an explicit
  `--convert` flag may perform the idle-gated convert-then-deliver as one
  command. This stays opt-in and explicit — a plain prompt never converts a
  session's mode as a side effect.
- **`--steer` is refused permanently on terminal mode.** Steering means
  injecting text into a turn that is *right now* mutating the screen and its
  key semantics — the exact moment a Claude permission dialog can appear and
  turn the submission Enter into "approve whatever is on screen". There is
  no future flag that makes that safe. The remedies are stated in the error:
  attach and type (a human can see the dialog), or convert the session to
  ACP mode, where steering is a first-class request.

### 4. Claude/Codex semantic control is delivered in ACP mode

The ADR 0027–0030 contract for Claude and Codex — launch, prompt, follow-up,
steer, cancel, result-bearing wait, exchange logs — is implemented by the
upcoming ACP runner, on the mechanics the spike verified:

- **Readiness** is the `initialize` round-trip; session existence is the
  `session/new`/`session/load` response. No elapsed-time policy.
- **Steer** is the `_session/steering` extension (not v1 core), gated at
  runtime on `InitializeResponse._meta.steering.supported` — advertised by
  both current adapters, verified live. Its injection semantics match pi's
  merged loop ("one loop is one turn", ADR 0027's 2026-07-28 amendment), so
  ADR 0030's observational wait needs no changes in kind. Where the
  capability is absent or a raced idle steer starts a new turn, gmux reports
  honestly rather than pretending.
- **Follow-up** is serialized **uniformly in the gmux runner**: at most one
  outstanding `session/prompt` per session, queued follow-ups delivered on
  idle. Claude's adapter-native prompt queueing exists but is deliberately
  not used as the mechanism — the two adapters differ (Codex leaves
  concurrent prompts undefined), and one runner-side queue keeps behavior
  adapter-independent and preserves "follow-up on idle acts like plain
  prompt".
- **Cancel** is `session/cancel`, with the spec-required `cancelled` stop
  reason mapping to gmux's *interrupted*.
- **Turn close** is the `session/prompt` JSON-RPC response; because updates
  and the response share one ordered pipe, ADR 0027 §9's
  content-before-turn-end barrier holds by transport construction. The v1
  response carries only a stop reason, so **gmux assembles the result** from
  the typed update stream (terminal prose, iteration segmentation at
  tool/thought boundaries) — the same class of work the pi extension does,
  from typed events instead of a PTY.
- **Waiting-on-user is first-class**: a pending `session/request_permission`
  or elicitation *is* the waiting state, distinguishable from active work —
  the exact fact #438 could not get from Claude hooks. The corollary is a
  genuinely new obligation: **gmux is the ACP client and must own the
  permission-answering policy** (auto-allow / allow-safe / ask, surfaced
  through ADR 0018 notifications and the web UI, with a default and timeout
  story before unattended orchestration is safe).

An ACP session is **not a pretend terminal**. gmux does not synthesize a PTY
around it, `attach`/`tail` do not apply (their exact CLI behavior for
terminal-less sessions is an open question below), and its UI is the
gmux-native conversation view. Where the semantic contract and ACP's
expressiveness disagree, the resolution is designed in the ACP runner work
with honest degradation, never by falling back to terminal emulation.

### 5. The canonical session spec is `model:effort@harness`

Driven launches address what the caller actually chooses — a model, an
effort, and a harness to run them in — with one spec syntax:

```text
gmux agent prompt --new --model anthropic/claude-fable-5:low@pi 'review this branch'
```

`model:effort@harness` is the canonical long-term surface for driven
launches, carried by `--model` and replacing the separate `--adapter` flag
as those verbs mature (the positional argument remains the session address,
per ADR 0027/0031). The harness names the identity (decision 1); the drive
mode is not part of the spec and is not a preference: a driven launch uses
the mode in which the harness supports driven launch per the capability
matrix (decision 9) — ACP for claude/codex, terminal for pi. Every component is optional shorthand in practice: the **resolver**
— unique whole-token shorthand matching over the usable catalogs, recency
tiebreak, and the `preferred_harnesses` semantics — is deliberately **not
specified here** and is reserved for a follow-up ADR. This ADR fixes only
the canonical shape and that resolution operates over (harness, mode) pairs
whose capability sets this document defines.

### 6. Mode conversion is an explicit relaunch, never a live switch

A session's drive mode can change through one lifecycle operation:
**"Relaunch as ACP" / "Relaunch as Terminal"**. It keeps the same gmux
session id and the same harness conversation, ends the current incarnation,
and starts one in the other mode using the **harness's native resume**
(`session/load` on the ACP side; `--resume`-style relaunch on the terminal
side) — the same continuation both tools perform themselves. It is not live
switching: there is no moment where one conversation is served by both
modes, and no implicit conversion ever happens as a side effect of another
verb (the future `--convert` of decision 3 is this operation, invoked
explicitly).

Preconditions and honesty requirements:

- **Idle-gated.** Conversion waits for the current activity to settle
  (ADR 0023's turn axis); it never tears down a mid-turn incarnation.
- **Confirmed.** The user (or the flag-bearing caller) confirms a prompt
  that enumerates what may be lost: unsubmitted composer text, and any
  in-flight UI state the native resume does not carry.
- **Version-skew warning for Claude.** The ACP adapter runs the SDK-bundled
  Claude binary, not the user's installed CLI (spike risk 5), so a converted
  session may continue under a different Claude Code version than the
  terminal it left; the confirmation says so. (`CODEX_PATH` lets Codex run
  the user's binary; a corresponding `CLAUDE_CODE_EXECUTABLE` policy is an
  ACP-runner design question.)
- **Composer preservation is future work**, best-effort by construction:
  for third-party TUIs a `$EDITOR`-hotkey capture (open the composer content
  in an editor gmux can read) is the candidate; for pi, a native extension
  command can return the draft exactly. Until then the confirmation names
  the composer as at-risk.

### 7. Pi remains terminal-first with full semantic control

Pi keeps its terminal mode and its complete semantic capability set. This is
decision 1's rule applied, not an exception to it: pi's terminal *is* part
of its product value — extensions, custom renderers, direct human
intervention mid-session — and the in-repo extension gives gmux both sides
of the interface, which is exactly the condition under which a terminal can
source-assert the contract. No pi ACP mode is planned; nothing here
precludes one later, as a second mode of the same adapter under the same
rule.

### 8. The product rule

**Use the native terminal when the terminal is part of the agent's value;
use ACP when the terminal is merely a UI around an automatable agent.** For
pi the terminal is the product surface. For Claude and Codex the terminal is
one client of an agent that offers a structured protocol for exactly the
control gmux wants; automating the human client instead of speaking the
protocol was the category error both closed PRs paid for.

### 9. Capability matrix

Capabilities by (harness, mode). "Refused" is decision 3's explicit error;
"n/a" means the surface does not exist in that mode. ACP-mode columns are
planned, on the spike's verified mechanics.

| Capability | pi · terminal | claude · terminal | codex · terminal | claude · ACP (planned) | codex · ACP (planned) |
|---|---|---|---|---|---|
| launch (`gmux --` / `-d` / launcher) | yes | yes | yes | yes (ACP runner spawn) | yes |
| driven launch (`prompt --new`, session spec §5) | yes | refused | refused | yes | yes |
| attach / raw `send` / `tail` | yes | yes | yes | n/a (no PTY; gmux-native conversation UI) | n/a |
| observational status (active/idle, error) | yes (extension) | yes (hooks) | yes (hooks ≥ 0.135, coarse Stop) | yes (protocol events) | yes |
| waiting-on-user, distinguishable | yes | no (hooks cannot) | no | yes (permission/elicitation requests) | yes |
| titles, attribution, retention, resume | yes | yes | yes | yes (`session/load`, stable ids) | yes |
| observational reads (`agent logs`, native storage) | yes | yes (#448) | yes (#450) | yes (same native storage) | yes |
| `gmux wait` synchronization | yes | yes (hook fidelity) | yes (hook fidelity) | yes | yes |
| result-bearing wait / exchange report | yes | refused / no result claim | refused / no result claim | yes (gmux-assembled, v1) | yes |
| `agent prompt` (plain / `--follow-up`) | yes | refused (future: explicit `--convert`) | refused (future: explicit `--convert`) | yes (runner-serialized queue) | yes |
| `agent prompt --steer` | yes | **refused permanently** | **refused permanently** | yes (`_session/steering`, capability-gated) | yes |
| `agent cancel` | yes | refused | refused | yes (`session/cancel`) | yes |
| explicit mode relaunch (§6) | n/a (single mode) | → ACP (planned) | → ACP (planned) | → terminal (planned) | → terminal (planned) |

### 10. Non-goals

- **No live mode switching.** One incarnation, one mode; conversion is a
  full relaunch under decision 6's gates, never a hot handover.
- **No implicit conversion.** No verb changes a session's mode as a side
  effect; the future `--convert` is explicit by name, idle-gated, and
  confirmed.
- **No claim of mode equivalence.** The two modes may differ in
  configuration surface (settings/MCP/plugins loading, binary version) and
  UI state; conversion states the differences (decision 6) rather than
  papering over them.
- **No pretend PTY for ACP sessions**, and no revival of TUI emulation for
  Claude/Codex under any flag.
- **No inferred semantics in terminal mode.** PTY output is presentation
  data (ADR 0027 §9); no amount of screen parsing promotes a terminal
  session into a semantically controllable one.

## Consequences

- **Claude/Codex semantic control is consciously blocked on the ACP runner
  landing.** We prefer the correct feature when ACP lands over a fragile TUI
  emulator now. The two-step interactive route (`gmux -d -- claude`, then
  `gmux send`/attach) keeps working throughout, as does hook-driven status.
- **The observational work survived as salvage.** PR #438's review-hardened
  Claude transcript pieces (parent-linked active-branch reconstruction,
  sidechain/meta privacy filtering, image-only boundaries, non-destructive
  settings/hook injection) landed as PR #448, and PR #439's Codex rollout
  renderer as PR #450 — observational reads and status fidelity on terminal
  mode, without semantic-control claims. The same native-storage parsers are
  the read-without-resume path for ACP-mode sessions (ADR 0029 §3 forbids
  resuming to read, and `session/load` requires a live adapter process), so
  their fidelity is shared infrastructure for both modes — and must be
  regression-tested against adapter/SDK upgrades, since the ACP adapters
  write the same stores.
- **ADR 0027 §1 is amended.** "Peer forwarding and Claude/Codex support are
  follow-ups" now reads with this ADR's split: the follow-up for
  Claude/Codex is the ACP drive mode, and terminal mode never grows their
  steer verb (prompt/follow-up only via explicit conversion). ADR 0027's
  2026-07-28 adapter-contract clause ("claude/codex do not become
  result-bearing") is confirmed for terminal mode. No other existing doc was
  found claiming terminal semantic support for Claude/Codex: the website
  adapter/integration pages describe only observational hooks, status,
  titles, and resume, which remain true.
- **Mode becomes a real session axis.** Stores, the CLI, and the web UI will
  need to carry and display the drive mode (launchers, capability checks,
  the conversation-view-vs-terminal split, the relaunch operation). That
  design belongs to the ACP runner work; this ADR fixes that the axis exists
  within one harness identity and that capability checks key on the pair.
- **gmux acquires a permission-answering obligation.** As the ACP client it
  must answer `session/request_permission`/elicitations; policy, UI, and
  timeout defaults are prerequisites for unattended ACP orchestration.
- **Adapter versions become contract surface.** The first-party adapters
  churn fast (the Codex adapter was replaced wholesale within months; the
  Claude adapter releases near-daily). Mitigation is decided here: **pinned
  adapter versions plus a conformance smoke suite** gmux runs against
  adapter upgrades (initialize capabilities, steer/queue/cancel semantics,
  taxonomy mapping, storage-parser compatibility).
- **The v1→v2 protocol transition is contained by design.** ACP v2 (draft)
  restructures exactly the lifecycle gmux depends on (prompt response
  becomes acceptance-only; completion via `state_update`; `messageId`
  upserts). The runner isolates the v1 lifecycle mapping behind one seam so
  v2 is a mapping change, not a redesign — and v2 would upgrade today's
  gmux-assembled result heuristics into exact facts.

## Open questions deferred to the ACP runner implementation

The ACP contract spike answered the questions this ADR's draft had deferred;
its findings are source-verified against pinned adapter versions, with only
the `initialize` handshakes exercised live. Answers adopted here:

- **Steer vs follow-up:** expressible. Steer via the `_session/steering`
  extension, gated on `_meta.steering.supported`, merging into the running
  turn like pi's loop; follow-up via **uniform runner-side serialization**
  (decision 4) despite Claude's native queue, because Codex has none and one
  queue keeps runner behavior adapter-independent.
- **Result boundaries:** the prompt response closes the turn with the §9
  barrier intact; gmux assembles terminal content from the typed stream
  (v1 has no result payload; tool-only turns are detectably prose-free).
- **Taxonomy:** `end_turn` → completed; `cancelled` after gmux's own cancel
  → interrupted; JSON-RPC error (with Claude's structured `errorKind`) →
  error; stdio EOF with a prompt in flight → error. `refusal`/`max_tokens`/
  `max_turn_requests` classify as error-with-reason (they end the turn
  without doing the work).
- **Waiting-on-user:** first-class via pending permission/elicitation
  requests; gmux must own the answering policy (decision 4).
- **Reads without resume:** stay on the native-storage parsers (#448/#450);
  `session/load` is the resume path, never the read path.
- **Churn and protocol risk:** adapter pinning + conformance smoke suite;
  v1 mapping behind one seam for v2 (Consequences).

Genuinely open, deferred to the runner work (and the follow-up ADRs named):

- **The session-spec resolver** (decision 5): shorthand matching over usable
  catalogs, recency tiebreak, and `preferred_harnesses` semantics are decided
  by ADR 0034; catalog wire shape remains open there.
- **Permission policy defaults and UI**: auto-allow/allow-safe/ask tiers,
  timeout behavior, ADR 0018 notification shape.
- **Adapter distribution and binary policy**: bundling vs `npx`, pin cadence,
  `CLAUDE_CODE_EXECUTABLE`/`CODEX_PATH` alignment with the user's CLIs
  (also feeds decision 6's skew warning).
- **Process topology**: adapter process per session vs shared multiplexed
  process (failure-domain coupling vs memory).
- **v1 result-assembly fidelity**: whether iteration counts meet ADR 0030's
  rendering promises before v2's `messageId`, to be validated at the ACP
  runner's live gate — which must also exercise steering injection, cancel
  latency, resume replay, and permission flows (the spike ran handshakes
  only; the terminal path also looked fine until its live gate).
- **`gmux tail`/attach behavior for terminal-less sessions**: polite refusal
  naming `agent logs`/the web view, or a conversation rendering.
- **Composer capture for conversion** (decision 6): `$EDITOR`-hotkey design
  for foreign TUIs; pi extension command.

## Alternatives considered

### Keep hardening the terminal automation path

Rejected. Both PRs demonstrated the failure is structural, not residual: for
Claude every fix (readiness delays, focus/keybinding assumptions, modal
handling) replaced one unverifiable timing assumption with another, against
an interface whose owner may change any of it in a patch release; for Codex
the required facts (readiness, turn identity, results) do not exist in the
interface at all. A contract built on those foundations would be ADR 0027's
loud-failure principle inverted — quiet lies instead of loud errors.

### Model claude/claude-acp as separate adapters or session kinds

Rejected (an earlier draft of this ADR did exactly this). The conversation,
its native storage, its titles, and its resume lineage belong to the
harness; two session kinds would fragment one identity across two names,
duplicate model catalogs and configuration, make the capability story a
naming convention instead of a (harness, mode) fact, and turn mode
conversion into a cross-identity migration with no principled owner for the
shared conversation.

### Headless one-shot modes (`claude -p`, `codex exec`) as the semantic transport

Rejected as the transport. One-shot invocations discard the durable session:
no follow-up queue, no steering, no mid-turn observation, no
permission-request surface, and a fresh process per prompt. ACP is the
structured, session-holding version of the same idea and is what these
vendors maintain for programmatic clients.

### Wait for upstream hooks to grow readiness/outcome facts

Rejected as a plan. Hook vocabularies are upstream property with no
committed timeline, and even a complete hook set still leaves *delivery*
running through the TUI's focus/modal/keybinding surface — hooks fix
observation, not control. Hooks remain terminal mode's observational
channel; if they improve, observation improves, and nothing here changes.

### Delegate follow-up queueing to Claude's adapter-native queue

Rejected. It exists (`_meta.claudeCode.promptQueueing`) and is clean in
isolation, but Codex's adapter leaves concurrent prompts undefined, and two
queueing regimes behind one verb is the adapter-dependent behavior this
namespace exists to prevent. One runner-side queue, drained on idle, gives
both harnesses the same phenotype and preserves gmux's "follow-up on idle
acts like plain prompt" rule.

### Refuse Claude/Codex semantic verbs silently or degrade to raw send

Rejected outright. Degrading a semantic verb to keystrokes is the old-runner
hazard of ADR 0027 §3 reintroduced deliberately; silent refusal teaches
callers nothing. The error names the boundary and the working paths —
send/attach, or mode conversion once it exists.

### Allow implicit convert-on-prompt instead of an explicit `--convert`

Rejected. A plain prompt that silently relaunches the session in another
mode would change its binary version (Claude), its configuration surface,
and its UI affordances as a side effect of delivering text — precisely the
class of surprise decision 6's confirmation gate exists to prevent. The
convenience is preserved as an explicit, flag-named opt-in.
