import { render } from 'preact'
import { useCallback, useEffect, useMemo, useRef, useState } from 'preact/hooks'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { ImageAddon } from '@xterm/addon-image'
// AttachAddon removed — we wire onmessage/onData manually for reconnect support
import '@xterm/xterm/css/xterm.css'
import './styles.css'
import { attachKeyboardHandler } from './keyboard'
import { createReplayBuffer } from './replay'
import { createSidebarState } from './sidebar-state'

import type { Session, Folder } from './types'
import { groupByFolder } from './types'
import { getMockFolders, MOCK_BY_ID } from './mock-data/index'
import { installCopySession } from './mock-data/export-session'
import type { Session as ProtocolSession } from '@gmux/protocol'

// ── Config ──

const USE_MOCK = import.meta.env.VITE_MOCK === '1' || location.search.includes('mock')

// Debug: __gmuxCopySession() in devtools console
installCopySession()

/** Map protocol session (partial fields) to UI session (all fields required) */
function toUISession(s: ProtocolSession): Session {
  return {
    id: s.id,
    created_at: s.created_at ?? new Date().toISOString(),
    command: s.command ?? [],
    cwd: s.cwd ?? '',
    kind: s.kind ?? 'shell',
    alive: s.alive,
    pid: s.pid ?? null,
    exit_code: s.exit_code ?? null,
    started_at: s.started_at ?? s.created_at ?? new Date().toISOString(),
    exited_at: s.exited_at ?? null,
    title: s.title ?? s.command?.[0] ?? 'session',
    subtitle: s.subtitle ?? '',
    status: s.status ?? null,
    unread: s.unread ?? false,
    resumable: s.resumable ?? false,
    resume_key: s.resume_key ?? '',
    close_action: s.close_action ?? (s.alive ? 'dismiss' : undefined),
    socket_path: s.socket_path ?? '',
    terminal_cols: s.terminal_cols ?? undefined,
    terminal_rows: s.terminal_rows ?? undefined,
  }
}

async function fetchSessions(): Promise<Session[]> {
  const resp = await fetch('/v1/sessions')
  const json = await resp.json()
  const data: ProtocolSession[] = json?.data ?? []
  return data.map(toUISession)
}

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

// ── Launcher types & config ──

interface LauncherDef {
  id: string
  label: string
  command: string[]
  description?: string
  available: boolean
}

interface LaunchConfig {
  default_launcher: string
  launchers: LauncherDef[]
}

interface HealthData {
  version: string
  tailscale_url?: string
}

let _healthCache: HealthData | null = null

async function fetchHealth(): Promise<HealthData | null> {
  if (_healthCache) return _healthCache
  try {
    const resp = await fetch('/v1/health')
    const json = await resp.json()
    _healthCache = json.data ?? null
    return _healthCache
  } catch {
    return null
  }
}

/** Mask tailnet name for privacy: "https://gmux.angler-map.ts.net" → "https://gmux.an****.ts.net" */
function maskTailnet(url: string): string {
  return url.replace(/(\.\w{2})[^.]*(?=\.ts\.net)/, '$1****')
}

let _configCache: LaunchConfig | null = null

