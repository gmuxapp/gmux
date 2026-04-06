import { render } from 'preact'
import { useCallback, useEffect, useMemo, useRef, useState } from 'preact/hooks'
import { LocationProvider, Router, Route, lazy, useLocation } from 'preact-iso'
import '@xterm/xterm/css/xterm.css'
import './styles.css'
import { TerminalView } from './terminal'
import { useArrivalPulse } from './use-arrival-pulse'
import { useSessionData, sidebarState } from './use-session-data'
import type { HealthData } from './use-session-data'
import { Sidebar } from './sidebar'
import type { DotState } from './sidebar'
import { usePresence } from './use-presence'

import type { Session, Folder, View } from './types'
import { ManageProjectsModal } from './manage-projects'
import { buildProjectFolders, matchSession, resolveViewFromPath, sessionPath, viewToPath } from './types'
import { ProjectHub } from './project-hub'
import type { LauncherDef } from './launcher'
import { LaunchButton, launchSession, consumePendingLaunch } from './launcher'
import { installCopySession } from './mock-data/export-session'

// Lazy-loaded routes (code-split, not bundled with the main app)
const InputDiagnostics = lazy(() => import('./input-diagnostics'))

// ── Config ──

const USE_MOCK = import.meta.env.VITE_MOCK === '1' || location.search.includes('mock')

// Mock mode: hide close buttons and other interactive chrome via CSS.
if (USE_MOCK) document.documentElement.classList.add('mock-mode')

// Debug: __gmuxCopySession() in devtools console
installCopySession()

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

async function restartSession(sessionId: string): Promise<void> {
  await postAction(`/v1/sessions/${sessionId}/restart`)
}

/** Mask tailnet name for privacy: "https://gmux.angler-map.ts.net" → "https://gmux.an****.ts.net" */
function maskTailnet(url: string): string {
  return url.replace(/(\.\w{2})[^.]*(?=\.ts\.net)/, '$1****')
}

// ── Components ──

function EmptyState({ launchers, health }: { launchers: LauncherDef[]; health: HealthData | null }) {
  const [launching, setLaunching] = useState<string | null>(null)

  const handleLaunch = (id: string) => {
    setLaunching(id)
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
          {tailscaleURL && <span class="empty-state-dot" />}
        </>}
        {tailscaleURL
          ? <span>{maskTailnet(tailscaleURL)}</span>
          : <>
              <span class="empty-state-dot" />
              <span>run <code>gmuxd remote</code> to enable remote access</span>
            </>
        }
      </div>
    </div>
  )
}

