import { describe, it, expect, beforeEach, vi, afterEach } from 'vitest'
import {
  sessions, sessionsLoaded, projects, upsertSession, removeSession,
  markSessionRead, dismissSession, reorderSessions,
  handleActivity, isSessionActive, isSessionFading, activityMap,
  sessionStaleness, peers, peerAppearance, peerStatusByName,
  isSessionUnavailable, urlPath, selectedId,
  navigateToSession, setNavigate,
  applyPending, _rawSessions, _rawWorld, _setRawWorld, _pendingMutations,
} from './store'
import type { PendingMutation } from './store'
import type { Session } from './types'
import type { ProjectItem } from './types'

function makeSession(overrides: Partial<Session> & { id: string }): Session {
  return {
    created_at: '2026-01-01T00:00:00Z',
    command: ['/bin/sh'],
    cwd: '/home/user',
    kind: 'shell',
    alive: true,
    pid: 1,
    exit_code: null,
    started_at: '2026-01-01T00:00:00Z',
    exited_at: null,
    title: 'shell',
    subtitle: '',
    status: null,
    unread: false,
    resumable: false,
    socket_path: '/tmp/s.sock',
    runner_version: undefined,
    ...overrides,
  }
}

// Reset signal state between tests.
beforeEach(() => {
  _rawSessions.value = []
  _setRawWorld({ projects: [], peers: [] })
  _pendingMutations.value = []
  sessionsLoaded.value = false
  urlPath.value = '/'
})

describe('upsertSession', () => {
  it('inserts a new session and returns true', () => {
    const isNew = upsertSession({
      id: 'sess-1', alive: true, cwd: '/home/user',
      command: ['/bin/sh'], kind: 'shell',
    } as any)
    expect(isNew).toBe(true)
    expect(sessions.value).toHaveLength(1)
    expect(sessions.value[0].id).toBe('sess-1')
  })

  it('updates an existing session and returns false', () => {
    _rawSessions.value = [makeSession({ id: 'sess-1', title: 'old' })]
    const isNew = upsertSession({
      id: 'sess-1', alive: true, title: 'new',
      cwd: '/home/user', command: ['/bin/sh'], kind: 'shell',
    } as any)
    expect(isNew).toBe(false)
    expect(sessions.value).toHaveLength(1)
    expect(sessions.value[0].title).toBe('new')
  })

  it('preserves other sessions during update', () => {
    _rawSessions.value = [
      makeSession({ id: 'sess-1', title: 'first' }),
      makeSession({ id: 'sess-2', title: 'second' }),
    ]
    upsertSession({
      id: 'sess-1', alive: false, title: 'updated',
      cwd: '/home/user', command: ['/bin/sh'], kind: 'shell',
    } as any)
    expect(sessions.value).toHaveLength(2)
    expect(sessions.value[0].title).toBe('updated')
    expect(sessions.value[1].title).toBe('second')
  })

  it('rewrites URL when selected session slug changes', () => {
    const testProjects: ProjectItem[] = [
      { slug: 'myproject', match: [{ path: '/dev/project' }] },
    ]
    _setRawWorld({ projects: testProjects })
    sessionsLoaded.value = true
    _rawSessions.value = [
      makeSession({ id: 'sess-1', cwd: '/dev/project', kind: 'pi', slug: 'fix-auth' }),
    ]
    // Simulate the session being selected via URL.
    urlPath.value = '/myproject/pi/fix-auth'
    expect(selectedId.value).toBe('sess-1')

    // SSE upserts with a new slug (e.g., /new changed the active file).
    upsertSession({
      id: 'sess-1', alive: true, cwd: '/dev/project', kind: 'pi',
      slug: 'refactor-login', command: ['pi'], title: 'pi',
    } as any)

    // URL should be atomically rewritten; session stays selected.
    expect(urlPath.value).toBe('/myproject/pi/refactor-login')
    expect(selectedId.value).toBe('sess-1')
  })

  it('does not rewrite URL when a non-selected session slug changes', () => {
    const testProjects: ProjectItem[] = [
      { slug: 'myproject', match: [{ path: '/dev/project' }] },
    ]
    _setRawWorld({ projects: testProjects })
    sessionsLoaded.value = true
    _rawSessions.value = [
      makeSession({ id: 'sess-1', cwd: '/dev/project', kind: 'pi', slug: 'fix-auth' }),
      makeSession({ id: 'sess-2', cwd: '/dev/project', kind: 'pi', slug: 'old-slug' }),
    ]
    urlPath.value = '/myproject/pi/fix-auth'
    expect(selectedId.value).toBe('sess-1')

    // sess-2's slug changes, but it's not the selected session.
    upsertSession({
      id: 'sess-2', alive: true, cwd: '/dev/project', kind: 'pi',
      slug: 'new-slug', command: ['pi'], title: 'pi',
    } as any)

    // URL should be unchanged.
    expect(urlPath.value).toBe('/myproject/pi/fix-auth')
    expect(selectedId.value).toBe('sess-1')
  })
})