async function fetchConfig(): Promise<LaunchConfig> {
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

async function launchSession(launcherId: string, cwd?: string): Promise<void> {
  await postAction('/v1/launch', { launcher_id: launcherId, cwd })
}


// ── LaunchButton — transforms into inline menu on click ──
//
// Idle:      [+]
// Open:      [+ button becomes default item] → other items appear below
// Launching: [spinner]
//
// Double-click works because the default item occupies the exact same
// position as the + button. First click opens, second click hits default.

// Track pending launches globally so App can auto-select new sessions
let _pendingLaunchAt = 0

function LaunchButton({ cwd, className }: { cwd?: string; className?: string }) {
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
    _pendingLaunchAt = Date.now()
    launchSession(id, cwd).finally(() => {
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
    defaultLauncher = config.launchers.find(l => l.id === config.default_launcher)
    others = config.launchers.filter(l => l.id !== config.default_launcher)
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

const TERM_THEME = {
  background: '#0f141a',            // --bg-surface
  foreground: '#d3d8de',            // --text
  cursor: '#d3d8de',                // --text
  cursorAccent: '#0f141a',          // --bg-surface
  selectionBackground: '#2a3a4acc', // visible selection with slight blue tint
  black: '#151b21',                 // --border
  red: '#c25d66',
  green: '#a3be8c',
  yellow: '#ebcb8b',
  blue: '#81a1c1',
  magenta: '#b48ead',
  cyan: '#49b8b8',                  // --accent
  white: '#d3d8de',                 // --text
  brightBlack: '#595e63',           // --text-muted
  brightRed: '#d06c75',
  brightGreen: '#b4d19a',
  brightYellow: '#f0d9a0',
  brightBlue: '#93b3d1',
  brightMagenta: '#c9a3c4',
  brightCyan: '#5fcece',
  brightWhite: '#eceff4',
}

// ── Utilities ──

function formatAge(iso: string): string {
  const ms = Date.now() - new Date(iso).getTime()
  const mins = Math.floor(ms / 60_000)
  if (mins < 1) return 'now'
  if (mins < 60) return `${mins}m`
  const hrs = Math.floor(mins / 60)
  if (hrs < 24) return `${hrs}h`
  const days = Math.floor(hrs / 24)
  return `${days}d`
}



// ── Components ──

/** Determine the dot indicator state for a session. */
function sessionDotState(session: Session): 'working' | 'unread' | 'none' {
  if (session.alive && session.status?.working) return 'working'
  if (session.unread) return 'unread'
  return 'none'
}

function SessionItem({
  session,
  selected,
  resuming,
  onClick,
  onClose,
}: {
  session: Session
  selected: boolean
  resuming?: boolean
  onClick: () => void
  onClose?: () => void
}) {
  const dotState = resuming ? 'working' : sessionDotState(session)
  const closeAction = session.close_action
  const showKind = false

  return (
    <div
      class={`session-item ${selected ? 'selected' : ''} ${!session.alive && !session.resumable ? 'dead' : ''}`}
      onClick={onClick}
    >
      <span class={`session-dot-indicator ${dotState}`} />
      <div class="session-content">
        <div class="session-title-row">
          <span class="session-title">{session.title}</span>
          <span class="session-time">{formatAge(session.created_at)}</span>
        </div>
        <div class="session-meta">
          {session.status?.label && (
            <span class="session-status-label">{session.status.label}</span>
          )}
          {showKind && (
            <span>{session.kind}</span>
          )}
        </div>
      </div>
      {onClose && closeAction && (
        <button
          class={`session-close-btn ${closeAction}`}
          onClick={(e) => { e.stopPropagation(); onClose() }}
          title={closeAction === 'minimize' ? 'Suspend session' : 'Remove session'}
        >
          {closeAction === 'minimize' ? '−' : '×'}
        </button>
      )}
    </div>
  )
}

function FolderGroup({
  folder,
  selectedId,
  resumingId,
  onSelect,
  onCloseSession,
  onHideFolder,
  isSessionVisible,
}: {
  folder: Folder
  selectedId: string | null
  resumingId: string | null
  onSelect: (id: string) => void
  onCloseSession: (session: Session) => void
  onHideFolder: (cwd: string) => void
  isSessionVisible: (session: Session) => boolean
}) {
  const [showMore, setShowMore] = useState(false)

  // Split sessions into visible and collapsed ("show more")
  const visible: Session[] = []
  const collapsed: Session[] = []
  for (const s of folder.sessions) {
    if (isSessionVisible(s)) visible.push(s)
    else collapsed.push(s)
  }

  return (
    <div class="folder">
      <div class="folder-header">
        <div class="folder-name">{folder.name}</div>
        <LaunchButton cwd={folder.path} className="folder-launch-btn" />
      </div>
      <div class="folder-sessions">
        {visible.map(s => (
          <SessionItem
            key={s.id}
            session={s}
            selected={selectedId === s.id}
            resuming={resumingId === s.id}
            onClick={() => onSelect(s.id)}
            onClose={() => onCloseSession(s)}
          />
        ))}
        <div class="folder-actions">
          {collapsed.length > 0 && (
            <button
              class="folder-action-btn"
              onClick={() => setShowMore(v => !v)}
            >
              {showMore ? 'Show less' : `Show ${collapsed.length} more`}
            </button>
          )}
          {collapsed.length > 0 && (
            <span class="folder-action-sep">·</span>
          )}
          <button
            class="folder-action-btn"
            onClick={() => onHideFolder(folder.path)}
          >
            Hide
          </button>
        </div>
        {showMore && collapsed.map(s => (
          <SessionItem
            key={s.id}
            session={s}
            selected={selectedId === s.id}
            resuming={resumingId === s.id}
            onClick={() => onSelect(s.id)}
            onClose={() => onCloseSession(s)}
          />
        ))}
      </div>
    </div>
  )
}

function Sidebar({
  folders,
  hiddenFolders,
  selectedId,
  resumingId,
  onSelect,
  onCloseSession,
  onHideFolder,
  onShowFolder,
  isSessionVisible,
  open,
  onClose,
}: {
  folders: Folder[]
  hiddenFolders: Folder[]
  selectedId: string | null
  resumingId: string | null
  onSelect: (id: string) => void
  onCloseSession: (session: Session) => void
  onHideFolder: (cwd: string) => void
  onShowFolder: (cwd: string) => void
  isSessionVisible: (session: Session) => boolean
  open: boolean
  onClose: () => void
}) {
  const [showFolderPicker, setShowFolderPicker] = useState(false)

  return (
    <>
      <div class={`sidebar-overlay ${open ? 'visible' : ''}`} onClick={onClose} />
      <aside class={`sidebar ${open ? 'open' : ''}`}>
        <div class="sidebar-header">
          <div class="sidebar-logo">gmux</div>
          <div class="sidebar-badge">alpha</div>
          <LaunchButton className="sidebar-launch-btn" />
        </div>
        <div class="sidebar-scroll">
          {folders.map(f => (
            <FolderGroup
              key={f.path}
              folder={f}
              selectedId={selectedId}
              resumingId={resumingId}
              onSelect={(id) => {
                onSelect(id)
                onClose()
              }}
              onCloseSession={onCloseSession}
              onHideFolder={onHideFolder}
              isSessionVisible={isSessionVisible}
            />
          ))}
        </div>
        {hiddenFolders.length > 0 && (
          <div class="sidebar-footer">
            <button
              class="add-folder-btn"
              onClick={() => setShowFolderPicker(v => !v)}
            >
              + Add folder
            </button>
            {showFolderPicker && (
              <div class="folder-picker">
                {hiddenFolders.map(f => (
                  <button
                    key={f.path}
                    class="folder-picker-item"
                    onClick={() => {
                      onShowFolder(f.path)
                      setShowFolderPicker(false)
                    }}
                  >
                    <span class="folder-picker-name">{f.name}</span>
                  </button>
                ))}
              </div>
            )}
          </div>
        )}
      </aside>
    </>
  )
}



/**
 * Single xterm.js instance with reconnecting WebSocket.
 *
 * Architecture: one Terminal lives for the app lifetime. Switching sessions
 * closes the old WS, clears the terminal, and opens a new WS. The runner's
 * 128KB scrollback ring buffer replays on connect, so history is preserved
 * without keeping per-session xterm instances alive.
 *
 * Auto-reconnect on WS drop with exponential backoff.
 * No AttachAddon — we wire onmessage/onData manually so we can reconnect.
 */
/** Send current terminal dimensions over WebSocket (including pixel size for image protocols). */
interface TerminalSize {
  cols: number
  rows: number
}

function getProposedTerminalSize(fit: FitAddon | null): TerminalSize | null {
  if (!fit) return null
  const dims = fit.proposeDimensions()
  if (!dims) return null
  return { cols: dims.cols, rows: dims.rows }
}

function sendResize(ws: WebSocket | null, fit: FitAddon | null, term: Terminal | null): TerminalSize | null {
  const dims = getProposedTerminalSize(fit)
  if (!dims || !term || !ws || ws.readyState !== WebSocket.OPEN) return null
  const msg: Record<string, unknown> = { type: 'resize', cols: dims.cols, rows: dims.rows }
  const el = term.element
  if (el) {
    msg.pixelWidth = el.clientWidth
    msg.pixelHeight = el.clientHeight
  }
  ws.send(JSON.stringify(msg))
  return dims
}

function ctrlSequenceFor(data: string): string | null {
  if (data.length !== 1) return null

  const ch = data
  if (/[a-z]/i.test(ch)) {
    return String.fromCharCode(ch.toUpperCase().charCodeAt(0) - 64)
  }

  switch (ch) {
    case '@':
      return '\x00'
    case '[':
      return '\x1b'
    case '\\':
      return '\x1c'
    case ']':
      return '\x1d'
    case '^':
      return '\x1e'
    case '_':
      return '\x1f'
    case '?':
      return '\x7f'
    default:
      return null
  }
}

function TerminalView({
  session,
  ctrlArmed,
  onCtrlConsumed,
  onInputReady,
}: {
  session: Session
  ctrlArmed: boolean
  onCtrlConsumed: () => void
  onInputReady?: (send: ((data: string) => void) | null) => void
}) {
  const containerRef = useRef<HTMLDivElement>(null)
  const termRef = useRef<Terminal | null>(null)
  const fitRef = useRef<FitAddon | null>(null)
  const wsRef = useRef<WebSocket | null>(null)
  const reconnectTimer = useRef<ReturnType<typeof setTimeout> | null>(null)
  const disposed = useRef(false)
  const currentSessionId = useRef(session.id)
  const currentSessionRef = useRef(session)
  const ctrlArmedRef = useRef(ctrlArmed)
  const isResizeOwnerRef = useRef(false)
  const [termLoading, setTermLoading] = useState(true)
  const [viewportSize, setViewportSize] = useState<TerminalSize | null>(null)
  const [isResizeOwner, setIsResizeOwner] = useState(false)

  currentSessionId.current = session.id
  currentSessionRef.current = session
  ctrlArmedRef.current = ctrlArmed

  // Keep ref in sync with state for use inside callbacks.
  isResizeOwnerRef.current = isResizeOwner

  const applyPassiveTerminalSize = useCallback(() => {
    const term = termRef.current
    const fit = fitRef.current
    const current = currentSessionRef.current
    if (!term || !fit) return

    const proposed = getProposedTerminalSize(fit)
    setViewportSize(proposed)

    if (current.terminal_cols && current.terminal_rows) {
      term.resize(current.terminal_cols, current.terminal_rows)
    }
  }, [])

  // Fit terminal to container and send resize to runner via WS.
  // Only effective when we're the resize owner (proxy will drop otherwise).
  const fitAndResize = useCallback(() => {
    const term = termRef.current
    const fit = fitRef.current
    const ws = wsRef.current
    if (!term || !fit) return

    fit.fit()
    const dims = sendResize(ws, fit, term)
    setViewportSize(dims)
  }, [])

  // Send claim_resize over WS to take resize ownership from another device.
  // The proxy confirms via resize_state, which triggers fit+resize after
  // React removes the overlay from the DOM.
  const claimResize = useCallback(() => {
    const ws = wsRef.current
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify({ type: 'claim_resize' }))
    }
  }, [])

  // Terminal + keyboard setup (stable across session changes).
  useEffect(() => {
    if (!containerRef.current || USE_MOCK) return
    disposed.current = false

    const term = new Terminal({
      theme: TERM_THEME,
      fontFamily: "'Fira Code', monospace",
      fontSize: 13,
      cursorBlink: true,
    })
    const fitAddon = new FitAddon()
    term.loadAddon(fitAddon)
    term.loadAddon(new ImageAddon())
    term.open(containerRef.current)
    fitAddon.fit()
    setViewportSize(getProposedTerminalSize(fitAddon))
    termRef.current = term
    fitRef.current = fitAddon
    ;(window as any).__gmuxTerm = term

    const sendRawInput = (data: string) => {
      const ws = wsRef.current
      if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(new TextEncoder().encode(data))
        term.focus()
      }
    }

    const sendInput = (data: string) => {
      if (ctrlArmedRef.current) {
        const ctrlData = ctrlSequenceFor(data)
        if (ctrlData) {
          ctrlArmedRef.current = false
          onCtrlConsumed()
          sendRawInput(ctrlData)
          return
        }
      }
      sendRawInput(data)
    }

    onInputReady?.(sendRawInput)

    const dataDisposable = term.onData((data) => sendInput(data))
    attachKeyboardHandler(term, sendInput)

    const handleGlobalKeydown = (ev: KeyboardEvent) => {
      const tag = (ev.target as HTMLElement)?.tagName
      if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return
      if (containerRef.current?.contains(ev.target as Node)) return
      term.focus()
    }
    window.addEventListener('keydown', handleGlobalKeydown, true)

    const onWindowResize = () => {
      if (!isResizeOwnerRef.current) {
        // Not the owner — adopt the owner's size from SSE.
        const current = currentSessionRef.current
        const fit = fitRef.current
        if (fit) setViewportSize(getProposedTerminalSize(fit))
        if (current.terminal_cols && current.terminal_rows && termRef.current) {
          termRef.current.resize(current.terminal_cols, current.terminal_rows)
        }
        return
      }
      // Owner — fit to our container and send resize.
      const t = termRef.current
      const f = fitRef.current
      const ws = wsRef.current
      if (t && f) {
        f.fit()
        sendResize(ws, f, t)
        setViewportSize(getProposedTerminalSize(f))
      }
    }
    window.addEventListener('resize', onWindowResize)

    return () => {
      disposed.current = true
      window.removeEventListener('keydown', handleGlobalKeydown, true)
      window.removeEventListener('resize', onWindowResize)
      dataDisposable.dispose()
      if (reconnectTimer.current) clearTimeout(reconnectTimer.current)
      wsRef.current?.close()
      wsRef.current = null
      onInputReady?.(null)
      if ((window as any).__gmuxTerm === term) (window as any).__gmuxTerm = null
      term.dispose()
      termRef.current = null
      fitRef.current = null
    }
  }, [onCtrlConsumed, onInputReady])

  // React to terminal_cols/terminal_rows changes from SSE when not the owner.
  useEffect(() => {
    if (!termRef.current || USE_MOCK) return
    if (isResizeOwner) {
      // We're the owner — fit to our own viewport.
      const fit = fitRef.current
      if (fit) {
        fit.fit()
        setViewportSize(getProposedTerminalSize(fit))
      }
    } else {
      applyPassiveTerminalSize()
    }
  }, [session.id, session.terminal_cols, session.terminal_rows, isResizeOwner, applyPassiveTerminalSize])

  // WebSocket connection (reconnects when session.id changes).
  useEffect(() => {
    if (!termRef.current || USE_MOCK) return

    const term = termRef.current
    let attempt = 0
    let intentionalClose = false

    setTermLoading(true)

    function connect() {
      if (disposed.current) return

      if (wsRef.current) {
        wsRef.current.close()
        wsRef.current = null
      }

      const replay = createReplayBuffer((chunks) => {
        for (const chunk of chunks) term.write(chunk)

        // Hide loading only if replay had real scrollback content.
        // Empty replay = BSU(8) + reset(14) + ESU(8) = 30bytes.
        // Anything ≤48 is just the wrapper with no meaningful content.
        const totalBytes = chunks.reduce((n, c) => n + c.length, 0);
        if (totalBytes > 48) setTermLoading(false);
      });

      const wsProtocol = location.protocol === 'https:' ? 'wss:' : 'ws:'
      const ws = new WebSocket(`${wsProtocol}//${location.host}/ws/${session.id}`)
      ws.binaryType = 'arraybuffer'
      wsRef.current = ws

      ws.onopen = () => {
        attempt = 0
        // The proxy will send us a resize_state message telling us if
        // we're the owner. We'll fit/resize in response to that.
      }

      ws.onmessage = (ev) => {
        // Text messages may be JSON control messages from the proxy.
        if (typeof ev.data === 'string') {
          try {
            const msg = JSON.parse(ev.data)
            if (msg.type === 'resize_state') {
              const nowOwner = !!msg.is_owner
              isResizeOwnerRef.current = nowOwner
              setIsResizeOwner(nowOwner)
              if (nowOwner) {
                // We just became the owner — fit to our viewport and resize.
                // Defer to next frame so React can flush the DOM first
                // (e.g. removing the resize overlay that takes up space).
                requestAnimationFrame(() => {
                  const f = fitRef.current
                  const t = termRef.current
                  if (f && t) {
                    f.fit()
                    sendResize(wsRef.current, f, t)
                    setViewportSize(getProposedTerminalSize(f))
                  }
                })
              } else {
                // Not the owner — resize xterm to match the PTY immediately
                // using the dimensions included in the control message.
                const t = termRef.current
                const cols = msg.cols as number | undefined
                const rows = msg.rows as number | undefined
                if (t && cols && rows) {
                  t.resize(cols, rows)
                }
              }
              return
            }
          } catch { /* not JSON — fall through to terminal write */ }
          // Non-control text message — write to terminal.
          const data = new TextEncoder().encode(ev.data)
          if (replay.state !== 'done') {
            replay.push(data)
            return
          }
          setTermLoading(false)
          term.write(data)
          return
        }

        const data = ev.data instanceof ArrayBuffer
          ? new Uint8Array(ev.data)
          : new TextEncoder().encode(ev.data)

        if (replay.state !== 'done') {
          replay.push(data)
          return
        }

        setTermLoading(false)
        term.write(data)
      }

      ws.onclose = () => {
        if (disposed.current || intentionalClose) return
        if (currentSessionId.current !== session.id) return

        const delay = Math.min(500 * Math.pow(2, attempt), 8000)
        attempt++
        reconnectTimer.current = setTimeout(connect, delay)
      }

      ws.onerror = () => {
      }
    }

    connect()

    return () => {
      intentionalClose = true
      if (reconnectTimer.current) clearTimeout(reconnectTimer.current)
      reconnectTimer.current = null
      wsRef.current?.close()
      wsRef.current = null
    }
  }, [session.id, applyPassiveTerminalSize])

  const showResizeOverlay = session.alive && !isResizeOwner
    && !!session.terminal_cols && !!session.terminal_rows

  if (USE_MOCK) {
    return <MockTerminal sessionId={session.id} />
  }

  return (
    <>
      {showResizeOverlay && (
        <div class="terminal-resize-overlay">
          <span>This terminal is sized for another device.</span>
          <button class="terminal-resize-overlay-btn" onClick={() => claimResize()}>
            Resize for this device
          </button>
        </div>
      )}
      <div class="terminal-shell">
        <div ref={containerRef} class="terminal-container" />
        {termLoading && (
          <div class="terminal-loading">
            Waiting for output…
          </div>
        )}
      </div>
    </>
  )
}

/** Read-only xterm instance showing pre-baked ANSI content for mock/demo mode. */
function MockTerminal({ sessionId }: { sessionId: string }) {
  const containerRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    if (!containerRef.current) return

    const term = new Terminal({
      theme: TERM_THEME,
      fontFamily: "'Fira Code', monospace",
      fontSize: 13,
      disableStdin: true,
      cursorBlink: false,
      cursorInactiveStyle: 'none',
    })
    const fit = new FitAddon()
    term.loadAddon(fit)
    term.open(containerRef.current)
    fit.fit()

    const mock = MOCK_BY_ID[sessionId]
    if (mock?.terminal) {
      // Normalize \n to \r\n so xterm carriage-returns to column 0 on each line.
      term.write(mock.terminal.replace(/\r?\n/g, '\r\n'), () => {
        if (mock.cursorX != null && mock.cursorY != null) {
          term.write(`\x1b[${mock.cursorY + 1};${mock.cursorX + 1}H`)
        }
      })
    }

    // Expose for debug: window.__gmuxTerm
    ;(window as any).__gmuxTerm = term

    const onResize = () => fit.fit()
    window.addEventListener('resize', onResize)

    return () => {
      window.removeEventListener('resize', onResize)
      if ((window as any).__gmuxTerm === term) (window as any).__gmuxTerm = null
      term.dispose()
    }
  }, [sessionId])

  return (
    <div class="terminal-shell">
      <div ref={containerRef} class="terminal-container" />
    </div>
  )
}

