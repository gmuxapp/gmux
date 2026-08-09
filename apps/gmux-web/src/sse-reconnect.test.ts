import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import {
  _rawSessions, appendSessionStreamWarning, connState, initStore,
  sessionStreamOmittedTotal, sessionStreamWarnings,
} from './store'

// A minimal EventSource stand-in that lets the test drive the listeners
// initStore registers (the `error` handler and the snapshot handlers).
class FakeEventSource {
  static instances: FakeEventSource[] = []
  listeners: Record<string, ((e: MessageEvent) => void)[]> = {}
  url: string
  closed = false
  constructor(url: string) {
    this.url = url
    FakeEventSource.instances.push(this)
  }
  addEventListener(type: string, fn: (e: MessageEvent) => void) {
    const list = this.listeners[type] ?? []
    list.push(fn)
    this.listeners[type] = list
  }
  close() { this.closed = true }
  emit(type: string, data?: unknown) {
    for (const fn of this.listeners[type] ?? []) {
      fn({ data: data === undefined ? '' : JSON.stringify(data) } as MessageEvent)
    }
  }
}

describe('SSE reconnecting state', () => {
  let cleanup: () => void

  beforeEach(() => {
    connState.value = 'connecting'
    FakeEventSource.instances = []
    vi.stubGlobal('EventSource', FakeEventSource as unknown as typeof EventSource)
    // fetchFrontendConfig rides a fetch; stub it so initStore doesn't hit
    // the network. The reconnect logic doesn't depend on the result.
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve({
      ok: true,
      json: () => Promise.resolve({}),
    } as Response)))
  })

  afterEach(() => {
    cleanup?.()
    vi.unstubAllGlobals()
  })

  function source(): FakeEventSource {
    return FakeEventSource.instances[0]
  }

  function ready(epoch = 1, sessions: unknown[] = []) {
    source().emit('snapshot.sessions.begin', { version: 3, epoch })
    if (sessions.length > 0) source().emit('snapshot.sessions.batch', { epoch, sessions })
    source().emit('snapshot.sessions.ready', { epoch })
  }

  it('explicitly requests protocol 3', () => {
    cleanup = initStore()
    expect(source().url).toBe('/v1/events?session_stream=3')
  })

  it('accepts the transitional legacy replacement on a legacy-only transport', () => {
    cleanup = initStore()
    source().emit('snapshot.sessions', { sessions: [
      { id: 'legacy', adapter: 'shell', alive: true, status: null, unread: false, unread_token: '' },
    ] })
    expect(_rawSessions.value.map(s => s.id)).toEqual(['legacy'])
    expect(connState.value).toBe('connected')
    // Once legacy is selected, protocol 3 cannot take over this transport.
    ready(1, [{ id: 'replayed', adapter: 'shell', alive: true, status: null, unread: false, unread_token: '' }])
    expect(_rawSessions.value.map(s => s.id)).toEqual(['legacy'])
  })

  it('locks a negotiated transport to protocol 3 across legacy injection and stale replay', () => {
    cleanup = initStore()
    ready(1, [{ id: 'old', adapter: 'shell', alive: true, status: null, unread: false, unread_token: '' }])
    ready(2, [{ id: 'new', adapter: 'shell', alive: true, status: null, unread: false, unread_token: '' }])
    source().emit('snapshot.sessions', { sessions: [
      { id: 'legacy-current', adapter: 'shell', alive: true, status: null, unread: false, unread_token: '' },
    ] })
    ready(1, [{ id: 'replayed-old', adapter: 'shell', alive: true, status: null, unread: false, unread_token: '' }])
    expect(_rawSessions.value.map(s => s.id)).toEqual(['new'])
  })

  it('goes to error when the initial connect drops before any snapshot', () => {
    cleanup = initStore()
    expect(connState.value).toBe('connecting')
    source().emit('error')
    expect(connState.value).toBe('error')
  })

  it('goes to reconnecting (not error) when an established stream drops', () => {
    cleanup = initStore()
    // First ready marker establishes the connection.
    ready()
    expect(connState.value).toBe('connected')
    // The established stream drops: transient, not a hard failure.
    source().emit('error')
    expect(connState.value).toBe('reconnecting')
  })

  it('clears back to connected once the next snapshot arrives', () => {
    cleanup = initStore()
    ready()
    source().emit('error')
    expect(connState.value).toBe('reconnecting')
    ready(2)
    expect(connState.value).toBe('connected')
  })

  it('discards an interrupted bootstrap without exposing partial rows', () => {
    cleanup = initStore()
    ready(1, [{ id: 'old', adapter: 'shell', alive: true, status: null, unread: false, unread_token: '' }])
    source().emit('snapshot.sessions.begin', { version: 3, epoch: 2 })
    source().emit('snapshot.sessions.batch', { epoch: 2, sessions: [
      { id: 'partial', adapter: 'shell', alive: true, status: null, unread: false, unread_token: '' },
    ] })
    expect(_rawSessions.value.map(s => s.id)).toEqual(['old'])
    source().emit('error')
    expect(_rawSessions.value.map(s => s.id)).toEqual(['old'])
    // No replacement begin: if the error handler retained staging, these two
    // events would publish "partial". They must be inert and release it.
    source().emit('snapshot.sessions.batch', { epoch: 2, sessions: [] })
    source().emit('snapshot.sessions.ready', { epoch: 2 })
    expect(_rawSessions.value.map(s => s.id)).toEqual(['old'])
    // Epochs restart on the new transport.
    ready(1, [{ id: 'fresh', adapter: 'shell', alive: true, status: null, unread: false, unread_token: '' }])
    expect(_rawSessions.value.map(s => s.id)).toEqual(['fresh'])
  })

  it('ignores duplicate and stale epochs without rolling back or destroying newer staging', () => {
    cleanup = initStore()
    ready(1, [{ id: 'old', adapter: 'shell', alive: true, status: null, unread: false, unread_token: '' }])
    ready(2, [{ id: 'new', adapter: 'shell', alive: true, status: null, unread: false, unread_token: '' }])
    ready(1, [{ id: 'replayed', adapter: 'shell', alive: true, status: null, unread: false, unread_token: '' }])
    expect(_rawSessions.value.map(s => s.id)).toEqual(['new'])

    source().emit('snapshot.sessions.begin', { version: 3, epoch: 3 })
    source().emit('snapshot.sessions.begin', { version: 3, epoch: 2 })
    source().emit('snapshot.sessions.batch', { epoch: 3, sessions: [
      { id: 'newest', adapter: 'shell', alive: true, status: null, unread: false, unread_token: '' },
    ] })
    source().emit('snapshot.sessions.ready', { epoch: 3 })
    expect(_rawSessions.value.map(s => s.id)).toEqual(['newest'])
  })

  it('keeps a degraded epoch valid after a quarantined-row diagnostic', () => {
    cleanup = initStore()
    source().emit('snapshot.sessions.begin', { version: 3, epoch: 1 })
    source().emit('snapshot.sessions.batch', { epoch: 1, sessions: [
      { id: 'good', adapter: 'shell', alive: true, status: null, unread: false, unread_token: '' },
    ] })
    source().emit('snapshot.sessions.error', { epoch: 1, code: 'row_too_large', id: 'bad', message: 'omitted' })
    source().emit('snapshot.sessions.ready', { epoch: 1 })
    expect(_rawSessions.value.map(s => s.id)).toEqual(['good'])
    expect(connState.value).toBe('connected')
    expect(sessionStreamWarnings.value).toEqual([
      { code: 'row_too_large', id: 'bad', message: 'omitted', count: 1 },
    ])
    expect(sessionStreamOmittedTotal.value).toBe(1)
    ready(2, [{ id: 'good', adapter: 'shell', alive: true, status: null, unread: false, unread_token: '' }])
    expect(sessionStreamWarnings.value).toEqual([])
    expect(sessionStreamOmittedTotal.value).toBe(0)
  })

  it('keeps the exact omitted total above the bounded detail cap', () => {
    cleanup = initStore()
    source().emit('snapshot.sessions.begin', { version: 3, epoch: 1 })
    for (let i = 0; i < 256; i++) {
      appendSessionStreamWarning(1, {
        id: `safe-${i}`, code: 'row_too_large', message: 'omitted', count: 1,
      })
    }
    // The sender emits this no-ID summary after its 256 detailed diagnostics.
    // It must update the total even though it cannot consume a detail slot.
    appendSessionStreamWarning(1, {
      id: '', code: 'diagnostics_suppressed', message: '44 additional rows omitted', count: 44,
    })
    source().emit('snapshot.sessions.ready', { epoch: 1 })
    expect(sessionStreamOmittedTotal.value).toBe(300)
    expect(sessionStreamWarnings.value).toHaveLength(256)
  })

  it('publishes a mutation captured during bootstrap only at its later ready', () => {
    cleanup = initStore()
    source().emit('snapshot.sessions.begin', { version: 3, epoch: 1 })
    source().emit('snapshot.sessions.batch', { epoch: 1, sessions: [
      { id: 's', title: 'before', adapter: 'shell', alive: true, status: null, unread: false, unread_token: '' },
    ] })
    // The fanout captures this replacement after subscribe; it is serialized
    // after epoch 1 and cannot overtake epoch 1's ready marker.
    source().emit('snapshot.sessions.ready', { epoch: 1 })
    expect(_rawSessions.value.map(s => s.title)).toEqual(['before'])
    source().emit('snapshot.sessions.begin', { version: 3, epoch: 2 })
    source().emit('snapshot.sessions.batch', { epoch: 2, sessions: [
      { id: 's', title: 'after', adapter: 'shell', alive: true, status: null, unread: false, unread_token: '' },
    ] })
    expect(_rawSessions.value.map(s => s.title)).toEqual(['before'])
    source().emit('snapshot.sessions.ready', { epoch: 2 })
    expect(_rawSessions.value).toHaveLength(1)
    expect(_rawSessions.value[0].title).toBe('after')
  })
})