describe('removeSession', () => {
  it('removes the session with the given id', () => {
    _rawSessions.value = [
      makeSession({ id: 'sess-1' }),
      makeSession({ id: 'sess-2' }),
    ]
    removeSession('sess-1')
    expect(sessions.value.map(s => s.id)).toEqual(['sess-2'])
  })

  it('is a no-op for unknown ids', () => {
    _rawSessions.value = [makeSession({ id: 'sess-1' })]
    removeSession('ghost')
    expect(sessions.value).toHaveLength(1)
  })
})

describe('markSessionRead', () => {
  // Prevent the actual fetch from firing.
  beforeEach(() => { vi.stubGlobal('fetch', vi.fn().mockResolvedValue({ ok: true })) })
  afterEach(() => { vi.restoreAllMocks() })

  it('clears unread flag on the target session', () => {
    _rawSessions.value = [makeSession({ id: 'sess-1', unread: true })]
    markSessionRead('sess-1')
    expect(sessions.value[0].unread).toBe(false)
  })

  it('clears error flag from status', () => {
    _rawSessions.value = [makeSession({
      id: 'sess-1',
      status: { label: 'failed', working: false, error: true },
    })]
    markSessionRead('sess-1')
    expect(sessions.value[0].status?.error).toBe(false)
    expect(sessions.value[0].status?.label).toBe('failed')
  })

  it('does not touch other sessions', () => {
    _rawSessions.value = [
      makeSession({ id: 'sess-1', unread: true }),
      makeSession({ id: 'sess-2', unread: true }),
    ]
    markSessionRead('sess-1')
    expect(sessions.value[0].unread).toBe(false)
    expect(sessions.value[1].unread).toBe(true)
  })

  it('posts to the server', () => {
    _rawSessions.value = [makeSession({ id: 'sess-1', unread: true })]
    markSessionRead('sess-1')
    expect(fetch).toHaveBeenCalledWith('/v1/sessions/sess-1/read', { method: 'POST' })
  })
})

describe('activity tracking', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    // Reset the activity map to a clean state.
    activityMap.value = new Map()
  })
  afterEach(() => {
    vi.useRealTimers()
  })

  it('marks a session as active immediately', () => {
    handleActivity('sess-1')
    expect(isSessionActive('sess-1')).toBe(true)
    expect(isSessionFading('sess-1')).toBe(false)
  })

  it('transitions to fading after the active window', () => {
    handleActivity('sess-1')
    vi.advanceTimersByTime(3000)
    expect(isSessionActive('sess-1')).toBe(false)
    expect(isSessionFading('sess-1')).toBe(true)
  })

  it('clears completely after fade-out', () => {
    handleActivity('sess-1')
    vi.advanceTimersByTime(3000 + 800)
    expect(isSessionActive('sess-1')).toBe(false)
    expect(isSessionFading('sess-1')).toBe(false)
  })

  it('resets the timer when activity fires again', () => {
    handleActivity('sess-1')
    vi.advanceTimersByTime(2000) // still active
    handleActivity('sess-1')     // reset
    vi.advanceTimersByTime(2000) // 2s since reset, still active
    expect(isSessionActive('sess-1')).toBe(true)
  })
})

