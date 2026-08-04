# ACP host program

**Status:** program of record for the ACP drive-mode work
**Date:** 2026-08-02
**Governing ADRs:** 0033 (drive modes and capability boundaries), 0034
(driven-launch model resolution), 0027–0032 (the semantic agent contract),
0021 (ACP as the normalized conversation schema)
**Consolidates:** the ACP contract spike report
(`.grove/acp-contract-spike/.memory/report-acp-contract-spike.md`), the
acp-host proposal (`.grove/acp-host/.memory/proposal-acp-host.md`), and the
PR #388 fit report
(`.grove/rebase-388-acp-ui/.memory/report-388-pi-acp-fit.md`), plus decisions
settled outside ADR text. Where those documents disagree, **this document
wins**; it supersedes the proposal's plan and the fit report's slice list.

## 1. Goal

gmux acts as an ACP client/host: `gmux agent prompt/follow-up/steer/cancel`,
`gmux wait`, and `gmux agent logs` drive Claude Code and Codex through
ACP-mode sessions with the same public contract pi has via its terminal
integration (ADR 0027–0030). ACP-mode sessions have no PTY; the web UI
renders the conversation. Per ADR 0033: one adapter per harness, terminal and
ACP are drive modes of one identity, capabilities attach to the
(harness, mode) pair, and Claude/Codex semantic control ships only in ACP
mode (terminal steer is refused permanently).

## 2. Settled architecture

### 2.1 Runner

- **One adapter process per gmux session**, spawned and supervised by a
  PTY-less variant of the existing runner (the `gmux` process), serving the
  standard per-session socket surface (`/meta`, `/prompt`, `/cancel`,
  `/events`, kill/reap) minus raw `/input` and the PTY WebSocket. Shared
  multiplexed adapter processes are rejected: they couple failure domains,
  share cwd/env, and break ADR 0011's runner-owned-truth model. Memory cost
  is bounded by ADR 0029 §5 collection, which is *safer* for ACP runners
  (no composer draft) and should be enabled for them first.
- **Protocol layer:** `coder/acp-go-sdk` (official Go SDK), pinned. It
  replaces the hand-rolled `cli/gmux/internal/acp` types. One upstream
  `ClientSideConnection` over the adapter's stdio per session.
- **Turn facts reuse the existing frame machinery.** The ACP runner drives
  `session.State` (`OpenTurn` on prompt send, `NoteInjection` on steers and
  user echoes, `NoteIteration`, `CloseTurnFrame` at the `session/prompt`
  response) so result-bearing wait, sync prompt reports, admission edges,
  and the ADR 0030 exchange renderer work **without gmuxd changes**.
  Readiness = the `initialize` round-trip feeding the existing
  `markReady`/`awaitReady` gate. Adapter stdio EOF while a prompt is in
  flight = failed activity (ADR 0029 vocabulary).
- **Session tape.** The runner retains a normalized transcript plus
  in-flight turn state per session. The tape answers viewer attaches
  (snapshot-then-stream), reconnects, and mid-turn joins without touching
  the adapter — the PTY ring-buffer/scrollback split (ADR 0004/0016)
  applied to conversations. **Retention: none.** The tape is
  incarnation-local, never persisted, and carries no eviction/size-policy
  machinery; the harness's own storage is the source of truth for anything
  durable (reads without resume stay on the native-storage renderers), and
  tape dumps exist only as debug artifacts.
- **Package shape:** a new **`acprunner` sibling package** sharing
  `session.State`, socket bind, registration and `/events` with
  `ptyserver`. `ptyserver` is deliberately **not** generalized behind a
  transport interface. A shared conformance suite in CI exercises the
  common runner contract (registration, `/meta`, `/events`, turn edges,
  semantic refusals) against both runner kinds; unification is revisited
  after slice 6 with two real implementations in hand.
