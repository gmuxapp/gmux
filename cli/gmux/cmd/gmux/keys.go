package main

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// Key-name handling for `gmux send` and `gmux send-keys`. Names follow
// tmux's send-keys vocabulary so existing tmux knowledge transfers
// (ADR 0009). A "key" renders to the byte sequence an xterm-class
// terminal emits for that key; gmux writes those bytes to the session
// PTY's input exactly as if typed.
//
// Modifiers use tmux's prefix spelling — C- (control), M- (alt/meta),
// S- (shift) — in any order and any combination the target key supports.
// The encodings are xterm's, not an invention:
//
//   - M-<char> is the ESC prefix (8-bit meta is never emitted: no
//     terminal in a UTF-8 world can distinguish 0xE1 from a text byte).
//     C-M-<char> is ESC followed by the control byte.
//   - special keys carry the standard xterm modifier parameter
//     mod = 1 + shift(1) + alt(2) + ctrl(4); cursor/Home/End keys become
//     CSI 1;<mod><final> and the tilde keys CSI <n>;<mod>~.
//   - F1-F4 are SS3 P/Q/R/S unmodified and CSI 1;<mod> P/Q/R/S modified
//     (xterm's own irregularity); F5-F12 are tilde keys throughout.
//
// Unmodified keys keep their historical byte-for-byte sequences.
//
// What is deliberately NOT here matters as much as what is, because an
// unrecognized token in key position is a `send` error (not silent text),
// and inventing bytes for a key whose encoding is not standard would be
// worse than refusing it:
//
//   - **S-<char>.** Shift on a plain character is not a distinct key — the
//     terminal sends the shifted character itself — so `S-a` has no
//     encoding (send `A`). Shift only survives as a modifier parameter on
//     the special keys.
//   - **Modified Enter/Tab/Space/Escape/BSpace**, except shift-Tab.
//     `C-Tab`, `M-Enter`, `C-Enter`, `M-Escape`, `S-Space` and friends have
//     no single encoding an xterm-class terminal agrees on: they depend on
//     the emulator and on whether a keyboard protocol (kitty, modifyOtherKeys,
//     CSI u) is negotiated — which gmux cannot know from here. Shift-Tab is
//     the exception because CSI Z is universal, and it is spelled BTab (tmux's
//     name) with S-Tab as an alias.
//   - **F13 and up, and the keypad.** Same reason: no agreed encoding.
//
// The modifiable set is therefore closed and enumerated (csiFinalKeys and
// csiTildeKeys below), and the help page names it rather than implying
// "any combination".

// namedKeys maps a key name to the bytes it produces when unmodified.
// Modified forms are computed in keyBytes from the tables below.
var namedKeys = map[string]string{
	"Enter":     "\r",
	"Tab":       "\t",
	"Space":     " ",
	"Escape":    "\x1b",
	"Esc":       "\x1b",
	"BSpace":    "\x7f",
	"Backspace": "\x7f",
	"Up":        "\x1b[A",
	"Down":      "\x1b[B",
	"Right":     "\x1b[C",
	"Left":      "\x1b[D",
	"Home":      "\x1b[H",
	"End":       "\x1b[F",
	"PageUp":    "\x1b[5~",
	"PageDown":  "\x1b[6~",
	"PPage":     "\x1b[5~", // tmux's names for the page keys
	"NPage":     "\x1b[6~",
	"Delete":    "\x1b[3~",
	"DC":        "\x1b[3~",
	"BTab":      "\x1b[Z", // back-tab (shift-Tab): CSI Z, spelled S-Tab too
	"Insert":    "\x1b[2~",
	"IC":        "\x1b[2~",
	"F1":        "\x1bOP",
	"F2":        "\x1bOQ",
	"F3":        "\x1bOR",
	"F4":        "\x1bOS",
	"F5":        "\x1b[15~",
	"F6":        "\x1b[17~",
	"F7":        "\x1b[18~",
	"F8":        "\x1b[19~",
	"F9":        "\x1b[20~",
	"F10":       "\x1b[21~",
	"F11":       "\x1b[23~",
	"F12":       "\x1b[24~",
}

// csiFinalKeys are the keys encoded as CSI <final> unmodified and
// CSI 1;<mod><final> modified.
var csiFinalKeys = map[string]byte{
	"Up":    'A',
	"Down":  'B',
	"Right": 'C',
	"Left":  'D',
	"Home":  'H',
	"End":   'F',
	"F1":    'P',
	"F2":    'Q',
	"F3":    'R',
	"F4":    'S',
}

// csiTildeKeys are the keys encoded as CSI <n>~ unmodified and
// CSI <n>;<mod>~ modified.
var csiTildeKeys = map[string]int{
	"Insert":   2,
	"IC":       2,
	"Delete":   3,
	"DC":       3,
	"PageUp":   5,
	"PageDown": 6,
	"PPage":    5,
	"NPage":    6,
	"F5":       15,
	"F6":       17,
	"F7":       18,
	"F8":       19,
	"F9":       20,
	"F10":      21,
	"F11":      23,
	"F12":      24,
}

