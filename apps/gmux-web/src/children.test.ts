import type { Session as ProtocolSession } from '@gmux/protocol'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  applyChildrenDelta, childrenViews, closeChildren, hydrateRows, loadMoreChildren, openChildren,
  resetChildren,
} from './children'
import {
  _hydrated, _rawSessions, _resetDeltaResumeState, _setRawWorld, familyActivityById, familyDotById,
  folders, noteStreamHello, sessions, sessionsLoaded, toUISession, unreadCount,
} from './store'

/* PROTO (3.0 world split) — the browser side of "children load on demand and
 * stay live while viewed":
 *  1. a roots-only world state drives the family summaries from
 *     descendant_counts, with no child rows present;
 *  2. loaded children are hydrated into `sessions` (so the family index sees
 *     them) but never double-counted against the root's counts;
 *  3. a scoped delta patches the loaded page (upsert / new child / removal),
 *     refuses a broken chain, and a promotion moves a row out of the page;
 *  4. closing a family drops its hydration except the selected spine. */

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

type FetchCall = { url: string; init?: RequestInit }
let calls: FetchCall[] = []
let pages: Record<string, unknown> = {}

function installFetch(): void {
  calls = []
  vi.stubGlobal('fetch', vi.fn(async (input: string, init?: RequestInit) => {
    calls.push({ url: input, init })
    if (input === '/v1/events/scopes') {
      return new Response(JSON.stringify({ ok: true, data: { epoch: 10, boot_id: 'boot-a' } }), { status: 200 })
    }
    const key = input.replace(/^\/v1\/sessions\//, '')
    const body = pages[key]
    if (!body) return new Response(JSON.stringify({ ok: false, error: { code: 'not_found', message: 'nope' } }), { status: 404 })
    return new Response(JSON.stringify({ ok: true, data: body }), { status: 200 })
  }))
}

beforeEach(() => {
  _rawSessions.value = []
  _hydrated.value = new Map()
  childrenViews.value = new Map()
  _setRawWorld({ projects: [{ slug: 'p', match: [{ path: '/home/user' }] }], peers: [] })
  sessionsLoaded.value = true
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
    // active outranks waiting in the aggregate dot; the root itself is idle.
    expect(familyDotById.value.get('root')).toBe('working')
    expect(unreadCount.value).toBe(2)
    expect(sidebarRows()).toEqual(['root'])
  })

  it('hydrated children are not double-counted against a root that carries counts', () => {
    _rawSessions.value = [toUISession(row('root', { descendant_counts: counts({ total: 1, alive: 1, unread: 1, waiting: 1, children: 1 }) }))]
    hydrateRows([toUISession(row('kid', { parent_session_id: 'root', unread: true }))])
    expect(sessions.value.map(s => s.id)).toEqual(['kid', 'root'])
    // The child is in the index (drawer/routing) but has no sidebar row.
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

describe('openChildren / loadMoreChildren', () => {
  it('loads page 1, hydrates it, registers a scope and reports the page state', async () => {
    _rawSessions.value = [toUISession(row('root', { descendant_counts: counts({ total: 3, children: 3 }) }))]
    pages['root/children?limit=50'] = {
      session_id: 'root', total: 3, epoch: 10, next_cursor: 'c1',
      rows: [row('k3', { parent_session_id: 'root', created_at: '2026-01-03T00:00:00Z' }), row('k2', { parent_session_id: 'root', created_at: '2026-01-02T00:00:00Z' })],
    }
    pages['root/children?limit=50&cursor=c1'] = {
      session_id: 'root', total: 3, epoch: 10,
      rows: [row('k1', { parent_session_id: 'root', created_at: '2026-01-01T00:00:00Z' })],
    }
    await openChildren('root')
    const view = childrenViews.value.get('root')!
    expect(view.rows.map(r => r.id)).toEqual(['k3', 'k2'])
    expect(view.total).toBe(3)
    expect(view.nextCursor).toBe('c1')
    expect(view.scoped).toBe(true)
    expect(view.epoch).toBe(10)
    expect(sessions.value.map(s => s.id)).toEqual(['k2', 'k3', 'root'])
    const scopeCall = calls.find(c => c.url === '/v1/events/scopes')!
    expect(JSON.parse(String(scopeCall.init?.body))).toEqual({ conn: 'conn-1', add: ['children:root'], remove: [] })

    await loadMoreChildren('root')
    const more = childrenViews.value.get('root')!
    expect(more.rows.map(r => r.id)).toEqual(['k3', 'k2', 'k1'])
    expect(more.nextCursor).toBeNull()
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
    expect(calls.some(c => c.url === '/v1/events/scopes')).toBe(false)
  })
})

describe('scoped deltas', () => {
  async function loaded(): Promise<void> {
    _rawSessions.value = [toUISession(row('root', { descendant_counts: counts({ total: 2, children: 2 }) }))]
    pages['root/children?limit=50'] = {
      session_id: 'root', total: 2, epoch: 10,
      rows: [row('k2', { parent_session_id: 'root', created_at: '2026-01-02T00:00:00Z' }), row('k1', { parent_session_id: 'root', created_at: '2026-01-01T00:00:00Z' })],
    }
    await openChildren('root')
  }

  it('upserts a loaded child in place, inserts a new one newest-first, removes a promoted one', async () => {
    await loaded()
    expect(applyChildrenDelta('children:root', 10, 11, [row('k2', { parent_session_id: 'root', created_at: '2026-01-02T00:00:00Z', title: 'renamed' })], [])).toBe(true)
    expect(childrenViews.value.get('root')!.rows.map(r => r.title)).toEqual(['renamed', 't-k1'])
    expect(sessions.value.find(s => s.id === 'k2')?.title).toBe('renamed')

    expect(applyChildrenDelta('children:root', 11, 12, [row('k9', { parent_session_id: 'root', created_at: '2026-01-09T00:00:00Z' })], [])).toBe(true)
    const v = childrenViews.value.get('root')!
    expect(v.rows.map(r => r.id)).toEqual(['k9', 'k2', 'k1'])
    expect(v.total).toBe(3)
    expect(v.epoch).toBe(12)

    // Promotion: the world delta makes k1 a root; the scope delta removes it.
    _rawSessions.value = [..._rawSessions.value, toUISession(row('k1', { created_at: '2026-01-01T00:00:00Z' }))]
    expect(applyChildrenDelta('children:root', 12, 13, [], ['k1'])).toBe(true)
    expect(childrenViews.value.get('root')!.rows.map(r => r.id)).toEqual(['k9', 'k2'])
    expect(_hydrated.value.has('k1')).toBe(false)
    expect(sessions.value.filter(s => s.id === 'k1')).toHaveLength(1) // once, as a root
    expect(sidebarRows().sort()).toEqual(['k1', 'root'])
  })

  it('skips a delta the page already reflects and refuses one that skips ahead', async () => {
    await loaded()
    expect(applyChildrenDelta('children:root', 9, 10, [row('k2', { parent_session_id: 'root', title: 'stale' })], [])).toBe(true)
    expect(childrenViews.value.get('root')!.rows[0].title).toBe('t-k2')
    expect(applyChildrenDelta('children:root', 12, 13, [], [])).toBe(false)
    expect(applyChildrenDelta('session:x', 10, 11, [], [])).toBe(false)
  })

  it('a reset refetches page 1', async () => {
    await loaded()
    const before = calls.filter(c => c.url.includes('/children')).length
    resetChildren('children:root')
    await new Promise(r => setTimeout(r, 0))
    await new Promise(r => setTimeout(r, 0))
    expect(calls.filter(c => c.url.includes('/children')).length).toBe(before + 1)
  })

  it('closing releases the scope and dehydrates everything but the kept spine', async () => {
    await loaded()
    closeChildren('root', new Set(['k1']))
    expect(childrenViews.value.has('root')).toBe(false)
    expect(_hydrated.value.has('k2')).toBe(false)
    expect(_hydrated.value.has('k1')).toBe(true)
    const release = calls.filter(c => c.url === '/v1/events/scopes').at(-1)!
    expect(JSON.parse(String(release.init?.body))).toEqual({ conn: 'conn-1', add: [], remove: ['children:root'] })
  })
})
