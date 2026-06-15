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
import type { Session, ProjectItem, DiscoveredProject, PeerInfo, PeerProject, LauncherDef, Folder } from './types'
import type { View } from './routing'
import { resolveViewFromPath, viewToPath } from './routing'
import { navigateWithReload } from './version-watch'
import { buildProjectFolders, discoverProjects } from './projects'
import { resolveReferences, removeReferenceItems, removeHostReferenceItems, refKey, type UnresolvedHost } from './references'

import { fetchFrontendConfig, buildTerminalOptions, resolveKeybinds, type ResolvedKeybind } from './config'
import { MOCK_SESSIONS, MOCK_PROJECTS, MOCK_PEERS, MOCK_HEALTH } from './mock-data/index'
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
  /**
   * Per-peer projection of each connected peer's owned projects.
   * Keyed by peer name. Drives the "On other hosts" section of
   * Manage Projects and lets references render their launch fallback
   * without proxying a separate request.
   */
  peerProjects: Record<string, PeerProject[]>
  /**
   * Per-peer authoritative discovered list, keyed by peer name.
   * Discovery is host-authoritative (ADR 0002/0005): each connected
   * peer advertises the sessions it owns but no project of its own
   * claims, and the viewer renders these rows verbatim rather than
   * recomputing peer discovery blind. The viewer's own (local) sessions
   * are discovered client-side; see the `discovered` computed.
   */
  peerDiscovered: Record<string, DiscoveredProject[]>
}

