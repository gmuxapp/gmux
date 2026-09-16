/** PROTO (3.0, round 2): children of a root, loaded on demand and kept live
 * while viewed.
 *
 * The world state the sidebar renders from holds ROOTS only; each root
 * carries `descendant_counts`. Opening a family loads the root's direct
 * children page by page (`GET /v1/sessions/{id}/children`, newest first,
 * keyset cursor) and registers a `children:<id>` scope on the one SSE
 * connection (`POST /v1/events/scopes`). The scope names the VIEWPORT: the
 * cursor of the last page held, so the daemon diffs exactly the rows this tab
 * shows (capped server-side; `liveRows` says how many of the held rows are
 * live). Scope payloads ride INSIDE `snapshot.sessions.delta`, so a root's
 * counts and its open rows change in one store transaction.
 *
 * Loaded rows are HYDRATED into the store's session list (`_hydrated` in
 * store.ts): the existing family index, drawer tree, routing and terminal
 * selection then work on them unchanged. Hydration is presentation state:
 * it is dropped when the family is closed (except the selected member and
 * its spine) and when a reload no longer covers a row, and it never counts
 * toward a root's summary — the root's `descendant_counts` are authoritative,
 * and the page/scope `total` is authoritative for the drawer's own count.
 *
 * Peer-owned subtrees: the hub forwards /children to the owning daemon and
 * re-namespaces the rows, but this prototype does not relay scopes across
 * the peer link. For those, the view marks itself `stale` whenever any of the
 * root's counts change and offers a refresh (count-triggered refetch).
 */
import type { Session as ProtocolSession } from '@gmux/protocol'
import { batch, signal } from '@preact/signals'
import { _hydrated, _rawSessions, reconcilePromotionPending, scopeHooks, selectedId, sessions, streamConnID, syncSelectedURL, toUISession } from './store'
import type { Session } from './types'

export const CHILDREN_PAGE_SIZE = 50

export interface ChildrenView {
  readonly rootId: string
  /** Newest first, in listing order. Row objects are the hydrated ones. */
  readonly rows: readonly Session[]
  readonly nextCursor: string | null
  /** Listing total as the daemon last reported it (page or scope payload). */
  readonly total: number
  /** Fanout epoch the loaded pages describe (0 = nothing loaded yet). */
  readonly epoch: number
  readonly loading: boolean
  readonly error: string | null
  /** True when the view must be refetched (peer subtree whose counts moved,
   *  or an error path). A daemon-side reset reloads automatically. */
  readonly stale: boolean
  /** True while a `children:<rootId>` scope is registered on the stream. */
  readonly scoped: boolean
  /** How many of `rows` (from the head) the daemon keeps live; the rest are
   *  pull-only (viewport cap). Equal to rows.length in the common case. */
  readonly liveRows: number
}

export const childrenViews = signal<ReadonlyMap<string, ChildrenView>>(new Map())

interface ChildrenPageWire {
  session_id: string
  rows: ProtocolSession[]
  next_cursor?: string
  total: number
  epoch?: number
  boot_id?: string
  peer?: string
}

interface ScopeAdd { scope: string, cursor?: string }
interface ScopesReply { epoch: number, scopes?: Record<string, { rows: number, total: number }> }

function scopeKey(rootId: string): string {
  return `children:${rootId}`
}

/** Bumped by closeChildren so an open/reload still in flight can tell it
 *  lost the race and must release anything it registered (M3). */
const generation = new Map<string, number>()
const genOf = (rootId: string) => generation.get(rootId) ?? 0

function patchView(rootId: string, patch: Partial<ChildrenView>): void {
  const prev = childrenViews.value.get(rootId) ?? emptyView(rootId)
  const next = new Map(childrenViews.value)
  next.set(rootId, { ...prev, ...patch })
  childrenViews.value = next
}

function emptyView(rootId: string): ChildrenView {
  return { rootId, rows: [], nextCursor: null, total: 0, epoch: 0, loading: false, error: null, stale: false, scoped: false, liveRows: 0 }
}

/** Merge rows into the hydration map (replacing same-id rows). */
export function hydrateRows(rows: readonly Session[]): void {
  if (rows.length === 0) return
  const next = new Map(_hydrated.value)
  for (const row of rows) next.set(row.id, row)
  _hydrated.value = next
  // A member arriving by hydration can settle a pending demotion (the row
  // left the roots view when it rejoined its family) and move the URL.
  reconcilePromotionPending(sessions.value)
  syncSelectedURL()
}

function dehydrate(ids: Iterable<string>): void {
  const next = new Map(_hydrated.value)
  let changed = false
  for (const id of ids) if (next.delete(id)) changed = true
  if (changed) _hydrated.value = next
}

