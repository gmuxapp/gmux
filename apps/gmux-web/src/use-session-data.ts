/**
 * Data layer hook: sessions, SSE subscription, peers, launchers, health,
 * sidebar state subscription, and activity tracking.
 *
 * Encapsulates all data-fetching and server-event handling so App() can
 * focus on routing, layout, and UI state.
 */

import { useCallback, useEffect, useRef, useState } from 'preact/hooks'
import { useActivityTracker } from './use-activity'
import { createSidebarState } from './sidebar-state'
import { fetchFrontendConfig, buildTerminalOptions, resolveKeybinds, type ResolvedKeybind } from './config'
import { fetchConfig, invalidateConfigCache, consumePendingLaunch } from './launcher'
import { MOCK_SESSIONS, MOCK_PROJECTS } from './mock-data/index'
import type { Session, PeerInfo } from './types'
import type { LauncherDef } from './launcher'
import type { ITerminalOptions } from '@xterm/xterm'
import type { Session as ProtocolSession } from '@gmux/protocol'

// ── Helpers (pure, no hooks) ──

/** Map protocol session (partial fields) to UI session (all fields required). */
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
    resume_key: s.resume_key ?? undefined,
    stale: s.stale ?? false,
    peer: s.peer ?? undefined,
  }
}

async function fetchSessions(): Promise<Session[]> {
  const resp = await fetch('/v1/sessions')
  const json = await resp.json()
  const data: ProtocolSession[] = json?.data ?? []
  return data.map(toUISession)
}

async function fetchPeers(): Promise<PeerInfo[]> {
  try {
    const resp = await fetch('/v1/peers')
    const json = await resp.json()
    return json?.data ?? []
  } catch {
    return []
  }
}

export interface HealthData {
  version: string
  tailscale_url?: string
  update_available?: string
}

let _healthCache: HealthData | null = null

async function fetchHealth(): Promise<HealthData | null> {
  if (_healthCache) return _healthCache
  try {
    const resp = await fetch('/v1/health')
    const json = await resp.json()
    _healthCache = json.data ?? null
    return _healthCache
  } catch {
    return null
  }
}

// ── Shared singleton ──

const USE_MOCK = import.meta.env.VITE_MOCK === '1' || location.search.includes('mock')

export const sidebarState = createSidebarState()

// ── Hook ──

export type ConnectionState = 'connecting' | 'connected' | 'error'

export interface SessionData {
  sessions: Session[]
  sessionsLoaded: boolean
  connState: ConnectionState
  setSessions: (fn: (prev: Session[]) => Session[]) => void
  peers: PeerInfo[]
  launchers: LauncherDef[]
  health: HealthData | null
  sidebarVersion: number
  isSessionActive: (id: string) => boolean
  isSessionFading: (id: string) => boolean
  /** Counter that increments on activity state changes; use as a
   *  useMemo dependency to recompute derived state. */
  activityVersion: number
  terminalOptions: ITerminalOptions | null
  keybinds: ResolvedKeybind[] | null
  macCommandIsCtrl: boolean
}

/**
 * Central data hook. Manages session state, SSE subscription, peers,
 * launcher configs, health, frontend config, sidebar subscriptions,
 * and activity tracking.
 *
 * @param onNewSession - called when a brand-new session arrives via SSE
 *   (not updates to existing sessions). The caller can use this to
 *   auto-select newly launched sessions.
 */
