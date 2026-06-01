/**
 * Performance regression tests for the file-tree component.
 *
 * Covers the synchronous work that runs on every session load and every
 * 2.5 s poll cycle. All assertions are < 50 ms — the budget for returning
 * focus to the user.
 *
 * Root cause of the regression (db67a38 + 99ec273):
 *   getExpandedPaths() calls model.getItem() for *every* directory path
 *   returned by the walk before every resetPaths() call.  On a large repo
 *   this is O(n) model lookups running on every session switch and every poll.
 */

import { describe, it, expect, beforeEach, vi } from 'vitest'
import { getExpandedPaths, findOpenFileSession } from './file-tree'
import {
  sessions,
  projects,
  sessionsLoaded,
  projectsLoaded,
  setNavigate,
  navigateToMarkdownEditor,
  navigateToImageViewer,
  navigateToSession,
} from './store'
import type { FileTreeItemHandle, FileTreeDirectoryHandle } from '@pierre/trees'
import type { Session } from './types'

// ── Shared budget ────────────────────────────────────────────────────────────

/** Maximum milliseconds allowed for any synchronous filetree operation. */
const BUDGET_MS = 50

// ── Test helpers ─────────────────────────────────────────────────────────────

function makeMockModel(expandedSet: Set<string>): {
  getItem(path: string): FileTreeItemHandle | null
} {
  const baseHandle = {
    deselect: () => {},
    focus: () => {},
    isFocused: () => false,
    isSelected: () => false,
    select: () => {},
    toggleSelect: () => {},
  }

  return {
    getItem(path: string): FileTreeItemHandle | null {
      if (!path.endsWith('/')) {
        return {
          ...baseHandle,
          isDirectory: () => false,
          getPath: () => path,
        } as unknown as FileTreeItemHandle
      }
      return {
        ...baseHandle,
        isDirectory: () => true,
        isExpanded: () => expandedSet.has(path),
        expand: () => {},
        collapse: () => {},
        toggle: () => {},
        getPath: () => path,
      } as unknown as FileTreeDirectoryHandle
    },
  }
}

function makeSession(id: string, cwd: string, command: string[]): Session {
  return {
    id,
    created_at: '2026-01-01T00:00:00Z',
    command,
    cwd,
    kind: 'terminal',
    alive: true,
    pid: 1,
    exit_code: null,
    started_at: '2026-01-01T00:00:00Z',
    exited_at: null,
    title: id,
    subtitle: '',
    status: null,
    unread: false,
    socket_path: `/tmp/${id}.sock`,
  } as Session
}

function makeSessions(count: number, cwd: string): Session[] {
  return Array.from({ length: count }, (_, i) =>
    makeSession(`sess-${i}`, cwd, ['hx', `file-${i}.ts`]),
  )
}

// ── Store reset ───────────────────────────────────────────────────────────────

const CWD = '/home/user/my-project'

beforeEach(() => {
  sessions.value = []
  projects.value = []
  sessionsLoaded.value = false
  projectsLoaded.value = false
  setNavigate(vi.fn())
})

// ── 1. getExpandedPaths — O(n) model.getItem calls ──────────────────────────

describe('getExpandedPaths performance', () => {
  it('processes 1 000 directory paths in <50 ms (matches typical project)', () => {
    const expandedSet = new Set(['src/', 'docs/', 'packages/'])
    const model = makeMockModel(expandedSet)
    const dirs = [
      ...Array.from({ length: 997 }, (_, i) => `module-${i}/`),
      'src/',
      'docs/',
      'packages/',
    ]

    const t0 = performance.now()
    const result = getExpandedPaths(model, dirs)
    const elapsed = performance.now() - t0

    expect(elapsed).toBeLessThan(BUDGET_MS)
    expect(result).toContain('src/')
    expect(result).toContain('docs/')
    expect(result).toContain('packages/')
    expect(result).toHaveLength(3)
  })

  it('processes 5 000 directory paths in <50 ms (large mono-repo)', () => {
    const expandedSet = new Set(
      Array.from({ length: 20 }, (_, i) => `packages/pkg-${i}/src/`),
    )
    const model = makeMockModel(expandedSet)
    const dirs = [
      ...Array.from({ length: 4980 }, (_, i) => `dir-${i}/`),
      ...Array.from(expandedSet),
    ]

    const t0 = performance.now()
    const result = getExpandedPaths(model, dirs)
    const elapsed = performance.now() - t0

    expect(elapsed).toBeLessThan(BUDGET_MS)
    expect(result).toHaveLength(20)
  })
})

