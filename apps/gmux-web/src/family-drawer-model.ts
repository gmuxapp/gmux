import {
  bucketedFamily, familyNavigation, FAMILY_GROUP_CAPS,
  type BucketedFamilyProjection, type FamilyBucket, type FamilyBucketGroup,
  type FamilyBucketNode, type FamilyNavigation,
} from './family'
import type { Session } from './types'

/** The drawer's frozen state: one projection held for as long as the same
 * session stays selected, so live status updates repaint rows in place but
 * never re-sort the list while the user is reading it. Expansion state is
 * keyed per (parent, bucket) and survives selection changes while the
 * drawer stays open; the component unmount (drawer close) discards it. */
export interface DrawerModel {
  selectedId: string
  projection: BucketedFamilyProjection
  navigation: FamilyNavigation
  expanded: ReadonlySet<string>
}

function projectDrawer(
  selected: Session,
  snapshot: readonly Session[],
  expanded: ReadonlySet<string>,
): DrawerModel {
  return {
    selectedId: selected.id,
    projection: bucketedFamily(selected, snapshot),
    navigation: familyNavigation(selected, snapshot),
    expanded,
  }
}

/** Freeze rule: while the selection is unchanged, newer snapshots must NOT
 * re-project (rows would reorder under the cursor). A selection change is
 * an explicit user action and re-projects, carrying expansion state over. */
export function syncDrawer(
  model: DrawerModel | null,
  selected: Session,
  snapshot: readonly Session[],
): DrawerModel {
  if (model && model.selectedId === selected.id) return model
  return projectDrawer(selected, snapshot, model?.expanded ?? new Set())
}

/** Expansion/collapse is the other explicit user action: flip the one
 * (parent, bucket) key and re-project from *current* session facts, so the
 * revealed (or re-capped) group reflects reality instead of the stale
 * membership captured when the drawer opened. */
export function toggleDrawerGroup(
  model: DrawerModel,
  selected: Session,
  snapshot: readonly Session[],
  key: string,
): DrawerModel {
  const expanded = new Set(model.expanded)
  if (expanded.has(key)) expanded.delete(key)
  else expanded.add(key)
  return projectDrawer(selected, snapshot, expanded)
}

export function groupKey(parentId: string, bucket: FamilyBucket): string {
  return `${parentId}:${bucket}`
}

function processNoun(count: number): string {
  return count === 1 ? 'process' : 'processes'
}

/** Wording for a collapsed group's summary row. Agents get the bucket noun,
 * processes their own, so `+547 finished · 12 processes done` reads as two
 * facts instead of one blurred count. */
function summaryLabel(bucket: FamilyBucket, agents: number, processes: number): string {
  const parts: string[] = []
  if (agents > 0) {
    parts.push(bucket === 'working' ? `+${agents} more working` : `+${agents} ${bucket}`)
  }
  if (processes > 0) {
    parts.push(`${agents > 0 ? '' : '+'}${processes} ${processNoun(processes)}${bucket === 'finished' ? ' done' : ''}`)
  }
  return parts.join(' · ')
}

export interface GroupSplit {
  shown: FamilyBucketNode[]
  /** Present iff the group exceeds its cap: the two-state summary row. */
  summary: { key: string; expanded: boolean; label: string } | null
}

/** Apply the per-bucket cap to one group: what the drawer actually renders.
 * Attention is never capped (`Infinity`); other groups show their first
 * `cap` rows plus a `+N …` summary, or everything plus `show fewer`. */
export function splitGroup(
  group: FamilyBucketGroup,
  parentId: string,
  expanded: ReadonlySet<string>,
): GroupSplit {
  const cap = FAMILY_GROUP_CAPS[group.bucket]
  if (group.nodes.length <= cap) return { shown: group.nodes, summary: null }
  const key = groupKey(parentId, group.bucket)
  const isExpanded = expanded.has(key)
  const shown = isExpanded ? group.nodes : group.nodes.slice(0, cap)
  const hidden = group.nodes.slice(shown.length)
  const hiddenAgents = hidden.filter(node => !node.process).length
  const label = isExpanded
    ? 'show fewer'
    : summaryLabel(group.bucket, hiddenAgents, hidden.length - hiddenAgents)
  return { shown, summary: { key, expanded: isExpanded, label } }
}
