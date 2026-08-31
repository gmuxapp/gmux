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

import type { Session as ProtocolSession } from '@gmux/protocol'
import { batch, computed, effect, signal, untracked } from '@preact/signals'
import { buildTerminalOptions, fetchFrontendConfig, type ResolvedKeybind, resolveKeybinds } from './config'
import {
  createFamilyIndex, 
  type FamilyActivity, type FamilyIndex,familyAncestors, familyIndex, familyRootId, familyStateOf, isProcessSession, promotionAction,
} from './family'
import { MOCK_SESSIONS, mockWorld } from './mock-data/index'
import { isWaitingPresentation, type SessionPresentationState, sessionPresentationState } from './presentation'
import { buildProjectFolders, discoverProjects, type TemporaryPresentationPlacement } from './projects'
import {
  ACTIVITY_FACT_KEYS, diffReplacedRows, factsUnchanged, PLACEMENT_FACT_KEYS,
  reconcileSessions, SIDEBAR_FACT_KEYS, substituteRows,
} from './reconcile'
import { referencePresence, removeHostReferenceItems, removeReferenceItems, type UnresolvedHost, unresolvedReferences } from './references'
import type { View } from './routing'
import { resolveViewFromPath, viewToPath } from './routing'
import type { ResolvedTerminalOptions } from './settings-schema'
import { formatFilterParam, parseFilterParam, type Selector, sessionMatchesFilter } from './tab-filter'
import { pushError } from './toasts'
import type { DiscoveredProject, Folder, LauncherDef, PeerInfo, PeerProject, ProjectItem, Session } from './types'
import { navigateWithReload } from './version-watch'

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
   * Discovery is host-authoritative (ADR 0002/0025): each connected
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
// dismiss) need to survive that overwrite until the server echoes them
// back. We do that by stacking mutations in `_pendingMutations` and
// replaying them on top of raw in the public projection.
//
// Session *order* deliberately has no overlay. It reaches the UI only as
// server-stamped `project_index` (the daemon owns placement for local,
// Local-peer and peer sessions alike), so the only honest local preview
// is the in-drag one the sidebar renders from its own drag state; the
// commit is the server's to make. See `reorderSessions`.
//
// Each mutation is auto-cleared two ways:
//   1. when raw state already reflects the mutation
//      (`isResolved` returns true), the next raw update sweeps it out;
//   2. otherwise it expires after `PENDING_TTL_MS`, so a server that
//      silently drops the request can't pin a stale optimistic value.

export type PendingMutation =
  | { kind: 'mark-read'; id: string; token: string; at: number }
  | { kind: 'dismiss'; id: string; at: number }

export const _pendingMutations = signal<PendingMutation[]>([])

const PENDING_TTL_MS = 5_000

/** Replay pending mutations on top of a raw sessions array.
 * Pure; safe to call from `computed`. */
export function applyPending(
  rawSessions: Session[],
  pending: PendingMutation[],
): Session[] {
  if (pending.length === 0) return rawSessions
  // Indexed, then one pass: "mark all read" on a family stacks a
  // mutation per member, and a pass each would make every recompute in
  // the app cost mutations × sessions until the server echoed them back
  // — measurably, ~3ms a recompute for 250 of them over 1500 sessions.
  //
  // Order between the two kinds doesn't survive the rewrite, and
  // doesn't need to: for one id, dismissal wins either way round —
  // marking a removed session read is a no-op, and dismissing a
  // read-marked one still removes it.
  const readTokens = new Map<string, Set<string>>()
  const dismissed = new Set<string>()
  for (const m of pending) {
    if (m.kind === 'dismiss') dismissed.add(m.id)
    else {
      // Several tokens can be in flight for one session; each only
      // applies to the state it was issued against.
      const tokens = readTokens.get(m.id)
      if (tokens) tokens.add(m.token)
      else readTokens.set(m.id, new Set([m.token]))
    }
  }
  const out: Session[] = []
  for (const s of rawSessions) {
    if (dismissed.has(s.id)) continue
    out.push(readTokens.get(s.id)?.has(s.unread_token ?? '')
      ? { ...s, unread: false }
      : s)
  }
  return out
}

/** True when the raw state already reflects the mutation, so replaying
 * it would be a no-op. The auto-clear effect uses this to drop
 * mutations the server has acknowledged. */
function isResolved(m: PendingMutation, rawSessions: Session[]): boolean {
  switch (m.kind) {
    case 'mark-read': {
      const s = rawSessions.find(x => x.id === m.id)
      if (!s) return true
      if ((s.unread_token ?? '') !== m.token) return true
      return !s.unread
    }
    case 'dismiss':
      return !rawSessions.some(s => s.id === m.id)
  }
}

/** Push a mutation onto the pending stack and schedule its TTL drop.
 *  Returns a `retract` that drops the mutation immediately and idempotently
 *  — used by `optimistic()` to roll back as soon as the server rejects,
 *  instead of waiting out the full TTL (the lagged-rollback artifact). */
function addPending(m: PendingMutation): () => void {
  _pendingMutations.value = [..._pendingMutations.value, m]
  const retract = () => {
    _pendingMutations.value = _pendingMutations.value.filter(x => x !== m)
  }
  setTimeout(retract, PENDING_TTL_MS)
  return retract
}

/**
 * Run an optimistic mutation: apply it locally now, fire `action`, and
 * retract immediately if the action reports failure (rather than letting
 * the row linger until the TTL sweeps it). `action` returns whether
 * the server accepted it. Used by `dismissSession`;
 * `mark-read` deliberately opts out (it's fire-and-forget — a failed
 * mark-read silently self-heals on the next snapshot and needs no toast
 * or eager rollback).
 */
async function optimistic(m: PendingMutation, action: () => Promise<boolean>): Promise<void> {
  const retract = addPending(m)
  if (!(await action())) retract()
}

// ── Public projections of raw state ─────────────────────────────────────────
//
// Components import these by name; they don't know about `_rawWorld`.
// Everything is `computed`, so writes go through the raw signals only.

export const sessions = computed<Session[]>(() =>
  applyPending(_rawSessions.value, _pendingMutations.value),
)
export const projects = computed<ProjectItem[]>(() => _rawWorld.value.projects)

/** Conversation files that are live in more than one runner (session → file
 *  is authoritative per-runner; ADR 0011). Two alive sessions sharing a
 *  conversation_file means the same conversation is open in multiple tabs, which
 *  the UI surfaces as a warning. Keyed by conversation_file. */
export const duplicateConversationFiles = computed<Set<string>>(() => {
  const counts = new Map<string, number>()
  for (const s of sessions.value) {
    if (!s.alive || !s.conversation_file) continue
    counts.set(s.conversation_file, (counts.get(s.conversation_file) ?? 0) + 1)
  }
  const dups = new Set<string>()
  for (const [file, n] of counts) if (n > 1) dups.add(file)
  return dups
})
export const peers = computed<PeerInfo[]>(() => _rawWorld.value.peers)

/**
 * Per-peer session-stream omission markers from the world snapshot.
 * A hub daemon stamps peers[].sessions_omitted when a spoke's last committed
 * protocol-3 transaction quarantined rows at the sender, meaning that peer's
 * sessions in the merged list are knowingly incomplete. Bounded server-side;
 * defensively re-validated here because the values cross a trust boundary.
 */
export const peerStreamOmissions = computed<{ peer: string, count: number }[]>(() =>
  peers.value
    .map(p => ({ peer: p.name, count: p.sessions_omitted ?? 0 }))
    .filter(o => Number.isSafeInteger(o.count) && o.count > 0))

/** Total sessions omitted upstream across all peers. */
export const peerOmittedTotal = computed<number>(() =>
  peerStreamOmissions.value.reduce((n, o) => n + o.count, 0))
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
  const pending = _pendingMutations.peek()
  if (pending.length === 0) return
  const filtered = pending.filter(m => !isResolved(m, rs))
  if (filtered.length !== pending.length) {
    _pendingMutations.value = filtered
  }
})

// Local-only UI state (never sourced from the wire).
export type SessionStreamWarning = { id: string, code: string, message: string, count: number }
export const sessionStreamWarnings = signal<SessionStreamWarning[]>([])
export const sessionStreamOmittedTotal = signal(0)
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
// 'connecting'   — initial connect, never yet established (full-screen)
// 'connected'    — live snapshot flowing
// 'reconnecting' — an *established* stream dropped; EventSource is
//                  auto-reconnecting. Subtle pill, not full-screen: the
//                  last snapshot stays on screen (sessionsLoaded holds).
// 'error'        — initial connect failed (full-screen + Retry)
export const connState = signal<'connecting' | 'connected' | 'reconnecting' | 'error'>('connecting')

// Discovered is host-authoritative (ADR 0002/0025): each host runs its
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
/** Map the canonical semantic state to the app's established CSS vocabulary. */
export function presentationDotState(state: SessionPresentationState): DotState {
  switch (state) {
    case 'active': return 'working'
    case 'active-error': return 'active-error'
    case 'waiting': return 'unread'
    case 'waiting-error': return 'error'
    case 'none': return 'none'
  }
}

/** Single status/attention derivation used by row, header, family, and mobile
 * surfaces. Transient terminal activity is only a fallback after semantic
 * state has resolved to none. */
