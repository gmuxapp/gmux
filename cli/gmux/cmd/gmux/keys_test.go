package main

import (
	"errors"
	"strings"
	"testing"
)

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
		// Unmodified special keys: bytes are frozen.
		{"Down", "\x1b[B", true},
		{"Right", "\x1b[C", true},
		{"Left", "\x1b[D", true},
		{"Home", "\x1b[H", true},
		{"End", "\x1b[F", true},
		{"PageUp", "\x1b[5~", true},
		{"PageDown", "\x1b[6~", true},
		{"Delete", "\x1b[3~", true},
		{"DC", "\x1b[3~", true},
		{"Insert", "\x1b[2~", true},
		{"IC", "\x1b[2~", true},
		// Function keys: SS3 for F1-F4, tilde sequences from F5 up.
		{"F1", "\x1bOP", true},
		{"F2", "\x1bOQ", true},
		{"F3", "\x1bOR", true},
		{"F4", "\x1bOS", true},
		{"F5", "\x1b[15~", true},
		{"F6", "\x1b[17~", true},
		{"F7", "\x1b[18~", true},
		{"F8", "\x1b[19~", true},
		{"F9", "\x1b[20~", true},
		{"F10", "\x1b[21~", true},
		{"F11", "\x1b[23~", true},
		{"F12", "\x1b[24~", true},
		// Alt/meta chords: ESC prefix, never 8-bit meta.
		{"M-a", "\x1ba", true},
		{"M-x", "\x1bx", true},
		{"M-A", "\x1bA", true},
		{"M-.", "\x1b.", true},
		{"m-a", "\x1ba", true},
		{"M-é", "\x1bé", true},
		{"M-Space", "\x1b ", true},
		// Control+alt: ESC + the control byte, in either prefix order.
		{"C-M-a", "\x1b\x01", true},
		{"M-C-a", "\x1b\x01", true},
		{"C-M-c", "\x1b\x03", true},
		{"C-Space", "\x00", true},
		{"C-M-Space", "\x1b\x00", true},
		// Modified cursor/Home/End keys: CSI 1;<mod><final>,
		// mod = 1 + shift + 2*alt + 4*ctrl.
		{"S-Up", "\x1b[1;2A", true},
		{"M-Up", "\x1b[1;3A", true},
		{"M-S-Up", "\x1b[1;4A", true},
		{"C-Up", "\x1b[1;5A", true},
		{"C-S-Up", "\x1b[1;6A", true},
		{"C-M-Up", "\x1b[1;7A", true},
		{"C-M-S-Up", "\x1b[1;8A", true},
		{"S-C-M-Up", "\x1b[1;8A", true}, // prefix order is free
		{"C-Left", "\x1b[1;5D", true},
		{"S-Right", "\x1b[1;2C", true},
		{"C-Down", "\x1b[1;5B", true},
		{"C-Home", "\x1b[1;5H", true},
		{"S-End", "\x1b[1;2F", true},
		// Modified tilde keys: CSI <n>;<mod>~.
		{"M-PageUp", "\x1b[5;3~", true},
		{"C-PageDown", "\x1b[6;5~", true},
		{"S-Delete", "\x1b[3;2~", true},
		{"C-DC", "\x1b[3;5~", true},
		{"C-Insert", "\x1b[2;5~", true},
		{"S-IC", "\x1b[2;2~", true},
		// Modified function keys: F1-F4 switch from SS3 to CSI 1;<mod>.
		{"S-F1", "\x1b[1;2P", true},
		{"C-F2", "\x1b[1;5Q", true},
		{"M-F3", "\x1b[1;3R", true},
		{"C-M-F4", "\x1b[1;7S", true},
		{"M-F5", "\x1b[15;3~", true},
		{"C-F12", "\x1b[24;5~", true},
		{"C-S-F10", "\x1b[21;6~", true},
		// Not keys.
		{"hello", "", false},
		// Shift on a plain character is not a key: the shifted character
		// itself is (send "A", not "S-a").
		{"S-a", "", false},
		{"S-A", "", false},
		{"C-S-a", "", false},
		{"M-S-x", "", false},
		{"M-", "", false},
		{"C-M-", "", false},
		{"M-hello", "", false}, // multi-char is not a character key
		{"C-1", "", false},     // no control byte for a digit
		{"X-Up", "", false},    // unknown modifier letter
		// Modified typing keys other than shift-Tab: no single encoding exists
		// (emulator- and keyboard-protocol-dependent), so gmux refuses them
		// instead of inventing bytes. Pinning the refusals is the point — these
		// are exactly the names the help page promises NOT to support, and a
		// refused name is a `send` error rather than silent text.
		{"M-Enter", "", false},
		{"C-Enter", "", false},
		{"C-Tab", "", false},
		{"C-S-Tab", "", false},
		{"M-Tab", "", false},
		{"M-Escape", "", false},
		{"C-Escape", "", false},
		{"S-Space", "", false},
		{"S-Enter", "", false},
		{"S-BSpace", "", false},
		{"F13", "", false}, // vocabulary stops at F12
		{"F0", "", false},
		{"KP0", "", false},       // keypad: no agreed encoding
		{"S-BTab", "", false},    // BTab already IS the modified key
		{"M-M-a", "\x1ba", true}, // a repeated modifier is still that modifier

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

