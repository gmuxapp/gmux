/** PROTO (3.0 world split): children of a root, loaded on demand and kept
 * live while viewed.
 *
 * The world state the sidebar renders from holds ROOTS only; each root
 * carries `descendant_counts`. Opening a family loads the root's direct
 * children page by page (`GET /v1/sessions/{id}/children`, newest first,
 * keyset cursor) and registers a `children:<id>` scope on the one SSE
 * connection (`POST /v1/events/scopes`). From then on the daemon sends
 * `snapshot.scope.delta` for that subtree at the same epoch as the world-state
 * delta, so the root's counts and the open view never disagree.
 *
 * Loaded rows are HYDRATED into the store's session list (`_hydrated` in
 * store.ts): the existing family index, drawer tree, routing and terminal
 * selection then work on them unchanged. Hydration is presentation state:
 * it is dropped when the family is closed (except the selected member and
 * its spine), and it never counts toward a root's summary — the root's
 * `descendant_counts` are authoritative.
 *
 * Peer-owned subtrees: the hub forwards /children to the owning daemon and
 * re-namespaces the rows, but this prototype does not relay scopes across
 * the peer link. For those, the view marks itself `stale` whenever the
 * root's counts change and offers a refresh (count-triggered refetch).
 */
import type { Session as ProtocolSession } from '@gmux/protocol'
import { signal } from '@preact/signals'
import { _hydrated, _rawSessions, scopeHooks, sessions, streamConnID, toUISession } from './store'
import type { Session } from './types'

export const CHILDREN_PAGE_SIZE = 50

export interface ChildrenView {
  readonly rootId: string
  /** Newest first, in listing order. Row objects are the hydrated ones. */
  readonly rows: readonly Session[]
  readonly nextCursor: string | null
  readonly total: number
  /** Fanout epoch the loaded pages describe (0 = nothing loaded yet). */
  readonly epoch: number
  readonly loading: boolean
  readonly error: string | null
  /** True when the daemon told us to refetch (scope reset) or, for a peer
   *  subtree without a live scope, when the root's counts changed. */
  readonly stale: boolean
  /** True while a `children:<rootId>` scope is registered on the stream. */
  readonly scoped: boolean
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

function scopeKey(rootId: string): string {
  return `children:${rootId}`
}

function patchView(rootId: string, patch: Partial<ChildrenView>): void {
  const prev = childrenViews.value.get(rootId) ?? emptyView(rootId)
  const next = new Map(childrenViews.value)
  next.set(rootId, { ...prev, ...patch })
  childrenViews.value = next
}

function emptyView(rootId: string): ChildrenView {
  return { rootId, rows: [], nextCursor: null, total: 0, epoch: 0, loading: false, error: null, stale: false, scoped: false }
}

/** Merge rows into the hydration map (replacing same-id rows). */
export function hydrateRows(rows: readonly Session[]): void {
  if (rows.length === 0) return
  const next = new Map(_hydrated.value)
  for (const row of rows) next.set(row.id, row)
  _hydrated.value = next
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
 * the epoch the scopes chain from, or null when there is no connection. */
export async function updateScopes(add: string[], remove: string[]): Promise<number | null> {
  const conn = streamConnID()
  if (!conn) return null
  try {
    const res = await fetch('/v1/events/scopes', {
      method: 'POST', headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ conn, add, remove }),
    })
    if (!res.ok) return null
    const body = await res.json() as { ok: boolean; data?: { epoch: number } }
    return body.ok && body.data ? body.data.epoch : null
  } catch {
    return null
  }
}

/** Open (or reload) a root's children: page 1 + live scope. Idempotent
 * while a view is already loaded and scoped. */
export async function openChildren(rootId: string, opts?: { force?: boolean }): Promise<void> {
  const existing = childrenViews.value.get(rootId)
  if (existing && existing.epoch > 0 && existing.scoped && !existing.stale && !opts?.force) return
  if (existing?.loading) return
  patchView(rootId, { loading: true, error: null })
  try {
    const page = await fetchPage(rootId, null)
    const rows = page.rows.map(toUISession)
    hydrateRows(rows)
    patchView(rootId, {
      rows, nextCursor: page.next_cursor ?? null, total: page.total,
      epoch: page.epoch ?? 0, loading: false, stale: false,
    })
    // Peer subtrees get no live scope in this prototype (see header).
    if (page.peer || rootId.includes('@')) {
      patchView(rootId, { scoped: false })
      return
    }
    const epoch = await updateScopes([scopeKey(rootId)], [])
    if (epoch === null) return
    patchView(rootId, { scoped: true })
    // The scope chains from `epoch`; a page cut before it may have missed a
    // mutation in between. Refetch once so the view is at or past the chain
    // start (rare: the window is one round trip).
    if ((page.epoch ?? 0) < epoch) {
      const again = await fetchPage(rootId, null)
      const rows2 = again.rows.map(toUISession)
      hydrateRows(rows2)
      patchView(rootId, { rows: rows2, nextCursor: again.next_cursor ?? null, total: again.total, epoch: again.epoch ?? epoch })
    }
  } catch (err) {
    patchView(rootId, { loading: false, error: err instanceof Error ? err.message : String(err) })
  }
}

