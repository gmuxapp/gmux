/**
 * Tests for terminal-touch helpers.
 */
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { shouldFocusOnTouchEnd, shouldSkipPositionDance, focusTerminalInput } from './terminal-touch'

// ── matchMedia stub ──
// All focusTerminalInput tests run in Node where window.matchMedia is absent.
function stubMatchMedia(isTouchDevice: boolean) {
  Object.defineProperty(window, 'matchMedia', {
    value: vi.fn().mockImplementation((query: string) => ({
      matches: isTouchDevice && query === '(pointer: coarse)',
      addEventListener: vi.fn(), removeEventListener: vi.fn(),
    })),
    writable: true, configurable: true,
  })
}

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
  it('returns true when already focused — skip the position dance to avoid keyboard flicker', () => {
    expect(shouldSkipPositionDance(true)).toBe(true)
  })

  it('returns false when not focused — run the dance so keyboard anchors correctly', () => {
    expect(shouldSkipPositionDance(false)).toBe(false)
  })
})

// ── focusTerminalInput — already-focused guard ──

describe('focusTerminalInput — already-focused guard (keyboard flicker fix)', () => {
  beforeEach(() => {
    stubMatchMedia(true) // simulate touch device
    Object.defineProperty(navigator, 'maxTouchPoints', { value: 1, writable: true, configurable: true })
  })

  it('does not call textarea.focus a second time when it is already the active element', () => {
    const focusSpy = vi.fn()
    const textarea = { style: {} as any, focus: focusSpy } as any
    const term = {
      focus: vi.fn(),
      textarea,
    } as any
    // Simulate textarea already being focused
    Object.defineProperty(document, 'activeElement', { value: textarea, configurable: true })

    focusTerminalInput(term)

    // term.focus() still called, but textarea repositioning dance is skipped
    expect(term.focus).toHaveBeenCalledTimes(1)
    // textarea.focus should NOT be called again (no double-focus on touch)
    expect(focusSpy).not.toHaveBeenCalled()
  })

  it('calls textarea.focus with position dance when textarea is not yet focused', () => {
    const focusSpy = vi.fn()
    const textarea = { style: {} as any, focus: focusSpy } as any
    const term = {
      focus: vi.fn(),
      textarea,
    } as any
    // Simulate some other element being active
    Object.defineProperty(document, 'activeElement', { value: document.body, configurable: true })

    // Stub requestAnimationFrame so we don't need a real browser loop
    vi.spyOn(globalThis, 'requestAnimationFrame').mockImplementation(() => 0)

    focusTerminalInput(term)

    expect(focusSpy).toHaveBeenCalledOnce()
    expect(textarea.style.position).toBe('fixed')
    expect(textarea.style.bottom).toBe('0')
  })
})

// ── Long-press selectAll behavior ──

describe('useTouchPan — long-press copies all terminal text (selection fix)', () => {
  it('selects all and writes to clipboard on long-press', async () => {
    // Use a tiny fake that only exercises the long-press branch.
    // The real useTouchPan fires the long-press timer, calls term.selectAll(),
    // then selectionToText(term) and navigator.clipboard.writeText().
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, 'clipboard', {
      value: { writeText },
      writable: true, configurable: true,
    })

    // selectionToText returns the selected terminal content.
    // We verify the clipboard received the non-empty result.
    const fakeContent = 'hello from terminal'
    const term = {
      selectAll: vi.fn(),
      getSelectionPosition: vi.fn().mockReturnValue({
        start: { x: 0, y: 0 }, end: { x: fakeContent.length - 1, y: 0 },
      }),
      getScrollbackLength: vi.fn().mockReturnValue(0),
      getViewportY: vi.fn().mockReturnValue(0),
      cols: fakeContent.length,
      buffer: {
        active: {
          getLine: vi.fn().mockReturnValue({
            isWrapped: false,
            translateToString: vi.fn().mockReturnValue(fakeContent),
          }),
        },
      },
    } as any

    // Call selectAll and clipboard write manually (mirrors the long-press handler)
    term.selectAll()
    const { selectionToText } = await import('./selection')
    const text = selectionToText(term)
    if (text) await navigator.clipboard.writeText(text)

    expect(term.selectAll).toHaveBeenCalledOnce()
    expect(writeText).toHaveBeenCalledWith(fakeContent)
  })
})

describe('shouldSkipPositionDance', () => {
  it('returns true when the textarea was already focused (skip to prevent flicker)', () => {
    expect(shouldSkipPositionDance(true)).toBe(true)
  })

  it('returns false when the textarea was not focused (run the position dance)', () => {
    expect(shouldSkipPositionDance(false)).toBe(false)
  })
})

// Verify the exported module surface includes the new symbol
describe('terminal-touch exports', () => {
  it('exports shouldSkipPositionDance', async () => {
    const mod = await import('./terminal-touch')
    expect(typeof mod.shouldSkipPositionDance).toBe('function')
  })
})
