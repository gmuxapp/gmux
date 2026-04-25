import { BSU, ESU, containsSequence, startsWith } from './replay'

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
 * position across synchronized output (BSU/ESU) blocks.
 */
export interface ScrollAccessor {
  /**
   * `viewportY` is the buffer row at the top of the visible region;
   * `baseY` is the largest valid `viewportY` (ie at-bottom). The total
   * buffer occupies `[0, baseY + rows)` so that `getLine(y)` is
   * meaningful across that whole range — anchor matching needs to
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
  /** Mark the next BSU/ESU block as a replay: scroll to bottom unconditionally. */
  forceNextScrollToBottom(): void
  enqueue(data: Uint8Array, epoch: number, onWritten?: () => void): void
  enqueueMany(chunks: Uint8Array[], epoch: number, onWritten?: () => void): void
  requestResize(size: TerminalSize, epoch: number): void
  hasPendingWork(): boolean
}

/**
 * Serializes xterm writes and resizes so resize only happens when the parser
 * is idle. This avoids xterm async-parser races (eg image addon + resize).
 *
 * Scroll preservation: when a write chunk contains BSU (Begin Synchronized
 * Update), we note whether the user was at the bottom. When the chunk
 * containing ESU (End Synchronized Update) is written, we capture xterm's
 * post-parse viewportY (which already accounts for scrollback evictions),
 * then restore it on the next animation frame. This prevents screen redraws
 * from disrupting the user's scroll position while correctly tracking content
 * that shifts as old lines fall off the scrollback buffer.
 */
