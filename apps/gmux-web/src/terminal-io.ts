import { CSI_3J, containsSequence } from './replay'

export interface TerminalWriter {
  write(data: string | Uint8Array, callback?: () => void): void
  resize(cols: number, rows: number): void
}

export interface TerminalSize {
  cols: number
  rows: number
}

/**
 * Provides scroll-state access so TerminalIO can save/restore viewport
 * position across writes.
 */
export interface ScrollAccessor {
  /**
   * `viewportY` is the buffer row at the top of the visible region;
   * `baseY` is the largest valid `viewportY` (ie at-bottom). The total
   * buffer occupies `[0, baseY + rows)` so that `getLine(y)` is
   * meaningful across that whole range - anchor matching needs to
   * search the visible region too, not just scrollback, since a line
   * we saw pre-wipe can land in either area post-wipe.
   */
  getState(): { viewportY: number; baseY: number; rows: number }
  scrollToLine(line: number): void
  scrollToBottom(): void
  /**
   * Read the visible text of the buffer line at `y` (post-trim, no ANSI
   * codes). Returns null if the line doesn't exist or is too trivial to
   * use as a scroll-restore anchor. "Too trivial" deliberately filters
   * out empty / whitespace-only / very short lines so a wipe-and-redraw
   * doesn't snap the user to the first stretch of separators it finds.
   */
  getLine(y: number): string | null
}

interface QueueItem {
  epoch: number
  data: Uint8Array
  onWritten?: () => void
}

export interface TerminalIO {
  reset(epoch: number): void
  /** Mark the next write as a replay: allow auto-scroll to stand (scroll to bottom). */
  forceNextScrollToBottom(): void
  enqueue(data: Uint8Array, epoch: number, onWritten?: () => void): void
  enqueueMany(chunks: Uint8Array[], epoch: number, onWritten?: () => void): void
  requestResize(size: TerminalSize, epoch: number): void
  hasPendingWork(): boolean
}

/**
 * Serializes terminal writes and resizes so resize only happens when the
 * parser is idle.
 *
 * Scroll preservation: ghostty-web calls `scrollToBottom()` inside every
 * `write()` call when `viewportY !== 0` (its "auto-follow output" behavior).
 * To prevent this from bouncing the user to the bottom on every output frame
 * while they are reading scrollback, we:
 *
 *   1. Before each write: if the user is scrolled up, save their position.
 *   2. After `term.write()` returns synchronously: restore it.
 *
 * The restore runs synchronously — before any `requestAnimationFrame`
 * callback fires — so the render loop always sees the corrected `viewportY`
 * on its next tick, eliminating all visible flicker.
 *
 * The buffer-reset branch (chunk contains `\x1b[3J`, ie pi-style end-of-turn
 * redraw) tries three anchors in order:
 *
 *   1. prevAnchorLine — the visible text the user was reading. If the
 *      redraw still contains that line we know exactly where to land.
 *   2. prevDistanceFromBottom — keep the user the same number of lines above
 *      the bottom as they were before the wipe.
 *   3. scrollToBottom — nothing else is meaningful.
 */
