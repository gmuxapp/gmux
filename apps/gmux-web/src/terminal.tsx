import { useCallback, useEffect, useRef, useState } from 'preact/hooks'
import type { WTerm } from '@wterm/dom'
import type { ResolvedTerminalOptions } from './settings-schema'
import { attachKeyboardHandler, attachPasteHandler, ctrlSequenceFor, defaultPasteFeedback, handlePasteAction } from './keyboard'
import { DEFAULT_THEME_COLORS, type ResolvedKeybind } from './config'
import { focusTerminalInput, useTouchPan } from './terminal-touch'
import { getSelectionText, clearSelection, selectAllAndCopy } from './selection'
import { handleTerminalLinkClick } from './terminal-links'
import { createReplayBuffer } from './replay'
import { createTerminalIO, type TerminalSize } from './terminal-io'
import { measureTerminalFit } from './terminal-fit'
import { applyWtermTheme } from './terminal-theme'
import { decideViewportResize, sameSize, useViewportResize } from './terminal-resize'
import { getGhosttyCore } from './terminal-init'
import { useWebSocket } from './use-websocket'
import type { Session } from './types'
import type { ITheme } from './types'

export type { SyncDiag } from './terminal-types'
import type { SyncDiag } from './terminal-types'

// ── Config ──

/**
 * Re-export for backward compat (used by input-diagnostics.tsx).
 */
export const TERM_THEME: ITheme = DEFAULT_THEME_COLORS

// ── File-link interceptor ──
//
// Browsers block window.open("file://...", "_blank") for security.
// Intercept those calls and POST to /v1/open-path instead.
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

function announceResize(ws: WebSocket | null, dims: TerminalSize): void {
  if (!ws || ws.readyState !== WebSocket.OPEN) return
  ws.send(JSON.stringify({ type: 'resize', cols: dims.cols, rows: dims.rows }))
}

// ── TerminalView ──

