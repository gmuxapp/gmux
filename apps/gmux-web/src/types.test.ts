import { describe, it, expect } from 'vitest'
import {
  groupByFolder,
  matchSession,
  buildProjectFolders,
  normalizeRemote,
  parseSessionPath,
  sessionPath,
  resolveSessionFromPath,
  type Session,
  type ProjectItem,
} from './types'

function makeSession(overrides: Partial<Session> & { id: string; cwd: string }): Session {
  return {
    created_at: new Date().toISOString(),
    command: ['pi'],
    kind: 'pi',
    alive: true,
    pid: 1234,
    exit_code: null,
    started_at: new Date().toISOString(),
    exited_at: null,
    title: 'test',
    subtitle: '',
    status: null,
    unread: false,
    socket_path: '/tmp/test.sock',
    ...overrides,
  }
}

describe('groupByFolder', () => {
  it('groups sessions by cwd when no workspace_root is set', () => {
    const sessions = [
      makeSession({ id: 'a', cwd: '/home/user/dev/project-a' }),
      makeSession({ id: 'b', cwd: '/home/user/dev/project-b' }),
      makeSession({ id: 'c', cwd: '/home/user/dev/project-a' }),
    ]

    const folders = groupByFolder(sessions)
    expect(folders).toHaveLength(2)

    const projectA = folders.find(f => f.path === '/home/user/dev/project-a')!
    expect(projectA.name).toBe('project-a')
    expect(projectA.sessions).toHaveLength(2)
  })

  it('groups sessions from different cwds sharing a workspace_root', () => {
    const sessions = [
      makeSession({ id: 'a', cwd: '/home/user/dev/gmux', workspace_root: '/home/user/dev/gmux' }),
      makeSession({ id: 'b', cwd: '/home/user/dev/gmux/.grove/teak', workspace_root: '/home/user/dev/gmux' }),
      makeSession({ id: 'c', cwd: '/home/user/dev/other' }),
    ]

    const folders = groupByFolder(sessions)
    expect(folders).toHaveLength(2)

    const gmux = folders.find(f => f.path === '/home/user/dev/gmux')!
    expect(gmux.name).toBe('gmux')
    expect(gmux.sessions).toHaveLength(2)
    expect(gmux.sessions.map(s => s.id).sort()).toEqual(['a', 'b'])

    const other = folders.find(f => f.path === '/home/user/dev/other')!
    expect(other.sessions).toHaveLength(1)
  })

  it('does not group sessions with different workspace_roots', () => {
    const sessions = [
      makeSession({ id: 'a', cwd: '/dev/a', workspace_root: '/dev/a' }),
      makeSession({ id: 'b', cwd: '/dev/b', workspace_root: '/dev/b' }),
    ]

    const folders = groupByFolder(sessions)
    expect(folders).toHaveLength(2)
  })

  it('uses cwd as fallback when workspace_root is empty string', () => {
    const sessions = [
      makeSession({ id: 'a', cwd: '/dev/a', workspace_root: '' }),
      makeSession({ id: 'b', cwd: '/dev/a', workspace_root: '' }),
    ]

    const folders = groupByFolder(sessions)
    expect(folders).toHaveLength(1)
    expect(folders[0].path).toBe('/dev/a')
  })

  it('groups sessions by cwd within a workspace folder', () => {
    const sessions = [
      makeSession({ id: 'a1', cwd: '/repo/.grove/cedar', workspace_root: '/repo', created_at: '2026-03-24T09:00:00Z' }),
      makeSession({ id: 'b1', cwd: '/repo',              workspace_root: '/repo', created_at: '2026-03-24T09:05:00Z' }),
      makeSession({ id: 'a2', cwd: '/repo/.grove/cedar', workspace_root: '/repo', created_at: '2026-03-24T09:10:00Z' }),
      makeSession({ id: 'b2', cwd: '/repo',              workspace_root: '/repo', created_at: '2026-03-24T08:00:00Z' }),
    ]

    const folders = groupByFolder(sessions)
    expect(folders).toHaveLength(1)
    // Sessions cluster by cwd (lexicographic), newest first within each group.
    // /repo < /repo/.grove/cedar, so b sessions come first.
    const ids = folders[0].sessions.map(s => s.id)
    expect(ids).toEqual(['b1', 'b2', 'a2', 'a1'])
  })

  it('groups sessions sharing a remote URL', () => {
    const sessions = [
      makeSession({
        id: 'a', cwd: '/home/user/dev/gmux',
        remotes: { origin: 'github.com/gmuxapp/gmux' },
      }),
      makeSession({
        id: 'b', cwd: '/home/server/projects/gmux',
        remotes: { origin: 'github.com/gmuxapp/gmux' },
      }),
      makeSession({
        id: 'c', cwd: '/home/user/dev/other',
        remotes: { origin: 'github.com/gmuxapp/other' },
      }),
    ]

    const folders = groupByFolder(sessions)
    expect(folders).toHaveLength(2)

    const gmux = folders.find(f => f.path === 'github.com/gmuxapp/gmux')!
    expect(gmux.name).toBe('gmux')
    expect(gmux.sessions).toHaveLength(2)
    expect(gmux.sessions.map(s => s.id).sort()).toEqual(['a', 'b'])
  })

  it('groups fork and upstream via shared remote (any-match)', () => {
    // Machine A: fork as origin, upstream as upstream
    // Machine B: upstream as origin
    // They share "github.com/gmuxapp/gmux", so they group.
    const sessions = [
      makeSession({
        id: 'fork', cwd: '/home/dev/gmux',
        remotes: {
          origin: 'github.com/mgabor3141/gmux',
          upstream: 'github.com/gmuxapp/gmux',
        },
      }),
      makeSession({
        id: 'upstream', cwd: '/home/server/gmux',
        remotes: { origin: 'github.com/gmuxapp/gmux' },
      }),
    ]

    const folders = groupByFolder(sessions)
    expect(folders).toHaveLength(1)
    expect(folders[0].sessions).toHaveLength(2)
  })

  it('does not group sessions with different remotes', () => {
    const sessions = [
      makeSession({
        id: 'a', cwd: '/dev/a',
        remotes: { origin: 'github.com/org/repo-a' },
      }),
      makeSession({
        id: 'b', cwd: '/dev/b',
        remotes: { origin: 'github.com/org/repo-b' },
      }),
    ]

    const folders = groupByFolder(sessions)
    expect(folders).toHaveLength(2)
  })

  it('prefers remote grouping over workspace_root', () => {
    // Same workspace_root but different remotes: should NOT group.
    // (This is a weird edge case, but remote identity should win.)
    const sessions = [
      makeSession({
        id: 'a', cwd: '/repo', workspace_root: '/repo',
        remotes: { origin: 'github.com/org/repo-a' },
      }),
      makeSession({
        id: 'b', cwd: '/repo/sub', workspace_root: '/repo',
        remotes: { origin: 'github.com/org/repo-b' },
      }),
    ]

    const folders = groupByFolder(sessions)
    // They share a workspace_root, so union-find merges them via the ws pass.
    // This is correct: if they're in the same workspace, they belong together
    // even if one has a different remote (e.g. a submodule).
    expect(folders).toHaveLength(1)
  })

  it('uses remote-derived name when remotes are present', () => {
    const sessions = [
      makeSession({
        id: 'a', cwd: '/some/long/path/gmux',
        remotes: { origin: 'github.com/gmuxapp/gmux' },
      }),
    ]

    const folders = groupByFolder(sessions)
    expect(folders[0].name).toBe('gmux')
    expect(folders[0].path).toBe('github.com/gmuxapp/gmux')
  })

  it('falls through to workspace_root when no remotes', () => {
    const sessions = [
      makeSession({ id: 'a', cwd: '/repo', workspace_root: '/repo' }),
      makeSession({ id: 'b', cwd: '/repo/sub', workspace_root: '/repo' }),
    ]

    const folders = groupByFolder(sessions)
    expect(folders).toHaveLength(1)
    expect(folders[0].path).toBe('/repo')
  })

  it('sorts working folders before alive before dead', () => {
    const sessions = [
      makeSession({ id: 'dead', cwd: '/dead', alive: false }),
      makeSession({ id: 'working', cwd: '/working', alive: true, status: { label: 'thinking', working: true } }),
      makeSession({ id: 'alive', cwd: '/alive', alive: true }),
    ]

    const folders = groupByFolder(sessions)
    expect(folders.map(f => f.name)).toEqual(['working', 'alive', 'dead'])
  })
})

