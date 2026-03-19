import { useCallback, useEffect, useRef, useState } from 'preact/hooks'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { ImageAddon } from '@xterm/addon-image'
import { WebglAddon } from '@xterm/addon-webgl'
import { attachKeyboardHandler } from './keyboard'
import { createReplayBuffer } from './replay'
import { createTerminalIO, type TerminalSize } from './terminal-io'
import { MOCK_BY_ID } from './mock-data/index'
import type { Session } from './types'

// ── Config ──

const USE_MOCK = import.meta.env.VITE_MOCK === '1' || location.search.includes('mock')

function loadPreferredRenderer(term: Terminal) {
  try {
    term.loadAddon(new WebglAddon())
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

/**
 * Calculate terminal cols/rows that fit within a given element.
 *
 * We intentionally do NOT use FitAddon.proposeDimensions() because it
 * measures `term.element.parentElement` — which may have grown with the
 * terminal content (passive mode) or be affected by overflow scrollbars.
 *
 * Instead we measure `shellEl` (the flex-allocated viewport) directly,
 * subtract the xterm element padding, and divide by cell size. This gives
 * a stable measurement that's immune to terminal content or scrollbar state.
 */
function measureTerminalFit(
  term: Terminal,
  shellEl: HTMLElement,
  /** Extra horizontal pixels to reserve (e.g. for xterm's internal scrollbar). */
  reserveWidth = 0,
): TerminalSize | null {
  const dims = term.dimensions
  if (!dims || dims.css.cell.width === 0 || dims.css.cell.height === 0) return null

  const xtermEl = term.element
  if (!xtermEl) return null

  // Read the xterm element's padding (our CSS sets padding on .xterm).
  const style = getComputedStyle(xtermEl)
  const padX = parseInt(style.paddingLeft) + parseInt(style.paddingRight)
  const padY = parseInt(style.paddingTop) + parseInt(style.paddingBottom)

  // Measure the shell — the stable flex-allocated viewport.
  const availW = shellEl.clientWidth - padX - reserveWidth
  const availH = shellEl.clientHeight - padY

  const cols = Math.max(2, Math.floor(availW / dims.css.cell.width))
  const rows = Math.max(1, Math.floor(availH / dims.css.cell.height))

  return { cols, rows }
}

/** Legacy wrapper — used in a few places that still go through FitAddon. */
export function getProposedTerminalSize(fit: FitAddon | null): TerminalSize | null {
  if (!fit) return null
  const dims = fit.proposeDimensions()
  if (!dims) return null
  return { cols: dims.cols, rows: dims.rows }
}

function announceResize(ws: WebSocket | null, dims: TerminalSize): void {
  if (!ws || ws.readyState !== WebSocket.OPEN) return
  ws.send(JSON.stringify({ type: 'resize', cols: dims.cols, rows: dims.rows }))
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
    case '\x7f': // Backspace → Ctrl+H (backward-kill-char / word-erase depending on app)
      return '\x08'
    default:
      return null
  }
}

function focusTerminalInput(term: Terminal | null): void {
  if (!term) return

  term.focus()

  const textarea = term.textarea
  if (!textarea) return

  const isTouchDevice = window.matchMedia('(pointer: coarse)').matches
    || navigator.maxTouchPoints > 0
  if (!isTouchDevice) return

  const prev = {
    position: textarea.style.position,
    left: textarea.style.left,
    bottom: textarea.style.bottom,
    top: textarea.style.top,
    width: textarea.style.width,
    height: textarea.style.height,
    opacity: textarea.style.opacity,
    zIndex: textarea.style.zIndex,
  }

  textarea.style.position = 'fixed'
  textarea.style.left = '0'
  textarea.style.bottom = '0'
  textarea.style.top = 'auto'
  textarea.style.width = '1px'
  textarea.style.height = '1px'
  textarea.style.opacity = '0.01'
  textarea.style.zIndex = '-1'
  textarea.focus({ preventScroll: true })

  requestAnimationFrame(() => {
    textarea.style.position = prev.position
    textarea.style.left = prev.left
    textarea.style.bottom = prev.bottom
    textarea.style.top = prev.top
    textarea.style.width = prev.width
    textarea.style.height = prev.height
    textarea.style.opacity = prev.opacity
    textarea.style.zIndex = prev.zIndex
  })
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
 * Resize model: clients start passive (respecting the PTY size). The user
 * clicks the "Sized for another device" pill to start driving resize. A
 * terminal_resize event from another source (local tty, other browser) stops
 * driving and shows the pill again. The pill is derived state: viewport ≠ PTY.
 *
 * Auto-reconnect on WS drop with exponential backoff.
 * No AttachAddon — we wire onmessage/onData manually so we can reconnect.
 */

export function TerminalView({
  session,
  ctrlArmed,
  onCtrlConsumed,
  altArmed,
  onAltConsumed,
  onInputReady,
  onFocusReady,
}: {
  session: Session
  ctrlArmed: boolean
  onCtrlConsumed: () => void
  altArmed: boolean
  onAltConsumed: () => void
  onInputReady?: (send: ((data: string) => void) | null) => void
  onFocusReady?: (focus: (() => void) | null) => void
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
  const altArmedRef = useRef(altArmed)
  const termIoRef = useRef<ReturnType<typeof createTerminalIO> | null>(null)
  const termEpochRef = useRef(0)

  // "Driving" means this client is actively controlling the PTY size.
  // Starts false — browsers always connect passive. Set true when the user
  // clicks the pill or explicitly triggers a resize. Set false when a
  // terminal_resize arrives from another source.
  const isDrivingRef = useRef(false)

  const [termLoading, setTermLoading] = useState(true)
  const [viewportSize, setViewportSize] = useState<TerminalSize | null>(null)
  const [scrolledUp, setScrolledUp] = useState(false)
  const SCROLL_THRESHOLD = 3 // rows above bottom before showing the button
  // Track the last PTY size we know about so we can derive the pill.
  const [ptySize, setPtySize] = useState<TerminalSize | null>(null)

  currentSessionId.current = session.id
  currentSessionRef.current = session
  ctrlArmedRef.current = ctrlArmed
  altArmedRef.current = altArmed

  const queueResize = useCallback((size: TerminalSize) => {
    termIoRef.current?.requestResize(size, termEpochRef.current)
  }, [])

  const queueData = useCallback((data: Uint8Array, onWritten?: () => void) => {
    termIoRef.current?.enqueue(data, termEpochRef.current, onWritten)
  }, [])

  const queueMany = useCallback((chunks: Uint8Array[], onWritten?: () => void) => {
    termIoRef.current?.enqueueMany(chunks, termEpochRef.current, onWritten)
  }, [])

  // Resize xterm to fit the viewport and announce the new size to the backend.
  const fitAndResize = useCallback(() => {
    const term = termRef.current
    const shell = shellRef.current
    const ws = wsRef.current
    if (!term || !shell) return

    const dims = measureTerminalFit(term, shell)
    setViewportSize(dims)
    if (!dims) return

    isDrivingRef.current = true
    queueResize(dims)
    announceResize(ws, dims)
  }, [queueResize])

  const focusTerminal = useCallback(() => {
    focusTerminalInput(termRef.current)
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
    // Initial fit: use FitAddon for the first resize (before shellRef is
    // guaranteed stable), then switch to measureTerminalFit for everything after.
    fitAddon.fit()
    setViewportSize(shellRef.current ? measureTerminalFit(term, shellRef.current) : getProposedTerminalSize(fitAddon))
    termRef.current = term
    fitRef.current = fitAddon
    termIoRef.current = createTerminalIO(term)
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
      if (altArmedRef.current) {
        altArmedRef.current = false
        onAltConsumed()
        sendRawInput('\x1b' + data)
        return
      }
      sendRawInput(data)
    }

    onInputReady?.(sendRawInput)
    onFocusReady?.(() => focusTerminalInput(term))

    const dataDisposable = term.onData((data) => sendInput(data))
    attachKeyboardHandler(term, sendInput)

    const scrollDisposable = term.onScroll(() => {
      const buf = term.buffer.active
      setScrolledUp(buf.baseY - buf.viewportY > SCROLL_THRESHOLD)
    })

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

      // Track touch start for both modes — focus happens on touchend
      // only if the user didn't drag (tap vs scroll distinction).
      touchPanState.active = true
      touchPanState.moved = false
      touchPanState.startX = ev.touches[0].clientX
      touchPanState.startY = ev.touches[0].clientY
      touchPanState.startScrollLeft = host.scrollLeft
      touchPanState.startScrollTop = host.scrollTop
    }

    const handleTouchMoveCapture = (ev: TouchEvent) => {
      if (!touchPanState.active || ev.touches.length !== 1) return

      const host = shellRef.current
      if (!host) return

      const touch = ev.touches[0]
      const deltaX = touch.clientX - touchPanState.startX
      const deltaY = touch.clientY - touchPanState.startY
      if (Math.abs(deltaX) > 6 || Math.abs(deltaY) > 6) {
        touchPanState.moved = true
      }

      // In driving mode, just track movement — let xterm handle the gesture.
      if (isDrivingRef.current) return

      const canScrollX = host.scrollWidth > host.clientWidth
      const canScrollY = host.scrollHeight > host.clientHeight
      if (!canScrollX && !canScrollY) return

      if (canScrollX) host.scrollLeft = touchPanState.startScrollLeft - deltaX
      if (canScrollY) host.scrollTop = touchPanState.startScrollTop - deltaY
      ev.preventDefault()
      ev.stopPropagation()
    }

    const handleTouchEndCapture = () => {
      if (touchPanState.active && !touchPanState.moved) {
        focusTerminalInput(term)
        term.scrollToBottom()
        const host = shellRef.current
        if (host) {
          host.scrollTop = host.scrollHeight
          host.scrollLeft = 0
        }
      }
      touchPanState.active = false
      touchPanState.moved = false
    }

    const clearTouchPan = () => {
      touchPanState.active = false
      touchPanState.moved = false
    }

    shell?.addEventListener('touchstart', handleTouchStartCapture, { capture: true, passive: false })
    shell?.addEventListener('touchmove', handleTouchMoveCapture, { capture: true, passive: false })
    shell?.addEventListener('touchend', handleTouchEndCapture, true)
    shell?.addEventListener('touchcancel', clearTouchPan, true)

    // On iOS, both window.resize and visualViewport.resize fire when the
    // keyboard opens/closes, causing double-handling per event. Additionally,
    // iOS fires visualViewport.resize on every frame of the keyboard animation
    // — not just once when it settles.
    //
    // Strategy:
    // - Use visualViewport exclusively when available (skip window.resize).
    // - Debounce: coalesce rapid fires during keyboard animation into one call.
    // - Re-focus after the viewport settles: keyboard animation blurs the
    //   xterm textarea mid-transition, causing keystrokes (e.g. spacebar) to
    //   be lost until the user taps again.
    const vv = window.visualViewport

    let resizeTimer: ReturnType<typeof setTimeout> | null = null
    let lastVvHeight = vv?.height ?? window.innerHeight

    const onViewportResize = () => {
      if (resizeTimer !== null) clearTimeout(resizeTimer)
      resizeTimer = setTimeout(() => {
        resizeTimer = null

        const t = termRef.current
        const s = shellRef.current
        if (t && s) setViewportSize(measureTerminalFit(t, s))

        if (!isDrivingRef.current) {
          // Passive — keep xterm at PTY size, update viewport for pill derivation.
          const current = currentSessionRef.current
          if (current.terminal_cols && current.terminal_rows) {
            queueResize({ cols: current.terminal_cols, rows: current.terminal_rows })
          }
        } else {
          fitAndResize()
        }

        // Re-focus after the viewport settles. iOS blurs the xterm textarea
        // during the keyboard slide animation; refocusing restores typing.
        const newHeight = vv?.height ?? window.innerHeight
        const heightChanged = Math.abs(newHeight - lastVvHeight) > 50
        lastVvHeight = newHeight
        if (heightChanged) {
          // Extra delay: let iOS fully finish the keyboard transition before
          // grabbing focus, otherwise iOS immediately re-blurs.
          setTimeout(() => focusTerminalInput(termRef.current), 120)
        }
      }, 80) // 80 ms debounce — keyboard animation typically takes ~250 ms
    }

    if (vv) {
      vv.addEventListener('resize', onViewportResize)
    } else {
      window.addEventListener('resize', onViewportResize)
    }

    return () => {
      if (resizeTimer !== null) clearTimeout(resizeTimer)
      disposed.current = true
      window.removeEventListener('keydown', handleGlobalKeydown, true)
      window.removeEventListener('resize', onViewportResize)
      if (vv) vv.removeEventListener('resize', onViewportResize)
      shell?.removeEventListener('touchstart', handleTouchStartCapture, true)
      shell?.removeEventListener('touchmove', handleTouchMoveCapture, true)
      shell?.removeEventListener('touchend', handleTouchEndCapture, true)
      shell?.removeEventListener('touchcancel', clearTouchPan, true)
      dataDisposable.dispose()
      scrollDisposable.dispose()
      setScrolledUp(false)
      if (reconnectTimer.current) clearTimeout(reconnectTimer.current)
      wsRef.current?.close()
      wsRef.current = null
      onInputReady?.(null)
      onFocusReady?.(null)
      if ((window as any).__gmuxTerm === term) (window as any).__gmuxTerm = null
      term.dispose()
      termRef.current = null
      fitRef.current = null
      termIoRef.current = null
    }
  }, [onCtrlConsumed, onInputReady])

  // Keep passive terminals in sync with the PTY size from session metadata.
  // This covers the case where the session is switched or the store updates
  // terminal size without a WS terminal_resize (e.g. from SSE discovery).
  useEffect(() => {
    if (!termRef.current || USE_MOCK || isDrivingRef.current) return
    if (session.terminal_cols && session.terminal_rows) {
      const size = { cols: session.terminal_cols, rows: session.terminal_rows }
      setPtySize(size)
      queueResize(size)
    }
  }, [session.id, session.terminal_cols, session.terminal_rows, queueResize])

  // WebSocket connection (reconnects when session.id changes).
  useEffect(() => {
    if (!termRef.current || USE_MOCK || !termIoRef.current) return

    let attempt = 0
    let intentionalClose = false
    const epoch = termEpochRef.current + 1
    termEpochRef.current = epoch
    termIoRef.current.reset(epoch)

    // Always start passive on new connection.
    isDrivingRef.current = false

    setTermLoading(true)

    function connect() {
      if (disposed.current) return

      if (wsRef.current) {
        wsRef.current.close()
        wsRef.current = null
      }

      const replay = createReplayBuffer((chunks) => {
        const totalBytes = chunks.reduce((n, c) => n + c.length, 0)
        queueMany(chunks, totalBytes > 48 ? () => setTermLoading(false) : undefined)
      })

      const wsProtocol = location.protocol === 'https:' ? 'wss:' : 'ws:'
      const ws = new WebSocket(`${wsProtocol}//${location.host}/ws/${session.id}`)
      ws.binaryType = 'arraybuffer'
      wsRef.current = ws

      ws.onopen = () => {
        attempt = 0

        // Always take over resize on connect — this device is clearly
        // the one the user is actively interacting with.
        isDrivingRef.current = true
        const t = termRef.current
        const s = shellRef.current
        if (t && s) {
          const dims = measureTerminalFit(t, s)
          setViewportSize(dims)
          if (dims) {
            setPtySize(dims)
            queueResize(dims)
            announceResize(ws, dims)
          }
        }
      }

      ws.onmessage = (ev) => {
        if (typeof ev.data === 'string') {
          try {
            const msg = JSON.parse(ev.data)
            // Legacy: old proxy sends resize_state on connect with cols/rows.
            // Use it to initialize ptySize if we don't have one yet.
            if (msg.type === 'resize_state') {
              const cols = msg.cols as number | undefined
              const rows = msg.rows as number | undefined
              if (cols && rows) {
                setPtySize({ cols, rows })
                queueResize({ cols, rows })
              }
              return
            }

            if (msg.type === 'terminal_resize' || msg.type === 'resize_applied') {
              const cols = msg.cols as number | undefined
              const rows = msg.rows as number | undefined
              if (cols && rows) {
                const size = { cols, rows }
                setPtySize(size)
                queueResize(size)

                // If the resize came from someone else, stop driving.
                if (isDrivingRef.current && msg.source && msg.source !== 'web_client') {
                  isDrivingRef.current = false
                }
              }
              return
            }
          } catch {
            // fall through to terminal write
          }

          const data = new TextEncoder().encode(ev.data)
          if (replay.state !== 'done') {
            replay.push(data)
            return
          }
          queueData(data, () => setTermLoading(false))
          return
        }

        const data = ev.data instanceof ArrayBuffer
          ? new Uint8Array(ev.data)
          : new TextEncoder().encode(ev.data)

        if (replay.state !== 'done') {
          replay.push(data)
          return
        }

        queueData(data, () => setTermLoading(false))
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
      termEpochRef.current = epoch + 1
      termIoRef.current?.reset(termEpochRef.current)
      if (reconnectTimer.current) clearTimeout(reconnectTimer.current)
      reconnectTimer.current = null
      wsRef.current?.close()
      wsRef.current = null
    }
  }, [queueData, queueMany, queueResize, session.id])

  // Pill is derived: viewport size differs from PTY size, and we're not
  // already driving (if driving, the mismatch is transient — our resize
  // is in flight and ptySize will catch up next frame).
  const showResizePill = session.alive && !isDrivingRef.current
    && ptySize != null && viewportSize != null
    && (viewportSize.cols !== ptySize.cols || viewportSize.rows !== ptySize.rows)

  if (USE_MOCK) {
    return <MockTerminal sessionId={session.id} />
  }

  return (
    <div
      ref={shellRef}
      class={`terminal-shell ${showResizePill ? 'terminal-shell-passive' : ''}`}
      onClick={handleShellClick}
    >
      <div ref={containerRef} class="terminal-container" />
      {showResizePill && (
        <button
          type="button"
          class="terminal-resize-overlay"
          onClick={() => fitAndResize()}
        >
          Sized for another device, click to resize
        </button>
      )}
      {termLoading && (
        <div class="terminal-loading">
          Waiting for output…
        </div>
      )}
      {scrolledUp && (
        <button
          type="button"
          class="terminal-scroll-end"
          onClick={() => termRef.current?.scrollToBottom()}
          title="Scroll to bottom"
        >
          End ↓
        </button>
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
