import { Component, Fragment, render, type ComponentChildren } from 'preact'
import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks'
import { LocationProvider, Router, Route, lazy, useLocation } from 'preact-iso'
import { batch } from '@preact/signals'
import '@xterm/xterm/css/xterm.css'
import './styles.css'

import { applyArmedModifiers } from './keyboard'
import { keyComboToSequence } from './config'
import { isTouchDevice } from './touch'
import { ReplayView } from './replay-view'
import { TerminalView } from './terminal'
import { Sidebar } from './sidebar'
import { usePresence } from './use-presence'
import { lifecycleAction } from './session-actions'
import { MenuButton } from './menu-button'
import { FamilyDrawer } from './family-drawer'
import { familyAncestors, familyRoot, familySegments, hasFamily, promotionAction, promotionCopy } from './family'
import { FamilyIcon } from './family-icon'

import type { Session } from './types'
import { SettingsModal } from './settings'
import { Home } from './home'
import { installCopySession } from './mock-data/export-session'
import { installVersionWatch } from './version-watch'
import { ToastHost } from './toast-host'
import { pushError } from './toasts'

import {
  sessions, connState, selected, selectedId, view, health, peers, projects,
  terminalOptions, keybinds, macCommandIsCtrl,
  keyboardOpen, terminalFindOpen, terminalScrolledUp, terminalScrollToBottom,
  urlPath, urlSearch, urlHash,
  initStore, setNavigate, navigate, navigateToSession,
  dismissSession, resumeSession, restartSession, promoteSession, demoteSession,
  promotionPending, beginPromotion, settlePromotion, promotionAnnouncements,
  acknowledgePromotionAnnouncement, isPromotionAnnouncementDelivered,
  sessionStaleness, sessionDotState, activityMap, familyActivityById, tabHref,
} from './store'
import { viewToPath } from './routing'

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
// Debug: whole store namespace for console poking in mock mode.
if (USE_MOCK) {
  import('./store').then(m => { (window as unknown as Record<string, unknown>).__store = m })
}

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

function MainHeader({ session, onRestart, onResume, resuming }: {
  session: Session | null
  onRestart?: () => void
  onResume?: (id: string) => void
  resuming?: boolean
}) {
  const [familyOpen, setFamilyOpen] = useState(false)
  const familyTriggerRef = useRef<HTMLButtonElement>(null)
  const closeFamily = useCallback(() => { setFamilyOpen(false) }, [])
  const showFamily = session ? hasFamily(session, sessions.value) : false
  useEffect(() => {
    if (!showFamily) setFamilyOpen(false)
  }, [showFamily])

  if (!session) {
    return (
      <div class="main-header">
        <div class="main-header-title" style={{ color: 'var(--text-muted)' }}>
          gmux
        </div>
      </div>
    )
  }

  return (
    <div class={`main-header ${keyboardOpen.value ? 'keyboard-collapsed' : ''}`}>
      <div class="main-header-left">
        {showFamily && (
          <FamilyTrigger
            session={session}
            open={familyOpen}
            triggerRef={familyTriggerRef}
            onToggle={() => { setFamilyOpen(v => !v) }}
          />
        )}
        {showFamily && <HeaderCrumbs session={session} />}
        <div class="main-header-title">
          {session.title}
        </div>
      </div>
      <div class="main-header-right">
        <HeaderStatusChip session={session} resuming={resuming} />
        <SessionMenu session={session} onRestart={onRestart} onResume={onResume} resuming={resuming} />
      </div>
      {showFamily && familyOpen && (
        <FamilyDrawer selected={session} onClose={closeFamily} triggerRef={familyTriggerRef} />
      )}
    </div>
  )
}

/** The family panel's trigger: a pill wearing the standard family
 * segments — `familySegments` over `familyActivityById`, the same
 * derivation and display order as the sidebar's line and the panel's
 * tally, so the same dots never wear different numbers or a different
 * order anywhere. Nothing here depends on which session you are
 * viewing: the count is a fact about the family, not the viewport. A
 * family with nothing to report shows the tree icon instead; no count
 * with it, because the segments' numbers are news and a quiet family
 * has none. */