export function useSessionData(onNewSession: (session: Session) => void): SessionData {
  const [sessions, setSessions] = useState<Session[]>([])
  const sessionsLoadedRef = useRef(false)
  const [connState, setConnState] = useState<ConnectionState>('connecting')
  const [peers, setPeers] = useState<PeerInfo[]>([])
  const [launchers, setLaunchers] = useState<LauncherDef[]>([])
  const [health, setHealth] = useState<HealthData | null>(null)
  const [sidebarVersion, forceUpdate] = useState(0)
  const { isActive: isSessionActive, isFading: isSessionFading, handleActivity, activityVersion } = useActivityTracker()

  const [terminalOptions, setTerminalOptions] = useState<ITerminalOptions | null>(null)
  const [keybinds, setKeybinds] = useState<ResolvedKeybind[] | null>(null)
  const [macCommandIsCtrl, setMacCommandIsCtrl] = useState(false)

  // Stable ref for the callback so the SSE effect doesn't re-subscribe
  // when the caller's closure changes.
  const onNewSessionRef = useRef(onNewSession)
  onNewSessionRef.current = onNewSession

  // One-shot fetches
  useEffect(() => { fetchConfig().then(cfg => setLaunchers(cfg.launchers)) }, [])
  useEffect(() => { fetchHealth().then(setHealth) }, [])
  useEffect(() => { fetchPeers().then(setPeers) }, [])
  useEffect(() => {
    fetchFrontendConfig().then(fc => {
      const macCtrl = fc.settings?.macCommandIsCtrl === true
      setTerminalOptions(buildTerminalOptions(fc.settings, fc.themeColors))
      setMacCommandIsCtrl(macCtrl)
      setKeybinds(resolveKeybinds(fc.settings?.keybinds ?? null, macCtrl))
    })
  }, [])

  // Subscribe to sidebar state changes for re-render
  useEffect(() => sidebarState.subscribe(() => forceUpdate(n => n + 1)), [])

  // Refresh timestamps every 60s
  useEffect(() => {
    const timer = setInterval(() => forceUpdate(n => n + 1), 60_000)
    return () => clearInterval(timer)
  }, [])

  // Load data + SSE
  useEffect(() => {
    if (USE_MOCK) {
      const localHost = new URLSearchParams(location.search).get('host')
      const mockSessions = localHost
        ? MOCK_SESSIONS.map(s => s.peer === localHost ? { ...s, peer: undefined } : s)
        : [...MOCK_SESSIONS]
      sidebarState.setMockProjects(MOCK_PROJECTS)
      setSessions(mockSessions)
      sessionsLoadedRef.current = true
      setConnState('connected')

      const activeIds = MOCK_SESSIONS.filter(s => s.mockActive).map(s => s.id)
      activeIds.forEach(id => handleActivity(id))
      const tick = setInterval(() => activeIds.forEach(id => handleActivity(id)), 2000)
      return () => clearInterval(tick)
    }

    sidebarState.fetchProjects()
    fetchSessions().then(list => {
      setSessions(list)
      sessionsLoadedRef.current = true
      setConnState('connected')
    }).catch(err => {
      console.error('Failed to fetch sessions:', err)
      setConnState('error')
    })

    const source = new EventSource('/v1/events')
    let sseConnected = false
    source.addEventListener('open', () => {
      if (sseConnected) {
        sidebarState.fetchProjects()
        fetchSessions().then(list => setSessions(list)).catch(() => {})
      }
      sseConnected = true
    })
    source.addEventListener('session-upsert', (e) => {
      try {
        const envelope = JSON.parse(e.data)
        const session = envelope.session ?? envelope
        const updated = toUISession(session)
        let isNew = false
        setSessions(prev => {
          const idx = prev.findIndex(s => s.id === updated.id)
          if (idx >= 0) {
            const next = [...prev]
            next[idx] = updated
            return next
          }
          isNew = true
          return [...prev, updated]
        })
        if (isNew) onNewSessionRef.current(updated)
      } catch (err) {
        console.warn('session-upsert: bad event', err)
      }
    })
    source.addEventListener('session-remove', (e) => {
      try {
        const { id } = JSON.parse(e.data)
        setSessions(prev => prev.filter(s => s.id !== id))
      } catch (err) {
        console.warn('session-remove: bad event', err)
      }
    })
    source.addEventListener('session-activity', (e) => {
      try {
        const { id } = JSON.parse(e.data)
        if (id) handleActivity(id)
      } catch {}
    })
    source.addEventListener('projects-update', () => {
      sidebarState.handleProjectsUpdate()
    })
    source.addEventListener('peer-status', () => {
      fetchPeers().then(setPeers).catch(() => {})
      invalidateConfigCache()
    })
    return () => source.close()
  }, [])

  // Stable setter that accepts an updater function (same signature as
  // React/Preact's setState). Exposed so callers can do optimistic updates
  // (e.g. marking a session as read).
  const stableSetter = useCallback(
    (fn: (prev: Session[]) => Session[]) => setSessions(fn),
    [],
  )

  return {
    sessions,
    sessionsLoaded: sessionsLoadedRef.current,
    connState,
    setSessions: stableSetter,
    peers,
    launchers,
    health,
    sidebarVersion,
    isSessionActive,
    isSessionFading,
    activityVersion,
    terminalOptions,
    keybinds,
    macCommandIsCtrl,
  }
}