/** Append the next page. */
export async function loadMoreChildren(rootId: string): Promise<void> {
  const view = childrenViews.value.get(rootId)
  if (!view || !view.nextCursor || view.loading) return
  patchView(rootId, { loading: true, error: null })
  try {
    const page = await fetchPage(rootId, view.nextCursor)
    const rows = page.rows.map(toUISession)
    hydrateRows(rows)
    const have = new Set(view.rows.map(r => r.id))
    const appended = rows.filter(r => !have.has(r.id))
    patchView(rootId, {
      rows: [...(childrenViews.value.get(rootId)?.rows ?? view.rows), ...appended],
      nextCursor: page.next_cursor ?? null, total: page.total, loading: false,
    })
  } catch (err) {
    patchView(rootId, { loading: false, error: err instanceof Error ? err.message : String(err) })
  }
}

/** Close a family view: release its scope and drop its hydrated rows,
 * except `keep` (the selected member and its spine). */
export function closeChildren(rootId: string, keep: ReadonlySet<string> = new Set()): void {
  const view = childrenViews.value.get(rootId)
  if (!view) return
  if (view.scoped) void updateScopes([], [scopeKey(rootId)])
  const next = new Map(childrenViews.value)
  next.delete(rootId)
  childrenViews.value = next
  dehydrate(view.rows.map(r => r.id).filter(id => !keep.has(id)))
}

/** Every open view, for re-registration after a reconnect. */
export function reopenAllChildren(): void {
  for (const view of childrenViews.value.values()) {
    patchView(view.rootId, { scoped: false })
    void openChildren(view.rootId, { force: true })
  }
}

function newestFirst(a: Session, b: Session): number {
  if (a.created_at !== b.created_at) return a.created_at < b.created_at ? 1 : -1
  return a.id < b.id ? 1 : a.id > b.id ? -1 : 0
}

/** Apply one `snapshot.scope.delta` for a children scope. Returns false when
 * it does not chain onto what we hold (the caller then refetches). */
export function applyChildrenDelta(
  scope: string, fromEpoch: number, epoch: number,
  upsert: ProtocolSession[], remove: string[],
): boolean {
  if (!scope.startsWith('children:')) return false
  const rootId = scope.slice('children:'.length)
  const view = childrenViews.value.get(rootId)
  if (!view || view.epoch === 0) return false
  // The chain is per connection and starts at scope registration; our page
  // was cut at or after that. A delta that ends at or before the page's
  // epoch carries values the page already reflects (skip it); one that
  // starts after the page's epoch means we missed a step (refetch).
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
  // New children: newest-first pages mean anything newer than our oldest
  // loaded row belongs in the loaded window; older rows live in the unloaded
  // tail and will page in.
  const oldest = view.rows.length > 0 ? view.rows[view.rows.length - 1] : null
  let hiddenNew = 0
  for (const row of incoming.values()) {
    if (!oldest || view.nextCursor === null || newestFirst(row, oldest) <= 0) rows.push(row)
    else hiddenNew++
  }
  rows.sort(newestFirst)
  hydrateRows([...rows])
  dehydrate(remove)
  const total = Math.max(0, view.total + incoming.size + hiddenNew - remove.length)
  patchView(rootId, { rows, epoch, total })
  return true
}

/** The daemon could not honor the scope chain: refetch page 1. */
export function resetChildren(scope: string): void {
  if (!scope.startsWith('children:')) return
  const rootId = scope.slice('children:'.length)
  if (!childrenViews.value.has(rootId)) return
  patchView(rootId, { stale: true })
  void openChildren(rootId, { force: true })
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
export function keepSetFor(selectedId: string | null): Set<string> {
  const keep = new Set<string>()
  if (!selectedId) return keep
  const byId = new Map(sessions.value.map(s => [s.id, s]))
  let cur = byId.get(selectedId)
  while (cur && !keep.has(cur.id)) {
    keep.add(cur.id)
    cur = cur.parent_session_id ? byId.get(cur.parent_session_id) : undefined
  }
  return keep
}

/** Roots whose counts changed between two committed world states, limited to
 * the families currently open (peer subtrees have no live scope). */
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
    if (before && after && before.total === after.total && before.alive === after.alive
      && before.unread === after.unread && before.active === after.active) continue
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
