/**
 * Regression tests for the scrollback cell colour bug in @wterm/ghostty.
 *
 * getScrollbackCell() was unconditionally setting fgRgb and bgRgb on every
 * returned cell — even for cells with default colours (colorFlags === 0).
 * packRgb(0,0,0) returns the integer 0, which the renderer treats as an
 * explicit "rgb(0,0,0)" (black), making all default-coloured scrollback text
 * invisible (black on black).
 *
 * The fix mirrors getCell(): only set fgRgb / bgRgb when the corresponding
 * bit in colorFlags is set.
 *
 * These tests construct a GhosttyCore with a synthetic WASM-like object so no
 * actual .wasm binary is required.
 */

import { describe, it, expect } from 'vitest'
import { GhosttyCore } from '@wterm/ghostty'

// Matches the CELL_BYTES constant in @wterm/ghostty/dist/wasm-bindings.js
const CELL_BYTES = 16

// ── Fake WASM factory ─────────────────────────────────────────────────────────

interface CellSpec {
  codepoint: number
  fgR: number; fgG: number; fgB: number
  bgR: number; bgG: number; bgB: number
  flags: number
  colorFlags: number
}

/**
 * Build a minimal fake WASM object.  alloc_buffer always returns BUF_PTR.
 * get_scrollback_line writes one cell at buf_ptr and returns len=1.
 */
function makeFakeWasm(cell: CellSpec) {
  const MEM_SIZE = 512
  const BUF_PTR  = 64   // chosen to avoid offset-0 (null ptr check)

  const mem = new ArrayBuffer(MEM_SIZE)

  return {
    exports: {
      memory: { buffer: mem },
      // Used by init() — return a non-zero termPtr.
      init:                (_c: number, _r: number, _s: number) => 1,
      resize:              () => {},
      alloc_buffer:        (_size: number) => BUF_PTR,
      free_buffer:         () => {},
      get_scrollback_count: () => 1,
      get_scrollback_line: (
        _termPtr: number,
        _offset: number,
        bufPtr: number,
        _maxCols: number,
      ) => {
        // Write the cell into the shared memory buffer at bufPtr.
        const view = new DataView(mem, bufPtr, CELL_BYTES)
        view.setUint32(0, cell.codepoint, true)
        view.setUint8(4,  cell.fgR)
        view.setUint8(5,  cell.fgG)
        view.setUint8(6,  cell.fgB)
        view.setUint8(7,  cell.bgR)
        view.setUint8(8,  cell.bgG)
        view.setUint8(9,  cell.bgB)
        view.setUint8(10, cell.flags)
        view.setUint8(11, 1)             // width
        view.setUint8(12, cell.colorFlags)
        return 1                         // line length = 1 cell
      },
      // Remaining exports needed by GhosttyCore internals (not exercised here).
      write:              () => {},
      update:             () => {},
      get_viewport:       () => 0,
      is_dirty:           () => 0,
      is_dirty_row:       () => 0,
      clear_dirty:        () => {},
      get_cursor_row:     () => 0,
      get_cursor_col:     () => 0,
      get_cursor_visible: () => 1,
      cursor_keys_app:    () => 0,
      bracketed_paste:    () => 0,
      using_alt_screen:   () => 0,
      get_cols:           () => 1,
      get_rows:           () => 1,
      read_response:      () => 0,
    },
  }
}

/**
 * Build a GhosttyCore that uses the fake WASM above.
 * The constructor is private in TypeScript; we bypass via `as any`.
 */
function makeCore(cell: CellSpec): GhosttyCore {
  const wasm = makeFakeWasm(cell)
  // GhosttyCore constructor: (wasm, options)
  const core = new (GhosttyCore as any)(wasm, {}) as GhosttyCore
  // Skip init() (which would allocate a viewport buffer) — set internal
  // fields directly so getScrollbackCell can run without crashing.
  ;(core as any)._cols    = 1
  ;(core as any)._rows    = 1
  ;(core as any).termPtr  = 1
  return core
}

// ── Tests ─────────────────────────────────────────────────────────────────────

describe('GhosttyCore.getScrollbackCell — colorFlags guard', () => {
  it('does NOT set fgRgb or bgRgb when colorFlags is 0 (default colours)', () => {
    // Cell with no explicit colour: colorFlags=0, all RGB components are 0.
    // The renderer must see fgRgb=undefined so it falls back to var(--term-fg).
    // BUG: the current implementation sets fgRgb=0 (packRgb(0,0,0)=0) which
    // the renderer interprets as rgb(0,0,0) — making text invisible.
    const core = makeCore({
      codepoint: 65, // 'A'
      fgR: 0, fgG: 0, fgB: 0,
      bgR: 0, bgG: 0, bgB: 0,
      flags: 0,
      colorFlags: 0,
    })

    const cell = core.getScrollbackCell(0, 0)

    expect(cell.fgRgb).toBeUndefined()
    expect(cell.bgRgb).toBeUndefined()
  })

  it('sets fgRgb when colorFlags bit 0 is set (explicit fg)', () => {
    const core = makeCore({
      codepoint: 65,
      fgR: 255, fgG: 100, fgB: 50,
      bgR: 0,   bgG: 0,   bgB: 0,
      flags: 0,
      colorFlags: 0b01,
    })

    const cell = core.getScrollbackCell(0, 0)

    expect(cell.fgRgb).toBe((255 << 16) | (100 << 8) | 50)
    expect(cell.bgRgb).toBeUndefined()
  })

  it('sets bgRgb when colorFlags bit 1 is set (explicit bg)', () => {
    const core = makeCore({
      codepoint: 65,
      fgR: 0, fgG: 0,   fgB: 0,
      bgR: 0, bgG: 128, bgB: 255,
      flags: 0,
      colorFlags: 0b10,
    })

    const cell = core.getScrollbackCell(0, 0)

    expect(cell.fgRgb).toBeUndefined()
    expect(cell.bgRgb).toBe((0 << 16) | (128 << 8) | 255)
  })

  it('sets both fgRgb and bgRgb when colorFlags bits 0 and 1 are both set', () => {
    const core = makeCore({
      codepoint: 65,
      fgR: 200, fgG: 150, fgB: 100,
      bgR: 10,  bgG: 20,  bgB: 30,
      flags: 0,
      colorFlags: 0b11,
    })

    const cell = core.getScrollbackCell(0, 0)

    expect(cell.fgRgb).toBe((200 << 16) | (150 << 8) | 100)
    expect(cell.bgRgb).toBe((10  << 16) | (20  << 8) | 30)
  })
})
