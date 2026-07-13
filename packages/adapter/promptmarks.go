package adapter

import "bytes"

// PromptMarkTracker derives a busy/idle signal from OSC 133 prompt
// marks ("semantic prompt" / FinalTerm sequences) in a raw PTY byte
// stream. The runner tracks marks on every session whose adapter is
// not hook-driven (see HookDriven): shells with an emitting
// integration get the same crisp busy→idle Status transitions that
// agent adapters report via hooks, upgrading the session from the
// default lifetime-as-turn model (active from launch to exit) to
// per-command prompt-cycle turns.
//
// Mark semantics (terminator is BEL or ST, extra params after the mark
// letter — "133;D;0", kitty's ";k=s" — are ignored):
//
//	OSC 133;C → working (command execution started)
//	OSC 133;D → idle    (command finished)
//	OSC 133;A → idle    (a fresh prompt is being drawn)
//	OSC 133;B → ignored (prompt end / input start: the shell is idle,
//	            but that was already established by the preceding A)
//
// Both D and A map to idle so integrations that emit only one of the
// two still produce a usable signal; the tracker dedupes, so a D
// immediately followed by an A reports a single transition.
//
// Feed is a streaming parser: escape sequences split across Feed calls
// (PTY reads chunk arbitrarily) are handled, and — critically — every
// transition inside a single chunk is reported in order. A fast command
// whose C and A marks arrive in one PTY read still produces the full
// working→idle pulse, which is what `gmux send --wait` keys on.
//
// Not safe for concurrent use; the runner feeds it from the single
// readPTY flush path.
type PromptMarkTracker struct {
	// onTransition is invoked once per busy/idle transition, in stream
	// order. The first observed mark always fires (there is no prior
	// state to dedupe against).
	onTransition func(working bool)

	state   promptParseState
	payload []byte // first bytes of the current OSC payload (capped)

	working bool // last reported polarity
	seen    bool // whether any transition has been reported yet
}

// SawMark reports whether any actionable prompt mark (133;A/C/D) has
// been observed. The runner uses this as the upgrade signal: once a
// session demonstrates prompt cycles, its turns are delimited by the
// marks and process exit no longer closes a turn by itself.
//
// Like Feed, not safe for concurrent use; the runner reads it only
// after the final PTY flush (PTYDone) has completed.
func (t *PromptMarkTracker) SawMark() bool { return t.seen }

type promptParseState uint8

const (
	ppGround promptParseState = iota // scanning for ESC
	ppEsc                            // saw ESC, expecting ']' for OSC
	ppOsc                            // inside OSC, consuming until BEL / ST
	ppOscEsc                         // inside OSC, saw ESC (ST if '\' follows)
)

// promptPayloadCap bounds how much of an OSC payload is retained for
// the prefix check. "133;X" is 5 bytes; anything longer than the cap
// can't be a bare prompt mark prefix we care about, and capping keeps
// unrelated giant OSCs (e.g. long titles) from growing the buffer.
const promptPayloadCap = 8

// NewPromptMarkTracker returns a tracker that calls onTransition for
// every busy/idle transition derived from the fed bytes.
func NewPromptMarkTracker(onTransition func(working bool)) *PromptMarkTracker {
	return &PromptMarkTracker{
		onTransition: onTransition,
		payload:      make([]byte, 0, promptPayloadCap),
	}
}

// Feed consumes the next chunk of raw PTY output. Parser state persists
// across calls, so sequences split between chunks are recognized.
func (t *PromptMarkTracker) Feed(data []byte) {
	for i := 0; i < len(data); i++ {
		b := data[i]
		switch t.state {
		case ppGround:
			if b != 0x1b {
				// Fast-skip: jump straight to the next ESC instead of
				// walking every byte of ordinary output.
				j := bytes.IndexByte(data[i:], 0x1b)
				if j < 0 {
					return
				}
				i += j
			}
			t.state = ppEsc
		case ppEsc:
			switch b {
			case ']':
				t.state = ppOsc
				t.payload = t.payload[:0]
			case 0x1b:
				// Another ESC: still at the start of an escape sequence.
			default:
				// Some other escape sequence (CSI, charset, ...). Not an
				// OSC; back to scanning. CSI parameter bytes can't contain
				// a raw ESC, so skipping their body is safe.
				t.state = ppGround
			}
		case ppOsc:
			switch b {
			case 0x07: // BEL terminator
				t.finishOSC()
			case 0x1b:
				t.state = ppOscEsc
			default:
				if len(t.payload) < promptPayloadCap {
					t.payload = append(t.payload, b)
				}
			}
		case ppOscEsc:
			switch b {
			case '\\': // ST terminator (ESC \)
				t.finishOSC()
			case ']':
				// The ESC aborted the OSC and immediately starts a new one.
				t.state = ppOsc
				t.payload = t.payload[:0]
			case 0x1b:
				// ESC ESC: the first aborted the OSC, the second starts a
				// fresh escape sequence.
				t.state = ppEsc
			default:
				// ESC + other: aborted OSC followed by some non-OSC escape.
				t.state = ppGround
			}
		}
	}
}

// finishOSC inspects the completed OSC payload and reports a transition
// when it is a recognized 133 prompt mark.
func (t *PromptMarkTracker) finishOSC() {
	p := t.payload
	t.payload = t.payload[:0]
	t.state = ppGround

	if len(p) < 5 || string(p[:4]) != "133;" {
		return
	}
	// Anything after the mark letter must be a parameter separator
	// ("133;D;0", "133;A;k=s"), otherwise it's not a prompt mark.
	if len(p) > 5 && p[5] != ';' {
		return
	}
	switch p[4] {
	case 'C':
		t.transition(true)
	case 'A', 'D':
		t.transition(false)
	}
}

// transition dedupes and reports a polarity change. The first observed
// mark always reports: a session's initial prompt (133;A) is what
// establishes "this shell emits marks" downstream, so it must surface
// even though nothing changed relative to the zero value.
func (t *PromptMarkTracker) transition(working bool) {
	if t.seen && t.working == working {
		return
	}
	t.seen = true
	t.working = working
	if t.onTransition != nil {
		t.onTransition(working)
	}
}
