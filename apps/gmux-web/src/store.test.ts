import { describe, it, expect, beforeEach, vi, afterEach } from 'vitest'
import { toasts } from './toasts'
import {
  sessions, sessionsLoaded, worldLoaded, projects, upsertSession, removeSession,
  markSessionRead, dismissSession, reorderSessions, resumeSession, killSession,
  promoteSession, demoteSession, promotionPending, beginPromotion, settlePromotion,
  handleActivity, isSessionActive, isSessionFading, activityMap,
  sessionStaleness, peers, peerAppearance, peerStatusByName,
  isSessionUnavailable, urlPath, urlSearch, urlHash, filteredSessions, sidebarSessions, selectedId, familySelectedId, folders,
  navigateToSession, setNavigate, navigate, tabHref, initStore,
  applyPending, _rawSessions, _setRawWorld, _pendingMutations,
  applySessionsSnapshot,
  toUISession, localHostLabel, parseConnectURL, unreadCount, discovered,
  view, duplicateConversationFiles,
  sidebarMode, setSidebarMode, setFilterSelectors, setHostFilter, homePartition,
  sidebarActivity, setAliveOnly, familyDotById, createViewConsumptionTracker,
  familyActivityById, selectedFamilyChild, ownDotState,
  familySlotById, recentFamilyChildren, rememberFamilyChild,
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
  promotionPending.value = new Map()
  sessionsLoaded.value = false
  worldLoaded.value = false
  urlPath.value = '/'
  urlSearch.value = ''
  urlHash.value = ''
  sidebarMode.value = 'projects'
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

describe('reorder failures are surfaced, never silent', () => {
  beforeEach(() => { toasts.value = []; _pendingMutations.value = [] })
  afterEach(() => { vi.unstubAllGlobals(); toasts.value = []; _pendingMutations.value = [] })

  it('toasts when the server rejects a local reorder', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({
      ok: false, status: 500, statusText: 'Internal Server Error',
      text: () => Promise.resolve(JSON.stringify({ error: { message: 'failed to save projects' } })),
    }))
    await reorderSessions('gmux', ['y', 'x'])
    expect(toasts.value[0]).toMatchObject({
      kind: 'error', message: 'Reorder failed: failed to save projects',
    })
    // No overlay to retract: the UI still shows the server's order, which
    // after a rejection is still the truth.
    expect(_pendingMutations.value).toHaveLength(0)
  })

  it('toasts when a peer reorder is rejected', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({
      ok: false, status: 502, statusText: 'Bad Gateway',
      text: () => Promise.resolve(''),
    }))
    await reorderSessions('gmux', ['y', 'x'], 'tower')
    expect(toasts.value[0].message).toBe('Reorder failed: Bad Gateway')
  })

  it('stays silent on a network reject (owned by the reconnecting pill)', async () => {
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('Failed to fetch')))
    await reorderSessions('gmux', ['y', 'x'])
    expect(toasts.value).toHaveLength(0)
    expect(_pendingMutations.value).toHaveLength(0)
  })
})

describe('filteredSessions reactivity to the URL query string', () => {
  it('recomputes when urlSearch flips, without an SSE session update', () => {
    _rawSessions.value = [
      makeSession({ id: 'a', project_slug: 'alpha' }),
      makeSession({ id: 'b', project_slug: 'beta', peer: 'server' }),
    ]
    // No query: all sessions pass through.
    expect(filteredSessions.value.map(s => s.id)).toEqual(['a', 'b'])

    // Flip only the query signal (no sessions.value change): the
    // ?filter= selectors apply.
    urlSearch.value = '?filter=alpha'
    expect(filteredSessions.value.map(s => s.id)).toEqual(['a'])

    // Host-wide selector, again query-only.
    urlSearch.value = '?filter=*@server'
    expect(filteredSessions.value.map(s => s.id)).toEqual(['b'])

    // Union of selectors.
    urlSearch.value = '?filter=alpha,*@server'
    expect(filteredSessions.value.map(s => s.id)).toEqual(['a', 'b'])

    // Clearing the query restores the full list.
    urlSearch.value = ''
    expect(filteredSessions.value.map(s => s.id)).toEqual(['a', 'b'])
  })
})

describe('filter never evicts the selected session', () => {
  beforeEach(() => {
    _setRawWorld({
      projects: [
        { slug: 'alpha', match: [{ path: '/a' }] },
        { slug: 'beta', match: [{ path: '/b' }] },
      ],
      peers: [],
    })
    _rawSessions.value = [
      makeSession({ id: 'a', cwd: '/a', adapter: 'shell', slug: 'aa', project_slug: 'alpha' }),
      makeSession({ id: 'b', cwd: '/b', adapter: 'shell', slug: 'bb', project_slug: 'beta' }),
    ]
    sessionsLoaded.value = true
    worldLoaded.value = true
    urlPath.value = '/beta/shell/bb'
  })

  it('routing is filter-blind: a filter excluding the open session keeps the session view', () => {
    expect(view.value).toEqual({ kind: 'session', sessionId: 'b' })
    // Narrow the tab to the *other* project: the terminal must stay put,
    // not fall back to the hub/home.
    urlSearch.value = '?filter=alpha'
    expect(view.value).toEqual({ kind: 'session', sessionId: 'b' })
    expect(selectedId.value).toBe('b')
  })

  it('sidebar pins the selected session into the list past the filter', () => {
    urlSearch.value = '?filter=alpha'
    // filteredSessions drops the out-of-scope session...
    expect(filteredSessions.value.map(s => s.id)).toEqual(['a'])
    // ...but the sidebar presentation set keeps the selected one.
    expect(sidebarSessions.value.map(s => s.id).sort()).toEqual(['a', 'b'])
    // ...and its folder is therefore rendered.
    expect(folders.value.some(f => f.slug === 'beta')).toBe(true)
  })

  it('no pin when nothing is selected (equals the filtered set)', () => {
    urlPath.value = '/'
    urlSearch.value = '?filter=alpha'
    expect(selectedId.value).toBeNull()
    expect(sidebarSessions.value.map(s => s.id)).toEqual(['a'])
  })
})

