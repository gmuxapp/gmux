import { describe, it, expect, beforeEach, vi, afterEach } from 'vitest'
import { sessions, sessionsLoaded, projects, upsertSession, removeSession, markSessionRead, handleActivity, isSessionActive, isSessionFading, activityMap, sessionStaleness, peers, peerAppearance, urlPath, selectedId, navigateToSession, setNavigate } from './store'
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
  sessions.value = []
  projects.value = []
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
    sessions.value = [makeSession({ id: 'sess-1', title: 'old' })]
    const isNew = upsertSession({
      id: 'sess-1', alive: true, title: 'new',
      cwd: '/home/user', command: ['/bin/sh'], kind: 'shell',
    } as any)
    expect(isNew).toBe(false)
    expect(sessions.value).toHaveLength(1)
    expect(sessions.value[0].title).toBe('new')
  })

  it('preserves other sessions during update', () => {
    sessions.value = [
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
    projects.value = testProjects
    sessionsLoaded.value = true
    sessions.value = [
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
    projects.value = testProjects
    sessionsLoaded.value = true
    sessions.value = [
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
    sessions.value = [
      makeSession({ id: 'sess-1' }),
      makeSession({ id: 'sess-2' }),
    ]
    removeSession('sess-1')
    expect(sessions.value.map(s => s.id)).toEqual(['sess-2'])
  })

  it('is a no-op for unknown ids', () => {
    sessions.value = [makeSession({ id: 'sess-1' })]
    removeSession('ghost')
    expect(sessions.value).toHaveLength(1)
  })
})

describe('markSessionRead', () => {
  // Prevent the actual fetch from firing.
  beforeEach(() => { vi.stubGlobal('fetch', vi.fn().mockResolvedValue({ ok: true })) })
  afterEach(() => { vi.restoreAllMocks() })

  it('clears unread flag on the target session', () => {
    sessions.value = [makeSession({ id: 'sess-1', unread: true })]
    markSessionRead('sess-1')
    expect(sessions.value[0].unread).toBe(false)
  })

  it('clears error flag from status', () => {
    sessions.value = [makeSession({
      id: 'sess-1',
      status: { label: 'failed', working: false, error: true },
    })]
    markSessionRead('sess-1')
    expect(sessions.value[0].status?.error).toBe(false)
    expect(sessions.value[0].status?.label).toBe('failed')
  })

  it('does not touch other sessions', () => {
    sessions.value = [
      makeSession({ id: 'sess-1', unread: true }),
      makeSession({ id: 'sess-2', unread: true }),
    ]
    markSessionRead('sess-1')
    expect(sessions.value[0].unread).toBe(false)
    expect(sessions.value[1].unread).toBe(true)
  })

  it('posts to the server', () => {
    sessions.value = [makeSession({ id: 'sess-1', unread: true })]
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
    projects.value = [{ slug: 'p', match: [{ path: '/dev/p' }] }]
    expect(navigateToSession('ghost')).toBe(false)
    expect(navigateMock).not.toHaveBeenCalled()
  })

  it('returns false and does not navigate when projects have not loaded', () => {
    sessions.value = [makeSession({ id: 'sess-1', cwd: '/dev/p' })]
    // projects.value left empty: simulates the SSE-vs-REST race where
    // sessions arrive before projects.
    expect(navigateToSession('sess-1')).toBe(false)
    expect(navigateMock).not.toHaveBeenCalled()
  })

  it('returns true and dispatches the project-prefixed URL once both are loaded', () => {
    projects.value = [{ slug: 'myproject', match: [{ path: '/dev/p' }] }]
    sessions.value = [makeSession({ id: 'sess-1', cwd: '/dev/p', kind: 'shell' })]
    expect(navigateToSession('sess-1', true)).toBe(true)
    expect(navigateMock).toHaveBeenCalledTimes(1)
    const [url, replace] = navigateMock.mock.calls[0]
    expect(url).toMatch(/^\/myproject\/shell\//)
    expect(replace).toBe(true)
  })
})

describe('peerAppearance', () => {
  afterEach(() => { peers.value = [] })

  it('computes unique single-char prefixes when first chars differ', () => {
    peers.value = [
      { name: 'dev', url: '', status: 'connected', session_count: 0 },
      { name: 'staging', url: '', status: 'connected', session_count: 0 },
    ]
    const map = peerAppearance.value
    expect(map.get('dev')!.label).toBe('D')
    expect(map.get('staging')!.label).toBe('S')
  })

  it('extends prefix to disambiguate shared first characters', () => {
    peers.value = [
      { name: 'dev', url: '', status: 'connected', session_count: 0 },
      { name: 'desktop', url: '', status: 'connected', session_count: 0 },
    ]
    const map = peerAppearance.value
    // 'dev' vs 'desktop': 'de' is shared, need 3 chars
    expect(map.get('dev')!.label).toBe('DEV')
    expect(map.get('desktop')!.label).toBe('DES')
  })

  it('uses full name when one name is a prefix of another', () => {
    peers.value = [
      { name: 'dev', url: '', status: 'connected', session_count: 0 },
      { name: 'development', url: '', status: 'connected', session_count: 0 },
    ]
    const map = peerAppearance.value
    // 'dev' is fully consumed before it diverges from 'development'
    expect(map.get('dev')!.label).toBe('DEV')
    expect(map.get('development')!.label).toBe('DEVE')
  })

  it('assigns stable colors by name hash, independent of list order', () => {
    peers.value = [
      { name: 'alpha', url: '', status: 'connected', session_count: 0 },
      { name: 'beta', url: '', status: 'connected', session_count: 0 },
    ]
    const color1 = peerAppearance.value.get('alpha')!.color
    // Reverse order: alpha's color should not change
    peers.value = [
      { name: 'beta', url: '', status: 'connected', session_count: 0 },
      { name: 'alpha', url: '', status: 'connected', session_count: 0 },
    ]
    expect(peerAppearance.value.get('alpha')!.color).toBe(color1)
  })
})
