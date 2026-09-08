import type { TerminalSize } from './terminal-io'

/**
 * Convert available pixels into a terminal grid, or refuse.
 *
 * Refusing (null) is the point. A container can transiently measure smaller
 * than a single cell — a collapsed pane, a flex child mid-relayout, the
 * mobile keyboard animating in — and clamping that to the minimum grid would
 * publish a real 1-row (or 2-column) resize to the PTY, reflowing the
 * running program for a layout state the user never saw. The caller treats
 * null as "no measurement yet" and retries on the next usable layout, which
 * is exactly what the initial-claim retry path already does for a terminal
 * whose cell metrics have not settled.
 */
export function terminalGridSize(
  availW: number,
  availH: number,
  cellWidth: number,
  cellHeight: number,
  roundRows: (value: number) => number = Math.floor,
): TerminalSize | null {
  if (!(cellWidth > 0) || !(cellHeight > 0)) return null
  if (availW < cellWidth * 2 || availH < cellHeight) return null
  return {
    cols: Math.max(2, Math.floor(availW / cellWidth)),
    rows: Math.max(1, roundRows(availH / cellHeight)),
  }
}

export function sameSize(a: TerminalSize | null, b: TerminalSize | null): boolean {
  return a != null && b != null && a.cols === b.cols && a.rows === b.rows
}

/** A claim barrier already applied its matching echo to xterm. */
export function shouldQueueResizeEcho(applied: TerminalSize, barrierClaim: TerminalSize | null): boolean {
  return !sameSize(applied, barrierClaim)
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
