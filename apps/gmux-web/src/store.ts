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
import { buildProjectFolders, matchSession, discoverProjects, countUnmatchedActive } from './projects'

import { fetchFrontendConfig, buildTerminalOptions, resolveKeybinds, type ResolvedKeybind } from './config'
import { MOCK_SESSIONS, MOCK_PROJECTS } from './mock-data/index'
import type { ResolvedTerminalOptions } from './settings-schema'
import type { Session as ProtocolSession } from '@gmux/protocol'

// ── HealthData type (used by both raw signal and consumers) ─────────────────

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

// ── Raw state (private; ADR 0001) ───────────────────────────────────────────
//
// Per ADR 0001 the wire delivers two snapshots: `snapshot.sessions`
// (just the sessions array) and `snapshot.world` (the bundle of
// projects + peers + health + launchers). The frontend stores those
// two payloads verbatim in `_rawSessions` and `_rawWorld`; everything
// else is a pure projection (computed) on top.
//
// The signals are exported with a leading underscore as a soft
// "private" marker. SSE handlers, bulk-fetch helpers, and the test
// suite write to them; the rest of the app reads only the public
// computed projections below.

export interface RawWorld {
  projects: ProjectItem[]
  peers: PeerInfo[]
  health: HealthData | null
  launchers: LauncherDef[]
  defaultLauncher: string
}

export const _rawSessions = signal<Session[]>([])
export const _rawWorld = signal<RawWorld>({
  projects: [],
  peers: [],
  health: null,
  launchers: [],
  defaultLauncher: 'shell',
})

/** Merge a partial world update into `_rawWorld`. Used by SSE handlers,
 * bulk-fetch responses, and tests; callers don't have to spread the
 * whole bundle every time. */
export function _setRawWorld(patch: Partial<RawWorld>) {
  _rawWorld.value = { ..._rawWorld.value, ...patch }
}

// ── Pending mutations (optimistic overlay; ADR 0001) ───────────────────────
//
// The wire delivers atomic snapshots that overwrite `_rawSessions` /
// `_rawWorld` wholesale. Optimistic UI mutations (mark-as-read,
// dismiss, reorder) need to survive that overwrite until the server
// echoes them back. We do that by stacking mutations in
// `_pendingMutations` and replaying them on top of raw in the public
// projections.
//
// Each mutation is auto-cleared two ways:
//   1. when raw state already reflects the mutation
//      (`isResolved` returns true), the next raw update sweeps it out;
//   2. otherwise it expires after `PENDING_TTL_MS`, so a server that
//      silently drops the request can't pin a stale optimistic value.

export type PendingMutation =
  | { kind: 'mark-read'; id: string; at: number }
  | { kind: 'dismiss'; id: string; at: number }
  | { kind: 'reorder'; slug: string; sessions: string[]; at: number }

export const _pendingMutations = signal<PendingMutation[]>([])

const PENDING_TTL_MS = 5_000

/** Replay pending mutations on top of a raw sessions/projects pair.
 * Pure; safe to call from `computed`. */
export function applyPending(
  rawSessions: Session[],
  rawProjects: ProjectItem[],
  pending: PendingMutation[],
): { sessions: Session[]; projects: ProjectItem[] } {
  if (pending.length === 0) return { sessions: rawSessions, projects: rawProjects }
  let sess = rawSessions
  let projs = rawProjects
  for (const m of pending) {
    switch (m.kind) {
      case 'mark-read':
        sess = sess.map(s => s.id !== m.id ? s : ({
          ...s,
          unread: false,
          status: s.status?.error ? { ...s.status, error: false } : s.status,
        }))
        break
      case 'dismiss':
        sess = sess.filter(s => s.id !== m.id)
        break
      case 'reorder':
        projs = projs.map(p => p.slug !== m.slug ? p : ({ ...p, sessions: m.sessions }))
        break
    }
  }
  return { sessions: sess, projects: projs }
}

/** True when the raw state already reflects the mutation, so replaying
 * it would be a no-op. The auto-clear effect uses this to drop
 * mutations the server has acknowledged. */
