export interface TerminalWriter {
  write(data: string | Uint8Array, callback?: () => void): void
  resize(cols: number, rows: number): void
}

export interface TerminalSize {
  cols: number
  rows: number
}

type QueueItem =
  | { epoch: number, kind: 'write', data: Uint8Array, onWritten?: () => void }
  | { epoch: number, kind: 'resize', size: TerminalSize, onApplied?: () => void }

export interface TerminalIOOptions {
  /** Current synchronized-output/wipe-resolution fence owned by the addon. */
  isBusy?: () => boolean
}

export interface TerminalIO {
  reset(epoch: number): void
  enqueue(data: Uint8Array, epoch: number, onWritten?: () => void): void
  enqueueMany(chunks: Uint8Array[], epoch: number, onWritten?: () => void): void
  /** Queue an ordered resize barrier immediately before these writes. */
  enqueueResizeThenMany(size: TerminalSize, chunks: Uint8Array[], epoch: number, onWritten?: () => void, onResized?: () => void): void
  requestResize(size: TerminalSize, epoch: number): void
  /** Reconsider a resize after the addon's combined busy fence changes. */
  busyStateChanged(): void
}

/**
 * Serializes xterm writes and resizes so resize only happens when the parser
 * is idle. This avoids xterm async-parser races (eg image addon + resize).
 * Resizes are also held across DEC 2026 synchronized output and post-ED3
 * re-resolution; the addon owns that combined busy fence so framing does not
 * depend on WebSocket chunks and resize cannot race its viewport catch-up.
 */
export function createTerminalIO(term: TerminalWriter, options: TerminalIOOptions = {}): TerminalIO {
  let currentEpoch = 0
  let queue: QueueItem[] = []
  let writeInFlight = false
  let pendingResize: (TerminalSize & { epoch: number }) | null = null

  const dropStaleFront = () => {
    while (queue.length && queue[0].epoch !== currentEpoch) queue.shift()
    if (pendingResize && pendingResize.epoch !== currentEpoch) pendingResize = null
  }

  const pump = () => {
    if (writeInFlight) return
    dropStaleFront()

    const next = queue[0]
    if (next?.kind === 'resize') {
      if (options.isBusy?.()) return
      queue.shift()
      term.resize(next.size.cols, next.size.rows)
      if (next.epoch === currentEpoch) next.onApplied?.()
      pump()
      return
    }
    if (next?.kind === 'write') {
      queue.shift()
      writeInFlight = true
      term.write(next.data, () => {
        writeInFlight = false
        if (next.epoch === currentEpoch) next.onWritten?.()
        pump()
      })
      return
    }

    if (pendingResize && pendingResize.epoch === currentEpoch && !options.isBusy?.()) {
      const { cols, rows } = pendingResize
      pendingResize = null
      term.resize(cols, rows)
    }
  }

  return {
    reset(epoch: number) {
      currentEpoch = epoch
      queue = []
      // An in-flight xterm parse cannot be cancelled. Its old callback keeps
      // the fence closed until it arrives, preventing overlap with new work.
      pendingResize = null
    },

    enqueue(data: Uint8Array, epoch: number, onWritten?: () => void) {
      if (epoch !== currentEpoch) return
      queue.push({ epoch, kind: 'write', data, onWritten })
      pump()
    },

    enqueueMany(chunks: Uint8Array[], epoch: number, onWritten?: () => void) {
      if (epoch !== currentEpoch || chunks.length === 0) return
      for (let i = 0; i < chunks.length; i++) {
        queue.push({ epoch, kind: 'write', data: chunks[i], onWritten: i === chunks.length - 1 ? onWritten : undefined })
      }
      pump()
    },

    enqueueResizeThenMany(size: TerminalSize, chunks: Uint8Array[], epoch: number, onWritten?: () => void, onResized?: () => void) {
      if (epoch !== currentEpoch) return
      queue.push({ epoch, kind: 'resize', size, onApplied: onResized })
      for (let i = 0; i < chunks.length; i++) {
        queue.push({ epoch, kind: 'write', data: chunks[i], onWritten: i === chunks.length - 1 ? onWritten : undefined })
      }
      pump()
    },

    requestResize(size: TerminalSize, epoch: number) {
      if (epoch !== currentEpoch) return
      pendingResize = { ...size, epoch }
      pump()
    },

    busyStateChanged() { pump() },
  }
}
