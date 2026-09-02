import { afterEach, describe, expect, it, vi } from 'vitest'
import { connState, initStore, sseRetryAvailable } from './store'

class FakeSource {
  static instances: FakeSource[] = []
  readyState = 0
  closed = false
  private listeners = new Map<string, ((event: MessageEvent) => void)[]>()

  constructor() {
    FakeSource.instances.push(this)
  }

  addEventListener(type: string, listener: (event: MessageEvent) => void) {
    this.listeners.set(type, [...(this.listeners.get(type) ?? []), listener])
  }

  close() {
    this.closed = true
    this.readyState = 2
  }

  emit(type: string, data: unknown = {}) {
    if (type === 'open') this.readyState = 1
    const event = { data: JSON.stringify(data) } as MessageEvent
    for (const listener of this.listeners.get(type) ?? []) listener(event)
  }

  /** Deliver a complete protocol-3 bootstrap, as the daemon does on subscribe. */
  bootstrap(epoch: number) {
    this.emit('open')
    this.emit('snapshot.sessions.begin', { version: 3, epoch })
    this.emit('snapshot.sessions.ready', { epoch })
  }
}

class FakeEventTarget {
  listeners = new Map<string, (() => void)[]>()
  addEventListener = (type: string, listener: () => void) => {
    this.listeners.set(type, [...(this.listeners.get(type) ?? []), listener])
  }
  removeEventListener = (type: string, listener: () => void) => {
    this.listeners.set(type, (this.listeners.get(type) ?? []).filter(fn => fn !== listener))
  }
  dispatch(type: string) {
    for (const listener of this.listeners.get(type) ?? []) listener()
  }
}

type FakeDocument = FakeEventTarget & { visibilityState: DocumentVisibilityState }

function install() {
  vi.useFakeTimers()
  vi.setSystemTime(1_000)
  FakeSource.instances = []
  const doc = new FakeEventTarget() as FakeDocument
  doc.visibilityState = 'visible'
  const win = new FakeEventTarget()
  vi.stubGlobal('document', doc)
  vi.stubGlobal('window', win)
  vi.stubGlobal('EventSource', FakeSource as unknown as typeof EventSource)
  vi.stubGlobal('fetch', vi.fn(() => Promise.resolve({ ok: true, json: () => Promise.resolve({}) } as Response)))
  const cleanup = initStore()
  /** One logical wake: the browser delivers all four signals, in any order. */
  const wake = () => {
    doc.dispatch('visibilitychange')
    win.dispatch('pageshow')
    doc.dispatch('resume')
    win.dispatch('online')
  }
  return { doc, win, cleanup, wake, sources: FakeSource.instances }
}

