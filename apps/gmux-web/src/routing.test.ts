import { describe, it, expect } from 'vitest'
import type { ProjectItem } from './types'
import {
  parseSessionPath,
  sessionPath,
  resolveSessionFromPath,
  resolveViewFromPath,
  viewToPath,
  viewsEqual,
  withTabParams,
} from './routing'
import { makeSession } from './test-helpers'

describe('withTabParams', () => {
  it('carries filter and sidebar params onto a path', () => {
    expect(withTabParams('/gmux/shell/x', '?filter=gmux%40server&sidebar=activity'))
      .toBe('/gmux/shell/x?filter=gmux%40server&sidebar=activity')
  })

  it('drops transient params (settings) and returns the bare path when none apply', () => {
    expect(withTabParams('/gmux', '?settings=hosts')).toBe('/gmux')
    expect(withTabParams('/gmux', '')).toBe('/gmux')
  })
})

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

  it('parses /@<owner>/<project> as a peer-owned project hub', () => {
    expect(parseSessionPath('/@tower/gmux'))
      .toEqual({ projectPeer: 'tower', project: 'gmux' })
  })

  it('parses /@<owner> alone as the peer with no project', () => {
    expect(parseSessionPath('/@tower'))
      .toEqual({ projectPeer: 'tower' })
  })

  it('parses /@<owner>/<project>/<adapter>/<slug> as a peer-project session', () => {
    expect(parseSessionPath('/@tower/gmux/pi/fix-auth'))
      .toEqual({ projectPeer: 'tower', project: 'gmux', adapter: 'pi', slug: 'fix-auth' })
  })

  it('does not treat non-@ second segment as host', () => {
    expect(parseSessionPath('/gmux/pi')).toEqual({
      project: 'gmux', adapter: 'pi',
    })
  })
})

describe('sessionPath', () => {
  it('builds URL from slug', () => {
    expect(sessionPath('gmux', { adapter: 'pi', slug: 'fix-auth', id: 'abc' }))
      .toBe('/gmux/pi/fix-auth')
  })

  it('falls back to the full session id when slug missing', () => {
    // Untitled session: no slug yet. Use the whole `sess-<hex>` id, not a
    // truncation — `slice(0, 8)` would leave only 3 hex of entropy and
    // collide across untitled sessions (resolveSessionFromPath prefix-matches).
    expect(sessionPath('gmux', { adapter: 'pi', id: 'sess-a1b2c3d4' }))
      .toBe('/gmux/pi/sess-a1b2c3d4')
  })

  it('includes @peer for remote sessions', () => {
    expect(sessionPath('gmux', { adapter: 'pi', slug: 'fix-auth', id: 'abc', peer: 'server' }))
      .toBe('/gmux/@server/pi/fix-auth')
  })

  it('peer-owned project: leading @owner, no redundant mid-path host', () => {
    // The session lives on the project's owner, so the mid-path host
    // segment is redundant and is omitted.
    expect(sessionPath(
      'gmux',
      { id: 'sess-1@tower', adapter: 'pi', slug: 'fix-auth', peer: 'tower' },
      'tower',
    )).toBe('/@tower/gmux/pi/fix-auth')
  })

  it('local project + adopted peer session: keeps mid-path @host', () => {
    // Disclaimed peer session adopted into a local folder. The project
    // owner is local (no leading @), but the session lives on a peer
    // (mid-path @<host> needed to disambiguate).
    expect(sessionPath(
      'gmux',
      { id: 'sess-1@dev', adapter: 'pi', slug: 'fix-auth', peer: 'dev' },
    )).toBe('/gmux/@dev/pi/fix-auth')
  })

  it('omits @peer for local sessions', () => {
    expect(sessionPath('gmux', { adapter: 'pi', slug: 'fix-auth', id: 'abc', peer: undefined }))
      .toBe('/gmux/pi/fix-auth')
  })
})