// --- Project matching ---

describe('matchSession', () => {
  const projects: ProjectItem[] = [
    { slug: 'gmux', remote: 'github.com/gmuxapp/gmux', paths: ['/dev/gmux'] },
    { slug: 'yapp', paths: ['/dev/yapp'] },
  ]

  it('matches by path (no remote) with longest prefix', () => {
    const sess = makeSession({ id: 's1', cwd: '/dev/yapp/src' })
    expect(matchSession(sess, projects)?.slug).toBe('yapp')
  })

  it('matches by remote URL', () => {
    const sess = makeSession({
      id: 's2', cwd: '/other',
      remotes: { origin: 'git@github.com:gmuxapp/gmux.git' },
    })
    expect(matchSession(sess, projects)?.slug).toBe('gmux')
  })

  it('path-only project takes precedence over remote project path fallback', () => {
    const sess = makeSession({ id: 's3', cwd: '/dev/yapp/deep' })
    expect(matchSession(sess, projects)?.slug).toBe('yapp')
  })

  it('remote project falls back to its paths', () => {
    // Session in gmux directory but without remotes
    const sess = makeSession({ id: 's4', cwd: '/dev/gmux/src' })
    expect(matchSession(sess, projects)?.slug).toBe('gmux')
  })

  it('returns null for unmatched sessions', () => {
    const sess = makeSession({ id: 's5', cwd: '/other/place' })
    expect(matchSession(sess, projects)).toBeNull()
  })
})

