# ADR 0015: References identify by name; node_id is a liveness anchor

**Status:** Proposed
**Date:** 2026-06-23
**Related:** ADR 0005 (references), ADR 0007 (host identity)
**Supersedes:** the ad-hoc reference resolver from refs #270
**Target:** gmux 2.0

## Context

A *reference* (ADR 0005) is a `projects.json` entry `{slug, peer}`
pointing at a peer-owned project. ADR 0007 made the peer **name** a
viewer-owned label and added a stable opaque **`node_id`**.

refs #270 stored `node_id` on references and **resolved them by node_id
at read time** in the viewer, to survive renames. That was both
over-built and broken:

- A peer's name is **frozen at first contact**: `peerstore.AddOrGet`
  updates URL/token/node_id on a node_id match but never the name
  (ADR 0007 §7), and a peer is re-probed only on an explicit "Connect
  to host", not on reconnect. Peer sessions, namespaced IDs,
  `peer_projects`, URLs and references all use that one frozen name. So
  names don't drift, and the resolver's node_id→current-name step was a
  no-op identity in every real case.
- It still computed name-refresh writes (`backfills`) that **nothing
  applied** — dead code that hid the fact the read-time re-resolution
  was load-bearing only because the writer was never built.
- It pushed node_id into the viewer's folder/hub matching layer — the
  one place ADR 0007 keeps it out of — and the hub matched raw names,
  disagreeing with folder-building.

## Decision

**Name is the key; node_id is a liveness anchor.** A reference's `peer`
name is the runtime key (bucketing, `peer_projects`, URLs, routing) — a
viewer-owned label that doesn't drift — so the viewer renders it
directly and does **no** name resolution.

The only residual "resolution" is a one-bit liveness check — *is the
reference's host in the roster?* — computed in the viewer
(`referencePresence` / `unresolvedReferences`): present if its node_id
is live, else if its name is in the roster and no *different* node_id
holds that name (so a reused name can't adopt a stale reference, while a
not-yet-reconnected peer after a restart doesn't flash unresolved).

It drives the unresolved flag and gates session bucketing. The inputs
(reference node_id + roster node_ids) are already in `snapshot.world`;
no new wire field.

Deleted: `resolveReferences`, `effectivePeer`, the dead `backfills`,
the `resolveRef` callback into `buildProjectFolders`, and the hub's
separate name matching. `buildProjectFolders` takes an `isPresent`
predicate instead.

node_id is stamped at creation (`addPeerReference`), when the peer is
known. Name-only references (hand-authored or legacy) match by name and
gain nothing from a node_id, so none is required.

**No daemon machinery:** because names don't drift, there is nothing to
reconcile (contrast Alternative B).

## Consequences

- One namespace (the frozen name) everywhere; the hub's folder lookup
  and its `project` lookup agree by construction.
- No `projects.json` migration: the `{slug, peer, node_id?}` shape is
  unchanged.
- More correct against name reuse than #270: a stale node_id whose old
  name is taken by a different host is flagged unresolved, not silently
  hijacked.
- Cheaper renders: the per-`(projects, peers)` resolver `computed` is
  gone.
- A renamed peer keeps its first-contact label until removed and
  re-added. Accepted: names are viewer-owned (ADR 0007 §7).

## Alternatives considered

**A. Keep node_id in the URL / matching layer.** Rejected — everything
at runtime keys on name; node_id-keying references forces a
node_id→name projection at every use-site, the concern ADR 0007 §4
avoids.

**B. Make peer renames propagate.** Refresh a peer's stored name from
its live identity on reconnect (a `peerstore` change), then have the
daemon rewrite reference names to follow. This would make node_id
genuinely load-bearing on references, and is the right design *if* we
decide self-renames should surface. Out of scope for 2.0; names stay
viewer-owned for now.

**C. node_id-anchored storage + opaque `projects.json` + Projects UI.**
Rejected, and not a planned direction: it costs a hard Projects-UI
dependency and makes the file non-hand-authorable, for no real gain
over a frozen-name label. Recorded only as an option should those
trade-offs change.

**D. Apply refs #270's `backfills` in the viewer.** Rejected: races
across viewers and keeps the read-time resolver — and moot anyway,
since names don't drift.
