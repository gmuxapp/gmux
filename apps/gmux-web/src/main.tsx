import { Component, Fragment, render, type ComponentChildren } from 'preact'
import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks'
import { LocationProvider, Router, Route, lazy, useLocation } from 'preact-iso'
import '@xterm/xterm/css/xterm.css'
import './styles.css'

import { applyArmedModifiers } from './keyboard'
import { isTouchDevice } from './touch'
import { ReplayView } from './replay-view'
import { TerminalView } from './terminal'
import { useArrivalPulse } from './use-arrival-pulse'
import { Sidebar } from './sidebar'
import { usePresence } from './use-presence'

import type { Session } from './types'
import { SettingsModal } from './settings'
import { ProjectHub } from './project-hub'
import { Home } from './home'
import { installCopySession } from './mock-data/export-session'
import { installVersionWatch } from './version-watch'
import { ToastHost } from './toast-host'
import { pushError } from './toasts'

import {
  sessions, connState, selected, selectedId, view, health, peers,
  terminalOptions, keybinds, macCommandIsCtrl,
  unreadCount, keyboardOpen, terminalFindOpen, terminalScrolledUp, terminalScrollToBottom,
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

// ── Error boundary ──

const GITHUB_ISSUES_URL = 'https://github.com/gmuxapp/gmux/issues/new'
const DISCORD_URL = 'https://discord.gg/Mg6EJHFZxu'

/** Build a copyable crash report. Mirrors input-diagnostics' buildReport:
 *  environment first, then the error + stacks. The `gmux` version comes
 *  from the last health snapshot (may be unknown if we crashed before it
 *  arrived). */
function buildCrashReport(err: unknown, componentStack?: string): string {
  const lines: string[] = []
  lines.push('=== gmux Crash Report ===')
  lines.push(`Date: ${new Date().toISOString()}`)
  lines.push(`gmux: ${health.value?.version ?? 'unknown'}`)
  lines.push(`URL: ${location.pathname}${location.search}`)
  lines.push(`User-Agent: ${navigator.userAgent}`)
  lines.push('')
  lines.push(`Error: ${err instanceof Error ? `${err.name}: ${err.message}` : String(err)}`)
  if (err instanceof Error && err.stack) {
    lines.push('Stack:')
    lines.push(err.stack)
  }
  if (componentStack) {
    lines.push('Component stack:')
    lines.push(componentStack.trim())
  }
  lines.push('=== End Report ===')
  return lines.join('\n')
}

// A thrown render error used to white-screen the whole app with nothing
// in the UI. This catches it (render-phase throws only — async/event
// failures go through toasts instead), shows a recoverable fallback with
// a copyable crash report + report links, and pushes an error toast.
// Preact supports componentDidCatch / getDerivedStateFromError on class
// components.
class ErrorBoundary extends Component<
  { children: ComponentChildren },
  { failed: boolean; report: string; copied: boolean }
> {
  state = { failed: false, report: '', copied: false }

  static getDerivedStateFromError() {
    return { failed: true }
  }

  componentDidCatch(err: unknown, info: { componentStack?: string }) {
    console.error('render error caught by boundary:', err)
    pushError(`Something broke: ${err instanceof Error ? err.message : 'render error'}`)
    this.setState({ report: buildCrashReport(err, info?.componentStack) })
  }

  copyReport = async () => {
    // Clipboard-then-textarea fallback, matching input-diagnostics so the
    // copy works in non-secure contexts / older browsers too.
    try {
      await navigator.clipboard.writeText(this.state.report)
    } catch {
      const ta = document.createElement('textarea')
      ta.value = this.state.report
      document.body.appendChild(ta)
      ta.select()
      document.execCommand('copy')
      document.body.removeChild(ta)
    }
    this.setState({ copied: true })
  }

  render() {
    if (this.state.failed) {
      return (
        <div class="state-message crash-fallback">
          <div class="state-icon" style={{ color: 'var(--status-error)' }}>⚠</div>
          <div class="state-title">Something broke</div>
          <div class="state-subtitle">
            gmux hit an unexpected error and couldn't render this view.
          </div>
          <div class="crash-report-links">
            Please report it so we can fix it:{' '}
            <a href={GITHUB_ISSUES_URL} target="_blank" rel="noreferrer">GitHub Issues</a>
            {' · '}
            <a href={DISCORD_URL} target="_blank" rel="noreferrer">Discord</a>
          </div>
          <pre class="crash-report">{this.state.report}</pre>
          <div class="crash-actions">
            <button class="btn" onClick={this.copyReport}>
              {this.state.copied ? 'Copied!' : 'Copy report'}
            </button>
            <button class="btn btn-primary" onClick={() => location.reload()}>
              Reload
            </button>
          </div>
        </div>
      )
    }
    return this.props.children
  }
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
        {(session.status?.working || session.status?.error) && (
          <div class={`main-header-status ${session.status.error ? 'error' : 'working'}`}>
            <span
              class={`session-dot ${session.status.error ? 'error' : 'working'}`}
              style={{ width: 5, height: 5 }}
            />
            {session.status.error ? 'Error' : 'Working…'}
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

  // Find works whenever the live terminal is mounted, i.e. the session is
  // alive. This is the touch path to the find bar (no hardware keyboard
  // for the secondary+F keybind).
  const canFind = session.alive

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
          {canFind && (
            <button
              class="session-menu-action"
              onClick={() => { setOpen(false); terminalFindOpen.value = true }}
            >
              Find in terminal
            </button>
          )}
          {session.alive && onRestart && (
            <button
              class={`session-menu-action${staleKind ? ' stale' : ''}`}
              onClick={() => { setOpen(false); onRestart!() }}
            >
              Restart session
              {staleKind && <span class="session-menu-action-tag">outdated</span>}
            </button>
          )}
          {(canFind || (session.alive && onRestart)) && <div class="session-menu-divider" />}
          <div class="session-menu-section-title">Session info</div>
          <div class="session-menu-row">
            <span class="session-menu-label">Adapter</span>
            <span class="session-menu-value">{session.adapter}</span>
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
const IconEnd = () => <svg viewBox="0 0 14 14" width="16" height="16" {...S}><path d="M7 2v7m0 0-3-3m3 3 3-3"/><path d="M3.5 12h7"/></svg>

// Press-and-hold auto-repeat for the navigation keys: fire once on press,
// then after a short delay repeat until release. Arrows repeat briskly;
// word-jumps slower, since each hop covers a whole word and a fast rate
// would overshoot. (No native key-repeat reaches these on-screen keys.)
const REPEAT_DELAY_MS = 300
const ARROW_REPEAT_MS = 70
const WORD_REPEAT_MS = 250

/** Returns a factory that builds the press-and-hold pointer handlers for a
 * repeatable key. One timer pair total — only a single key is held at a
 * time on touch — and any running timer is cleared on unmount. */
function useAutoRepeat() {
  const timers = useRef<{ delay?: number; interval?: number }>({})
  const stop = useCallback(() => {
    clearTimeout(timers.current.delay)
    clearInterval(timers.current.interval)
    timers.current = {}
  }, [])
  useEffect(() => stop, [stop])
  return useCallback((fire: () => void, intervalMs: number) => ({
    onPointerDown: (ev: Event) => {
      ev.preventDefault() // act on press; no focus-steal or long-press callout
      stop()              // defensive: never stack onto a lingering hold
      fire()
      timers.current.delay = window.setTimeout(() => {
        timers.current.interval = window.setInterval(fire, intervalMs)
      }, REPEAT_DELAY_MS)
    },
    onPointerUp: stop,
    onPointerLeave: stop,
    onPointerCancel: stop,
  }), [stop])
}

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

  // Arrows and word-jumps key-repeat on hold; the rest fire once per tap.
  const repeat = useAutoRepeat()

  // The bar is a CSS grid laid out via named areas (.mk-* → grid-area), so the
  // DOM order below is only tab/reading order — the visual arrangement lives
  // in styles.css. Narrow phones get a 7×2 grid (empty top-left corner;
  // scroll-end or empty top-right). Wider viewports (landscape / tablets)
  // collapse to a single row, and the widest step folds the word-jumps back
  // in. Keys never relabel; ctrl/alt only arm + highlight.
  const armedClass = (armed: boolean) => `mobile-bottom-action${armed ? ' armed' : ''}`

  return (
    <div class="mobile-bottom-bar" role="toolbar" aria-label="Terminal keys" onMouseDown={keepFocus}>
      <button
        class={`mobile-bottom-action menu-btn mk-menu${waiting ? ' bg-waiting' : ''}${arrival ? ` bg-${arrival}` : ''}`}
        onClick={() => {
          // Dismiss the on-screen keyboard when opening the menu. keepFocus
          // holds focus on the textarea through pointerdown, so it's still
          // the active element here; blurring it slides the keyboard away.
          (document.activeElement as HTMLElement | null)?.blur()
          onMenu()
        }}
        title="Open sessions"
      ><span class="mkey-face">☰</span></button>
      <button class="mobile-bottom-action mk-esc" disabled={!canSend} onClick={() => sendKey('\x1b')} title="Escape"><span class="mkey-face">esc</span></button>
      <button class="mobile-bottom-action mk-tab" disabled={!canSend} onClick={() => sendKey('\t')} title="Tab"><span class="mkey-face">tab</span></button>
      <button class={`${armedClass(ctrlArmed)} mk-ctrl`} disabled={!canSend} aria-pressed={ctrlArmed} onClick={onToggleCtrl} title={ctrlArmed ? 'Ctrl armed for next key' : 'Arm Ctrl'}><span class="mkey-face">ctrl</span></button>
      <button class={`${armedClass(altArmed)} mk-alt`} disabled={!canSend} aria-pressed={altArmed} onClick={onToggleAlt} title={altArmed ? 'Alt armed for next key' : 'Arm Alt'}><span class="mkey-face">alt</span></button>
      <button class="mobile-bottom-action mk-wl" disabled={!canSend} {...repeat(() => sendWord('\x1b[D'), WORD_REPEAT_MS)} title="Word left"><span class="mkey-face"><IconWordLeft /></span></button>
      <button class="mobile-bottom-action mk-al" disabled={!canSend} {...repeat(() => sendKey('\x1b[D'), ARROW_REPEAT_MS)} title="Left arrow"><span class="mkey-face"><IconLeft /></span></button>
      <button class="mobile-bottom-action mk-au" disabled={!canSend} {...repeat(() => sendKey('\x1b[A'), ARROW_REPEAT_MS)} title="Up arrow"><span class="mkey-face"><IconUp /></span></button>
      <button class="mobile-bottom-action mk-ad" disabled={!canSend} {...repeat(() => sendKey('\x1b[B'), ARROW_REPEAT_MS)} title="Down arrow"><span class="mkey-face"><IconDown /></span></button>
      <button class="mobile-bottom-action mk-ar" disabled={!canSend} {...repeat(() => sendKey('\x1b[C'), ARROW_REPEAT_MS)} title="Right arrow"><span class="mkey-face"><IconRight /></span></button>
      <button class="mobile-bottom-action mk-wr" disabled={!canSend} {...repeat(() => sendWord('\x1b[C'), WORD_REPEAT_MS)} title="Word right"><span class="mkey-face"><IconWordRight /></span></button>
      {terminalScrolledUp.value && (
        <button class="mobile-bottom-action mk-end" onClick={() => terminalScrollToBottom.value?.()} title="Scroll to bottom"><span class="mkey-face"><IconEnd /></span></button>
      )}
      <button class="mobile-bottom-action send-btn mk-send" disabled={!canSend} onClick={() => sendKey('\r')} title={altArmed ? 'Send Alt+Enter' : 'Send'}><span class="mkey-face"><IconSend /></span></button>
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
    // resumeSession never rejects (postAction converts failures to false
    // and surfaces the toast itself); branch on the boolean to clear the
    // "resuming…" spinner immediately on rejection instead of letting it
    // linger until the 10s timeout.
    void resumeSession(id).then(ok => {
      if (!ok) setResumingId(prev => prev === id ? null : prev)
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
            onRestart={selectedVal ? () => { void restartSession(selectedVal.id) } : undefined}
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

        {selectedVal && (canAttach || USE_MOCK) && termOpts && keybindsVal && (
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
        )}
      </div>
    </div>
  )
}

render(
  // ToastHost is a *sibling* of the boundary, not a child: when the
  // boundary fires it unmounts its children, so a ToastHost nested
  // inside would be gone exactly when componentDidCatch pushes its
  // error toast (the signal would update with no mounted consumer).
  // Keeping it outside means toasts survive an app crash.
  <Fragment>
    <ErrorBoundary>
      <LocationProvider>
        <Router>
          <Route path="/_/input-diagnostics" component={InputDiagnostics} />
          <Route default component={App} />
        </Router>
      </LocationProvider>
    </ErrorBoundary>
    <ToastHost />
  </Fragment>,
  document.getElementById('app')!,
)
