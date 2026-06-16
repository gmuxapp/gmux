package main

import "strings"

// Key-name handling for `gmux send` and `gmux send-keys`. Names follow
// tmux's send-keys vocabulary so existing tmux knowledge transfers
// (ADR 0009). A "key" renders to the byte sequence an xterm-class
// terminal emits for that key; gmux writes those bytes to the session
// PTY's input exactly as if typed.

// namedKeys maps a key name to the bytes it produces. Control keys
// (C-<letter>) are handled separately in keyBytes.
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
	"Delete":    "\x1b[3~",
	"DC":        "\x1b[3~",
}

// isKeyName reports whether s is a recognized key-name token. Used by
// `gmux send` to tell a trailing key (Enter, C-c) from literal text.
func isKeyName(s string) bool {
	_, ok := keyBytes(s)
	return ok
}

// keyBytes renders a single key name to its byte sequence. Recognizes
// the named keys above and control combos of the form C-<char>
// (C-c → 0x03, C-d → 0x04, ...).
func keyBytes(name string) (string, bool) {
	if b, ok := namedKeys[name]; ok {
		return b, true
	}
	if len(name) == 3 && (name[0] == 'C' || name[0] == 'c') && name[1] == '-' {
		ch := name[2]
		switch {
		case ch >= 'a' && ch <= 'z':
			return string([]byte{ch - 'a' + 1}), true
		case ch >= 'A' && ch <= 'Z':
			return string([]byte{ch - 'A' + 1}), true
		case ch == ' ' || ch == '@':
			return string([]byte{0}), true // C-Space / C-@ → NUL
		}
	}
	return "", false
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
