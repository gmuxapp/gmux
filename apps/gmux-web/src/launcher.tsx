// Launcher UI.
//
// Pure functions for launcher resolution + the LaunchButton component.
// Launcher definitions come from the store's health signal (populated
// from /v1/health). The component reads them reactively.

import { useState, useRef, useEffect, useLayoutEffect, useMemo } from 'preact/hooks'
import { createPortal } from 'preact/compat'
import type { LauncherDef, PeerInfo } from './types'
import { launchers as launchersSignal, defaultLauncher as defaultLauncherSignal, peers as peersSignal, launchSession } from './store'

/** Resolved launch target: where the session will be created. */
export interface LaunchTarget {
  peer?: string
  cwd: string
}

/** Return launchers for a specific peer, falling back to local config. */
export function launchersForPeer(
  localLaunchers: LauncherDef[],
  localDefault: string,
  peers: PeerInfo[],
  peer: string | undefined,
): { default_launcher: string; launchers: LauncherDef[] } {
  if (peer) {
    const p = peers.find(pp => pp.name === peer)
    if (p?.launchers?.length) {
      return { default_launcher: p.default_launcher ?? localDefault, launchers: p.launchers }
    }
  }
  return { default_launcher: localDefault, launchers: localLaunchers }
}

/** Format a target for display: "~/dev/gmux" or "laptop: ~/dev/gmux". */
export function formatTarget(target: LaunchTarget): string {
  const shortCwd = target.cwd.replace(/^\/home\/[^/]+/, '~')
  if (target.peer) return `${target.peer}: ${shortCwd}`
  return shortCwd
}

// ── Menu positioning ──

/** A button rect (the subset of DOMRect we need). */
export interface Rect {
  top: number
  left: number
}

/** Viewport bounds for clamping. */
export interface Viewport {
  innerWidth: number
}

/** Margin kept between the menu and the viewport edges (px). */
const MENU_VIEWPORT_MARGIN = 8

/**
 * Clamp a menu's left coordinate so a menu of `width` stays inside the
 * viewport: right edge first, then left edge (which wins if the menu is
 * wider than the viewport).
 */
export function clampMenuLeft(left: number, width: number, innerWidth: number): number {
  const maxLeft = innerWidth - MENU_VIEWPORT_MARGIN - width
  if (left > maxLeft) left = maxLeft
  if (left < MENU_VIEWPORT_MARGIN) left = MENU_VIEWPORT_MARGIN
  return left
}
/** How far left of the button the menu's left edge sits (px). */
const MENU_LEFT_OFFSET = 6

/**
 * Compute the fixed position for the launch menu.
 *
 * Horizontal: anchor the menu's LEFT edge slightly left of the button so the
 * text appears right where the user just clicked, growing rightward, clamped
 * to the viewport so it never overflows either edge.
 *
 * Vertical: lift the menu so its default item (first adapter) lands directly
 * under the button — enabling open-then-double-click on the default. The menu
 * has 4px padding-top; a shown target line adds ~32px (line + divider) that we
 * offset so the first adapter stays aligned. The lift is clamped so the menu
 * never pokes above the viewport (e.g. a button in the very first sidebar row
 * on mobile, where the sidebar has no header above the project list).
 */
export function computeMenuPos(
  rect: Rect,
  viewport: Viewport,
  showTarget: boolean,
  menuWidth = 180,
): { top: number; left: number } {
  const targetOffset = showTarget ? 32 : 0
  let top = rect.top - 4 - targetOffset // 4px = menu padding-top
  if (top < MENU_VIEWPORT_MARGIN) top = MENU_VIEWPORT_MARGIN

  const left = clampMenuLeft(rect.left - MENU_LEFT_OFFSET, menuWidth, viewport.innerWidth)

  return { top, left }
}

// ── LaunchButton ──
//
// Transforms into an inline menu on click:
//
//   Idle:      [+]
//   Open:      target context line + adapter list
//   Launching: [spinner]
//
// Double-click works because the default adapter occupies the exact same
// position as the + button. First click opens, second click hits default.
//
// The launch target is explicit: callers pass `cwd` (the project's
// canonical dir) and optional `peer`. The button never derives a cwd
// from session context.

interface LaunchButtonProps {
  className?: string
  onLaunch?: () => void
  /** Async action to run before the launch request (e.g. seed a project). */
  beforeLaunch?: () => Promise<void>
  /** Working directory for the new session (the project's canonical dir). */
  cwd?: string
  /** Peer name for a remote launch; authoritative for the target's host. */
  peer?: string
}

