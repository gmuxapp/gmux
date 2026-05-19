import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import {
  normalizeFsPath,
  joinFsPath,
  buildTreeNodes,
  isExternalFileDrop,
  isHiddenName,
  copyRelativePath,
  pruneExpanded,
  findOpenFileSession,
  type FileEntry,
} from './file-tree'
import type { Session } from './types'

describe('normalizeFsPath', () => {
  it('strips leading slashes', () => {
    expect(normalizeFsPath('/src/main.go')).toBe('src/main.go')
    expect(normalizeFsPath('///foo/bar')).toBe('foo/bar')
  })

  it('leaves relative paths unchanged', () => {
    expect(normalizeFsPath('src/main.go')).toBe('src/main.go')
    expect(normalizeFsPath('README.md')).toBe('README.md')
  })

  it('collapses repeated slashes', () => {
    expect(normalizeFsPath('src//main.go')).toBe('src/main.go')
  })

  it('strips trailing slash', () => {
    expect(normalizeFsPath('src/')).toBe('src')
  })

  it('handles empty string', () => {
    expect(normalizeFsPath('')).toBe('')
  })
})

describe('joinFsPath', () => {
  it('joins parent and name with slash', () => {
    expect(joinFsPath('src', 'main.go')).toBe('src/main.go')
  })

  it('returns name when parent is empty (root)', () => {
    expect(joinFsPath('', 'README.md')).toBe('README.md')
  })

  it('handles nested parent', () => {
    expect(joinFsPath('a/b/c', 'file.ts')).toBe('a/b/c/file.ts')
  })
})

describe('buildTreeNodes', () => {
  const entries: FileEntry[] = [
    { name: 'zoo.txt', type: 'file' },
    { name: 'alpha', type: 'dir' },
    { name: 'src', type: 'dir' },
    { name: 'README.md', type: 'file' },
  ]

  it('sorts dirs before files', () => {
    const nodes = buildTreeNodes(entries, '')
    expect(nodes[0].type).toBe('dir')
    expect(nodes[1].type).toBe('dir')
    expect(nodes[2].type).toBe('file')
    expect(nodes[3].type).toBe('file')
  })

  it('sorts alphabetically within dirs and files', () => {
    const nodes = buildTreeNodes(entries, '')
    expect(nodes[0].name).toBe('alpha')
    expect(nodes[1].name).toBe('src')
    expect(nodes[2].name).toBe('README.md')
    expect(nodes[3].name).toBe('zoo.txt')
  })

  it('prefixes path with parentPath', () => {
    const nodes = buildTreeNodes(entries, 'packages/web')
    expect(nodes[0].path).toBe('packages/web/alpha')
    expect(nodes[2].path).toBe('packages/web/README.md')
  })

  it('uses bare name when parentPath is empty (root)', () => {
    const nodes = buildTreeNodes(entries, '')
    expect(nodes[0].path).toBe('alpha')
    expect(nodes[2].path).toBe('README.md')
  })

  it('handles empty entry list', () => {
    expect(buildTreeNodes([], '')).toEqual([])
  })
})

describe('isExternalFileDrop', () => {
  function makeDataTransfer(fileCount: number): DataTransfer {
    const files: Partial<FileList> = { length: fileCount }
    return { files } as unknown as DataTransfer
  }

  it('returns true when files are present', () => {
    expect(isExternalFileDrop(makeDataTransfer(1))).toBe(true)
    expect(isExternalFileDrop(makeDataTransfer(3))).toBe(true)
  })

  it('returns false when no files', () => {
    expect(isExternalFileDrop(makeDataTransfer(0))).toBe(false)
  })
})

describe('isHiddenName', () => {
  it('returns true for names starting with a dot', () => {
    expect(isHiddenName('.gitignore')).toBe(true)
    expect(isHiddenName('.env')).toBe(true)
    expect(isHiddenName('.git')).toBe(true)
  })

  it('returns false for normal names', () => {
    expect(isHiddenName('src')).toBe(false)
    expect(isHiddenName('README.md')).toBe(false)
    expect(isHiddenName('main.go')).toBe(false)
  })

  it('returns false for empty string', () => {
    expect(isHiddenName('')).toBe(false)
  })
})

describe('buildTreeNodes hidden-file filtering', () => {
  const entries: FileEntry[] = [
    { name: '.gitignore', type: 'file' },
    { name: '.git', type: 'dir' },
    { name: 'src', type: 'dir' },
    { name: 'README.md', type: 'file' },
  ]

  it('buildTreeNodes still returns all entries (filtering is display-side)', () => {
    const nodes = buildTreeNodes(entries, '')
    expect(nodes).toHaveLength(4)
  })

  it('manual filter with isHiddenName excludes dotfiles', () => {
    const nodes = buildTreeNodes(entries, '').filter(n => !isHiddenName(n.name))
    expect(nodes).toHaveLength(2)
    expect(nodes.map(n => n.name)).toEqual(['src', 'README.md'])
  })

  it('manual filter with isHiddenName keeps dotfiles when showHidden=true', () => {
    const nodes = buildTreeNodes(entries, '').filter(() => true)
    expect(nodes).toHaveLength(4)
  })
})

