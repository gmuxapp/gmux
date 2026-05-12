import { describe, it, expect } from 'vitest'
import { sessionEnvironmentIcon } from './sidebar'

describe('sessionEnvironmentIcon', () => {
  it('returns sandbox icon for pi-sbx kind', () => {
    expect(sessionEnvironmentIcon('pi-sbx')).toBe('🏖️')
  })

  it('returns host icon for pi kind', () => {
    expect(sessionEnvironmentIcon('pi')).toBe('🏡')
  })

  it('returns host icon for shell kind', () => {
    expect(sessionEnvironmentIcon('shell')).toBe('🏡')
  })

  it('returns host icon for claude kind', () => {
    expect(sessionEnvironmentIcon('claude')).toBe('🏡')
  })

  it('returns host icon for codex kind', () => {
    expect(sessionEnvironmentIcon('codex')).toBe('🏡')
  })

  it('returns host icon for unknown kinds', () => {
    expect(sessionEnvironmentIcon('anything-else')).toBe('🏡')
  })
})
