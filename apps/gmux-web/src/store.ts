/**
 * Reactive application store built on @preact/signals.
 *
 * All shared state lives here as signals. Derived values are `computed`.
 * Components import signals directly; no prop drilling needed for data.
 *
 * Mutation rules:
 *  - SSE/fetch handlers call the exported mutators (upsertSession, etc.)
 *  - Components read signals in JSX (auto-subscribed) or via `.value`
 *  - `batch()` groups multiple writes into one notification cycle
 *
 * This module is intentionally side-effect-free at import time.
 * Call `initStore()` once from the app root to start SSE, fetch data, etc.
 */

import { signal, computed, batch, effect } from '@preact/signals'
import type { Session, ProjectItem, DiscoveredProject, PeerInfo, LauncherDef, Folder } from './types'
import type { View } from './routing'
import { resolveViewFromPath, viewToPath, sessionPath } from './routing'
import { buildProjectFolders, matchSession } from './projects'

import { fetchFrontendConfig, buildTerminalOptions, resolveKeybinds, type ResolvedKeybind } from './config'
import { MOCK_SESSIONS, MOCK_PROJECTS } from './mock-data/index'
import type { ITerminalOptions } from '@xterm/xterm'
import type { Session as ProtocolSession } from '@gmux/protocol'

// ── Raw state (sources of truth) ────────────────────────────────────────────

export const sessions = signal<Session[]>([])
export const sessionsLoaded = signal(false)
export const connState = signal<'connecting' | 'connected' | 'error'>('connecting')

export const projects = signal<ProjectItem[]>([])
export const discovered = signal<DiscoveredProject[]>([])
export const unmatchedActiveCount = signal(0)

export const peers = signal<PeerInfo[]>([])
export const launchers = signal<LauncherDef[]>([])
export const defaultLauncher = signal<string>('shell')

export interface HealthData {
  version: string
  hostname?: string
  tailscale_url?: string
  update_available?: string
  /** SHA-256 of the gmux runner binary on disk. Compared against
   * session.binary_hash to detect dev-mode hash drift. */
  runner_hash?: string
  default_launcher?: string
  launchers?: LauncherDef[]
  peers?: PeerInfo[]
}
export const health = signal<HealthData | null>(null)

// ── Peer appearance: unique prefix + deterministic color ─────────────────────

/** 6-color palette: [foreground, background] pairs for dark backgrounds.
 *  Hues chosen for visual distinction and to avoid muddy tones. */
const PEER_PALETTE: [string, string][] = [
  ['oklch(72% 0.11 195)', 'oklch(25% 0.04 195)'], // teal
  ['oklch(72% 0.12 55)',  'oklch(25% 0.04 55)'],   // amber
  ['oklch(72% 0.10 285)', 'oklch(25% 0.04 285)'], // violet
  ['oklch(72% 0.12 25)',  'oklch(25% 0.04 25)'],   // coral
  ['oklch(72% 0.10 230)', 'oklch(25% 0.04 230)'], // blue
  ['oklch(72% 0.10 340)', 'oklch(25% 0.04 340)'], // rose
]

/** Shortest unique prefix for each name among a set of names. */
function uniquePrefixes(names: string[]): Map<string, string> {
  const result = new Map<string, string>()
  for (const name of names) {
    let len = 1
    while (len < name.length && names.some(n => n !== name && n.slice(0, len) === name.slice(0, len))) {
      len++
    }
    result.set(name, name.slice(0, len).toUpperCase())
  }
  return result
}

export interface PeerAppearance {
  label: string
  color: string
  bg: string
}

/** Derived map from peer name to { label, color, bg }. Colors assigned by list order. */
export const peerAppearance = computed<ReadonlyMap<string, PeerAppearance>>(() => {
  const names = peers.value.map(p => p.name)
  const prefixes = uniquePrefixes(names)
  const map = new Map<string, PeerAppearance>()
  for (let i = 0; i < names.length; i++) {
    const [color, bg] = PEER_PALETTE[i % PEER_PALETTE.length]
    map.set(names[i], { label: prefixes.get(names[i])!, color, bg })
  }
  return map
})

export const terminalOptions = signal<ITerminalOptions | null>(null)
export const keybinds = signal<ResolvedKeybind[] | null>(null)
export const macCommandIsCtrl = signal(false)

/** Current URL path, kept in sync with preact-iso's location. */
export const urlPath = signal(
  typeof location !== 'undefined' ? (location.pathname.replace(/\/+$/, '') || '/') : '/',
)

