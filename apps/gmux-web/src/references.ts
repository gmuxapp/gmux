// --- Peer reference liveness (ADR 0015) ---
//
// A reference points at a peer-owned project. Its `peer` name is the
// runtime key (folder bucketing, peer_projects, URLs, routing) and is a
// viewer-owned label frozen at first contact (ADR 0007 §7), so it
// doesn't drift and the viewer renders it directly — no name
// resolution. The only question left is liveness: is the host in the
// roster? Answered by node_id when the reference has one (immune to a
// reused name), else by name. That also blocks a freed-then-reused
// name from adopting a stale reference.

import type { PeerInfo, ProjectItem } from './types'

/** A referenced host name that matches no roster peer, with the slugs
 *  that point at it. One entry per distinct unresolved name. */
export interface UnresolvedHost {
  name: string
  slugs: string[]
}

/**
 * Predicate: is a reference's host in the roster?
 *
 *  - node_id present in the roster → present (the anchor is live).
 *  - name absent from the roster → not present.
 *  - name present but the reference's node_id isn't: present *unless* a
 *    confirmed, different node_id holds that name. A roster peer with no
 *    node_id yet (e.g. not reconnected after a daemon restart) is not a
 *    confirmed mismatch, so we stay present rather than flashing
 *    "host not found"; a different node_id *is* name reuse, so we don't.
 */
export function referencePresence(
  roster: readonly PeerInfo[],
): (peer: string, nodeId?: string) => boolean {
  const liveNodeIds = new Set<string>()
  const nodeIdByName = new Map<string, string>() // '' = in roster, node_id unknown
  for (const p of roster) {
    if (p.node_id) liveNodeIds.add(p.node_id)
    nodeIdByName.set(p.name, p.node_id ?? '')
  }
  return (peer, nodeId) => {
    if (nodeId && liveNodeIds.has(nodeId)) return true
    const named = nodeIdByName.get(peer)
    if (named === undefined) return false // name not in roster
    if (!nodeId) return true // name-only reference: name match suffices
    return named === '' // present unless a different node_id owns the name
  }
}

/**
 * Distinct referenced hosts absent from the roster, with the slugs
 * pointing at each. Drives the Hosts-tab "Referenced but not found"
 * group and the gear pip.
 */
export function unresolvedReferences(
  projects: readonly ProjectItem[],
  roster: readonly PeerInfo[],
): UnresolvedHost[] {
  const present = referencePresence(roster)
  const byName = new Map<string, string[]>()
  for (const item of projects) {
    if (!item.peer) continue // owned project, not a reference
    if (present(item.peer, item.node_id)) continue
    const slugs = byName.get(item.peer) ?? []
    slugs.push(item.slug)
    byName.set(item.peer, slugs)
  }
  const out: UnresolvedHost[] = []
  for (const [name, slugs] of byName) out.push({ name, slugs })
  return out
}

/**
 * Remove the references `(peer, slug)` for `slug` in `slugs`. Pure:
 * returns a new items array. Scoping to the surfaced unresolved `slugs`
 * is what prevents deleting a same-named reference that is actually
 * present via its node_id anchor (ADR 0015).
 */
export function removeReferenceItems(
  items: readonly ProjectItem[],
  peer: string,
  slugs: readonly string[],
): ProjectItem[] {
  const scope = new Set(slugs)
  return items.filter(p => !(p.peer === peer && scope.has(p.slug)))
}

/** Drop every project reference to a host being removed from the
 *  roster, matched by node_id (rename-proof) or cached name. Removing a
 *  host is deliberate, so its references go with it rather than
 *  lingering as "Referenced but not found". Owned projects (no peer) and
 *  references to other hosts are left untouched. */
export function removeHostReferenceItems(
  items: readonly ProjectItem[],
  name: string,
  nodeId?: string,
): ProjectItem[] {
  return items.filter(
    p => !(p.peer !== undefined && (p.peer === name || (nodeId !== undefined && p.node_id === nodeId))),
  )
}
