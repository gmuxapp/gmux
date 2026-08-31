import { sessionPresentationState } from './presentation'
import { sidebarProjectForSession } from './projects'
import { diffReplacedRows, FAMILY_FACT_KEYS, factsUnchanged } from './reconcile'
// Type-only: erased at emit, so `family.ts` stays runtime-free of the store.
import type { DotState } from './store'
import type { ProjectItem, Session } from './types'

/** Resolve one potential task-family edge without trusting the rest of the
 * ancestry. The parent must be a semantic agent; its child may be any session.
 * Unresolved provenance is not a presentation edge. Promotion clears the
 * parent edge; launch provenance remains available for Return to family. */
function potentialFamilyParent(session: Session, byId: ReadonlyMap<string, Session>): Session | undefined {
  if (!session.parent_session_id
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

/** The most recently indexed snapshot, kept for incremental patching: the
 * protocol-3 steady state is a full-list replacement whose rows are mostly
 * the *same objects* after snapshot reconciliation (see reconcile.ts), so the
 * previous index can usually be patched in O(changed + N identity checks)
 * instead of re-walking every ancestry. */
let lastIndexed: { sessions: readonly Session[]; index: FamilyIndex } | null = null

/** Patch `index` (built for `prev`) in place so it describes `next`, or
 * return null when `next` is not a replaced-rows-only successor of `prev`
 * with all family facts (parentage, agent-ness, recency ordering keys)
 * preserved. Mutation is safe because the caller re-keys the cache: the
 * entry for `prev` is deleted, so a later `familyIndex(prev)` rebuilds
 * rather than seeing the patched maps. */
function patchFamilyIndex(
  prev: readonly Session[],
  index: FamilyIndex,
  next: readonly Session[],
): FamilyIndex | null {
  const replaced = diffReplacedRows(prev, next)
  if (!replaced || !factsUnchanged(replaced, FAMILY_FACT_KEYS)) return null
  if (replaced.size === 0) return index
  const byId = index.byId as Map<string, Session>
  for (const n of replaced.values()) byId.set(n.id, n)
  // Root pointers and child lists hold row objects; swap replaced ones.
  // Topology and sort order are unchanged (family facts held).
  const rootById = index.rootById as Map<string, Session>
  for (const [id, root] of rootById) {
    const n = replaced.get(root)
    if (n) rootById.set(id, n)
  }
  for (const children of (index.childrenByParent as Map<string, Session[]>).values()) {
    for (let i = 0; i < children.length; i++) {
      const n = replaced.get(children[i])
      if (n) children[i] = n
    }
  }
  return index
}

/** Return the projection index cached for this exact session-list snapshot. */
export function familyIndex(sessions: readonly Session[]): FamilyIndex {
  const cached = familyIndexCache.get(sessions)
  if (cached) return cached
  let index: FamilyIndex | null = null
  if (lastIndexed && lastIndexed.sessions !== sessions) {
    index = patchFamilyIndex(lastIndexed.sessions, lastIndexed.index, sessions)
    if (index) familyIndexCache.delete(lastIndexed.sessions)
  }
  index ??= createFamilyIndex(sessions)
  familyIndexCache.set(sessions, index)
  lastIndexed = { sessions, index }
  return index
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

/** The one promotion mutation this session admits right now, or null.
 *
 * Families have one mutable axis: `parent_session_id`. Promoting clears that
 * edge, making the session a genuine root for presentation, ownership,
 * active-subagent budgeting, recursive dismissal, and notification
 * suppression. Return to family restores the immutable launch parent when it
 * is still a valid local agent; arbitrary reparenting remains a CLI operation.
 * Eligibility mirrors the projection's own edge rule (`familyIndex`), so the
 * menu can never offer a mutation whose result the sidebar wouldn't show:
 *
 *  - peer-projected sessions (network peers and Local/devcontainer peers
 *    alike) get nothing: the daemon refuses promote/demote for sessions it
 *    doesn't own (`local_only`), so offering the verb would be a lie;
 *  - a family child (cycle-safe, parent local and a semantic agent) can be
 *    promoted — but only if the resulting root row has
 *    the same real stamp-backed placement the sidebar uses. A matching rule
 *    alone is not enough: `buildProjectFolders` buckets only stamped rows,
 *    including retained-dead sessions. The daemon deliberately has no
 *    parentage fallback for project placement (ADR 0026 §8), so an unstamped
 *    or unknown-project child gets a visible blocked action instead of a
 *    mutation that strands it;
 *  - a parentless session with launch provenance can return only while that
 *    launch family still exists and the post-reparent root has that same
 *    placement. A parent outside every project is not a safe target, even if
 *    the child itself is placed. Deleted/non-agent launch parents hide the
 *    action; an existing but unplaced family root blocks it with a reason.
 *
 * `parent` is the session named by the copy: the family being left or rejoined. */
export type PromotionAction =
  | { readonly kind: 'promote'; readonly parent: Session; readonly blocked?: 'no-project' }
  | { readonly kind: 'demote'; readonly parent: Session; readonly blocked?: 'no-project' }

/** The exact stamp-backed predicate used by `buildProjectFolders` for local
 * rows. Routing can serialize a disclaimed match, but that is not enough for
 * this menu: after a promotion/demotion the user must have a sidebar row too. */
function hasSidebarPlacement(session: Session, projects: ProjectItem[]): boolean {
  return sidebarProjectForSession(session, projects) !== null
}

export function promotionAction(
  session: Session,
  source: FamilySource,
  projects: ProjectItem[],
): PromotionAction | null {
  if (session.peer) return null
  const index = indexFor(source)
  if (index.childIds.has(session.id)) {
    const parent = index.byId.get(session.parent_session_id!)
    if (!parent) return null
    const placeable = hasSidebarPlacement(session, projects)
    return placeable
      ? { kind: 'promote', parent }
      : { kind: 'promote', parent, blocked: 'no-project' }
  }
  // A parentless session can return to its immutable launch parent.
  if (session.parent_session_id) return null
  if (!session.launched_from_session_id || session.launched_from_session_id === session.id) return null
  const parent = index.byId.get(session.launched_from_session_id)
  if (!parent || parent.semantic_agent !== true || parent.peer) return null

  // Test the projection produced by reparenting to the launch parent. This
  // catches an unplaced ancestor even when the immediate target is placed.
  const returnedSessions = Array.from(index.byId.values(), candidate =>
    candidate.id === session.id
      ? { ...candidate, parent_session_id: parent.id }
      : candidate)
  const returnedRoot = familyRoot(session, returnedSessions)
  return hasSidebarPlacement(returnedRoot, projects)
    ? { kind: 'demote', parent }
    : { kind: 'demote', parent, blocked: 'no-project' }
}

/** The words the menu says for a promotion action. Centralized so every
 * surface (and its tests) quotes one copy. A note appears only when the
 * action is blocked — a disabled item owes its reason, but an offerable
 * verb explains itself and a subtext under it is noise.
 * `pending` is the in-flight state (same convention as `lifecycleAction`'s
 * resuming labels): the request left, the authoritative snapshot hasn't
 * flipped the projection yet, and the item is busy rather than offerable. */
export function promotionCopy(action: PromotionAction, pending = false): { label: string; note?: string } {
  if (action.blocked === 'no-project') {
    return {
      label: action.kind === 'promote' ? 'Promote to root' : 'Return to family',
      note: action.kind === 'promote'
        ? 'Needs a project: no project contains this session’s folder, so it would have no row of its own. Add one in Settings → Projects.'
        : 'Unavailable: the family root has no project-backed sidebar row. Add one in Settings → Projects before returning this session to its family.',
    }
  }
  if (pending) return { label: action.kind === 'promote' ? 'Promoting…' : 'Returning…' }
  return { label: action.kind === 'promote' ? 'Promote to root' : 'Return to family' }
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

/** What the member row's glyph column shows. Agent state lives on its
 * dot; `$` is stable process identity and is never recolored as agent
 * attention — but it does carry the one process fact, lifecycle:
 * running is lit, finished is dimmed. An agent with nothing to report
 * shows nothing. */
export type MemberGlyph =
  | { readonly kind: 'process'; readonly running: boolean }
  | { readonly kind: 'dot'; readonly state: Exclude<DotState, 'none'> }
  | { readonly kind: 'branch' }

export function familyMemberGlyph(member: Session, dot: DotState): MemberGlyph {
  if (isProcessSession(member)) return { kind: 'process', running: isRunningProcess(member) }
  return dot === 'none' ? { kind: 'branch' } : { kind: 'dot', state: dot }
}

/** A family member whose parent is a semantic agent but who is not one
 * itself: a process (shell command, watcher, …) owned by an agent. */
export function isProcessSession(session: Session): boolean {
  return session.semantic_agent !== true
}

/** The process lifecycle fact shared by summaries and the process view. */
export function isRunningProcess(session: Session): boolean {
  return isProcessSession(session) && session.alive && session.status?.active === true
}

/** The one overview state a member contributes. Agent turn state and
 * process lifecycle are different axes: agents can need attention or be
 * active; a process contributes only while it is running. Its unread output
 * and exit status belong to the launching agent and terminal, not to family
 * attention. The drawer's `processes` control is consequently a type filter,
 * separate from these state buckets. */
export type FamilyState = 'error' | 'active' | 'running' | 'waiting'

export function familyStateOf(session: Session): FamilyState | null {
  if (isProcessSession(session)) return isRunningProcess(session) ? 'running' : null
  switch (sessionPresentationState(session)) {
    case 'active':
    case 'active-error': return 'active'
    case 'waiting-error': return 'error'
    case 'waiting': return 'waiting'
    case 'none': return null
  }
}

/** The standard family numbers: what the descendants of one
 * presentation root are doing right now, one bucket per canonical
 * state, each contributing member counted once by `familyStateOf`.
 * Sidebar and header summaries quote this shape directly. The panel quotes
 * its agent segments directly, then uses `running` as the live count on its
 * broader process-history control. Idle members are deliberately not
 * counted: the summaries exist to surface live work, not take a
 * census. */
export type FamilyActivity = Readonly<Record<FamilyState, number>>

/** How each state presents itself, in the one display order every
 * surface uses: attention first (error, then waiting — the members
 * that need you), ambient work after (active agents, then running
 * commands). Narrow surfaces clip from the right, so what survives a
 * clip is what matters; bucketing precedence is `familyStateOf`'s
 * concern, display order is this one's.
 *
 * One row per state, carrying everything a summary needs to say it:
 * the dot CSS token the rows themselves wear (`unread` is how the app
 * spells "waiting on you"; `null` means the `$` glyph, exactly like a
 * process row), and the words for readers who get the sentence instead
 * of the glyphs. Surfaces choose typography — never what a state looks
 * like, is called, or comes before. */
const FAMILY_DISPLAY = [
  { state: 'error', dot: 'error', phrase: (n: number) => `${plural(n, 'member')} with an error` },
  { state: 'waiting', dot: 'unread', phrase: (n: number) => `${plural(n, 'member')} waiting on you` },
  { state: 'active', dot: 'working', phrase: (n: number) => plural(n, 'active subagent') },
  { state: 'running', dot: null, phrase: (n: number) => plural(n, 'running process') },
] as const

/** 'process' takes -es; everything else here takes -s. */
function plural(n: number, word: string): string {
  return `${n} ${word}${n === 1 ? '' : word.endsWith('s') ? 'es' : 's'}`
}

export interface FamilySegment {
  readonly state: FamilyState
  /** Dot CSS token, or null for the `$`-glyphed running state. */
  readonly dot: 'error' | 'unread' | 'working' | null
  readonly count: number
}

/** One segment per non-zero state. Takes the raw map entry, so a
 * family with no activity at all (absent from the map) is simply no
 * segments rather than a constant every caller has to remember. */
export function familySegments(activity: FamilyActivity | undefined): FamilySegment[] {
  if (!activity) return []
  return FAMILY_DISPLAY
    .filter(({ state }) => activity[state] > 0)
    .map(({ state, dot }) => ({ state, dot, count: activity[state] }))
}

/** Spoken form of the segments, which are otherwise pure glyphs — the
 * turn model's words (waiting on you, active), not the wire's field
 * names, and in the same order the glyphs appear. */
export function familyActivityLabel(activity: FamilyActivity): string {
  const parts = FAMILY_DISPLAY
    .filter(({ state }) => activity[state] > 0)
    .map(({ state, phrase }) => phrase(activity[state]))
  return `In this family: ${parts.join(', ')}`
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
 * That is *membership*. Aggregation is one rule now, shared by every
 * surface (`familyActivityById` + `familySegments`): the root's
 * descendants, one bucket per state. The surfaces once each subtracted
 * whatever they happened to name, which made the same glyphs quote a
 * different number depending on where you stood.
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
