import { describe, expect, it } from 'vitest'
import { terminalGridSize } from './terminal-resize'

describe('terminalGridSize', () => {
  it('rejects a viewport that cannot fit the minimum grid', () => {
    expect(terminalGridSize(0, 200, 10, 20)).toBeNull()
    expect(terminalGridSize(19, 200, 10, 20)).toBeNull()
    expect(terminalGridSize(200, 19, 10, 20)).toBeNull()
  })

  it('calculates a usable grid at the minimum boundary', () => {
    expect(terminalGridSize(20, 20, 10, 20)).toEqual({ cols: 2, rows: 1 })
  })

  it('supports overlay row rounding', () => {
    expect(terminalGridSize(85, 45, 10, 20, Math.ceil)).toEqual({ cols: 8, rows: 3 })
  })
})