function isResolved(
  m: PendingMutation,
  rawSessions: Session[],
  rawProjects: ProjectItem[],
): boolean {
  switch (m.kind) {
    case 'mark-read': {
      const s = rawSessions.find(x => x.id === m.id)
      if (!s) return true
      return !s.unread && !s.status?.error
    }
    case 'dismiss':
      return !rawSessions.some(s => s.id === m.id)
    case 'reorder': {
      const p = rawProjects.find(x => x.slug === m.slug)
      if (!p) return true
      const cur = p.sessions ?? []
      return cur.length === m.sessions.length && cur.every((v, i) => v === m.sessions[i])
    }
  }
}

/** Push a mutation onto the pending stack and schedule its TTL drop. */
function addPending(m: PendingMutation) {
  _pendingMutations.value = [..._pendingMutations.value, m]
  setTimeout(() => {
    _pendingMutations.value = _pendingMutations.value.filter(x => x !== m)
  }, PENDING_TTL_MS)
}

// ── Public projections of raw state ─────────────────────────────────────────
//
// Components import these by name; they don't know about `_rawWorld`.
// Everything is `computed`, so writes go through the raw signals only.

export const sessions = computed<Session[]>(() =>
  applyPending(_rawSessions.value, _rawWorld.value.projects, _pendingMutations.value).sessions,
)
export const projects = computed<ProjectItem[]>(() =>
  applyPending(_rawSessions.value, _rawWorld.value.projects, _pendingMutations.value).projects,
)
export const peers = computed<PeerInfo[]>(() => _rawWorld.value.peers)
export const health = computed<HealthData | null>(() => _rawWorld.value.health)
export const launchers = computed<LauncherDef[]>(() => _rawWorld.value.launchers)
export const defaultLauncher = computed<string>(() => _rawWorld.value.defaultLauncher)

// Auto-clear pending mutations that the wire has acknowledged. Runs on
// every raw update; uses .peek() to avoid re-triggering itself.
effect(() => {
  const rs = _rawSessions.value
  const rw = _rawWorld.value
  const pending = _pendingMutations.peek()
  if (pending.length === 0) return
  const filtered = pending.filter(m => !isResolved(m, rs, rw.projects))
  if (filtered.length !== pending.length) {
    _pendingMutations.value = filtered
  }
})

// Local-only UI state (never sourced from the wire).
export const sessionsLoaded = signal(false)
export const connState = signal<'connecting' | 'connected' | 'error'>('connecting')

// Per ADR 0001: Discovered and UnmatchedActiveCount are per-viewer
// projections, not server-pushed state. They derive from the same
// public sessions/projects projections everyone else uses.
export const discovered = computed<DiscoveredProject[]>(
  () => discoverProjects(sessions.value, projects.value),
)
export const unmatchedActiveCount = computed<number>(
  () => countUnmatchedActive(sessions.value, projects.value),
)

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

/** Simple string hash (djb2) mapped to palette index. */
function hashPaletteIndex(s: string): number {
  let h = 5381
  for (let i = 0; i < s.length; i++) h = ((h << 5) + h + s.charCodeAt(i)) | 0
  return (h >>> 0) % PEER_PALETTE.length
}

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

/** Derived map from peer name to status string. Sessions whose peer
 *  is not 'connected' are unreachable from this viewer right now (the
 *  peer may still be running them); the sidebar dims them and replaces
 *  the activity dot with an unavailable indicator. */
export const peerStatusByName = computed<ReadonlyMap<string, string>>(() => {
  const map = new Map<string, string>()
  for (const p of peers.value) map.set(p.name, p.status)
  return map
})

/** True when a session lives on a peer we can't reach right now.
 *  Local sessions (peer === undefined) are never unavailable. */
export function isSessionUnavailable(
  session: { peer?: string },
  statusByName: ReadonlyMap<string, string>,
): boolean {
  if (!session.peer) return false
  // Treat unknown peers as unavailable too: if the session claims a
  // peer name no longer present in the world snapshot (e.g. peer was
  // removed from config but still appears in lingering session data),
  // the safe default is to flag it rather than pretend it's reachable.
  const status = statusByName.get(session.peer)
  return status !== 'connected'
}

