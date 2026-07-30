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
// emulator. It handles the 7-bit and 8-bit C1 forms of CSI, OSC, ST, and
// DCS/SOS/PM/APC control strings, plus lone two-byte ESC sequences. UTF-8 text
// is preserved, including continuation bytes whose values overlap 8-bit C1.
type ansiStrippingWriter struct {
	w             io.Writer
	state         ansiStripState
	utf8Remaining uint8
	utf8Visible   bool
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
		// A UTF-8 continuation can have the same byte value as an 8-bit C1
		// control. Track the sequence independently of the ANSI state so a
		// continuation is data rather than a CSI/string introducer or ST.
		// Text data is forwarded; data inside escape/control sequences is
		// discarded without affecting the ANSI parser state.
		if a.utf8Remaining > 0 {
			if c >= 0x80 && c <= 0xbf {
				if a.utf8Visible {
					out = append(out, c)
				}
				a.utf8Remaining--
				continue
			}
			// Keep the existing pragmatic malformed-input behavior: abandon
			// the incomplete rune and interpret this byte normally.
			a.utf8Remaining = 0
			a.utf8Visible = false
		}
		switch {
		case c >= 0xc2 && c <= 0xdf:
			a.utf8Remaining = 1
		case c >= 0xe0 && c <= 0xef:
			a.utf8Remaining = 2
		case c >= 0xf0 && c <= 0xf4:
			a.utf8Remaining = 3
		}
		if a.utf8Remaining > 0 {
			a.utf8Visible = a.state == ansiStripText
		}

		switch a.state {
		case ansiStripText:
			switch c {
			case 0x1b:
				a.state = ansiStripEsc
			case '\r':
				// dropped: CRLF becomes LF, bare-CR redraws collapse
			case 0x9b: // 8-bit CSI
				a.state = ansiStripCSI
			case 0x9d: // 8-bit OSC
				a.state = ansiStripOSC
			case 0x90, 0x98, 0x9e, 0x9f: // 8-bit DCS, SOS, PM, APC
				a.state = ansiStripString
			case 0x80, 0x81, 0x82, 0x83, 0x84, 0x85, 0x86, 0x87,
				0x88, 0x89, 0x8a, 0x8b, 0x8c, 0x8d, 0x8e, 0x8f,
				0x91, 0x92, 0x93, 0x94, 0x95, 0x96, 0x97, 0x99,
				0x9a, 0x9c: // other single-byte C1 controls, including standalone ST
				// dropped
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
			case 0x07, 0x9c: // BEL or 8-bit ST terminator
				a.state = ansiStripText
			case 0x1b: // possible ESC \ (7-bit ST) terminator
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
			switch c {
			case 0x9c: // 8-bit ST
				a.state = ansiStripText
			case 0x1b: // possible ESC \ (7-bit ST)
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
