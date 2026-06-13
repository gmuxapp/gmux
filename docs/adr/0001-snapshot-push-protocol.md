# ADR 0001: Snapshot push protocol

**Status:** Proposed
**Date:** 2026-04-25
**Related:** ADR 0002 (Project ownership from session origin)

## Context

Today's gmuxd uses a hybrid push/pull model for state delivery to its
consumers (browsers and peer gmuxd instances):

- **Hot data via SSE** at `/v1/events`: per-row deltas
  (`session-upsert`, `session-remove`, `session-activity`) plus
  trigger-only events (`projects-update`, `peer-status`) whose
  semantics are "the named resource changed; go re-fetch it."
- **Initial state via parallel HTTP fetches**: `/v1/sessions`,
  `/v1/projects`, `/v1/health`, `/v1/frontend-config`.

This shape produces a recurring set of problems:

1. **Hydration races.** The browser fetches multiple resources in
   parallel; their arrival order is non-deterministic. The frontend
   carries an explicit `sessionsLoaded` gate to prevent the URL
   normalization effect from running before sessions arrive and
   clobbering the path.
2. **Reconnect requires manual refetches.** On SSE reconnect the
   client refetches projects and sessions to catch any deltas that
   happened during the gap, because deltas are not replayed.
3. **Latent bug with network-peer visibility.** `/v1/events` filters
   out network-peer sessions to prevent multi-hop forwarding cycles.
   The same filter applies to browser consumers, so the browser
   receives initial network-peer sessions via the bulk fetch but no
   live updates between reconnects.
4. **Drop-permanence.** A missed `session-remove` leaves a permanent
   ghost in the receiver's view; the only recovery is a manual
   refetch. The hybrid protocol has no self-healing property.
5. **Schema surface.** Seven event types, each with its own payload
   shape and per-event ordering rules. Maintenance and test cost
   scale with the number of event types.

## Decision

Replace the hybrid model with a **snapshot push protocol** on
`/v1/events`. The protocol has three event types, separated by the
cadence and audience of the data they carry:

1. **`snapshot.sessions`**: the full sessions array. High cadence;
   driven by every session mutation (create, update, remove, alive
   flip, project assignment). Subject to coalescing.
2. **`snapshot.world`**: the bundle of low-frequency, browser-only
   resources: `projects`, `peers`, `health`, `frontend_config`.
   Driven by user actions, peer connectivity changes, and settings
   edits. Subject to coalescing.
3. **`session-activity`**: small fire-and-forget notification used
   for UI animations. References a session id; receivers ignore
   activity for unknown ids. Not coalesced.

Both snapshot types are **full** within their resource scope. Drop
tolerance is preserved: a missed `snapshot.sessions` is restored by
the next one, and likewise for `snapshot.world`.

### Why split sessions from world

Sessions and the world bundle have fundamentally different update
characteristics:

| Property | sessions | world |
|---|---|---|
| Cadence | high (terminal activity, peer-driven) | low (user-driven) |
| Payload size | 10–20 KB typical | 4–6 KB typical |
| Peer audience | yes (filtered) | no (each node owns its own) |
| Receiver action | drives sidebar / route logic | settings, project list, peer status |

Bundling them forces the high-cadence stream to drag the
low-frequency payload along on every emission. Splitting them lets
each have its own coalescer, its own drop policy, and its own
filter rule. The world bundle stays bundled internally because all
its members are individually low-frequency and small; further
splitting would multiply event types without a corresponding
benefit.

### Single endpoint with consumer hint

`/v1/events` accepts a `?as=peer|browser` query parameter (default
`browser`). The hint controls which event types the consumer
receives:

- `?as=browser`: all three event types. Sessions field includes
  everything the node can see (own + Local-peer + network-peer
  sessions). Fixes the latent bug in (3) above.
- `?as=peer`: only `snapshot.sessions` (filtered to owned sessions
  only — `Peer == ""` or `peerManager.IsLocalPeer(s.Peer)`) and
  `session-activity` (filtered to visible sessions). `snapshot.world`
  is not emitted to peer consumers; peers don't need other peers'
  projects, peer lists, health, or frontend config.

This preserves today's no-transit-forwarding semantic and is more
honest about the data model than shipping fields the consumer is
expected to ignore.

