import { describe, it, expect } from 'vitest'
import {
  matchSession,
  buildProjectFolders,
  normalizeRemote,
  parseSessionPath,
  sessionPath,
  resolveSessionFromPath,
  resolveViewFromPath,
  viewToPath,
  viewsEqual,
  parseSessionHostPath,
  buildProjectTopology,
  isSessionVisibleInProject,
  type Session,
  type ProjectItem,
  type PeerInfo,
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

// --- Project matching ---

describe('matchSession', () => {
  const projects: ProjectItem[] = [
    { slug: 'gmux', match: [{ remote: 'github.com/gmuxapp/gmux' }, { path: '/dev/gmux' }] },
    { slug: 'yapp', match: [{ path: '/dev/yapp' }] },
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

  it('falls back to path matching when no remote matches', () => {
    const sess = makeSession({ id: 's3', cwd: '/dev/yapp/deep' })
    expect(matchSession(sess, projects)?.slug).toBe('yapp')
  })

  it('uses project paths when a session has no remotes', () => {
    const sess = makeSession({ id: 's4', cwd: '/dev/gmux/src' })
    expect(matchSession(sess, projects)?.slug).toBe('gmux')
  })

  it('returns null for unmatched sessions', () => {
    const sess = makeSession({ id: 's5', cwd: '/other/place' })
    expect(matchSession(sess, projects)).toBeNull()
  })

  it('lets remote-backed child projects beat a vague parent path', () => {
    const projects: ProjectItem[] = [
      { slug: 'mg', match: [{ path: '/home/mg' }] },
      { slug: 'gmux', match: [{ remote: 'github.com/gmuxapp/gmux' }, { path: '/home/mg/dev/gmux' }] },
      { slug: 'dots', match: [{ remote: 'github.com/mgabor3141/dots' }, { path: '/home/mg/.local/share/chezmoi' }] },
    ]

    const gmuxSession = makeSession({
      id: 'g1',
      cwd: '/home/mg/dev/gmux/src',
      remotes: { origin: 'git@github.com:gmuxapp/gmux.git' },
    })
    expect(matchSession(gmuxSession, projects)?.slug).toBe('gmux')

    const dotsSession = makeSession({
      id: 'd1',
      cwd: '/home/mg/.local/share/chezmoi',
      remotes: { origin: 'git@github.com:mgabor3141/dots.git' },
    })
    expect(matchSession(dotsSession, projects)?.slug).toBe('dots')
  })

  it('prefers most specific path over remote match', () => {
    const projects: ProjectItem[] = [
      { slug: 'teak', match: [{ path: '/home/user/dev/gmux/.grove/teak' }] },
      { slug: 'gmux', match: [{ remote: 'github.com/gmuxapp/gmux' }, { path: '/home/user/dev/gmux' }] },
    ]

    // Session in teak's directory: teak path is more specific than gmux path.
    const sess = makeSession({
      id: 'w1',
      cwd: '/home/user/dev/gmux/.grove/teak/src',
      remotes: { origin: 'git@github.com:gmuxapp/gmux.git' },
    })
    expect(matchSession(sess, projects)?.slug).toBe('teak')

    // Session with remote but no path match: falls back to remote.
    const sess2 = makeSession({
      id: 'w2',
      cwd: '/other/dir',
      remotes: { origin: 'git@github.com:gmuxapp/gmux.git' },
    })
    expect(matchSession(sess2, projects)?.slug).toBe('gmux')
  })

  it('exact path matches only the exact directory, not subdirs', () => {
    const projects: ProjectItem[] = [
      { slug: 'home', match: [{ path: '~', exact: true }] },
      { slug: 'gmux', match: [{ path: '~/dev/gmux' }] },
    ]

    // Session at ~ itself: matches home.
    expect(matchSession(makeSession({ id: 'h1', cwd: '~' }), projects)?.slug).toBe('home')

    // Session under ~/dev/gmux: matches gmux, NOT home.
    expect(matchSession(makeSession({ id: 'g1', cwd: '~/dev/gmux/src' }), projects)?.slug).toBe('gmux')

    // Session under ~ but not in any other project: does NOT match home (exact).
    expect(matchSession(makeSession({ id: 'x1', cwd: '~/documents' }), projects)).toBeNull()
  })

  it('exact path matches via workspace_root', () => {
    const projects: ProjectItem[] = [
      { slug: 'scripts', match: [{ path: '/home/user/scripts', exact: true }] },
    ]
    const sess = makeSession({ id: 's1', cwd: '/other', workspace_root: '/home/user/scripts' })
    expect(matchSession(sess, projects)?.slug).toBe('scripts')
  })

  it('exact path does not match subdirectories', () => {
    const projects: ProjectItem[] = [
      { slug: 'scripts', match: [{ path: '/home/user/scripts', exact: true }] },
    ]
    const sess = makeSession({ id: 's1', cwd: '/home/user/scripts/bin' })
    expect(matchSession(sess, projects)).toBeNull()
  })
})

describe('buildProjectFolders', () => {
  it('builds folders in project order', () => {
    const projects: ProjectItem[] = [
      { slug: 'beta', match: [{ path: '/dev/beta' }] },
      { slug: 'alpha', match: [{ path: '/dev/alpha' }] },
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
      { slug: 'empty', match: [{ path: '/dev/empty' }] },
    ]
    const folders = buildProjectFolders(projects, [])
    expect(folders).toHaveLength(1)
    expect(folders[0].name).toBe('empty')
    expect(folders[0].sessions).toHaveLength(0)
  })

  it('sets launchCwd from project paths', () => {
    const projects: ProjectItem[] = [
      { slug: 'proj', match: [{ path: '/dev/proj' }, { path: '/dev/proj2' }] },
    ]
    const folders = buildProjectFolders(projects, [])
    expect(folders[0].launchCwd).toBe('/dev/proj')
  })

  it('excludes dead sessions not in the project sessions array', () => {
    const projects: ProjectItem[] = [
      { slug: 'proj', match: [{ path: '/dev/proj' }], sessions: ['kept-id'] },
    ]
    const sessions = [
      makeSession({ id: 'kept-id', cwd: '/dev/proj', alive: false, resumable: true }),
      makeSession({ id: 'old-dead', cwd: '/dev/proj', alive: false, resumable: true }),
      makeSession({ id: 'alive-1', cwd: '/dev/proj', alive: true }),
    ]
    const folders = buildProjectFolders(projects, sessions)
    const ids = folders[0].sessions.map(s => s.id)
    expect(ids).toContain('alive-1')   // alive: always shown
    expect(ids).toContain('kept-id')   // dead but in array: shown
    expect(ids).not.toContain('old-dead') // dead, not in array: hidden
  })

  it('matches dead sessions by slug in array', () => {
    const projects: ProjectItem[] = [
      { slug: 'proj', match: [{ path: '/dev/proj' }], sessions: ['my-resume-key'] },
    ]
    const sessions = [
      makeSession({ id: 'sess-1', cwd: '/dev/proj', alive: false, resumable: true, slug: 'my-resume-key' }),
    ]
    const folders = buildProjectFolders(projects, sessions)
    expect(folders[0].sessions).toHaveLength(1)
  })

  it('excludes dead sessions whose slug is not in the array', () => {
    const projects: ProjectItem[] = [
      { slug: 'proj', match: [{ path: '/dev/proj' }], sessions: ['other-key'] },
    ]
    const sessions = [
      makeSession({ id: 'sess-1', cwd: '/dev/proj', alive: false, resumable: true, slug: 'my-resume-key' }),
    ]
    const folders = buildProjectFolders(projects, sessions)
    expect(folders[0].sessions).toHaveLength(0)
  })
})

// --- URL routing ---

describe('parseSessionPath', () => {
  it('parses full local path', () => {
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

  it('parses @host segment as remote host', () => {
    expect(parseSessionPath('/gmux/@desktop/pi/fix-auth')).toEqual({
      project: 'gmux', host: 'desktop', adapter: 'pi', slug: 'fix-auth',
    })
  })

  it('parses project + @host only', () => {
    expect(parseSessionPath('/gmux/@server')).toEqual({
      project: 'gmux', host: 'server',
    })
  })

  it('parses project + @host + adapter', () => {
    expect(parseSessionPath('/gmux/@server/pi')).toEqual({
      project: 'gmux', host: 'server', adapter: 'pi',
    })
  })

  it('does not treat non-@ second segment as host', () => {
    expect(parseSessionPath('/gmux/pi')).toEqual({
      project: 'gmux', adapter: 'pi',
    })
  })
})

describe('sessionPath', () => {
  it('builds URL from slug', () => {
    expect(sessionPath('gmux', { kind: 'pi', slug: 'fix-auth', id: 'abc' }))
      .toBe('/gmux/pi/fix-auth')
  })

  it('falls back to ID prefix when slug missing', () => {
    expect(sessionPath('gmux', { kind: 'pi', id: 'abcdef12-3456-7890' }))
      .toBe('/gmux/pi/abcdef12')
  })

  it('includes @peer for remote sessions', () => {
    expect(sessionPath('gmux', { kind: 'pi', slug: 'fix-auth', id: 'abc', peer: 'server' }))
      .toBe('/gmux/@server/pi/fix-auth')
  })

  it('omits @peer for local sessions', () => {
    expect(sessionPath('gmux', { kind: 'pi', slug: 'fix-auth', id: 'abc', peer: undefined }))
      .toBe('/gmux/pi/fix-auth')
  })
})

describe('resolveSessionFromPath', () => {
  const projects: ProjectItem[] = [
    { slug: 'gmux', match: [{ remote: 'github.com/gmuxapp/gmux' }, { path: '/dev/gmux' }] },
  ]
  const localSessions = [
    makeSession({ id: 'sess-1', cwd: '/dev/gmux', kind: 'pi', slug: 'fix-auth',
      remotes: { origin: 'github.com/gmuxapp/gmux' } }),
    makeSession({ id: 'sess-2', cwd: '/dev/gmux', kind: 'shell', slug: 'fish',
      remotes: { origin: 'github.com/gmuxapp/gmux' } }),
  ]

  it('resolves full path to session ID', () => {
    const id = resolveSessionFromPath(
      { project: 'gmux', adapter: 'pi', slug: 'fix-auth' }, projects, localSessions,
    )
    expect(id).toBe('sess-1')
  })

  it('resolves project-only to first alive session', () => {
    const id = resolveSessionFromPath({ project: 'gmux' }, projects, localSessions)
    expect(id).toBe('sess-1')
  })

  it('returns null for unknown project', () => {
    const id = resolveSessionFromPath({ project: 'nope' }, projects, localSessions)
    expect(id).toBeNull()
  })

  // Peer-aware resolution
  const mixedSessions = [
    ...localSessions,
    makeSession({ id: 'sess-r1@server', cwd: '/dev/gmux', kind: 'pi', slug: 'fix-auth',
      peer: 'server', remotes: { origin: 'github.com/gmuxapp/gmux' } }),
    makeSession({ id: 'sess-r2@server', cwd: '/dev/gmux', kind: 'shell', slug: 'bash',
      peer: 'server', remotes: { origin: 'github.com/gmuxapp/gmux' } }),
  ]

  it('resolves remote session with @host in URL', () => {
    const id = resolveSessionFromPath(
      { project: 'gmux', host: 'server', adapter: 'pi', slug: 'fix-auth' },
      projects, mixedSessions,
    )
    expect(id).toBe('sess-r1@server')
  })

  it('local path resolves to local session, not remote', () => {
    const id = resolveSessionFromPath(
      { project: 'gmux', adapter: 'pi', slug: 'fix-auth' },
      projects, mixedSessions,
    )
    expect(id).toBe('sess-1')
  })

  it('returns null for unknown peer', () => {
    const id = resolveSessionFromPath(
      { project: 'gmux', host: 'unknown', adapter: 'pi', slug: 'fix-auth' },
      projects, mixedSessions,
    )
    expect(id).toBeNull()
  })

  it('project-only with @host resolves to first alive remote session', () => {
    const id = resolveSessionFromPath(
      { project: 'gmux', host: 'server' },
      projects, mixedSessions,
    )
    expect(id).toBe('sess-r1@server')
  })

  it('resolves by ID prefix when session has no slug', () => {
    const unattributed = [
      makeSession({ id: 'sess-abc12345', cwd: '/dev/gmux', kind: 'pi',
        remotes: { origin: 'github.com/gmuxapp/gmux' } }),
    ]
    const id = resolveSessionFromPath(
      { project: 'gmux', adapter: 'pi', slug: 'sess-abc' },
      projects, unattributed,
    )
    expect(id).toBe('sess-abc12345')
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

// --- View routing ---

describe('resolveViewFromPath', () => {
  const projects: ProjectItem[] = [
    { slug: 'gmux', match: [{ remote: 'github.com/gmuxapp/gmux' }, { path: '/dev/gmux' }] },
  ]
  const sessions = [
    makeSession({ id: 'sess-1', cwd: '/dev/gmux', kind: 'pi', slug: 'fix-auth',
      remotes: { origin: 'github.com/gmuxapp/gmux' } }),
  ]

  it('root path resolves to home', () => {
    expect(resolveViewFromPath('/', projects, sessions)).toEqual({ kind: 'home' })
  })

  it('empty path resolves to home', () => {
    expect(resolveViewFromPath('', projects, sessions)).toEqual({ kind: 'home' })
  })

  it('internal routes resolve to home', () => {
    expect(resolveViewFromPath('/_/input-diagnostics', projects, sessions)).toEqual({ kind: 'home' })
  })

  it('project-only path resolves to project view (hub page)', () => {
    expect(resolveViewFromPath('/gmux', projects, sessions)).toEqual({
      kind: 'project', projectSlug: 'gmux',
    })
  })

  it('project-only path with no sessions still resolves to project view', () => {
    expect(resolveViewFromPath('/gmux', projects, [])).toEqual({
      kind: 'project', projectSlug: 'gmux',
    })
  })

  it('unknown project resolves to home', () => {
    expect(resolveViewFromPath('/unknown', projects, sessions)).toEqual({ kind: 'home' })
  })

  it('full session path resolves to session view', () => {
    expect(resolveViewFromPath('/gmux/pi/fix-auth', projects, sessions)).toEqual({
      kind: 'session', sessionId: 'sess-1',
    })
  })

  it('session path with missing session falls back to project view', () => {
    expect(resolveViewFromPath('/gmux/pi/no-such-session', projects, sessions)).toEqual({
      kind: 'project', projectSlug: 'gmux',
    })
  })

  it('remote session URL resolves to session view', () => {
    const remoteSess = makeSession({
      id: 'sess-3@server', cwd: '/dev/gmux', kind: 'shell', slug: 'bash',
      peer: 'server', remotes: { origin: 'github.com/gmuxapp/gmux' },
    })
    expect(resolveViewFromPath('/gmux/@server/shell/bash', projects, [...sessions, remoteSess])).toEqual({
      kind: 'session', sessionId: 'sess-3@server',
    })
  })

  it('remote URL with missing session falls back to project view', () => {
    expect(resolveViewFromPath('/gmux/@server/shell/gone', projects, sessions)).toEqual({
      kind: 'project', projectSlug: 'gmux',
    })
  })
})

describe('viewToPath', () => {
  const projects: ProjectItem[] = [
    { slug: 'gmux', match: [{ remote: 'github.com/gmuxapp/gmux' }, { path: '/dev/gmux' }] },
  ]
  const sessions = [
    makeSession({ id: 'sess-1', cwd: '/dev/gmux', kind: 'pi', slug: 'fix-auth',
      remotes: { origin: 'github.com/gmuxapp/gmux' } }),
    makeSession({ id: 'sess-2@server', cwd: '/dev/gmux', kind: 'shell', slug: 'bash',
      peer: 'server', remotes: { origin: 'github.com/gmuxapp/gmux' } }),
  ]

  it('home view → /', () => {
    expect(viewToPath({ kind: 'home' }, projects, sessions)).toBe('/')
  })

  it('project view → /:project', () => {
    expect(viewToPath({ kind: 'project', projectSlug: 'gmux' }, projects, sessions)).toBe('/gmux')
  })

  it('session view → full session path', () => {
    expect(viewToPath({ kind: 'session', sessionId: 'sess-1' }, projects, sessions))
      .toBe('/gmux/pi/fix-auth')
  })

  it('session view with peer → path includes @host', () => {
    expect(viewToPath({ kind: 'session', sessionId: 'sess-2@server' }, projects, sessions))
      .toBe('/gmux/@server/shell/bash')
  })

  it('session view for missing session → null', () => {
    expect(viewToPath({ kind: 'session', sessionId: 'gone' }, projects, sessions)).toBeNull()
  })

  it('session view for unmatched session → null', () => {
    const orphan = makeSession({ id: 'orphan', cwd: '/nowhere', kind: 'pi' })
    expect(viewToPath({ kind: 'session', sessionId: 'orphan' }, projects, [orphan])).toBeNull()
  })
})

describe('viewsEqual', () => {
  it('same home views are equal', () => {
    expect(viewsEqual({ kind: 'home' }, { kind: 'home' })).toBe(true)
  })

  it('same project views are equal', () => {
    expect(viewsEqual(
      { kind: 'project', projectSlug: 'a' },
      { kind: 'project', projectSlug: 'a' },
    )).toBe(true)
  })

  it('different project slugs are not equal', () => {
    expect(viewsEqual(
      { kind: 'project', projectSlug: 'a' },
      { kind: 'project', projectSlug: 'b' },
    )).toBe(false)
  })

  it('same session views are equal', () => {
    expect(viewsEqual(
      { kind: 'session', sessionId: 'x' },
      { kind: 'session', sessionId: 'x' },
    )).toBe(true)
  })

  it('different kinds are not equal', () => {
    expect(viewsEqual(
      { kind: 'home' },
      { kind: 'project', projectSlug: 'a' },
    )).toBe(false)
  })
})

describe('View round-trip', () => {
  const projects: ProjectItem[] = [
    { slug: 'gmux', match: [{ remote: 'github.com/gmuxapp/gmux' }, { path: '/dev/gmux' }] },
  ]
  const sessions = [
    makeSession({ id: 'sess-1', cwd: '/dev/gmux', kind: 'pi', slug: 'fix-auth',
      remotes: { origin: 'github.com/gmuxapp/gmux' } }),
  ]

  it('home view round-trips', () => {
    const path = viewToPath({ kind: 'home' }, projects, sessions)
    expect(path).toBe('/')
    expect(resolveViewFromPath(path!, projects, sessions)).toEqual({ kind: 'home' })
  })

  it('session view round-trips', () => {
    const path = viewToPath({ kind: 'session', sessionId: 'sess-1' }, projects, sessions)
    expect(path).toBe('/gmux/pi/fix-auth')
    expect(resolveViewFromPath(path!, projects, sessions)).toEqual({
      kind: 'session', sessionId: 'sess-1',
    })
  })

  it('project view round-trips regardless of sessions', () => {
    const path = viewToPath({ kind: 'project', projectSlug: 'gmux' }, projects, sessions)
    expect(path).toBe('/gmux')
    expect(viewsEqual(
      resolveViewFromPath(path!, projects, sessions),
      { kind: 'project', projectSlug: 'gmux' },
    )).toBe(true)
    expect(viewsEqual(
      resolveViewFromPath(path!, projects, []),
      { kind: 'project', projectSlug: 'gmux' },
    )).toBe(true)
  })
})

describe('isSessionVisibleInProject', () => {
  const project: ProjectItem = { slug: 'test', match: [{ path: '/dev/test' }], sessions: ['my-session', 'sess-tracked'] }

  it('alive sessions are always visible', () => {
    const s = makeSession({ id: 'sess-new', cwd: '/dev/test', alive: true })
    expect(isSessionVisibleInProject(s, project)).toBe(true)
  })

  it('dead non-resumable sessions are hidden', () => {
    const s = makeSession({ id: 'sess-gone', cwd: '/dev/test', alive: false, resumable: false })
    expect(isSessionVisibleInProject(s, project)).toBe(false)
  })

  it('dead resumable sessions show when slug is tracked', () => {
    const s = makeSession({ id: 'sess-x', cwd: '/dev/test', alive: false, resumable: true, slug: 'my-session' })
    expect(isSessionVisibleInProject(s, project)).toBe(true)
  })

  it('dead resumable sessions show when id is tracked', () => {
    const s = makeSession({ id: 'sess-tracked', cwd: '/dev/test', alive: false, resumable: true })
    expect(isSessionVisibleInProject(s, project)).toBe(true)
  })

  it('dead resumable sessions are hidden when not tracked', () => {
    const s = makeSession({ id: 'sess-orphan', cwd: '/dev/test', alive: false, resumable: true, slug: 'orphan' })
    expect(isSessionVisibleInProject(s, project)).toBe(false)
  })

  it('handles projects with no sessions array', () => {
    const bare: ProjectItem = { slug: 'bare', match: [{ path: '/dev/bare' }] }
    const s = makeSession({ id: 'sess-x', cwd: '/dev/bare', alive: false, resumable: true })
    expect(isSessionVisibleInProject(s, bare)).toBe(false)
  })
})

describe('parseSessionHostPath', () => {
  it('treats bare ids as local', () => {
    expect(parseSessionHostPath('sess-abc')).toEqual({ originalId: 'sess-abc', path: [] })
  })

  it('extracts a single peer hop', () => {
    expect(parseSessionHostPath('sess-abc@workstation')).toEqual({
      originalId: 'sess-abc', path: ['workstation'],
    })
  })

  it('reverses nested chains to outermost-first', () => {
    // On-the-wire (innermost-first): sess-abc@dev@workstation
    // Means: session sess-abc lives on dev, which is workstation's peer.
    // UI path (root → leaf): ['workstation', 'dev']
    expect(parseSessionHostPath('sess-abc@dev@workstation')).toEqual({
      originalId: 'sess-abc', path: ['workstation', 'dev'],
    })
  })

  it('preserves original id characters other than @', () => {
    expect(parseSessionHostPath('file-abc-123@peer')).toEqual({
      originalId: 'file-abc-123', path: ['peer'],
    })
  })
})

describe('buildProjectTopology', () => {
  const projects: ProjectItem[] = [
    { slug: 'fluxer', match: [{ path: '/home/mg/dev/fluxer' }], sessions: [] },
  ]

  const peers: PeerInfo[] = [
    { name: 'workstation', url: 'http://100.64.0.2:8790', status: 'connected', session_count: 2 },
    { name: 'offline-box', url: 'http://10.0.0.9:8790', status: 'disconnected', session_count: 0 },
  ]

  it('returns empty array for unknown project', () => {
    expect(buildProjectTopology('ghost', [], projects, peers)).toEqual([])
  })

  it('returns empty array when project has no sessions', () => {
    expect(buildProjectTopology('fluxer', [], projects, peers)).toEqual([])
  })

  it('groups local sessions by cwd', () => {
    const sessions = [
      makeSession({ id: 'sess-1', cwd: '/home/mg/dev/fluxer', created_at: '2026-01-01T00:00:02Z' }),
      makeSession({ id: 'sess-2', cwd: '/home/mg/dev/fluxer', created_at: '2026-01-01T00:00:01Z' }),
      makeSession({ id: 'sess-3', cwd: '/home/mg/dev/fluxer/api' }),
    ]
    const hosts = buildProjectTopology('fluxer', sessions, projects, peers)
    expect(hosts).toHaveLength(1)
    expect(hosts[0].path).toEqual([])
    expect(hosts[0].status).toBe('local')
    expect(hosts[0].folders).toHaveLength(2)
    expect(hosts[0].folders[0].cwd).toBe('/home/mg/dev/fluxer')
    expect(hosts[0].folders[0].sessions.map(s => s.id)).toEqual(['sess-1', 'sess-2'])
    expect(hosts[0].folders[1].cwd).toBe('/home/mg/dev/fluxer/api')
  })

  it('separates local and peer sessions', () => {
    const sessions = [
      makeSession({ id: 'sess-local', cwd: '/home/mg/dev/fluxer' }),
      makeSession({ id: 'sess-remote@workstation', cwd: '/home/mg/dev/fluxer', peer: 'workstation' }),
    ]
    const hosts = buildProjectTopology('fluxer', sessions, projects, peers)
    expect(hosts).toHaveLength(2)
    // Local first.
    expect(hosts[0].path).toEqual([])
    expect(hosts[0].status).toBe('local')
    // Peer second.
    expect(hosts[1].path).toEqual(['workstation'])
    expect(hosts[1].status).toBe('connected')
    expect(hosts[1].meta).toBe('http://100.64.0.2:8790')
  })

  it('nested peers form multi-segment paths', () => {
    // Nested devcontainer sessions live at paths that don't overlap the
    // project's local paths, so matchSession relies on git remotes here.
    const projectsWithRemote: ProjectItem[] = [
      { slug: 'fluxer', match: [{ remote: 'github.com/mg/fluxer' }, { path: '/home/mg/dev/fluxer' }], sessions: [] },
    ]
    const sessions = [
      makeSession({
        id: 'sess-nested@dev@workstation', cwd: '/workspace/fluxer', peer: 'workstation',
        remotes: { origin: 'https://github.com/mg/fluxer.git' },
      }),
    ]
    const hosts = buildProjectTopology('fluxer', sessions, projectsWithRemote, peers)
    expect(hosts).toHaveLength(1)
    expect(hosts[0].path).toEqual(['workstation', 'dev'])
    // Nested inherits root peer status.
    expect(hosts[0].status).toBe('connected')
  })

  it('marks unknown peers as disconnected', () => {
    const sessions = [
      makeSession({ id: 'sess-a@ghost', cwd: '/home/mg/dev/fluxer', peer: 'ghost' }),
    ]
    const hosts = buildProjectTopology('fluxer', sessions, projects, peers)
    expect(hosts).toHaveLength(1)
    expect(hosts[0].status).toBe('disconnected')
    expect(hosts[0].meta).toBe('')
  })

  it('reflects peer status from the peers list', () => {
    const sessions = [
      makeSession({ id: 'sess-o@offline-box', cwd: '/home/mg/dev/fluxer', peer: 'offline-box' }),
    ]
    const hosts = buildProjectTopology('fluxer', sessions, projects, peers)
    expect(hosts[0].status).toBe('disconnected')
  })

  it('sorts peers alphabetically, local first', () => {
    const peers2: PeerInfo[] = [
      { name: 'alpha', url: 'http://a', status: 'connected', session_count: 1 },
      { name: 'bravo', url: 'http://b', status: 'connected', session_count: 1 },
    ]
    const sessions = [
      makeSession({ id: 's-b@bravo', cwd: '/home/mg/dev/fluxer', peer: 'bravo' }),
      makeSession({ id: 's-a@alpha', cwd: '/home/mg/dev/fluxer', peer: 'alpha' }),
      makeSession({ id: 's-local', cwd: '/home/mg/dev/fluxer' }),
    ]
    const hosts = buildProjectTopology('fluxer', sessions, projects, peers2)
    expect(hosts.map(h => h.path.join('/') || '(local)')).toEqual(['(local)', 'alpha', 'bravo'])
  })

  it('sorts sessions within a folder: alive first, then newest-first', () => {
    // Dead session must be resumable AND in project.sessions[] to pass
    // the visibility filter (mirrors sidebar behavior).
    const projectsWithDead: ProjectItem[] = [
      { slug: 'fluxer', match: [{ path: '/home/mg/dev/fluxer' }], sessions: ['dead-old'] },
    ]
    const sessions = [
      makeSession({
        id: 'dead-old', cwd: '/home/mg/dev/fluxer',
        alive: false, resumable: true, created_at: '2026-01-01T00:00:00Z',
      }),
      makeSession({
        id: 'alive-old', cwd: '/home/mg/dev/fluxer',
        alive: true, created_at: '2026-01-02T00:00:00Z',
      }),
      makeSession({
        id: 'alive-new', cwd: '/home/mg/dev/fluxer',
        alive: true, created_at: '2026-01-03T00:00:00Z',
      }),
    ]
    const hosts = buildProjectTopology('fluxer', sessions, projectsWithDead, peers)
    expect(hosts[0].folders[0].sessions.map(s => s.id)).toEqual([
      'alive-new', 'alive-old', 'dead-old',
    ])
  })

  it('hides dead non-resumable sessions', () => {
    const sessions = [
      makeSession({ id: 'alive-1', cwd: '/home/mg/dev/fluxer', alive: true }),
      makeSession({ id: 'dead-1', cwd: '/home/mg/dev/fluxer', alive: false, resumable: false }),
    ]
    const hosts = buildProjectTopology('fluxer', sessions, projects, peers)
    expect(hosts[0].folders[0].sessions.map(s => s.id)).toEqual(['alive-1'])
  })

  it('hides resumable sessions not tracked in project.sessions[]', () => {
    const sessions = [
      makeSession({ id: 'alive-1', cwd: '/home/mg/dev/fluxer', alive: true }),
      // Dead, resumable, but not in project.sessions[] → filtered out.
      makeSession({
        id: 'orphan', cwd: '/home/mg/dev/fluxer',
        alive: false, resumable: true,
      }),
    ]
    const hosts = buildProjectTopology('fluxer', sessions, projects, peers)
    expect(hosts[0].folders[0].sessions.map(s => s.id)).toEqual(['alive-1'])
  })

  it('filters sessions by project match (ignores unrelated sessions)', () => {
    const projects2: ProjectItem[] = [
      { slug: 'fluxer', match: [{ path: '/home/mg/dev/fluxer' }], sessions: [] },
      { slug: 'other', match: [{ path: '/home/mg/dev/other' }], sessions: [] },
    ]
    const sessions = [
      makeSession({ id: 'f1', cwd: '/home/mg/dev/fluxer' }),
      makeSession({ id: 'o1', cwd: '/home/mg/dev/other' }),
    ]
    const hosts = buildProjectTopology('fluxer', sessions, projects2, peers)
    expect(hosts).toHaveLength(1)
    expect(hosts[0].folders[0].sessions.map(s => s.id)).toEqual(['f1'])
  })
})
