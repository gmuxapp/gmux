import { render } from 'preact'
import { useCallback, useEffect, useMemo, useRef, useState } from 'preact/hooks'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
// AttachAddon removed — we wire onmessage/onData manually for reconnect support
import '@xterm/xterm/css/xterm.css'
import './styles.css'
import { attachKeyboardHandler } from './keyboard'
import { createReplayBuffer } from './replay'

import type { Session, Folder, SessionStatus } from './mock-data'
import { getMockFolders, groupByFolder } from './mock-data'
import type { Session as ProtocolSession } from '@gmux/protocol'

// ── Config ──

const USE_MOCK = import.meta.env.VITE_MOCK === '1' || location.search.includes('mock')

/** Map protocol session (partial fields) to UI session (all fields required) */
function toUISession(s: ProtocolSession): Session {
  return {
    id: s.id,
    created_at: s.created_at ?? new Date().toISOString(),
    command: s.command ?? [],
    cwd: s.cwd ?? '',
    kind: s.kind ?? 'generic',
    alive: s.alive,
    pid: s.pid ?? null,
    exit_code: s.exit_code ?? null,
    started_at: s.started_at ?? s.created_at ?? new Date().toISOString(),
    exited_at: s.exited_at ?? null,
    title: s.title ?? s.command?.[0] ?? 'session',
    subtitle: s.subtitle ?? '',
    status: s.status ?? null,
    unread: s.unread ?? false,
    socket_path: s.socket_path ?? '',
  }
}

async function fetchSessions(): Promise<Session[]> {
  const resp = await fetch('/trpc/sessions.list')
  const json = await resp.json()
  // tRPC wraps in { result: { data: [...] } }
  const data: ProtocolSession[] = json?.result?.data ?? []
  return data.map(toUISession)
}

async function killSession(sessionId: string): Promise<void> {
  await fetch('/trpc/sessions.kill', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ sessionId }),
  })
}

