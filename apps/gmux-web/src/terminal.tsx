import { useCallback, useEffect, useRef, useState } from 'preact/hooks'
import { Terminal, FitAddon, UrlRegexProvider, OSC8LinkProvider } from 'ghostty-web'
import type { ResolvedTerminalOptions } from './settings-schema'
import { attachKeyboardHandler, attachPasteHandler, ctrlSequenceFor, defaultPasteFeedback, handlePasteAction } from './keyboard'
import { DEFAULT_THEME_COLORS, type ResolvedKeybind } from './config'
import { attachMobileInputHandler } from './mobile-input'
import { focusTerminalInput, useTouchPan } from './terminal-touch'
import { createReplayBuffer } from './replay'
import { createTerminalIO, type TerminalSize } from './terminal-io'
import { decideViewportResize, sameSize, useViewportResize } from './terminal-resize'
import { ghosttyInitPromise } from './terminal-init'
import { useWebSocket } from './use-websocket'
import { MOCK_BY_ID } from './mock-data/index'
import type { Session } from './types'
import type { ITheme } from './types'

export type { SyncDiag } from './terminal-types'
import type { SyncDiag } from './terminal-types'

// ── Config ──

const USE_MOCK = import.meta.env.VITE_MOCK === '1' || location.search.includes('mock')

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

// ── Utilities ──

/**
 * Measure terminal cols/rows that fit within a given element using the FitAddon.
 */
function measureTerminalFit(fitAddon: FitAddon): TerminalSize | null {
  const dims = fitAddon.proposeDimensions()
  if (!dims) return null
  return { cols: dims.cols, rows: dims.rows }
}

/** Legacy wrapper — kept for call sites that need it. */
export function getProposedTerminalSize(fit: FitAddon | null): TerminalSize | null {
  if (!fit) return null
  return measureTerminalFit(fit)
}

function announceResize(ws: WebSocket | null, dims: TerminalSize): void {
  if (!ws || ws.readyState !== WebSocket.OPEN) return
  ws.send(JSON.stringify({ type: 'resize', cols: dims.cols, rows: dims.rows }))
}

function buildGhosttyOptions(terminalOptions: ResolvedTerminalOptions) {
  return {
    fontSize:             terminalOptions.fontSize,
    fontFamily:           terminalOptions.fontFamily,
    cursorBlink:          terminalOptions.cursorBlink,
    cursorStyle:          terminalOptions.cursorStyle,
    theme:                terminalOptions.theme,
    scrollback:           terminalOptions.scrollback,
    smoothScrollDuration: terminalOptions.smoothScrollDuration,
  }
}

// ── TerminalView ──