// ── 2. Full sync loadPaths work — path filter + getExpandedPaths ─────────────

describe('loadPaths synchronous work (session load / poll cycle)', () => {
  it('filters dirs and runs getExpandedPaths over 2 000 paths in <50 ms', () => {
    // Mirrors what loadPaths() does synchronously before the network response
    // is awaited on every poll cycle and every session switch.
    const expandedSet = new Set(
      Array.from({ length: 15 }, (_, i) => `src/layer-${i}/`),
    )
    const model = makeMockModel(expandedSet)

    // Mix of files and dirs (realistic walk output)
    const allPaths: string[] = []
    for (let i = 0; i < 1000; i++) allPaths.push(`dir-${i}/`)
    for (let i = 0; i < 1000; i++) allPaths.push(`src/file-${i}.ts`)
    expandedSet.forEach(d => allPaths.push(d))

    const t0 = performance.now()
    // Replicate the synchronous work from loadPaths()
    const dirPaths = allPaths.filter(p => p.endsWith('/'))
    const expandedPaths = getExpandedPaths(model, dirPaths)
    const elapsed = performance.now() - t0

    expect(elapsed).toBeLessThan(BUDGET_MS)
    expect(expandedPaths).toHaveLength(15)
  })
})

// ── 3. Selection handler — clicking a file in the tree ───────────────────────

describe('filetree click — navigating to a markdown doc', () => {
  it('navigateToMarkdownEditor completes in <50 ms', () => {
    const t0 = performance.now()
    navigateToMarkdownEditor('my-project', 'docs/README.md')
    const elapsed = performance.now() - t0

    expect(elapsed).toBeLessThan(BUDGET_MS)
  })
})

describe('filetree click — navigating to an image', () => {
  it('navigateToImageViewer completes in <50 ms', () => {
    const t0 = performance.now()
    navigateToImageViewer('my-project', 'assets/logo.png')
    const elapsed = performance.now() - t0

    expect(elapsed).toBeLessThan(BUDGET_MS)
  })
})

// ── 4. Selection handler — finding + switching to an existing session ─────────

describe('filetree click — switching to an open session', () => {
  it('findOpenFileSession with 200 sessions completes in <50 ms', () => {
    const sessionList = makeSessions(200, CWD)
    // Place the matching session at the end (worst case for linear scan)
    sessionList[199] = makeSession('sess-199', CWD, ['hx', 'main.go'])

    const t0 = performance.now()
    const found = findOpenFileSession(sessionList, CWD, 'src/main.go')
    const elapsed = performance.now() - t0

    expect(elapsed).toBeLessThan(BUDGET_MS)
    expect(found).toBe(sessionList[199])
  })

  it('navigateToSession with 200 sessions in store completes in <50 ms', () => {
    const sessionList = makeSessions(200, CWD)
    sessions.value = sessionList
    projects.value = [{ slug: 'my-project', match: [{ path: CWD }] }]
    sessionsLoaded.value = true
    projectsLoaded.value = true

    const target = sessionList[150]

    const t0 = performance.now()
    navigateToSession(target.id)
    const elapsed = performance.now() - t0

    expect(elapsed).toBeLessThan(BUDGET_MS)
  })
})

// ── 5. Opening a different session via the sidebar ────────────────────────────

describe('loading a different session — full selection-handler path', () => {
  it('finding + navigating to a different session stays within budget', () => {
    // Simulate 200 sessions across multiple projects (realistic multi-project setup)
    const sessionList = [
      ...makeSessions(100, '/home/user/project-a'),
      ...makeSessions(100, CWD),
    ]
    sessions.value = sessionList
    projects.value = [
      { slug: 'project-a', match: [{ path: '/home/user/project-a' }] },
      { slug: 'my-project', match: [{ path: CWD }] },
    ]
    sessionsLoaded.value = true
    projectsLoaded.value = true

    const target = sessionList[180]

    const t0 = performance.now()
    // This is what the sidebar session-click fires
    navigateToSession(target.id)
    const elapsed = performance.now() - t0

    expect(elapsed).toBeLessThan(BUDGET_MS)
  })
})
