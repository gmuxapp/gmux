import { describe, it, expect } from 'vitest'
import { formatGitStat, type GitStatusResult } from './git-status'

describe('formatGitStat', () => {
  it('formats files, insertions, and deletions', () => {
    const r: GitStatusResult = { files: 5, insertions: 120, deletions: 34 }
    const parts = formatGitStat(r)
    expect(parts.files).toBe('5~')
    expect(parts.insertions).toBe('+120')
    expect(parts.deletions).toBe('−34')
  })

  it('omits insertions when zero', () => {
    const r: GitStatusResult = { files: 2, insertions: 0, deletions: 5 }
    const parts = formatGitStat(r)
    expect(parts.insertions).toBeNull()
    expect(parts.deletions).toBe('−5')
  })

  it('omits deletions when zero', () => {
    const r: GitStatusResult = { files: 1, insertions: 10, deletions: 0 }
    const parts = formatGitStat(r)
    expect(parts.insertions).toBe('+10')
    expect(parts.deletions).toBeNull()
  })

  it('handles single file', () => {
    const r: GitStatusResult = { files: 1, insertions: 1, deletions: 0 }
    const parts = formatGitStat(r)
    expect(parts.files).toBe('1~')
  })
})
