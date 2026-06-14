import { render } from 'preact'
import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks'
import { LocationProvider, Router, Route, lazy, useLocation } from 'preact-iso'
import '@xterm/xterm/css/xterm.css'
import './styles.css'

import { applyArmedModifiers } from './keyboard'
import { ReplayView } from './replay-view'
import { TerminalView } from './terminal'
import { useArrivalPulse } from './use-arrival-pulse'
import { Sidebar } from './sidebar'
import { usePresence } from './use-presence'

import type { Session } from './types'
import { SettingsModal } from './settings'
import { ProjectHub } from './project-hub'
import { Home } from './home'
import { LaunchButton } from './launcher'
import { installCopySession } from './mock-data/export-session'
import { installVersionWatch } from './version-watch'

import {
  sessions, connState, selected, selectedId, view, health, peers,
  terminalOptions, keybinds, macCommandIsCtrl,
  unreadCount, keyboardOpen,
  urlPath, urlSearch,
  initStore, setNavigate, navigateToSession,
  dismissSession, resumeSession, restartSession,
  sessionStaleness,
} from './store'

// Lazy-loaded routes (code-split, not bundled with the main app)
const InputDiagnostics = lazy(() => import('./input-diagnostics'))

// ── Config ──

const USE_MOCK = import.meta.env.VITE_MOCK === '1' || location.search.includes('mock')

/** Visual-viewport occlusion (px) above which the on-screen keyboard is
 * considered open. Low enough to trip early in the slide-up animation and
 * above sub-pixel/URL-bar noise, yet above a hardware-keyboard accessory
 * bar (~44px on iPad) so that doesn't read as a soft keyboard. */
const KEYBOARD_PRESENCE_PX = 60

// On touch devices, focusing the terminal's hidden textarea pops the
// on-screen keyboard. So auto-focus (session select, terminal mount) is
// gated to non-touch — the keyboard opens only when the user taps the
// terminal. Toolbar keys send via the raw input channel and never focus.
const isTouchDevice = (): boolean =>
  window.matchMedia?.('(pointer: coarse)').matches || navigator.maxTouchPoints > 0

// Mock mode: hide close buttons and other interactive chrome via CSS.
if (USE_MOCK) document.documentElement.classList.add('mock-mode')

// Debug: __gmuxCopySession() in devtools console
installCopySession()

// Auto-reload when the bundle goes stale relative to the daemon.
// Mock mode is offline-only and the daemon version is fixed, so the
// watcher is pointless there and would risk masking real bugs.
if (!USE_MOCK) installVersionWatch()

// Disable pinch-to-zoom app-wide. This is a terminal, not a document;
// page zoom only breaks the layout. iOS Safari ignores user-scalable=no
// and touch-action for *page* pinch, so the only reliable lever is
// preventing the non-standard gesture events it fires. Harmless on
// browsers that don't emit them.
for (const type of ['gesturestart', 'gesturechange', 'gestureend']) {
  document.addEventListener(type, e => e.preventDefault(), { passive: false })
}

// ── Components ──

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
    <div class={`main-header ${keyboardOpen.value ? 'keyboard-collapsed' : ''}`}>
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
        <SessionMenu session={session} onRestart={onRestart} />
      </div>
    </div>
  )
}