describe('sessionStaleness', () => {
  const h = { version: '1.2.0', runner_hash: 'aabbccdd1122' }

  it('returns null when health is null (not yet loaded)', () => {
    expect(sessionStaleness({ runner_version: '1.1.0' }, null)).toBeNull()
  })

  it('returns null when runner_version is absent (pre-version runner)', () => {
    expect(sessionStaleness({}, h)).toBeNull()
    expect(sessionStaleness({ binary_hash: 'aabbccdd1122' }, h)).toBeNull()
  })

  it("returns 'version' when runner version differs from daemon version", () => {
    expect(sessionStaleness({ runner_version: '1.1.0' }, h)).toBe('version')
    expect(sessionStaleness({ runner_version: '0.9.0' }, h)).toBe('version')
  })

  it('returns null when runner and daemon versions match and no hash info', () => {
    expect(sessionStaleness({ runner_version: '1.2.0' }, { version: '1.2.0' })).toBeNull()
  })

  it('returns null when versions and hashes both match', () => {
    expect(sessionStaleness(
      { runner_version: '1.2.0', binary_hash: 'aabbccdd1122' }, h,
    )).toBeNull()
  })

  it("returns 'hash' when versions match but hashes differ (dev-mode drift)", () => {
    expect(sessionStaleness(
      { runner_version: '1.2.0', binary_hash: 'deadbeef9999' }, h,
    )).toBe('hash')
  })

  it("returns 'version' not 'hash' when both differ (version takes priority)", () => {
    expect(sessionStaleness(
      { runner_version: '1.1.0', binary_hash: 'deadbeef9999' }, h,
    )).toBe('version')
  })

  it("returns null for 'dev'/'dev' version match with no hash available", () => {
    // Common in dev: both report "dev", hash unknown on health side
    expect(sessionStaleness(
      { runner_version: 'dev', binary_hash: 'aabbcc' },
      { version: 'dev' },
    )).toBeNull()
  })

  it("returns 'hash' for 'dev'/'dev' version match with differing hashes", () => {
    expect(sessionStaleness(
      { runner_version: 'dev', binary_hash: 'deadbeef' },
      { version: 'dev', runner_hash: 'aabbccdd' },
    )).toBe('hash')
  })

  it('returns null when compared against peer with matching version (no hash)', () => {
    // Remote sessions are compared against their peer version, which has
    // no runner_hash. Hash drift should not trigger a false positive.
    expect(sessionStaleness(
      { runner_version: '1.2.0', binary_hash: 'deadbeef9999' },
      { version: '1.2.0' },
    )).toBeNull()
  })

  it("returns 'version' when compared against peer with different version", () => {
    expect(sessionStaleness(
      { runner_version: '1.1.0' },
      { version: '1.2.0' },
    )).toBe('version')
  })
})

