import { describe, expect, it, vi } from 'vitest'
import { createTerminalIO, type ScrollAccessor } from './terminal-io'
import { BSU, CSI_3J, ESU } from './replay'

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
 * Simulates ghostty-web's scroll behavior for testing scroll preservation.
 *
 * ghostty-web coordinate system (mirrored here):
 *   viewportY = baseY  → at the bottom (newest content visible)
 *   viewportY < baseY  → scrolled up into scrollback
 *   baseY              → scrollbackLength (fixed at scrollbackLimit once full)
 *
 * ghostty-web's write() always calls scrollToBottom() when viewportY < baseY
 * (ie whenever the user is scrolled up). The harness models this via
 * `autoScroll: true` in `primeWrite`, which is applied synchronously inside
 * the mock write() before storing the callback.
 *
 * Effects (linesAdded, autoScroll, clearScrollback) are primed with
 * `primeWrite()` BEFORE calling `io.enqueue()`. The mock write() consumes
 * and applies them synchronously. This models ghostty-web's behaviour where
 * all terminal state changes (parse + auto-scroll) happen inside write()
 * before write() returns.
 *
 * `flushOne()` just fires the pending callback (the rAF equivalent) — it no
 * longer triggers any terminal-state side-effects.
 *
 * `flushRAF()` is kept as a no-op for documentation purposes: the synchronous
 * restore means no rAF is needed.
 */
