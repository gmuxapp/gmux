import { describe, expect, it } from 'vitest'
import { wsStateOnClose, type WsState } from './ws-state'

describe('wsStateOnClose', () => {
  it('marks the connection lost when the current socket closes while open', () => {
    expect(wsStateOnClose('open', true)).toBe('lost')
  })

  it('does not regress connecting/lost states on a current-socket close', () => {
    expect(wsStateOnClose('connecting', true)).toBe('connecting')
    expect(wsStateOnClose('lost', true)).toBe('lost')
  })

  it('ignores stale-socket closes entirely', () => {
    for (const prev of ['connecting', 'open', 'lost'] as const) {
      expect(wsStateOnClose(prev, false)).toBe(prev)
    }
  })

  // Regression: the stuck "Connection lost, reconnecting…" pill.
  //
  // Sequence: the connection effect re-runs (session switch, font load, dep
  // identity change). Cleanup closes socket A, the new run opens socket B.
  // Close events dispatch asynchronously, so A's close often arrives *after*
  // B's open. Before the fix, A's close handler ran the 'open' → 'lost'
  // transition unconditionally, flipping state to 'lost' with no path back:
  // the pill stayed on screen while socket B streamed live output behind it.
  it('a stale close arriving after the replacement socket opened does not flip open → lost', () => {
    let state: WsState = 'connecting'
    // socket B opens
    state = 'open'
    // socket A's close event finally dispatches; A is no longer current
    state = wsStateOnClose(state, false)
    expect(state).toBe('open')
  })
})
