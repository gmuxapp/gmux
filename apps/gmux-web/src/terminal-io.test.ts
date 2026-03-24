import { describe, expect, it, vi } from 'vitest'
import { createTerminalIO, type ScrollAccessor } from './terminal-io'
import { BSU, ESU } from './replay'

function makeHarness() {
  const writes: Array<string> = []
  const resizes: Array<{ cols: number, rows: number }> = []
  const pending: Array<() => void> = []

  const io = createTerminalIO({
    write(data, callback) {
      writes.push(typeof data === 'string' ? data : new TextDecoder().decode(data))
      pending.push(() => callback?.())
    },
    resize(cols, rows) {
      resizes.push({ cols, rows })
    },
  })

  return {
    io,
    writes,
    resizes,
    flushOne() {
      const cb = pending.shift()
      if (!cb) throw new Error('no pending write callback')
      cb()
    },
    flushAll() {
      while (pending.length) pending.shift()?.()
    },
  }
}

/**
 * Simulates xterm's scroll buffer behavior for testing scroll preservation.
 *
 * Models viewportY/baseY and the effect of scrollback eviction: when
 * scrollback is full, adding lines evicts from the top and decrements
 * viewportY so the same content stays visible.
 */
function makeScrollHarness(opts: { scrollbackLimit: number; rows: number }) {
  const { scrollbackLimit, rows } = opts
  let totalLines = rows // start with one screenful
  let viewportY = 0
  let baseY = 0

  const scrollToLineCalls: number[] = []
  const scrollToBottomCalls: number[] = [] // tracks call count

  const scroll: ScrollAccessor = {
    getState: () => ({ viewportY, baseY }),
    scrollToLine(line: number) {
      scrollToLineCalls.push(line)
      viewportY = Math.max(0, Math.min(line, baseY))
    },
    scrollToBottom() {
      scrollToBottomCalls.push(baseY)
      viewportY = baseY
    },
  }

  const writes: string[] = []
  const pending: Array<() => void> = []
  // rAF callbacks queued by the production code
  const rafQueue: Array<() => void> = []

  // Stub requestAnimationFrame/cancelAnimationFrame for deterministic tests.
  let rafId = 0
  const rafMap = new Map<number, () => void>()
  vi.stubGlobal('requestAnimationFrame', (cb: () => void) => {
    const id = ++rafId
    rafMap.set(id, cb)
    rafQueue.push(cb)
    return id
  })
  vi.stubGlobal('cancelAnimationFrame', (id: number) => {
    const cb = rafMap.get(id)
    if (cb) {
      rafMap.delete(id)
      const idx = rafQueue.indexOf(cb)
      if (idx >= 0) rafQueue.splice(idx, 1)
    }
  })

  const resizeCalls: Array<{ cols: number; rows: number }> = []
  /** Optional callback to simulate xterm's resize side-effects (e.g. viewportY changes). */
  let onResize: ((cols: number, rows: number) => void) | null = null

  const io = createTerminalIO(
    {
      write(data, callback) {
        writes.push(typeof data === 'string' ? data : new TextDecoder().decode(data))
        pending.push(() => callback?.())
      },
      resize(cols, rows) {
        resizeCalls.push({ cols, rows })
        onResize?.(cols, rows)
      },
    },
    scroll,
  )

  return {
    io,
    writes,
    scroll,
    scrollToLineCalls,
    scrollToBottomCalls,
    resizeCalls,
    /** Set a callback to simulate xterm viewport side-effects during resize. */
    set onResize(fn: ((cols: number, rows: number) => void) | null) { onResize = fn },

    /** Current scroll state (convenience). */
    get viewportY() { return viewportY },
    get baseY() { return baseY },

    /** Simulate the user scrolling to a specific line. */
    userScrollTo(line: number) {
      viewportY = Math.max(0, Math.min(line, baseY))
    },

    /** Simulate the user scrolling to the bottom. */
    userScrollToBottom() {
      viewportY = baseY
    },

    /**
     * Simulate xterm processing new output lines, including scrollback
     * eviction. Call this between enqueue and flushOne to model what xterm
     * does during write().
     */
    addLines(count: number) {
      totalLines += count
      const overflow = totalLines - (scrollbackLimit + rows)
      if (overflow > 0) {
        const evicted = overflow
        totalLines = scrollbackLimit + rows
        baseY = scrollbackLimit
        // xterm decrements viewportY when lines are evicted
        viewportY = Math.max(0, viewportY - evicted)
      } else {
        baseY = Math.max(0, totalLines - rows)
      }
    },

    /**
     * Flush one pending write callback. Optionally simulate xterm adding
     * lines (scrollback eviction) as part of the write processing.
     */
    flushOne(linesAdded = 0) {
      const cb = pending.shift()
      if (!cb) throw new Error('no pending write callback')
      // Simulate xterm processing the write: adjust buffer state
      if (linesAdded > 0) this.addLines(linesAdded)
      cb()
    },

    /**
     * Run all queued rAF callbacks (our scroll restore runs here).
     *
     * Note: xterm's viewport catch-up after ESU does NOT change ydisp; it
     * only updates the DOM scrollTop to reflect the existing ydisp (with
     * _suppressOnScrollHandler to prevent feedback). So we don't simulate
     * any ydisp snap here; the buffer state from flushOne is already final.
     */
    flushRAF() {
      const queue = [...rafQueue]
      rafQueue.length = 0
      rafMap.clear()
      for (const cb of queue) cb()
    },

    cleanup() {
      vi.unstubAllGlobals()
    },
  }
}

