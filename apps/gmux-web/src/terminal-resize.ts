import type { TerminalSize } from './terminal-io'

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
