import { describe, expect, it, vi } from 'vitest'
import { resolveScrollAnchor, ScrollAnchorAddon, type ScrollAnchorSnapshot } from './index.js'

function buffer(lines: Array<string | null>, baseY: number, rows: number) {
  return { baseY, rows, getLine: (y: number) => lines[y] ?? null }
}

describe('resolveScrollAnchor', () => {
  const snap = (line: string | null, distanceFromBottom: number): ScrollAnchorSnapshot => ({ line, distanceFromBottom })

  it('matches content across the whole buffer', () => {
    const lines = Array<string | null>(30).fill(null)
    lines[7] = 'recognizable line'
    expect(resolveScrollAnchor(snap('recognizable line', 2), buffer(lines, 20, 10))).toBe(7)
  })

  it('tiebreaks repeated content by distance from bottom', () => {
    const lines = Array<string | null>(30).fill(null)
    lines[12] = lines[16] = lines[18] = 'session ready'
    expect(resolveScrollAnchor(snap('session ready', 5), buffer(lines, 20, 10))).toBe(16)
  })

  it('maps a visible-region match to bottom', () => {
    const lines = Array<string | null>(45).fill(null)
    lines[20] = 'visible anchor'
    expect(resolveScrollAnchor(snap('visible anchor', 5), buffer(lines, 5, 40))).toBe(5)
  })

  it('falls back to distance for null or missing anchors', () => {
    expect(resolveScrollAnchor(snap(null, 3), buffer([], 20, 40))).toBe(17)
    expect(resolveScrollAnchor(snap('gone', 3), buffer([], 20, 40))).toBe(17)
  })

  it('falls back to bottom when distance no longer fits', () => {
    expect(resolveScrollAnchor(snap(null, 100), buffer([], 15, 40))).toBe(15)
  })
})

function makeHarness() {
  const element = new EventTarget()
  const lines = new Map<number, string>()
  const scrollListeners = new Set<() => void>()
  const writeListeners = new Set<() => void>()
  const csi = new Map<string, (params: Array<number | number[]>) => boolean>()
  let viewportY = 0
  let baseY = 0
  let type: 'normal' | 'alternate' = 'normal'
  const emitScroll = () => { for (const cb of scrollListeners) cb() }
  const scrollToLine = vi.fn((line: number) => {
    viewportY = Math.max(0, Math.min(line, baseY))
    emitScroll()
  })
  const scrollToBottom = vi.fn(() => {
    viewportY = baseY
    emitScroll()
  })
  const disposable = (set: Set<() => void>, cb: () => void) => ({ dispose: () => set.delete(cb) })
  const active = {
    get type() { return type },
    get viewportY() { return viewportY },
    get baseY() { return baseY },
    getLine(y: number) {
      const text = lines.get(y)
      return text === undefined ? undefined : { translateToString: () => text }
    },
  }
  const terminal = {
    element,
    rows: 10,
    buffer: { active },
    scrollToLine,
    scrollToBottom,
    onScroll(cb: () => void) { scrollListeners.add(cb); return disposable(scrollListeners, cb) },
    onWriteParsed(cb: () => void) { writeListeners.add(cb); return disposable(writeListeners, cb) },
    parser: {
      registerCsiHandler(id: { prefix?: string, final: string }, cb: (params: Array<number | number[]>) => boolean) {
        const key = `${id.prefix ?? ''}${id.final}`
        csi.set(key, cb)
        return { dispose: () => csi.delete(key) }
      },
    },
  }
  const addon = new ScrollAnchorAddon()
  addon.activate(terminal as any)

  return {
    addon,
    lines,
    scrollToLine,
    scrollToBottom,
    setBuffer(vy: number, by: number) { viewportY = vy; baseY = by },
    armIntent(event = 'wheel') { element.dispatchEvent(new Event(event)) },
    userScroll(line: number, event = 'wheel') {
      element.dispatchEvent(new Event(event))
      viewportY = Math.max(0, Math.min(line, baseY))
      emitScroll()
    },
    async asyncUserScroll(line: number) {
      element.dispatchEvent(new Event('wheel'))
      viewportY = Math.max(0, Math.min(line, baseY))
      await new Promise(resolve => setTimeout(resolve, 1))
      emitScroll()
    },
    outputScroll(line: number) {
      viewportY = Math.max(0, Math.min(line, baseY))
      emitScroll()
    },
    emitScroll,
    csi(key: string, params: Array<number | number[]>) { csi.get(key)?.(params) },
    parsed() { for (const cb of writeListeners) cb() },
    setAlternate(value: boolean) { type = value ? 'alternate' : 'normal' },
    listenerCount() { return scrollListeners.size + writeListeners.size + csi.size },
  }
}

