import { describe, it, expect, vi, beforeEach } from 'vitest'

// Reset module cache between tests so each starts from a clean import.
beforeEach(() => {
  vi.resetModules()
})

describe('getGhosttyCore', () => {
  it('loads a fresh, independent core on every call (no shared singleton)', async () => {
    // Each WTerm MUST own its own GhosttyCore — sharing one corrupts WASM
    // terminal state across instances. So repeated calls must NOT return the
    // same instance.
    const mockLoad = vi.fn().mockImplementation(() => Promise.resolve({ init: vi.fn() }))
    vi.doMock('@wterm/ghostty', () => ({
      GhosttyCore: { load: mockLoad },
    }))
    const { getGhosttyCore } = await import('./terminal-init')

    const core1 = await getGhosttyCore()
    const core2 = await getGhosttyCore()
    expect(core1).not.toBe(core2)
  })

  it('calls GhosttyCore.load once per call with the expected options', async () => {
    const mockLoad = vi.fn().mockResolvedValue({ init: vi.fn() })
    vi.doMock('@wterm/ghostty', () => ({
      GhosttyCore: { load: mockLoad },
    }))
    const { getGhosttyCore } = await import('./terminal-init')

    await getGhosttyCore()
    await getGhosttyCore()
    await getGhosttyCore()
    expect(mockLoad).toHaveBeenCalledTimes(3)
    expect(mockLoad).toHaveBeenCalledWith({ scrollbackLimit: 10000, wasmPath: '/ghostty-vt.wasm' })
  })

  it('exports prefetchCache as an empty Map', async () => {
    vi.doMock('@wterm/ghostty', () => ({
      GhosttyCore: { load: vi.fn().mockResolvedValue({}) },
    }))
    const { prefetchCache } = await import('./terminal-init')
    expect(prefetchCache).toBeInstanceOf(Map)
    expect(prefetchCache.size).toBe(0)
  })
})

describe('ScrollbackCapturingCore.writeRaw', () => {
  // Build a minimal inner core that simulates a real terminal:
  // tracks a viewport of given size, scrolls when the cursor hits the bottom.
  function makeInner(rows: number, cols: number) {
    const viewport: number[][] = Array.from({ length: rows }, () => Array(cols).fill(0x20))
    let native = 0
    let cursorRow = 0
    let cursorCol = 0
    return {
      usingAltScreen: () => false,
      getRows: () => rows,
      getCols: () => cols,
      getScrollbackCount: () => native,
      getCell: (r: number, c: number) => ({
        char: viewport[r]?.[c] ?? 0x20, fg: 256, bg: 256, flags: 0,
      }),
      writeRaw: vi.fn().mockImplementation((data: Uint8Array) => {
        for (const b of data) {
          if (b === 0x0a) {
            if (cursorRow === rows - 1) {
              viewport.shift()
              viewport.push(Array(cols).fill(0x20))
              native++
            } else {
              cursorRow++
            }
            cursorCol = 0
          } else if (cursorRow < rows && cursorCol < cols) {
            viewport[cursorRow][cursorCol++] = b
          }
        }
      }),
      isDirtyRow: () => false,
      clearDirty: vi.fn(),
      init: vi.fn(),
      resize: vi.fn(),
    }
  }

  it('captures each scrolled-off row with its actual content, not a blank pre-write snapshot', async () => {
    vi.doMock('@wterm/ghostty', () => ({ GhosttyCore: { load: vi.fn() } }))
    const { ScrollbackCapturingCore } = await import('./terminal-init')

    const inner = makeInner(2, 5)  // 2-row terminal
    const core = new ScrollbackCapturingCore(inner as any, 100)

    // Writing 4 lines into a 2-row terminal scrolls 3 rows off (A, B, C).
    // The old single-snapshot writeRaw captures a blank viewport for all of them.
    core.writeRaw(new TextEncoder().encode('A\nB\nC\nD\n'))

    expect(core.getScrollbackCount()).toBe(3)
    expect(core.getScrollbackCell(0, 0).char).toBe('C'.charCodeAt(0))  // most recent
    expect(core.getScrollbackCell(1, 0).char).toBe('B'.charCodeAt(0))
    expect(core.getScrollbackCell(2, 0).char).toBe('A'.charCodeAt(0))  // oldest
  })
})