describe('sidebar views share one membership (Projects == Activity)', () => {
  const ids = (buckets: { sessions: { id: string }[] }[]) =>
    buckets.flatMap(b => b.sessions).map(s => s.id).sort()
  const folderIds = () => folders.value.flatMap(f => f.sessions.map(s => s.id)).sort()

  beforeEach(() => {
    _setRawWorld({ projects: [{ slug: 'proj', match: [{ path: '/p' }] }], peers: [] })
    sessionsLoaded.value = true
    worldLoaded.value = true
    urlPath.value = '/'
    urlSearch.value = ''
    setAliveOnly(false)
  })
  afterEach(() => setAliveOnly(false))

  it('hides agent and process children from both views and selects their root row', () => {
    _rawSessions.value = [
      makeSession({ id: 'root', cwd: '/p', adapter: 'pi', slug: 'root', project_slug: 'proj', semantic_agent: true, alive: false, resumable: false }),
      makeSession({ id: 'child', cwd: '/other', adapter: 'pi', slug: 'child', project_slug: 'proj', semantic_agent: true, parent_session_id: 'root' }),
      makeSession({ id: 'helper', cwd: '/p', adapter: 'shell', slug: 'helper', project_slug: 'proj', parent_session_id: 'root' }),
    ]
    urlPath.value = '/proj/pi/child'
    expect(folderIds()).toEqual(['root'])
    expect(ids(sidebarActivity.value)).toEqual(['root'])
    expect(familySelectedId.value).toBe('root')
  })

  it('projects normalized peer family facts as one viewer-visible root row', () => {
    _setRawWorld({
      projects: [{ slug: 'proj', peer: 'tower' }],
      peers: [{ name: 'tower', url: '', status: 'connected', session_count: 2 }],
    })
    const now = new Date().toISOString()
    _rawSessions.value = [
      makeSession({ id: 'root@tower', peer: 'tower', cwd: '/p', adapter: 'pi', slug: 'root', project_slug: 'proj', semantic_agent: true, last_output_at: now }),
      makeSession({ id: 'child@tower', peer: 'tower', cwd: '/p', adapter: 'pi', slug: 'child', project_slug: 'proj', semantic_agent: true, parent_session_id: 'root@tower', last_output_at: now }),
    ]
    expect(folderIds()).toEqual(['root@tower'])
    expect(ids(sidebarActivity.value)).toEqual(['root@tower'])
    expect(homePartition.value.flatMap(bucket => bucket.sessions.map(s => s.id))).toEqual(['root@tower'])
  })

  it('temporarily places an unstamped root from its relevant child folder', () => {
    const root = makeSession({ id: 'root', cwd: '/p', adapter: 'pi', slug: 'root', semantic_agent: true, alive: false, resumable: false })
    const child = makeSession({ id: 'child', cwd: '/p', adapter: 'pi', slug: 'child', project_slug: 'proj', semantic_agent: true, parent_session_id: 'root', alive: true })
    _rawSessions.value = [root, child]

    expect(folders.value.map(folder => [folder.slug, ...folder.sessions.map(s => s.id)])).toEqual([['proj', 'root']])
    expect(ids(sidebarActivity.value)).toEqual(['root'])
    expect(_rawSessions.value.find(s => s.id === 'root')?.project_slug).toBeUndefined()

    _rawSessions.value = [{ ...root, project_slug: 'proj' }, child]
    expect(folders.value.map(folder => [folder.slug, ...folder.sessions.map(s => s.id)])).toEqual([['proj', 'root']])
    expect(ids(sidebarActivity.value)).toEqual(['root'])
    expect(folders.value[0]?.sessions[0]?.project_slug).toBe('proj')
  })

  it('keeps a dead root visible while a live child makes its family relevant', () => {
    _rawSessions.value = [
      makeSession({ id: 'root', cwd: '/p', adapter: 'pi', slug: 'root', project_slug: 'proj', semantic_agent: true, alive: false, resumable: false }),
      makeSession({ id: 'child', cwd: '/p', adapter: 'pi', slug: 'child', project_slug: 'proj', semantic_agent: true, parent_session_id: 'root', alive: true }),
    ]
    expect(folderIds()).toEqual(['root'])
    expect(ids(sidebarActivity.value)).toEqual(['root'])
  })

  it('resolves family edges against the full snapshot under a tab filter', () => {
    _setRawWorld({ projects: [
      { slug: 'root-project', match: [{ path: '/root' }] },
      { slug: 'child-project', match: [{ path: '/child' }] },
    ], peers: [] })
    _rawSessions.value = [
      makeSession({ id: 'root', cwd: '/root', adapter: 'pi', slug: 'root', project_slug: 'root-project', semantic_agent: true, alive: false, resumable: false }),
      makeSession({ id: 'child', cwd: '/child', adapter: 'pi', slug: 'child', project_slug: 'child-project', semantic_agent: true, parent_session_id: 'root', alive: true }),
    ]
    urlSearch.value = '?filter=child-project'
    expect(folderIds()).toEqual(['root'])
  })

  it('keeps malformed cycle members reachable in the sidebar and Home', () => {
    const now = new Date().toISOString()
    _rawSessions.value = [
      makeSession({ id: 'a', cwd: '/p', adapter: 'pi', slug: 'a', project_slug: 'proj', semantic_agent: true, parent_session_id: 'b', last_output_at: now }),
      makeSession({ id: 'b', cwd: '/p', adapter: 'pi', slug: 'b', project_slug: 'proj', semantic_agent: true, parent_session_id: 'a', last_output_at: now }),
    ]
    expect(folderIds()).toEqual(['a', 'b'])
    expect(homePartition.value.flatMap(bucket => bucket.sessions.map(s => s.id)).sort()).toEqual(['a', 'b'])
  })

  it('groups process children only under resolved agent parents', () => {
    _rawSessions.value = [
      makeSession({ id: 'agent-parent', cwd: '/p', adapter: 'pi', project_slug: 'proj', semantic_agent: true }),
      makeSession({ id: 'shell-child', cwd: '/p', adapter: 'shell', project_slug: 'proj', parent_session_id: 'agent-parent' }),
      makeSession({ id: 'shell-parent', cwd: '/p', adapter: 'shell', project_slug: 'proj' }),
      makeSession({ id: 'agent-under-shell', cwd: '/p', adapter: 'pi', project_slug: 'proj', semantic_agent: true, parent_session_id: 'shell-parent' }),
      makeSession({ id: 'orphan', cwd: '/p', adapter: 'pi', project_slug: 'proj', semantic_agent: true, parent_session_id: 'missing' }),
    ]
    expect(folderIds()).toEqual(['agent-parent', 'agent-under-shell', 'orphan', 'shell-parent'])
    expect(ids(sidebarActivity.value)).toEqual(folderIds())
  })

  it('Activity lists exactly the sessions the folders do (alive, resumable, dead-selected)', () => {
    _rawSessions.value = [
      makeSession({ id: 'live', cwd: '/p', adapter: 'shell', slug: 'lv', alive: true, project_slug: 'proj' }),
      makeSession({ id: 'resumable', cwd: '/p', adapter: 'shell', slug: 'rs', alive: false, resumable: true, project_slug: 'proj' }),
    ]
    // No selection: folders and Activity agree.
    expect(ids(sidebarActivity.value)).toEqual(folderIds())
    expect(ids(sidebarActivity.value)).toEqual(['live', 'resumable'])
  })

  it('keeps Activity aligned with Projects while recovered sessions are unstamped', () => {
    // gmuxd emits recovered sessions before its asynchronous project
    // stamping pass completes. They are sidebar-eligible but cannot be
    // placed in a project folder yet, so neither view should render them.
    _rawSessions.value = [
      makeSession({ id: 'recovered', cwd: '/p', adapter: 'shell', slug: 'rc', alive: true }),
    ]
    expect(sidebarSessions.value.map(s => s.id)).toEqual(['recovered'])
    expect(folderIds()).toEqual([])
    expect(ids(sidebarActivity.value)).toEqual([])

    // The later sessions snapshot carries the stamp and both views
    // repopulate from the same membership in the same update.
    _rawSessions.value = [
      makeSession({ id: 'recovered', cwd: '/p', adapter: 'shell', slug: 'rc', alive: true, project_slug: 'proj' }),
    ]
    expect(folderIds()).toEqual(['recovered'])
    expect(ids(sidebarActivity.value)).toEqual(['recovered'])
  })

  it('keeps a resumable-dead session reachable in Activity (Finding 1)', () => {
    // The original bug: Activity ran through partitionForHome, which
    // dropped dead sessions, so a resumable corpse open in the terminal
    // had no Activity row. partitionByDay day-buckets it like anything
    // else (dead included), so both views agree on membership.
    _rawSessions.value = [
      makeSession({ id: 'live', cwd: '/p', adapter: 'shell', slug: 'lv', alive: true, last_output_at: new Date().toISOString(), project_slug: 'proj' }),
      makeSession({ id: 'corpse', cwd: '/p', adapter: 'shell', slug: 'cp', alive: false, resumable: true, project_slug: 'proj' }),
    ]
    expect(ids(sidebarActivity.value)).toEqual(folderIds())
    expect(ids(sidebarActivity.value)).toEqual(['corpse', 'live'])
  })

  it('alive-only narrows both views identically (but keeps the selected)', () => {
    _rawSessions.value = [
      makeSession({ id: 'live', cwd: '/p', adapter: 'shell', slug: 'lv', alive: true, project_slug: 'proj' }),
      makeSession({ id: 'resumable', cwd: '/p', adapter: 'shell', slug: 'rs', alive: false, resumable: true, project_slug: 'proj' }),
    ]
    setAliveOnly(true)
    expect(ids(sidebarActivity.value)).toEqual(folderIds())
    expect(ids(sidebarActivity.value)).toEqual(['live'])
    // Selecting the resumable one keeps it visible in both despite alive-only.
    urlPath.value = '/proj/shell/rs'
    expect(selectedId.value).toBe('resumable')
    expect(folderIds()).toEqual(['live', 'resumable'])
    expect(ids(sidebarActivity.value)).toEqual(['live', 'resumable'])
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
      id: '1vshk4fu', alive: true,
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
      id: '1vshk4fu', alive: true,
      project_slug: 'gmux', project_index: 0,
    } as any)
    expect(ui.project_index).toBe(0)
  })

  it('leaves stamps undefined when the wire omits them', () => {
    const ui = toUISession({
      id: '1vshk4fu', alive: true,
    } as any)
    expect(ui.project_slug).toBeUndefined()
  })

  it('passes last_output_at through from the wire', () => {
    // The owning daemon stamps this; the UI uses it for the home
    // dashboard's Recent section sort. Pure passthrough at the
    // boundary; no client-side derivation.
    const ui = toUISession({
      id: '1vshk4fu', alive: true,
      last_output_at: '2026-01-15T08:00:00Z',
    } as any)
    expect(ui.last_output_at).toBe('2026-01-15T08:00:00Z')
  })

  it('leaves last_output_at undefined when the wire omits it', () => {
    const ui = toUISession({ id: '1vshk4fu', alive: true } as any)
    expect(ui.last_output_at).toBeUndefined()
  })

  it('treats empty-string project_slug as unstamped', () => {
    // Go's omitempty drops empty strings, but legacy / dev paths may
    // emit them. buildProjectFolders treats an empty stamp the same
    // as no stamp; normalize at the boundary so consumers never
    // see the difference.
    const ui = toUISession({
      id: '1vshk4fu', alive: true, project_slug: '',
    } as any)
    expect(ui.project_slug).toBeUndefined()
  })
})

