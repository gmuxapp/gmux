import { useCallback, useEffect, useRef, useState } from 'preact/hooks'
import { Terminal, FitAddon, UrlRegexProvider, OSC8LinkProvider, init as initGhostty } from 'ghostty-web'
import type { ResolvedTerminalOptions } from './settings-schema'
import { attachKeyboardHandler, attachPasteHandler, ctrlSequenceFor, defaultPasteFeedback, handlePasteAction } from './keyboard'
import { DEFAULT_THEME_COLORS, type ResolvedKeybind } from './config'
import { attachMobileInputHandler } from './mobile-input'
import { shouldFocusOnTouchEnd } from './terminal-touch'
import { createReplayBuffer, type ReplayState, BSU } from './replay'
import { stripSyncBlocks, extractScrollbackContent } from './replay-strip'
import { fetchScrollback } from './replay-fetch'
import { createTerminalIO, type TerminalSize } from './terminal-io'

// ── Sync diagnostics ──────────────────────────────────────────────────────────

export interface SyncDiag {
  /** Mirror of ReplayBuffer.state — updated as data flows in */
  syncPhase: ReplayState | 'skipped' | 'idle'
  /** Total bytes received in the scrollback (BSU…ESU) block */
  scrollbackBytes: number
  /** WebSocket messages that arrived before replay was done */
  scrollbackMsgs: number
  /** Wall-clock time when first BSU byte arrived (ms since epoch) */
  syncStartedAt: number | null
  /** Wall-clock time when ESU was detected */
  syncEndedAt: number | null
  /** True while terminalIO still has queued write work */
  pendingWrite: boolean
  /** WS connection state */
  wsState: 'connecting' | 'open' | 'lost'
  /** How many times the WS has reconnected (0 = first connect) */
  reconnects: number
  /** Raw bytes fetched from GET /v1/sessions/<id>/scrollback */
  prefetchBytes: number
  /** Bytes after extractScrollbackContent deduplication */
  prefetchExtractedBytes: number
  /** Number of BSU/ESU full-render blocks found in the prefetch */
  prefetchBlockCount: number
  /** Current lines in the ghostty-web scrollback buffer (live) */
  ghosttyScrollbackLines: number
  /** Configured ghostty-web scrollback line limit */
  ghosttyScrollbackLimit: number
}
import { decideViewportResize, sameSize } from './terminal-resize'
import { MOCK_BY_ID } from './mock-data/index'
import type { Session } from './types'
import type { ITheme } from './types'

// ── Config ──

const USE_MOCK = import.meta.env.VITE_MOCK === '1' || location.search.includes('mock')

// Shared init promise — ghostty-web must be initialised once before any
// Terminal is constructed.  We kick it off at module load time so the WASM
// is ready by the time the first component mounts.
const ghosttyInitPromise: Promise<void> = initGhostty()

// Per-session prefetch cache. Avoids re-downloading (up to 20 MB) and
// re-processing (O(n²) over thousands of blocks) on every tab switch.
// Key: session ID.  Value: extracted bytes to inject, or null if empty.
// Populated on first load; cleared on page reload only.
const _prefetchCache = new Map<string, Uint8Array | null>()

/**
 * Re-export for backward compat (used by input-diagnostics.tsx).
 * The actual colors now live in config.ts as DEFAULT_THEME_COLORS.
 */
export const TERM_THEME: ITheme = DEFAULT_THEME_COLORS

// ── File-link interceptor ──
//
// Browsers block window.open("file://...", "_blank") for security reasons, so
// OSC 8 hyperlinks with file:// URIs silently fail.  We intercept those calls
// here and POST to /v1/open-path instead, which asks the gmux daemon to open
// the file on the server (same machine) with the configured file opener.
;
;(function interceptFileLinks() {
  const _orig = window.open.bind(window)
  window.open = function (url?: string | URL, target?: string, features?: string) {
    const href = typeof url === 'string' ? url : url instanceof URL ? url.href : ''
    if (href.startsWith('file://')) {
      // Strip the file:// prefix and decode percent-encoding to get a plain path.
      const path = decodeURIComponent(href.slice('file://'.length))
      fetch('/v1/open-path', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ path }),
      }).catch(console.error)
      return null
    }
    return _orig(url as string, target, features)
  } as typeof window.open
})()

/**
 * Scan a Uint8Array for complete OSC 52 sequences, write their payload
 * to the clipboard, and return the data with those sequences stripped.
 *
 * ghostty-web has no parser-hook API, so we handle OSC 52 (terminal
 * clipboard writes) before passing data to the terminal.
 *
 * Handles both BEL-terminated (0x07) and ST-terminated (ESC \) sequences.
 * Cross-chunk sequences are NOT preserved — practically, pi always emits
 * them in a single chunk.
 */
/**
 * Count the number of BSU (Begin Synchronized Update) marker occurrences in
 * a byte buffer. Each occurrence corresponds to one full-render block that
 * extractScrollbackContent processed (pi emits one per turn redraw).
 */
