import { describe, it, expect } from 'vitest'
import { relativeCwd, cwdBadge } from './cwd-format'

describe('relativeCwd', () => {
  it('returns empty when cwd equals the canonical folder', () => {
    expect(relativeCwd('~/dev/gmux', '~/dev/gmux')).toBe('')
  })

  it('returns a project-relative path for a descendant', () => {
    expect(relativeCwd('~/dev/gmux/apps/web', '~/dev/gmux')).toBe('./apps/web')
  })

  it('returns a project-relative path for a grove/worktree descendant', () => {
    expect(relativeCwd('~/dev/gmux/.grove/feature', '~/dev/gmux'))
      .toBe('./.grove/feature')
  })

  it('returns the absolute cwd when unrelated to the canonical folder', () => {
    expect(relativeCwd('~/tmp/scratch', '~/dev/gmux')).toBe('~/tmp/scratch')
  })

  it('does not treat a sibling prefix as a descendant', () => {
    // ~/dev/gmux-other must not be seen as under ~/dev/gmux
    expect(relativeCwd('~/dev/gmux-other', '~/dev/gmux')).toBe('~/dev/gmux-other')
  })

  it('returns the cwd verbatim when there is no canonical folder', () => {
    expect(relativeCwd('~/dev/gmux', undefined)).toBe('~/dev/gmux')
    expect(relativeCwd('~/dev/gmux', '')).toBe('~/dev/gmux')
  })
})

describe('cwdBadge', () => {
  it('is null for an empty/unresolved cwd', () => {
    expect(cwdBadge('', '~/dev/gmux')).toBeNull()
    expect(cwdBadge(undefined, '~/dev/gmux')).toBeNull()
  })

  it('is null when cwd matches the canonical folder', () => {
    expect(cwdBadge('~/dev/gmux', '~/dev/gmux')).toBeNull()
  })

  it('surfaces a descendant as a relative path', () => {
    expect(cwdBadge('~/dev/gmux/apps/web', '~/dev/gmux')).toBe('./apps/web')
  })

  it('surfaces an unrelated cwd as an absolute path', () => {
    expect(cwdBadge('~/tmp/scratch', '~/dev/gmux')).toBe('~/tmp/scratch')
  })
})
