# ADR 0022: conversation refs are adapter-opaque

**Status:** Accepted
**Date:** 2026-07-12
**Related:** ADR 0011 (runner-owned session state), ADR 0012 (relational storage a non-goal; search is a derived index), ADR 0014 (adapter-owned conversation sources), ADR 0016 (scrollback as cache), ADR 0021 (ACP session/update as internal conversation schema)

## Context

ADR 0014 made conversation *discovery* adapter-owned: the daemon consumes a
`ConversationSource` event stream instead of running a global file monitor,
and explicitly promised that "non-file adapters fit without new daemon code".
But the interfaces around that seam still spoke in files:

- The sink and index traded in **paths** (`ConversationSink.Upsert(path)`,
  `Index.ScanFile`, `Info.FilePath`, `RemoveByPath`).
- The daemon resolved those paths itself via
  `ConversationFiler.ParseConversationFile(path)` — an interface whose other
  methods (`ConversationRootDir`, `ConversationDir`) exposed each tool's
  on-disk **layout** to any importer.
- The session record carried the transcript path as a first-class field
  (`store.Session.ConversationFile`, wire key `conversation_file`), and
  resume/retention keyed off it as a *path* (`filepath.Clean` matching,
  mtime-based freshness proposals such as PR #393 statting it directly).

For pi/claude/codex — all one-JSONL-per-conversation — this works. For a
future adapter that stores conversations in a database (e.g. opencode), every
one of these sites breaks: there is no path to emit, parse, stat, or clean.
That contradicts both ADR 0014's consequence and the direction of ADR 0021
(conversation content is adapter/runner-translated, never daemon-interpreted).

## Decision

**Introduce the *conversation ref*: an opaque, adapter-scoped locator string
for one stored conversation. Everything above the adapter stores, compares,
and round-trips refs; only the owning adapter interprets them.**

For today's file-backed adapters the ref *is* the transcript's absolute path —
but that is now the adapter's private convention, not a contract.

### The corrected adapter contract (`packages/adapter`)

| Capability | Shape | Replaces |
| --- | --- | --- |
| `ConversationSource` | unchanged: `SnapshotConversations(sink)` + `WatchConversations(ctx, sink)`; the sink's `Upsert(ref)` / `Remove(ref)` now carry opaque refs | path-carrying sink |
| `ConversationDescriber` | `DescribeConversation(ref) (*ConversationInfo, error)` — the only way to turn a ref into metadata | `ConversationFiler.ParseConversationFile(path)` |
| `ConversationOpener` | `OpenConversation(ref) (io.ReadCloser, error)` — raw, adapter-native content stream (the fulltext-search content seam) | *(new)* |
| `ConversationProber` | `ConversationGone(ref) (gone, ok bool)` — deleted vs storage-unavailable | same, path-typed |
| `Resumer` | `CanResume(ref)`; `ResumeCommand(*ConversationInfo)` | same, path-typed |

`ConversationInfo` gains two fields and loses one:

- `Ref string` replaces `FilePath` — the ref the info was described from, so
  resume commands that embed a locator (pi's `--session <path>`) get it from
  the adapter's own answer, not from daemon-side path knowledge.
- `LastActivity time.Time` — **adapter-provided freshness**. File-backed
  adapters implement it as the transcript's mtime (shared `fileLastActivity`
  helper); a DB adapter returns its own timestamp. Consumers (activity
  reseeding à la PR #393, search recency ranking) must take this field, never
  stat anything themselves.

`ConversationRootDir` / `ConversationDir` are no longer part of any
daemon-facing interface. They remain concrete methods on the file-backed
adapters (layout knowledge stays co-located with the layout owner), used only
inside `packages/adapter/adapters` and its tests.

### Daemon changes (`services/gmuxd`)

- `conversations.Index` speaks refs: `Info.Ref` (+ `Info.LastActivity`),
  `Scan(adapter, ref)` via `ConversationDescriber`, `RemoveByRef(adapter, ref)`.
  Because a ref is only unique *within* an adapter, every removal path is
  keyed by `(adapter, ref)` — the sink re-scopes its source's bare ref with
  the owning adapter's name before touching the index or the retirement
  callback, so two adapters using the same ref string can never delete
  each other's conversations. (Absolute file paths made this collision
  impossible by accident; opaque refs make the scoping explicit.)
  Ref comparison itself is exact; only refs that are *rooted paths* (the
  file adapters' spelling) are `filepath.Clean`-normalized, because the
  hook-reported and watcher-reported spellings of the same transcript can
  differ cosmetically. An opaque ref is never normalized — an adapter may
  legitimately use "a/../b" and "b" as distinct locators.
- `store.Session.ConversationFile` → **`ConversationRef`**. The wire and
  persisted JSON key stays `conversation_file`, and the runner's
  `conversation_file` /events event (payload key `path`) stays, purely for
  compatibility — both are documented as legacy names carrying a ref.
  Likewise the runner's `session.State.ConversationRef`.
- Retention (`RemoveDeadByConversationRef(adapter, ref)`,
  `reconcileDeletedConversations`) and resume (`ResolveResumeCommand`) key
  off `(adapter, ref)` and hand the ref straight back to the owning adapter. The one residual path-ism is deliberate:
  `RemoveDeadByConversationRef` still compares refs after `filepath.Clean`,
  because hook-reported and watcher-reported *paths* can differ cosmetically;
  for non-path refs (no separators) `Clean` is the identity, so it degrades to
  exact equality.
- `internal/sessionfiles` → `internal/storegc`: the package had nothing to do
  with files anymore (it purges never-attributed dead sessions); the name was
  a fossil asserting the file model.

### What ADR 0014 got right, kept

ADR 0014 rejected "adapter emits parsed `ConversationInfo` instead of file
paths" to keep the sink thin. That still holds: the sink stays *locator-level*
(one string), and the daemon still decides *when* to resolve a ref to
metadata. What changes is *who understands the locator*: resolution goes
through `DescribeConversation`, so the daemon no longer knows the locator is a
file. ADR 0014's mechanism-ownership (filewatch lives in the adapter module)
is untouched.

## Consequences

- **A DB-backed adapter needs zero daemon changes**: implement
  `ConversationSource` (poll/subscribe), `ConversationDescriber`,
  `ConversationOpener`, `ConversationProber`, `Resumer` with row-key refs, and
  discovery, URL resolution, resume, retention, and activity freshness all
  work. No stat, no mtime, no directory layout anywhere above the adapter.
- **Fulltext search builds on this seam** (per ADR 0012's derived-index
  stance): enumerate via the conversations index / `ConversationSource`
  stream, rank freshness via `Info.LastActivity`, fetch content via
  `ConversationOpener.OpenConversation(ref)`. Content bytes are
  adapter-native; a consumer needing a normalized schema translates per
  ADR 0021.
- **Freshness-from-mtime is now an adapter detail.** The activity reseed
  (PR #393's `activitySeedFor`) asks the adapter
  (`DescribeConversation(...).LastActivity`) instead of statting the ref,
  falling back to the runner's scrollback tee — a daemon-owned cache
  (ADR 0016) that a direct stat is legitimate for.
- Wire and on-disk formats are unchanged (`conversation_file` key, event
  name, meta.json) — pure Go-level refactor, no migration.

## Alternatives

- **Record-level sink (adapter pushes `ConversationInfo`).** Re-rejected for
  the same reason as ADR 0014: duplicates the index's type across the module
  boundary and forces every source event to carry a full parse even when the
  index will discard it (dedupe, slug-unchanged).
- **Rename the wire key to `conversation_ref` now.** Rejected: long-lived
  runners survive daemon upgrades (see the `session_file` shim being retired
  in v2.1); a second rename would need a second shim for zero user value.
- **Keep `ConversationFiler` and add a parallel DB interface later.** Rejected:
  two capability families for one concept is exactly the interface zoo ADR
  0014 collapsed; the opaque ref makes one family sufficient.