/**
 * Single ghostty-web Terminal instance with reconnecting WebSocket.
 *
 * When `isActive` is false the terminal is hidden (display:none) but its
 * WebSocket stays open. Switching sessions is now instant — no snapshot
 * replay needed on return.
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
  isActive,
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
  /** When false, the terminal is hidden (display:none) and its WS stays open.
   *  Callbacks (onInputReady etc.) are deregistered while inactive so the
   *  parent routes input/focus to the visible session only. Defaults to true. */
  isActive?: boolean
}) {
  const shellRef     = useRef<HTMLDivElement>(null)
  const containerRef = useRef<HTMLDivElement>(null)
  const termRef      = useRef<Terminal | null>(null)
  const fitAddonRef  = useRef<FitAddon | null>(null)
  const wsRef        = useRef<WebSocket | null>(null)
  const reconnectTimer  = useRef<ReturnType<typeof setTimeout> | null>(null)
  const disposed        = useRef(false)
  const currentSessionId = useRef(session.id)
  const sessionRef      = useRef(session)
  const ctrlArmedRef    = useRef(ctrlArmed)
  const altArmedRef     = useRef(altArmed)
  const termIoRef       = useRef<ReturnType<typeof createTerminalIO> | null>(null)
  const termEpochRef    = useRef(0)
  const savedScrollRef  = useRef<Map<string, number>>(new Map())
  const isActiveRef     = useRef(isActive ?? true)
  // Stable handler refs — set during terminal setup, read by the isActive effect.
  const sendRawInputRef = useRef<((data: string) => void) | null>(null)
  const pasteActionRef  = useRef<(() => void) | null>(null)
  const focusActionRef  = useRef<(() => void) | null>(null)

  const [ghosttyReady, setGhosttyReady] = useState(false)
  const [termLoading,  setTermLoading]  = useState(true)
  const [wsState,      setWsState]      = useState<'connecting' | 'open' | 'lost'>('connecting')
  const [viewportSize, setViewportSize] = useState<TerminalSize | null>(null)
  const [scrolledUp,   setScrolledUp]  = useState(false)
  const SCROLL_THRESHOLD = 3
  const [ptySize,      setPtySize]     = useState<TerminalSize | null>(null)

  // Sync diagnostics
  const syncDiagRef = useRef<SyncDiag>({
    syncPhase: 'idle', scrollbackBytes: 0, scrollbackMsgs: 0,
    syncStartedAt: null, syncEndedAt: null, pendingWrite: false,
    wsState: 'connecting', reconnects: 0, prefetchBytes: 0,
    ghosttyScrollbackLines: 0, ghosttyScrollbackLimit: terminalOptions.scrollback,
  })
  const reconnectCountRef = useRef(0)
  // Route onSyncDiag through a ref so emitSyncDiag stays stable regardless of
  // which session is active (onSyncDiag is undefined for inactive sessions).
  const onSyncDiagRef = useRef(onSyncDiag)
  onSyncDiagRef.current = onSyncDiag
  const emitSyncDiag = useCallback((patch: Partial<SyncDiag>) => {
    syncDiagRef.current = { ...syncDiagRef.current, ...patch }
    onSyncDiagRef.current?.(syncDiagRef.current)
  }, []) // stable — reads prop via ref

  const viewportSizeRef = useRef<TerminalSize | null>(null)
  const ptySizeRef      = useRef<TerminalSize | null>(null)
  const resizeEchoGateRef = useRef<{
    awaitingEcho: TerminalSize | null
    dirty: boolean
    timer: ReturnType<typeof setTimeout> | null
  }>({ awaitingEcho: null, dirty: false, timer: null })
  const processViewportResizeRef = useRef<((forceDrive?: boolean) => void) | null>(null)

  // Keep refs current each render.
  currentSessionId.current = session.id
  isActiveRef.current      = isActive ?? true
  sessionRef.current       = session
  ctrlArmedRef.current     = ctrlArmed
  altArmedRef.current      = altArmed

  // Kick off ghostty-web WASM init (shared promise — safe to await multiple times).
  useEffect(() => {
    ghosttyInitPromise.then(() => setGhosttyReady(true))
  }, [])

  // ── Stable callbacks ──

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
    gate.timer = setTimeout(() => { releaseResizeEchoGate(size) }, 2000)
  }, [queueResize, releaseResizeEchoGate, resetResizeEchoGate])

  const processViewportResize = useCallback((forceDrive = false) => {
    if (!isActiveRef.current) return // skip when hidden — fitAddon can't measure
    const fit = fitAddonRef.current
    if (!fit) return
    const newVp   = measureTerminalFit(fit)
    const gate    = resizeEchoGateRef.current
    const decision = decideViewportResize({
      prevViewport: viewportSizeRef.current,
      ptySize:      ptySizeRef.current,
      newViewport:  newVp,
      awaitingEcho: gate.awaitingEcho != null,
      forceDrive,
    })
    if (decision.kind === 'wait') {
      viewportSizeRef.current = newVp
      gate.dirty = true
      return
    }
    setViewportSize(newVp); viewportSizeRef.current = newVp
    if (decision.kind === 'drive')  { applyOwnedResize(decision.size); return }
    if (decision.kind === 'follow') { queueResize(decision.size) }
  }, [applyOwnedResize, queueResize])

  processViewportResizeRef.current = processViewportResize

  const fitAndResize = useCallback(() => {
    if (!isActiveRef.current) return // skip when hidden
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
    if (target instanceof HTMLElement && target.closest('button, input, textarea, select, a, label, [role="button"]')) return
    focusTerminal()
  }, [focusTerminal])

  // ── Touch pan (separate hook — stable, runs once on mount) ──
  useTouchPan(shellRef, termRef, viewportSizeRef, ptySizeRef)

  // ── Viewport resize (separate hook — stable, runs once on mount) ──
  useViewportResize(shellRef, processViewportResizeRef, () => focusTerminalInput(termRef.current))

  // ── Terminal + keyboard setup ──
  useEffect(() => {
    if (!containerRef.current || USE_MOCK || !ghosttyReady) return
    disposed.current = false

    const term     = new Terminal(buildGhosttyOptions(terminalOptions))
    const fitAddon = new FitAddon()
    term.loadAddon(fitAddon)
    term.open(containerRef.current)
    term.registerLinkProvider(new UrlRegexProvider(term))
    term.registerLinkProvider(new OSC8LinkProvider(term))

    fitAddon.fit()
    const initialVp = measureTerminalFit(fitAddon)
    setViewportSize(initialVp); viewportSizeRef.current = initialVp

    termRef.current     = term
    fitAddonRef.current = fitAddon
    termIoRef.current   = createTerminalIO(term, {
      getState() {
        const scrollbackLen = term.getScrollbackLength()
        const gvY           = Math.floor(term.getViewportY())
        return { viewportY: scrollbackLen - gvY, baseY: scrollbackLen, rows: term.rows }
      },
      scrollToLine(line) {
        const s = term.getScrollbackLength()
        term.scrollToLine(s - line)
      },
      scrollToBottom() { term.scrollToBottom() },
      getLine(y) {
        const line = term.buffer.active.getLine(y)
        if (!line) return null
        const text = line.translateToString(true)
        return text.trim().length < 4 ? null : text
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
      ghosttyScrollbackLines: term.getScrollbackLength(),
    })

    const sendRawInput = (data: string) => {
      const ws = wsRef.current
      if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(new TextEncoder().encode(data))
        termRef.current?.focus()
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

    // Store handler refs so the isActive effect can register/deregister them.
    sendRawInputRef.current = sendRawInput
    pasteActionRef.current  = () => {
      void handlePasteAction({
        sessionId:           session.id,
        bracketedPasteMode:  term.hasBracketedPaste(),
        feedback:            defaultPasteFeedback,
        emit:                sendRawInput,
      })
    }
    focusActionRef.current = () => focusTerminalInput(term)

    // Blocker 2 fix: register immediately if already active.
    if (isActiveRef.current) {
      onInputReady?.(sendRawInput)
      onPasteReady?.(pasteActionRef.current)
      onFocusReady?.(focusActionRef.current)
    }

    const dataDisposable       = term.onData(sendInput)
    const disposeKeyboard      = attachKeyboardHandler(term, sendInput, sendRawInput, keybinds, macCommandIsCtrl, session.id)
    const disposePaste         = attachPasteHandler(term, containerRef.current!, sendRawInput, session.id)
    const disposeMobile        = attachMobileInputHandler(term, containerRef.current!, sendRawInput)

    const scrollDisposable = term.onScroll(() => {
      if (!isActiveRef.current) return
      const gvY = Math.floor(term.getViewportY())
      setScrolledUp(gvY > SCROLL_THRESHOLD)
      emitSyncDiag({ ghosttyScrollbackLines: term.getScrollbackLength() })
    })

    const handleGlobalKeydown = (ev: KeyboardEvent) => {
      if (!isActiveRef.current) return
      const tag = (ev.target as HTMLElement)?.tagName
      if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return
      if (containerRef.current?.contains(ev.target as Node)) return
      term.focus()
    }
    window.addEventListener('keydown', handleGlobalKeydown, true)

    return () => {
      disposed.current = true
      window.removeEventListener('keydown', handleGlobalKeydown, true)
      disposePaste()
      disposeMobile()
      disposeKeyboard()
      dataDisposable.dispose()
      scrollDisposable.dispose()
      setScrolledUp(false)
      if (reconnectTimer.current) clearTimeout(reconnectTimer.current)
      wsRef.current?.close()
      wsRef.current = null
      onInputReady?.(null)
      onPasteReady?.(null)
      onFocusReady?.(null)
      sendRawInputRef.current = null
      pasteActionRef.current  = null
      focusActionRef.current  = null
      ;(window as any).__gmuxTerm   = null
      ;(window as any).__gmuxInject = null
      ;(window as any).__gmuxDiag   = null
      term.dispose()
      termRef.current    = null
      fitAddonRef.current = null
      termIoRef.current  = null
    }
  }, [onCtrlConsumed, ghosttyReady])

  // ── isActive: register/deregister callbacks + fit on activation ──
  useEffect(() => {
    const active = isActive ?? true
    if (!active) {
      onInputReady?.(null)
      onFocusReady?.(null)
      onPasteReady?.(null)
      return
    }
    if (sendRawInputRef.current) onInputReady?.(sendRawInputRef.current)
    if (focusActionRef.current)  onFocusReady?.(focusActionRef.current)
    if (pasteActionRef.current)  onPasteReady?.(pasteActionRef.current)
    onSyncDiag?.(syncDiagRef.current)
    requestAnimationFrame(() => {
      if (!isActiveRef.current) return
      fitAndResize()
      focusTerminalInput(termRef.current)
    })
    return () => {
      onInputReady?.(null)
      onFocusReady?.(null)
      onPasteReady?.(null)
    }
  }, [isActive, onInputReady, onFocusReady, onPasteReady, onSyncDiag, fitAndResize])

  // ── WebSocket connection ──
  useWebSocket({
    session, ghosttyReady,
    termRef, termIoRef, wsRef, reconnectTimer, disposed, currentSessionId,
    sessionRef, termEpochRef, savedScrollRef, reconnectCountRef,
    ptySizeRef, viewportSizeRef,
    queueData, queueMany, queueResize,
    resetResizeEchoGate, releaseResizeEchoGate, fitAndResize, emitSyncDiag,
    setPtySize, setViewportSize, setWsState, setTermLoading,
    scrollbackLimit: terminalOptions.scrollback,
  })

  // ── Render ──

  const showDisconnectedPill = wsState === 'lost'
  const showResizePill = !showDisconnectedPill
    && session.alive
    && ptySize != null && viewportSize != null
    && (viewportSize.cols !== ptySize.cols || viewportSize.rows !== ptySize.rows)

  if (USE_MOCK) return <MockTerminal sessionId={session.id} />

  return (
    <div
      ref={shellRef}
      class={`terminal-shell ${showResizePill ? 'terminal-shell-passive' : ''}`}
      onClick={handleShellClick}
      style={{ display: (isActive ?? true) ? '' : 'none' }}
    >
      {showDisconnectedPill && (
        <div class="terminal-resize-anchor">
          <div class="terminal-disconnected-pill">Connection lost, reconnecting…</div>
        </div>
      )}
      {showResizePill && (
        <div class="terminal-resize-anchor">
          <button type="button" class="terminal-resize-overlay" onClick={() => fitAndResize()}>
            Sized for another device, click to resize
          </button>
        </div>
      )}
      <div ref={containerRef} class="terminal-container" />
      {termLoading && (
        <div class="terminal-loading">Waiting for output…</div>
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
        theme:        TERM_THEME,
        fontFamily:   "'Fira Code', monospace",
        fontSize:     13,
        disableStdin: true,
        cursorBlink:  false,
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
