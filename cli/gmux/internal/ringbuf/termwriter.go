package ringbuf

// maxPendingLine is the maximum size of the pending line buffer before
// it is flushed to the ring buffer unconditionally. This prevents
// unbounded memory growth from programs that output large amounts of
// data without newlines (e.g. base64-encoded images via kitty graphics).
// Set to 64KB, which is well above any realistic terminal line.
const maxPendingLine = 64 * 1024

// TermWriter wraps a RingBuf with terminal-awareness. It collapses
// carriage-return overwrites (e.g. spinner frames) and resets on screen
// clears, so the scrollback buffer stores meaningful content rather than
// redundant animation frames.
//
// Only complete lines (terminated by \n) are flushed to the ring buffer.
// The current partial line is held in memory until a newline arrives or
// it exceeds maxPendingLine bytes. Snapshot includes the pending partial
// line without flushing it.
//
// TermWriter is not safe for concurrent use. Callers must provide
// external synchronization (e.g. the ptyserver mutex).
type TermWriter struct {
	rb          *RingBuf
	currentLine []byte
	pendingCR   bool // CR at end of previous chunk, awaiting next byte
}

// NewTermWriter creates a TermWriter that writes to the given RingBuf.
func NewTermWriter(rb *RingBuf) *TermWriter {
	return &TermWriter{rb: rb}
}

// Write processes terminal data, collapsing CR-overwritten content and
// resetting the buffer on screen clears, then writes complete lines to
// the underlying RingBuf.
func (tw *TermWriter) Write(data []byte) {
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
			// Bare CR confirmed: discard current line content.
			tw.currentLine = tw.currentLine[:0]
			// Fall through to process data[i] normally.
		}

		// Check for erase-display sequences: ESC [ 2 J or ESC [ 3 J.
		if data[i] == 0x1b {
			if n := matchClear(data[i:]); n > 0 {
				tw.currentLine = tw.currentLine[:0]
				tw.rb.Reset()
				i += n
				continue
			}
		}

		switch data[i] {
		case '\r':
			if i+1 < len(data) {
				if data[i+1] == '\n' {
					// CRLF: flush line.
					tw.currentLine = append(tw.currentLine, '\r', '\n')
					tw.rb.Write(tw.currentLine)
					tw.currentLine = tw.currentLine[:0]
					i += 2
				} else {
					// Bare CR: discard current line (overwrite).
					tw.currentLine = tw.currentLine[:0]
					i++
				}
			} else {
				// CR at end of chunk; defer until next Write.
				tw.pendingCR = true
				i++
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
			for i < len(data) && data[i] != '\r' && data[i] != '\n' && data[i] != 0x1b {
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
func (tw *TermWriter) Snapshot() []byte {
	snap := tw.rb.Snapshot()
	pending := len(tw.currentLine)
	if tw.pendingCR {
		pending++
	}
	if pending == 0 {
		return snap
	}
	result := make([]byte, len(snap)+pending)
	copy(result, snap)
	copy(result[len(snap):], tw.currentLine)
	if tw.pendingCR {
		result[len(result)-1] = '\r'
	}
	return result
}

// Len returns the total bytes stored (ring buffer + pending line).
func (tw *TermWriter) Len() int {
	n := tw.rb.Len() + len(tw.currentLine)
	if tw.pendingCR {
		n++
	}
	return n
}

// Reset clears both the ring buffer and any pending line data.
func (tw *TermWriter) Reset() {
	tw.rb.Reset()
	tw.currentLine = tw.currentLine[:0]
	tw.pendingCR = false
}

// matchClear checks if data starts with ESC [ 2 J (erase display) or
// ESC [ 3 J (erase scrollback) and returns the sequence length, or 0.
func matchClear(data []byte) int {
	if len(data) < 4 {
		return 0
	}
	if data[0] == 0x1b && data[1] == '[' && (data[2] == '2' || data[2] == '3') && data[3] == 'J' {
		return 4
	}
	return 0
}