describe('pruneExpanded', () => {
  it('removes the exact deleted path', () => {
    const expanded = new Set(['src', 'src/components', 'docs'])
    const result = pruneExpanded(expanded, 'src')
    expect(result.has('src')).toBe(false)
  })

  it('removes all children of the deleted path', () => {
    const expanded = new Set(['src', 'src/components', 'src/components/ui', 'docs'])
    const result = pruneExpanded(expanded, 'src')
    expect(result.has('src')).toBe(false)
    expect(result.has('src/components')).toBe(false)
    expect(result.has('src/components/ui')).toBe(false)
    expect(result.has('docs')).toBe(true)
  })

  it('does not remove sibling paths that share a prefix', () => {
    const expanded = new Set(['src', 'src-backup', 'docs'])
    const result = pruneExpanded(expanded, 'src')
    expect(result.has('src')).toBe(false)
    expect(result.has('src-backup')).toBe(true)
    expect(result.has('docs')).toBe(true)
  })

  it('is a no-op when the path is not in the set', () => {
    const expanded = new Set(['docs', 'lib'])
    const result = pruneExpanded(expanded, 'src')
    expect([...result]).toEqual(['docs', 'lib'])
  })

  it('does not mutate the original set', () => {
    const expanded = new Set(['src', 'src/components'])
    pruneExpanded(expanded, 'src')
    expect(expanded.has('src')).toBe(true)
    expect(expanded.has('src/components')).toBe(true)
  })
})

describe('copyRelativePath', () => {
  let writeText: ReturnType<typeof vi.fn>

  beforeEach(() => {
    writeText = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(globalThis, 'navigator', {
      value: { clipboard: { writeText } },
      configurable: true,
    })
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('writes the path as-is to the clipboard', async () => {
    await copyRelativePath('src/components/button.tsx')
    expect(writeText).toHaveBeenCalledWith('src/components/button.tsx')
  })

  it('handles a root-level file (no directory prefix)', async () => {
    await copyRelativePath('README.md')
    expect(writeText).toHaveBeenCalledWith('README.md')
  })
})

// ── findOpenFileSession ──

function makeSession(overrides: Partial<Session> = {}): Session {
  return {
    id: 'sess-1',
    created_at: '',
    command: ['hx', 'file.tsx'],
    cwd: '/home/james/projects/jmux',
    kind: 'terminal',
    alive: true,
    pid: 123,
    exit_code: null,
    started_at: '',
    exited_at: null,
    title: 'file.tsx',
    subtitle: '',
    status: null,
    unread: false,
    ...overrides,
  }
}

describe('findOpenFileSession', () => {
  const cwd = '/home/james/projects/jmux'

  it('returns undefined when sessions list is empty', () => {
    expect(findOpenFileSession([], cwd, 'apps/src/file.tsx')).toBeUndefined()
  })

  it('returns undefined when no session is alive', () => {
    const s = makeSession({ alive: false, command: ['hx', 'file.tsx'], cwd })
    expect(findOpenFileSession([s], cwd, 'apps/src/file.tsx')).toBeUndefined()
  })

  it('returns undefined when cwd does not match', () => {
    const s = makeSession({ cwd: '/other/path', command: ['hx', 'file.tsx'] })
    expect(findOpenFileSession([s], cwd, 'apps/src/file.tsx')).toBeUndefined()
  })

  it('returns undefined when filename does not match', () => {
    const s = makeSession({ cwd, command: ['hx', 'other.tsx'] })
    expect(findOpenFileSession([s], cwd, 'apps/src/file.tsx')).toBeUndefined()
  })

  it('returns session when alive, cwd matches, leaf filename matches nested relPath', () => {
    const s = makeSession({ cwd, command: ['hx', 'file.tsx'] })
    expect(findOpenFileSession([s], cwd, 'apps/src/file.tsx')).toBe(s)
  })

  it('returns session for root-level file (no slash in relPath)', () => {
    const s = makeSession({ cwd, command: ['hx', 'README.md'] })
    expect(findOpenFileSession([s], cwd, 'README.md')).toBe(s)
  })

  it('returns the first matching session when multiple exist', () => {
    const s1 = makeSession({ id: 'sess-1', cwd, command: ['hx', 'file.tsx'] })
    const s2 = makeSession({ id: 'sess-2', cwd, command: ['hx', 'file.tsx'] })
    expect(findOpenFileSession([s1, s2], cwd, 'apps/src/file.tsx')).toBe(s1)
  })
})