function EmptyState({ launchers, health }: { launchers: LauncherDef[]; health: HealthData | null }) {
  const [launching, setLaunching] = useState<string | null>(null)

  const handleLaunch = (id: string) => {
    setLaunching(id)
    _pendingLaunchAt = Date.now()
    launchSession(id).finally(() => setLaunching(null))
  }

  const tailscaleURL = health?.tailscale_url

  return (
    <div class="empty-state">
      {launchers.length > 0 && (
        <div class="empty-state-launchers">
          {launchers.map(l => (
            <button
              key={l.id}
              class={`empty-state-launcher ${launching === l.id ? 'launching' : ''}`}
              onClick={() => handleLaunch(l.id)}
              disabled={launching !== null}
            >
              <span class="empty-state-launcher-label">{l.label}</span>
              {l.description && <span class="empty-state-launcher-desc">{l.description}</span>}
            </button>
          ))}
        </div>
      )}
      <div class="empty-state-footer">
        <div class="empty-state-meta">
          or <code>gmux {'<command>'}</code> from any terminal
        </div>
        <div class="empty-state-meta">
          <span>http://{location.host}</span>
          {tailscaleURL && <>
            <span class="empty-state-dot" />
            <span>{maskTailnet(tailscaleURL)}</span>
          </>}
        </div>
      </div>
    </div>
  )
}

