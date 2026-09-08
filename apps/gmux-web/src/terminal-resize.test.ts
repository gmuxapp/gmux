import { describe, expect, test } from 'vitest'
import { decideViewportResize, sameSize, shouldQueueResizeEcho, terminalGridSize } from './terminal-resize'

describe('terminalGridSize', () => {
  test('refuses a viewport that cannot fit the minimum grid', () => {
    // A transiently tiny container (keyboard animation, collapsed pane) must
    // not become a 1-row PTY resize.
    expect(terminalGridSize(800, 19, 10, 20)).toBeNull()
    expect(terminalGridSize(800, 0, 10, 20)).toBeNull()
    expect(terminalGridSize(800, -40, 10, 20)).toBeNull()
    expect(terminalGridSize(19, 400, 10, 20)).toBeNull()
    expect(terminalGridSize(0, 400, 10, 20)).toBeNull()
  })

  test('refuses when cell metrics are not measurable yet', () => {
    expect(terminalGridSize(800, 400, 0, 20)).toBeNull()
    expect(terminalGridSize(800, 400, 10, 0)).toBeNull()
  })

  test('measures at and above the minimum boundary', () => {
    expect(terminalGridSize(20, 20, 10, 20)).toEqual({ cols: 2, rows: 1 })
    expect(terminalGridSize(800, 400, 10, 20)).toEqual({ cols: 80, rows: 20 })
  })

  test('rounds rows up in overlay-bar mode, but still refuses sub-cell height', () => {
    expect(terminalGridSize(85, 45, 10, 20, Math.ceil)).toEqual({ cols: 8, rows: 3 })
    expect(terminalGridSize(85, 19, 10, 20, Math.ceil)).toBeNull()
  })
})

describe('sameSize', () => {
  test('matches identical sizes and suppresses an echo already applied by a claim barrier', () => {
    const claim = { cols: 80, rows: 24 }
    expect(sameSize(claim, { cols: 80, rows: 24 })).toBe(true)
    expect(shouldQueueResizeEcho({ cols: 80, rows: 24 }, claim)).toBe(false)
    expect(shouldQueueResizeEcho({ cols: 81, rows: 24 }, claim)).toBe(true)
    expect(shouldQueueResizeEcho(claim, null)).toBe(true)
  })

  test('rejects nulls and mismatches', () => {
    expect(sameSize(null, { cols: 80, rows: 24 })).toBe(false)
    expect(sameSize({ cols: 80, rows: 24 }, null)).toBe(false)
    expect(sameSize({ cols: 80, rows: 24 }, { cols: 81, rows: 24 })).toBe(false)
  })
})

describe('decideViewportResize', () => {
  test('drives when viewport and PTY were in sync', () => {
    expect(decideViewportResize({
      prevViewport: { cols: 80, rows: 24 },
      ptySize: { cols: 80, rows: 24 },
      newViewport: { cols: 100, rows: 30 },
      awaitingEcho: false,
    })).toEqual({ kind: 'drive', size: { cols: 100, rows: 30 } })
  })

  test('waits when a previous resize is still awaiting echo', () => {
    expect(decideViewportResize({
      prevViewport: { cols: 80, rows: 24 },
      ptySize: { cols: 80, rows: 24 },
      newViewport: { cols: 100, rows: 30 },
      awaitingEcho: true,
    })).toEqual({ kind: 'wait' })
  })

  test('keeps waiting for the echo across repeated viewport changes', () => {
    expect(decideViewportResize({
      prevViewport: { cols: 100, rows: 30 },
      ptySize: { cols: 80, rows: 24 },
      newViewport: { cols: 120, rows: 40 },
      awaitingEcho: true,
    })).toEqual({ kind: 'wait' })
  })

  test('keeps driving after the awaited echo lands', () => {
    expect(decideViewportResize({
      prevViewport: { cols: 100, rows: 30 },
      ptySize: { cols: 80, rows: 24 },
      newViewport: { cols: 120, rows: 40 },
      awaitingEcho: false,
      forceDrive: true,
    })).toEqual({ kind: 'drive', size: { cols: 120, rows: 40 } })
  })

  // Why a refused measurement must never be committed as the viewport
  // (terminal.tsx `processViewportResize`): drive/follow is derived from
  // sameSize(prevViewport, ptySize), so a null prevViewport makes the *next*
  // real measurement passive — the terminal stays at the stale grid behind the
  // reclaim pill. Collapse → settle-at-a-different-size is the mobile keyboard
  // case, so this is the difference between self-healing and needing a tap.
  test('a forgotten viewport turns the next real measurement passive', () => {
    const pty = { cols: 80, rows: 24 }
    const settled = { cols: 80, rows: 12 }
    // What committing the refusal would do:
    expect(decideViewportResize({
      prevViewport: null, ptySize: pty, newViewport: settled, awaitingEcho: false,
    })).toEqual({ kind: 'follow', size: pty })
    // What keeping the last known viewport does:
    expect(decideViewportResize({
      prevViewport: pty, ptySize: pty, newViewport: settled, awaitingEcho: false,
    })).toEqual({ kind: 'drive', size: settled })
  })

  test('follows the PTY when passive', () => {
    expect(decideViewportResize({
      prevViewport: { cols: 100, rows: 30 },
      ptySize: { cols: 80, rows: 24 },
      newViewport: { cols: 120, rows: 40 },
      awaitingEcho: false,
    })).toEqual({ kind: 'follow', size: { cols: 80, rows: 24 } })
  })
})