function FamilyTrigger({ session, open, triggerRef, onToggle }: {
  session: Session
  open: boolean
  triggerRef: { current: HTMLButtonElement | null }
  onToggle: () => void
}) {
  const rootId = familyRoot(session, sessions.value).id
  const segments = familySegments(familyActivityById.value.get(rootId))
  return (
    <button
      ref={triggerRef}
      class="family-trigger"
      type="button"
      aria-label="Session family"
      title="Session family"
      aria-expanded={open}
      aria-controls="agent-family-drawer"
      onClick={onToggle}
    >
      {segments.length > 0
        ? segments.map(segment => (
          <span key={segment.state} class="family-trigger-seg">
            {segment.dot
              ? <span class={`session-dot-indicator ${segment.dot}`} aria-hidden="true" />
              : <span class="family-trigger-proc" aria-hidden="true">$</span>}
            {segment.count}
          </span>
        ))
        : <FamilyIcon class="family-trigger-icon" />}
    </button>
  )
}

/** Ancestor breadcrumbs in the title row: `●root › ●parent › title`. Each
 * crumb is a live-dotted ghost link; the current session is the plain bold
 * title that follows (its state lives in the status chip). Depth > 3
 * collapses the middle to a static `…` — the panel still shows the full
 * structure. */
function HeaderCrumbs({ session }: { session: Session }) {
  const ancestors = familyAncestors(session, sessions.value)
  if (ancestors.length === 0) return null
  const shown: (Session | null)[] = ancestors.length > 3
    ? [ancestors[0], null, ancestors[ancestors.length - 1]]
    : [...ancestors]
  const am = activityMap.value
  return (
    <nav class="header-crumbs" aria-label="Ancestor agents">
      {shown.map((ancestor) => {
        const dot = ancestor ? sessionDotState(ancestor, am) : 'none'
        return (
          <Fragment key={ancestor?.id ?? 'gap'}>
            {ancestor
              ? (
                <a class="header-crumb" href={sessionHref(ancestor)}>
                  {/* The sidebar's `none` dot is an invisible placeholder
                    * that holds a column; a crumb has no column, so a
                    * quiet ancestor would just wear a permanent hole
                    * where its dot should be. */}
                  {dot !== 'none' && <span class={`session-dot-indicator ${dot}`} aria-hidden="true" />}
                  <span class="header-crumb-title">{ancestor.title}</span>
                </a>
              )
              : <span class="header-crumb-gap" title="More ancestors in the family panel">…</span>}
            <span class="header-crumb-sep" aria-hidden="true">›</span>
          </Fragment>
        )
      })}
    </nav>
  )
}

function sessionHref(session: Session): string | undefined {
  const path = viewToPath({ kind: 'session', sessionId: session.id }, projects.value, sessions.value)
  return path ? tabHref(path) : undefined
}

/** Session status in the header — only states that need the header to
 * say something the view doesn't already: Error, and the resume-in-
 * flight busy label (the menu closes on click, so this chip is the
 * visible feedback until the session comes alive). A dead session gets
 * no chip: a replay view is unmistakably an ended session, and the chip
 * only restated it. */
function HeaderStatusChip({ session, resuming }: {
  session: Session
  resuming?: boolean
}) {
  if (!session.alive) {
    if (resuming) {
      return (
        <div class="main-header-status working">
          <span class="session-dot working" style={{ width: 5, height: 5 }} />
          {lifecycleAction(session, true)?.label ?? 'Resuming…'}
        </div>
      )
    }
    return null
  }
  // A live "Working…" chip is redundant with the session's own dot
  // indicator, so only the error state earns a header chip here.
  if (!session.status?.error) return null
  return (
    <div class="main-header-status error">
      <span class="session-dot error" style={{ width: 5, height: 5 }} />
      Error
    </div>
  )
}

