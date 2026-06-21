import { describe, it, expect, vi } from 'vitest'
import { refreshAtlasWhenIconFontLoads } from './nerd-font'

/** Minimal FontFaceSet fake: drives `loadingdone` and a togglable check(). */
function makeFonts(initiallyLoaded: boolean) {
  let loaded = initiallyLoaded
  const listeners = new Set<() => void>()
  return {
    set loaded(v: boolean) {
      loaded = v
    },
    fire() {
      for (const l of [...listeners]) l()
    },
    listenerCount: () => listeners.size,
    set: {
      check: vi.fn(() => loaded),
      addEventListener: vi.fn((_: string, cb: () => void) => listeners.add(cb)),
      removeEventListener: vi.fn((_: string, cb: () => void) => listeners.delete(cb)),
    } as unknown as FontFaceSet,
  }
}

describe('refreshAtlasWhenIconFontLoads', () => {
  it('does nothing when the icon font is already loaded', () => {
    const fonts = makeFonts(true)
    const term = { clearTextureAtlas: vi.fn() }

    refreshAtlasWhenIconFontLoads(term, 13, fonts.set)

    expect(fonts.set.addEventListener).not.toHaveBeenCalled()
    expect(term.clearTextureAtlas).not.toHaveBeenCalled()
  })

  it('clears the atlas once the icon font finishes loading', () => {
    const fonts = makeFonts(false)
    const term = { clearTextureAtlas: vi.fn() }

    refreshAtlasWhenIconFontLoads(term, 13, fonts.set)

    // Some other font finished first: icon font still unavailable, no clear.
    fonts.fire()
    expect(term.clearTextureAtlas).not.toHaveBeenCalled()

    // Now the icon font is ready.
    fonts.loaded = true
    fonts.fire()
    expect(term.clearTextureAtlas).toHaveBeenCalledTimes(1)
  })

  it('detaches its listener after the first successful refresh', () => {
    const fonts = makeFonts(false)
    const term = { clearTextureAtlas: vi.fn() }

    refreshAtlasWhenIconFontLoads(term, 13, fonts.set)
    fonts.loaded = true
    fonts.fire()
    fonts.fire() // a later loadingdone must not re-clear

    expect(term.clearTextureAtlas).toHaveBeenCalledTimes(1)
    expect(fonts.listenerCount()).toBe(0)
  })

  it('disposer removes the listener', () => {
    const fonts = makeFonts(false)
    const term = { clearTextureAtlas: vi.fn() }

    const dispose = refreshAtlasWhenIconFontLoads(term, 13, fonts.set)
    expect(fonts.listenerCount()).toBe(1)
    dispose()
    expect(fonts.listenerCount()).toBe(0)
  })
})