export function LaunchButton({ className, onLaunch, beforeLaunch, cwd, peer }: LaunchButtonProps) {
  const target = useMemo((): LaunchTarget => ({ peer, cwd: cwd || '' }), [peer, cwd])

  const showTarget = target.cwd !== ''

  const [state, setState] = useState<'idle' | 'open' | 'launching'>('idle')
  const [menuPos, setMenuPos] = useState<{ top: number; left: number } | null>(null)
  const containerRef = useRef<HTMLDivElement>(null)
  const btnRef = useRef<HTMLButtonElement>(null)
  const menuRef = useRef<HTMLDivElement>(null)

  // Read launcher config from the store (populated by /v1/health).
  const hasLaunchers = launchersSignal.value.length > 0

  /** Position the menu (fixed, so it escapes overflow:hidden parents). */
  const positionMenu = () => {
    const btn = btnRef.current
    if (!btn) return
    const r = btn.getBoundingClientRect()
    setMenuPos(computeMenuPos(r, { innerWidth: window.innerWidth }, showTarget))
  }

  // computeMenuPos clamps with the menu's *minimum* width (180px); nowrap
  // items can render it wider, overflowing the right edge on narrow screens.
  // Re-clamp with the real width once the menu is in the DOM — layout effect,
  // so the correction lands in the same paint as the initial position.
  useLayoutEffect(() => {
    if (state !== 'open' || !menuRef.current || !menuPos) return
    const left = clampMenuLeft(menuPos.left, menuRef.current.offsetWidth, window.innerWidth)
    if (left !== menuPos.left) setMenuPos({ top: menuPos.top, left })
  }, [state, menuPos])

  const handleClick = (e: MouseEvent) => {
    e.stopPropagation()
    if (state === 'idle') {
      positionMenu()
      setState('open')
    } else if (state === 'open') {
      setState('idle')
    }
  }

  const handleLaunch = async (id: string) => {
    setState('launching')
    if (beforeLaunch) await beforeLaunch()
    onLaunch?.()
    launchSession(id, { cwd: target.cwd || undefined, peer: target.peer }).finally(() => {
      setTimeout(() => setState('idle'), 600)
    })
  }

  // Close on outside click
  useEffect(() => {
    if (state !== 'open') return
    const handler = (e: MouseEvent) => {
      const t = e.target as Node
      if (containerRef.current?.contains(t)) return
      if (menuRef.current?.contains(t)) return
      setState('idle')
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

  const isOpen = state === 'open' && hasLaunchers
  const isLoading = state === 'launching'

  let defLauncher: LauncherDef | undefined
  let others: LauncherDef[] = []
  if (isOpen) {
    const resolved = launchersForPeer(
      launchersSignal.value, defaultLauncherSignal.value,
      peersSignal.value, target.peer,
    )
    defLauncher = resolved.launchers.find(l => l.id === resolved.default_launcher)
    others = resolved.launchers.filter(l => l.id !== resolved.default_launcher)
  }

  return (
    <div class={`launch-container ${className ?? ''} ${isOpen ? 'open' : ''}`} ref={containerRef}>
      <button
        ref={btnRef}
        class={`launch-btn ${isLoading ? 'loading' : ''}`}
        title={target.cwd
          ? target.peer ? `New session on ${target.peer} in ${target.cwd}` : `New session in ${target.cwd}`
          : 'New session'}
        onClick={handleClick}
      >
        {isLoading ? (
          <svg viewBox="0 0 16 16" width="14" height="14" class="spin">
            <circle cx="8" cy="8" r="6" fill="none" stroke="currentColor"
              stroke-width="2" stroke-dasharray="28" stroke-dashoffset="8" stroke-linecap="round" />
          </svg>
        ) : '+'}
      </button>
      {/* Portaled to <body>: on mobile the sidebar is a transformed drawer
          (translateX), which makes it the containing block for fixed
          descendants — rendered in place the menu would be positioned
          relative to the sidebar and clipped at its edge. The portal keeps
          the fixed coords viewport-relative on every layout. */}
      {isOpen && menuPos && createPortal(
        <div
          ref={menuRef}
          class="launch-inline-menu"
          style={{ top: menuPos.top, left: menuPos.left }}
        >
          {showTarget && (
            <>
              <div class="launch-target-line">{formatTarget(target)}</div>
              <div class="launch-inline-divider" />
            </>
          )}
          {defLauncher && (
            <button
              class="launch-inline-item launch-inline-default"
              onClick={(e) => { e.stopPropagation(); handleLaunch(defLauncher!.id) }}
            >
              {defLauncher.label}
            </button>
          )}
          {others.map((l) => (
            <button
              key={l.id}
              class="launch-inline-item"
              onClick={(e) => { e.stopPropagation(); handleLaunch(l.id) }}
            >
              {l.label}
            </button>
          ))}
        </div>,
        document.body,
      )}
    </div>
  )
}