/** Derived map from peer name to { label, color, bg }. Colors assigned by list order. */
export const peerAppearance = computed<ReadonlyMap<string, PeerAppearance>>(() => {
  const names = peers.value.map(p => p.name)
  const prefixes = uniquePrefixes(names)
  const map = new Map<string, PeerAppearance>()
  for (const name of names) {
    const [color, bg] = PEER_PALETTE[hashPaletteIndex(name)]
    map.set(name, { label: prefixes.get(name)!, color, bg })
  }
  return map
})

export const terminalOptions = signal<ResolvedTerminalOptions | null>(null)
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

/**
 * Folder key when the view is a project hub. Matches `Folder.key`
 * (`${peer ?? ''}::${slug}`) so the sidebar can highlight the active
 * folder uniformly across local and peer-owned projects (ADR 0002).
 */
export const currentProjectKey = computed(() => {
  const v = view.value
  if (v?.kind !== 'project') return null
  return `${v.projectPeer ?? ''}::${v.projectSlug}`
})

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
  const prev = _rawSessions.value
  const idx = prev.findIndex(s => s.id === updated.id)
  if (idx >= 0) {
    const old = prev[idx]
    const next = [...prev]
    next[idx] = updated

    // When the currently-selected session changes slug, update the URL
    // atomically with the session data. Without batch(), the view
    // computed would see the new sessions (slug changed) but the old
    // URL (still has the old slug), fail to resolve, and briefly
    // deselect the session.
    if (old.slug !== updated.slug && selectedId.value === updated.id) {
      const project = matchSession(updated, projects.value)
      if (project) {
        const newUrl = sessionPath(project.slug, updated)
        batch(() => {
          _rawSessions.value = next
          urlPath.value = newUrl
        })
        // Sync the browser URL bar. navigate() calls preact-iso's
        // loc.route which would also set urlPath via the
        // useLayoutEffect in App, but we already set it above
        // inside the batch for atomicity.
        navigate(newUrl, true)
        return isNew
      }
    }

    _rawSessions.value = next
  } else {
    isNew = true
    _rawSessions.value = [...prev, updated]
  }
  return isNew
}

export function removeSession(id: string) {
  _rawSessions.value = _rawSessions.value.filter(s => s.id !== id)
}

export function markSessionRead(id: string) {
  // Optimistic mark-as-read. The server's next session update overwrites
  // raw with the authoritative state and `isResolved` clears the
  // mutation; if the server stays silent the TTL drops it eventually.
  addPending({ kind: 'mark-read', id, at: Date.now() })
  fetch(`/v1/sessions/${id}/read`, { method: 'POST' }).catch(() => {})
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

/**
 * Persist a new session order for a project. The `sessionKeys` array
 * contains session keys (slug or id) in the desired display order.
 *
 * Local projects (peer === undefined) get an optimistic overlay via
 * `_pendingMutations` so the sidebar re-renders immediately, before
 * the snapshot.world round-trip echoes the new order back.
 *
 * Peer-owned projects route through the generic peer-write proxy at
 * `/v1/peers/{peer}/v1/projects/{slug}/sessions` (ADR 0002): the peer
 * owns its own projects.json and re-stamps each session's
 * project_index, which arrives over the snapshot stream. We don't
 * apply a local optimistic overlay for peer reorders because the
 * sidebar derives peer-folder order from those stamps, not from any
 * local projects array; the round-trip latency is the cost of
 * honesty about who owns the data.
 */
export async function reorderSessions(
  projectSlug: string,
  sessionKeys: string[],
  peer?: string,
): Promise<void> {
  if (peer === undefined) {
    addPending({ kind: 'reorder', slug: projectSlug, sessions: sessionKeys, at: Date.now() })
  }
  const url = peer
    ? `/v1/peers/${encodeURIComponent(peer)}/v1/projects/${encodeURIComponent(projectSlug)}/sessions`
    : `/v1/projects/${encodeURIComponent(projectSlug)}/sessions`
  try {
    const resp = await fetch(url, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ sessions: sessionKeys }),
    })
    if (!resp.ok) console.warn('PATCH sessions failed:', resp.status)
  } catch (err) {
    console.warn('PATCH sessions error:', err)
  }
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
  // Optimistic dismissal: hide the session locally until the next
  // snapshot.sessions confirms it's gone. If the server rejects the
  // dismiss, the mutation TTL-expires and the session reappears on the
  // following snapshot.
  addPending({ kind: 'dismiss', id: sessionId, at: Date.now() })
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
 * Returns true when a URL change was actually dispatched, false when
 * the session or its project hasn't loaded yet.
 */
