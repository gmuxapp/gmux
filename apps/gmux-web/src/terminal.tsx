import { ScrollAnchorAddon } from '@gmux/scroll-anchor'
import { FitAddon } from '@xterm/addon-fit'
import { ImageAddon } from '@xterm/addon-image'
import { SearchAddon } from '@xterm/addon-search'
import { WebLinksAddon } from '@xterm/addon-web-links'
import { Terminal } from '@xterm/xterm'
import { useCallback, useEffect, useRef, useState } from 'preact/hooks'
import { DEFAULT_THEME_COLORS, type ResolvedKeybind } from './config'
import { applyArmedModifiers, attachKeyboardHandler, attachPasteHandler, handlePasteAction, type PasteDestination } from './keyboard'
import { LinkActionSheet } from './link-action-sheet'
import { createLongPressRecognizer } from './long-press'
import { attachMobileInputHandler } from './mobile-input'
import { MOCK_BY_ID } from './mock-data/index'
import { refreshAtlasWhenIconFontLoads } from './nerd-font'
import { createReplayBuffer } from './replay'
import type { ResolvedTerminalOptions } from './settings-schema'
import { terminalFindOpen, terminalScrolledUp, terminalScrollToBottom } from './store'
import { type CheckpointMargins, prepareBrowserCheckpoint } from './terminal-checkpoint'
import { resolveCheckpointGeometry } from './terminal-checkpoint-geometry'
import { TerminalFindBar } from './terminal-find'
import { canSendTerminalInput } from './terminal-input'
import { createTerminalIO, type TerminalSize } from './terminal-io'
import { type LinkInfo, linkAtPoint, openLinkAtPoint } from './terminal-link'
import { decideViewportResize, sameSize, shouldQueueResizeEcho } from './terminal-resize'
import { pressedBufferRow, readTerminalText } from './terminal-text'
import { TerminalTextSheet } from './terminal-text-sheet'
import { pushError } from './toasts'
import { isTouchDevice } from './touch'
import type { Session } from './types'
import { loadWebglRenderer } from './webgl-renderer'
import { type WsState, wsStateOnClose, wsStateOnOutput } from './ws-state'

// ── Config ──

type SessionConnection = { readonly sessionId: string, readonly ws: WebSocket }

const USE_MOCK = import.meta.env.VITE_MOCK === '1' || location.search.includes('mock')

/**
 * Re-export for backward compat (used by input-diagnostics.tsx).
 * The actual colors now live in config.ts as DEFAULT_THEME_COLORS.
 */
export const TERM_THEME = DEFAULT_THEME_COLORS

// ── Utilities ──

/** Frames of immediate re-measurement after a null first-claim measurement. */
const CLAIM_RETRY_FRAME_BUDGET = 20
/** Hard deadline after which a stuck claim falls back to checkpoint geometry. */
const CLAIM_RETRY_FALLBACK_MS = 4000

/**
 * Waits for a viewport-claim measurement to become available after the first
 * attempt returned null (e.g. xterm's renderer dimensions transiently
 * undefined). Bounded, no busy loop: a short requestAnimationFrame chain
 * covers transient renderer states, each ResizeObserver notification on the
 * shell earns one more attempt, and a hard deadline fires onGiveUp so the
 * attach can never remain in 'claiming' forever. The returned cancel function
 * must be called on socket replacement, session switch, or unmount.
 */
