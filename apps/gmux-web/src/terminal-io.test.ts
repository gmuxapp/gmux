import { describe, expect, it, vi } from 'vitest'
import { createTerminalIO } from './terminal-io'

const enc = (s: string) => new TextEncoder().encode(s)

function makeHarness(isSyncActive: () => boolean = () => false) {
  const writes: string[] = []
  const resizes: Array<{ cols: number, rows: number }> = []
  const pending: Array<() => void> = []
  const io = createTerminalIO({
    write(data, callback) {
      writes.push(typeof data === 'string' ? data : new TextDecoder().decode(data))
      pending.push(() => callback?.())
    },
    resize(cols, rows) { resizes.push({ cols, rows }) },
  }, { isSyncActive })
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

  it('runs completion after the final enqueueMany chunk', () => {
    const h = makeHarness()
    const done = vi.fn()
    h.io.reset(1)
    h.io.enqueueMany([enc('a'), enc('b'), enc('c')], 1, done)
    h.flushAll()
    expect(h.writes).toEqual(['a', 'b', 'c'])
    expect(done).toHaveBeenCalledTimes(1)
  })

  it('defers and coalesces resize while synchronized output is active', () => {
    let syncActive = true
    const h = makeHarness(() => syncActive)
    h.io.reset(1)
    h.io.requestResize({ cols: 80, rows: 24 }, 1)
    h.io.requestResize({ cols: 100, rows: 30 }, 1)
    expect(h.resizes).toEqual([])
    syncActive = false
    h.io.syncStateChanged()
    expect(h.resizes).toEqual([{ cols: 100, rows: 30 }])
  })
})
