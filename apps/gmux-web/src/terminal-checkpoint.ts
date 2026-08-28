import { BSU, ESU, startsWith } from './replay'

// ── Checkpoint framing ──
//
// The runner's raw attach checkpoint is shared with `gmux attach`; the
// browser-only transform below adds buffer authority and margin restoration
// on top of it, driven by the optional `terminal_checkpoint` metadata frame.
// Older runners send no metadata frame at all — that skew case must stay
// non-destructive (plain SGR-reset prepend, no guessed buffer or margins).

const encoder = new TextEncoder()
export const CHECKPOINT_WIRE_RESET = encoder.encode('\x1b[r\x1b[H\x1b[2J\x1b[3J')
// The browser checkpoint resets margins before repaint and restores the
// authoritative region before the cursor-position/visibility tail. The raw
// frame remains unchanged for CLI and legacy consumers.
const CHECKPOINT_BROWSER_RESET = encoder.encode('\x1b[r\x1b[H\x1b[2J\x1b[3J')
const CHECKPOINT_SGR_RESET = encoder.encode('\x1b[0m')

export type CheckpointMargins = { top: number, bottom: number, rows: number }

/**
 * Add browser-only buffer authority at the complete checkpoint boundary.
 * The binary frame itself is also consumed by `gmux attach`, so putting
 * ?1049h/l in it would change an operator's terminal. Reordering the known
 * SGR is emitted before buffer selection and erase, preventing the old
 * background from coloring blank cells. The browser-specific transform also
 * resets margins before repaint and restores the emulator's authoritative
 * 1-based region before the frame's final cursor-position/visibility tail.
 * The raw frame's CSI r is never changed, and no other terminal state is
 * claimed to be serialized.
 */
function cursorTailOffset(frameBody: Uint8Array): number | null {
  // The shared frame ends with CUP + DECTCEM + ESU. Find CUP only within that
  // anchored tail so escape sequences in rendered terminal content cannot be
  // mistaken for the authoritative cursor position.
  const visibilityLength = 6 // CSI ? 25 h/l
  const visibilityOffset = frameBody.length - ESU.length - visibilityLength
  if (visibilityOffset <= 0 || !startsWith(frameBody.slice(frameBody.length - ESU.length), ESU)) return null
  const visibility = frameBody.slice(visibilityOffset, visibilityOffset + visibilityLength)
  if (!(visibility[0] === 0x1b && visibility[1] === 0x5b && visibility[2] === 0x3f
    && visibility[3] === 0x32 && visibility[4] === 0x35 && (visibility[5] === 0x68 || visibility[5] === 0x6c))) return null

  const cursorEnd = visibilityOffset
  if (frameBody[cursorEnd - 1] !== 0x48) return null // H
  for (let i = cursorEnd - 2; i >= Math.max(0, cursorEnd - 24); i--) {
    if (frameBody[i] !== 0x1b) continue
    if (frameBody[i + 1] !== 0x5b) return null
    let semicolons = 0
    for (let j = i + 2; j < cursorEnd - 1; j++) {
      const byte = frameBody[j]
      if (byte === 0x3b) semicolons++
      else if (byte < 0x30 || byte > 0x39) return null
    }
    return semicolons === 1 ? i : null
  }
  return null
}

export function prepareBrowserCheckpoint(chunks: Uint8Array[], activeAlternate: boolean | null, margins: CheckpointMargins | null): Uint8Array[] {
  if (chunks.length === 0) return chunks
  const first = chunks[0]
  if (!startsWith(first, BSU)) {
    const prepared = new Uint8Array(CHECKPOINT_SGR_RESET.length + first.length)
    prepared.set(CHECKPOINT_SGR_RESET)
    prepared.set(first, CHECKPOINT_SGR_RESET.length)
    return [prepared, ...chunks.slice(1)]
  }

  const hasReset = first.length >= BSU.length + CHECKPOINT_WIRE_RESET.length
    && startsWith(first.slice(BSU.length), CHECKPOINT_WIRE_RESET)
  // Older runners provide neither authoritative buffer nor margins. Keep the
  // legacy fallback non-destructive rather than guessing a state checkpoint.
  const validMargins = margins !== null && margins.rows > 0
    && margins.top >= 1 && margins.bottom <= margins.rows
    && (margins.top < margins.bottom || (margins.rows === 1 && margins.top === 1 && margins.bottom === 1))
  if (!hasReset || activeAlternate === null || !validMargins) {
    const prepared = new Uint8Array(CHECKPOINT_SGR_RESET.length + first.length)
    prepared.set(CHECKPOINT_SGR_RESET)
    prepared.set(first, CHECKPOINT_SGR_RESET.length)
    return [prepared, ...chunks.slice(1)]
  }

  const buffer = encoder.encode(activeAlternate ? '\x1b[?1049h' : '\x1b[?1049l')
  // DECSTBM rejects equal bounds; on a one-row terminal the only safe
  // representation is the default full-screen reset itself.
  const restoreMargins = margins.rows === 1
    ? encoder.encode('\x1b[r')
    : encoder.encode(`\x1b[${margins.top};${margins.bottom}r`)
  const body = first.slice(BSU.length + CHECKPOINT_WIRE_RESET.length)
  const tailOffset = cursorTailOffset(body)
  if (tailOffset === null) {
    const prepared = new Uint8Array(CHECKPOINT_SGR_RESET.length + first.length)
    prepared.set(CHECKPOINT_SGR_RESET)
    prepared.set(first, CHECKPOINT_SGR_RESET.length)
    return [prepared, ...chunks.slice(1)]
  }
  const repaint = body.slice(0, tailOffset)
  const cursorTail = body.slice(tailOffset)
  const prepared = new Uint8Array(BSU.length + CHECKPOINT_SGR_RESET.length + buffer.length + CHECKPOINT_BROWSER_RESET.length + repaint.length + restoreMargins.length + cursorTail.length)
  let offset = 0
  prepared.set(BSU, offset); offset += BSU.length
  prepared.set(CHECKPOINT_SGR_RESET, offset); offset += CHECKPOINT_SGR_RESET.length
  prepared.set(buffer, offset); offset += buffer.length
  prepared.set(CHECKPOINT_BROWSER_RESET, offset); offset += CHECKPOINT_BROWSER_RESET.length
  prepared.set(repaint, offset); offset += repaint.length
  prepared.set(restoreMargins, offset); offset += restoreMargins.length
  prepared.set(cursorTail, offset)
  return [prepared, ...chunks.slice(1)]
}