### Server-side coalescing

Each snapshot kind has its own notifier. State mutators call into
the relevant notifier (or both, for mutations that span both
domains, e.g., a project change that also updates a session's
`project_index` stamp).

Each `/v1/events` connection has one goroutine per snapshot kind it
subscribes to, each with a **trailing-edge throttle** (~50ms): bursts
of mutations within the window coalesce into one snapshot emitted at
window end. An idle mutation emits immediately.

Effect: bounded emission rate per kind (≤20 snapshots/sec/consumer
worst case), no starvation, no unbounded backlog. World snapshots
are typically much rarer than sessions snapshots, so the world
coalescer is mostly a passthrough in practice.

### Mutation hygiene (load-bearing)

The ≤20 snapshots/sec/consumer bound only holds if a *mutation*
means *state actually changed*. Two invariants make that true; both
are easy to violate accidentally and were the cause of a production
incident (a backgrounded mobile tab burned ~4 GB in two days):

1. **No-op writes must not broadcast.** The session store suppresses
   the `session-upsert` event when a write leaves the stored session
   byte-identical (compared *after* normalization: path
   canonicalization and unique-slug renumbering). Every write path
   that can broadcast — `Upsert`, `UpsertRemote`, `Update`, and
   `SetTerminalSize` — routes through this single guard, so the
   property is structural rather than re-derived per call site. The
   peer-mirroring path re-applies a spoke's entire snapshot on every
   snapshot it receives; without this guard, N mirrored sessions ×
   the spoke's emit rate × peer count became N×20×peers redundant
   mutations/sec on the hub bus, each fanning out a full
   `snapshot.sessions` to every browser — ~600 KB/sec/consumer of
   pure churn. The same guard also absorbs the lower-frequency no-op
   churn from the file monitor re-reading unchanged metadata and the
   runner re-emitting identical `terminal_resize` events.
2. **Snapshot composition is deterministically ordered.** The
   sessions array is sorted by ID before emission. The store is
   map-backed, so without an explicit sort two snapshots of
   *identical* state serialize to *different* bytes (Go randomizes
   map iteration), defeating any byte-level dedup downstream and
   making the stream impossible to reason about in captures.

Rule of thumb: a coalescer bounds *how often* you ship; it does
nothing about *whether you should have shipped at all*. That
decision belongs at the mutation source.

### Per-subscriber latest-only buffer

Each subscriber has a bounded channel **per snapshot kind** with
**latest-only on overflow**: a queued snapshot can be dropped in
favor of a newer one. Slow consumers receive coalesced updates
rather than being disconnected. Drops are safe because each
snapshot is self-contained within its kind.

### Frontend projection layer

The browser store consolidates around three private signals:

- `_rawSessions`: written exactly once per `snapshot.sessions`
  received.
- `_rawWorld`: written exactly once per `snapshot.world` received.
- `_pendingMutations`: optimistic mutations the user has issued but
  the server has not yet echoed.

Public signals (`sessions`, `projects`, `peers`, `health`,
`frontendConfig`, `discovered`, `unmatchedActiveCount`, ...) are
`computed` projections of the appropriate raw signal, with the
optimistic overlay applied where relevant.

`discovered` and `unmatchedActiveCount` move from server-side
computation to local `computed` derivations; the server keeps them
on the HTTP `GET /v1/projects` path for CLI compatibility but
removes them from the SSE path.

**Amendment (host-authoritative discovery).** The "discovered is a
per-viewer projection" framing is correct only for the viewer's OWN
(local) sessions, whose owner is the viewer. Discovery is the inverse
of ownership ("which of MY sessions does no project of mine claim"),
and per ADR 0002/0005 ownership is host-authoritative — so a PEER's
discovery belongs to that peer, not the viewer. A viewer recomputing
peer discovery is rule-blind (it can't see the peer's match rules) and
would offer a project the peer already owns by a path-vs-remote rule
mismatch, forking a duplicate slug on "+ Add". The viewer therefore
computes discovery only for its local sessions and merges in each
connected peer's self-advertised `discovered` list verbatim, relayed
through the hub's `snapshot.world.peer_discovered` map (the hub caches
it from the peer's `GET /v1/projects` response). Local-peer /
devcontainer sessions flow through the parent's local discovery (ADR
0005). Disconnected peers contribute no rows.