describe('upsertSession', () => {
  it('inserts a new session and returns true', () => {
    const isNew = upsertSession({
      id: '1vshk4fu', alive: true, cwd: '/home/user',
      command: ['/bin/sh'], adapter: 'shell',
    } as any)
    expect(isNew).toBe(true)
    expect(sessions.value).toHaveLength(1)
    expect(sessions.value[0].id).toBe('1vshk4fu')
  })

  it('updates an existing session and returns false', () => {
    _rawSessions.value = [makeSession({ id: '1vshk4fu', title: 'old' })]
    const isNew = upsertSession({
      id: '1vshk4fu', alive: true, title: 'new',
      cwd: '/home/user', command: ['/bin/sh'], adapter: 'shell',
    } as any)
    expect(isNew).toBe(false)
    expect(sessions.value).toHaveLength(1)
    expect(sessions.value[0].title).toBe('new')
  })

  it('preserves other sessions during update', () => {
    _rawSessions.value = [
      makeSession({ id: '1vshk4fu', title: 'first' }),
      makeSession({ id: '155mk8b7', title: 'second' }),
    ]
    upsertSession({
      id: '1vshk4fu', alive: false, title: 'updated',
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
      makeSession({ id: '1vshk4fu', cwd: '/dev/project', adapter: 'pi', slug: 'fix-auth' }),
    ]
    // Simulate the session being selected via URL.
    urlPath.value = '/myproject/pi/fix-auth'
    expect(selectedId.value).toBe('1vshk4fu')

    // SSE upserts with a new slug (e.g., /new changed the active file).
    upsertSession({
      id: '1vshk4fu', alive: true, cwd: '/dev/project', adapter: 'pi',
      slug: 'refactor-login', command: ['pi'], title: 'pi',
    } as any)

    // URL should be atomically rewritten; session stays selected.
    expect(urlPath.value).toBe('/myproject/pi/refactor-login')
    expect(selectedId.value).toBe('1vshk4fu')
  })

  it('does not rewrite URL when a non-selected session slug changes', () => {
    const testProjects: ProjectItem[] = [
      { slug: 'myproject', match: [{ path: '/dev/project' }] },
    ]
    _setRawWorld({ projects: testProjects })
    sessionsLoaded.value = true
    worldLoaded.value = true
    _rawSessions.value = [
      makeSession({ id: '1vshk4fu', cwd: '/dev/project', adapter: 'pi', slug: 'fix-auth' }),
      makeSession({ id: '155mk8b7', cwd: '/dev/project', adapter: 'pi', slug: 'old-slug' }),
    ]
    urlPath.value = '/myproject/pi/fix-auth'
    expect(selectedId.value).toBe('1vshk4fu')

    // 155mk8b7's slug changes, but it's not the selected session.
    upsertSession({
      id: '155mk8b7', alive: true, cwd: '/dev/project', adapter: 'pi',
      slug: 'new-slug', command: ['pi'], title: 'pi',
    } as any)

    // URL should be unchanged.
    expect(urlPath.value).toBe('/myproject/pi/fix-auth')
    expect(selectedId.value).toBe('1vshk4fu')
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
      makeSession({ id: '1vshk4fu', cwd: '/dev/project', adapter: 'pi', slug: 'fix-auth' }),
    ]
    sessionsLoaded.value = true
    worldLoaded.value = true
    urlPath.value = '/myproject/pi/fix-auth'
  })

  it('rewrites the URL and keeps the session view when the selected slug changes', () => {
    expect(view.value).toEqual({ kind: 'session', sessionId: '1vshk4fu' })

    // Snapshot: same id, resumed slug, brand-new array (the wire shape).
    applySessionsSnapshot([
      makeSession({ id: '1vshk4fu', cwd: '/dev/project', adapter: 'pi', slug: 'refactor-login' }),
    ])

    expect(urlPath.value).toBe('/myproject/pi/refactor-login')
    // The user stays on the terminal — no boot to the project hub.
    expect(view.value).toEqual({ kind: 'session', sessionId: '1vshk4fu' })
    // Address bar synced via replaceState (replace=true), so back/forward
    // history isn't polluted.
    expect(navCalls).toContainEqual(['/myproject/pi/refactor-login', true])
  })

  it('atomically switches the selected row to its full ID and carries route state when a duplicate slug arrives', () => {
    expect(view.value).toEqual({ kind: 'session', sessionId: '1vshk4fu' })
    urlSearch.value = '?filter=myproject'
    urlHash.value = '#terminal'
    sidebarMode.value = 'activity'

    applySessionsSnapshot([
      makeSession({ id: '1vshk4fu', cwd: '/dev/project', adapter: 'pi', slug: 'fix-auth', alive: true }),
      makeSession({ id: '1eha7rdu', cwd: '/dev/project', adapter: 'pi', slug: 'fix-auth', alive: false }),
    ])

    expect(urlPath.value).toBe('/myproject/pi/~1vshk4fu')
    expect(view.value).toEqual({ kind: 'session', sessionId: '1vshk4fu' })
    expect(navCalls).toContainEqual([
      '/myproject/pi/~1vshk4fu?filter=myproject&sidebar=activity#terminal', true,
    ])
  })

  it('preserves the tab filter across a slug-rewrite navigation', () => {
    // Regression: the canonicalization rewrite used to navigate with the
    // bare path, stripping ?filter= (and ?sidebar=) from a pinned tab.
    urlSearch.value = '?filter=myproject'
    sidebarMode.value = 'activity'

    applySessionsSnapshot([
      makeSession({ id: '1vshk4fu', cwd: '/dev/project', adapter: 'pi', slug: 'refactor-login' }),
    ])

    expect(navCalls).toContainEqual(
      ['/myproject/pi/refactor-login?filter=myproject&sidebar=activity', true],
    )
  })

  it('leaves the URL untouched when the selected slug is unchanged', () => {
    applySessionsSnapshot([
      makeSession({ id: '1vshk4fu', cwd: '/dev/project', adapter: 'pi', slug: 'fix-auth', title: 'now working' }),
    ])

    expect(urlPath.value).toBe('/myproject/pi/fix-auth')
    expect(view.value).toEqual({ kind: 'session', sessionId: '1vshk4fu' })
    expect(navCalls).toEqual([])
  })

  it('does not rewrite when a non-selected session changes slug', () => {
    _rawSessions.value = [
      makeSession({ id: '1vshk4fu', cwd: '/dev/project', adapter: 'pi', slug: 'fix-auth' }),
      makeSession({ id: '155mk8b7', cwd: '/dev/project', adapter: 'pi', slug: 'old' }),
    ]

    applySessionsSnapshot([
      makeSession({ id: '1vshk4fu', cwd: '/dev/project', adapter: 'pi', slug: 'fix-auth' }),
      makeSession({ id: '155mk8b7', cwd: '/dev/project', adapter: 'pi', slug: 'new' }),
    ])

    expect(urlPath.value).toBe('/myproject/pi/fix-auth')
    expect(navCalls).toEqual([])
  })

  it('boots home only when the selected session is genuinely gone', () => {
    // A snapshot that drops the session (killed) — distinct from a slug
    // change — should fall through to the normal commit and let the view
    // resolve home (project hubs are retired).
    applySessionsSnapshot([])

    expect(view.value).toEqual({ kind: 'home' })
  })

  it('still commits the snapshot (loaded flags flip) when nothing is selected', () => {
    urlPath.value = '/'
    sessionsLoaded.value = false
    applySessionsSnapshot([
      makeSession({ id: '12ak7jhz', cwd: '/dev/project', adapter: 'pi', slug: 'x' }),
    ])
    expect(sessionsLoaded.value).toBe(true)
    expect(sessions.value.map(s => s.id)).toContain('12ak7jhz')
    expect(navCalls).toEqual([])
  })
})

describe('promote/demote pending request ownership', () => {
  it('settles only the matching request token, including across sessions', () => {
    const sessionA = beginPromotion('session-a', 'promote')
    const sessionB = beginPromotion('session-b', 'demote')

    settlePromotion('session-a', sessionB)
    expect(promotionPending.value.get('session-a')?.seq).toBe(sessionA)
    expect(promotionPending.value.get('session-b')?.seq).toBe(sessionB)

    settlePromotion('session-a', sessionA)
    expect(promotionPending.value.has('session-a')).toBe(false)
    expect(promotionPending.value.has('session-b')).toBe(true)
  })

  it('does not let an older request clear a newer request for one session', () => {
    const first = beginPromotion('session', 'promote')
    const second = beginPromotion('session', 'promote')

    settlePromotion('session', first)
    expect(promotionPending.value.get('session')?.seq).toBe(second)
    settlePromotion('session', second)
    expect(promotionPending.value.has('session')).toBe(false)
  })
})

describe('promote/demote: endpoint wiring and failure surfacing', () => {
  beforeEach(() => { toasts.value = [] })
  afterEach(() => { vi.unstubAllGlobals(); toasts.value = [] })

  it('hits the #475 endpoints with POST', async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true, status: 200, statusText: 'OK', text: () => Promise.resolve(''),
    })
    vi.stubGlobal('fetch', fetchMock)
    await expect(promoteSession('kid1')).resolves.toBe(true)
    expect(fetchMock).toHaveBeenCalledWith('/v1/sessions/kid1/promote', { method: 'POST' })
    await expect(demoteSession('kid1')).resolves.toBe(true)
    expect(fetchMock).toHaveBeenCalledWith('/v1/sessions/kid1/demote', { method: 'POST' })
    expect(toasts.value).toHaveLength(0)
  })

  it('toasts the daemon message on rejection (e.g. local_only for a raced peer)', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({
      ok: false, status: 400, statusText: 'Bad Request',
      text: () => Promise.resolve(JSON.stringify({
        ok: false, error: { code: 'local_only', message: 'promote is only available for sessions owned by this daemon; run gmux on the owning host' },
      })),
    }))
    await expect(promoteSession('peer-kid')).resolves.toBe(false)
    expect(toasts.value[0]).toMatchObject({
      kind: 'error',
      message: 'Promote failed: promote is only available for sessions owned by this daemon; run gmux on the owning host',
    })
    // No optimistic overlay exists for promotion, so there is nothing to
    // retract and the sidebar keeps showing the server's truth throughout.
    expect(_pendingMutations.value).toHaveLength(0)
  })

  it('demote failures label the action in the user\u2019s words', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({
      ok: false, status: 404, statusText: 'Not Found',
      text: () => Promise.resolve(JSON.stringify({ error: { message: 'session not found' } })),
    }))
    await expect(demoteSession('ghost')).resolves.toBe(false)
    expect(toasts.value[0].message).toBe('Return to family failed: session not found')
  })

  it('stays silent on a network reject (connectivity is the pill\u2019s story)', async () => {
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('Failed to fetch')))
    await expect(promoteSession('kid1')).resolves.toBe(false)
    expect(toasts.value).toHaveLength(0)
  })
})

