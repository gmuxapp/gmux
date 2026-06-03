// --- Peer reference resolution (refs #270) ---
//
// A reference item in projects.json points at a project owned by
// another host. It stores the host's display `peer` name and,
// optionally, the host's stable opaque `node_id` (ADR 0007). Because
// ADR 0007 derives a host's name from Tailscale / the OS hostname, that
// name can change on upgrade — so resolving a reference purely by name
// breaks silently when a host is renamed.
//
// `resolveReferences` is the single source of truth for "which live
// host (if any) does this reference point at, and under what current
// name." Every surface — the sidebar folders, the Hosts tab, the
// settings gear pip — reads off its output so they all agree.
//
// Resolution order, per reference:
//   1. by node_id — if the reference has a node_id and a roster peer
//      reports the same one, it resolves to that peer's *current* name
//      (the display name follows the live host across renames).
//   2. by name — legacy references with no node_id (or whose node_id
//      matches no live peer) fall back to matching the stored name. A
//      name match against a peer that has a node_id yields a backfill,
//      so the reference becomes rename-proof going forward.
//   3. unresolved — no roster peer matches by id or name. The host
//      isn't on the tailnet (online or offline), manually added, or a
//      devcontainer; it may have been renamed or removed.

import type { PeerInfo, ProjectItem } from './types'

/** Composite key for a reference, by its stored (peer, slug). */
export function refKey(peer: string, slug: string): string {
  return `${peer}\u0000${slug}`
}

export interface RefResolution {
  /** Peer name to bucket sessions under and label the folder with.
   *  Equals the live host's current name when resolved by node_id. */
  effectivePeer: string
  /** True when the reference points at a host in the current roster. */
  resolved: boolean
}

/** A referenced host name that matches no roster peer, with the slugs
 *  that point at it. One entry per distinct unresolved name. */
export interface UnresolvedHost {
  name: string
  slugs: string[]
}

/** A projects.json write needed to make a reference rename-proof:
 *  stamp `node_id` (and follow a drifted display name). The writer
 *  finds the item by (fromPeer, slug) and rewrites it to
 *  { peer: toPeer, node_id }. */
export interface RefBackfill {
  slug: string
  fromPeer: string
  toPeer: string
  node_id: string
}

export interface ResolvedReferences {
  /** Per-reference resolution, keyed by refKey(item.peer, item.slug). */
  resolution: Map<string, RefResolution>
  /** Distinct referenced host names not present in the roster. */
  unresolved: UnresolvedHost[]
  /** Opportunistic projects.json updates (stamp node_id / follow rename). */
  backfills: RefBackfill[]
}

/**
 * Resolve every reference item in `projects` against the live peer
 * `roster`. Owned items (no `peer`) are ignored. Pure: no I/O, no
 * signals — callers feed it the current projects + peers and act on
 * the result.
 */
export function resolveReferences(
  projects: readonly ProjectItem[],
  roster: readonly PeerInfo[],
): ResolvedReferences {
  const byNodeId = new Map<string, PeerInfo>()
  const byName = new Map<string, PeerInfo>()
  for (const p of roster) {
    if (p.node_id) byNodeId.set(p.node_id, p)
    byName.set(p.name, p)
  }

  const resolution = new Map<string, RefResolution>()
  const unresolvedMap = new Map<string, string[]>()
  const backfills: RefBackfill[] = []

  for (const item of projects) {
    if (!item.peer) continue // owned project, not a reference
    const key = refKey(item.peer, item.slug)

    // 1. node_id anchor: the durable identity. The live host's current
    //    name wins, so a renamed host stays linked.
    if (item.node_id) {
      const live = byNodeId.get(item.node_id)
      if (live) {
        resolution.set(key, { effectivePeer: live.name, resolved: true })
        if (live.name !== item.peer) {
          // Host was renamed; follow it and refresh the cached name.
          backfills.push({ slug: item.slug, fromPeer: item.peer, toPeer: live.name, node_id: item.node_id })
        }
        continue
      }
      // node_id set but no live peer reports it (host offline and never
      // re-probed, or gone) — fall through to a name match.
    }

    // 2. name match: legacy references, or a reference whose stored
    //    node_id matched no live peer (host gone / replaced). Stamp the
    //    matched peer's node_id whenever it differs from what's stored
    //    — covering both the never-stamped case and refreshing a stale
    //    node_id — so the reference is (re)made rename-proof rather than
    //    repeating the failed-id-then-name dance on every load.
    const named = byName.get(item.peer)
    if (named) {
      resolution.set(key, { effectivePeer: item.peer, resolved: true })
      if (named.node_id && named.node_id !== item.node_id) {
        backfills.push({ slug: item.slug, fromPeer: item.peer, toPeer: item.peer, node_id: named.node_id })
      }
      continue
    }

    // 3. unresolved: no live host matches. Keep the dangling name for
    //    the folder label; the surface flags it.
    resolution.set(key, { effectivePeer: item.peer, resolved: false })
    const slugs = unresolvedMap.get(item.peer) ?? []
    slugs.push(item.slug)
    unresolvedMap.set(item.peer, slugs)
  }

  const unresolved: UnresolvedHost[] = []
  for (const [name, slugs] of unresolvedMap) unresolved.push({ name, slugs })

  return { resolution, unresolved, backfills }
}
