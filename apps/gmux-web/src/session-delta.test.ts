import { beforeEach, describe, expect, it } from 'vitest'
import {
  _deltaResumeState, _rawSessions, _resetDeltaResumeState, _setRawWorld, appendSessionsBootstrap, applySessionsDelta,
  beginSessionsBootstrap, folders,noteStreamHello, readySessionsBootstrap,
  resetSessionsTransport, sessionStreamURL, sessionsLoaded, sidebarSessions, 
} from './store'
import type { ProtocolSession } from './types'

/* SPIKE (R5/R6) — delta application on the frontend.
 *
 * The properties under test are the ones the design rests on:
 *  1. a delta chain lands on exactly the state the equivalent full snapshot
 *     would have produced;
 *  2. untouched rows keep object identity (#512/#518 structural sharing), so
 *     projections still patch O(changed) instead of rebuilding O(N);
 *  3. a broken chain is refused rather than silently applied. */

function row(id: string, over: Partial<ProtocolSession> = {}): ProtocolSession {
  return {
    id,
    created_at: '2026-01-01T00:00:00Z',
    command: ['/bin/sh'],
    cwd: '/home/user',
    adapter: 'shell',
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
    ...over,
  } as ProtocolSession
}

function bootstrap(epoch: number, rows: ProtocolSession[]): void {
  beginSessionsBootstrap(3, epoch)
  appendSessionsBootstrap(epoch, rows, 0)
  expect(readySessionsBootstrap(epoch)).toBe(true)
}

beforeEach(() => {
  _rawSessions.value = []
  _setRawWorld({ projects: [], peers: [] })
  sessionsLoaded.value = false
  resetSessionsTransport()
  _resetDeltaResumeState()
  noteStreamHello('boot-a')
})

describe('delta application', () => {
  it('upserts, removes and inserts in server (ascending id) order', () => {
    bootstrap(10, [row('a'), row('b'), row('d')])
    expect(applySessionsDelta('boot-a', 10, 11, [row('b', { title: 'changed' }), row('c')], ['d'])).toBe(true)
    expect(_rawSessions.value.map(s => s.id)).toEqual(['a', 'b', 'c'])
    expect(_rawSessions.value.find(s => s.id === 'b')?.title).toBe('changed')
    expect(_deltaResumeState()).toEqual({ bootID: 'boot-a', epoch: 11 })
  })

  it('lands on the same state a full snapshot would have produced', () => {
    bootstrap(1, [row('a'), row('b'), row('c')])
    applySessionsDelta('boot-a', 1, 2, [row('b', { alive: false })], [])
    applySessionsDelta('boot-a', 2, 3, [row('d')], ['a'])
    const viaDelta = JSON.stringify(_rawSessions.value)

    _rawSessions.value = []
    resetSessionsTransport()
    bootstrap(9, [row('b', { alive: false }), row('c'), row('d')])
    expect(JSON.stringify(_rawSessions.value)).toBe(viaDelta)
  })

  it('keeps object identity for untouched rows (#512/#518 structural sharing)', () => {
    bootstrap(1, [row('a'), row('b'), row('c')])
    const before = _rawSessions.value
    const [a0, b0, c0] = before
    const foldersBefore = folders.value
    const sidebarBefore = sidebarSessions.value

    applySessionsDelta('boot-a', 1, 2, [row('b', { title: 'new title' })], [])
    const after = _rawSessions.value
    expect(after[0]).toBe(a0)
    expect(after[2]).toBe(c0)
    expect(after[1]).not.toBe(b0)
    expect(after[1].title).toBe('new title')
    // The projections must still be reachable and consistent after a delta.
    expect(sidebarSessions.value.map(s => s.id).sort()).toEqual(['a', 'b', 'c'])
    expect(folders.value.length).toBe(foldersBefore.length)
    expect(sidebarBefore.length).toBe(3)
  })

  it('is a full no-op (previous array identity) for an empty delta', () => {
    bootstrap(1, [row('a'), row('b')])
    const before = _rawSessions.value
    expect(applySessionsDelta('boot-a', 1, 2, [], [])).toBe(true)
    expect(_rawSessions.value).toBe(before)
  })

  it('refuses a delta that does not chain onto the committed epoch', () => {
    bootstrap(5, [row('a')])
    expect(applySessionsDelta('boot-a', 4, 6, [row('b')], [])).toBe(false)
    expect(_rawSessions.value.map(s => s.id)).toEqual(['a'])
  })

  it('refuses a delta minted in another daemon boot', () => {
    bootstrap(5, [row('a')])
    expect(applySessionsDelta('boot-b', 5, 6, [row('b')], [])).toBe(false)
    expect(_rawSessions.value.map(s => s.id)).toEqual(['a'])
  })

  it('drops the resume point when the daemon reports a new boot id', () => {
    bootstrap(5, [row('a')])
    expect(_deltaResumeState().epoch).toBe(5)
    noteStreamHello('boot-b')
    expect(_deltaResumeState()).toEqual({ bootID: 'boot-b', epoch: 0 })
  })
})

describe('subscription URL', () => {
  it('opts in to deltas and carries the resume point once one exists', () => {
    _resetDeltaResumeState()
    expect(sessionStreamURL()).toBe('/v1/events?session_stream=3&delta=1')
    noteStreamHello('boot-a')
    bootstrap(42, [row('a')])
    expect(sessionStreamURL()).toBe('/v1/events?session_stream=3&delta=1&since=42&boot=boot-a')
  })
})