function MainHeader({ session, onRestart }: {
  session: Session | null
  onRestart?: () => void
}) {
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
        </div>
        <div class="main-header-meta">
          <span class="main-header-cwd">{shortCwd}</span>

        </div>
      </div>
      <div class="main-header-right">
        {session.status && session.status.label && (
          <div class={`main-header-status ${session.status.error ? 'error' : session.status.working ? 'working' : ''}`}>
            <span
              class={`session-dot ${session.status.error ? 'error' : session.status.working ? 'working' : 'idle'}`}
              style={{ width: 5, height: 5 }}
            />
            {session.status.label}
          </div>
        )}
        {session.stale && (
          <button class="stale-badge" title="Click to restart this session with the latest build" onClick={onRestart}>
            outdated
          </button>
        )}
        {session.kind && session.kind !== 'shell' && (
          <div class="main-header-kind" title="Adapter">{session.kind}</div>
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

// Word-jump: 18×14 viewbox. Bar height matches arrow span (y 3-11). 7-unit shaft, 2.5-unit gap to bar.
// |← jump to start of previous word
const IconWordLeft  = () => <svg viewBox="0 0 18 14" width="20" height="16" {...S}><line x1="3.5" y1="3" x2="3.5" y2="11"/><path d="M13 7H6m0 0 3-3M6 7l3 3"/></svg>
// →| jump to end of next word
const IconWordRight = () => <svg viewBox="0 0 18 14" width="20" height="16" {...S}><line x1="14.5" y1="3" x2="14.5" y2="11"/><path d="M5 7h7m0 0-3-3m3 3-3 3"/></svg>
// ▶ send - filled triangle pointing right (submit / send message)
const IconSend = () => <svg viewBox="0 0 14 14" width="16" height="16" fill="currentColor" stroke="none"><path d="M3 2.5l8 4.5-8 4.5V8.5L7.5 7 3 5.5z"/></svg>
// 📋 paste - clipboard with down-arrow suggesting "paste into"
const IconPaste = () => <svg viewBox="0 0 14 14" width="16" height="16" {...S}><rect x="3" y="3" width="8" height="9" rx="1"/><path d="M5.5 3V2.5a1.5 1.5 0 0 1 3 0V3"/><path d="M7 7v3m0 0-1.5-1.5M7 10l1.5-1.5"/></svg>
function MobileTerminalBar({
  canSend,
  ctrlArmed,
  altArmed,
  backgroundActivity,
  unreadCount,
  onMenu,
  onSend,
  onPaste,
  onToggleCtrl,
  onToggleAlt,
  onFocusTerminal,
}: {
  canSend: boolean
  ctrlArmed: boolean
  altArmed: boolean
  backgroundActivity: DotState
  unreadCount: number
  onMenu: () => void
  onSend: (data: string) => void
  onPaste: () => void
  onToggleCtrl: () => void
  onToggleAlt: () => void
  onFocusTerminal: () => void
}) {
  const arrival = useArrivalPulse(backgroundActivity, unreadCount)

  // Prevent blur from stealing focus away from the terminal on mousedown,
  // while still allowing individual onClick handlers to refocus as needed.
  const keepFocus = (ev: Event) => ev.preventDefault()

  // Send a sequence and re-focus the terminal (opens the keyboard on mobile).
  const tap = (seq: string) => { onSend(seq); onFocusTerminal() }

  // ── Hold-to-repeat for ← / → ──────────────────────────────────────────
  // Phase 1 (after 400 ms): repeat the arrow at ~50 ms (fast, like iOS key repeat).
  // Phase 2 (after 400 + 700 ms): switch to word navigation at ~180 ms and light
  // up the ctrl button so the user sees what is happening.
  //
  // Two pitfalls guarded against:
  //  • Pointer leaving the button before release: solved with setPointerCapture so
  //    pointerup always fires on the element that started the hold.
  //  • Race between clearHold() and a queued timer callback calling setHoldWordMode(true):
  //    solved with a generation counter - the callback checks gen before touching state.
  const [holdWordMode, setHoldWordMode] = useState(false)
  const holdTimer1   = useRef<ReturnType<typeof setTimeout>  | null>(null)
  const holdTimer2   = useRef<ReturnType<typeof setTimeout>  | null>(null)
  const holdInterval = useRef<ReturnType<typeof setInterval> | null>(null)
  const holdGen      = useRef(0)

  const clearHold = () => {
    holdGen.current++                                                          // invalidate in-flight timer callbacks
    if (holdTimer1.current)   { clearTimeout(holdTimer1.current);   holdTimer1.current   = null }
    if (holdTimer2.current)   { clearTimeout(holdTimer2.current);   holdTimer2.current   = null }
    if (holdInterval.current) { clearInterval(holdInterval.current); holdInterval.current = null }
    setHoldWordMode(false)
  }

  // Clean up timers when the bar unmounts.
  useEffect(() => () => clearHold(), [])

  const startArrowHold = (arrowSeq: string, wordSeq: string) => {
    const gen = holdGen.current
    holdTimer1.current = setTimeout(() => {
      if (holdGen.current !== gen) return
      // Phase 1: fast arrow repeat
      holdInterval.current = setInterval(() => tap(arrowSeq), 50)
      // Phase 2: switch to word navigation
      holdTimer2.current = setTimeout(() => {
        if (holdGen.current !== gen) return          // clearHold already ran - don't override false→true
        clearInterval(holdInterval.current!)
        holdInterval.current = null
        setHoldWordMode(true)
        tap(wordSeq)
        holdInterval.current = setInterval(() => tap(wordSeq), 180)
      }, 700)
    }, 400)
  }

  // showCtrl: ctrl button appears armed either because the user armed it, or
  // because hold-repeat has entered word-navigation mode.
  const showCtrl = ctrlArmed || holdWordMode

  return (
    <div class="mobile-bottom-bar" aria-label="Mobile terminal controls">
      <button
        class={`mobile-bottom-action menu-btn${backgroundActivity !== 'none' ? ` bg-${backgroundActivity}` : ''}${arrival ? ` bg-${arrival}` : ''}`}
        onClick={onMenu}
        title="Open sessions"
      >
        ☰
      </button>
      <div class="mobile-bottom-sep" />
      <div class="mobile-terminal-actions" role="toolbar" aria-label="Terminal keys" onMouseDown={keepFocus}>

        {/* Position 1: esc  ──or──  ↑ when ctrl or alt armed */}
        {(ctrlArmed || altArmed)
          ? <button class="mobile-bottom-action" disabled={!canSend} onClick={() => tap('\x1b[A')} title="Up arrow"><IconUp /></button>
          : <button class="mobile-bottom-action" disabled={!canSend} onClick={() => tap('\x1b')} title="Escape">esc</button>
        }

        {/* Position 2: tab  ──or──  ↓ when ctrl or alt armed */}
        {(ctrlArmed || altArmed)
          ? <button class="mobile-bottom-action" disabled={!canSend} onClick={() => tap('\x1b[B')} title="Down arrow"><IconDown /></button>
          : <button class="mobile-bottom-action" disabled={!canSend} onClick={() => tap('\t')} title="Tab">tab</button>
        }

        {/* ctrl toggle */}
        <button
          class={`mobile-bottom-action ${showCtrl ? 'armed' : ''}`}
          disabled={!canSend}
          onClick={() => { if (holdWordMode) { clearHold(); } else { onToggleCtrl(); } onFocusTerminal() }}
          title={showCtrl ? 'Ctrl armed for next typed key' : 'Arm Ctrl for next typed key'}
          aria-pressed={showCtrl}
        >
          ctrl
        </button>

        {/* alt toggle */}
        <button
          class={`mobile-bottom-action ${altArmed ? 'armed' : ''}`}
          disabled={!canSend}
          onClick={() => { onToggleAlt(); onFocusTerminal() }}
          title={altArmed ? 'Alt armed for next typed key' : 'Arm Alt for next typed key'}
          aria-pressed={altArmed}
        >
          alt
        </button>

        {/* Position 4 & 5: ← →  ──or──  word-left/right when ctrl/hold-word active.
            Hold to repeat; hold longer to switch to word navigation. */}
        {([
          { seq: '\x1b[D', wordSeq: '\x1b[1;5D', title: 'Left arrow',  wordTitle: 'Word left',  Icon: IconLeft,  WordIcon: IconWordLeft  },
          { seq: '\x1b[C', wordSeq: '\x1b[1;5C', title: 'Right arrow', wordTitle: 'Word right', Icon: IconRight, WordIcon: IconWordRight },
        ] as const).map(({ seq, wordSeq, title, wordTitle, Icon, WordIcon }) => (
          <button
            class="mobile-bottom-action"
            disabled={!canSend}
            onPointerDown={e => { e.currentTarget.setPointerCapture(e.pointerId); e.preventDefault(); const s = showCtrl ? wordSeq : seq; tap(s); startArrowHold(s, wordSeq) }}
            onPointerUp={clearHold}
            onPointerCancel={clearHold}
            onContextMenu={e => e.preventDefault()}
            title={showCtrl ? wordTitle : `${title} (hold to repeat)`}
          >
            {showCtrl ? <WordIcon /> : <Icon />}
          </button>
        ))}

        {ctrlArmed
          ? <button class="mobile-bottom-action" disabled={!canSend} onClick={() => { onPaste(); onFocusTerminal() }} title="Paste from clipboard"><IconPaste /></button>
          : <button class="mobile-bottom-action send-btn" disabled={!canSend} onClick={() => tap('\r')} title="Send"><IconSend /></button>
        }
      </div>
    </div>
  )
}

// ── App ──

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

  const loc = useLocation()

  // Auto-select newly launched sessions arriving via SSE.
  const handleNewSession = useCallback((updated: Session) => {
    if (consumePendingLaunch()) {
      const project = matchSession(updated, sidebarState.configured)
      if (project) loc.route(sessionPath(project.slug, updated), true)
    }
  }, [loc])

  const {
    sessions, sessionsLoaded, connState, setSessions,
    peers, launchers, health, sidebarVersion,
    isSessionActive, isSessionFading, activityVersion,
    terminalOptions, keybinds, macCommandIsCtrl,
  } = useSessionData(handleNewSession)

  const [sidebarOpen, setSidebarOpen] = useState(false)
  const [manageProjectsOpen, setManageProjectsOpen] = useState(false)
  const [ctrlArmed, setCtrlArmed] = useState(false)
  const [altArmed, setAltArmed] = useState(false)

  const terminalInputRef = useRef<((data: string) => void) | null>(null)
  const terminalFocusRef = useRef<(() => void) | null>(null)
  const terminalPasteRef = useRef<((text: string) => void) | null>(null)



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

  // `view` is derived from the URL. The URL is the single source of truth:
  // actions that change what's shown navigate (loc.route), never setView.
  // Null until we've loaded projects/sessions at least once so we don't
  // render a bogus home/project view before data arrives.
  const view = useMemo<View | null>(() => {
    if (filteredSessions.length === 0 && sidebarState.configured.length === 0) return null
    return resolveViewFromPath(loc.path, sidebarState.configured, filteredSessions)
  }, [loc.path, filteredSessions, sidebarVersion])
  const selectedId = view?.kind === 'session' ? view.sessionId : null

  const folders = useMemo(
    () => buildProjectFolders(sidebarState.configured, filteredSessions),
    [filteredSessions, sidebarVersion],
  )
  const selected = useMemo(() => {
    const s = sessions.find(s => s.id === selectedId) ?? null
    ;(window as any).__gmuxSession = s
    return s
  }, [sessions, selectedId])

  // Notification click: navigate to the session.
  const handleNotificationClick = useCallback((sessionId: string) => {
    const sess = sessions.find(s => s.id === sessionId)
    if (sess) {
      const project = matchSession(sess, sidebarState.configured)
      if (project) loc.route(sessionPath(project.slug, sess))
    }
  }, [sessions, loc, sidebarVersion])

  const { notifPermission, requestNotifPermission } = usePresence({
    selectedId,
    sessions,
    onNotificationClick: handleNotificationClick,
  })

  // Dot indicator for the hamburger: any *other* alive session that is busy, errored, or unread.
  const backgroundActivity = useMemo((): DotState => {
    const others = sessions.filter(s => s.id !== selectedId && s.alive)
    if (others.some(s => s.status?.error))   return 'error'
    if (others.some(s => s.status?.working)) return 'working'
    if (others.some(s => s.unread))          return 'unread'
    if (others.some(s => isSessionActive(s.id))) return 'active'
    if (others.some(s => isSessionFading(s.id))) return 'fading'
    return 'none'
  }, [sessions, selectedId, isSessionActive, isSessionFading, activityVersion])

  // Count of unread sessions (excluding selected). Used as a generation counter
  // so the hamburger badge re-animates when a new session becomes unread.
  const unreadCount = useMemo(
    () => sessions.filter(s => s.id !== selectedId && s.alive && s.unread).length,
    [sessions, selectedId],
  )

  // --- URL normalization ---
  // The URL is the single source of truth; `view` is derived above. This
  // effect only *normalizes* the URL when it drifts from the view: it
  // rewrites `/:project` → `/:project/:kind/:slug` when the URL resolved
  // to a specific session, and falls back to the project hub (or home)
  // when the session we were viewing disappeared.
  //
  // Guard: don't normalize until sessions have loaded at least once.
  // Without this, projects may load first (setting configured.length > 0),
  // causing the view memo to resolve a session URL to a project fallback
  // (no matching sessions yet), which overwrites the URL before the real
  // session data arrives.
  useEffect(() => {
    if (view === null) return
    if (!sessionsLoaded) return
    const url = viewToPath(view, sidebarState.configured, sessions)
    if (url && url !== loc.path) loc.route(url, true)
  }, [view, sessions, sidebarVersion, loc.path])

  // Mark as read when selecting a session, or when attention flags (unread,
  // error) appear while we're already looking at it (e.g. a turn completes,
  // filemon re-reads on resume, or an adapter sets error while in view).
  const selectedNeedsRead = !!(selected?.unread || selected?.status?.error)
  useEffect(() => {
    if (!selectedId || !selectedNeedsRead) return
    // Clear locally for immediate UI feedback.
    setSessions(prev => prev.map(s =>
      s.id === selectedId ? { ...s, unread: false, status: s.status?.error ? { ...s.status, error: false } : s.status } : s
    ))
    // Persist to server.
    fetch(`/v1/sessions/${selectedId}/read`, { method: 'POST' }).catch(() => {})
  }, [selectedId, selectedNeedsRead])

  // --- Actions: send to backend, wait for SSE. No optimistic updates. ---

  // resumingId is pure UI state - shows a spinner while waiting for the
  // backend to confirm the session is alive. Not session state.
  const [resumingId, setResumingId] = useState<string | null>(null)

  // Dismiss always: kills if alive, removes from project array, gone from sidebar.
  // Sessions that die on their own (crash, restart) stay as resumable.
  const handleCloseSession = useCallback((session: Session) => {
    dismissSession(session.id)
  }, [])

  // Resume a sleeping session. Navigation happens automatically via the
  // auto-select effect once the session comes alive.
  const handleResume = useCallback((id: string) => {
    setResumingId(id)
    resumeSession(id).catch(err => {
      console.error('resume failed:', err)
      setResumingId(prev => prev === id ? null : prev)
    })
  }, [])

  // Clear modifier state and focus the terminal when the selected session changes.
  useEffect(() => {
    if (!selectedId) return
    setResumingId(null)
    setCtrlArmed(false)
    setAltArmed(false)
    requestAnimationFrame(() => terminalFocusRef.current?.())
  }, [selectedId])

  // When a resumed session comes alive, select it.
  useEffect(() => {
    if (resumingId) {
      const sess = sessions.find(s => s.id === resumingId)
      if (sess?.alive && sess?.socket_path) {
        const project = matchSession(sess, sidebarState.configured)
        if (project) loc.route(sessionPath(project.slug, sess), true)
        setResumingId(null)
      }
    }
  }, [sessions, resumingId, sidebarVersion])

  // Resume timeout - clear after 10s if the session never came alive.
  useEffect(() => {
    if (!resumingId) return
    const t = setTimeout(() => setResumingId(null), 10_000)
    return () => clearTimeout(t)
  }, [resumingId])

  const canAttach = !!selected?.alive && (!!selected?.socket_path || !!selected?.peer) && !USE_MOCK

  // Clear modifier state when the terminal isn't attachable. Dead-but-present
  // sessions stay in view so the header persists through restart cycles.
  // Gone-from-store sessions are handled by Effect A, which re-resolves the
  // URL and falls back to the project (or home) view when the session
  // disappears.
  useEffect(() => {
    if (!canAttach) { setCtrlArmed(false); setAltArmed(false) }
  }, [canAttach])

  const handleTerminalInputReady = useCallback((send: ((data: string) => void) | null) => {
    terminalInputRef.current = send
  }, [])

  const handleTerminalFocusReady = useCallback((focus: (() => void) | null) => {
    terminalFocusRef.current = focus
    // Auto-focus the terminal as soon as it becomes ready.
    focus?.()
  }, [])

  const handleFocusTerminal = useCallback(() => {
    terminalFocusRef.current?.()
  }, [])

  const handleMobileInput = useCallback((data: string) => {
    terminalInputRef.current?.(data)
  }, [])

  const handleTerminalPasteReady = useCallback((paste: ((text: string) => void) | null) => {
    terminalPasteRef.current = paste
  }, [])

  const handleMobilePaste = useCallback(async () => {
    try {
      const text = await navigator.clipboard.readText()
      if (text) terminalPasteRef.current?.(text)
    } catch {
      // Clipboard read denied or unavailable; ignore silently.
    }
  }, [])

  const handleToggleCtrl = useCallback(() => {
    if (!canAttach) return
    setCtrlArmed((armed) => !armed)
  }, [canAttach])

  const handleCtrlConsumed = useCallback(() => {
    setCtrlArmed(false)
  }, [])

  const handleToggleAlt = useCallback(() => {
    if (!canAttach) return
    setAltArmed((armed) => !armed)
  }, [canAttach])

  const handleAltConsumed = useCallback(() => {
    setAltArmed(false)
  }, [])

  return (
    <div class="app-layout">
      <Sidebar
        folders={folders}
        unmatchedActiveCount={sidebarState.unmatchedActiveCount}
        selectedId={selectedId}
        currentProjectSlug={view?.kind === 'project' ? view.projectSlug : null}
        resumingId={resumingId}
        isSessionActive={isSessionActive}
        isSessionFading={isSessionFading}
        onResume={handleResume}
        onCloseSession={handleCloseSession}
        onManageProjects={() => { setSidebarOpen(false); setManageProjectsOpen(true) }}
        open={sidebarOpen}
        onClose={() => setSidebarOpen(false)}
        health={health}
        notifPermission={notifPermission}
        onRequestNotifPermission={requestNotifPermission}
      />

      <ManageProjectsModal
        open={manageProjectsOpen}
        onClose={() => setManageProjectsOpen(false)}
        sidebarState={sidebarState}
      />

      <div class="main-panel">
        {view?.kind !== 'project' && (
          <MainHeader
            session={selected}
            onRestart={selected ? () => { restartSession(selected.id).catch(err => console.error('restart failed:', err)) } : undefined}
          />
        )}

        {connState === 'connecting' ? (
          <div class="state-message">
            <div class="state-icon">⋯</div>
            <div class="state-title">Connecting</div>
            <div class="state-subtitle">Reaching gmuxd...</div>
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
        ) : view?.kind === 'project' ? (
          <ProjectHub
            projectSlug={view.projectSlug}
            sessions={sessions}
            projects={sidebarState.configured}
            peers={peers}
            onResume={handleResume}
            onCloseSession={handleCloseSession}
          />
        ) : selected && (canAttach || USE_MOCK) && terminalOptions && keybinds ? (
          <TerminalView
            session={selected}
            terminalOptions={terminalOptions}
            keybinds={keybinds}
            macCommandIsCtrl={macCommandIsCtrl}
            ctrlArmed={ctrlArmed}
            onCtrlConsumed={handleCtrlConsumed}
            altArmed={altArmed}
            onAltConsumed={handleAltConsumed}
            onInputReady={handleTerminalInputReady}
            onPasteReady={handleTerminalPasteReady}
            onFocusReady={handleTerminalFocusReady}
          />
        ) : selected ? (
          // Selected session is not yet connectable: starting up, briefly dead
          // during restart, or exited. Keep the header visible and show a
          // minimal placeholder; the terminal attaches when ready.
          <div class="state-message">
            <div class="state-subtitle">{selected.alive ? 'Connecting…' : 'Session ended'}</div>
          </div>
        ) : (
          <EmptyState launchers={launchers} health={health} />
        )}

        <MobileTerminalBar
          canSend={canAttach}
          ctrlArmed={ctrlArmed}
          altArmed={altArmed}
          backgroundActivity={backgroundActivity}
          unreadCount={unreadCount}
          onMenu={() => setSidebarOpen(true)}
          onSend={handleMobileInput}
          onPaste={handleMobilePaste}
          onToggleCtrl={handleToggleCtrl}
          onToggleAlt={handleToggleAlt}
          onFocusTerminal={handleFocusTerminal}
        />
      </div>
    </div>
  )
}

render(
  <LocationProvider>
    <Router>
      <Route path="/_/input-diagnostics" component={InputDiagnostics} />
      <Route default component={App} />
    </Router>
  </LocationProvider>,
  document.getElementById('app')!,
)
