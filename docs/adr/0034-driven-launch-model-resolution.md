# ADR 0034: driven-launch model resolution

**Status:** Accepted as design; **not implemented** — see [Implementation status](#implementation-status)
**Date:** 2026-08-02
**Related:** ADR 0027 (semantic agent CLI), ADR 0029 (agent sessions abstract runner residency), ADR 0033 (session drive modes, capability boundaries, and canonical session spec)

## Implementation status

**As of 2.1 none of the resolution described below ships.** The decision is
accepted and written in the present tense throughout, as ADRs here are; it
lands with the model-catalog slice of the ACP host program
(`docs/acp-host-program.md` §2.8 and slice 7). What ships today is only the
launch surface the resolver will later sit behind:

- `gmux agent prompt --new --model <M>` launches **pi** and hands `<M>` to
  pi's own `--model` flag verbatim (`agentLaunchAdapter` in
  `cli/gmux/cmd/gmux/agent.go`, `Pi.LaunchCommand` in
  `packages/adapter/adapters/pi.go`). gmux does not parse, normalize, or
  validate the value: `model:effort@harness` is not recognized as a grammar,
  `@harness` selects nothing, and `:effort` is not split off. An unusable
  value fails inside pi, after the session exists — not in gmux.
- There is **no candidate corpus**: no adapter model-catalog capability, no
  installed/authenticated filtering, and no web model picker to share it
  with (the web new-session flow has no model field).
- **No rung of the ladder exists**: no whole-token matching, no
  `preferred_harnesses` key (`host.toml` is strictly validated, so writing
  it is an error today), no launch history, no recency tiebreak, no effort
  default, no echo of a resolved canonical form. The launch argv is recorded
  on the session, which is the only sense in which the selection is frozen.

