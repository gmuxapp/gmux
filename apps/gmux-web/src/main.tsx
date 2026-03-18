import { render } from 'preact'
import { useCallback, useEffect, useMemo, useRef, useState } from 'preact/hooks'
import '@xterm/xterm/css/xterm.css'
import './styles.css'
import { createSidebarState } from './sidebar-state'
import { TerminalView } from './terminal'

import type { Session, Folder } from './types'
import { groupByFolder } from './types'
import { getMockFolders } from './mock-data/index'
import { installCopySession } from './mock-data/export-session'
import type { Session as ProtocolSession } from '@gmux/protocol'

// ── Config ──

const USE_MOCK = import.meta.env.VITE_MOCK === '1' || location.search.includes('mock')

// Debug: __gmuxCopySession() in devtools console
installCopySession()

/** Map protocol session (partial fields) to UI session (all fields required) */
function toUISession(s: ProtocolSession): Session {
  return {
    id: s.id,
    created_at: s.created_at ?? new Date().toISOString(),
    command: s.command ?? [],
    cwd: s.cwd ?? '',
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
    resume_key: s.resume_key ?? '',
    socket_path: s.socket_path ?? '',
    terminal_cols: s.terminal_cols ?? undefined,
    terminal_rows: s.terminal_rows ?? undefined,
  }
}

async function fetchSessions(): Promise<Session[]> {
  const resp = await fetch('/v1/sessions')
  const json = await resp.json()
  const data: ProtocolSession[] = json?.data ?? []
  return data.map(toUISession)
}

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

async function killSession(sessionId: string): Promise<void> {
  await postAction(`/v1/sessions/${sessionId}/kill`)
}

async function dismissSession(sessionId: string): Promise<void> {
  await postAction(`/v1/sessions/${sessionId}/dismiss`)
}

async function resumeSession(sessionId: string): Promise<void> {
  await postAction(`/v1/sessions/${sessionId}/resume`)
}

// ── Launcher types & config ──

interface LauncherDef {
  id: string
  label: string
  command: string[]
  description?: string
  available: boolean
}

interface LaunchConfig {
  default_launcher: string
  launchers: LauncherDef[]
}

