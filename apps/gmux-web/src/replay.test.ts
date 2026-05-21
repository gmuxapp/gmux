import { describe, it, expect, vi } from 'vitest'
import { createReplayBuffer, startsWith, containsSequence, BSU, ESU } from './replay'

// Helper: encode string to Uint8Array
const enc = (s: string) => new TextEncoder().encode(s)

// Helper: build a frame with BSU + content + ESU
function syncFrame(content: string): Uint8Array {
  const body = enc(content)
  const frame = new Uint8Array(BSU.length + body.length + ESU.length)
  frame.set(BSU, 0)
  frame.set(body, BSU.length)
  frame.set(ESU, BSU.length + body.length)
  return frame
}

// Helper: build a frame with BSU + content (no ESU)
function bsuFrame(content: string): Uint8Array {
  const body = enc(content)
  const frame = new Uint8Array(BSU.length + body.length)
  frame.set(BSU, 0)
  frame.set(body, BSU.length)
  return frame
}

// Helper: build a frame with content + ESU
function esuFrame(content: string): Uint8Array {
  const body = enc(content)
  const frame = new Uint8Array(body.length + ESU.length)
  frame.set(body, 0)
  frame.set(ESU, body.length)
  return frame
}

describe('startsWith', () => {
  it('returns true when data starts with prefix', () => {
    expect(startsWith(syncFrame('hello'), BSU)).toBe(true)
  })

  it('returns false when data does not start with prefix', () => {
    expect(startsWith(enc('hello world'), BSU)).toBe(false)
  })

  it('returns false when data is shorter than prefix', () => {
    expect(startsWith(new Uint8Array([0x1b, 0x5b]), BSU)).toBe(false)
  })

  it('returns true for exact match', () => {
    expect(startsWith(BSU, BSU)).toBe(true)
  })
})

describe('containsSequence', () => {
  it('finds ESU in a sync frame', () => {
    expect(containsSequence(syncFrame('test'), ESU)).toBe(true)
  })

  it('finds ESU with fromIndex skipping BSU', () => {
    expect(containsSequence(syncFrame('test'), ESU, BSU.length)).toBe(true)
  })

  it('does not find ESU in plain data', () => {
    expect(containsSequence(enc('hello world'), ESU)).toBe(false)
  })

  it('does not find ESU when fromIndex is past it', () => {
    const frame = syncFrame('x')
    expect(containsSequence(frame, ESU, frame.length)).toBe(false)
  })

  it('finds ESU at the very end', () => {
    expect(containsSequence(esuFrame('data'), ESU)).toBe(true)
  })
})

