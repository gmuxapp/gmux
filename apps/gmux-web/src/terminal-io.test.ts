import { ScrollAnchorAddon } from '@gmux/scroll-anchor'
import { describe, expect, it, vi } from 'vitest'
import { createTerminalIO } from './terminal-io'

const enc = (s: string) => new TextEncoder().encode(s)

function makeHarness(isBusy: () => boolean = () => false) {
  const writes: string[] = []
  const resizes: Array<{ cols: number, rows: number }> = []
  const pending: Array<() => void> = []
  const io = createTerminalIO({
    write(data, callback) {
      writes.push(typeof data === 'string' ? data : new TextDecoder().decode(data))
      pending.push(() => callback?.())
    },
    resize(cols, rows) { resizes.push({ cols, rows }) },
  }, { isBusy })
  return {
    io, writes, resizes,
    flushOne() {
      const callback = pending.shift()
      if (!callback) throw new Error('no pending write callback')
      callback()
    },
    flushAll() { while (pending.length) pending.shift()?.() },
  }
}

describe('createTerminalIO', () => {
  it('serializes writes one at a time', () => {
    const h = makeHarness()
    h.io.reset(1)
    h.io.enqueue(enc('a'), 1)
    h.io.enqueue(enc('b'), 1)
    h.io.enqueue(enc('c'), 1)
    expect(h.writes).toEqual(['a'])
    h.flushOne(); expect(h.writes).toEqual(['a', 'b'])
    h.flushOne(); expect(h.writes).toEqual(['a', 'b', 'c'])
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
    expect(h.writes).toEqual(['stale'])
    h.flushOne()
    expect(onWritten).not.toHaveBeenCalled()
    expect(h.writes).toEqual(['stale', 'fresh'])
    expect(h.resizes).toEqual([])
  })

  it('does not let an old completion advance a new epoch past a resize fence', () => {
    const h = makeHarness()
    h.io.reset(1); h.io.enqueue(enc('old'), 1)
    h.io.reset(2); h.io.enqueue(enc('new'), 2)
    h.io.requestResize({ cols: 80, rows: 24 }, 2)
    h.flushOne()
    expect(h.writes).toEqual(['old', 'new'])
    expect(h.resizes).toEqual([])
    h.flushOne()
    expect(h.resizes).toEqual([{ cols: 80, rows: 24 }])
  })

  it('holds an ordered resize barrier and its writes behind the busy fence', () => {
    let busy = true
    const h = makeHarness(() => busy)
    h.io.reset(1)
    h.io.enqueueResizeThenMany({ cols: 80, rows: 25 }, [enc('a'), enc('b')], 1)

    expect(h.resizes).toEqual([])
    expect(h.writes).toEqual([])

    busy = false
    h.io.busyStateChanged()
    expect(h.resizes).toEqual([{ cols: 80, rows: 25 }])
    expect(h.writes).toEqual(['a'])
    h.flushOne()
    expect(h.writes).toEqual(['a', 'b'])
  })

  it('drops every queued old-epoch payload behind an unfinished write', () => {
    const h = makeHarness()
    h.io.reset(1)
    h.io.enqueueMany([enc('old-in-flight'), enc('old-queued-a'), enc('old-queued-b')], 1)
    expect(h.writes).toEqual(['old-in-flight'])

    h.io.reset(2)
    h.io.enqueueMany([enc('new-a'), enc('new-b')], 2)
    h.flushOne()
    expect(h.writes).toEqual(['old-in-flight', 'new-a'])
    h.flushOne()
    expect(h.writes).toEqual(['old-in-flight', 'new-a', 'new-b'])
  })

  it('suppresses a resize callback when applying the resize resets its epoch', () => {
    const onApplied = vi.fn()
    let io: ReturnType<typeof createTerminalIO>
    const term = {
      write(_data: string | Uint8Array, callback?: () => void) { callback?.() },
      resize() { io.reset(2) },
    }
    io = createTerminalIO(term)
    io.reset(1)
    io.enqueueResizeThenMany({ cols: 80, rows: 25 }, [], 1, undefined, onApplied)
    expect(onApplied).not.toHaveBeenCalled()
  })

  it('applies enqueueResizeThenMany before writes and completes after its final chunk', () => {
    const h = makeHarness()
    const done = vi.fn()
    const resized = vi.fn()
    h.io.reset(1)
    h.io.enqueueResizeThenMany({ cols: 80, rows: 25 }, [enc('a'), enc('b'), enc('c')], 1, done, resized)
    // A later coalescible request cannot overwrite the ordered barrier.
    h.io.requestResize({ cols: 114, rows: 44 }, 1)
    expect(h.resizes).toEqual([{ cols: 80, rows: 25 }])
    expect(resized).toHaveBeenCalledTimes(1)
    expect(h.writes).toEqual(['a'])
    h.flushAll()
    expect(h.writes).toEqual(['a', 'b', 'c'])
    expect(h.resizes).toEqual([{ cols: 80, rows: 25 }, { cols: 114, rows: 44 }])
    expect(done).toHaveBeenCalledTimes(1)
  })

  it('defers and coalesces resize between split BSU and ESU writes', () => {
    let busy = true
    const h = makeHarness(() => busy)
    h.io.reset(1)
    h.io.enqueue(enc('\x1b[?2026hpartial'), 1)
    h.flushOne()
    h.io.requestResize({ cols: 80, rows: 24 }, 1)
    h.io.requestResize({ cols: 100, rows: 30 }, 1)
    expect(h.resizes).toEqual([])
    h.io.enqueue(enc('rest\x1b[?2026l'), 1)
    h.flushOne()
    expect(h.resizes).toEqual([])
    busy = false
    h.io.busyStateChanged()
    expect(h.resizes).toEqual([{ cols: 100, rows: 30 }])
  })

  it('keeps resize deferred through the post-wipe re-resolution window', () => {
    let busy = true
    const h = makeHarness(() => busy)
    h.io.reset(1)
    h.io.requestResize({ cols: 120, rows: 40 }, 1)
    expect(h.resizes).toEqual([])
    // ESU has closed, but the addon's combined busy fence remains true until
    // its deterministic restore and rendering catch-up have completed.
    h.io.busyStateChanged()
    expect(h.resizes).toEqual([])
    busy = false
    h.io.busyStateChanged()
    expect(h.resizes).toEqual([{ cols: 120, rows: 40 }])
  })

  it('defers a real addon-integrated resize through ED3 and its rAF', () => {
    let viewportY = 20
    let baseY = 20
    const raf = { current: null as FrameRequestCallback | null }
    vi.stubGlobal('requestAnimationFrame', (callback: FrameRequestCallback) => { raf.current = callback; return 1 })
    const scrollListeners = new Set<() => void>()
    const parsedListeners = new Set<() => void>()
    const handlers = new Map<string, (params: Array<number | number[]>) => boolean>()
    const resizes: Array<{ cols: number, rows: number }> = []
    const active = {
      type: 'normal',
      get viewportY() { return viewportY },
      get baseY() { return baseY },
      getLine() { return { translateToString: () => 'anchor line' } },
    }
    const terminal = {
      element: new EventTarget(),
      rows: 10,
      buffer: { active },
      parser: {
        registerCsiHandler(id: { prefix?: string, final: string }, handler: (params: Array<number | number[]>) => boolean) {
          const key = `${id.prefix ?? ''}${id.final}`
          handlers.set(key, handler)
          return { dispose: () => handlers.delete(key) }
        },
      },
      onScroll(listener: () => void) { scrollListeners.add(listener); return { dispose: () => scrollListeners.delete(listener) } },
      onWriteParsed(listener: () => void) { parsedListeners.add(listener); return { dispose: () => parsedListeners.delete(listener) } },
      scrollToLine(line: number) { viewportY = Math.min(line, baseY); for (const listener of scrollListeners) listener() },
      scrollToBottom() { viewportY = baseY; for (const listener of scrollListeners) listener() },
      write(_data: Uint8Array, callback?: () => void) { callback?.() },
      resize(cols: number, rows: number) { resizes.push({ cols, rows }) },
    }
    const addon = new ScrollAnchorAddon()
    addon.activate(terminal as any)
    const io = createTerminalIO(terminal, { isBusy: () => addon.busy })
    addon.onBusyChange(() => io.busyStateChanged())
    io.reset(1)

    terminal.element.dispatchEvent(new Event('wheel'))
    viewportY = 10
    for (const listener of scrollListeners) listener()
    handlers.get('?h')?.([2026])
    handlers.get('J')?.([3])
    viewportY = 0
    baseY = 12
    handlers.get('?l')?.([2026])
    for (const listener of parsedListeners) listener()

    io.requestResize({ cols: 120, rows: 40 }, 1)
    // ESU already closed DEC 2026 here: the fence stays shut only because the
    // addon still owes a post-ED3 re-resolution, which is the whole reason
    // terminal-io gates on `busy` rather than synchronized output alone.
    expect(addon.busy).toBe(true)
    expect(resizes).toEqual([])
    expect(raf.current).not.toBeNull()
    raf.current?.(0)
    expect(addon.busy).toBe(false)
    expect(resizes).toEqual([{ cols: 120, rows: 40 }])
    addon.dispose()
    vi.unstubAllGlobals()
  })

})