export const _rawSessions = signal<Session[]>([])
export const _rawWorld = signal<RawWorld>({
  projects: [],
  peers: [],
  health: null,
  launchers: [],
  defaultLauncher: 'shell',
  peerProjects: {},
  peerDiscovered: {},
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

/** Per-peer projects from the world snapshot. Map from peer name to
 *  its owned projects (slug + launch_cwd hint). Empty object when no
 *  peers are connected or none have fetched yet. */
export const peerProjects = computed<Record<string, PeerProject[]>>(
  () => _rawWorld.value.peerProjects,
)

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
/**
 * Whether the leading-edge `snapshot.world` (projects, peers, health)
 * has arrived at least once. Tracked separately from `sessionsLoaded`
 * because the daemon emits `snapshot.sessions` and `snapshot.world` as
 * two distinct SSE events (sessions first; ADR 0001). On a deep-link
 * refresh the sessions event lands while `projects` is still empty, and
 * resolving a local-project URL against an empty projects list yields
 * `home` — which the URL-normalization effect would then write to the
 * address bar, dropping the session the user was on. Gating the view on
 * *both* flags keeps it `null` until a coherent snapshot exists.
 */
export const worldLoaded = signal(false)
export const connState = signal<'connecting' | 'connected' | 'error'>('connecting')

// Discovered is host-authoritative (ADR 0002/0005): each host runs its
// own match rules over its own sessions and decides which are
// unclaimed. So this viewer computes discovery only for its OWN (local)
// sessions, and merges in each connected peer's self-advertised
// discovered list verbatim. A peer can therefore never offer (in
// Discovered) a project it already owns by a rule the viewer can't see.
//
// Per ADR 0001's note: "Discovered is a per-viewer projection." That
// remains true for the viewer's own local sessions (the viewer is their
// owner); for peer sessions the projection is the peer's own, relayed
// over the wire.
export const discovered = computed<DiscoveredProject[]>(() => {
  const lp = localPeerNames.value
  const local = discoverProjects(sessions.value, projects.value, (name) => lp.has(name))
  const statuses = peerStatusByName.value
  const peerRows: DiscoveredProject[] = []
  const byPeer = _rawWorld.value.peerDiscovered
  for (const [peerName, rows] of Object.entries(byPeer)) {
    // Disconnected peers contribute no rows: their advertised list may
    // be stale and "+ Add" would hit a host we can't reach.
    if (statuses.get(peerName) !== 'connected') continue
    for (const row of rows) {
      peerRows.push({ ...row, peer: peerName })
    }
  }
  return sortDiscovered([...local, ...peerRows])
})

/** Sort discovered suggestions by recency, then active count, then
 *  session count, then suggested_slug, then originating path. Mirrors
 *  the Go-side Discovered() sort so local and peer-advertised rows
 *  interleave consistently. */
function sortDiscovered(rows: DiscoveredProject[]): DiscoveredProject[] {
  return rows.sort((a, b) => {
    const ta = a.last_active ?? ''
    const tb = b.last_active ?? ''
    if (ta !== tb) return tb < ta ? -1 : 1
    if (a.active_count !== b.active_count) return b.active_count - a.active_count
    if (a.session_count !== b.session_count) return b.session_count - a.session_count
    const slugCmp = a.suggested_slug.localeCompare(b.suggested_slug)
    if (slugCmp !== 0) return slugCmp
    return (a.paths[0] ?? '').localeCompare(b.paths[0] ?? '')
  })
}

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
/**
 * Single source of truth for the visual dot state of a session, used
 * by both the sidebar row and the wide dashboard row. Encodes the
 * precedence: error > working > unread > recent terminal activity.
 * Selection-aware muting ("unread is suppressed when you're already
 * viewing the session") lives at the call site, not here.
 */
export function sessionDotState(
  session: Session,
  am: ReadonlyMap<string, 'active' | 'fading'>,
): DotState {
  if (session.alive && session.status?.error)   return 'error'
  if (session.alive && session.status?.working) return 'working'
  if (session.unread)                            return 'unread'
  const act = am.get(session.id)
  if (act === 'active') return 'active'
  if (act === 'fading') return 'fading'
  return 'none'
}

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

/**
 * True while the on-screen keyboard is open, detected via visual-viewport
 * occlusion on touch devices (window.innerHeight - visualViewport.height
 * exceeds a threshold). Drives keyboard-aware layout (e.g. collapsing the
 * header on phones to reclaim rows). Set by App's viewport effect; CSS
 * decides whether/when a collapse actually applies.
 */
export const keyboardOpen = signal(false)

/** Current URL path, kept in sync with preact-iso's location. */
export const urlPath = signal(
  typeof location !== 'undefined' ? (location.pathname.replace(/\/+$/, '') || '/') : '/',
)

/** Current URL query string (including the leading '?'), kept in sync
 * with preact-iso's location alongside `urlPath`. Tracked as its own
 * signal so query-only changes (?project=, ?cwd=) reactively recompute
 * `filteredSessions` and its dependents (folders, view, sidebar)
 * without waiting for an unrelated SSE session update to nudge
 * `sessions.value`. */
export const urlSearch = signal(
  typeof location !== 'undefined' ? location.search : '',
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
  const params = new URLSearchParams(urlSearch.value)
  const project = params.get('project')
  const cwdFilter = params.get('cwd')
  if (!project && !cwdFilter) return sessions.value
  return sessions.value.filter(s => {
    if (project && !s.cwd.toLowerCase().includes(project.toLowerCase())) return false
    if (cwdFilter && !s.cwd.startsWith(cwdFilter)) return false
    return true
  })
})

/** Set of peer names that are Local (devcontainers, PeerConfig.Local).
 *  Local peers don't own their own project assignments; their sessions
 *  are stamped by the parent and bucket into local sidebar folders. */
export const localPeerNames = computed<ReadonlySet<string>>(() => {
  const set = new Set<string>()
  for (const p of peers.value) {
    if (p.local) set.add(p.name)
  }
  return set
})

/** Project folders for the sidebar, built from projects + sessions. */
/** Reference resolution against the live roster: maps each reference
 *  to the host's current name (rename-proof via node_id) or flags it
 *  unresolved. Single source of truth for folders, the Hosts tab, and
 *  the gear pip. (refs #270) */
export const resolvedReferences = computed(() =>
  resolveReferences(projects.value, peers.value),
)

/** Distinct referenced host names that match no roster peer. Drives
 *  the Hosts-tab "Referenced but not found" group and the gear pip. */
export const unresolvedHosts = computed<UnresolvedHost[]>(
  () => resolvedReferences.value.unresolved,
)

/** Build sidebar folders from an arbitrary session list. Shared by the
 *  visible `folders` (filtered by URL params) and the unfiltered folder
 *  set behind `unreadCount` (a global signal that must ignore the
 *  ?project=/?cwd= view filter). */
function foldersFrom(ss: Session[]): Folder[] {
  return buildProjectFolders(
    projects.value,
    ss,
    (name) => localPeerNames.value.has(name),
    _rawWorld.value.peerProjects,
    (peer, slug) => resolvedReferences.value.resolution.get(refKey(peer, slug)),
  )
}

export const folders = computed(() => foldersFrom(filteredSessions.value))

/**
 * Local host's display name, but only when the shown folders span more
 * than one host.
 *
 * Normally locally-owned project headers render no host suffix (the
 * viewer is on that host, so naming it is noise). Once projects from
 * more than one host are present, though, the local host becomes just
 * one host among several, and labelling every header — local included —
 * is clearer than leaving the local ones ambiguously bare. This yields
 * the name to fall back to in that case, or `undefined` when there's a
 * single host (or the daemon hasn't reported its hostname yet).
 */
export const localHostLabel = computed<string | undefined>(() => {
  const local = health.value?.hostname
  // Count distinct hosts among the shown folders. Local folders
  // (peer === undefined) are tracked separately from named peers
  // rather than via a '' sentinel, so a (misconfigured) peer named
  // '' can't masquerade as local and skew the threshold.
  const peerHosts = new Set<string>()
  let hasLocal = false
  for (const f of folders.value) {
    if (f.peer === undefined) hasLocal = true
    else peerHosts.add(f.peer)
  }
  const hostCount = peerHosts.size + (hasLocal ? 1 : 0)
  return hostCount > 1 ? local : undefined
})

/**
 * Current view, derived from the URL + data.
 *
 * Returns null until both the sessions and world snapshots have loaded
 * at least once. This prevents the URL normalization effect from
 * overwriting a deep session URL with a fallback before data arrives —
 * in particular before `projects` is populated, when a local-project
 * URL would otherwise mis-resolve to home. After loading, always
 * returns a concrete View (home/project/session).
 */
export const view = computed((): View | null => {
  if (!sessionsLoaded.value || !worldLoaded.value) return null
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

/** Count of unread sessions (excluding selected).
 *
 * Folder-derived rather than read off the raw `sessions` set, so the
 * attention blip counts only sessions stamped into a project in
 * projects.json (owned project or resolved reference) — discovered
 * (unstamped/unreferenced) sessions render nowhere in the sidebar and
 * must not ping for attention. `buildProjectFolders` buckets each
 * session into at most one folder, so summing across folders needs no
 * dedup.
 *
 * Built from the *unfiltered* session set (`foldersFrom(sessions)`),
 * not the visible `folders` (which honor the ?project=/?cwd= view
 * filter). The blip is a global signal: an unread session in another
 * project must still count while the user browses a filtered view. */
export const unreadCount = computed(() => {
  const sel = selectedId.value
  let n = 0
  for (const f of foldersFrom(sessions.value)) {
    for (const s of f.sessions) {
      if (s.id !== sel && s.alive && s.unread) n++
    }
  }
  return n
})

// ── Home dashboard partitioning ─────────────────────────────────────────────
//
// The home page surfaces sessions in three sections. Each session
// appears in at most one section; priority is Needs attention >
// Running > Recent. Sort within every section is newest-first by
// last_activity_at (falling back to created_at for sessions that
// have not transitioned yet, so a brand-new idle session shows at
// the top of Running and an old quiet one drops to the bottom).
//
// The idle-alive remainder is grouped into recency buckets (Last
// hour / Earlier today / Yesterday / Earlier this week) rather than a
// single capped list. Buckets give structure so the section can grow
// without truncation; anything older than a week drops off home (find
// it via the project page or search). Dead sessions are NOT included
// (they live on the project page).
export const RECENT_WINDOW_DAYS = 7
const MS_PER_DAY = 24 * 60 * 60 * 1000
const MS_PER_HOUR = 60 * 60 * 1000

/** One recency bucket on the home dashboard. */
export interface RecentBucket {
  label: string
  sessions: Session[]
}

function activityTimeMs(s: Session): number {
  // last_activity_at is canonical when present (daemon-stamped on
  // noteworthy transitions). For never-transitioned sessions, fall
  // back to created_at so they sort relative to peers rather than
  // landing at the epoch.
  const stamp = s.last_activity_at ?? s.created_at
  const t = Date.parse(stamp)
  return Number.isFinite(t) ? t : 0
}

function byActivityDesc(a: Session, b: Session): number {
  const dt = activityTimeMs(b) - activityTimeMs(a)
  if (dt !== 0) return dt
  // Stable tiebreaker on id. Sessions with identical timestamps
  // (notably corpses persisted before last_activity_at existed,
  // which all fall back to created_at) must sort identically
  // across re-renders. Without this, every SSE event rebuilds
  // sessions.value in a different order and the section visibly
  // re-orders the tied entries.
  return a.id < b.id ? -1 : a.id > b.id ? 1 : 0
}

/**
 * Pure partition of a session list into the dashboard sections:
 * Waiting (unread), Active (working), and recency buckets for the
 * idle-alive remainder. Exported so tests can drive it with fixtures
 * and `now` injected (real wall-clock time would otherwise make the
 * day boundaries untestable).
 *
 * Alive-only: dead sessions never appear on the home dashboard.
 * They live exclusively in the project page's "All sessions"
 * section (see partitionForProject). The home page is a triage
 * surface for sessions you can still interact with right now;
 * dead corpses would dilute the signal.
 */
export function partitionForHome(
  all: readonly Session[],
  now: number,
): { needsAttention: Session[]; running: Session[]; buckets: RecentBucket[] } {
  const needsAttention: Session[] = []
  const running: Session[] = []
  const leftover: Session[] = []

  for (const s of all) {
    // Dead sessions never surface on the home dashboard; they live
    // only in the project page's "All sessions" section.
    if (!s.alive) continue
    // status.error is intentionally NOT escalated here: most error
    // states come from background subcommands that exited non-zero
    // without the agent itself halting, so flagging them as "needs
    // attention" produces false alarms. The per-row error dot still
    // shows individually.
    if (s.unread) {
      needsAttention.push(s)
    } else if (s.status?.working) {
      running.push(s)
    } else {
      // Idle-alive: not unread, not working. Bucketed by recency below.
      leftover.push(s)
    }
  }
  needsAttention.sort(byActivityDesc)
  running.sort(byActivityDesc)
  leftover.sort(byActivityDesc)

  // Recency buckets. "Last hour" is a rolling 60-minute window and
  // takes priority; "Earlier today" / "Yesterday" use local-midnight
  // calendar boundaries (so an 11pm session reads as "yesterday", not
  // "20 hours ago"); "Earlier this week" is the rolling 2–7 day tail.
  // Sessions older than a week (or without a parseable timestamp) are
  // omitted from home.
  const d = new Date(now)
  const todayMidnight = new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime()
  // Let the Date constructor normalize the day rollover so the
  // boundary stays correct across DST transitions (a subtraction of
  // a fixed 24h would drift by an hour on spring-forward / fall-back).
  const yesterdayMidnight = new Date(d.getFullYear(), d.getMonth(), d.getDate() - 1).getTime()
  const hourAgo = now - MS_PER_HOUR
  const weekAgo = now - RECENT_WINDOW_DAYS * MS_PER_DAY

  const lastHour: Session[] = []
  const earlierToday: Session[] = []
  const yesterday: Session[] = []
  const earlierWeek: Session[] = []
  for (const s of leftover) {
    const t = activityTimeMs(s)
    if (t <= 0) continue
    if (t >= hourAgo) lastHour.push(s)
    else if (t >= todayMidnight) earlierToday.push(s)
    else if (t >= yesterdayMidnight) yesterday.push(s)
    else if (t >= weekAgo) earlierWeek.push(s)
  }

  const buckets: RecentBucket[] = [
    { label: 'Last hour', sessions: lastHour },
    { label: 'Earlier today', sessions: earlierToday },
    { label: 'Yesterday', sessions: yesterday },
    { label: 'Earlier this week', sessions: earlierWeek },
  ].filter(b => b.sessions.length > 0)

  return { needsAttention, running, buckets }
}

export const homePartition = computed(() =>
  partitionForHome(sessions.value, Date.now()),
)

/**
 * Project-page partition: same Needs-attention / Running buckets as
 * the home dashboard, but the third bucket holds *every* remaining
 * session in the project, not a recency-windowed subset. Rationale:
 * the project page is the user's exhaustive view of a project; an
 * idle shell from yesterday or an exited session from last week
 * must remain visible here or they're effectively unreachable (the
 * sidebar still lists them, but losing them from the project page
 * means losing them from the only dedicated project surface).
 *
 * Pure; tests drive it with fixtures.
 */
export function partitionForProject(
  all: readonly Session[],
): { needsAttention: Session[]; running: Session[]; rest: Session[] } {
  const needsAttention: Session[] = []
  const running: Session[] = []
  const rest: Session[] = []

  for (const s of all) {
    // Waiting/Active are alive-only here too (mirrors
    // partitionForHome): a dead session, even with unread output,
    // can't be "waiting on you" or "active" anymore. It belongs in
    // the All-sessions tail. See partitionForHome for why
    // status.error is also intentionally not escalated.
    if (s.alive && s.unread) {
      needsAttention.push(s)
    } else if (s.alive && s.status?.working) {
      running.push(s)
    } else {
      rest.push(s)
    }
  }
  needsAttention.sort(byActivityDesc)
  running.sort(byActivityDesc)
  rest.sort(byActivityDesc)
  return { needsAttention, running, rest }
}

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
    last_activity_at: s.last_activity_at ?? undefined,
    socket_path: s.socket_path ?? '',
    terminal_cols: s.terminal_cols ?? undefined,
    terminal_rows: s.terminal_rows ?? undefined,
    slug: s.slug ?? undefined,
    runner_version: s.runner_version ?? undefined,
    binary_hash: s.binary_hash ?? undefined,
    peer: s.peer ?? undefined,
    // Stamps from the session's origin host. Drive sidebar bucketing
    // under the references model: a session that arrives without
    // these is invisible (no folder claims it), so this passthrough
    // is load-bearing rather than incidental.
    project_slug: s.project_slug || undefined,
    project_index: s.project_index,
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
      // viewToPath gets the post-update sessions array so it sees the
      // new slug. Routes peer-owned sessions through `/@<peer>/...`
      // (ADR 0002) just like every other URL serializer.
      const newUrl = viewToPath(
        { kind: 'session', sessionId: updated.id },
        projects.value,
        next,
      )
      if (newUrl) {
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
  fetch(`/v1/sessions/${id}/read`, { method: 'POST' }).catch(() => {/* fire-and-forget; TTL handles failures */})
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

/** Remove an owned project by slug. References (peer + slug) are
 *  removed via removePeerReference, which keys on the (peer, slug)
 *  pair to handle same-slug coexistence with owned projects. */
export async function removeProject(slug: string): Promise<void> {
  await putProjects(projects.value.filter(p => p.peer || p.slug !== slug))
}

export async function addProject(
  req: { remote?: string; paths: string[] },
  peer?: string,
): Promise<{ slug: string }> {
  // For remote adds, proxy through the hub: /v1/peers/{peer}/v1/projects/add.
  // The peer applies the change to its own projects.json; we'll receive
  // the new items[] back via projects-update + fetchProjects.
  //
  // Throws on non-2xx or network failure. Callers that chain follow-up
  // work (auto-add a reference after a remote create) rely on this to
  // avoid the dangling-reference failure mode where the peer rejected
  // the add but the viewer's projects.json still gains a reference
  // pointing at a slug that doesn't exist upstream.
  //
  // Returns the actual slug the server assigned. The server may
  // deduplicate (e.g. "api" → "api-2" on collision), so callers must
  // not assume the client-suggested slug round-tripped unchanged —
  // referencing the wrong slug would produce an immediately dangling
  // reference.
  const path = peer
    ? `/v1/peers/${encodeURIComponent(peer)}/v1/projects/add`
    : '/v1/projects/add'
  let resp: Response
  try {
    resp = await fetch(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(req),
    })
  } catch (err) {
    console.warn(`POST ${path} error:`, err)
    throw err
  }
  if (!resp.ok) {
    const msg = `POST ${path} failed: ${resp.status}`
    console.warn(msg)
    throw new Error(msg)
  }
  const body = await resp.json() as { ok?: boolean; data?: { slug?: string } }
  const slug = body.data?.slug
  if (!slug) {
    throw new Error(`POST ${path}: missing slug in response`)
  }
  return { slug }
}

/** Append a reference item to local projects.json. The reference
 *  points at a peer-owned project; the peer's projects.json remains
 *  the source of truth for rules and session order. */
export async function addPeerReference(peer: string, slug: string): Promise<void> {
  const existing = projects.value
  if (existing.some(p => p.peer === peer && p.slug === slug)) return
  // Stamp the peer's stable node_id at creation (when known) so the
  // reference is rename-proof from the start: a later rename of the
  // host follows automatically rather than orphaning it. (refs #270)
  const node_id = peers.value.find(p => p.name === peer)?.node_id
  await putProjects([...existing, node_id ? { peer, slug, node_id } : { peer, slug }])
}

/** Parse a pasted connect URL of the form
 *  `https://host[/auth/login]?token=<token>` (printed by `gmuxd auth`)
 *  into its peer URL (the origin) and token. Returns null when the
 *  input has no `token` query param so callers can treat it as a plain
 *  URL and use the separate token field (ADR 0008). */
export function parseConnectURL(input: string): { url: string; token: string } | null {
  let parsed: URL
  try {
    parsed = new URL(input.trim())
  } catch {
    return null
  }
  const token = parsed.searchParams.get('token')
  if (!token) return null
  return { url: parsed.origin, token }
}

/** Connect to a host (ADR 0007): POST /v1/peers probes the target,
 *  dedups by node_id, and persists it to peers.json. Returns the name
 *  the peer was stored under (may be suffixed on a collision) and
 *  whether it was already connected. Throws with the server message. */
export async function connectHost(
  url: string,
  token: string,
): Promise<{ name: string; alreadyConnected: boolean; updated: boolean }> {
  const resp = await fetch('/v1/peers', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ url, token }),
  })
  const body = await resp.json().catch(() => ({})) as {
    peer?: { name?: string }; already_connected?: boolean; updated?: boolean; error?: { message?: string }
  }
  if (!resp.ok) {
    throw new Error(body.error?.message || `Could not connect (${resp.status})`)
  }
  // updated: the host was already known and its URL/token were refreshed
  // (the "Add token" path). alreadyConnected: known with identical creds.
  return { name: body.peer?.name ?? '', alreadyConnected: !!body.already_connected, updated: !!body.updated }
}

