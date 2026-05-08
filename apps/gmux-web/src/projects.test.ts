import { describe, it, expect } from 'vitest'
import type { ProjectItem, PeerInfo } from './types'
import {
  normalizeRemote,
  matchSession,
  buildProjectFolders,
  parseSessionHostPath,
  buildProjectTopology,
  isSessionVisibleInProject,
  slugify,
  slugFromRemote,
  slugFromPath,
  mostCommonRemote,
  discoverProjects,
  countUnmatchedActive,
} from './projects'
import { makeSession } from './test-helpers'

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
    expect(folders[0].peer).toBeUndefined()
  })

  it('sets launchCwd from project paths', () => {
    const projects: ProjectItem[] = [
      { slug: 'proj', match: [{ path: '/dev/proj' }, { path: '/dev/proj2' }] },
    ]
    const folders = buildProjectFolders(projects, [])
    expect(folders[0].launchCwd).toBe('/dev/proj')
  })

  it('hides dead sessions that the origin disclaims (not stamped to this folder)', () => {
    // "old-dead" is dead and disclaimed: nothing claims it. The viewer's
    // rule matches its cwd, but adopted-disclaimed dead sessions are
    // not visible — only the viewer's intent put a rule there, and a
    // dead session can't validate the match.
    const projects: ProjectItem[] = [
      { slug: 'proj', match: [{ path: '/dev/proj' }] },
    ]
    const sessions = [
      makeSession({ id: 'kept', cwd: '/dev/proj', alive: false, resumable: true,
                    project_slug: 'proj', project_index: 0 }),
      makeSession({ id: 'old-dead', cwd: '/dev/proj', alive: false, resumable: true }),
      makeSession({ id: 'alive-1', cwd: '/dev/proj', alive: true }),
    ]
    const folders = buildProjectFolders(projects, sessions)
    const ids = folders[0].sessions.map(s => s.id)
    expect(ids).toContain('alive-1')   // alive always renders
    expect(ids).toContain('kept')      // dead but stamped: claimed, renders
    expect(ids).not.toContain('old-dead') // dead, disclaimed: hidden
  })

  it('hides dead sessions stamped to a different project than the folder', () => {
    const projects: ProjectItem[] = [
      { slug: 'proj', match: [{ path: '/dev/proj' }] },
      { slug: 'other', match: [{ path: '/elsewhere' }] },
    ]
    const sessions = [
      // Dead, stamped to 'other', but its cwd would also rule-match 'proj'.
      // Stamp wins; it lives in 'other', not 'proj', and being dead-and-
      // stamped-elsewhere it doesn't render in 'proj' either.
      makeSession({ id: 'wanderer', cwd: '/dev/proj', alive: false, resumable: true,
                    project_slug: 'other', project_index: 0 }),
    ]
    const folders = buildProjectFolders(projects, sessions)
    const proj = folders.find(f => f.slug === 'proj')!
    const other = folders.find(f => f.slug === 'other')!
    expect(proj.sessions).toHaveLength(0)
    expect(other.sessions.map(s => s.id)).toEqual(['wanderer'])
  })

  it('sorts stamped local sessions by project_index', () => {
    const projects: ProjectItem[] = [
      { slug: 'proj', match: [{ path: '/dev/proj' }] },
    ]
    const sessions = [
      makeSession({ id: 'a', cwd: '/dev/proj', alive: true, project_slug: 'proj', project_index: 1 }),
      makeSession({ id: 'b', cwd: '/dev/proj', alive: true, project_slug: 'proj', project_index: 2 }),
      makeSession({ id: 'c', cwd: '/dev/proj', alive: true, project_slug: 'proj', project_index: 0 }),
    ]
    const folders = buildProjectFolders(projects, sessions)
    expect(folders[0].sessions.map(s => s.id)).toEqual(['c', 'a', 'b'])
  })

  it('puts adopted-disclaimed sessions after stamped ones, ordered by created_at', () => {
    // Adopted-disclaimed: viewer's rule matched, but origin hasn't
    // stamped them yet (just-spawned, Reconcile-pending). They should
    // sit at the end so they don't reshuffle the existing order.
    const projects: ProjectItem[] = [
      { slug: 'proj', match: [{ path: '/dev/proj' }] },
    ]
    const sessions = [
      makeSession({ id: 'unstamped-old', cwd: '/dev/proj', alive: true,
                    created_at: '2025-01-01T00:00:00Z' }),
      makeSession({ id: 'unstamped-new', cwd: '/dev/proj', alive: true,
                    created_at: '2025-01-03T00:00:00Z' }),
      makeSession({ id: 'stamped', cwd: '/dev/proj', alive: true,
                    project_slug: 'proj', project_index: 0,
                    created_at: '2025-01-02T00:00:00Z' }),
    ]
    const folders = buildProjectFolders(projects, sessions)
    expect(folders[0].sessions.map(s => s.id)).toEqual([
      'stamped', 'unstamped-old', 'unstamped-new',
    ])
  })

  // ── Peer-owned folders (ADR 0002) ──────────────────────────────

  it('renders a stamped peer session under its origin (peer, slug) folder', () => {
    const projects: ProjectItem[] = [
      { slug: 'gmux', match: [{ path: '/dev/gmux' }] },
    ]
    const sessions = [
      makeSession({ id: 's1@tower', cwd: '/elsewhere', alive: true,
                    peer: 'tower', project_slug: 'gmux', project_index: 0 }),
    ]
    const folders = buildProjectFolders(projects, sessions)
    // Local 'gmux' folder still emits (empty); peer folder follows.
    const tower = folders.find(f => f.peer === 'tower')!
    expect(tower).toBeDefined()
    expect(tower.slug).toBe('gmux')
    expect(tower.name).toBe('gmux')
    expect(tower.sessions.map(s => s.id)).toEqual(['s1@tower'])
    expect(tower.key).toBe('tower::gmux')
  })

  it('keeps two same-slug projects on different hosts as separate folders', () => {
    const projects: ProjectItem[] = [
      { slug: 'gmux', match: [{ path: '/home/me/dev/gmux' }] },
    ]
    const sessions = [
      makeSession({ id: 'local-1', cwd: '/home/me/dev/gmux', alive: true,
                    project_slug: 'gmux', project_index: 0 }),
      makeSession({ id: 'tower-1@tower', cwd: '/home/me/dev/gmux', alive: true,
                    peer: 'tower', project_slug: 'gmux', project_index: 0 }),
    ]
    const folders = buildProjectFolders(projects, sessions)
    const local = folders.find(f => f.peer === undefined && f.slug === 'gmux')!
    const tower = folders.find(f => f.peer === 'tower')!
    expect(local.sessions.map(s => s.id)).toEqual(['local-1'])
    expect(tower.sessions.map(s => s.id)).toEqual(['tower-1@tower'])
  })

  it('skips empty peer folders (no enumeration on the wire)', () => {
    // No peer sessions visible → no peer folder, even if a peer is
    // connected. Empty peer folders would be a viewer fiction.
    const projects: ProjectItem[] = [
      { slug: 'gmux', match: [{ path: '/dev/gmux' }] },
    ]
    const folders = buildProjectFolders(projects, [])
    expect(folders.filter(f => f.peer !== undefined)).toEqual([])
  })

  it('hides a peer folder once its last visible session goes away', () => {
    const projects: ProjectItem[] = []
    const sessions = [
      // Dead and not resumable: never visible.
      makeSession({ id: 's1@tower', cwd: '/x', alive: false, resumable: false,
                    peer: 'tower', project_slug: 'gmux', project_index: 0 }),
    ]
    const folders = buildProjectFolders(projects, sessions)
    expect(folders.filter(f => f.peer === 'tower')).toEqual([])
  })

  it('sorts peer folders by peer name then slug', () => {
    const projects: ProjectItem[] = []
    const sessions = [
      makeSession({ id: 'a@zulu', cwd: '/x', alive: true,
                    peer: 'zulu', project_slug: 'beta', project_index: 0 }),
      makeSession({ id: 'b@alpha', cwd: '/x', alive: true,
                    peer: 'alpha', project_slug: 'gamma', project_index: 0 }),
      makeSession({ id: 'c@alpha', cwd: '/x', alive: true,
                    peer: 'alpha', project_slug: 'beta', project_index: 0 }),
    ]
    const folders = buildProjectFolders(projects, sessions)
    expect(folders.map(f => `${f.peer}/${f.slug}`)).toEqual([
      'alpha/beta', 'alpha/gamma', 'zulu/beta',
    ])
  })

  it('orders sessions inside a peer folder by project_index', () => {
    const projects: ProjectItem[] = []
    const sessions = [
      makeSession({ id: 'b@tower', cwd: '/x', alive: true,
                    peer: 'tower', project_slug: 'gmux', project_index: 1 }),
      makeSession({ id: 'a@tower', cwd: '/x', alive: true,
                    peer: 'tower', project_slug: 'gmux', project_index: 0 }),
      makeSession({ id: 'c@tower', cwd: '/x', alive: true,
                    peer: 'tower', project_slug: 'gmux', project_index: 2 }),
    ]
    const folders = buildProjectFolders(projects, sessions)
    const tower = folders.find(f => f.peer === 'tower')!
    expect(tower.sessions.map(s => s.id)).toEqual(['a@tower', 'b@tower', 'c@tower'])
  })

  it('adopts a disclaimed peer session into a local folder via match rules', () => {
    // Devcontainer-style: peer's projects.json is empty, so all its
    // sessions disclaim. The viewer's rule for /workspaces/gmux
    // matches the session's cwd; viewer adopts it into local 'gmux'.
    const projects: ProjectItem[] = [
      { slug: 'gmux', match: [{ path: '/workspaces/gmux' }] },
    ]
    const sessions = [
      makeSession({ id: 's1@dev', cwd: '/workspaces/gmux', alive: true,
                    peer: 'dev' }), // no project_slug = disclaimed
    ]
    const folders = buildProjectFolders(projects, sessions)
    const local = folders.find(f => f.slug === 'gmux' && f.peer === undefined)!
    expect(local.sessions.map(s => s.id)).toEqual(['s1@dev'])
    // No separate peer folder for an adopted disclaim.
    expect(folders.filter(f => f.peer === 'dev')).toEqual([])
  })

  it('drops a disclaimed peer session that no viewer rule adopts', () => {
    // ADR 0002: unmatched disclaimed sessions appear via
    // discoverProjects, not in any folder.
    const projects: ProjectItem[] = [
      { slug: 'gmux', match: [{ path: '/dev/gmux' }] },
    ]
    const sessions = [
      makeSession({ id: 's1@dev', cwd: '/elsewhere', alive: true, peer: 'dev' }),
    ]
    const folders = buildProjectFolders(projects, sessions)
    expect(folders.flatMap(f => f.sessions.map(s => s.id))).toEqual([])
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
    // UI path (root -> leaf): ['workstation', 'dev']
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

describe('slugify', () => {
  it('lowercases and replaces non-alnum with hyphens', () => {
    expect(slugify('Hello World')).toBe('hello-world')
  })

  it('collapses repeated hyphens', () => {
    expect(slugify('a---b__c')).toBe('a-b-c')
  })

  it('trims leading and trailing hyphens', () => {
    expect(slugify('--my-thing--')).toBe('my-thing')
  })

  it('returns "project" for empty results', () => {
    expect(slugify('!!!')).toBe('project')
    expect(slugify('')).toBe('project')
  })
})

describe('slugFromRemote', () => {
  it('uses repo basename of normalized URL', () => {
    expect(slugFromRemote('git@github.com:gmuxapp/gmux.git')).toBe('gmux')
    expect(slugFromRemote('https://github.com/Org/My-Repo.git')).toBe('my-repo')
  })
})

describe('slugFromPath', () => {
  it('uses basename', () => {
    expect(slugFromPath('/dev/gmux')).toBe('gmux')
    expect(slugFromPath('~/code/My-Project/')).toBe('my-project')
  })
})

describe('mostCommonRemote', () => {
  it('returns the most frequent normalized remote', () => {
    const sessions = [
      makeSession({ id: 's1', cwd: '/x', remotes: { origin: 'git@github.com:foo/bar.git' } }),
      makeSession({ id: 's2', cwd: '/x', remotes: { origin: 'https://github.com/foo/bar' } }),
      makeSession({ id: 's3', cwd: '/x', remotes: { origin: 'git@github.com:other/repo.git' } }),
    ]
    expect(mostCommonRemote(sessions)).toBe('github.com/foo/bar')
  })

  it('returns empty string when no remotes', () => {
    expect(mostCommonRemote([makeSession({ id: 's1', cwd: '/x' })])).toBe('')
  })

  it('breaks ties lexicographically', () => {
    const sessions = [
      makeSession({ id: 's1', cwd: '/x', remotes: { origin: 'github.com/zzz/repo' } }),
      makeSession({ id: 's2', cwd: '/x', remotes: { origin: 'github.com/aaa/repo' } }),
    ]
    expect(mostCommonRemote(sessions)).toBe('github.com/aaa/repo')
  })
})

describe('discoverProjects', () => {
  it('groups unmatched sessions by directory', () => {
    const projects: ProjectItem[] = [
      { slug: 'gmux', match: [{ path: '/dev/gmux' }] },
    ]
    const sessions = [
      makeSession({ id: 'a', cwd: '/dev/gmux/sub', alive: true }),  // matches gmux
      makeSession({ id: 'b', cwd: '/work/foo', alive: true }),
      makeSession({ id: 'c', cwd: '/work/foo', alive: false }),
      makeSession({ id: 'd', workspace_root: '/work/bar', cwd: '/work/bar/src', alive: true }),
    ]
    const out = discoverProjects(sessions, projects)
    expect(out).toHaveLength(2)
    const fooBucket = out.find(d => d.paths[0] === '/work/foo')
    expect(fooBucket).toMatchObject({ session_count: 2, active_count: 1 })
    const barBucket = out.find(d => d.paths[0] === '/work/bar')
    expect(barBucket).toMatchObject({ session_count: 1, active_count: 1 })
  })

  it('skips sessions with no directory at all', () => {
    expect(discoverProjects([makeSession({ id: 'a', cwd: '' })], [])).toEqual([])
  })

  it('prefers workspace_root over cwd as bucket key', () => {
    const sessions = [
      makeSession({ id: 'a', workspace_root: '/work/repo', cwd: '/work/repo/sub' }),
      makeSession({ id: 'b', workspace_root: '/work/repo', cwd: '/work/repo/other' }),
    ]
    const out = discoverProjects(sessions, [])
    expect(out).toHaveLength(1)
    expect(out[0].paths).toEqual(['/work/repo'])
    expect(out[0].session_count).toBe(2)
  })

  it('uses remote-derived slug when available', () => {
    const sessions = [
      makeSession({ id: 'a', cwd: '/work/foo', remotes: { origin: 'git@github.com:org/cool-repo.git' } }),
    ]
    const out = discoverProjects(sessions, [])
    expect(out[0].suggested_slug).toBe('cool-repo')
    expect(out[0].remote).toBe('github.com/org/cool-repo')
  })

  it('falls back to path-derived slug when no remote', () => {
    const sessions = [makeSession({ id: 'a', cwd: '/work/cool-app' })]
    const out = discoverProjects(sessions, [])
    expect(out[0].suggested_slug).toBe('cool-app')
    expect(out[0].remote).toBeUndefined()
  })

  it('sorts by active count, then session count, then alphabetically', () => {
    const sessions = [
      makeSession({ id: '1', cwd: '/a', alive: false }),
      makeSession({ id: '2', cwd: '/b', alive: true }),
      makeSession({ id: '3', cwd: '/c', alive: true }),
      makeSession({ id: '4', cwd: '/c', alive: false }),
    ]
    const out = discoverProjects(sessions, [])
    expect(out.map(d => d.paths[0])).toEqual(['/c', '/b', '/a'])
  })

  it('excludes claimed sessions (stamped by their origin)', () => {
    // A session claimed on its origin has a definite home; only
    // disclaimed sessions are eligible for discovery.
    const sessions = [
      makeSession({ id: 'claimed', cwd: '/work/foo', alive: true,
                    project_slug: 'foo', project_index: 0 }),
      makeSession({ id: 'free', cwd: '/work/bar', alive: true }),
    ]
    const out = discoverProjects(sessions, [])
    expect(out).toHaveLength(1)
    expect(out[0].paths).toEqual(['/work/bar'])
  })

  it('excludes claimed peer sessions (peer-stamped)', () => {
    const sessions = [
      makeSession({ id: 's1@tower', cwd: '/work/foo', alive: true,
                    peer: 'tower', project_slug: 'gmux', project_index: 0 }),
    ]
    expect(discoverProjects(sessions, [])).toEqual([])
  })
})

describe('countUnmatchedActive', () => {
  it('counts alive sessions outside any project', () => {
    const projects: ProjectItem[] = [
      { slug: 'gmux', match: [{ path: '/dev/gmux' }] },
    ]
    const sessions = [
      makeSession({ id: 'a', cwd: '/dev/gmux/x', alive: true }),  // adopted by viewer rules
      makeSession({ id: 'b', cwd: '/elsewhere', alive: true }),   // free
      makeSession({ id: 'c', cwd: '/elsewhere', alive: false }),  // dead, not counted
      makeSession({ id: 'd', cwd: '/other', alive: true }),       // free
    ]
    expect(countUnmatchedActive(sessions, projects)).toBe(2)
  })

  it('excludes claimed sessions (stamped by their origin)', () => {
    // Stamped sessions have a folder; the badge counts only sessions
    // genuinely outside any project from any host's perspective.
    const projects: ProjectItem[] = [
      { slug: 'gmux', match: [{ path: '/dev/gmux' }] },
    ]
    const sessions = [
      makeSession({ id: 'a', cwd: '/elsewhere', alive: true,
                    project_slug: 'gmux', project_index: 0 }),
    ]
    expect(countUnmatchedActive(sessions, projects)).toBe(0)
  })

  it('excludes claimed peer sessions', () => {
    // A peer's claimed session lives in the peer's folder; not
    // "outside any project" from the viewer's perspective.
    const sessions = [
      makeSession({ id: 's@tower', cwd: '/elsewhere', alive: true,
                    peer: 'tower', project_slug: 'gmux', project_index: 0 }),
    ]
    expect(countUnmatchedActive(sessions, [])).toBe(0)
  })

  it('counts a disclaimed peer session that no viewer rule adopts', () => {
    // Devcontainer disclaim + no viewer rule = genuinely homeless.
    const sessions = [
      makeSession({ id: 's@dev', cwd: '/elsewhere', alive: true, peer: 'dev' }),
    ]
    expect(countUnmatchedActive(sessions, [])).toBe(1)
  })

  it('does not count a disclaimed peer session adopted by a viewer rule', () => {
    const projects: ProjectItem[] = [
      { slug: 'gmux', match: [{ path: '/workspaces/gmux' }] },
    ]
    const sessions = [
      makeSession({ id: 's@dev', cwd: '/workspaces/gmux', alive: true, peer: 'dev' }),
    ]
    expect(countUnmatchedActive(sessions, projects)).toBe(0)
  })

  it('returns 0 when there are no sessions', () => {
    expect(countUnmatchedActive([], [])).toBe(0)
  })
})