export function sessionDotState(
  session: Session,
  am: ReadonlyMap<string, 'active' | 'fading'>,
): DotState {
  const semantic = presentationDotState(sessionPresentationState(session))
  if (semantic !== 'none') return semantic
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
 * occlusion on touch devices: layout viewport (window.innerHeight) minus
 * visual viewport (visualViewport.height) above a threshold. Relies on the
 * keyboard shrinking only the visual viewport while the layout viewport
 * stays full — guaranteed on iOS Safari and, via the
 * interactive-widget=resizes-visual viewport meta, Chrome >=108. Browsers
 * that resize the layout viewport instead read ~0 and leave this false
 * (fail-safe: no header collapse). Drives keyboard-aware layout (e.g.
 * collapsing the header on phones to reclaim rows). Set by App's viewport
 * effect; CSS decides whether/when a collapse actually applies. See the
 * detection comment in main.tsx for the full cross-device matrix.
 */
export const keyboardOpen = signal(false)

/** True while the terminal viewport is scrolled up from the bottom (past a
 * small threshold). Lets a scroll-to-end control live outside the terminal
 * (the mobile toolbar) and stay in sync, without threading state through App. */
export const terminalScrolledUp = signal(false)

/** Scroll-to-bottom handle for the live terminal, or null when none is
 * mounted. Set by TerminalView, invoked by the toolbar's end key. */
export const terminalScrollToBottom = signal<(() => void) | null>(null)

/** True while the find-in-terminal bar is open. Lives here (not in
 * TerminalView state) so both the keybind handler (keyboard.ts) and the
 * session "⋮" menu (main.tsx) can open it without threading callbacks. */
export const terminalFindOpen = signal(false)

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

/** Current URL fragment (including the leading '#'). Kept separate from
 * `urlSearch`: fragments are not query data, but same-view rewrites such as
 * filter/sidebar edits and slug canonicalization must preserve them. */
export const urlHash = signal(
  typeof location !== 'undefined' ? location.hash : '',
)

/** Read the live fragment at a rewrite seam. `urlHash` drives reactive
 * effects and supplies DOM-less tests; the browser value also covers a
 * synchronous hash-only change before its `hashchange` event is delivered. */
function currentHash(fallback?: string): string {
  return typeof location !== 'undefined' ? location.hash : (fallback ?? urlHash.value)
}

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

/** Selectors parsed from the tab's `?filter=` param (see selectors.ts).
 *  The URL is the persistence layer for tab narrowing: pin a tab to
 *  `?filter=gmux` or `?filter=*@server`, bookmark it, and the browser's
 *  own tab management becomes gmux's window manager. */
export const activeSelectors = computed<Selector[]>(() =>
  parseFilterParam(new URLSearchParams(urlSearch.value).get('filter')),
)

/** Sessions narrowed by the tab's `?filter=` selectors. This scopes the
 *  tab's browsing surfaces — sidebar (both views), home dashboard, and
 *  the waiting badge — not just one list: a pinned tab that leaked
 *  global activity would defeat the pin. Management surfaces (Settings,
 *  project discovery) deliberately stay global. */
export const filteredSessions = computed(() => {
  const sel = activeSelectors.value
  if (sel.length === 0) return sessions.value
  const localHost = _rawWorld.value.health?.hostname
  return sessions.value.filter(s => sessionMatchesFilter(s, sel, localHost))
})

/** Rewrite the tab's `?filter=` param (null/empty clears it). Replaces
 *  the history entry: narrowing tweaks shouldn't pollute Back. Targets
 *  the full current URL so non-tab params (?settings, ?mock) survive a
 *  filter edit. */
export function setFilterSelectors(selectors: readonly Selector[]) {
  navigate(urlPath.value + urlSearch.value + currentHash(), true, {
    filter: formatFilterParam(selectors) || null,
  })
}

/** Href that carries the tab-identity params (?filter=, ?sidebar=).
 *  Every in-app link should go through this so navigating within a
 *  pinned tab keeps the pin. Same carry contract as `navigate()`
 *  (anchors bypass it: preact-iso routes the literal href), and reads
 *  the same signals, so links re-render when the tab's params change. */
export function tabHref(path: string): string {
  return withCarriedParams(path)
}

export function removeSelector(sel: Selector) {
  setFilterSelectors(activeSelectors.value.filter(
    s => !(s.project === sel.project && s.host === sel.host),
  ))
}

/** Convenience for the sidebar's Host menu: narrow the tab to one host
 *  (`*@host`), replacing any previous host-wide selector. `null` clears
 *  host narrowing but keeps project selectors. */
export function setHostFilter(host: string | null) {
  const keep = activeSelectors.value.filter(s => !(s.project === '*'))
  setFilterSelectors(host ? [...keep, { project: '*', host }] : keep)
}

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

/** Folder identity keys (`${ownerPeer}::${slug}`) for every catalog entry.
 *  Shared O(1) membership index for the per-session "does this stamp resolve
 *  to a folder?" checks in `foldersFrom`; recomputed only when the project
 *  catalog changes, never per snapshot. */
const projectKeys = computed<ReadonlySet<string>>(() => {
  const set = new Set<string>()
  for (const project of projects.value) set.add(`${project.peer ?? ''}::${project.slug}`)
  return set
})

/** Distinct referenced host names absent from the roster. Drives the
 *  Hosts-tab "Referenced but not found" group and the gear pip — a
 *  node_id/name membership check against the roster (ADR 0017). */
export const unresolvedHosts = computed<UnresolvedHost[]>(
  () => unresolvedReferences(projects.value, peers.value),
)

/** Build sidebar folders from an arbitrary session list. Shared by the
 *  visible `folders` (filtered by URL params) and the unfiltered folder
 *  set behind `unreadCount` (a global signal that must ignore the
 *  ?project=/?cwd= view filter). */
function foldersWith(ss: readonly Session[], index: FamilyIndex): Folder[] {
  const forceVisible = new Set<string>()
  const temporaryPlacements = new Map<string, TemporaryPresentationPlacement>()
  const selectedRoot = familyRootId(selectedId.value, index)
  if (selectedRoot) forceVisible.add(selectedRoot)
  for (const candidate of ss) {
    if (index.childIds.has(candidate.id)) {
      const root = index.rootById.get(candidate.id)
      if (!root) continue
      forceVisible.add(root.id)
      if (root.project_slug || !candidate.project_slug) continue
      const sessionPeer = candidate.peer ?? ''
      const ownerPeer = sessionPeer && !localPeerNames.value.has(sessionPeer) ? sessionPeer : ''
      if (!projectKeys.value.has(`${ownerPeer}::${candidate.project_slug}`)) continue
      const placement = { ownerPeer, slug: candidate.project_slug }
      const previous = temporaryPlacements.get(root.id)
      // A malformed/transitional family can briefly report children in more
      // than one folder. Pick a stable projection until the root stamp lands.
      if (!previous || `${placement.ownerPeer}::${placement.slug}` < `${previous.ownerPeer}::${previous.slug}`) {
        temporaryPlacements.set(root.id, placement)
      }
    }
  }
  return buildProjectFolders(
    projects.value,
    // Sidebar rows are family roots. Unpromoted semantic-agent children are
    // navigable in the family drawer but have no independent project row.
    // Resolve edges against the complete snapshot, not this tab-filtered
    // subset. A filtered-out parent must not turn its child into a fake root.
    ss.filter(s => !index.childIds.has(s.id)) as Session[],
    (name) => localPeerNames.value.has(name),
    _rawWorld.value.peerProjects,
    referencePresence(peers.value),
    forceVisible,
    temporaryPlacements,
  )
}

// ── Incremental projection memos (structural sharing; see reconcile.ts) ─────
//
// After snapshot reconciliation, the protocol-3 steady state reaching these
// projections is "same rows, a few object identities replaced". Each heavy
// projection keeps its last (input, deps, output); when the new input is a
// replaced-rows-only successor whose replaced rows kept that projection's
// structure facts, the previous output is patched by object substitution —
// O(changed + affected slices) — instead of rebuilt. Any structural change
// (insert/remove/reorder, a fact change, a dep change) falls through to the
// full rebuild, so output values are always identical to the uncached path
// (pinned by reconcile-differential.test.ts).

function sameDeps(a: readonly unknown[], b: readonly unknown[]): boolean {
  for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return false
  return true
}

function substituteFolders(prev: Folder[], replaced: ReadonlyMap<Session, Session>): Folder[] {
  if (replaced.size === 0) return prev
  let changed = false
  const next = prev.map(f => {
    const swapped = substituteRows(f.sessions, replaced)
    if (swapped === f.sessions) return f
    changed = true
    return { ...f, sessions: swapped as Session[] }
  })
  return changed ? next : prev
}

/** Everything `foldersWith` reads besides the session rows themselves. Any
 * identity change here disables the fast path for one recompute. */
function foldersDeps(): readonly unknown[] {
  return [
    projects.value, peers.value, _rawWorld.value.peerProjects,
    localPeerNames.value, projectKeys.value, selectedId.value,
  ]
}

function makeFoldersMemo(): (ss: readonly Session[]) => Folder[] {
  let last: {
    input: readonly Session[]; all: readonly Session[]
    deps: readonly unknown[]; out: Folder[]
  } | null = null
  return (ss: readonly Session[]): Folder[] => {
    const all = sessions.value
    const deps = foldersDeps()
    if (last && sameDeps(last.deps, deps)) {
      // Folder structure reads the *whole* snapshot (family edges, roots of
      // filtered-out children), so the facts must hold for every replaced
      // row in `sessions`, not just those in `ss`. The `ss` diff proves
      // membership/order of the input list is unchanged; substitution then
      // carries the replaced row objects into the previous folder slices.
      const dAll = diffReplacedRows(last.all, all)
      if (dAll && factsUnchanged(dAll, PLACEMENT_FACT_KEYS) && diffReplacedRows(last.input, ss)) {
        const out = substituteFolders(last.out, dAll)
        last = { input: ss, all, deps, out }
        return out
      }
    }
    const out = foldersWith(ss, familyIndex(all))
    last = { input: ss, all, deps, out }
    return out
  }
}

// One memo per call site: `folders` (sidebar-scoped input) and `unreadCount`
// (filter-scoped input) see different lists and must not evict each other.
const sidebarFoldersMemo = makeFoldersMemo()
const unreadFoldersMemo = makeFoldersMemo()

function substituteBuckets(prev: DayBucket[], replaced: ReadonlyMap<Session, Session>): DayBucket[] {
  if (replaced.size === 0) return prev
  let changed = false
  const next = prev.map(b => {
    const swapped = substituteRows(b.sessions, replaced)
    if (swapped === b.sessions) return b
    changed = true
    return { ...b, sessions: swapped as Session[] }
  })
  return changed ? next : prev
}

/** Memoized `partitionByDay`: bucket keys and in-bucket order derive only
 * from the activity timestamps, so replaced rows that kept them slot into
 * the previous buckets by substitution. Day-scoped: crossing local midnight
 * invalidates (labels and bucket keys move). */
function makePartitionMemo(): (input: readonly Session[], now: number) => DayBucket[] {
  let last: { input: readonly Session[]; todayMid: number; out: DayBucket[] } | null = null
  return (input: readonly Session[], now: number): DayBucket[] => {
    const todayMid = localMidnight(now)
    if (last && last.todayMid === todayMid) {
      const d = diffReplacedRows(last.input, input)
      if (d && factsUnchanged(d, ACTIVITY_FACT_KEYS)) {
        const out = substituteBuckets(last.out, d)
        last = { input, todayMid, out }
        return out
      }
    }
    const out = partitionByDay(input, now)
    last = { input, todayMid, out }
    return out
  }
}

const sidebarActivityMemo = makePartitionMemo()
const homePartitionMemo = makePartitionMemo()

/**
 * Local-midnight timestamp the day-partition projections depend on.
 *
 * Pre-reconciliation, `partitionByDay`'s day-relative labels ("Today",
 * "Yesterday", "Last <weekday>") were re-evaluated on every snapshot commit
 * because the array identity always changed. With structural sharing, an
 * identical re-encoded snapshot (e.g. an SSE reconnect replaying an
 * unchanged world after laptop wake) commits as a signal no-op — so the
 * partitions must carry their clock input as an explicit dependency instead
 * of riding on array-identity churn. `refreshDayBoundary()` runs at the
 * snapshot commit seam and writes only when the local day actually moved,
 * so identical snapshots stay O(reconcile) with zero recomputation except
 * the (necessary) partition re-evaluation across midnight.
 */
const dayBoundary = signal(localMidnight(Date.now()))

function refreshDayBoundary(): void {
  const mid = localMidnight(Date.now())
  if (mid !== dayBoundary.value) dayBoundary.value = mid
}

/** The single source of truth for which sessions are eligible for the
 *  sidebar. Projects places this list into configured folders; Activity
 *  rearranges that placed set, so unstamped/unreferenced sessions that
 *  cannot appear in Projects cannot leak into Activity either. Every
 *  user-facing inclusion rule lives here:
 *
 *   1. the tab's `?filter=` scope (via `filteredSessions`),
 *   2. a baseline of "sessions you can still act on" — alive or
 *      resumable (a truly-gone corpse is unreachable, so it's dropped),
 *   3. the alive-only toggle, which narrows (2) to just alive,
 *   4. the selected session, always kept — you can't navigate away from
 *      what you're looking at, even if (1)–(3) would hide it.
 *
 *  Distinct from routing: `view` / `selectedId` resolve against the full
 *  `sessions` (filter-blind), so a filter never evicts the open
 *  terminal; rule 4 only ever *adds* the selected session back here. */
function sidebarSessionsWith(index: FamilyIndex): Session[] {
  const sel = selectedId.value
  const onlyAlive = aliveOnly.value
  const base = filteredSessions.value.filter(s =>
    s.id === sel || (onlyAlive ? s.alive : s.alive || s.resumable),
  )
  // Membership set mirrors `out` so dedup is O(1) per push instead of an
  // `out.some` scan per candidate (the old quadratic hot spot).
  const out = [...base]
  const outIds = new Set<string>()
  for (const s of base) outIds.add(s.id)
  const push = (s: Session | undefined) => {
    if (s && !outIds.has(s.id)) { outIds.add(s.id); out.push(s) }
  }
  // Every relevant child keeps its presentation root locatable, even when the
  // root is dead or outside the tab filter. The child itself is later removed
  // from folders; this turns an active subtree into its single root-led row.
  for (const candidate of base) {
    push(index.rootById.get(candidate.id))
  }
  // The selected session may be absent from `filteredSessions` entirely.
  // Add both it and its family root for stable selected-row highlighting.
  if (sel) {
    push(index.byId.get(sel))
    const rootId = familyRootId(sel, index)
    if (rootId) push(index.byId.get(rootId))
  }
  return out
}

let lastSidebarMemo: {
  filtered: readonly Session[]; all: readonly Session[]
  sel: string | null; onlyAlive: boolean; out: Session[]
} | null = null

export const sidebarSessions = computed(() => {
  const sel = selectedId.value
  const onlyAlive = aliveOnly.value
  const all = sessions.value
  const filtered = filteredSessions.value
  const m = lastSidebarMemo
  if (m && m.sel === sel && m.onlyAlive === onlyAlive) {
    // Fast path: same rows with some identities replaced, membership facts
    // (liveness, resumability, family edges) intact, filter membership
    // unchanged (the `filtered` diff proves it). Output is the previous
    // list with the replaced objects substituted.
    const dAll = diffReplacedRows(m.all, all)
    if (dAll && factsUnchanged(dAll, SIDEBAR_FACT_KEYS) && diffReplacedRows(m.filtered, filtered)) {
      const out = substituteRows(m.out, dAll) as Session[]
      lastSidebarMemo = { filtered, all, sel, onlyAlive, out }
      return out
    }
  }
  const out = sidebarSessionsWith(familyIndex(all))
  lastSidebarMemo = { filtered, all, sel, onlyAlive, out }
  return out
})

export const folders = computed(() => sidebarFoldersMemo(sidebarSessions.value))

/** Sidebar selection follows a nested session to its presentation root row. */
export const familySelectedId = computed(() => familyRootId(selectedId.value, sessions.value))

/** Activity-grouped arrangement of the sessions that Projects can
 *  actually place in folders. In particular, recovered sessions may be
 *  briefly unstamped while gmuxd reapplies project ownership after a
 *  restart. `sidebarSessions` already contains them, but Projects cannot
 *  show them until their `project_slug` arrives. Flattening `folders`
 *  keeps both views' membership identical through that reconnect window
 *  (and makes every Activity row's folder lookup total).
 *
 *  Day-groups the full set (alive + dead/resumable, any age), so the
 *  Activity view lists exactly what Projects does. */
export const sidebarActivity = computed(() => {
  // Clock dependency: recompute when the local day moves (see dayBoundary).
  void dayBoundary.value
  return sidebarActivityMemo(folders.value.flatMap(folder => folder.sessions), Date.now())
})

// ── Sidebar view mode ───────────────────────────────────────────────────────
//
// The sidebar list has two presentations, switched from a compact menu
// in the sidebar header:
//
//   'projects' — grouped by project folder, manual order (the classic).
//   'activity' — flat, partitioned by activity/recency exactly like the
//                home dashboard (Waiting / Active / recency buckets).
//
// Persistence is the URL (`?sidebar=activity`; absent = projects), so a
// pinned tab keeps its view across reloads and other tabs can't
// reconfigure it out from under the user.
//
// The URL is a *mirror*, not the source of truth: this signal is. It's
// seeded from `?sidebar` once at boot (deep links work), and thereafter
// the URL follows the signal — `navigate()` stamps the current mode
// into every target URL, and a repair effect in `initStore` rewrites
// (replaceState) any URL that disagrees, e.g. a history entry that
// snapshotted an older mode. Back/Forward therefore never flips the
// sidebar view; only the explicit toggle does.

export type SidebarMode = 'projects' | 'activity'

export const sidebarMode = signal<SidebarMode>(
  typeof location !== 'undefined'
    && new URLSearchParams(location.search).get('sidebar') === 'activity'
    ? 'activity'
    : 'projects',
)

export function setSidebarMode(m: SidebarMode) {
  sidebarMode.value = m
  // Mirror into the URL immediately (replace: a view toggle is not a
  // navigation). Preserve any non-tab params (e.g. ?settings).
  navigate(urlPath.value + urlSearch.value + currentHash(), true)
}

// ── Alive-only toggle ────────────────────────────────────────────────────
//
// Hide dead-but-resumable sessions in the Projects view. Deliberately
// sessionStorage (per tab, dies with it): after a host reboot every
// session becomes resumable, so a durable version of this toggle would
// greet the user with an empty sidebar and no obvious reason why.

const ALIVE_ONLY_KEY = 'gmux.aliveOnly'

export const aliveOnly = signal<boolean>((() => {
  try { return sessionStorage.getItem(ALIVE_ONLY_KEY) === '1' } catch { return false }
})())

export function setAliveOnly(v: boolean) {
  aliveOnly.value = v
  try {
    if (v) sessionStorage.setItem(ALIVE_ONLY_KEY, '1')
    else sessionStorage.removeItem(ALIVE_ONLY_KEY)
  } catch { /* private mode */ }
}

// ── Collapsed folders ─────────────────────────────────────────────────
//
// Which sidebar folders are collapsed (sessions hidden). Keyed by
// `Folder.key` (`${peer ?? ''}::${slug}`). Per-tab sessionStorage, same
// browser-first ethos as the alive-only toggle: collapse is a transient
// view preference, not durable config.

const COLLAPSED_KEY = 'gmux.collapsedFolders'

export const collapsedFolders = signal<ReadonlySet<string>>((() => {
  try {
    const raw = sessionStorage.getItem(COLLAPSED_KEY)
    return new Set<string>(raw ? JSON.parse(raw) as string[] : [])
  } catch { return new Set<string>() }
})())

export function toggleFolderCollapsed(key: string) {
  const next = new Set(collapsedFolders.value)
  if (next.has(key)) next.delete(key)
  else next.add(key)
  collapsedFolders.value = next
  try {
    sessionStorage.setItem(COLLAPSED_KEY, JSON.stringify([...next]))
  } catch { /* private mode */ }
}

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
  // Filter-blind: routing addresses session *identity*, which the tab's
  // `?filter=` must never change. Resolving against `filteredSessions`
  // would let a narrowing filter evict the currently-open terminal back
  // to the hub. Filtering is a sidebar-presentation concern only (see
  // `sidebarSessions`).
  return resolveViewFromPath(urlPath.value, projects.value, sessions.value)
})