/** Disconnect a manually-added host: DELETE /v1/peers/{name}. */
export async function disconnectHost(name: string): Promise<void> {
  const resp = await fetch(`/v1/peers/${encodeURIComponent(name)}`, { method: 'DELETE' })
  if (!resp.ok) {
    const body = await resp.json().catch(() => ({})) as { error?: { message?: string } }
    throw new Error(body.error?.message || `Could not disconnect (${resp.status})`)
  }
}

/** Remove a manual host from the roster and drop every project
 *  reference to it. Removal is deliberate, so its references go too —
 *  otherwise they'd resurface as "Referenced but not found" the moment
 *  the host left the roster. */
export async function removeHost(name: string, nodeId?: string): Promise<void> {
  await disconnectHost(name)
  const pruned = removeHostReferenceItems(projects.value, name, nodeId)
  if (pruned.length !== projects.value.length) await putProjects(pruned)
}

/** Remove a reference item from local projects.json. */
export async function removePeerReference(peer: string, slug: string): Promise<void> {
  const filtered = projects.value.filter(p => !(p.peer === peer && p.slug === slug))
  await putProjects(filtered)
}

/** Drop the unresolved references `(peer, slug)` for the given slugs.
 *  Scoped to the surfaced slugs so a same-named reference that still
 *  resolves correctly (via node_id) is never deleted. */
