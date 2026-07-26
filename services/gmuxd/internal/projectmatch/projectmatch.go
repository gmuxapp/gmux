// Package projectmatch implements the pure project-attribution policy shared
// by durable and legacy project representations. It performs no persistence.
package projectmatch

import (
	"strings"

	"github.com/gmuxapp/gmux/packages/paths"
)

// Rule matches on a filesystem path and/or a normalized git remote. Both
// arms may be set; each set arm is evaluated independently (a rule with both
// arms can contribute a path match and a remote fallback in the same pass).
// User-authored rules are not validated to exactly one arm.
type Rule struct {
	Path   string
	Remote string
	Exact  bool
}

// Entry is one catalog entry. Reference entries are viewer-side pointers to
// peer-owned projects and are never eligible to claim local sessions.
type Entry struct {
	Reference bool
	Rules     []Rule
}

// Inputs are the common session facts used for project attribution.
type Inputs struct {
	CWD           string
	WorkspaceRoot string
	Remotes       map[string]string
}

// Match returns the index of the winning entry. Path matches take precedence
// over remote matches; the longest normalized path wins globally. Equal path
// lengths and remote matches retain entry/rule iteration order.
func Match(entries []Entry, in Inputs) (int, bool) {
	normCWD := NormalizePath(in.CWD)
	normWorkspace := NormalizePath(in.WorkspaceRoot)

	bestPathIndex := -1
	bestPathLen := 0
	firstRemoteIndex := -1

	for i, entry := range entries {
		if entry.Reference {
			continue
		}
		for _, rule := range entry.Rules {
			if rule.Remote != "" {
				normRule := NormalizeRemote(rule.Remote)
				for _, remote := range in.Remotes {
					if NormalizeRemote(remote) == normRule {
						if firstRemoteIndex == -1 {
							firstRemoteIndex = i
						}
						break
					}
				}
			}

			if rule.Path != "" {
				normRule := NormalizePath(rule.Path)
				if normRule == "" {
					continue
				}
				matched := false
				if rule.Exact {
					matched = normCWD == normRule || normWorkspace == normRule
				} else {
					matched = pathUnder(normCWD, normRule) || pathUnder(normWorkspace, normRule)
				}
				if matched && len(normRule) > bestPathLen {
					bestPathLen = len(normRule)
					bestPathIndex = i
				}
			}
		}
	}

	if bestPathIndex != -1 {
		return bestPathIndex, true
	}
	return firstRemoteIndex, firstRemoteIndex != -1
}

// NormalizePath preserves the path normalization used by project matching.
func NormalizePath(path string) string { return paths.NormalizePath(path) }

// NormalizeRemote canonicalizes the URL forms accepted by project matching.
func NormalizeRemote(remote string) string {
	for _, prefix := range []string{"https://", "http://", "ssh://", "git://"} {
		remote = strings.TrimPrefix(remote, prefix)
	}
	if at := strings.Index(remote, "@"); at >= 0 {
		remote = remote[at+1:]
	}
	if colon := strings.Index(remote, ":"); colon > 0 && !strings.Contains(remote[:colon], "/") {
		remote = remote[:colon] + "/" + remote[colon+1:]
	}
	remote = strings.TrimSuffix(remote, ".git")
	return strings.TrimRight(remote, "/")
}

func pathUnder(candidate, base string) bool {
	if candidate == "" || base == "" {
		return false
	}
	return candidate == base || strings.HasPrefix(candidate, base+"/")
}