/**
 * Single wterm Terminal instance with reconnecting WebSocket.
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
  onCopyReady,
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
  /** Called with a function that copies the active terminal selection (or
   *  selects-all + copies when nothing is selected). Null on cleanup. */
  onCopyReady?: (copy: (() => void) | null) => void
  onSyncDiag?: (diag: SyncDiag) => void
  /** When false, the terminal is hidden (display:none) and its WS stays open.
   *  Callbacks (onInputReady etc.) are deregistered while inactive so the
   *  parent routes input/focus to the visible session only. Defaults to true. */
  isActive?: boolean
}) {
  const shellRef     = useRef<HTMLDivElement>(null)
  const containerRef = useRef<HTMLDivElement>(null)
  const termRef      = useRef<WTerm | null>(null)
  const wsRef        = useRef<WebSocket | null>(null)
  const reconnectTimer  = useRef<ReturnType<typeof setTimeout> | null>(null)
  const disposed        = useRef(false)
  const currentSessionId = useRef(session.id)
  const sessionRef      = useRef(session)
  const ctrlArmedRef    = useRef(ctrlArmed)
  const altArmedRef     = useRef(altArmed)
  const termIoRef       = useRef<ReturnType<typeof createTerminalIO> | null>(null)
  const termEpochRef    = useRef(0)
  const isActiveRef     = useRef(isActive ?? true)
  // Stable handler refs — set during terminal setup, read by the isActive effect.
  const sendRawInputRef = useRef<((data: string) => void) | null>(null)
  const pasteActionRef  = useRef<(() => void) | null>(null)
  const focusActionRef  = useRef<(() => void) | null>(null)
  const copyActionRef   = useRef<(() => void) | null>(null)

  const [termReady,    setTermReady]   = useState(false)
  const [termLoading,  setTermLoading] = useState(true)
  const [wsState,      setWsState]     = useState<'connecting' | 'open' | 'lost'>('connecting')
  const [viewportSize, setViewportSize] = useState<TerminalSize | null>(null)
  const [scrolledUp,   setScrolledUp]  = useState(false)
  const [ptySize,      setPtySize]     = useState<TerminalSize | null>(null)

  // Sync diagnostics
  const syncDiagRef = useRef<SyncDiag>({
    syncPhase: 'idle', scrollbackBytes: 0, scrollbackMsgs: 0,
    syncStartedAt: null, syncEndedAt: null, pendingWrite: false,
    wsState: 'connecting', reconnects: 0, prefetchBytes: 0,
    scrollbackLines: 0, scrollbackLimit: terminalOptions.scrollback,
  })
  const reconnectCountRef = useRef(0)
  const onSyncDiagRef = useRef(onSyncDiag)
  onSyncDiagRef.current = onSyncDiag
  const emitSyncDiag = useCallback((patch: Partial<SyncDiag>) => {
    syncDiagRef.current = { ...syncDiagRef.current, ...patch }
    onSyncDiagRef.current?.(syncDiagRef.current)
  }, [])

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

  // ── Stable callbacks ──

  const queueResize = useCallback((size: TerminalSize) => {
    termIoRef.current?.resize(size, termEpochRef.current)
  }, [])

  const queueData = useCallback((data: Uint8Array, onWritten?: () => void) => {
    if (onWritten) {
      termIoRef.current?.writeMany([data], termEpochRef.current, onWritten)
    } else {
      termIoRef.current?.write(data, termEpochRef.current)
    }
  }, [])

  const queueMany = useCallback((chunks: Uint8Array[], onWritten?: () => void) => {
    termIoRef.current?.writeMany(chunks, termEpochRef.current, onWritten)
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
    if (!isActiveRef.current) return
    const term = termRef.current
    const container = containerRef.current
    if (!term || !container) return
    const newVp   = measureTerminalFit(term, container)
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
    if (!isActiveRef.current) return
    const term = termRef.current
    const container = containerRef.current
    if (!term || !container) return
    const dims = measureTerminalFit(term, container)
    setViewportSize(dims); viewportSizeRef.current = dims
    if (!dims) return
    applyOwnedResize(dims)
  }, [applyOwnedResize])

  const focusTerminal = useCallback(() => {
    focusTerminalInput(termRef.current)
  }, [])

  const handleShellClick = useCallback((ev: MouseEvent) => {
    // Don't steal focus when the user just made a text selection.
    if (!(window.getSelection()?.isCollapsed ?? true)) return
    const target = ev.target
    if (target instanceof HTMLElement && target.closest('button, input, textarea, select, a, label, [role="button"]')) return
    // Check for a URL at the click position before stealing focus.
    if (handleTerminalLinkClick(ev)) return
    focusTerminal()
  }, [focusTerminal])

  // ── Touch pan (separate hook — stable, runs once on mount) ──
  useTouchPan(shellRef, termRef, viewportSizeRef, ptySizeRef, containerRef)

  // ── Viewport resize (separate hook — stable, runs once on mount) ──
  useViewportResize(shellRef, processViewportResizeRef, () => focusTerminalInput(termRef.current))

  // ── Terminal init ──
  useEffect(() => {
    if (!containerRef.current) return
    let cancelled = false
    let term: WTerm | null = null
    let cleanupKeyboard: (() => void) | null = null
    let cleanupPaste: (() => void) | null = null

    const run = async () => {
      const { WTerm } = await import('@wterm/dom')
      const core = await getGhosttyCore()
      if (cancelled || !containerRef.current) return

      term = new WTerm(containerRef.current, {
        core,
        autoResize: false,    // gmux owns resize decisions
        cursorBlink: terminalOptions.cursorBlink,
      })

      applyWtermTheme(term.element, terminalOptions)
      await term.init()
      if (cancelled) { term.destroy(); return }

      ;(window as any).__gmuxTerm = term
      ;(window as any).__gmuxInject = (b64: string) => {
        const bin = atob(b64)
        const bytes = new Uint8Array(bin.length)
        for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i)
        termIoRef.current?.write(bytes, termEpochRef.current)
      }
      ;(window as any).__gmuxDiag = () => ({
        ...syncDiagRef.current,
        scrollbackLines: term?.bridge?.getScrollbackCount() ?? 0,
      })

      // Scroll event — track scrolled-up state and emit diag
      const handleScroll = () => {
        if (!isActiveRef.current) return
        const el = term!.element
        const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 5
        setScrolledUp(!atBottom)
        emitSyncDiag({ scrollbackLines: term?.bridge?.getScrollbackCount() ?? 0 })
      }
      term.element.addEventListener('scroll', handleScroll, { passive: true })

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

      term.onData = sendInput

      termRef.current   = term
      termIoRef.current = createTerminalIO(term)
      setTermReady(true)

      cleanupKeyboard = attachKeyboardHandler(term, sendInput, sendRawInput, keybinds, macCommandIsCtrl, session.id)
      cleanupPaste    = attachPasteHandler(term, containerRef.current!, sendRawInput, session.id)

      // Store handler refs so the isActive effect can register/deregister them.
      sendRawInputRef.current = sendRawInput
      pasteActionRef.current  = () => {
        void handlePasteAction({
          sessionId:           session.id,
          bracketedPasteMode:  term!.bridge?.bracketedPaste() ?? false,
          feedback:            defaultPasteFeedback,
          emit:                sendRawInput,
        })
      }
      focusActionRef.current = () => focusTerminalInput(term!)
      copyActionRef.current  = () => {
        const text = getSelectionText()
        if (text) {
          navigator.clipboard.writeText(text).catch(() => {})
          clearSelection()
        } else {
          selectAllAndCopy(term!.element)
        }
      }

      // Register if already active.
      if (isActiveRef.current) {
        onInputReady?.(sendRawInput)
        onPasteReady?.(pasteActionRef.current)
        onFocusReady?.(focusActionRef.current)
        onCopyReady?.(copyActionRef.current)
      }

      const handleGlobalKeydown = (ev: KeyboardEvent) => {
        if (!isActiveRef.current) return
        const tag = (ev.target as HTMLElement)?.tagName
        if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return
        if (containerRef.current?.contains(ev.target as Node)) return
        term!.focus()
      }
      window.addEventListener('keydown', handleGlobalKeydown, true)

      // Store cleanup for scroll listener and global keydown
      ;(term as any).__gmuxScrollCleanup = () => {
        term!.element.removeEventListener('scroll', handleScroll)
        window.removeEventListener('keydown', handleGlobalKeydown, true)
      }
    }

    run().catch(console.error)

    return () => {
      cancelled = true
      if (reconnectTimer.current) clearTimeout(reconnectTimer.current)
      wsRef.current?.close()
      wsRef.current = null
      onInputReady?.(null)
      onPasteReady?.(null)
      onFocusReady?.(null)
      onCopyReady?.(null)
      sendRawInputRef.current = null
      pasteActionRef.current  = null
      focusActionRef.current  = null
      copyActionRef.current   = null
      ;(window as any).__gmuxTerm   = null
      ;(window as any).__gmuxInject = null
      ;(window as any).__gmuxDiag   = null
      cleanupKeyboard?.()
      cleanupPaste?.()
      ;(term as any)?.__gmuxScrollCleanup?.()
      term?.destroy()
      termRef.current    = null
      termIoRef.current  = null
      setTermReady(false)
    }
  }, [onCtrlConsumed]) // re-init only if fundamentally replaced

  // ── isActive: register/deregister callbacks + fit on activation ──
  useEffect(() => {
    const active = isActive ?? true
    if (!active) {
      onInputReady?.(null)
      onFocusReady?.(null)
      onPasteReady?.(null)
      onCopyReady?.(null)
      return
    }
    if (sendRawInputRef.current) onInputReady?.(sendRawInputRef.current)
    if (focusActionRef.current)  onFocusReady?.(focusActionRef.current)
    if (pasteActionRef.current)  onPasteReady?.(pasteActionRef.current)
    if (copyActionRef.current)   onCopyReady?.(copyActionRef.current)
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
      onCopyReady?.(null)
    }
  }, [isActive, onInputReady, onFocusReady, onPasteReady, onCopyReady, onSyncDiag, fitAndResize])

  // ── WebSocket connection ──
  useWebSocket({
    session, ghosttyReady: termReady,
    termRef, termIoRef, wsRef, reconnectTimer, disposed, currentSessionId,
    sessionRef, termEpochRef, reconnectCountRef,
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
          onClick={() => {
            const el = termRef.current?.element
            if (el) el.scrollTop = el.scrollHeight
          }}
          title="Scroll to bottom"
        >
          End ↓
        </button>
      )}
    </div>
  )
}

