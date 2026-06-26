// Launcher UI.
//
// Pure functions for launcher resolution + the LaunchButton component.
// Launcher definitions come from the store's health signal (populated
// from /v1/health). The component reads them reactively.

import { useState, useRef, useEffect, useMemo } from 'preact/hooks'
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
 * offset so the first adapter stays aligned.
 */
export function computeMenuPos(
  rect: Rect,
  viewport: Viewport,
  showTarget: boolean,
  menuWidth = 180,
): { top: number; left: number } {
  const targetOffset = showTarget ? 32 : 0
  const top = rect.top - 4 - targetOffset // 4px = menu padding-top

  let left = rect.left - MENU_LEFT_OFFSET
  // Clamp right edge inside the viewport...
  const maxLeft = viewport.innerWidth - MENU_VIEWPORT_MARGIN - menuWidth
  if (left > maxLeft) left = maxLeft
  // ...then clamp left edge (wins if the menu is wider than the viewport).
  if (left < MENU_VIEWPORT_MARGIN) left = MENU_VIEWPORT_MARGIN

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

  // Read launcher config from the store (populated by /v1/health).
  const hasLaunchers = launchersSignal.value.length > 0

  /** Position the menu (fixed, so it escapes overflow:hidden parents). */
  const positionMenu = () => {
    const btn = btnRef.current
    if (!btn) return
    const r = btn.getBoundingClientRect()
    setMenuPos(computeMenuPos(r, { innerWidth: window.innerWidth }, showTarget))
  }

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
      {isOpen && menuPos && (
        <div
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
        </div>
      )}
    </div>
  )
}
