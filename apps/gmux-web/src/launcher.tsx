// Launcher UI.
//
// Pure functions for launcher resolution + the LaunchButton component.
// Launcher definitions come from the store's health signal (populated
// from /v1/health). The component reads them reactively.

import { useState, useRef, useEffect, useMemo } from 'preact/hooks'
import type { Session, LauncherDef, PeerInfo } from './types'
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

// ── Target resolution ──

/**
 * Derive the launch target from the user's current context.
 *
 * Priority:
 *  1. The currently selected session (if it belongs to this project)
 *  2. The most recently created alive session in the project
 *  3. The project's configured fallback path (local)
 */
export function resolveTarget(
  sessions: Session[],
  selectedId: string | null,
  fallbackCwd: string,
): LaunchTarget {
  // 1. Selected session in this project?
  if (selectedId) {
    const selected = sessions.find(s => s.id === selectedId)
    if (selected) return { peer: selected.peer, cwd: selected.cwd }
  }

  // 2. Most recently created alive session?
  const alive = sessions
    .filter(s => s.alive)
    .sort((a, b) => new Date(b.created_at).getTime() - new Date(a.created_at).getTime())
  if (alive.length > 0) return { peer: alive[0].peer, cwd: alive[0].cwd }

  // 3. Fallback to the project's configured path, local.
  return { cwd: fallbackCwd }
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
// Two modes:
//  - Explicit target: `cwd` and optional `peer` passed directly (used by
//    project hub folder rows where the target is known from topology).
//  - Context-aware: `sessions`, `selectedId`, and `fallbackCwd` passed;
//    the target is derived from the user's current context (used by
//    sidebar folder headers).

interface LaunchButtonProps {
  className?: string
  onLaunch?: () => void
  /** Async action to run before the launch request (e.g. seed a project). */
  beforeLaunch?: () => Promise<void>
  /** Explicit target: working directory for the new session. */
  cwd?: string
  /** Explicit target: peer name for remote launch. */
  peer?: string
  /**
   * Context-aware target: when `sessions` is provided, the launch target
   * is derived from the user's current context (selected session or most
   * recent alive session) instead of `cwd`/`peer`.
   */
  sessions?: Session[]
  selectedId?: string | null
  fallbackCwd?: string
}

export function LaunchButton({ className, onLaunch, beforeLaunch, cwd, peer, sessions, selectedId, fallbackCwd }: LaunchButtonProps) {
  // Resolve the target: context-aware mode (sessions provided) derives
  // a smart cwd, while an explicit `peer` prop is always authoritative
  // (the caller knows the folder's owner; ADR 0002). This matters for
  // peer folders whose sessions are all dead-resumable, where context
  // mode would otherwise lose the peer in resolveTarget's fallback.
  const target = useMemo((): LaunchTarget => {
    if (sessions && fallbackCwd !== undefined) {
      const ctx = resolveTarget(sessions, selectedId ?? null, fallbackCwd)
      return peer ? { ...ctx, peer } : ctx
    }
    return { peer, cwd: cwd || '' }
  }, [sessions, selectedId, fallbackCwd, cwd, peer])

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