describe('SSE store lifecycle wiring', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    vi.useRealTimers()
  })

  it('coalesces a wake into at most one revalidation and skips a healthy stream', () => {
    const { sources, cleanup, wake, doc, win } = install()
    sources[0].bootstrap(1)
    expect(connState.value).toBe('connected')

    wake()
    vi.advanceTimersByTime(999)
    expect(sources).toHaveLength(1)
    vi.advanceTimersByTime(1)
    // The stream is open and recently active: no new full bootstrap is paid.
    expect(sources).toHaveLength(1)

    doc.visibilityState = 'hidden'
    win.dispatch('online')
    doc.dispatch('resume')
    vi.advanceTimersByTime(5_000)
    expect(sources).toHaveLength(1)

    cleanup()
    doc.visibilityState = 'visible'
    wake()
    vi.advanceTimersByTime(5_000)
    expect(sources).toHaveLength(1)
  })

  it('does not arm any work from a hidden-tab online event', () => {
    const { sources, cleanup, wake, doc, win } = install()
    sources[0].bootstrap(1)
    vi.advanceTimersByTime(120_000)

    doc.visibilityState = 'hidden'
    win.dispatch('online')
    vi.advanceTimersByTime(500)
    // Becoming visible later fires visibilitychange, which is the wake that
    // matters. The hidden `online` must not have left a timer behind.
    doc.visibilityState = 'visible'
    vi.advanceTimersByTime(600)
    expect(sources).toHaveLength(1)
    expect(sources[0].closed).toBe(false)

    wake()
    vi.advanceTimersByTime(1_000)
    vi.runOnlyPendingTimers()
    expect(sources).toHaveLength(2)
    cleanup()
  })

  it('does not revalidate when the tab is hidden again inside the debounce window', () => {
    const { sources, cleanup, wake, doc } = install()
    sources[0].bootstrap(1)
    vi.advanceTimersByTime(120_000)

    wake()
    doc.visibilityState = 'hidden'
    vi.advanceTimersByTime(5_000)
    expect(sources).toHaveLength(1)
    expect(sources[0].closed).toBe(false)

    doc.visibilityState = 'visible'
    wake()
    vi.advanceTimersByTime(1_000)
    vi.runOnlyPendingTimers()
    expect(sources).toHaveLength(2)
    cleanup()
  })

  it('treats delivered frames as liveness, so a long-lived tab pays no bootstrap', () => {
    const { sources, cleanup, wake } = install()
    sources[0].bootstrap(1)

    // The daemon sends a session-activity frame (and a heartbeat) on a live
    // stream. A tab open for hours is the normal case: if only the open
    // counted as liveness, every wake past the threshold would re-send the
    // full sessions transaction and world frame.
    for (let i = 0; i < 5; i++) {
      vi.advanceTimersByTime(50_000)
      sources[0].emit('session-activity', { id: 'sess-1' })
      wake()
      vi.advanceTimersByTime(1_000)
      vi.runOnlyPendingTimers()
    }
    expect(sources).toHaveLength(1)
    expect(sources[0].closed).toBe(false)
    expect(connState.value).toBe('connected')

    // Frames stop for longer than the threshold: the next wake revalidates.
    vi.advanceTimersByTime(61_000)
    wake()
    vi.advanceTimersByTime(1_000)
    vi.runOnlyPendingTimers()
    expect(sources).toHaveLength(2)
    cleanup()
  })

  it('replaces a stale stream on a wake and stages the new epoch atomically', () => {
    const { sources, cleanup, wake } = install()
    sources[0].bootstrap(1)
    // Past the staleness threshold: an OPEN readyState is not proof that the
    // TCP stream still delivers bytes after a mobile suspend.
    vi.advanceTimersByTime(120_000)
    wake()
    vi.advanceTimersByTime(1_000)
    expect(sources[0].closed).toBe(true)
    expect(connState.value).toBe('reconnecting')
    vi.runOnlyPendingTimers()
    expect(sources).toHaveLength(2)
    sources[1].bootstrap(1)
    expect(connState.value).toBe('connected')
    cleanup()
  })

  it('ignores an error from a source the supervisor already replaced', () => {
    const { sources, cleanup, wake } = install()
    sources[0].bootstrap(1)
    vi.advanceTimersByTime(120_000)
    wake()
    vi.advanceTimersByTime(1_000)
    vi.runOnlyPendingTimers()
    expect(sources).toHaveLength(2)
    sources[1].bootstrap(2)
    expect(connState.value).toBe('connected')

    // The replaced source's late error must not reset the live transport's
    // staging or park the UI in 'reconnecting' with no retry pending.
    sources[0].emit('error')
    expect(connState.value).toBe('connected')
    expect(sseRetryAvailable.value).toBe(false)
    expect(sources).toHaveLength(2)
    cleanup()
  })

  it('recovers on a wake after the whole retry budget was exhausted', () => {
    const { sources, cleanup, wake } = install()
    sources[0].bootstrap(1)

    // A daemon that keeps failing: every replacement source errors at once.
    // The supervisor's 60 s budget must run out and stop on its own.
    for (let i = 0; i < 400 && !sseRetryAvailable.value; i++) {
      sources[sources.length - 1].emit('error')
      vi.advanceTimersByTime(10_000)
    }
    expect(sseRetryAvailable.value).toBe(true)
    const exhaustedCount = sources.length
    expect(exhaustedCount).toBeLessThan(40)

    // Idle: an exhausted supervisor must not spin in the background.
    vi.advanceTimersByTime(600_000)
    expect(sources).toHaveLength(exhaustedCount)

    // The phone comes back minutes later. No tap: the wake alone must open a
    // new source against the now-healthy daemon.
    wake()
    vi.advanceTimersByTime(1_000)
    vi.runOnlyPendingTimers()
    expect(sources.length).toBeGreaterThan(exhaustedCount)
    sources[sources.length - 1].bootstrap(3)
    expect(connState.value).toBe('connected')
    expect(sseRetryAvailable.value).toBe(false)
    cleanup()
  })
})