- **Fan-out topology.** ACP is 1:1 per connection; the runner is the
  multiplex point. One upstream client connection to the adapter; N
  runner-hosted **agent-side facade connections** (one per WebSocket
  attach) re-serving the tape as `session/load` then live
  `session/update`s. All viewer writes route through the runner's single
  prompt-serialization point.
- **Minimal capabilities on both faces.** The client capabilities gmux
  advertises to the adapter and the agent capabilities the facade
  advertises to browsers are both minimal: no fs capabilities, no terminal
  capabilities, no form elicitation (§2.3). The rule: **every capability
  advertised must have a consumer in a shipped slice** — capabilities are
  added when the slice that consumes them lands, never speculatively.

### 2.2 Semantics (from the spike, adopted by ADR 0033 §4)

- **Prompt:** `session/prompt`, `require inactive` enforced at the runner.
- **Follow-up:** uniform **runner-side queue**, at most one outstanding
  `session/prompt` per session, drained on idle. Claude's native
  `promptQueueing` is deliberately unused.
- **Steer:** `_session/steering` extension, gated at runtime on
  `InitializeResponse._meta.steering.supported`. Claude's
  `idleBehavior: "promptRequired"` opt-in maps to gmux's "no activity in
  progress" error; Codex's `startedNewTurn` race is reported honestly.
- **Cancel:** `session/cancel`; the spec-required `cancelled` stop reason →
  interrupted.
- **Outcome taxonomy:** `end_turn` → completed; `cancelled` following
  gmux's own cancel → interrupted (a `cancelled` gmux never asked for is an
  error — interruption is an intent fact); JSON-RPC error → error, mining
  Claude's structured `errorKind`/`authRequired` for the diagnostic;
  `refusal`/`max_tokens`/`max_turn_requests` → **error-with-reason**; stdio
  EOF mid-prompt → error.
- **Result assembly (v1):** the prompt response carries only a stop reason,
  so the runner assembles the result from the typed update stream behind
  one versioned seam (the *turn assembler*): terminal prose = the trailing
  `agent_message_chunk` run of the turn (exact by transport construction);
  iteration segmentation per §2.5. ACP v2 (`messageId` upserts,
  `state_update`) replaces the assembler's internals only.
