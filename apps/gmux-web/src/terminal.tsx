import { useCallback, useEffect, useRef, useState } from 'preact/hooks'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { ImageAddon } from '@xterm/addon-image'
import { WebglAddon } from '@xterm/addon-webgl'
import { attachKeyboardHandler } from './keyboard'
import { createReplayBuffer } from './replay'
import { MOCK_BY_ID } from './mock-data/index'
import type { Session } from './types'

// ── Config ──

const SEARCH = new URLSearchParams(location.search)
const USE_MOCK = import.meta.env.VITE_MOCK === '1' || SEARCH.has('mock')
const USE_WEBGL = SEARCH.get('renderer') !== 'canvas'
const PRESERVE_WEBGL_DRAWING_BUFFER = SEARCH.has('preserveDrawingBuffer') || SEARCH.has('screenshot')

function loadPreferredRenderer(term: Terminal) {
  if (!USE_WEBGL) return
  try {
    term.loadAddon(new WebglAddon({ preserveDrawingBuffer: PRESERVE_WEBGL_DRAWING_BUFFER }))
  } catch {
    /* falls back to DOM renderer */
  }
}

export const TERM_THEME = {
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

export interface TerminalSize {
  cols: number
  rows: number
}

export function getProposedTerminalSize(fit: FitAddon | null): TerminalSize | null {
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

// ── TerminalView ──

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

export function TerminalView({
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
  const shellRef = useRef<HTMLDivElement>(null)
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
    setViewportSize(dims ?? getProposedTerminalSize(fit))
  }, [])

  // Send claim_resize over WS to take resize ownership from another device.
  // The proxy confirms via resize_state; once ownership flips we fit+resize.
  const claimResize = useCallback(() => {
    const ws = wsRef.current
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify({ type: 'claim_resize' }))
    }
  }, [])

  const focusTerminal = useCallback(() => {
    termRef.current?.focus()
  }, [])

  const handleShellClick = useCallback((ev: MouseEvent) => {
    const target = ev.target
    if (target instanceof HTMLElement && target.closest('button, input, textarea, select, a, label, [role="button"]')) {
      return
    }
    focusTerminal()
  }, [focusTerminal])

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
    loadPreferredRenderer(term)
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

    const shell = shellRef.current
    const isInteractiveTarget = (target: EventTarget | null) => target instanceof HTMLElement
      && !!target.closest('button, input, textarea, select, a, label, [role="button"]')
    const touchPanState = {
      active: false,
      moved: false,
      startX: 0,
      startY: 0,
      startScrollLeft: 0,
      startScrollTop: 0,
    }

    const handlePointerDownCapture = (ev: PointerEvent) => {
      if (!isResizeOwnerRef.current) return
      if (ev.button !== 0) return
      if (isInteractiveTarget(ev.target)) return
      term.focus()
    }

    const handleTouchStartCapture = (ev: TouchEvent) => {
      if (ev.touches.length !== 1 || isInteractiveTarget(ev.target)) {
        touchPanState.active = false
        touchPanState.moved = false
        return
      }

      const host = shellRef.current
      if (!host) {
        touchPanState.active = false
        touchPanState.moved = false
        return
      }

      if (isResizeOwnerRef.current) {
        term.focus()
        touchPanState.active = false
        touchPanState.moved = false
        return
      }

      touchPanState.active = true
      touchPanState.moved = false
      touchPanState.startX = ev.touches[0].clientX
      touchPanState.startY = ev.touches[0].clientY
      touchPanState.startScrollLeft = host.scrollLeft
      touchPanState.startScrollTop = host.scrollTop
    }

    const handleTouchMoveCapture = (ev: TouchEvent) => {
      if (isResizeOwnerRef.current || !touchPanState.active || ev.touches.length !== 1) return

      const host = shellRef.current
      if (!host) return

      const touch = ev.touches[0]
      const deltaX = touch.clientX - touchPanState.startX
      const deltaY = touch.clientY - touchPanState.startY
      if (Math.abs(deltaX) > 6 || Math.abs(deltaY) > 6) {
        touchPanState.moved = true
      }

      const canScrollX = host.scrollWidth > host.clientWidth
      const canScrollY = host.scrollHeight > host.clientHeight
      if (!canScrollX && !canScrollY) return

      if (canScrollX) host.scrollLeft = touchPanState.startScrollLeft - deltaX
      if (canScrollY) host.scrollTop = touchPanState.startScrollTop - deltaY
      ev.preventDefault()
      ev.stopPropagation()
    }

    const handleTouchEndCapture = () => {
      if (!isResizeOwnerRef.current && touchPanState.active && !touchPanState.moved) {
        term.focus()
      }
      touchPanState.active = false
      touchPanState.moved = false
    }

    const clearTouchPan = () => {
      touchPanState.active = false
      touchPanState.moved = false
    }

    shell?.addEventListener('pointerdown', handlePointerDownCapture, true)
    shell?.addEventListener('touchstart', handleTouchStartCapture, { capture: true, passive: false })
    shell?.addEventListener('touchmove', handleTouchMoveCapture, { capture: true, passive: false })
    shell?.addEventListener('touchend', handleTouchEndCapture, true)
    shell?.addEventListener('touchcancel', clearTouchPan, true)

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
      shell?.removeEventListener('pointerdown', handlePointerDownCapture, true)
      shell?.removeEventListener('touchstart', handleTouchStartCapture, true)
      shell?.removeEventListener('touchmove', handleTouchMoveCapture, true)
      shell?.removeEventListener('touchend', handleTouchEndCapture, true)
      shell?.removeEventListener('touchcancel', clearTouchPan, true)
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
    if (!isResizeOwner) {
      applyPassiveTerminalSize()
      return
    }

    const frame = requestAnimationFrame(() => {
      if (!isResizeOwnerRef.current) return
      fitAndResize()
    })
    return () => cancelAnimationFrame(frame)
  }, [session.id, session.terminal_cols, session.terminal_rows, isResizeOwner, applyPassiveTerminalSize, fitAndResize])

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
              if (!nowOwner) {
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
    <div
      ref={shellRef}
      class={`terminal-shell ${showResizeOverlay ? 'terminal-shell-passive' : ''}`}
      onClick={handleShellClick}
    >
      <div ref={containerRef} class="terminal-container" />
      {showResizeOverlay && (
        <button
          type="button"
          class="terminal-resize-overlay"
          onClick={() => claimResize()}
        >
          Sized for another device, click to resize
        </button>
      )}
      {termLoading && (
        <div class="terminal-loading">
          Waiting for output…
        </div>
      )}
    </div>
  )
}

// ── MockTerminal ──

/** Read-only xterm instance showing pre-baked ANSI content for mock/demo mode. */
export function MockTerminal({ sessionId }: { sessionId: string }) {
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
    loadPreferredRenderer(term)
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
