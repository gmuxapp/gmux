import { render } from 'preact'
import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks'
import { LocationProvider, Router, Route, lazy, useLocation } from 'preact-iso'
import './styles.css'

import { ReplayView } from './replay-view'
import { TerminalView, type SyncDiag } from './terminal'
import { useArrivalPulse } from './use-arrival-pulse'
import { Sidebar } from './sidebar'
import type { DotState } from './store'
import { usePresence } from './use-presence'

import type { Session } from './types'
import { ManageProjectsModal } from './manage-projects'
import { ProjectHub } from './project-hub'
import { Home } from './home'
import { LaunchButton } from './launcher'
import { MarkdownEditor } from './markdown-editor'
import { ImageViewer } from './image-viewer'
import { DiffPanel } from './diff-panel'
import { installCopySession } from './mock-data/export-session'

import {
  sessions, connState, selected, selectedId, view, health, peers,
  folders,
  terminalOptions, keybinds, macCommandIsCtrl,
  backgroundActivity, unreadCount,
  urlPath,
  initStore, setNavigate, navigateToSession, navigate,
  dismissSession, resumeSession, restartSession,
  sessionStaleness,
} from './store'

// Lazy-loaded routes (code-split, not bundled with the main app)
const InputDiagnostics = lazy(() => import('./input-diagnostics'))

// ── Config ──

const USE_MOCK = import.meta.env.VITE_MOCK === '1' || location.search.includes('mock')

// Mock mode: hide close buttons and other interactive chrome via CSS.
if (USE_MOCK) document.documentElement.classList.add('mock-mode')

// Debug: __gmuxCopySession() in devtools console
installCopySession()

// ── Components ──

function MainHeader({ session, onRestart, syncDiag }: {
  session: Session | null
  onRestart?: () => void
  syncDiag?: SyncDiag | null
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
        {syncDiag && <SyncDiagBadge diag={syncDiag} />}
        {session.status && session.status.label && (
          <div class={`main-header-status ${session.status.error ? 'error' : session.status.working ? 'working' : ''}`}>
            <span
              class={`session-dot ${session.status.error ? 'error' : session.status.working ? 'working' : 'idle'}`}
              style={{ width: 5, height: 5 }}
            />
            {session.status.label}
          </div>
        )}
        <SessionMenu session={session} onRestart={onRestart} syncDiag={syncDiag} />
      </div>
    </div>
  )
}

function fmtBytes(n: number): string {
  if (n < 1024) return `${n}B`
  if (n < 1048576) return `${(n / 1024).toFixed(1)}KB`
  return `${(n / 1048576).toFixed(2)}MB`
}

function fmtMs(startMs: number, endMs: number | null): string {
  const ms = (endMs ?? Date.now()) - startMs
  return ms < 1000 ? `${ms}ms` : `${(ms / 1000).toFixed(2)}s`
}

function SyncDiagBadge({ diag }: { diag: SyncDiag }) {
  const { syncPhase, scrollbackBytes, scrollbackMsgs, syncStartedAt, syncEndedAt, pendingWrite, wsState, reconnects } = diag

  // Only show during/after a sync, and on reconnects — hide when idle
  const interesting = syncPhase !== 'idle' || wsState === 'lost' || reconnects > 0
  if (!interesting) return null

  const phaseLabel: Record<string, string> = {
    idle: '', waiting: 'waiting', buffering: 'syncing…', done: 'synced', skipped: 'skipped'
  }

  const parts: string[] = []

  if (wsState === 'lost') parts.push('ws\u00a0lost')
  else if (wsState === 'connecting') parts.push('connecting…')

  if (reconnects > 0) parts.push(`reconnects\u00a0${reconnects}`)

  if (syncPhase !== 'idle' && syncPhase !== 'waiting') {
    parts.push(phaseLabel[syncPhase] ?? syncPhase)
  }

  if ((syncPhase === 'buffering' || syncPhase === 'done' || syncPhase === 'skipped') && scrollbackBytes > 0) {
    parts.push(`${fmtBytes(scrollbackBytes)}\u00a0/\u00a0${scrollbackMsgs}msg`)
  }

  if (syncPhase === 'buffering' && syncStartedAt) {
    parts.push(fmtMs(syncStartedAt, null))
  }

  if (syncPhase === 'done' && syncStartedAt) {
    parts.push(fmtMs(syncStartedAt, syncEndedAt))
  }

  if (pendingWrite) parts.push('writing…')

  if (parts.length === 0) return null

  const isError = syncPhase === 'skipped' || wsState === 'lost'
  const isActive = syncPhase === 'buffering' || pendingWrite || wsState === 'connecting'

  return (
    <div class={`sync-diag ${isError ? 'error' : isActive ? 'active' : 'done'}`}>
      {parts.join('\u2002·\u2002')}
    </div>
  )
}
function fmtNum(n: number): string {
  return n.toLocaleString()
}


