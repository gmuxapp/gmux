import type { Session } from './types'
// Type-only: erased at emit, so `family.ts` stays runtime-free of the store.
import type { DotState } from './store'

/** Resolve one potential task-family edge without trusting the rest of the
 * ancestry. The parent must be a semantic agent; its child may be any session.
 * Unresolved provenance is not a presentation edge. Promotion breaks only the
 * presentation edge without erasing parent_session_id provenance. */
function potentialFamilyParent(session: Session, byId: ReadonlyMap<string, Session>): Session | undefined {
  if (session.promoted_to_root === true
    || !session.parent_session_id
    || session.parent_session_id === session.id) return undefined
  const parent = byId.get(session.parent_session_id)
  return parent?.semantic_agent === true ? parent : undefined
}

/** Snapshot-wide family facts. Build this once for a session-list identity and
 * share it across projection callers; membership and roots are then O(1). */
export interface FamilyIndex {
  readonly byId: ReadonlyMap<string, Session>
  readonly childIds: ReadonlySet<string>
  readonly childrenByParent: ReadonlyMap<string, readonly Session[]>
  readonly rootById: ReadonlyMap<string, Session>
}

const familyIndexCache = new WeakMap<readonly Session[], FamilyIndex>()

/** Classify every session with memoized ancestor walks. An ancestry component
 * is visited at most once, including malformed cycles and their descendants. */
export function createFamilyIndex(sessions: readonly Session[]): FamilyIndex {
  const byId = new Map<string, Session>()
  for (const session of sessions) byId.set(session.id, session)

  const ancestrySafe = new Map<string, boolean>()
  const rootById = new Map<string, Session>()
  for (const start of sessions) {
    if (ancestrySafe.has(start.id)) continue
    const path: Session[] = []
    const pathIds = new Set<string>()
    let cursor: Session | undefined = start
    let safe = true
    let root: Session | undefined
    while (cursor) {
      const known = ancestrySafe.get(cursor.id)
      if (known !== undefined) {
        safe = known
        root = rootById.get(cursor.id)
        break
      }
      if (pathIds.has(cursor.id)) {
        safe = false
        break
      }
      path.push(cursor)
      pathIds.add(cursor.id)
      const parent = potentialFamilyParent(cursor, byId)
      if (!parent) {
        root = cursor
        break
      }
      cursor = parent
    }
    for (const session of path) {
      ancestrySafe.set(session.id, safe)
      rootById.set(session.id, safe && root ? root : session)
    }
  }

  const childIds = new Set<string>()
  const childrenByParent = new Map<string, Session[]>()
  for (const session of sessions) {
    if (ancestrySafe.get(session.id) !== true || !potentialFamilyParent(session, byId)) continue
    childIds.add(session.id)
    const children = childrenByParent.get(session.parent_session_id!) ?? []
    children.push(session)
    childrenByParent.set(session.parent_session_id!, children)
  }
  for (const children of childrenByParent.values()) children.sort(byRecency)
  return { byId, childIds, childrenByParent, rootById }
}

/** Return the projection index cached for this exact session-list snapshot. */
export function familyIndex(sessions: readonly Session[]): FamilyIndex {
  const cached = familyIndexCache.get(sessions)
  if (cached) return cached
  const created = createFamilyIndex(sessions)
  familyIndexCache.set(sessions, created)
  return created
}

type FamilySource = readonly Session[] | FamilyIndex

function indexFor(source: FamilySource): FamilyIndex {
  return Array.isArray(source) ? familyIndex(source) : source as FamilyIndex
}

/** Resolve whether this session has a safe direct task-family edge. Malformed
 * snapshots can contain ancestry cycles even though the daemon rejects them at
 * registration. Every edge whose ancestry reaches a cycle fails open, keeping
 * all affected sessions visible rather than filtering the whole component. */
export function isFamilyChild(session: Session, source: FamilySource): boolean {
  return indexFor(source).childIds.has(session.id)
}

