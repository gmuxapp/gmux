import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

// Capture the loss handlers and instances created by the mocked WebglAddon so
// tests can drive context-loss events and assert on dispose/recreate.
type MockAddon = {
  dispose: ReturnType<typeof vi.fn>
  fireContextLoss: () => void
}
const created: MockAddon[] = []
let throwOnConstruct = false

vi.mock('@xterm/addon-webgl', () => ({
  WebglAddon: vi.fn().mockImplementation(() => {
    if (throwOnConstruct) throw new Error('no webgl context')
    let lossHandler: (() => void) | null = null
    const addon: any = {
      dispose: vi.fn(),
      onContextLoss: (cb: () => void) => { lossHandler = cb },
    }
    created.push({ dispose: addon.dispose, fireContextLoss: () => lossHandler?.() })
    return addon
  }),
}))

import { loadWebglRenderer } from './webgl-renderer'

// A live terminal: element is connected to the DOM. dispose() in real xterm
// detaches the element (isConnected → false) without nulling it, which is
// exactly the lifecycle the recreate guard keys off.
function makeTerm() {
  return {
    element: { isConnected: true },
    loadAddon: vi.fn(),
  } as any
}

let rafQueue: FrameRequestCallback[]
beforeEach(() => {
  created.length = 0
  throwOnConstruct = false
  rafQueue = []
  vi.useFakeTimers()
  vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback) => {
    rafQueue.push(cb)
    return rafQueue.length
  })
})
afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

// Run queued rAF callbacks synchronously (recreation is deferred to a frame).
function flushRaf() {
  const q = rafQueue
  rafQueue = []
  for (const cb of q) cb(0)
}

// Simulate one full context-loss cycle: the addon loses its context, then the
// deferred recreation frame runs.
function loseContextAndFlush() {
  created[created.length - 1].fireContextLoss()
  flushRaf()
}

describe('loadWebglRenderer', () => {
  it('loads the WebGL addon onto the terminal', () => {
    const term = makeTerm()
    loadWebglRenderer(term)
    expect(term.loadAddon).toHaveBeenCalledTimes(1)
    expect(created).toHaveLength(1)
  })

  it('disposes the dead addon and recreates one on context loss', () => {
    const term = makeTerm()
    loadWebglRenderer(term)

    created[0].fireContextLoss()
    expect(created[0].dispose).toHaveBeenCalledTimes(1)
    // Recreation is deferred to the next frame, not done inside the event.
    expect(created).toHaveLength(1)

    flushRaf()
    expect(created).toHaveLength(2)
    expect(term.loadAddon).toHaveBeenCalledTimes(2)
  })

  it('keeps recovering from losses that are spread out over time', () => {
    const term = makeTerm()
    loadWebglRenderer(term)

    // Ten losses, each well outside the breaker window (e.g. daily
    // suspend/resume). Every one should recover — the breaker must not be a
    // lifetime counter.
    for (let i = 0; i < 10; i++) {
      vi.advanceTimersByTime(2 * 60_000)
      loseContextAndFlush()
    }

    expect(created).toHaveLength(11) // 1 initial + 10 recoveries
    expect(term.loadAddon).toHaveBeenCalledTimes(11)
  })

  it('stops recreating once losses thrash within the window, staying on DOM', () => {
    const term = makeTerm()
    loadWebglRenderer(term)

    // Rapid losses (a few seconds apart) all land inside the 60s window.
    for (let i = 0; i < 6; i++) {
      vi.advanceTimersByTime(3_000)
      loseContextAndFlush()
    }

    // 1 initial + MAX_LOSSES_PER_WINDOW (3) recoveries; the 4th loss trips the
    // breaker and we stay on the DOM renderer.
    expect(created).toHaveLength(4)
    expect(term.loadAddon).toHaveBeenCalledTimes(4)
  })

  it('does not recreate if the terminal was disposed while waiting', () => {
    const term = makeTerm()
    loadWebglRenderer(term)

    created[0].fireContextLoss()
    term.element.isConnected = false // terminal torn down before the frame fires
    flushRaf()

    expect(created).toHaveLength(1)
  })

  it('falls back silently to the DOM renderer when WebGL is unavailable', () => {
    throwOnConstruct = true
    const term = makeTerm()
    expect(() => loadWebglRenderer(term)).not.toThrow()
    expect(term.loadAddon).not.toHaveBeenCalled()
  })

  it('disposes the addon if activate (loadAddon) throws synchronously', () => {
    const term = makeTerm()
    term.loadAddon.mockImplementation(() => { throw new Error('activate failed') })

    expect(() => loadWebglRenderer(term)).not.toThrow()
    expect(created).toHaveLength(1)
    expect(created[0].dispose).toHaveBeenCalledTimes(1)
  })
})
