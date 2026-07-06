import { useCallback, useEffect, useRef, useState } from 'preact/hooks'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { ImageAddon } from '@xterm/addon-image'
import { WebLinksAddon } from '@xterm/addon-web-links'
import { SearchAddon } from '@xterm/addon-search'
import type { ResolvedTerminalOptions } from './settings-schema'
import { loadWebglRenderer } from './webgl-renderer'
import { refreshAtlasWhenIconFontLoads } from './nerd-font'
import { applyArmedModifiers, attachKeyboardHandler, attachPasteHandler, defaultPasteFeedback, handlePasteAction } from './keyboard'
import { DEFAULT_THEME_COLORS, type ResolvedKeybind } from './config'
import { attachMobileInputHandler } from './mobile-input'
import { isTouchDevice } from './touch'
import { createReplayBuffer } from './replay'
import { createTerminalIO, type TerminalSize } from './terminal-io'
import { linkAtPoint, type LinkInfo, openLinkAtPoint } from './terminal-link'
import { createLongPressRecognizer } from './long-press'
import { LinkActionSheet } from './link-action-sheet'
import { TerminalTextSheet } from './terminal-text-sheet'
import { pressedBufferRow, readTerminalText } from './terminal-text'
import { decideViewportResize, sameSize } from './terminal-resize'
import { wsStateOnClose, wsStateOnOutput, type WsState } from './ws-state'
import { terminalFindOpen, terminalScrolledUp, terminalScrollToBottom } from './store'
import { TerminalFindBar } from './terminal-find'
import { MOCK_BY_ID } from './mock-data/index'
import type { Session } from './types'

// ── Config ──