function countBSUBlocks(data: Uint8Array): number {
  let count = 0
  for (let i = 0; i <= data.length - BSU.length; i++) {
    let match = true
    for (let j = 0; j < BSU.length; j++) {
      if (data[i + j] !== BSU[j]) { match = false; break }
    }
    if (match) { count++; i += BSU.length - 1 }
  }
  return count
}

function interceptOsc52(data: Uint8Array): Uint8Array {
  const ESC = 0x1b
  const BRACKET = 0x5d // ]
  const BEL = 0x07
  const BACKSLASH = 0x5c

  // Fast path: no ESC in data
  let hasEsc = false
  for (let i = 0; i < data.length; i++) {
    if (data[i] === ESC) { hasEsc = true; break }
  }
  if (!hasEsc) return data

  const output: number[] = []
  let i = 0
  while (i < data.length) {
    if (data[i] === ESC && i + 1 < data.length && data[i + 1] === BRACKET) {
      const oscStart = i
      i += 2
      let body = ''
      while (i < data.length) {
        if (data[i] === BEL) { i++; break }
        if (data[i] === ESC && i + 1 < data.length && data[i + 1] === BACKSLASH) { i += 2; break }
        body += String.fromCharCode(data[i])
        i++
      }
      if (body.startsWith('52;')) {
        const rest = body.slice(3) // strip "52;"
        const semiIdx = rest.indexOf(';')
        if (semiIdx >= 0) {
          const payload = rest.slice(semiIdx + 1)
          if (payload !== '?') {
            try {
              const bytes = Uint8Array.from(atob(payload), c => c.charCodeAt(0))
              const text = new TextDecoder().decode(bytes)
              navigator.clipboard.writeText(text).catch(() => {})
            } catch { /* invalid base64; ignore */ }
          }
          // Consumed — do NOT push to output
          continue
        }
      }
      // Not OSC 52: push original bytes
      for (let j = oscStart; j < i; j++) output.push(data[j])
      continue
    }
    output.push(data[i])
    i++
  }
  // If nothing was stripped, return the original buffer
  if (output.length === data.length) return data
  return new Uint8Array(output)
}

// ── Utilities ──

/**
 * Measure terminal cols/rows that fit within a given element using the
 * FitAddon.  This replaces the old xterm-specific measureTerminalFit that
 * read term.dimensions.css.cell.{width,height} directly.
 *
 * ghostty-web's FitAddon measures term.element (the container we passed to
 * term.open), which is containerRef.current.  containerRef sits inside
 * shellRef; since the canvas fills its container without overflow there is
 * no difference between measuring one vs the other.
 */
function measureTerminalFit(
  fitAddon: FitAddon,
): TerminalSize | null {
  const dims = fitAddon.proposeDimensions()
  if (!dims) return null
  return { cols: dims.cols, rows: dims.rows }
}

/** Legacy wrapper — kept for call sites that need it. */
export function getProposedTerminalSize(fit: FitAddon | null): TerminalSize | null {
  if (!fit) return null
  return measureTerminalFit(fit)
}

