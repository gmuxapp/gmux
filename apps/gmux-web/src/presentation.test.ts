import { describe, expect, it } from 'vitest'
import { sessionPresentationState } from './presentation'

const bools = [false, true] as const

describe('sessionPresentationState', () => {
  it('exhaustively derives the finite presentation contract', () => {
    const expected = new Map([
      ['000', 'none'], ['001', 'waiting'],
      ['010', 'none'], ['011', 'waiting-error'],
      ['100', 'active'], ['101', 'active'],
      ['110', 'active-error'], ['111', 'active-error'],
    ])
    for (const active of bools) for (const error of bools) for (const unread of bools) {
      const key = `${+active}${+error}${+unread}`
      expect(sessionPresentationState({ status: { active, error }, unread }), key)
        .toBe(expected.get(key))
    }
  })
})