const USE_MOCK = import.meta.env.VITE_MOCK === '1' || location.search.includes('mock')

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

  // Measure the shell, the stable flex-allocated viewport. Use offsetWidth/
  // offsetHeight, NOT clientWidth/clientHeight: the shell is the scroll
  // container (overflow: auto), so client* shrinks when a scrollbar appears.
  // Measuring client* creates a feedback loop — e.g. after a DPR change
  // (moving the window between monitors) the grid can overflow by 1px,
  // a scrollbar appears, client* shrinks, we refit smaller, the scrollbar
  // disappears, client* grows, we refit bigger, the grid overflows again,
  // forever. offset* is the border-box and ignores scrollbars, so the
  // measurement is a fixed point regardless of transient overflow.
  // (.terminal-shell has no border/padding, so offset* == the viewport.)
  // On mobile the control bar floats over the terminal's bottom (out of
  // flow, translucent — see styles.css), so the shell fills the full height
  // behind it. Reserve the bar's height, but round the row count UP: the
  // terminal then claims one extra row whose bottom sliver tucks behind the
  // translucent keys, instead of leaving a sub-cell gap above an opaque bar.
  // Detected here — not at the call sites — so every resize path (initial
  // fit, keyboard transitions, manual refit) computes identically. The bar's
  // offsetParent is null when it's display:none (desktop) ⇒ plain floor fit.
  const bar = document.querySelector<HTMLElement>('.mobile-bottom-bar')
  const overlayBar = bar?.offsetParent ? bar.offsetHeight : 0

  const availW = shellEl.offsetWidth - padX - reserveWidth
  const availH = shellEl.offsetHeight - padY - overlayBar

  let cols = Math.max(2, Math.floor(availW / dims.css.cell.width))
  let rows = Math.max(1, (overlayBar > 0 ? Math.ceil : Math.floor)(availH / dims.css.cell.height))

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
  // Same guard vertically: row height rounding across device/css pixels can
  // overflow by 1px at fractional DPRs (the monitor-move case), which is
  // exactly what seeds the scrollbar flicker described above.
  // (Skipped in overlay-bar mode: the gained row intentionally exceeds
  // availH, spilling its bottom sliver behind the translucent bar.)
  if (overlayBar === 0 && dims.device.cell.height > 0) {
    const predictedHeight = Math.round(dims.device.cell.height * rows / dpr)
    if (predictedHeight > availH && rows > 1) rows--
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

function getResizeSignalPixels(host: HTMLElement | null, vv: VisualViewport | null): { width: number; height: number } {
  if (host) {
    // offset* not client*: must match measureTerminalFit so scrollbar
    // appearance/disappearance doesn't register as a size change.
    return {
      width: host.offsetWidth,
      height: host.offsetHeight,
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

  if (!isTouchDevice()) return

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
 * persisted scrollback (an on-disk append-only file that rotates at ~1 MiB,
 * not a ring buffer) replays on connect, so history is preserved without
 * keeping per-session xterm instances alive.
 *
 * Resize model: selecting a session claims ownership — the first WS connect
 * resizes the PTY to fit this browser's viewport. While driving, viewport
 * resize sends are gated by the matching terminal_resize echo from the server,
 * so drag-resize stays responsive without flooding. If another source (local
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
  onFocusReady,
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
  onFocusReady?: (focus: (() => void) | null) => void
}) {
  const shellRef = useRef<HTMLDivElement>(null)
  const containerRef = useRef<HTMLDivElement>(null)
  const termRef = useRef<Terminal | null>(null)
  const wsRef = useRef<WebSocket | null>(null)
  const reconnectTimer = useRef<ReturnType<typeof setTimeout> | null>(null)
  const disposed = useRef(false)
  const currentSessionId = useRef(session.id)
  const sessionRef = useRef(session)
  const ctrlArmedRef = useRef(ctrlArmed)
  const altArmedRef = useRef(altArmed)
  const termIoRef = useRef<ReturnType<typeof createTerminalIO> | null>(null)
  const searchAddonRef = useRef<SearchAddon | null>(null)
  const termEpochRef = useRef(0)

  // True once the terminal's font is downloaded; gates xterm mount.
  // See the preload effect below for why this matters.
  const [fontReady, setFontReady] = useState(false)

  const [termLoading, setTermLoading] = useState(true)
  const [wsState, setWsState] = useState<WsState>('connecting')
  const [viewportSize, setViewportSize] = useState<TerminalSize | null>(null)
  const [linkSheet, setLinkSheet] = useState<LinkInfo | null>(null)
  const [textSheet, setTextSheet] = useState<{ lines: string[]; anchorRow: number } | null>(null)
  // The paste trigger lives in the attach effect (it reads bracketed-paste
  // mode + clipboard fresh), so bridge it out to the sheet's Paste button
  // via a ref.
  const pasteActionRef = useRef<(() => void) | null>(null)
  const SCROLL_THRESHOLD = 3 // rows above bottom before showing the button
  // Track the last PTY size we know about so we can derive the pill.
  const [ptySize, setPtySize] = useState<TerminalSize | null>(null)

  // Refs shadow viewportSize/ptySize for use inside event handlers that
  // must not trigger effect re-runs but need current values.
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

    // Optimistically sync ptySize so the pill hides immediately, before the
    // server echoes the resize back. Without this, ptySize would lag behind
    // viewportSize for one round-trip, causing a spurious pill flash.
    setPtySize(size); ptySizeRef.current = size
    queueResize(size)

    if (sameSize(prevPty, size)) return

    // A new outbound resize supersedes any older echo wait or pending dirty
    // viewport event. The server echo for this exact size re-opens the gate.
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
    const term = termRef.current
    const shell = shellRef.current
    if (!term || !shell) return

    const newVp = measureTerminalFit(term, shell)
    const gate = resizeEchoGateRef.current
    const decision = decideViewportResize({
      prevViewport: viewportSizeRef.current,
      ptySize: ptySizeRef.current,
      newViewport: newVp,
      awaitingEcho: gate.awaitingEcho != null,
      forceDrive,
    })

    if (decision.kind === 'wait') {
      // Keep the ref fresh for the next decision, but skip the React state
      // update so the pill doesn't flash while we wait for the echo.
      viewportSizeRef.current = newVp
      gate.dirty = true
      return
    }

    setViewportSize(newVp); viewportSizeRef.current = newVp

    if (decision.kind === 'drive') {
      // Viewport matched PTY, or we were already driving and just finished
      // waiting for the previous echo. Resize xterm now, then wait for the
      // server echo before sending the next viewport change.
      applyOwnedResize(decision.size)
      return
    }

    if (decision.kind === 'follow') {
      // Out of sync (pill visible), keep xterm at the PTY size.
      queueResize(decision.size)
    }
  }, [applyOwnedResize, queueResize])

  processViewportResizeRef.current = processViewportResize

  // Resize xterm to fit the viewport and announce the new size to the backend.
  const fitAndResize = useCallback(() => {
    const term = termRef.current
    const shell = shellRef.current
    if (!term || !shell) return

    const dims = measureTerminalFit(term, shell)
    setViewportSize(dims); viewportSizeRef.current = dims
    if (!dims) return

    applyOwnedResize(dims)
  }, [applyOwnedResize])

  const focusTerminal = useCallback(() => {
    focusTerminalInput(termRef.current)
  }, [])

  // A tap on the shell *outside* the rendered grid (the strip that slides
  // under the translucent toolbar, including the empty key-row corners)
  // would let the browser's synthesized mousedown blur the textarea and
  // dismiss the soft keyboard. Hold focus there by cancelling the default,
  // mirroring the toolbar's keepFocus. The grid (.xterm) manages its own
  // focus, so leave those taps untouched. Touch-only: there's no soft
  // keyboard to protect off-touch, and cancelling mousedown there would
  // only suppress focus/selection on shell controls for no benefit.
  const holdShellFocus = useCallback((ev: MouseEvent) => {
    if (!isTouchDevice()) return
    // The find bar's input/buttons need default mousedown behavior to
    // gain focus, so exempt it alongside the grid.
    if (!(ev.target instanceof Element) || !ev.target.closest('.xterm, .terminal-find-bar')) ev.preventDefault()
  }, [])

  const handleShellClick = useCallback((ev: MouseEvent) => {
    // Touch focuses the terminal via the touchend handler (a deliberate
    // tap opens the keyboard). Ignore synthesized clicks here so a click
    // falling through from a just-dismissed sheet can't reopen it.
    if (isTouchDevice()) return
    const target = ev.target
    if (target instanceof HTMLElement && target.closest('button, input, textarea, select, a, label, [role="button"]')) {
      return
    }
    focusTerminal()
  }, [focusTerminal])

  // Mirror the resolved terminal background into CSS (--terminal-bg) so the
  // overlay fade and the shell/container fills match a themed background
  // instead of a hard-coded literal. Falls back to the default in CSS when
  // unset, so behaviour is unchanged for the default theme.
  useEffect(() => {
    const bg = terminalOptions.theme.background
    if (shellRef.current && bg) shellRef.current.style.setProperty('--terminal-bg', bg)
  }, [terminalOptions.theme.background])

  // Force-fetch the terminal font before mounting xterm.
  //
  // xterm picks its cell metrics from the first measurement it takes
  // inside term.open(). If the woff2 hasn't downloaded yet, that
  // measurement uses fallback monospace metrics (cell ≈ 18 px). xterm
  // re-measures internally when the real font arrives a few ms later
  // (cell ≈ 17 px) and the rendered grid shrinks, but the row count we
  // derived from the original measurement doesn't get recomputed,
  // leaving an extra row's worth of unused space at the bottom of the
  // viewport.
  //
  // document.fonts.ready isn't enough: @fontsource only registers the
  // @font-face declarations, so nothing is in flight at mount and ready
  // resolves immediately. document.fonts.load(spec) actually triggers
  // the fetch and resolves once the bytes are in.
  //
  // .finally rather than .then so a fetch failure (offline, flaky network,
  // CSP) still unblocks the gate. xterm falls back to monospace metrics in
  // that case, which is much better UX than a terminal stuck on the
  // loading overlay forever.
  useEffect(() => {
    let cancelled = false
    const spec = `${terminalOptions.fontSize}px ${terminalOptions.fontFamily}`
    document.fonts.load(spec).finally(() => {
      if (!cancelled) setFontReady(true)
    })
    return () => { cancelled = true }
  }, [terminalOptions.fontFamily, terminalOptions.fontSize])

  // Terminal + keyboard setup (stable across session changes).
  useEffect(() => {
    if (!containerRef.current || USE_MOCK || !fontReady) return
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
    // Find-in-terminal (the find bar drives it; see terminal-find.tsx).
    const searchAddon = new SearchAddon()
    term.loadAddon(searchAddon)
    searchAddonRef.current = searchAddon
    term.open(containerRef.current)
    loadWebglRenderer(term)
    // The Nerd Font icon fallback loads lazily; refresh the glyph atlas once
    // it arrives so icons rasterized as tofu beforehand get redrawn.
    const disposeIconFontWatch = refreshAtlasWhenIconFontLoads(term, terminalOptions.fontSize)
    // Initial fit: use FitAddon for the first resize (before shellRef is
    // guaranteed stable), then switch to measureTerminalFit for everything after.
    fitAddon.fit()
    const initialVp = shellRef.current ? measureTerminalFit(term, shellRef.current) : getProposedTerminalSize(fitAddon)
    setViewportSize(initialVp); viewportSizeRef.current = initialVp
    termRef.current = term
    termIoRef.current = createTerminalIO(term, {
      getState() {
        const buf = term.buffer.active
        return { viewportY: buf.viewportY, baseY: buf.baseY, rows: term.rows }
      },
      scrollToLine(line: number) { term.scrollToLine(line) },
      scrollToBottom() { term.scrollToBottom() },
      getLine(y: number): string | null {
        const line = term.buffer.active.getLine(y)
        if (!line) return null
        const text = line.translateToString(true)
        // Filter trivial anchors so a wipe-and-redraw doesn't snap the
        // user to the first stretch of separators or whitespace it
        // finds. Four visible chars is enough to be distinctive without
        // excluding short but meaningful lines ("DONE", "PASS", etc.).
        if (text.trim().length < 4) return null
        return text
      },
    })
    ;(window as any).__gmuxTerm = term
    // Test-only inject hook: pumps bytes through the same path as ws.onmessage
    // (createTerminalIO.enqueue) bypassing the WebSocket and replay buffer.
    // Used by e2e/tests/terminal-scroll.spec.ts to exercise scroll preservation
    // against real xterm with deterministic byte sequences and frame boundaries.
    ;(window as any).__gmuxInject = (b64: string) => {
      const bin = atob(b64)
      const bytes = new Uint8Array(bin.length)
      for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i)
      termIoRef.current?.enqueue(bytes, termEpochRef.current)
    }

    const sendRawInput = (data: string) => {
      const ws = wsRef.current
      if (!ws || ws.readyState !== WebSocket.OPEN) return
      // Only re-assert focus if the terminal already had it (keyboard
      // open). Grabbing focus unconditionally would pop the on-screen
      // keyboard on every toolbar key, even when it was closed — the
      // whole point of the toolbar is to work with the keyboard down.
      const hadFocus = document.activeElement === term.textarea
      ws.send(new TextEncoder().encode(data))
      if (hadFocus) term.focus()
    }

    const sendInput = (data: string) => {
      const r = applyArmedModifiers(data, ctrlArmedRef.current, altArmedRef.current)
      if (r.ctrlApplied) { ctrlArmedRef.current = false; onCtrlConsumed() }
      if (r.altApplied) { altArmedRef.current = false; onAltConsumed() }
      sendRawInput(r.seq)
    }

    onInputReady?.(sendRawInput)
    terminalScrollToBottom.value = () => term.scrollToBottom()
    // The paste trigger reads bracketedPasteMode and the clipboard fresh
    // on every invocation: bracketed mode flips at runtime as TUIs come
    // and go, and the clipboard contents are obviously volatile. Sharing
    // handlePasteAction with the keybind path means long-press paste gets
    // binary-paste support without divergent code.
    pasteActionRef.current = () => {
      void handlePasteAction({
        sessionId: session.id,
        bracketedPasteMode: term.modes.bracketedPasteMode,
        feedback: defaultPasteFeedback,
        emit: sendRawInput,
      })
    }
    onFocusReady?.(() => focusTerminalInput(term))

    const dataDisposable = term.onData((data) => sendInput(data))
    attachKeyboardHandler(term, sendInput, sendRawInput, keybinds, macCommandIsCtrl, session.id)
    const disposePasteHandler = attachPasteHandler(term, containerRef.current!, sendRawInput, session.id)
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
        navigator.clipboard.writeText(text).catch(() => {/* clipboard write can fail silently */})
      } catch {
        // invalid base64; ignore
      }
      return true
    })

    const scrollDisposable = term.onScroll(() => {
      const buf = term.buffer.active
      terminalScrolledUp.value = buf.baseY - buf.viewportY > SCROLL_THRESHOLD
    })

    const handleGlobalKeydown = (ev: KeyboardEvent) => {
      const tag = (ev.target as HTMLElement)?.tagName
      if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return
      if (containerRef.current?.contains(ev.target as Node)) return
      term.focus()
    }
    window.addEventListener('keydown', handleGlobalKeydown, true)

    const shell = shellRef.current
    // Overlays (the link action sheet) render inside the shell; their
    // touches must not arm tap/pan/long-press handling.
    const isInteractiveTarget = (target: EventTarget | null) => target instanceof HTMLElement
      && !!target.closest('button, input, textarea, select, a, label, [role="button"], .modal-backdrop')

    // Long-press on a link → action sheet (copy / open / inspect the
    // real target of OSC 8 hyperlinks). A ≥500ms hold is a distinct
    // intent from a tap: even when nothing is under the finger, the
    // release must not open a link or toggle the keyboard.
    const longPress = createLongPressRecognizer((x, y) => {
      const link = linkAtPoint(term, x, y)
      try { navigator.vibrate?.(10) } catch { /* unsupported */ }
      // On a link: offer open/copy. On empty space: open the text sheet —
      // the buffer as natively-selectable text, scrolled to the pressed
      // row, with Paste at the bottom.
      if (link) { setLinkSheet(link); return }
      const lines = readTerminalText(term)
      const anchorRow = Math.max(0, Math.min(pressedBufferRow(term, y), lines.length - 1))
      setTextSheet({ lines, anchorRow })
    })
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
        longPress.cancel()
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
      longPress.start(touchPanState.startX, touchPanState.startY)
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
        longPress.cancel()
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

    const handleTouchEndCapture = (ev: TouchEvent) => {
      // A fired long-press owns this touch: suppress tap behavior and
      // the browser's synthesized cascade.
      if (longPress.end()) {
        ev.preventDefault()
        touchPanState.active = false
        touchPanState.moved = false
        return
      }

      if (touchPanState.active && !touchPanState.moved) {
        // Tap on a link opens it by driving xterm's Linkifier with a
        // synthetic mousemove/mousedown/mouseup handshake (see
        // terminal-link.ts for why the browser's own synthesized cascade
        // is unreliable here, especially on iOS). preventDefault stops
        // the browser from synthesizing its own cascade for this touch,
        // so the link can't be activated twice. When a link opens, skip
        // the keyboard focus/scroll — the tap was navigation, not input
        // intent.
        if (openLinkAtPoint(term, touchPanState.startX, touchPanState.startY)) {
          ev.preventDefault()
          touchPanState.active = false
          touchPanState.moved = false
          return
        }

        focusTerminalInput(term)
        // No eager scroll-to-bottom here: the keyboard-open viewport
        // shrink triggers one PTY reflow that already lands at the
        // bottom. A scrollToBottom here would fire mid-slide (its
        // setTimeout is deferred by the focus/layout work to ~30% of
        // the keyboard animation), producing a redundant scroll jump
        // before the reflow does the same thing again.
      }
      touchPanState.active = false
      touchPanState.moved = false
    }

    const clearTouchPan = () => {
      longPress.end() // full reset: discard pending and fired state
      touchPanState.active = false
      touchPanState.moved = false
    }

    shell?.addEventListener('touchstart', handleTouchStartCapture, { capture: true, passive: false })
    shell?.addEventListener('touchmove', handleTouchMoveCapture, { capture: true, passive: false })
    shell?.addEventListener('touchend', handleTouchEndCapture, { capture: true, passive: false })
    shell?.addEventListener('touchcancel', clearTouchPan, true)

    // Resize strategy (no debounce — two natural throttles make it
    // unnecessary, and dropping it lets the soft-keyboard reflow fire in
    // sync with the layout change instead of ~36ms later):
    // - A ResizeObserver on the shell + window/visualViewport resize
    //   events detect every layout change (flex settle, sidebar, soft
    //   keyboard, rotation).
    // - Measure on the next animation frame so layout has settled (width
    //   can update before flex heights finish recalculating).
    // - Cell quantization: measureTerminalFit floors pixels to cols/rows,
    //   so sub-character jitter never produces a resize at all.
    // - Echo gate: only one resize is in flight at a time (send → await the
    //   server terminal_resize echo → send the latest pending), which
    //   serializes and coalesces drag-resizes without flooding the PTY.
    const vv = window.visualViewport

    let resizeFrame: number | null = null
    let lastViewportPixels = getResizeSignalPixels(shell, vv)

    const flushViewportResize = () => {
      resizeFrame = null
      processViewportResize()
    }

    const scheduleViewportResize = () => {
      if (resizeFrame !== null) cancelAnimationFrame(resizeFrame)
      resizeFrame = requestAnimationFrame(flushViewportResize)
    }

    const onViewportResize = () => {
      const nextViewportPixels = getResizeSignalPixels(shell, vv)
      const widthChanged = nextViewportPixels.width !== lastViewportPixels.width
      const heightChanged = nextViewportPixels.height !== lastViewportPixels.height
      // Ignore duplicate window.resize / visualViewport.resize notifications
      // that report the same laid-out shell size. We key off the shell rather
      // than visualViewport because window.resize can fire before
      // visualViewport catches up on some browsers.
      if (!widthChanged && !heightChanged) return

      lastViewportPixels = nextViewportPixels
      scheduleViewportResize()
    }

    // ResizeObserver on the shell catches layout changes that don't fire
    // window.resize: initial flex settle, sidebar toggle, CSS transitions.
    // It fires after layout, so measurements are always up-to-date.
    const shellObserver = new ResizeObserver(() => onViewportResize())
    if (shell) shellObserver.observe(shell)

    // Also listen on window/visualViewport for zoom and soft keyboard.
    window.addEventListener('resize', onViewportResize)
    if (vv) vv.addEventListener('resize', onViewportResize)

    return () => {
      shellObserver.disconnect()
      if (resizeFrame !== null) cancelAnimationFrame(resizeFrame)
      longPress.cancel()
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
      terminalScrolledUp.value = false
      terminalScrollToBottom.value = null
      terminalFindOpen.value = false
      searchAddonRef.current = null
      if (reconnectTimer.current) clearTimeout(reconnectTimer.current)
      wsRef.current?.close()
      wsRef.current = null
      onInputReady?.(null)
      pasteActionRef.current = null
      onFocusReady?.(null)
      if ((window as any).__gmuxTerm === term) (window as any).__gmuxTerm = null
      ;(window as any).__gmuxInject = null
      disposeIconFontWatch()
      term.dispose()
      termRef.current = null
      termIoRef.current = null
    }
  }, [onCtrlConsumed, onInputReady, fontReady])

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

    // Full RIS on the xterm instance so SGR colors, modes, cursor state, and
    // scroll regions from the previous session don't bleed into the next one.
    // Without this, switching away from a colorful TUI (btop, htop) leaves
    // its trailing bg/fg attributes active until the new session emits an SGR
    // of its own, painting plain-text output in btop's last color.
    termRef.current.reset()

    // Reset sizes so stale values from a previous session can't trigger a
    // spurious pill while the loading overlay is visible (before ws.onopen).
    resetResizeEchoGate()
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
        if (wsRef.current !== ws) return
        attempt = 0
        setWsState('open')

        if (!isFirstConnect) {
          // Reconnect: re-sync ptySize from session metadata in case a
          // terminal_resize WS event was missed during the drop. Session
          // metadata is updated via SSE independently, so it may be
          // fresher than our cached ptySize after a network blip.
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

        // First connect for this session: claim ownership by fitting the PTY
        // to our viewport. fitAndResize measures, sets viewport+pty
        // optimistically, and sends the resize over this ws (wsRef was set
        // above).
        fitAndResize()
      }

      ws.onmessage = (ev) => {
        if (wsRef.current !== ws) return
        // Safety net: live output proves the connection works. Never show the
        // disconnected pill while data is flowing on the current socket.
        setWsState(wsStateOnOutput)
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
                releaseResizeEchoGate(size)
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
        // A stale socket (superseded by a newer connect() or by the effect
        // re-running) must not touch shared state: its close event often
        // fires *after* the replacement socket opened, and marking the
        // connection 'lost' then would leave the pill stuck on screen
        // forever while the live socket streams output behind it.
        const isCurrent = wsRef.current === ws
        setWsState(prev => wsStateOnClose(prev, isCurrent))
        if (!isCurrent) return
        resetResizeEchoGate()
        if (disposed.current || intentionalClose) return
        if (currentSessionId.current !== session.id) return

        const delay = Math.min(500 * Math.pow(2, attempt), 8000)
        attempt++
        reconnectTimer.current = setTimeout(connect, delay)
      }

      ws.onerror = () => {
        // errors surface via onclose; nothing to do here
      }
    }

    connect()

    return () => {
      intentionalClose = true
      termEpochRef.current = epoch + 1
      termIoRef.current?.reset(termEpochRef.current)
      if (reconnectTimer.current) clearTimeout(reconnectTimer.current)
      reconnectTimer.current = null
      resetResizeEchoGate()
      wsRef.current?.close()
      wsRef.current = null
    }
  }, [fitAndResize, queueData, queueMany, queueResize, releaseResizeEchoGate, resetResizeEchoGate, session.id, fontReady])

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
      onMouseDown={holdShellFocus}
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
      {terminalFindOpen.value && searchAddonRef.current && (
        <div class="terminal-find-anchor">
          <TerminalFindBar
            addon={searchAddonRef.current}
            onClose={() => {
              terminalFindOpen.value = false
              // Hand focus back to the terminal — but not on touch, where
              // that would immediately re-pop the on-screen keyboard.
              if (!isTouchDevice()) focusTerminal()
            }}
          />
        </div>
      )}
      <div ref={containerRef} class="terminal-container" />
      {termLoading && (
        <div class="terminal-loading">
          Waiting for output…
        </div>
      )}
      {terminalScrolledUp.value && (
        <button
          type="button"
          class="terminal-scroll-end"
          onClick={() => termRef.current?.scrollToBottom()}
          title="Scroll to bottom"
        >
          End ↓
        </button>
      )}
      {linkSheet && (
        <LinkActionSheet link={linkSheet} onClose={() => setLinkSheet(null)} />
      )}
      {textSheet && (
        <TerminalTextSheet
          lines={textSheet.lines}
          anchorRow={textSheet.anchorRow}
          onPaste={() => pasteActionRef.current?.()}
          onClose={() => setTextSheet(null)}
        />
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
      fontFamily: "'Fira Code', 'Symbols Nerd Font Mono', monospace",
      fontSize: 13,
      disableStdin: true,
      cursorBlink: false,
      cursorInactiveStyle: 'none',
    })
    const fit = new FitAddon()
    term.loadAddon(fit)
    term.open(containerRef.current)
    loadWebglRenderer(term)
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
