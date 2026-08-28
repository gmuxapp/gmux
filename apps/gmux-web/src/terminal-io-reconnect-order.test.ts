import { describe, expect, it } from 'vitest'
import { createTerminalIO } from './terminal-io'

const enc = (text: string) => new TextEncoder().encode(text)

describe('TerminalIO reconnect checkpoint ordering', () => {
  it('keeps coalescible resizes outside the declared-geometry frame transaction', () => {
    const events: string[] = []
    const writeCompletions: Array<() => void> = []
    const io = createTerminalIO({
      resize(cols, rows) { events.push(`resize:${cols}x${rows}`) },
      write(data, callback) {
        events.push(`write:${new TextDecoder().decode(data as Uint8Array)}`)
        writeCompletions.push(() => callback?.())
      },
    })
    io.reset(1)

    // Reconnect onopen may re-sync cached session metadata first.
    io.requestResize({ cols: 120, rows: 40 }, 1)
    // The checkpoint is an ordered resize + frame transaction.
    io.enqueueResizeThenMany({ cols: 79, rows: 25 }, [enc('frame-1'), enc('frame-2')], 1)
    // A concurrent viewport change enters the coalescible resize slot while
    // the first frame write is still parsing.
    io.requestResize({ cols: 140, rows: 50 }, 1)

    expect(events).toEqual([
      'resize:120x40',
      'resize:79x25',
      'write:frame-1',
    ])
    writeCompletions.shift()?.()
    expect(events).toEqual([
      'resize:120x40',
      'resize:79x25',
      'write:frame-1',
      'write:frame-2',
    ])
    writeCompletions.shift()?.()
    expect(events).toEqual([
      'resize:120x40',
      'resize:79x25',
      'write:frame-1',
      'write:frame-2',
      'resize:140x50',
    ])
  })
})
