package ringbuf

import "bytes"

// maxPendingLine is the maximum size of the pending line buffer before
// it is flushed to the ring buffer unconditionally. This prevents
// unbounded memory growth from programs that output large amounts of
// data without newlines (e.g. base64-encoded images via kitty graphics).
// Set to 64KB, which is well above any realistic terminal line.
const maxPendingLine = 64 * 1024

// TermWriter wraps a RingBuf with terminal-awareness.
//
// Screen clear sequences (ESC[2J, ESC[3J) reset the ring buffer, discarding
// all prior content. The scrollback is the source of truth for session
// replay; keeping pre-clear content would cause stale lines to appear when
// a client connects and replays the snapshot. The clear sequence itself is
// also discarded since the replaying terminal starts with a clean screen.
//
// CR handling accounts for PTY line discipline: the kernel's onlcr flag
// translates \n to \r\n, so a program writing \r\n produces \r\r\n on the
// master side. Any run of \r characters followed by \n is treated as a
// single CRLF line terminator.
//
// A bare CR (\r followed by a non-\r, non-\n byte) flushes the pending
// line content plus the \r to the ring buffer. This preserves cursor
// movement sequences that TUI renderers (e.g. bubbletea) place before \r
// as part of cursor positioning. On replay, the \r returns the cursor to
// column 1, producing the correct visual result for both cursor positioning
// and spinner-style overwrites.
//
// Only complete lines (terminated by \n) are flushed to the ring buffer.
// The current partial line is held in memory until a newline arrives or
// it exceeds maxPendingLine bytes. Snapshot includes the pending partial
// line without flushing it.
//
// When the ring buffer has wrapped (Full), Snapshot trims to the last
// frame-start marker (BSU or cursor-home) so replay starts at the
// beginning of a complete TUI render frame. Falls back to trimming to
// the first newline if no marker is found (plain shell output).
//
// TermWriter is not safe for concurrent use. Callers must provide
// external synchronization (e.g. the ptyserver mutex).
type TermWriter struct {
	rb          *RingBuf
	currentLine []byte
	pendingCR   bool   // CR at end of previous chunk, awaiting next byte
	escBuf      []byte // partial clear-sequence prefix held across chunks (up to 3 bytes)
}

// NewTermWriter creates a TermWriter that writes to the given RingBuf.
func NewTermWriter(rb *RingBuf) *TermWriter {
	return &TermWriter{rb: rb}
}

// lastClearEnd returns the byte offset immediately after the last ESC[2J
// or ESC[3J in data, or -1 if none is found.
func lastClearEnd(data []byte) int {
	best := -1
	for i := 0; i <= len(data)-4; i++ {
		if data[i] == '\x1b' && data[i+1] == '[' &&
			(data[i+2] == '2' || data[i+2] == '3') && data[i+3] == 'J' {
			best = i + 4
		}
	}
	return best
}

// clearTail returns how many bytes at the end of data could be the prefix
// of a clear sequence (ESC[2J or ESC[3J). Returns 0 when the tail cannot
// start a clear.
func clearTail(data []byte) int {
	n := len(data)
	if n >= 3 && data[n-3] == '\x1b' && data[n-2] == '[' &&
		(data[n-1] == '2' || data[n-1] == '3') {
		return 3
	}
	if n >= 2 && data[n-2] == '\x1b' && data[n-1] == '[' {
		return 2
	}
	if n >= 1 && data[n-1] == '\x1b' {
		return 1
	}
	return 0
}

// Write processes terminal data: detects screen clears, flushes
// CR-positioned content, then writes complete lines to the RingBuf.
func (tw *TermWriter) Write(data []byte) {
	if len(data) == 0 {
		return
	}

	// Prepend any partial escape sequence saved from the previous chunk
	// so that split clear sequences (e.g. "\x1b[" | "2J") are detected.
	if len(tw.escBuf) > 0 {
		combined := make([]byte, len(tw.escBuf)+len(data))
		copy(combined, tw.escBuf)
		copy(combined[len(tw.escBuf):], data)
		data = combined
		tw.escBuf = tw.escBuf[:0]
	}

	// Find the last screen-clear sequence (ESC[2J or ESC[3J) and discard
	// everything before it. This prevents stale content from appearing
	// when the scrollback is replayed to a new client.
	if clearEnd := lastClearEnd(data); clearEnd >= 0 {
		tw.rb.Reset()
		tw.currentLine = tw.currentLine[:0]
		tw.pendingCR = false
		data = data[clearEnd:]
	}

	// If data ends with a partial clear prefix, hold it back until the
	// next Write determines whether it completes a clear sequence.
	if tail := clearTail(data); tail > 0 {
		tw.escBuf = append(tw.escBuf[:0], data[len(data)-tail:]...)
		data = data[:len(data)-tail]
	}

	// Process the remaining data through line/CR logic.
	tw.processLines(data)
}