export async function removeReferences(peer: string, slugs: readonly string[]): Promise<void> {
  await putProjects(removeReferenceItems(projects.value, peer, slugs))
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
  // When the bundle has drifted from the daemon, the version watcher
  // converts the next in-app navigation into a full document load.
  // The user perceives it as a normal route transition that happens
  // to also pick up the new bundle. (The store ↔ version-watch
  // module cycle is safe: neither side touches the other at top
  // level.)
  if (navigateWithReload(url, replace)) return
  _navigate?.(url, replace)
}

/**
 * Navigate to a session by ID. Builds the URL via viewToPath so peer
 * ownership and disclaimed-but-adopted cases both serialize correctly
 * (ADR 0002). Used by auto-select, resume, and notification handlers.
 * Returns true when a URL change was actually dispatched, false when
 * the session or its containing project hasn't loaded yet.
 */
export function navigateToSession(sessionId: string, replace?: boolean): boolean {
  const path = viewToPath(
    { kind: 'session', sessionId },
    projects.value,
    sessions.value,
  )
  if (!path) return false
  navigate(path, replace)
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
      _setRawWorld({ projects: MOCK_PROJECTS, peers: MOCK_PEERS, health: MOCK_HEALTH })
      _rawSessions.value = mockSessions
      sessionsLoaded.value = true
      worldLoaded.value = true
      connState.value = 'connected'
      terminalOptions.value = buildTerminalOptions(null, null)
      keybinds.value = resolveKeybinds(null, false)
    })
    const activeIds = MOCK_SESSIONS.filter(s => s.mockActive).map(s => s.id)
    activeIds.forEach(id => { handleActivity(id) })
    const tick = setInterval(() => activeIds.forEach(id => { handleActivity(id) }), 2000)
    cleanups.push(() => clearInterval(tick))
    return () => cleanups.forEach(fn => { fn() })
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
        peer_projects?: Record<string, PeerProject[]>
        peer_discovered?: Record<string, DiscoveredProject[]>
      }
      batch(() => {
        _setRawWorld({
          projects: env.projects ?? [],
          peers: env.peers ?? env.health?.peers ?? [],
          health: env.health ?? null,
          launchers: env.launchers ?? [],
          defaultLauncher: env.default_launcher ?? 'shell',
          peerProjects: env.peer_projects ?? {},
          peerDiscovered: env.peer_discovered ?? {},
        })
        worldLoaded.value = true
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
  // specific session). `view` is null until both the sessions and
  // world snapshots have loaded, so the early `v === null` return
  // already gates this against the load-order race that would
  // otherwise clobber a deep-link URL before projects arrive.
  const disposeUrlNorm = effect(() => {
    const v = view.value
    if (v === null) return
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

  return () => cleanups.forEach(fn => { fn() })
}