describe('promotion snapshots preserve the selected session\u2019s routing', () => {
  // Promotion changes which project a session presents under: an
  // unpromoted child routes through its root's project, a promoted one
  // through its own project_slug. The authoritative SSE snapshot must
  // therefore land together with a URL rewrite (commitWithSlugRewrite),
  // or the old URL stops resolving and the router boots the user home —
  // the "false root state" failure mode. These pin the seam for both
  // directions with the worst case: the child's own project differs
  // from the root's.
  const navCalls: Array<[string, boolean | undefined]> = []
  const rootSession = () => makeSession({
    id: '1aaaaaaa', cwd: '/dev/alpha', adapter: 'pi', slug: 'orchestrator',
    semantic_agent: true, project_slug: 'alpha',
  })
  const childSession = (promoted: boolean) => makeSession({
    id: '1bbbbbbb', cwd: '/dev/beta', adapter: 'pi', slug: 'worker',
    semantic_agent: true, parent_session_id: '1aaaaaaa',
    project_slug: 'beta', promoted_to_root: promoted,
  })
  beforeEach(() => {
    navCalls.length = 0
    setNavigate((url, replace) => { navCalls.push([url, replace]) })
    _setRawWorld({ projects: [
      { slug: 'alpha', match: [{ path: '/dev/alpha' }] },
      { slug: 'beta', match: [{ path: '/dev/beta' }] },
    ] })
    sessionsLoaded.value = true
    worldLoaded.value = true
  })
  afterEach(() => { setNavigate(() => {/* no-op */}) })

  it('promote: URL moves from the root\u2019s project to the child\u2019s own, view stays put', () => {
    _rawSessions.value = [rootSession(), childSession(false)]
    urlPath.value = '/alpha/pi/worker'
    expect(view.value).toEqual({ kind: 'session', sessionId: '1bbbbbbb' })

    applySessionsSnapshot([rootSession(), childSession(true)])

    expect(urlPath.value).toBe('/beta/pi/worker')
    expect(view.value).toEqual({ kind: 'session', sessionId: '1bbbbbbb' })
    expect(navCalls).toContainEqual(['/beta/pi/worker', true])
  })

  it('demote: URL rejoins the root\u2019s project, view stays put', () => {
    _rawSessions.value = [rootSession(), childSession(true)]
    urlPath.value = '/beta/pi/worker'
    expect(view.value).toEqual({ kind: 'session', sessionId: '1bbbbbbb' })

    applySessionsSnapshot([rootSession(), childSession(false)])

    expect(urlPath.value).toBe('/alpha/pi/worker')
    expect(view.value).toEqual({ kind: 'session', sessionId: '1bbbbbbb' })
    expect(navCalls).toContainEqual(['/alpha/pi/worker', true])
  })

  it('promoting a NON-selected session never touches the URL', () => {
    _rawSessions.value = [rootSession(), childSession(false)]
    urlPath.value = '/alpha/pi/orchestrator'
    expect(view.value).toEqual({ kind: 'session', sessionId: '1aaaaaaa' })

    applySessionsSnapshot([rootSession(), childSession(true)])

    expect(urlPath.value).toBe('/alpha/pi/orchestrator')
    expect(navCalls).toEqual([])
  })

  it('promote into a slug collision: the selected child switches to its ~id URL', () => {
    // Project beta already holds a pi session slugged `worker`. Promoting the
    // selected child (same adapter, same slug) into beta makes the plain slug
    // ambiguous, so the canonical URL must switch to the exact-id form in the
    // same snapshot commit — not resolve to the wrong session or fall home.
    const squatter = makeSession({
      id: '1ccccccc', cwd: '/dev/beta', adapter: 'pi', slug: 'worker', project_slug: 'beta',
    })
    _rawSessions.value = [rootSession(), childSession(false), squatter]
    urlPath.value = '/alpha/pi/worker'
    expect(view.value).toEqual({ kind: 'session', sessionId: '1bbbbbbb' })

    applySessionsSnapshot([rootSession(), childSession(true), squatter])

    expect(urlPath.value).toBe('/beta/pi/~1bbbbbbb')
    expect(view.value).toEqual({ kind: 'session', sessionId: '1bbbbbbb' })
    expect(navCalls).toContainEqual(['/beta/pi/~1bbbbbbb', true])
  })

  it('a NON-selected promotion that collides moves the selected session to its ~id URL', () => {
    // Viewing the beta squatter; someone promotes the child into beta with
    // the same adapter+slug. The squatter's plain-slug URL becomes ambiguous
    // and must be rewritten to its exact-id form, view preserved.
    const squatter = makeSession({
      id: '1ccccccc', cwd: '/dev/beta', adapter: 'pi', slug: 'worker', project_slug: 'beta',
    })
    _rawSessions.value = [rootSession(), childSession(false), squatter]
    urlPath.value = '/beta/pi/worker'
    expect(view.value).toEqual({ kind: 'session', sessionId: '1ccccccc' })

    applySessionsSnapshot([rootSession(), childSession(true), squatter])

    expect(urlPath.value).toBe('/beta/pi/~1ccccccc')
    expect(view.value).toEqual({ kind: 'session', sessionId: '1ccccccc' })
  })

  it('promote reprojects the sidebar: the child earns its own folder row', () => {
    _rawSessions.value = [rootSession(), childSession(false)]
    const before = folders.value
    expect(before.find(f => f.slug === 'beta')?.sessions ?? []).toHaveLength(0)

    applySessionsSnapshot([rootSession(), childSession(true)])

    const after = folders.value
    expect(after.find(f => f.slug === 'beta')?.sessions.map(s => s.id)).toEqual(['1bbbbbbb'])
    expect(after.find(f => f.slug === 'alpha')?.sessions.map(s => s.id)).toEqual(['1aaaaaaa'])
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
      makeSession({ id: '1vshk4fu', cwd: '/dev/project', adapter: 'pi', slug: 'fix-auth', project_slug: 'myproject' }),
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

    expect(view.value).toEqual({ kind: 'session', sessionId: '1vshk4fu' })
    expect(selectedId.value).toBe('1vshk4fu')
  })
})

describe('removeSession', () => {
  it('removes the session with the given id', () => {
    _rawSessions.value = [
      makeSession({ id: '1vshk4fu' }),
      makeSession({ id: '155mk8b7' }),
    ]
    removeSession('1vshk4fu')
    expect(sessions.value.map(s => s.id)).toEqual(['155mk8b7'])
  })

  it('is a no-op for unknown ids', () => {
    _rawSessions.value = [makeSession({ id: '1vshk4fu' })]
    removeSession('ghost')
    expect(sessions.value).toHaveLength(1)
  })
})

describe('markSessionRead', () => {
  // Prevent the actual fetch from firing.
  beforeEach(() => { vi.stubGlobal('fetch', vi.fn().mockResolvedValue({ ok: true })) })
  afterEach(() => { vi.restoreAllMocks() })

  it('clears unread flag on the target session', () => {
    _rawSessions.value = [makeSession({ id: '1vshk4fu', unread: true })]
    markSessionRead('1vshk4fu')
    expect(sessions.value[0].unread).toBe(false)
  })

  it('clears error flag from status', () => {
    _rawSessions.value = [makeSession({
      id: '1vshk4fu',
      status: { active: false, error: true },
    })]
    markSessionRead('1vshk4fu')
    expect(sessions.value[0].status?.error).toBe(false)
  })

  it('does not touch other sessions', () => {
    _rawSessions.value = [
      makeSession({ id: '1vshk4fu', unread: true }),
      makeSession({ id: '155mk8b7', unread: true }),
    ]
    markSessionRead('1vshk4fu')
    expect(sessions.value[0].unread).toBe(false)
    expect(sessions.value[1].unread).toBe(true)
  })

  it('posts the observed unread generation to the server', () => {
    _rawSessions.value = [makeSession({ id: '1vshk4fu', unread: true, unread_token: 'token-7' })]
    markSessionRead('1vshk4fu')
    expect(fetch).toHaveBeenCalledWith('/v1/sessions/1vshk4fu/read?token=token-7', { method: 'POST' })
  })

  it('does not let a delayed view acknowledgement mask a newer completion', () => {
    _rawSessions.value = [makeSession({ id: '1vshk4fu', unread: true, unread_token: 'token-7' })]
    markSessionRead('1vshk4fu')
    expect(sessions.value[0].unread).toBe(false)
    _rawSessions.value = [makeSession({ id: '1vshk4fu', unread: true, unread_token: 'token-8' })]
    expect(sessions.value[0].unread).toBe(true)
    expect(fetch).toHaveBeenCalledWith('/v1/sessions/1vshk4fu/read?token=token-7', { method: 'POST' })
  })
})

