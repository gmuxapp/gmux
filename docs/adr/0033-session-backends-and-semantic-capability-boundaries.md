# ADR 0033: session backends and semantic capability boundaries

**Status:** Accepted
**Date:** 2026-08-02
**Related:** ADR 0009 (verb-first CLI), ADR 0013 (codex authoritative state via hooks), ADR 0015 (hook translation at the agent side), ADR 0021 (ACP as the normalized conversation schema), ADR 0023 (unified turn model), ADR 0027 (semantic agent CLI and result-bearing wait), ADR 0028 (CLI output channels), ADR 0029 (agent sessions abstract runner residency), ADR 0030 (exchange-oriented reads and observational wait), ADR 0031 (session ID issuance), ADR 0032 (session input doors)
**Amends:** ADR 0027 §1 (the "Claude/Codex support are follow-ups" clause — the follow-up is an ACP backend, not the terminal path)

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

Meanwhile ADR 0021 already anticipated ACP-native, terminal-less adapters,
and draft PR #388 holds the gmux-native conversation UI seam (streaming
text/thinking/tool-calls). ACP is the interface Claude and Codex *do* offer
for automation: structured prompts, streaming updates, tool calls, permission
requests, and cancellation — asserted facts, not scraped ones.

## Decision

### 1. Capabilities attach to the session backend, not the harness name

A session's capability set is determined by **backend** — the transport and
contract through which gmux hosts the agent — not by which agent runs inside.
"A Claude session" is not one thing: a Claude **terminal** session and a
Claude **ACP** session are different session kinds with different capability
sets, different configuration surfaces, and potentially different identity
and lifecycle semantics. Adapter names stop implying capabilities; the
backend a session was created on decides what gmux may claim about it.

Two backends exist under this ADR:

- **Terminal backend** — the existing PTY session: a real process on a
  durable PTY, attachable, raw-sendable, tailable, with whatever
  *observational* facts the adapter's hook/extension and native storage
  trustworthily provide (ADR 0011/0013/0015). Semantic control on this
  backend requires the ADR 0027 contract to be source-asserted, which today
  only pi's in-repo extension does.
