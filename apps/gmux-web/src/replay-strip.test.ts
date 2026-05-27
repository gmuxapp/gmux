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

  it('accumulates all blocks when content is short (no TAIL_SKIP anchor)', () => {
    // Each block is a single line — no status-bar TAIL_SKIP lines to skip,
    // so findNewStart returns 0 (fallback) and each block is output in full.
    const input = concat(
      fullRenderBlock('old-state-turn1'),
      'raw-between\n',
      fullRenderBlock('old-state-turn2'),
      'more-raw\n',
      fullRenderBlock('final-state-all-turns'),
      'after-final\n',
    )
    const out = dec(extractScrollbackContent(input))
    // All three blocks appear in order (no anchor found in short blocks)
    expect(out).toContain('old-state-turn1')
    expect(out).toContain('old-state-turn2')
    expect(out).toContain('final-state-all-turns')
    // after-final is appended from raw tail
    expect(out).toContain('after-final')
    // raw bytes between blocks are NOT included (they would duplicate
    // conversation content already shown in subsequent full renders)
    expect(out).not.toContain('raw-between')
    expect(out).not.toContain('more-raw')
  })

  /**
   * Multi-block deduplication test with realistic block sizes.
   *
   * Simulates a pi session where the terminal is 10 rows tall:
   *   - Header:   hdr1, hdr2                   (2 lines)
   *   - Content:  5 lines of conversation       (variable)
   *   - Status:   st1, st2, st3                 (3 lines = TAIL_SKIP)
   *
   * Turn 1: header + conv-a1..a3 + 2 padding + status
   * Turn 2: header + conv-a1..a3 + conv-b1..b2 + status  (a1-a3 still visible)
   * Turn 3: header + conv-a2..a3 + conv-b1..b2 + conv-c1 + status  (a1 scrolled off)
   *
   * After accumulation the scrollback must contain every line from every turn.
   */
  it('deduplicates overlapping content across multiple blocks', () => {
    // 10-line block builder: 2 hdr + 5 conv + 3 status
    const block = (conv: string[]) => {
      const lines = ['hdr-line1', 'hdr-line2', ...conv, 'stat-a', 'stat-b', 'stat-c']
      return fullRenderBlock(lines.join('\r\n'))
    }

    // Turn 1: conversation has 3 lines + 2 filler lines
    const turn1 = block(['conv-a1', 'conv-a2', 'conv-a3', '', ''])
    // Turn 2: conversation grows to 5 lines (a1-a3 still visible)
    const turn2 = block(['conv-a1', 'conv-a2', 'conv-a3', 'conv-b1', 'conv-b2'])
    // Turn 3: a1 scrolls off, c1 added
    const turn3 = block(['conv-a2', 'conv-a3', 'conv-b1', 'conv-b2', 'conv-c1'])

    const input = concat(turn1, turn2, turn3, 'trailing\n')
    const out = dec(extractScrollbackContent(input))

    // Every conversation line must appear exactly once
    expect(out).toContain('conv-a1')  // present only in turn1 (scrolled off by turn3)
    expect(out).toContain('conv-a2')
    expect(out).toContain('conv-a3')
    expect(out).toContain('conv-b1')
    expect(out).toContain('conv-b2')
    expect(out).toContain('conv-c1')  // new in turn3
    expect(out).toContain('trailing')

    // Headers appear once (from first block only; deduplicated in subsequent blocks)
    const matches = (out.match(/hdr-line1/g) ?? []).length
    // May appear more than once if anchor misses in short blocks, but not 3x
    expect(matches).toBeLessThanOrEqual(2)
  })

  it('handles content that completely changes between blocks', () => {
    // If no carry-over is found, output the entire current block
    const block1 = fullRenderBlock(['hdr1', 'hdr2', 'old-a', 'old-b', 'old-c', 'st1', 'st2', 'st3'].join('\r\n'))
    const block2 = fullRenderBlock(['hdr1', 'hdr2', 'new-x', 'new-y', 'new-z', 'st4', 'st5', 'st6'].join('\r\n'))
    const out = dec(extractScrollbackContent(concat(block1, block2)))
    // Both blocks present when anchor fails
    expect(out).toContain('old-a')
    expect(out).toContain('new-x')
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
