import { describe, expect, it } from 'vitest'
import {
  hasSessionSlugCollision,
  parseSessionPath,
  resolveSessionFromPath,
  resolveViewFromPath,
  sessionPath,
  viewsEqual,
  viewToPath,
} from './routing'
import { makeSession } from './test-helpers'
import type { ProjectItem } from './types'

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

  it('falls back to the sigiled full session id when slug missing', () => {
    expect(sessionPath('gmux', { adapter: 'pi', id: '10vjvid0' }))
      .toBe('/gmux/pi/~10vjvid0')
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
      { id: '1vshk4fu@tower', adapter: 'pi', slug: 'fix-auth', peer: 'tower' },
      'tower',
    )).toBe('/@tower/gmux/pi/fix-auth')
  })

  it('local project + adopted peer session: keeps mid-path @host', () => {
    // Disclaimed peer session adopted into a local folder. The project
    // owner is local (no leading @), but the session lives on a peer
    // (mid-path @<host> needed to disambiguate).
    expect(sessionPath(
      'gmux',
      { id: '1vshk4fu@dev', adapter: 'pi', slug: 'fix-auth', peer: 'dev' },
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
    makeSession({ id: '1vshk4fu', cwd: '/dev/gmux', adapter: 'pi', slug: 'fix-auth',
      remotes: { origin: 'github.com/gmuxapp/gmux' } }),
    makeSession({ id: '155mk8b7', cwd: '/dev/gmux', adapter: 'shell', slug: 'fish',
      remotes: { origin: 'github.com/gmuxapp/gmux' } }),
  ]

  it('resolves full path to session ID', () => {
    const id = resolveSessionFromPath(
      { project: 'gmux', adapter: 'pi', slug: 'fix-auth' }, projects, localSessions,
    )
    expect(id).toBe('1vshk4fu')
  })

  it('refuses an ambiguous exact slug and links colliding rows by full ID', () => {
    const duplicate = makeSession({ id: '1eha7rdu', cwd: '/dev/gmux', adapter: 'pi', slug: 'fix-auth', alive: false,
      remotes: { origin: 'github.com/gmuxapp/gmux' } })
    const colliding = [localSessions[0], duplicate]
    expect(resolveSessionFromPath(
      { project: 'gmux', adapter: 'pi', slug: 'fix-auth' }, projects, colliding,
    )).toBeNull()
    expect(hasSessionSlugCollision(localSessions[0], colliding, projects)).toBe(true)
    expect(sessionPath('gmux', localSessions[0], undefined, true)).toBe('/gmux/pi/~1vshk4fu')
    expect(sessionPath('gmux', duplicate, undefined, true)).toBe('/gmux/pi/~1eha7rdu')
  })

  it('resolves project-only to first alive session', () => {
    const id = resolveSessionFromPath({ project: 'gmux' }, projects, localSessions)
    expect(id).toBe('1vshk4fu')
  })

  it('returns null for unknown project', () => {
    const id = resolveSessionFromPath({ project: 'nope' }, projects, localSessions)
    expect(id).toBeNull()
  })

  // Peer-aware resolution
  const mixedSessions = [
    ...localSessions,
    makeSession({ id: '10z0uxe6@server', cwd: '/dev/gmux', adapter: 'pi', slug: 'fix-auth',
      peer: 'server', remotes: { origin: 'github.com/gmuxapp/gmux' } }),
    makeSession({ id: '1b4k46rv@server', cwd: '/dev/gmux', adapter: 'shell', slug: 'bash',
      peer: 'server', remotes: { origin: 'github.com/gmuxapp/gmux' } }),
  ]

  it('resolves remote session with @host in URL', () => {
    const id = resolveSessionFromPath(
      { project: 'gmux', host: 'server', adapter: 'pi', slug: 'fix-auth' },
      projects, mixedSessions,
    )
    expect(id).toBe('10z0uxe6@server')
  })

  it('local path resolves to local session, not remote', () => {
    const id = resolveSessionFromPath(
      { project: 'gmux', adapter: 'pi', slug: 'fix-auth' },
      projects, mixedSessions,
    )
    expect(id).toBe('1vshk4fu')
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
    expect(id).toBe('10z0uxe6@server')
  })

  it('separates exact ID and slug forms', () => {
    const crossCollision = [
      makeSession({ id: '11q9rc89', cwd: '/dev/gmux', adapter: 'pi' }),
      makeSession({ id: '1agkzudt', cwd: '/dev/gmux', adapter: 'pi', slug: '11q9rc89' }),
    ]
    expect(resolveSessionFromPath(
      { project: 'gmux', adapter: 'pi', slug: '11q9rc89' }, projects, crossCollision,
    )).toBe('1agkzudt')
    expect(resolveSessionFromPath(
      { project: 'gmux', adapter: 'pi', slug: '~11q9rc89' }, projects, crossCollision,
    )).toBe('11q9rc89')
  })

  it('does not flag equal slugs in different project route namespaces', () => {
    const scopedProjects: ProjectItem[] = [
      { slug: 'one', match: [{ path: '/one' }] },
      { slug: 'two', match: [{ path: '/two' }] },
    ]
    const scoped = [
      makeSession({ id: '1wqhbfge', cwd: '/one', adapter: 'pi', slug: 'same', project_slug: 'one' }),
      makeSession({ id: '16fnwicq', cwd: '/two', adapter: 'pi', slug: 'same', project_slug: 'two' }),
    ]
    expect(hasSessionSlugCollision(scoped[0], scoped, scopedProjects)).toBe(false)
  })

  it('refuses an ambiguous ID prefix', () => {
    const unattributed = [
      makeSession({ id: '1xe4xm7r', cwd: '/dev/gmux', adapter: 'pi' }),
      makeSession({ id: '1pqq8owv', cwd: '/dev/gmux', adapter: 'pi' }),
    ]
    expect(resolveSessionFromPath(
      { project: 'gmux', adapter: 'pi', slug: '1j6y9mx6' }, projects, unattributed,
    )).toBeNull()
  })

  it('resolves only the exact sigiled ID when session has no slug', () => {
    const unattributed = [
      makeSession({ id: '1284kpxv', cwd: '/dev/gmux', adapter: 'pi',
        remotes: { origin: 'github.com/gmuxapp/gmux' } }),
    ]
    expect(resolveSessionFromPath(
      { project: 'gmux', adapter: 'pi', slug: '~1284' }, projects, unattributed,
    )).toBeNull()
    expect(resolveSessionFromPath(
      { project: 'gmux', adapter: 'pi', slug: '~1284kpxv' }, projects, unattributed,
    )).toBe('1284kpxv')
  })

  it('resolves a peer-owned project URL via stamps, not viewer rules', () => {
    // Peer 'tower' has its own 'gmux' project. The viewer also has
    // a 'gmux' project, but the URL `/@tower/gmux/...` addresses the
    // peer-owned one; we must trust the stamp, not re-run match.
    const claimed = makeSession({
      id: '1tzbf6vy@tower', cwd: '/elsewhere', adapter: 'pi', slug: 'fix-auth',
      peer: 'tower', project_slug: 'gmux', project_index: 0,
    })
    const id = resolveSessionFromPath(
      { projectPeer: 'tower', project: 'gmux', adapter: 'pi', slug: 'fix-auth' },
      projects, [claimed],
    )
    expect(id).toBe('1tzbf6vy@tower')
  })

  it('peer-owned project URL ignores local-stamped same-slug sessions', () => {
    const localGmux = makeSession({
      id: '1cmac7wo', cwd: '/dev/gmux', adapter: 'pi', slug: 'fix-auth',
      project_slug: 'gmux', project_index: 0,
    })
    const towerGmux = makeSession({
      id: '1k9m861l@tower', cwd: '/elsewhere', adapter: 'pi', slug: 'fix-auth',
      peer: 'tower', project_slug: 'gmux', project_index: 0,
    })
    const id = resolveSessionFromPath(
      { projectPeer: 'tower', project: 'gmux', adapter: 'pi', slug: 'fix-auth' },
      projects, [localGmux, towerGmux],
    )
    expect(id).toBe('1k9m861l@tower')
  })
})

describe('task-family project routing', () => {
  const projects: ProjectItem[] = [{ slug: 'root-project', match: [{ path: '/root' }] }, { slug: 'child-project', match: [{ path: '/child' }] }]
  const root = makeSession({ id: 'root', cwd: '/root', adapter: 'pi', slug: 'root', project_slug: 'root-project', semantic_agent: true })
  const child = makeSession({ id: 'child', cwd: '/child', adapter: 'pi', slug: 'child', parent_session_id: 'root', semantic_agent: true, project_slug: 'child-project' })

  it('serializes and resolves an unpromoted child in its root project', () => {
    expect(viewToPath({ kind: 'session', sessionId: 'child' }, projects, [root, child])).toBe('/root-project/pi/child')
    expect(resolveViewFromPath('/root-project/pi/child', projects, [root, child])).toEqual({ kind: 'session', sessionId: 'child' })
  })

  it('uses normal matching without root fallback after promotion', () => {
    const promoted = { ...child, parent_session_id: undefined }
    expect(viewToPath({ kind: 'session', sessionId: 'child' }, projects, [root, promoted])).toBe('/child-project/pi/child')
    expect(resolveViewFromPath('/root-project/pi/child', projects, [root, promoted])).toEqual({ kind: 'home' })
  })

  it('detects slug collisions in the root project namespace', () => {
    const sibling = { ...child, id: 'sibling', project_slug: 'elsewhere' }
    expect(hasSessionSlugCollision(child, [root, child, sibling], projects)).toBe(true)
    expect(viewToPath({ kind: 'session', sessionId: 'child' }, projects, [root, child, sibling])).toBe('/root-project/pi/~child')
  })

  it('round-trips peer-owner and local children that serialize without a host segment', () => {
    const peerProjects: ProjectItem[] = [{ slug: 'peer-project', peer: 'tower' }]
    const peerRoot = makeSession({ id: 'root@tower', peer: 'tower', cwd: '/root', adapter: 'pi', slug: 'root', project_slug: 'peer-project', semantic_agent: true })
    const ownerChild = makeSession({ id: 'owner@tower', peer: 'tower', cwd: '/owner', adapter: 'pi', slug: 'same', parent_session_id: peerRoot.id, semantic_agent: true })
    const localChild = makeSession({ id: 'local', cwd: '/local', adapter: 'pi', slug: 'same', parent_session_id: peerRoot.id, semantic_agent: true })
    const snapshot = [peerRoot, ownerChild, localChild]

    for (const [session, path] of [
      [ownerChild, '/@tower/peer-project/pi/~owner@tower'],
      [localChild, '/@tower/peer-project/pi/~local'],
    ] as const) {
      expect(hasSessionSlugCollision(session, snapshot, peerProjects)).toBe(true)
      expect(viewToPath({ kind: 'session', sessionId: session.id }, peerProjects, snapshot)).toBe(path)
      expect(resolveViewFromPath(path, peerProjects, snapshot)).toEqual({ kind: 'session', sessionId: session.id })
    }
  })
})

describe('resolveViewFromPath', () => {
  const projects: ProjectItem[] = [
    { slug: 'gmux', match: [{ remote: 'github.com/gmuxapp/gmux' }, { path: '/dev/gmux' }] },
  ]
  const sessions = [
    makeSession({ id: '1vshk4fu', cwd: '/dev/gmux', adapter: 'pi', slug: 'fix-auth',
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
      id: '1k9m861l@tower', cwd: '/elsewhere', adapter: 'pi', slug: 'fix-auth',
      peer: 'tower', project_slug: 'gmux', project_index: 0,
    })
    expect(resolveViewFromPath('/@tower/gmux', projects, [peerSession])).toEqual({ kind: 'home' })
  })

  it('unknown project resolves to home', () => {
    expect(resolveViewFromPath('/unknown', projects, sessions)).toEqual({ kind: 'home' })
  })

  it('full session path resolves to session view', () => {
    expect(resolveViewFromPath('/gmux/pi/fix-auth', projects, sessions)).toEqual({
      kind: 'session', sessionId: '1vshk4fu',
    })
  })

  it('session path with missing session falls back to home', () => {
    expect(resolveViewFromPath('/gmux/pi/no-such-session', projects, sessions)).toEqual({ kind: 'home' })
  })

  it('remote session URL resolves to session view', () => {
    const remoteSess = makeSession({
      id: '1mfmu6xt@server', cwd: '/dev/gmux', adapter: 'shell', slug: 'bash',
      peer: 'server', remotes: { origin: 'github.com/gmuxapp/gmux' },
    })
    expect(resolveViewFromPath('/gmux/@server/shell/bash', projects, [...sessions, remoteSess])).toEqual({
      kind: 'session', sessionId: '1mfmu6xt@server',
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
    makeSession({ id: '1vshk4fu', cwd: '/dev/gmux', adapter: 'pi', slug: 'fix-auth',
      remotes: { origin: 'github.com/gmuxapp/gmux' } }),
    makeSession({ id: '155mk8b7@server', cwd: '/dev/gmux', adapter: 'shell', slug: 'bash',
      peer: 'server', remotes: { origin: 'github.com/gmuxapp/gmux' } }),
  ]

  it('home view -> /', () => {
    expect(viewToPath({ kind: 'home' }, projects, sessions)).toBe('/')
  })

  it('session view -> full session path', () => {
    expect(viewToPath({ kind: 'session', sessionId: '1vshk4fu' }, projects, sessions))
      .toBe('/gmux/pi/fix-auth')
  })

  it('session view with peer -> path includes @host', () => {
    expect(viewToPath({ kind: 'session', sessionId: '155mk8b7@server' }, projects, sessions))
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
      id: '10cel6cx@tower', cwd: '/dev/gmux', adapter: 'pi', slug: 'on-tower',
      peer: 'tower', project_slug: 'gmux', project_index: 0,
    })
    expect(viewToPath(
      { kind: 'session', sessionId: '10cel6cx@tower' },
      projects, [claimed],
    )).toBe('/@tower/gmux/pi/on-tower')
  })

  it('local-claimed session uses local URL form', () => {
    const claimed = makeSession({
      id: '1c5az2og', cwd: '/dev/gmux', adapter: 'pi', slug: 'local',
      project_slug: 'gmux', project_index: 0,
    })
    expect(viewToPath(
      { kind: 'session', sessionId: '1c5az2og' },
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
    makeSession({ id: '1vshk4fu', cwd: '/dev/gmux', adapter: 'pi', slug: 'fix-auth',
      remotes: { origin: 'github.com/gmuxapp/gmux' } }),
  ]

  it('home view round-trips', () => {
    const path = viewToPath({ kind: 'home' }, projects, sessions)
    expect(path).toBe('/')
    expect(resolveViewFromPath(path!, projects, sessions)).toEqual({ kind: 'home' })
  })

  it('session view round-trips', () => {
    const path = viewToPath({ kind: 'session', sessionId: '1vshk4fu' }, projects, sessions)
    expect(path).toBe('/gmux/pi/fix-auth')
    expect(resolveViewFromPath(path!, projects, sessions)).toEqual({
      kind: 'session', sessionId: '1vshk4fu',
    })
  })

  it('bare project path round-trips to home (hubs retired)', () => {
    expect(resolveViewFromPath('/gmux', projects, sessions)).toEqual({ kind: 'home' })
  })
})
