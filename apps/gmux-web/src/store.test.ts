import { describe, it, expect, beforeEach, vi, afterEach } from 'vitest'
import { toasts } from './toasts'
import {
  sessions, sessionsLoaded, worldLoaded, projects, upsertSession, removeSession,
  markSessionRead, dismissSession, reorderSessions, resumeSession, killSession,
  handleActivity, isSessionActive, isSessionFading, activityMap,
  sessionStaleness, peers, peerAppearance, peerStatusByName,
  isSessionUnavailable, urlPath, urlSearch, filteredSessions, selectedId,
  navigateToSession, setNavigate,
  applyPending, _rawSessions, _rawWorld, _setRawWorld, _pendingMutations,
  applySessionsSnapshot,
  toUISession, localHostLabel, parseConnectURL, unreadCount, discovered,
  view, duplicateConversationFiles,
} from './store'
import { SessionSchema } from '@gmux/protocol'
import type { PendingMutation } from './store'
import type { Session } from './types'
import type { ProjectItem } from './types'

function makeSession(overrides: Partial<Session> & { id: string }): Session {
  return {
    created_at: '2026-01-01T00:00:00Z',
    command: ['/bin/sh'],
    cwd: '/home/user',
    adapter: 'shell',
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
  worldLoaded.value = false
  urlPath.value = '/'
  urlSearch.value = ''
})

describe('postAction surfaces backend failures as error toasts', () => {
  beforeEach(() => { toasts.value = [] })
  afterEach(() => { vi.unstubAllGlobals(); toasts.value = [] })

  it('parses the structured error contract and labels the action', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({
      ok: false,
      status: 400,
      statusText: 'Bad Request',
      text: () => Promise.resolve(JSON.stringify({
        ok: false, error: { code: 'not_resumable', message: 'session is not resumable' },
      })),
    }))
    await resumeSession('s1')
    expect(toasts.value).toHaveLength(1)
    expect(toasts.value[0]).toMatchObject({ kind: 'error', message: 'Resume failed: session is not resumable' })
  })

  it('falls back to the status line when the body is not the structured shape', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({
      ok: false, status: 500, statusText: 'Internal Server Error',
      text: () => Promise.resolve(''),
    }))
    await killSession('s1')
    expect(toasts.value[0].message).toBe('Kill failed: Internal Server Error')
  })

  it('does NOT toast a network reject (connectivity is owned by the reconnecting pill)', async () => {
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new Error('offline')))
    await resumeSession('s1')
    expect(toasts.value).toHaveLength(0)
  })

  it('pushes nothing on success', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({
      ok: true, status: 200, statusText: 'OK', text: () => Promise.resolve(''),
    }))
    await killSession('s1')
    expect(toasts.value).toHaveLength(0)
  })

  it('resolves the success boolean and never rejects (callers branch, not catch)', async () => {
    // handleResume clears its "resuming…" spinner off this boolean; a
    // rejection-based contract would leave the spinner stuck for the
    // fallback timeout since postAction swallows all throws.
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({
      ok: false, status: 500, statusText: 'Internal Server Error',
      text: () => Promise.resolve(''),
    }))
    await expect(resumeSession('s1')).resolves.toBe(false)
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({
      ok: true, status: 200, statusText: 'OK', text: () => Promise.resolve(''),
    }))
    await expect(resumeSession('s1')).resolves.toBe(true)
  })
})

describe('optimistic dismiss retracts on failure', () => {
  beforeEach(() => { toasts.value = []; _pendingMutations.value = [] })
  afterEach(() => { vi.unstubAllGlobals(); toasts.value = []; _pendingMutations.value = [] })

  it('hides the row, then retracts immediately when the server rejects', async () => {
    _rawSessions.value = [makeSession({ id: 's1' })]
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({
      ok: false, status: 404, statusText: 'Not Found',
      text: () => Promise.resolve(JSON.stringify({ error: { message: 'no such session' } })),
    }))
    await dismissSession('s1')
    // Toast surfaced AND the optimistic mutation was retracted (not left
    // to linger until the TTL), so the session is visible again now.
    expect(toasts.value[0].message).toBe('Dismiss failed: no such session')
    expect(_pendingMutations.value).toHaveLength(0)
    expect(sessions.value.some(s => s.id === 's1')).toBe(true)
  })

  it('keeps the optimistic dismissal when the server accepts', async () => {
    _rawSessions.value = [makeSession({ id: 's1' })]
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({
      ok: true, status: 200, statusText: 'OK', text: () => Promise.resolve(''),
    }))
    await dismissSession('s1')
    expect(toasts.value).toHaveLength(0)
    // Mutation stands until the next snapshot confirms removal.
    expect(_pendingMutations.value).toHaveLength(1)
    expect(sessions.value.some(s => s.id === 's1')).toBe(false)
  })
})