// flushBareCR handles a bare CR (\r followed by non-LF content).
// Instead of discarding currentLine (which would lose cursor-positioning
// escape sequences), it flushes currentLine + \r to the ring buffer.
// On replay, the \r returns the cursor to column 1, matching what the
// original terminal saw.
//
// This preserves TUI cursor movement (e.g. bubbletea's \x1b[3A\r pattern)
// while still producing correct visual output for spinner-style overwrites:
// "frame1\rframe2\r\n" replays as frame1 then \r (col 1) then frame2
// overwriting frame1.
func (tw *TermWriter) flushBareCR() {
	if len(tw.currentLine) > 0 {
		tw.currentLine = append(tw.currentLine, '\r')
		tw.rb.Write(tw.currentLine)
	}
	tw.currentLine = tw.currentLine[:0]
}

// processLines handles CR flushing and newline-delimited flushing.
func (tw *TermWriter) processLines(data []byte) {
	for i := 0; i < len(data); {
		// Resolve a CR that was deferred from the previous chunk.
		if tw.pendingCR {
			tw.pendingCR = false
			if data[i] == '\n' {
				// Was CRLF split across chunks.
				tw.currentLine = append(tw.currentLine, '\r', '\n')
				tw.rb.Write(tw.currentLine)
				tw.currentLine = tw.currentLine[:0]
				i++
				continue
			}
			if data[i] == '\r' {
				// Another CR: skip the pending one and let this new CR
				// be processed normally (handles \r\r\n from PTY onlcr).
				i++
				// Look ahead: if this \r is also at chunk end, defer again.
				if i >= len(data) {
					tw.pendingCR = true
					continue
				}
				if data[i] == '\n' {
					// \r\r\n: treat as CRLF.
					tw.currentLine = append(tw.currentLine, '\r', '\n')
					tw.rb.Write(tw.currentLine)
					tw.currentLine = tw.currentLine[:0]
					i++
					continue
				}
				// \r\r followed by non-LF: bare CR. Flush with \r to preserve
				// cursor positioning sequences for replay.
				tw.flushBareCR()
				continue
			}
			// Bare CR confirmed (followed by non-CR, non-LF).
			// Flush with \r to preserve cursor positioning.
			tw.flushBareCR()
			// Fall through to process data[i] normally.
		}

		switch data[i] {
		case '\r':
			// Scan past consecutive CRs to find the terminating byte.
			// \r+\n → CRLF (flush line). \r+<other> → bare CR (flush with \r).
			j := i + 1
			for j < len(data) && data[j] == '\r' {
				j++
			}
			if j >= len(data) {
				// All CRs at end of chunk; defer.
				tw.pendingCR = true
				i = j
			} else if data[j] == '\n' {
				// \r+\n: CRLF, flush line.
				tw.currentLine = append(tw.currentLine, '\r', '\n')
				tw.rb.Write(tw.currentLine)
				tw.currentLine = tw.currentLine[:0]
				i = j + 1
			} else {
				// \r+<non-LF>: bare CR. Flush with \r to preserve
				// cursor positioning sequences for replay.
				tw.flushBareCR()
				i = j
			}

		case '\n':
			tw.currentLine = append(tw.currentLine, '\n')
			tw.rb.Write(tw.currentLine)
			tw.currentLine = tw.currentLine[:0]
			i++

		default:
			// Scan ahead to the next control byte for bulk append.
			start := i
			i++
			for i < len(data) && data[i] != '\r' && data[i] != '\n' {
				i++
			}
			tw.currentLine = append(tw.currentLine, data[start:i]...)
		}

		// Safety valve: flush oversized pending lines to the ring buffer
		// to prevent unbounded memory growth. This sacrifices CR compaction
		// for lines longer than maxPendingLine, which in practice are never
		// spinner lines.
		if len(tw.currentLine) >= maxPendingLine {
			tw.rb.Write(tw.currentLine)
			tw.currentLine = tw.currentLine[:0]
		}
	}
}