// TestSendKeyHeuristicWithModifierChords pins the consequence of the
// wider vocabulary: isKeyName drives send's text-vs-key split, so a
// modifier chord in key position is a key, not literal text.
func TestSendKeyHeuristicWithModifierChords(t *testing.T) {
	for _, tok := range []string{"M-x", "C-M-a", "C-S-Up", "M-PageUp", "F5", "S-F1", "Insert"} {
		c, err := parseCLI([]string{"send", "abc", tok})
		if err != nil {
			t.Fatalf("parseCLI(send abc %s): %v", tok, err)
		}
		if c.sendText != nil {
			t.Errorf("%s: text = %q, want nil (token is a key)", tok, *c.sendText)
		}
		if len(c.sendKeys) != 1 || c.sendKeys[0] != tok {
			t.Errorf("%s: keys = %v", tok, c.sendKeys)
		}
	}
	// A near-miss stays literal text, which is what keeps prose sendable.
	for _, tok := range []string{"S-a", "M-hello", "F13"} {
		c, err := parseCLI([]string{"send", "abc", tok})
		if err != nil {
			t.Fatalf("parseCLI(send abc %s): %v", tok, err)
		}
		if c.sendText == nil || *c.sendText != tok || len(c.sendKeys) != 0 {
			t.Errorf("%s: text=%v keys=%v, want literal text", tok, c.sendText, c.sendKeys)
		}
	}
}

// TestSendRefusesUnknownKeyPositionTokens pins the one behavior change of
// the key work: in key position (everything after the first token), an
// unrecognized name is a parse error. Typing it as literal text under exit 0
// — the previous behavior, and tmux send-keys' — turns a typo or an
// unsupported key into a silent text injection reported as a successful
// delivery.
func TestSendRefusesUnknownKeyPositionTokens(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		bad  string
	}{
		{"typo after text", []string{"send", "abc", "make test", "Etner"}, "Etner"},
		{"typo after a key", []string{"send", "abc", "C-c", "Entr"}, "Entr"},
		{"unsupported key", []string{"send", "abc", "hi", "C-Tab"}, "C-Tab"},
		{"shift on a char", []string{"send", "abc", "hi", "S-a"}, "S-a"},
		{"second text argument", []string{"send", "abc", "one", "two"}, "two"},
		{"prose split by the shell", []string{"send", "abc", "make", "test", "Enter"}, "test"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseCLI(tt.args)
			if err == nil {
				t.Fatalf("parseCLI(%v) = nil error; the token would have been typed as text", tt.args)
			}
			if !strings.Contains(err.Error(), tt.bad) {
				t.Errorf("error %q must name the offending token %q", err, tt.bad)
			}
			if !strings.Contains(err.Error(), "gmux send --help") {
				t.Errorf("error %q must point at the key vocabulary", err)
			}
			var ue *usageError
			if !errors.As(err, &ue) || ue.topic != "send" {
				t.Errorf("error is not tagged with send's help topic: %v", err)
			}
		})
	}

	// Valid forms are untouched: the first token stays literal text however
	// unkeylike, and real keys still parse.
	for _, args := range [][]string{
		{"send", "abc", "make test", "Enter"},
		{"send", "abc", "Etner"},                         // sole token: text, not a key
		{"send", "abc", "-v"},                            // dash-leading text after the ref
		{"send", "abc", "C-c"},                           // keys only
		{"send", "abc", "hi", "C-M-\\", "BTab", "Enter"}, // newly added names
		{"send", "abc"},                                  // stdin form
	} {
		if _, err := parseCLI(args); err != nil {
			t.Errorf("parseCLI(%v) = %v, want success", args, err)
		}
	}

	// send-keys keeps tmux's literal fallback — that is its contract, and -l
	// exists for it. The asymmetry is deliberate and documented on both pages.
	c, err := parseCLI([]string{"send-keys", "-t", "abc", "Etner"})
	if err != nil || len(c.keys) != 1 || c.keys[0] != "Etner" {
		t.Fatalf("send-keys must accept unknown tokens: (%+v, %v)", c, err)
	}
	if got := renderKeys([]string{"Etner"}, false); got != "Etner" {
		t.Errorf("renderKeys still types unknown tokens literally for send-keys, got %q", got)
	}
}

