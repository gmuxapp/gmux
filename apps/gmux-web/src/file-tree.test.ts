import { describe, it, expect } from 'vitest'
import {
  normalizeFsPath,
  joinFsPath,
  buildTreeNodes,
  isExternalFileDrop,
  type FileEntry,
} from './file-tree'

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
