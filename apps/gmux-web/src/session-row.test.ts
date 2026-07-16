import { describe, it, expect } from 'vitest'
import { middleTruncate } from './session-row'

describe('middleTruncate', () => {
  it('leaves names within 16 chars untouched', () => {
    expect(middleTruncate('gmux')).toBe('gmux')
    expect(middleTruncate('sixteen-chars-16')).toBe('sixteen-chars-16') // exactly 16
  })

  it('middle-truncates longer names to first 10 + … + last 5', () => {
    const out = middleTruncate('registration-snapshot-service') // 29 chars
    expect(out).toBe('registrati…rvice')
    expect(out.length).toBe(16)
  })

  it('keeps head and tail so similar names stay distinguishable', () => {
    expect(middleTruncate('review-coordinator')).toBe('review-coo…nator') // 18
    expect(middleTruncate('review-controller')).toBe('review-con…oller') // 17
  })

  it('does not split surrogate pairs at the cut points', () => {
    // Non-BMP emoji at both the head boundary (10th cp) and tail
    // boundary. Counting UTF-16 units would slice mid-surrogate and
    // emit a lone \uFFFD; counting code points keeps them whole.
    const out = middleTruncate('aaaaaaaaa😀bbbbbbbbbb😀cccc')
    expect(out).toBe('aaaaaaaaa😀…😀cccc')
    expect(out).not.toContain('�')
    expect([...out]).toHaveLength(16)
  })
})
