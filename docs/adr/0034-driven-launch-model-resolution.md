# ADR 0034: driven-launch model resolution

**Status:** Accepted
**Date:** 2026-08-02
**Related:** ADR 0027 (semantic agent CLI), ADR 0029 (agent sessions abstract runner residency), ADR 0033 (session backends, capability boundaries, and canonical session spec)

## Context

ADR 0033 defines `model:effort@harness` as the canonical session spec for
driven launches and reserves shorthand resolution for this ADR. A caller should
be able to ask for the current member of a model family without maintaining an
alias every time a provider releases a version. That convenience must not make
a launch depend on an arbitrary fuzzy match.

Pi's current matching demonstrates the failure. It selects the first fuzzy
match across every model it knows, so `fable` can resolve to
`amazon-bedrock/fable` even when the user has never configured Amazon Bedrock.
The resulting launch fails. The shorthand is not the underlying problem: the
candidate corpus admitted unusable models, and the match had no trustworthy
ambiguity boundary.

Resolution is also host-dependent. A driven launch may target another host,
where installed harnesses and configured providers differ from the caller's.
The same catalog data is needed by the web UI's new-session model picker; the
CLI and UI must not invent separate availability rules.

## Decision

### Scope

Resolution applies only when gmux chooses the launch command: semantic/driven
launches, including `gmux agent prompt --new`, and the web UI new-session flow.
It runs daemon-side at launch time against the target host.

Interactive passthrough launch, `gmux -- <harness>`, is unchanged. gmux does not
reinterpret a command the user supplied.

### Candidate corpus

Shorthand resolution considers only **usable** catalog entries. A model is
usable when, on the target host at resolution time:

- the backend that can launch it is installed;
- its provider is configured and authenticated; and
- the model is not deprecated.

Unavailable backends and providers therefore disappear before matching. This
is the structural correction for the Bedrock failure: a provider the user
cannot launch can never win shorthand resolution.

Each backend capable of discovery advertises a model catalog containing:

- canonical model identity;
- usability status on that host;
- version ordering, by release date or an explicit backend ordering;
- model-family grouping;
- the harness/drive information needed to launch it; and
- backend-specific selection facts, including pi's scoped/featured marker
  where applicable.

Codex's `model/list` and Claude's model and effort config options establish that
a catalog capability is feasible, but do not fix its wire shape. A backend
that cannot provide a catalog degrades safely: its models remain launchable by
exact canonical spec, but the backend contributes no candidates to token or
history-based resolution. The same advertised catalogs feed the web model
picker, so picker visibility and shorthand eligibility share one source of
truth.

### Deterministic resolution ladder

The resolver evaluates the following ladder in order. Each rung either produces
a trustworthy result, fails explicitly, or advances; it never falls through
after making an ambiguous choice.

1. **Exact canonical form.** A complete spec such as
   `anthropic/claude-fable-5:low@pi` is accepted verbatim. gmux performs no
   model, effort, or harness inference. Ordinary launch validation can still
   report that its explicitly named backend or credentials are unavailable.
2. **Unique whole-token match over usable models.** A shorthand must equal a
   complete token in a usable model name. Token boundaries are punctuation
   boundaries in the catalog identity: `sol` matches `gpt-5.6-sol`, but does
   not match `solar`. This is **partial-name specification, not fuzzy
   matching**: there is no substring, prefix, edit-distance, or similarity
   search.
   - If all matches are members of one declared model family, select the
     latest member by catalog version ordering. Thus `fable` advances from
     fable 5 to fable 6 without an alias edit.
   - If that family is available through multiple otherwise-equivalent launch
     backends, select according to `preferred_backends`.
   - If matches span distinct families, fail and list the canonical candidates.
     gmux never guesses across families. A human can choose; an agent can retry
     cheaply with another token or a canonical spec.
3. **History as a constrained tiebreak and effort default.** Usage history has
   exactly two responsibilities: it supplies the most recently used thinking
   effort for the resolved model when effort was omitted, and breaks a tie
   among candidates that remain otherwise equal. The most recent applicable
   launch wins; frequency is irrelevant. History never introduces a candidate
   absent from the current usable catalog and never overrides a family or
   backend preference decision.
