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
