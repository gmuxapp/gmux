# ADR 0012: Relational (SQLite) storage is a non-goal

**Status:** Accepted
**Date:** 2026-06-16
**Related:** ADR 0002 (project ownership; share-nothing peering), ADR 0010 /
ADR 0011 (authoritative attribution + runner-owned state)

## Context

An earlier exploration ("attribution is the foundation"; the sidebar is
conversation-keyed) parked a larger idea: move dead-session snapshots and
project **membership** out of the JSON files into an embedded relational
store (SQLite), keyed by a stable identity, so that membership integrity,
atomic updates, and retention-by-query come for free. The motivating bugs
were: sessions disappearing on relaunch, dismissed sessions reappearing,
and renamed sessions sorting to the bottom.

That exploration deferred the decision until attribution was solid.
Attribution is now solid (ADR 0010 → 0011): the agent-hook makes
attribution authoritative and **runner-owned**, the daemon `store.Store`
is a single-writer **read cache**, and `attributions.json` was retired.
This ADR records the resulting decision on the storage migration itself.

### What persists after ADR 0011

| File | Holds | |
| --- | --- | --- |
| `meta.json` (sessionmeta) | dead-session snapshots, dir-per-id | live |
| `projects.json` | project definitions **+ membership** (Key arrays) | live, versioned (v3) |
| `peers.json` | peering config | live |
| ~~`attributions.json`~~ | — | retired by #315 |
| live session state | — | no longer persisted (runner-owned, re-announced) |

The single-writer read-cache model already dissolved the "two writers /
drift between stores" problem that was half the original case for a
relational store. The remaining `SessionKey` (slug-or-id) membership and
the startup `CleanupSessions` are unchanged, but #315's instant,
authoritative attribution removed the *timing* that triggered the
reorder-to-bottom race for hooked agents.

## Decision

**Do not migrate to a relational store.** Relational storage is a
non-goal for v2.0 and the foreseeable future; do not block the v2.0
breaking window on it. Revisit only if a genuinely relational feature
appears (see "No warranted trigger identified" below).

### Rationale

1. **The motivating bugs are already fixed** by ADR 0010/0011
   (authoritative attribution) — not by storage. A migration now would
   buy structural tidiness, not bug fixes.
2. **The migration is not actually gated by the v2.0 breaking window.**
   Persisted state is purely local: peering is share-nothing (ADR 0002),
   so a storage change has **zero wire/peer-protocol impact**. On-disk
   format changes are already routine via the `projects.json` versioned
   migration framework, and an importer makes "migrate later" as cheap as
   "migrate now." Keeping `projects.json` for definitions and scrollback
   as files means nothing user-facing breaks either. There is no
   second-breaking-window penalty for waiting.
3. **The cost is real:** a net-new pure-Go SQLite dependency (cgo is out —
   gmux cross-compiles), rewriting the membership half of `projects`
   (~756 LOC) and `sessionmeta` (~271 LOC) plus their tests (~2000 LOC), a
   new schema + migration framework, a one-time importer, and full
   re-validation of core sidebar-persistence/retention/rehydrate state
   (bugs here mean lost or misplaced sessions).

### No warranted trigger identified

There is no concrete feature on the horizon that justifies a relational
store. Relational storage stays a **non-goal** unless a genuinely
relational feature materializes (e.g. cross-entity history/analytics that
needs joins and ranking over primary state). The bar to revisit is high
and specific; "it would be tidier" does not clear it.

**Fulltext search is explicitly *not* such a trigger.** A search index is
*derived*, not authoritative — the conversation transcripts are the SOT
on disk, so any index is rebuildable at will. That removes the only thing
a relational store offers over the rest of this design (durability and
integrity of *primary* state): a derived index needs no schema,
migration, importer, or drift reconciliation. Search is also one user's
history queried interactively (low QPS), which fits an **in-memory**
inverted index — an extension of the existing in-memory conversations
index, not a new substrate. If startup re-index cost or RAM ever strains
at large corpora, the in-memory-first answers are a serialized index
cache (persist the index, not the data; rebuild opportunistically) and
bounding what is indexed (recent-N / retention-aligned) — both far
lighter than adopting SQLite.

### Cleanup-hardening finding

The earlier exploration floated "harden `CleanupSessions` to resolve keys
against persisted meta, not just the live store." On inspection this was
already effectively true — `CleanupSessions` runs **once** at startup,
after `Sweep` has loaded dead sessions into the store, so a key is pruned
only when neither live nor persisted. No behavioural change is warranted.
The only change made is a small **ordering-robustness refactor**: `known`
is now built explicitly as `persisted-on-disk ∪ live store` (captured from
the sweep result) rather than relying on the incidental fact that swept
dead sessions are still in the store at cleanup time. See
`services/gmuxd/cmd/gmuxd/main.go` (`persistedKeys`).

## Consequences

- v2.0 ships on the current JSON stores (`projects.json` + `meta.json`),
  which is sufficient now that attribution is authoritative.
- Fulltext search, when built, is an in-memory derived index (optionally
  with a serialized cache) — not a reason to adopt SQLite.
- A relational store would remain importer-friendly and wire-neutral if a
  qualifying relational feature ever appears, so nothing is foreclosed.
- No new dependency, no core-state rewrite, no migration risk taken on for
  gains that are currently cosmetic.

## Alternatives considered

- **Do the migration in v2.0 because "it's the breaking window."**
  Rejected: the premise is false — it's an importer-friendly local change
  with no wire impact, so there is no now-or-never pressure.
- **Do it for the structural integrity benefits (FK membership, atomic
  updates).** Real but cosmetic post-#315; not worth the rewrite/risk on
  core state until a feature needs the queries.
- **Adopt SQLite to back fulltext search.** Rejected: a search index is
  derived and rebuildable from the on-disk transcripts, so it gains
  nothing from relational durability/integrity; an in-memory inverted
  index (with an optional serialized cache) serves it more simply.
