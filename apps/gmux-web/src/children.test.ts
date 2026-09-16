import type { Session as ProtocolSession } from '@gmux/protocol'
import { effect } from '@preact/signals'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  applyChildrenDelta, childrenViews, closeChildren, hydrateRows, loadMoreChildren, openChildren,
  reopenAllChildren, resetChildren,
} from './children'
import {
  _hydrated, _rawSessions, _resetDeltaResumeState, _setRawWorld, appendSessionsBootstrap, applySessionsDelta,
  beginSessionsBootstrap, familyActivityById, familyDotById, folders, noteStreamHello, readySessionsBootstrap,
  resetSessionsTransport, sessions, sessionsLoaded, toUISession, unreadCount,
} from './store'

/* PROTO (3.0 world split, round 2) — the browser side of "children load on
 * demand and stay live while viewed":
 *  1. a roots-only world state drives the family summaries from
 *     descendant_counts, with no child rows present;
 *  2. loaded children are hydrated into `sessions` (so the family index sees
 *     them) but never double-counted against the root's counts;
 *  3. scope payloads ride inside the world delta and commit in ONE batch with
 *     the root's counts (D1); the daemon's total is authoritative (M1);
 *  4. a reset / reconnect reloads the pages the drawer holds and dehydrates
 *     what it no longer covers (H1); closing mid-open releases (M3);
 *  5. the registration names the viewport (cursor of the last held page). */

function row(id: string, over: Partial<ProtocolSession> = {}): ProtocolSession {
  return {
    id,
    created_at: '2026-01-01T00:00:00Z',
    command: ['pi'],
    cwd: '/home/user',
    adapter: 'pi',
    semantic_agent: true,
    alive: true,
    pid: 1,
    exit_code: null,
    started_at: '2026-01-01T00:00:00Z',
    exited_at: null,
    title: `t-${id}`,
    subtitle: '',
    status: null,
    unread: false,
    resumable: false,
    socket_path: '/tmp/s.sock',
    project_slug: 'p',
    ...over,
  } as ProtocolSession
}

const counts = (over: Partial<NonNullable<ProtocolSession['descendant_counts']>> = {}) => ({
  total: 0, alive: 0, unread: 0, error: 0, waiting: 0, active: 0, running: 0, children: 0, ...over,
})

const sidebarRows = () => folders.value.flatMap(f => f.sessions.map(s => s.id))
const kid = (n: number, over: Partial<ProtocolSession> = {}) => row(`k${n}`, { parent_session_id: 'root', created_at: `2026-01-${String(n).padStart(2, '0')}T00:00:00Z`, ...over })

type FetchCall = { url: string; init?: RequestInit }
let calls: FetchCall[] = []
let pages: Record<string, unknown> = {}
let scopeEpoch = 10
let scopeReplyRows: number | null = null

function scopeCalls() {
  return calls.filter(c => c.url === '/v1/events/scopes').map(c => JSON.parse(String(c.init?.body)) as { conn: string, add: { scope: string, cursor?: string }[], remove: string[] })
}