function SessionMenu({ session, onRestart, syncDiag }: {
  session: Session
  onRestart?: () => void
  syncDiag?: SyncDiag | null
}) {
  const [open, setOpen] = useState(false)
  const menuRef = useRef<HTMLDivElement>(null)
  const healthVal = health.value

  // For remote sessions, compare against the peer's version (not the local
  // daemon's). Peers don't expose runner_hash, so only version comparison
  // is possible for remote sessions.
  const peerVersion = session.peer
    ? peers.value.find(p => p.name === session.peer)?.version
    : undefined
  const compareTarget = session.peer
    ? (peerVersion ? { version: peerVersion } : null)
    : healthVal
  const staleKind = sessionStaleness(session, compareTarget)

  // Close on outside click or Escape.
  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') setOpen(false) }
    const onClick = (e: MouseEvent) => {
      if (menuRef.current && !menuRef.current.contains(e.target as Node)) setOpen(false)
    }
    document.addEventListener('keydown', onKey)
    document.addEventListener('mousedown', onClick)
    return () => {
      document.removeEventListener('keydown', onKey)
      document.removeEventListener('mousedown', onClick)
    }
  }, [open])

  const versionDisplay = session.runner_version
    ? `v${session.runner_version}`
    : session.binary_hash
      ? session.binary_hash.slice(0, 8)
      : 'unknown'

  const hasActions = session.alive && onRestart

  return (
    <div class="session-menu" ref={menuRef}>
      <button
        class={`session-menu-trigger${staleKind ? ' stale' : ''}`}
        onClick={() => setOpen(!open)}
        title="Session actions"
        aria-expanded={open}
      >
        <span class="session-menu-icon">⋮</span>
        {staleKind && <span class="session-menu-badge" />}
      </button>
      {open && (
        <div class="session-menu-dropdown">
          {hasActions && (
            <>
              <button
                class={`session-menu-action${staleKind ? ' stale' : ''}`}
                onClick={() => { setOpen(false); onRestart!() }}
              >
                Restart session
                {staleKind && <span class="session-menu-action-tag">outdated</span>}
              </button>
              <div class="session-menu-divider" />
            </>
          )}
          <div class="session-menu-section-title">Session info</div>
          <div class="session-menu-row">
            <span class="session-menu-label">Adapter</span>
            <span class="session-menu-value">{session.kind}</span>
          </div>
          <div class="session-menu-row">
            <span class="session-menu-label">Version</span>
            <span class={`session-menu-value${staleKind ? ' stale' : ''}`}>
              {versionDisplay}
            </span>
          </div>
          {session.peer && (
            <div class="session-menu-row">
              <span class="session-menu-label">Host</span>
              <span class="session-menu-value">{session.peer}</span>
            </div>
          )}
          {syncDiag && (syncDiag.prefetchBytes > 0 || syncDiag.ghosttyScrollbackLines > 0) && (
            <>
              <div class="session-menu-divider" />
              <div class="session-menu-section-title">Scrollback</div>
              {syncDiag.prefetchBytes > 0 && (
                <div class="session-menu-row">
                  <span class="session-menu-label">On disk</span>
                  <span class="session-menu-value">
                    {fmtBytes(syncDiag.prefetchBytes)}
                  </span>
                </div>
              )}
              <div class="session-menu-row">
                <span class="session-menu-label">Buffer</span>
                <span class={`session-menu-value${syncDiag.ghosttyScrollbackLines >= syncDiag.ghosttyScrollbackLimit * 0.95 ? ' stale' : ''}`}>
                  {fmtNum(syncDiag.ghosttyScrollbackLines)} / {fmtNum(syncDiag.ghosttyScrollbackLimit)} lines
                </span>
              </div>
            </>
          )}
        </div>
      )}
    </div>
  )
}

// ── Mobile nav icons ─────────────────────────────────────────────────────────

const S = { fill: 'none', stroke: 'currentColor', 'stroke-width': '1.4', 'stroke-linecap': 'round' as const, 'stroke-linejoin': 'round' as const }

