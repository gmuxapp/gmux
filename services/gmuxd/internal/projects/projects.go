// Package projects manages the user-curated project list that controls
// which sessions appear in the sidebar.
//
// A project matches sessions by either a remote URL or filesystem paths
// (never both). Match precedence: path-based (longest prefix) before
// remote-based. State is persisted to projects.json in the state directory.
package projects

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const fileName = "projects.json"

// Item is a user-configured project entry.
// Item is a user-configured project entry.
// Every project has Paths (where the code lives on disk).
// Remote is optional: when set, matching uses the remote URL
// instead of paths, enabling cross-machine and cross-clone grouping.
type Item struct {
	Slug   string   `json:"slug"`
	Remote string   `json:"remote,omitempty"`
	Paths  []string `json:"paths"`
}

// State holds the ordered list of configured projects.
type State struct {
	Items []Item `json:"items"`
}

// Load reads the project state from stateDir/projects.json.
// Returns an empty state if the file doesn't exist.
func Load(stateDir string) (*State, error) {
	path := filepath.Join(stateDir, fileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &State{}, nil
		}
		return nil, fmt.Errorf("projects: reading %s: %w", path, err)
	}

	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("projects: parsing %s: %w", path, err)
	}
	return &s, nil
}

// Save writes the project state atomically to stateDir/projects.json.
func (s *State) Save(stateDir string) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("projects: creating state dir: %w", err)
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("projects: marshaling: %w", err)
	}
	data = append(data, '\n')

	path := filepath.Join(stateDir, fileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("projects: writing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("projects: renaming %s -> %s: %w", tmp, path, err)
	}
	return nil
}

// Validate checks the state for consistency:
//   - Each item has a valid, non-empty slug.
//   - Each item has at least one path.
//   - No duplicate slugs.
//   - No exact duplicate normalized paths across items.
//   - Nesting (one path under another) is allowed.
func (s *State) Validate() error {
	slugs := make(map[string]bool)
	allPaths := make(map[string]string) // normalized path -> owning slug

	for i, item := range s.Items {
		if item.Slug == "" {
			return fmt.Errorf("item %d: slug is empty", i)
		}
		if !IsValidSlug(item.Slug) {
			return fmt.Errorf("item %d: slug %q is not URL-safe", i, item.Slug)
		}
		if slugs[item.Slug] {
			return fmt.Errorf("duplicate slug %q", item.Slug)
		}
		slugs[item.Slug] = true

		if len(item.Paths) == 0 {
			return fmt.Errorf("item %q: paths is empty", item.Slug)
		}

		for _, p := range item.Paths {
			norm := NormalizePath(p)
			if norm == "" {
				return fmt.Errorf("item %q: contains empty path", item.Slug)
			}
			if owner, ok := allPaths[norm]; ok {
				return fmt.Errorf("path %q appears in both %q and %q", norm, owner, item.Slug)
			}
			allPaths[norm] = item.Slug
		}
	}
	return nil
}

// Match returns the project that best matches the given session.
//
// Precedence (first match wins):
//  1. Path-matched projects (no remote), longest prefix.
//  2. Remote-matched projects, by remote URL.
//  3. Remote-matched projects, falling back to path prefix.
//     This catches sessions that are physically inside a project
//     directory but don't have remotes set (e.g. new repos).
//
// Returns nil if no project matches.
func (s *State) Match(cwd, workspaceRoot string, remotes map[string]string) *Item {
	normCwd := NormalizePath(cwd)
	normWS := NormalizePath(workspaceRoot)

	// Phase 1: path-matched projects (no remote), longest prefix wins.
	// These are projects that opted into path-based matching.
	var best *Item
	bestLen := 0

	for i := range s.Items {
		item := &s.Items[i]
		if item.Remote != "" {
			continue
		}
		for _, p := range item.Paths {
			norm := NormalizePath(p)
			if norm == "" {
				continue
			}
			if pathUnder(normCwd, norm) || pathUnder(normWS, norm) {
				if len(norm) > bestLen {
					bestLen = len(norm)
					best = item
				}
			}
		}
	}
	if best != nil {
		return best
	}

	// Phase 2: remote-matched projects, by remote URL.
	for i := range s.Items {
		item := &s.Items[i]
		if item.Remote == "" {
			continue
		}
		normRemote := NormalizeRemote(item.Remote)
		for _, url := range remotes {
			if NormalizeRemote(url) == normRemote {
				return item
			}
		}
	}

	// Phase 3: remote-matched projects, falling back to their paths.
	// A session might be inside a project directory but lack remotes
	// (e.g. newly cloned repo before first push, or a non-VCS subdirectory).
	best = nil
	bestLen = 0
	for i := range s.Items {
		item := &s.Items[i]
		if item.Remote == "" {
			continue // already checked in phase 1
		}
		for _, p := range item.Paths {
			norm := NormalizePath(p)
			if norm == "" {
				continue
			}
			if pathUnder(normCwd, norm) || pathUnder(normWS, norm) {
				if len(norm) > bestLen {
					bestLen = len(norm)
					best = item
				}
			}
		}
	}
	return best
}

