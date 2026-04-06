// Launcher UI + config fetching.
//
// This module owns everything related to kicking off new sessions:
//  - launcher definitions from the server (`/v1/config`)
//  - per-peer launcher resolution (remote hosts surface their own launchers)
//  - the `+` button used throughout the UI (sidebar, folder rows, etc.)
//  - the "recent launch" flag that lets the app auto-select the session
//    the server creates in response

import { useState, useRef, useEffect } from 'preact/hooks'

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

// ── Launch action + auto-select tracking ──
//
// When the user triggers a launch we stash a timestamp here, then the SSE
// handler in main.tsx consumes it when the server broadcasts the new
// session. Keeping this bookkeeping inside the launcher module (rather
// than a shared global) keeps the coupling local: whoever kicks off a
// launch marks the timestamp implicitly through `launchSession`.

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

// ── Quick-launch memory ──

const STORAGE_PREFIX = 'gmux.launch.lastTarget.'

export function getLastLauncher(storageKey: string | undefined): string | null {
  if (!storageKey) return null
  try { return localStorage.getItem(STORAGE_PREFIX + storageKey) } catch { return null }
}

export function setLastLauncher(storageKey: string | undefined, launcherId: string): void {
  if (!storageKey) return
  try { localStorage.setItem(STORAGE_PREFIX + storageKey, launcherId) } catch {}
}

// ── LaunchButton ──
//
// Transforms into an inline menu on click:
//
//   Idle:      [+]
//   Open:      [+ button becomes default item] → other items appear below
//   Launching: [spinner]
//
// Double-click works because the default item occupies the exact same
// position as the + button. First click opens, second click hits default.
//
// When `storageKey` is set, the last-launched adapter is remembered in
// localStorage and used as the default on the next open.

export function LaunchButton({ cwd, peer, className, onLaunch, storageKey }: { cwd?: string; peer?: string; className?: string; onLaunch?: () => void; storageKey?: string }) {
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
    setLastLauncher(storageKey, id)
    onLaunch?.()
    launchSession(id, { cwd, peer }).finally(() => {
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
    const resolved = launchersForPeer(config, peer)
    // Use the last-used launcher if remembered, falling back to the server default.
    const remembered = getLastLauncher(storageKey)
    const defaultId = (remembered && resolved.launchers.some(l => l.id === remembered))
      ? remembered
      : resolved.default_launcher
    defaultLauncher = resolved.launchers.find(l => l.id === defaultId)
    others = resolved.launchers.filter(l => l.id !== defaultId)
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