describe('resolveSessionFromPath', () => {
  const projects: ProjectItem[] = [
    { slug: 'gmux', match: [{ remote: 'github.com/gmuxapp/gmux' }, { path: '/dev/gmux' }] },
  ]
  const localSessions = [
    makeSession({ id: 'sess-1', cwd: '/dev/gmux', adapter: 'pi', slug: 'fix-auth',
      remotes: { origin: 'github.com/gmuxapp/gmux' } }),
    makeSession({ id: 'sess-2', cwd: '/dev/gmux', adapter: 'shell', slug: 'fish',
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
    makeSession({ id: 'sess-r1@server', cwd: '/dev/gmux', adapter: 'pi', slug: 'fix-auth',
      peer: 'server', remotes: { origin: 'github.com/gmuxapp/gmux' } }),
    makeSession({ id: 'sess-r2@server', cwd: '/dev/gmux', adapter: 'shell', slug: 'bash',
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
      makeSession({ id: 'sess-abc12345', cwd: '/dev/gmux', adapter: 'pi',
        remotes: { origin: 'github.com/gmuxapp/gmux' } }),
    ]
    const id = resolveSessionFromPath(
      { project: 'gmux', adapter: 'pi', slug: 'sess-abc' },
      projects, unattributed,
    )
    expect(id).toBe('sess-abc12345')
  })

  it('resolves a peer-owned project URL via stamps, not viewer rules', () => {
    // Peer 'tower' has its own 'gmux' project. The viewer also has
    // a 'gmux' project, but the URL `/@tower/gmux/...` addresses the
    // peer-owned one; we must trust the stamp, not re-run match.
    const claimed = makeSession({
      id: 'sess-t1@tower', cwd: '/elsewhere', adapter: 'pi', slug: 'fix-auth',
      peer: 'tower', project_slug: 'gmux', project_index: 0,
    })
    const id = resolveSessionFromPath(
      { projectPeer: 'tower', project: 'gmux', adapter: 'pi', slug: 'fix-auth' },
      projects, [claimed],
    )
    expect(id).toBe('sess-t1@tower')
  })

  it('peer-owned project URL ignores local-stamped same-slug sessions', () => {
    const localGmux = makeSession({
      id: 'sess-local', cwd: '/dev/gmux', adapter: 'pi', slug: 'fix-auth',
      project_slug: 'gmux', project_index: 0,
    })
    const towerGmux = makeSession({
      id: 'sess-t@tower', cwd: '/elsewhere', adapter: 'pi', slug: 'fix-auth',
      peer: 'tower', project_slug: 'gmux', project_index: 0,
    })
    const id = resolveSessionFromPath(
      { projectPeer: 'tower', project: 'gmux', adapter: 'pi', slug: 'fix-auth' },
      projects, [localGmux, towerGmux],
    )
    expect(id).toBe('sess-t@tower')
  })
})

describe('resolveViewFromPath', () => {
  const projects: ProjectItem[] = [
    { slug: 'gmux', match: [{ remote: 'github.com/gmuxapp/gmux' }, { path: '/dev/gmux' }] },
  ]
  const sessions = [
    makeSession({ id: 'sess-1', cwd: '/dev/gmux', adapter: 'pi', slug: 'fix-auth',
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

  it('project-only path resolves to home (hubs retired)', () => {
    expect(resolveViewFromPath('/gmux', projects, sessions)).toEqual({ kind: 'home' })
  })

  it('peer-owned project URL resolves to home (hubs retired)', () => {
    const peerSession = makeSession({
      id: 'sess-t@tower', cwd: '/elsewhere', adapter: 'pi', slug: 'fix-auth',
      peer: 'tower', project_slug: 'gmux', project_index: 0,
    })
    expect(resolveViewFromPath('/@tower/gmux', projects, [peerSession])).toEqual({ kind: 'home' })
  })

  it('unknown project resolves to home', () => {
    expect(resolveViewFromPath('/unknown', projects, sessions)).toEqual({ kind: 'home' })
  })

  it('full session path resolves to session view', () => {
    expect(resolveViewFromPath('/gmux/pi/fix-auth', projects, sessions)).toEqual({
      kind: 'session', sessionId: 'sess-1',
    })
  })

  it('session path with missing session falls back to home', () => {
    expect(resolveViewFromPath('/gmux/pi/no-such-session', projects, sessions)).toEqual({ kind: 'home' })
  })

  it('remote session URL resolves to session view', () => {
    const remoteSess = makeSession({
      id: 'sess-3@server', cwd: '/dev/gmux', adapter: 'shell', slug: 'bash',
      peer: 'server', remotes: { origin: 'github.com/gmuxapp/gmux' },
    })
    expect(resolveViewFromPath('/gmux/@server/shell/bash', projects, [...sessions, remoteSess])).toEqual({
      kind: 'session', sessionId: 'sess-3@server',
    })
  })

  it('remote URL with missing session falls back to home', () => {
    expect(resolveViewFromPath('/gmux/@server/shell/gone', projects, sessions)).toEqual({ kind: 'home' })
  })
})

describe('viewToPath', () => {
  const projects: ProjectItem[] = [
    { slug: 'gmux', match: [{ remote: 'github.com/gmuxapp/gmux' }, { path: '/dev/gmux' }] },
  ]
  const sessions = [
    makeSession({ id: 'sess-1', cwd: '/dev/gmux', adapter: 'pi', slug: 'fix-auth',
      remotes: { origin: 'github.com/gmuxapp/gmux' } }),
    makeSession({ id: 'sess-2@server', cwd: '/dev/gmux', adapter: 'shell', slug: 'bash',
      peer: 'server', remotes: { origin: 'github.com/gmuxapp/gmux' } }),
  ]

  it('home view -> /', () => {
    expect(viewToPath({ kind: 'home' }, projects, sessions)).toBe('/')
  })

  it('session view -> full session path', () => {
    expect(viewToPath({ kind: 'session', sessionId: 'sess-1' }, projects, sessions))
      .toBe('/gmux/pi/fix-auth')
  })

  it('session view with peer -> path includes @host', () => {
    expect(viewToPath({ kind: 'session', sessionId: 'sess-2@server' }, projects, sessions))
      .toBe('/gmux/@server/shell/bash')
  })

  it('session view for missing session -> null', () => {
    expect(viewToPath({ kind: 'session', sessionId: 'gone' }, projects, sessions)).toBeNull()
  })

  it('session view for unmatched session -> null', () => {
    const orphan = makeSession({ id: 'orphan', cwd: '/nowhere', adapter: 'pi' })
    expect(viewToPath({ kind: 'session', sessionId: 'orphan' }, projects, [orphan])).toBeNull()
  })

  it('peer-claimed session -> /@<owner>/<slug>/...', () => {
    const claimed = makeSession({
      id: 'sess-c@tower', cwd: '/dev/gmux', adapter: 'pi', slug: 'on-tower',
      peer: 'tower', project_slug: 'gmux', project_index: 0,
    })
    expect(viewToPath(
      { kind: 'session', sessionId: 'sess-c@tower' },
      projects, [claimed],
    )).toBe('/@tower/gmux/pi/on-tower')
  })

  it('local-claimed session uses local URL form', () => {
    const claimed = makeSession({
      id: 'sess-l', cwd: '/dev/gmux', adapter: 'pi', slug: 'local',
      project_slug: 'gmux', project_index: 0,
    })
    expect(viewToPath(
      { kind: 'session', sessionId: 'sess-l' },
      projects, [claimed],
    )).toBe('/gmux/pi/local')
  })
})

describe('viewsEqual', () => {
  it('same home views are equal', () => {
    expect(viewsEqual({ kind: 'home' }, { kind: 'home' })).toBe(true)
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
      { kind: 'session', sessionId: 'x' },
    )).toBe(false)
  })
})

describe('View round-trip', () => {
  const projects: ProjectItem[] = [
    { slug: 'gmux', match: [{ remote: 'github.com/gmuxapp/gmux' }, { path: '/dev/gmux' }] },
  ]
  const sessions = [
    makeSession({ id: 'sess-1', cwd: '/dev/gmux', adapter: 'pi', slug: 'fix-auth',
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

  it('bare project path round-trips to home (hubs retired)', () => {
    expect(resolveViewFromPath('/gmux', projects, sessions)).toEqual({ kind: 'home' })
  })
})