function SessionMenu({ session, onRestart, onResume, resuming }: {
  session: Session
  onRestart?: () => void
  onResume?: (id: string) => void
  resuming?: boolean
}) {
  const [open, setOpen] = useState(false)
  const menuRef = useRef<HTMLDivElement>(null)
  const triggerRef = useRef<HTMLButtonElement>(null)
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

  // Close on outside press or Escape. pointerdown rather than mousedown:
  // the terminal cancels the synthesized mouse cascade on its touch
  // gestures, so a tap into the terminal would otherwise leave this menu
  // open on top of it (see FamilyDrawer for the same fix).
  //
  // useLayoutEffect, not useEffect: a passive effect is deferred to a later
  // task, so a fast keyboard user's very first Escape after opening could
  // arrive before the listener existed and leave the menu stranded open.
  // The layout effect attaches synchronously with the commit that mounts
  // the dropdown, so no keystroke can slip through the gap.
  useLayoutEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return
      setOpen(false)
      // A keyboard user may have tabbed into the dropdown; closing unmounts
      // the focused item and would strand focus on <body>. Same convention
      // as FamilyDrawer's Escape: hand focus back to the trigger.
      triggerRef.current?.focus()
    }
    const onPointerDown = (e: PointerEvent) => {
      if (menuRef.current && !menuRef.current.contains(e.target as Node)) setOpen(false)
    }
    document.addEventListener('keydown', onKey)
    document.addEventListener('pointerdown', onPointerDown)
    return () => {
      document.removeEventListener('keydown', onKey)
      document.removeEventListener('pointerdown', onPointerDown)
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

  // One lifecycle action per state (Restart alive, Resume/Rerun dead).
  // The menu is where muscle memory expects it in both states; for dead
  // sessions the same action is deliberately mirrored as the primary
  // button in ReplayView's action bar.
  const action = lifecycleAction(session, resuming)
  const actionHandler = action?.id === 'restart' ? onRestart
    : action?.id === 'resume' && onResume ? () => onResume(session.id)
    : undefined
  const showStale = action?.id === 'restart' && !!staleKind

  // Presentation promotion (ADR 0026 §8). Eligibility mirrors the family
  // projection's own edge rule and is null for peer-projected sessions, so
  // the menu never offers a mutation the daemon would refuse. The `⋮` menu
  // is deliberately the only surface carrying these verbs: it exists for
  // every session view (desktop and mobile, alive and dead), and a promoted
  // session — no longer a family member — has no family panel to demote from.
  const promotion = promotionAction(session, sessions.value, projects.value)
  // In-flight guard: the menu closes on activation, but reopening it before
  // the authoritative snapshot lands would offer the same verb again — a
  // second POST per click (reproduced in e2e). The entry lives in the
  // module-level map (survives navigation), is keyed by session, and speaks
  // only for the action kind it started: once the snapshot flips the offered
  // action, the entry is spent. Failures settle their own generation — and
  // nobody else's — re-arming the item beside the failure toast.
  const pendingEntry = promotionPending.value.get(session.id)
  const promotionInFlight = !!promotion && pendingEntry?.kind === promotion.kind
  const promotionBlocked = promotion?.blocked !== undefined
  const promotionWords = promotion ? promotionCopy(promotion, promotionInFlight) : null

  return (
    <div class="session-menu" ref={menuRef}>
      <button
        ref={triggerRef}
        class={`session-menu-trigger${staleKind ? ' stale' : ''}`}
        onClick={() => setOpen(!open)}
        title="Session actions"
        // The visible content is a bare "⋮" glyph, which is also what a
        // screen reader would announce without this (every other icon
        // button in the app carries its name the same way).
        aria-label="Session actions"
        aria-expanded={open}
        aria-controls="session-menu-dropdown"
      >
        <span class="session-menu-icon">⋮</span>
        {staleKind && <span class="session-menu-badge" />}
      </button>
      {/* Pending/success status for promote/demote. Lives outside the
        * dropdown so it survives the close-on-activation, and is sr-only:
        * sighted feedback is the sidebar row moving / the reopened item's
        * busy label. Failures are deliberately absent — the error toast's
        * live region already announces those once. */}
      {open && (
        <div class="session-menu-dropdown" id="session-menu-dropdown">
          {canFind && (
            <button
              class="session-menu-action"
              onClick={() => { setOpen(false); terminalFindOpen.value = true }}
            >
              Find in terminal
            </button>
          )}
          {action && actionHandler && (
            <button
              class={`session-menu-action${showStale ? ' stale' : ''}`}
              disabled={action.disabled}
              onClick={() => { setOpen(false); actionHandler() }}
            >
              {action.label}
              {showStale && <span class="session-menu-action-tag">outdated</span>}
            </button>
          )}
          {promotion && promotionWords && (
            <button
              class="session-menu-action session-menu-promotion"
              disabled={promotionInFlight}
              aria-disabled={promotionBlocked ? 'true' : undefined}
              aria-describedby="session-promotion-note"
              onClick={e => {
                if (promotionInFlight || promotionBlocked) {
                  e.preventDefault()
                  return
                }
                setOpen(false)
                // Focus back to the trigger: the activated item unmounts with
                // the dropdown, and a keyboard user shouldn't land on <body>.
                triggerRef.current?.focus()
                const id = session.id
                const seq = beginPromotion(id, promotion.kind)
                void (promotion.kind === 'promote'
                  ? promoteSession(id)
                  : demoteSession(id)
                ).then(ok => {
                  // Rejection re-arms exactly the entry this request created
                  // (generation-checked), beside its failure toast; success
                  // leaves the entry until the snapshot's kind flip spends it.
                  if (!ok) settlePromotion(id, seq)
                })
              }}
            >
              {promotionWords.label}
              <span id="session-promotion-note" class="session-menu-action-note">{promotionWords.note}</span>
            </button>
          )}
          {(canFind || (action && actionHandler) || promotion) && <div class="session-menu-divider" />}
          <div class="session-menu-section-title">Session info</div>
          <div class="session-menu-row">
            <span class="session-menu-label">Adapter</span>
            <span class="session-menu-value">
              {session.adapter}
              {session.drive_mode === 'acp' ? ' (acp)' : ''}
            </span>
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

const IconBackTab  = () => <svg viewBox="0 0 24 18" width="22" height="16" {...S}><path d="M5 6V2m0 0L3 4m2-2 2 2"/><line x1="10" y1="7" x2="10" y2="15"/><path d="M19 11h-6m0 0 3-3m-3 3 3 3"/></svg>
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
  // in styles.css. Narrow phones get a 7×2 grid (Esc sits above the hamburger;
  // scroll-end or empty top-right). Wider viewports (landscape / tablets)
  // collapse to a single row, and the widest step folds the word-jumps back
  // in. Keys never relabel; ctrl/alt only arm + highlight.
  const armedClass = (armed: boolean) => `mobile-bottom-action${armed ? ' armed' : ''}`

  return (
    <div class="mobile-bottom-bar" role="toolbar" aria-label="Terminal keys" onMouseDown={keepFocus}>
      <MenuButton variant="bar" onMenu={onMenu} />
      <button class="mobile-bottom-action mk-esc" disabled={!canSend} aria-label="Escape" onClick={() => sendKey('\x1b')} title="Escape"><span class="mkey-face">esc</span></button>
      <button class="mobile-bottom-action mk-shift-tab" disabled={!canSend} aria-label="Shift+Tab (BackTab)" onClick={() => sendKey(keyComboToSequence('shift+tab'))} title="Shift+Tab (BackTab)"><span class="mkey-face"><IconBackTab /></span></button>
      <button class="mobile-bottom-action mk-tab" disabled={!canSend} aria-label="Tab" onClick={() => sendKey('\t')} title="Tab"><span class="mkey-face">tab</span></button>
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

// ── Promotion status ──

/** Stable announcement host: unlike SessionMenu, this survives the selected
 * session's route/header remount during a promote or demote snapshot. The
 * store owns reconciliation; this component owns only one screen-reader
 * delivery, keyed by session and request token. */
function PromotionAnnouncementHost() {
  const id = selectedId.value
  const pending = id ? promotionPending.value.get(id) : undefined
  const announcement = id ? promotionAnnouncements.value.get(id) : undefined
  const [message, setMessage] = useState('')
  const lastSessionIdRef = useRef<string | null>(null)

  useEffect(() => {
    if (!id) {
      // URL replacement can transiently clear selectedId while the session
      // remains the same. Keep that identity so its already-delivered result
      // can survive the route remount without being re-announced.
      setMessage('')
      return
    }
    const sameSession = lastSessionIdRef.current === id
    lastSessionIdRef.current = id
    if (pending) {
      setMessage(pending.kind === 'promote' ? 'Promoting to root…' : 'Returning to family…')
      return
    }
    if (announcement) {
      const delivered = isPromotionAnnouncementDelivered(id, announcement.seq)
      if (!delivered || sameSession) setMessage(announcement.message)
      if (!delivered) acknowledgePromotionAnnouncement(id, announcement.seq)
      return
    }
    setMessage('')
  }, [id, pending?.kind, pending?.seq, announcement?.seq])

  return (
    <span class="sr-only" data-promotion-status role="status" aria-live="polite" aria-atomic="true">
      {message}
    </span>
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
    // Publish one coherent location snapshot so repair effects cannot run
    // against a new path with the previous entry's query or fragment. Read
    // search/hash from the browser's parsed location: preact-iso's `loc.url`
    // is not consistent about retaining fragments across initial and SPA
    // navigation, and fragments must never be fed to URLSearchParams.
    batch(() => {
      urlPath.value = loc.path
      urlSearch.value = location.search
      urlHash.value = location.hash
    })
  }, [loc.url])

  // Preact-iso routes path/query changes but deliberately ignores hash-only
  // navigation. Keep the fragment signal current for native hash links and
  // hash-only Back/Forward entries as well.
  useEffect(() => {
    const syncHash = () => { urlHash.value = location.hash }
    window.addEventListener('hashchange', syncHash)
    return () => window.removeEventListener('hashchange', syncHash)
  }, [])

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
    navigate(`${loc.path}?${params.toString()}${location.hash}`, replace || alreadyActive)
  }, [loc])
  const closeSettings = useCallback(() => {
    const params = new URLSearchParams(location.search)
    params.delete('settings')
    const qs = params.toString()
    navigate((qs ? `${loc.path}?${qs}` : loc.path) + location.hash, true)
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

  const openSidebar = useCallback(() => setSidebarOpen(true), [])

  // The key bar only accompanies an attached (or mock) terminal; dead
  // sessions carry ☰ in ReplayView's action bar instead of a full set of
  // disabled keys. Everything else (home, transient states)
  // gets the floating ☰ so the sidebar overlay stays reachable on touch.
  const keyBarShown = !!selectedVal && (canAttach || USE_MOCK) && !!termOpts && !!keybindsVal
  const replayShown = !keyBarShown && !!selectedVal && !selectedVal.alive && !!termOpts && !USE_MOCK

  // App-level reconnecting cue for the SSE control-plane. Distinct from
  // the per-terminal WS pill: it covers the sidebar / home / project
  // views where no terminal WS is active. When a terminal *is* attached
  // its own "Connection lost, reconnecting…" pill already owns the
  // offline cue for that view, so we suppress this one to avoid a
  // doubled-up message on the same screen.
  const showReconnecting = connVal === 'reconnecting' && !(selectedVal && canAttach)

  return (
    <div class="app-layout">
      {showReconnecting && (
        <div class="reconnecting-pill app-reconnecting-pill" role="status">
          Connection lost, reconnecting…
        </div>
      )}
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
        {viewVal !== null && viewVal.kind !== 'home' && (
          <MainHeader
            session={selectedVal}
            onRestart={selectedVal ? () => { void restartSession(selectedVal.id) } : undefined}
            onResume={handleResume}
            resuming={!!selectedVal && resumingId === selectedVal.id}
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
            onMenu={openSidebar}
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

        {keyBarShown && (
          <MobileTerminalBar
            canSend={canAttach || USE_MOCK}
            ctrlArmed={ctrlArmed}
            altArmed={altArmed}
            onMenu={openSidebar}
            onSend={handleMobileInput}
            onToggleCtrl={handleToggleCtrl}
            onToggleAlt={handleToggleAlt}
            onCtrlConsumed={handleCtrlConsumed}
            onAltConsumed={handleAltConsumed}
          />
        )}

        {/* On touch the sidebar is an off-canvas overlay, so every screen
            must carry a ☰ somewhere. The key bar and ReplayView's action
            bar have their own; everything else (home,
            connecting/error states) gets the floating one. Hidden on fine
            pointers via CSS. */}
        {!keyBarShown && !replayShown && (
          <MenuButton variant="floating" onMenu={openSidebar} />
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
    <PromotionAnnouncementHost />
  </Fragment>,
  document.getElementById('app')!,
)