async function fetchPage(rootId: string, cursor: string | null): Promise<ChildrenPageWire> {
  const params = new URLSearchParams({ limit: String(CHILDREN_PAGE_SIZE) })
  if (cursor) params.set('cursor', cursor)
  const res = await fetch(`/v1/sessions/${encodeURIComponent(rootId)}/children?${params}`)
  if (!res.ok) throw new Error(`children ${res.status}`)
  const body = await res.json() as { ok: boolean; data?: ChildrenPageWire; error?: { message?: string } }
  if (!body.ok || !body.data) throw new Error(body.error?.message ?? 'children failed')
  return body.data
}

/** Register / release scopes on the current stream connection. Resolves to
 * the reply (epoch the scopes chain from + per-scope live window), or null
 * when there is no connection or the daemon refused. */
export async function updateScopes(add: ScopeAdd[], remove: string[]): Promise<ScopesReply | null> {
  const conn = streamConnID()
  if (!conn) return null
  try {
    const res = await fetch('/v1/events/scopes', {
      method: 'POST', headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ conn, add, remove }),
    })
    if (!res.ok) return null
    const body = await res.json() as { ok: boolean; data?: ScopesReply }
    return body.ok && body.data ? body.data : null
  } catch {
    return null
  }
}

const isPeerRoot = (rootId: string, page?: ChildrenPageWire) => !!page?.peer || rootId.includes('@')

/** The window a view holds: the cursor of its last page ('' = all of it). */
const windowCursor = (view: ChildrenView) => view.nextCursor ?? ''

/** (Re)register this view's viewport. Returns false when the view was closed
 *  meanwhile (and releases what it just registered). */
async function registerWindow(rootId: string, gen: number): Promise<boolean> {
  const view = childrenViews.value.get(rootId)
  if (!view || gen !== genOf(rootId)) return false
  const key = scopeKey(rootId)
  const reply = await updateScopes([{ scope: key, cursor: windowCursor(view) }], [])
  if (gen !== genOf(rootId) || !childrenViews.value.has(rootId)) {
    // Closed while the POST was in flight: release, do not leak (M3).
    if (reply) void updateScopes([], [key])
    return false
  }
  if (!reply) { patchView(rootId, { scoped: false }); return true }
  const live = reply.scopes?.[key]?.rows
  const cur = childrenViews.value.get(rootId)!
  patchView(rootId, { scoped: true, liveRows: Math.min(live ?? cur.rows.length, cur.rows.length) })
  // The scope chains from `reply.epoch`; pages cut before it may have missed
  // a mutation in between. Reload once so the view is at or past the chain
  // start (rare: the daemon baselines on the same state a page names).
  if (cur.epoch < reply.epoch) await reloadHeldPages(rootId, gen, { register: false })
  return true
}

/** Fetch pages from the head until at least `want` rows (or the listing is
 *  exhausted). Rows previously held but no longer covered are dehydrated
 *  (H1): the drawer never shows a row the daemon does not maintain. */
async function reloadHeldPages(rootId: string, gen: number, opts: { register: boolean, want?: number }): Promise<void> {
  const before = childrenViews.value.get(rootId)
  if (!before) return
  const want = Math.max(opts.want ?? before.rows.length, 1)
  const rows: Session[] = []
  let cursor: string | null = null
  let total = before.total
  let epoch = before.epoch
  let peer = false
  do {
    const page: ChildrenPageWire = await fetchPage(rootId, cursor)
    if (gen !== genOf(rootId) || !childrenViews.value.has(rootId)) return
    rows.push(...page.rows.map(toUISession))
    cursor = page.next_cursor ?? null
    total = page.total
    epoch = page.epoch ?? epoch
    peer = peer || !!page.peer
  } while (cursor && rows.length < want)
  const keep = keepSetFor(selectedId.peek())
  const covered = new Set(rows.map(r => r.id))
  const dropped = before.rows.filter(r => !covered.has(r.id) && !keep.has(r.id)).map(r => r.id)
  batch(() => {
    hydrateRows(rows)
    dehydrate(dropped)
    patchView(rootId, { rows, nextCursor: cursor, total, epoch, loading: false, stale: false, error: null, liveRows: Math.min(rows.length, childrenViews.value.get(rootId)?.liveRows || rows.length) })
  })
  if (opts.register && !isPeerRoot(rootId) && !peer) await registerWindow(rootId, gen)
}

/** Open (or reload) a root's children: page 1 + live scope. Idempotent
 * while a view is already loaded and scoped. */
