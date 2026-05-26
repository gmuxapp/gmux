import { containsSequence, BSU, ESU, CSI_3J } from './replay'

/**
 * Strip BSU…ESU synchronized-update blocks from a byte stream.
 *
 * The on-disk scrollback file (`/v1/sessions/<id>/scrollback`)
 * contains the raw PTY bytes the runner emitted, including pi's
 * end-of-turn redraws. Each redraw is wrapped in DEC 2026
 * synchronized-update markers (BSU `\x1b[?2026h` … ESU
 * `\x1b[?2026l`) and carries a screen-reset sequence inside
 * (`\x1b[2J\x1b[H\x1b[3J`) plus a fresh repaint of pi's UI.
 *
 * When the live web client replays this file into ghostty-web
 * before opening the WS, those embedded resets destroy the
 * scrollback the user wants to read. Concretely:
 *
 *   - `\x1b[3J` empties xterm's scrollback.
 *   - `\x1b[2J` clears the visible screen without pushing rows
 *     into scrollback (each turn's text is destroyed in place).
 *   - `\x1b[H` then redraw rewrites the same rows.
 *
 * Stripping any one of these escape sequences in isolation is
 * insufficient (the others still cause loss). The cleanest
 * cut-line is the BSU/ESU envelope: discard whole synchronized-
 * update blocks. Outside those blocks, the bytes are streaming
 * turn content (user prompts, agent text, tool output) which is
 * exactly what we want to accumulate in scrollback.
 *
 * Trade-offs:
 *
 *   - Pi's end-of-turn UI repaint is omitted from the replay.
 *     The live WS snapshot (sent on connect with `?no_erase=1`)
 *     paints the current UI state on top, so the user sees the
 *     same final UI; only the per-turn redraws en route are
 *     skipped.
 *   - Any BSU/ESU block written by something other than pi
 *     (e.g. an interactive subprocess) is also stripped. In
 *     practice BSU/ESU is rare outside agents that explicitly
 *     emit it, so this is acceptable for v1.
 *   - An unterminated BSU at the tail of the buffer (incomplete
 *     write or in-flight redraw at file rotation) is also
 *     dropped, which keeps the output a coherent stream rather
 *     than emitting a half-block followed by spurious bytes.
 */
export function stripSyncBlocks(input: Uint8Array): Uint8Array {
  // Fast path: no BSU at all → nothing to strip.
  if (!containsSequence(input, BSU)) return input

  const out = new Uint8Array(input.length)
  let outLen = 0
  let i = 0
  while (i < input.length) {
    if (matchesAt(input, i, BSU)) {
      // Skip until ESU (inclusive). If no ESU is found, drop the
      // tail entirely — see comment above.
      const esuStart = indexOf(input, ESU, i + BSU.length)
      if (esuStart < 0) break
      i = esuStart + ESU.length
      continue
    }
    out[outLen++] = input[i++]
  }
  return out.subarray(0, outLen)
}

function matchesAt(data: Uint8Array, pos: number, seq: Uint8Array): boolean {
  if (pos + seq.length > data.length) return false
  for (let j = 0; j < seq.length; j++) {
    if (data[pos + j] !== seq[j]) return false
  }
  return true
}

/**
 * Extract the best scrollback content from a byte stream.
 *
 * Rather than discarding all BSU…ESU blocks (as stripSyncBlocks does),
 * this finds the LAST "full-render" block — one that contains CSI_3J
 * (\x1b[3J]) inside it. Pi emits a full render on every turn: BSU +
 * \x1b[2J\x1b[H\x1b[3J + all conversation lines + ESU. The content
 * after the \x1b[3J is the complete conversation as rendered at that
 * snapshot — user prompts, agent responses, tool output, all formatted.
 *
 * Strategy:
 *   1. Find the LAST full-render block (last BSU/ESU containing CSI_3J).
 *   2. Extract its content (everything from after CSI_3J to before ESU).
 *   3. Append raw bytes that follow the block, with any later differential
 *      BSU/ESU blocks stripped via stripSyncBlocks.
 *   4. Fall back to stripSyncBlocks if no full-render block exists.
 *
 * This gives the complete conversation at the most recent snapshot, plus
 * any tool output that streamed after it, without duplicating earlier turns
 * or risking cursor-movement artifacts from differential-render blocks.
 */
export function extractScrollbackContent(input: Uint8Array): Uint8Array {
  // Fast path: no BSU at all → nothing to extract.
  if (!containsSequence(input, BSU)) return input

  // Scan all BSU/ESU blocks and track the last one containing CSI_3J.
  let lastContentStart = -1   // index right after CSI_3J in the last full-render block
  let lastBlockEnd = -1        // index right after ESU of the last full-render block
  let i = 0
  while (i < input.length) {
    const bsuPos = indexOf(input, BSU, i)
    if (bsuPos < 0) break

    const esuPos = indexOf(input, ESU, bsuPos + BSU.length)
    if (esuPos < 0) break

    // Is CSI_3J present between BSU and ESU?
    const csi3jPos = indexOf(input, CSI_3J, bsuPos + BSU.length)
    if (csi3jPos >= 0 && csi3jPos < esuPos) {
      lastContentStart = csi3jPos + CSI_3J.length
      lastBlockEnd = esuPos + ESU.length
    }

    i = esuPos + ESU.length
  }

  // No full-render block found — fall back to stripping everything.
  if (lastContentStart < 0) return stripSyncBlocks(input)

  // Content of the last full render: lines from after CSI_3J to before ESU.
  const fullRenderContent = input.subarray(lastContentStart, lastBlockEnd - ESU.length)

  // Bytes after the last full-render block: keep raw bytes, strip any
  // subsequent differential BSU/ESU blocks (they use relative cursor moves
  // that are only valid in the live terminal context).
  const afterBlock = input.subarray(lastBlockEnd)
  const strippedAfter = stripSyncBlocks(afterBlock)

  const out = new Uint8Array(fullRenderContent.length + strippedAfter.length)
  out.set(fullRenderContent)
  out.set(strippedAfter, fullRenderContent.length)
  return out
}

function indexOf(data: Uint8Array, seq: Uint8Array, from: number): number {
  const limit = data.length - seq.length
  for (let i = from; i <= limit; i++) {
    let ok = true
    for (let j = 0; j < seq.length; j++) {
      if (data[i + j] !== seq[j]) { ok = false; break }
    }
    if (ok) return i
  }
  return -1
}