describe('filteredSessions reactivity to the URL query string', () => {
  it('recomputes when urlSearch flips, without an SSE session update', () => {
    _rawSessions.value = [
      makeSession({ id: 'a', cwd: '/home/user/projects/alpha' }),
      makeSession({ id: 'b', cwd: '/home/user/projects/beta' }),
    ]
    // No query: all sessions pass through.
    expect(filteredSessions.value.map(s => s.id)).toEqual(['a', 'b'])

    // Flip only the query signal (no sessions.value change): filter applies.
    urlSearch.value = '?project=alpha'
    expect(filteredSessions.value.map(s => s.id)).toEqual(['a'])

    // cwd prefix filter, again query-only.
    urlSearch.value = '?cwd=/home/user/projects/beta'
    expect(filteredSessions.value.map(s => s.id)).toEqual(['b'])

    // Clearing the query restores the full list.
    urlSearch.value = ''
    expect(filteredSessions.value.map(s => s.id)).toEqual(['a', 'b'])
  })
})

// Pin the protocol-to-UI translation for stamp fields. Stamps are
// the sole authority for sidebar bucketing under the references
// model; if toUISession silently drops them (as it did between PR
// #191 landing and this fix), every session becomes invisible
// regardless of project configuration.
describe('toUISession project stamp passthrough', () => {
  it('preserves project_slug and project_index from the wire', () => {
    const ui = toUISession({
      id: 'sess-1', alive: true,
      project_slug: 'gmux', project_index: 3,
    } as any)
    expect(ui.project_slug).toBe('gmux')
    expect(ui.project_index).toBe(3)
  })

  it('preserves project_index: 0 (falsy but valid first position)', () => {
    // 0 is the most common value (first session in a project) and is
    // falsy in JS. Guards against future ||-coercion regressions on
    // this field.
    const ui = toUISession({
      id: 'sess-1', alive: true,
      project_slug: 'gmux', project_index: 0,
    } as any)
    expect(ui.project_index).toBe(0)
  })

  it('leaves stamps undefined when the wire omits them', () => {
    const ui = toUISession({
      id: 'sess-1', alive: true,
    } as any)
    expect(ui.project_slug).toBeUndefined()
  })

  it('passes last_activity_at through from the wire', () => {
    // The owning daemon stamps this; the UI uses it for the home
    // dashboard's Recent section sort. Pure passthrough at the
    // boundary; no client-side derivation.
    const ui = toUISession({
      id: 'sess-1', alive: true,
      last_activity_at: '2026-01-15T08:00:00Z',
    } as any)
    expect(ui.last_activity_at).toBe('2026-01-15T08:00:00Z')
  })

  it('leaves last_activity_at undefined when the wire omits it', () => {
    const ui = toUISession({ id: 'sess-1', alive: true } as any)
    expect(ui.last_activity_at).toBeUndefined()
  })

  it('treats empty-string project_slug as unstamped', () => {
    // Go's omitempty drops empty strings, but legacy / dev paths may
    // emit them. buildProjectFolders treats an empty stamp the same
    // as no stamp; normalize at the boundary so consumers never
    // see the difference.
    const ui = toUISession({
      id: 'sess-1', alive: true, project_slug: '',
    } as any)
    expect(ui.project_slug).toBeUndefined()
  })
})

