# ADR 0034: driven-launch model resolution

**Status:** Accepted
**Date:** 2026-08-02
**Related:** ADR 0027 (semantic agent CLI), ADR 0029 (agent sessions abstract runner residency), ADR 0033 (session backends, capability boundaries, and canonical session spec)

## Context

ADR 0033 defines `model:effort@harness` as the canonical session spec for
driven launches and reserves shorthand resolution for this ADR. Callers want
"the latest fable" to work without maintaining an alias per release; that
convenience must not rest on arbitrary fuzzy matching.

Pi's current behavior shows the failure: it picks the first fuzzy match across
every model it knows, so `fable` can resolve to `amazon-bedrock/fable` — a
provider the user never configured — and the launch fails. The corpus was
wrong, not the shorthand idea.

Resolution is host-dependent (installed harnesses and configured providers
differ per host), and the same catalog data must drive the web UI model
picker.

## Decision

### Scope

Resolution applies only where gmux chooses the launch command: driven
launches (`gmux agent prompt --new --model <spec>`) and the web UI
new-session flow. It runs daemon-side at launch time against the target
host. Interactive `gmux -- <harness>` is untouched.

### Candidate corpus: usable models only

Shorthand resolution considers only models that are **usable** on the target
host at resolution time: harness installed, provider configured and
authenticated, model not deprecated. Unconfigured providers drop out before
matching — the structural fix for the Bedrock failure.

The corpus comes from a new adapter capability: a **model catalog** carrying
canonical model identity, usability status, version ordering (release date or
explicit), model-family grouping, and adapter-specific selection facts such as
pi's scoped/featured flag. (Feasible today: Codex exposes `model/list`; Claude
exposes model and effort as config options.) An adapter without a catalog
degrades gracefully: its models resolve only via exact canonical spec and
contribute no shorthand candidates. The same catalogs feed the web model
picker, so picker visibility and shorthand eligibility share one source of
truth.

### Resolution ladder

Deterministic; each rung produces a trustworthy result, fails explicitly, or
advances — never an ambiguous guess.

1. **Exact canonical form** (`anthropic/claude-fable-5:low@pi`) is used
   verbatim, with no inference. Ordinary launch validation still applies.
2. **Unique whole-token match** over the usable corpus: the shorthand must
   equal a complete token of the model name (`sol` matches `gpt-5.6-sol`, not
   `solar`). This is **partial-name specification, not fuzzy matching** — no
   substring, prefix, or edit-distance search.
   - Matches within one model family → pick the latest by catalog version
     ordering, so `fable` tracks new releases with zero maintenance.
   - Same family launchable through multiple harnesses →
     `preferred_harnesses` order wins.
   - Matches spanning distinct families → fail, listing the canonical
     candidates. An agent retries cheaply with a longer token.
3. **History** has exactly two jobs: default thinking effort for the resolved
   model (its most recent launch) and tiebreak among otherwise-equal
   candidates (recency, never frequency). History never introduces a
   candidate the catalog did not offer.
4. **Echo and freeze**: the resolved canonical form is printed at launch and
   recorded in the session's durable state. Nothing ever re-resolves.

A shorthand reaching no usable candidate fails with an error asking for a
model. There is no implicit default model.

### Configuration

One new key: `preferred_harnesses`, an ordered preference shipped with a sane
default covering all supported harnesses, so zero-config works — unavailable
harnesses drop out of the corpus naturally. A future `default_model` may be
added; initially an underspecified launch fails and asks.

### Stability convention

Shorthand is a human/agent ergonomic affordance; its meaning legitimately
advances when a new family member releases. The echo makes that visible and
the frozen record makes it auditable. Automation needing reproducibility uses
the canonical form.

## Consequences

- Resolution happens on the target host, so its actual installation and
  credentials govern availability in multi-host launches.
- Echo + freeze are required contract, not diagnostics: a shorthand may
  resolve differently across time and hosts by design.
- Family shorthand correctness depends on accurate family identity and
  version ordering in catalogs (see open questions).
- CLI resolution and the web picker consume one catalog capability and cannot
  disagree about what is launchable.

## Rejected alternatives

- **User-maintained aliases** — drift, require an edit per release, and fail
  the "latest fable without config" requirement.
- **Auto-populated preferences from usage** — self-modifying configuration
  the user never wrote; history stays runtime data with its two narrow jobs.
- **Most-common-usage ranking** — frequency entrenches old choices; recency
  only, as tiebreaker.
- **Silent cross-harness or cross-provider fallback** — changes capabilities,
  configuration, and privacy semantics; fail with candidates instead.
- **Substring/edit-distance fuzzy matching** — admits accidental neighbors;
  whole-token equality has a stable, explainable boundary.
- **History expanding the catalog** — past use is not evidence of present
  launchability.

## Open questions

- **Cross-harness model-family identity** — the one real design risk: "latest
  fable" requires knowing fable-5 and fable-6 are one family, including when
  different harnesses expose them. Declared per adapter, reconciled centrally,
  or inferred from naming?
- **Catalog capability wire shape**: framing, refresh/invalidation, and how
  pi's scoped-model list maps to usability and ranking.
- Whether this resolver also powers `gmux agent prompt` on existing sessions
  that permit changing model; this ADR governs launch selection only.
