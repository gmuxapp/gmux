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
  getState(): { viewportY: number; baseY: number }
  scrollToLine(line: number): void
  scrollToBottom(): void
}

interface QueueItem {
  epoch: number
  data: Uint8Array
  onWritten?: () => void
}

export interface TerminalIO {
  reset(epoch: number): void
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
  // Only wasAtBottom is saved at BSU time. The actual viewportY is captured
  // later, at ESU write-callback time, after xterm has processed the data
  // and adjusted viewportY for any scrollback evictions.
  let savedScroll: { wasAtBottom: boolean } | null = null
  let restoreRAF: number | null = null

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
      savedScroll = { wasAtBottom: baseY - viewportY <= 3 }
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
      const { viewportY, baseY } = scroll.getState()
      const currentlyAtBottom = baseY - viewportY <= 3
      if (snap.wasAtBottom || currentlyAtBottom) {
        // Was at bottom before BSU, or user/code scrolled to bottom during
        // the BSU block — stay there.
        scroll.scrollToBottom()
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
      if (restoreRAF !== null) {
        cancelAnimationFrame(restoreRAF)
        restoreRAF = null
      }
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