export async function openChildren(rootId: string, opts?: { force?: boolean }): Promise<void> {
  const existing = childrenViews.value.get(rootId)
  if (existing && existing.epoch > 0 && existing.scoped && !existing.stale && !opts?.force) return
  if (existing?.loading) return
  const gen = genOf(rootId)
  patchView(rootId, { loading: true, error: null })
  try {
    if (existing && existing.epoch > 0) {
      await reloadHeldPages(rootId, gen, { register: true })
      return
    }
    const page = await fetchPage(rootId, null)
    if (gen !== genOf(rootId) || !childrenViews.value.has(rootId)) return
    const rows = page.rows.map(toUISession)
    batch(() => {
      hydrateRows(rows)
      patchView(rootId, {
        rows, nextCursor: page.next_cursor ?? null, total: page.total,
        epoch: page.epoch ?? 0, loading: false, stale: false, liveRows: rows.length,
      })
    })
    // Peer subtrees get no live scope in this prototype (see header).
    if (isPeerRoot(rootId, page)) { patchView(rootId, { scoped: false }); return }
    await registerWindow(rootId, gen)
  } catch (err) {
    if (gen !== genOf(rootId) || !childrenViews.value.has(rootId)) return
    patchView(rootId, { loading: false, error: err instanceof Error ? err.message : String(err) })
  }
}

/** Append the next page and widen the live viewport to include it. */
export async function loadMoreChildren(rootId: string): Promise<void> {
  const view = childrenViews.value.get(rootId)
  if (!view || !view.nextCursor || view.loading) return
  const gen = genOf(rootId)
  patchView(rootId, { loading: true, error: null })
  try {
    const page = await fetchPage(rootId, view.nextCursor)
    if (gen !== genOf(rootId) || !childrenViews.value.has(rootId)) return
    const rows = page.rows.map(toUISession)
    const current = childrenViews.value.get(rootId) ?? view
    const have = new Set(current.rows.map(r => r.id))
    const appended = rows.filter(r => !have.has(r.id))
    batch(() => {
      hydrateRows(rows)
      patchView(rootId, { rows: [...current.rows, ...appended], nextCursor: page.next_cursor ?? null, total: page.total, loading: false })
    })
    if (current.scoped) await registerWindow(rootId, gen) // widen the window
  } catch (err) {
    if (gen !== genOf(rootId) || !childrenViews.value.has(rootId)) return
    patchView(rootId, { loading: false, error: err instanceof Error ? err.message : String(err) })
  }
}

/** Close a family view: release its scope and drop its hydrated rows,
 * except `keep` (the selected member and its spine). */
export function closeChildren(rootId: string, keep: ReadonlySet<string> = new Set()): void {
  const view = childrenViews.value.get(rootId)
  if (!view) return
  generation.set(rootId, genOf(rootId) + 1)
  if (view.scoped) void updateScopes([], [scopeKey(rootId)])
  const next = new Map(childrenViews.value)
  next.delete(rootId)
  childrenViews.value = next
  dehydrate(view.rows.map(r => r.id).filter(id => !keep.has(id)))
}

/** Every open view, for re-registration after a reconnect: reload each view's
 * held pages, then register all viewports in ONE request (L7). */
export function reopenAllChildren(): void {
  const views = [...childrenViews.value.values()]
  if (views.length === 0) return
  void (async () => {
    const gens = new Map(views.map(v => [v.rootId, genOf(v.rootId)]))
    await Promise.all(views.map(async v => {
      patchView(v.rootId, { scoped: false, loading: true })
      try { await reloadHeldPages(v.rootId, gens.get(v.rootId)!, { register: false }) } catch { patchView(v.rootId, { loading: false, stale: true }) }
    }))
    const adds: ScopeAdd[] = []
    for (const v of views) {
      const cur = childrenViews.value.get(v.rootId)
      if (!cur || gens.get(v.rootId) !== genOf(v.rootId) || isPeerRoot(v.rootId)) continue
      adds.push({ scope: scopeKey(v.rootId), cursor: windowCursor(cur) })
    }
    if (adds.length === 0) return
    const reply = await updateScopes(adds, [])
    if (!reply) return
    for (const a of adds) {
      const rootId = a.scope.slice('children:'.length)
      const cur = childrenViews.value.get(rootId)
      if (!cur || gens.get(rootId) !== genOf(rootId)) { void updateScopes([], [a.scope]); continue }
      patchView(rootId, { scoped: true, liveRows: Math.min(reply.scopes?.[a.scope]?.rows ?? cur.rows.length, cur.rows.length) })
    }
  })()
}

function newestFirst(a: Session, b: Session): number {
  if (a.created_at !== b.created_at) return a.created_at < b.created_at ? 1 : -1
  return a.id < b.id ? 1 : a.id > b.id ? -1 : 0
}

/** Apply one scope payload from a folded `snapshot.sessions.delta`. Called
 * inside the store's delta batch, so the root's counts and these rows commit
 * together. Returns false when it does not chain onto what we hold (the
 * caller then reloads the held pages). */
