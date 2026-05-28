import type { TerminalSize } from './terminal-io'
import { useEffect } from 'preact/hooks'
import type { RefObject } from 'preact'

export function sameSize(a: TerminalSize | null, b: TerminalSize | null): boolean {
  return a != null && b != null && a.cols === b.cols && a.rows === b.rows
}

/**
 * Decide how a viewport change should affect terminal sizing.
 *
 * - drive: this browser owns the PTY size, resize to the measured viewport.
 * - wait: we are still driving, but a previous resize is awaiting server echo.
 * - follow: another source owns the PTY, keep xterm at the known PTY size.
 * - noop: not enough information yet to do anything.
 */
type ResizeDecision
  = { kind: 'drive'; size: TerminalSize }
  | { kind: 'wait' }
  | { kind: 'follow'; size: TerminalSize }
  | { kind: 'noop' }

export function decideViewportResize({
  prevViewport,
  ptySize,
  newViewport,
  awaitingEcho,
  forceDrive = false,
}: {
  prevViewport: TerminalSize | null
  ptySize: TerminalSize | null
  newViewport: TerminalSize | null
  awaitingEcho: boolean
  forceDrive?: boolean
}): ResizeDecision {
  const wasInSync = sameSize(prevViewport, ptySize)
  // While waiting for a previous resize echo, viewport and PTY will often be
  // out of sync temporarily. That mismatch does not mean we became passive;
  // it means we are still driving and should queue the latest viewport change.
  const isDriving = forceDrive || wasInSync || awaitingEcho

  if (isDriving && newViewport) {
    return awaitingEcho
      ? { kind: 'wait' }
      : { kind: 'drive', size: newViewport }
  }

  if (ptySize) return { kind: 'follow', size: ptySize }
  return { kind: 'noop' }
}

// ── Resize-signal measurement ──────────────────────────────────────────────

/**
 * Returns the pixel dimensions used to detect a viewport resize event.
 * Prefers the shell element's client dimensions; falls back to visualViewport
 * or window dimensions for environments where the element isn't available.
 */
export function getResizeSignalPixels(
  host: HTMLElement | null,
  vv: VisualViewport | null,
): { width: number; height: number } {
  if (host) return { width: host.clientWidth, height: host.clientHeight }
  return {
    width:  vv?.width  ?? window.innerWidth,
    height: vv?.height ?? window.innerHeight,
  }
}

// ── useViewportResize ───────────────────────────────────────────────────────

/**
 * Attaches ResizeObserver + window/visualViewport resize listeners to the
 * shell container and drives processViewportResize on every change.
 * Stable effect (empty deps) — all callbacks accessed via refs.
 */
export function useViewportResize(
  shellRef: RefObject<HTMLDivElement | null>,
  processViewportResizeRef: RefObject<((forceDrive?: boolean) => void) | null>,
  onRefocus: () => void,
): void {
  useEffect(() => {
    const shell = shellRef.current
    const vv    = window.visualViewport
    const isTouchDevice = window.matchMedia('(pointer: coarse)').matches
      || navigator.maxTouchPoints > 0
    const DEBOUNCE_MS = 20

    let resizeTimer:  ReturnType<typeof setTimeout> | null = null
    let resizeFrame:  number | null = null
    let refocusTimer: ReturnType<typeof setTimeout> | null = null
    let lastPx = getResizeSignalPixels(shell, vv)
    let pendingHeightChange = false

    const flush = () => {
      resizeTimer = null
      resizeFrame = null
      processViewportResizeRef.current?.()

      const shouldRefocus = pendingHeightChange && isTouchDevice
      pendingHeightChange = false
      if (!shouldRefocus) return
      if (refocusTimer !== null) clearTimeout(refocusTimer)
      refocusTimer = setTimeout(onRefocus, 120)
    }

    const schedule = () => {
      if (resizeFrame !== null) cancelAnimationFrame(resizeFrame)
      resizeFrame = requestAnimationFrame(flush)
    }

    const onResize = () => {
      const nextPx = getResizeSignalPixels(shell, vv)
      const widthChanged  = nextPx.width  !== lastPx.width
      const heightChanged = nextPx.height !== lastPx.height
      if (!widthChanged && !heightChanged) return

      lastPx = nextPx
      pendingHeightChange = pendingHeightChange || heightChanged

      if (resizeTimer !== null) { clearTimeout(resizeTimer); resizeTimer = null }

      if (isTouchDevice && heightChanged && !widthChanged) {
        resizeTimer = setTimeout(schedule, DEBOUNCE_MS)
        return
      }
      schedule()
    }

    const observer = new ResizeObserver(() => onResize())
    if (shell) observer.observe(shell)
    window.addEventListener('resize', onResize)
    if (vv) vv.addEventListener('resize', onResize)

    return () => {
      observer.disconnect()
      if (resizeTimer  !== null) clearTimeout(resizeTimer)
      if (resizeFrame  !== null) cancelAnimationFrame(resizeFrame)
      if (refocusTimer !== null) clearTimeout(refocusTimer)
      window.removeEventListener('resize', onResize)
      if (vv) vv.removeEventListener('resize', onResize)
    }
  }, []) // stable — all state accessed via refs at handler-call time
}

