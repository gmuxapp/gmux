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
 * this accumulates conversation history across ALL "full-render" blocks.
 *
 * Pi's TUI re-renders on every turn: BSU + \x1b[2J\x1b[H\x1b[3J + the current
 * terminal viewport + ESU. The viewport is fixed-height (terminal rows), so
 * each block shows only the most recent N turns that fit on screen. Early
 * conversation turns scroll off the top as the session grows.
 *
 * Strategy (multi-block):
 *   1. Collect ALL full-render blocks (BSU/ESU containing \x1b[3J]) in order.
 *   2. Output the first block's content in full (earliest visible conversation).
 *   3. For each subsequent block, find where it diverges from the previous block
 *      (the "anchor": the last few meaningful lines of the previous block that
 *      still appear in the current block) and output only the NEW lines after
 *      the anchor. This deduplicates the overlapping conversation content.
 *   4. Append raw bytes after the last block (stripped of further BSU/ESU).
 *   5. Fall back to stripSyncBlocks if no full-render block exists.
 *   6. Single-block path: unchanged from Option B (last-block-only behaviour).
 *
 * Result: complete conversation history across the full session, not just the
 * current viewport, allowing the user to scroll to the very start.
 */
export function extractScrollbackContent(input: Uint8Array): Uint8Array {
  // Fast path: no BSU at all → nothing to extract.
  if (!containsSequence(input, BSU)) return input

  // Collect all full-render blocks (BSU/ESU blocks that contain CSI_3J).
  type Block = { contentStart: number; blockEnd: number }
  const blocks: Block[] = []
  let i = 0
  while (i < input.length) {
    const bsuPos = indexOf(input, BSU, i)
    if (bsuPos < 0) break
    const esuPos = indexOf(input, ESU, bsuPos + BSU.length)
    if (esuPos < 0) break
    const csi3jPos = indexOf(input, CSI_3J, bsuPos + BSU.length)
    if (csi3jPos >= 0 && csi3jPos < esuPos) {
      blocks.push({ contentStart: csi3jPos + CSI_3J.length, blockEnd: esuPos + ESU.length })
    }
    i = esuPos + ESU.length
  }

  // No full-render block found — fall back to stripping everything.
  if (blocks.length === 0) return stripSyncBlocks(input)

  const getContent = (b: Block): Uint8Array =>
    input.subarray(b.contentStart, b.blockEnd - ESU.length)

  // Single block: original Option B — use last block, append stripped tail.
  if (blocks.length === 1) {
    const content = getContent(blocks[0])
    const after = stripSyncBlocks(input.subarray(blocks[0].blockEnd))
    return concatU8(content, after)
  }

  // Multiple blocks: accumulate new lines from each block via anchor matching.
  const dec = new TextDecoder()
  const enc = new TextEncoder()

  const blockLines = (b: Block): string[] =>
    dec.decode(getContent(b)).split(/\r?\n/)

  /** Strip CSI sequences and null bytes for line comparison. */
  const stripCSI = (s: string): string =>
    s.replace(/\x1b\[[\d;]*[A-Za-z]/g, '').replace(/\0/g, '').trimEnd()

  /**
   * Find the cut point in currLines: the index of the first truly-new line,
   * i.e. the first line that does not appear as carry-over from prevLines.
   *
   * Searches from the BOTTOM of prevLines (ignoring the trailing TAIL_SKIP
   * status-bar lines that change every turn) for the deepest line that still
   * appears in currLines. That line is the carry-over boundary; everything
   * after it is new conversation content.
   *
   * Two consecutive lines (VERIFY_LEN=2) must match in both blocks to guard
   * against false positives from repeated lines.
   */
  const findNewStart = (prevLines: string[], currLines: string[]): number => {
    const TAIL_SKIP = 3   // pi status-bar lines at the bottom of every block
    const VERIFY_LEN = 2  // verify this many subsequent lines for confidence

    const prevConvEnd = Math.max(0, prevLines.length - TAIL_SKIP)
    const prevStripped = prevLines.slice(0, prevConvEnd).map(stripCSI)
    const currStripped = currLines.map(stripCSI)

    // Build a position-list index of currLines for O(1) lookup.
    const currIndex = new Map<string, number[]>()
    for (let c = 0; c < currStripped.length; c++) {
      const line = currStripped[c]
      if (!line) continue
      if (!currIndex.has(line)) currIndex.set(line, [])
      currIndex.get(line)!.push(c)
    }

    // Scan prevConv from the BOTTOM upward. The first non-empty line we find
    // in currLines (verified by the lines that follow it) marks the carry-over
    // boundary. We use the LAST (rightmost) occurrence in currLines to maximise
    // the cut point and output the fewest re-duplicated lines.
    for (let p = prevConvEnd - 1; p >= 0; p--) {
      const target = prevStripped[p]
      if (!target) continue

      const positions = currIndex.get(target)
      if (!positions) continue

      // Try each occurrence from rightmost to leftmost.
      for (let k = positions.length - 1; k >= 0; k--) {
        const c = positions[k]
        let verified = true
        for (let v = 1; v <= VERIFY_LEN; v++) {
          const pi = p + v
          const ci = c + v
          // Reaching the end of prevConv (before status bar) is a valid boundary.
          if (pi >= prevConvEnd || ci >= currStripped.length) break
          if (prevStripped[pi] !== currStripped[ci]) { verified = false; break }
        }
        if (verified) return c + 1  // first new line is right after this
      }
    }

    return 0  // no carry-over found — output entire current block
  }

  const parts: Uint8Array[] = []

  // First block: output its full content (earliest visible conversation).
  parts.push(getContent(blocks[0]))
  let prevLines = blockLines(blocks[0])

  for (let b = 1; b < blocks.length; b++) {
    const currLines = blockLines(blocks[b])
    const newStart = findNewStart(prevLines, currLines)

    if (newStart < currLines.length) {
      const newText = currLines.slice(newStart).join('\r\n')
      if (newText.trim()) {
        // Join to previous content with CRLF (standard terminal line ending).
        parts.push(enc.encode('\r\n' + newText))
      }
    }

    prevLines = currLines
  }

  // Append stripped raw bytes after the last block.
  const lastBlock = blocks[blocks.length - 1]
  const after = stripSyncBlocks(input.subarray(lastBlock.blockEnd))
  if (after.length > 0) parts.push(after)

  return concatU8(...parts)
}

function concatU8(...arrays: Uint8Array[]): Uint8Array {
  const total = arrays.reduce((n, a) => n + a.length, 0)
  const out = new Uint8Array(total)
  let off = 0
  for (const a of arrays) { out.set(a, off); off += a.length }
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