/** Currently selected session ID, if the view is a session view. */
export const selectedId = computed(() =>
  view.value?.kind === 'session' ? view.value.sessionId : null,
)

/** Currently selected session object. */
export const selected = computed(() => {
  const id = selectedId.value
  if (!id) return null
  const s = familyIndex(sessions.value).byId.get(id) ?? null
  // Expose on window for debugging.
  ;(window as any).__gmuxSession = s
  return s
})

/** Dot state for the mobile hamburger: summarizes background session activity. */
export type DotState = 'working' | 'active-error' | 'error' | 'unread' | 'active' | 'fading' | 'none'

export const backgroundActivity = computed((): DotState => {
  const sel = selectedId.value
  const am = activityMap.value
  let result: DotState = 'none'
  for (const s of sessions.value) {
    // Preserve the mobile summary's live-session scope. Current presentation
    // is derived canonically rather than reading status/error independently.
    if (s.id === sel || !s.alive) continue
    const dot = sessionDotState(s, am)
    if (AGGREGATE_DOT_RANK[dot] > AGGREGATE_DOT_RANK[result]) result = dot
  }
  return result
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
 * Agent family children have no folder row of their own — their
 * presentation root stands in — so each folder-visible root also counts
 * unread agent descendants, alive or retained-dead. Process unread remains
 * consumable but is task history, not an attention blip.
 *
 * Scoped to the tab's `?filter=` selectors: a tab pinned to a project
 * or host shouldn't blink for sessions outside its scope (another tab
 * or a notification covers those). Within the scope it's built from
 * folder-bucketed sessions so unstamped strays can't ping. */
function unreadCountWith(index: FamilyIndex, folderList: Folder[]): number {
  const sel = selectedId.value
  const childUnread = new Map<string, number>()
  for (const s of sessions.value) {
    // Process output remains unread for explicit consumption, but command
    // completion is not agent attention and must not light the family's row.
    if (s.id === sel || !s.unread || isProcessSession(s) || !index.childIds.has(s.id)) continue
    const rootId = index.rootById.get(s.id)?.id
    if (rootId) childUnread.set(rootId, (childUnread.get(rootId) ?? 0) + 1)
  }
  let n = 0
  for (const f of folderList) {
    for (const s of f.sessions) {
      // A standalone process owns its own visible row, so its unread output
      // still badges normally. Only agent-owned process children disappear
      // behind a family root and are excluded by `childUnread` above.
      if (s.id !== sel && s.unread) n++
      n += childUnread.get(s.id) ?? 0
    }
  }
  return n
}

export const unreadCount = computed(() =>
  unreadCountWith(familyIndex(sessions.value), unreadFoldersMemo(filteredSessions.value)))

/** Aggregate precedence intentionally differs from a session's own semantic
 * derivation only for waiting-error attention: it outranks an active sibling
 * so the failure cannot disappear. Ordinary waiting remains below active,
 * matching the established family aggregation behavior. */
const AGGREGATE_DOT_RANK: Record<DotState, number> = {
  none: 0, fading: 1, active: 2, unread: 3, working: 4, 'active-error': 5, error: 6,
}

/** Family-aggregated dot state, keyed by presentation-root session id.
 *
 * A root's sidebar/dashboard row stands in for its agent family, so the
 * row's dot reflects the highest-precedence agent state among the root and
 * descendants. Processes use the separate running `$` summary; their state
 * never becomes an agent-style aggregate dot.
 *
 * Selection muting happens per member before aggregation: the selected
 * session's waiting/waiting-error attention is dropped ("you're already
 * looking at it"), while active/active-error status and sibling attention
 * still surface on the root row.
 * Standalone sessions are their own root, so this map is the single
 * dot-state source for every root-level row. */
export const familyDotById = computed<ReadonlyMap<string, DotState>>(() => {
  const sel = selectedId.value
  const am = activityMap.value
  const index = familyIndex(sessions.value)
  const map = new Map<string, DotState>()
  for (const s of sessions.value) {
    const rootId = index.rootById.get(s.id)?.id ?? s.id
    // A process child contributes through the separate running `$` summary,
    // never through the agent-style aggregate dot. Standalone processes keep
    // their own row state because no family root stands in for them.
    if (isProcessSession(s) && rootId !== s.id) continue
    let own = sessionDotState(s, am)
    if (s.id === sel && isWaitingPresentation(sessionPresentationState(s))) own = 'none'
    const prev = map.get(rootId)
    if (prev === undefined || AGGREGATE_DOT_RANK[own] > AGGREGATE_DOT_RANK[prev]) map.set(rootId, own)
  }
  return map
})

/** Dot state for a single session's *own* status, with the same
 *  "you're already looking at it" muting `familyDotById` applies per
 *  member. The sidebar's Projects rows use this so a family root's
 *  primary dot always answers "how is the root itself doing?" — the
 *  family's own attention is carried separately by
 *  `familyActivityById` on the family's `+` line. */
export function ownDotState(
  session: Session,
  am: ReadonlyMap<string, 'active' | 'fading'>,
  sel: string | null,
): DotState {
  const own = sessionDotState(session, am)
  return session.id === sel && isWaitingPresentation(sessionPresentationState(session)) ? 'none' : own
}

/** The selected session projected onto its family's sidebar row.
 *
 * Non-null only when the selection is a family *child*: the sidebar row
 * belongs to the root, so the row has to say which member you're
 * actually looking at. `ancestors` is root-first, immediate parent last
 * (empty for a direct child of the root) and lets a deep descendant
 * render its path without a second tree. */
export interface SelectedFamilyChild {
  readonly session: Session
  readonly rootId: string
  readonly ancestors: readonly Session[]
}

export const selectedFamilyChild = computed<SelectedFamilyChild | null>(() => {
  const sel = selectedId.value
  if (!sel) return null
  const index = familyIndex(sessions.value)
  const session = index.byId.get(sel)
  if (!session || !index.childIds.has(sel)) return null
  const rootId = index.rootById.get(sel)?.id
  if (!rootId || rootId === sel) return null
  return { session, rootId, ancestors: familyAncestors(session, index) }
})

/** The one member shown beneath a family root.
 *
 * The slot is a direct projection of current selection: it exists only when
 * a family descendant is selected, and names exactly that descendant. Root
 * selection and selection outside the family produce no member row. */
export interface FamilySlot {
  readonly session: Session
  /** Root-first spine, for the row's hover trail. */
  readonly ancestors: readonly Session[]
}

export const familySlotById = computed<ReadonlyMap<string, FamilySlot>>(() => {
  const map = new Map<string, FamilySlot>()
  const sel = selectedFamilyChild.value
  if (sel) map.set(sel.rootId, { session: sel.session, ancestors: sel.ancestors })
  return map
})

/** What each family is doing, keyed by presentation-root id: the
 * standard family numbers — every descendant of the root, the root
 * excluded, bucketed once each by `familyStateOf` — the same rule and
 * the same population as the header pill and the panel tally, so the
 * same dots never wear different numbers anywhere.
 *
 * Deliberately a fact about the family, not the viewport: the line
 * does not subtract the slot member named beneath it (a summary may
 * include what is separately visible — the panel's tally counts the
 * rows below it too) and does not subtract members the alive-only
 * toggle hides (the panel one click away shows them regardless). Both
 * subtractions used to make this number wobble with unrelated view
 * state, which read as the count being wrong.
 *
 * Idle members are not counted, and an entry exists if and only if
 * some count is non-zero — a quiet family is absent from the map, and
 * `undefined` is the whole of "nothing to report" for every caller. */
export const familyActivityById = computed<ReadonlyMap<string, FamilyActivity>>(() => {
  const index = familyIndex(sessions.value)
  const map = new Map<string, { error: number; waiting: number; active: number; running: number }>()
  for (const s of sessions.value) {
    if (!index.childIds.has(s.id)) continue
    const rootId = index.rootById.get(s.id)?.id
    if (!rootId || rootId === s.id) continue
    const state = familyStateOf(s)
    if (!state) continue
    const entry = map.get(rootId) ?? { error: 0, waiting: 0, active: 0, running: 0 }
    entry[state]++
    map.set(rootId, entry)
  }
  return map
})

// ── Home dashboard partitioning ─────────────────────────────────────────────
//
// The activity feed groups sessions by the calendar day of their last
// output (last_output_at, falling back to created_at). It's a plain
// recency feed — no status buckets. Status lives on the per-row dot;
// pulling "waiting"/"active" sessions to a pinned top would reshuffle
// the list every time you act on one. A session floats up only when it
// produces new unseen output, so the queue stays stable as you work
// down it.
//
// Two surfaces share the grouping, differing only in their input:
//   - The sidebar's Activity view feeds it every session (alive + dead/
//     resumable), so it lists exactly what the Projects view does.
//   - The home dashboard feeds it only alive sessions and renders just
//     the named-day window (Today … Last <weekday>), leaving the dated
//     tail to the sidebar — home stays a recent-activity dashboard.
const MS_PER_DAY = 24 * 60 * 60 * 1000

export type DayBucketKind = 'today' | 'named' | 'dated'

/** One day-grouped section of the activity feed. `label` is null for
 *  today (rendered with no heading). `kind` lets the home dashboard
 *  drop the `dated` tail while the sidebar keeps it. */
export interface DayBucket {
  label: string | null
  kind: DayBucketKind
  sessions: Session[]
}

/** Local calendar midnight (ms) for a timestamp. */
function localMidnight(t: number): number {
  const d = new Date(t)
  return new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime()
}

// Per-row memo: Date.parse dominated the partition sorts (O(N log N) parses
// per recompute). Structural sharing keeps row identities stable across
// snapshots, so the WeakMap hits for every unchanged row — and a replaced
// row is a new object, so a moved timestamp can never read a stale entry.
const outputTimeCache = new WeakMap<Session, number>()

function outputTimeMs(s: Session): number {
  const hit = outputTimeCache.get(s)
  if (hit !== undefined) return hit
  // last_output_at is canonical when present (daemon-stamped when the
  // session last produced unseen output). For sessions that never went
  // unread, fall back to created_at so they sort relative to peers
  // rather than landing at the epoch.
  const stamp = s.last_output_at ?? s.created_at
  const t = Date.parse(stamp)
  const v = Number.isFinite(t) ? t : 0
  outputTimeCache.set(s, v)
  return v
}

function byActivityDesc(a: Session, b: Session): number {
  const dt = outputTimeMs(b) - outputTimeMs(a)
  if (dt !== 0) return dt
  // Stable tiebreaker on id. Sessions with identical timestamps
  // (notably corpses persisted before last_output_at existed,
  // which all fall back to created_at) must sort identically
  // across re-renders. Without this, every SSE event rebuilds
  // sessions.value in a different order and the section visibly
  // re-orders the tied entries.
  return a.id < b.id ? -1 : a.id > b.id ? 1 : 0
}

/**
 * Group sessions into an ordered, newest-first list of day buckets:
 *   - Today            (unlabeled)
 *   - Yesterday
 *   - Last <weekday>   for 2–7 days ago. The `Last ` prefix keeps
 *     day-7 distinct from today's own weekday, so we name a full week
 *     before falling back to dates.
 *   - <short date>     for 8+ days ago — localized via Intl, with the
 *     year shown only when it differs from the current one.
 * Sessions sort newest-first within each bucket. Total: every input
 * session lands in exactly one bucket, so the sidebar can rely on it
 * for full Projects/Activity membership parity. `now` is injected so
 * tests can pin the day boundaries.
 */
export function partitionByDay(all: readonly Session[], now: number): DayBucket[] {
  const nd = new Date(now)
  const y = nd.getFullYear()
  const m = nd.getMonth()
  const d = nd.getDate()
  const todayMid = new Date(y, m, d).getTime()
  // Round, not floor: a calendar day is 24h ±1h across DST, so rounding
  // the ms delta keeps the day count correct through the transition.
  const daysAgo = (t: number) => Math.round((todayMid - localMidnight(t)) / MS_PER_DAY)

  // Bucket key: days-ago (0–7) for the named window, or the day's
  // midnight for the dated tail (each old day its own bucket). A real
  // timestamp's midnight is always ≫ 7, so the two key spaces never
  // collide. An unparseable stamp (outputTimeMs === 0 sentinel) is the
  // exception: localMidnight(0) is 0 in UTC but negative east of it,
  // which would miss both the 0–7 window and the >7 dated tail —
  // silently dropping the session and breaking Projects/Activity parity.
  // Pin those to Today (byActivityDesc still sorts them last within it).
  const byKey = new Map<number, Session[]>()
  for (const s of all) {
    const t = outputTimeMs(s)
    const ago = t > 0 ? daysAgo(t) : 0
    const key = ago <= 7 ? Math.max(ago, 0) : localMidnight(t)
    const list = byKey.get(key)
    if (list) list.push(s)
    else byKey.set(key, [s])
  }

  const weekday = new Intl.DateTimeFormat(undefined, { weekday: 'long' })
  const labelFor = (key: number): { label: string | null; kind: DayBucketKind } => {
    if (key === 0) return { label: null, kind: 'today' }
    if (key === 1) return { label: 'Yesterday', kind: 'named' }
    if (key <= 7) return { label: `Last ${weekday.format(new Date(y, m, d - key))}`, kind: 'named' }
    const day = new Date(key)
    const opts: Intl.DateTimeFormatOptions = { month: 'short', day: 'numeric' }
    if (day.getFullYear() !== y) opts.year = 'numeric'
    return { label: new Intl.DateTimeFormat(undefined, opts).format(day), kind: 'dated' }
  }

  const out: DayBucket[] = []
  const add = (key: number) => {
    const sessions = byKey.get(key)
    if (!sessions) return
    sessions.sort(byActivityDesc)
    out.push({ ...labelFor(key), sessions })
  }
  // Named window first, in order: Today, Yesterday, Last <weekday>.
  for (let k = 0; k <= 7; k++) add(k)
  // Then the dated tail, newest day → oldest.
  for (const k of [...byKey.keys()].filter(k => k > 7).sort((a, b) => b - a)) add(k)
  return out
}

export const homePartition = computed(() => {
  // Clock dependency: recompute when the local day moves (see dayBoundary).
  void dayBoundary.value
  // Home is its own curated surface: scoped to the tab's `?filter=` but
  // independent of the sidebar's list rules (alive-only, selected-pin).
  // Alive only — dead/resumable corpses live in the sidebar's Activity
  // view, not the dashboard. Home renders only the non-`dated` buckets
  // (see home.tsx), keeping it a recent-activity view.
  const index = familyIndex(sessions.value)
  return homePartitionMemo(
    filteredSessions.value.filter(s => s.alive && !index.childIds.has(s.id)),
    Date.now(),
  )
})

// ── Mutators ────────────────────────────────────────────────────────────────

export function toUISession(s: ProtocolSession): Session {
  return {
    id: s.id,
    created_at: s.created_at ?? new Date().toISOString(),
    command: s.command ?? [],
    cwd: s.cwd ?? '',
    workspace_root: s.workspace_root ?? undefined,
    remotes: s.remotes ?? undefined,
    adapter: s.adapter ?? 'shell',
    // Preserve launch provenance and server-owned family semantics. These were
    // previously dropped by this protocol → UI mapper.
    parent_session_id: s.parent_session_id,
    launched_from_session_id: s.launched_from_session_id,
    semantic_agent: s.semantic_agent,
    alive: s.alive,
    pid: s.pid ?? null,
    exit_code: s.exit_code ?? null,
    started_at: s.started_at ?? s.created_at ?? new Date().toISOString(),
    exited_at: s.exited_at ?? null,
    title: s.title ?? s.command?.[0] ?? 'session',
    subtitle: s.subtitle ?? '',
    // The shared wire preserves durable active-at-death for gmux wait. At the
    // local+peer UI funnel, project it to current activity and defend against
    // older/version-skewed peers that can send dead+active snapshots.
    status: s.status ? { ...s.status, active: Boolean(s.alive && s.status.active) } : null,
    unread: s.unread ?? false,
    unread_token: s.unread_token ?? '',
    resumable: s.resumable ?? false,
    conversation_file: s.conversation_file ?? undefined,
    last_output_at: s.last_output_at ?? undefined,
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

/**
 * When the currently-selected session's slug changes between two session
 * arrays, compute the canonical URL for the new slug. Callers rewrite the
 * address bar in place with this URL so the stale slug doesn't fail to
 * resolve and boot the user back home.
 *
 * The slug is title-derived (#348/#360), so it changes on rename *and*
 * when a pi `/resume` swaps the active conversation. Both cases keep the
 * same underlying gmux session id; only the slug moves. Resolving the
 * post-update session list here means the returned URL sees the new slug.
 *
 * `selectedId` is read before the caller commits the new array, so it
 * still resolves against the current (pre-change) slug in the URL.
 *
 * Returns null when nothing needs rewriting.
 */
function selectedSlugRewrite(prev: Session[], next: Session[]): string | null {
  const id = selectedId.value
  if (!id) return null
  const old = prev.find(s => s.id === id)
  const cur = next.find(s => s.id === id)
  if (!old || !cur) return null
  // Recompute canonical serialization even when this row's slug did not
  // change: arrival of a duplicate can make the old slug route ambiguous,
  // in which case viewToPath switches the selected row to its full ID.
  const canonical = viewToPath({ kind: 'session', sessionId: id }, projects.value, next)
  return canonical && canonical !== urlPath.value ? canonical : null
}

/**
 * Rewrite the address bar in place when the selected session's slug moved.
 * Sets `urlPath` inside the same batch as the session commit so the `view`
 * computed never observes the new slug against the old URL (which would
 * briefly fail to resolve and deselect the session). Returns true when a
 * rewrite happened, meaning the caller has already committed `nextSessions`
 * to `_rawSessions` inside the batch.
 */
function commitWithSlugRewrite(
  nextSessions: Session[],
  extra?: () => void,
): boolean {
  const newUrl = selectedSlugRewrite(_rawSessions.value, nextSessions)
  if (!newUrl) return false
  batch(() => {
    _rawSessions.value = nextSessions
    urlPath.value = newUrl
    extra?.()
  })
  // Sync the browser URL bar. navigate(replace) uses history.replaceState
  // via preact-iso, so back/forward history isn't polluted. urlPath was
  // already set above inside the batch for atomicity with the session data.
  navigate(newUrl + currentHash(), true)
  return true
}

/**
 * Apply one ready wholesale session replacement (ADR 0001 protocol 3):
 * the daemon re-sends the *entire* session list on any session state
 * change as bounded batches, and the ready handler replaces `_rawSessions`
 * in one shot rather than exposing batches or patching individual entries.
 *
 * Because the replacement is wholesale, a slug change on the currently
 * viewed session (a rename, or a pi `/resume` that swaps the active
 * conversation — #348/#360) arrives here, NOT via `upsertSession`. This is
 * the seam that silently regressed when #191 replaced per-session
 * `session-upsert` events (which ran `upsertSession`, and with it the
 * slug→URL rewrite) with snapshots: the rewrite logic was left in
 * `upsertSession`, which the live path no longer calls. Routing the
 * commit through `commitWithSlugRewrite` reconnects it, so the URL follows
 * the new slug in place instead of the stale slug failing to resolve and
 * booting the user back home.
 */
const SESSION_STREAM_VERSION = 3
const MAX_STAGED_SESSION_ROWS = 100_000
const MAX_STAGED_SESSION_BYTES = 64 * 1024 * 1024

type SessionsBootstrap = {
  epoch: number
  rows: ProtocolSession[]
  bytes: number
  warnings: SessionStreamWarning[]
  omittedTotal: number
}

type SessionStreamMode = 'unknown' | 'legacy' | 'v3'
let sessionStreamMode: SessionStreamMode = 'unknown'
let sessionsBootstrap: SessionsBootstrap | null = null
let lastSessionEpoch = 0

/** Protocol-3 bootstrap helpers are exported for deterministic reconnect and
 * atomic-visibility tests. Production calls them only from EventSource. */
export function beginSessionsBootstrap(version: number, epoch: number): void {
  if (version !== SESSION_STREAM_VERSION || !Number.isSafeInteger(epoch) || epoch <= 0) {
    sessionsBootstrap = null
    return
  }
  if (sessionStreamMode === 'legacy') return
  // Epochs are strictly increasing within one EventSource transport. Ignore
  // replay without destroying a newer transaction already in flight.
  if (epoch <= lastSessionEpoch) return
  sessionStreamMode = 'v3'
  lastSessionEpoch = epoch
  sessionsBootstrap = { epoch, rows: [], bytes: 0, warnings: [], omittedTotal: 0 }
}

export function appendSessionsBootstrap(epoch: number, rows: ProtocolSession[], encodedBytes = 0): void {
  if (!sessionsBootstrap || sessionsBootstrap.epoch !== epoch) return
  if (sessionsBootstrap.rows.length + rows.length > MAX_STAGED_SESSION_ROWS
      || sessionsBootstrap.bytes + encodedBytes > MAX_STAGED_SESSION_BYTES) {
    sessionsBootstrap = null
    return
  }
  sessionsBootstrap.rows.push(...rows)
  sessionsBootstrap.bytes += encodedBytes
}

export function appendSessionStreamWarning(epoch: number, warning: SessionStreamWarning): void {
  if (sessionStreamMode !== 'v3' || !sessionsBootstrap || sessionsBootstrap.epoch !== epoch) return
  const count = Number.isSafeInteger(warning.count) && warning.count > 0 ? warning.count : 1
  sessionsBootstrap.omittedTotal = Math.min(Number.MAX_SAFE_INTEGER, sessionsBootstrap.omittedTotal + count)
  // Keep safe per-row detail bounded. The sender's final counted summary has
  // no ID and updates omittedTotal even after this list reaches its cap.
  if (warning.id && sessionsBootstrap.warnings.length < 256) {
    sessionsBootstrap.warnings.push({ ...warning, count })
  }
}

export function readySessionsBootstrap(epoch: number): boolean {
  if (!sessionsBootstrap || sessionsBootstrap.epoch !== epoch) return false
  const { rows, warnings, omittedTotal } = sessionsBootstrap
  sessionsBootstrap = null
  applySessionsSnapshot(rows.map(toUISession))
  sessionStreamWarnings.value = warnings
  sessionStreamOmittedTotal.value = omittedTotal
  return true
}

/**
 * Non-incremental reference projections for the differential tests: the same
 * pure building blocks the computeds use, but with a freshly built family
 * index and no memo fast paths. Output values (not identities) from the
 * production computeds must always deep-equal these.
 */
export function _uncachedProjections(now = Date.now()): {
  sidebarSessions: Session[]
  folders: Folder[]
  unreadCount: number
  sidebarActivity: DayBucket[]
  homePartition: DayBucket[]
} {
  const index = createFamilyIndex(sessions.value)
  const sidebar = sidebarSessionsWith(index)
  const folderList = foldersWith(sidebar, index)
  return {
    sidebarSessions: sidebar,
    folders: folderList,
    unreadCount: unreadCountWith(index, foldersWith(filteredSessions.value, index)),
    sidebarActivity: partitionByDay(folderList.flatMap(f => f.sessions), now),
    homePartition: partitionByDay(
      filteredSessions.value.filter(s => s.alive && !index.childIds.has(s.id)),
      now,
    ),
  }
}

export function discardSessionsBootstrap(): void {
  sessionsBootstrap = null
}

export function resetSessionsTransport(): void {
  sessionStreamMode = 'unknown'
  sessionsBootstrap = null
  lastSessionEpoch = 0
}

export function applySessionsSnapshot(list: Session[]): void {
  // Structural sharing (reconcile.ts): rows are re-encoded server-side every
  // snapshot, so reuse the previous row *objects* wherever the incoming row
  // is deep-equal — and the previous array identity when nothing changed at
  // all. Semantics are still wholesale replacement (the reconciled array is
  // deep-equal to `list`); this only lets projections use identity as a
  // provably-unchanged test and patch O(changed) instead of rebuilding O(N).
  list = reconcileSessions(_rawSessions.peek(), list)
  // Even a fully-reused (identity no-op) snapshot must refresh day-relative
  // partitions when local midnight has passed since the last commit.
  refreshDayBoundary()
  // Detect newly-arrived IDs vs the previous snapshot so a pending launch
  // (just-POSTed /v1/launch awaiting an id) can navigate to its session as
  // soon as the daemon publishes it. Computed before we commit the new
  // array so the launch navigation runs against the new state.
  const prevIds = new Set(_rawSessions.value.map(s => s.id))
  const newIds = list.filter(s => !prevIds.has(s.id)).map(s => s.id)

  const rewritten = commitWithSlugRewrite(list, () => {
    sessionsLoaded.value = true
    connState.value = 'connected'
    reconcilePromotionPending(list)
  })
  if (!rewritten) {
    batch(() => {
      _rawSessions.value = list
      sessionsLoaded.value = true
      connState.value = 'connected'
      reconcilePromotionPending(list)
    })
  }

  if (newIds.length > 0 && consumePendingLaunch()) {
    // Most recent new id wins; with one launch in flight at a time this
    // is unambiguous.
    navigateToSession(newIds[newIds.length - 1], true)
  }
}

/** Upsert a session from SSE. Returns true if the session was new. */
export function upsertSession(raw: ProtocolSession): boolean {
  const updated = toUISession(raw)
  let isNew = false
  const prev = _rawSessions.value
  const idx = prev.findIndex(s => s.id === updated.id)
  if (idx >= 0) {
    const next = [...prev]
    next[idx] = updated
    if (commitWithSlugRewrite(next)) {
      reconcilePromotionPending(next)
      return isNew
    }
    _rawSessions.value = next
    reconcilePromotionPending(next)
  } else {
    isNew = true
    const next = [...prev, updated]
    _rawSessions.value = next
    reconcilePromotionPending(next)
  }
  return isNew
}

export function removeSession(id: string) {
  const next = _rawSessions.value.filter(s => s.id !== id)
  _rawSessions.value = next
  reconcilePromotionPending(next)
}

export function markSessionRead(id: string, observedToken?: string) {
  const raw = _rawSessions.peek().find(s => s.id === id)
  const token = observedToken ?? raw?.unread_token ?? ''
  // Optimism is token-bound too: a newer completion must immediately escape
  // an older pending overlay even before the delayed /read returns.
  addPending({ kind: 'mark-read', id, token, at: Date.now() })
  fetch(`/v1/sessions/${id}/read?token=${encodeURIComponent(token)}`, { method: 'POST' }).catch(() => {/* fire-and-forget; TTL handles failures */})
}

type ReadObservation = { id: string; token: string }

export function createViewConsumptionTracker() {
  let establishedSelection: string | null = null
  const unreadObservation = (id: string | null, sess: Session | null): ReadObservation | null =>
    id && sess?.unread
      ? { id, token: sess.unread_token ?? '' }
      : null
  return {
    selection(id: string | null, sess: Session | null): ReadObservation | null {
      if (!id || !sess) {
        establishedSelection = null
        return null
      }
      if (id === establishedSelection) return null
      establishedSelection = id
      return unreadObservation(id, sess)
    },
    interaction(id: string | null, sess: Session | null): ReadObservation | null {
      return unreadObservation(id, sess)
    },
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
    if (!resp.ok) {
      pushError(`Save projects failed: ${await errorMessageFromResponse(resp)}`)
    }
  } catch {
    // Network reject = connectivity, not a semantic failure. The
    // reconnecting pill owns "we're offline"; don't add a toast flood
    // on top of it. (See postAction for the full rationale.)
    console.debug('PUT /v1/projects: network error (suppressed toast)')
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
 * contains session IDs in the desired display order.
 *
 * Fire-and-report, with no optimistic overlay for either ownership case:
 * the sidebar derives folder order solely from server-stamped
 * project_index, so there is no local array an overlay could usefully
 * pre-write. The owning daemon re-stamps and re-emits snapshot.sessions,
 * which lands in the same tick as the response for a local write; the
 * round-trip is the cost of honesty about who owns the data.
 *
 * Peer-owned projects route through the generic peer-write proxy at
 * `/v1/peers/{peer}/v1/projects/{slug}/sessions` (ADR 0002): the peer
 * owns its own catalog and its own stamps.
 *
 * A rejection toasts (the drag then visibly snaps back to the
 * server-stamped order, which is the truth); a network reject stays
 * silent — connectivity is the reconnecting pill's story to tell.
 */
export async function reorderSessions(
  projectSlug: string,
  sessionKeys: string[],
  peer?: string,
): Promise<void> {
  const url = peer
    ? `/v1/peers/${encodeURIComponent(peer)}/v1/projects/${encodeURIComponent(projectSlug)}/sessions`
    : `/v1/projects/${encodeURIComponent(projectSlug)}/sessions`

  // Same reporting contract as postAction: a received error toasts, a
  // network reject only logs.
  try {
    const resp = await fetch(url, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ sessions: sessionKeys }),
    })
    if (!resp.ok) {
      pushError(`Reorder failed: ${await errorMessageFromResponse(resp)}`)
    }
  } catch {
    console.debug(`${url}: network error (suppressed toast)`)
  }
}

// ── Session actions ─────────────────────────────────────────────────────────

/**
 * Pull a human message out of a failed response. The daemon's
 * `writeError` (services/gmuxd) returns
 * `{ ok:false, error:{ code, message } }`, so prefer `error.message`;
 * fall back to a raw text body, then the HTTP status line, so even an
 * unexpected non-JSON 500 still produces something legible.
 */
async function errorMessageFromResponse(resp: Response): Promise<string> {
  const raw = await resp.text().catch(() => '')
  if (raw) {
    try {
      const body = JSON.parse(raw) as { error?: { message?: string } }
      if (body?.error?.message) return body.error.message
    } catch { /* not the structured shape; fall through to raw text */ }
    const trimmed = raw.trim()
    if (trimmed) return trimmed
  }
  return resp.statusText || `HTTP ${resp.status}`
}

/**
 * Single seam for session actions (kill/dismiss/resume/restart).
 *
 * Failures are classified by whether the server *answered*:
 *  - `!resp.ok` (we got an HTTP response carrying a structured error) is
 *    a semantic failure — the server was reachable and is telling us why
 *    — so it always surfaces a labelled toast ("Resume failed: …").
 *  - a `fetch` *reject* (no response: offline / connection dropped) is a
 *    connectivity symptom, which the reconnecting pill already owns. We
 *    deliberately do NOT toast it; otherwise a connection drop produces a
 *    toast flood duplicating the pill, exactly when it's least wanted.
 *
 * Returns whether the action succeeded, so optimistic callers can retract
 * on failure. A network reject counts as "not succeeded" for rollback,
 * even though it's silent toast-wise.
 */
async function postAction(endpoint: string, label = 'Action', opts: {
  /** Suppress the per-call toast; the caller reports once instead. */
  quiet?: boolean
  /** One more status to treat as success, for endpoints where a
   * rejection is the outcome the caller asked for anyway. */
  alsoOk?: number
  body?: unknown
} = {}): Promise<boolean> {
  const { quiet = false, alsoOk, body } = opts
  try {
    const resp = await fetch(endpoint, {
      method: 'POST',
      ...(body === undefined ? {} : {
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      }),
    })
    if (!resp.ok && resp.status !== alsoOk) {
      // `quiet` is for bulk callers: one toast per failed member turns a
      // daemon hiccup during "Stop all" into two hundred toasts, so they
      // take the boolean and report once.
      if (!quiet) pushError(`${label} failed: ${await errorMessageFromResponse(resp)}`)
      return false
    }
    return true
  } catch {
    console.debug(`${endpoint}: network error (suppressed toast)`)
    return false
  }
}

// Kill/resume/restart return postAction's success boolean so callers can
// sync UI state (e.g. clear a "resuming…" spinner) the moment a rejection
// toast fires, instead of waiting out a timeout. Note these promises never
// reject — postAction converts all failures into `false` — so `.catch()`
// on them is dead code; branch on the boolean instead.
export function killSession(sessionId: string, opts?: { quiet?: boolean }): Promise<boolean> {
  return postAction(`/v1/sessions/${sessionId}/kill`, 'Kill', { quiet: opts?.quiet })
}

/** Interrupt an agent's current turn (POST /cancel — the daemon's word;
 * the UI says "interrupt", ADR 0023's word for ending a turn early).
 *
 * A 409 is swallowed rather than toasted: cancel is live-and-active
 * only, enforced by the runner at delivery, so "no turn to cancel"
 * means the agent finished between your click and the wire. For the
 * bulk caller iterating a family that race is routine, and the outcome
 * is the one the click asked for — the agent is no longer mid-turn. */
export function cancelSession(sessionId: string, opts?: { quiet?: boolean }): Promise<boolean> {
  return postAction(`/v1/sessions/${sessionId}/cancel`, 'Interrupt', {
    quiet: opts?.quiet, alsoOk: 409,
  })
}

export function dismissSession(sessionId: string): Promise<void> {
  // Optimistic dismissal: hide the session locally now, and retract
  // immediately if the server rejects — so the toast and the row
  // reappearing are coincident, not lagged by the pending TTL.
  return optimistic(
    { kind: 'dismiss', id: sessionId, at: Date.now() },
    () => postAction(`/v1/sessions/${sessionId}/dismiss`, 'Dismiss'),
  )
}

export function resumeSession(sessionId: string): Promise<boolean> {
  return postAction(`/v1/sessions/${sessionId}/resume`, 'Resume')
}

export function restartSession(sessionId: string): Promise<boolean> {
  return postAction(`/v1/sessions/${sessionId}/restart`, 'Restart')
}

// Family mutations change the single parent edge. Deliberately no optimistic
// session overlay: projection and URL derive from the family root, and
// commitWithSlugRewrite applies both atomically when the snapshot arrives.
export function reparentSession(sessionId: string, parentSessionId: string | null, label = 'Reparent'): Promise<boolean> {
  return postAction(`/v1/sessions/${sessionId}/reparent`, label, {
    body: { parent_session_id: parentSessionId },
  })
}

export function promoteSession(sessionId: string): Promise<boolean> {
  return reparentSession(sessionId, null, 'Promote')
}

// In-flight promote/demote requests, keyed by session id. Module scope, not
// menu component state: reconciliation must run for every authoritative
// snapshot even while this session's menu is unmounted. Each request carries
// its action, target flag, and generation so stale A cannot settle B.
export type PromotionPendingEntry = {
  kind: 'promote' | 'demote'
  /** The authoritative current-parent value this request is waiting to observe. */
  targetParent: string | null
  seq: number
}

export type PromotionAnnouncement = {
  kind: PromotionPendingEntry['kind']
  seq: number
  message: string
}

export const promotionPending = signal<ReadonlyMap<string, PromotionPendingEntry>>(new Map())
export const promotionAnnouncements = signal<ReadonlyMap<string, PromotionAnnouncement>>(new Map())
const deliveredPromotionAnnouncements = new Set<string>()
let _promotionSeq = 0

const promotionAnnouncementKey = (id: string, seq: number): string => `${id}:${seq}`
const promotionTimers = new Map<string, ReturnType<typeof setTimeout>>()
/** Final safety valve only: a hung request cannot wedge the menu forever. */
export const PROMOTION_PENDING_TTL_MS = 30_000

function clearPromotionTimer(id: string): void {
  const timer = promotionTimers.get(id)
  if (!timer) return
  clearTimeout(timer)
  promotionTimers.delete(id)
}

export function beginPromotion(id: string, kind: 'promote' | 'demote', targetParent: string | null): number {
  clearPromotionTimer(id)
  const seq = ++_promotionSeq
  const next = new Map(promotionPending.value)
  next.set(id, { kind, targetParent, seq })
  promotionPending.value = next
  if (promotionAnnouncements.value.has(id)) {
    const announcements = new Map(promotionAnnouncements.value)
    const old = announcements.get(id)
    announcements.delete(id)
    if (old) deliveredPromotionAnnouncements.delete(promotionAnnouncementKey(id, old.seq))
    promotionAnnouncements.value = announcements
  }
  const timer = setTimeout(() => {
    const entry = promotionPending.peek().get(id)
    if (entry?.seq === seq) settlePromotion(id, seq)
  }, PROMOTION_PENDING_TTL_MS)
  promotionTimers.set(id, timer)
  return seq
}

/** Drop the entry, but only if it is still the one this request created. */
export function settlePromotion(id: string, seq: number): void {
  const entry = promotionPending.value.get(id)
  if (!entry || entry.seq !== seq) return
  clearPromotionTimer(id)
  const next = new Map(promotionPending.value)
  next.delete(id)
  promotionPending.value = next
}

/** Called at the store boundary for every full/individual authoritative
 * session update. It consumes target transitions for *all* sessions, not just
 * the selected mounted menu. A target can be observed while away and then
 * reversed externally without leaving a stale guard behind. */
export function reconcilePromotionPending(nextSessions: readonly Session[]): void {
  const pending = promotionPending.peek()
  if (pending.size === 0) return
  const byId = new Map(nextSessions.map(session => [session.id, session]))
  const nextPending = new Map(pending)
  const nextAnnouncements = new Map(promotionAnnouncements.peek())
  let pendingChanged = false
  let announcementsChanged = false

  for (const [id, entry] of pending) {
    const session = byId.get(id)
    if (!session) {
      nextPending.delete(id)
      clearPromotionTimer(id)
      const old = nextAnnouncements.get(id)
      nextAnnouncements.delete(id)
      if (old) deliveredPromotionAnnouncements.delete(promotionAnnouncementKey(id, old.seq))
      announcementsChanged = true
      pendingChanged = true
      continue
    }

    if ((session.parent_session_id ?? null) === entry.targetParent) {
      nextPending.delete(id)
      clearPromotionTimer(id)
      nextAnnouncements.set(id, {
        kind: entry.kind,
        seq: entry.seq,
        message: entry.kind === 'promote' ? 'Promoted to root.' : 'Returned to family.',
      })
      pendingChanged = true
      announcementsChanged = true
      continue
    }

    // A parent deletion/reparent/peer transition can make the requested
    // action terminal without reaching its target. Re-arm is not honest here,
    // and no success announcement is emitted; the ordinary snapshot is truth.
    const action = promotionAction(session, nextSessions, projects.value)
    if (!action || action.kind !== entry.kind) {
      nextPending.delete(id)
      clearPromotionTimer(id)
      pendingChanged = true
    }
  }

  if (pendingChanged) promotionPending.value = nextPending
  if (announcementsChanged) promotionAnnouncements.value = nextAnnouncements
}

/** Mark one announcement delivered after the mounted status region has
 * copied it into local rendered state. The central text remains available
 * through a route/remount race, while this token prevents duplicate speech
 * after revisiting the session. A later request replaces the token. */
export function acknowledgePromotionAnnouncement(id: string, seq: number): void {
  const announcement = promotionAnnouncements.value.get(id)
  if (!announcement || announcement.seq !== seq) return
  deliveredPromotionAnnouncements.add(promotionAnnouncementKey(id, seq))
}

export function isPromotionAnnouncementDelivered(id: string, seq: number): boolean {
  return deliveredPromotionAnnouncements.has(promotionAnnouncementKey(id, seq))
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
    if (!resp.ok) {
      pushError(`Launch failed: ${await errorMessageFromResponse(resp)}`)
    }
  } catch {
    console.debug('/v1/launch: network error (suppressed toast)')
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

/** Stamp the tab-identity params onto a target URL. The contract for
 *  all programmatic navigation (`navigate`):
 *
 *   - `?filter=` is carried from the current URL by default; callers
 *     change it only via `opts.filter` (string = set, null = clear) —
 *     never by hand-building a query string.
 *   - `?sidebar=` always mirrors the `sidebarMode` signal (the source
 *     of truth), so no navigation can capture a stale mode.
 *   - Recognized mock boot context (`?mock`, `?host`) is also carried,
 *     so an SPA route remains reloadable in the same mock environment.
 *   - Any other params already on the target URL pass through.
 *
 *  This is why canonicalization rewrites (rename, slug collision,
 *  resume) can navigate with a bare path and still keep the tab's
 *  narrowing — carrying is the default, not a per-call-site chore. */
function withCarriedParams(url: string, opts?: { filter?: string | null }): string {
  // Detach a hash fragment first: URLSearchParams would otherwise
  // swallow it into the last param's value.
  const hi = url.indexOf('#')
  const hash = hi >= 0 ? url.slice(hi) : ''
  const base = hi >= 0 ? url.slice(0, hi) : url
  const qi = base.indexOf('?')
  const path = qi >= 0 ? base.slice(0, qi) : base
  const params = new URLSearchParams(qi >= 0 ? base.slice(qi) : '')
  const current = new URLSearchParams(urlSearch.value)
  const filter = opts?.filter !== undefined
    ? opts.filter
    : current.get('filter')
  if (filter) params.set('filter', filter)
  else params.delete('filter')
  if (sidebarMode.value === 'activity') params.set('sidebar', 'activity')
  else params.delete('sidebar')
  // Mock mode is selected only at document boot. Keep its recognized context
  // on route changes so a later reload does not silently leave mock mode.
  for (const key of ['mock', 'host']) {
    if (!params.has(key)) {
      for (const value of current.getAll(key)) params.append(key, value)
    }
  }
  const qs = params.toString()
  return (qs ? `${path}?${qs}` : path) + hash
}

export function navigate(url: string, replace?: boolean, opts?: { filter?: string | null }) {
  const full = withCarriedParams(url, opts)
  // When the bundle has drifted from the daemon, the version watcher
  // converts the next in-app navigation into a full document load.
  // The user perceives it as a normal route transition that happens
  // to also pick up the new bundle. (The store ↔ version-watch
  // module cycle is safe: neither side touches the other at top
  // level.)
  if (navigateWithReload(full, replace)) return
  _navigate?.(full, replace)
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
  // navigate() carries the tab-identity params (?filter=, ?sidebar=),
  // so programmatic navigation doesn't un-pin a narrowed tab.
  navigate(path, replace)
  return true
}

/**
 * Start the store: connect SSE, fetch initial data, start timers.
 * Call once from the app root.
 */
export function initStore(): () => void {
  const cleanups: (() => void)[] = []

  // Sidebar-mode repair: the URL mirrors the `sidebarMode` signal, but
  // history entries snapshot the query string at push time, so Back can
  // land on an entry stamped with an older mode (or a pre-signal-era
  // bookmark). The signal is authoritative after boot: rewrite any URL
  // that disagrees, in place (replaceState — no history pollution, and
  // the forward stack survives). Converges in one step: the rewritten
  // URL matches the signal, so the effect re-runs once and does nothing.
  const disposeSidebarRepair = effect(() => {
    const path = urlPath.value
    const search = urlSearch.value
    const hash = urlHash.value
    const params = new URLSearchParams(search)
    const sidebarValues = params.getAll('sidebar')
    const mode = sidebarMode.peek()
    const canonical = mode === 'activity'
      ? sidebarValues.length === 1 && sidebarValues[0] === 'activity'
      : sidebarValues.length === 0
    if (!canonical) {
      // The setter owns signal-driven mirroring. This effect tracks URL
      // changes only, avoiding a duplicate soft (or hard) replacement when
      // the user toggles modes; untracked keeps navigate's signal reads from
      // becoming dependencies during a repair run.
      untracked(() => navigate(path + search + currentHash(hash), true))
    }
  })
  cleanups.push(disposeSidebarRepair)

  if (USE_MOCK) {
    const localHost = new URLSearchParams(location.search).get('host')
    const mockSessions = localHost
      ? MOCK_SESSIONS.map(s => s.peer === localHost ? { ...s, peer: undefined } : s)
      : [...MOCK_SESSIONS]
    batch(() => {
      _setRawWorld(mockWorld(location.search))
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
  resetSessionsTransport()
  sessionStreamWarnings.value = []
  sessionStreamOmittedTotal.value = 0
  const source = new EventSource('/v1/events?session_stream=3')
  source.addEventListener('error', () => {
    // A transport reconnect starts a new epoch. Never retain unpublished
    // rows from the interrupted response.
    resetSessionsTransport()
    // Browser EventSource auto-reconnects; flag the UI as degraded
    // until the next snapshot arrives. `sessionsLoaded` stays true
    // once it has flipped, so reconnect doesn't blank the sidebar.
    //
    // A drop on the *initial* connect is a hard failure (full-screen +
    // Retry). A drop on an *established* stream is transient: show a
    // subtle reconnecting pill and keep the last snapshot on screen
    // while EventSource retries. The next snapshot flips it back to
    // 'connected'.
    if (connState.value === 'connecting') connState.value = 'error'
    else if (connState.value === 'connected') connState.value = 'reconnecting'
  })

  // Protocol 3 streams complete semantic rows into private staging. No row
  // becomes visible until the matching ready marker atomically replaces the
  // projection. A later epoch is an ordinary full replacement too.
  source.addEventListener('snapshot.sessions.begin', (e) => {
    try {
      const { version, epoch } = JSON.parse(e.data) as { version: number, epoch: number }
      beginSessionsBootstrap(version, epoch)
    } catch (err) {
      discardSessionsBootstrap()
      console.warn('snapshot.sessions.begin: bad event', err)
    }
  })

  source.addEventListener('snapshot.sessions.batch', (e) => {
    try {
      const { epoch, sessions: rows } = JSON.parse(e.data) as { epoch: number, sessions: ProtocolSession[] }
      if (!Array.isArray(rows)) throw new Error('sessions must be an array')
      appendSessionsBootstrap(epoch, rows, e.data.length)
    } catch (err) {
      discardSessionsBootstrap()
      console.warn('snapshot.sessions.batch: bad event', err)
    }
  })

  source.addEventListener('snapshot.sessions.ready', (e) => {
    try {
      const { epoch } = JSON.parse(e.data) as { epoch: number }
      if (!readySessionsBootstrap(epoch)) console.warn('snapshot.sessions.ready: no matching bootstrap')
    } catch (err) {
      discardSessionsBootstrap()
      console.warn('snapshot.sessions.ready: bad event', err)
    }
  })

  source.addEventListener('snapshot.sessions.error', (e) => {
    // A diagnostic quarantines one row but does not invalidate the epoch.
    // It becomes a persistent sidebar warning when ready commits this epoch.
    try {
      const diagnostic = JSON.parse(e.data) as SessionStreamWarning & { epoch: number }
      appendSessionStreamWarning(diagnostic.epoch, {
        id: diagnostic.id ?? '', code: diagnostic.code ?? 'row_omitted', message: diagnostic.message ?? 'session row omitted', count: diagnostic.count ?? 1,
      })
    } catch { /* malformed diagnostics cannot invalidate staging */ }
    console.warn('snapshot.sessions.error:', e.data)
  })

  // Transitional fallback for a proxy or one-release-old daemon which ignores
  // the explicit protocol marker. Old tabs request no marker and receive this
  // event from a new daemon as well.
  source.addEventListener('snapshot.sessions', (e) => {
    try {
      if (sessionStreamMode === 'v3') return
      const envelope = JSON.parse(e.data) as { sessions?: ProtocolSession[] }
      sessionStreamMode = 'legacy'
      discardSessionsBootstrap()
      applySessionsSnapshot((envelope.sessions ?? []).map(toUISession))
      sessionStreamWarnings.value = []
      sessionStreamOmittedTotal.value = 0
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
      // navigate() preserves tab-identity params across normalization.
      navigate(url + currentHash(), true)
    }
  })
  cleanups.push(disposeUrlNorm)

  // Entering a session consumes what was already there. A completion that
  // arrives while this same session remains open is different: unread must be
  // observable first, and is consumed only by the next deliberate page
  // interaction. Presence still suppresses delivery while focused+visible.
  const viewConsumption = createViewConsumptionTracker()
  const disposeMarkRead = effect(() => {
    const observed = viewConsumption.selection(selectedId.value, selected.value)
    if (observed) markSessionRead(observed.id, observed.token)
  })
  cleanups.push(disposeMarkRead)

  const consumeSelectedOnInteraction = () => {
    const observed = viewConsumption.interaction(selectedId.peek(), selected.peek())
    if (observed) markSessionRead(observed.id, observed.token)
  }
  if (typeof document !== 'undefined') {
    // Capture so terminal/keybinding handlers cannot stop the interaction
    // before it reaches this consumption boundary.
    document.addEventListener('click', consumeSelectedOnInteraction, true)
    document.addEventListener('keydown', consumeSelectedOnInteraction, true)
    cleanups.push(() => {
      document.removeEventListener('click', consumeSelectedOnInteraction, true)
      document.removeEventListener('keydown', consumeSelectedOnInteraction, true)
    })
  }

  return () => cleanups.forEach(fn => { fn() })
}