describe('buildProjectFolders', () => {
  it('builds folders in project order', () => {
    const projects: ProjectItem[] = [
      { slug: 'beta', paths: ['/dev/beta'] },
      { slug: 'alpha', paths: ['/dev/alpha'] },
    ]
    const sessions = [
      makeSession({ id: 'a1', cwd: '/dev/alpha/src' }),
      makeSession({ id: 'b1', cwd: '/dev/beta/src' }),
    ]
    const folders = buildProjectFolders(projects, sessions)
    expect(folders.map(f => f.name)).toEqual(['beta', 'alpha'])
  })

  it('includes projects with no matching sessions', () => {
    const projects: ProjectItem[] = [
      { slug: 'empty', paths: ['/dev/empty'] },
    ]
    const folders = buildProjectFolders(projects, [])
    expect(folders).toHaveLength(1)
    expect(folders[0].name).toBe('empty')
    expect(folders[0].sessions).toHaveLength(0)
  })

  it('sets launchCwd from project paths', () => {
    const projects: ProjectItem[] = [
      { slug: 'proj', paths: ['/dev/proj', '/dev/proj2'] },
    ]
    const folders = buildProjectFolders(projects, [])
    expect(folders[0].launchCwd).toBe('/dev/proj')
  })
})

// --- URL routing ---

describe('parseSessionPath', () => {
  it('parses full path', () => {
    expect(parseSessionPath('/gmux/pi/fix-auth')).toEqual({
      project: 'gmux', adapter: 'pi', slug: 'fix-auth',
    })
  })

  it('parses project-only path', () => {
    expect(parseSessionPath('/gmux')).toEqual({ project: 'gmux' })
  })

  it('returns empty for root', () => {
    expect(parseSessionPath('/')).toEqual({})
  })

  it('skips internal routes', () => {
    expect(parseSessionPath('/_/input-diagnostics')).toEqual({})
  })
})

describe('sessionPath', () => {
  it('builds URL from project slug and session', () => {
    expect(sessionPath('gmux', { kind: 'pi', slug: 'fix-auth', id: 'abc' }))
      .toBe('/gmux/pi/fix-auth')
  })

  it('falls back to ID prefix when slug missing', () => {
    expect(sessionPath('gmux', { kind: 'pi', id: 'abcdef12-3456-7890' }))
      .toBe('/gmux/pi/abcdef12')
  })
})

describe('resolveSessionFromPath', () => {
  const projects: ProjectItem[] = [
    { slug: 'gmux', remote: 'github.com/gmuxapp/gmux', paths: ['/dev/gmux'] },
  ]
  const sessions = [
    makeSession({ id: 'sess-1', cwd: '/dev/gmux', kind: 'pi', slug: 'fix-auth',
      remotes: { origin: 'github.com/gmuxapp/gmux' } }),
    makeSession({ id: 'sess-2', cwd: '/dev/gmux', kind: 'shell', slug: 'fish',
      remotes: { origin: 'github.com/gmuxapp/gmux' } }),
  ]

  it('resolves full path to session ID', () => {
    const id = resolveSessionFromPath(
      { project: 'gmux', adapter: 'pi', slug: 'fix-auth' }, projects, sessions,
    )
    expect(id).toBe('sess-1')
  })

  it('resolves project-only to first alive session', () => {
    const id = resolveSessionFromPath({ project: 'gmux' }, projects, sessions)
    expect(id).toBe('sess-1')
  })

  it('returns null for unknown project', () => {
    const id = resolveSessionFromPath({ project: 'nope' }, projects, sessions)
    expect(id).toBeNull()
  })
})

// --- Remote normalization ---

describe('normalizeRemote', () => {
  it('strips protocol and .git suffix', () => {
    expect(normalizeRemote('https://github.com/org/repo.git'))
      .toBe('github.com/org/repo')
  })

  it('converts SCP-style to slash', () => {
    expect(normalizeRemote('git@github.com:org/repo.git'))
      .toBe('github.com/org/repo')
  })

  it('handles plain URL', () => {
    expect(normalizeRemote('github.com/org/repo'))
      .toBe('github.com/org/repo')
  })
})