export function familyRoot(session: Session, source: FamilySource): Session {
  const index = indexFor(source)
  const known = index.rootById.get(session.id)
  if (known) return known

  // Preserve the old behavior for a caller-provided session not present in the
  // snapshot while still sharing the snapshot's by-ID index.
  const seen = new Set<string>()
  let current = session
  while (true) {
    if (seen.has(current.id)) return session
    seen.add(current.id)
    const parent = potentialFamilyParent(current, index.byId)
    if (!parent) return current
    current = parent
  }
}

export function familyRootId(id: string | null, source: FamilySource): string | null {
  if (!id) return null
  const index = indexFor(source)
  const session = index.byId.get(id)
  return session ? familyRoot(session, index).id : id
}

/** True when the selected session belongs to a family with at least one real
 * presentation edge. Orphans and standalone semantic agents stay ordinary
 * roots and therefore do not get family controls. */
export function hasFamily(session: Session, source: FamilySource): boolean {
  const index = indexFor(source)
  return index.childIds.has(session.id) || (index.childrenByParent.get(session.id)?.length ?? 0) > 0
}

/** Ancestor spine for the header breadcrumbs, root first, parent last (the
 * selected session itself excluded). Empty for roots, promoted sessions and
 * unresolved provenance — exactly the sessions that show a plain title. */
