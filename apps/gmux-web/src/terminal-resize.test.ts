import { describe, expect, test } from 'vitest'
import { decideViewportResize, sameSize } from './terminal-resize'

describe('sameSize', () => {
  test('matches identical sizes', () => {
    expect(sameSize({ cols: 80, rows: 24 }, { cols: 80, rows: 24 })).toBe(true)
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

  test('follows the PTY when passive', () => {
    expect(decideViewportResize({
      prevViewport: { cols: 100, rows: 30 },
      ptySize: { cols: 80, rows: 24 },
      newViewport: { cols: 120, rows: 40 },
      awaitingEcho: false,
    })).toEqual({ kind: 'follow', size: { cols: 80, rows: 24 } })
  })
})
