import { describe, it, expect } from 'vitest'
import type { ProjectItem, PeerInfo } from './types'
import {
  normalizeRemote,
  matchSession,
  buildProjectFolders,
  parseSessionHostPath,
  reorderKeysForFolder,
  buildProjectTopology,
  isSessionVisibleInProject,
  slugify,
  slugFromRemote,
  slugFromPath,
  mostCommonRemote,
  discoverProjects,
  countUnmatchedActive,
  projectAvailability,
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

  it('drops disclaimed sessions even when their cwd matches a local rule', () => {
    // Under the references model, viewer match rules don't adopt
    // sessions client-side. Only stamps put a session in a folder.
    // A disclaimed session waiting on AutoAssign is invisible until
    // the daemon stamps it.
    const projects: ProjectItem[] = [
      { slug: 'proj', match: [{ path: '/dev/proj' }] },
    ]
    const sessions = [
      makeSession({ id: 'kept', cwd: '/dev/proj', alive: false, resumable: true,
                    project_slug: 'proj', project_index: 0 }),
      makeSession({ id: 'old-dead', cwd: '/dev/proj', alive: false, resumable: true }),
      makeSession({ id: 'disclaimed-alive', cwd: '/dev/proj', alive: true }),
    ]
    const folders = buildProjectFolders(projects, sessions)
    expect(folders[0].sessions.map(s => s.id)).toEqual(['kept'])
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

  // ── References to peer-owned projects ──────────────────────────

  it('renders a reference folder filled by the peer\'s stamped sessions', () => {
    const projects: ProjectItem[] = [
      { slug: 'gmux', peer: 'tower' },
    ]
    const sessions = [
      makeSession({ id: 's1@tower', cwd: '/elsewhere', alive: true,
                    peer: 'tower', project_slug: 'gmux', project_index: 0 }),
    ]
    const folders = buildProjectFolders(projects, sessions)
    expect(folders).toHaveLength(1)
    expect(folders[0].peer).toBe('tower')
    expect(folders[0].slug).toBe('gmux')
    expect(folders[0].key).toBe('tower::gmux')
    expect(folders[0].sessions.map(s => s.id)).toEqual(['s1@tower'])
  })

  it('renders an empty referenced folder when the peer has no sessions', () => {
    // References are first-class: empty folders still render so the
    // user knows the reference is configured.
    const projects: ProjectItem[] = [
      { slug: 'gmux', peer: 'tower' },
    ]
    const folders = buildProjectFolders(projects, [])
    expect(folders).toHaveLength(1)
    expect(folders[0].peer).toBe('tower')
    expect(folders[0].sessions).toEqual([])
  })

  it('derives launchCwd for a reference from peer_projects', () => {
    // Empty referenced folder still needs a sensible cwd for the
    // launch button: pull it from peer_projects so the launcher
    // doesn't fall back to the peer's $HOME.
    const projects: ProjectItem[] = [
      { slug: 'gmux', peer: 'tower' },
    ]
    const peerProjects = {
      tower: [{ slug: 'gmux', launch_cwd: '/home/me/dev/gmux' }],
    }
    const folders = buildProjectFolders(projects, [], undefined, peerProjects)
    expect(folders[0].launchCwd).toBe('/home/me/dev/gmux')
  })

  it('flags a reference as missing when the peer no longer enumerates the slug', () => {
    // Peer is connected (we have a peer_projects entry for it) but
    // doesn't list the slug we're referencing: project was removed
    // upstream. Mark the folder so the UI renders a question-mark
    // badge prompting the user to remove the reference.
    const projects: ProjectItem[] = [
      { slug: 'gmux', peer: 'tower' },
    ]
    const peerProjects = {
      tower: [{ slug: 'something-else' }], // peer reachable, slug gone
    }
    const folders = buildProjectFolders(projects, [], undefined, peerProjects)
    expect(folders[0].missing).toBe(true)
  })

  it('does not flag a reference as missing when the peer is disconnected', () => {
    // No peer_projects entry for a peer means we have no enumeration
    // (disconnected or not yet fetched). Distinguishing missing from
    // disconnected matters for the UI: disconnected uses the dimmed
    // PeerLabel, missing uses the ? badge. Conflating them would
    // nag users when their laptop goes to sleep.
    const projects: ProjectItem[] = [
      { slug: 'gmux', peer: 'tower' },
    ]
    const folders = buildProjectFolders(projects, [], undefined, {})
    expect(folders[0].missing).toBeUndefined()
  })

  it('buckets a reference\'s sessions by its stored name and stays resolved when present', () => {
    // `peer` is the runtime key (ADR 0015): sessions stamped with the
    // same name land in the folder, and the liveness predicate (node_id
    // present) keeps it resolved.
    const projects: ProjectItem[] = [{ slug: 'apps', peer: 'gmux-hs', node_id: 'node_hs' }]
    const sessions = [
      makeSession({ id: 's1', cwd: '/mnt/apps', peer: 'gmux-hs', project_slug: 'apps', alive: true }),
    ]
    const isPresent = (_peer: string, nodeId?: string) => nodeId === 'node_hs'
    const folders = buildProjectFolders(projects, sessions, undefined, {}, isPresent)
    expect(folders).toHaveLength(1)
    expect(folders[0].peer).toBe('gmux-hs')
    expect(folders[0].unresolved).toBeUndefined()
    expect(folders[0].sessions.map(s => s.id)).toEqual(['s1'])
  })

  it('flags an unresolved reference and takes precedence over missing', () => {
    // The reference's anchor (node_id) is in no roster bucket — host
    // renamed-away-and-gone or removed. Even with no peer_projects entry,
    // the folder is flagged unresolved rather than left a plain empty
    // reference — and unresolved is never conflated with missing.
    const projects: ProjectItem[] = [{ slug: 'apps', peer: 'hs', node_id: 'node_gone' }]
    const isPresent = () => false
    const folders = buildProjectFolders(projects, [], undefined, {}, isPresent)
    expect(folders[0].unresolved).toBe(true)
    expect(folders[0].missing).toBeUndefined()
    expect(folders[0].peer).toBe('hs')
  })

  it('keeps a local owned project and a same-slug reference as separate folders', () => {
    const projects: ProjectItem[] = [
      { slug: 'gmux', match: [{ path: '/dev/gmux' }] },
      { slug: 'gmux', peer: 'tower' },
    ]
    const sessions = [
      makeSession({ id: 'local-1', cwd: '/dev/gmux', alive: true,
                    project_slug: 'gmux', project_index: 0 }),
      makeSession({ id: 'tower-1@tower', cwd: '/x', alive: true,
                    peer: 'tower', project_slug: 'gmux', project_index: 0 }),
    ]
    const folders = buildProjectFolders(projects, sessions)
    expect(folders).toHaveLength(2)
    expect(folders[0].peer).toBeUndefined()
    expect(folders[0].sessions.map(s => s.id)).toEqual(['local-1'])
    expect(folders[1].peer).toBe('tower')
    expect(folders[1].sessions.map(s => s.id)).toEqual(['tower-1@tower'])
  })

  it('orders sessions in a referenced folder by project_index', () => {
    const projects: ProjectItem[] = [
      { slug: 'gmux', peer: 'tower' },
    ]
    const sessions = [
      makeSession({ id: 'b@tower', cwd: '/x', alive: true,
                    peer: 'tower', project_slug: 'gmux', project_index: 1 }),
      makeSession({ id: 'a@tower', cwd: '/x', alive: true,
                    peer: 'tower', project_slug: 'gmux', project_index: 0 }),
      makeSession({ id: 'c@tower', cwd: '/x', alive: true,
                    peer: 'tower', project_slug: 'gmux', project_index: 2 }),
    ]
    const folders = buildProjectFolders(projects, sessions)
    expect(folders[0].sessions.map(s => s.id)).toEqual(['a@tower', 'b@tower', 'c@tower'])
  })

  it('non-Local peer sessions never land in a locally-owned folder', () => {
    // Pinned for the sidebar mixed-host marker (sidebar.tsx,
    // DevcontainerMarker): inside a folder with peer===undefined,
    // any session whose .peer is set must be a Local peer
    // (devcontainer). The marker leans on this invariant to render
    // a container icon unconditionally rather than re-classifying
    // peers at render time. If bucketing ever folds non-Local peer
    // sessions into a local folder, this test fires and the marker
    // needs to be revisited.
    const projects: ProjectItem[] = [
      { slug: 'gmux', match: [{ path: '/dev/gmux' }] },
    ]
    const sessions = [
      makeSession({ id: 's-local', cwd: '/dev/gmux', alive: true,
                    project_slug: 'gmux', project_index: 0 }),
      makeSession({ id: 's-tower@tower', cwd: '/x', alive: true,
                    peer: 'tower', project_slug: 'gmux', project_index: 1 }),
    ]
    // Crucial: 'tower' is *not* a Local peer. The hint must be
    // honest about this; an incorrect Local claim would land the
    // session here legitimately.
    const isLocal = (name: string) => name !== 'tower'
    const folders = buildProjectFolders(projects, sessions, isLocal)
    expect(folders).toHaveLength(1)
    expect(folders[0].peer).toBeUndefined()
    expect(folders[0].sessions.map(s => s.id)).toEqual(['s-local'])
  })

  it('ignores stamped peer sessions when no reference exists for them', () => {
    // Under the references model, peer-stamped sessions only appear
    // if the viewer added a reference. No emergent peer folders.
    const projects: ProjectItem[] = []
    const sessions = [
      makeSession({ id: 's1@tower', cwd: '/x', alive: true,
                    peer: 'tower', project_slug: 'gmux', project_index: 0 }),
    ]
    const folders = buildProjectFolders(projects, sessions)
    expect(folders).toEqual([])
  })

  it('routes Local-peer sessions into a local folder (parent owns assignment)', () => {
    // Devcontainer case: the parent's match rules stamp container
    // sessions; they should bucket into the parent's local folder,
    // not a peer folder. The session row still shows the peer chip.
    const projects: ProjectItem[] = [
      { slug: 'gmux', match: [{ path: '/workspaces/gmux' }] },
    ]
    const sessions = [
      makeSession({ id: 's1@dev', cwd: '/workspaces/gmux', alive: true,
                    peer: 'dev', project_slug: 'gmux', project_index: 0 }),
    ]
    const isLocal = (name: string) => name === 'dev'
    const folders = buildProjectFolders(projects, sessions, isLocal)
    expect(folders).toHaveLength(1)
    expect(folders[0].peer).toBeUndefined()
    expect(folders[0].sessions.map(s => s.id)).toEqual(['s1@dev'])
  })

  it('without the local-peer hint, a peer-stamped session misses a non-referenced folder', () => {
    // Routing depends on the caller telling us which peers are Local.
    // Without that, a Local-peer session looks like a regular peer
    // session and falls through (would need a reference to land).
    const projects: ProjectItem[] = [
      { slug: 'gmux', match: [{ path: '/workspaces/gmux' }] },
    ]
    const sessions = [
      makeSession({ id: 's1@dev', cwd: '/workspaces/gmux', alive: true,
                    peer: 'dev', project_slug: 'gmux', project_index: 0 }),
    ]
    const folders = buildProjectFolders(projects, sessions)
    expect(folders[0].sessions).toEqual([])
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

  it('dead resumable sessions are visible (stamps already gate folder membership)', () => {
    // Under the references model, buildProjectFolders buckets only
    // stamped sessions, so by the time isSessionVisibleInProject
    // runs, the session already belongs in this folder. The check
    // collapses to alive-or-resumable.
    const s = makeSession({ id: 'sess-x', cwd: '/dev/test', alive: false, resumable: true, slug: 'my-session' })
    expect(isSessionVisibleInProject(s, project)).toBe(true)
  })

  it('ignores project.sessions[] tracking', () => {
    // The tracked-set check was a holdover from cross-host adoption,
    // where some sessions arrived in a folder without a stamp. With
    // stamps as the sole authority, the helper no longer reads it.
    const s = makeSession({ id: 'sess-orphan', cwd: '/dev/test', alive: false, resumable: true, slug: 'orphan' })
    expect(isSessionVisibleInProject(s, project)).toBe(true)
  })

  it('handles reference projects (no match[] or sessions[])', () => {
    const ref: ProjectItem = { slug: 'gmux', peer: 'tower' }
    const alive = makeSession({ id: 's', cwd: '/x', alive: true })
    const resumable = makeSession({ id: 'r', cwd: '/x', alive: false, resumable: true })
    const dead = makeSession({ id: 'd', cwd: '/x', alive: false, resumable: false })
    expect(isSessionVisibleInProject(alive, ref)).toBe(true)
    expect(isSessionVisibleInProject(resumable, ref)).toBe(true)
    expect(isSessionVisibleInProject(dead, ref)).toBe(false)
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

describe('reorderKeysForFolder', () => {
  // Greptile flagged that the sidebar's drag-end was sending
  // `s.slug || s.id` to the peer reorder endpoint. For slugless
  // peer-owned sessions that evaluates to the hub-namespaced id
  // (`"orig@peer"`), which the peer's ReorderSessions treats as a
  // new entry and prepends to projects.json. Each drag would
  // append a phantom key. These cases pin the corruption-prevention
  // contract.

  it('peer folder: strips @<peer> namespace from slugless ids', () => {
    const sessions = [
      makeSession({ id: 'a@tower', cwd: '/x', slug: '', peer: 'tower' }),
      makeSession({ id: 'b@tower', cwd: '/x', slug: '', peer: 'tower' }),
    ]
    // Without stripping, the peer's projects.json would gain phantom
    // "a@tower" / "b@tower" entries while its real "a" / "b" keys
    // are pushed to the tail.
    expect(reorderKeysForFolder(sessions, 'tower')).toEqual(['a', 'b'])
  })

  it('peer folder: keeps slug as-is for slugged sessions', () => {
    const sessions = [
      makeSession({ id: 'a@tower', cwd: '/x', slug: 'fix-auth', peer: 'tower' }),
      makeSession({ id: 'b@tower', cwd: '/x', slug: 'login-page', peer: 'tower' }),
    ]
    expect(reorderKeysForFolder(sessions, 'tower')).toEqual(['fix-auth', 'login-page'])
  })

  it('local folder: drops adopted peer-owned sessions', () => {
    // A local folder showing a peer session adopted via match rules:
    // sending it to the local /v1/projects PATCH would write the
    // namespaced id into the viewer's own projects.json, polluting
    // future Reconcile passes.
    const sessions = [
      makeSession({ id: 'local-1', cwd: '/x', slug: 'fix-auth' }),
      makeSession({ id: 'adopted@spoke', cwd: '/x', slug: '', peer: 'spoke' }),
      makeSession({ id: 'local-2', cwd: '/x', slug: '' }),
    ]
    expect(reorderKeysForFolder(sessions, undefined))
      .toEqual(['fix-auth', 'local-2'])
  })

  it('returns empty when no session matches the folder owner', () => {
    // Caller short-circuits on empty: the daemon shouldn't see a
    // request that boils down to "reorder nothing".
    const sessions = [
      makeSession({ id: 'a@spoke', cwd: '/x', slug: '', peer: 'spoke' }),
    ]
    expect(reorderKeysForFolder(sessions, 'tower')).toEqual([])
  })

  it('local folder + Local peer: keeps namespaced id (parent owns assignment)', () => {
    // Container sessions bucket into local folders. The parent's
    // projects.json keys them by full namespaced id (their identity
    // from the parent's POV); the reorder PATCH must preserve that
    // shape, not strip @<peer>, or the merge logic will treat the
    // session as new and prepend it.
    const sessions = [
      makeSession({ id: 'sess-1', cwd: '/x', slug: '' }),
      makeSession({ id: 'sess-2@container', cwd: '/x', slug: '', peer: 'container' }),
    ]
    const isLocal = (n: string) => n === 'container'
    expect(reorderKeysForFolder(sessions, undefined, isLocal))
      .toEqual(['sess-1', 'sess-2@container'])
  })

  it('local folder + Local peer: slug takes precedence over namespaced id', () => {
    // A container session that's been attribution-resolved (e.g.
    // claude/codex) keys by slug, not id, in projects.json.
    const sessions = [
      makeSession({ id: 'sess-1@container', cwd: '/x', slug: 'claude-fix', peer: 'container' }),
    ]
    const isLocal = (n: string) => n === 'container'
    expect(reorderKeysForFolder(sessions, undefined, isLocal))
      .toEqual(['claude-fix'])
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
      makeSession({ id: 'sess-1', cwd: '/home/mg/dev/fluxer', created_at: '2026-01-01T00:00:02Z',
                    project_slug: 'fluxer', project_index: 0 }),
      makeSession({ id: 'sess-2', cwd: '/home/mg/dev/fluxer', created_at: '2026-01-01T00:00:01Z',
                    project_slug: 'fluxer', project_index: 1 }),
      makeSession({ id: 'sess-3', cwd: '/home/mg/dev/fluxer/api',
                    project_slug: 'fluxer', project_index: 2 }),
    ]
    const hosts = buildProjectTopology('fluxer', sessions, projects, peers)
    expect(hosts).toHaveLength(1)
    expect(hosts[0].path).toEqual([])
    expect(hosts[0].status).toBe('local')
    expect(hosts[0].folders).toHaveLength(2)
    expect(hosts[0].folders[0].cwd).toBe('/home/mg/dev/fluxer')
    expect(hosts[0].folders[1].cwd).toBe('/home/mg/dev/fluxer/api')
  })

  it('collapses Local-peer (devcontainer) sessions into the local host node', () => {
    // Under the references model, Local-peer sessions are owned by
    // the parent: their stamps come from the parent's match rules
    // and they live in the parent's local folder. The hub shows
    // them under the local host node, with their session id chain
    // still indicating the container hop in the folder grouping.
    const sessions = [
      makeSession({ id: 's-local', cwd: '/home/mg/dev/fluxer',
                    project_slug: 'fluxer', project_index: 0 }),
      makeSession({ id: 's-container@dev', cwd: '/workspaces/fluxer', peer: 'dev',
                    project_slug: 'fluxer', project_index: 1 }),
    ]
    const isLocal = (name: string) => name === 'dev'
    const hosts = buildProjectTopology('fluxer', sessions, projects, peers, undefined, isLocal)
    expect(hosts).toHaveLength(1)
    expect(hosts[0].path).toEqual([])
    // Both sessions land under local; their cwds differ so they
    // become separate folder groups.
    const allIds = hosts[0].folders.flatMap(f => f.sessions.map(s => s.id))
    expect(allIds.sort()).toEqual(['s-container@dev', 's-local'].sort())
  })

  it('reference hub: shows only the peer\'s stamped sessions', () => {
    const refProjects: ProjectItem[] = [
      { slug: 'fluxer', peer: 'workstation' },
    ]
    const sessions = [
      makeSession({ id: 'sess-local', cwd: '/home/mg/dev/fluxer',
                    project_slug: 'fluxer', project_index: 0 }),
      makeSession({ id: 'sess-remote@workstation', cwd: '/home/mg/dev/fluxer',
                    peer: 'workstation',
                    project_slug: 'fluxer', project_index: 0 }),
    ]
    const hosts = buildProjectTopology('fluxer', sessions, refProjects, peers, 'workstation')
    expect(hosts).toHaveLength(1)
    expect(hosts[0].path).toEqual(['workstation'])
    expect(hosts[0].status).toBe('connected')
    expect(hosts[0].meta).toBe('http://100.64.0.2:8790')
    expect(hosts[0].folders[0].sessions.map(s => s.id)).toEqual(['sess-remote@workstation'])
  })

  it('nested peers form multi-segment paths', () => {
    const refProjects: ProjectItem[] = [
      { slug: 'fluxer', peer: 'workstation' },
    ]
    const sessions = [
      makeSession({ id: 'sess-nested@dev@workstation', cwd: '/workspace/fluxer',
                    peer: 'workstation',
                    project_slug: 'fluxer', project_index: 0 }),
    ]
    const hosts = buildProjectTopology('fluxer', sessions, refProjects, peers, 'workstation')
    expect(hosts).toHaveLength(1)
    expect(hosts[0].path).toEqual(['workstation', 'dev'])
    // Nested inherits root peer status.
    expect(hosts[0].status).toBe('connected')
  })

  it('marks unknown peers as disconnected', () => {
    const refProjects: ProjectItem[] = [
      { slug: 'fluxer', peer: 'ghost' },
    ]
    const sessions = [
      makeSession({ id: 'sess-a@ghost', cwd: '/home/mg/dev/fluxer', peer: 'ghost',
                    project_slug: 'fluxer', project_index: 0 }),
    ]
    const hosts = buildProjectTopology('fluxer', sessions, refProjects, peers, 'ghost')
    expect(hosts).toHaveLength(1)
    expect(hosts[0].status).toBe('disconnected')
    expect(hosts[0].meta).toBe('')
  })

  it('reflects peer status from the peers list', () => {
    const refProjects: ProjectItem[] = [
      { slug: 'fluxer', peer: 'offline-box' },
    ]
    const sessions = [
      makeSession({ id: 'sess-o@offline-box', cwd: '/home/mg/dev/fluxer', peer: 'offline-box',
                    project_slug: 'fluxer', project_index: 0 }),
    ]
    const hosts = buildProjectTopology('fluxer', sessions, refProjects, peers, 'offline-box')
    expect(hosts[0].status).toBe('disconnected')
  })

  it('sorts sessions within a folder: alive first, then newest-first', () => {
    const sessions = [
      makeSession({
        id: 'dead-old', cwd: '/home/mg/dev/fluxer',
        alive: false, resumable: true, created_at: '2026-01-01T00:00:00Z',
        project_slug: 'fluxer', project_index: 0,
      }),
      makeSession({
        id: 'alive-old', cwd: '/home/mg/dev/fluxer',
        alive: true, created_at: '2026-01-02T00:00:00Z',
        project_slug: 'fluxer', project_index: 1,
      }),
      makeSession({
        id: 'alive-new', cwd: '/home/mg/dev/fluxer',
        alive: true, created_at: '2026-01-03T00:00:00Z',
        project_slug: 'fluxer', project_index: 2,
      }),
    ]
    const hosts = buildProjectTopology('fluxer', sessions, projects, peers)
    expect(hosts[0].folders[0].sessions.map(s => s.id)).toEqual([
      'alive-new', 'alive-old', 'dead-old',
    ])
  })

  it('hides dead non-resumable sessions', () => {
    const sessions = [
      makeSession({ id: 'alive-1', cwd: '/home/mg/dev/fluxer', alive: true,
                    project_slug: 'fluxer', project_index: 0 }),
      makeSession({ id: 'dead-1', cwd: '/home/mg/dev/fluxer', alive: false, resumable: false,
                    project_slug: 'fluxer', project_index: 1 }),
    ]
    const hosts = buildProjectTopology('fluxer', sessions, projects, peers)
    expect(hosts[0].folders[0].sessions.map(s => s.id)).toEqual(['alive-1'])
  })

  it('filters sessions by stamp (ignores unrelated stamps)', () => {
    const projects2: ProjectItem[] = [
      { slug: 'fluxer', match: [{ path: '/home/mg/dev/fluxer' }] },
      { slug: 'other', match: [{ path: '/home/mg/dev/other' }] },
    ]
    const sessions = [
      makeSession({ id: 'f1', cwd: '/home/mg/dev/fluxer',
                    project_slug: 'fluxer', project_index: 0 }),
      makeSession({ id: 'o1', cwd: '/home/mg/dev/other',
                    project_slug: 'other', project_index: 0 }),
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
    // Equal created_at so the recency key ties and the active/session
    // count keys are what's exercised here.
    const ts = '2026-01-01T00:00:00Z'
    const sessions = [
      makeSession({ id: '1', cwd: '/a', alive: false, created_at: ts }),
      makeSession({ id: '2', cwd: '/b', alive: true, created_at: ts }),
      makeSession({ id: '3', cwd: '/c', alive: true, created_at: ts }),
      makeSession({ id: '4', cwd: '/c', alive: false, created_at: ts }),
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

  it('breaks suggested_slug ties on path for stable order across snapshots', () => {
    // Two unrelated sessions whose cwd basenames collide on
    // slugFromPath both produce suggested_slug = "api". With only
    // active_count, session_count, suggested_slug as sort keys the
    // pair would tie; the sort then falls through to the byKey Map's
    // insertion order, which mirrors the input sessions array.
    //
    // In the manage-projects modal that input order is set by
    // snapshot.sessions, whose Go-map iteration is randomized, so
    // every snapshot re-emit would visually flip the rows. The
    // paths[0] tiebreak pins them.
    // Pin created_at equal so the recency sort key ties and the
    // path tiebreak is what's under test.
    const a = makeSession({ id: 'sa', cwd: '/home/me/api', alive: true, created_at: '2026-01-01T00:00:00Z' })
    const b = makeSession({ id: 'sb', cwd: '/srv/api', alive: true, created_at: '2026-01-01T00:00:00Z' })

    for (const input of [[a, b], [b, a]]) {
      const out = discoverProjects(input, [])
      expect(out.map(d => d.paths[0])).toEqual(['/home/me/api', '/srv/api'])
    }
  })

  it('excludes ALL peer sessions (discovery is host-authoritative)', () => {
    // Discovery is owned by the originating host (ADR 0002/0005): the
    // viewer computes discovery only for its own local sessions. Peer
    // sessions — connected or not — are discovered by their owning
    // host and relayed verbatim (merged in store.discovered), never
    // recomputed here.
    const sessions = [
      makeSession({ id: 'a@up', cwd: '/work/a', alive: true, peer: 'up' }),
      makeSession({ id: 'b@down', cwd: '/work/b', alive: true, peer: 'down' }),
      makeSession({ id: 'local', cwd: '/work/local', alive: true }),
    ]
    const out = discoverProjects(sessions, [])
    expect(out.map(d => d.paths[0])).toEqual(['/work/local'])
    expect(out.every(d => d.peer === undefined)).toBe(true)
  })

  it('treats local-peer (devcontainer) sessions as local', () => {
    // Per ADR 0005 a Local peer's project assignment is owned by the
    // parent, so its disclaimed sessions flow through the parent's
    // local discovery rather than the container's own.
    const sessions = [
      makeSession({ id: 's@dev', cwd: '/work/dc', alive: true, peer: 'dev' }),
    ]
    const out = discoverProjects(sessions, [], (name) => name === 'dev')
    expect(out).toHaveLength(1)
    expect(out[0].paths).toEqual(['/work/dc'])
    expect(out[0].peer).toBeUndefined()
  })

  it('populates last_active from the most recent session timestamp', () => {
    const sessions = [
      makeSession({ id: 'a', cwd: '/work/foo', alive: true, created_at: '2026-01-01T00:00:00Z' }),
      makeSession({ id: 'b', cwd: '/work/foo', alive: true, created_at: '2026-02-01T00:00:00Z' }),
    ]
    const out = discoverProjects(sessions, [])
    expect(out[0].last_active).toBe('2026-02-01T00:00:00Z')
  })
})

describe('countUnmatchedActive', () => {
  it('counts alive disclaimed sessions regardless of viewer rule matches', () => {
    // Under the references model, viewer rules don't adopt anything;
    // a disclaimed session is unmatched until the owning daemon
    // stamps it. So 'a' (waiting on local AutoAssign) is counted
    // alongside 'b' and 'd'.
    const projects: ProjectItem[] = [
      { slug: 'gmux', match: [{ path: '/dev/gmux' }] },
    ]
    const sessions = [
      makeSession({ id: 'a', cwd: '/dev/gmux/x', alive: true }),
      makeSession({ id: 'b', cwd: '/elsewhere', alive: true }),
      makeSession({ id: 'c', cwd: '/elsewhere', alive: false }),
      makeSession({ id: 'd', cwd: '/other', alive: true }),
    ]
    expect(countUnmatchedActive(sessions, projects)).toBe(3)
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

  it('counts a connected peer\'s disclaimed sessions', () => {
    // A connected peer's disclaimed session is unmatched per its own
    // host; viewer rules no longer adopt it, so it always counts.
    const sessions = [
      makeSession({ id: 's@dev', cwd: '/workspaces/gmux', alive: true, peer: 'dev' }),
    ]
    const peerStatus = new Map([['dev', 'connected']])
    expect(countUnmatchedActive(sessions, [], peerStatus)).toBe(1)
  })

  it('skips disclaimed sessions on disconnected peers', () => {
    // Don\'t nag the user about an unreachable peer; the count may
    // be stale.
    const sessions = [
      makeSession({ id: 's@tower', cwd: '/x', alive: true, peer: 'tower' }),
    ]
    const peerStatus = new Map([['tower', 'disconnected']])
    expect(countUnmatchedActive(sessions, [], peerStatus)).toBe(0)
  })

  it('returns 0 when there are no sessions', () => {
    expect(countUnmatchedActive([], [])).toBe(0)
  })
})

describe('projectAvailability', () => {
  const connected = new Map([['server', 'connected'], ['bespin', 'disconnected']])

  it('reports owned (local) projects as ok regardless of peer roster', () => {
    expect(projectAvailability({ peer: undefined }, new Map())).toBe('ok')
  })

  it('reports a reference to a connected host as ok', () => {
    expect(projectAvailability({ peer: 'server' }, connected)).toBe('ok')
  })

  it('reports a reference to a roster host that is not connected as offline', () => {
    expect(projectAvailability({ peer: 'bespin' }, connected)).toBe('offline')
  })

  it('reports a reference to a host absent from the roster as offline', () => {
    // peer set but not in the status map at all (e.g. removed peer that
    // still resolved by name moments ago) is treated as not reachable.
    expect(projectAvailability({ peer: 'ghost' }, connected)).toBe('offline')
  })

  it('reports an unresolved reference as unresolved', () => {
    expect(projectAvailability({ peer: 'old-tower', unresolved: true }, connected)).toBe('unresolved')
  })

  it('reports a connected host missing the slug as missing', () => {
    expect(projectAvailability({ peer: 'server', missing: true }, connected)).toBe('missing')
  })

  it('prefers unresolved over offline when both could apply', () => {
    // A stored name that is both unresolved and (coincidentally) a
    // disconnected roster entry surfaces as unresolved — the actionable
    // state — not offline.
    expect(projectAvailability({ peer: 'bespin', unresolved: true }, connected)).toBe('unresolved')
  })
})