/**
 * Activity tracking: which sessions recently produced output.
 *
 * Maps session ID to a state: 'active' (within window) or 'fading'
 * (in the fade-out phase). Absence = no recent activity. Entries are
 * cleaned up by timers; the map reference changes on every transition
 * so computed values that read it recompute.
 */
export const activityMap = signal<ReadonlyMap<string, 'active' | 'fading'>>(new Map())

// Internal mutable map + timers. We write to this and then publish a
// new (frozen) snapshot to the signal so reads trigger recomputation.
const _actMap = new Map<string, 'active' | 'fading'>()
const _actTimers = new Map<string, ReturnType<typeof setTimeout>>()
const _fadeTimers = new Map<string, ReturnType<typeof setTimeout>>()
const ACTIVITY_MS = 3000
const FADE_MS = 800

function publishActivity() {
  activityMap.value = new Map(_actMap)
}

export function handleActivity(sessionId: string) {
  // Clear existing timers for this session.
  const t1 = _actTimers.get(sessionId)
  if (t1) clearTimeout(t1)
  const t2 = _fadeTimers.get(sessionId)
  if (t2) { clearTimeout(t2); _fadeTimers.delete(sessionId) }

  _actMap.set(sessionId, 'active')

  _actTimers.set(sessionId, setTimeout(() => {
    _actTimers.delete(sessionId)
    _actMap.set(sessionId, 'fading')
    publishActivity()

    _fadeTimers.set(sessionId, setTimeout(() => {
      _fadeTimers.delete(sessionId)
      _actMap.delete(sessionId)
      publishActivity()
    }, FADE_MS))
  }, ACTIVITY_MS))

  publishActivity()
}

export function isSessionActive(id: string): boolean {
  return activityMap.value.get(id) === 'active'
}

export function isSessionFading(id: string): boolean {
  return activityMap.value.get(id) === 'fading'
}



// ── Derived state (computed, auto-cached) ───────────────────────────────────

/** Sessions filtered by URL params (?project=, ?cwd=). */
export const filteredSessions = computed(() => {
  const search = typeof location !== 'undefined' ? location.search : ''
  const params = new URLSearchParams(search)
  const project = params.get('project')
  const cwdFilter = params.get('cwd')
  if (!project && !cwdFilter) return sessions.value
  return sessions.value.filter(s => {
    if (project && !s.cwd.toLowerCase().includes(project.toLowerCase())) return false
    if (cwdFilter && !s.cwd.startsWith(cwdFilter)) return false
    return true
  })
})

/** Project folders for the sidebar, built from projects + sessions. */
export const folders = computed(() =>
  buildProjectFolders(projects.value, filteredSessions.value),
)

/**
 * Current view, derived from the URL + data.
 *
 * Returns null until sessions have loaded at least once. This prevents
 * the URL normalization effect from overwriting a deep session URL with
 * a fallback before data arrives. After loading, always returns a
 * concrete View (home/project/session).
 */
export const view = computed((): View | null => {
  if (!sessionsLoaded.value) return null
  return resolveViewFromPath(urlPath.value, projects.value, filteredSessions.value)
})

/** Currently selected session ID, if the view is a session view. */
export const selectedId = computed(() =>
  view.value?.kind === 'session' ? view.value.sessionId : null,
)

/** Currently selected session object. */
export const selected = computed(() => {
  const id = selectedId.value
  if (!id) return null
  const s = sessions.value.find(s => s.id === id) ?? null
  // Expose on window for debugging.
  ;(window as any).__gmuxSession = s
  return s
})

/** Project slug when the view is a project hub. */
export const currentProjectSlug = computed(() =>
  view.value?.kind === 'project' ? view.value.projectSlug : null,
)

/** Dot state for the mobile hamburger: summarizes background session activity. */
export type DotState = 'working' | 'error' | 'unread' | 'active' | 'fading' | 'none'

export const backgroundActivity = computed((): DotState => {
  const sel = selectedId.value
  const am = activityMap.value
  const others = sessions.value.filter(s => s.id !== sel && s.alive)
  if (others.some(s => s.status?.error))          return 'error'
  if (others.some(s => s.status?.working))        return 'working'
  if (others.some(s => s.unread))                 return 'unread'
  if (others.some(s => am.get(s.id) === 'active')) return 'active'
  if (others.some(s => am.get(s.id) === 'fading')) return 'fading'
  return 'none'
})

