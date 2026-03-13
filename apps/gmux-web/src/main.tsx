import { render } from 'preact'
import { useCallback, useEffect, useMemo, useRef, useState } from 'preact/hooks'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { AttachAddon } from '@xterm/addon-attach'
import '@xterm/xterm/css/xterm.css'
import './styles.css'

import type { Session, Folder, SessionStatus } from './mock-data'
import { getMockFolders, groupByFolder } from './mock-data'

// ── Config ──

const USE_MOCK = import.meta.env.VITE_MOCK === '1' || location.search.includes('mock')

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
          <button class="btn btn-danger">Kill Session</button>
        ) : (
          <button class="btn btn-primary">Resume Session</button>
        )}
      </div>
    </div>
  )
}

function TerminalView({ sessionId }: { sessionId: string }) {
  const containerRef = useRef<HTMLDivElement>(null)
  const termRef = useRef<Terminal | null>(null)
  const wsRef = useRef<WebSocket | null>(null)

  useEffect(() => {
    if (!containerRef.current || USE_MOCK) return

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

    const wsProtocol = location.protocol === 'https:' ? 'wss:' : 'ws:'
    const ws = new WebSocket(`${wsProtocol}//${location.host}/ws/${sessionId}`)
    ws.binaryType = 'arraybuffer'
    wsRef.current = ws

    ws.onopen = () => {
      const attachAddon = new AttachAddon(ws)
      term.loadAddon(attachAddon)
      const dims = fitAddon.proposeDimensions()
      if (dims) {
        ws.send(JSON.stringify({ type: 'resize', cols: dims.cols, rows: dims.rows }))
      }
    }

    const onResize = () => {
      fitAddon.fit()
      const dims = fitAddon.proposeDimensions()
      if (dims && wsRef.current?.readyState === WebSocket.OPEN) {
        wsRef.current.send(JSON.stringify({ type: 'resize', cols: dims.cols, rows: dims.rows }))
      }
    }
    window.addEventListener('resize', onResize)

    termRef.current = term

    return () => {
      window.removeEventListener('resize', onResize)
      ws.close()
      term.dispose()
      termRef.current = null
      wsRef.current = null
    }
  }, [sessionId])

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
        <code>gmux-run pi</code>
      </div>
    </div>
  )
}

function MainHeader({ session }: { session: Session | null }) {
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
    <div class="main-header">
      <div class="main-header-title">{session.title}</div>
      {session.subtitle && (
        <div class="main-header-subtitle">{session.subtitle}</div>
      )}
      {session.status && (
        <div class={`main-header-status ${session.status.state}`}>
          <span
            class={`session-dot ${session.status.state}`}
            style={{ width: 6, height: 6 }}
          />
          {session.status.label}
        </div>
      )}
    </div>
  )
}

// ── App ──

function App() {
  const [sessions, setSessions] = useState<Session[]>([])
  const [selectedId, setSelectedId] = useState<string | null>(null)
  const [sidebarOpen, setSidebarOpen] = useState(false)

  // Load data
  useEffect(() => {
    if (USE_MOCK) {
      const mockFolders = getMockFolders()
      const allSessions = mockFolders.flatMap(f => f.sessions)
      setSessions(allSessions)
      // Auto-select: attention > active > any alive
      const attention = allSessions.find(s => s.alive && s.status?.state === 'attention')
      const active = allSessions.find(s => s.alive && s.status?.state === 'active')
      const first = attention ?? active ?? allSessions.find(s => s.alive)
      if (first) setSelectedId(first.id)
    } else {
      // Real data via tRPC / SSE — same as before
      // TODO: wire up real data
    }
  }, [])

  const folders = useMemo(() => groupByFolder(sessions), [sessions])
  const selected = useMemo(
    () => sessions.find(s => s.id === selectedId) ?? null,
    [sessions, selectedId],
  )

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

        <MainHeader session={selected} />

        {selected ? (
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
