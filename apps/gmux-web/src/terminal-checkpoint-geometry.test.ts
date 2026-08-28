import { describe, expect, it } from 'vitest'
import { resolveCheckpointGeometry } from './terminal-checkpoint-geometry'

describe('resolveCheckpointGeometry', () => {
  it('prefers the checkpoint-declared columns', () => {
    expect(resolveCheckpointGeometry({ cols: 79, rows: 25 }, 80, 81)).toEqual({ cols: 79, rows: 25 })
  })

  it('falls back through session, cached, and default columns for old runners', () => {
    expect(resolveCheckpointGeometry({ rows: 25 }, 90, 91)).toEqual({ cols: 90, rows: 25 })
    expect(resolveCheckpointGeometry({ rows: 25 }, null, 91)).toEqual({ cols: 91, rows: 25 })
    expect(resolveCheckpointGeometry({ rows: 25 })).toEqual({ cols: 80, rows: 25 })
  })
})
