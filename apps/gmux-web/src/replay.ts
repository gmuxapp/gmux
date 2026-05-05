/**
 * Scrollback replay buffer with synchronized output detection.
 *
 * When switching sessions or reconnecting, the runner sends the scrollback
 * history wrapped in DEC 2026 synchronized output markers:
 *   BSU (\x1b[?2026h) + scrollback data + ESU (\x1b[?2026l)
 *
 * This module buffers incoming data and detects when the replay is complete,
 * allowing the terminal to be cleared and redrawn atomically.
 *
 * Detection strategy:
 * - If the first received data starts with BSU → wait for ESU before flushing
 * - If it doesn't → flush immediately (old runner, no scrollback, or live data)
 */

/** Begin Synchronized Update: CSI ? 2026 h */
export const BSU = new Uint8Array([0x1b, 0x5b, 0x3f, 0x32, 0x30, 0x32, 0x36, 0x68])

/** End Synchronized Update: CSI ? 2026 l */
export const ESU = new Uint8Array([0x1b, 0x5b, 0x3f, 0x32, 0x30, 0x32, 0x36, 0x6c])

/**
 * Erase Saved Lines (clear scrollback): CSI 3 J.
 *
 * Resets xterm's `ybase` and `ydisp` to 0 mid-frame, breaking the
 * line-tracking invariant that the BSU/ESU restore path relies on. Its
 * presence inside a synchronized-output block is the signal to fall back
 * to distance-from-bottom restoration instead of trusting the post-parse
 * `viewportY`. Pi emits this as part of its end-of-turn redraw shape.
 */
export const CSI_3J = new Uint8Array([0x1b, 0x5b, 0x33, 0x4a])

export type FlushCallback = (chunks: Uint8Array[]) => void

export type ReplayState = 'waiting' | 'buffering' | 'done'

export interface ReplayBuffer {
  /** Feed a chunk of data. Returns true if flush was triggered. */
  push(data: Uint8Array): boolean
  /** Current state */
  state: ReplayState
}

/**
 * Create a replay buffer that detects synchronized scrollback replay.
 *
 * @param onFlush Called once when the replay is complete (or immediately if
 *   no sync markers detected). Receives all buffered chunks.
 */
export function createReplayBuffer(onFlush: FlushCallback): ReplayBuffer {
  let state: ReplayState = 'waiting'
  const chunks: Uint8Array[] = []

  return {
    get state() { return state },

    push(data: Uint8Array): boolean {
      if (state === 'done') return false

      if (state === 'waiting') {
        // First data: does it start with BSU?
        if (startsWith(data, BSU)) {
          // Runner sent sync markers — buffer until ESU
          state = 'buffering'
          chunks.push(data)
          if (containsSequence(data, ESU, BSU.length)) {
            // ESU already in this first frame (common: single large replay)
            state = 'done'
            onFlush(chunks)
            return true
          }
          return false
        } else {
          // No sync markers — flush immediately
          state = 'done'
          chunks.push(data)
          onFlush(chunks)
          return true
        }
      }

      // state === 'buffering'
      chunks.push(data)
      if (containsSequence(data, ESU)) {
        state = 'done'
        onFlush(chunks)
        return true
      }
      return false
    },
  }
}

/** Check if `data` starts with `prefix` */
export function startsWith(data: Uint8Array, prefix: Uint8Array): boolean {
  if (data.length < prefix.length) return false
  for (let i = 0; i < prefix.length; i++) {
    if (data[i] !== prefix[i]) return false
  }
  return true
}

/**
 * Check if `data` contains `seq` starting from `fromIndex`.
 * Searches byte-by-byte; fine for short sequences like ESU (8 bytes).
 */
export function containsSequence(data: Uint8Array, seq: Uint8Array, fromIndex = 0): boolean {
  const limit = data.length - seq.length
  for (let i = fromIndex; i <= limit; i++) {
    let match = true
    for (let j = 0; j < seq.length; j++) {
      if (data[i + j] !== seq[j]) { match = false; break }
    }
    if (match) return true
  }
  return false
}