function watchForClaimMeasurement(opts: {
  shell: HTMLElement | null
  measure: () => TerminalSize | null
  onMeasured: (size: TerminalSize) => void
  onGiveUp: () => void
}): () => void {
  let done = false
  let raf: number | null = null
  let framesLeft = CLAIM_RETRY_FRAME_BUDGET
  let observer: ResizeObserver | null = null
  const cleanup = () => {
    done = true
    observer?.disconnect()
    observer = null
    clearTimeout(deadline)
    if (raf !== null) cancelAnimationFrame(raf)
    raf = null
  }
  const deadline = setTimeout(() => {
    if (done) return
    cleanup()
    opts.onGiveUp()
  }, CLAIM_RETRY_FALLBACK_MS)
  const attempt = () => {
    raf = null
    if (done) return
    const size = opts.measure()
    if (size) {
      cleanup()
      opts.onMeasured(size)
      return
    }
    if (framesLeft > 0) {
      framesLeft--
      raf = requestAnimationFrame(attempt)
    }
  }
  if (typeof ResizeObserver !== 'undefined' && opts.shell) {
    observer = new ResizeObserver(() => {
      // A real layout change earns one fresh measurement attempt.
      if (!done && raf === null) raf = requestAnimationFrame(attempt)
    })
    observer.observe(opts.shell)
  }
  raf = requestAnimationFrame(attempt)
  return cleanup
}

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
  shiftArmed,
  onShiftConsumed,
  onModifiersCancelled,
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
  shiftArmed: boolean
  onShiftConsumed: () => void
  onModifiersCancelled: () => void
  onInputReady?: (send: ((data: string) => void) | null) => void
  onFocusReady?: (focus: (() => void) | null) => void
}) {
  const shellRef = useRef<HTMLDivElement>(null)
  const containerRef = useRef<HTMLDivElement>(null)
  const termRef = useRef<Terminal | null>(null)
  // Session ID and socket form one atomic connection identity.
  const connectionRef = useRef<SessionConnection | null>(null)
  const reconnectTimer = useRef<ReturnType<typeof setTimeout> | null>(null)
  const disposed = useRef(false)
  const currentSessionId = useRef(session.id)
  // The WebSocket effect can be re-run by unrelated dependency changes. Keep
  // the reset tied to an observed session-ID transition, not to effect
  // lifetime or to a reconnect attempt.
  const terminalSessionId = useRef<string | null>(null)
  const sessionRef = useRef(session)
  const ctrlArmedRef = useRef(ctrlArmed)
  const altArmedRef = useRef(altArmed)
  const shiftArmedRef = useRef(shiftArmed)
  const termIoRef = useRef<ReturnType<typeof createTerminalIO> | null>(null)
  const scrollAnchorRef = useRef<ScrollAnchorAddon | null>(null)
  const searchAddonRef = useRef<SearchAddon | null>(null)
  const termEpochRef = useRef(0)
  // Drop initial-attach input until replay and viewport claim commit.
  const inputClaimedRef = useRef(false)

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
  // A claim resize is a FIFO barrier rather than a pending resize. Its echo
  // releases ownership gating but must not resize xterm a second time.
  const barrierClaimResizeRef = useRef<TerminalSize | null>(null)
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
  shiftArmedRef.current = shiftArmed

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
    barrierClaimResizeRef.current = null
    gate.dirty = false
    gate.timer = null
  }, [])

  const releaseResizeEchoGate = useCallback((applied: TerminalSize) => {
    const gate = resizeEchoGateRef.current
    if (!gate.awaitingEcho || !sameSize(gate.awaitingEcho, applied)) return

    if (gate.timer !== null) clearTimeout(gate.timer)
    gate.awaitingEcho = null
    if (sameSize(barrierClaimResizeRef.current, applied)) barrierClaimResizeRef.current = null
    gate.timer = null

    if (!gate.dirty) return
    gate.dirty = false
    processViewportResizeRef.current?.(true)
  }, [])

  const applyOwnedResize = useCallback((size: TerminalSize, queueTerminalResize = true, announceAlways = false) => {
    const prevPty = ptySizeRef.current

    // Optimistically sync ptySize so the pill hides immediately, before the
    // server echoes the resize back. Without this, ptySize would lag behind
    // viewportSize for one round-trip, causing a spurious pill flash.
    setPtySize(size); ptySizeRef.current = size
    if (queueTerminalResize) queueResize(size)

    if (!announceAlways && sameSize(prevPty, size)) return

    // A new outbound resize supersedes any older echo wait or pending dirty
    // viewport event. The server echo for this exact size re-opens the gate.
    resetResizeEchoGate()

    const ws = connectionRef.current?.ws
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

  // Announce ownership without occupying TerminalIO's coalescible resize
  // slot. Initial attach queues this size as a FIFO barrier before held output.
  const announceViewportClaim = useCallback((): TerminalSize | null => {
    const term = termRef.current
    const shell = shellRef.current
    if (!term || !shell) return null
    const dims = measureTerminalFit(term, shell)
    setViewportSize(dims); viewportSizeRef.current = dims
    if (!dims) return null
    // A claim is also the ownership assertion and reconnect-redraw trigger,
    // so it must reach the runner even when checkpoint and viewport match.
    applyOwnedResize(dims, false, true)
    return dims
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
    // Load after open so the addon can observe wheel/touch intent on
    // terminal.element as well as parser-level output sequences.
    const scrollAnchor = new ScrollAnchorAddon()
    term.loadAddon(scrollAnchor)
    scrollAnchorRef.current = scrollAnchor
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
      isBusy: () => scrollAnchor.busy,
    })
    const busyDisposable = scrollAnchor.onBusyChange((busy) => {
      if (!busy) termIoRef.current?.busyStateChanged()
    })
    ;(window as any).__gmuxTerm = term
    ;(window as any).__gmuxScrollAnchor = scrollAnchor
    // Test-only inject hook: pumps bytes through the same path as ws.onmessage
    // (createTerminalIO.enqueue) bypassing the WebSocket and replay buffer.
    // Used by e2e/tests/terminal-scroll.spec.ts to exercise scroll preservation
    // against real xterm with deterministic byte sequences and frame boundaries.
    ;(window as any).__gmuxInject = (b64: string) => {
      const bin = atob(b64)
      const bytes = new Uint8Array(bin.length)
      for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i)
      termIoRef.current?.enqueue(bytes, termEpochRef.current, () => setTermLoading(false))
    }

    const sendRawInput = (data: string) => {
      const connection = connectionRef.current
      if (!canSendTerminalInput(inputClaimedRef.current, connectionRef.current, connection, currentSessionId.current)) return
      // Only re-assert focus if the terminal already had it (keyboard
      // open). Grabbing focus unconditionally would pop the on-screen
      // keyboard on every toolbar key, even when it was closed — the
      // whole point of the toolbar is to work with the keyboard down.
      const hadFocus = document.activeElement === term.textarea
      connection.ws.send(new TextEncoder().encode(data))
      if (hadFocus) term.focus()
    }

    const sendInput = (data: string) => {
      const r = applyArmedModifiers(data, ctrlArmedRef.current, altArmedRef.current, shiftArmedRef.current)
      if (r.ctrlApplied) { ctrlArmedRef.current = false; onCtrlConsumed() }
      if (r.altApplied) { altArmedRef.current = false; onAltConsumed() }
      if (r.shiftApplied) { shiftArmedRef.current = false; onShiftConsumed() }
      sendRawInput(r.seq)
    }

    onInputReady?.(sendRawInput)
    // follow() already scrolls to the bottom, so a second term.scrollToBottom()
    // here would compute a zero delta and only add a scroll event for the addon
    // to classify.
    terminalScrollToBottom.value = () => scrollAnchor.follow()
    const pasteFeedback = (kind: 'info' | 'error', message: string) => {
      if (kind === 'error') {
        console.warn('[paste]', message)
        pushError(message)
      }
    }
    const getPasteDestination = (): PasteDestination | null => {
      const connection = connectionRef.current
      if (!canSendTerminalInput(inputClaimedRef.current, connectionRef.current, connection, currentSessionId.current)) return null
      return {
        sessionId: connection.sessionId,
        bracketedPasteMode: term.modes.bracketedPasteMode,
        send(text) {
          if (!canSendTerminalInput(inputClaimedRef.current, connectionRef.current, connection, currentSessionId.current)) return false
          connection.ws.send(new TextEncoder().encode(text))
          return true
        },
      }
    }
    // Every gesture acquires one immutable session/socket capability. The
    // keybind, DOM paste, and mobile sheet paths therefore share routing.
    // The paste trigger reads bracketedPasteMode and the clipboard fresh
    // on every invocation: bracketed mode flips at runtime as TUIs come
    // and go, and the clipboard contents are obviously volatile. Sharing
    // handlePasteAction with the keybind path means long-press paste gets
    // binary-paste support without divergent code.
    pasteActionRef.current = () => {
      const destination = getPasteDestination()
      if (!destination) {
        pasteFeedback('error', 'Paste failed: no active terminal connection')
        return
      }
      void handlePasteAction(destination, pasteFeedback)
    }
    onFocusReady?.(() => focusTerminalInput(term))

    const dataDisposable = term.onData((data) => sendInput(data))
    attachKeyboardHandler(term, sendInput, keybinds, macCommandIsCtrl, getPasteDestination, pasteFeedback)
    const disposePasteHandler = attachPasteHandler(containerRef.current!, getPasteDestination, pasteFeedback)
    const sendMobileReplacement = (data: string) => {
      // Replacement backspaces and text stay byte-for-byte raw so Ctrl/Alt or
      // Shift cannot corrupt autocorrect/dictation. The replacement is still
      // the next logical input, so cancel one-shot Shift unconditionally;
      // this also closes the narrow arm/render race before the ref updates.
      shiftArmedRef.current = false
      onShiftConsumed()
      sendRawInput(data)
    }
    const disposeMobileHandler = attachMobileInputHandler(
      term, containerRef.current!, sendRawInput, sendMobileReplacement,
    )

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
      busyDisposable.dispose()
      terminalScrolledUp.value = false
      terminalScrollToBottom.value = null
      terminalFindOpen.value = false
      searchAddonRef.current = null
      scrollAnchorRef.current = null
      if (reconnectTimer.current) clearTimeout(reconnectTimer.current)
      connectionRef.current?.ws.close()
      connectionRef.current = null
      onInputReady?.(null)
      pasteActionRef.current = null
      onFocusReady?.(null)
      if ((window as any).__gmuxTerm === term) (window as any).__gmuxTerm = null
      ;(window as any).__gmuxScrollAnchor = null
      ;(window as any).__gmuxInject = null
      disposeIconFontWatch()
      term.dispose()
      termRef.current = null
      termIoRef.current = null
    }
  }, [onCtrlConsumed, onAltConsumed, onShiftConsumed, onModifiersCancelled, onInputReady, fontReady])

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
    // Pending retry for a first claim whose measurement returned null.
    // Cancelled on socket replacement, reconnect, session switch, unmount.
    let cancelClaimRetry: (() => void) | null = null
    const epoch = termEpochRef.current + 1
    termEpochRef.current = epoch
    termIoRef.current.reset(epoch)
    scrollAnchorRef.current?.reset()
    inputClaimedRef.current = false

    const sessionChanged = terminalSessionId.current !== null
      && terminalSessionId.current !== session.id
    terminalSessionId.current = session.id
    // A session change is the one boundary where the existing xterm state is
    // not authoritative. Reset only on that actual ID transition:
    // same-session reconnect attempts retain the last committed screen and
    // parser state during the outage.
    if (sessionChanged) termRef.current.reset()

    // Reset sizes so stale values from a previous session can't trigger a
    // spurious pill while the loading overlay is visible (before ws.onopen).
    resetResizeEchoGate()
    setPtySize(null); ptySizeRef.current = null
    setViewportSize(null); viewportSizeRef.current = null
    setWsState('connecting')

    setTermLoading(true)

    function connect() {
      if (disposed.current) return
      // A dropped socket can strand an unmatched BSU. Preserve the user's
      // mode, but clear parser/transient fences before each replay attempt.
      scrollAnchorRef.current?.reset()

      cancelClaimRetry?.()
      cancelClaimRetry = null

      if (connectionRef.current) {
        connectionRef.current.ws.close()
        connectionRef.current = null
      }

      // The runner's binary frame is shared with `gmux attach`, so browser
      // buffer selection is delivered as metadata rather than ANSI bytes in
      // that stream. Apply it only once the complete BSU/ESU checkpoint is
      // staged; the existing screen remains visible during failed attempts.
      let checkpointAlt: boolean | null = null
      let checkpointMargins: CheckpointMargins | null = null
      let checkpointCols: number | undefined
      // Keep post-checkpoint bytes out of TerminalIO until the initial replay
      // has committed and xterm has claimed the browser geometry.
      let attachPhase: 'replay' | 'claiming' | 'claimed' = isFirstConnect ? 'replay' : 'claimed'
      const postClaimWrites: Uint8Array[] = []
      let claimFallbackTimer: ReturnType<typeof setTimeout> | null = null
      const finishPostClaimWrite = () => {
        if (claimFallbackTimer !== null) clearTimeout(claimFallbackTimer)
        claimFallbackTimer = null
        setTermLoading(false)
      }
      const replay = createReplayBuffer((chunks) => {
        const prepared = prepareBrowserCheckpoint(chunks, checkpointAlt, checkpointMargins)
        const claiming = attachPhase === 'replay'
        if (claiming) attachPhase = 'claiming'
        // New runners declare the exact geometry used to render this frame.
        // Fall back to the old metadata chain for rolling upgrades.
        const checkpointSize = resolveCheckpointGeometry(
          checkpointMargins ? { cols: checkpointCols, rows: checkpointMargins.rows } : null,
          sessionRef.current.terminal_cols,
          ptySizeRef.current?.cols,
        )
        if (checkpointSize) {
          setPtySize(checkpointSize); ptySizeRef.current = checkpointSize
        }
        const onReplayed = () => {
          if (connectionRef.current !== connection || termEpochRef.current !== epoch) return
          scrollAnchorRef.current?.follow()
          if (!claiming) {
            setTermLoading(false)
            return
          }

          isFirstConnect = false
          const proceedWithClaim = (claimSize: TerminalSize) => {
            barrierClaimResizeRef.current = claimSize
            const onClaimResized = () => {
              if (connectionRef.current !== connection || termEpochRef.current !== epoch) return
              attachPhase = 'claimed'
              inputClaimedRef.current = true
              claimFallbackTimer = setTimeout(() => {
                if (connectionRef.current !== connection || termEpochRef.current !== epoch) return
                claimFallbackTimer = null
                setTermLoading(false)
              }, 500)
              // Include bytes that arrived while the resize barrier was waiting
              // on the addon's busy fence.
              const heldWrites = postClaimWrites.splice(0)
              for (let i = 0; i < heldWrites.length; i++) {
                queueData(heldWrites[i], i === heldWrites.length - 1 ? finishPostClaimWrite : undefined)
              }
            }
            // Claim geometry is a second FIFO barrier. Held live bytes cannot
            // parse at checkpoint geometry, and input opens only when it applies.
            termIoRef.current?.enqueueResizeThenMany(claimSize, [], epoch, undefined, onClaimResized)
          }
          const claimSize = announceViewportClaim()
          if (claimSize) {
            proceedWithClaim(claimSize)
            return
          }
          // Measurement unavailable (renderer dimensions transiently
          // undefined). Never park the attach in 'claiming' forever: retry on
          // real layout/renderer lifecycle, and after a hard deadline fall
          // back to flowing at checkpoint geometry — the reconnect path
          // already proves that is safe — without ever sending a made-up
          // size to the runner.
          cancelClaimRetry?.()
          cancelClaimRetry = watchForClaimMeasurement({
            shell: shellRef.current,
            measure: announceViewportClaim,
            onMeasured: (size) => {
              cancelClaimRetry = null
              if (connectionRef.current !== connection || termEpochRef.current !== epoch) return
              proceedWithClaim(size)
            },
            onGiveUp: () => {
              cancelClaimRetry = null
              if (connectionRef.current !== connection || termEpochRef.current !== epoch) return
              // Deterministic fallback: enter 'claimed' at the checkpoint
              // geometry already applied by the replay barrier and release
              // held output and input. No resize is sent: the runner keeps
              // its size until a real measurement exists (the pill lets the
              // user reclaim, and any later resize path re-measures).
              attachPhase = 'claimed'
              inputClaimedRef.current = true
              const heldWrites = postClaimWrites.splice(0)
              if (heldWrites.length === 0) {
                setTermLoading(false)
                return
              }
              for (let i = 0; i < heldWrites.length; i++) {
                queueData(heldWrites[i], i === heldWrites.length - 1 ? finishPostClaimWrite : undefined)
              }
            },
          })
        }
        if (checkpointSize) {
          // Every replay gets an ordered local geometry barrier. Reconnects
          // do not reclaim or send this checkpoint size back to the runner.
          termIoRef.current?.enqueueResizeThenMany(checkpointSize, prepared, epoch, onReplayed)
        } else {
          queueMany(prepared, onReplayed)
        }
      })

      const wsProtocol = location.protocol === 'https:' ? 'wss:' : 'ws:'
      const ws = new WebSocket(`${wsProtocol}//${location.host}/ws/${session.id}?client=browser`)
      ws.binaryType = 'arraybuffer'
      const connection: SessionConnection = { sessionId: session.id, ws }
      connectionRef.current = connection

      ws.onopen = () => {
        if (connectionRef.current !== connection) return
        attempt = 0
        setWsState('open')

        if (isFirstConnect) return
        inputClaimedRef.current = true

        // Reconnect: re-sync ptySize from session metadata in case a
        // terminal_resize WS event was missed during the drop. Reconnects
        // deliberately never reclaim the viewport.
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
      }

      ws.onmessage = (ev) => {
        if (connectionRef.current !== connection) return
        // Safety net: live output proves the connection works. Never show the
        // disconnected pill while data is flowing on the current socket.
        setWsState(wsStateOnOutput)
        if (typeof ev.data === 'string') {
          try {
            const msg = JSON.parse(ev.data)
            // Legacy: old proxy sends resize_state on connect with cols/rows.
            // Use it to initialize ptySize if we don't have one yet.
            if (msg.type === 'terminal_checkpoint') {
              checkpointAlt = msg.active_buffer === 'alternate'
              checkpointCols = Number.isInteger(msg.cols) && msg.cols > 0 ? msg.cols : undefined
              if (Number.isInteger(msg.scroll_top) && Number.isInteger(msg.scroll_bottom) && Number.isInteger(msg.rows)) {
                checkpointMargins = { top: msg.scroll_top, bottom: msg.scroll_bottom, rows: msg.rows }
              }
              return
            }

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
                if (shouldQueueResizeEcho(size, barrierClaimResizeRef.current)) queueResize(size)
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
          if (attachPhase === 'claiming') postClaimWrites.push(data)
          else queueData(data, attachPhase === 'claimed' ? finishPostClaimWrite : () => setTermLoading(false))
          return
        }

        const data = ev.data instanceof ArrayBuffer
          ? new Uint8Array(ev.data)
          : new TextEncoder().encode(ev.data)

        if (replay.state !== 'done') {
          replay.push(data)
          return
        }

        if (attachPhase === 'claiming') postClaimWrites.push(data)
        else queueData(data, attachPhase === 'claimed' ? finishPostClaimWrite : () => setTermLoading(false))
      }

      ws.onclose = () => {
        // A stale socket (superseded by a newer connect() or by the effect
        // re-running) must not touch shared state: its close event often
        // fires *after* the replacement socket opened, and marking the
        // connection 'lost' then would leave the pill stuck on screen
        // forever while the live socket streams output behind it.
        const isCurrent = connectionRef.current === connection
        setWsState(prev => wsStateOnClose(prev, isCurrent))
        if (isCurrent) onModifiersCancelled()
        if (!isCurrent) return
        if (claimFallbackTimer !== null) clearTimeout(claimFallbackTimer)
        claimFallbackTimer = null
        cancelClaimRetry?.()
        cancelClaimRetry = null
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

    const revalidateConnection = () => {
      if (disposed.current || document.visibilityState !== 'visible') return
      const current = connectionRef.current
      if (!current || current.ws.readyState === WebSocket.CLOSED) {
        if (reconnectTimer.current === null) connect()
        return
      }
      // An OPEN WebSocket can be a suspended, dead TCP after a mobile
      // background/resume cycle. Closing it hands recovery to the existing
      // bounded-delay reconnect path rather than trusting readyState.
      current.ws.close()
    }
    const onVisibilityChange = () => revalidateConnection()
    const onResume = () => revalidateConnection()
    const onOnline = () => revalidateConnection()
    document.addEventListener('visibilitychange', onVisibilityChange)
    document.addEventListener('resume', onResume)
    window.addEventListener('pageshow', onResume)
    window.addEventListener('online', onOnline)

    connect()

    return () => {
      intentionalClose = true
      cancelClaimRetry?.()
      cancelClaimRetry = null
      inputClaimedRef.current = false
      termEpochRef.current = epoch + 1
      termIoRef.current?.reset(termEpochRef.current)
      scrollAnchorRef.current?.reset()
      document.removeEventListener('visibilitychange', onVisibilityChange)
      document.removeEventListener('resume', onResume)
      window.removeEventListener('pageshow', onResume)
      window.removeEventListener('online', onOnline)
      if (reconnectTimer.current) clearTimeout(reconnectTimer.current)
      reconnectTimer.current = null
      resetResizeEchoGate()
      connectionRef.current?.ws.close()
      connectionRef.current = null
    }
  }, [announceViewportClaim, queueData, queueMany, queueResize, releaseResizeEchoGate, resetResizeEchoGate, session.id, fontReady])

  // Pill is purely derived from size mismatch. No "driving" flag: we claim
  // on every fresh session select (first ws.onopen), and fitAndResize sets
  // ptySize = viewportSize optimistically so the pill self-clears the moment
  // we start a resize, before the server echoes it back. The pill only
  // reappears when a server-sourced terminal_resize (another client, local
  // terminal) changes ptySize away from our viewport.
  const showDisconnectedPill = wsState === 'lost'
  const showResizePill = !showDisconnectedPill
    && !termLoading
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
          <div class="reconnecting-pill terminal-disconnected-pill">
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
          onClick={() => scrollAnchorRef.current?.follow()}
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
    // Fit like the real terminal: measureTerminalFit reserves the mobile
    // control bar's height (rounding rows up so one row tucks behind the
    // translucent keys) instead of FitAddon's naive full-shell fit, which
    // would paint the terminal background underneath the whole key grid.
    const refit = () => {
      const shell = containerRef.current?.closest<HTMLElement>('.terminal-shell')
      const dims = shell ? measureTerminalFit(term, shell) : null
      if (dims) term.resize(dims.cols, dims.rows)
      else fit.fit()
    }
    refit()

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

    const onResize = () => refit()
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