export function createTerminalIO(term: TerminalWriter, scroll?: ScrollAccessor): TerminalIO {
  let currentEpoch = 0
  let queue: QueueItem[] = []
  let writeInFlight = false
  let pendingResize: (TerminalSize & { epoch: number }) | null = null

  // Scroll preservation across BSU/ESU blocks.
  // wasAtBottom, prevBaseY, prevDistanceFromBottom, prevAnchorLine are
  // saved at BSU time; the post-parse viewportY is captured later at ESU
  // write-callback time, after xterm has adjusted viewportY for any
  // scrollback evictions.
  //
  // The wipe branch (baseY shrinks across BSU/ESU, eg via `\x1b[3J`)
  // tries three anchors in order, falling through on each miss:
  //
  //   1. prevAnchorLine — the visible text the user was reading. If the
  //      redraw still contains that line we know exactly where the user
  //      wants to be. Multiple matches are tiebroken by closeness to
  //      prevDistanceFromBottom, so common lines (eg "✓ done") still
  //      land somewhere reasonable.
  //   2. prevDistanceFromBottom — if the new buffer has room, restore
  //      the user's pre-wipe distance from the bottom. Loses the
  //      identity of the line they were reading but keeps their intent
  //      ("N lines above the latest content").
  //   3. scrollToBottom — nothing else is meaningful.
  let savedScroll: {
    wasAtBottom: boolean
    prevBaseY: number
    prevDistanceFromBottom: number
    prevAnchorLine: string | null
  } | null = null
  let restoreRAF: number | null = null

  // When true, the next BSU/ESU block will scroll to bottom unconditionally
  // instead of trying to preserve scroll position. Set during replay so the
  // initial snapshot always lands at the bottom.
  let forceScrollToBottom = false

  const dropStaleFront = () => {
    while (queue.length && queue[0].epoch !== currentEpoch) {
      queue.shift()
    }
    if (pendingResize && pendingResize.epoch !== currentEpoch) {
      pendingResize = null
    }
  }

  /** Save scroll state if this chunk starts or contains BSU. */
  const maybeSaveScroll = (data: Uint8Array): void => {
    if (!scroll || savedScroll) return // already saved, or no accessor
    if (startsWith(data, BSU) || containsSequence(data, BSU)) {
      const { viewportY, baseY } = scroll.getState()
      const distance = Math.max(0, baseY - viewportY)
      if (forceScrollToBottom) {
        savedScroll = {
          wasAtBottom: true,
          prevBaseY: baseY,
          prevDistanceFromBottom: 0,
          prevAnchorLine: null,
        }
        forceScrollToBottom = false
      } else {
        // Strict equality: only consider the user "at bottom" if the
        // viewport is exactly at the end. A loose threshold (e.g. <= 3)
        // would fight the user's scroll intent during rapid TUI redraws.
        const wasAtBottom = viewportY >= baseY
        savedScroll = {
          wasAtBottom,
          prevBaseY: baseY,
          prevDistanceFromBottom: distance,
          // Only capture the anchor when scrolled up: at-bottom always
          // wants scrollToBottom and never reaches the search.
          prevAnchorLine: wasAtBottom ? null : scroll.getLine(viewportY),
        }
      }
    }
  }

  /**
   * If this chunk contains ESU, schedule a scroll restore on the next
   * animation frame. xterm defers its viewport sync during synchronized
   * output, so we must restore AFTER that deferred sync runs.
   *
   * We capture viewportY HERE (in the write callback, after xterm has parsed
   * the data) rather than at BSU time. This is critical: xterm adjusts
   * viewportY during parsing when scrollback lines are evicted, so the
   * post-parse value correctly accounts for content that shifted out of the
   * buffer. Using the pre-BSU value would restore a stale position, causing
   * the viewport to drift as old lines are evicted.
   */
  const maybeRestoreScroll = (data: Uint8Array): void => {
    if (!scroll || !savedScroll) return
    if (!containsSequence(data, ESU)) return

    const snap = savedScroll
    savedScroll = null

    // Capture the adjusted viewportY now, after xterm has processed the
    // data (including any scrollback evictions) but before the deferred
    // viewport DOM sync runs.
    const { viewportY: adjustedY } = scroll.getState()

    // Cancel any previous pending restore (e.g. nested BSU/ESU).
    if (restoreRAF !== null) cancelAnimationFrame(restoreRAF)

    restoreRAF = requestAnimationFrame(() => {
      restoreRAF = null
      const { viewportY, baseY, rows } = scroll.getState()
      if (snap.wasAtBottom || viewportY >= baseY) {
        // Was at bottom before BSU, or user/code scrolled to bottom during
        // the BSU block — stay there.
        scroll.scrollToBottom()
      } else if (baseY < snap.prevBaseY) {
        // Scrollback shrank during the BSU/ESU block. The dominant cause
        // is `\x1b[3J` (clear scrollback), which agents like pi emit at
        // end-of-turn redraws and which resets ybase/ydisp to 0. The
        // user's pre-BSU line is gone from xterm's perspective; adjustedY
        // points at the top of a freshly-rebuilt buffer, which is the
        // "jump to top" bug. Eviction within a full scrollback never
        // reaches this branch because it leaves baseY at the cap.
        //
        // We try three anchors in order, falling through on each miss
        // (rationale on the savedScroll declaration above).
        const anchorY = snap.prevAnchorLine !== null
          ? findAnchorMatch(scroll, snap.prevAnchorLine, baseY, rows, snap.prevDistanceFromBottom)
          : null
        if (anchorY !== null) {
          scroll.scrollToLine(anchorY)
        } else if (snap.prevDistanceFromBottom <= baseY) {
          scroll.scrollToLine(baseY - snap.prevDistanceFromBottom)
        } else {
          scroll.scrollToBottom()
        }
      } else {
        // User was scrolled up — restore the post-parse position, clamped
        // to the current buffer range. We use adjustedY (captured after xterm
        // processed the data) rather than a pre-BSU snapshot, so scrollback
        // evictions are already accounted for.
        scroll.scrollToLine(Math.min(adjustedY, baseY))
      }
      // Flush any resize that was deferred while the BSU/ESU block or
      // restore rAF was in progress.
      pump()
    })
  }

  const pump = () => {
    if (writeInFlight) return
    dropStaleFront()

    const next = queue.shift()
    if (next) {
      writeInFlight = true
      maybeSaveScroll(next.data)
      term.write(next.data, () => {
        maybeRestoreScroll(next.data)
        writeInFlight = false
        if (next.epoch === currentEpoch) next.onWritten?.()
        pump()
      })
      return
    }

    // Defer resize while a BSU/ESU block is in progress (savedScroll set)
    // or a scroll-restore rAF is pending. A resize between BSU and ESU
    // (when they arrive in separate WebSocket messages) would change
    // viewportY, causing the ESU restore to capture a post-resize position
    // instead of the user's actual scroll position. Similarly, a resize
    // between the ESU write-callback and the restore rAF would invalidate
    // the captured adjustedY. The deferred resize is flushed from the rAF
    // callback after scroll is restored.
    if (pendingResize && pendingResize.epoch === currentEpoch
        && !savedScroll && restoreRAF === null) {
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
      savedScroll = null
      forceScrollToBottom = false
      if (restoreRAF !== null) {
        cancelAnimationFrame(restoreRAF)
        restoreRAF = null
      }
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
 * Tiebreak by closeness to the pre-wipe `distanceFromBottom`. Multiple
 * visible-region matches all collapse to the same target (`baseY`)
 * with the same restore-distance (`0`); among scrollback matches we
 * prefer the one whose `baseY - y` is closest to the captured
 * distance, so the user's relative scroll position is preserved when
 * possible.
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
