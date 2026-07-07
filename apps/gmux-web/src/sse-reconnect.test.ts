import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { connState, initStore } from './store'

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

  it('goes to error when the initial connect drops before any snapshot', () => {
    cleanup = initStore()
    expect(connState.value).toBe('connecting')
    source().emit('error')
    expect(connState.value).toBe('error')
  })

  it('goes to reconnecting (not error) when an established stream drops', () => {
    cleanup = initStore()
    // First snapshot establishes the connection.
    source().emit('snapshot.sessions', { sessions: [] })
    expect(connState.value).toBe('connected')
    // The established stream drops: transient, not a hard failure.
    source().emit('error')
    expect(connState.value).toBe('reconnecting')
  })

  it('clears back to connected once the next snapshot arrives', () => {
    cleanup = initStore()
    source().emit('snapshot.sessions', { sessions: [] })
    source().emit('error')
    expect(connState.value).toBe('reconnecting')
    source().emit('snapshot.sessions', { sessions: [] })
    expect(connState.value).toBe('connected')
  })
})
