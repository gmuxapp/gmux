import { afterEach, describe, expect, it, vi } from 'vitest'
import { createSSESupervisor, type SSESource } from './sse-supervisor'

class FakeSource implements SSESource {
  readyState = 0
  closed = false
  private listeners = new Map<string, ((event: MessageEvent) => void)[]>()

  addEventListener(type: string, listener: (event: MessageEvent) => void) {
    this.listeners.set(type, [...(this.listeners.get(type) ?? []), listener])
  }

  close() {
    this.closed = true
    this.readyState = 2
  }

  emit(type: 'open' | 'error') {
    if (type === 'open') this.readyState = 1
    for (const listener of this.listeners.get(type) ?? []) listener(new MessageEvent(type))
  }
}

describe('SSE supervisor', () => {
  afterEach(() => vi.useRealTimers())

  function setup(options: Partial<Parameters<typeof createSSESupervisor>[0]> = {}) {
    const sources: FakeSource[] = []
    const supervisor = createSSESupervisor({
      connect: callbacks => {
        const source = new FakeSource()
        source.addEventListener('open', callbacks.opened)
        source.addEventListener('error', callbacks.failed)
        sources.push(source)
        return source
      },
      random: () => 0.5,
      ...options,
    })
    return { supervisor, sources }
  }

  it('replaces a source after a fatal error instead of trusting native retry', () => {
    vi.useFakeTimers()
    const scheduled = vi.fn()
    const { supervisor, sources } = setup({ onRetryScheduled: scheduled })

    supervisor.start()
    sources[0].emit('error')
    expect(sources[0].closed).toBe(true)
    expect(scheduled).toHaveBeenCalledTimes(1)
    expect(sources).toHaveLength(1)

    vi.advanceTimersByTime(499)
    expect(sources).toHaveLength(1)
    vi.advanceTimersByTime(1)
    expect(sources).toHaveLength(2)
  })

  it('ignores a superseded source error without closing the live source or scheduling', () => {
    vi.useFakeTimers()
    const scheduled = vi.fn()
    const { supervisor, sources } = setup({ onRetryScheduled: scheduled })
    supervisor.start()
    sources[0].emit('error')
    expect(scheduled).toHaveBeenCalledTimes(1)
    vi.advanceTimersByTime(500)
    expect(sources).toHaveLength(2)
    sources[1].emit('open')

    // The replaced source's error must not touch the live source's state.
    sources[0].emit('error')
    expect(sources[1].closed).toBe(false)
    expect(scheduled).toHaveBeenCalledTimes(1)
    vi.runOnlyPendingTimers()
    expect(sources).toHaveLength(2)
  })

  it('does not let a late open from a superseded source credit the retry budget', () => {
    vi.useFakeTimers()
    const exhausted = vi.fn()
    const { supervisor, sources } = setup({
      maxRetryDurationMs: 1000, stableOpenDurationMs: 200, onExhausted: exhausted,
    })
    supervisor.start()
    sources[0].emit('error')
    vi.advanceTimersByTime(500)
    expect(sources).toHaveLength(2)

    // A late open belonging to the replaced generation is not evidence that
    // the live source is healthy: it must not count as a stable open.
    sources[0].emit('open')
    vi.advanceTimersByTime(700)
    sources[1].emit('error')
    expect(exhausted).toHaveBeenCalledTimes(1)
    vi.advanceTimersByTime(1000)
    expect(sources).toHaveLength(2)
  })

  it('lets a pending retry absorb a wake instead of adding a source', () => {
    vi.useFakeTimers()
    const { supervisor, sources } = setup()
    supervisor.start()
    sources[0].emit('error')
    supervisor.revalidate()
    vi.advanceTimersByTime(0)
    expect(sources).toHaveLength(1)
    vi.advanceTimersByTime(500)
    expect(sources).toHaveLength(2)

    supervisor.stop()
    sources[1].emit('error')
    vi.runAllTimers()
    expect(sources).toHaveLength(2)
  })

  it('cancels a pending automatic retry when a manual retry replaces it', () => {
    vi.useFakeTimers()
    const { supervisor, sources } = setup()
    supervisor.start()
    sources[0].emit('error')
    supervisor.retry()
    vi.advanceTimersByTime(0)
    expect(sources).toHaveLength(2)
    // The superseded backoff timer must not open a second live source.
    vi.advanceTimersByTime(1000)
    expect(sources).toHaveLength(2)
    expect(sources.filter(source => !source.closed)).toHaveLength(1)
  })

  it('does not stack sources on repeated wakes, but replaces a wedged attempt', () => {
    vi.useFakeTimers()
    const { supervisor, sources } = setup({ attemptTimeoutMs: 1000 })
    supervisor.start()
    expect(sources[0].readyState).toBe(0)
    supervisor.revalidate()
    supervisor.revalidate()
    vi.advanceTimersByTime(0)
    expect(sources).toHaveLength(1)

    vi.advanceTimersByTime(1001)
    supervisor.revalidate()
    vi.advanceTimersByTime(0)
    expect(sources).toHaveLength(2)
    expect(sources[0].closed).toBe(true)
  })

  it('keeps short-lived opens in the same bounded retry window', () => {
    vi.useFakeTimers()
    const exhausted = vi.fn()
    const { supervisor, sources } = setup({ maxRetryDurationMs: 1000, stableOpenDurationMs: 500, onExhausted: exhausted })
    supervisor.start()
    sources[0].emit('error')
    vi.advanceTimersByTime(500)
    sources[1].emit('open')
    vi.advanceTimersByTime(100)
    sources[1].emit('error')
    vi.advanceTimersByTime(1000)
    expect(exhausted).toHaveBeenCalledTimes(1)
    expect(sources).toHaveLength(2)
  })

  it('resets the retry budget only after a stable open', () => {
    vi.useFakeTimers()
    const exhausted = vi.fn()
    const { supervisor, sources } = setup({ maxRetryDurationMs: 1000, stableOpenDurationMs: 500, onExhausted: exhausted })
    supervisor.start()
    sources[0].emit('error')
    vi.advanceTimersByTime(500)
    sources[1].emit('open')
    vi.advanceTimersByTime(500) // stable-open timer credits this source
    sources[1].emit('error')
    vi.advanceTimersByTime(500)
    expect(sources).toHaveLength(3)
    expect(exhausted).not.toHaveBeenCalled()
  })

  it('restarts after a wake even when the previous retry window was exhausted', () => {
    vi.useFakeTimers()
    const exhausted = vi.fn()
    const { supervisor, sources } = setup({ maxRetryDurationMs: 1000, onExhausted: exhausted })
    supervisor.start()
    sources[0].emit('error')
    vi.advanceTimersByTime(500)
    sources[1].emit('error')
    vi.advanceTimersByTime(1000)
    expect(exhausted).toHaveBeenCalledTimes(1)
    expect(sources).toHaveLength(2)

    supervisor.revalidate()
    vi.advanceTimersByTime(0)
    expect(sources).toHaveLength(3)
    expect(sources[2].closed).toBe(false)
  })

  it('does not replace a recently active source, but revalidates a stale one', () => {
    vi.useFakeTimers()
    const { supervisor, sources } = setup({ staleDurationMs: 1000 })
    supervisor.start()
    sources[0].emit('open')
    supervisor.revalidate()
    vi.advanceTimersByTime(0)
    expect(sources[0].closed).toBe(false)
    expect(sources).toHaveLength(1)

    vi.advanceTimersByTime(1001)
    supervisor.revalidate()
    vi.advanceTimersByTime(0)
    expect(sources[0].closed).toBe(true)
    expect(sources).toHaveLength(2)
  })
})