describe('upsertSession', () => {
  it('inserts a new session and returns true', () => {
    const isNew = upsertSession({
      id: 'sess-1', alive: true, cwd: '/home/user',
      command: ['/bin/sh'], adapter: 'shell',
    } as any)
    expect(isNew).toBe(true)
    expect(sessions.value).toHaveLength(1)
    expect(sessions.value[0].id).toBe('sess-1')
  })

  it('updates an existing session and returns false', () => {
    _rawSessions.value = [makeSession({ id: 'sess-1', title: 'old' })]
    const isNew = upsertSession({
      id: 'sess-1', alive: true, title: 'new',
      cwd: '/home/user', command: ['/bin/sh'], adapter: 'shell',
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
      cwd: '/home/user', command: ['/bin/sh'], adapter: 'shell',
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
    worldLoaded.value = true
    _rawSessions.value = [
      makeSession({ id: 'sess-1', cwd: '/dev/project', adapter: 'pi', slug: 'fix-auth' }),
    ]
    // Simulate the session being selected via URL.
    urlPath.value = '/myproject/pi/fix-auth'
    expect(selectedId.value).toBe('sess-1')

    // SSE upserts with a new slug (e.g., /new changed the active file).
    upsertSession({
      id: 'sess-1', alive: true, cwd: '/dev/project', adapter: 'pi',
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
    worldLoaded.value = true
    _rawSessions.value = [
      makeSession({ id: 'sess-1', cwd: '/dev/project', adapter: 'pi', slug: 'fix-auth' }),
      makeSession({ id: 'sess-2', cwd: '/dev/project', adapter: 'pi', slug: 'old-slug' }),
    ]
    urlPath.value = '/myproject/pi/fix-auth'
    expect(selectedId.value).toBe('sess-1')

    // sess-2's slug changes, but it's not the selected session.
    upsertSession({
      id: 'sess-2', alive: true, cwd: '/dev/project', adapter: 'pi',
      slug: 'new-slug', command: ['pi'], title: 'pi',
    } as any)

    // URL should be unchanged.
    expect(urlPath.value).toBe('/myproject/pi/fix-auth')
    expect(selectedId.value).toBe('sess-1')
  })
})

describe('applySessionsSnapshot: /resume keeps the terminal mounted', () => {
  // Regression for the /resume boot-to-project bug (and the same class of
  // rename bug, #348/#360). A pi /resume keeps the gmux session id but
  // swaps the active conversation, so the title-derived slug changes. The
  // daemon re-pushes the *entire* session list (protocol 2 / ADR 0001),
  // which the client applies wholesale via applySessionsSnapshot.
  //
  // These drive the real production entry point (not the extracted helper
  // in isolation) and assert the observable that actually regressed: the
  // resolved `view` must stay on the session, with the URL rewritten in
  // place — not fall back to the project hub. Testing the seam, not just
  // the helper, is deliberate: the original regression was precisely that
  // the rewrite logic existed but nothing on the live path called it.
  const testProjects: ProjectItem[] = [
    { slug: 'myproject', match: [{ path: '/dev/project' }] },
  ]
  const navCalls: Array<[string, boolean | undefined]> = []
  beforeEach(() => {
    navCalls.length = 0
    setNavigate((url, replace) => { navCalls.push([url, replace]) })
    _setRawWorld({ projects: testProjects })
    _rawSessions.value = [
      makeSession({ id: 'sess-1', cwd: '/dev/project', adapter: 'pi', slug: 'fix-auth' }),
    ]
    sessionsLoaded.value = true
    worldLoaded.value = true
    urlPath.value = '/myproject/pi/fix-auth'
  })

  it('rewrites the URL and keeps the session view when the selected slug changes', () => {
    expect(view.value).toEqual({ kind: 'session', sessionId: 'sess-1' })

    // Snapshot: same id, resumed slug, brand-new array (the wire shape).
    applySessionsSnapshot([
      makeSession({ id: 'sess-1', cwd: '/dev/project', adapter: 'pi', slug: 'refactor-login' }),
    ])

    expect(urlPath.value).toBe('/myproject/pi/refactor-login')
    // The user stays on the terminal — no boot to the project hub.
    expect(view.value).toEqual({ kind: 'session', sessionId: 'sess-1' })
    // Address bar synced via replaceState (replace=true), so back/forward
    // history isn't polluted.
    expect(navCalls).toContainEqual(['/myproject/pi/refactor-login', true])
  })

  it('leaves the URL untouched when the selected slug is unchanged', () => {
    applySessionsSnapshot([
      makeSession({ id: 'sess-1', cwd: '/dev/project', adapter: 'pi', slug: 'fix-auth', title: 'now working' }),
    ])

    expect(urlPath.value).toBe('/myproject/pi/fix-auth')
    expect(view.value).toEqual({ kind: 'session', sessionId: 'sess-1' })
    expect(navCalls).toEqual([])
  })

  it('does not rewrite when a non-selected session changes slug', () => {
    _rawSessions.value = [
      makeSession({ id: 'sess-1', cwd: '/dev/project', adapter: 'pi', slug: 'fix-auth' }),
      makeSession({ id: 'sess-2', cwd: '/dev/project', adapter: 'pi', slug: 'old' }),
    ]

    applySessionsSnapshot([
      makeSession({ id: 'sess-1', cwd: '/dev/project', adapter: 'pi', slug: 'fix-auth' }),
      makeSession({ id: 'sess-2', cwd: '/dev/project', adapter: 'pi', slug: 'new' }),
    ])

    expect(urlPath.value).toBe('/myproject/pi/fix-auth')
    expect(navCalls).toEqual([])
  })

  it('boots to the hub only when the selected session is genuinely gone', () => {
    // A snapshot that drops the session (killed) — distinct from a slug
    // change — should fall through to the normal commit and let the view
    // resolve to the project hub.
    applySessionsSnapshot([])

    expect(view.value).toEqual({ kind: 'project', projectSlug: 'myproject' })
  })

  it('still commits the snapshot (loaded flags flip) when nothing is selected', () => {
    urlPath.value = '/'
    sessionsLoaded.value = false
    applySessionsSnapshot([
      makeSession({ id: 'sess-9', cwd: '/dev/project', adapter: 'pi', slug: 'x' }),
    ])
    expect(sessionsLoaded.value).toBe(true)
    expect(sessions.value.map(s => s.id)).toContain('sess-9')
    expect(navCalls).toEqual([])
  })
})

describe('deep-link refresh: snapshot ordering race (#308-adjacent)', () => {
  // The daemon emits snapshot.sessions *before* snapshot.world on a
  // fresh SSE subscription (ADR 0001). On a refresh while viewing a
  // session, the sessions event lands first and flips sessionsLoaded
  // while projects are still empty. Resolving the local-project URL
  // against an empty projects list yields `home`, which the URL
  // normalization effect would then write to the address bar —
  // bouncing the user off their session. The view must stay `null`
  // until the world snapshot has also arrived.
  it('does not resolve a session URL to home before the world loads', () => {
    urlPath.value = '/myproject/pi/fix-auth'
    _rawSessions.value = [
      makeSession({ id: 'sess-1', cwd: '/dev/project', adapter: 'pi', slug: 'fix-auth', project_slug: 'myproject' }),
    ]
    // Sessions snapshot arrived; world has not (projects still empty).
    sessionsLoaded.value = true
    worldLoaded.value = false

    // View must be unresolved — NOT home — so nothing rewrites the URL.
    expect(view.value).toBeNull()
    expect(selectedId.value).toBeNull()

    // World snapshot arrives: now the session resolves.
    _setRawWorld({ projects: [{ slug: 'myproject', match: [{ path: '/dev/project' }] }] })
    worldLoaded.value = true

    expect(view.value).toEqual({ kind: 'session', sessionId: 'sess-1' })
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
      status: { working: false, error: true },
    })]
    markSessionRead('sess-1')
    expect(sessions.value[0].status?.error).toBe(false)
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
    setNavigate(() => {/* no-op navigation */})
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
    _rawSessions.value = [makeSession({ id: 'sess-1', cwd: '/dev/p', adapter: 'shell' })]
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
      adapter: 'shell',
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

describe('discovered (host-authoritative)', () => {
  beforeEach(() => {
    _rawSessions.value = []
    _setRawWorld({ projects: [], peers: [], peerProjects: {}, peerDiscovered: {} })
  })
  afterEach(() => {
    _rawSessions.value = []
    _setRawWorld({ projects: [], peers: [], peerProjects: {}, peerDiscovered: {} })
  })

  it('merges local discovery with a connected peer\'s advertised list', () => {
    _rawSessions.value = [
      makeSession({ id: 'local', cwd: '/work/local', alive: true, created_at: '2026-01-01T00:00:00Z' }),
    ]
    _setRawWorld({
      peers: [{ name: 'tower', url: '', status: 'connected', session_count: 0 }],
      peerDiscovered: {
        tower: [{
          suggested_slug: 'apps', remote: 'github.com/mgabor3141/apps',
          paths: ['/mnt/user/apps'], session_count: 1, active_count: 1,
          last_active: '2026-02-01T00:00:00Z',
        }],
      },
    })
    const out = discovered.value
    expect(out).toHaveLength(2)
    // Peer row is more recent, so it sorts first.
    expect(out[0]).toMatchObject({ suggested_slug: 'apps', peer: 'tower' })
    expect(out[1]).toMatchObject({ paths: ['/work/local'] })
    expect(out[1].peer).toBeUndefined()
  })

  it('drops a disconnected peer\'s advertised discovered rows', () => {
    _setRawWorld({
      peers: [{ name: 'tower', url: '', status: 'disconnected', session_count: 0 }],
      peerDiscovered: {
        tower: [{ suggested_slug: 'apps', paths: ['/mnt/user/apps'], session_count: 1, active_count: 1 }],
      },
    })
    expect(discovered.value).toEqual([])
  })

  it('does not recompute peer sessions locally', () => {
    // A peer session present in the snapshot must NOT generate a
    // discovered row on the viewer side; only the peer's own
    // advertised list (peerDiscovered) counts.
    _rawSessions.value = [
      makeSession({ id: 's@tower', cwd: '/mnt/user/apps', alive: true, peer: 'tower' }),
    ]
    _setRawWorld({
      peers: [{ name: 'tower', url: '', status: 'connected', session_count: 1 }],
      peerDiscovered: {},
    })
    expect(discovered.value).toEqual([])
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

describe('unreadCount (sidebar-only attention blip)', () => {
  beforeEach(() => {
    _rawSessions.value = []
    _setRawWorld({ projects: [{ slug: 'proj', match: [{ path: '/work' }] }], peers: [] })
    sessionsLoaded.value = true
    worldLoaded.value = true
    urlPath.value = '/'
  })

  it('excludes discovered (unstamped) sessions even when alive + unread', () => {
    _rawSessions.value = [
      makeSession({ id: 'disc', cwd: '/work', alive: true, unread: true }), // no project_slug
    ]
    expect(unreadCount.value).toBe(0)
  })

  it('counts alive + unread sessions stamped into a folder', () => {
    _rawSessions.value = [
      makeSession({ id: 'a', cwd: '/work', alive: true, unread: true, project_slug: 'proj' }),
      makeSession({ id: 'b', cwd: '/work', alive: true, unread: false, project_slug: 'proj' }),
      makeSession({ id: 'c', cwd: '/work', alive: false, unread: true, project_slug: 'proj' }),
    ]
    expect(unreadCount.value).toBe(1)
  })

  it('excludes the currently-selected session', () => {
    _rawSessions.value = [
      makeSession({ id: 'a', cwd: '/work', adapter: 'shell', slug: 'aa', alive: true, unread: true, project_slug: 'proj' }),
      makeSession({ id: 'b', cwd: '/work', adapter: 'shell', slug: 'bb', alive: true, unread: true, project_slug: 'proj' }),
    ]
    expect(unreadCount.value).toBe(2)
    urlPath.value = '/proj/shell/aa'
    expect(selectedId.value).toBe('a')
    expect(unreadCount.value).toBe(1)
  })

  it('ignores the ?project=/?cwd= view filter (global signal)', () => {
    _rawSessions.value = [
      makeSession({ id: 'a', cwd: '/work', alive: true, unread: true, project_slug: 'proj' }),
    ]
    // A cwd filter that excludes the unread session from the visible
    // sidebar must not zero out the global blip.
    urlSearch.value = '?cwd=/elsewhere'
    expect(unreadCount.value).toBe(1)
  })
})

describe('localHostLabel', () => {
  beforeEach(() => {
    _rawSessions.value = []
    _setRawWorld({ projects: [], peers: [], health: null })
  })

  it('is undefined when every folder is local (single host)', () => {
    _setRawWorld({
      health: { version: 'dev', hostname: 'workstation' },
      projects: [
        { slug: 'a', match: [{ path: '/a' }] },
        { slug: 'b', match: [{ path: '/b' }] },
      ],
    })
    expect(localHostLabel.value).toBeUndefined()
  })

  it('yields the local hostname once a peer reference adds a second host', () => {
    _setRawWorld({
      health: { version: 'dev', hostname: 'workstation' },
      projects: [
        { slug: 'a', match: [{ path: '/a' }] },
        { slug: 'b', peer: 'unraid' },
      ],
    })
    expect(localHostLabel.value).toBe('workstation')
  })

  it('is undefined in multi-host mode when the daemon has not reported a hostname', () => {
    _setRawWorld({
      health: { version: 'dev' },
      projects: [
        { slug: 'a', match: [{ path: '/a' }] },
        { slug: 'b', peer: 'unraid' },
      ],
    })
    expect(localHostLabel.value).toBeUndefined()
  })

  it('is undefined when only peer references exist but all share one host', () => {
    _setRawWorld({
      health: { version: 'dev', hostname: 'workstation' },
      projects: [{ slug: 'b', peer: 'unraid' }],
    })
    expect(localHostLabel.value).toBeUndefined()
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
        makeSession({ id: 'a', unread: true, status: { working: false, error: true } }),
        makeSession({ id: 'b', unread: true }),
      ]
      const m: PendingMutation = { kind: 'mark-read', id: 'a', at: 0 }
      const out = applyPending(sess, [], [m])
      expect(out.sessions[0].unread).toBe(false)
      expect(out.sessions[0].status?.error).toBe(false)
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


describe('parseConnectURL', () => {
  it('splits a pasted connect URL into origin + token', () => {
    expect(parseConnectURL('https://gmux-host.tailnet.ts.net/auth/login?token=abc123'))
      .toEqual({ url: 'https://gmux-host.tailnet.ts.net', token: 'abc123' })
  })

  it('handles a token on the bare origin (no /auth/login path)', () => {
    expect(parseConnectURL('https://gmux-host.tailnet.ts.net/?token=xyz'))
      .toEqual({ url: 'https://gmux-host.tailnet.ts.net', token: 'xyz' })
  })

  it('returns null for a plain URL with no token param', () => {
    expect(parseConnectURL('https://gmux-host.tailnet.ts.net')).toBeNull()
  })

  it('returns null for non-URL input so the separate token field is used', () => {
    expect(parseConnectURL('gmux-host')).toBeNull()
  })
})

describe('conversation_file (duplicate-open warning)', () => {
  it('carries conversation_file from the wire through to the UI session', () => {
    // Guards the two gaps that silently disabled the warning: the protocol
    // Zod schema must keep conversation_file (not strip it), and toUISession must
    // map it through.
    const parsed = SessionSchema.parse({ id: 'a', alive: true, conversation_file: '/conv.jsonl' })
    expect(parsed.conversation_file).toBe('/conv.jsonl')
    expect(toUISession(parsed).conversation_file).toBe('/conv.jsonl')
  })

  it('flags a conversation that is live in more than one tab', () => {
    _rawSessions.value = [
      makeSession({ id: 'a', alive: true, conversation_file: '/conv.jsonl' }),
      makeSession({ id: 'b', alive: true, conversation_file: '/conv.jsonl' }),
      makeSession({ id: 'c', alive: true, conversation_file: '/other.jsonl' }),
      makeSession({ id: 'd', alive: false, conversation_file: '/conv.jsonl' }), // dead doesn't count
    ]
    const dups = duplicateConversationFiles.value
    expect(dups.has('/conv.jsonl')).toBe(true)
    expect(dups.has('/other.jsonl')).toBe(false)
  })
})
