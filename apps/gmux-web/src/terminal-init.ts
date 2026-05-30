import { GhosttyCore } from '@wterm/ghostty'
import type { TerminalCore, CellData } from '@wterm/dom'

// ── Scrollback Capture Wrapper ──────────────────────────────────────────────
//
// @wterm/ghostty@0.3.0's get_scrollback_line() is an unimplemented stub that
// always returns 0. This wrapper captures scrollback lines in JavaScript by
// snapshotting the viewport before each write and recording rows that scroll
// off. It implements the full TerminalCore interface so WTerm sees no
// difference.
//
// The colorFlags patch (patches/@wterm__ghostty@0.3.0.patch) handles the
// secondary bug — once get_scrollback_line is properly implemented upstream,
// this wrapper can be removed and the patch alone will suffice.

const BLANK_CELL: CellData = { char: 32, fg: 256, bg: 256, flags: 0 }

/**
 * Wraps GhosttyCore to capture scrollback lines that get_scrollback_line()
 * cannot provide (stub returns 0). Intercepts writeRaw/writeString, snapshots
 * the viewport before each write, and records rows that scroll off.
 */
class ScrollbackCapturingCore implements TerminalCore {
  private readonly inner: GhosttyCore
  // index 0 = most recently scrolled-off row (just above viewport)
  // index N-1 = oldest scrollback row
  private readonly scrollbackBuf: CellData[][] = []
  private readonly maxScrollback: number

  constructor(inner: GhosttyCore, maxScrollback: number) {
    this.inner = inner
    this.maxScrollback = maxScrollback
  }

  // ── Snapshot helpers ──

  private snapshotViewport(): CellData[][] {
    const rows = this.inner.getRows()
    const cols = this.inner.getCols()
    const snap: CellData[][] = []
    for (let r = 0; r < rows; r++) {
      // Find last non-blank column to avoid storing trailing spaces.
      let lastNonBlank = 0
      const rowCells: CellData[] = []
      for (let c = 0; c < cols; c++) {
        const cell = this.inner.getCell(r, c)
        rowCells.push(cell)
        if (
          cell.char !== 32 ||
          cell.fgRgb !== undefined ||
          cell.bgRgb !== undefined ||
          cell.flags !== 0
        ) {
          lastNonBlank = c + 1
        }
      }
      rowCells.length = lastNonBlank
      snap.push(rowCells)
    }
    return snap
  }

  private captureScrolledOff(delta: number, preWriteSnap: CellData[][]): void {
    if (delta <= 0) return
    // The first `delta` rows of the pre-write viewport are the lines that
    // scrolled off. They become the newest scrollback entries (index 0).
    const newLines = preWriteSnap.slice(0, Math.min(delta, preWriteSnap.length))
    this.scrollbackBuf.unshift(...newLines)
    if (this.scrollbackBuf.length > this.maxScrollback) {
      this.scrollbackBuf.length = this.maxScrollback
    }
  }

  // ── Intercepted write methods ──

  writeRaw(data: Uint8Array): void {
    if (this.inner.usingAltScreen()) {
      // Alt-screen has no scrollback — skip snapshot.
      this.inner.writeRaw(data)
      return
    }
    const before = this.inner.getScrollbackCount()
    const snap = this.snapshotViewport()
    this.inner.writeRaw(data)
    const delta = this.inner.getScrollbackCount() - before
    this.captureScrolledOff(delta, snap)
  }

  writeString(str: string): void {
    if (this.inner.usingAltScreen()) {
      this.inner.writeString(str)
      return
    }
    const before = this.inner.getScrollbackCount()
    const snap = this.snapshotViewport()
    this.inner.writeString(str)
    const delta = this.inner.getScrollbackCount() - before
    this.captureScrolledOff(delta, snap)
  }

  // ── Scrollback reads (from JS buffer) ──

  getScrollbackCount(): number {
    return this.inner.getScrollbackCount()
  }

  getScrollbackLineLen(offset: number): number {
    return offset < this.scrollbackBuf.length ? this.scrollbackBuf[offset].length : 0
  }

  getScrollbackCell(offset: number, col: number): CellData {
    if (offset >= this.scrollbackBuf.length) return BLANK_CELL
    const row = this.scrollbackBuf[offset]
    return col < row.length ? row[col] : BLANK_CELL
  }

  // ── Delegated methods ──

  init(cols: number, rows: number): void     { this.inner.init(cols, rows) }
  resize(cols: number, rows: number): void   { this.inner.resize(cols, rows) }
  getCell(row: number, col: number): CellData { return this.inner.getCell(row, col) }
  isDirtyRow(row: number): boolean           { return this.inner.isDirtyRow(row) }
  clearDirty(): void                         { this.inner.clearDirty() }
  getCols(): number                          { return this.inner.getCols() }
  getRows(): number                          { return this.inner.getRows() }
  getCursor()                                { return this.inner.getCursor() }
  cursorKeysApp(): boolean                   { return this.inner.cursorKeysApp() }
  bracketedPaste(): boolean                  { return this.inner.bracketedPaste() }
  usingAltScreen(): boolean                  { return this.inner.usingAltScreen() }
  getTitle(): string | null                  { return this.inner.getTitle() }
  getResponse(): string | null               { return this.inner.getResponse() }
  getUnhandledSequences()                    { return this.inner.getUnhandledSequences() }
}

/**
 * Load a fresh GhosttyCore instance for one terminal, wrapped with the
 * JS-side scrollback capture to compensate for the unimplemented
 * get_scrollback_line() stub in @wterm/ghostty@0.3.0.
 *
 * Each WTerm MUST have its own isolated GhosttyCore — they are NOT shareable.
 * GhosttyCore.init() stores a single WASM terminal pointer (termPtr); if two
 * WTerm instances share one core, the second init() overwrites the pointer and
 * both terminals silently read/write the same WASM state (corrupt colors,
 * blended scrollback, broken SGR attributes).
 *
 * The WASM binary at /ghostty-vt.wasm is served from the browser HTTP cache
 * after the first fetch. Chrome and Firefox also cache the compiled module, so
 * the per-terminal instantiation cost is negligible beyond the first terminal.
 */
export async function getGhosttyCore(): Promise<TerminalCore> {
  const scrollbackLimit = 10000
  const inner = await GhosttyCore.load({ scrollbackLimit, wasmPath: '/ghostty-vt.wasm' })
  return new ScrollbackCapturingCore(inner, scrollbackLimit)
}

// Per-session prefetch cache. Avoids re-downloading and re-processing on
// every tab switch. Key: session ID. Value: extracted bytes or null if empty.
export const prefetchCache = new Map<string, Uint8Array | null>()
