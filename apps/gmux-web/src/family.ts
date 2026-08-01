import type { Session } from './types'

/** Resolve one potential task-family edge without trusting the rest of the
 * ancestry. Both endpoints must be semantic agents; unresolved provenance is
 * not a presentation edge. Promotion breaks only the presentation edge without
 * erasing parent_session_id provenance. */
function potentialFamilyParent(session: Session, byId: ReadonlyMap<string, Session>): Session | undefined {
  if (session.semantic_agent !== true
    || session.promoted_to_root === true
    || !session.parent_session_id
    || session.parent_session_id === session.id) return undefined
  const parent = byId.get(session.parent_session_id)
  return parent?.semantic_agent === true ? parent : undefined
}

/** Resolve whether this session has a safe direct task-family edge. Malformed
 * snapshots can contain ancestry cycles even though the daemon rejects them at
 * registration. Every edge whose ancestry reaches a cycle fails open, keeping
 * all affected sessions visible rather than filtering the whole component. */
export function isFamilyChild(session: Session, sessions: readonly Session[]): boolean {
  const byId = new Map(sessions.map(candidate => [candidate.id, candidate]))
  if (!potentialFamilyParent(session, byId)) return false
  const seen = new Set<string>()
  let current: Session | undefined = session
  while (current) {
    if (seen.has(current.id)) return false
    seen.add(current.id)
    current = potentialFamilyParent(current, byId)
  }
  return true
}

export function familyRoot(session: Session, sessions: readonly Session[]): Session {
  const byId = new Map(sessions.map(s => [s.id, s]))
  const seen = new Set<string>()
  let current = session
  while (isFamilyChild(current, sessions) && !seen.has(current.id)) {
    seen.add(current.id)
    const parent = byId.get(current.parent_session_id!)
    if (!parent) break
    current = parent
  }
  return current
}

export function familyRootId(id: string | null, sessions: readonly Session[]): string | null {
  if (!id) return null
  const session = sessions.find(s => s.id === id)
  return session ? familyRoot(session, sessions).id : id
}

export interface FamilyNavigation {
  parent?: Session
  root?: Session
}

/** True when the selected session belongs to a family with at least one real
 * presentation edge. Orphans and standalone semantic agents stay ordinary
 * roots and therefore do not get family controls. */
export function hasFamily(session: Session, sessions: readonly Session[]): boolean {
  if (isFamilyChild(session, sessions)) return true
  return sessions.some(candidate =>
    candidate.parent_session_id === session.id && isFamilyChild(candidate, sessions),
  )
}

/** Header/drawer navigation for a genuinely nested family member. Root is
 * omitted when it would duplicate Parent. Promoted sessions and unresolved
 * provenance intentionally expose neither control. */
export function familyNavigation(selected: Session, sessions: readonly Session[]): FamilyNavigation {
  if (!isFamilyChild(selected, sessions)) return {}
  const parent = sessions.find(s => s.id === selected.parent_session_id)
  if (!parent) return {}
  const root = familyRoot(selected, sessions)
  return { parent, root: root.id !== parent.id ? root : undefined }
}

export interface FamilyNode {
  session: Session
  children: FamilyNode[]
}

function byRecency(a: Session, b: Session): number {
  const at = a.last_output_at || a.created_at
  const bt = b.last_output_at || b.created_at
  return bt.localeCompare(at) || a.id.localeCompare(b.id)
}

/** Complete descendant tree for a presentation root. Promoted descendants are
 * intentionally excluded: each starts a new family, while provenance remains
 * available on the raw session. */
export function descendantTree(root: Session, sessions: readonly Session[]): FamilyNode {
  const childrenByParent = new Map<string, Session[]>()
  for (const candidate of sessions) {
    if (!isFamilyChild(candidate, sessions)) continue
    const children = childrenByParent.get(candidate.parent_session_id!) ?? []
    children.push(candidate)
    childrenByParent.set(candidate.parent_session_id!, children)
  }
  const build = (session: Session, spine: Set<string>): FamilyNode => {
    const nextSpine = new Set(spine).add(session.id)
    const children = (childrenByParent.get(session.id) ?? [])
      .filter(child => !nextSpine.has(child.id))
      .sort(byRecency)
      .map(child => build(child, nextSpine))
    return { session, children }
  }
  return build(root, new Set())
}

/** Drawer projection. Roots see their tree. A child sees its ancestor spine,
 * then the complete trees rooted at its own sibling level (including itself),
 * rather than unrelated siblings of each ancestor. */
export interface FamilyDrawerProjection {
  root: Session
  ancestors: Session[]
  siblingTrees: FamilyNode[]
}

export function projectFamily(selected: Session, sessions: readonly Session[]): FamilyDrawerProjection {
  const byId = new Map(sessions.map(s => [s.id, s]))
  const root = familyRoot(selected, sessions)
  if (root.id === selected.id) {
    return { root, ancestors: [], siblingTrees: [descendantTree(root, sessions)] }
  }

  const reverse: Session[] = []
  const seen = new Set<string>([selected.id])
  let cursor = selected
  while (isFamilyChild(cursor, sessions)) {
    const parent = byId.get(cursor.parent_session_id!)
    if (!parent || seen.has(parent.id)) break
    reverse.push(parent)
    seen.add(parent.id)
    cursor = parent
  }
  const parent = reverse[0]
  const ancestors = reverse.reverse()
  const siblings = sessions
    .filter(s => isFamilyChild(s, sessions) && s.parent_session_id === parent?.id)
    .sort(byRecency)
  return { root, ancestors, siblingTrees: siblings.map(s => descendantTree(s, sessions)) }
}