A `ready` computed gates initial render: `_rawSessions !== null &&
_rawWorld !== null`. The hydration race the existing
`sessionsLoaded` gate addresses goes away because each raw signal
has exactly one writer and there are no order dependencies between
them within their respective domains.

### Optimistic overlay clearance

Each pending mutation targets a specific resource (sessions or
world.projects, etc.) and carries an `appliesToSnapshot(rawSignal)
-> boolean` predicate. When a new snapshot arrives that matches the
mutation's target resource, mutations whose predicate matches are
removed; remaining mutations stay overlaid. A timeout (~10s) clears
stuck entries and surfaces a warning. Errors from the action
endpoint clear the corresponding entry immediately.

### Cross-channel consistency

`snapshot.sessions` and `snapshot.world` arrive on independent
channels. Brief inconsistencies are possible: a session whose
`project_slug` references a project that hasn't yet arrived in the
next world snapshot, or vice versa.

The convergence guarantee handles this: the mutation that produced
the inconsistency triggers a follow-up snapshot in the lagging
channel. Receivers tolerate the transient mismatch (e.g., a session
whose `project_slug` doesn't match any known project falls through
to disclaimed rendering for one frame).

### Action-ack vs snapshot timing

HTTP action responses and SSE snapshots arrive on independent
channels. A 200 OK can land before the snapshot reflecting the
action's effect. The optimistic overlay covers the gap.

## Consequences

### Positive

- **Self-healing per resource.** Drops, missed events, and version
  skew between receiver and sender all converge on the next
  snapshot of the affected kind. No class of bug "we lost a remove
  and now have a permanent ghost."
- **Reconnect parity.** Initial connect and reconnect are the same
  handshake; no fork in client logic.
- **Honest peer protocol.** Peer subscribers receive exactly what
  they need (sessions + activity), not a bundled payload they're
  expected to ignore.
- **Independent cadences.** Sessions stream is decoupled from world
  edits. A burst of session activity doesn't delay a settings
  change; a settings change doesn't ride along with every session
  flip.
- **Smaller schema.** Three event types instead of seven;
  maintenance and test surface shrink.
- **Latent bug fixed.** Browsers receive live updates for
  network-peer sessions.
- **Foundation for further consolidation.** Future fields (e.g.,
  per-session ProjectSlug from ADR 0002) plug into the appropriate
  snapshot without protocol changes.

### Negative

- **Bandwidth per mutation.** Each state change re-ships the full
  state of its kind (capped by coalescer). At gmux's intended
  scale (single user, dozens of sessions, single-digit peers), this
  is bounded and acceptable; not suitable for thousands of sessions
  per node.
- **Receiver responsibility for diff-based actions.** Some receiver
  logic (e.g., the project-ownership receiver rule from ADR 0002)
  needs to know what changed between snapshots. The diff is
  computed per snapshot rather than carried in the event; cost is
  O(N_sessions) per `snapshot.sessions`.
- **Cross-channel inconsistency window.** Tiny (single-digit ms)
  and self-correcting; receivers must be lenient about
  cross-channel references (e.g., session.project_slug pointing at
  an unknown slug for one frame).

### Backwards compatibility (removed)

