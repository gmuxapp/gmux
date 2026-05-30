// @vitest-environment jsdom
/**
 * Tests for terminal-touch helpers.
 */
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { shouldFocusOnTouchEnd, shouldSkipPositionDance, focusTerminalInput } from './terminal-touch'
import type { WTerm } from '@wterm/dom'

// ── shouldFocusOnTouchEnd ──

describe('shouldFocusOnTouchEnd', () => {
  it('returns true when the user tapped (no move, no long-press)', () => {
    expect(shouldFocusOnTouchEnd({ moved: false, wasLongPress: false })).toBe(true)
  })

  it('returns false when the user dragged', () => {
    expect(shouldFocusOnTouchEnd({ moved: true, wasLongPress: false })).toBe(false)
  })

  it('returns false when a long-press was detected (text selection gesture)', () => {
    expect(shouldFocusOnTouchEnd({ moved: false, wasLongPress: true })).toBe(false)
  })

  it('returns false when both moved and long-press are true', () => {
    expect(shouldFocusOnTouchEnd({ moved: true, wasLongPress: true })).toBe(false)
  })
})

// ── shouldSkipPositionDance ──

describe('shouldSkipPositionDance', () => {
  it('returns true when already focused — skip the position dance', () => {
    expect(shouldSkipPositionDance(true)).toBe(true)
  })

  it('returns false when not focused — run the dance', () => {
    expect(shouldSkipPositionDance(false)).toBe(false)
  })
})

// ── focusTerminalInput ──

describe('focusTerminalInput', () => {
  it('calls term.focus() when term is provided', () => {
    const term = { focus: vi.fn() } as unknown as WTerm
    focusTerminalInput(term)
    expect(term.focus).toHaveBeenCalledOnce()
  })

  it('does nothing when term is null', () => {
    expect(() => focusTerminalInput(null)).not.toThrow()
  })
})

// ── Long-press copies all terminal text ──

describe('useTouchPan — long-press copies all terminal text', () => {
  beforeEach(() => {
    vi.stubGlobal('navigator', {
      clipboard: { writeText: vi.fn().mockResolvedValue(undefined) },
    })
  })

  it('selectAllAndCopy is called with term.element on long-press', async () => {
    // Test the underlying operation: selectAllAndCopy uses window.getSelection()
    // The long-press handler in useTouchPan calls selectAllAndCopy(term.element)
    const writeText = vi.fn().mockResolvedValue(undefined)
    vi.stubGlobal('navigator', { clipboard: { writeText } })

    const mockSel = {
      removeAllRanges: vi.fn(),
      addRange: vi.fn(),
      toString: vi.fn().mockReturnValue('hello from terminal'),
    }
    vi.stubGlobal('getSelection', vi.fn().mockReturnValue(mockSel))
    vi.stubGlobal('document', {
      createRange: vi.fn().mockReturnValue({ selectNodeContents: vi.fn() }),
    })

    const { selectAllAndCopy } = await import('./selection')
    const el = {} as HTMLElement
    selectAllAndCopy(el)

    expect(mockSel.removeAllRanges).toHaveBeenCalled()
    expect(writeText).toHaveBeenCalledWith('hello from terminal')
    vi.unstubAllGlobals()
  })
})
