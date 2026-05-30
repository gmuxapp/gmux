import { describe, expect, it, vi } from 'vitest'
import { createTerminalIO } from './terminal-io'
import type { WTerm } from '@wterm/dom'

const enc = (s: string) => new TextEncoder().encode(s)

function makeTerm() {
  const writes: string[] = []
  const resizes: Array<{ cols: number; rows: number }> = []
  const term = {
    write(data: string | Uint8Array) {
      writes.push(typeof data === 'string' ? data : new TextDecoder().decode(data))
    },
    resize(cols: number, rows: number) { resizes.push({ cols, rows }) },
  } as unknown as WTerm
  return { term, writes, resizes }
}

describe('createTerminalIO', () => {
  it('writes data synchronously when epoch matches', () => {
    const { term, writes } = makeTerm()
    const io = createTerminalIO(term)
    io.reset(1)
    io.write(enc('hello'), 1)
    expect(writes).toEqual(['hello'])
  })

  it('drops write when epoch is stale', () => {
    const { term, writes } = makeTerm()
    const io = createTerminalIO(term)
    io.reset(2)
    io.write(enc('stale'), 1)
    expect(writes).toEqual([])
  })

  it('writeMany writes all chunks synchronously in order', () => {
    const { term, writes } = makeTerm()
    const io = createTerminalIO(term)
    io.reset(1)
    io.writeMany([enc('a'), enc('b'), enc('c')], 1)
    expect(writes).toEqual(['a', 'b', 'c'])
  })

  it('writeMany fires onDone via requestAnimationFrame after writing', () => {
    const rafCallbacks: FrameRequestCallback[] = []
    vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback) => {
      rafCallbacks.push(cb)
      return 0
    })

    const { term } = makeTerm()
    const io = createTerminalIO(term)
    const done = vi.fn()
    io.reset(1)
    io.writeMany([enc('x'), enc('y')], 1, done)
    expect(done).not.toHaveBeenCalled()
    rafCallbacks.forEach(cb => cb(0))
    expect(done).toHaveBeenCalledTimes(1)

    vi.unstubAllGlobals()
  })

  it('writeMany drops all chunks when epoch is stale', () => {
    const { term, writes } = makeTerm()
    const io = createTerminalIO(term)
    io.reset(2)
    io.writeMany([enc('a'), enc('b')], 1)
    expect(writes).toEqual([])
  })

  it('writeMany does nothing when chunks array is empty', () => {
    const rafCalled = vi.fn()
    vi.stubGlobal('requestAnimationFrame', rafCalled)
    const { term, writes } = makeTerm()
    const io = createTerminalIO(term)
    const done = vi.fn()
    io.reset(1)
    io.writeMany([], 1, done)
    expect(writes).toEqual([])
    expect(rafCalled).not.toHaveBeenCalled()
    expect(done).not.toHaveBeenCalled()
    vi.unstubAllGlobals()
  })

  it('resize passes through synchronously when epoch matches', () => {
    const { term, resizes } = makeTerm()
    const io = createTerminalIO(term)
    io.reset(1)
    io.resize({ cols: 120, rows: 40 }, 1)
    expect(resizes).toEqual([{ cols: 120, rows: 40 }])
  })

  it('resize is dropped when epoch is stale', () => {
    const { term, resizes } = makeTerm()
    const io = createTerminalIO(term)
    io.reset(2)
    io.resize({ cols: 80, rows: 24 }, 1)
    expect(resizes).toEqual([])
  })

  it('reset clears epoch so old-epoch calls are dropped', () => {
    const { term, writes } = makeTerm()
    const io = createTerminalIO(term)
    io.reset(1)
    io.reset(2)
    io.write(enc('old'), 1)
    io.write(enc('new'), 2)
    expect(writes).toEqual(['new'])
  })
})