function MainHeader({ session, onKill }: { session: Session | null; onKill?: (id: string) => void }) {
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
          {session.stale && (
            <span class="stale-badge" title="This session is running a different build of gmux. Restart the session to update.">
              outdated
            </span>
          )}
        </div>
        <div class="main-header-meta">
          <span class="main-header-cwd">{shortCwd}</span>

        </div>
      </div>
      <div class="main-header-right">
        {session.status && session.status.label && (
          <div class={`main-header-status ${session.status.working ? 'working' : ''}`}>
            <span
              class={`session-dot ${session.status.working ? 'working' : 'idle'}`}
              style={{ width: 5, height: 5 }}
            />
            {session.status.label}
          </div>
        )}
        {session.alive && onKill && (
          <button
            class="header-kill-btn"
            onClick={() => onKill(session.id)}
            title="Kill session"
          >
            X
          </button>
        )}
      </div>
    </div>
  )
}

function MobileTerminalBar({
  canSend,
  ctrlArmed,
  onMenu,
  onSend,
  onToggleCtrl,
}: {
  canSend: boolean
  ctrlArmed: boolean
  onMenu: () => void
  onSend: (data: string) => void
  onToggleCtrl: () => void
}) {
  return (
    <div class="mobile-bottom-bar" aria-label="Mobile terminal controls">
      <button class="mobile-bottom-action" onClick={onMenu} title="Open sessions">
        ☰
      </button>
      <div class="mobile-bottom-sep" />
      <div class="mobile-terminal-actions" role="toolbar" aria-label="Terminal keys">
        <button class="mobile-bottom-action" disabled={!canSend} onClick={() => onSend('\x1b')} title="Escape">esc</button>
        <button class="mobile-bottom-action" disabled={!canSend} onClick={() => onSend('\t')} title="Tab">tab</button>
        <button
          class={`mobile-bottom-action ${ctrlArmed ? 'armed' : ''}`}
          disabled={!canSend}
          onClick={onToggleCtrl}
          title={ctrlArmed ? 'Ctrl armed for next typed key' : 'Arm Ctrl for next typed key'}
          aria-pressed={ctrlArmed}
        >
          ctrl
        </button>
        <button class="mobile-bottom-action" disabled={!canSend} onClick={() => onSend('\x1b[A')} title="Up arrow">↑</button>
        <button class="mobile-bottom-action" disabled={!canSend} onClick={() => onSend('\x1b[B')} title="Down arrow">↓</button>
        <button class="mobile-bottom-action" disabled={!canSend} onClick={() => onSend('\n')} title="Enter">↵</button>
      </div>
    </div>
  )
}