export function createTerminalIO(term: TerminalWriter, scroll?: ScrollAccessor): TerminalIO {
  let currentEpoch = 0
  let queue: QueueItem[] = []
  let writeInFlight = false
  let pendingResize: (TerminalSize & { epoch: number }) | null = null

  // Scroll snapshot captured before each write if the user is scrolled up.
  // Cleared immediately after the synchronous restore in restoreScroll().
  let scrollSnap: {
    viewportY: number
    baseY: number
    anchorLine: string | null
  } | null = null

  // When true, the next write will NOT capture scroll state, allowing
  // ghostty's auto-scroll to stand. Consumed by the very next write().
  // Set by the caller before enqueuing a replay block so that session
  // switches always land at the bottom regardless of prior scroll position.
  let forceScrollToBottom = false

  const dropStaleFront = () => {
    while (queue.length && queue[0].epoch !== currentEpoch) {
      queue.shift()
    }
    if (pendingResize && pendingResize.epoch !== currentEpoch) {
      pendingResize = null
    }
  }

  /**
   * Capture the user's scroll position before a write.
   * Skips capture when the user is already at the bottom (auto-scroll welcome)
   * or when forceScrollToBottom is set (replay mode).
   */
  const captureScroll = (): void => {
    if (!scroll) return
    if (forceScrollToBottom) {
      forceScrollToBottom = false
      return
    }
    const { viewportY, baseY } = scroll.getState()
    if (viewportY >= baseY) return // at bottom — let auto-scroll stand
    scrollSnap = {
      viewportY,
      baseY,
      anchorLine: scroll.getLine(viewportY),
    }
  }

  /**
   * Restore the user's scroll position synchronously after term.write()
   * returns. ghostty-web has already auto-scrolled to the bottom inside
   * write(); we undo that here before the render loop fires its next frame.
   *
   * For chunks containing `\x1b[3J` (scrollback wipe) we use anchor-then-
   * distance-then-bottom fallback. For all other chunks we simply return
   * the user to the position they had before the write.
   */
  const restoreScroll = (data: Uint8Array): void => {
    if (!scroll || !scrollSnap) return
    const snap = scrollSnap
    scrollSnap = null

    const { baseY, rows } = scroll.getState()

    if (containsSequence(data, CSI_3J)) {
      // Scrollback was wiped mid-write: the user's old absolute position is
      // gone. Try three anchors in order, falling through on each miss.
      const prevDistance = snap.baseY - snap.viewportY
      const anchorY = snap.anchorLine !== null
        ? findAnchorMatch(scroll, snap.anchorLine, baseY, rows, prevDistance)
        : null
      if (anchorY !== null) {
        scroll.scrollToLine(anchorY)
      } else if (prevDistance <= baseY) {
        scroll.scrollToLine(baseY - prevDistance)
      } else {
        scroll.scrollToBottom()
      }
    } else {
      // Plain streaming: return to the exact position saved before the write.
      scroll.scrollToLine(Math.min(snap.viewportY, baseY))
    }
  }

  const pump = () => {
    if (writeInFlight) return
    dropStaleFront()

    const next = queue.shift()
    if (next) {
      writeInFlight = true
      captureScroll()
      term.write(next.data, () => {
        writeInFlight = false
        if (next.epoch === currentEpoch) next.onWritten?.()
        pump()
      })
      // Restore synchronously — before any rAF fires. ghostty-web's render
      // loop runs on every animation frame; the write callback is deferred
      // to requestAnimationFrame(callback). If we restored inside the
      // callback, the render loop would fire first and render one frame at
      // the wrong (bottom) position. Restoring here eliminates that frame.
      restoreScroll(next.data)
      return
    }

    if (pendingResize && pendingResize.epoch === currentEpoch) {
      const { cols, rows } = pendingResize
      pendingResize = null
      term.resize(cols, rows)
    }
  }

  return {
    reset(epoch: number) {
      currentEpoch = epoch
      queue = []
      writeInFlight = false
      pendingResize = null
      scrollSnap = null
      forceScrollToBottom = false
    },

    forceNextScrollToBottom() {
      forceScrollToBottom = true
    },

    enqueue(data: Uint8Array, epoch: number, onWritten?: () => void) {
      if (epoch !== currentEpoch) return
      queue.push({ epoch, data, onWritten })
      pump()
    },

    enqueueMany(chunks: Uint8Array[], epoch: number, onWritten?: () => void) {
      if (epoch !== currentEpoch || chunks.length === 0) return
      for (let i = 0; i < chunks.length; i++) {
        queue.push({ epoch, data: chunks[i], onWritten: i === chunks.length - 1 ? onWritten : undefined })
      }
      pump()
    },

    requestResize(size: TerminalSize, epoch: number) {
      if (epoch !== currentEpoch) return
      pendingResize = { ...size, epoch }
      pump()
    },

    hasPendingWork() {
      dropStaleFront()
      return writeInFlight || queue.length > 0 || (!!pendingResize && pendingResize.epoch === currentEpoch)
    },
  }
}

/**
 * Find the best post-wipe scroll target whose line content matches the
 * pre-wipe anchor.
 *
 * Searches the **whole buffer**, not just `[0, baseY]`. A match at
 * `y > baseY` lives in the visible region, so we can't position the
 * viewport's top there directly (`scrollToLine` clamps to `baseY`),
 * but `scrollToBottom` keeps the line in the viewport at offset
 * `y - baseY`, which is the user's anchor visibly preserved.
 *
 * The restore target for a match at `y` is therefore `min(y, baseY)`:
 *
 *   - scrollback match (`y <= baseY`): anchor lands at the top of the
 *     viewport, exactly where the user had it.
 *   - visible-region match (`y > baseY`): viewport snaps to the bottom
 *     and the anchor sits somewhere inside the visible region, still
 *     readable.
 *
 * Tiebreak by closeness to the pre-wipe `distanceFromBottom`.
 */
function findAnchorMatch(
  scroll: ScrollAccessor,
  anchor: string,
  baseY: number,
  rows: number,
  prevDistanceFromBottom: number,
): number | null {
  let best: number | null = null
  let bestDiff = Number.POSITIVE_INFINITY
  const totalLines = baseY + rows
  for (let y = 0; y < totalLines; y++) {
    if (scroll.getLine(y) !== anchor) continue
    const restoreY = Math.min(y, baseY)
    const restoreDistance = baseY - restoreY
    const diff = Math.abs(restoreDistance - prevDistanceFromBottom)
    if (diff < bestDiff) {
      best = restoreY
      bestDiff = diff
    }
  }
  return best
}
