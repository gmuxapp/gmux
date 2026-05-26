import { describe, it, expect } from 'vitest'
import { stripSyncBlocks, extractScrollbackContent } from './replay-strip'
import { BSU, ESU, CSI_3J } from './replay'

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

// pi full-render block shape: BSU + kitty-deletes? + \x1b[2J\x1b[H + CSI_3J + lines + ESU
// We construct these with concat() for clarity.
const wipe = '\x1b[2J\x1b[H'  // precedes CSI_3J in real blocks
function fullRenderBlock(content: string): Uint8Array {
  return concat(BSU, wipe, CSI_3J, content, ESU)
}
function diffBlock(content: string): Uint8Array {
  // Differential render: BSU/ESU with cursor-movement content but no CSI_3J
  return concat(BSU, content, ESU)
}

describe('extractScrollbackContent', () => {
  it('returns input unchanged (same reference) when no BSU present', () => {
    const input = enc('hello\nworld\n')
    expect(extractScrollbackContent(input)).toBe(input)
  })

  it('falls back to stripSyncBlocks when no full-render block exists', () => {
    // Only differential blocks — no CSI_3J anywhere
    const input = concat('raw-before\n', diffBlock('diff1'), 'raw-after\n')
    expect(dec(extractScrollbackContent(input))).toBe('raw-before\nraw-after\n')
  })

  it('returns content after CSI_3J for a single full-render block', () => {
    const input = fullRenderBlock('line1\r\nline2\r\nline3')
    const out = dec(extractScrollbackContent(input))
    expect(out).toBe('line1\r\nline2\r\nline3')
    // Destructive sequences must not leak
    expect(out).not.toContain('\x1b[2J')
    expect(out).not.toContain('\x1b[3J')
    expect(out).not.toContain('\x1b[?2026h')
    expect(out).not.toContain('\x1b[?2026l')
  })

  it('appends raw bytes that follow the full-render block', () => {
    const input = concat(fullRenderBlock('conv-lines'), 'tool-output\n')
    const out = dec(extractScrollbackContent(input))
    expect(out).toBe('conv-linesツool-output\n'.replace('ツ', 't'))
    // spelled out to avoid confusion
    expect(out).toBe('conv-linestool-output\n')
  })

  it('strips differential BSU/ESU blocks that follow the full-render block', () => {
    const input = concat(
      fullRenderBlock('conv-lines'),
      diffBlock('\x1b[2K updated-line'),  // differential render — should be dropped
      'raw-after\n',
    )
    const out = dec(extractScrollbackContent(input))
    expect(out).toBe('conv-linesraw-after\n')
    expect(out).not.toContain('updated-line')
  })

  it('uses only the LAST full-render block when multiple exist', () => {
    // Early full renders have outdated content; the last one is canonical
    const input = concat(
      fullRenderBlock('old-state-turn1'),
      'raw-between\n',
      fullRenderBlock('old-state-turn2'),
      'more-raw\n',
      fullRenderBlock('final-state-all-turns'),
      'after-final\n',
    )
    const out = dec(extractScrollbackContent(input))
    expect(out).toBe('final-state-all-turnsafter-final\n')
    expect(out).not.toContain('old-state-turn1')
    expect(out).not.toContain('old-state-turn2')
    expect(out).not.toContain('raw-between')
    expect(out).not.toContain('more-raw')
  })

  it('handles a realistic pi session shape (raw + full + diff + raw)', () => {
    // Simulate: tool output → full render → streaming diff → more tool output
    const input = concat(
      'bash-output-turn1\n',
      fullRenderBlock('user: hello\r\nassistant: world'),
      diffBlock('\x1b[1A\x1b[2K updated streaming line'),
      'bash-output-turn2\n',
    )
    const out = dec(extractScrollbackContent(input))
    // Full render content is present
    expect(out).toContain('user: hello')
    expect(out).toContain('assistant: world')
    // Tool output after the block is present
    expect(out).toContain('bash-output-turn2')
    // Earlier raw output is NOT included (superseded by full render)
    expect(out).not.toContain('bash-output-turn1')
    // Differential block content is stripped
    expect(out).not.toContain('updated streaming line')
  })

  it('drops an unterminated BSU at the tail after the full-render block', () => {
    const input = concat(fullRenderBlock('lines'), BSU, 'no-close')
    // The trailing unterminated block is dropped; full render content survives
    expect(dec(extractScrollbackContent(input))).toBe('lines')
  })
})