// Snapshot returns the current buffer contents in chronological order,
// including any partial line not yet flushed to the ring buffer.
//
// When the ring buffer has wrapped, the snapshot is trimmed to the last
// frame-start marker (BSU or cursor-home) so that replay starts at the
// beginning of a complete TUI render frame. This prevents stale frames
// from stacking on replay (old content scrolls into terminal scrollback
// before being overwritten). Falls back to trimming to the first newline
// if no marker is found (plain shell output).
func (tw *TermWriter) Snapshot() []byte {
	snap := tw.rb.Snapshot()

	if tw.rb.Full() && len(snap) > 0 {
		snap = trimWrappedSnapshot(snap)
	}

	pending := len(tw.currentLine) + len(tw.escBuf)
	if tw.pendingCR {
		pending++
	}
	if pending == 0 {
		return snap
	}
	result := make([]byte, len(snap)+pending)
	n := copy(result, snap)
	n += copy(result[n:], tw.currentLine)
	if tw.pendingCR {
		result[n] = '\r'
		n++
	}
	copy(result[n:], tw.escBuf)
	return result
}

// lastFrameStart returns the byte offset of the last TUI frame-start
// marker in data, or -1 if none is found.
//
// Markers checked (in priority order):
//   - BSU (\x1b[?2026h): DEC 2026 synchronized update, used by pi-tui
//     for every differential render frame
//   - Cursor home (ESC[H or ESC[1;1H): used by some TUI frameworks for
//     full-screen redraws
func lastFrameStart(data []byte) int {
	best := -1
	for i := 0; i < len(data); i++ {
		if data[i] != '\x1b' || i+1 >= len(data) {
			continue
		}
		// BSU: \x1b[?2026h (8 bytes)
		if i+7 < len(data) && data[i+1] == '[' && data[i+2] == '?' &&
			data[i+3] == '2' && data[i+4] == '0' && data[i+5] == '2' &&
			data[i+6] == '6' && data[i+7] == 'h' {
			best = i
			continue
		}
		if data[i+1] != '[' {
			continue
		}
		// ESC[H (3 bytes)
		if i+2 < len(data) && data[i+2] == 'H' {
			best = i
			continue
		}
		// ESC[1;1H (6 bytes)
		if i+5 < len(data) &&
			data[i+2] == '1' && data[i+3] == ';' &&
			data[i+4] == '1' && data[i+5] == 'H' {
			best = i
		}
	}
	return best
}

// trimWrappedSnapshot trims a wrapped ring buffer snapshot to a good
// replay starting point.
//
// Strategy (tried in order):
//  1. Last frame-start marker (BSU or cursor-home): start of the latest
//     TUI render frame. This prevents duplicate "groups of lines" caused
//     by replaying multiple overlapping render frames.
//  2. First newline: ensures replay doesn't start mid-line (for plain
//     shell output with no cursor positioning).
//  3. No trimming: return as-is (single long line that fills the buffer).
func trimWrappedSnapshot(snap []byte) []byte {
	// Strategy 1: trim to the last frame-start marker.
	if idx := lastFrameStart(snap); idx > 0 {
		return snap[idx:]
	}

	// Strategy 2: trim to the first newline.
	if idx := bytes.IndexByte(snap, '\n'); idx >= 0 && idx < len(snap)-1 {
		return snap[idx+1:]
	}

	// Strategy 3: no good trim point found.
	return snap
}

// Len returns the total bytes stored (ring buffer + pending line + escape buffer).
func (tw *TermWriter) Len() int {
	n := tw.rb.Len() + len(tw.currentLine) + len(tw.escBuf)
	if tw.pendingCR {
		n++
	}
	return n
}

// Reset clears both the ring buffer and any pending line data.
func (tw *TermWriter) Reset() {
	tw.rb.Reset()
	tw.currentLine = tw.currentLine[:0]
	tw.escBuf = tw.escBuf[:0]
	tw.pendingCR = false
}
