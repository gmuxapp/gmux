import { describe, it, expect } from 'vitest'
import { relativeCwd, cwdBadge, hubCwdLabel } from './cwd-format'

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

describe('hubCwdLabel', () => {
  it('marks a session at the project root with a folder-shaped ./', () => {
    // './' (not a bare '.') so the token always reads as a directory.
    expect(hubCwdLabel('~/dev/gmux', '~/dev/gmux')).toBe('./')
  })

  it('is blank for an unresolved (empty) cwd, not a stray ./', () => {
    // Regression guard: an empty cwd must not collapse to './', which
    // would label a placeholder as if it were the project root.
    expect(hubCwdLabel('', '~/dev/gmux')).toBe('')
    expect(hubCwdLabel(undefined, '~/dev/gmux')).toBe('')
  })

  it('shows descendants and unrelated paths like relativeCwd', () => {
    expect(hubCwdLabel('~/dev/gmux/apps/web', '~/dev/gmux')).toBe('./apps/web')
    expect(hubCwdLabel('~/tmp/scratch', '~/dev/gmux')).toBe('~/tmp/scratch')
  })
})