- **ACP backend (planned)** — a runner speaking the Agent Client Protocol to
  a terminal-less agent process: structured prompt turns, streaming
  `session/update`, tool calls, permission requests, cancellation, and
  stable completion boundaries, normalized per ADR 0021 and rendered in
  gmux-native UI (PR #388 is the seam). No PTY exists; there is nothing to
  attach to and no screen to tail.

### 2. Claude and Codex terminal sessions are interactive-only

`gmux -- claude` and `gmux -- codex` remain plain interactive terminal
sessions. They keep everything the terminal backend honestly provides:
launch, attach, raw `send`, `tail`, hook-driven active/idle status, titles,
attribution, retention, resume-by-relaunch, and observational transcript
reads from native storage where implemented. They gain no semantic control:
`gmux agent prompt` (including `--follow-up`/`--steer`), `gmux agent cancel`,
and `gmux agent prompt --new --adapter claude|codex` refuse these sessions.

`gmux wait` remains universal (ADR 0027 §11): it synchronizes on any
session's activity, including hook-observed Claude/Codex turns, with the
fidelity the hooks actually provide (Codex's outcome-less `Stop` stays
coarse, per ADR 0013). It is result-bearing only where the backend asserts
results; on Claude/Codex terminal sessions it never fabricates an exchange
report from a screen.

### 3. Refusal is explicit, permanent-shaped, and names the working path

A semantic action against a terminal session whose adapter does not
source-assert the contract fails **before any delivery**, with an error that
names the backend boundary rather than a transient condition:

```text
gmux: claude terminal sessions are interactive-only; semantic control
requires a Claude ACP session (not yet available). Use gmux send / gmux tail
to drive this session, or gmux attach to take over.
```

The refusal is a capability fact in the `unsupported_adapter` family of the
established error taxonomy (ADR 0027's 2026-07-27 amendment): checked and
reported before readiness, activity, or residency, because "this session
kind cannot do that" is permanent and actionable while the others are
transient. It never degrades to raw input — a semantic verb must not
silently become keystrokes (ADR 0027 §3's loud-failure rule applies at this
boundary too). Once the ACP backend exists, the parenthetical points at how
to create the capable session kind instead.

### 4. Claude/Codex semantic control is delivered on the ACP backend

The ADR 0027–0030 contract for Claude and Codex — launch, prompt, follow-up,
steer, cancel, result-bearing wait, exchange logs — is implemented by the
upcoming ACP runner, where readiness is protocol initialization, submission
is a `session/prompt` request, completion is a protocol response, results
arrive as structured updates, and permission requests are first-class events
rather than key-stealing dialogs. ADR 0027 §4's forward reference ("future
ACP-native runners set ready after protocol initialization") becomes the
plan of record for these harnesses.

An ACP session is **not a pretend terminal**. gmux does not synthesize a PTY
around it, `attach` and `tail` do not apply, and its UI is the gmux-native
conversation view. Where the semantic contract and ACP's expressiveness
disagree, the resolution is designed in the ACP runner work with honest
degradation (decision 7's open questions), never by falling back to terminal
emulation.

### 5. Pi remains terminal-first with full semantic control

Pi keeps its terminal backend and its complete semantic capability set. This
is the rule of decision 1 applied, not an exception to it: pi's terminal
*is* part of its product value — extensions, custom renderers, direct human
intervention mid-session — and the in-repo extension gives gmux both sides
of the interface, which is exactly the condition under which a terminal can
source-assert the contract. No pi ACP session is planned; nothing here
precludes one later, as its own session kind under the same rule.

### 6. The product rule

**Use the native terminal when the terminal is part of the agent's value;
use ACP when the terminal is merely a UI around an automatable agent.** For
pi the terminal is the product surface. For Claude and Codex the terminal is
one client of an agent that offers a structured protocol for exactly the
control gmux wants; automating the human client instead of speaking the
protocol was the category error both PRs paid for.

### 7. Capability matrix

Capabilities by harness × backend. "Refused" is decision 3's explicit error;
"n/a" means the surface does not exist on that backend.

| Capability | pi · terminal | claude · terminal | codex · terminal | claude/codex · ACP (planned) |
|---|---|---|---|---|
| launch (`gmux --` / `-d` / launcher) | yes | yes | yes | yes (ACP runner spawn) |
| attach / raw `send` / `tail` | yes | yes | yes | n/a (no PTY; gmux-native UI) |
| hook/event status (active/idle, error) | yes (extension) | yes (hooks) | yes (hooks ≥ 0.135, coarse Stop) | yes (protocol events) |
| titles, attribution, retention, resume-by-relaunch | yes | yes | yes | per ACP session identity (open) |
| observational transcript reads (`agent logs`) | yes | salvage pending (see Consequences) | salvage pending | yes (normalized updates/storage) |
| `gmux wait` synchronization | yes | yes (hook fidelity) | yes (hook fidelity, outcome-less Stop) | yes |
| result-bearing wait / exchange report | yes | refused / no result claim | refused / no result claim | yes |
| `agent prompt` (plain / `--follow-up` / `--steer`) | yes | refused | refused | yes (steer expressibility open) |
| `agent cancel` | yes | refused | refused | yes (`session/cancel`) |
| `agent prompt --new` | yes | refused | refused | yes |

### 8. Non-goals

- **No live backend switching.** A session is created on one backend and
  stays there for its lifetime. There is no "upgrade this terminal session
  to ACP" or the reverse.
- **No transparent conversion.** A terminal conversation and an ACP
  conversation may differ in identity, configuration (settings/MCP/plugins
  loading), and lifecycle semantics; gmux does not claim that continuing one
  as the other preserves meaning, and does not attempt it.
- **No pretend PTY for ACP sessions**, and no revival of TUI emulation for
  Claude/Codex under any flag.
- **No inferred semantics on the terminal backend.** PTY output is
  presentation data (ADR 0027 §9); no amount of screen parsing promotes a
  terminal session into a semantically controllable one.

## Consequences

- **Claude/Codex semantic control is consciously blocked on the ACP backend
  landing.** We prefer the correct feature when ACP lands over a fragile TUI
  emulator now. The two-step interactive route (`gmux -d -- claude`, then
  `gmux send`/attach) keeps working throughout, as does hook-driven status.
- **Salvage: the observational work survives.** PR #438's review-hardened
  transcript pieces — parent-linked active-branch reconstruction with
  sidechain/meta privacy filtering, image-only boundaries, non-destructive
  settings/hook injection — and PR #439's rollout JSONL renderer are
  extraction candidates for follow-up PRs *without semantic-control claims*:
  they serve `agent logs`-style reads and status fidelity on the terminal
  backend, where storage parsing is trustworthy even though driving the TUI
  is not.
- **ADR 0027 §1 is amended.** "Peer forwarding and Claude/Codex support are
  follow-ups" now reads with this ADR's split: the follow-up for Claude/Codex
  is the ACP backend, and the terminal backend never grows their semantic
  verbs. ADR 0027's 2026-07-28 adapter-contract clause ("claude/codex do not
  become result-bearing") is confirmed and made permanent for the terminal
  backend. No other existing doc was found claiming terminal semantic
  support for Claude/Codex: the website adapter/integration pages describe
  only observational hooks, status, titles, and resume, which remain true.
- **Session kinds become a real axis.** Stores, the CLI, and the web UI will
  eventually need to distinguish a Claude terminal session from a Claude ACP
  session (naming, launchers, capability checks). That design belongs to the
  ACP runner work; this ADR fixes only that the axis exists and that
  capability checks key on it.
- **Error-message wording gains a second boundary.** Alongside ADR 0029's
  activity-vocabulary rule, semantic refusals on terminal Claude/Codex name
  the backend, not a missing feature flag or a "dead" anything.

## Open questions deferred to the ACP implementation

An ACP contract spike is running in parallel (`.grove/acp-contract-spike/`),
building a coverage matrix of the ADR 0027–0032 contract against the ACP
spec, `claude-code-acp`, and Codex's ACP support. Its findings are **pending
input** to the ACP runner design; this ADR does not wait for them. Deferred:

- **Steer vs follow-up expressibility.** Whether ACP can distinguish
  mid-turn steering from a queued follow-up per adapter, and the honest
  degradation if not (refuse `--steer`? cancel-and-reprompt with the
  semantics stated?).
- **Result boundaries and taxonomy mapping.** Whether each adapter's turn
  completion reliably carries final content, and how
  cancellation/API-error/process-death map onto ADR 0027 §8's
  completed/interrupted/error.
- **Waiting-on-user.** Whether permission requests and user questions are
  distinguishable from active work well enough to surface as a first-class
  state.
- **Resume and identity.** `session/load` support per adapter, identity
  stability across gmux restarts, and how ACP session identity composes with
  ADR 0029's resumable-conversation handle.
- **Configuration parity.** How close an ACP session's settings/MCP/plugin
  surface comes to the interactive tool, and how the difference is stated to
  users (decision 8's no-transparent-conversion rule is the floor).
- **Privacy.** Whether hidden reasoning is excluded or marked in updates,
  and how that composes with ADR 0030's rendering rules.

## Alternatives considered

### Keep hardening the terminal automation path

Rejected. Both PRs demonstrated the failure is structural, not residual: for
Claude every fix (readiness delays, focus/keybinding assumptions,
modal handling) replaced one unverifiable timing assumption with another,
against an interface whose owner may change any of it in a patch release;
for Codex the required facts (readiness, turn identity, results) do not
exist in the interface at all. A contract built on those foundations would
be ADR 0027's loud-failure principle inverted — quiet lies instead of loud
errors.

### Headless one-shot modes (`claude -p`, `codex exec`) as the semantic backend

Rejected as the backend. One-shot invocations discard the durable session:
no follow-up queue, no steering, no mid-turn observation, no
permission-request surface, and a fresh process per prompt. ACP is the
structured, session-holding version of the same idea and is what these
vendors maintain for programmatic clients.

### Wait for upstream hooks to grow readiness/outcome facts

Rejected as a plan. Hook vocabularies are upstream property with no
committed timeline, and even a complete hook set still leaves *delivery*
running through the TUI's focus/modal/keybinding surface — hooks fix
observation, not control. Hooks remain the terminal backend's observational
channel; if they improve, observation improves, and nothing here changes.

### Attach capabilities to the adapter with per-verb feature flags

Rejected. It encodes the false premise that "a Claude session" is one thing
with a feature list, and invites exactly the drift this ADR closes: an
adapter flag flipped on for a backend that cannot honor it. The backend is
the unit that has a contract; capabilities follow it.

### Refuse Claude/Codex semantic verbs silently or degrade to raw send

Rejected outright. Degrading a semantic verb to keystrokes is the
old-runner hazard of ADR 0027 §3 reintroduced deliberately; silent refusal
teaches callers nothing. The error names the boundary and the working path.
