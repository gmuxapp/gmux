/**
 * Tests for mobile text-selection improvements:
 * 1. shouldUseWebgl — returns false on coarse-pointer devices (DOM renderer)
 * 2. shouldFocusOnTouchEnd — skips focus when a long-press was detected
 */
import { describe, it, expect } from 'vitest'
import { shouldUseWebgl, shouldFocusOnTouchEnd } from './terminal-touch'

// ── shouldUseWebgl ──────────────────────────────────────────────────────────

describe('shouldUseWebgl', () => {
  function makeMatchMedia(coarse: boolean) {
    return (query: string) => ({
      matches: coarse ? query === '(pointer: coarse)' : false,
      media: query,
    })
  }

  it('returns false on a coarse-pointer (touch) device', () => {
    expect(shouldUseWebgl(makeMatchMedia(true))).toBe(false)
  })

  it('returns true on a fine-pointer (mouse) device', () => {
    expect(shouldUseWebgl(makeMatchMedia(false))).toBe(true)
  })
})

// ── shouldFocusOnTouchEnd ───────────────────────────────────────────────────

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
