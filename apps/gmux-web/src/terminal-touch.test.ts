/**
 * Tests for shouldFocusOnTouchEnd.
 */
import { describe, it, expect } from 'vitest'
import { shouldFocusOnTouchEnd } from './terminal-touch'

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
