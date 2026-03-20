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
 * Update), the current scroll state is saved. After the chunk containing ESU
 * (End Synchronized Update) is written, the scroll position is restored on
 * the next animation frame (after xterm's deferred viewport sync runs).
 * This prevents screen redraws from disrupting the user's scroll position.
 */
export function createTerminalIO(term: TerminalWriter, scroll?: ScrollAccessor): TerminalIO {
  let currentEpoch = 0
  let queue: QueueItem[] = []
  let writeInFlight = false
  let pendingResize: (TerminalSize & { epoch: number }) | null = null

  // Scroll preservation across BSU/ESU blocks.
  let savedScroll: { viewportY: number; wasAtBottom: boolean } | null = null
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
      savedScroll = { viewportY, wasAtBottom: baseY - viewportY <= 3 }
    }
  }

  /**
   * If this chunk contains ESU, schedule a scroll restore on the next
   * animation frame. xterm defers its viewport sync during synchronized
   * output, so we must restore AFTER that deferred sync runs.
   */
  const maybeRestoreScroll = (data: Uint8Array): void => {
    if (!scroll || !savedScroll) return
    if (!containsSequence(data, ESU)) return

    const snap = savedScroll
    savedScroll = null

    // Cancel any previous pending restore (e.g. nested BSU/ESU).
    if (restoreRAF !== null) cancelAnimationFrame(restoreRAF)

    restoreRAF = requestAnimationFrame(() => {
      restoreRAF = null
      const { viewportY, baseY } = scroll.getState()
      const currentlyAtBottom = baseY - viewportY <= 3
      if (snap.wasAtBottom || currentlyAtBottom) {
        // Was at bottom before BSU, or something else already scrolled us
        // to bottom (e.g. replay's scrollToBottom) — stay there.
        scroll.scrollToBottom()
      } else {
        // User was scrolled up — restore their position, clamped to the
        // new buffer range (scrollback may have been cleared/resized).
        scroll.scrollToLine(Math.min(snap.viewportY, baseY))
      }
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
