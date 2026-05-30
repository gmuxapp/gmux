import { describe, it, expect, vi, beforeEach } from 'vitest'

// Reset module cache between tests so singleton state is fresh
beforeEach(() => {
  vi.resetModules()
})

describe('getGhosttyCore', () => {
  it('returns the same promise on repeated calls (singleton)', async () => {
    const mockCore = { init: vi.fn() }
    vi.doMock('@wterm/ghostty', () => ({
      GhosttyCore: { load: vi.fn().mockResolvedValue(mockCore) },
    }))
    const { getGhosttyCore } = await import('./terminal-init')

    const p1 = getGhosttyCore()
    const p2 = getGhosttyCore()
    expect(p1).toBe(p2)
    await expect(p1).resolves.toBe(mockCore)
  })

  it('calls GhosttyCore.load exactly once across multiple calls', async () => {
    const mockLoad = vi.fn().mockResolvedValue({ init: vi.fn() })
    vi.doMock('@wterm/ghostty', () => ({
      GhosttyCore: { load: mockLoad },
    }))
    const { getGhosttyCore } = await import('./terminal-init')

    await getGhosttyCore()
    await getGhosttyCore()
    await getGhosttyCore()
    expect(mockLoad).toHaveBeenCalledTimes(1)
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
