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

  it('ignores events from a superseded source and cancels pending work on stop', () => {
    vi.useFakeTimers()
    const { supervisor, sources } = setup()
    supervisor.start()
    sources[0].emit('error')
    supervisor.revalidate()
    expect(sources[0].closed).toBe(true)
    expect(sources).toHaveLength(1)
    vi.advanceTimersByTime(0)
    expect(sources).toHaveLength(2)

    sources[0].emit('error')
    expect(sources).toHaveLength(2)
    supervisor.stop()
    sources[1].emit('error')
    vi.runAllTimers()
    expect(sources).toHaveLength(2)
  })

  it('resets the retry budget after a successful open', () => {
    vi.useFakeTimers()
    const exhausted = vi.fn()
    const { supervisor, sources } = setup({ maxRetryDurationMs: 1000, onExhausted: exhausted })
    supervisor.start()
    sources[0].emit('error')
    vi.advanceTimersByTime(500)
    sources[1].emit('open')
    sources[1].emit('error')
    vi.advanceTimersByTime(500)
    expect(sources).toHaveLength(3)
    expect(exhausted).not.toHaveBeenCalled()
  })

  it('stops automatic retries at the deadline and reports manual retry availability', () => {
    vi.useFakeTimers()
    const exhausted = vi.fn()
    const { supervisor, sources } = setup({ maxRetryDurationMs: 1000, onExhausted: exhausted })
    supervisor.start()
    sources[0].emit('error')
    vi.advanceTimersByTime(500)
    sources[1].emit('error')
    vi.advanceTimersByTime(999)
    expect(exhausted).not.toHaveBeenCalled()
    vi.advanceTimersByTime(1)
    // The second failed attempt's backoff would fire at t=1500, but the
    // deadline is checked before opening another source. No polling timer is
    // needed and no post-deadline attempt is created.
    expect(sources).toHaveLength(2)
    expect(exhausted).toHaveBeenCalledTimes(1)
    expect(sources[1].closed).toBe(true)

    supervisor.retry()
    vi.advanceTimersByTime(0)
    expect(sources).toHaveLength(3)
  })

  it('revalidates an OPEN source on resume without polling in the quiet path', () => {
    vi.useFakeTimers()
    const { supervisor, sources } = setup()
    supervisor.start()
    sources[0].emit('open')
    supervisor.revalidate()
    expect(sources[0].closed).toBe(true)
    vi.advanceTimersByTime(0)
    expect(sources).toHaveLength(2)
  })
})
