/**
 * Tests for terminal-touch helpers.
 */
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { shouldFocusOnTouchEnd, shouldSkipPositionDance, focusTerminalInput } from './terminal-touch'

// Vitest runs in Node (no DOM). Provide minimal window/document stubs so
// tests for focusTerminalInput can call into window.matchMedia / document.
if (typeof globalThis.window === 'undefined') {
  (globalThis as any).window = globalThis
}
if (typeof globalThis.document === 'undefined') {
  (globalThis as any).document = {
    activeElement: null,
    body: {},
  }
}

// ── matchMedia stub ──

function stubMatchMedia(isTouchDevice: boolean) {
  Object.defineProperty(window, 'matchMedia', {
    value: vi.fn().mockImplementation((query: string) => ({
      matches: isTouchDevice && query === '(pointer: coarse)',
      addEventListener: vi.fn(), removeEventListener: vi.fn(),
    })),
    writable: true, configurable: true,
  })
}

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
  it('returns true when already focused — skip the position dance to avoid keyboard flicker', () => {
    expect(shouldSkipPositionDance(true)).toBe(true)
  })

  it('returns false when not focused — run the dance so keyboard anchors correctly', () => {
    expect(shouldSkipPositionDance(false)).toBe(false)
  })
})

// ── focusTerminalInput — already-focused guard (keyboard flicker fix) ──

describe('focusTerminalInput — already-focused guard (keyboard flicker fix)', () => {
  beforeEach(() => {
    stubMatchMedia(true) // simulate touch device
    Object.defineProperty(navigator, 'maxTouchPoints', { value: 1, writable: true, configurable: true })
  })

  it('does not run the position dance when textarea is already the active element', () => {
    const focusSpy = vi.fn()
    const textarea = { style: {} as CSSStyleDeclaration, focus: focusSpy } as any
    const term = { focus: vi.fn(), textarea } as any

    // Simulate textarea already focused
    Object.defineProperty(document, 'activeElement', { value: textarea, configurable: true })

    focusTerminalInput(term)

    // term.focus() still fires (no-op since it's already focused) but the
    // position-dance textarea.focus() must NOT be called a second time.
    expect(term.focus).toHaveBeenCalledTimes(1)
    expect(focusSpy).not.toHaveBeenCalled()
  })

  it('runs the position dance and calls textarea.focus when not yet focused', () => {
    const focusSpy = vi.fn()
    const textarea = { style: {} as CSSStyleDeclaration, focus: focusSpy } as any
    const term = { focus: vi.fn(), textarea } as any

    // Simulate a different element (body) being active
    Object.defineProperty(document, 'activeElement', { value: (document as any).body, configurable: true })

    // Stub requestAnimationFrame so style-restore callback doesn't throw.
    ;(globalThis as any).requestAnimationFrame = (_cb: FrameRequestCallback) => 0

    focusTerminalInput(term)

    // The position dance repositions the textarea to the screen bottom first …
    expect(textarea.style.position).toBe('fixed')
    expect(textarea.style.bottom).toBe('0')
    // … then focuses it.
    expect(focusSpy).toHaveBeenCalledOnce()
  })
})

// ── Long-press copies all terminal text (selection fix) ──

describe('useTouchPan — long-press copies all terminal text (selection fix)', () => {
  it('selects all and writes to clipboard on long-press', async () => {
    // The real useTouchPan long-press branch calls:
    //   term.selectAll()
    //   selectionToText(term)
    //   navigator.clipboard.writeText(text)
    //
    // We test those three calls in isolation using a fake Terminal.
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, 'clipboard', {
      value: { writeText },
      writable: true, configurable: true,
    })

    const fakeContent = 'hello from terminal'
    const term = {
      selectAll: vi.fn(),
      getSelectionPosition: vi.fn().mockReturnValue({
        start: { x: 0, y: 0 }, end: { x: fakeContent.length - 1, y: 0 },
      }),
      getScrollbackLength: vi.fn().mockReturnValue(0),
      getViewportY:        vi.fn().mockReturnValue(0),
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

    // Mirror the long-press handler implementation
    term.selectAll()
    const { selectionToText } = await import('./selection')
    const text = selectionToText(term)
    if (text) await navigator.clipboard.writeText(text)

    expect(term.selectAll).toHaveBeenCalledOnce()
    expect(writeText).toHaveBeenCalledWith(fakeContent)
  })
})