function SessionMenu({ session, onRestart }: {
  session: Session
  onRestart?: () => void
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
const IconSend = () => <svg viewBox="0 0 14 14" width="16" height="16" fill="currentColor" stroke="none"><path d="M3 2.5l8 4.5-8 4.5V8.5L7.5 7 3 5.5z"/></svg>

function MobileTerminalBar({
  canSend,
  ctrlArmed,
  altArmed,
  onMenu,
  onSend,
  onToggleCtrl,
  onToggleAlt,
  onCtrlConsumed,
  onAltConsumed,
}: {
  canSend: boolean
  ctrlArmed: boolean
  altArmed: boolean
  onMenu: () => void
  onSend: (data: string) => void
  onToggleCtrl: () => void
  onToggleAlt: () => void
  onCtrlConsumed: () => void
  onAltConsumed: () => void
}) {
  // Read signals directly; no props needed for these. The hamburger
  // badge surfaces only the waiting (unread) state — working/active are
  // deliberately omitted. unreadCount excludes the selected session and
  // its value re-fires the arrival pulse when another session starts
  // waiting.
  const waitingCount = unreadCount.value
  const waiting = waitingCount > 0
  const arrival = useArrivalPulse(waiting ? 'unread' : 'none', waitingCount)

  // Don't steal focus from the terminal: a control tap leaves the
  // keyboard exactly as it is (open or closed). Bytes reach the PTY via
  // the raw input channel (onSend), independent of DOM focus, so every
  // key works with the keyboard closed without ever opening it — which
  // is the whole point of arrows/esc being reachable while navigating.
  const keepFocus = (ev: Event) => ev.preventDefault()

  // Modifier-aware send: the toolbar writes through the raw input
  // channel (bypassing the arm logic in sendInput), so armed ctrl/alt
  // are encoded here. Consumes whichever arms were actually applied.
  // ctrl+esc / ctrl+↑ / alt+esc all encode correctly via CSI-u.
  const sendKey = (seq: string) => {
    const r = applyArmedModifiers(seq, ctrlArmed, altArmed)
    if (r.ctrlApplied && ctrlArmed) onCtrlConsumed()
    if (r.altApplied) onAltConsumed()
    onSend(r.seq)
  }

  // Word-jump is intrinsically ctrl+arrow. Force ctrl on so it works
  // regardless of armed state, fold in an armed alt (→ ctrl+alt+arrow),
  // and consume both arms so neither leaks to the next key.
  const sendWord = (arrow: string) => {
    const r = applyArmedModifiers(arrow, true, altArmed)
    if (ctrlArmed) onCtrlConsumed()
    if (r.altApplied) onAltConsumed()
    onSend(r.seq)
  }

  // Static two-row grid, identical whether the keyboard is open or
  // closed (no relabeling, no reflow). ☰ and send are double-height
  // bookends; the inner 5×2 grid holds the keys with ↑/↓ stacked into a
  // center D-pad column (← ↓ → across the bottom, word-jumps tucked into
  // the top corners). ctrl/alt only arm + highlight — they never change
  // another key's meaning.
  const armedClass = (armed: boolean) => `mobile-bottom-action${armed ? ' armed' : ''}`

  return (
    <div class="mobile-bottom-bar" aria-label="Mobile terminal controls" onMouseDown={keepFocus}>
      <button
        class={`mobile-bottom-action menu-btn${waiting ? ' bg-waiting' : ''}${arrival ? ` bg-${arrival}` : ''}`}
        onClick={() => {
          // Dismiss the on-screen keyboard when opening the menu. keepFocus
          // holds focus on the textarea through pointerdown, so it's still
          // the active element here; blurring it slides the keyboard away.
          (document.activeElement as HTMLElement | null)?.blur()
          onMenu()
        }}
        title="Open sessions"
      >
        ☰
      </button>

      <div class="mobile-key-grid" role="toolbar" aria-label="Terminal keys">
        {/* Row 1 */}
        <button class="mobile-bottom-action" disabled={!canSend} onClick={() => sendKey('\x1b')} title="Escape">esc</button>
        <button class={armedClass(altArmed)} disabled={!canSend} aria-pressed={altArmed} onClick={onToggleAlt} title={altArmed ? 'Alt armed for next key' : 'Arm Alt'}>alt</button>
        <button class="mobile-bottom-action" disabled={!canSend} onClick={() => sendWord('\x1b[D')} title="Word left"><IconWordLeft /></button>
        <button class="mobile-bottom-action" disabled={!canSend} onClick={() => sendKey('\x1b[A')} title="Up arrow"><IconUp /></button>
        <button class="mobile-bottom-action" disabled={!canSend} onClick={() => sendWord('\x1b[C')} title="Word right"><IconWordRight /></button>
        {/* Row 2 */}
        <button class="mobile-bottom-action" disabled={!canSend} onClick={() => sendKey('\t')} title="Tab">tab</button>
        <button class={armedClass(ctrlArmed)} disabled={!canSend} aria-pressed={ctrlArmed} onClick={onToggleCtrl} title={ctrlArmed ? 'Ctrl armed for next key' : 'Arm Ctrl'}>ctrl</button>
        <button class="mobile-bottom-action" disabled={!canSend} onClick={() => sendKey('\x1b[D')} title="Left arrow"><IconLeft /></button>
        <button class="mobile-bottom-action" disabled={!canSend} onClick={() => sendKey('\x1b[B')} title="Down arrow"><IconDown /></button>
        <button class="mobile-bottom-action" disabled={!canSend} onClick={() => sendKey('\x1b[C')} title="Right arrow"><IconRight /></button>
      </div>

      <button
        class="mobile-bottom-action send-btn"
        disabled={!canSend}
        onClick={() => sendKey('\r')}
        title={altArmed ? 'Send Alt+Enter' : 'Send'}
      ><IconSend /></button>
    </div>
  )
}

// ── App ──

function App() {
  // Visual viewport tracking for keyboard-aware layout. Lives here (not
  // in TerminalView) because viewport occlusion is an app-global fact:
  // App never unmounts, so keyboardOpen can't flash on session switch or
  // navigation, and there's no per-component cleanup to get wrong.
  useEffect(() => {
    const vv = window.visualViewport
    if (!vv) return
    // Keyboard presence = occluded height = layout viewport (innerHeight)
    // minus visual viewport (vv.height). This is only meaningful because
    // the keyboard shrinks the *visual* viewport while the *layout*
    // viewport stays full — which holds on:
    //   - iOS Safari: always (it ignores interactive-widget but already
    //     behaves this way).
    //   - Chrome/Android >=108: because index.html sets the viewport meta
    //     interactive-widget=resizes-visual. That meta is load-bearing
    //     here; without it Chrome's default (resizes-content) shrinks the
    //     layout viewport too, and the difference collapses to ~0.
    // Browsers that ignore the meta and resize the layout viewport read
    // ~0 and so never flip keyboardOpen — a deliberate fail-safe: the
    // header just doesn't collapse, nothing breaks. (We don't support
    // pre-108 Android beyond that.)
    //
    // Deliberately not the VirtualKeyboard API (navigator.virtualKeyboard):
    // it's Chromium-only (no iOS Safari — our primary target — and no
    // Firefox), and its boundingRect/geometrychange only report anything
    // once overlaysContent=true, which stops the browser resizing the
    // viewport and makes the keyboard overlay content instead. That would
    // invert this entire vv-resize model for no gain: we need a boolean,
    // not pixel geometry, and the continuous resize is already free here.
    //
    // The URL bar moves both viewports together, so it nets out and
    // doesn't trip the threshold. Detected via the viewport, never
    // textarea focus, which lies: the textarea can stay focused while the
    // keyboard is dismissed (hardware keyboard, swipe-to-hide), and
    // focus/blur don't track the keyboard's slide. CSS decides whether a
    // collapse actually applies.
    const touch = isTouchDevice()
    const update = () => {
      document.documentElement.style.setProperty('--app-height', `${vv.height}px`)
      if (touch) {
        keyboardOpen.value = window.innerHeight - vv.height > KEYBOARD_PRESENCE_PX
      }
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
    // Derive the query string from loc.url (path+search) so query-only
    // navigations (?project=, ?cwd=) reactively re-filter sessions.
    const q = loc.url.indexOf('?')
    urlSearch.value = q >= 0 ? loc.url.slice(q) : ''
  }, [loc.url])

  // Settings modal is driven by the `?settings` query param rather than
  // local state, so it's deep-linkable and shareable. It's read off the
  // query (not the path), leaving the path-derived `view` untouched —
  // opening settings over a live session keeps the terminal mounted.
  // Open pushes a history entry (back closes); close replaces so the
  // collapsed entry doesn't reopen on a subsequent back.
  const settingsOpen = loc.query.settings !== undefined
  const settingsTab = loc.query.settings ?? 'projects'
  const openSettings = useCallback((tab = 'projects', replace = false) => {
    const params = new URLSearchParams(location.search)
    // Replace (don't push) when the requested tab is already active,
    // so clicking the always-visible gear while the modal is open
    // doesn't stack a duplicate history entry that Back has to clear.
    const alreadyActive = params.get('settings') === tab
    params.set('settings', tab)
    loc.route(`${loc.path}?${params.toString()}`, replace || alreadyActive)
  }, [loc])
  const closeSettings = useCallback(() => {
    const params = new URLSearchParams(location.search)
    params.delete('settings')
    const qs = params.toString()
    loc.route(qs ? `${loc.path}?${qs}` : loc.path, true)
  }, [loc])

  // Initialize the store (SSE, data fetching, effects).
  useEffect(() => initStore(), [])

  // ── Local UI state (not shared, belongs to App) ──
  const [sidebarOpen, setSidebarOpen] = useState(false)
  const [ctrlArmed, setCtrlArmed] = useState(false)
  const [altArmed, setAltArmed] = useState(false)

  const terminalInputRef = useRef<((data: string) => void) | null>(null)
  const terminalFocusRef = useRef<(() => void) | null>(null)
  const terminalPasteRef = useRef<(() => void) | null>(null)

  // Read signals.
  const viewVal = view.value
  const selId = selectedId.value
  const selectedVal = selected.value
  const sessionsVal = sessions.value
  const connVal = connState.value
  const termOpts = terminalOptions.value
  const keybindsVal = keybinds.value
  const macCtrl = macCommandIsCtrl.value

  const { notifPermission, requestNotifPermission } = usePresence()

  // ── Resume ──
  const [resumingId, setResumingId] = useState<string | null>(null)

  const handleCloseSession = useCallback((session: Session) => {
    dismissSession(session.id)
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
    // Don't auto-open the keyboard on touch when switching sessions.
    if (!isTouchDevice()) requestAnimationFrame(() => terminalFocusRef.current?.())
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
    // Auto-focus on mount only off-touch; on touch this would pop the
    // on-screen keyboard the moment a session opens (surprising).
    if (!isTouchDevice()) focus?.()
  }, [])
  const handleMobileInput = useCallback((data: string) => { terminalInputRef.current?.(data) }, [])
  // Paste capability is wired up (TerminalView reports its trigger here)
  // but currently has no UI affordance on mobile: the toolbar paste
  // button was removed in favor of a forthcoming long-press-to-paste on
  // the terminal, which will call terminalPasteRef. Desktop paste still
  // goes through the keyboard handler.
  const handleTerminalPasteReady = useCallback((paste: (() => void) | null) => {
    terminalPasteRef.current = paste
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
        onOpenSettings={() => { setSidebarOpen(false); openSettings() }}
        open={sidebarOpen}
        onClose={() => setSidebarOpen(false)}
      />

      <SettingsModal
        open={settingsOpen}
        tab={settingsTab}
        onClose={closeSettings}
        onSelectTab={(t) => openSettings(t, true)}
      />

      <div class="main-panel">
        {viewVal !== null && viewVal.kind !== 'project' && viewVal.kind !== 'home' && (
          <MainHeader
            session={selectedVal}
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
            projectPeer={viewVal.projectPeer}
            onCloseSession={handleCloseSession}
          />
        ) : selectedVal && (canAttach || USE_MOCK) && termOpts && keybindsVal ? (
          <TerminalView
            session={selectedVal}
            terminalOptions={termOpts}
            keybinds={keybindsVal}
            macCommandIsCtrl={macCtrl}
            ctrlArmed={ctrlArmed}
            onCtrlConsumed={handleCtrlConsumed}
            altArmed={altArmed}
            onAltConsumed={handleAltConsumed}
            onInputReady={handleTerminalInputReady}
            onPasteReady={handleTerminalPasteReady}
            onFocusReady={handleTerminalFocusReady}
          />
        ) : selectedVal && !selectedVal.alive && termOpts && !USE_MOCK ? (
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
          <Home
            onManageProjects={() => openSettings()}
            notifPermission={notifPermission}
            onRequestNotifPermission={requestNotifPermission}
          />
        )}

        <MobileTerminalBar
          canSend={canAttach}
          ctrlArmed={ctrlArmed}
          altArmed={altArmed}
          onMenu={() => setSidebarOpen(true)}
          onSend={handleMobileInput}
          onToggleCtrl={handleToggleCtrl}
          onToggleAlt={handleToggleAlt}
          onCtrlConsumed={handleCtrlConsumed}
          onAltConsumed={handleAltConsumed}
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