const NORD_TERM_THEME = {
  background: '#242933',
  foreground: '#d8dee9',
  cursor: '#d8dee9',
  cursorAccent: '#242933',
  selectionBackground: '#434c5ecc',
  black: '#3b4252',
  red: '#bf616a',
  green: '#a3be8c',
  yellow: '#ebcb8b',
  blue: '#81a1c1',
  magenta: '#b48ead',
  cyan: '#88c0d0',
  white: '#e5e9f0',
  brightBlack: '#4c566a',
  brightRed: '#bf616a',
  brightGreen: '#a3be8c',
  brightYellow: '#ebcb8b',
  brightBlue: '#81a1c1',
  brightMagenta: '#b48ead',
  brightCyan: '#8fbcbb',
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

function dotClass(session: Session): string {
  if (!session.alive) return 'dead'
  if (!session.status) return 'paused'
  return session.status.state
}

function statusColor(state: string): string {
  const map: Record<string, string> = {
    active: 'var(--status-active)',
    attention: 'var(--status-attention)',
    success: 'var(--status-success)',
    error: 'var(--status-error)',
    paused: 'var(--status-paused)',
    info: 'var(--status-info)',
  }
  return map[state] ?? 'var(--text-muted)'
}

function folderDotColor(folder: Folder): string | null {
  // Show the highest-priority state across all sessions
  const priorities = ['attention', 'error', 'active', 'info', 'success', 'paused']
  for (const p of priorities) {
    if (folder.sessions.some(s => s.alive && s.status?.state === p)) {
      return statusColor(p)
    }
  }
  if (folder.sessions.some(s => s.alive)) return statusColor('paused')
  return null
}

// ── Components ──

function SessionDot({ session }: { session: Session }) {
  return (
    <div class="session-dot-wrap">
      <span class={`session-dot ${dotClass(session)}`} />
    </div>
  )
}

function SessionItem({
  session,
  selected,
  onClick,
}: {
  session: Session
  selected: boolean
  onClick: () => void
}) {
  return (
    <div
      class={`session-item ${selected ? 'selected' : ''} ${!session.alive ? 'dead' : ''}`}
      onClick={onClick}
    >
      <SessionDot session={session} />
      <div class="session-content">
        <div class="session-title">{session.title}</div>
        <div class="session-meta">
          {session.status && (
            <>
              <span class="session-status-label">{session.status.label}</span>
              <span class="session-meta-sep">·</span>
            </>
          )}
          <span class="session-time">{formatAge(session.created_at)}</span>
          {session.kind !== 'generic' && (
            <>
              <span class="session-meta-sep">·</span>
              <span>{session.kind}</span>
            </>
          )}
        </div>
      </div>
      {session.unread && <div class="unread-badge" />}
    </div>
  )
}

function FolderGroup({
  folder,
  selectedId,
  onSelect,
}: {
  folder: Folder
  selectedId: string | null
  onSelect: (id: string) => void
}) {
  const [expanded, setExpanded] = useState(true)
  const dotColor = folderDotColor(folder)
  const aliveCount = folder.sessions.filter(s => s.alive).length

  return (
    <div class="folder">
      <div class="folder-header" onClick={() => setExpanded(e => !e)}>
        <div class={`folder-chevron ${expanded ? 'open' : ''}`}>▶</div>
        <div class="folder-name">{folder.name}</div>
        {dotColor && (
          <div class="folder-dot" style={{ background: dotColor }} />
        )}
        <div class="folder-count">
          {aliveCount > 0 ? aliveCount : folder.sessions.length}
        </div>
      </div>
      {expanded && (
        <div class="folder-sessions">
          {folder.sessions.map(s => (
            <SessionItem
              key={s.id}
              session={s}
              selected={selectedId === s.id}
              onClick={() => onSelect(s.id)}
            />
          ))}
        </div>
      )}
    </div>
  )
}

function Sidebar({
  folders,
  selectedId,
  onSelect,
  open,
  onClose,
}: {
  folders: Folder[]
  selectedId: string | null
  onSelect: (id: string) => void
  open: boolean
  onClose: () => void
}) {
  return (
    <>
      <div class={`sidebar-overlay ${open ? 'visible' : ''}`} onClick={onClose} />
      <aside class={`sidebar ${open ? 'open' : ''}`}>
        <div class="sidebar-header">
          <div class="sidebar-logo">gmux</div>
          <div class="sidebar-badge">alpha</div>
        </div>
        <div class="sidebar-scroll">
          {folders.map(f => (
            <FolderGroup
              key={f.path}
              folder={f}
              selectedId={selectedId}
              onSelect={(id) => {
                onSelect(id)
                onClose()
              }}
            />
          ))}
        </div>
      </aside>
    </>
  )
}

function SessionDetail({ session }: { session: Session }) {
  const shortCwd = session.cwd.replace(/^\/home\/[^/]+/, '~')

  return (
    <div class="session-detail">
      <div class="detail-hero">
        <div class="detail-hero-status">
          <span class={`session-dot ${dotClass(session)}`} style={{ width: 10, height: 10 }} />
          <span class="detail-hero-state">
            {session.alive
              ? session.status?.label ?? 'running'
              : session.exit_code === 0 ? 'completed' : `exited (${session.exit_code})`
            }
          </span>
        </div>
        <div class="detail-hero-cwd">{shortCwd}</div>
      </div>

      <div class="detail-grid">
        <div class="detail-section">
          <div class="detail-label">Command</div>
          <div class="detail-value">{session.command.join(' ')}</div>
        </div>
        <div class="detail-section">
          <div class="detail-label">Adapter</div>
          <div class="detail-value">{session.kind}</div>
        </div>
        <div class="detail-section">
          <div class="detail-label">Session</div>
          <div class="detail-value">{session.id}</div>
        </div>
        <div class="detail-section">
          <div class="detail-label">Started</div>
          <div class="detail-value">{formatAge(session.started_at)} ago</div>
        </div>
        {session.pid && (
          <div class="detail-section">
            <div class="detail-label">PID</div>
            <div class="detail-value">{session.pid}</div>
          </div>
        )}
        {!session.alive && session.exit_code !== null && (
          <div class="detail-section">
            <div class="detail-label">Exit Code</div>
            <div class="detail-value" style={{
              color: session.exit_code === 0 ? 'var(--status-success)' : 'var(--status-error)'
            }}>
              {session.exit_code}
            </div>
          </div>
        )}
      </div>

      <div class="session-actions">
        {session.alive ? (
          <button class="btn btn-danger" onClick={() => killSession(session.id)}>Kill Session</button>
        ) : (
          <button class="btn btn-primary" disabled>Resume Session</button>
        )}
      </div>
    </div>
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
function TerminalView({ sessionId }: { sessionId: string }) {
  const containerRef = useRef<HTMLDivElement>(null)
  const termRef = useRef<Terminal | null>(null)
  const fitRef = useRef<FitAddon | null>(null)
  const wsRef = useRef<WebSocket | null>(null)
  const reconnectTimer = useRef<ReturnType<typeof setTimeout> | null>(null)
  const disposed = useRef(false)
  const currentSessionId = useRef(sessionId)

  // Keep ref in sync so reconnect closure sees latest value
  currentSessionId.current = sessionId

  // One-time terminal setup
  useEffect(() => {
    if (!containerRef.current || USE_MOCK) return
    disposed.current = false

    const term = new Terminal({
      theme: NORD_TERM_THEME,
      fontFamily: "'JetBrains Mono', 'Fira Code', monospace",
      fontSize: 14,
      cursorBlink: true,
    })
    const fitAddon = new FitAddon()
    term.loadAddon(fitAddon)
    term.open(containerRef.current)
    fitAddon.fit()
    termRef.current = term
    fitRef.current = fitAddon

    // Send raw input to PTY — always uses current wsRef
    const sendInput = (data: string) => {
      const ws = wsRef.current
      if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(new TextEncoder().encode(data))
      }
    }

    // Terminal input → WS
    const dataDisposable = term.onData((data) => sendInput(data))

    // Keyboard handling
    attachKeyboardHandler(term, sendInput)

    // Auto-focus terminal on any keydown outside of it.
    // This ensures keyboard input always goes to the terminal
    // without requiring the user to click it first.
    const handleGlobalKeydown = (ev: KeyboardEvent) => {
      const tag = (ev.target as HTMLElement)?.tagName
      if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return
      if (containerRef.current?.contains(ev.target as Node)) return
      term.focus()
    }
    window.addEventListener('keydown', handleGlobalKeydown, true)

    // Window resize → fit + send dims
    const onResize = () => {
      fitAddon.fit()
      const ws = wsRef.current
      const dims = fitAddon.proposeDimensions()
      if (dims && ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'resize', cols: dims.cols, rows: dims.rows }))
      }
    }
    window.addEventListener('resize', onResize)

    return () => {
      disposed.current = true
      window.removeEventListener('keydown', handleGlobalKeydown, true)
      window.removeEventListener('resize', onResize)
      dataDisposable.dispose()
      if (reconnectTimer.current) clearTimeout(reconnectTimer.current)
      wsRef.current?.close()
      wsRef.current = null
      term.dispose()
      termRef.current = null
      fitRef.current = null
    }
  }, []) // terminal lives for component lifetime

  // WebSocket connection — reconnects on sessionId change or drop
  useEffect(() => {
    if (!termRef.current || USE_MOCK) return

    const term = termRef.current
    const fitAddon = fitRef.current
    let attempt = 0
    let intentionalClose = false

    function connect() {
      if (disposed.current) return

      // Close previous connection
      if (wsRef.current) {
        wsRef.current.close()
        wsRef.current = null
      }

      // Replay buffer: detects synchronized scrollback replay.
      // The runner wraps the replay in BSU + reset sequences + scrollback + ESU,
      // so xterm handles the clear internally as part of the atomic render.
      // If BSU detected → buffer until ESU, write all at once (xterm renders atomically)
      // If no BSU → write immediately (old runner / no scrollback)
      // Frontend never calls term.clear()/term.reset() — all done via escape sequences.
      const replay = createReplayBuffer((chunks) => {
        for (const chunk of chunks) term.write(chunk)
      })

      const wsProtocol = location.protocol === 'https:' ? 'wss:' : 'ws:'
      const ws = new WebSocket(`${wsProtocol}//${location.host}/ws/${sessionId}`)
      ws.binaryType = 'arraybuffer'
      wsRef.current = ws

      ws.onopen = () => {
        attempt = 0
        if (fitAddon) {
          const dims = fitAddon.proposeDimensions()
          if (dims) {
            ws.send(JSON.stringify({ type: 'resize', cols: dims.cols, rows: dims.rows }))
          }
        }
      }

      // WS data → terminal
      ws.onmessage = (ev) => {
        const data = ev.data instanceof ArrayBuffer
          ? new Uint8Array(ev.data)
          : new TextEncoder().encode(ev.data)

        // During replay: buffer feeds into replay detector which writes to term
        if (replay.state !== 'done') {
          replay.push(data)
          return
        }

        // Post-replay: write directly
        term.write(data)
      }

      ws.onclose = () => {
        if (disposed.current || intentionalClose) return
        // Don't reconnect if session switched away
        if (currentSessionId.current !== sessionId) return

        // Exponential backoff: 500ms, 1s, 2s, 4s, max 8s
        const delay = Math.min(500 * Math.pow(2, attempt), 8000)
        attempt++
        reconnectTimer.current = setTimeout(connect, delay)
      }

      ws.onerror = () => {
        // onclose will fire after this, which handles reconnect
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
  }, [sessionId]) // reconnect when session changes

  if (USE_MOCK) {
    return (
      <div
        ref={containerRef}
        class="terminal-container"
        style={{
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'center',
          fontFamily: "'JetBrains Mono', monospace",
          fontSize: '13px',
          color: 'var(--text-muted)',
        }}
      >
        Terminal: {sessionId}
      </div>
    )
  }

  return <div ref={containerRef} class="terminal-container" />
}

function EmptyState() {
  return (
    <div class="empty-state">
      <div class="empty-state-icon">⌘</div>
      <div class="empty-state-title">No session selected</div>
      <div class="empty-state-hint">
        Select a session from the sidebar, or launch a new one with{' '}
        <code>gmuxr pi</code>
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
        <div class="main-header-title">{session.title}</div>
        <div class="main-header-meta">
          <span class="main-header-cwd">{shortCwd}</span>
          <span class="main-header-sep">·</span>
          <span class="main-header-kind">{session.kind}</span>
        </div>
      </div>
      <div class="main-header-right">
        {session.status && (
          <div class={`main-header-status ${session.status.state}`}>
            <span
              class={`session-dot ${session.status.state}`}
              style={{ width: 6, height: 6 }}
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

// ── App ──

type ConnectionState = 'connecting' | 'connected' | 'error'

function App() {
  const [sessions, setSessions] = useState<Session[]>([])
  const [selectedId, setSelectedId] = useState<string | null>(null)
  const [sidebarOpen, setSidebarOpen] = useState(false)
  const [connState, setConnState] = useState<ConnectionState>('connecting')

  // Load data
  useEffect(() => {
    if (USE_MOCK) {
      const mockFolders = getMockFolders()
      const allSessions = mockFolders.flatMap(f => f.sessions)
      setSessions(allSessions)
      setConnState('connected')
      // Auto-select: attention > active > any alive
      const attention = allSessions.find(s => s.alive && s.status?.state === 'attention')
      const active = allSessions.find(s => s.alive && s.status?.state === 'active')
      const first = attention ?? active ?? allSessions.find(s => s.alive)
      if (first) setSelectedId(first.id)
    } else {
      // Fetch initial session list
      fetchSessions().then(list => {
        setSessions(list)
        setConnState('connected')
        // Auto-select first alive session
        const attention = list.find(s => s.alive && s.status?.state === 'attention')
        const active = list.find(s => s.alive && s.status?.state === 'active')
        const first = attention ?? active ?? list.find(s => s.alive)
        if (first && !selectedId) setSelectedId(first.id)
      }).catch(err => {
        console.error('Failed to fetch sessions:', err)
        setConnState('error')
      })

      // Subscribe to SSE for live updates
      const source = new EventSource('/api/events')
      source.addEventListener('session-upsert', (e) => {
        try {
          const envelope = JSON.parse(e.data)
          const session = envelope.session ?? envelope
          const updated = toUISession(session)
          setSessions(prev => {
            const idx = prev.findIndex(s => s.id === updated.id)
            if (idx >= 0) {
              const next = [...prev]
              next[idx] = updated
              return next
            }
            return [...prev, updated]
          })
        } catch {}
      })
      source.addEventListener('session-remove', (e) => {
        try {
          const { id } = JSON.parse(e.data)
          setSessions(prev => prev.filter(s => s.id !== id))
        } catch {}
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

  const folders = useMemo(() => groupByFolder(filteredSessions), [filteredSessions])
  const selected = useMemo(
    () => sessions.find(s => s.id === selectedId) ?? null,
    [sessions, selectedId],
  )

  // Auto-select only when nothing is selected (initial load).
  // When a selected session dies, we let the user decide — don't override their click.
  const hasAutoSelected = useRef(false)
  useEffect(() => {
    if (!selectedId && !hasAutoSelected.current && filteredSessions.length > 0) {
      hasAutoSelected.current = true
      const best =
        filteredSessions.find(s => s.alive && s.status?.state === 'attention') ??
        filteredSessions.find(s => s.alive && s.status?.state === 'active') ??
        filteredSessions.find(s => s.alive) ??
        filteredSessions[0]
      if (best) setSelectedId(best.id)
    }
  }, [filteredSessions, selectedId])

  const handleSelect = useCallback((id: string) => {
    setSelectedId(id)
  }, [])

  const canAttach = selected?.alive && !USE_MOCK

  return (
    <div class="app-layout">
      <Sidebar
        folders={folders}
        selectedId={selectedId}
        onSelect={handleSelect}
        open={sidebarOpen}
        onClose={() => setSidebarOpen(false)}
      />

      <div class="main-panel">
        <div class="mobile-header">
          <button class="mobile-toggle" onClick={() => setSidebarOpen(true)}>
            ☰
          </button>
          <div class="sidebar-logo" style={{ marginLeft: 8 }}>gmux</div>
        </div>

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
        ) : selected ? (
          canAttach ? (
            <TerminalView sessionId={selected.id} />
          ) : (
            <SessionDetail session={selected} />
          )
        ) : (
          <EmptyState />
        )}
      </div>
    </div>
  )
}

render(<App />, document.getElementById('app')!)
