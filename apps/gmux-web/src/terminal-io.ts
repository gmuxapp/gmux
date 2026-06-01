import type { WTerm } from '@wterm/dom'

/**
 * True for WASM RuntimeError panics (ghostty-vt unreachable traps).
 * These indicate the VT parser hit an unhandled sequence; the WASM
 * instance recovers on the next call so we swallow the error and skip
 * the offending chunk rather than crashing the WebSocket handler.
 */
function isWasmPanic(e: unknown): boolean {
  return e instanceof Error && e.message.includes('unreachable')
}

let _wasmPanicCount = 0
function warnWasmPanic(e: unknown) {
  _wasmPanicCount++
  if (_wasmPanicCount === 1 || _wasmPanicCount % 100 === 0) {
    console.warn(`[wterm] ghostty WASM panic #${_wasmPanicCount} — skipping chunk:`, e)
  }
}

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
      try {
        term.write(data)
      } catch (e) {
        if (isWasmPanic(e)) { warnWasmPanic(e); return }
        throw e
      }
    },

    writeMany(chunks: Uint8Array[], epoch: number, onDone?: () => void) {
      if (epoch !== currentEpoch || chunks.length === 0) return
      for (const chunk of chunks) {
        try {
          term.write(chunk)
        } catch (e) {
          if (isWasmPanic(e)) { warnWasmPanic(e); continue }
          throw e
        }
      }
      if (onDone) requestAnimationFrame(onDone)
    },

    resize({ cols, rows }: TerminalSize, epoch: number) {
      if (epoch !== currentEpoch) return
      term.resize(cols, rows)
    },
  }
}