export function applyChildrenDelta(
  scope: string, fromEpoch: number, epoch: number, total: number | undefined,
  upsert: ProtocolSession[], remove: string[],
): boolean {
  if (!scope.startsWith('children:')) return false
  const rootId = scope.slice('children:'.length)
  const view = childrenViews.value.get(rootId)
  if (!view || view.epoch === 0) return false
  // A payload that ends at or before the page's epoch carries values the
  // page already reflects (skip); one that starts after it means we missed
  // a step (reload).
  if (epoch <= view.epoch) return true
  if (fromEpoch > view.epoch) return false
  const removed = new Set(remove)
  const incoming = new Map<string, Session>()
  for (const row of upsert) incoming.set(row.id, toUISession(row))
  const rows: Session[] = []
  for (const row of view.rows) {
    if (removed.has(row.id)) continue
    const next = incoming.get(row.id)
    if (next) { incoming.delete(row.id); rows.push(next) } else rows.push(row)
  }
  // Anything else the daemon upserted is inside our viewport by definition
  // (a spawn at the head, or a row reparented into the held range).
  for (const row of incoming.values()) rows.push(row)
  rows.sort(newestFirst)
  batch(() => {
    hydrateRows(rows)
    dehydrate(remove)
    patchView(rootId, { rows, epoch, total: total ?? view.total })
  })
  return true
}

/** The daemon could not honor the scope chain (or we could not apply it):
 *  reload the pages we hold. */
export function resetChildren(scope: string): void {
  if (!scope.startsWith('children:')) return
  const rootId = scope.slice('children:'.length)
  const view = childrenViews.value.get(rootId)
  if (!view || view.loading) return
  const gen = genOf(rootId)
  patchView(rootId, { loading: true })
  void reloadHeldPages(rootId, gen, { register: true }).catch(() => {
    if (gen === genOf(rootId) && childrenViews.value.has(rootId)) patchView(rootId, { loading: false, stale: true })
  })
}

/** Count-triggered invalidation for views without a live scope (peer
 * subtrees): when a root's descendant_counts change, mark its open view
 * stale so the drawer offers a refresh. */
export function noteRootCountsChanged(rootId: string): void {
  const view = childrenViews.value.get(rootId)
  if (!view || view.scoped || view.loading) return
  patchView(rootId, { stale: true })
}

/** Hydrate one session (and its spine) by id — for a deep link to a child
 * that is not in the roots-only world state. Resolves false when the daemon
 * does not know the id. */
export async function ensureSessionLoaded(id: string): Promise<boolean> {
  if (_rawSessions.value.some(s => s.id === id) || _hydrated.value.has(id)) return true
  try {
    const res = await fetch(`/v1/sessions/${encodeURIComponent(id)}?ancestors=1`)
    if (!res.ok) return false
    const body = await res.json() as { ok: boolean; data?: { session: ProtocolSession; ancestors?: ProtocolSession[] } }
    if (!body.ok || !body.data) return false
    const rows = [...(body.data.ancestors ?? []), body.data.session].map(toUISession)
    hydrateRows(rows)
    return true
  } catch {
    return false
  }
}

/** Is this id currently hydrated (i.e. a loaded child rather than a root)? */
export function isHydrated(id: string): boolean {
  return _hydrated.value.has(id)
}

/** Drawer helper: the spine + selected id that must survive a close. */
export function keepSetFor(selected: string | null): Set<string> {
  const keep = new Set<string>()
  if (!selected) return keep
  const byId = new Map(sessions.peek().map(s => [s.id, s]))
  let cur = byId.get(selected)
  while (cur && !keep.has(cur.id)) {
    keep.add(cur.id)
    cur = cur.parent_session_id ? byId.get(cur.parent_session_id) : undefined
  }
  return keep
}

/** Roots whose counts changed between two committed world states, limited to
 * the families currently open without a live scope (peer subtrees). Any
 * count field counts (L4). */
function noteCommit(prev: readonly Session[], next: readonly Session[]): void {
  const views = childrenViews.value
  if (views.size === 0) return
  const prevById = new Map(prev.map(s => [s.id, s]))
  for (const row of next) {
    const view = views.get(row.id)
    if (!view || view.scoped) continue
    const before = prevById.get(row.id)?.descendant_counts
    const after = row.descendant_counts
    if (before === after) continue
    if (before && after && (Object.keys(after) as (keyof typeof after)[]).every(k => before[k] === after[k])) continue
    noteRootCountsChanged(row.id)
  }
}

// Install the stream hooks (see store.ts scopeHooks).
scopeHooks.onHello = reopenAllChildren
scopeHooks.onScopeDelta = applyChildrenDelta
scopeHooks.onScopeReset = resetChildren
scopeHooks.onCommit = noteCommit
scopeHooks.ensureMember = ensureSessionLoaded
scopeHooks.keepMemberLive = (parentId) => { void openChildren(parentId) }