const enc = (s: string) => new TextEncoder().encode(s)

/** Wrap payload in BSU + ESU markers. */
function wrapBSU(payload: string): Uint8Array {
  const payloadBytes = enc(payload)
  const result = new Uint8Array(BSU.length + payloadBytes.length + ESU.length)
  result.set(BSU, 0)
  result.set(payloadBytes, BSU.length)
  result.set(ESU, BSU.length + payloadBytes.length)
  return result
}

describe('createTerminalIO', () => {
  it('serializes writes one at a time', () => {
    const h = makeHarness()
    h.io.reset(1)

    h.io.enqueue(enc('a'), 1)
    h.io.enqueue(enc('b'), 1)
    h.io.enqueue(enc('c'), 1)

    expect(h.writes).toEqual(['a'])

    h.flushOne()
    expect(h.writes).toEqual(['a', 'b'])

    h.flushOne()
    expect(h.writes).toEqual(['a', 'b', 'c'])
  })

  it('waits for queued writes before resizing', () => {
    const h = makeHarness()
    h.io.reset(1)

    h.io.enqueue(enc('hello'), 1)
    h.io.requestResize({ cols: 120, rows: 40 }, 1)

    expect(h.resizes).toEqual([])
    h.flushOne()
    expect(h.resizes).toEqual([{ cols: 120, rows: 40 }])
  })

  it('coalesces to the latest pending resize', () => {
    const h = makeHarness()
    h.io.reset(1)

    h.io.enqueue(enc('hello'), 1)
    h.io.requestResize({ cols: 100, rows: 30 }, 1)
    h.io.requestResize({ cols: 140, rows: 50 }, 1)

    h.flushOne()
    expect(h.resizes).toEqual([{ cols: 140, rows: 50 }])
  })

  it('drops stale queued writes and resizes after epoch reset', () => {
    const h = makeHarness()
    const onWritten = vi.fn()

    h.io.reset(1)
    h.io.enqueue(enc('stale'), 1, onWritten)
    h.io.requestResize({ cols: 90, rows: 20 }, 1)
    h.io.reset(2)
    h.io.enqueue(enc('fresh'), 2)

    expect(h.writes).toEqual(['stale', 'fresh'])
    h.flushOne()

    expect(onWritten).not.toHaveBeenCalled()
    expect(h.writes).toEqual(['stale', 'fresh'])
    expect(h.resizes).toEqual([])
  })

  it('runs completion callback after the final chunk in enqueueMany', () => {
    const h = makeHarness()
    const done = vi.fn()
    h.io.reset(1)

    h.io.enqueueMany([enc('a'), enc('b'), enc('c')], 1, done)
    h.flushAll()

    expect(h.writes).toEqual(['a', 'b', 'c'])
    expect(done).toHaveBeenCalledTimes(1)
  })
})