// ── App ──

type ConnectionState = 'connecting' | 'connected' | 'error'

const sidebarState = createSidebarState()

function App() {
  const [sessions, setSessions] = useState<Session[]>([])
  const [selectedId, setSelectedId] = useState<string | null>(null)
  const [sidebarOpen, setSidebarOpen] = useState(false)
  const [connState, setConnState] = useState<ConnectionState>('connecting')
  const [ctrlArmed, setCtrlArmed] = useState(false)
  const [launchers, setLaunchers] = useState<LauncherDef[]>([])
  const [health, setHealth] = useState<HealthData | null>(null)
  const [sidebarVersion, forceUpdate] = useState(0) // re-render on sidebar state change
  const terminalInputRef = useRef<((data: string) => void) | null>(null)

  useEffect(() => { fetchConfig().then(cfg => setLaunchers(cfg.launchers)) }, [])
  useEffect(() => { fetchHealth().then(setHealth) }, [])

  // Subscribe to sidebar state changes for re-render
  useEffect(() => sidebarState.subscribe(() => forceUpdate(n => n + 1)), [])

  // Refresh timestamps every 60s
  useEffect(() => {
    const timer = setInterval(() => forceUpdate(n => n + 1), 60_000)
    return () => clearInterval(timer)
  }, [])

  // Sync sidebar visibility whenever sessions change
  useEffect(() => { sidebarState.syncSessions(sessions) }, [sessions])

  // Load data
  useEffect(() => {
    if (USE_MOCK) {
      const mockFolders = getMockFolders()
      const allSessions = mockFolders.flatMap(f => f.sessions)
      setSessions(allSessions)
      setConnState('connected')
    } else {
      fetchSessions().then(list => {
        setSessions(list)
        setConnState('connected')
      }).catch(err => {
        console.error('Failed to fetch sessions:', err)
        setConnState('error')
      })

      // Subscribe to SSE for live updates
      const source = new EventSource('/v1/events')
      let sseConnected = false
      source.addEventListener('open', () => {
        if (sseConnected) {
          // Reconnected after a drop — do a full refresh to catch missed events
          fetchSessions().then(list => setSessions(list)).catch(() => {})
        }
        sseConnected = true
      })
      source.addEventListener('session-upsert', (e) => {
        try {
          const envelope = JSON.parse(e.data)
          const session = envelope.session ?? envelope
          const updated = toUISession(session)
          let isNew = false
          setSessions(prev => {
            const idx = prev.findIndex(s => s.id === updated.id)
            if (idx >= 0) {
              const next = [...prev]
              next[idx] = updated
              return next
            }
            isNew = true
            return [...prev, updated]
          })
          // Auto-select newly launched sessions
          if (isNew && _pendingLaunchAt && Date.now() - _pendingLaunchAt < 10_000) {
            _pendingLaunchAt = 0
            setSelectedId(updated.id)
          }
        } catch (err) {
          console.warn('session-upsert: bad event', err)
        }
      })
      source.addEventListener('session-remove', (e) => {
        try {
          const { id } = JSON.parse(e.data)
          setSessions(prev => prev.filter(s => s.id !== id))
        } catch (err) {
          console.warn('session-remove: bad event', err)
        }
      })
      return () => source.close()
    }
  }, [])

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

  const allFolders = useMemo(() => groupByFolder(filteredSessions), [filteredSessions])
  const folders = useMemo(
    () => allFolders.filter(f => sidebarState.isFolderVisible(f.path)),
    [allFolders, sidebarVersion],
  )
  const hiddenFolders = useMemo(
    () => allFolders.filter(f => !sidebarState.isFolderVisible(f.path)),
    [allFolders, sidebarVersion],
  )
  const selected = useMemo(() => {
    const s = sessions.find(s => s.id === selectedId) ?? null
    ;(window as any).__gmuxSession = s
    return s
  }, [sessions, selectedId])

  // Auto-select: pick first attachable session on initial load.
  const hasAutoSelected = useRef(false)
  useEffect(() => {
    if (!selectedId && !hasAutoSelected.current && filteredSessions.length > 0) {
      hasAutoSelected.current = true
      const best = filteredSessions.find(s => s.alive && s.socket_path)
      if (best) setSelectedId(best.id)
    }
  }, [filteredSessions, selectedId])

  // --- Actions: send to backend, wait for SSE. No optimistic updates. ---

  // resumingId is pure UI state — shows a spinner while waiting for the
  // backend to confirm the session is alive. Not session state.
  const [resumingId, setResumingId] = useState<string | null>(null)

  const handleCloseSession = useCallback((session: Session) => {
    if (session.close_action === 'minimize') {
      killSession(session.id)
    } else {
      dismissSession(session.id)
      // No optimistic removal — SSE session-remove will update the list.
    }
  }, [])

  const handleHideFolder = useCallback((cwd: string) => {
    sidebarState.hideFolder(cwd)
  }, [])

  const handleSelect = useCallback((id: string) => {
    const sess = sessions.find(s => s.id === id)
    if (sess?.resumable) {
      // Resume: show spinner, send request, wait for SSE to make it alive.
      setResumingId(id)
      resumeSession(id).catch(err => {
        console.error('resume failed:', err)
        setResumingId(prev => prev === id ? null : prev)
      })
      return
    }
    setResumingId(null) // cancel any pending resume auto-select
    setSelectedId(id)
    setCtrlArmed(false)
  }, [sessions])

  // When a resumed session comes alive, select it.
  useEffect(() => {
    if (resumingId) {
      const sess = sessions.find(s => s.id === resumingId)
      if (sess?.alive && sess?.socket_path) {
        setSelectedId(resumingId)
        setResumingId(null)
      }
    }
  }, [sessions, resumingId])

  // Resume timeout — clear after 10s if the session never came alive.
  useEffect(() => {
    if (!resumingId) return
    const t = setTimeout(() => setResumingId(null), 10_000)
    return () => clearTimeout(t)
  }, [resumingId])

  const canAttach = !!selected?.alive && !!selected?.socket_path && !USE_MOCK

  // Selected = what the terminal shows. No terminal → deselect.
  useEffect(() => {
    if (!canAttach) setCtrlArmed(false)
    if (selectedId && selected && !selected.alive) {
      setSelectedId(null)
    }
  }, [canAttach, selectedId, selected])

  const handleTerminalInputReady = useCallback((send: ((data: string) => void) | null) => {
    terminalInputRef.current = send
  }, [])

  const handleMobileInput = useCallback((data: string) => {
    terminalInputRef.current?.(data)
  }, [])

  const handleToggleCtrl = useCallback(() => {
    if (!canAttach) return
    setCtrlArmed((armed) => !armed)
  }, [canAttach])

  const handleCtrlConsumed = useCallback(() => {
    setCtrlArmed(false)
  }, [])

  return (
    <div class="app-layout">
      <Sidebar
        folders={folders}
        hiddenFolders={hiddenFolders}
        selectedId={selectedId}
        resumingId={resumingId}
        onSelect={handleSelect}
        onCloseSession={handleCloseSession}
        onHideFolder={handleHideFolder}
        onShowFolder={(cwd) => sidebarState.showFolder(cwd)}
        isSessionVisible={(s) => sidebarState.isSessionVisible(s)}
        open={sidebarOpen}
        onClose={() => setSidebarOpen(false)}
      />

      <div class="main-panel">
        <MainHeader session={selected} onKill={killSession} />

        {connState === 'connecting' ? (
          <div class="state-message">
            <div class="state-icon">⋯</div>
            <div class="state-title">Connecting</div>
            <div class="state-subtitle">Reaching gmuxd…</div>
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
        ) : selected && (canAttach || USE_MOCK) ? (
          <TerminalView
            session={selected}
            ctrlArmed={ctrlArmed}
            onCtrlConsumed={handleCtrlConsumed}
            onInputReady={handleTerminalInputReady}
          />
        ) : (
          <EmptyState launchers={launchers} health={health} />
        )}

        <MobileTerminalBar
          canSend={canAttach}
          ctrlArmed={ctrlArmed}
          onMenu={() => setSidebarOpen(true)}
          onSend={handleMobileInput}
          onToggleCtrl={handleToggleCtrl}
        />
      </div>
    </div>
  )
}

render(<App />, document.getElementById('app')!)