// isKeyName reports whether s is a recognized key-name token. Used by
// `gmux send` to tell a trailing key (Enter, C-c, M-x) from literal
// text.
func isKeyName(s string) bool {
	_, ok := keyBytes(s)
	return ok
}

// keyModifiers is the parsed modifier set of a key token.
type keyModifiers struct {
	ctrl, alt, shift bool
}

// param is xterm's modifier parameter: 1 + shift + 2*alt + 4*ctrl.
func (m keyModifiers) param() int {
	p := 1
	if m.shift {
		p += 1
	}
	if m.alt {
		p += 2
	}
	if m.ctrl {
		p += 4
	}
	return p
}

func (m keyModifiers) any() bool { return m.ctrl || m.alt || m.shift }

// splitModifiers peels tmux-style C-/M-/S- prefixes (case-insensitive,
// any order) off a key token and returns the remaining key name.
func splitModifiers(name string) (keyModifiers, string) {
	var mods keyModifiers
	for len(name) >= 2 && name[1] == '-' {
		switch name[0] {
		case 'C', 'c':
			mods.ctrl = true
		case 'M', 'm':
			mods.alt = true
		case 'S', 's':
			mods.shift = true
		default:
			return mods, name
		}
		name = name[2:]
	}
	return mods, name
}

// keyBytes renders a single key name to its byte sequence.
func keyBytes(name string) (string, bool) {
	// Unmodified names first: their bytes are frozen, and the lookup also
	// protects names that begin with a modifier-shaped pair.
	if b, ok := namedKeys[name]; ok {
		return b, true
	}
	mods, base := splitModifiers(name)
	if !mods.any() || base == "" {
		return "", false
	}
	// Shift-Tab is the one modified "typing" key with a universal encoding.
	// Any other modifier on Tab (C-Tab, M-Tab) is terminal-dependent and
	// stays unrecognized rather than guessed.
	if base == "Tab" {
		if mods.shift && !mods.ctrl && !mods.alt {
			return namedKeys["BTab"], true
		}
		return "", false
	}
	if seq, ok := modifiedSpecialKey(mods, base); ok {
		return seq, true
	}
	return modifiedCharKey(mods, base)
}

// modifiedSpecialKey renders a special (non-character) key with
// modifiers applied.
func modifiedSpecialKey(mods keyModifiers, base string) (string, bool) {
	p := strconv.Itoa(mods.param())
	if final, ok := csiFinalKeys[base]; ok {
		return "\x1b[1;" + p + string(final), true
	}
	if n, ok := csiTildeKeys[base]; ok {
		return "\x1b[" + strconv.Itoa(n) + ";" + p + "~", true
	}
	return "", false
}

// modifiedCharKey renders a modified plain character: C-<char> control
// bytes, M-<char> ESC prefixing, and C-M-<char> both. Shift is refused
// here (see the file comment: the shifted character is its own key).
func modifiedCharKey(mods keyModifiers, base string) (string, bool) {
	if mods.shift {
		return "", false
	}
	if base == "Space" {
		// The word spelling stays usable under modifiers (C-Space, M-Space);
		// a literal space argument works too.
		base = " "
	}
	if utf8.RuneCountInString(base) != 1 {
		return "", false
	}
	var body string
	if mods.ctrl {
		if len(base) != 1 { // control bytes exist for ASCII only
			return "", false
		}
		c := base[0]
		switch {
		case c >= 'a' && c <= 'z':
			body = string([]byte{c - 'a' + 1})
		case c >= 'A' && c <= 'Z':
			body = string([]byte{c - 'A' + 1})
		case c == ' ' || c == '@':
			body = string([]byte{0}) // C-Space / C-@ → NUL
		case c >= '[' && c <= '_':
			// C-[ → ESC, C-\ → FS (SIGQUIT), C-] → GS, C-^ → RS, C-_ → US.
			// Real payloads that used to be typed as literal text.
			body = string([]byte{c - '@'})
		case c == '?':
			body = "\x7f" // C-? → DEL, as every terminal sends it
		default:
			return "", false
		}
	} else {
		// Alt only: ESC + the character verbatim (any single rune, so
		// M-é works as well as M-x).
		body = base
	}
	if mods.alt {
		return "\x1b" + body, true
	}
	return body, true
}

// renderKeys turns a list of key/text tokens into the bytes to send.
// When literal is true (send-keys -l), every token is sent verbatim.
// Otherwise each recognized key name renders to its sequence and any
// unrecognized token is sent as literal text (matching tmux send-keys).
func renderKeys(tokens []string, literal bool) string {
	var b strings.Builder
	for _, t := range tokens {
		if literal {
			b.WriteString(t)
			continue
		}
		if seq, ok := keyBytes(t); ok {
			b.WriteString(seq)
		} else {
			b.WriteString(t)
		}
	}
	return b.String()
}
