import { describe, it, expect } from 'vitest'
import { groupByFolder, type Session } from './types'

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