/** Count of unread sessions (excluding selected). */
export const unreadCount = computed(() =>
  sessions.value.filter(s => s.id !== selectedId.value && s.alive && s.unread).length,
)

// ── Mutators ────────────────────────────────────────────────────────────────

export function toUISession(s: ProtocolSession): Session {
  return {
    id: s.id,
    created_at: s.created_at ?? new Date().toISOString(),
    command: s.command ?? [],
    cwd: s.cwd ?? '',
    workspace_root: s.workspace_root ?? undefined,
    remotes: s.remotes ?? undefined,
    kind: s.kind ?? 'shell',
    alive: s.alive,
    pid: s.pid ?? null,
    exit_code: s.exit_code ?? null,
    started_at: s.started_at ?? s.created_at ?? new Date().toISOString(),
    exited_at: s.exited_at ?? null,
    title: s.title ?? s.command?.[0] ?? 'session',
    subtitle: s.subtitle ?? '',
    status: s.status ?? null,
    unread: s.unread ?? false,
    resumable: s.resumable ?? false,
    socket_path: s.socket_path ?? '',
    terminal_cols: s.terminal_cols ?? undefined,
    terminal_rows: s.terminal_rows ?? undefined,
    slug: s.slug ?? undefined,
    runner_version: s.runner_version ?? undefined,
    binary_hash: s.binary_hash ?? undefined,
    peer: s.peer ?? undefined,
  }
}

/**
 * Derive staleness from a session's build-identity fields.
 *
 * Returns:
 *   'version' - runner_version differs from the daemon version (production mismatch)
 *   'hash'    - versions match but binary_hash differs from health.runner_hash
 *               (dev-mode: both sides report "dev" but from different builds)
 *   null      - current, or insufficient data to determine (graceful degradation
 *               for runners that predate version tracking)
 */
export function sessionStaleness(
  session: Pick<Session, 'runner_version' | 'binary_hash'>,
  h: Pick<HealthData, 'version' | 'runner_hash'> | null,
): 'version' | 'hash' | null {
  if (!h || !session.runner_version) return null
  if (session.runner_version !== h.version) return 'version'
  if (session.binary_hash && h.runner_hash && session.binary_hash !== h.runner_hash) return 'hash'
  return null
}

/** Upsert a session from SSE. Returns true if the session was new. */
export function upsertSession(raw: ProtocolSession): boolean {
  const updated = toUISession(raw)
  let isNew = false
  const prev = sessions.value
  const idx = prev.findIndex(s => s.id === updated.id)
  if (idx >= 0) {
    const next = [...prev]
    next[idx] = updated
    sessions.value = next
  } else {
    isNew = true
    sessions.value = [...prev, updated]
  }
  return isNew
}

export function removeSession(id: string) {
  sessions.value = sessions.value.filter(s => s.id !== id)
}

export function markSessionRead(id: string) {
  sessions.value = sessions.value.map(s =>
    s.id === id
      ? { ...s, unread: false, status: s.status?.error ? { ...s.status, error: false } : s.status }
      : s,
  )
  fetch(`/v1/sessions/${id}/read`, { method: 'POST' }).catch(() => {})
}

export function setProjects(data: { configured: ProjectItem[]; discovered: DiscoveredProject[]; unmatchedActiveCount: number }) {
  batch(() => {
    projects.value = data.configured
    discovered.value = data.discovered
    unmatchedActiveCount.value = data.unmatchedActiveCount
  })
}

// ── API helpers ─────────────────────────────────────────────────────────────

async function fetchSessions(): Promise<Session[]> {
  const resp = await fetch('/v1/sessions')
  const json = await resp.json()
  const data: ProtocolSession[] = json?.data ?? []
  return data.map(toUISession)
}



export async function fetchProjects(): Promise<void> {
  try {
    const resp = await fetch('/v1/projects')
    const json = await resp.json()
    if (json.ok && json.data) {
      setProjects({
        configured: json.data.configured ?? [],
        discovered: json.data.discovered ?? [],
        unmatchedActiveCount: json.data.unmatched_active_count ?? 0,
      })
    }
  } catch (err) {
    console.warn('Failed to fetch projects:', err)
  }
}

function applyHealth(h: HealthData) {
  batch(() => {
    health.value = h
    peers.value = h.peers ?? []
    launchers.value = h.launchers ?? []
    defaultLauncher.value = h.default_launcher ?? 'shell'
  })
}

