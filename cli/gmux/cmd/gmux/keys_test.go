package main

import "testing"

func TestKeyBytes(t *testing.T) {
	tests := []struct {
		name string
		want string
		ok   bool
	}{
		// Control combos: the whole point is C-c → 0x03, not the text "C-c".
		{"C-c", "\x03", true},
		{"C-d", "\x04", true},
		{"C-a", "\x01", true},
		{"C-z", "\x1a", true},
		{"C-C", "\x03", true}, // upper-case letter, same control byte
		{"C-@", "\x00", true}, // NUL
		// Named keys.
		{"Enter", "\r", true},
		{"Tab", "\t", true},
		{"Escape", "\x1b", true},
		{"Esc", "\x1b", true},
		{"Up", "\x1b[A", true},
		{"Backspace", "\x7f", true},
		// Not keys.
		{"hello", "", false},
		{"c", "", false},    // bare letter is not a control combo
		{"C-", "", false},   // malformed
		{"Entr", "", false}, // typo is not a key
		{"", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := keyBytes(tc.name)
			if ok != tc.ok || got != tc.want {
				t.Errorf("keyBytes(%q) = (%q, %v), want (%q, %v)", tc.name, got, ok, tc.want, tc.ok)
			}
			if isKeyName(tc.name) != tc.ok {
				t.Errorf("isKeyName(%q) = %v, want %v", tc.name, isKeyName(tc.name), tc.ok)
			}
		})
	}
}

func TestRenderKeys(t *testing.T) {
	// Non-literal: recognized names render to sequences, unknown tokens
	// pass through as literal text (matching tmux send-keys).
	if got := renderKeys([]string{"echo hi", "Enter"}, false); got != "echo hi\r" {
		t.Errorf("renderKeys = %q, want %q", got, "echo hi\r")
	}
	if got := renderKeys([]string{"Escape", ":wq", "Enter"}, false); got != "\x1b:wq\r" {
		t.Errorf("renderKeys = %q, want %q", got, "\x1b:wq\r")
	}
	// Literal: every token verbatim, even ones that look like key names.
	if got := renderKeys([]string{"Enter", "C-c"}, true); got != "EnterC-c" {
		t.Errorf("renderKeys literal = %q, want %q", got, "EnterC-c")
	}
}
