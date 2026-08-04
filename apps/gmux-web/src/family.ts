import type { Session } from './types'

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

export interface FamilyNavigation {
  parent?: Session
  root?: Session
}

/** True when the selected session belongs to a family with at least one real
 * presentation edge. Orphans and standalone semantic agents stay ordinary
 * roots and therefore do not get family controls. */
export function hasFamily(session: Session, source: FamilySource): boolean {
  const index = indexFor(source)
  return index.childIds.has(session.id) || (index.childrenByParent.get(session.id)?.length ?? 0) > 0
}

/** Header/drawer navigation for a genuinely nested family member. Root is
 * omitted when it would duplicate Parent. Promoted sessions and unresolved
 * provenance intentionally expose neither control. */
export function familyNavigation(selected: Session, source: FamilySource): FamilyNavigation {
  const index = indexFor(source)
  if (!index.childIds.has(selected.id)) return {}
  const parent = index.byId.get(selected.parent_session_id!)
  if (!parent) return {}
  const root = familyRoot(selected, index)
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

/** A family member whose parent is a semantic agent but who is not one
 * itself: a process (shell command, watcher, …) owned by an agent. */
export function isProcessSession(session: Session): boolean {
  return session.semantic_agent !== true
}

/** Count of alive semantic agents in the selected session's presentation
 * family, root included. Drives the header pill's `Agents · N` count;
 * processes are intentionally excluded so the label stays truthful. */
export function familyAgentCount(selected: Session, source: FamilySource): number {
  const index = indexFor(source)
  const root = familyRoot(selected, index)
  let count = 0
  for (const session of index.byId.values()) {
    if (session.alive
      && !isProcessSession(session)
      && (index.rootById.get(session.id) ?? session) === root) count++
  }
  return count
}

// ── Bucketed drawer projection (noise reduction) ────────────────────────────

/** Drawer grouping vocabulary, in display order. `attention` is error or
 * unread — the reason the user opened the drawer; never capped. */
export type FamilyBucket = 'attention' | 'working' | 'idle' | 'finished'

const BUCKET_RANK: Record<FamilyBucket, number> = { attention: 0, working: 1, idle: 2, finished: 3 }

/** Per-group visible-row caps before a `+N …` summary row takes over. */
export const FAMILY_GROUP_CAPS: Record<FamilyBucket, number> = {
  attention: Infinity,
  working: 20,
  idle: 10,
  finished: 3,
}

/** A session's own bucket from its own facts (no subtree inheritance).
 * Follows the `sessionDotState` precedence exactly (error > working >
 * unread) so a row's dot and its group never disagree, with one addition:
 * everything dead is `finished` — a swarm of dead children whose final
 * output was never viewed is noise, not 500 rows of attention (matching
 * `unreadCount`'s alive gate). */
export function familyBucket(session: Session): FamilyBucket {
  if (!session.alive) return 'finished'
  if (session.status?.error) return 'attention'
  if (session.status?.active) return 'working'
  return session.unread ? 'attention' : 'idle'
}

export interface FamilyBucketNode {
  session: Session
  /** Effective bucket: the highest-urgency bucket in this node's subtree.
   * A dead parent with a working descendant sorts (and stays visible) as
   * working — noise is only what is transitively noise. */
  bucket: FamilyBucket
  process: boolean
  /** Children partitioned into bucket groups, in display order. */
  groups: FamilyBucketGroup[]
}

export interface FamilyBucketGroup {
  bucket: FamilyBucket
  nodes: FamilyBucketNode[]
}

/** Status-truth totals for the heading counts line: agents tallied by their
 * own bucket (not the hoisted effective one), processes tallied separately. */
export interface FamilyBucketCounts {
  attention: number
  working: number
  idle: number
  finished: number
  processes: number
}

export interface BucketedFamilyProjection {
  root: Session
  ancestors: Session[]
  groups: FamilyBucketGroup[]
  counts: FamilyBucketCounts
}

const titleCollator = new Intl.Collator(undefined, { numeric: true, sensitivity: 'base' })

function compareBucketNodes(a: FamilyBucketNode, b: FamilyBucketNode): number {
  // Bucket order, agents before processes, recency, then natural title so
  // same-batch children (`swarm-425/426/…`) read ordered instead of shuffled.
  const at = a.session.last_output_at || a.session.created_at
  const bt = b.session.last_output_at || b.session.created_at
  return BUCKET_RANK[a.bucket] - BUCKET_RANK[b.bucket]
    || Number(a.process) - Number(b.process)
    || (bt < at ? -1 : bt > at ? 1 : 0)
    || titleCollator.compare(a.session.title, b.session.title)
    || (a.session.id < b.session.id ? -1 : a.session.id > b.session.id ? 1 : 0)
}

function groupBucketNodes(nodes: FamilyBucketNode[]): FamilyBucketGroup[] {
  const sorted = [...nodes].sort(compareBucketNodes)
  const groups: FamilyBucketGroup[] = []
  for (const node of sorted) {
    const last = groups[groups.length - 1]
    if (last && last.bucket === node.bucket) last.nodes.push(node)
    else groups.push({ bucket: node.bucket, nodes: [node] })
  }
  return groups
}

function toBucketNode(node: FamilyNode, counts: FamilyBucketCounts): FamilyBucketNode {
  const children = node.children.map(child => toBucketNode(child, counts))
  const process = isProcessSession(node.session)
  const own = familyBucket(node.session)
  if (process) counts.processes++
  else counts[own]++
  let bucket = own
  for (const child of children) {
    if (BUCKET_RANK[child.bucket] < BUCKET_RANK[bucket]) bucket = child.bucket
  }
  return { session: node.session, bucket, process, groups: groupBucketNodes(children) }
}

/** `projectFamily` with every children list (the top-level sibling list
 * included) partitioned into attention → working → idle → finished groups.
 * One pass over the projected trees on top of the shared snapshot index,
 * so the whole thing stays O(n) for a snapshot. */
export function bucketedFamily(selected: Session, source: FamilySource): BucketedFamilyProjection {
  const base = projectFamily(selected, indexFor(source))
  const counts: FamilyBucketCounts = { attention: 0, working: 0, idle: 0, finished: 0, processes: 0 }
  const top = base.siblingTrees.map(tree => toBucketNode(tree, counts))
  return { root: base.root, ancestors: base.ancestors, groups: groupBucketNodes(top), counts }
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

/** Drawer projection. Roots see their tree. A child sees its ancestor spine,
 * then the complete trees rooted at its own sibling level (including itself),
 * rather than unrelated siblings of each ancestor. */
export interface FamilyDrawerProjection {
  root: Session
  ancestors: Session[]
  siblingTrees: FamilyNode[]
}

export function projectFamily(selected: Session, source: FamilySource): FamilyDrawerProjection {
  const index = indexFor(source)
  const root = familyRoot(selected, index)
  if (root.id === selected.id) {
    return { root, ancestors: [], siblingTrees: [descendantTree(root, index)] }
  }

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
  const parent = reverse[0]
  const ancestors = reverse.reverse()
  const siblings = parent ? index.childrenByParent.get(parent.id) ?? [] : []
  return { root, ancestors, siblingTrees: siblings.map(s => descendantTree(s, index)) }
}