function installFetch(): void {
  calls = []
  vi.stubGlobal('fetch', vi.fn(async (input: string, init?: RequestInit) => {
    calls.push({ url: input, init })
    if (input === '/v1/events/scopes') {
      const req = JSON.parse(String(init?.body)) as { add: { scope: string }[] }
      const scopes: Record<string, { rows: number, total: number }> = {}
      for (const a of req.add) scopes[a.scope] = { rows: scopeReplyRows ?? 999, total: 0 }
      return new Response(JSON.stringify({ ok: true, data: { epoch: scopeEpoch, boot_id: 'boot-a', scopes } }), { status: 200 })
    }
    const key = input.replace(/^\/v1\/sessions\//, '')
    const body = pages[key]
    if (!body) return new Response(JSON.stringify({ ok: false, error: { code: 'not_found', message: 'nope' } }), { status: 404 })
    return new Response(JSON.stringify({ ok: true, data: body }), { status: 200 })
  }))
}

const flush = async () => { for (let i = 0; i < 6; i++) await new Promise(r => setTimeout(r, 0)) }

beforeEach(() => {
  _rawSessions.value = []
  _hydrated.value = new Map()
  childrenViews.value = new Map()
  pages = {}
  scopeEpoch = 10
  scopeReplyRows = null
  _setRawWorld({ projects: [{ slug: 'p', match: [{ path: '/home/user' }] }], peers: [] })
  sessionsLoaded.value = true
  resetSessionsTransport()
  _resetDeltaResumeState()
  noteStreamHello('boot-a', 'conn-1')
  installFetch()
})
afterEach(() => { vi.unstubAllGlobals() })

describe('roots-only world state', () => {
  it('a root with descendant_counts gets its family summary, dot and unread badge from the counts alone', () => {
    _rawSessions.value = [toUISession(row('root', {
      descendant_counts: counts({ total: 40, alive: 3, unread: 2, waiting: 2, active: 1, running: 1, children: 12 }),
    }))]
    expect(sessions.value).toHaveLength(1)
    expect(familyActivityById.value.get('root')).toEqual({ error: 0, waiting: 2, active: 1, running: 1 })
    expect(familyDotById.value.get('root')).toBe('working')
    expect(unreadCount.value).toBe(2)
    expect(sidebarRows()).toEqual(['root'])
  })

  it('hydrated children are not double-counted against a root that carries counts', () => {
    _rawSessions.value = [toUISession(row('root', { descendant_counts: counts({ total: 1, alive: 1, unread: 1, waiting: 1, children: 1 }) }))]
    hydrateRows([toUISession(row('kid', { parent_session_id: 'root', unread: true }))])
    expect(sessions.value.map(s => s.id)).toEqual(['kid', 'root'])
    expect(sidebarRows()).toEqual(['root'])
    expect(unreadCount.value).toBe(1)
    expect(familyActivityById.value.get('root')).toEqual({ error: 0, waiting: 1, active: 0, running: 0 })
  })

  it('a root without counts (a 2.x full stream) still tallies its children client-side', () => {
    _rawSessions.value = [
      toUISession(row('root')),
      toUISession(row('kid', { parent_session_id: 'root', unread: true })),
    ]
    expect(familyActivityById.value.get('root')).toEqual({ error: 0, waiting: 1, active: 0, running: 0 })
    expect(unreadCount.value).toBe(1)
  })
})

function threePages(): void {
  _rawSessions.value = [toUISession(row('root', { descendant_counts: counts({ total: 3, children: 3 }) }))]
  pages['root/children?limit=50'] = { session_id: 'root', total: 3, epoch: 10, next_cursor: 'c1', rows: [kid(3)] }
  pages['root/children?limit=50&cursor=c1'] = { session_id: 'root', total: 3, epoch: 10, next_cursor: 'c2', rows: [kid(2)] }
  pages['root/children?limit=50&cursor=c2'] = { session_id: 'root', total: 3, epoch: 10, rows: [kid(1)] }
}

describe('openChildren / loadMoreChildren', () => {
  it('loads page 1, hydrates it, registers the viewport and widens it on load-more', async () => {
    threePages()
    await openChildren('root')
    const view = childrenViews.value.get('root')!
    expect(view.rows.map(r => r.id)).toEqual(['k3'])
    expect(view.total).toBe(3)
    expect(view.nextCursor).toBe('c1')
    expect(view.scoped).toBe(true)
    expect(view.epoch).toBe(10)
    expect(view.liveRows).toBe(1)
    expect(sessions.value.map(s => s.id)).toEqual(['k3', 'root'])
    // The registration names the window: the cursor of the last held page.
    expect(scopeCalls()).toEqual([{ conn: 'conn-1', add: [{ scope: 'children:root', cursor: 'c1' }], remove: [] }])

    await loadMoreChildren('root')
    const more = childrenViews.value.get('root')!
    expect(more.rows.map(r => r.id)).toEqual(['k3', 'k2'])
    expect(more.nextCursor).toBe('c2')
    // Widening re-registers with the new bound (replace, not a second scope).
    expect(scopeCalls().at(-1)).toEqual({ conn: 'conn-1', add: [{ scope: 'children:root', cursor: 'c2' }], remove: [] })
    // Idempotent while loaded + scoped.
    const before = calls.length
    await openChildren('root')
    expect(calls.length).toBe(before)
  })

  it('a peer-owned subtree loads but registers no scope', async () => {
    _rawSessions.value = [toUISession(row('r@hs', { peer: 'hs', descendant_counts: counts({ total: 1, children: 1 }) }))]
    pages['r%40hs/children?limit=50'] = { session_id: 'r@hs', total: 1, epoch: 10, peer: 'hs', rows: [row('k@hs', { peer: 'hs', parent_session_id: 'r@hs' })] }
    await openChildren('r@hs')
    expect(childrenViews.value.get('r@hs')?.scoped).toBe(false)
    expect(scopeCalls()).toHaveLength(0)
  })

  it('the daemon may cap the live window below what is held', async () => {
    threePages()
    scopeReplyRows = 0
    await openChildren('root')
    expect(childrenViews.value.get('root')!.liveRows).toBe(0)
  })

  it('closing while page 1 is in flight releases the scope it registered (M3)', async () => {
    threePages()
    const opening = openChildren('root')
    await new Promise(r => setTimeout(r, 0)) // page fetched, POST about to go
    closeChildren('root')
    await opening
    await flush()
    const posts = scopeCalls()
    const adds = posts.flatMap(p => p.add).length
    const removes = posts.flatMap(p => p.remove).length
    expect(childrenViews.value.has('root')).toBe(false)
    expect(adds).toBe(removes) // whatever was registered was released
    expect(_hydrated.value.size).toBe(0)
  })
})

describe('scoped payloads', () => {
  async function loaded(): Promise<void> {
    _rawSessions.value = [toUISession(row('root', { descendant_counts: counts({ total: 2, children: 2 }) }))]
    pages['root/children?limit=50'] = { session_id: 'root', total: 2, epoch: 10, rows: [kid(2), kid(1)] }
    await openChildren('root')
  }

  it('upserts a loaded child in place, inserts a new one newest-first, removes a promoted one; total is the daemon\'s', async () => {
    await loaded()
    expect(applyChildrenDelta('children:root', 10, 11, 2, [kid(2, { title: 'renamed' })], [])).toBe(true)
    expect(childrenViews.value.get('root')!.rows.map(r => r.title)).toEqual(['renamed', 't-k1'])
    expect(sessions.value.find(s => s.id === 'k2')?.title).toBe('renamed')

    expect(applyChildrenDelta('children:root', 11, 12, 3, [kid(9)], [])).toBe(true)
    const v = childrenViews.value.get('root')!
    expect(v.rows.map(r => r.id)).toEqual(['k9', 'k2', 'k1'])
    expect(v.total).toBe(3)
    expect(v.epoch).toBe(12)

    // Promotion: the world delta makes k1 a root; the scope payload removes it.
    _rawSessions.value = [..._rawSessions.value, toUISession(row('k1', { created_at: '2026-01-01T00:00:00Z' }))]
    expect(applyChildrenDelta('children:root', 12, 13, 2, [], ['k1'])).toBe(true)
    expect(childrenViews.value.get('root')!.rows.map(r => r.id)).toEqual(['k9', 'k2'])
    expect(_hydrated.value.has('k1')).toBe(false)
    expect(sessions.value.filter(s => s.id === 'k1')).toHaveLength(1)
    expect(sidebarRows().sort()).toEqual(['k1', 'root'])
  })

  it('a tail insert is reported through total only, never double-counted (M1)', async () => {
    _rawSessions.value = [toUISession(row('root', { descendant_counts: counts({ total: 3, children: 3 }) }))]
    pages['root/children?limit=50'] = { session_id: 'root', total: 3, epoch: 10, next_cursor: 'c1', rows: [kid(3)] }
    await openChildren('root')
    // The daemon's window excludes the tail; it sends no row, just the total.
    expect(applyChildrenDelta('children:root', 10, 11, 4, [], [])).toBe(true)
    expect(childrenViews.value.get('root')!.total).toBe(4)
    expect(childrenViews.value.get('root')!.rows).toHaveLength(1)
    // A row reparented INTO the held range arrives as an upsert and is shown.
    expect(applyChildrenDelta('children:root', 11, 12, 5, [kid(3, { id: 'moved', title: 'moved' })], [])).toBe(true)
    expect(childrenViews.value.get('root')!.rows.map(r => r.title)).toEqual(['moved', 't-k3'])
    expect(childrenViews.value.get('root')!.total).toBe(5)
  })

  it('skips a payload the page already reflects and refuses one that skips ahead', async () => {
    await loaded()
    expect(applyChildrenDelta('children:root', 9, 10, 2, [kid(2, { title: 'stale' })], [])).toBe(true)
    expect(childrenViews.value.get('root')!.rows[0].title).toBe('t-k2')
    expect(applyChildrenDelta('children:root', 12, 13, 2, [], [])).toBe(false)
    expect(applyChildrenDelta('session:x', 10, 11, 1, [], [])).toBe(false)
  })

  it('closing releases the scope and dehydrates everything but the kept spine', async () => {
    await loaded()
    closeChildren('root', new Set(['k1']))
    expect(childrenViews.value.has('root')).toBe(false)
    expect(_hydrated.value.has('k2')).toBe(false)
    expect(_hydrated.value.has('k1')).toBe(true)
    expect(scopeCalls().at(-1)).toEqual({ conn: 'conn-1', add: [], remove: ['children:root'] })
  })
})

describe('one epoch, one transaction (D1)', () => {
  it('a child mutation\'s count change and row change commit in one batch', async () => {
    beginSessionsBootstrap(3, 10)
    appendSessionsBootstrap(10, [row('root', { descendant_counts: counts({ total: 1, alive: 1, children: 1 }) })], 0)
    expect(readySessionsBootstrap(10)).toBe(true)
    pages['root/children?limit=50'] = { session_id: 'root', total: 1, epoch: 10, rows: [kid(1)] }
    await openChildren('root')

    // Observe the sidebar's derived count and the drawer's rows together.
    const observed: string[] = []
    const dispose = effect(() => {
      const root = sessions.value.find(s => s.id === 'root')
      const rows = childrenViews.value.get('root')?.rows ?? []
      observed.push(`${root?.descendant_counts?.total ?? 0}/${rows.length}`)
    })
    expect(observed).toEqual(['1/1'])
    // Spawn: the root's counts and the new row arrive in one folded delta.
    expect(applySessionsDelta('boot-a', 10, 11,
      [row('root', { descendant_counts: counts({ total: 2, alive: 2, children: 2 }) })], [],
      { 'children:root': { from_epoch: 10, total: 2, upsert: [kid(2)] } },
    )).toBe(true)
    dispose()
    // Never a frame where the count moved but the rows had not (or vice versa).
    expect(observed).toEqual(['1/1', '2/2'])
    expect(childrenViews.value.get('root')!.epoch).toBe(11)
  })

  it('a reset marker inside the delta reloads the held pages after the commit', async () => {
    beginSessionsBootstrap(3, 10)
    appendSessionsBootstrap(10, [row('root', { descendant_counts: counts({ total: 1, children: 1 }) })], 0)
    expect(readySessionsBootstrap(10)).toBe(true)
    pages['root/children?limit=50'] = { session_id: 'root', total: 1, epoch: 10, rows: [kid(1)] }
    await openChildren('root')
    const before = calls.filter(c => c.url.includes('/children')).length
    pages['root/children?limit=50'] = { session_id: 'root', total: 1, epoch: 11, rows: [kid(1, { title: 'fresh' })] }
    expect(applySessionsDelta('boot-a', 10, 11, [], [], { 'children:root': { reset: true } })).toBe(true)
    await flush()
    expect(calls.filter(c => c.url.includes('/children')).length).toBe(before + 1)
    expect(childrenViews.value.get('root')!.rows[0].title).toBe('fresh')
  })
})

describe('reload keeps the drawer honest (H1)', () => {
  it('a reconnect reloads every held page, dehydrates what is gone, and re-registers all views in one request', async () => {
    threePages()
    await openChildren('root')
    await loadMoreChildren('root')
    await loadMoreChildren('root')
    expect(childrenViews.value.get('root')!.rows.map(r => r.id)).toEqual(['k3', 'k2', 'k1'])
    // Meanwhile k1 was dismissed on the daemon: the listing is two rows now.
    pages['root/children?limit=50'] = { session_id: 'root', total: 2, epoch: 20, next_cursor: 'c1', rows: [kid(3)] }
    pages['root/children?limit=50&cursor=c1'] = { session_id: 'root', total: 2, epoch: 20, rows: [kid(2, { title: 'after' })] }
    const postsBefore = scopeCalls().length
    noteStreamHello('boot-a', 'conn-2')
    reopenAllChildren()
    await flush()
    const v = childrenViews.value.get('root')!
    expect(v.rows.map(r => r.id)).toEqual(['k3', 'k2'])
    expect(v.total).toBe(2)
    expect(v.epoch).toBe(20)
    expect(v.scoped).toBe(true)
    expect(_hydrated.value.has('k1')).toBe(false) // no ghost row
    expect(sessions.value.find(s => s.id === 'k2')?.title).toBe('after')
    const posts = scopeCalls().slice(postsBefore)
    expect(posts).toEqual([{ conn: 'conn-2', add: [{ scope: 'children:root', cursor: '' }], remove: [] }])
    // A later payload for k2 lands (it is in the reloaded window).
    expect(applyChildrenDelta('children:root', 20, 21, 2, [kid(2, { title: 'later' })], [])).toBe(true)
    expect(childrenViews.value.get('root')!.rows[1].title).toBe('later')
  })

  it('a scope reset re-pages to the held depth', async () => {
    threePages()
    await openChildren('root')
    await loadMoreChildren('root')
    const before = calls.filter(c => c.url.includes('/children')).length
    resetChildren('children:root')
    await flush()
    // Two pages were held → two page fetches (+ re-registration).
    expect(calls.filter(c => c.url.includes('/children')).length).toBe(before + 2)
    expect(childrenViews.value.get('root')!.rows.map(r => r.id)).toEqual(['k3', 'k2'])
    expect(childrenViews.value.get('root')!.loading).toBe(false)
  })
})