describe('focused view consumption', () => {
  afterEach(() => { vi.unstubAllGlobals() })

  it('sets completion unread while open and clears only on the next interaction', () => {
    const tracker = createViewConsumptionTracker()
    const open = makeSession({ id: 'child', unread: false })
    expect(tracker.selection('child', open)).toBeNull()

    const completed = makeSession({ id: 'child', unread: true, unread_token: 'turn-1', alive: false })
    // Snapshot update for the same open session must not consume it.
    expect(tracker.selection('child', completed)).toBeNull()
    expect(tracker.interaction('child', completed)).toEqual({ id: 'child', token: 'turn-1' })
  })

  it('retains the exact viewed completion across a delayed acknowledgement', () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({ ok: true }))
    const tracker = createViewConsumptionTracker()
    const turn1 = makeSession({ id: 'child', unread: true, unread_token: 'turn-1' })
    const observed = tracker.interaction('child', turn1)
    expect(observed).toEqual({ id: 'child', token: 'turn-1' })

    // N+1 arrives after the interaction observed N but before /read is issued.
    _rawSessions.value = [makeSession({ id: 'child', unread: true, unread_token: 'turn-2' })]
    if (observed) markSessionRead(observed.id, observed.token)

    expect(fetch).toHaveBeenCalledWith('/v1/sessions/child/read?token=turn-1', { method: 'POST' })
    expect(sessions.value[0].unread).toBe(true)
  })

  it('consumes unread that already exists when entering a session', () => {
    const tracker = createViewConsumptionTracker()
    expect(tracker.selection('child', makeSession({ id: 'child', unread: true, unread_token: 'turn-1' })))
      .toEqual({ id: 'child', token: 'turn-1' })
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
    handleActivity('1vshk4fu')
    expect(isSessionActive('1vshk4fu')).toBe(true)
    expect(isSessionFading('1vshk4fu')).toBe(false)
  })

  it('transitions to fading after the active window', () => {
    handleActivity('1vshk4fu')
    vi.advanceTimersByTime(3000)
    expect(isSessionActive('1vshk4fu')).toBe(false)
    expect(isSessionFading('1vshk4fu')).toBe(true)
  })

  it('clears completely after fade-out', () => {
    handleActivity('1vshk4fu')
    vi.advanceTimersByTime(3000 + 800)
    expect(isSessionActive('1vshk4fu')).toBe(false)
    expect(isSessionFading('1vshk4fu')).toBe(false)
  })

  it('resets the timer when activity fires again', () => {
    handleActivity('1vshk4fu')
    vi.advanceTimersByTime(2000) // still active
    handleActivity('1vshk4fu')     // reset
    vi.advanceTimersByTime(2000) // 2s since reset, still active
    expect(isSessionActive('1vshk4fu')).toBe(true)
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
    _rawSessions.value = [makeSession({ id: '1vshk4fu', cwd: '/dev/p' })]
    // projects left empty: simulates the snapshot.sessions-vs-snapshot.world
    // race where sessions arrive before projects.
    expect(navigateToSession('1vshk4fu')).toBe(false)
    expect(navigateMock).not.toHaveBeenCalled()
  })

  it('returns true and dispatches the exact-ID URL with applicable route context once loaded', () => {
    _setRawWorld({ projects: [{ slug: 'myproject', match: [{ path: '/dev/p' }] }] })
    _rawSessions.value = [makeSession({ id: '1vshk4fu', cwd: '/dev/p', adapter: 'shell' })]
    urlSearch.value = '?filter=myproject&mock=&host=server'
    sidebarMode.value = 'activity'
    expect(navigateToSession('1vshk4fu', true)).toBe(true)
    expect(navigateMock).toHaveBeenCalledTimes(1)
    expect(navigateMock).toHaveBeenCalledWith(
      '/myproject/shell/~1vshk4fu?filter=myproject&sidebar=activity&mock=&host=server',
      true,
    )
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

  it('counts live and retained-dead unread sessions stamped into a folder', () => {
    _rawSessions.value = [
      makeSession({ id: 'a', cwd: '/work', alive: true, unread: true, project_slug: 'proj' }),
      makeSession({ id: 'b', cwd: '/work', alive: true, unread: false, project_slug: 'proj' }),
      makeSession({ id: 'c', cwd: '/work', alive: false, resumable: true, unread: true, project_slug: 'proj' }),
    ]
    expect(unreadCount.value).toBe(2)
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

  it('counts live and retained-dead unread family children toward their folder-visible root', () => {
    _rawSessions.value = [
      makeSession({ id: 'root', cwd: '/work', adapter: 'pi', semantic_agent: true, project_slug: 'proj' }),
      makeSession({ id: 'child', cwd: '/work', adapter: 'pi', semantic_agent: true, parent_session_id: 'root', alive: true, unread: true }),
      makeSession({ id: 'proc', cwd: '/work', parent_session_id: 'root', alive: true, unread: true }),
      makeSession({ id: 'dead-child', cwd: '/work', adapter: 'pi', semantic_agent: true, parent_session_id: 'root', alive: false, unread: true }),
    ]
    // Children have no folder row of their own; the root row stands in and
    // must ping for both live children and the retained-dead unread child.
    expect(unreadCount.value).toBe(3)
  })

  it('excludes the selected child from its root roll-up', () => {
    _rawSessions.value = [
      makeSession({ id: 'root', cwd: '/work', adapter: 'pi', slug: 'rooty', semantic_agent: true, project_slug: 'proj' }),
      makeSession({ id: 'child', cwd: '/work', adapter: 'pi', slug: 'kiddo', semantic_agent: true, parent_session_id: 'root', alive: true, unread: true }),
      makeSession({ id: 'sibling', cwd: '/work', adapter: 'pi', slug: 'sib', semantic_agent: true, parent_session_id: 'root', alive: true, unread: true }),
    ]
    expect(unreadCount.value).toBe(2)
    urlPath.value = '/proj/pi/kiddo'
    expect(selectedId.value).toBe('child')
    // You're looking at `child`; its sibling still pings.
    expect(unreadCount.value).toBe(1)
  })

  it('is scoped to the tab\u2019s ?filter= selectors', () => {
    // A pinned tab must not blink for sessions outside its scope
    // (another tab or a notification covers those), and must still
    // blink for in-scope ones.
    _setRawWorld({
      projects: [
        { slug: 'proj', match: [{ path: '/work' }] },
        { slug: 'other', match: [{ path: '/other' }] },
      ],
      peers: [],
    })
    _rawSessions.value = [
      makeSession({ id: 'in', cwd: '/work', alive: true, unread: true, project_slug: 'proj' }),
      makeSession({ id: 'out', cwd: '/other', alive: true, unread: true, project_slug: 'other' }),
    ]
    expect(unreadCount.value).toBe(2)
    urlSearch.value = '?filter=proj'
    expect(unreadCount.value).toBe(1)
    urlSearch.value = '?filter=elsewhere'
    expect(unreadCount.value).toBe(0)
  })
})

describe('familyDotById (family-aggregated row dot)', () => {
  beforeEach(() => {
    _rawSessions.value = []
    _setRawWorld({ projects: [{ slug: 'proj', match: [{ path: '/work' }] }], peers: [] })
    sessionsLoaded.value = true
    worldLoaded.value = true
    urlPath.value = '/'
    activityMap.value = new Map()
  })

  const pi = (id: string, extra: Partial<Session> = {}) => makeSession({
    id, cwd: '/work', adapter: 'pi', semantic_agent: true, project_slug: 'proj', ...extra,
  })

  it('rolls the highest-precedence member state up to the presentation root', () => {
    _rawSessions.value = [
      pi('root'),
      pi('working-child', { parent_session_id: 'root', status: { active: true } }),
      pi('unread-child', { parent_session_id: 'root', unread: true }),
      makeSession({ id: 'proc', cwd: '/work', parent_session_id: 'root', status: { active: true, error: true } }),
    ]
    // error (process!) > working > unread; the root row shows the max.
    expect(familyDotById.value.get('root')).toBe('error')
    expect(familyDotById.value.get('working-child')).toBeUndefined()
  })

  it('keeps standalone sessions at their own dot state', () => {
    _rawSessions.value = [pi('solo', { unread: true })]
    expect(familyDotById.value.get('solo')).toBe('unread')
  })

  it('lets a never-viewed dead child surface unread on the root', () => {
    // Unread is not alive-gated in sessionDotState, and the roll-up
    // follows the same vocabulary: unseen output pings until viewed.
    _rawSessions.value = [
      pi('root'),
      pi('dead-child', { parent_session_id: 'root', alive: false, unread: true }),
    ]
    expect(familyDotById.value.get('root')).toBe('unread')
  })

  it('mutes only the selected member, not its siblings', () => {
    _rawSessions.value = [
      pi('root', { slug: 'rooty' }),
      pi('a', { slug: 'aa', parent_session_id: 'root', unread: true }),
      pi('b', { slug: 'bb', parent_session_id: 'root', unread: true }),
    ]
    expect(familyDotById.value.get('root')).toBe('unread')
    urlPath.value = '/proj/pi/aa'
    expect(selectedId.value).toBe('a')
    // `a` is muted (you're looking at it) but `b` still pings the root row.
    expect(familyDotById.value.get('root')).toBe('unread')
    _rawSessions.value = _rawSessions.value.map(s => s.id === 'b' ? { ...s, unread: false } : s)
    expect(familyDotById.value.get('root')).toBe('none')
  })
})

describe('sidebar family entry derivations', () => {
  beforeEach(() => {
    _rawSessions.value = []
    _setRawWorld({ projects: [{ slug: 'proj', match: [{ path: '/work' }] }], peers: [] })
    sessionsLoaded.value = true
    worldLoaded.value = true
    urlPath.value = '/'
    activityMap.value = new Map()
  })

  const agent = (id: string, extra: Partial<Session> = {}) => makeSession({
    id, cwd: '/work', adapter: 'pi', semantic_agent: true, project_slug: 'proj', ...extra,
  })
  const proc = (id: string, parent: string, extra: Partial<Session> = {}) => makeSession({
    id, cwd: '/work', adapter: 'shell', parent_session_id: parent, project_slug: 'proj', ...extra,
  })

  describe('familyActivityById (live activity counts)', () => {
    it('buckets every descendant once, by dot precedence, kind-split for working', () => {
      _rawSessions.value = [
        agent('root', { status: { active: true } }),
        agent('kid', { parent_session_id: 'root', status: { active: true } }),
        agent('grandkid', { parent_session_id: 'kid', status: { active: true } }),
        // Error wins over its own unread; working wins over unread.
        agent('boom', { parent_session_id: 'root', status: { active: false, error: true }, unread: true }),
        agent('waiting', { parent_session_id: 'root', unread: true }),
        proc('p1', 'root', { status: { active: true } }),
        proc('p2', 'grandkid', { status: { active: true }, unread: true }),
      ]
      expect(familyActivityById.value.get('root')).toEqual({
        error: 1, waiting: 1, active: 2, running: 2,
      })
      // The root's own working state is never in its family's counts.
      expect(familyActivityById.value.get('kid')).toBeUndefined()
    })

    it('counts the same with the alive-only filter on or off', () => {
      _rawSessions.value = [
        agent('root'),
        agent('live', { parent_session_id: 'root', unread: true }),
        agent('dead', { parent_session_id: 'root', unread: true, alive: false, resumable: true }),
      ]
      expect(familyActivityById.value.get('root')?.waiting).toBe(2)
      // The standard family numbers are a fact about the family, not
      // the viewport: the toggle hides rows, and the panel one click
      // away shows the hidden member regardless, so the count stands.
      setAliveOnly(true)
      expect(familyActivityById.value.get('root')?.waiting).toBe(2)
      setAliveOnly(false)
    })

    it('omits idle families entirely (no line, no entry)', () => {
      _rawSessions.value = [
        agent('root'),
        agent('kid', { parent_session_id: 'root' }),
        proc('p', 'root'),
      ]
      expect(familyActivityById.value.has('root')).toBe(false)
    })

    it('ignores a dead member that is merely alive-gated', () => {
      // status.active/error only count while alive; unread still does,
      // matching sessionDotState's vocabulary.
      _rawSessions.value = [
        agent('root'),
        agent('zombie', { parent_session_id: 'root', alive: false, status: { active: true } }),
        agent('gone', { parent_session_id: 'root', alive: false, status: { active: false, error: true } }),
        agent('unseen', { parent_session_id: 'root', alive: false, unread: true }),
      ]
      expect(familyActivityById.value.get('root')).toEqual({
        error: 0, waiting: 1, active: 0, running: 0,
      })
    })

    beforeEach(() => { recentFamilyChildren.value = [] })

    it('holds the same numbers whichever member the slot row names', () => {
      _rawSessions.value = [
        agent('root'),
        agent('a', { slug: 'aa', parent_session_id: 'root', unread: true }),
        agent('b', { slug: 'bb', parent_session_id: 'root', unread: true }),
      ]
      expect(familyActivityById.value.get('root')?.waiting).toBe(2)
      // Viewing 'a' names it on the slot row; the count does not budge.
      // A summary may include what is separately visible — the panel's
      // tally counts the rows below it too — and subtracting the slot
      // made this number wobble with which member you last visited.
      urlPath.value = '/proj/pi/aa'
      expect(familyActivityById.value.get('root')?.waiting).toBe(2)
      _rawSessions.value = _rawSessions.value.map(s =>
        s.id === 'a' ? { ...s, unread: false, status: { active: true } } : s)
      expect(familyActivityById.value.get('root')).toEqual({
        error: 0, waiting: 1, active: 1, running: 0,
      })
    })

    it('keeps the line when the only activity is the named member', () => {
      _rawSessions.value = [
        agent('root', { slug: 'rooty' }),
        agent('a', { slug: 'aa', parent_session_id: 'root', unread: true }),
      ]
      urlPath.value = '/proj/pi/aa'
      rememberFamilyChild('a')
      expect(familySlotById.value.get('root')?.session.id).toBe('a')
      expect(familyActivityById.value.get('root')?.waiting).toBe(1)
    })

    it('gives a promoted descendant its own counts, not its old family\u2019s', () => {
      _rawSessions.value = [
        agent('root'),
        agent('kid', { parent_session_id: 'root', promoted_to_root: true }),
        proc('p', 'kid', { status: { active: true } }),
      ]
      expect(familyActivityById.value.has('root')).toBe(false)
      expect(familyActivityById.value.get('kid')?.running).toBe(1)
    })
  })

  describe('root-dot semantics', () => {
    it('shows the root\u2019s own status, never the family roll-up', () => {
      _rawSessions.value = [
        agent('root', { unread: true }),
        agent('kid', { parent_session_id: 'root', status: { active: true, error: true } }),
      ]
      const root = sessions.value.find(s => s.id === 'root')!
      // The aggregate would say "error"; the row dot stays the root's own.
      expect(familyDotById.value.get('root')).toBe('error')
      expect(ownDotState(root, activityMap.value, selectedId.value)).toBe('unread')
    })

    it('mutes the root\u2019s own attention while the root is selected', () => {
      _rawSessions.value = [agent('root', { slug: 'rooty', unread: true }), agent('kid', { parent_session_id: 'root' })]
      const root = () => sessions.value.find(s => s.id === 'root')!
      expect(ownDotState(root(), activityMap.value, selectedId.value)).toBe('unread')
      urlPath.value = '/proj/pi/rooty'
      expect(selectedId.value).toBe('root')
      expect(ownDotState(root(), activityMap.value, selectedId.value)).toBe('none')
    })

    it('keeps working/active states visible while selected (only attention mutes)', () => {
      _rawSessions.value = [agent('root', { slug: 'rooty', status: { active: true } })]
      urlPath.value = '/proj/pi/rooty'
      const root = sessions.value.find(s => s.id === 'root')!
      expect(ownDotState(root, activityMap.value, selectedId.value)).toBe('working')
    })
  })

  describe('familySlotById (the one member a family entry shows)', () => {
    beforeEach(() => { recentFamilyChildren.value = [] })

    const slot = (rootId: string) => familySlotById.value.get(rootId)

    it('is empty for a family you have never descended into', () => {
      _rawSessions.value = [agent('root'), agent('kid', { parent_session_id: 'root' })]
      expect(familySlotById.value.size).toBe(0)
    })

    it('shows the selected member, marked as current', () => {
      _rawSessions.value = [
        agent('root', { slug: 'rooty' }),
        agent('kid', { slug: 'kiddo', parent_session_id: 'root' }),
      ]
      urlPath.value = '/proj/pi/kiddo'
      expect(slot('root')).toMatchObject({ selected: true })
      expect(slot('root')?.session.id).toBe('kid')
    })

    it('keeps the member as a way back after you navigate to the root', () => {
      _rawSessions.value = [
        agent('root', { slug: 'rooty' }),
        agent('kid', { slug: 'kiddo', parent_session_id: 'root' }),
      ]
      rememberFamilyChild('kid')
      urlPath.value = '/proj/pi/rooty'
      expect(slot('root')?.session.id).toBe('kid')
      // No longer current: it is a memory, not the selection.
      expect(slot('root')?.selected).toBe(false)
    })

    it('keeps it after you leave the family entirely', () => {
      _rawSessions.value = [
        agent('root'),
        agent('kid', { parent_session_id: 'root' }),
        agent('elsewhere', { slug: 'far' }),
      ]
      rememberFamilyChild('kid')
      urlPath.value = '/proj/pi/far'
      expect(slot('root')?.session.id).toBe('kid')
    })

    it('gives the slot to the selection over an older visit', () => {
      _rawSessions.value = [
        agent('root'),
        agent('a', { slug: 'aa', parent_session_id: 'root' }),
        agent('b', { slug: 'bb', parent_session_id: 'root' }),
      ]
      rememberFamilyChild('a')
      urlPath.value = '/proj/pi/bb'
      expect(slot('root')?.session.id).toBe('b')
      expect(slot('root')?.selected).toBe(true)
    })

    it('keeps only the most recent visit per family (one slot)', () => {
      _rawSessions.value = [
        agent('root'),
        agent('a', { parent_session_id: 'root' }),
        agent('b', { parent_session_id: 'root' }),
      ]
      rememberFamilyChild('a')
      rememberFamilyChild('b')
      expect(familySlotById.value.size).toBe(1)
      expect(slot('root')?.session.id).toBe('b')
    })

    it('drops a member that left the family: promoted or reparented', () => {
      _rawSessions.value = [
        agent('root'),
        agent('kid', { parent_session_id: 'root' }),
        agent('other'),
      ]
      rememberFamilyChild('kid')
      expect(slot('root')?.session.id).toBe('kid')
      // Promotion gives it its own sidebar row; a duplicate would lie.
      _rawSessions.value = _rawSessions.value.map(s =>
        s.id === 'kid' ? { ...s, promoted_to_root: true } : s)
      expect(familySlotById.value.size).toBe(0)
      // Reparenting moves the memory to the new family, no re-keying.
      _rawSessions.value = _rawSessions.value.map(s =>
        s.id === 'kid' ? { ...s, promoted_to_root: false, parent_session_id: 'other' } : s)
      expect(slot('other')?.session.id).toBe('kid')
      expect(familySlotById.value.has('root')).toBe(false)
    })

    it('drops a member that is gone, or a corpse you cannot reopen', () => {
      _rawSessions.value = [agent('root'), agent('kid', { parent_session_id: 'root' })]
      rememberFamilyChild('kid')
      _rawSessions.value = _rawSessions.value.map(s =>
        s.id === 'kid' ? { ...s, alive: false, resumable: false } : s)
      expect(familySlotById.value.size).toBe(0)
      // Resumable corpses stay: they are still a way back.
      _rawSessions.value = _rawSessions.value.map(s =>
        s.id === 'kid' ? { ...s, resumable: true } : s)
      expect(slot('root')?.session.id).toBe('kid')
      // …unless the alive-only toggle is hiding dead sessions.
      setAliveOnly(true)
      expect(familySlotById.value.size).toBe(0)
      setAliveOnly(false)
    })

    it('never shows a member that vanished from the snapshot', () => {
      _rawSessions.value = [agent('root'), agent('kid', { parent_session_id: 'root' })]
      rememberFamilyChild('kid')
      _rawSessions.value = [agent('root')]
      expect(familySlotById.value.size).toBe(0)
      // The id is still remembered; only its resolution failed. If the
      // session comes back (peer reconnect), so does the row.
      expect(recentFamilyChildren.value).toEqual(['kid'])
    })

    it('remembers a bounded number of families, most recent first', () => {
      const sessions: Session[] = []
      for (let i = 0; i < 7; i++) {
        sessions.push(agent(`root${i}`), agent(`kid${i}`, { parent_session_id: `root${i}` }))
      }
      _rawSessions.value = sessions
      for (let i = 0; i < 7; i++) rememberFamilyChild(`kid${i}`)
      // Cap is 5; the two oldest families lost their memory.
      expect(recentFamilyChildren.value).toEqual(['kid6', 'kid5', 'kid4', 'kid3', 'kid2'])
      expect(familySlotById.value.size).toBe(5)
      expect(familySlotById.value.has('root0')).toBe(false)
    })

    it('keeps one member per family, so one family cannot evict the rest', () => {
      _rawSessions.value = [
        agent('rootA'), agent('rootB'),
        ...['a0', 'a1', 'a2', 'a3', 'a4'].map(id => agent(id, { parent_session_id: 'rootA' })),
        agent('b0', { parent_session_id: 'rootB' }),
      ]
      rememberFamilyChild('b0')
      for (const id of ['a0', 'a1', 'a2', 'a3', 'a4']) rememberFamilyChild(id)
      // Family A occupies exactly one entry however many members you
      // bounce through, so B's way back survives.
      expect(recentFamilyChildren.value).toEqual(['a4', 'b0'])
      expect(slot('rootA')?.session.id).toBe('a4')
      expect(slot('rootB')?.session.id).toBe('b0')
    })

    it('is idempotent for the member already at the head', () => {
      rememberFamilyChild('a')
      const first = recentFamilyChildren.value
      rememberFamilyChild('a')
      expect(recentFamilyChildren.value).toBe(first)
    })
  })

  describe('selectedFamilyChild (selected-child projection)', () => {
    it('is null when nothing or a root is selected', () => {
      _rawSessions.value = [agent('root', { slug: 'rooty' }), agent('kid', { slug: 'kiddo', parent_session_id: 'root' })]
      expect(selectedFamilyChild.value).toBeNull()
      urlPath.value = '/proj/pi/rooty'
      expect(selectedFamilyChild.value).toBeNull()
    })

    it('reports the child, its root row, and the ancestor trail', () => {
      _rawSessions.value = [
        agent('root', { slug: 'rooty', title: 'orchestrator' }),
        agent('kid', { slug: 'kiddo', parent_session_id: 'root', title: 'implement' }),
        agent('grandkid', { slug: 'grandkiddo', parent_session_id: 'kid', title: 'refactor' }),
      ]
      urlPath.value = '/proj/pi/grandkiddo'
      const projection = selectedFamilyChild.value!
      expect(projection.session.id).toBe('grandkid')
      expect(projection.rootId).toBe('root')
      // Root first, immediate parent last — the sidebar renders the
      // trail as a hover title on the child row.
      expect(projection.ancestors.map(a => a.id)).toEqual(['root', 'kid'])
    })

    it('follows a selected process child too', () => {
      _rawSessions.value = [agent('root'), proc('p', 'root', { slug: 'pp', title: 'pnpm test' })]
      urlPath.value = '/proj/shell/pp'
      expect(selectedFamilyChild.value?.session.id).toBe('p')
      expect(selectedFamilyChild.value?.rootId).toBe('root')
    })

    it('is null for a promoted child (it owns a sidebar row of its own)', () => {
      _rawSessions.value = [
        agent('root', { slug: 'rooty' }),
        agent('kid', { slug: 'kiddo', parent_session_id: 'root', promoted_to_root: true }),
      ]
      urlPath.value = '/proj/pi/kiddo'
      expect(selectedId.value).toBe('kid')
      expect(selectedFamilyChild.value).toBeNull()
      expect(familySelectedId.value).toBe('kid')
    })

    it('keeps the sidebar row selection on the root while a child is selected', () => {
      _rawSessions.value = [
        agent('root', { slug: 'rooty' }),
        agent('kid', { slug: 'kiddo', parent_session_id: 'root' }),
      ]
      urlPath.value = '/proj/pi/kiddo'
      expect(familySelectedId.value).toBe('root')
      expect(selectedFamilyChild.value?.session.id).toBe('kid')
    })
  })
})

describe('tab-identity params (?filter=, ?sidebar=)', () => {
  let navigateMock: ReturnType<typeof vi.fn>

  beforeEach(() => {
    navigateMock = vi.fn()
    setNavigate(navigateMock)
    urlPath.value = '/'
    urlSearch.value = ''
  })
  afterEach(() => {
    setNavigate(() => {/* no-op */})
  })

  it('setSidebarMode mirrors the signal into the URL, omitting the default', () => {
    // The default state leaves a clean URL: no ?sidebar=projects.
    urlSearch.value = '?filter=gmux&sidebar=activity'
    sidebarMode.value = 'activity'
    setSidebarMode('projects')
    expect(sidebarMode.value).toBe('projects')
    expect(navigateMock).toHaveBeenLastCalledWith('/?filter=gmux', true)

    urlSearch.value = '?filter=gmux'
    setSidebarMode('activity')
    expect(sidebarMode.value).toBe('activity')
    expect(navigateMock).toHaveBeenLastCalledWith('/?filter=gmux&sidebar=activity', true)
  })

  it('setSidebarMode preserves non-tab params (?settings)', () => {
    urlSearch.value = '?settings=hosts'
    setSidebarMode('activity')
    expect(navigateMock).toHaveBeenLastCalledWith('/?settings=hosts&sidebar=activity', true)
  })

  it('navigate stamps the sidebar mode from the signal, not the URL', () => {
    // A stale URL param (e.g. an old history entry) never wins: the
    // signal is the source of truth for every navigation.
    urlSearch.value = '?sidebar=activity'
    sidebarMode.value = 'projects'
    navigate('/gmux/pi/x', true)
    expect(navigateMock).toHaveBeenLastCalledWith('/gmux/pi/x', true)

    urlSearch.value = ''
    sidebarMode.value = 'activity'
    navigate('/gmux/pi/x')
    expect(navigateMock).toHaveBeenLastCalledWith('/gmux/pi/x?sidebar=activity', undefined)
  })

  it('navigate carries ?filter= from the current URL by default', () => {
    urlSearch.value = '?filter=gmux%40server'
    navigate('/gmux/pi/x')
    expect(navigateMock).toHaveBeenLastCalledWith('/gmux/pi/x?filter=gmux%40server', undefined)
  })

  it('setFilterSelectors clears the param when the list empties', () => {
    urlSearch.value = '?filter=gmux&sidebar=activity'
    sidebarMode.value = 'activity'
    setFilterSelectors([])
    expect(navigateMock).toHaveBeenLastCalledWith('/?sidebar=activity', true)
  })

  it('tabHref uses the same carry contract as navigate', () => {
    urlSearch.value = '?filter=gmux&settings=hosts'
    sidebarMode.value = 'activity'
    // Filter and sidebar identify the tab; transient settings state drops.
    expect(tabHref('/gmux/pi/x')).toBe('/gmux/pi/x?filter=gmux&sidebar=activity')
    sidebarMode.value = 'projects'
    urlSearch.value = ''
    expect(tabHref('/gmux/pi/x')).toBe('/gmux/pi/x')
  })

  it('carries mock boot context so an SPA destination remains reloadable', () => {
    urlSearch.value = '?mock=&host=server&filter=gmux&settings=hosts'
    expect(tabHref('/gmux/pi/~1vshk4fu')).toBe(
      '/gmux/pi/~1vshk4fu?filter=gmux&mock=&host=server',
    )
    // Explicit target values win over the current mock context.
    expect(tabHref('/gmux/pi/~1vshk4fu?mock=target&host=laptop')).toBe(
      '/gmux/pi/~1vshk4fu?mock=target&host=laptop&filter=gmux',
    )
  })

  it('setHostFilter replaces host-wide selectors but keeps project selectors', () => {
    urlSearch.value = '?filter=gmux,*@laptop'
    setHostFilter('server')
    expect(navigateMock).toHaveBeenLastCalledWith('/?filter=gmux%2C*%40server', true)

    urlSearch.value = '?filter=gmux,*@server'
    setHostFilter(null)
    expect(navigateMock).toHaveBeenLastCalledWith('/?filter=gmux', true)
  })

  it('setFilterSelectors preserves non-tab params (?settings, ?mock)', () => {
    // Regression: targeting the bare path dropped every non-tab param,
    // so a filter edit silently closed the settings modal / left mock mode.
    urlSearch.value = '?mock=&settings=hosts&filter=old'
    setFilterSelectors([{ project: 'gmux', host: '*' }])
    expect(navigateMock).toHaveBeenLastCalledWith('/?mock=&settings=hosts&filter=gmux', true)
  })

  it('keeps hash fragments out of query data and preserves them on same-view edits', () => {
    urlSearch.value = '?filter=gmux'
    navigate('/gmux/pi/x?settings=hosts#terminal', true)
    expect(navigateMock).toHaveBeenLastCalledWith(
      '/gmux/pi/x?settings=hosts&filter=gmux#terminal', true,
    )

    urlPath.value = '/gmux/pi/x'
    urlHash.value = '#stale'
    // Hash-only navigation can update the browser before `hashchange` runs;
    // rewrite seams must prefer the live fragment over the signal snapshot.
    vi.stubGlobal('location', { hash: '#terminal' })
    try {
      setFilterSelectors([])
    } finally {
      vi.unstubAllGlobals()
    }
    expect(navigateMock).toHaveBeenLastCalledWith('/gmux/pi/x#terminal', true)
    // Apply the router's resulting location signals before the next edit.
    urlSearch.value = ''
    urlHash.value = '#terminal'
    setSidebarMode('activity')
    expect(navigateMock).toHaveBeenLastCalledWith('/gmux/pi/x?sidebar=activity#terminal', true)
  })

  it('homePartition participates in the tab scope', () => {
    _setRawWorld({ projects: [{ slug: 'proj', match: [{ path: '/work' }] }], peers: [] })
    _rawSessions.value = [
      makeSession({ id: 'in', cwd: '/work', alive: true, unread: true, project_slug: 'proj' }),
      makeSession({ id: 'out', alive: true, unread: true, project_slug: 'other', peer: 'server' }),
    ]
    urlSearch.value = '?filter=proj'
    expect(homePartition.value.flatMap(b => b.sessions.map(s => s.id))).toEqual(['in'])
  })
})

describe('sidebar-mode deep-link seed', () => {
  it('seeds activity from the initial document URL before repair starts', async () => {
    vi.stubGlobal('location', { pathname: '/', search: '?sidebar=activity', hash: '' })
    vi.resetModules()
    try {
      const freshStore = await import('./store')
      expect(freshStore.sidebarMode.value).toBe('activity')
    } finally {
      vi.unstubAllGlobals()
    }
  })
})

describe('sidebar-mode repair effect (initStore)', () => {
  // The commit's core invariant: after boot the sidebarMode signal is
  // authoritative and the URL is a mirror. A history entry stamped with
  // a stale ?sidebar (Back/Forward, old bookmark) must be rewritten in
  // place — never adopted into the signal. Mutation-proof: inverting
  // the repair direction (sidebarMode.value = urlMode) fails this test.
  let navigateMock: ReturnType<typeof vi.fn>
  let cleanup: (() => void) | null = null

  beforeEach(() => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({ ok: false }))
    class FakeEventSource {
      addEventListener() { /* no-op */ }
      close() { /* no-op */ }
    }
    vi.stubGlobal('EventSource', FakeEventSource)
    navigateMock = vi.fn()
    setNavigate(navigateMock)
  })
  afterEach(() => {
    cleanup?.()
    cleanup = null
    setNavigate(() => {/* no-op */})
    vi.unstubAllGlobals()
  })

  it('records the family member you are viewing (the recorder effect runs)', () => {
    // Every other MRU test calls rememberFamilyChild directly, which
    // proves the list but not the wiring. This one pins the wiring: an
    // effect registered by initStore, above the mock-mode early return
    // that would otherwise leave it dead in ?mock previews.
    _setRawWorld({ projects: [{ slug: 'proj', match: [{ path: '/work' }] }], peers: [] })
    sessionsLoaded.value = true
    worldLoaded.value = true
    _rawSessions.value = [
      makeSession({ id: 'root', slug: 'rooty', cwd: '/work', adapter: 'pi', semantic_agent: true, project_slug: 'proj' }),
      makeSession({ id: 'kid', slug: 'kiddo', cwd: '/work', adapter: 'pi', semantic_agent: true, project_slug: 'proj', parent_session_id: 'root' }),
    ]
    recentFamilyChildren.value = []
    // A resolvable selection wakes the mark-as-read effect, which reads
    // `selected` — and that computed pokes `window` for debugging.
    vi.stubGlobal('window', {})
    cleanup = initStore()
    urlPath.value = '/proj/pi/kiddo'
    expect(selectedFamilyChild.value?.session.id).toBe('kid')
    expect(recentFamilyChildren.value).toEqual(['kid'])
  })

  it('rewrites a stale ?sidebar entry in place; the signal never adopts from the URL', () => {
    cleanup = initStore()
    // Simulate Back onto an entry stamped with the old mode while the
    // tab is in projects mode. Non-tab params must survive the repair.
    urlPath.value = '/gmux/pi/x'
    urlSearch.value = '?sidebar=activity&settings=hosts'

    expect(sidebarMode.value).toBe('projects')
    expect(navigateMock).toHaveBeenLastCalledWith('/gmux/pi/x?settings=hosts', true)

    // Convergence: once the router applies the repaired URL, the effect
    // re-runs and does nothing further.
    navigateMock.mockClear()
    urlSearch.value = '?settings=hosts'
    expect(navigateMock).not.toHaveBeenCalled()
  })

  it('repairs in the other direction too (activity signal, bare URL)', () => {
    cleanup = initStore()
    sidebarMode.value = 'activity'
    navigateMock.mockClear()
    urlPath.value = '/gmux/pi/x'
    urlSearch.value = '?filter=gmux'
    expect(sidebarMode.value).toBe('activity')
    expect(navigateMock).toHaveBeenLastCalledWith('/gmux/pi/x?filter=gmux&sidebar=activity', true)
  })

  it('dispatches exactly one replacement for an explicit mode toggle', () => {
    cleanup = initStore()
    navigateMock.mockClear()
    setSidebarMode('activity')
    expect(navigateMock).toHaveBeenCalledTimes(1)
    expect(navigateMock).toHaveBeenCalledWith('/?sidebar=activity', true)
  })

  it('canonicalizes malformed and repeated sidebar params in place', () => {
    cleanup = initStore()
    navigateMock.mockClear()
    urlPath.value = '/gmux/pi/x'
    urlSearch.value = '?sidebar=bogus&sidebar=activity&settings=hosts'
    expect(sidebarMode.value).toBe('projects')
    expect(navigateMock).toHaveBeenLastCalledWith('/gmux/pi/x?settings=hosts', true)
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
      expect(applyPending(sess, [])).toBe(sess)
    })

    it('mark-read clears unread and status.error on the targeted session', () => {
      const sess = [
        makeSession({ id: 'a', unread: true, status: { active: false, error: true } }),
        makeSession({ id: 'b', unread: true }),
      ]
      const m: PendingMutation = { kind: 'mark-read', id: 'a', token: '', at: 0 }
      const out = applyPending(sess, [m])
      expect(out[0].unread).toBe(false)
      expect(out[0].status?.error).toBe(false)
      // Untouched session keeps its flags.
      expect(out[1].unread).toBe(true)
    })

    it('applies a mark-read per session in one pass, whatever the pile', () => {
      // "Mark all read" stacks one mutation per family member, and this
      // overlay is replayed on every recompute in the app until the
      // server echoes them back. A pass per mutation made that cost
      // mutations x sessions.
      const sess = Array.from({ length: 200 }, (_, i) =>
        makeSession({ id: `s${i}`, unread: true, unread_token: `t${i}` }))
      const pending: PendingMutation[] = sess.map(s => (
        { kind: 'mark-read', id: s.id, token: s.unread_token ?? '', at: 0 }))
      const out = applyPending(sess, pending)
      expect(out.every(s => !s.unread)).toBe(true)
      expect(out).toHaveLength(200)
    })

    it('honours each token separately when several are in flight for one session', () => {
      // The token binds a mark to the state it was issued against, so a
      // session that spoke again escapes the older mark rather than
      // being silenced by it.
      const sess = [makeSession({ id: 'a', unread: true, unread_token: 'new' })]
      const out = applyPending(sess, [
        { kind: 'mark-read', id: 'a', token: 'stale', at: 0 },
        { kind: 'mark-read', id: 'a', token: 'new', at: 0 },
      ])
      expect(out[0].unread).toBe(false)
      const stillUnread = applyPending(sess, [{ kind: 'mark-read', id: 'a', token: 'stale', at: 0 }])
      expect(stillUnread[0].unread).toBe(true)
    })

    it('dismissal wins over a mark-read for the same session, either order', () => {
      const sess = [makeSession({ id: 'a', unread: true, unread_token: '' })]
      const read: PendingMutation = { kind: 'mark-read', id: 'a', token: '', at: 0 }
      const gone: PendingMutation = { kind: 'dismiss', id: 'a', at: 0 }
      expect(applyPending(sess, [read, gone])).toEqual([])
      expect(applyPending(sess, [gone, read])).toEqual([])
    })

    it('dismiss removes the targeted session', () => {
      const sess = [makeSession({ id: 'a' }), makeSession({ id: 'b' })]
      const out = applyPending(sess, [{ kind: 'dismiss', id: 'a', at: 0 }])
      expect(out.map(s => s.id)).toEqual(['b'])
    })

    it('stacks multiple mutations in order', () => {
      const sess = [makeSession({ id: 'a', unread: true }), makeSession({ id: 'b' })]
      const out = applyPending(sess, [
        { kind: 'mark-read', id: 'a', token: '', at: 0 },
        { kind: 'dismiss', id: 'b', at: 0 },
      ])
      expect(out.map(s => s.id)).toEqual(['a'])
      expect(out[0].unread).toBe(false)
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

    it('reorder adds no optimistic overlay, local or peer', () => {
      // Order reaches the UI only as server-stamped project_index, so
      // there is no local array an overlay could pre-write: the owning
      // daemon's echo is the only thing that moves a row.
      _setRawWorld({ projects: [{ slug: 'p', match: [], sessions: ['x', 'y'] }] })
      reorderSessions('p', ['y', 'x'])
      reorderSessions('p', ['y', 'x'], 'tower')
      expect(_pendingMutations.value).toHaveLength(0)
      expect(projects.value[0].sessions).toEqual(['x', 'y'])
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

describe('family wire mapping', () => {
  it('preserves parent provenance, semantic capability, and promotion', () => {
    const parsed = SessionSchema.parse({
      id: 'child', alive: true, parent_session_id: 'root',
      semantic_agent: true, promoted_to_root: true,
    })
    expect(toUISession(parsed)).toMatchObject({
      parent_session_id: 'root', semantic_agent: true, promoted_to_root: true,
    })
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

describe('mock-mode boot (?mock)', () => {
  // `?mock` is the surface design work is reviewed on, and initStore
  // returns early for it — anything registered below that return is dead
  // in mock previews while looking perfectly wired in production. The
  // recorder effect was, once. This boots a *fresh* store module with
  // ?mock in the URL, which is the only way to see that difference.
  afterEach(() => {
    vi.unstubAllGlobals()
    vi.resetModules()
  })

  it('records the family member you are viewing in a mock preview too', async () => {
    vi.stubGlobal('location', { search: '?mock', hash: '', pathname: '/' })
    vi.stubGlobal('window', {})
    vi.resetModules()
    const store = await import('./store')
    // Stand in for the router: the store navigates through this.
    store.setNavigate((url: string) => { store.urlPath.value = url.split('?')[0] })
    const cleanup = store.initStore()
    try {
      const kid = store.sessions.value.find(s => s.parent_session_id && s.semantic_agent)
      expect(kid, 'the mock fixtures should contain a family child').toBeDefined()
      store.navigateToSession(kid!.id)
      expect(store.selectedFamilyChild.value?.session.id).toBe(kid!.id)
      expect(store.recentFamilyChildren.value).toEqual([kid!.id])
    } finally {
      cleanup()
    }
  })
})
