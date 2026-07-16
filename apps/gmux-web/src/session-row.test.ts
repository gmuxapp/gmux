import { describe, it, expect } from 'vitest'
import { middleTruncate } from './session-row'

describe('middleTruncate', () => {
  it('leaves names within the limit untouched', () => {
    expect(middleTruncate('gmux')).toBe('gmux')
    expect(middleTruncate('review-coordinator')).toBe('review-coordinator') // 18 ≤ 20
    expect(middleTruncate('x'.repeat(20))).toBe('x'.repeat(20))
  })

  it('middle-truncates longer names, keeping head and tail', () => {
    const out = middleTruncate('registration-snapshot-service', 20) // 29 chars
    expect(out).toContain('…')
    expect(out).toMatch(/^regi/) // head preserved
    expect(out).toMatch(/vice$/) // tail preserved
    // One … replaces the middle; the result never exceeds the budget.
    expect(out.length).toBeLessThanOrEqual(20)
  })

  it('always keeps at least three chars each side (per the spec)', () => {
    // Even at an absurdly small budget the head/tail floors hold.
    const out = middleTruncate('abcdefghijklmnop', 4)
    const [head, tail] = out.split('…')
    expect(head.length).toBeGreaterThanOrEqual(3)
    expect(tail.length).toBeGreaterThanOrEqual(3)
    expect(out.startsWith('abc')).toBe(true)
    expect(out.endsWith('nop')).toBe(true)
  })
})
