package adapters

import (
	"time"

	"github.com/gmuxapp/gmux/packages/adapter"
)

// --- Shared attribution helpers ---
//
// Authoritative attribution comes from the agent reporting its own session
// file (pi via its extension; see packages/agent-ext). These helpers are the
// FALLBACK for agents with no such signal: codex (Rust, no extension) matches
// a file to a session by metadata. They are a post-hoc guess — ambiguous for
// sessions started close together in the same directory — so do not extend
// them in preference to a native signal.

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