Stability, per the 2.x covenant (PR #507, `reference/stability.md`): the
`--model` **flag** is covenanted, its **value grammar is adapter-owned and
experimental**. Everything below — the spec grammar, the ladder, the
`preferred_harnesses` key, and the echo/freeze contract — is future work and
carries no compatibility promise until it ships and the stability reference
says so.

## Context

ADR 0033 defines `model:effort@harness` as the canonical session spec for
driven launches and reserves shorthand resolution for this ADR. Callers want
`fable` to work as a shorthand without maintaining aliases; that convenience
must not rest on arbitrary fuzzy matching.

Pi's current behavior shows the failure: it picks the first fuzzy match
across every model it knows, so `fable` can resolve to
`amazon-bedrock/fable` — a provider the user never configured — and the
launch fails. The corpus was wrong, not the shorthand idea.

Each harness already maintains a user-facing notion of which models its own
picker offers: pi's scoped-models list (`enabledModels`), Claude's
`availableModels` settings allowlist over the SDK model list, Codex's
non-hidden `model/list` catalog. gmux should resolve against that, not
against a parallel model universe of its own. Resolution is host-dependent
(installed harnesses and configured providers differ per host), and the same
data must drive the web UI model picker.

## Decision

### Scope

*(Design, not current behavior — see [Implementation status](#implementation-status).)*

Resolution applies only where gmux chooses the launch command: driven
launches (`gmux agent prompt --new --model <spec>`) and the web UI
new-session flow. It runs daemon-side at launch time against the target
host. Interactive `gmux -- <harness>` is untouched.

### Candidate corpus: the harness's own in-scope, usable models

Shorthand resolution considers only models that are, on the target host at
resolution time:

- **in scope for their harness** — the list the harness's own picker would
  show, as the user configured it in the harness itself (pi's scoped models;
  Claude's `availableModels` allowlist; Codex's non-hidden catalog); and
- **usable** — harness installed, provider configured and authenticated.

Unconfigured providers drop out before matching — the structural fix for the
Bedrock failure. gmux introduces no model-curation surface of its own; scope
is edited where it already lives, in each harness.

The corpus comes from a new adapter capability: report the harness's
in-scope model list with canonical identity (`provider/model` where the
harness has providers), usability status, and available effort levels. An
adapter without this capability degrades gracefully: its models resolve only
via exact canonical spec and contribute no shorthand candidates. The same
lists feed the web model picker, so picker visibility and shorthand
eligibility share one source of truth.

### Resolution ladder

Deterministic; each rung produces a trustworthy result, fails explicitly, or
advances — never an ambiguous guess.

1. **Exact canonical form** (`anthropic/claude-fable-5:low@pi`) is used
   verbatim, with no inference. Ordinary launch validation still applies.
2. **Whole-token match** over the corpus: the shorthand must equal a
   complete token of the model name (`sol` matches `gpt-5.6-sol`, not
   `solar`). This is **partial-name specification, not fuzzy matching** — no
   substring, prefix, or edit-distance search. A unique match resolves.
3. **Harness preference narrows first**: when matches span multiple
   harnesses (the same model is often in scope in several), only the
   matches from the highest-ranked harness in `preferred_harnesses` remain.
   The shorthand never launches via a lower-ranked harness while a
   higher-ranked one offers a match — use canonical `@harness` to override.
4. **Recency selects among the remaining matches**: the one most recently
   launched through gmux wins. With no usable history and multiple
   remaining matches, fail listing the canonical candidates — an agent
   retries cheaply with a longer token. History never introduces a
   candidate the corpus did not offer.
5. **Effort default**: when effort is omitted, use the resolved model's most
   recent gmux launch effort; otherwise the harness default.
6. **Echo and freeze**: the resolved canonical form is printed at launch and
   recorded in the session's durable state. Nothing ever re-resolves.

A shorthand reaching no candidate fails with an error asking for a model.
There is no implicit default model.

### Configuration

One new key: `preferred_harnesses` (not yet present in `host.toml`), the
ordered harness preference of rung 3, to ship with a sane default covering
all supported harnesses so zero-config works. It is per-user intent: one user routes shorthand `fable`
via pi, another via claude, each without canonical specs. A future `default_model` may be added;
initially an underspecified launch fails and asks.

### Stability convention

Shorthand is a human/agent ergonomic affordance: `fable` means "the fable I
last used, via my preferred harness that offers it", and switches to a new
release only when the caller picks it once (canonically or via the picker). The echo makes each resolution visible and
the frozen record makes it auditable. Automation needing reproducibility
uses the canonical form.

## Consequences

- Resolution happens on the target host, so its actual installation,
  credentials, and harness-side scope govern availability in multi-host
  launches.
- Echo + freeze are required contract, not diagnostics: a shorthand may
  resolve differently across time and hosts by design.
- Selection needs no model-family identity or version-ordering knowledge:
  recency replaces "latest within a family". A newly released model is
  adopted by using it once, not automatically.
- CLI resolution and the web picker consume one adapter capability and
  cannot disagree about what is launchable.
- gmux launch history becomes resolution input and must be recorded per
  (model, harness) with effort.

## Rejected alternatives

- **User-maintained aliases in gmux** — a second curation surface that
  drifts from the harness-side scope; scope plus recency covers the need.
- **Family/version machinery ("latest fable")** — requires cross-harness
  model-family identity and version ordering that no harness reports today;
  recency gives a simpler, more predictable meaning with zero new metadata.
- **Auto-populated preferences from usage** — self-modifying configuration
  the user never wrote; history stays runtime data.
- **Most-common-usage ranking** — frequency entrenches old choices; recency
  only.
- **Silent cross-harness or cross-provider fallback** — changes
  capabilities, configuration, and privacy semantics; fail with candidates
  instead.
- **Substring/edit-distance fuzzy matching** — admits accidental neighbors;
  whole-token equality has a stable, explainable boundary.
- **History expanding the corpus** — past use is not evidence of present
  scope or launchability.

## Open questions

- **Adapter capability wire shape**: framing, refresh/invalidation, and the
  per-harness mapping (pi exposes scoped models via RPC; Claude's list
  requires the SDK/adapter or reading its settings; Codex's `model/list`
  needs `hidden` filtering).
- **Adapter-side recency as a cold-start hint**: pi persists a single
  last-used model/effort default; whether adapters should report it to seed
  resolution before any gmux history exists.
- Whether this resolver also powers `gmux agent prompt` on existing sessions
  that permit changing model; this ADR governs launch selection only.