// TestSendTimeoutShortFlagDiagnostic: -t on send is --timeout, while every
// tmux reflex says "target session". A non-numeric value is therefore far
// more likely a misremembered target than a typo'd duration, and the error
// has to say so — including where the real target flag lives.
func TestSendTimeoutShortFlagDiagnostic(t *testing.T) {
	_, err := parseCLI([]string{"send", "-w", "-t", "abc", "hi", "Enter"})
	if err == nil {
		t.Fatal("send -t abc: nil error")
	}
	for _, want := range []string{"-t is --timeout", "positional", "send-keys", `"abc"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must mention %q", err, want)
		}
	}
	// The long spelling gets the plain message: nobody confuses --timeout
	// with a target.
	_, err = parseCLI([]string{"send", "-w", "--timeout", "abc", "hi", "Enter"})
	if err == nil || strings.Contains(err.Error(), "send-keys") {
		t.Errorf("--timeout error = %v, want the plain positive-seconds message", err)
	}
}

// documentedKeyForms is the vocabulary the shared help block claims, written
// out as the page writes it. Every entry must appear on the page AND be
// rendered by keyBytes, so the two can only drift by failing this test.
//
// The productions are expanded, not sampled: `C-<letter>` means all 26, and
// the control set is closed, because `send` now refuses unrecognized tokens
// and points the caller at this page as the authority. A page that promises
// one more form than the parser accepts turns that refusal into a lie.
var documentedKeyForms = struct {
	names       []string // spelled out verbatim on the page
	controlMisc []string // the closed non-letter control set
	unsupported []string // named on the page as refused
}{
	names: []string{
		"Enter", "Tab", "BTab", "S-Tab", "Space", "Escape", "Esc", "BSpace", "Backspace",
		"Up", "Down", "Left", "Right", "Home", "End",
		"PageUp", "PPage", "PageDown", "NPage", "Insert", "IC", "Delete", "DC",
		"F1", "F12",
		"C-Left", "S-Up", "M-PageUp", "C-S-Home", "C-M-End", "M-F5", "C-F12",
		"M-x", "M-b", "M-.",
	},
	controlMisc: []string{"C-Space", "C-@", "C-[", "C-\\", "C-]", "C-^", "C-_", "C-?"},
	unsupported: []string{"C-Tab", "M-Enter", "C-Enter", "M-Escape", "S-Space", "F13"},
}

// TestKeyVocabularyIsHonest checks the page and the parser against each other
// in both directions: everything claimed is accepted, everything named as
// unsupported is refused, and the closed sets really are closed.
func TestKeyVocabularyIsHonest(t *testing.T) {
	// Every documented name must be on the page and must render. No skipping:
	// a missing spelling is a page bug, not a reason to pass.
	for _, name := range documentedKeyForms.names {
		if !strings.Contains(keyVocabulary, name) {
			t.Errorf("the key vocabulary no longer documents %q", name)
		}
		if !isKeyName(name) {
			t.Errorf("the key vocabulary documents %q but keyBytes refuses it", name)
		}
	}

	// The `C-<letter>` production, expanded: the page claims the whole range,
	// in both cases.
	if !strings.Contains(keyVocabulary, "C-<letter>") {
		t.Error("the key vocabulary must state the control production as C-<letter>")
	}
	for c := byte('a'); c <= 'z'; c++ {
		for _, form := range []string{"C-" + string(c), "C-" + string(c-32), "C-M-" + string(c)} {
			if !isKeyName(form) {
				t.Errorf("C-<letter> is documented but %q is refused", form)
			}
		}
	}

	// The non-letter control set is closed: each documented form renders, and
	// C-M- composes with exactly the same set.
	for _, form := range documentedKeyForms.controlMisc {
		if !strings.Contains(keyVocabulary, form) {
			t.Errorf("the key vocabulary no longer documents %q", form)
		}
		if !isKeyName(form) {
			t.Errorf("the key vocabulary documents %q but keyBytes refuses it", form)
		}
		if alt := "C-M-" + strings.TrimPrefix(form, "C-"); !isKeyName(alt) {
			t.Errorf("the page says C-M- covers the same set, but %q is refused", alt)
		}
	}

	// The representative overclaim the old wording invited: `C-<char>` read as
	// "any character" promises C-1 and C-, which have no control byte. They
	// must be refused, and the page must say so rather than implying them.
	for _, form := range []string{"C-1", "C-,", "C-M-1"} {
		if isKeyName(form) {
			t.Errorf("%q renders, but no control byte exists for it", form)
		}
	}
	if strings.Contains(keyVocabulary, "C-<char>") {
		t.Error("the key vocabulary must not promise C-<char>: only letters and the named punctuation have control bytes")
	}
	if !strings.Contains(keyVocabulary, "C-1") {
		t.Error("the key vocabulary must name C-1 as not a key, since C-<letter> invites the question")
	}

	// M-<char> IS the general production, so it must accept what it promises.
	for _, form := range []string{"M-a", "M-Z", "M-1", "M-,", "M-/", "M-é"} {
		if !isKeyName(form) {
			t.Errorf("M-<char> is documented as any single character but %q is refused", form)
		}
	}

	// Everything named as unsupported must in fact be refused.
	for _, name := range documentedKeyForms.unsupported {
		if !strings.Contains(keyVocabulary, name) {
			t.Errorf("the key vocabulary must name %q as unsupported", name)
		}
		if isKeyName(name) {
			t.Errorf("%q is documented as unsupported but renders", name)
		}
	}
	if strings.Contains(keyVocabulary, "in any combination") {
		t.Error("the key vocabulary must enumerate the modifiable keys, not imply any combination")
	}

	// Both pages that embed it stay in sync by construction.
	for _, topic := range []string{"send", "send-keys"} {
		if !strings.Contains(verbHelpPages[topic], keyVocabulary) {
			t.Errorf("%s page does not embed the shared key vocabulary", topic)
		}
	}
}

// TestKeyBytesRecentAdditions pins exact bytes for everything the vocabulary
// gained in this commit, including the modified forms of the new aliases.
// TestKeyBytes covers the unmodified spellings; these are the combinations a
// reviewer had to verify by hand.
func TestKeyBytesRecentAdditions(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		// Back-tab is CSI Z under both spellings — the one modified typing key
		// with a universal encoding.
		{"BTab", "\x1b[Z"},
		{"S-Tab", "\x1b[Z"},
		// tmux's page-key aliases, unmodified and modified, byte-identical to
		// PageUp/PageDown throughout (mod = 1 + shift + 2*alt + 4*ctrl).
		{"PPage", "\x1b[5~"},
		{"NPage", "\x1b[6~"},
		{"C-PPage", "\x1b[5;5~"},
		{"C-PageUp", "\x1b[5;5~"},
		{"M-NPage", "\x1b[6;3~"},
		{"M-PageDown", "\x1b[6;3~"},
		{"S-PPage", "\x1b[5;2~"},
		{"C-M-S-NPage", "\x1b[6;8~"},
		// The six remaining control bytes.
		{"C-[", "\x1b"},
		{"C-\\", "\x1c"},
		{"C-]", "\x1d"},
		{"C-^", "\x1e"},
		{"C-_", "\x1f"},
		{"C-?", "\x7f"},
		// ...and ESC-prefixed, which is what C-M- means for them.
		{"C-M-\\", "\x1b\x1c"},
		{"C-M-[", "\x1b\x1b"},
		{"C-M-?", "\x1b\x7f"},
		{"C-M-Space", "\x1b\x00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := keyBytes(tc.name)
			if !ok || got != tc.want {
				t.Errorf("keyBytes(%q) = (%q, %v), want (%q, true)", tc.name, got, ok, tc.want)
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
	// Modifier chords render as sequences too; unknown chord-looking
	// tokens (S-a) fall through as text.
	if got := renderKeys([]string{"M-x", "C-S-Up", "S-a"}, false); got != "\x1bx\x1b[1;6AS-a" {
		t.Errorf("renderKeys = %q, want %q", got, "\x1bx\x1b[1;6AS-a")
	}
	// Literal: every token verbatim, even ones that look like key names.
	if got := renderKeys([]string{"Enter", "C-c"}, true); got != "EnterC-c" {
		t.Errorf("renderKeys literal = %q, want %q", got, "EnterC-c")
	}
}