Protocol 1 (per-event SSE: `session-upsert`, `session-remove`,
`peer-status` on the wire) was kept alongside protocol 2 through the
v1.x fleet upgrade and **removed in the 2.0 release** (#224). The
daemon no longer emits per-event session/peer frames or the
initial `session-upsert` hydration dump, and the hub's `handleEvent`
no longer decodes per-event `session-upsert` / `session-remove`.
A v2 hub consumes `snapshot.sessions`; a v2 browser hydrates from the
leading-edge `snapshot.sessions` / `snapshot.world`. An old hub can no
longer source sessions from a new spoke, and old browsers / scripts
hitting the removed per-event stream see nothing — hence the major bump.

Two things that look like protocol-1 leftovers were **deliberately
kept** because they are load-bearing in v2, not compat:

  - **`projects-update` SSE event**, emitted to `?as=peer` subscribers
    only. A peer hub has no `snapshot.world`, so it relies on this
    event as the trigger to re-fetch the spoke's project list and
    refresh the `peer_projects` it surfaces upstream. Browser
    subscribers no longer receive it (they get projects via
    `snapshot.world`; `projects-update` fires the world coalescer for
    them, see `snapshotPumpRoute`).
  - **`GET /v1/projects`**, called by the peering layer
    (`apiclient.GetProjects` → `Peer.fetchProjects`) on connect and on
    every `projects-update`. The browser does not use it. The other
    bulk GETs (`/v1/sessions`, `/v1/health`, `/v1/frontend-config`)
    likewise remain for CLI / scripting use.

These internal broadcast events (`session-upsert`, `session-remove`,
`projects-update`, `peer-status`) all still exist on the in-process
bus as the canonical "something changed" signal that drives the
snapshot pump; only their legacy *wire* emission and decoders were
removed.

## Alternatives considered

### A. Hybrid: per-row deltas for sessions, full-blob for other resources

Rejected. At gmux's scale, sessions do not churn enough to justify
per-row complexity. The protocol surface stays at seven event types
with their associated ordering rules. Per-row deltas reintroduce
the drop-permanence problem (a missed remove leaves a ghost) for
the highest-frequency resource.

### B. One unified `snapshot` event with all resources bundled

Rejected. The first sketch of this ADR. Sends every world resource
on every session mutation. Honest about being a single source of
truth, but ships fields peer subscribers explicitly don't need
(other peers' projects/peers/health/config) and couples the
high-cadence sessions stream to the low-frequency world data on the
wire. F-3 (the chosen split) gets the same drop-tolerance and
reconnect parity with cleaner channel separation.

### C. Per-resource SSE endpoints (one connection each)

Rejected. Five concurrent SSE connections per browser, one per peer.
N reconnect handlers, N times the connection overhead, fragmented
filter logic. The single endpoint with multiplexed event types
captures the resource separation without the connection
multiplication.

### D. Fully per-resource event types (sessions, projects, peers, health, config)

Rejected. Five snapshot kinds in the schema. Most of the splits
buy nothing: `peers`, `health`, and `config` all change rarely and
together feel like one logical "world" bundle. Splitting them gives
five coalescers, five reconnect-state machines, and five filter
rules, where one bundle suffices. F-3 keeps the split where it
actually pays — sessions vs world.

### E. Single endpoint, full state to all consumers, receiver-side cycle filter

Rejected. Sending V's full session set to peer Q and letting Q drop
already-direct-origin sessions enables transitive peering as a side
effect (Q can see V's peers through V). That is a feature with
non-trivial routing and trust implications and deserves its own
design discussion. The `?as=` hint preserves today's
no-transit-forwarding semantic explicitly.

### F. Two separate endpoints (browser vs peer SSE)

Rejected. The data shape is unified; only the event-type set and
session filter differ. A single endpoint with a small query param
is cleaner than two endpoints with duplicated schema and handler
plumbing.

### G. Feature-flag the new protocol; land incrementally on `main`

Rejected. The chosen design lands both paths atomically in a single
feature-branch PR (rebase-merged): protocol 2 as the new push
shape, protocol 1 as the deprecated-but-still-served fallback for
v1.x peers and tooling. The two paths share state (one store, one
internal broadcast bus) so there is no risk of them drifting on
the daemon side; the hub's `handleEvent` switch makes the dispatch
symmetry explicit on the consumer side. A separate feature flag
would add a knob we have no use for: every gmuxd should serve
both shapes.

### H. Snapshot full state on every event, including activity

Rejected. Activity events are high-frequency (one per output burst)
and tiny (an id). Snapshotting on activity would push bandwidth into
the tens of KB/sec range during active terminal use for no
correctness benefit. Keeping `session-activity` as a separate
notification event is the right size match.

### I. Partial snapshots with "changed fields" hint

Rejected. Drops would lose sub-state for an unbounded period (until
the next mutation to the dropped field's resource). Recovery would
require periodic full re-syncs. The drop-tolerance property is the
single most valuable correctness lever in this design; trading it
for a 30–50% bandwidth saving on the common case is the wrong
exchange.
