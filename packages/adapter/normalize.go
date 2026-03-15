package adapter

import (
	"regexp"
	"strings"
)

// ansiRe matches ANSI escape sequences: CSI (ESC[...X), OSC (ESC]...ST/BEL),
// and short escapes (ESC followed by one char).
var ansiRe = regexp.MustCompile(`\x1b(?:\[[0-9;?]*[A-Za-z]|\][^\x07\x1b]*(?:\x07|\x1b\\)|\[[0-9;]*m|.)`)

// NormalizeScrollback strips ANSI escape sequences and control characters
// from raw terminal output, collapses whitespace, and returns plain text
// suitable for similarity matching.
func NormalizeScrollback(raw []byte) string {
	s := ansiRe.ReplaceAllString(string(raw), "")

	// Strip remaining control characters (except newline).
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\n' || r == '\t' || (r >= 32 && r != 127) {
			b.WriteRune(r)
		}
	}

	// Collapse runs of whitespace to single space.
	return strings.Join(strings.Fields(b.String()), " ")
}