function getResizeSignalPixels(host: HTMLElement | null, vv: VisualViewport | null): { width: number; height: number } {
  if (host) {
    return {
      width: host.clientWidth,
      height: host.clientHeight,
    }
  }
  return {
    width: vv?.width ?? window.innerWidth,
    height: vv?.height ?? window.innerHeight,
  }
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

/**
 * Build the ghostty-web ITerminalOptions subset from our resolved settings.
 * Unsupported options (fontWeight, lineHeight, etc.) are silently dropped.
 */
function buildGhosttyOptions(terminalOptions: ResolvedTerminalOptions) {
  return {
    fontSize: terminalOptions.fontSize,
    fontFamily: terminalOptions.fontFamily,
    cursorBlink: terminalOptions.cursorBlink,
    cursorStyle: terminalOptions.cursorStyle,
    theme: terminalOptions.theme,
    scrollback: terminalOptions.scrollback,
    smoothScrollDuration: terminalOptions.smoothScrollDuration,
  }
}

// ── TerminalView ──

/**
 * Single ghostty-web Terminal instance with reconnecting WebSocket.
 *
 * Architecture unchanged from the xterm version: one Terminal lives for the
 * app lifetime. ghostty-web is API-compatible with xterm.js, so the overall
 * structure is the same. Key differences from the xterm build:
 *
 * - Renderer: canvas (native to ghostty-web; no WebGL/DOM addon needed)
 * - FitAddon: from ghostty-web (same API, different internals)
 * - Links: registerLinkProvider instead of WebLinksAddon
 * - OSC 52: pre-processed in the write path (no parser hook API)
 * - Init: async init() required before first Terminal construction
 * - Mobile selection: ghostty-web's canvas SelectionManager handles
 *   mouse-drag selection natively; long-press → copy works on Android
 *   via the OS context menu over the selected canvas region
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
  onSyncDiag,
}: {
  session: Session
  terminalOptions: ResolvedTerminalOptions
  keybinds: ResolvedKeybind[]
  macCommandIsCtrl: boolean
  ctrlArmed: boolean
  onCtrlConsumed: () => void
  altArmed: boolean
  onAltConsumed: () => void
  onInputReady?: (send: ((data: string) => void) | null) => void
  onPasteReady?: (paste: (() => void) | null) => void
  onFocusReady?: (focus: (() => void) | null) => void
  onSyncDiag?: (diag: SyncDiag) => void
}) {
  const shellRef = useRef<HTMLDivElement>(null)
  const containerRef = useRef<HTMLDivElement>(null)
  const termRef = useRef<Terminal | null>(null)
  const fitAddonRef = useRef<FitAddon | null>(null)
  const wsRef = useRef<WebSocket | null>(null)
  const reconnectTimer = useRef<ReturnType<typeof setTimeout> | null>(null)
  const disposed = useRef(false)
  const currentSessionId = useRef(session.id)
  const sessionRef = useRef(session)
  const ctrlArmedRef = useRef(ctrlArmed)
  const altArmedRef = useRef(altArmed)
  const termIoRef = useRef<ReturnType<typeof createTerminalIO> | null>(null)
  const termEpochRef = useRef(0)
  // Per-session saved scroll position (ghostty gvY = lines above bottom).
  // Populated on session-switch away; consumed on the next connect() for that session.
  const savedScrollRef = useRef<Map<string, number>>(new Map())

  // Gates terminal mount until ghostty-web WASM is loaded.
  const [ghosttyReady, setGhosttyReady] = useState(false)

  const [termLoading, setTermLoading] = useState(true)
  const [wsState, setWsState] = useState<'connecting' | 'open' | 'lost'>('connecting')
  const [viewportSize, setViewportSize] = useState<TerminalSize | null>(null)
  const [scrolledUp, setScrolledUp] = useState(false)
  const SCROLL_THRESHOLD = 3
  const [ptySize, setPtySize] = useState<TerminalSize | null>(null)

  // Sync diagnostics state — read by the header via onSyncDiag callback
  const syncDiagRef = useRef<SyncDiag>({
    syncPhase: 'idle',
    scrollbackBytes: 0,
    scrollbackMsgs: 0,
    syncStartedAt: null,
    syncEndedAt: null,
    pendingWrite: false,
    wsState: 'connecting',
    reconnects: 0,
    prefetchBytes: 0,
    prefetchExtractedBytes: 0,
    prefetchBlockCount: 0,
    ghosttyScrollbackLines: 0,
    ghosttyScrollbackLimit: terminalOptions.scrollback,
  })
  const reconnectCountRef = useRef(0)
  const emitSyncDiag = useCallback((patch: Partial<SyncDiag>) => {
    syncDiagRef.current = { ...syncDiagRef.current, ...patch }
    onSyncDiag?.(syncDiagRef.current)
  }, [onSyncDiag])

  const viewportSizeRef = useRef<TerminalSize | null>(null)
  const ptySizeRef = useRef<TerminalSize | null>(null)
  const resizeEchoGateRef = useRef<{
    awaitingEcho: TerminalSize | null
    dirty: boolean
    timer: ReturnType<typeof setTimeout> | null
  }>({
    awaitingEcho: null,
    dirty: false,
    timer: null,
  })
  const processViewportResizeRef = useRef<((forceDrive?: boolean) => void) | null>(null)

  currentSessionId.current = session.id
  sessionRef.current = session
  ctrlArmedRef.current = ctrlArmed
  altArmedRef.current = altArmed

  // Kick off ghostty-web init (shared promise — safe to await multiple times)
  useEffect(() => {
    ghosttyInitPromise.then(() => setGhosttyReady(true))
  }, [])

  const queueResize = useCallback((size: TerminalSize) => {
    termIoRef.current?.requestResize(size, termEpochRef.current)
  }, [])

  const queueData = useCallback((data: Uint8Array, onWritten?: () => void) => {
    termIoRef.current?.enqueue(data, termEpochRef.current, onWritten)
  }, [])

  const queueMany = useCallback((chunks: Uint8Array[], onWritten?: () => void) => {
    termIoRef.current?.enqueueMany(chunks, termEpochRef.current, onWritten)
  }, [])

  const resetResizeEchoGate = useCallback(() => {
    const gate = resizeEchoGateRef.current
    if (gate.timer !== null) clearTimeout(gate.timer)
    gate.awaitingEcho = null
    gate.dirty = false
    gate.timer = null
  }, [])

  const releaseResizeEchoGate = useCallback((applied: TerminalSize) => {
    const gate = resizeEchoGateRef.current
    if (!gate.awaitingEcho || !sameSize(gate.awaitingEcho, applied)) return

    if (gate.timer !== null) clearTimeout(gate.timer)
    gate.awaitingEcho = null
    gate.timer = null

    if (!gate.dirty) return
    gate.dirty = false
    processViewportResizeRef.current?.(true)
  }, [])

  const applyOwnedResize = useCallback((size: TerminalSize) => {
    const prevPty = ptySizeRef.current

    setPtySize(size); ptySizeRef.current = size
    queueResize(size)

    if (sameSize(prevPty, size)) return

    resetResizeEchoGate()

    const ws = wsRef.current
    if (!ws || ws.readyState !== WebSocket.OPEN) return

    announceResize(ws, size)
    const gate = resizeEchoGateRef.current
    gate.awaitingEcho = size
    gate.timer = setTimeout(() => {
      releaseResizeEchoGate(size)
    }, 2000)
  }, [queueResize, releaseResizeEchoGate, resetResizeEchoGate])

  const processViewportResize = useCallback((forceDrive = false) => {
    const fit = fitAddonRef.current
    if (!fit) return

    const newVp = measureTerminalFit(fit)
    const gate = resizeEchoGateRef.current
    const decision = decideViewportResize({
      prevViewport: viewportSizeRef.current,
      ptySize: ptySizeRef.current,
      newViewport: newVp,
      awaitingEcho: gate.awaitingEcho != null,
      forceDrive,
    })

    if (decision.kind === 'wait') {
      viewportSizeRef.current = newVp
      gate.dirty = true
      return
    }

    setViewportSize(newVp); viewportSizeRef.current = newVp

    if (decision.kind === 'drive') {
      applyOwnedResize(decision.size)
      return
    }

    if (decision.kind === 'follow') {
      queueResize(decision.size)
    }
  }, [applyOwnedResize, queueResize])

  processViewportResizeRef.current = processViewportResize

  const fitAndResize = useCallback(() => {
    const fit = fitAddonRef.current
    if (!fit) return

    const dims = measureTerminalFit(fit)
    setViewportSize(dims); viewportSizeRef.current = dims
    if (!dims) return

    applyOwnedResize(dims)
  }, [applyOwnedResize])

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
    if (!containerRef.current || USE_MOCK || !ghosttyReady) return
    disposed.current = false

    const term = new Terminal(buildGhosttyOptions(terminalOptions))
    const fitAddon = new FitAddon()
    term.loadAddon(fitAddon)
    term.open(containerRef.current)

    // Link detection (replaces WebLinksAddon)
    term.registerLinkProvider(new UrlRegexProvider(term))
    term.registerLinkProvider(new OSC8LinkProvider(term))

    // Initial fit
    fitAddon.fit()
    const initialVp = measureTerminalFit(fitAddon)
    setViewportSize(initialVp); viewportSizeRef.current = initialVp

    termRef.current = term
    fitAddonRef.current = fitAddon
    termIoRef.current = createTerminalIO(term, {
      getState() {
        // ghostty-web coordinate system:
        //   getViewportY() = 0     → at the bottom (no scrollback visible)
        //                  = N > 0 → N scrollback lines visible at the top of the
        //                            viewport (i.e. distance-from-bottom in lines)
        //   getScrollbackLength()  → total scrollback lines (= xterm's baseY)
        //
        // terminal-io expects xterm-STABLE absolute coords:
        //   viewportY = absolute buffer-line index at the top of the viewport
        //             = 0 at top of scrollback, scrollbackLen at bottom
        //   baseY     = scrollbackLen
        //
        // Converting: viewportY = scrollbackLen - gvY
        //
        // Stability guarantee: when G new lines are pushed into scrollback the same
        // absolute index still refers to the same buffer line. terminal-io's plain
        // restore (scrollToLine(snap.viewportY)) therefore keeps the right content
        // visible without any explicit growth-compensation arithmetic.
        const scrollbackLen = term.getScrollbackLength()
        // Floor: wheel scroll sets viewportY = currentY - deltaY/33 (fractional).
        // Using the raw fractional value here would store non-integer prevViewportY,
        // causing restoreScroll to land at a fractional position (persistent off-by-one).
        const gvY = Math.floor(term.getViewportY())
        const viewportY = scrollbackLen - gvY
        return { viewportY, baseY: scrollbackLen, rows: term.rows }
      },
      scrollToLine(line: number) {
        // Invert the xterm-stable → ghostty translation:
        //   ghostty gvY = scrollbackLen - xterm-stable line
        // Re-fetch scrollbackLen here (after the write) so the conversion uses the
        // current buffer size — not the size captured before the write.
        const s = term.getScrollbackLength()
        term.scrollToLine(s - line)
      },
      scrollToBottom() { term.scrollToBottom() },
      getLine(y: number): string | null {
        const line = term.buffer.active.getLine(y)
        if (!line) return null
        const text = line.translateToString(true)
        if (text.trim().length < 4) return null
        return text
      },
    })
    ;(window as any).__gmuxTerm = term
    ;(window as any).__gmuxInject = (b64: string) => {
      const bin = atob(b64)
      const bytes = new Uint8Array(bin.length)
      for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i)
      termIoRef.current?.enqueue(bytes, termEpochRef.current)
    }
    ;(window as any).__gmuxDiag = () => ({
      ...syncDiagRef.current,
      // Re-read ghosttyScrollbackLines live so the value is always current,
      // even if no scroll event has fired recently.
      ghosttyScrollbackLines: term.getScrollbackLength(),
    })

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
    onPasteReady?.(() => {
      void handlePasteAction({
        sessionId: session.id,
        bracketedPasteMode: term.hasBracketedPaste(),
        feedback: defaultPasteFeedback,
        emit: sendRawInput,
      })
    })
    onFocusReady?.(() => focusTerminalInput(term))

    const dataDisposable = term.onData((data) => sendInput(data))
    const disposeKeyboardHandler = attachKeyboardHandler(term, sendInput, sendRawInput, keybinds, macCommandIsCtrl, session.id)
    const disposePasteHandler = attachPasteHandler(term, containerRef.current!, sendRawInput, session.id)
    const disposeMobileHandler = attachMobileInputHandler(term, containerRef.current!, sendRawInput)

    const scrollDisposable = term.onScroll(() => {
      // ghostty-web: getViewportY()=0 at bottom, =N when N scrollback lines are visible.
      // gvY IS the distance scrolled from the bottom; show the jump-to-bottom button
      // only when that distance exceeds the threshold.
      const gvY = Math.floor(term.getViewportY())
      setScrolledUp(gvY > SCROLL_THRESHOLD)
      // Snapshot the live scrollback length so the diag panel stays current
      // without needing a dedicated poll loop.
      emitSyncDiag({ ghosttyScrollbackLines: term.getScrollbackLength() })
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
      longPressTimer: null as ReturnType<typeof setTimeout> | null,
      wasLongPress: false,
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

      touchPanState.active = true
      touchPanState.moved = false
      touchPanState.wasLongPress = false
      touchPanState.startX = ev.touches[0].clientX
      touchPanState.startY = ev.touches[0].clientY
      touchPanState.startScrollLeft = host.scrollLeft
      touchPanState.startScrollTop = host.scrollTop

      if (touchPanState.longPressTimer !== null) clearTimeout(touchPanState.longPressTimer)
      touchPanState.longPressTimer = setTimeout(() => {
        touchPanState.longPressTimer = null
        touchPanState.wasLongPress = true
      }, 400)
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
        if (touchPanState.longPressTimer !== null) {
          clearTimeout(touchPanState.longPressTimer)
          touchPanState.longPressTimer = null
        }
      }

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
      if (touchPanState.longPressTimer !== null) {
        clearTimeout(touchPanState.longPressTimer)
        touchPanState.longPressTimer = null
      }
      if (touchPanState.active && shouldFocusOnTouchEnd({ moved: touchPanState.moved, wasLongPress: touchPanState.wasLongPress })) {
        focusTerminalInput(term)
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
      if (touchPanState.longPressTimer !== null) {
        clearTimeout(touchPanState.longPressTimer)
        touchPanState.longPressTimer = null
      }
      touchPanState.active = false
      touchPanState.moved = false
      touchPanState.wasLongPress = false
    }

    shell?.addEventListener('touchstart', handleTouchStartCapture, { capture: true, passive: false })
    shell?.addEventListener('touchmove', handleTouchMoveCapture, { capture: true, passive: false })
    shell?.addEventListener('touchend', handleTouchEndCapture, true)
    shell?.addEventListener('touchcancel', clearTouchPan, true)

    const vv = window.visualViewport
    const isTouchDevice = window.matchMedia('(pointer: coarse)').matches
      || navigator.maxTouchPoints > 0
    const KEYBOARD_RESIZE_DEBOUNCE_MS = 20

    let resizeTimer: ReturnType<typeof setTimeout> | null = null
    let resizeFrame: number | null = null
    let refocusTimer: ReturnType<typeof setTimeout> | null = null
    let lastViewportPixels = getResizeSignalPixels(shell, vv)
    let pendingHeightChange = false

    const flushViewportResize = () => {
      resizeTimer = null
      resizeFrame = null
      processViewportResize()

      const shouldRefocus = pendingHeightChange && isTouchDevice
      pendingHeightChange = false
      if (!shouldRefocus) return

      if (refocusTimer !== null) clearTimeout(refocusTimer)
      refocusTimer = setTimeout(() => focusTerminalInput(termRef.current), 120)
    }

    const scheduleViewportResize = () => {
      if (resizeFrame !== null) cancelAnimationFrame(resizeFrame)
      resizeFrame = requestAnimationFrame(flushViewportResize)
    }

    const onViewportResize = () => {
      const nextViewportPixels = getResizeSignalPixels(shell, vv)
      const widthChanged = nextViewportPixels.width !== lastViewportPixels.width
      const heightChanged = nextViewportPixels.height !== lastViewportPixels.height
      if (!widthChanged && !heightChanged) return

      lastViewportPixels = nextViewportPixels
      pendingHeightChange = pendingHeightChange || heightChanged

      if (resizeTimer !== null) {
        clearTimeout(resizeTimer)
        resizeTimer = null
      }

      if (isTouchDevice && heightChanged && !widthChanged) {
        resizeTimer = setTimeout(scheduleViewportResize, KEYBOARD_RESIZE_DEBOUNCE_MS)
        return
      }

      scheduleViewportResize()
    }

    const shellObserver = new ResizeObserver(() => onViewportResize())
    if (shell) shellObserver.observe(shell)

    window.addEventListener('resize', onViewportResize)
    if (vv) vv.addEventListener('resize', onViewportResize)

    return () => {
      shellObserver.disconnect()
      if (resizeTimer !== null) clearTimeout(resizeTimer)
      if (resizeFrame !== null) cancelAnimationFrame(resizeFrame)
      if (refocusTimer !== null) clearTimeout(refocusTimer)
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
      disposeKeyboardHandler()
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
      ;(window as any).__gmuxInject = null
      ;(window as any).__gmuxDiag = null
      term.dispose()
      termRef.current = null
      fitAddonRef.current = null
      termIoRef.current = null
    }
  }, [onCtrlConsumed, onInputReady, ghosttyReady])

  // WebSocket connection (reconnects when session.id changes).
  useEffect(() => {
    if (!termRef.current || USE_MOCK || !termIoRef.current) return

    let isFirstConnect = true
    let attempt = 0
    let intentionalClose = false
    const epoch = termEpochRef.current + 1
    termEpochRef.current = epoch
    termIoRef.current.reset(epoch)

    // Consume any saved scroll position for this session (set when switching away).
    // gvY = ghostty getViewportY() = lines above bottom (0 = at bottom). Floor to int.
    const savedGvY = Math.floor(savedScrollRef.current.get(session.id) ?? 0)
    savedScrollRef.current.delete(session.id)

    resetResizeEchoGate()
    setPtySize(null); ptySizeRef.current = null
    setViewportSize(null); viewportSizeRef.current = null
    setWsState('connecting')
    reconnectCountRef.current = 0
    emitSyncDiag({
      syncPhase: 'idle',
      scrollbackBytes: 0,
      scrollbackMsgs: 0,
      syncStartedAt: null,
      syncEndedAt: null,
      pendingWrite: false,
      wsState: 'connecting',
      reconnects: 0,
      prefetchBytes: 0,
      prefetchExtractedBytes: 0,
      prefetchBlockCount: 0,
      ghosttyScrollbackLines: 0,
      ghosttyScrollbackLimit: terminalOptions.scrollback,
    })

    setTermLoading(true)

    function connect() {
      if (disposed.current) return

      if (wsRef.current) {
        wsRef.current.close()
        wsRef.current = null
      }

      termIoRef.current?.forceNextScrollToBottom()
      emitSyncDiag({ syncPhase: 'waiting', wsState: 'connecting' })

      const wsProtocol = location.protocol === 'https:' ? 'wss:' : 'ws:'
      // Prefetch the on-disk scrollback in parallel with opening the WS so the
      // socket connects immediately instead of waiting for the full file download.
      //
      // Strategy:
      //   1. Clear the previous session's buffer immediately (hidden by termLoading
      //      overlay) so the fresh epoch starts clean.
      //   2. Start the prefetch HTTP fetch.
      //   3. Open the WS right away with ?no_erase=1 — gmuxd will send only the
      //      visible screen without \x1b[3J, preserving whatever we write into
      //      scrollback from the prefetch.
      //   4. In wireWs: buffer the BSU/ESU replay block and any live messages that
      //      arrive before the prefetch settles; when it does, write in order:
      //      prefetch bytes → WS snapshot → live output.
      //
      // Reconnects skip the prefetch — scrollback is already in the host buffer.
      const openWs = (noErase: boolean, prefetchBarrier?: Promise<void>) => {
        if (disposed.current || currentSessionId.current !== session.id) return
        const url = `${wsProtocol}//${location.host}/ws/${session.id}` + (noErase ? '?no_erase=1' : '')
        const ws = new WebSocket(url)
        wireWs(ws, prefetchBarrier)
      }

      if (!isFirstConnect) {
        // Reconnect: the host terminal already has whatever the
        // prefetch put there on first connect. Don't re-prefetch
        // and don't switch no_erase mode mid-session: a reconnect
        // under no_erase=1 would leave the snapshot's reset behind
        // without restoring scrollback. Use the simple snapshot.
        openWs(false)
        return
      }

      // Clear the old session's buffer immediately (termLoading overlay hides the
      // flash). We always use no_erase=1 for first connects since we handle the
      // erase ourselves; this lets gmuxd skip \x1b[3J so the prefetch bytes we
      // write in parallel remain in scrollback.
      queueData(new TextEncoder().encode('\x1b[3J\x1b[2J\x1b[H'))

      // Barrier promise: resolves once the prefetch has either written its bytes
      // or determined it has nothing to write (empty / not-found / error).
      let prefetchResolve!: () => void
      const prefetchBarrier = new Promise<void>(resolve => { prefetchResolve = resolve })

      const prefetchSessionId = session.id
      const _injectPrefetch = (stripped: Uint8Array, fromCache: boolean) => {
        emitSyncDiag({
          prefetchBytes: stripped.length,
          prefetchExtractedBytes: stripped.length,
          prefetchBlockCount: fromCache ? 0 : countBSUBlocks(stripped),
        })
        if (stripped.length > 0) {
          queueData(stripped)
          const rows = termRef.current?.rows ?? 24
          queueData(new TextEncoder().encode('\r\n'.repeat(rows)))
        }
      }

      // Cache check: avoid re-downloading (up to 20 MB) and re-processing on
      // every tab switch. Cache misses fall through to the full fetch path.
      const _cached = _prefetchCache.get(prefetchSessionId)
      if (_cached !== undefined) {
        // Cache hit — immediate resolve, no network round-trip.
        if (_cached !== null) _injectPrefetch(_cached, true)
        prefetchResolve()
      } else {
        fetchScrollback(prefetchSessionId).then((result) => {
          if (disposed.current || currentSessionId.current !== prefetchSessionId) {
            prefetchResolve()
            return
          }
          if (result.kind === 'bytes') {
            const stripped = extractScrollbackContent(result.bytes)
            emitSyncDiag({
              prefetchBytes: result.bytes.length,
              prefetchExtractedBytes: stripped.length,
              prefetchBlockCount: countBSUBlocks(result.bytes),
            })
            const toCache = stripped.length > 0 ? stripped : null
            _prefetchCache.set(prefetchSessionId, toCache)
            if (stripped.length > 0) {
              // Use the same queue live data uses, so writes serialize
              // correctly with the BSU/ESU snapshot that arrives on WS open.
              queueData(stripped)
              // Push the prefetch content past the visible region into
              // scrollback. The WS snapshot's reset (\x1b[2J) clears the
              // visible rows in place; without this padding, the most
              // recent ~rows of prefetch content would be erased before
              // the visible-screen render lands. A run of CRLFs ensures
              // the bottom of the prefetch sits in scrollback by the
              // time the snapshot's clear-screen fires.
              const rows = termRef.current?.rows ?? 24
              const padding = '\r\n'.repeat(rows)
              queueData(new TextEncoder().encode(padding))
            }
          } else if (result.kind === 'empty' || result.kind === 'not-found') {
            // Definitive empty — cache the negative so we skip on next visit.
            _prefetchCache.set(prefetchSessionId, null)
          }
          // error: don't cache so next visit retries
          prefetchResolve()
        }).catch(() => prefetchResolve())
      }

      // Open the WS immediately — don't wait for the prefetch to finish.
      openWs(true, prefetchBarrier)
    }

    // wireWs attaches the message/error/close handlers to a freshly
    // opened WebSocket. Extracted so the prefetch path and the
    // reconnect path share the same wiring without duplicating it.
    function wireWs(ws: WebSocket, prefetchBarrier?: Promise<void>) {
      ws.binaryType = 'arraybuffer'
      wsRef.current = ws

      // Track bytes/msgs for diagnostics as chunks arrive. Per-WS
      // because wireWs runs again on each reconnect.
      let replaySyncBytes = 0
      let replaySyncMsgs = 0

      // Gate the replay-block callback and live messages on the prefetch so
      // write order is always: prefetch bytes → WS snapshot → live output.
      // Messages that race the barrier are buffered and flushed in one go.
      let prefetchSettled = !prefetchBarrier
      let pendingReplayWrite: (() => void) | null = null
      const pendingLive: Uint8Array[] = []

      if (prefetchBarrier) {
        prefetchBarrier.then(() => {
          prefetchSettled = true
          // Write the replay block first (if it already arrived), then live.
          if (pendingReplayWrite) { pendingReplayWrite(); pendingReplayWrite = null }
          for (const chunk of pendingLive) {
            queueData(chunk, () => setTermLoading(false))
          }
          pendingLive.length = 0
        })
      }

      const replay = createReplayBuffer((chunks) => {
        // Strip OSC 52 from replayed bytes (suppress clipboard writes from
        // original session replay — same rationale as the old
        // term.parser.registerOscHandler(52, () => true) in replay-view.tsx)
        const doWrite = () => {
          const filtered = chunks.map(interceptOsc52)
          queueMany(filtered, () => {
            // Restore saved scroll position (from a previous visit to this session)
            // before removing the loading overlay — the overlay hides the jump.
            if (savedGvY > 0 && termRef.current) {
              termRef.current.scrollToLine(savedGvY)
            }
            setTermLoading(false)
            emitSyncDiag({
              pendingWrite: false,
              ghosttyScrollbackLines: termRef.current?.getScrollbackLength() ?? syncDiagRef.current.ghosttyScrollbackLines,
            })
          })
          emitSyncDiag({ syncEndedAt: Date.now(), pendingWrite: true })
        }
        if (prefetchSettled) {
          doWrite()
        } else {
          // Prefetch still in flight — defer the write; the barrier's .then()
          // handler will invoke it once prefetch bytes are queued.
          pendingReplayWrite = doWrite
        }
      })

      ws.onopen = () => {
        attempt = 0
        setWsState('open')
        const rc = reconnectCountRef.current
        emitSyncDiag({ wsState: 'open', reconnects: rc })

        if (!isFirstConnect) {
          reconnectCountRef.current += 1
          emitSyncDiag({ reconnects: reconnectCountRef.current })
          resetResizeEchoGate()
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
        fitAndResize()
      }

      ws.onmessage = (ev) => {
        if (typeof ev.data === 'string') {
          try {
            const msg = JSON.parse(ev.data)
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
                releaseResizeEchoGate(size)
              }
              return
            }
          } catch {
            // fall through to terminal write
          }

          const data = interceptOsc52(new TextEncoder().encode(ev.data))
          if (replay.state !== 'done') {
            replaySyncBytes += data.length
            replaySyncMsgs += 1
            const wasWaiting = replay.state === 'waiting'
            replay.push(data)
            if (wasWaiting) {
              // After first push: 'buffering' (BSU found), 'done' (BSU+ESU single
              // frame = success), or 'done' with wasSkipped=true (no BSU = skip).
              const phase = replay.state as ReplayState
              emitSyncDiag({
                syncPhase: replay.wasSkipped ? 'skipped' : phase,
                syncStartedAt: Date.now(),
                scrollbackBytes: replaySyncBytes,
                scrollbackMsgs: replaySyncMsgs,
              })
            } else {
              emitSyncDiag({
                syncPhase: replay.state,
                scrollbackBytes: replaySyncBytes,
                scrollbackMsgs: replaySyncMsgs,
              })
            }
            return
          }
          if (prefetchSettled) {
            queueData(data, () => setTermLoading(false))
          } else {
            pendingLive.push(data)
          }
          return
        }

        const rawData = ev.data instanceof ArrayBuffer
          ? new Uint8Array(ev.data)
          : new TextEncoder().encode(ev.data)
        const data = interceptOsc52(rawData)

        if (replay.state !== 'done') {
          replaySyncBytes += data.length
          replaySyncMsgs += 1
          const wasWaiting2 = replay.state === 'waiting'
          replay.push(data)
          if (wasWaiting2) {
            const phase = replay.state as ReplayState
            emitSyncDiag({
              syncPhase: replay.wasSkipped ? 'skipped' : phase,
              syncStartedAt: Date.now(),
              scrollbackBytes: replaySyncBytes,
              scrollbackMsgs: replaySyncMsgs,
            })
          } else {
            emitSyncDiag({
              syncPhase: replay.state,
              scrollbackBytes: replaySyncBytes,
              scrollbackMsgs: replaySyncMsgs,
            })
          }
          return
        }

        if (prefetchSettled) {
          queueData(data, () => setTermLoading(false))
        } else {
          pendingLive.push(data)
        }
      }

      ws.onclose = () => {
        resetResizeEchoGate()
        setWsState(prev => prev === 'open' ? 'lost' : prev)
        emitSyncDiag({ wsState: 'lost' })
        if (disposed.current || intentionalClose) return
        if (currentSessionId.current !== session.id) return

        const delay = Math.min(500 * Math.pow(2, attempt), 8000)
        attempt++
        reconnectTimer.current = setTimeout(connect, delay)
      }

      ws.onerror = () => {}
    }

    connect()

    return () => {
      intentionalClose = true
      // Save scroll position so returning to this session restores it.
      const t = termRef.current
      if (t) {
        const gvY = Math.floor(t.getViewportY())
        if (gvY > 0) {
          savedScrollRef.current.set(session.id, gvY)
        } else {
          savedScrollRef.current.delete(session.id)
        }
      }
      termEpochRef.current = epoch + 1
      termIoRef.current?.reset(termEpochRef.current)
      if (reconnectTimer.current) clearTimeout(reconnectTimer.current)
      reconnectTimer.current = null
      resetResizeEchoGate()
      wsRef.current?.close()
      wsRef.current = null
    }
  }, [fitAndResize, queueData, queueMany, queueResize, releaseResizeEchoGate, resetResizeEchoGate, emitSyncDiag, session.id, ghosttyReady])

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

/** Read-only ghostty-web Terminal showing pre-baked ANSI content for mock/demo mode. */
export function MockTerminal({ sessionId }: { sessionId: string }) {
  const containerRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    if (!containerRef.current) return
    let cancelled = false
    let cleanup: (() => void) | null = null

    ghosttyInitPromise.then(() => {
      if (cancelled || !containerRef.current) return

      const term = new Terminal({
        theme: TERM_THEME,
        fontFamily: "'Fira Code', monospace",
        fontSize: 13,
        disableStdin: true,
        cursorBlink: false,
      })
      const fit = new FitAddon()
      term.loadAddon(fit)
      term.open(containerRef.current)
      fit.fit()

      const mock = MOCK_BY_ID[sessionId]
      if (mock?.terminal) {
        term.write(mock.terminal.replace(/\r?\n/g, '\r\n'), () => {
          if (mock.cursorX != null && mock.cursorY != null) {
            term.write(`\x1b[${mock.cursorY + 1};${mock.cursorX + 1}H`)
          }
        })
      }

      ;(window as any).__gmuxTerm = term

      const onResize = () => fit.fit()
      window.addEventListener('resize', onResize)

      cleanup = () => {
        window.removeEventListener('resize', onResize)
        if ((window as any).__gmuxTerm === term) (window as any).__gmuxTerm = null
        term.dispose()
      }

      if (cancelled) cleanup()
    })

    return () => {
      cancelled = true
      cleanup?.()
    }
  }, [sessionId])

  return (
    <div class="terminal-shell">
      <div ref={containerRef} class="terminal-container" />
    </div>
  )
}