const IconUp    = () => <svg viewBox="0 0 14 14" width="16" height="16" {...S}><path d="M7 10V4m0 0-3 3m3-3 3 3"/></svg>
const IconDown  = () => <svg viewBox="0 0 14 14" width="16" height="16" {...S}><path d="M7 4v6m0 0-3-3m3 3 3-3"/></svg>
const IconLeft  = () => <svg viewBox="0 0 14 14" width="16" height="16" {...S}><path d="M10 7H4m0 0 3-3M4 7l3 3"/></svg>
const IconRight = () => <svg viewBox="0 0 14 14" width="16" height="16" {...S}><path d="M4 7h6m0 0-3-3m3 3-3 3"/></svg>

const IconWordLeft  = () => <svg viewBox="0 0 18 14" width="20" height="16" {...S}><line x1="3.5" y1="3" x2="3.5" y2="11"/><path d="M13 7H6m0 0 3-3M6 7l3 3"/></svg>
const IconWordRight = () => <svg viewBox="0 0 18 14" width="20" height="16" {...S}><line x1="14.5" y1="3" x2="14.5" y2="11"/><path d="M5 7h7m0 0-3-3m3 3-3 3"/></svg>
const IconSend  = () => <svg viewBox="0 0 14 14" width="16" height="16" fill="currentColor" stroke="none"><path d="M3 2.5l8 4.5-8 4.5V8.5L7.5 7 3 5.5z"/></svg>
const IconPaste = () => <svg viewBox="0 0 14 14" width="16" height="16" {...S}><rect x="3" y="3" width="8" height="9" rx="1"/><path d="M5.5 3V2.5a1.5 1.5 0 0 1 3 0V3"/><path d="M7 7v3m0 0-1.5-1.5M7 10l1.5-1.5"/></svg>
const IconCopy  = () => <svg viewBox="0 0 14 14" width="16" height="16" {...S}><rect x="4" y="4" width="7" height="8" rx="1"/><path d="M3 10V3a1 1 0 0 1 1-1h6"/></svg>

function MobileTerminalBar({
  canSend,
  ctrlArmed,
  altArmed,
  onMenu,
  onSend,
  onCopy,
  onPaste,
  onToggleCtrl,
  onToggleAlt,
  onFocusTerminal,
}: {
  canSend: boolean
  ctrlArmed: boolean
  altArmed: boolean
  onMenu: () => void
  onSend: (data: string) => void
  /** Copy active selection to clipboard, or select-all + copy if nothing selected. */
  onCopy: () => void
  onPaste: () => void
  onToggleCtrl: () => void
  onToggleAlt: () => void
  onFocusTerminal: () => void
}) {
  // Read signals directly; no props needed for these.
  const bgActivity: DotState = backgroundActivity.value
  const unread = unreadCount.value
  const arrival = useArrivalPulse(bgActivity, unread)

  const keepFocus = (ev: Event) => ev.preventDefault()
  const tap = (seq: string) => { onSend(seq); onFocusTerminal() }

  const [holdWordMode, setHoldWordMode] = useState(false)
  const holdTimer1   = useRef<ReturnType<typeof setTimeout>  | null>(null)
  const holdTimer2   = useRef<ReturnType<typeof setTimeout>  | null>(null)
  const holdInterval = useRef<ReturnType<typeof setInterval> | null>(null)
  const holdGen      = useRef(0)

  const clearHold = () => {
    holdGen.current++
    if (holdTimer1.current)   { clearTimeout(holdTimer1.current);   holdTimer1.current   = null }
    if (holdTimer2.current)   { clearTimeout(holdTimer2.current);   holdTimer2.current   = null }
    if (holdInterval.current) { clearInterval(holdInterval.current); holdInterval.current = null }
    setHoldWordMode(false)
  }

  useEffect(() => () => clearHold(), [])

  const startArrowHold = (arrowSeq: string, wordSeq: string) => {
    const gen = holdGen.current
    holdTimer1.current = setTimeout(() => {
      if (holdGen.current !== gen) return
      holdInterval.current = setInterval(() => tap(arrowSeq), 50)
      holdTimer2.current = setTimeout(() => {
        if (holdGen.current !== gen) return
        clearInterval(holdInterval.current!)
        holdInterval.current = null
        setHoldWordMode(true)
        tap(wordSeq)
        holdInterval.current = setInterval(() => tap(wordSeq), 180)
      }, 700)
    }, 400)
  }

  const showCtrl = ctrlArmed || holdWordMode

  return (
    <div class="mobile-bottom-bar" aria-label="Mobile terminal controls">
      <button
        class={`mobile-bottom-action menu-btn${bgActivity !== 'none' ? ` bg-${bgActivity}` : ''}${arrival ? ` bg-${arrival}` : ''}`}
        onClick={onMenu}
        title="Open sessions"
      >
        ☰
      </button>
      <div class="mobile-bottom-sep" />
      <div class="mobile-terminal-actions" role="toolbar" aria-label="Terminal keys" onMouseDown={keepFocus}>
        {(ctrlArmed || altArmed)
          ? <button class="mobile-bottom-action" disabled={!canSend} onClick={() => tap('\x1b[A')} title="Up arrow"><IconUp /></button>
          : <button class="mobile-bottom-action" disabled={!canSend} onClick={() => tap('\x1b')} title="Escape">esc</button>
        }
        {(ctrlArmed || altArmed)
          ? <button class="mobile-bottom-action" disabled={!canSend} onClick={() => tap('\x1b[B')} title="Down arrow"><IconDown /></button>
          : <button class="mobile-bottom-action" disabled={!canSend} onClick={() => tap('\t')} title="Tab">tab</button>
        }
        <button
          class={`mobile-bottom-action ${showCtrl ? 'armed' : ''}`}
          disabled={!canSend}
          onClick={() => { if (holdWordMode) { clearHold(); } else { onToggleCtrl(); } onFocusTerminal() }}
          title={showCtrl ? 'Ctrl armed for next typed key' : 'Arm Ctrl for next typed key'}
          aria-pressed={showCtrl}
        >
          ctrl
        </button>
        <button
          class={`mobile-bottom-action ${altArmed ? 'armed' : ''}`}
          disabled={!canSend}
          onClick={() => { onToggleAlt(); onFocusTerminal() }}
          title={altArmed ? 'Alt armed for next typed key' : 'Arm Alt for next typed key'}
          aria-pressed={altArmed}
        >
          alt
        </button>
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
        <button class="mobile-bottom-action" disabled={!canSend} onClick={onCopy} title="Copy selection (or select all)"><IconCopy /></button>
      </div>
    </div>
  )
}

