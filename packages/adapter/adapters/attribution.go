package adapters

import (
	"regexp"
	"strings"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter"
)

// --- Shared attribution helpers ---

// attributeByScrollback picks the candidate whose scrollback text best
// matches the file text. Returns "" if no candidate scores above the
// threshold. Used by adapters with per-cwd directories (pi, claude).
func attributeByScrollback(fileText string, candidates []adapter.FileCandidate) string {
	if fileText == "" {
		return ""
	}
	fileTail := tail(fileText, 500)

	bestID := ""
	bestScore := 0.0

	for _, c := range candidates {
		if c.Scrollback == "" {
			continue
		}
		score := similarityScore(fileTail, tail(c.Scrollback, 2000))
		if score > bestScore {
			bestScore = score
			bestID = c.SessionID
		}
	}

	if bestScore < 0.3 {
		return ""
	}
	return bestID
}

// attributeByScrollbackNormalized is like attributeByScrollback but
// strips TUI chrome and normalizes formatting before comparison.
// Both the file text and scrollback represent the same conversation,
// but the scrollback is a terminal rendering with box-drawing borders,
// status bars, and collapsed whitespace, while the file text is raw
// JSONL content with markdown formatting and newlines. Cleaning both
// sides makes the underlying conversation text match.
//
// Uses a 200-char file tail (not 500) because the scrollback is a
// narrow window and a shorter tail is more likely to overlap with
// the visible screen.
func attributeByScrollbackNormalized(fileText string, candidates []adapter.FileCandidate) string {
	if fileText == "" {
		return ""
	}
	fileTail := cleanForMatching(tail(fileText, 200))
	if len(fileTail) < 20 {
		return "" // too short for reliable matching
	}

	bestID := ""
	bestScore := 0.0

	for _, c := range candidates {
		if c.Scrollback == "" {
			continue
		}
		sbTail := cleanForMatching(tail(c.Scrollback, 2000))
		score := similarityScore(fileTail, sbTail)
		if score > bestScore {
			bestScore = score
			bestID = c.SessionID
		}
	}

	if bestScore < 0.3 {
		return ""
	}
	return bestID
}

// boxDrawing matches box-drawing characters used by TUI borders.
var boxDrawing = regexp.MustCompile("[─━│┌┐└┘├┤┬┴┼╭╮╰╯║═╔╗╚╝╠╣╦╩╬]+")

// cleanForMatching strips TUI chrome (box-drawing borders), markdown
// formatting (backticks, bold markers), and collapses whitespace.
// This brings terminal-rendered text and JSONL source text into the
// same form for LCS comparison.
func cleanForMatching(s string) string {
	s = boxDrawing.ReplaceAllString(s, " ")
	s = strings.ReplaceAll(s, "`", "")
	s = strings.ReplaceAll(s, "*", "")
	return strings.Join(strings.Fields(s), " ")
}

// attributeByMetadata picks the candidate whose cwd and start time best
// match the file's session metadata. Returns "" if no match within the
// time threshold. Used by adapters with shared directories (codex).
func attributeByMetadata(info *adapter.SessionFileInfo, candidates []adapter.FileCandidate) string {
	if info == nil || info.Cwd == "" {
		return ""
	}

	bestID := ""
	var bestDelta time.Duration = 1<<63 - 1

	for _, c := range candidates {
		if c.Cwd != info.Cwd {
			continue
		}
		delta := info.Created.Sub(c.StartedAt).Abs()
		if delta < bestDelta {
			bestDelta = delta
			bestID = c.SessionID
		}
	}

	if bestID == "" || bestDelta > 5*time.Minute {
		return ""
	}
	return bestID
}

// --- String utilities ---

func similarityScore(fileTail, scrollbackTail string) float64 {
	if len(fileTail) == 0 || len(scrollbackTail) == 0 {
		return 0
	}
	lcs := longestCommonSubstring(fileTail, scrollbackTail)
	return float64(lcs) / float64(len(fileTail))
}

func longestCommonSubstring(a, b string) int {
	if len(a) > len(b) {
		a, b = b, a
	}
	prev := make([]int, len(a)+1)
	curr := make([]int, len(a)+1)
	best := 0

	for j := 1; j <= len(b); j++ {
		for i := 1; i <= len(a); i++ {
			if a[i-1] == b[j-1] {
				curr[i] = prev[i-1] + 1
				if curr[i] > best {
					best = curr[i]
				}
			} else {
				curr[i] = 0
			}
		}
		prev, curr = curr, prev
		for i := range curr {
			curr[i] = 0
		}
	}
	return best
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