// pathUnder returns true if candidate is equal to or a subdirectory of base.
// Both must be cleaned paths (no trailing slashes except root).
func pathUnder(candidate, base string) bool {
	if candidate == "" || base == "" {
		return false
	}
	if candidate == base {
		return true
	}
	return strings.HasPrefix(candidate, base+"/")
}

// --- Path normalization ---

// NormalizePath cleans a filesystem path for comparison.
// Expands ~ prefix and calls filepath.Clean.
func NormalizePath(p string) string {
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[2:])
		}
	}
	return filepath.Clean(p)
}

// --- Remote normalization ---

// NormalizeRemote converts a git remote URL to a canonical form.
// Strips protocol, user prefix, and .git suffix so that different
// URL styles for the same repo compare equal.
//
// Examples:
//
//	https://github.com/org/repo.git  -> github.com/org/repo
//	git@github.com:org/repo.git      -> github.com/org/repo
//	ssh://git@github.com/org/repo    -> github.com/org/repo
func NormalizeRemote(url string) string {
	// Strip protocol prefix.
	for _, prefix := range []string{"https://", "http://", "ssh://", "git://"} {
		url = strings.TrimPrefix(url, prefix)
	}
	// Strip user@ prefix (e.g. "git@github.com:..." -> "github.com:...").
	if at := strings.Index(url, "@"); at >= 0 {
		url = url[at+1:]
	}
	// Convert SCP-style colon to slash (github.com:org/repo -> github.com/org/repo).
	// Only if there's no slash before the colon (avoids mangling port numbers in URLs
	// that already had their protocol stripped, like "host:8080/repo").
	if colon := strings.Index(url, ":"); colon > 0 && !strings.Contains(url[:colon], "/") {
		url = url[:colon] + "/" + url[colon+1:]
	}
	url = strings.TrimSuffix(url, ".git")
	url = strings.TrimRight(url, "/")
	return url
}

// --- Slug helpers ---

var slugRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// IsValidSlug returns true if s is a non-empty, URL-safe slug.
// Allowed: lowercase alphanumeric and hyphens, must start and end with alnum.
func IsValidSlug(s string) bool {
	return slugRe.MatchString(s)
}

// Slugify converts a string to a URL-safe slug.
func Slugify(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	s = b.String()
	// Collapse multiple hyphens.
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	s = strings.Trim(s, "-")
	if s == "" {
		return "project"
	}
	return s
}

// SlugFromRemote derives a project slug from a remote URL.
// Uses the repo name (last path segment of the normalized URL).
func SlugFromRemote(remote string) string {
	norm := NormalizeRemote(remote)
	parts := strings.Split(norm, "/")
	return Slugify(parts[len(parts)-1])
}

// SlugFromPath derives a project slug from a filesystem path.
func SlugFromPath(p string) string {
	return Slugify(filepath.Base(NormalizePath(p)))
}

// UniqueSlug returns a slug that doesn't conflict with existing items.
// If slug is already unique, returns it unchanged. Otherwise appends -2, -3, etc.
func UniqueSlug(slug string, items []Item) string {
	taken := make(map[string]bool, len(items))
	for _, item := range items {
		taken[item.Slug] = true
	}
	if !taken[slug] {
		return slug
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", slug, i)
		if !taken[candidate] {
			return candidate
		}
	}
}

// --- Discovery (offered projects) ---

// SessionInfo contains the session fields needed for matching and discovery.
type SessionInfo struct {
	ID            string
	Cwd           string
	WorkspaceRoot string
	Remotes       map[string]string
}

// DiscoveredProject is a group of unmatched sessions offered to the user.
type DiscoveredProject struct {
	SuggestedSlug string   `json:"suggested_slug"`
	Remote        string   `json:"remote,omitempty"`
	Paths         []string `json:"paths"`
	SessionCount  int      `json:"session_count"`
}