// ── App ──

function App() {
  // Visual viewport tracking for keyboard-aware layout.
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

  // Wire the store's navigate function to preact-iso's router.
  const loc = useLocation()
  useEffect(() => {
    setNavigate((url, replace) => loc.route(url, replace))
    // Test-only navigation hook: routes to a session by ID. Used by
    // e2e/helpers.ts to drive the app from a known session ID, since
    // the post-refactor home page no longer auto-selects.
    //
    // Returns true only when navigation was actually dispatched.
    // Returns false until both the session and its project have
    // loaded, so callers (and waitForURL) can rely on the URL having
    // changed once this returns true.
    ;(window as any).__gmuxNavigateToSession = (sessionId: string): boolean => {
      return navigateToSession(sessionId, true)
    }
  }, [loc])

  // Sync preact-iso's URL to the store signal on every navigation.
  // useLayoutEffect ensures urlPath updates before paint, so the view
  // computed reacts before the browser renders a stale frame.
  useLayoutEffect(() => {
    urlPath.value = loc.path
  }, [loc.path])

  // Initialize the store (SSE, data fetching, effects).
  useEffect(() => initStore(), [])

  // ── Local UI state (not shared, belongs to App) ──
  const [sidebarOpen, setSidebarOpen] = useState(false)
  const [manageProjectsOpen, setManageProjectsOpen] = useState(false)
  const [ctrlArmed, setCtrlArmed] = useState(false)
  const [altArmed, setAltArmed] = useState(false)

  const selId = selectedId.value

  const [syncDiag, setSyncDiag] = useState<SyncDiag | null>(null)
  // Reset diag when the selected session changes
  useEffect(() => { setSyncDiag(null) }, [selId])
  const handleSyncDiag = useCallback((d: SyncDiag) => setSyncDiag(d), [])
  const selectedVal = selected.value
  const sessionsVal = sessions.value
  const connVal = connState.value
  const termOpts = terminalOptions.value
  const keybindsVal = keybinds.value
  const macCtrl = macCommandIsCtrl.value
  const viewVal = view.value

  const terminalInputRef = useRef<((data: string) => void) | null>(null)
  const terminalFocusRef = useRef<(() => void) | null>(null)
  const terminalPasteRef = useRef<(() => void) | null>(null)
  const terminalCopyRef  = useRef<(() => void) | null>(null)
  // Sessions that have been opened (TerminalView mounted + WS established).
  // Add when a live session is first selected; never remove (WS persists).
  // Use a ref so we can mutate during render without triggering re-renders.
  const openedSessionIdsRef = useRef(new Set<string>())

  const { notifPermission, requestNotifPermission } = usePresence()

  // ── Resume ──
  const [resumingId, setResumingId] = useState<string | null>(null)

  const handleCloseSession = useCallback((session: Session) => {
    // If closing the currently-viewed session, navigate to the one above it
    // (or below if it was the first), rather than landing on the project hub.
    const isSelected = selectedId.value === session.id
    if (isSelected) {
      const folder = folders.value.find(f => f.sessions.some(s => s.id === session.id))
      const allVisible = folder?.sessions.filter(s => s.alive || s.resumable) ?? []
      const idx = allVisible.findIndex(s => s.id === session.id)
      const remaining = allVisible.filter(s => s.id !== session.id)
      // Prefer the item above; fall back to the new first item.
      const sibling = idx > 0 ? remaining[idx - 1] : remaining[0]
      dismissSession(session.id)
      if (sibling) {
        navigateToSession(sibling.id, true)
      } else {
        navigate('/', true)
      }
    } else {
      dismissSession(session.id)
    }
  }, [])

  const handleResume = useCallback((id: string) => {
    setResumingId(id)
    resumeSession(id).catch(err => {
      console.error('resume failed:', err)
      setResumingId(prev => prev === id ? null : prev)
    })
  }, [])

  // Clear modifier state and focus terminal when selection changes.
  useEffect(() => {
    if (!selId) return
    setResumingId(null)
    setCtrlArmed(false)
    setAltArmed(false)
    requestAnimationFrame(() => terminalFocusRef.current?.())
  }, [selId])

  // When a resumed session comes alive, navigate to it.
  useEffect(() => {
    if (resumingId) {
      const sess = sessionsVal.find(s => s.id === resumingId)
      if (sess?.alive && sess?.socket_path) {
        navigateToSession(resumingId, true)
        setResumingId(null)
      }
    }
  }, [sessionsVal, resumingId])

  // Resume timeout.
  useEffect(() => {
    if (!resumingId) return
    const t = setTimeout(() => setResumingId(null), 10_000)
    return () => clearTimeout(t)
  }, [resumingId])

  const canAttach = !!selectedVal?.alive && (!!selectedVal?.socket_path || !!selectedVal?.peer) && !USE_MOCK

  // Track which sessions have live TerminalView instances.
  // Mutating the ref during render is safe: the update is visible in the
  // same render pass, before the terminal stack JSX is evaluated below.
  if (selId && selectedVal?.alive && (canAttach || USE_MOCK)) {
    openedSessionIdsRef.current.add(selId)
  }
  const terminalSessions = (termOpts && keybindsVal)
    ? sessionsVal.filter(s => s.alive && openedSessionIdsRef.current.has(s.id))
    : []
  // The active view is a live terminal if it's in the opened set.
  const activeIsTerminal = !!selectedVal?.alive && openedSessionIdsRef.current.has(selectedVal.id)

  // Clear modifiers when terminal isn't attachable.
  useEffect(() => {
    if (!canAttach) { setCtrlArmed(false); setAltArmed(false) }
  }, [canAttach])

  // ── Terminal callbacks ──
  const handleTerminalInputReady = useCallback((send: ((data: string) => void) | null) => {
    terminalInputRef.current = send
  }, [])
  const handleTerminalFocusReady = useCallback((focus: (() => void) | null) => {
    terminalFocusRef.current = focus
    focus?.()
  }, [])
  const handleFocusTerminal = useCallback(() => { terminalFocusRef.current?.() }, [])
  const handleMobileInput = useCallback((data: string) => { terminalInputRef.current?.(data) }, [])
  const handleTerminalPasteReady = useCallback((paste: (() => void) | null) => {
    terminalPasteRef.current = paste
  }, [])
  const handleTerminalCopyReady = useCallback((copy: (() => void) | null) => {
    terminalCopyRef.current = copy
  }, [])
  // The trigger encapsulates clipboard read, binary detection, upload,
  // and PTY emission. Mobile and desktop now share one paste code path,
  // so binary clipboard items work from the toolbar button too.
  const handleMobilePaste = useCallback(() => {
    terminalPasteRef.current?.()
  }, [])
  const handleMobileCopy = useCallback(() => {
    terminalCopyRef.current?.()
  }, [])
  const handleToggleCtrl = useCallback(() => {
    if (!canAttach) return
    setCtrlArmed(armed => !armed)
  }, [canAttach])
  const handleCtrlConsumed = useCallback(() => { setCtrlArmed(false) }, [])
  const handleToggleAlt = useCallback(() => {
    if (!canAttach) return
    setAltArmed(armed => !armed)
  }, [canAttach])
  const handleAltConsumed = useCallback(() => { setAltArmed(false) }, [])

  return (
    <div class="app-layout">
      <Sidebar
        resumingId={resumingId}
        onCloseSession={handleCloseSession}
        onManageProjects={() => { setSidebarOpen(false); setManageProjectsOpen(true) }}
        open={sidebarOpen}
        onClose={() => setSidebarOpen(false)}
        notifPermission={notifPermission}
        onRequestNotifPermission={requestNotifPermission}
      />

      <ManageProjectsModal
        open={manageProjectsOpen}
        onClose={() => setManageProjectsOpen(false)}
      />

      <div class="main-panel">
        {viewVal !== null && viewVal.kind !== 'project' && viewVal.kind !== 'home' && viewVal.kind !== 'markdown-editor' && viewVal.kind !== 'image-viewer' && viewVal.kind !== 'diff-viewer' && (
          <MainHeader
            session={selectedVal}
            syncDiag={syncDiag}
            onRestart={selectedVal ? () => { restartSession(selectedVal.id).catch(err => console.error('restart failed:', err)) } : undefined}
          />
        )}

        {connVal === 'connecting' ? (
          <div class="state-message">
            <div class="state-icon">⋯</div>
            <div class="state-title">Connecting</div>
            <div class="state-subtitle">Reaching gmuxd...</div>
          </div>
        ) : connVal === 'error' ? (
          <div class="state-message">
            <div class="state-icon" style={{ color: 'var(--status-error)' }}>⚠</div>
            <div class="state-title">Connection failed</div>
            <div class="state-subtitle">Could not reach gmuxd. Is it running?</div>
            <button class="btn btn-primary" style={{ marginTop: 12 }} onClick={() => location.reload()}>
              Retry
            </button>
          </div>
        ) : viewVal?.kind === 'project' ? (
          <ProjectHub
            projectSlug={viewVal.projectSlug}
            onCloseSession={handleCloseSession}
          />
        ) : viewVal?.kind === 'markdown-editor' ? (
          <MarkdownEditor
            projectSlug={viewVal.projectSlug}
            filePath={viewVal.filePath}
          />
        ) : viewVal?.kind === 'image-viewer' ? (
          <ImageViewer
            projectSlug={viewVal.projectSlug}
            filePath={viewVal.filePath}
          />
        ) : viewVal?.kind === 'diff-viewer' ? (
          <DiffPanel
            projectSlug={viewVal.projectSlug}
            cwd={viewVal.cwd}
          />
        ) : null}

        {/* Persistent terminal stack: one TerminalView per opened live session.
            Hidden (display:none) when not active; WS stays open across switches.
            Only unmounts when the session is explicitly dismissed. */}
        {terminalSessions.map(s => (
          <TerminalView
            key={s.id}
            session={s}
            isActive={s.id === selId}
            terminalOptions={termOpts!}
            keybinds={keybindsVal!}
            macCommandIsCtrl={macCtrl}
            ctrlArmed={ctrlArmed}
            onCtrlConsumed={handleCtrlConsumed}
            altArmed={altArmed}
            onAltConsumed={handleAltConsumed}
            onInputReady={handleTerminalInputReady}
            onPasteReady={handleTerminalPasteReady}
            onFocusReady={handleTerminalFocusReady}
            onCopyReady={handleTerminalCopyReady}
            onSyncDiag={s.id === selId ? handleSyncDiag : undefined}
          />
        ))}

        {/* Non-terminal overlay: shown when the active view isn't a live terminal */}
        {!activeIsTerminal && (
          selectedVal && !selectedVal.alive && termOpts && !USE_MOCK ? (
            <ReplayView
              session={selectedVal}
              terminalOptions={termOpts}
              onResume={handleResume}
              resuming={resumingId === selectedVal.id}
            />
          ) : selectedVal ? (
            <div class="state-message">
              <div class="state-subtitle">Connecting…</div>
            </div>
          ) : (
            <Home />
          )
        )}

        <MobileTerminalBar
          canSend={canAttach}
          ctrlArmed={ctrlArmed}
          altArmed={altArmed}
          onMenu={() => setSidebarOpen(true)}
          onSend={handleMobileInput}
          onCopy={handleMobileCopy}
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
