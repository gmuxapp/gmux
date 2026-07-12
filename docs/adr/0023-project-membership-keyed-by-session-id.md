# ADR 0023: Project membership keyed by session ID; conversation-lineage takeover

**Status:** Accepted
**Date:** 2026-07-12
**Related:** ADR 0002 (project ownership), ADR 0003 (resume-by-id), ADR 0011
(runner-owned session state), ADR 0012 (relational storage non-goal — partially
superseded, see below), ADR 0014 (adapter-owned conversation sources),
ADR 0022 (adapter-opaque conversation refs)

## Context

`projects.json` `Item.Sessions[]` is the ordered list of session keys that
controls sidebar membership and order. A key is `SessionKey(id, slug)` =
`slug || id`, justified by a comment claiming slugs are "stable across
restarts" while IDs are "ephemeral". Both halves of that justification are
now false:

- **Session IDs are durable.** Daemon-driven resume/restart preserves the
  `sess-` ID across the seam (`--resume-id`, ADR 0003), and dead sessions
  persist under `sessions/<id>/meta.json` (sessionmeta) across daemon
  restarts. The ID survives the full alive → dead → resumed lifecycle.
- **Slugs are mutable display names.** Per #348 the slug follows the title;
  per ADR 0011 it is *runner-owned* state; since #360/#394 it is empty until
  the conversation is titled and cleared on genuine re-binds. It is also
  renumbered (`-2`) on collision per (adapter, peer) — so the key depends on
  what else happened to be alive at commit time — and it is only unique per
  (adapter, peer) while `Sessions[]` spans adapters within a project.

The slug was doing three jobs; only the first is legitimate:

1. **Display/URL name** — its real job (web routing).
2. **Membership key** in `projects.json` — every slug transition (title
   arrives, rename, clear on re-bind) orphans the key, and the session is
   re-appended at the bottom of the array: the "sidebar reorder" bug.
   Cross-adapter same-title sessions silently collide on one key.
3. **Continuity detection** — the store's takeover rule evicts a dead
   session shadowed by a live one with the same (adapter, peer, slug). This
   both *misses* true continuations (untitled conversations have no slug to
   collide on → duplicate rows) and *invents* false ones (two different
   conversations with the same title → a dead resumable session is wrongly
   evicted).

ADR 0012 froze `SessionKey` on the premise that "the motivating bugs are
already fixed" by authoritative attribution. That held only while slug
transitions were rare; #394 made them the normal session lifecycle (every
session starts slugless), re-exposing the reorder-to-bottom bug ADR 0012
listed as motivation. This ADR supersedes ADR 0012's "the remaining
`SessionKey` (slug-or-id) membership … unchanged" — while keeping its actual
decision (no relational store; JSON files + versioned migration).

## Decision

Give each identifier exactly one job:

| Identifier | Job |
| --- | --- |
| **Session ID** | row identity: membership, order, dismiss, cleanup |
| **Conversation ID (+ lineage)** | continuity: takeover/eviction |
| **Slug** | display and URLs, nothing else |

### 1. Membership keys are session IDs

`Item.Sessions[]` holds store session IDs only: `sess-<hex>` for
runner-born sessions, the conversation UUID for migration-window
convIndex-rehydrated rows, namespaced IDs for Local-peer sessions — in all
cases, exactly the ID the store row carries. `SessionKey` is deleted;
`AutoAssignSession`'s ID→slug replacement hack is deleted; `DismissSession`
takes only the ID. `projects.json` becomes less human-editable
(`"sessions": ["sess-a1b2c3d4", …]`); that is accepted. Hand-edited slug
keys are no longer supported.

### 2. Takeover is keyed by conversation lineage, not slug