function makeScrollHarness(opts: { scrollbackLimit: number; rows: number }) {
  const { scrollbackLimit, rows } = opts
  let totalLines = rows // start with one screenful
  let viewportY = 0
  let baseY = 0

  const scrollToLineCalls: number[] = []
  const scrollToBottomCalls: number[] = []
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
  const rafQueue: Array<() => void> = []

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
  let onResize: ((cols: number, rows: number) => void) | null = null

  // Per-write effect slot: consumed by the next mock write() call.
  let nextEffect: {
    linesAdded?: number
    autoScroll?: boolean
    clearScrollback?: boolean
    scrollTo?: number
    /** Lines to set in lineContent AFTER clearScrollback + addLines run
     *  (simulates content written by WASM during parse that restoreScroll
     *  needs to find via getLine). */
    postClearLines?: Map<number, string>
  } = {}

  function addLinesInternal(count: number) {
    totalLines += count
    const overflow = totalLines - (scrollbackLimit + rows)
    if (overflow > 0) {
      totalLines = scrollbackLimit + rows
      baseY = scrollbackLimit
      // xterm decrements viewportY on eviction; ghostty-web doesn't, but
      // the harness still models it for tests that rely on this behaviour.
      viewportY = Math.max(0, viewportY - overflow)
    } else {
      baseY = Math.max(0, totalLines - rows)
    }
  }

  function clearScrollbackInternal() {
    totalLines = rows
    baseY = 0
    viewportY = 0
    lineContent.clear()
  }

  const io = createTerminalIO(
    {
      write(data, callback) {
        writes.push(typeof data === 'string' ? data : new TextDecoder().decode(data))
        // Apply primed effects synchronously — this models what ghostty-web's
        // write() does internally before returning.
        const effect = nextEffect
        nextEffect = {}
        if (effect.clearScrollback) clearScrollbackInternal()
        if (effect.linesAdded && effect.linesAdded > 0) addLinesInternal(effect.linesAdded)
        // Populate new buffer content before restoreScroll queries getLine()
        if (effect.postClearLines) {
          for (const [y, text] of effect.postClearLines) lineContent.set(y, text)
        }
        if (effect.scrollTo !== undefined) {
          viewportY = Math.max(0, Math.min(effect.scrollTo, baseY))
        } else if (effect.autoScroll && viewportY !== baseY) {
          viewportY = baseY // scrollToBottom
        }
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
    set onResize(fn: ((cols: number, rows: number) => void) | null) { onResize = fn },

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
     * Prime the next write() call with terminal-state side-effects.
     * Must be called BEFORE `io.enqueue()` — effects are consumed synchronously
     * inside the mock write().
     *
     * @param linesAdded   New output lines processed during the write (may trigger eviction).
     * @param autoScroll   If true, simulate ghostty-web's auto-scroll to bottom before
     *                     the write returns (models `viewportY !== 0 && scrollToBottom()`).
     * @param clearScrollback  If true, simulate `\x1b[3J` clearing scrollback before
     *                         linesAdded are applied.
     * @param scrollTo     Explicit viewportY override (for edge-case positioning tests).
     */
    primeWrite(effect: {
      linesAdded?: number
      autoScroll?: boolean
      clearScrollback?: boolean
      scrollTo?: number
      postClearLines?: Map<number, string>
    } = {}) {
      nextEffect = effect
    },

    /**
     * Directly add lines to the buffer (outside of a write call).
     * Used for setup before enqueue/primeWrite.
     */
    addLines(count: number) {
      addLinesInternal(count)
    },

    /**
     * Directly clear scrollback (outside of a write call). Used for setup.
     */
    clearScrollback() {
      clearScrollbackInternal()
    },

    /** Set the visible text of the buffer line at `y`. */
    setLine(y: number, text: string) {
      lineContent.set(y, text)
    },

    /**
     * Fire one pending write callback.
     * In the new synchronous-restore model this is just cleanup (pump + onWritten).
     * No terminal-state side-effects happen here.
     */
    flushOne() {
      const cb = pending.shift()
      if (!cb) throw new Error('no pending write callback')
      cb()
    },

    /**
     * No-op: kept for documentation.
     * Scroll restore happens synchronously after write(), so no rAF flush
     * is ever needed for scroll assertions.
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

/** Wrap payload in BSU + \x1b[3J + ESU markers (pi-style redraw). */
function wrapBSUWithClear(payload: string): Uint8Array {
  const payloadBytes = enc(payload)
  const result = new Uint8Array(BSU.length + CSI_3J.length + payloadBytes.length + ESU.length)
  let offset = 0
  result.set(BSU, offset); offset += BSU.length
  result.set(CSI_3J, offset); offset += CSI_3J.length
  result.set(payloadBytes, offset); offset += payloadBytes.length
  result.set(ESU, offset)
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

describe('scroll preservation — per-write save/restore', () => {
  it('stays at bottom when user was at the bottom', () => {
    const h = makeScrollHarness({ scrollbackLimit: 100, rows: 25 })
    h.io.reset(1)
    h.addLines(50) // baseY=50
    h.userScrollToBottom() // viewportY=50

    // At bottom → captureScroll skips → auto-scroll is a no-op anyway.
    h.primeWrite({ linesAdded: 5, autoScroll: true })
    h.io.enqueue(wrapBSU('output'), 1)

    expect(h.viewportY).toBe(h.baseY)
    h.flushOne()
    h.cleanup()
  })

  it('restores scroll when user is scrolled up — plain streaming (ghostty auto-scroll)', () => {
    const h = makeScrollHarness({ scrollbackLimit: 100, rows: 25 })
    h.io.reset(1)
    h.addLines(50) // baseY=50
    h.userScrollTo(20) // scrolled up

    // ghostty auto-scrolls to bottom inside write(); we restore synchronously.
    h.primeWrite({ linesAdded: 5, autoScroll: true })
    h.io.enqueue(enc('live streaming output'), 1)

    // Restore fires synchronously: user back at 20.
    expect(h.viewportY).toBe(20)
    h.flushOne()
    h.cleanup()
  })

  it('restores scroll across multiple consecutive writes (working-state animation)', () => {
    // The core scenario: pi is running, emitting the "working…" animation.
    // Each output frame is a separate write(). Without the fix, each write
    // auto-scrolls the user back to the bottom. With the fix they stay put.
    const h = makeScrollHarness({ scrollbackLimit: 100, rows: 25 })
    h.io.reset(1)
    h.addLines(100) // at capacity, baseY=100
    h.userScrollTo(80) // reading 20 lines above the bottom

    for (let frame = 0; frame < 5; frame++) {
      h.primeWrite({ autoScroll: true }) // each frame auto-scrolls
      h.io.enqueue(enc(`frame-${frame}`), 1)
      // Restore fires synchronously after each write.
      expect(h.viewportY).toBe(80)
      h.flushOne()
    }
    h.cleanup()
  })

  it('restores scroll when user is scrolled up — BSU/ESU wrapped output', () => {
    const h = makeScrollHarness({ scrollbackLimit: 100, rows: 25 })
    h.io.reset(1)
    h.addLines(50) // baseY=50
    h.userScrollTo(20)

    h.primeWrite({ linesAdded: 5, autoScroll: true })
    h.io.enqueue(wrapBSU('output'), 1)

    expect(h.viewportY).toBe(20)
    h.flushOne()
    h.cleanup()
  })

  it('clamps restored position to baseY when baseY shrinks (edge case)', () => {
    const h = makeScrollHarness({ scrollbackLimit: 100, rows: 25 })
    h.io.reset(1)
    h.addLines(50) // baseY=50
    h.userScrollTo(45) // 5 lines above bottom

    // Simulate a write that somehow reduces baseY (unlikely but guarded against).
    // linesAdded=0, autoScroll: viewportY snaps to baseY=50.
    // savedViewportY=45, new baseY=50 → scrollToLine(min(45,50))=45.
    h.primeWrite({ autoScroll: true })
    h.io.enqueue(enc('output'), 1)

    expect(h.viewportY).toBe(45)
    h.flushOne()
    h.cleanup()
  })

  it('does not interfere when user is already at the bottom', () => {
    const h = makeScrollHarness({ scrollbackLimit: 100, rows: 25 })
    h.io.reset(1)
    h.addLines(50)
    h.userScrollToBottom()

    h.primeWrite({ linesAdded: 5, autoScroll: true })
    h.io.enqueue(enc('regular output'), 1)

    // No save → no restore; auto-scroll stands.
    expect(h.scrollToLineCalls.length).toBe(0)
    expect(h.viewportY).toBe(h.baseY)
    h.flushOne()
    h.cleanup()
  })

  it('handles BSU and ESU split across separate chunks', () => {
    // Each chunk is independently saved/restored. The anchor is re-captured
    // before each write, so the split-chunk case works correctly.
    const h = makeScrollHarness({ scrollbackLimit: 100, rows: 25 })
    h.io.reset(1)
    h.addLines(100)
    h.userScrollTo(50)

    const bsuChunk = new Uint8Array([...BSU, ...enc('partial')])
    const esuChunk = new Uint8Array([...enc('rest'), ...ESU])

    // Chunk A: BSU + partial content. autoScroll simulates ghostty.
    h.primeWrite({ linesAdded: 3, autoScroll: true })
    h.io.enqueue(bsuChunk, 1)
    // Restore: min(saved=50, baseY=100) = 50.
    expect(h.viewportY).toBe(50)
    h.flushOne()

    // Chunk B: rest + ESU.
    h.primeWrite({ linesAdded: 3, autoScroll: true })
    h.io.enqueue(esuChunk, 1)
    // Restore: min(saved=50, baseY=100) = 50.
    expect(h.viewportY).toBe(50)
    h.flushOne()
    h.cleanup()
  })

  it('after scrollback wipe, restores distance from bottom when new buffer is large enough', () => {
    const h = makeScrollHarness({ scrollbackLimit: 5000, rows: 40 })
    h.io.reset(1)
    h.addLines(200) // baseY=200
    h.userScrollTo(197) // 3 lines above bottom
    expect(h.baseY - h.viewportY).toBe(3)

    h.primeWrite({ clearScrollback: true, linesAdded: 15, autoScroll: true })
    h.io.enqueue(wrapBSUWithClear('redraw'), 1)

    // Distance preserved: viewportY == baseY - 3 (= 12).
    expect(h.scrollToLineCalls).toContain(h.baseY - 3)
    expect(h.scrollToBottomCalls.length).toBe(0)
    expect(h.baseY - h.viewportY).toBe(3)
    h.flushOne()
    h.cleanup()
  })

  it('after scrollback wipe, snaps to bottom when distance exceeds new buffer', () => {
    const h = makeScrollHarness({ scrollbackLimit: 5000, rows: 40 })
    h.io.reset(1)
    h.addLines(200)
    h.userScrollTo(100) // 100 lines above bottom
    expect(h.baseY - h.viewportY).toBe(100)

    h.primeWrite({ clearScrollback: true, linesAdded: 15, autoScroll: true })
    h.io.enqueue(wrapBSUWithClear('redraw'), 1)

    // prevDistance (100) > new baseY (15) → bottom.
    expect(h.scrollToBottomCalls.length).toBeGreaterThan(0)
    expect(h.viewportY).toBe(h.baseY)
    h.flushOne()
    h.cleanup()
  })

  it('does not snap to bottom when baseY simply grows from 0', () => {
    const h = makeScrollHarness({ scrollbackLimit: 100, rows: 25 })
    h.io.reset(1)
    expect(h.baseY).toBe(0)

    h.primeWrite({ linesAdded: 40, autoScroll: true })
    h.io.enqueue(wrapBSU('first output'), 1)

    // User was implicitly at bottom (viewportY=0=baseY=0 pre-write),
    // so no capture → auto-scroll stays → lands at bottom.
    expect(h.viewportY).toBe(h.baseY)
    h.flushOne()
    h.cleanup()
  })

  it('after scrollback wipe, jumps to the line whose content matches the user\'s anchor', () => {
    // postClearLines provides the new buffer content that restoreScroll
    // queries via getLine() — it is set AFTER clearScrollback runs inside
    // the mock write(), matching how WASM populates the buffer synchronously.
    const h = makeScrollHarness({ scrollbackLimit: 5000, rows: 40 })
    h.io.reset(1)
    h.addLines(200)
    h.userScrollTo(150)
    h.setLine(150, 'ERROR: cannot find foo') // pre-write anchor
    expect(h.baseY - h.viewportY).toBe(50)

    h.primeWrite({
      clearScrollback: true, linesAdded: 80, autoScroll: true,
      postClearLines: new Map([[60, 'ERROR: cannot find foo']]),
    })
    h.io.enqueue(wrapBSUWithClear('redraw'), 1)

    // Anchor matched at y=60: scroll there.
    expect(h.scrollToLineCalls).toContain(60)
    expect(h.scrollToBottomCalls.length).toBe(0)
    expect(h.viewportY).toBe(60)
    h.flushOne()
    h.cleanup()
  })

  it('after scrollback wipe with multiple anchor matches, picks the one closest to prevDistanceFromBottom', () => {
    const h = makeScrollHarness({ scrollbackLimit: 5000, rows: 40 })
    h.io.reset(1)
    h.addLines(50)
    h.userScrollTo(45)         // distance = 5
    h.setLine(45, 'session ready') // pre-write anchor
    expect(h.baseY - h.viewportY).toBe(5)

    h.primeWrite({
      clearScrollback: true, linesAdded: 20, autoScroll: true,
      postClearLines: new Map([
        [12, 'session ready'],  // distance: 8, |8-5|=3
        [16, 'session ready'],  // distance: 4, |4-5|=1 ← winner
        [18, 'session ready'],  // distance: 2, |2-5|=3
      ]),
    })
    h.io.enqueue(wrapBSUWithClear('redraw'), 1)

    expect(h.scrollToLineCalls).toContain(16)
    expect(h.viewportY).toBe(16)
    h.flushOne()
    h.cleanup()
  })

  it('after scrollback wipe, falls back to distance when anchor is null', () => {
    const h = makeScrollHarness({ scrollbackLimit: 5000, rows: 40 })
    h.io.reset(1)
    h.addLines(50)
    h.userScrollTo(47) // distance = 3

    h.primeWrite({ clearScrollback: true, linesAdded: 20, autoScroll: true })
    h.io.enqueue(wrapBSUWithClear('redraw'), 1)

    // No anchor captured → distance fallback: 20 - 3 = 17.
    expect(h.scrollToLineCalls).toContain(17)
    expect(h.viewportY).toBe(17)
    h.flushOne()
    h.cleanup()
  })

  it('after scrollback wipe, anchor in visible region snaps to bottom with anchor still in view', () => {
    const h = makeScrollHarness({ scrollbackLimit: 5000, rows: 40 })
    h.io.reset(1)
    h.addLines(50)
    h.userScrollTo(45)         // distance = 5
    h.setLine(45, 'streaming-target') // pre-write anchor
    expect(h.baseY - h.viewportY).toBe(5)

    h.primeWrite({
      clearScrollback: true, linesAdded: 5, autoScroll: true,
      postClearLines: new Map([[20, 'streaming-target']]), // y=20 in visible region [5, 44]
    })
    h.io.enqueue(wrapBSUWithClear('redraw'), 1)

    // Visible-region match → restoreY = min(20, 5) = 5 (= at-bottom).
    expect(h.scrollToLineCalls).toContain(5)
    expect(h.viewportY).toBe(5)
    h.flushOne()
    h.cleanup()
  })

  it('after scrollback wipe with both scrollback and visible matches, picks by closeness to prevDistanceFromBottom', () => {
    const h = makeScrollHarness({ scrollbackLimit: 5000, rows: 40 })
    h.io.reset(1)
    h.addLines(50)
    h.userScrollTo(49)         // distance = 1
    h.setLine(49, 'shared-line') // pre-write anchor
    expect(h.baseY - h.viewportY).toBe(1)

    h.primeWrite({
      clearScrollback: true, linesAdded: 15, autoScroll: true,
      postClearLines: new Map([
        [2, 'shared-line'],   // scrollback: restoreY=2,  restoreDistance=13, diff=12
        [30, 'shared-line'],  // visible:    restoreY=15, restoreDistance=0,  diff=1
      ]),
    })
    h.io.enqueue(wrapBSUWithClear('redraw'), 1)

    expect(h.scrollToLineCalls).toContain(15)
    expect(h.viewportY).toBe(15)
    h.flushOne()
    h.cleanup()
  })

  it('after scrollback wipe, falls back to distance when anchor does not match anywhere', () => {
    const h = makeScrollHarness({ scrollbackLimit: 5000, rows: 40 })
    h.io.reset(1)
    h.addLines(50)
    h.userScrollTo(47) // distance = 3
    h.setLine(47, 'previous turn output line')

    // New buffer has completely different content.
    h.setLine(10, 'pi status bar version 0.70.2')

    h.primeWrite({ clearScrollback: true, linesAdded: 20, autoScroll: true })
    h.io.enqueue(wrapBSUWithClear('redraw'), 1)

    // No match → distance fallback: 20 - 3 = 17.
    expect(h.scrollToLineCalls).toContain(17)
    expect(h.viewportY).toBe(17)
    h.flushOne()
    h.cleanup()
  })

  it('after \\x1b[3J + redraw growing baseY past prevBaseY, restores distance from bottom', () => {
    const h = makeScrollHarness({ scrollbackLimit: 5000, rows: 40 })
    h.io.reset(1)
    h.addLines(100) // prevBaseY = 100
    h.userScrollTo(97) // distance = 3

    // Simulate: clearScrollback + 200 lines added (baseY grows to 160) + auto-scroll.
    h.primeWrite({ clearScrollback: true, linesAdded: 200, autoScroll: true })
    h.io.enqueue(wrapBSUWithClear('redraw'), 1)
    expect(h.baseY).toBeGreaterThan(100)

    // Distance preserved: 3 rows above the new bottom.
    expect(h.viewportY).toBe(h.baseY - 3)
    h.flushOne()
    h.cleanup()
  })

  it('\\x1b[3J in a standalone chunk restores by distance (split-chunk case)', () => {
    // The \x1b[3J chunk is NOT the same chunk as BSU. In the per-write model
    // each chunk is independently handled. The \x1b[3J is detected in the
    // chunk that contains it, and anchor-based restore fires for that chunk.
    const h = makeScrollHarness({ scrollbackLimit: 5000, rows: 40 })
    h.io.reset(1)
    h.addLines(100)
    h.userScrollTo(97) // distance = 3

    // Chunk A: BSU only. autoScroll simulates ghostty.
    h.primeWrite({ autoScroll: true })
    h.io.enqueue(new Uint8Array([...BSU]), 1)
    // Simple restore: user stays at 97.
    expect(h.viewportY).toBe(97)
    h.flushOne()

    // Chunk B: \x1b[3J + redraw payload. Scrollback wiped here.
    const clearChunk = new Uint8Array([...CSI_3J, ...enc('redraw')])
    h.primeWrite({ clearScrollback: true, linesAdded: 200, autoScroll: true })
    h.io.enqueue(clearChunk, 1)
    // Anchor-based restore fires for this chunk (contains \x1b[3J).
    // prevDistance=3, new baseY=160 → scrollToLine(157).
    expect(h.viewportY).toBe(h.baseY - 3)
    h.flushOne()

    // Chunk C: ESU only. User already at correct position.
    const savedViewportY = h.viewportY
    h.primeWrite({ autoScroll: true })
    h.io.enqueue(new Uint8Array([...ESU]), 1)
    // Simple restore: stays at savedViewportY.
    expect(h.viewportY).toBe(savedViewportY)
    h.flushOne()
    h.cleanup()
  })

  it('preserves position when near but not at the bottom', () => {
    const h = makeScrollHarness({ scrollbackLimit: 100, rows: 25 })
    h.io.reset(1)
    h.addLines(50) // baseY=50
    h.userScrollTo(48) // 2 rows above bottom

    h.primeWrite({ linesAdded: 5, autoScroll: true })
    h.io.enqueue(wrapBSU('output'), 1)

    expect(h.viewportY).toBe(48)
    h.flushOne()
    h.cleanup()
  })

  it('user scrolling after restore is honoured', () => {
    const h = makeScrollHarness({ scrollbackLimit: 100, rows: 25 })
    h.io.reset(1)
    h.addLines(100)
    h.userScrollTo(50)

    h.primeWrite({ linesAdded: 5, autoScroll: true })
    h.io.enqueue(wrapBSU('output'), 1)
    // Restore fires synchronously: user at 50.
    expect(h.viewportY).toBe(50)
    h.flushOne()

    // User scrolls to bottom AFTER the restore.
    h.userScrollToBottom()
    expect(h.viewportY).toBe(h.baseY)

    // flushRAF is a no-op.
    h.flushRAF()
    expect(h.viewportY).toBe(h.baseY)
    h.cleanup()
  })

  it('resize fires immediately after write completes (no BSU/ESU deferral needed)', () => {
    // Resize is no longer deferred across BSU/ESU chunks — each write is
    // independently handled. Resize fires in the pump() triggered by the
    // write callback.
    const h = makeScrollHarness({ scrollbackLimit: 100, rows: 25 })
    h.io.reset(1)
    h.addLines(100)
    h.userScrollTo(50)

    h.primeWrite({ linesAdded: 3, autoScroll: true })
    h.io.enqueue(enc('chunk-A'), 1)
    expect(h.viewportY).toBe(50)

    h.io.requestResize({ cols: 80, rows: 20 }, 1)
    // Resize is pending, will fire when the write callback calls pump().
    expect(h.resizeCalls).toEqual([])

    h.flushOne() // callback fires → pump() → resize applied
    expect(h.resizeCalls).toEqual([{ cols: 80, rows: 20 }])
    h.cleanup()
  })

  it('processes resize normally when no write is in flight', () => {
    const h = makeScrollHarness({ scrollbackLimit: 100, rows: 25 })
    h.io.reset(1)
    h.addLines(50)
    h.userScrollTo(20)

    h.primeWrite({})
    h.io.enqueue(enc('output'), 1)
    h.flushOne()

    h.io.requestResize({ cols: 120, rows: 40 }, 1)
    expect(h.resizeCalls).toEqual([{ cols: 120, rows: 40 }])
    h.cleanup()
  })

  it('forceNextScrollToBottom overrides scroll preservation for one write', () => {
    const h = makeScrollHarness({ scrollbackLimit: 100, rows: 25 })
    h.io.reset(1)
    h.addLines(50) // baseY=50
    h.userScrollTo(10) // user scrolled up

    // Simulate what terminal.tsx does before enqueuing replay.
    h.io.forceNextScrollToBottom()

    // Force flag consumed: captureScroll skips → auto-scroll stands.
    h.primeWrite({ linesAdded: 5, autoScroll: true })
    h.io.enqueue(wrapBSU('replay-frame'), 1)
    expect(h.viewportY).toBe(h.baseY) // stayed at bottom
    h.flushOne()
    h.cleanup()
  })

  it('forceNextScrollToBottom only applies to the next write, not subsequent ones', () => {
    const h = makeScrollHarness({ scrollbackLimit: 100, rows: 25 })
    h.io.reset(1)
    h.addLines(50)
    h.userScrollTo(10)

    h.io.forceNextScrollToBottom()

    // First write: force consumed → user ends at bottom.
    h.primeWrite({ linesAdded: 5, autoScroll: true })
    h.io.enqueue(wrapBSU('replay'), 1)
    expect(h.viewportY).toBe(h.baseY)
    h.flushOne()

    // User scrolls up again.
    h.userScrollTo(20)

    // Second write: force is consumed, scroll is preserved.
    h.primeWrite({ linesAdded: 3, autoScroll: true })
    h.io.enqueue(wrapBSU('live-update'), 1)
    expect(h.viewportY).toBe(20)
    h.flushOne()
    h.cleanup()
  })

  it('resets scroll state on epoch change — stale snap does not affect new epoch', () => {
    // What reset() guarantees: scrollSnap captured under epoch N is
    // discarded, so a partially-completed write cannot corrupt new-epoch
    // scroll state. New-epoch writes still independently save/restore if
    // the user is scrolled up (that is correct behaviour, not a bug).
    const h = makeScrollHarness({ scrollbackLimit: 100, rows: 25 })
    h.io.reset(1)
    h.addLines(100)
    h.userScrollTo(50)

    // Epoch 1 write in flight (callback not yet fired).
    h.primeWrite({ linesAdded: 2, autoScroll: true })
    h.io.enqueue(enc('data'), 1)
    expect(h.viewportY).toBe(50) // synchronous restore happened

    // Reset clears scrollSnap and the queue.
    h.io.reset(2)
    h.userScrollToBottom() // simulate session reconnect puts user at bottom

    // New epoch write; user is at bottom → no capture → no restore.
    h.io.enqueue(enc('new session'), 2)
    const callsBefore = h.scrollToLineCalls.length
    h.flushOne() // epoch-1 callback (stale, ignored by onWritten)
    h.flushOne() // epoch-2 callback
    // No extra scrollToLine calls since user was at bottom.
    expect(h.scrollToLineCalls.length).toBe(callsBefore)
    h.cleanup()
  })
})
