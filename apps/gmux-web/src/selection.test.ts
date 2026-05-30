// @vitest-environment jsdom
/**
 * selection.ts now uses window.getSelection() — no terminal buffer API.
 * Tests live here after consolidation.
 */
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { getSelectionText, selectAllAndCopy, clearSelection } from './selection'

// ── getSelectionText ──

describe('getSelectionText', () => {
  it('returns selected text from window.getSelection()', () => {
    vi.stubGlobal('getSelection', vi.fn().mockReturnValue({ toString: () => 'copied text' }))
    expect(getSelectionText()).toBe('copied text')
    vi.unstubAllGlobals()
  })

  it('returns empty string when getSelection is null', () => {
    vi.stubGlobal('getSelection', vi.fn().mockReturnValue(null))
    expect(getSelectionText()).toBe('')
    vi.unstubAllGlobals()
  })
})

// ── clearSelection ──

describe('clearSelection', () => {
  it('calls removeAllRanges on the selection', () => {
    const removeAllRanges = vi.fn()
    vi.stubGlobal('getSelection', vi.fn().mockReturnValue({ removeAllRanges }))
    clearSelection()
    expect(removeAllRanges).toHaveBeenCalledOnce()
    vi.unstubAllGlobals()
  })
})

// ── selectAllAndCopy ──

describe('selectAllAndCopy', () => {
  beforeEach(() => {
    vi.stubGlobal('navigator', {
      clipboard: { writeText: vi.fn().mockResolvedValue(undefined) },
    })
  })

  it('selects all content and writes to clipboard', () => {
    const range = { selectNodeContents: vi.fn() }
    vi.stubGlobal('document', {
      createRange: vi.fn().mockReturnValue(range),
    })
    const removeAllRanges = vi.fn()
    const addRange = vi.fn()
    vi.stubGlobal('getSelection', vi.fn().mockReturnValue({
      removeAllRanges,
      addRange,
      toString: () => 'terminal text',
    }))

    selectAllAndCopy({} as HTMLElement)

    expect(removeAllRanges).toHaveBeenCalled()
    expect(addRange).toHaveBeenCalledWith(range)
    expect(navigator.clipboard.writeText).toHaveBeenCalledWith('terminal text')
    vi.unstubAllGlobals()
  })

  it('does not write to clipboard when selection is empty', () => {
    const range = { selectNodeContents: vi.fn() }
    vi.stubGlobal('document', {
      createRange: vi.fn().mockReturnValue(range),
    })
    vi.stubGlobal('getSelection', vi.fn().mockReturnValue({
      removeAllRanges: vi.fn(),
      addRange: vi.fn(),
      toString: () => '',
    }))

    selectAllAndCopy({} as HTMLElement)
    expect(navigator.clipboard.writeText).not.toHaveBeenCalled()
    vi.unstubAllGlobals()
  })
})
