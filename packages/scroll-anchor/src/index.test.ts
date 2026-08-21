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
  const scrollToLine = vi.fn((line: number) => { viewportY = Math.max(0, Math.min(line, baseY)) })
  const scrollToBottom = vi.fn(() => { viewportY = baseY })
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
    userScroll(line: number, event = 'wheel') {
      element.dispatchEvent(new Event(event))
      viewportY = Math.max(0, Math.min(line, baseY))
      for (const cb of scrollListeners) cb()
    },
    outputScroll(line: number) {
      viewportY = Math.max(0, Math.min(line, baseY))
      for (const cb of scrollListeners) cb()
    },
    csi(key: string, params: Array<number | number[]>) { csi.get(key)?.(params) },
    parsed() { for (const cb of writeListeners) cb() },
    setAlternate(value: boolean) { type = value ? 'alternate' : 'normal' },
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
})