describe('scroll preservation across BSU/ESU', () => {
  it('stays at bottom when user was at the bottom', () => {
    const h = makeScrollHarness({ scrollbackLimit: 100, rows: 25 })
    h.io.reset(1)
    h.addLines(50) // baseY=50, totalLines=75
    h.userScrollToBottom() // viewportY=50

    h.io.enqueue(wrapBSU('output'), 1)
    h.flushOne(5) // 5 new lines, baseY=55
    h.flushRAF()

    expect(h.scrollToBottomCalls.length).toBeGreaterThan(0)
    expect(h.viewportY).toBe(h.baseY)
    h.cleanup()
  })

  it('restores scroll position when user is scrolled up, no overflow', () => {
    const h = makeScrollHarness({ scrollbackLimit: 100, rows: 25 })
    h.io.reset(1)
    h.addLines(50) // baseY=50
    h.userScrollTo(20) // user looking at line 20

    h.io.enqueue(wrapBSU('output'), 1)
    // xterm processes: 5 new lines, no overflow. baseY=55, viewportY stays 20.
    h.flushOne(5)
    h.flushRAF()

    // Should restore to 20 (the post-parse value), not jump to bottom.
    expect(h.viewportY).toBe(20)
    h.cleanup()
  })

  it('adjusts scroll position when scrollback overflows', () => {
    const h = makeScrollHarness({ scrollbackLimit: 100, rows: 25 })
    h.io.reset(1)
    // Fill to capacity: 125 total lines, baseY=100
    h.addLines(100)
    expect(h.baseY).toBe(100)

    h.userScrollTo(50) // user looking at line 50

    h.io.enqueue(wrapBSU('output'), 1)
    // xterm processes: 10 new lines, all overflow (scrollback full).
    // baseY stays 100, viewportY decremented by 10 to 40.
    h.flushOne(10)
    expect(h.baseY).toBe(100)

    // The write callback captures viewportY=40 (xterm's adjusted value).
    h.flushRAF()

    expect(h.viewportY).toBe(40)
    h.cleanup()
  })

  it('handles multiple BSU/ESU cycles with progressive overflow', () => {
    const h = makeScrollHarness({ scrollbackLimit: 100, rows: 25 })
    h.io.reset(1)
    h.addLines(100) // at capacity, baseY=100
    h.userScrollTo(60)

    // First BSU/ESU: 5 lines overflow
    h.io.enqueue(wrapBSU('batch1'), 1)
    h.flushOne(5)
    h.flushRAF()
    expect(h.viewportY).toBe(55) // shifted down by 5

    // Second BSU/ESU: 5 more lines overflow
    h.io.enqueue(wrapBSU('batch2'), 1)
    h.flushOne(5)
    h.flushRAF()
    expect(h.viewportY).toBe(50) // shifted down by another 5

    // Third BSU/ESU: 5 more lines overflow
    h.io.enqueue(wrapBSU('batch3'), 1)
    h.flushOne(5)
    h.flushRAF()
    expect(h.viewportY).toBe(45) // shifted down by another 5

    h.cleanup()
  })

  it('clamps viewportY to 0 when evictions exceed original position', () => {
    const h = makeScrollHarness({ scrollbackLimit: 100, rows: 25 })
    h.io.reset(1)
    h.addLines(100) // at capacity, baseY=100
    h.userScrollTo(5) // near the top

    h.io.enqueue(wrapBSU('lots of output'), 1)
    // 20 lines overflow, but viewportY was only 5: clamped to 0
    h.flushOne(20)
    h.flushRAF()

    expect(h.viewportY).toBe(0)
    h.cleanup()
  })

  it('handles BSU and ESU split across separate chunks', () => {
    const h = makeScrollHarness({ scrollbackLimit: 100, rows: 25 })
    h.io.reset(1)
    h.addLines(100)
    h.userScrollTo(50)

    // BSU in first chunk
    const bsuChunk = new Uint8Array([...BSU, ...enc('partial')])
    // ESU in second chunk
    const esuChunk = new Uint8Array([...enc('rest'), ...ESU])

    h.io.enqueue(bsuChunk, 1)
    h.flushOne(3) // 3 lines, viewportY adjusted to 47

    h.io.enqueue(esuChunk, 1)
    h.flushOne(3) // 3 more lines, viewportY adjusted to 44
    h.flushRAF()

    expect(h.viewportY).toBe(44)
    h.cleanup()
  })

  it('does not interfere with non-BSU/ESU writes', () => {
    const h = makeScrollHarness({ scrollbackLimit: 100, rows: 25 })
    h.io.reset(1)
    h.addLines(50)
    h.userScrollTo(20)

    // Regular write (no BSU/ESU)
    h.io.enqueue(enc('regular output'), 1)
    h.flushOne(5)

    // No rAF should have been scheduled
    expect(h.scrollToLineCalls.length).toBe(0)
    expect(h.scrollToBottomCalls.length).toBe(0)
    // viewportY should be whatever xterm set it to (20, adjusted for no overflow)
    expect(h.viewportY).toBe(20)
    h.cleanup()
  })

  it('snaps to bottom when close to bottom (within threshold)', () => {
    const h = makeScrollHarness({ scrollbackLimit: 100, rows: 25 })
    h.io.reset(1)
    h.addLines(50) // baseY=50
    h.userScrollTo(48) // within 3 lines of bottom (baseY=50)

    h.io.enqueue(wrapBSU('output'), 1)
    h.flushOne(5)
    h.flushRAF()

    // Was within threshold of bottom, should snap to bottom
    expect(h.scrollToBottomCalls.length).toBeGreaterThan(0)
    expect(h.viewportY).toBe(h.baseY)
    h.cleanup()
  })

  it('respects user scrolling to bottom between write callback and rAF', () => {
    const h = makeScrollHarness({ scrollbackLimit: 100, rows: 25 })
    h.io.reset(1)
    h.addLines(100)
    h.userScrollTo(50) // scrolled up

    h.io.enqueue(wrapBSU('output'), 1)
    h.flushOne(5) // viewportY adjusted to 45

    // User scrolls to bottom AFTER the write callback captured adjustedY=45
    // but BEFORE the rAF fires.
    h.userScrollToBottom()
    expect(h.viewportY).toBe(h.baseY) // confirm at bottom

    h.flushRAF()

    // Should respect the user's scroll-to-bottom, not yank back to 45.
    expect(h.viewportY).toBe(h.baseY)
    expect(h.scrollToBottomCalls.length).toBeGreaterThan(0)
    h.cleanup()
  })

  it('defers resize between split BSU and ESU chunks', () => {
    const h = makeScrollHarness({ scrollbackLimit: 100, rows: 25 })
    h.io.reset(1)
    h.addLines(100) // at capacity, baseY=100
    h.userScrollTo(50)

    // Simulate xterm's resize resetting viewportY to 0 (reflow side-effect).
    h.onResize = () => { h.userScrollTo(0) }

    // BSU in first chunk (no ESU yet)
    const bsuChunk = new Uint8Array([...BSU, ...enc('partial')])
    h.io.enqueue(bsuChunk, 1)
    h.flushOne(3) // viewportY adjusted to 47

    // A resize arrives while queue is empty (between BSU and ESU).
    // Without the fix, pump() would process it now, resetting viewportY
    // to 0 before the ESU capture, corrupting adjustedY.
    h.io.requestResize({ cols: 80, rows: 20 }, 1)

    // The resize should NOT have been applied yet (savedScroll is set).
    expect(h.resizeCalls).toEqual([])
    expect(h.viewportY).toBe(47)

    // ESU in second chunk
    const esuChunk = new Uint8Array([...enc('rest'), ...ESU])
    h.io.enqueue(esuChunk, 1)
    h.flushOne(3) // viewportY adjusted to 44

    // Still no resize (restoreRAF is now pending).
    expect(h.resizeCalls).toEqual([])

    // rAF: restores scroll to the correct position (44), then flushes
    // the deferred resize. The resize side-effect changes viewportY after.
    h.flushRAF()

    // Scroll restore targeted the correct line (44), not 0.
    expect(h.scrollToLineCalls).toContain(44)
    // The resize was applied after scroll restore, not before.
    expect(h.resizeCalls).toEqual([{ cols: 80, rows: 20 }])

    h.cleanup()
  })

  it('defers resize between ESU write-callback and restore rAF', () => {
    const h = makeScrollHarness({ scrollbackLimit: 100, rows: 25 })
    h.io.reset(1)
    h.addLines(100)
    h.userScrollTo(50)

    // Simulate xterm's resize resetting viewportY to 0 (reflow side-effect).
    h.onResize = () => { h.userScrollTo(0) }

    // Single chunk with BSU + content + ESU
    h.io.enqueue(wrapBSU('output'), 1)
    h.flushOne(5) // viewportY adjusted to 45

    // Resize arrives after ESU write-callback but before rAF fires.
    // Without the fix, pump() would process it now since the queue is
    // empty and savedScroll was just cleared.
    h.io.requestResize({ cols: 80, rows: 20 }, 1)

    // The resize should NOT have been applied yet (restoreRAF pending).
    expect(h.resizeCalls).toEqual([])
    expect(h.viewportY).toBe(45)

    // rAF restores scroll, then flushes the deferred resize.
    h.flushRAF()

    // Scroll was restored to 45 BEFORE resize ran. Resize then ran and
    // reset viewportY to 0, but that's expected: the resize legitimately
    // changes layout. The important thing is the scroll restore wasn't
    // corrupted by a resize that snuck in before it could run.
    expect(h.resizeCalls).toEqual([{ cols: 80, rows: 20 }])

    h.cleanup()
  })

  it('processes resize normally when no BSU/ESU is in flight', () => {
    const h = makeScrollHarness({ scrollbackLimit: 100, rows: 25 })
    h.io.reset(1)
    h.addLines(50)
    h.userScrollTo(20)

    // Regular write (no BSU/ESU), then resize.
    h.io.enqueue(enc('output'), 1)
    h.flushOne(0)

    h.io.requestResize({ cols: 120, rows: 40 }, 1)
    // Resize should fire immediately (no BSU/ESU block, no pending rAF).
    expect(h.resizeCalls).toEqual([{ cols: 120, rows: 40 }])

    h.cleanup()
  })

  it('resets scroll state on epoch change', () => {
    const h = makeScrollHarness({ scrollbackLimit: 100, rows: 25 })
    h.io.reset(1)
    h.addLines(100)
    h.userScrollTo(50)

    // Start a BSU block
    h.io.enqueue(new Uint8Array([...BSU, ...enc('data')]), 1)
    h.flushOne(2)

    // Reset epoch before ESU arrives
    h.io.reset(2)

    // Send ESU under old epoch (should be ignored since data doesn't arrive)
    // Send new data under new epoch
    h.io.enqueue(enc('new session'), 2)
    h.flushOne(0)

    // No scroll restore should have happened
    expect(h.scrollToLineCalls.length).toBe(0)
    expect(h.scrollToBottomCalls.length).toBe(0)
    h.cleanup()
  })
})