- **Reads without resume:** the native-storage renderers (#448 Claude JSONL
  active-branch walk, #450 Codex rollout renderer) serve runnerless
  `agent logs`/late waits. `session/load` is the resume path, never the
  read path (ADR 0029 §3). The runner asserts the conversation ref itself:
  Claude ACP sessionId = session UUID →
  `~/.claude/projects/<encoded-cwd>/<id>.jsonl`; Codex sessionId = threadId
  → its rollout file. Ref mapping is a method of the per-adapter ACP drive
  seam.

### 2.3 Permissions: yolo first, policy surface later

gmux's model is unattended orchestration. v1 answers
`session/request_permission` with a **runner-side auto-allow policy** (select
the most permissive allow option), and additionally sets the harness's native
low-friction mode at `session/new` (Claude permission mode, Codex approval
preset) to reduce request volume. The client does not advertise form
elicitation capability, so Claude's `AskUserQuestion` is disallowed by the
adapter rather than left hanging. Auto-answered requests should still appear
in the conversation stream as resolved facts, not phantom prompts.

The full policy surface — an `ask` tier with first-class waiting-on-user
status, ADR 0018 notifications, inline web answering, an `allow-safe`
classification tier, per-session policy selection — is a **deferred slice**
(§4, "Later"). The protocol slot and the pending-action envelope design in
the fit report §4 are the blueprint when it comes due. No timeout ever
auto-answers a real `ask`: the caller's `wait --timeout` plus
`gmux agent cancel` (which answers pending requests as cancelled, per spec)
bound unattended stalls.

### 2.4 Internal wire and web UI: real ACP, official SDKs

The internal conversation feed is **real ACP v1**, not #388's dialect:

- The runner re-serves genuine ACP (duplex JSON-RPC; client-initiated
  `session/load`; verbatim `session/update` shapes) per viewer connection,
  proxied by gmuxd over WebSocket (content-free, per ADR 0011). Any
  existing ACP client can be pointed at a gmux session during development —
  the verification tool for the endpoint before our web code touches it.
- The browser holds a genuine `ClientSideConnection` from
  `@agentclientprotocol/sdk` (official TS SDK; Web-Streams core), pinned.
  The #388 island's accumulation store, assistant-ui renderer, and
  attachment composer carry over; its hand-written frame types,
  optimistic-echo machinery (authoritative `user_message_chunk` + tape
  replay make it redundant), and `adapter === "pi"`/`?conv` gating do not.
- The composer verb for ACP sessions is `session/prompt` (routed through
  the runner's serialization), never PTY `/input`.
- The web session body selects on **wire-carried drive mode and
  capabilities**, never on adapter name: ACP mode → conversation body (no
  PTY); pi terminal → terminal primary (conversation toggle later, when the
  pi facade lands).
- **Pi integration comes last** (fit report §6): a runner-side
  implementation of the SDK `Agent` interface over pi's extension events
  and JSONL retires the `/acp/ingest` dialect and unifies pi with the same
  feed. Pi's keystroke `/input` remains its mode-native write actuator.

### 2.5 Iteration identity must match PR #454

PR #454 defines the storage-side iteration unit: **one completed model
response**, grouped by Claude's `message.id` (legacy combined records = one
response each), with terminal prose = the final response's prose — replaced
even when empty — and injected-user envelopes (command/bash tags,
`isCompactSummary`) filtered from exchange boundaries. The ACP turn
assembler must match this unit so live wait reports agree with storage reads
(`agent logs`). ACP v1 chunks carry no message id, so live segmentation at
tool/thought boundaries is best-effort; the live gate must diff assembled
reports against the #454 storage projection for multi-tool, steered, and
cache-replay turns, and any residual divergence is documented as v1
best-effort (v2's `messageId` makes it exact).

### 2.6 Lineage and identity obligations (PR #453, ADR 0031/0024)

The ACP runner must carry the same session facts the PTY runner does:

- **Launch parent provenance**: inherit `GMUX_SESSION_ID` at driven launch
  into `ParentSessionID`, through runner state and `/meta`. Registration copies
  it into both organizational `parent_session_id` and write-once
  `launched_from_session_id`; resume/restart changes neither.
- **`semantic_agent`** derives from `adapter.ConversationSource` — Claude
  and Codex adapters already implement it, so ACP-mode sessions
  participate in family edges and child-notification suppression
  automatically. Verify at the live gate; #453's own live-gate fix was in
  exactly this propagation path.
- Standard id issuance/registration (ADR 0031), project membership
  (ADR 0024), titles and retention flow through the shared runner
  machinery unchanged.

### 2.7 Distribution, pinning, conformance

- **Exact adapter versions pinned in code** per gmux release
  (`claude-agent-acp`, `codex-acp`), plus pinned SDKs (`coder/acp-go-sdk`,
  `@agentclientprotocol/sdk`) — all four churn fast and their behavior is
  contract surface.
- **Tracer:** launch adapters via `npx -y <pkg>@<pin>`; node absent is a
  capability refusal naming the dependency. **Before release:** a
  gmux-managed install cache (`~/.local/share/gmux/acp/<pkg>@<ver>/`) with
  integrity pinning, mirroring the pi-ext materialization pattern.
- **Binary policy:** run the adapter-bundled Claude/Codex binaries
  (`CLAUDE_CODE_EXECUTABLE`/`CODEX_PATH` unset) — the adapter is tested
  against what it bundles. Config overrides exist but are
  unsupported-territory; ADR 0033 §6's version-skew warning applies at mode
  conversion.
- **Conformance smoke suite** (live-gated, env-opt-in like the pi e2e):
  initialize capability asserts, one prompt turn, steer injected +
  steer-on-idle, cancel → `cancelled`, resume identity, permission
  round-trip, and **storage-parser compatibility** (#448/#450/#454 parse
  what the adapter wrote — the ACP adapters write the same stores under an
  SDK-bundled binary). Runs on every adapter/SDK pin bump.

### 2.8 Launch surface and model resolution (ADR 0034 + settled details)

- The tracer launch surface is the **canonical spec on `--model`**:
  `model:effort@harness`, exact names, no resolver
  (`gmux agent prompt --new --model anthropic/claude-opus-5:low@claude …`).
  Drive mode is not part of the spec: driven launch uses the mode in which
  the harness supports it (ACP for claude/codex, terminal for pi). The
  `--adapter` flag is never introduced.
- **gmux records launch history — (model, harness, effort, timestamp) per
  driven launch — from day one** (slice 1), because ADR 0034's recency rung
  consumes it later.
- Resolver corpus per harness (settled with the user, not in ADR text):
  - **pi:** `enabledModels` patterns resolved via
    `AgentSession.scopedModels`, exposed through an available-models RPC
    keyed `provider/modelId`; pi's persisted `defaultModel`/
    `defaultThinkingLevel` seed recency before any gmux history exists.
  - **claude:** SDK `initializationResult.models` filtered by the
    `availableModels` settings allowlist. Acquisition (settled): **spawn
    the pinned adapter, run the `initialize` handshake, read the model
    list, kill it**; cache the result keyed on
    **(adapter version, settings file mtime)**. gmux parses Claude
    settings files **only as the `availableModels` allowlist filter**,
    never as the catalog source.
  - **codex:** App Server `model/list`, non-hidden vendor catalog.
- Launch-menu curation and a UI default-launch-mode are **deferred to their
  own small ADR**, kept separate from `preferred_harnesses`.

## 3. Decision register

The proposal's seven questions, restated against current reality:

| # | Question | Status | Answer / recommendation |
|---|---|---|---|
| 1 | Tracer launch surface | **RESOLVED** | Canonical `model:effort@harness` on `--model`, exact names, resolver later per ADR 0034. No `--adapter` flag, ever. |
| 2 | Fate of PR #388 | **RESOLVED** | Stays a **draft donor branch**; the dialect wire (`cli/gmux/internal/acp`, `/acp/ingest`, server-pushed `session/load`) is never merged. The island/renderer/attachment code is absorbed in slice 4, which closes #388. |
| 3 | Permission default | **RESOLVED (v1)** | Runner-side auto-allow (yolo) + native low-friction session mode; no form-elicitation capability advertised. The `ask`/`allow-safe` tiers, waiting-on-user status, notifications and web answering are one deferred slice; no timeout ever auto-answers. |
| 4 | Distribution/pinning | **OPEN (last one)** | The tracer path is fixed by the plan (npx-with-pin, §2.7); still needing a yes before release: the gmux-managed install cache and the adapter-bundled-binary default. Does not block slices 0–2. |
| 5 | `drive_mode` store axis now | **RESOLVED** | Mandated by ADR 0033 ("mode becomes a real session axis"); pre-release schema change, no migration (ADR 0026 clean-state policy). Store + `/meta` + wire + web (fit report §5 lists the missing wire facts). |
| 6 | Runner packaging | **RESOLVED** | Protocol layer is `coder/acp-go-sdk`. Shape: new **`acprunner` sibling package** sharing `session.State`, socket bind, registration and `/events` with `ptyserver`; `ptyserver` is not generalized. A shared conformance suite in CI covers both runner kinds; unification is revisited after slice 6 (§2.1). |
| 7 | Hardcoded tracer permission policy | **RESOLVED** | Subsumed by #3: auto-allow *is* the v1 policy, not a temporary hack. |

New decisions recorded (settled with the user, outside ADR text): resolver
corpus sources and cold-start seed (§2.8); launch-history recording from day
one (§2.8); launch-menu curation deferred to its own ADR; ACP-first/pi-last
ordering and official SDKs at both ends (§2.4); runner-as-multiplex-point
with session tape (§2.1).

**Settled at program approval** (2026-08-04, replacing the former open list):

1. **PR #388** stays a draft donor branch; its island is absorbed in
   slice 4 (register #2).
2. **Tape retention: none** — the harness's own storage is the source of
   truth; tapes are incarnation-local debug artifacts only (§2.1).
3. **Refusal copy** for PTY-less `tail`/`attach`/`send` and terminal-mode
   semantic verbs is delegated to the slice 0 implementation (direction
   fixed: refuse by name, stating the working surface; ADR 0033 §3 style).
4. **Claude model catalog**: spawn the pinned adapter, initialize handshake,
   read models, kill; cache keyed on (adapter version, settings mtime);
   settings parsing is allowlist-only (§2.8).
5. **Runner shape**: `acprunner` sibling package, no `ptyserver`
   generalization; shared conformance suite in CI; revisit unification
   after slice 6 (§2.1).
6. **Capabilities**: minimal on both faces — no fs/terminal capabilities
   advertised to the adapter or to browsers; every advertised capability
   must have a consumer in a shipped slice (§2.1).
7. **Protocol layer**: `coder/acp-go-sdk`, pinned (§2.1).
8. **Permissions v1**: runner-side auto-allow (§2.3).

**Genuinely remaining open decision:** distribution confirmation
(register #4) — the release-time gmux-managed adapter cache and the
adapter-bundled-binary default. Blocks release packaging, not slice work.

## 4. Slice plan

Ordering is dependency order; slices 3–4 (UI wire) can proceed in parallel
with slice 2. Each slice lands green on main with unit/integration tests;
**live gates** are env-opt-in e2e runs plus a manual gate note — the #438
lesson is that handshakes lie until a live gate runs.

**Slice 0 — mode axis + ADR 0033 refusals** *(S; no ACP code)*
`drive_mode` (`terminal`|`acp`) in the central store, registration, `/meta`,
and the session wire; capability checks key on (harness, mode); terminal
claude/codex semantic verbs emit ADR 0033 §3 refusals (steer permanent;
prompt/follow-up naming the ACP path); `ls`/web display the mode. The final
refusal copy — including the PTY-less `tail`/`attach`/`send` wording — is
decided and asserted in this slice.
*Exit:* refusal texts asserted in tests; mode visible end-to-end; no
behavior change for existing sessions.

**Slice 1 — ACP host runner + Claude tracer (prompt → wait end-to-end)** *(M–L)*
`acprunner` on `coder/acp-go-sdk`: spawn pinned `claude-agent-acp` via npx,
initialize → ready, `session/new` (cwd, low-friction mode), yolo permission
one-liner, `POST /prompt` (plain, require-inactive) → `session/prompt`, turn
assembler v1 (terminal prose + §2.5 iteration unit) → `CloseTurnFrame` with
§2.2 taxonomy; EOF mid-prompt → failed activity; conversation-ref assertion;
lineage facts (§2.6); launch via canonical `--model …@claude`; launch-history
recording. Session tape retained from day one (it feeds slices 3–5).
*Exit:* `gmux agent prompt --new --model …@claude 'x'` returns an exchange
report; `wait`/`agent logs` correct on live and runnerless sessions.
*Live gate (week one):* real prompt; multi-tool turn; report diffed against
the #454 storage projection; family edge + notification suppression
verified.

**Slice 2 — cancel, follow-up queue, steering** *(M)*
`session/cancel` → interrupted; runner-side serialization + follow-up queue
drained on idle; `--steer` via `_session/steering` gated on
`_meta.steering.supported`, `promptRequired` → "no activity in progress",
injections → `NoteInjection`.
*Exit:* all four verb modes behave per ADR 0027 preconditions.
*Live gate:* steer injection mid-turn; steer-on-idle; cancel latency;
queued follow-up becomes its own turn.

**Slice 3 — real-ACP re-serve endpoint + WS proxy** *(M)*
Runner agent-side facade per viewer connection serving tape replay
(`session/load`) then live updates; gmuxd WebSocket proxy (content-free);
viewer prompts route through the slice-2 serialization.
*Exit:* an **existing third-party ACP client** pointed at the proxy renders
a live gmux session, including a mid-turn join — verified before any gmux
web code changes.

**Slice 4 — web island on `@agentclientprotocol/sdk` + body selection** *(M)*
Browser `ClientSideConnection` over the proxy; keep #388's accumulation
store/assistant-ui renderer/attachments; authoritative `user_message_chunk`
replaces optimistic echoes; composer → `session/prompt`; body selection on
wire mode/capabilities (kills `adapter === "pi"` and `?conv`). Auto-allowed
permissions render as resolved facts.
*Exit:* ACP session body is the conversation view end-to-end; #388
disposition executed (register #2).

**Slice 5 — resume + collection** *(M)*
Transparent resume of runnerless ACP sessions under the same gmux id via
`session/load` through the existing `RunnerSpawner`/lifecycle-claim
machinery (mode-aware spawn argv); startup convergence rules unchanged;
runner collection enabled for idle ACP sessions.
*Live gate:* kill runner while idle → prompt again → same identity, history
intact, family edges preserved; resume replay fidelity.

**Slice 6 — Codex + conformance suite** *(M)*
Second implementation of the ACP drive seam (`codex-acp` pin; quirk table:
no native queue, no `promptRequired`, `startedNewTurn` honesty; threadId ref
mapping). The §2.7 conformance suite lands here and runs against both pins;
pin-bump procedure documented.
*Exit:* full verb set on Codex; suite green on both adapters.

**Slice 7 — model catalogs + resolver (ADR 0034)** *(M–L)*
Adapter catalog capability (pi scoped-models RPC; Claude allowlist per the
open sub-choice; Codex `model/list` non-hidden), usability filtering,
resolution ladder (whole-token, `preferred_harnesses` before recency,
echo + freeze), pi cold-start recency seed; web model picker consumes the
same capability.
*Exit:* shorthand launches resolve deterministically or fail listing
canonical candidates; echo printed and frozen in session state.

**Slice 8 — tool lifecycle + auxiliary surfaces** *(M)*
Structured ACP tool content (diffs, locations, terminal embeds) in store and
renderer with tolerant fallback; plan/mode/config/commands projections and
small UI. Unknown updates remain safely ignored throughout.

**Slice 9 — reconnect + cross-host parity** *(M)*
Backoff/re-attach via tape replay; peer proxying of the conversation WS
(today's 501); multi-client handoff tests (two browsers + one CLI waiter on
one active turn).

**Slice 10 — pi facade** *(L; deliberately last)*
Runner-side SDK `Agent` implementation over pi extension events + JSONL
(minted message ids, authoritative user boundaries, tool lifecycle); retires
the `/acp/ingest` dialect and #388's remaining transport; pi keeps keystroke
`/input` as its write actuator. Pi works via its PTY the whole time
meanwhile.

**Later (post-program, each its own design):** explicit mode relaunch /
`--convert` (ADR 0033 §6); permission policy surface (`ask`/`allow-safe`,
waiting-on-user status, ADR 0018 notifications, inline answering — fit
report §4 is the blueprint); launch-menu curation + UI default-launch-mode
ADR; ACP v2 mapping swap inside the turn assembler.

## 5. Risk register (deltas from the spike's §6)

Unchanged and still governing: adapter churn as contract surface (pin +
conformance suite), v1→v2 transition (one assembler seam), `_session/steering`
extension risk (capability-gated), follow-up asymmetry (uniform runner
queue), Claude binary skew (bundled-binary default + parser regression
tests), v1 result-assembly fidelity (live-gate diff vs #454), permission
deadlocks (yolo v1 sidesteps; returns with the `ask` tier), untested live
flows (per-slice live gates). New since the spike: **four** pinned upstream
artifacts instead of two (both SDKs added) — same mitigation, one pin-bump
procedure covering all of them.
