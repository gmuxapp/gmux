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
  // Per-line content for anchor-matching tests. y → visible text. Lines
  // not in the map return null from getLine() (the production accessor
  // also returns null for empty / trivial lines).
  const lineContent = new Map<number, string>()

  const scroll: ScrollAccessor = {
    getState: () => ({ viewportY, baseY, rows }),
    scrollToLine(line: number) {
      scrollToLineCalls.push(line)
      viewportY = Math.max(0, Math.min(line, baseY))
    },
    scrollToBottom() {
      scrollToBottomCalls.push(baseY)
      viewportY = baseY
    },
    getLine(y: number): string | null {
      return lineContent.get(y) ?? null
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
     * Simulate xterm processing `\x1b[3J` (clear scrollback): both ybase
     * and ydisp reset to 0, scrollback content is gone, only the visible
     * rows remain. Call between enqueue and flushOne to model an agent
     * (eg pi) wiping scrollback inside a BSU/ESU block.
     */
    clearScrollback() {
      totalLines = rows
      baseY = 0
      viewportY = 0
      lineContent.clear()
    },

    /** Set the visible text of the buffer line at `y`. */
    setLine(y: number, text: string) {
      lineContent.set(y, text)
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

  it('after scrollback wipe, restores user\'s distance from bottom when new buffer is large enough', () => {
    // The user was reading 3 lines above the latest content (eg the
    // last line of pi's previous turn). Pi ends a turn with
    // \x1b[3J + redraw, scrollback gets rebuilt to ~15 lines. The
    // user's pre-BSU absolute line is gone, but their *intent* ("keep
    // me 3 lines above the newest content") is preserved.
    const h = makeScrollHarness({ scrollbackLimit: 5000, rows: 40 })
    h.io.reset(1)
    h.addLines(200)            // baseY=200
    h.userScrollTo(197)        // 3 lines above the bottom
    expect(h.baseY - h.viewportY).toBe(3)

    h.io.enqueue(wrapBSU('redraw'), 1)
    h.clearScrollback()
    h.addLines(15)             // new baseY=15, plenty of room for distance=3
    h.flushOne(0)
    h.flushRAF()

    // Distance preserved: viewportY == baseY - 3.
    expect(h.scrollToLineCalls).toContain(h.baseY - 3)
    expect(h.scrollToBottomCalls.length).toBe(0)
    expect(h.baseY - h.viewportY).toBe(3)
    h.cleanup()
  })

  it('after scrollback wipe, snaps to bottom when distance exceeds new buffer', () => {
    // The pi end-of-turn shape from the e2e fixture: user was reading
    // far back in scrollback (100 lines above the bottom), the new
    // buffer is only ~15 lines, so the user's intent has nowhere to
    // anchor. Snap to bottom: nothing else is meaningful.
    const h = makeScrollHarness({ scrollbackLimit: 5000, rows: 40 })
    h.io.reset(1)
    h.addLines(200)
    h.userScrollTo(100)        // 100 lines above the bottom
    expect(h.baseY - h.viewportY).toBe(100)

    h.io.enqueue(wrapBSU('redraw'), 1)
    h.clearScrollback()
    h.addLines(15)             // new baseY=15, distance(100) > baseY
    h.flushOne(0)
    h.flushRAF()

    // Pre-fix bug: scrollToLine(min(adjustedY=0, baseY=15)) = 0 (top).
    // Post-fix: prevDistanceFromBottom (100) > new baseY (15) → bottom.
    expect(h.scrollToBottomCalls.length).toBeGreaterThan(0)
    expect(h.viewportY).toBe(h.baseY)
    h.cleanup()
  })

  it('does not snap to bottom when baseY simply grows from 0', () => {
    // First-ever output to a fresh terminal: baseY transitions from 0
    // upward. snap.prevBaseY=0 must NOT trigger the wipe branch
    // (baseY=N > prevBaseY=0, so the existing scrolled-up restore path
    // applies as normal).
    const h = makeScrollHarness({ scrollbackLimit: 100, rows: 25 })
    h.io.reset(1)
    expect(h.baseY).toBe(0)

    h.io.enqueue(wrapBSU('first output'), 1)
    h.flushOne(40)             // baseY now 15 (40 lines, 25 rows visible)
    h.flushRAF()

    // No wipe happened, just growth. The user was implicitly at bottom
    // (viewportY=0=baseY=0 pre-BSU), so the wasAtBottom branch fires.
    expect(h.viewportY).toBe(h.baseY)
    h.cleanup()
  })

  it('after scrollback wipe, jumps to the line whose content matches the user\'s anchor', () => {
    // The user is reading a recognizable line ("ERROR: cannot find
    // foo"). The agent emits a wipe + redraw, the same line reappears
    // in the new buffer at a different position. Anchor-match wins
    // over distance restoration.
    const h = makeScrollHarness({ scrollbackLimit: 5000, rows: 40 })
    h.io.reset(1)
    h.addLines(200)
    h.userScrollTo(150)
    h.setLine(150, 'ERROR: cannot find foo')
    expect(h.baseY - h.viewportY).toBe(50)

    h.io.enqueue(wrapBSU('redraw'), 1)
    h.clearScrollback()
    h.addLines(80)
    // Same line lands at a different y after the redraw.
    h.setLine(60, 'ERROR: cannot find foo')
    h.flushOne(0)
    h.flushRAF()

    // Anchor matched at y=60: scroll there, no bottom snap.
    expect(h.scrollToLineCalls).toContain(60)
    expect(h.scrollToBottomCalls.length).toBe(0)
    expect(h.viewportY).toBe(60)
    h.cleanup()
  })

  it('after scrollback wipe with multiple anchor matches, picks the one closest to prevDistanceFromBottom', () => {
    // The anchor line appears three times in the redraw. The match
    // closest to the user's pre-wipe distance from the bottom (5)
    // wins. The winning y is deliberately chosen so it differs from
    // the distance-fallback position (baseY - prevDistance = 15),
    // so a regression that disables anchor matching is caught here.
    const h = makeScrollHarness({ scrollbackLimit: 5000, rows: 40 })
    h.io.reset(1)
    h.addLines(50)
    h.userScrollTo(45)         // distance = 5
    h.setLine(45, 'session ready')
    expect(h.baseY - h.viewportY).toBe(5)

    h.io.enqueue(wrapBSU('redraw'), 1)
    h.clearScrollback()
    h.addLines(20)             // new baseY=20
    h.setLine(12, 'session ready')  // distance: 8, |8-5|=3
    h.setLine(16, 'session ready')  // distance: 4, |4-5|=1 ← winner
    h.setLine(18, 'session ready')  // distance: 2, |2-5|=3
    h.flushOne(0)
    h.flushRAF()

    // y=16 is the closest match. Distance fallback would have given
    // y=15, so this assertion is sensitive to anchor matching being
    // active rather than coincidentally matching the fallback.
    expect(h.scrollToLineCalls).toContain(16)
    expect(h.viewportY).toBe(16)
    h.cleanup()
  })

  it('after scrollback wipe, falls back to distance restoration when anchor is null', () => {
    // The user was on a trivial line so getLine returned null and
    // savedScroll.prevAnchorLine is null. We fall through to the
    // distance restoration path.
    const h = makeScrollHarness({ scrollbackLimit: 5000, rows: 40 })
    h.io.reset(1)
    h.addLines(50)
    h.userScrollTo(47)         // distance = 3
    // Deliberately NOT calling setLine: getLine returns null (the
    // production accessor's filter for trivial lines).

    h.io.enqueue(wrapBSU('redraw'), 1)
    h.clearScrollback()
    h.addLines(20)             // new baseY=20
    h.setLine(17, 'unrelated content')  // present but won't match anything
    h.flushOne(0)
    h.flushRAF()

    // No anchor captured → distance fallback: 20 - 3 = 17.
    expect(h.scrollToLineCalls).toContain(17)
    expect(h.viewportY).toBe(17)
    h.cleanup()
  })

  it('after scrollback wipe, anchor in visible region snaps to bottom with the anchor still in view', () => {
    // Mid-distance scenario: the user was scrolled up just a few
    // lines, reading streaming output. After the wipe their anchor
    // line ends up in the visible region of the new buffer, not
    // scrollback. `scrollToLine` clamps to baseY, so the best we
    // can do is `scrollToBottom` and let the anchor sit somewhere
    // in the visible region rather than disappearing entirely or
    // landing the user at distance restoration's chosen y where
    // the anchor may be off-screen.
    //
    // Concretely: pre-wipe distance = 5. Post-wipe baseY = 5 with
    // the anchor placed at y = 20 (visible region [5, 44]). With
    // the search bounded to [0, baseY] (the previous behavior),
    // anchor matching would miss and distance restoration would
    // land the user at viewportY = 0. With the search extended to
    // the full buffer, we land at viewportY = baseY = 5, with the
    // anchor visible at offset 15 of the viewport. The two
    // viewportY values differ, so this test catches a regression
    // that re-narrows the search range.
    const h = makeScrollHarness({ scrollbackLimit: 5000, rows: 40 })
    h.io.reset(1)
    h.addLines(50)
    h.userScrollTo(45)         // distance = 5
    h.setLine(45, 'streaming-target')
    expect(h.baseY - h.viewportY).toBe(5)

    h.io.enqueue(wrapBSU('redraw'), 1)
    h.clearScrollback()
    h.addLines(5)              // new baseY=5; total = baseY+rows = 45
    h.setLine(20, 'streaming-target')  // y=20 in visible region [5, 44]
    h.flushOne(0)
    h.flushRAF()

    // Visible-region match → restoreY = min(20, 5) = 5 (= at-bottom).
    expect(h.scrollToLineCalls).toContain(5)
    expect(h.viewportY).toBe(5)
    h.cleanup()
  })

  it('after scrollback wipe with both scrollback and visible matches, picks by closeness to prevDistanceFromBottom', () => {
    // Anchor appears in both regions. The user's pre-wipe distance
    // (1) is close to the visible match's restore-distance (0,
    // since visible matches force scrollToBottom) and far from the
    // scrollback match's restore-distance (13). Tiebreaker picks
    // the visible match.
    //
    // Without the full-buffer search, the visible match would never
    // be considered, the scrollback match would win by default,
    // and viewportY would land at 2 instead of baseY (= 15). The
    // two outcomes differ, so this catches a regression that
    // re-narrows the search range.
    const h = makeScrollHarness({ scrollbackLimit: 5000, rows: 40 })
    h.io.reset(1)
    h.addLines(50)
    h.userScrollTo(49)         // distance = 1 (just barely scrolled up)
    h.setLine(49, 'shared-line')
    expect(h.baseY - h.viewportY).toBe(1)

    h.io.enqueue(wrapBSU('redraw'), 1)
    h.clearScrollback()
    h.addLines(15)             // new baseY=15; visible region [15, 54]
    h.setLine(2, 'shared-line')   // scrollback: restoreY=2,  restoreDistance=13, diff=12
    h.setLine(30, 'shared-line')  // visible:    restoreY=15, restoreDistance=0,  diff=1
    h.flushOne(0)
    h.flushRAF()

    // Visible match wins because its restore-distance is closer to
    // the pre-wipe distance.
    expect(h.scrollToLineCalls).toContain(15)
    expect(h.viewportY).toBe(15)
    h.cleanup()
  })

  it('after scrollback wipe, falls back to distance when anchor does not match anywhere', () => {
    // The agent's redraw doesn't include the user's pre-wipe content
    // (eg pi's status-bar-only redraw vs the user reading prior
    // conversation output). Distance restoration takes over.
    const h = makeScrollHarness({ scrollbackLimit: 5000, rows: 40 })
    h.io.reset(1)
    h.addLines(50)
    h.userScrollTo(47)         // distance = 3
    h.setLine(47, 'previous turn output line')

    h.io.enqueue(wrapBSU('redraw'), 1)
    h.clearScrollback()
    h.addLines(20)             // new baseY=20
    h.setLine(10, 'pi status bar version 0.70.2')  // wholly different
    h.flushOne(0)
    h.flushRAF()

    // No match → distance fallback: 20 - 3 = 17.
    expect(h.scrollToLineCalls).toContain(17)
    expect(h.viewportY).toBe(17)
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

  it('preserves position when near but not at the bottom', () => {
    const h = makeScrollHarness({ scrollbackLimit: 100, rows: 25 })
    h.io.reset(1)
    h.addLines(50) // baseY=50
    h.userScrollTo(48) // 2 rows above bottom

    h.io.enqueue(wrapBSU('output'), 1)
    h.flushOne(5)
    h.flushRAF()

    // Not exactly at bottom, so scroll position should be preserved.
    // With the old <= 3 threshold this snapped to bottom; now it
    // respects the user's explicit scroll-up.
    expect(h.viewportY).toBe(48)
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

  it('forceNextScrollToBottom overrides scroll-up during replay', () => {
    const h = makeScrollHarness({ scrollbackLimit: 100, rows: 25 })
    h.io.reset(1)
    h.addLines(50) // baseY=50
    h.userScrollTo(10) // user was scrolled way up (stale from previous session)

    // Simulate what terminal.tsx does before enqueuing replay:
    h.io.forceNextScrollToBottom()

    h.io.enqueue(wrapBSU('replay-frame'), 1)
    h.flushOne(5)
    h.flushRAF()

    // Even though the user was scrolled up, the force flag makes
    // the BSU handler treat it as wasAtBottom=true.
    expect(h.scrollToBottomCalls.length).toBeGreaterThan(0)
    expect(h.viewportY).toBe(h.baseY)
    h.cleanup()
  })

  it('forceNextScrollToBottom only applies to the next BSU, not subsequent ones', () => {
    const h = makeScrollHarness({ scrollbackLimit: 100, rows: 25 })
    h.io.reset(1)
    h.addLines(50)
    h.userScrollTo(10)

    h.io.forceNextScrollToBottom()

    // First BSU: force-scrolls to bottom.
    h.io.enqueue(wrapBSU('replay'), 1)
    h.flushOne(5)
    h.flushRAF()
    expect(h.viewportY).toBe(h.baseY)

    // User scrolls up again.
    h.userScrollTo(20)

    // Second BSU: force is consumed, so user's scroll is preserved.
    h.io.enqueue(wrapBSU('live-update'), 1)
    h.flushOne(3)
    h.flushRAF()
    expect(h.viewportY).toBe(20)
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
