import type { WTerm } from '@wterm/dom'

export interface TerminalSize {
  cols: number
  rows: number
}

export interface TerminalIO {
  /** Reset epoch — drops all in-flight writes/resizes for old sessions. */
  reset(epoch: number): void

  /** Write raw PTY bytes. No-op if epoch is stale. Synchronous. */
  write(data: Uint8Array, epoch: number): void

  /**
   * Write multiple chunks. onDone fires after first render frame
   * following the last write (requestAnimationFrame).
   */
  writeMany(chunks: Uint8Array[], epoch: number, onDone?: () => void): void

  /** Resize the terminal. Safe to call synchronously. */
  resize(size: TerminalSize, epoch: number): void
}

export function createTerminalIO(term: WTerm): TerminalIO {
  let currentEpoch = 0

  return {
    reset(epoch: number) {
      currentEpoch = epoch
    },

    write(data: Uint8Array, epoch: number) {
      if (epoch !== currentEpoch) return
      term.write(data)
    },

    writeMany(chunks: Uint8Array[], epoch: number, onDone?: () => void) {
      if (epoch !== currentEpoch || chunks.length === 0) return
      for (const chunk of chunks) term.write(chunk)
      if (onDone) requestAnimationFrame(onDone)
    },

    resize({ cols, rows }: TerminalSize, epoch: number) {
      if (epoch !== currentEpoch) return
      term.resize(cols, rows)
    },
  }
}
