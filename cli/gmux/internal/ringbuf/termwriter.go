package ringbuf

// maxPendingLine is the maximum size of the pending line buffer before
// it is flushed to the ring buffer unconditionally. This prevents
// unbounded memory growth from programs that output large amounts of
// data without newlines (e.g. base64-encoded images via kitty graphics).
// Set to 64KB, which is well above any realistic terminal line.
const maxPendingLine = 64 * 1024

// TermWriter wraps a RingBuf with terminal-awareness. It collapses
// carriage-return overwrites (e.g. spinner frames) so the scrollback
// buffer stores meaningful content rather than redundant animation frames.
//
// Screen clear sequences (ESC[2J, ESC[3J) are passed through as regular
// content rather than resetting the buffer. This preserves scrollback for
// TUI apps like pi and claude that use clears as part of normal rendering.
// WebSocket clients that replay the scrollback will process the clear
// sequences naturally, showing only post-clear content on screen.
//
// CR handling accounts for PTY line discipline: the kernel's onlcr flag
// translates \n to \r\n, so a program writing \r\n produces \r\r\n on the
// master side. Any run of \r characters followed by \n is treated as a
// single CRLF line terminator. Only \r followed by a printable character
// (not \r or \n) triggers line-overwrite collapsing.
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

// Write processes terminal data, collapsing CR-overwritten content,
// then writes complete lines to the underlying RingBuf.
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
				// \r\r followed by non-LF: bare CR, discard line.
				tw.currentLine = tw.currentLine[:0]
				continue
			}
			// Bare CR confirmed (followed by non-CR, non-LF): discard.
			tw.currentLine = tw.currentLine[:0]
			// Fall through to process data[i] normally.
		}

		switch data[i] {
		case '\r':
			// Scan past consecutive CRs to find the terminating byte.
			// \r+\n → CRLF (flush line). \r+<other> → bare CR (discard line).
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
				// \r+<non-LF>: bare CR, discard current line (overwrite).
				tw.currentLine = tw.currentLine[:0]
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
