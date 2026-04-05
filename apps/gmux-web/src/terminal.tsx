import { useCallback, useEffect, useRef, useState } from 'preact/hooks'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { ImageAddon } from '@xterm/addon-image'
import { WebLinksAddon } from '@xterm/addon-web-links'
import { WebglAddon } from '@xterm/addon-webgl'
import type { ITerminalOptions } from '@xterm/xterm'
import { attachKeyboardHandler, attachPasteHandler, ctrlSequenceFor, formatPasteText } from './keyboard'
import { DEFAULT_THEME_COLORS, type ResolvedKeybind } from './config'
import { attachMobileInputHandler } from './mobile-input'
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

/**
 * Re-export for backward compat (used by input-diagnostics.tsx).
 * The actual colors now live in config.ts as DEFAULT_THEME_COLORS.
 */
export const TERM_THEME = DEFAULT_THEME_COLORS

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
  // Use parseFloat (not parseInt) to preserve sub-pixel precision under zoom.
  const style = getComputedStyle(xtermEl)
  const padX = parseFloat(style.paddingLeft) + parseFloat(style.paddingRight)
  const padY = parseFloat(style.paddingTop) + parseFloat(style.paddingBottom)

  // Measure the shell, the stable flex-allocated viewport.
  const availW = shellEl.clientWidth - padX - reserveWidth
  const availH = shellEl.clientHeight - padY

  let cols = Math.max(2, Math.floor(availW / dims.css.cell.width))
  const rows = Math.max(1, Math.floor(availH / dims.css.cell.height))

  // Guard against 1px overflow: xterm computes screen width as
  // Math.round(device.cell.width * cols / dpr). Because css.cell.width is
  // derived from rounded values (round(device_canvas / dpr) / cols), it can
  // be slightly smaller than the true character width. This makes floor()
  // occasionally produce one extra column whose screen pixel width rounds up
  // past availW, causing 1px horizontal scroll.
  const dpr = window.devicePixelRatio || 1
  if (dims.device.cell.width > 0) {
    const predictedWidth = Math.round(dims.device.cell.width * cols / dpr)
    if (predictedWidth > availW && cols > 2) cols--
  }

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
 * Resize model: selecting a session claims ownership — the first WS connect
 * resizes the PTY to fit this browser's viewport. If another source (local
 * terminal, other browser) later changes the PTY size, the "Sized for another
 * device" pill appears (derived from viewport ≠ PTY). Clicking it reclaims.
 * Auto-reconnects after a network blip re-sync from session metadata without
 * reclaiming, so they don't steal from another driver.
 *
 * Auto-reconnect on WS drop with exponential backoff.
 * No AttachAddon — we wire onmessage/onData manually so we can reconnect.
 */