describe('navigateToSession', () => {
  // The e2e helper (e2e/helpers.ts) polls a test hook that wraps
  // navigateToSession and treats its return value as "the URL has
  // changed". If the contract regresses (e.g. someone makes the
  // function return void again, or fires navigate() without a project
  // match), the e2e suite goes flaky in CI under SSE-vs-REST races
  // between sessions and projects. These tests pin that contract.
  let navigateMock: ReturnType<typeof vi.fn>

  beforeEach(() => {
    navigateMock = vi.fn()
    setNavigate(navigateMock)
  })
  afterEach(() => {
    setNavigate(() => {})
  })

  it('returns false and does not navigate when the session is unknown', () => {
    _setRawWorld({ projects: [{ slug: 'p', match: [{ path: '/dev/p' }] }] })
    expect(navigateToSession('ghost')).toBe(false)
    expect(navigateMock).not.toHaveBeenCalled()
  })

  it('returns false and does not navigate when projects have not loaded', () => {
    _rawSessions.value = [makeSession({ id: 'sess-1', cwd: '/dev/p' })]
    // projects left empty: simulates the snapshot.sessions-vs-snapshot.world
    // race where sessions arrive before projects.
    expect(navigateToSession('sess-1')).toBe(false)
    expect(navigateMock).not.toHaveBeenCalled()
  })

  it('returns true and dispatches the project-prefixed URL once both are loaded', () => {
    _setRawWorld({ projects: [{ slug: 'myproject', match: [{ path: '/dev/p' }] }] })
    _rawSessions.value = [makeSession({ id: 'sess-1', cwd: '/dev/p', kind: 'shell' })]
    expect(navigateToSession('sess-1', true)).toBe(true)
    expect(navigateMock).toHaveBeenCalledTimes(1)
    const [url, replace] = navigateMock.mock.calls[0]
    expect(url).toMatch(/^\/myproject\/shell\//)
    expect(replace).toBe(true)
  })

  it('routes peer-owned sessions through /@<peer>/<slug>/...', () => {
    // Peer-stamped session: project_slug + peer set, viewer has no
    // local match rule. Without ADR-0002 awareness this would either
    // return false (matchSession finds no project) or build a URL
    // missing the @<peer> prefix, which the e2e helper then can't
    // round-trip back to a session view.
    _setRawWorld({ projects: [] })
    _rawSessions.value = [makeSession({
      id: 'remote-1',
      kind: 'shell',
      slug: 'remote-1-slug',
      peer: 'workstation',
      project_slug: 'gmux',
    })]
    expect(navigateToSession('remote-1')).toBe(true)
    const [url] = navigateMock.mock.calls[0]
    expect(url).toBe('/@workstation/gmux/shell/remote-1-slug')
  })
})

describe('peerAppearance', () => {
  afterEach(() => { _setRawWorld({ peers: [] }) })

  it('computes unique single-char prefixes when first chars differ', () => {
    _setRawWorld({ peers: [
      { name: 'dev', url: '', status: 'connected', session_count: 0 },
      { name: 'staging', url: '', status: 'connected', session_count: 0 },
    ] })
    const map = peerAppearance.value
    expect(map.get('dev')!.label).toBe('D')
    expect(map.get('staging')!.label).toBe('S')
  })

  it('extends prefix to disambiguate shared first characters', () => {
    _setRawWorld({ peers: [
      { name: 'dev', url: '', status: 'connected', session_count: 0 },
      { name: 'desktop', url: '', status: 'connected', session_count: 0 },
    ] })
    const map = peerAppearance.value
    // 'dev' vs 'desktop': 'de' is shared, need 3 chars
    expect(map.get('dev')!.label).toBe('DEV')
    expect(map.get('desktop')!.label).toBe('DES')
  })

  it('uses full name when one name is a prefix of another', () => {
    _setRawWorld({ peers: [
      { name: 'dev', url: '', status: 'connected', session_count: 0 },
      { name: 'development', url: '', status: 'connected', session_count: 0 },
    ] })
    const map = peerAppearance.value
    // 'dev' is fully consumed before it diverges from 'development'
    expect(map.get('dev')!.label).toBe('DEV')
    expect(map.get('development')!.label).toBe('DEVE')
  })

  it('assigns stable colors by name hash, independent of list order', () => {
    _setRawWorld({ peers: [
      { name: 'alpha', url: '', status: 'connected', session_count: 0 },
      { name: 'beta', url: '', status: 'connected', session_count: 0 },
    ] })
    const color1 = peerAppearance.value.get('alpha')!.color
    // Reverse order: alpha's color should not change
    _setRawWorld({ peers: [
      { name: 'beta', url: '', status: 'connected', session_count: 0 },
      { name: 'alpha', url: '', status: 'connected', session_count: 0 },
    ] })
    expect(peerAppearance.value.get('alpha')!.color).toBe(color1)
  })
})

describe('peerStatusByName + isSessionUnavailable', () => {
  afterEach(() => { _setRawWorld({ peers: [] }) })

  it('maps each peer name to its current status', () => {
    _setRawWorld({ peers: [
      { name: 'tower', url: '', status: 'connected', session_count: 0 },
      { name: 'laptop', url: '', status: 'disconnected', session_count: 0 },
    ] })
    const map = peerStatusByName.value
    expect(map.get('tower')).toBe('connected')
    expect(map.get('laptop')).toBe('disconnected')
  })

  it('flags sessions on disconnected peers as unavailable', () => {
    const map = new Map([['tower', 'disconnected']])
    expect(isSessionUnavailable({ peer: 'tower' }, map)).toBe(true)
  })

  it('treats sessions on connected peers as available', () => {
    const map = new Map([['tower', 'connected']])
    expect(isSessionUnavailable({ peer: 'tower' }, map)).toBe(false)
  })

  it('treats local sessions (no peer) as available', () => {
    expect(isSessionUnavailable({}, new Map())).toBe(false)
    expect(isSessionUnavailable({ peer: undefined }, new Map())).toBe(false)
  })

  it('flags sessions claiming an unknown peer as unavailable', () => {
    // Peer absent from the world snapshot (e.g. removed from config but still
    // showing up in lingering snapshot data). Safer to flag than to
    // pretend the session is reachable.
    expect(isSessionUnavailable({ peer: 'ghost' }, new Map())).toBe(true)
  })

  it('treats "connecting" as unavailable', () => {
    // PeerInfo.status is 'connecting' during reconnect. The user can't
    // reach the session yet, so render as unavailable.
    const map = new Map([['tower', 'connecting']])
    expect(isSessionUnavailable({ peer: 'tower' }, map)).toBe(true)
  })
})

describe('raw signal projections', () => {
  it('exposes _rawSessions through the public sessions computed', () => {
    _rawSessions.value = [makeSession({ id: 'a', title: 'first' })]
    expect(sessions.value.map(s => s.id)).toEqual(['a'])
    _rawSessions.value = [
      makeSession({ id: 'a' }),
      makeSession({ id: 'b' }),
    ]
    expect(sessions.value.map(s => s.id)).toEqual(['a', 'b'])
  })

  it('exposes _rawWorld.projects through the public projects computed', () => {
    const items: ProjectItem[] = [{ slug: 'one', match: [{ path: '/x' }] }]
    _setRawWorld({ projects: items })
    expect(projects.value).toBe(items)
  })

  it('exposes _rawWorld.peers through the public peers computed', () => {
    _setRawWorld({ peers: [{ name: 'p', url: '', status: 'connected', session_count: 0 }] })
    expect(peers.value).toHaveLength(1)
    expect(peers.value[0].name).toBe('p')
  })

  it('_setRawWorld merges patches without dropping unrelated keys', () => {
    _setRawWorld({
      projects: [{ slug: 'a', match: [] }],
      peers: [{ name: 'p', url: '', status: 'connected', session_count: 0 }],
    })
    _setRawWorld({ peers: [] })
    // projects survived; peers cleared.
    expect(projects.value).toHaveLength(1)
    expect(peers.value).toHaveLength(0)
  })
})

describe('pending mutations overlay', () => {
  beforeEach(() => { vi.stubGlobal('fetch', vi.fn().mockResolvedValue({ ok: true })) })
  afterEach(() => { vi.restoreAllMocks() })

  describe('applyPending (pure function)', () => {
    it('returns raw unchanged when there are no mutations', () => {
      const sess = [makeSession({ id: 'a' })]
      const projs: ProjectItem[] = [{ slug: 'p', match: [] }]
      const out = applyPending(sess, projs, [])
      expect(out.sessions).toBe(sess)
      expect(out.projects).toBe(projs)
    })

    it('mark-read clears unread and status.error on the targeted session', () => {
      const sess = [
        makeSession({ id: 'a', unread: true, status: { label: 'oops', working: false, error: true } }),
        makeSession({ id: 'b', unread: true }),
      ]
      const m: PendingMutation = { kind: 'mark-read', id: 'a', at: 0 }
      const out = applyPending(sess, [], [m])
      expect(out.sessions[0].unread).toBe(false)
      expect(out.sessions[0].status?.error).toBe(false)
      expect(out.sessions[0].status?.label).toBe('oops')
      // Untouched session keeps its flags.
      expect(out.sessions[1].unread).toBe(true)
    })

    it('dismiss removes the targeted session', () => {
      const sess = [makeSession({ id: 'a' }), makeSession({ id: 'b' })]
      const out = applyPending(sess, [], [{ kind: 'dismiss', id: 'a', at: 0 }])
      expect(out.sessions.map(s => s.id)).toEqual(['b'])
    })

    it('reorder replaces a project sessions array', () => {
      const projs: ProjectItem[] = [{ slug: 'p', match: [], sessions: ['x', 'y'] }]
      const out = applyPending([], projs, [{ kind: 'reorder', slug: 'p', sessions: ['y', 'x'], at: 0 }])
      expect(out.projects[0].sessions).toEqual(['y', 'x'])
    })

    it('stacks multiple mutations in order', () => {
      const projs: ProjectItem[] = [{ slug: 'p', match: [], sessions: ['x'] }]
      const out = applyPending([], projs, [
        { kind: 'reorder', slug: 'p', sessions: ['a', 'b'], at: 0 },
        { kind: 'reorder', slug: 'p', sessions: ['b', 'a'], at: 0 },
      ])
      expect(out.projects[0].sessions).toEqual(['b', 'a'])
    })
  })

  describe('public projections apply pending mutations', () => {
    it('markSessionRead reflects via the pending overlay (raw is untouched)', () => {
      _rawSessions.value = [makeSession({ id: 'a', unread: true })]
      markSessionRead('a')
      expect(sessions.value[0].unread).toBe(false)
      // Raw is untouched.
      expect(_rawSessions.value[0].unread).toBe(true)
      expect(_pendingMutations.value).toHaveLength(1)
    })

    it('dismissSession hides the session via overlay without touching raw', () => {
      _rawSessions.value = [makeSession({ id: 'a' }), makeSession({ id: 'b' })]
      dismissSession('a')
      expect(sessions.value.map(s => s.id)).toEqual(['b'])
      expect(_rawSessions.value.map(s => s.id)).toEqual(['a', 'b'])
    })

    it('reorderSessions reflects via the projects overlay', () => {
      _setRawWorld({ projects: [{ slug: 'p', match: [], sessions: ['x', 'y'] }] })
      reorderSessions('p', ['y', 'x'])
      expect(projects.value[0].sessions).toEqual(['y', 'x'])
      // Raw is untouched.
      expect(_rawWorld.value.projects[0].sessions).toEqual(['x', 'y'])
    })

    it('reorderSessions for a local project hits /v1/projects/{slug}/sessions', () => {
      reorderSessions('gmux', ['y', 'x'])
      expect(globalThis.fetch).toHaveBeenCalledWith(
        '/v1/projects/gmux/sessions',
        expect.objectContaining({ method: 'PATCH' }),
      )
    })

    it('reorderSessions for a peer project routes through the peer proxy', () => {
      // ADR 0002: a peer's projects.json is owned by the peer; the
      // viewer asks the peer to reorder via /v1/peers/{peer}/...
      // rather than writing into its own projects.json.
      reorderSessions('gmux', ['y', 'x'], 'tower')
      expect(globalThis.fetch).toHaveBeenCalledWith(
        '/v1/peers/tower/v1/projects/gmux/sessions',
        expect.objectContaining({ method: 'PATCH' }),
      )
    })

    it('peer reorder does not add a local optimistic overlay', () => {
      // The local pending-mutations overlay is keyed by local project
      // slug; applying it to a peer reorder would clobber the peer
      // folder's session order with the viewer's local-projects view.
      const before = _pendingMutations.value.length
      reorderSessions('gmux', ['y', 'x'], 'tower')
      expect(_pendingMutations.value.length).toBe(before)
    })
  })

  describe('auto-clear on raw acknowledgement', () => {
    it('drops a mark-read mutation once raw shows unread=false', () => {
      _rawSessions.value = [makeSession({ id: 'a', unread: true })]
      markSessionRead('a')
      expect(_pendingMutations.value).toHaveLength(1)
      // Server echoes the cleared state.
      _rawSessions.value = [makeSession({ id: 'a', unread: false })]
      expect(_pendingMutations.value).toHaveLength(0)
    })

    it('drops a dismiss mutation once raw no longer contains the session', () => {
      _rawSessions.value = [makeSession({ id: 'a' })]
      dismissSession('a')
      expect(_pendingMutations.value).toHaveLength(1)
      _rawSessions.value = []
      expect(_pendingMutations.value).toHaveLength(0)
    })

    it('drops a reorder mutation once raw matches the requested order', () => {
      _setRawWorld({ projects: [{ slug: 'p', match: [], sessions: ['x', 'y'] }] })
      reorderSessions('p', ['y', 'x'])
      expect(_pendingMutations.value).toHaveLength(1)
      _setRawWorld({ projects: [{ slug: 'p', match: [], sessions: ['y', 'x'] }] })
      expect(_pendingMutations.value).toHaveLength(0)
    })

    it('keeps a mark-read mutation when raw still shows unread=true', () => {
      _rawSessions.value = [makeSession({ id: 'a', unread: true })]
      markSessionRead('a')
      // Some unrelated raw update arrives.
      _rawSessions.value = [makeSession({ id: 'a', unread: true, title: 'new' })]
      expect(_pendingMutations.value).toHaveLength(1)
      // Public projection still hides the unread flag.
      expect(sessions.value[0].unread).toBe(false)
    })
  })

  describe('TTL expiry', () => {
    beforeEach(() => { vi.useFakeTimers() })
    afterEach(() => { vi.useRealTimers() })

    it('drops a mutation after PENDING_TTL_MS even if raw never acknowledges', () => {
      _rawSessions.value = [makeSession({ id: 'a', unread: true })]
      markSessionRead('a')
      expect(_pendingMutations.value).toHaveLength(1)
      vi.advanceTimersByTime(5_000)
      expect(_pendingMutations.value).toHaveLength(0)
      // Public projection now reflects raw again.
      expect(sessions.value[0].unread).toBe(true)
    })
  })
})


