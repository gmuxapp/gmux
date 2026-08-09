import type { FamilyNode } from './family'

/** Per-level visible-row cap before a `+N more` summary row takes over.
 * Recency ordering does the triage that status buckets used to: unread
 * members have recent output by definition, so they surface within the
 * cap while long-dead noise sinks below the fold. */
export const FAMILY_LEVEL_CAP = 15

export interface LevelSplit {
  shown: readonly FamilyNode[]
  /** Present iff the level exceeds the cap: the two-state summary row. */
  summary: { key: string; expanded: boolean; label: string } | null
}

/** Apply the cap to one children level: what the panel actually renders.
 * Expansion is keyed per parent id and two-state — expanding shows the
 * whole level plus `show fewer`, never an incremental ladder.
 *
 * `pinned` is the spine of the session you're viewing. Those rows are
 * never folded away, whatever their recency: the panel projects the
 * whole family from the root, so every level between you and the root
 * is now capped, and a quiet parent — the ordinary state of an
 * orchestrator whose subagent is doing the talking — would otherwise
 * take your own row down with it. A pinned row costs one slot of the
 * cap rather than widening it, so the level's height stays put. */
export function splitLevel(
  nodes: readonly FamilyNode[],
  parentId: string,
  expanded: ReadonlySet<string>,
  pinned: ReadonlySet<string> = new Set(),
): LevelSplit {
  if (nodes.length <= FAMILY_LEVEL_CAP) return { shown: nodes, summary: null }
  const isExpanded = expanded.has(parentId)
  if (isExpanded) return { shown: nodes, summary: { key: parentId, expanded: true, label: 'show fewer' } }
  // Keep recency order: choose which rows survive, then render them in
  // the order they already had.
  const keep = new Set(nodes.filter(n => pinned.has(n.session.id)).map(n => n.session.id))
  for (const node of nodes) {
    if (keep.size >= FAMILY_LEVEL_CAP) break
    keep.add(node.session.id)
  }
  const shown = nodes.filter(n => keep.has(n.session.id))
  return {
    shown,
    summary: { key: parentId, expanded: false, label: `+${nodes.length - shown.length} more` },
  }
}
