import { describe, expect, it } from 'vitest'
import { BSU, ESU } from './replay'
import { CHECKPOINT_WIRE_RESET, prepareBrowserCheckpoint } from './terminal-checkpoint'

const encoder = new TextEncoder()
const decoder = new TextDecoder()

const SGR_RESET = '\x1b[0m'
const WIRE_RESET = decoder.decode(CHECKPOINT_WIRE_RESET)
const bsu = decoder.decode(BSU)
const esu = decoder.decode(ESU)

/** Build a runner-shaped shared attach frame: BSU + reset + repaint + CUP + DECTCEM + ESU. */
function sharedFrame(repaint: string, cursor = '\x1b[5;7H', visibility = '\x1b[?25h'): Uint8Array {
  return encoder.encode(bsu + WIRE_RESET + repaint + cursor + visibility + esu)
}

function prepare(frame: Uint8Array, alt: boolean | null, margins: { top: number, bottom: number, rows: number } | null): string {
  const out = prepareBrowserCheckpoint([frame], alt, margins)
  expect(out).toHaveLength(1)
  return decoder.decode(out[0])
}

describe('prepareBrowserCheckpoint version skew', () => {
  // An older runner never sends the `terminal_checkpoint` metadata frame, so
  // the browser learns neither the active buffer nor the margins. The
  // checkpoint must stay non-destructive: SGR reset prepended, nothing else
  // invented (no ?1049 buffer switch, no DECSTBM).
  it('falls back non-destructively when no checkpoint metadata arrived', () => {
    const frame = sharedFrame('hello')
    const prepared = prepare(frame, null, null)
    expect(prepared).toBe(SGR_RESET + decoder.decode(frame))
    expect(prepared).not.toContain('\x1b[?1049')
  })

  it('treats metadata without margins as a legacy checkpoint', () => {
    const frame = sharedFrame('hello')
    const prepared = prepare(frame, false, null)
    expect(prepared).toBe(SGR_RESET + decoder.decode(frame))
  })

  it('rejects invalid margins and keeps the legacy fallback', () => {
    const frame = sharedFrame('hello')
    for (const margins of [
      { top: 0, bottom: 24, rows: 24 }, // top below DECSTBM's 1-based floor
      { top: 5, bottom: 30, rows: 24 }, // bottom beyond the screen
      { top: 10, bottom: 10, rows: 24 }, // equal bounds on a multi-row screen
    ]) {
      expect(prepare(frame, false, margins)).toBe(SGR_RESET + decoder.decode(frame))
    }
  })

  it('prepends only the SGR reset to frames without a BSU prefix', () => {
    const frame = encoder.encode('plain output')
    const prepared = prepare(frame, true, { top: 1, bottom: 24, rows: 24 })
    expect(prepared).toBe(`${SGR_RESET}plain output`)
  })

  it('passes empty chunk lists through untouched', () => {
    expect(prepareBrowserCheckpoint([], true, { top: 1, bottom: 24, rows: 24 })).toEqual([])
  })

  it('treats a BSU frame without the wire reset as legacy', () => {
    // A frame that opens synchronized update but lacks the runner's reset
    // preamble is not a state checkpoint; never invent buffer or margins.
    const frame = encoder.encode(bsu + 'partial output' + esu)
    const prepared = prepare(frame, false, { top: 1, bottom: 24, rows: 24 })
    expect(prepared).toBe(SGR_RESET + decoder.decode(frame))
  })

  it('rewrites only the first chunk and preserves the rest verbatim', () => {
    const first = sharedFrame('body')
    const rest = [encoder.encode('later output'), encoder.encode('even later')]
    const out = prepareBrowserCheckpoint([first, ...rest], false, { top: 2, bottom: 20, rows: 24 })
    expect(out).toHaveLength(3)
    expect(decoder.decode(out[0])).toContain('\x1b[?1049l')
    expect(out[1]).toBe(rest[0])
    expect(out[2]).toBe(rest[1])
  })
})

describe('prepareBrowserCheckpoint with authoritative metadata', () => {
  it('selects the normal buffer and restores margins before the cursor tail', () => {
    const prepared = prepare(sharedFrame('body'), false, { top: 2, bottom: 20, rows: 24 })
    expect(prepared).toBe(
      bsu + SGR_RESET + '\x1b[?1049l' + WIRE_RESET + 'body' + '\x1b[2;20r' + '\x1b[5;7H\x1b[?25h' + esu,
    )
  })

  it('selects the alternate buffer when the checkpoint says so', () => {
    const prepared = prepare(sharedFrame('tui'), true, { top: 1, bottom: 24, rows: 24 })
    expect(prepared).toContain('\x1b[?1049h')
    expect(prepared).toContain('\x1b[1;24r')
  })

  it('represents a one-row terminal as the full-screen margin reset', () => {
    const prepared = prepare(sharedFrame('x'), false, { top: 1, bottom: 1, rows: 1 })
    expect(prepared).toContain('x\x1b[r\x1b[5;7H')
    expect(prepared).not.toContain('\x1b[1;1r')
  })

  it('preserves a hidden-cursor tail verbatim', () => {
    const prepared = prepare(sharedFrame('body', '\x1b[1;1H', '\x1b[?25l'), false, { top: 1, bottom: 24, rows: 24 })
    expect(prepared.endsWith('\x1b[1;1H\x1b[?25l' + esu)).toBe(true)
  })

  it('falls back when the frame tail is not the anchored CUP/DECTCEM shape', () => {
    const frame = encoder.encode(bsu + WIRE_RESET + 'body with no tail' + esu)
    const prepared = prepare(frame, false, { top: 1, bottom: 24, rows: 24 })
    expect(prepared).toBe(SGR_RESET + decoder.decode(frame))
  })
})