4. **Echo and freeze.** Before launch, gmux prints the fully resolved canonical
   `model:effort@harness` form. It records that same value in session metadata.
   The session is thereafter bound to that value: resume, reads, and later
   operations never re-run shorthand resolution.

If the supplied shorthand reaches no usable candidate, resolution fails with an
error asking for a model and, where useful, naming discoverable choices. There
is no implicit global default model in this decision.

### Configuration

The only new configuration is `preferred_backends`, an ordered preference used
when the same family has otherwise-equivalent usable launch choices. gmux ships
a sane order covering every supported choice, so zero-config resolution works;
choices unavailable on the target host naturally drop out with the corpus.

A future `default_model` may remove the need to supply any model. It is omitted
initially: an underspecified launch fails and asks for a model rather than
silently acquiring a product-wide default.

### Stability convention

Shorthand is a human and agent ergonomic affordance, not a reproducibility
mechanism. Its meaning may intentionally advance when a new family member is
released. The launch echo makes that choice visible and session metadata makes
it auditable and permanent for that session. Automation requiring a stable
choice uses the canonical form.

## Consequences

- Unconfigured providers cannot capture shorthand matches. Catalog usability,
  rather than match ranking, enforces that property.
- Family shorthand follows new releases without user-maintained configuration.
  This depends on accurate family identity and version order from catalogs.
- Ambiguity is cheap and explicit. Distinct-family matches return candidates
  instead of embedding an unstable heuristic in a launch.
- Multi-host launches resolve where they execute, so the target host's actual
  installation and credentials govern availability.
- CLI launches and the web picker consume the same catalog capability and
  should agree about what can be launched.
- A shorthand may resolve differently across time or hosts by design. The
  canonical launch echo and frozen metadata are therefore required contract,
  not optional diagnostics.
- Backends without discovery support remain usable through canonical specs and
  do not weaken resolution for other backends.

## Rejected alternatives

### User-maintained aliases

Rejected. Aliases drift, require maintenance for every release, and fail the
core requirement that “latest fable” keep working without a config edit. A
canonical form already serves users who want a pinned name.

### Auto-populate preference configuration from usage

Rejected. This would create self-modifying configuration the user never wrote.
History remains runtime data with the two narrow responsibilities above;
configuration remains an explicit statement of preference.

### Most-common-usage ranking

Rejected. Frequency entrenches old choices and makes outcomes depend on an
opaque lifetime count. Only recency may break an otherwise-equal tie or default
effort.

### Silent cross-backend or cross-provider fallback

Rejected. A different backend or provider can change capabilities,
configuration, billing, and privacy boundaries. Preference may choose among
catalog-declared equivalent offerings; gmux does not silently reinterpret an
unavailable explicit choice or cross an ambiguity boundary. It fails and lists
candidates.

### Substring or edit-distance fuzzy matching

Rejected. These searches admit accidental neighbors and make the winner depend
on catalog contents and ordering. Whole-token equality has a stable,
explainable boundary.

### Let history expand the catalog

Rejected. A previously usable model may be removed, deprecated, logged out, or
unavailable on the target host. Historical use is not evidence of present
launchability.

## Open questions

- **Cross-backend family identity:** is family grouping declared by each
  backend, reconciled in a shared registry, or inferred? Naming inference is a
  material design risk: selecting the latest fable requires knowing that fable
  5 and fable 6 belong to one family, including when different backends expose
  them.
- **Catalog wire shape:** request/response framing, refresh and invalidation,
  authentication failure representation, version-order fields, and capability
  negotiation remain implementation work.
- **Pi scoping:** how pi's scoped/featured model list maps to usability and
  ranking without creating a second candidate policy remains open.
- **Existing-session model changes:** whether this resolver also serves
  `gmux agent prompt` when an existing session permits changing model is not
  decided here; this ADR governs launch selection.
- **Backend preference identifiers:** ADR 0033 uses “backend” for terminal/ACP
  drive modes while model availability can also span harnesses/providers. The
  concrete values and equivalence key for `preferred_backends` must be fixed
  with the catalog schema without changing the ordering rule above.
