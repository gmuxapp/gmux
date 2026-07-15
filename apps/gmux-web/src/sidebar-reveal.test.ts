import { describe, expect, test } from 'vitest'
import { needsReveal } from './sidebar-reveal'

describe('needsReveal', () => {
  const container = { top: 100, bottom: 500 }

  test('fully visible row stays put', () => {
    expect(needsReveal(container, { top: 200, bottom: 300 })).toBe(false)
  })

  test('row flush with the edges counts as visible', () => {
    expect(needsReveal(container, { top: 100, bottom: 500 })).toBe(false)
  })

  test('row above the viewport needs revealing', () => {
    expect(needsReveal(container, { top: 40, bottom: 90 })).toBe(true)
  })

  test('row below the viewport needs revealing', () => {
    expect(needsReveal(container, { top: 520, bottom: 600 })).toBe(true)
  })

  test('row taller than the viewport (clipped both ends) needs revealing', () => {
    expect(needsReveal(container, { top: 50, bottom: 700 })).toBe(true)
  })
})