The store's duplicate resolution (`resolveDuplicateSlugsLocked`) changes
trigger: a **live** session that binds conversation *C* evicts a **dead**
row whose conversation ID is in *C*'s lineage set:

    lineage(C) = { C's own conversation ID } ∪ C's AncestorIDs

`adapter.ConversationInfo` gains `AncestorIDs []string` (adapter-owned
knowledge, ADR 0014):

- **pi / codex:** empty — they resume into the same file, so plain
  conversation-ID equality covers them (the degenerate lineage set).
- **claude:** `--resume` writes a *new* transcript (new UUID) that replays
  the prior history. Replayed lines keep their original per-line
  `sessionId`, and the first user message embeds a
  `# session-start -- <original-uuid>.jsonl` marker. `DescribeConversation`
  (which already scans every line) collects distinct `sessionId` values;
  ancestors = all except the file's own UUID, with the marker as a
  corroborating signal. Both signals are load-bearing for Claude's own
  replay mechanism, not incidental formatting.

No runner or hook-protocol change: the runner keeps reporting only the
conversation ref (ADR 0011, opaque per ADR 0022); the daemon computes lineage with the
adapter parse it already performs for the conversations index and resume
commands. The eviction fires at conversation-bind time — the earliest
moment identity is actually known.

Failure modes are asymmetric by construction: a future Claude that stops
replaying history degrades to a *missed* link (duplicate rows — old row
remains as a legitimate fork point), never a false eviction. A conversation
ID appears in a lineage only if the agent itself wrote it there.

**Alive–alive collisions coexist.** Two live sessions bound to the same
conversation (external resume of a still-running conversation) are two
honest rows; takeover is strictly live-evicts-dead. `ensureUniqueSlug`
survives, re-scoped as URL/display dedup only.

### 3. No identity-transfer event; relaunch heals

There is no `session-replace {old→new}` plumbing. On takeover the evicted
row's key is removed from `projects.json` by a small projects-manager hook
on the **existing** `session-remove` broadcast (the sessionmeta
`WatchRemovals` pattern; idempotent with explicit dismiss; no-op for peer
sessions, which hold no local keys per ADR 0002). The successor session is
added by the existing live auto-assign — at the **end** of the array.
External resume therefore does not inherit sidebar position; "a new session
lands at the end" is the accepted model.

### 4. One-shot v4 migration; no compatibility layer

`projects.json` v3 → v4 converts keys once at load:

- Keys matching a known session ID pass through.
- Slug keys are resolved to IDs against the sessionmeta sweep, which runs
  before `projects.Load` and covers exactly the cohort that cannot
  self-heal: **dead/resumable sessions are never auto-assigned** (dismiss
  must stick), so dropping their keys would orphan them permanently.
- Unresolvable keys (sessions live at upgrade time, hand-edited entries)
  are dropped; live sessions re-auto-assign on register at the end of the
  array. `backupFile` snapshots the pre-migration file.

There is **no** dual-key resolution layer and **no** wire tolerance for
slug keys. The web is embedded in the gmuxd binary (version-locked), so no
old-web/new-daemon window exists. `ReorderSessions` filters incoming keys
to known sessions (previously it merged arbitrary strings into the array).
Mixed 1.6/2.0 *peer* fleets already degrade to a silent no-sessions mode on
the SSE protocol seam (no version gate exists; see the peer-skew
investigation), so cross-version reorder compatibility is moot; a peer
protocol-version handshake is optional backlog, out of scope here.

`CleanupSessions`' `known` set becomes persisted sessionmeta IDs ∪ live
store IDs (slug entries dropped from both sides). The pre-existing
local-peer first-scan race (keys pruned if the peer connects after first
scan) is unchanged and self-healing (live re-auto-assign).

### 5. Slug becomes display-only everywhere

With no identity riding on slugs, the last UUID-slug remnant is fixed:
convIndex-rehydrated rows no longer surface `Slugify(conversationID)` as
their slug. The conversations index may keep an internal unique key, but
the surfaced row slug for untitled conversations is empty; the web falls
back to the session ID for URLs (as established in #394).

## Accepted breakage (one-time, v2.0 window)

1. Sessions **live at upgrade** lose their sidebar position once
   (re-auto-assigned at the end on first register). No data loss.
2. **Hand-edited** slug keys in `projects.json` stop working.
3. Mixed-version **peer** reorder no-ops during a fleet rolling upgrade
   (subsumed by the existing silent no-sessions skew; self-heals on
   upgrade).
4. **External resume** no longer inherits sidebar position (ongoing, by
   design — see §3).

## Alternatives considered

- **Patch slug keys harder** (more re-key paths in `AutoAssignSession`).
  Rejected: post-#394, slug transitions are the normal lifecycle, not an
  edge case; this is whack-a-mole on a wrong key.
- **ID keys, but keep slug-collision takeover.** Rejected: it wires correct
  membership to an incorrect continuity signal — still misses untitled
  resumes (duplicates) and still falsely evicts on same-title collisions.
- **Conversation ID as the membership key** ("the sidebar row is the
  conversation"). Rejected: shells have no conversation → a mixed key space
  again (the `slug || id` shape we are escaping); everything else — store,
  SSE, dismiss, sessionmeta, web — speaks session IDs, so it adds a
  parallel identity layer with a mapping obligation at every consumer; and
  the key would still mutate once at first bind.
- **`session-replace` pair event to preserve position across takeover.**
  Rejected as machinery disproportionate to external resume's frequency;
  can be added later without schema impact if it ever grates.
- **Permanent dual-key resolver + opportunistic rewrite** (accept slug keys
  at every use site forever). Rejected: it existed to protect a
  mixed-version window that doesn't exist (embedded web) and a one-time
  upgrade cohort crossing a declared breaking window anyway.
- **Claude-only slug-takeover fallback** for the lineage gap. Rejected:
  reintroduces the false-positive eviction this ADR eliminates.

## Consequences

- Rename, titling, re-bind, and slug clears no longer touch
  `projects.json`: sidebar order is stable under all display-name changes.
- Untitled conversations resume without duplicating rows (pi/codex by file
  identity; claude by lineage); same-title conversations can no longer
  evict each other.
- `projects.json` v4 is machine-oriented; the versioned-migration framework
  (ADR 0012) carries the one-shot conversion with a backup.
- The store's eviction seam gains an adapter parse at bind time — bounded
  by the same parsing the conversations index already does.
- ADR 0012's storage decision stands; its `SessionKey` freeze is
  superseded by this ADR.