// Discovered groups sessions that don't match any configured project.
// Uses the same union-find algorithm as the frontend's groupByFolder:
// merge by shared remotes, then shared workspace_root, then shared cwd
// (for sessions with neither remotes nor workspace_root).
func (s *State) Discovered(sessions []SessionInfo) []DiscoveredProject {
	// Filter to unmatched sessions.
	var unmatched []SessionInfo
	for _, sess := range sessions {
		if s.Match(sess.Cwd, sess.WorkspaceRoot, sess.Remotes) == nil {
			unmatched = append(unmatched, sess)
		}
	}
	if len(unmatched) == 0 {
		return nil
	}

	// Union-find.
	parent := make(map[string]string, len(unmatched))
	for _, sess := range unmatched {
		parent[sess.ID] = sess.ID
	}

	var find func(string) string
	find = func(id string) string {
		root := id
		for parent[root] != root {
			root = parent[root]
		}
		cur := id
		for cur != root {
			next := parent[cur]
			parent[cur] = root
			cur = next
		}
		return root
	}
	union := func(a, b string) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[ra] = rb
		}
	}

	// Merge by shared remote URLs.
	remoteToGroup := make(map[string]string)
	for _, sess := range unmatched {
		for _, url := range sess.Remotes {
			norm := NormalizeRemote(url)
			if existing, ok := remoteToGroup[norm]; ok {
				union(sess.ID, existing)
			} else {
				remoteToGroup[norm] = sess.ID
			}
		}
	}

	// Merge by shared workspace_root.
	wsToGroup := make(map[string]string)
	for _, sess := range unmatched {
		if sess.WorkspaceRoot == "" {
			continue
		}
		if existing, ok := wsToGroup[sess.WorkspaceRoot]; ok {
			union(sess.ID, existing)
		} else {
			wsToGroup[sess.WorkspaceRoot] = sess.ID
		}
	}

	// Merge by shared cwd (only for sessions with no remotes and no workspace_root).
	cwdToGroup := make(map[string]string)
	for _, sess := range unmatched {
		if len(sess.Remotes) > 0 || sess.WorkspaceRoot != "" {
			continue
		}
		if existing, ok := cwdToGroup[sess.Cwd]; ok {
			union(sess.ID, existing)
		} else {
			cwdToGroup[sess.Cwd] = sess.ID
		}
	}

	// Collect groups.
	groups := make(map[string][]SessionInfo)
	for _, sess := range unmatched {
		root := find(sess.ID)
		groups[root] = append(groups[root], sess)
	}

	// Build discovered projects from groups.
	result := make([]DiscoveredProject, 0, len(groups))
	for _, group := range groups {
		result = append(result, buildDiscovered(group))
	}

	// Sort: most sessions first, then alphabetically by slug for stability.
	sort.Slice(result, func(i, j int) bool {
		if result[i].SessionCount != result[j].SessionCount {
			return result[i].SessionCount > result[j].SessionCount
		}
		return result[i].SuggestedSlug < result[j].SuggestedSlug
	})

	return result
}

func buildDiscovered(sessions []SessionInfo) DiscoveredProject {
	dp := DiscoveredProject{
		SessionCount: len(sessions),
	}

	// Find the most common remote URL.
	urlCounts := make(map[string]int)
	for _, s := range sessions {
		for _, url := range s.Remotes {
			urlCounts[NormalizeRemote(url)]++
		}
	}
	if len(urlCounts) > 0 {
		var bestURL string
		var bestCount int
		for url, count := range urlCounts {
			if count > bestCount || (count == bestCount && url < bestURL) {
				bestURL = url
				bestCount = count
			}
		}
		dp.Remote = bestURL
		dp.SuggestedSlug = SlugFromRemote(bestURL)
	}

	// Collect unique paths (prefer workspace_root, fall back to cwd).
	pathSet := make(map[string]bool)
	for _, s := range sessions {
		if s.WorkspaceRoot != "" {
			pathSet[s.WorkspaceRoot] = true
		} else if s.Cwd != "" {
			pathSet[s.Cwd] = true
		}
	}
	dp.Paths = make([]string, 0, len(pathSet))
	for p := range pathSet {
		dp.Paths = append(dp.Paths, p)
	}
	sort.Strings(dp.Paths)

	// Slug fallback to path basename if no remote.
	if dp.SuggestedSlug == "" && len(dp.Paths) > 0 {
		dp.SuggestedSlug = SlugFromPath(dp.Paths[0])
	}
	if dp.SuggestedSlug == "" {
		dp.SuggestedSlug = "project"
	}

	return dp
}