describe('ScrollAnchorAddon', () => {
  it('keeps following across output and wipes', () => {
    const h = makeHarness()
    h.setBuffer(20, 20)
    h.csi('?h', [2026])
    h.setBuffer(0, 25)
    h.csi('J', [3])
    h.csi('?l', [[2026]])
    h.parsed()
    expect(h.addon.mode).toBe('following')
    expect(h.scrollToBottom).toHaveBeenCalledTimes(1)
  })

  it('anchors when the user wheels up in an open block and never re-pins', () => {
    const h = makeHarness()
    h.setBuffer(20, 20)
    h.csi('?h', [2026])
    h.userScroll(12)
    h.setBuffer(12, 25)
    h.csi('?l', [2026])
    h.parsed()
    expect(h.addon.mode).toBe('anchored')
    expect(h.scrollToBottom).not.toHaveBeenCalled()
    expect(h.scrollToLine).not.toHaveBeenCalled()
  })

  it('does not revert a wheel between block close and write resolution', () => {
    const h = makeHarness()
    h.setBuffer(20, 20)
    h.csi('?h', [2026])
    h.csi('?l', [2026])
    h.userScroll(11)
    h.parsed()
    expect(h.addon.mode).toBe('anchored')
    expect(h.scrollToBottom).not.toHaveBeenCalled()
  })

  it('performs zero programmatic scrolls for anchored streaming without a wipe', () => {
    const h = makeHarness()
    h.setBuffer(20, 20)
    h.userScroll(10)
    h.setBuffer(10, 25)
    h.outputScroll(10)
    h.parsed()
    expect(h.scrollToLine).not.toHaveBeenCalled()
    expect(h.scrollToBottom).not.toHaveBeenCalled()
  })

  it('re-resolves a bare ED3 outside synchronized output', () => {
    const h = makeHarness()
    h.setBuffer(20, 20)
    h.lines.set(15, 'anchor-worthy line')
    h.userScroll(15)
    h.csi('J', [3])
    h.lines.clear()
    h.lines.set(8, 'anchor-worthy line')
    h.setBuffer(0, 12)
    h.parsed()
    expect(h.scrollToLine).toHaveBeenCalledWith(8)
  })

  it('uses distance after a wipe when the top line is trivial', () => {
    const h = makeHarness()
    h.setBuffer(17, 20)
    h.lines.set(17, '---')
    h.userScroll(17)
    h.csi('J', [3])
    h.setBuffer(0, 10)
    h.parsed()
    expect(h.scrollToLine).toHaveBeenCalledWith(7)
  })

  it('suspends transitions and enforcement in the alternate buffer', () => {
    const h = makeHarness()
    h.setBuffer(20, 20)
    h.setAlternate(true)
    h.userScroll(10)
    h.setBuffer(0, 20)
    h.parsed()
    expect(h.addon.mode).toBe('following')
    expect(h.scrollToBottom).not.toHaveBeenCalled()
  })

  it('keeps wheel intent latched until an asynchronous onScroll', async () => {
    const h = makeHarness()
    h.setBuffer(20, 20)
    await h.asyncUserScroll(12)
    expect(h.addon.mode).toBe('anchored')
  })

  it('follows structurally when a non-wheel scroll lands at bottom', () => {
    const h = makeHarness()
    h.setBuffer(20, 20)
    h.userScroll(10)
    expect(h.addon.mode).toBe('anchored')
    h.outputScroll(20)
    expect(h.addon.mode).toBe('following')
  })

  it('preserves a near-bottom user anchor', () => {
    const h = makeHarness()
    h.setBuffer(20, 20)
    h.userScroll(18)
    h.setBuffer(18, 25)
    h.parsed()
    expect(h.addon.mode).toBe('anchored')
    expect(h.scrollToBottom).not.toHaveBeenCalled()
    expect(h.scrollToLine).not.toHaveBeenCalled()
  })

  it('follow scrolls immediately and emits mode changes once', () => {
    const h = makeHarness()
    const modes: string[] = []
    h.addon.onModeChange(mode => { modes.push(mode) })
    h.setBuffer(20, 20)
    h.userScroll(10)
    h.addon.follow()
    h.addon.follow()
    expect(modes).toEqual(['anchored', 'following'])
    expect(h.scrollToBottom).toHaveBeenCalledTimes(2)
    expect(h.addon.mode).toBe('following')
  })

  it('treats repeated BSU as idempotent and one ESU closes it', () => {
    const h = makeHarness()
    const changes: boolean[] = []
    h.addon.onSyncActiveChange(active => { changes.push(active) })
    h.csi('?h', [2026])
    h.csi('?h', [[2026]])
    expect(h.addon.syncActive).toBe(true)
    h.csi('?l', [2026])
    h.csi('?l', [2026])
    expect(h.addon.syncActive).toBe(false)
    expect(changes).toEqual([true, false])
  })

  it('reset clears unmatched sync, wipe, snapshot, and latched intent', () => {
    const h = makeHarness()
    const sync: boolean[] = []
    h.addon.onSyncActiveChange(active => { sync.push(active) })
    h.setBuffer(20, 20)
    h.userScroll(10)
    h.csi('?h', [2026])
    h.csi('J', [3])
    h.armIntent()
    h.addon.reset()
    expect(h.addon.syncActive).toBe(false)
    expect(h.addon.busy).toBe(false)
    expect(sync).toEqual([true, false])
    h.outputScroll(8)
    expect(h.addon.mode).toBe('anchored')
    h.parsed()
    expect(h.scrollToLine).not.toHaveBeenCalled()
  })

  it('only treats ED3 when 3 is the first parameter', () => {
    const h = makeHarness()
    h.setBuffer(20, 20)
    h.userScroll(10)
    h.csi('J', [2, 3])
    h.setBuffer(0, 10)
    h.parsed()
    expect(h.scrollToLine).not.toHaveBeenCalled()
  })

  it('keeps busy true through the wipe rAF and honors user bottom intent', () => {
    let raf: FrameRequestCallback | null = null
    vi.stubGlobal('requestAnimationFrame', (callback: FrameRequestCallback) => { raf = callback; return 1 })
    vi.stubGlobal('cancelAnimationFrame', () => { raf = null })
    const h = makeHarness()
    const busy: boolean[] = []
    h.addon.onBusyChange(value => { busy.push(value) })
    h.setBuffer(20, 20)
    h.userScroll(10)
    h.csi('J', [3])
    h.setBuffer(0, 12)
    h.parsed()
    expect(h.addon.busy).toBe(true)
    expect(busy).toEqual([true])
    h.userScroll(12)
    expect(h.addon.mode).toBe('following')
    expect(h.addon.busy).toBe(false)
    expect(raf).toBeNull()
    expect(busy).toEqual([true, false])
    vi.unstubAllGlobals()
  })

  it('emits busy false only after the wipe rendering catch-up', () => {
    let raf: FrameRequestCallback | null = null
    vi.stubGlobal('requestAnimationFrame', (callback: FrameRequestCallback) => { raf = callback; return 1 })
    const h = makeHarness()
    const busy: boolean[] = []
    h.addon.onBusyChange(value => { busy.push(value) })
    h.setBuffer(20, 20)
    h.userScroll(10)
    h.csi('?h', [2026])
    h.csi('J', [3])
    h.setBuffer(0, 12)
    h.csi('?l', [2026])
    h.parsed()
    expect(h.addon.busy).toBe(true)
    expect(busy).toEqual([true])
    raf?.(0)
    expect(h.addon.busy).toBe(false)
    expect(busy).toEqual([true, false])
    vi.unstubAllGlobals()
  })

  it('dispose cancels pending work and removes listeners', () => {
    let cancelled = false
    vi.stubGlobal('requestAnimationFrame', () => 7)
    vi.stubGlobal('cancelAnimationFrame', (id: number) => { cancelled = id === 7 })
    const h = makeHarness()
    h.setBuffer(20, 20)
    h.userScroll(10)
    h.csi('J', [3])
    h.setBuffer(0, 12)
    h.parsed()
    expect(h.listenerCount()).toBeGreaterThan(0)
    h.addon.dispose()
    expect(cancelled).toBe(true)
    expect(h.listenerCount()).toBe(0)
    vi.unstubAllGlobals()
  })
})