async function fetchHealth(): Promise<void> {
  try {
    const resp = await fetch('/v1/health')
    const json = await resp.json()
    const h: HealthData | null = json.data ?? null
    if (h) applyHealth(h)
  } catch {
    // Health fetch is best-effort; UI works without it.
  }
}

// ── Project mutations (used by manage-projects) ─────────────────────────────

async function putProjects(items: ProjectItem[]): Promise<void> {
  try {
    const resp = await fetch('/v1/projects', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ items }),
    })
    if (!resp.ok) console.warn('PUT /v1/projects failed:', resp.status)
  } catch (err) {
    console.warn('PUT /v1/projects error:', err)
  }
}

export async function removeProject(slug: string): Promise<void> {
  await putProjects(projects.value.filter(p => p.slug !== slug))
}

export async function addProject(req: { remote?: string; paths: string[] }): Promise<void> {
  try {
    const resp = await fetch('/v1/projects/add', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(req),
    })
    if (!resp.ok) console.warn('POST /v1/projects/add failed:', resp.status)
  } catch (err) {
    console.warn('POST /v1/projects/add error:', err)
  }
}

export async function updateProjects(items: ProjectItem[]): Promise<void> {
  await putProjects(items)
}

// ── Session actions ─────────────────────────────────────────────────────────

async function postAction(endpoint: string, body?: Record<string, unknown>): Promise<void> {
  try {
    const resp = await fetch(endpoint, {
      method: 'POST',
      ...(body ? {
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      } : {}),
    })
    if (!resp.ok) console.warn(`${endpoint} failed:`, resp.status, await resp.text().catch(() => ''))
  } catch (err) {
    console.warn(`${endpoint} error:`, err)
  }
}

export function killSession(sessionId: string): Promise<void> {
  return postAction(`/v1/sessions/${sessionId}/kill`)
}

export function dismissSession(sessionId: string): Promise<void> {
  removeSession(sessionId)
  return postAction(`/v1/sessions/${sessionId}/dismiss`)
}

export function resumeSession(sessionId: string): Promise<void> {
  return postAction(`/v1/sessions/${sessionId}/resume`)
}

export function restartSession(sessionId: string): Promise<void> {
  return postAction(`/v1/sessions/${sessionId}/restart`)
}

// ── Launch ───────────────────────────────────────────────────────────────────

let _pendingLaunchAt = 0

export async function launchSession(launcherId: string, opts?: { cwd?: string; peer?: string }): Promise<void> {
  _pendingLaunchAt = Date.now()
  try {
    const resp = await fetch('/v1/launch', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ launcher_id: launcherId, cwd: opts?.cwd, peer: opts?.peer }),
    })
    if (!resp.ok) console.warn('/v1/launch failed:', resp.status, await resp.text().catch(() => ''))
  } catch (err) {
    console.warn('/v1/launch error:', err)
  }
}

/**
 * Check + clear the pending-launch flag. Returns true if a launch was
 * kicked off within `maxAgeMs` and the caller should auto-select the
 * newly-arrived session.
 */
function consumePendingLaunch(maxAgeMs = 10_000): boolean {
  if (!_pendingLaunchAt) return false
  const fresh = Date.now() - _pendingLaunchAt < maxAgeMs
  _pendingLaunchAt = 0
  return fresh
}

// ── Initialization ──────────────────────────────────────────────────────────

const USE_MOCK = import.meta.env.VITE_MOCK === '1' ||
  (typeof location !== 'undefined' && location.search.includes('mock'))

/** Navigation callback: set by App on mount so the store can navigate. */
let _navigate: ((url: string, replace?: boolean) => void) | null = null

export function setNavigate(fn: (url: string, replace?: boolean) => void) {
  _navigate = fn
}

export function navigate(url: string, replace?: boolean) {
  _navigate?.(url, replace)
}

/**
 * Navigate to a session by ID. Finds the matching project and builds
 * the URL. Used by auto-select, resume, and notification handlers.
 */
export function navigateToSession(sessionId: string, replace?: boolean) {
  const sess = sessions.value.find(s => s.id === sessionId)
  if (!sess) return
  const project = matchSession(sess, projects.value)
  if (project) navigate(sessionPath(project.slug, sess), replace)
}

/**
 * Start the store: connect SSE, fetch initial data, start timers.
 * Call once from the app root.
 */