export function familyAncestors(selected: Session, source: FamilySource): Session[] {
  const index = indexFor(source)
  const reverse: Session[] = []
  const seen = new Set<string>([selected.id])
  let cursor = selected
  while (index.childIds.has(cursor.id)) {
    const parent = index.byId.get(cursor.parent_session_id!)
    if (!parent || seen.has(parent.id)) break
    reverse.push(parent)
    seen.add(parent.id)
    cursor = parent
  }
  return reverse.reverse()
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

/** What the member row's glyph column shows, given the member and the
 *  dot state the sidebar computed for it.
 *
 *  The column is always occupied, and — this is the load-bearing part —
 *  it is the *only* place a named member's state can appear, because
 *  the activity line subtracts whoever the row names. A glyph that
 *  can't express `unread` therefore doesn't just look plainer; it drops
 *  that member's attention entirely: counted nowhere, shown nowhere.
 *  So a process keeps its `$` shape but carries state as colour, and an
 *  agent with nothing to report falls back to the branch. */
export type MemberGlyph =
  | { readonly kind: 'process'; readonly state: DotState }
  | { readonly kind: 'dot'; readonly state: Exclude<DotState, 'none'> }
  | { readonly kind: 'branch' }

export function familyMemberGlyph(member: Session, dot: DotState): MemberGlyph {
  if (isProcessSession(member)) return { kind: 'process', state: dot }
  return dot === 'none' ? { kind: 'branch' } : { kind: 'dot', state: dot }
}

/** A family member whose parent is a semantic agent but who is not one
 * itself: a process (shell command, watcher, …) owned by an agent. */
export function isProcessSession(session: Session): boolean {
  return session.semantic_agent !== true
}

/** What a family is *doing right now*, counted over the descendants of
 * one presentation root. Idle members are deliberately not counted: the
 * sidebar line exists to surface live work, not to take a census.
 *
 * Every counted member lands in exactly one bucket, under the same
 * precedence `sessionDotState` uses (error > working > unread), so the
 * line and the family drawer can never disagree. */
export interface FamilyActivity {
  /** Alive members whose last turn failed. */
  readonly error: number
  /** Members with output you haven't seen. */
  readonly unread: number
  /** Semantic-agent members mid-turn ("subagents"). */
  readonly workingAgents: number
  /** Non-agent members running a command ("processes"). */
  readonly workingProcesses: number
}

export const NO_FAMILY_ACTIVITY: FamilyActivity = {
  error: 0, unread: 0, workingAgents: 0, workingProcesses: 0,
}

/** True when a family has something worth a second sidebar line. */
export function hasFamilyActivity(activity: FamilyActivity): boolean {
  return activity.error > 0 || activity.unread > 0
    || activity.workingAgents > 0 || activity.workingProcesses > 0
}

/** Spoken form of the `+` line, which is otherwise pure glyphs. It
 * counts the members the entry does *not* name, so it reads as an
 * addition to the rows above it. */
export function familyActivityLabel(activity: FamilyActivity): string {
  const parts: string[] = []
  // 'process' takes -es; everything else here takes -s.
  const plural = (n: number, word: string) =>
    `${n} ${word}${n === 1 ? '' : word.endsWith('s') ? 'es' : 's'}`
  if (activity.error > 0) parts.push(`${plural(activity.error, 'member')} with an error`)
  if (activity.unread > 0) parts.push(plural(activity.unread, 'unread member'))
  if (activity.workingAgents > 0) parts.push(plural(activity.workingAgents, 'working subagent'))
  if (activity.workingProcesses > 0) parts.push(plural(activity.workingProcesses, 'running process'))
  return `Also in this family: ${parts.join(', ')}`
}

/** Hover title for the sidebar's selected-child row: the path from the
 * family's root row down to the selected member, e.g.
 * `orchestrator › implement drawer › wire up the adapter`. `ancestors`
 * is the root-first spine from `familyAncestors` (its first entry is
 * the root itself), so a direct child collapses to `root › child`. */
export function childTrailTitle(
  root: Session,
  ancestors: readonly Session[],
  child: Session,
): string {
  const middle = ancestors.filter(a => a.id !== root.id).map(a => a.title)
  return [root.title, ...middle, child.title].join(' › ')
}

/** Complete descendant tree for a presentation root. Promoted descendants are
 * intentionally excluded: each starts a new family, while provenance remains
 * available on the raw session. */
export function descendantTree(root: Session, source: FamilySource): FamilyNode {
  const index = indexFor(source)
  const spine = new Set<string>()
  const build = (session: Session): FamilyNode => {
    spine.add(session.id)
    const children = (index.childrenByParent.get(session.id) ?? [])
      .filter(child => !spine.has(child.id))
      .map(build)
    spine.delete(session.id)
    return { session, children }
  }
  return build(root)
}

/** Panel projection: the whole family, from the root, wherever you're
 * standing in it.
 *
 * It used to scope to your own sibling level, which meant the panel
 * showed less the deeper you went — five levels down it showed one row,
 * the session you were already looking at. Depth is exactly when you
 * need the map, and a scope that shifts under you makes the counts line
 * mean something different on every row you visit. Now one scope holds
 * wherever you stand: the same members, the same counts.
 *
 * That is *membership*, not aggregation. The three family surfaces
 * still summarise this set differently on purpose — the trigger badge
 * mutes what you're looking at, the sidebar line subtracts the member
 * its own row names, and this panel counts every row it draws, because
 * it draws them all.
 *
 * `ancestors` stays for callers that want the spine on its own (the
 * header crumbs); the tree already contains those rows. */
export interface FamilyDrawerProjection {
  root: Session
  ancestors: Session[]
  tree: FamilyNode
}

export function projectFamily(selected: Session, source: FamilySource): FamilyDrawerProjection {
  const index = indexFor(source)
  const root = familyRoot(selected, index)
  return {
    root,
    ancestors: familyAncestors(selected, index),
    tree: descendantTree(root, index),
  }
}

/** Status totals for the panel's counts line, over every family member
 * the panel shows — which is now the whole family, root included. Each
 * member is tallied once under its dot-precedence state so the line and
 * the row dots can never disagree. */
export interface FamilyCounts {
  error: number
  /** Semantic-agent members mid-turn. */
  workingAgents: number
  /** Non-agent members running a command. Split from the agents for the
   * same reason the rows carry different glyphs: three subagents
   * thinking and three shells running are different news, and a family
   * is routinely mostly shells. */
  workingProcesses: number
  unread: number
  total: number
}

export function familyCounts(trees: readonly FamilyNode[]): FamilyCounts {
  const counts: FamilyCounts = {
    error: 0, workingAgents: 0, workingProcesses: 0, unread: 0, total: 0,
  }
  const visit = (node: FamilyNode) => {
    const s = node.session
    counts.total++
    if (s.alive && s.status?.error) counts.error++
    else if (s.alive && s.status?.active) {
      if (isProcessSession(s)) counts.workingProcesses++
      else counts.workingAgents++
    }
    else if (s.unread) counts.unread++
    for (const child of node.children) visit(child)
  }
  for (const tree of trees) visit(tree)
  return counts
}
