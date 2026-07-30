package main

import "io"

// ansiStrippingWriter is a stream-safe variant of stripANSI: it removes
// ANSI/VT escape sequences and carriage returns from PTY output on its way
// to a downstream writer, carrying parser state across Write calls so an
// escape sequence split over two PTY reads is still recognized. It exists
// for the non-interactive `gmux -- <cmd>` flow, where the child's PTY output
// is relayed to the caller's stdout: the UI and scrollback keep the full
// escape stream, only this relay is cleaned.
//
// Dropping every '\r' both normalises the PTY's CRLF line endings to LF and
// collapses bare-CR progress redraws, matching stripANSI's behaviour for
// `gmux tail`. Like stripANSI it is a pragmatic stripper, not a terminal
// emulator: CSI (ESC [ ... final-byte), OSC (ESC ] ... BEL/ST), DCS/SOS/PM/APC
// control strings (ESC P/X/^/_ ... ST), and lone two-byte escapes are handled.
type ansiStrippingWriter struct {
	w     io.Writer
	state ansiStripState
}

type ansiStripState int

const (
	ansiStripText      ansiStripState = iota // ordinary output
	ansiStripEsc                             // seen ESC, awaiting the selector byte
	ansiStripCSI                             // inside ESC [ ... awaiting final byte 0x40-0x7e
	ansiStripOSC                             // inside ESC ] ... awaiting BEL or ESC \
	ansiStripOSCEsc                          // inside OSC, seen ESC (possible ST)
	ansiStripString                          // inside DCS/SOS/PM/APC ... awaiting ESC \
	ansiStripStringEsc                       // inside a control string, seen ESC (possible ST)
)

func newANSIStrippingWriter(w io.Writer) *ansiStrippingWriter {
	return &ansiStrippingWriter{w: w}
}

// Write filters p and forwards the surviving bytes downstream. It reports
// len(p) consumed on success: bytes swallowed by the filter are consumed by
// design, not lost.
func (a *ansiStrippingWriter) Write(p []byte) (int, error) {
	out := make([]byte, 0, len(p))
	for _, c := range p {
		switch a.state {
		case ansiStripText:
			switch c {
			case 0x1b:
				a.state = ansiStripEsc
			case '\r':
				// dropped: CRLF becomes LF, bare-CR redraws collapse
			default:
				out = append(out, c)
			}
		case ansiStripEsc:
			switch c {
			case '[':
				a.state = ansiStripCSI
			case ']':
				a.state = ansiStripOSC
			case 'P', 'X', '^', '_': // DCS, SOS, PM, APC: terminated by ST
				a.state = ansiStripString
			default: // two-byte escape: the selector is the last byte
				a.state = ansiStripText
			}
		case ansiStripCSI:
			if c >= 0x40 && c <= 0x7e { // final byte
				a.state = ansiStripText
			}
		case ansiStripOSC:
			switch c {
			case 0x07: // BEL terminator
				a.state = ansiStripText
			case 0x1b: // possible ESC \ (ST) terminator
				a.state = ansiStripOSCEsc
			}
		case ansiStripOSCEsc:
			if c == '\\' { // ST
				a.state = ansiStripText
			} else {
				// Not a terminator; stay inside the OSC string. A second
				// ESC keeps the maybe-ST state alive.
				if c != 0x1b {
					a.state = ansiStripOSC
				}
			}
		case ansiStripString:
			if c == 0x1b {
				a.state = ansiStripStringEsc
			}
		case ansiStripStringEsc:
			if c == '\\' { // ST
				a.state = ansiStripText
			} else if c != 0x1b {
				// Not a terminator. A second ESC remains a possible ST.
				a.state = ansiStripString
			}
		}
	}
	if len(out) > 0 {
		n, err := a.w.Write(out)
		if err != nil {
			return 0, err
		}
		if n != len(out) {
			return 0, io.ErrShortWrite
		}
	}
	return len(p), nil
}