export function initStore(): () => void {
  const cleanups: (() => void)[] = []

  if (USE_MOCK) {
    const localHost = new URLSearchParams(location.search).get('host')
    const mockSessions = localHost
      ? MOCK_SESSIONS.map(s => s.peer === localHost ? { ...s, peer: undefined } : s)
      : [...MOCK_SESSIONS]
    batch(() => {
      projects.value = MOCK_PROJECTS
      sessions.value = mockSessions
      sessionsLoaded.value = true
      connState.value = 'connected'
      terminalOptions.value = buildTerminalOptions(null, null)
      keybinds.value = resolveKeybinds(null, false)
    })
    const activeIds = MOCK_SESSIONS.filter(s => s.mockActive).map(s => s.id)
    activeIds.forEach(id => handleActivity(id))
    const tick = setInterval(() => activeIds.forEach(id => handleActivity(id)), 2000)
    cleanups.push(() => clearInterval(tick))
    return () => cleanups.forEach(fn => fn())
  }

  // Fetch initial data in parallel.
  fetchProjects()
  fetchSessions().then(list => {
    batch(() => {
      sessions.value = list
      sessionsLoaded.value = true
      connState.value = 'connected'
    })
  }).catch(err => {
    console.error('Failed to fetch sessions:', err)
    connState.value = 'error'
  })
  fetchHealth()
  fetchFrontendConfig().then(fc => {
    const macCtrl = fc.settings?.macCommandIsCtrl === true
    batch(() => {
      terminalOptions.value = buildTerminalOptions(fc.settings, fc.themeColors)
      macCommandIsCtrl.value = macCtrl
      keybinds.value = resolveKeybinds(fc.settings?.keybinds ?? null, macCtrl)
    })
  })

  // SSE subscription.
  //
  // The server replays all sessions as upserts on connect. Since we
  // already fetch via GET /v1/sessions, the initial SSE dump is
  // redundant. We skip session-upsert events until the bulk fetch
  // has completed (sessionsLoaded is true). After that, the SSE
  // stream carries incremental updates.
  //
  // On reconnect, the SSE dump IS useful because events may have been
  // missed. We pair it with a fresh fetchSessions to be safe.
  const source = new EventSource('/v1/events')
  let sseConnected = false

  source.addEventListener('open', () => {
    if (sseConnected) {
      // Reconnect: refresh everything to catch missed events.
      fetchProjects()
      fetchSessions().then(list => { sessions.value = list }).catch(() => {})
    }
    sseConnected = true
  })

  source.addEventListener('session-upsert', (e) => {
    // Skip the initial SSE dump: the bulk GET /v1/sessions fetch is
    // authoritative for the first load. Processing the dump would
    // trigger O(n²) array mutations for no benefit.
    if (!sessionsLoaded.value) return

    try {
      const envelope = JSON.parse(e.data)
      const session = envelope.session ?? envelope
      const isNew = upsertSession(session)
      if (isNew && consumePendingLaunch()) {
        navigateToSession(session.id, true)
      }
    } catch (err) {
      console.warn('session-upsert: bad event', err)
    }
  })

  source.addEventListener('session-remove', (e) => {
    try {
      const { id } = JSON.parse(e.data)
      removeSession(id)
    } catch (err) {
      console.warn('session-remove: bad event', err)
    }
  })

  source.addEventListener('session-activity', (e) => {
    try {
      const { id } = JSON.parse(e.data)
      if (id) handleActivity(id)
    } catch { /* ignore */ }
  })

  source.addEventListener('projects-update', () => {
    fetchProjects()
  })

  source.addEventListener('peer-status', () => {
    fetchHealth()
  })

  cleanups.push(() => source.close())

  // URL normalization effect: rewrites the URL when the resolved view
  // differs from the current path (e.g., `/:project` resolves to a
  // specific session). Gated on sessionsLoaded to prevent the race
  // where projects load first and clobber the URL before sessions arrive.
  const disposeUrlNorm = effect(() => {
    const v = view.value
    if (v === null) return
    if (!sessionsLoaded.value) return
    const url = viewToPath(v, projects.value, sessions.value)
    if (url && url !== urlPath.value) {
      navigate(url, true)
    }
  })
  cleanups.push(disposeUrlNorm)

  // Mark-as-read effect: clear unread/error flags when viewing a session.
  const disposeMarkRead = effect(() => {
    const id = selectedId.value
    const sess = selected.value
    if (!id || !sess) return
    if (sess.unread || sess.status?.error) {
      markSessionRead(id)
    }
  })
  cleanups.push(disposeMarkRead)

  return () => cleanups.forEach(fn => fn())
}
