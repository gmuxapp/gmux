// Launcher UI + config fetching.
//
// This module owns everything related to kicking off new sessions:
//  - launcher definitions from the server (`/v1/config`)
//  - per-peer launcher resolution (remote hosts surface their own launchers)
//  - the `+` button used throughout the UI (sidebar, folder rows, etc.)
//  - the "recent launch" flag that lets the app auto-select the session
//    the server creates in response

import { useState, useRef, useEffect, useMemo } from 'preact/hooks'
import type { Session } from './types'

// ── Types ──

export interface LauncherDef {
  id: string
  label: string
  command: string[]
  description?: string
  available: boolean
}

export interface LaunchConfig {
  default_launcher: string
  launchers: LauncherDef[]
  peers?: Record<string, { default_launcher: string; launchers: LauncherDef[] }>
}

/** Resolved launch target: where the session will be created. */
export interface LaunchTarget {
  peer?: string
  cwd: string
}

// ── Config fetching (module-level cache) ──

let _configCache: LaunchConfig | null = null

export async function fetchConfig(): Promise<LaunchConfig> {
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

export function invalidateConfigCache() {
  _configCache = null
}

/** Return launchers for a specific peer, falling back to local config. */
export function launchersForPeer(
  config: LaunchConfig,
  peer: string | undefined,
): { default_launcher: string; launchers: LauncherDef[] } {
  if (peer && config.peers?.[peer]) return config.peers[peer]
  return config
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

// ── Launch action + auto-select tracking ──

let _pendingLaunchAt = 0

export async function launchSession(launcherId: string, opts?: { cwd?: string; peer?: string }): Promise<void> {
  _pendingLaunchAt = Date.now()
  try {
    const resp = await fetch('/v1/launch', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ launcher_id: launcherId, cwd: opts?.cwd, peer: opts?.peer }),
    })
    if (!resp.ok) console.warn('/v1/launch failed:', resp.status, await resp.text().catch(() => ''))
  } catch (err) {
    console.warn('/v1/launch error:', err)
  }
}

/**
 * Check + clear the pending-launch flag. Returns true if a launch was
 * kicked off within `maxAgeMs` and the caller should auto-select the
 * newly-arrived session.
 */
export function consumePendingLaunch(maxAgeMs = 10_000): boolean {
  if (!_pendingLaunchAt) return false
  const fresh = Date.now() - _pendingLaunchAt < maxAgeMs
  _pendingLaunchAt = 0
  return fresh
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
  // Resolve the target: context-aware mode (sessions provided) takes
  // priority over explicit cwd/peer.
  const target = useMemo((): LaunchTarget => {
    if (sessions && fallbackCwd !== undefined) {
      return resolveTarget(sessions, selectedId ?? null, fallbackCwd)
    }
    return { peer, cwd: cwd || '' }
  }, [sessions, selectedId, fallbackCwd, cwd, peer])

  const showTarget = target.cwd !== ''

  const [state, setState] = useState<'idle' | 'loading' | 'open' | 'launching'>('idle')
  const [config, setConfig] = useState<LaunchConfig | null>(null)
  const [menuPos, setMenuPos] = useState<{ top: number; right: number } | null>(null)
  const containerRef = useRef<HTMLDivElement>(null)
  const btnRef = useRef<HTMLButtonElement>(null)

  // Pre-fetch config on first hover so open is instant
  const handleMouseEnter = () => {
    if (!config) fetchConfig().then(setConfig)
  }

  /** Compute fixed position for the menu so it escapes overflow:hidden parents. */
  const computeMenuPos = () => {
    const btn = btnRef.current
    if (!btn) return
    const r = btn.getBoundingClientRect()
    // Align the menu's default item (first adapter) with the button.
    // The menu has 4px padding; if a target line is shown it adds ~32px
    // (target line + divider) that we offset so the first adapter stays aligned.
    const targetOffset = showTarget ? 32 : 0
    setMenuPos({
      top: r.top - 4 - targetOffset,       // 4px = menu padding-top
      right: window.innerWidth - r.right,  // align menu's right edge with button's right edge
    })
  }

  const handleClick = (e: MouseEvent) => {
    e.stopPropagation()
    if (state === 'idle') {
      if (config) {
        computeMenuPos()
        setState('open')
      } else {
        setState('loading')
        fetchConfig().then(cfg => {
          setConfig(cfg)
          computeMenuPos()
          setState('open')
        })
      }
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

  const isOpen = state === 'open' && config
  const isLoading = state === 'launching' || state === 'loading'

  let defaultLauncher: LauncherDef | undefined
  let others: LauncherDef[] = []
  if (isOpen && config) {
    const resolved = launchersForPeer(config, target.peer)
    defaultLauncher = resolved.launchers.find(l => l.id === resolved.default_launcher)
    others = resolved.launchers.filter(l => l.id !== resolved.default_launcher)
  }

  return (
    <div class={`launch-container ${className ?? ''}`} ref={containerRef} onMouseEnter={handleMouseEnter}>
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
          style={{ top: menuPos.top, right: menuPos.right }}
        >
          {showTarget && (
            <>
              <div class="launch-target-line">{formatTarget(target)}</div>
              <div class="launch-inline-divider" />
            </>
          )}
          {defaultLauncher && (
            <button
              class="launch-inline-item launch-inline-default"
              onClick={(e) => { e.stopPropagation(); handleLaunch(defaultLauncher!.id) }}
            >
              {defaultLauncher.label}
            </button>
          )}
          {others.map((l, i) => (
            <button
              key={l.id}
              class="launch-inline-item"
              style={{ animationDelay: `${(i + 1) * 50}ms` }}
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