interface HealthData {
  version: string
  tailscale_url?: string
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

/** Mask tailnet name for privacy: "https://gmux.angler-map.ts.net" → "https://gmux.an****.ts.net" */
function maskTailnet(url: string): string {
  return url.replace(/(\.\w{2})[^.]*(?=\.ts\.net)/, '$1****')
}

let _configCache: LaunchConfig | null = null

async function fetchConfig(): Promise<LaunchConfig> {
  if (_configCache) return _configCache
  try {
    const resp = await fetch('/v1/config')
    const json = await resp.json()
    _configCache = json.data ?? json
    return _configCache!
  } catch {
    return { default_launcher: 'shell', launchers: [{ id: 'shell', label: 'Shell', command: [], available: true }] }
  }
}

async function launchSession(launcherId: string, cwd?: string): Promise<void> {
  await postAction('/v1/launch', { launcher_id: launcherId, cwd })
}


// ── LaunchButton — transforms into inline menu on click ──
//
// Idle:      [+]
// Open:      [+ button becomes default item] → other items appear below
// Launching: [spinner]
//
// Double-click works because the default item occupies the exact same
// position as the + button. First click opens, second click hits default.

// Track pending launches globally so App can auto-select new sessions
let _pendingLaunchAt = 0

function LaunchButton({ cwd, className, onLaunch }: { cwd?: string; className?: string; onLaunch?: () => void }) {
  const [state, setState] = useState<'idle' | 'loading' | 'open' | 'launching'>('idle')
  const [config, setConfig] = useState<LaunchConfig | null>(null)
  const containerRef = useRef<HTMLDivElement>(null)

  // Pre-fetch config on first hover so open is instant
  const handleMouseEnter = () => {
    if (!config) fetchConfig().then(setConfig)
  }

  const handleClick = (e: MouseEvent) => {
    e.stopPropagation()
    if (state === 'idle') {
      if (config) {
        setState('open')
      } else {
        setState('loading')
        fetchConfig().then(cfg => {
          setConfig(cfg)
          setState('open')
        })
      }
    } else if (state === 'open') {
      setState('idle')
    }
  }

  const handleLaunch = (id: string) => {
    setState('launching')
    _pendingLaunchAt = Date.now()
    onLaunch?.()
    launchSession(id, cwd).finally(() => {
      // Reset after a short delay to show spinner
      setTimeout(() => setState('idle'), 600)
    })
  }

  // Close on outside click
  useEffect(() => {
    if (state !== 'open') return
    const handler = (e: MouseEvent) => {
      if (containerRef.current && !containerRef.current.contains(e.target as Node)) {
        setState('idle')
      }
    }
    const timer = setTimeout(() => document.addEventListener('mousedown', handler), 0)
    return () => {
      clearTimeout(timer)
      document.removeEventListener('mousedown', handler)
    }
  }, [state])

  // Close on Escape
  useEffect(() => {
    if (state !== 'open') return
    const handler = (e: KeyboardEvent) => { if (e.key === 'Escape') setState('idle') }
    document.addEventListener('keydown', handler)
    return () => document.removeEventListener('keydown', handler)
  }, [state])

  const isOpen = state === 'open' && config
  const isLoading = state === 'launching' || state === 'loading'

  let defaultLauncher: LauncherDef | undefined
  let others: LauncherDef[] = []
  if (isOpen && config) {
    defaultLauncher = config.launchers.find(l => l.id === config.default_launcher)
    others = config.launchers.filter(l => l.id !== config.default_launcher)
  }

  // Always render the + button for stable layout. Menu overlays on top.
  return (
    <div class={`launch-container ${className ?? ''}`} ref={containerRef} onMouseEnter={handleMouseEnter}>
      <button
        class={`launch-btn ${isLoading ? 'loading' : ''}`}
        title={cwd ? `New session in ${cwd}` : 'New session in ~'}
        onClick={handleClick}
      >
        {isLoading ? (
          <svg viewBox="0 0 16 16" width="14" height="14" class="spin">
            <circle cx="8" cy="8" r="6" fill="none" stroke="currentColor"
              stroke-width="2" stroke-dasharray="28" stroke-dashoffset="8" stroke-linecap="round" />
          </svg>
        ) : '+'}
      </button>
      {isOpen && (
        <div class="launch-inline-menu">
          {defaultLauncher && (
            <button
              class="launch-inline-item launch-inline-default"
              onClick={(e) => { e.stopPropagation(); handleLaunch(defaultLauncher!.id) }}
            >
              <span class="launch-inline-label">{defaultLauncher.label}</span>
              <span class="launch-inline-desc">{defaultLauncher.description ?? ''}</span>
            </button>
          )}
          {others.length > 0 && (
            <div class="launch-inline-divider" />
          )}
          {others.map((l, i) => (
            <button
              key={l.id}
              class="launch-inline-item"
              style={{ animationDelay: `${(i + 1) * 50}ms` }}
              onClick={(e) => { e.stopPropagation(); handleLaunch(l.id) }}
            >
              <span class="launch-inline-label">{l.label}</span>
              <span class="launch-inline-desc">{l.description ?? ''}</span>
            </button>
          ))}
        </div>
      )}
    </div>
  )
}

// ── Utilities ──

function formatAge(iso: string): string {
  const ms = Date.now() - new Date(iso).getTime()
  const mins = Math.floor(ms / 60_000)
  if (mins < 1) return 'now'
  if (mins < 60) return `${mins}m`
  const hrs = Math.floor(mins / 60)
  if (hrs < 24) return `${hrs}h`
  const days = Math.floor(hrs / 24)
  return `${days}d`
}

// ── Components ──

/** Determine the dot indicator state for a session. */
function sessionDotState(session: Session): 'working' | 'unread' | 'none' {
  if (session.alive && session.status?.working) return 'working'
  if (session.unread) return 'unread'
  return 'none'
}

function SessionItem({
  session,
  selected,
  resuming,
  onClick,
  onClose,
}: {
  session: Session
  selected: boolean
  resuming?: boolean
  onClick: () => void
  onClose?: () => void
}) {
  const dotState = resuming ? 'working' : sessionDotState(session)

  return (
    <div
      class={`session-item ${selected ? 'selected' : ''}`}
      onClick={onClick}
    >
      <span class={`session-dot-indicator ${dotState}`} />
      <div class="session-content">
        <div class="session-title-row">
          <span class="session-title">{session.title}</span>
          <span class="session-time">{formatAge(session.created_at)}</span>
        </div>
        <div class="session-meta">
          {session.status?.label && (
            <span class="session-status-label">{session.status.label}</span>
          )}
        </div>
      </div>
      {onClose && (
        <button
          class="session-close-btn"
          onClick={(e) => { e.stopPropagation(); onClose() }}
          title={session.alive ? 'Kill session' : 'Dismiss'}
        >
          ×
        </button>
      )}
    </div>
  )
}

function FolderGroup({
  folder,
  selectedId,
  resumingId,
  onSelect,
  onCloseSession,
  onHideFolder,
}: {
  folder: Folder
  selectedId: string | null
  resumingId: string | null
  onSelect: (id: string) => void
  onCloseSession: (session: Session) => void
  onHideFolder: (cwd: string) => void
}) {
  const [showResumable, setShowResumable] = useState(false)

  // Split sessions: live (top section) vs resumable (bottom drawer)
  const live: Session[] = []
  const resumable: Session[] = []
  for (const s of folder.sessions) {
    if (s.alive) live.push(s)
    else if (s.resumable) resumable.push(s)
    // Non-resumable dead sessions are not shown
  }

  return (
    <div class="folder">
      <div class="folder-header">
        <div class="folder-name">{folder.name}</div>
        <LaunchButton cwd={folder.path} className="folder-launch-btn" />
      </div>
      <div class="folder-sessions">
        {live.map(s => (
          <SessionItem
            key={s.id}
            session={s}
            selected={selectedId === s.id}
            onClick={() => onSelect(s.id)}
            onClose={() => onCloseSession(s)}
          />
        ))}
        <div class="folder-actions">
          {resumable.length > 0 && (
            <button
              class="folder-action-btn"
              onClick={() => setShowResumable(v => !v)}
            >
              {showResumable ? 'Hide previous' : `Resume previous (${resumable.length})`}
            </button>
          )}
          {resumable.length > 0 && (
            <span class="folder-action-sep">·</span>
          )}
          <button
            class="folder-action-btn"
            onClick={() => onHideFolder(folder.path)}
          >
            Hide
          </button>
        </div>
        {showResumable && resumable.map(s => (
          <SessionItem
            key={s.id}
            session={s}
            selected={false}
            resuming={resumingId === s.id}
            onClick={() => { setShowResumable(false); onSelect(s.id) }}
            onClose={() => onCloseSession(s)}
          />
        ))}
      </div>
    </div>
  )
}

function Sidebar({
  folders,
  hiddenFolders,
  selectedId,
  resumingId,
  onSelect,
  onCloseSession,
  onHideFolder,
  onShowFolder,
  open,
  onClose,
}: {
  folders: Folder[]
  hiddenFolders: Folder[]
  selectedId: string | null
  resumingId: string | null
  onSelect: (id: string) => void
  onCloseSession: (session: Session) => void
  onHideFolder: (cwd: string) => void
  onShowFolder: (cwd: string) => void
  open: boolean
  onClose: () => void
}) {
  const [showFolderPicker, setShowFolderPicker] = useState(false)

  return (
    <>
      <div class={`sidebar-overlay ${open ? 'visible' : ''}`} onClick={onClose} />
      <aside class={`sidebar ${open ? 'open' : ''}`}>
        <div class="sidebar-header">
          <div class="sidebar-logo">gmux</div>
          <div class="sidebar-badge">alpha</div>
          <LaunchButton className="sidebar-launch-btn" onLaunch={onClose} />
        </div>
        <div class="sidebar-scroll">
          {folders.map(f => (
            <FolderGroup
              key={f.path}
              folder={f}
              selectedId={selectedId}
              resumingId={resumingId}
              onSelect={(id) => {
                onSelect(id)
                onClose()
              }}
              onCloseSession={onCloseSession}
              onHideFolder={onHideFolder}
            />
          ))}
        </div>
        {hiddenFolders.length > 0 && (
          <div class="sidebar-footer">
            <button
              class="add-folder-btn"
              onClick={() => setShowFolderPicker(v => !v)}
            >
              + Add folder
            </button>
            {showFolderPicker && (
              <div class="folder-picker">
                {hiddenFolders.map(f => (
                  <button
                    key={f.path}
                    class="folder-picker-item"
                    onClick={() => {
                      onShowFolder(f.path)
                      setShowFolderPicker(false)
                    }}
                  >
                    <span class="folder-picker-name">{f.name}</span>
                  </button>
                ))}
              </div>
            )}
          </div>
        )}
      </aside>
    </>
  )
}

function EmptyState({ launchers, health }: { launchers: LauncherDef[]; health: HealthData | null }) {
  const [launching, setLaunching] = useState<string | null>(null)

  const handleLaunch = (id: string) => {
    setLaunching(id)
    _pendingLaunchAt = Date.now()
    launchSession(id).finally(() => setLaunching(null))
  }

  const tailscaleURL = health?.tailscale_url

  const defaultLauncher = launchers.find(l => l.id === 'shell') ?? launchers[0]
  const others = launchers.filter(l => l !== defaultLauncher)

  return (
    <div class="empty-state">
      <div class="empty-state-center">
        <div class="empty-state-title">Launch a new session</div>
        <div class="empty-state-others">
          <button
            class={`empty-state-launcher ${launching === defaultLauncher.id ? 'launching' : ''}`}
            onClick={() => handleLaunch(defaultLauncher.id)}
            disabled={launching !== null}
          >
            {defaultLauncher.id}
          </button>
          {others.map(l => (
            <button
              key={l.id}
              class={`empty-state-launcher ${launching === l.id ? 'launching' : ''} ${!l.available ? 'unavailable' : ''}`}
              onClick={() => handleLaunch(l.id)}
              disabled={launching !== null || !l.available}
            >
              {l.id.toLowerCase()}
            </button>
          ))}
        </div>
        <div class="empty-state-hint">
          or <code>gmux {'<command>'}</code> from any terminal
        </div>
      </div>
      <div class="empty-state-bottom">
        {!tailscaleURL?.includes(location.host) &&<>
          <span>http://{location.host}</span>
          <span class="empty-state-dot" />
        </>}
        {tailscaleURL &&
          <span>{maskTailnet(tailscaleURL)}</span>
        }
      </div>
    </div>
  )
}

function MainHeader({ session, onKill }: { session: Session | null; onKill?: (id: string) => void }) {
  if (!session) {
    return (
      <div class="main-header">
        <div class="main-header-title" style={{ color: 'var(--text-muted)' }}>
          gmux
        </div>
      </div>
    )
  }

  const shortCwd = session.cwd.replace(/^\/home\/[^/]+/, '~')

  return (
    <div class="main-header">
      <div class="main-header-left">
        <div class="main-header-title">
          {session.title}
          {session.stale && (
            <span class="stale-badge" title="This session is running a different build of gmux. Restart the session to update.">
              outdated
            </span>
          )}
        </div>
        <div class="main-header-meta">
          <span class="main-header-cwd">{shortCwd}</span>

        </div>
      </div>
      <div class="main-header-right">
        {session.status && session.status.label && (
          <div class={`main-header-status ${session.status.working ? 'working' : ''}`}>
            <span
              class={`session-dot ${session.status.working ? 'working' : 'idle'}`}
              style={{ width: 5, height: 5 }}
            />
            {session.status.label}
          </div>
        )}
        {session.alive && onKill && (
          <button
            class="header-kill-btn"
            onClick={() => onKill(session.id)}
            title="Kill session"
          >
            X
          </button>
        )}
      </div>
    </div>
  )
}

// ── Mobile nav icons ─────────────────────────────────────────────────────────
// Shared stroke style: round caps/joins, 1.8px weight, currentColor.
// Word-jump icons use a vertical boundary bar to signal "skip to word edge".

const S = { fill: 'none', stroke: 'currentColor', 'stroke-width': '1.4', 'stroke-linecap': 'round' as const, 'stroke-linejoin': 'round' as const }

// All plain arrows: 14×14 viewbox, 6-unit shaft, arrowhead ±3 from centre, centred at (7,7).
const IconUp    = () => <svg viewBox="0 0 14 14" width="16" height="16" {...S}><path d="M7 10V4m0 0-3 3m3-3 3 3"/></svg>
const IconDown  = () => <svg viewBox="0 0 14 14" width="16" height="16" {...S}><path d="M7 4v6m0 0-3-3m3 3 3-3"/></svg>
const IconLeft  = () => <svg viewBox="0 0 14 14" width="16" height="16" {...S}><path d="M10 7H4m0 0 3-3M4 7l3 3"/></svg>
const IconRight = () => <svg viewBox="0 0 14 14" width="16" height="16" {...S}><path d="M4 7h6m0 0-3-3m3 3-3 3"/></svg>

// Word-jump: 18×14 viewbox. Bar height matches arrow span (y 3–11). 7-unit shaft, 2.5-unit gap to bar.
// |← jump to start of previous word
const IconWordLeft  = () => <svg viewBox="0 0 18 14" width="20" height="16" {...S}><line x1="3.5" y1="3" x2="3.5" y2="11"/><path d="M13 7H6m0 0 3-3M6 7l3 3"/></svg>
// →| jump to end of next word
const IconWordRight = () => <svg viewBox="0 0 18 14" width="20" height="16" {...S}><line x1="14.5" y1="3" x2="14.5" y2="11"/><path d="M5 7h7m0 0-3-3m3 3-3 3"/></svg>
// ↵ return — stem drops from top-right, curves into a horizontal shaft pointing left
const IconReturn = () => <svg viewBox="0 0 14 14" width="16" height="16" {...S}><path d="M12 4V5.5Q12 7 10 7H4m0 0 3-3M4 7l3 3"/></svg>

function MobileTerminalBar({
  canSend,
  ctrlArmed,
  onMenu,
  onSend,
  onToggleCtrl,
  onFocusTerminal,
}: {
  canSend: boolean
  ctrlArmed: boolean
  onMenu: () => void
  onSend: (data: string) => void
  onToggleCtrl: () => void
  onFocusTerminal: () => void
}) {
  // Prevent blur from stealing focus away from the terminal on mousedown,
  // while still allowing individual onClick handlers to refocus as needed.
  const keepFocus = (ev: Event) => ev.preventDefault()

  // Send a sequence and re-focus the terminal (opens the keyboard on mobile).
  const tap = (seq: string) => { onSend(seq); onFocusTerminal() }

  return (
    <div class="mobile-bottom-bar" aria-label="Mobile terminal controls">
      <button class="mobile-bottom-action" onClick={onMenu} title="Open sessions">
        ☰
      </button>
      <div class="mobile-bottom-sep" />
      <div class="mobile-terminal-actions" role="toolbar" aria-label="Terminal keys" onMouseDown={keepFocus}>

        {/* Position 1: esc  ──or──  ↑ when ctrl armed */}
        {ctrlArmed
          ? <button class="mobile-bottom-action" disabled={!canSend} onClick={() => tap('\x1b[A')} title="Up arrow"><IconUp /></button>
          : <button class="mobile-bottom-action" disabled={!canSend} onClick={() => onSend('\x1b')} title="Escape">esc</button>
        }

        {/* Position 2: tab  ──or──  ↓ when ctrl armed */}
        {ctrlArmed
          ? <button class="mobile-bottom-action" disabled={!canSend} onClick={() => tap('\x1b[B')} title="Down arrow"><IconDown /></button>
          : <button class="mobile-bottom-action" disabled={!canSend} onClick={() => tap('\t')} title="Tab">tab</button>
        }

        {/* ctrl toggle — opens keyboard so the user can type the modified key */}
        <button
          class={`mobile-bottom-action ${ctrlArmed ? 'armed' : ''}`}
          disabled={!canSend}
          onClick={() => { onToggleCtrl(); onFocusTerminal() }}
          title={ctrlArmed ? 'Ctrl armed for next typed key' : 'Arm Ctrl for next typed key'}
          aria-pressed={ctrlArmed}
        >
          ctrl
        </button>

        {/* Position 4: ←  ──or──  word-left when ctrl armed */}
        {ctrlArmed
          ? <button class="mobile-bottom-action" disabled={!canSend} onClick={() => tap('\x1b[1;5D')} title="Word left"><IconWordLeft /></button>
          : <button class="mobile-bottom-action" disabled={!canSend} onClick={() => tap('\x1b[D')} title="Left arrow"><IconLeft /></button>
        }

        {/* Position 5: →  ──or──  word-right when ctrl armed */}
        {ctrlArmed
          ? <button class="mobile-bottom-action" disabled={!canSend} onClick={() => tap('\x1b[1;5C')} title="Word right"><IconWordRight /></button>
          : <button class="mobile-bottom-action" disabled={!canSend} onClick={() => tap('\x1b[C')} title="Right arrow"><IconRight /></button>
        }

        <button class="mobile-bottom-action" disabled={!canSend} onClick={() => tap('\n')} title="Enter"><IconReturn /></button>
      </div>
    </div>
  )
}

// ── App ──

type ConnectionState = 'connecting' | 'connected' | 'error'

const sidebarState = createSidebarState()

function App() {
  // Track visual viewport height for keyboard-aware layout.
  // dvh doesn't respond to the virtual keyboard; visualViewport does.
  useEffect(() => {
    const vv = window.visualViewport
    if (!vv) return

    const update = () => {
      document.documentElement.style.setProperty('--app-height', `${vv.height}px`)
    }
    update()
    vv.addEventListener('resize', update)
    return () => vv.removeEventListener('resize', update)
  }, [])

  const [sessions, setSessions] = useState<Session[]>([])
  const [selectedId, setSelectedId] = useState<string | null>(null)
  const [sidebarOpen, setSidebarOpen] = useState(false)
  const [connState, setConnState] = useState<ConnectionState>('connecting')
  const [ctrlArmed, setCtrlArmed] = useState(false)
  const [launchers, setLaunchers] = useState<LauncherDef[]>([])
  const [health, setHealth] = useState<HealthData | null>(null)
  const [sidebarVersion, forceUpdate] = useState(0) // re-render on sidebar state change
  const terminalInputRef = useRef<((data: string) => void) | null>(null)
  const terminalFocusRef = useRef<(() => void) | null>(null)

  useEffect(() => { fetchConfig().then(cfg => setLaunchers(cfg.launchers)) }, [])
  useEffect(() => { fetchHealth().then(setHealth) }, [])

  // Subscribe to sidebar state changes for re-render
  useEffect(() => sidebarState.subscribe(() => forceUpdate(n => n + 1)), [])

  // Refresh timestamps every 60s
  useEffect(() => {
    const timer = setInterval(() => forceUpdate(n => n + 1), 60_000)
    return () => clearInterval(timer)
  }, [])

  // Sync sidebar visibility whenever sessions change
  useEffect(() => { sidebarState.syncSessions(sessions) }, [sessions])

  // Load data
  useEffect(() => {
    if (USE_MOCK) {
      const mockFolders = getMockFolders()
      const allSessions = mockFolders.flatMap(f => f.sessions)
      setSessions(allSessions)
      setConnState('connected')
    } else {
      fetchSessions().then(list => {
        setSessions(list)
        setConnState('connected')
      }).catch(err => {
        console.error('Failed to fetch sessions:', err)
        setConnState('error')
      })

      // Subscribe to SSE for live updates
      const source = new EventSource('/v1/events')
      let sseConnected = false
      source.addEventListener('open', () => {
        if (sseConnected) {
          // Reconnected after a drop — do a full refresh to catch missed events
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
          // Auto-select newly launched sessions
          if (isNew && _pendingLaunchAt && Date.now() - _pendingLaunchAt < 10_000) {
            _pendingLaunchAt = 0
            setSelectedId(updated.id)
          }
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
      return () => source.close()
    }
  }, [])

  // URL param filtering: ?project=name or ?cwd=/path
  const filteredSessions = useMemo(() => {
    const params = new URLSearchParams(location.search)
    const project = params.get('project')
    const cwdFilter = params.get('cwd')
    if (!project && !cwdFilter) return sessions
    return sessions.filter(s => {
      if (project && !s.cwd.toLowerCase().includes(project.toLowerCase())) return false
      if (cwdFilter && !s.cwd.startsWith(cwdFilter)) return false
      return true
    })
  }, [sessions])

  const allFolders = useMemo(() => groupByFolder(filteredSessions), [filteredSessions])
  const folders = useMemo(
    () => allFolders.filter(f => sidebarState.isFolderVisible(f.path)),
    [allFolders, sidebarVersion],
  )
  const hiddenFolders = useMemo(
    () => allFolders.filter(f => !sidebarState.isFolderVisible(f.path)),
    [allFolders, sidebarVersion],
  )
  const selected = useMemo(() => {
    const s = sessions.find(s => s.id === selectedId) ?? null
    ;(window as any).__gmuxSession = s
    return s
  }, [sessions, selectedId])

  // Auto-select: pick first attachable session on initial load.
  const hasAutoSelected = useRef(false)
  useEffect(() => {
    if (!selectedId && !hasAutoSelected.current && filteredSessions.length > 0) {
      hasAutoSelected.current = true
      const best = filteredSessions.find(s => s.alive && s.socket_path)
      if (best) setSelectedId(best.id)
    }
  }, [filteredSessions, selectedId])

  // --- Actions: send to backend, wait for SSE. No optimistic updates. ---

  // resumingId is pure UI state — shows a spinner while waiting for the
  // backend to confirm the session is alive. Not session state.
  const [resumingId, setResumingId] = useState<string | null>(null)

  const handleCloseSession = useCallback((session: Session) => {
    if (session.alive) {
      killSession(session.id)
    } else {
      dismissSession(session.id)
    }
  }, [])

  const handleHideFolder = useCallback((cwd: string) => {
    sidebarState.hideFolder(cwd)
  }, [])

  const handleSelect = useCallback((id: string) => {
    const sess = sessions.find(s => s.id === id)
    if (sess?.resumable) {
      // Resume: show spinner, send request, wait for SSE to make it alive.
      setResumingId(id)
      resumeSession(id).catch(err => {
        console.error('resume failed:', err)
        setResumingId(prev => prev === id ? null : prev)
      })
      return
    }
    setResumingId(null) // cancel any pending resume auto-select
    setSelectedId(id)
    setCtrlArmed(false)
  }, [sessions])

  // When a resumed session comes alive, select it.
  useEffect(() => {
    if (resumingId) {
      const sess = sessions.find(s => s.id === resumingId)
      if (sess?.alive && sess?.socket_path) {
        setSelectedId(resumingId)
        setResumingId(null)
      }
    }
  }, [sessions, resumingId])

  // Resume timeout — clear after 10s if the session never came alive.
  useEffect(() => {
    if (!resumingId) return
    const t = setTimeout(() => setResumingId(null), 10_000)
    return () => clearTimeout(t)
  }, [resumingId])

  const canAttach = !!selected?.alive && !!selected?.socket_path && !USE_MOCK

  // Selected = what the terminal shows. No terminal → deselect.
  useEffect(() => {
    if (!canAttach) setCtrlArmed(false)
    if (selectedId && (!selected || !selected.alive)) {
      setSelectedId(null)
    }
  }, [canAttach, selectedId, selected])

  const handleTerminalInputReady = useCallback((send: ((data: string) => void) | null) => {
    terminalInputRef.current = send
  }, [])

  const handleTerminalFocusReady = useCallback((focus: (() => void) | null) => {
    terminalFocusRef.current = focus
  }, [])

  const handleFocusTerminal = useCallback(() => {
    terminalFocusRef.current?.()
  }, [])

  const handleMobileInput = useCallback((data: string) => {
    terminalInputRef.current?.(data)
  }, [])

  const handleToggleCtrl = useCallback(() => {
    if (!canAttach) return
    setCtrlArmed((armed) => !armed)
  }, [canAttach])

  const handleCtrlConsumed = useCallback(() => {
    setCtrlArmed(false)
  }, [])

  return (
    <div class="app-layout">
      <Sidebar
        folders={folders}
        hiddenFolders={hiddenFolders}
        selectedId={selectedId}
        resumingId={resumingId}
        onSelect={handleSelect}
        onCloseSession={handleCloseSession}
        onHideFolder={handleHideFolder}
        onShowFolder={(cwd) => sidebarState.showFolder(cwd)}
        open={sidebarOpen}
        onClose={() => setSidebarOpen(false)}
      />

      <div class="main-panel">
        <MainHeader session={selected} onKill={killSession} />

        {connState === 'connecting' ? (
          <div class="state-message">
            <div class="state-icon">⋯</div>
            <div class="state-title">Connecting</div>
            <div class="state-subtitle">Reaching gmuxd…</div>
          </div>
        ) : connState === 'error' ? (
          <div class="state-message">
            <div class="state-icon" style={{ color: 'var(--status-error)' }}>⚠</div>
            <div class="state-title">Connection failed</div>
            <div class="state-subtitle">Could not reach gmuxd. Is it running?</div>
            <button class="btn btn-primary" style={{ marginTop: 12 }} onClick={() => location.reload()}>
              Retry
            </button>
          </div>
        ) : selected && (canAttach || USE_MOCK) ? (
          <TerminalView
            session={selected}
            ctrlArmed={ctrlArmed}
            onCtrlConsumed={handleCtrlConsumed}
            onInputReady={handleTerminalInputReady}
            onFocusReady={handleTerminalFocusReady}
          />
        ) : (
          <EmptyState launchers={launchers} health={health} />
        )}

        <MobileTerminalBar
          canSend={canAttach}
          ctrlArmed={ctrlArmed}
          onMenu={() => setSidebarOpen(true)}
          onSend={handleMobileInput}
          onToggleCtrl={handleToggleCtrl}
          onFocusTerminal={handleFocusTerminal}
        />
      </div>
    </div>
  )
}

render(<App />, document.getElementById('app')!)