export function navigateToSession(sessionId: string, replace?: boolean): boolean {
  const sess = sessions.value.find(s => s.id === sessionId)
  if (!sess) return false
  const project = matchSession(sess, projects.value)
  if (!project) return false
  navigate(sessionPath(project.slug, sess), replace)
  return true
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
      _setRawWorld({ projects: MOCK_PROJECTS })
      _rawSessions.value = mockSessions
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

  // Fetch one-shot per-user config (theme, settings, keybinds) that
  // doesn't ride the snapshot stream. Everything else — sessions,
  // projects, peers, health, launchers — arrives as the leading-edge
  // snapshot.sessions / snapshot.world emitted on SSE subscribe
  // (ADR 0001).
  fetchFrontendConfig().then(fc => {
    const macCtrl = fc.settings?.macCommandIsCtrl === true
    batch(() => {
      terminalOptions.value = buildTerminalOptions(fc.settings, fc.themeColors)
      macCommandIsCtrl.value = macCtrl
      keybinds.value = resolveKeybinds(fc.settings?.keybinds ?? null, macCtrl)
    })
  })

  // SSE subscription. The server emits a leading-edge snapshot for
  // both kinds (sessions, world) immediately on subscribe, so we
  // don't need a bulk-GET prefetch on first load or on reconnect.
  // Missed deltas don't matter: each snapshot is a full replacement.
  const source = new EventSource('/v1/events')
  source.addEventListener('error', () => {
    // Browser EventSource auto-reconnects; flag the UI as degraded
    // until the next snapshot arrives. `sessionsLoaded` stays true
    // once it has flipped, so reconnect doesn't blank the sidebar.
    if (connState.value === 'connecting') connState.value = 'error'
  })

  // Protocol 2 (ADR 0001). The server pushes two snapshot kinds plus
  // bare activity. We replace `_rawSessions` and `_rawWorld`
  // wholesale on each snapshot; the projection layer derives
  // everything else.
  source.addEventListener('snapshot.sessions', (e) => {
    try {
      const envelope = JSON.parse(e.data) as { sessions?: ProtocolSession[] }
      const list = (envelope.sessions ?? []).map(toUISession)

      // Detect newly-arrived IDs vs the previous snapshot so a
      // pending launch (just-POSTed /v1/launch awaiting an id) can
      // navigate to its session as soon as the daemon publishes it.
      // Done before we commit the new array so consumers see the
      // navigation against the new state.
      const prevIds = new Set(_rawSessions.value.map(s => s.id))
      const newIds = list.filter(s => !prevIds.has(s.id)).map(s => s.id)

      batch(() => {
        _rawSessions.value = list
        sessionsLoaded.value = true
        connState.value = 'connected'
      })

      if (newIds.length > 0 && consumePendingLaunch()) {
        // Most recent new id wins; with one launch in flight at a
        // time this is unambiguous.
        navigateToSession(newIds[newIds.length - 1], true)
      }
    } catch (err) {
      console.warn('snapshot.sessions: bad event', err)
    }
  })

  source.addEventListener('snapshot.world', (e) => {
    try {
      const env = JSON.parse(e.data) as {
        projects?: ProjectItem[]
        peers?: PeerInfo[]
        health?: HealthData
        launchers?: LauncherDef[]
        default_launcher?: string
      }
      _setRawWorld({
        projects: env.projects ?? [],
        peers: env.peers ?? env.health?.peers ?? [],
        health: env.health ?? null,
        launchers: env.launchers ?? [],
        defaultLauncher: env.default_launcher ?? 'shell',
      })
    } catch (err) {
      console.warn('snapshot.world: bad event', err)
    }
  })

  source.addEventListener('session-activity', (e) => {
    try {
      const { id } = JSON.parse(e.data)
      if (id) handleActivity(id)
    } catch { /* ignore */ }
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