export function TerminalView({
  session,
  terminalOptions,
  keybinds,
  macCommandIsCtrl,
  ctrlArmed,
  onCtrlConsumed,
  altArmed,
  onAltConsumed,
  onInputReady,
  onPasteReady,
  onFocusReady,
}: {
  session: Session
  terminalOptions: ITerminalOptions
  keybinds: ResolvedKeybind[]
  macCommandIsCtrl: boolean
  ctrlArmed: boolean
  onCtrlConsumed: () => void
  altArmed: boolean
  onAltConsumed: () => void
  onInputReady?: (send: ((data: string) => void) | null) => void
  onPasteReady?: (paste: ((text: string) => void) | null) => void
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
  const sessionRef = useRef(session)
  const ctrlArmedRef = useRef(ctrlArmed)
  const altArmedRef = useRef(altArmed)
  const termIoRef = useRef<ReturnType<typeof createTerminalIO> | null>(null)
  const termEpochRef = useRef(0)

  const [termLoading, setTermLoading] = useState(true)
  const [wsState, setWsState] = useState<'connecting' | 'open' | 'lost'>('connecting')
  const [viewportSize, setViewportSize] = useState<TerminalSize | null>(null)
  const [scrolledUp, setScrolledUp] = useState(false)
  const SCROLL_THRESHOLD = 3 // rows above bottom before showing the button
  // Track the last PTY size we know about so we can derive the pill.
  const [ptySize, setPtySize] = useState<TerminalSize | null>(null)

  // Refs shadow viewportSize/ptySize for use inside event handlers that
  // must not trigger effect re-runs but need current values.
  const viewportSizeRef = useRef<TerminalSize | null>(null)
  const ptySizeRef = useRef<TerminalSize | null>(null)

  currentSessionId.current = session.id
  sessionRef.current = session
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
    setViewportSize(dims); viewportSizeRef.current = dims
    if (!dims) return

    // Optimistically sync ptySize so the pill hides immediately, before the
    // server echoes the resize back. Without this, ptySize would lag behind
    // viewportSize for one round-trip, causing a spurious pill flash.
    setPtySize(dims); ptySizeRef.current = dims
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

    // Add non-serializable options that can't live in JSON config.
    const term = new Terminal({
      ...terminalOptions,
      linkHandler: {
        activate(_event, text) {
          window.open(text, '_blank', 'noopener')
        },
      },
    })
    const fitAddon = new FitAddon()
    term.loadAddon(fitAddon)
    term.loadAddon(new ImageAddon())
    // Detect plain-text URLs in terminal output and make them clickable.
    term.loadAddon(new WebLinksAddon())
    term.open(containerRef.current)
    loadPreferredRenderer(term)
    // Initial fit: use FitAddon for the first resize (before shellRef is
    // guaranteed stable), then switch to measureTerminalFit for everything after.
    fitAddon.fit()
    const initialVp = shellRef.current ? measureTerminalFit(term, shellRef.current) : getProposedTerminalSize(fitAddon)
    setViewportSize(initialVp); viewportSizeRef.current = initialVp
    termRef.current = term
    fitRef.current = fitAddon
    termIoRef.current = createTerminalIO(term, {
      getState() {
        const buf = term.buffer.active
        return { viewportY: buf.viewportY, baseY: buf.baseY }
      },
      scrollToLine(line: number) { term.scrollToLine(line) },
      scrollToBottom() { term.scrollToBottom() },
    })
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
    onPasteReady?.((text: string) => {
      sendRawInput(formatPasteText(text, term.modes.bracketedPasteMode))
    })
    onFocusReady?.(() => focusTerminalInput(term))

    const dataDisposable = term.onData((data) => sendInput(data))
    attachKeyboardHandler(term, sendInput, sendRawInput, keybinds, macCommandIsCtrl)
    const disposePasteHandler = attachPasteHandler(term, containerRef.current!, sendRawInput)
    const disposeMobileHandler = attachMobileInputHandler(term, containerRef.current!, sendRawInput)

    // OSC 52 clipboard: applications (e.g. pi /copy) write
    //   ESC ] 52 ; <selection> ; <base64-payload> BEL
    // to set the system clipboard. The payload is UTF-8 text encoded as
    // base64. Decode and write via the Clipboard API.
    const osc52Disposable = term.parser.registerOscHandler(52, (data) => {
      const semi = data.indexOf(';')
      if (semi < 0) return false
      const payload = data.substring(semi + 1)
      if (payload === '?') return false // clipboard read request; not supported
      try {
        // atob() decodes base64 to a Latin-1 binary string. The underlying
        // bytes are UTF-8, so we must re-decode through TextDecoder.
        const bytes = Uint8Array.from(atob(payload), c => c.charCodeAt(0))
        const text = new TextDecoder().decode(bytes)
        navigator.clipboard.writeText(text).catch(() => {})
      } catch {
        // invalid base64; ignore
      }
      return true
    })

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

      // If viewport matches PTY (in sync), no overflow to pan — let xterm
      // handle the gesture for selection/scrollback.
      const vp = viewportSizeRef.current
      const pty = ptySizeRef.current
      if (vp && pty && vp.cols === pty.cols && vp.rows === pty.rows) return

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
        // Defer scroll so synthesized mouse events (which the browser fires
        // after touchend returns) reach xterm's Linkifier at the current
        // scroll position. Without this, scrollToBottom() changes the
        // viewport before the Linkifier can resolve the link under the tap
        // coordinates, making link taps a no-op on mobile.
        //
        // setTimeout(0) and not rAF: synthesized mouse events fire as part
        // of the current user interaction, before queued tasks. rAF timing
        // relative to synthesized events is unspecified and varies by
        // browser; on some it fires before them, reproducing the bug.
        setTimeout(() => {
          term.scrollToBottom()
          const host = shellRef.current
          if (host) {
            host.scrollTop = host.scrollHeight
            host.scrollLeft = 0
          }
        }, 0)
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
        if (!t || !s) return

        const newVp = measureTerminalFit(t, s)

        // Were we in sync before this viewport change?
        const vp = viewportSizeRef.current
        const pty = ptySizeRef.current
        const wasInSync = vp != null && pty != null
          && vp.cols === pty.cols && vp.rows === pty.rows

        setViewportSize(newVp); viewportSizeRef.current = newVp

        if (wasInSync && newVp) {
          // Viewport matched PTY — this client is actively using the terminal.
          // Auto-resize to follow the viewport change.
          fitAndResize()
        } else if (pty) {
          // Out of sync (pill visible) — keep xterm at PTY size.
          queueResize(pty)
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

    // Listen on both: visualViewport fires for soft keyboard / pinch-zoom,
    // window fires for browser window resize / Playwright setViewportSize.
    window.addEventListener('resize', onViewportResize)
    if (vv) vv.addEventListener('resize', onViewportResize)

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
      disposePasteHandler()
      disposeMobileHandler()
      osc52Disposable.dispose()
      dataDisposable.dispose()
      scrollDisposable.dispose()
      setScrolledUp(false)
      if (reconnectTimer.current) clearTimeout(reconnectTimer.current)
      wsRef.current?.close()
      wsRef.current = null
      onInputReady?.(null)
      onPasteReady?.(null)
      onFocusReady?.(null)
      if ((window as any).__gmuxTerm === term) (window as any).__gmuxTerm = null
      term.dispose()
      termRef.current = null
      fitRef.current = null
      termIoRef.current = null
    }
  }, [onCtrlConsumed, onInputReady])

  // WebSocket connection (reconnects when session.id changes).
  useEffect(() => {
    if (!termRef.current || USE_MOCK || !termIoRef.current) return

    // Claim ownership on the first WS open for this session: resize the PTY
    // to fit this browser's viewport. Auto-reconnects (same session.id) skip
    // the claim, so we don't steal ownership from another driver after a
    // network blip. User can reclaim by clicking the pill if needed.
    let isFirstConnect = true
    let attempt = 0
    let intentionalClose = false
    const epoch = termEpochRef.current + 1
    termEpochRef.current = epoch
    termIoRef.current.reset(epoch)

    // Reset sizes so stale values from a previous session can't trigger a
    // spurious pill while the loading overlay is visible (before ws.onopen).
    setPtySize(null); ptySizeRef.current = null
    setViewportSize(null); viewportSizeRef.current = null
    setWsState('connecting')

    setTermLoading(true)

    function connect() {
      if (disposed.current) return

      if (wsRef.current) {
        wsRef.current.close()
        wsRef.current = null
      }

      // Tell the scroll preservation layer to force-scroll-to-bottom for
      // the replay frame. This avoids the "jump to top" bug: xterm's
      // isUserScrolling flag can persist from the previous session, and
      // \x1b[3J resets ybase/ydisp to 0 without clearing that flag. The
      // force flag makes the BSU/ESU handler treat it as wasAtBottom=true
      // regardless of the stale scroll state.
      termIoRef.current?.forceNextScrollToBottom()

      const replay = createReplayBuffer((chunks) => {
        queueMany(chunks, () => {
          termRef.current?.scrollToBottom()
          setTermLoading(false)
        })
      })

      const wsProtocol = location.protocol === 'https:' ? 'wss:' : 'ws:'
      const ws = new WebSocket(`${wsProtocol}//${location.host}/ws/${session.id}`)
      ws.binaryType = 'arraybuffer'
      wsRef.current = ws

      ws.onopen = () => {
        attempt = 0
        setWsState('open')

        if (!isFirstConnect) {
          // Reconnect: re-sync ptySize from session metadata in case a
          // terminal_resize WS event was missed during the drop. Session
          // metadata is updated via SSE independently, so it may be
          // fresher than our cached ptySize after a network blip.
          const sess = sessionRef.current
          if (sess.terminal_cols && sess.terminal_rows) {
            const cached = ptySizeRef.current
            if (!cached || cached.cols !== sess.terminal_cols || cached.rows !== sess.terminal_rows) {
              const size = { cols: sess.terminal_cols, rows: sess.terminal_rows }
              setPtySize(size); ptySizeRef.current = size
              queueResize(size)
            }
          }
          return
        }
        isFirstConnect = false

        // First connect for this session: claim ownership by fitting the PTY
        // to our viewport. fitAndResize measures, sets viewport+pty
        // optimistically, and sends the resize over this ws (wsRef was set
        // above).
        fitAndResize()
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
                const size = { cols, rows }
                setPtySize(size); ptySizeRef.current = size
                queueResize(size)
              }
              return
            }

            if (msg.type === 'terminal_resize' || msg.type === 'resize_applied') {
              const cols = msg.cols as number | undefined
              const rows = msg.rows as number | undefined
              if (cols && rows) {
                const size = { cols, rows }
                setPtySize(size); ptySizeRef.current = size
                queueResize(size)
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
        setWsState(prev => prev === 'open' ? 'lost' : prev)
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

  // Pill is purely derived from size mismatch. No "driving" flag: we claim
  // on every fresh session select (first ws.onopen), and fitAndResize sets
  // ptySize = viewportSize optimistically so the pill self-clears the moment
  // we start a resize, before the server echoes it back. The pill only
  // reappears when a server-sourced terminal_resize (another client, local
  // terminal) changes ptySize away from our viewport.
  const showDisconnectedPill = wsState === 'lost'
  const showResizePill = !showDisconnectedPill
    && session.alive
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
      {showDisconnectedPill && (
        <div class="terminal-resize-anchor">
          <div class="terminal-disconnected-pill">
            Connection lost, reconnecting…
          </div>
        </div>
      )}
      {showResizePill && (
        <div class="terminal-resize-anchor">
          <button
            type="button"
            class="terminal-resize-overlay"
            onClick={() => fitAndResize()}
          >
            Sized for another device, click to resize
          </button>
        </div>
      )}
      <div ref={containerRef} class="terminal-container" />
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
