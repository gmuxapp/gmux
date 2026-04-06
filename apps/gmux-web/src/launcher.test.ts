import { describe, expect, test } from 'vitest'
import { launchersForPeer, resolveTarget, formatTarget, type LaunchConfig } from './launcher'
import type { Session } from './types'

const localConfig: LaunchConfig = {
  default_launcher: 'shell',
  launchers: [
    { id: 'shell', label: 'Shell', command: ['bash'], available: true },
    { id: 'claude', label: 'Claude', command: ['claude'], available: true },
  ],
  peers: {
    'work-laptop': {
      default_launcher: 'pi',
      launchers: [
        { id: 'shell', label: 'Shell', command: ['zsh'], available: true },
        { id: 'pi', label: 'pi', command: ['pi'], available: true },
      ],
    },
  },
}

describe('launchersForPeer', () => {
  test('returns local config when peer is undefined', () => {
    const resolved = launchersForPeer(localConfig, undefined)
    expect(resolved.default_launcher).toBe('shell')
    expect(resolved.launchers.map(l => l.id)).toEqual(['shell', 'claude'])
  })

  test('returns peer config when peer matches', () => {
    const resolved = launchersForPeer(localConfig, 'work-laptop')
    expect(resolved.default_launcher).toBe('pi')
    expect(resolved.launchers.map(l => l.id)).toEqual(['shell', 'pi'])
  })

  test('falls back to local when peer is unknown', () => {
    const resolved = launchersForPeer(localConfig, 'mystery-host')
    expect(resolved.default_launcher).toBe('shell')
    expect(resolved.launchers.map(l => l.id)).toEqual(['shell', 'claude'])
  })

  test('falls back to local when peers map is absent', () => {
    const noPeers: LaunchConfig = { default_launcher: 'shell', launchers: localConfig.launchers }
    const resolved = launchersForPeer(noPeers, 'work-laptop')
    expect(resolved.default_launcher).toBe('shell')
  })
})

// -- Helpers for building test sessions --

function sess(overrides: Partial<Session> & { id: string }): Session {
  return {
    created_at: '2026-04-01T00:00:00Z',
    command: [],
    cwd: '/home/mg/dev/gmux',
    kind: 'shell',
    alive: true,
    pid: 1,
    exit_code: null,
    started_at: '2026-04-01T00:00:00Z',
    exited_at: null,
    title: 'test',
    subtitle: '',
    status: null,
    unread: false,
    resumable: false,
    socket_path: '/tmp/test.sock',
    ...overrides,
  }
}

describe('resolveTarget', () => {
  test('uses selected session when it is in the list', () => {
    const sessions = [
      sess({ id: 'a', cwd: '/home/mg/dev/gmux' }),
      sess({ id: 'b', cwd: '/workspace', peer: 'laptop' }),
    ]
    const target = resolveTarget(sessions, 'b', '/fallback')
    expect(target).toEqual({ peer: 'laptop', cwd: '/workspace' })
  })

  test('ignores selectedId when it is not in the session list', () => {
    const sessions = [
      sess({ id: 'a', cwd: '/home/mg/dev/gmux' }),
    ]
    const target = resolveTarget(sessions, 'other-project-session', '/fallback')
    expect(target).toEqual({ peer: undefined, cwd: '/home/mg/dev/gmux' })
  })

  test('uses most recently created alive session when none selected', () => {
    const sessions = [
      sess({ id: 'old', cwd: '/old', created_at: '2026-01-01T00:00:00Z' }),
      sess({ id: 'new', cwd: '/new', peer: 'server', created_at: '2026-04-01T00:00:00Z' }),
    ]
    const target = resolveTarget(sessions, null, '/fallback')
    expect(target).toEqual({ peer: 'server', cwd: '/new' })
  })

  test('skips dead sessions when finding most recent alive', () => {
    const sessions = [
      sess({ id: 'dead', cwd: '/dead', created_at: '2026-04-02T00:00:00Z', alive: false }),
      sess({ id: 'alive', cwd: '/alive', created_at: '2026-04-01T00:00:00Z' }),
    ]
    const target = resolveTarget(sessions, null, '/fallback')
    expect(target).toEqual({ peer: undefined, cwd: '/alive' })
  })

  test('uses selected dead session (user is looking at it)', () => {
    const sessions = [
      sess({ id: 'alive-local', cwd: '/local', created_at: '2026-04-01T00:00:00Z' }),
      sess({ id: 'dead-remote', cwd: '/workspace', peer: 'laptop', alive: false, resumable: true }),
    ]
    const target = resolveTarget(sessions, 'dead-remote', '/fallback')
    expect(target).toEqual({ peer: 'laptop', cwd: '/workspace' })
  })

  test('falls back to fallbackCwd when no sessions', () => {
    const target = resolveTarget([], null, '/home/mg/dev/gmux')
    expect(target).toEqual({ cwd: '/home/mg/dev/gmux' })
  })

  test('falls back when all sessions are dead', () => {
    const sessions = [
      sess({ id: 'a', alive: false, cwd: '/dead' }),
    ]
    const target = resolveTarget(sessions, null, '/fallback')
    expect(target).toEqual({ cwd: '/fallback' })
  })
})

describe('formatTarget', () => {
  test('shows short cwd for local target', () => {
    expect(formatTarget({ cwd: '/home/mg/dev/gmux' })).toBe('~/dev/gmux')
  })

  test('prefixes peer name for remote target', () => {
    expect(formatTarget({ peer: 'laptop', cwd: '/workspace' })).toBe('laptop: /workspace')
  })

  test('shortens home dir even with peer', () => {
    expect(formatTarget({ peer: 'server', cwd: '/home/mg/work' })).toBe('server: ~/work')
  })
})
