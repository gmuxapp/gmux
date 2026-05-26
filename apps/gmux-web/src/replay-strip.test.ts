import { describe, it, expect } from 'vitest'
import { stripSyncBlocks } from './replay-strip'
import { BSU, ESU } from './replay'

const enc = (s: string) => new TextEncoder().encode(s)
const dec = (b: Uint8Array) => new TextDecoder().decode(b)

function concat(...parts: (Uint8Array | string)[]): Uint8Array {
  const bufs = parts.map(p => typeof p === 'string' ? enc(p) : p)
  const total = bufs.reduce((n, b) => n + b.length, 0)
  const out = new Uint8Array(total)
  let off = 0
  for (const b of bufs) { out.set(b, off); off += b.length }
  return out
}

describe('stripSyncBlocks', () => {
  it('returns input unchanged when no BSU is present', () => {
    const input = enc('hello\nworld\n')
    expect(stripSyncBlocks(input)).toBe(input) // same reference (fast path)
  })

  it('removes a single BSU…ESU block in the middle of the stream', () => {
    const input = concat('before\n', BSU, 'redraw bytes', ESU, 'after\n')
    expect(dec(stripSyncBlocks(input))).toBe('before\nafter\n')
  })

  it('removes embedded clear sequences inside the BSU block', () => {
    // Pi's actual end-of-turn shape: BSU + clear screen + cursor home
    // + clear scrollback + redraw + ESU.
    const wipe = '\x1b[2J\x1b[H\x1b[3J'
    const input = concat('turn-1 content\n', BSU, wipe, 'redraw-1', ESU, 'turn-2 content\n')
    const out = dec(stripSyncBlocks(input))
    expect(out).toBe('turn-1 content\nturn-2 content\n')
    // Crucially: none of \x1b[2J, \x1b[3J, redraw-1 leaks through.
    expect(out).not.toContain('\x1b[2J')
    expect(out).not.toContain('\x1b[3J')
    expect(out).not.toContain('redraw-1')
  })

  it('removes multiple BSU…ESU blocks (pi-shape long session)', () => {
    const wipe = '\x1b[2J\x1b[H\x1b[3J'
    let input: Uint8Array = enc('')
    for (let i = 1; i <= 5; i++) {
      input = concat(input,
        `START-MARKER-${i}\n`,
        `filler-${i}\n`,
        BSU, wipe, `redraw-${i}`, ESU,
      )
    }
    const out = dec(stripSyncBlocks(input))
    // All start markers and fillers survived.
    for (let i = 1; i <= 5; i++) {
      expect(out).toContain(`START-MARKER-${i}`)
      expect(out).toContain(`filler-${i}`)
    }
    // None of the BSU/ESU/wipe artifacts leaked through.
    expect(out).not.toContain('\x1b[2J')
    expect(out).not.toContain('\x1b[3J')
    expect(out).not.toContain('\x1b[?2026h')
    expect(out).not.toContain('\x1b[?2026l')
    for (let i = 1; i <= 5; i++) {
      expect(out).not.toContain(`redraw-${i}`)
    }
  })

  it('drops an unterminated BSU at the tail', () => {
    const input = concat('before\n', BSU, 'redraw with no close')
    expect(dec(stripSyncBlocks(input))).toBe('before\n')
  })

  it('preserves bytes immediately before BSU and after ESU', () => {
    const input = concat('A', BSU, 'X', ESU, 'B')
    expect(dec(stripSyncBlocks(input))).toBe('AB')
  })

  it('handles back-to-back BSU…ESU blocks', () => {
    const input = concat(
      BSU, 'block1', ESU,
      BSU, 'block2', ESU,
      'tail',
    )
    expect(dec(stripSyncBlocks(input))).toBe('tail')
  })
})
