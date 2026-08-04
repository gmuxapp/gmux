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
 * whole level plus `show fewer`, never an incremental ladder. */
export function splitLevel(
  nodes: readonly FamilyNode[],
  parentId: string,
  expanded: ReadonlySet<string>,
): LevelSplit {
  if (nodes.length <= FAMILY_LEVEL_CAP) return { shown: nodes, summary: null }
  const isExpanded = expanded.has(parentId)
  const shown = isExpanded ? nodes : nodes.slice(0, FAMILY_LEVEL_CAP)
  const label = isExpanded ? 'show fewer' : `+${nodes.length - shown.length} more`
  return { shown, summary: { key: parentId, expanded: isExpanded, label } }
}