describe('createReplayBuffer', () => {
  it('starts in waiting state', () => {
    const buf = createReplayBuffer(vi.fn())
    expect(buf.state).toBe('waiting')
  })

  it('wasSkipped is false before any push', () => {
    const buf = createReplayBuffer(vi.fn())
    expect(buf.wasSkipped).toBe(false)
  })

  describe('sync replay (first frame starts with BSU)', () => {
    it('single frame: BSU + data + ESU → immediate flush', () => {
      const onFlush = vi.fn()
      const buf = createReplayBuffer(onFlush)
      const frame = syncFrame('scrollback content')

      const flushed = buf.push(frame)

      expect(flushed).toBe(true)
      expect(buf.state).toBe('done')
      expect(buf.wasSkipped).toBe(false)
      expect(onFlush).toHaveBeenCalledTimes(1)
      expect(onFlush).toHaveBeenCalledWith([frame])
    })

    it('multi-frame: BSU in first, ESU in second → flush after second', () => {
      const onFlush = vi.fn()
      const buf = createReplayBuffer(onFlush)

      const frame1 = bsuFrame('part one ')
      const frame2 = esuFrame('part two')

      expect(buf.push(frame1)).toBe(false)
      expect(buf.state).toBe('buffering')
      expect(onFlush).not.toHaveBeenCalled()

      expect(buf.push(frame2)).toBe(true)
      expect(buf.state).toBe('done')
      expect(buf.wasSkipped).toBe(false)
      expect(onFlush).toHaveBeenCalledTimes(1)
      expect(onFlush).toHaveBeenCalledWith([frame1, frame2])
    })

    it('multi-frame: BSU, middle chunks, then ESU → all chunks in flush', () => {
      const onFlush = vi.fn()
      const buf = createReplayBuffer(onFlush)

      const frame1 = bsuFrame('part1')
      const frame2 = enc('middle')
      const frame3 = esuFrame('end')

      expect(buf.push(frame1)).toBe(false)
      expect(buf.push(frame2)).toBe(false)
      expect(onFlush).not.toHaveBeenCalled()

      expect(buf.push(frame3)).toBe(true)
      expect(buf.wasSkipped).toBe(false)
      expect(onFlush).toHaveBeenCalledWith([frame1, frame2, frame3])
    })

    it('ignores pushes after flush', () => {
      const onFlush = vi.fn()
      const buf = createReplayBuffer(onFlush)

      buf.push(syncFrame('replay'))
      expect(onFlush).toHaveBeenCalledTimes(1)

      // Subsequent data (live output) should return false
      expect(buf.push(enc('live data'))).toBe(false)
      expect(buf.state).toBe('done')
      expect(onFlush).toHaveBeenCalledTimes(1)
    })
  })

  describe('no sync markers (old runner or empty scrollback)', () => {
    it('plain data → immediate flush, wasSkipped=true', () => {
      const onFlush = vi.fn()
      const buf = createReplayBuffer(onFlush)
      const data = enc('hello from terminal')

      expect(buf.push(data)).toBe(true)
      expect(buf.state).toBe('done')
      expect(buf.wasSkipped).toBe(true)
      expect(onFlush).toHaveBeenCalledWith([data])
    })

    it('data starting with partial BSU → immediate flush (not a real marker)', () => {
      const onFlush = vi.fn()
      const buf = createReplayBuffer(onFlush)
      // Just the first 3 bytes of BSU, not the full sequence
      const partial = new Uint8Array([0x1b, 0x5b, 0x3f, 0x41, 0x42])

      expect(buf.push(partial)).toBe(true)
      expect(buf.state).toBe('done')
      expect(buf.wasSkipped).toBe(true)
      expect(onFlush).toHaveBeenCalledWith([partial])
    })

    it('empty frame → immediate flush, wasSkipped=true', () => {
      const onFlush = vi.fn()
      const buf = createReplayBuffer(onFlush)
      const empty = new Uint8Array(0)

      expect(buf.push(empty)).toBe(true)
      expect(buf.state).toBe('done')
      expect(buf.wasSkipped).toBe(true)
      expect(onFlush).toHaveBeenCalledWith([empty])
    })
  })

  describe('edge cases', () => {
    it('BSU alone (no content, no ESU yet) → buffering', () => {
      const onFlush = vi.fn()
      const buf = createReplayBuffer(onFlush)

      expect(buf.push(new Uint8Array(BSU))).toBe(false)
      expect(buf.state).toBe('buffering')
      expect(onFlush).not.toHaveBeenCalled()
    })

    it('BSU immediately followed by ESU (empty scrollback) → flush', () => {
      const onFlush = vi.fn()
      const buf = createReplayBuffer(onFlush)
      const frame = new Uint8Array(BSU.length + ESU.length)
      frame.set(BSU, 0)
      frame.set(ESU, BSU.length)

      expect(buf.push(frame)).toBe(true)
      expect(buf.state).toBe('done')
      expect(buf.wasSkipped).toBe(false)
      expect(onFlush).toHaveBeenCalledWith([frame])
    })

    it('large scrollback split across many small frames', () => {
      const onFlush = vi.fn()
      const buf = createReplayBuffer(onFlush)

      // Frame 1: BSU + start of data
      buf.push(bsuFrame('chunk0'))
      expect(buf.state).toBe('buffering')

      // Frames 2-9: pure data
      for (let i = 1; i <= 8; i++) {
        buf.push(enc(`chunk${i}`))
      }
      expect(buf.state).toBe('buffering')
      expect(onFlush).not.toHaveBeenCalled()

      // Frame 10: data + ESU
      buf.push(esuFrame('chunk9'))
      expect(buf.state).toBe('done')
      expect(buf.wasSkipped).toBe(false)
      expect(onFlush).toHaveBeenCalledTimes(1)
      // All 10 chunks present
      expect(onFlush.mock.calls[0][0]).toHaveLength(10)
    })
  })
})
