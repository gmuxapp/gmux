package projects

import "os"

// CanonicalDir returns the project's canonical launch directory: the
// first match-rule path, normalized. Empty when the item is nil, a
// reference, or carries no path rule.
//
// This is the single server-side source of truth for "the project's
// canonical folder". It matches the definition used everywhere else:
//   - the frontend project-row / hub "+" button (launchCwd = first
//     match-rule path), and
//   - the peering launch_cwd hint advertised to viewers.
//
// Keep the three in agreement: a session resumed in CanonicalDir lands
// where the "+" button would have launched a fresh one.
func (i *Item) CanonicalDir() string {
	if i == nil || i.IsReference() {
		return ""
	}
	for _, r := range i.Match {
		if r.Path == "" {
			continue
		}
		if n := NormalizePath(r.Path); n != "" {
			return n
		}
	}
	return ""
}

// ProjectBySlug returns the owned project with the given slug, or nil.
// References are skipped: only host-owned projects carry the match
// rules CanonicalDir reads.
func (s *State) ProjectBySlug(slug string) *Item {
	if slug == "" {
		return nil
	}
	for i := range s.Items {
		if s.Items[i].Slug == slug && !s.Items[i].IsReference() {
			return &s.Items[i]
		}
	}
	return nil
}

// CanonicalDirForSession returns the canonical launch directory for a
// session's owning project. It prefers the session's assigned project
// slug and falls back to matching the session's stored cwd / workspace
// / remotes against the project rules (the stored cwd string still
// matches even when the directory no longer exists on disk). Empty when
// no owning project is found or that project has no path rule.
func (s *State) CanonicalDirForSession(projectSlug string, p MatchParams) string {
	item := s.ProjectBySlug(projectSlug)
	if item == nil {
		item = s.Match(p)
	}
	return item.CanonicalDir()
}

// IsDir reports whether path is an existing directory.
func IsDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// ResolveLaunchDir returns the first candidate that isDir reports as an
// existing directory, skipping empty candidates, along with its index
// (0 = primary, >0 = a fallback was used). Returns ("", -1) when none
// qualify. isDir is injected so callers and tests can supply a fake
// filesystem; production callers pass IsDir.
func ResolveLaunchDir(isDir func(string) bool, candidates ...string) (string, int) {
	for idx, c := range candidates {
		if c == "" {
			continue
		}
		if isDir(c) {
			return c, idx
		}
	}
	return "", -1
}
