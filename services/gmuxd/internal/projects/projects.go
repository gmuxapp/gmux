// Package projects manages the user-curated project list that controls
// which sessions appear in the sidebar.
//
// Each project has a list of match rules (remote URLs, filesystem paths,
// optionally scoped to specific hosts). A session matches a project if
// any rule matches. First matching project in list order wins; among
// path rules, longest prefix wins. State is persisted to
// projects.json in the state directory.
package projects

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/gmuxapp/gmux/packages/paths"
)

const fileName = "projects.json"

// currentVersion is the latest projects.json schema version.
// See migrateState for the evolution history.
const currentVersion = 2

// MatchRule is a single criterion for matching sessions to a project.
// Exactly one of Path or Remote should be set.
type MatchRule struct {
	Path   string   `json:"path,omitempty"`
	Remote string   `json:"remote,omitempty"`
	Hosts  []string `json:"hosts,omitempty"` // empty = any host
	Exact  bool     `json:"exact,omitempty"` // path must match exactly, not as prefix
}

// Item is a user-configured project entry.
// Match contains the rules that determine which sessions belong here.
// Sessions is an ordered list of session keys (resume_key or session ID)
// that controls sidebar order.
type Item struct {
	Slug     string      `json:"slug"`
	Match    []MatchRule `json:"match"`
	Sessions []string    `json:"sessions,omitempty"`
}

// State holds the ordered list of configured projects.
type State struct {
	Version int    `json:"version"`
	Items   []Item `json:"items"`
}

// Load reads the project state from stateDir/projects.json.
// Returns an empty state if the file doesn't exist.
// Older schema versions are migrated in memory; the migrated form is
// written back on the next Save.
func Load(stateDir string) (*State, error) {
	path := filepath.Join(stateDir, fileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &State{Version: currentVersion}, nil
		}
		return nil, fmt.Errorf("projects: reading %s: %w", path, err)
	}

	// Run migrations on the raw JSON before unmarshaling into the
	// current struct layout. This keeps each migration self-contained
	// and independent of the Go types.
	data, err = migrateState(data)
	if err != nil {
		return nil, fmt.Errorf("projects: migrating %s: %w", path, err)
	}

	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("projects: parsing %s: %w", path, err)
	}
	return &s, nil
}

// Save writes the project state atomically to stateDir/projects.json.
func (s *State) Save(stateDir string) error {
	s.Version = currentVersion

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
//   - Each item has at least one match rule.
//   - Each match rule has exactly one of Path or Remote.
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

		if len(item.Match) == 0 {
			return fmt.Errorf("item %q: match rules are empty", item.Slug)
		}

		for j, rule := range item.Match {
			hasPath := rule.Path != ""
			hasRemote := rule.Remote != ""
			if !hasPath && !hasRemote {
				return fmt.Errorf("item %q, rule %d: must have path or remote", item.Slug, j)
			}
			if hasPath && hasRemote {
				return fmt.Errorf("item %q, rule %d: cannot have both path and remote", item.Slug, j)
			}
			if rule.Exact && !hasPath {
				return fmt.Errorf("item %q, rule %d: exact requires a path", item.Slug, j)
			}
			if hasPath {
				norm := NormalizePath(rule.Path)
				if norm == "" {
					return fmt.Errorf("item %q, rule %d: empty path", item.Slug, j)
				}
				if owner, ok := allPaths[norm]; ok {
					return fmt.Errorf("path %q appears in both %q and %q", norm, owner, item.Slug)
				}
				allPaths[norm] = item.Slug
			}
		}
	}
	return nil
}

// MatchParams holds the session metadata needed for project matching.
type MatchParams struct {
	Cwd           string
	WorkspaceRoot string
	Remotes       map[string]string
	Host          string // peer name for remote sessions; empty for local
}

// Match returns the project that best matches the given session.
//
// Each project's match rules are checked in order. Remote rules match
// against the session's git remotes. Path rules use longest-prefix
// matching against cwd and workspace_root. If a rule has Hosts set,
// it only matches sessions from those hosts.
//
// Among all matching projects, the one with the longest matching path
// wins. If no path rule matches, the first remote-matched project wins.
func (s *State) Match(p MatchParams) *Item {
	normCwd := NormalizePath(p.Cwd)
	normWS := NormalizePath(p.WorkspaceRoot)

	var bestPath *Item
	bestPathLen := 0
	var firstRemote *Item

	for i := range s.Items {
		item := &s.Items[i]
		for _, rule := range item.Match {
			if !ruleMatchesHost(rule, p.Host) {
				continue
			}

			if rule.Remote != "" {
				normRemote := NormalizeRemote(rule.Remote)
				for _, url := range p.Remotes {
					if NormalizeRemote(url) == normRemote {
						if firstRemote == nil {
							firstRemote = item
						}
						break
					}
				}
			}

			if rule.Path != "" {
				norm := NormalizePath(rule.Path)
				if norm == "" {
					continue
				}
				var matched bool
				if rule.Exact {
					matched = normCwd == norm || normWS == norm
				} else {
					matched = pathUnder(normCwd, norm) || pathUnder(normWS, norm)
				}
				if matched && len(norm) > bestPathLen {
					bestPathLen = len(norm)
					bestPath = item
				}
			}
		}
	}

	// Path match is more specific; prefer it when available.
	if bestPath != nil {
		return bestPath
	}
	return firstRemote
}

// ruleMatchesHost returns true if the rule applies to the given host.
// Rules without Hosts match any host.
func ruleMatchesHost(rule MatchRule, host string) bool {
	if len(rule.Hosts) == 0 {
		return true
	}
	for _, h := range rule.Hosts {
		if h == host {
			return true
		}
	}
	return false
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

// NormalizePath expands a stored path to an absolute form for comparison.
// Expands ~ prefix to $HOME and calls filepath.Clean.
func NormalizePath(p string) string {
	return paths.NormalizePath(p)
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

// --- Session membership ---

// AddSession appends a session key to a project's sessions list if not
// already present. Returns true if the session was added.
func (s *State) AddSession(slug, key string) bool {
	for i := range s.Items {
		if s.Items[i].Slug != slug {
			continue
		}
		for _, existing := range s.Items[i].Sessions {
			if existing == key {
				return false // already present
			}
		}
		s.Items[i].Sessions = append(s.Items[i].Sessions, key)
		return true
	}
	return false
}

// RemoveSession removes a session key from a project's sessions list.
// Returns true if the session was found and removed.
func (s *State) RemoveSession(slug, key string) bool {
	for i := range s.Items {
		if s.Items[i].Slug != slug {
			continue
		}
		for j, existing := range s.Items[i].Sessions {
			if existing == key {
				s.Items[i].Sessions = append(s.Items[i].Sessions[:j], s.Items[i].Sessions[j+1:]...)
				return true
			}
		}
		return false
	}
	return false
}

// RemoveSessionFromAll removes a session key from all projects.
// Returns the slug of the project it was removed from, or "".
func (s *State) RemoveSessionFromAll(key string) string {
	for i := range s.Items {
		for j, existing := range s.Items[i].Sessions {
			if existing == key {
				s.Items[i].Sessions = append(s.Items[i].Sessions[:j], s.Items[i].Sessions[j+1:]...)
				return s.Items[i].Slug
			}
		}
	}
	return ""
}

// FindSessionProject returns the slug of the project containing the given
// session key, or "" if not found.
func (s *State) FindSessionProject(key string) string {
	for _, item := range s.Items {
		for _, existing := range item.Sessions {
			if existing == key {
				return item.Slug
			}
		}
	}
	return ""
}

// SessionKey returns the key used to identify a session in project arrays.
// Uses ResumeKey if available (stable across restarts), falls back to
// session ID (ephemeral, for sessions without attribution).
func SessionKey(id, resumeKey string) string {
	if resumeKey != "" {
		return resumeKey
	}
	return id
}

// --- Discovery (offered projects) ---

// SessionInfo contains the session fields needed for matching and discovery.
type SessionInfo struct {
	ID            string
	Cwd           string
	WorkspaceRoot string
	Remotes       map[string]string
	Host          string // peer name; empty for local sessions
	Alive         bool
	ResumeKey     string
}

// matchParamsFromInfo builds MatchParams from a SessionInfo.
func matchParamsFromInfo(info SessionInfo) MatchParams {
	return MatchParams{
		Cwd:           info.Cwd,
		WorkspaceRoot: info.WorkspaceRoot,
		Remotes:       info.Remotes,
		Host:          info.Host,
	}
}

// DiscoveredProject is a group of unmatched sessions offered to the user.
type DiscoveredProject struct {
	SuggestedSlug string   `json:"suggested_slug"`
	Remote        string   `json:"remote,omitempty"`
	Paths         []string `json:"paths"`
	SessionCount  int      `json:"session_count"`
	ActiveCount   int      `json:"active_count"`
}

// UnmatchedActiveCount returns the number of alive sessions that don't
// match any configured project and aren't in any project's sessions array.
func (s *State) UnmatchedActiveCount(sessions []SessionInfo) int {
	count := 0
	for _, sess := range sessions {
		if !sess.Alive {
			continue
		}
		key := SessionKey(sess.ID, sess.ResumeKey)
		if s.FindSessionProject(key) != "" {
			continue
		}
		if s.Match(matchParamsFromInfo(sess)) == nil {
			count++
		}
	}
	return count
}

// Discovered groups sessions that don't match any configured project.
// Uses the same union-find algorithm as the frontend's groupByFolder:
// merge by shared remotes, then shared workspace_root, then shared cwd
// (for sessions with neither remotes nor workspace_root).
func (s *State) Discovered(sessions []SessionInfo) []DiscoveredProject {
	// Group unmatched sessions by directory (workspace_root if set,
	// otherwise cwd). Each unique directory becomes one suggestion.
	byDir := make(map[string][]SessionInfo)
	for _, sess := range sessions {
		if s.Match(matchParamsFromInfo(sess)) != nil {
			continue
		}
		dir := sess.WorkspaceRoot
		if dir == "" {
			dir = sess.Cwd
		}
		if dir == "" {
			continue
		}
		byDir[dir] = append(byDir[dir], sess)
	}
	if len(byDir) == 0 {
		return nil
	}

	result := make([]DiscoveredProject, 0, len(byDir))
	for dir, group := range byDir {
		active := 0
		for _, s := range group {
			if s.Alive {
				active++
			}
		}

		dp := DiscoveredProject{
			Paths:        []string{dir},
			SessionCount: len(group),
			ActiveCount:  active,
		}

		// Extract the most common remote for display and the add request.
		dp.Remote = mostCommonRemote(group)

		// Slug: prefer remote repo name, fall back to directory basename.
		if dp.Remote != "" {
			dp.SuggestedSlug = SlugFromRemote(dp.Remote)
		}
		if dp.SuggestedSlug == "" {
			dp.SuggestedSlug = SlugFromPath(dir)
		}
		if dp.SuggestedSlug == "" {
			dp.SuggestedSlug = "project"
		}

		result = append(result, dp)
	}

	// Active sessions first, then most sessions, then alphabetically.
	sort.Slice(result, func(i, j int) bool {
		if result[i].ActiveCount != result[j].ActiveCount {
			return result[i].ActiveCount > result[j].ActiveCount
		}
		if result[i].SessionCount != result[j].SessionCount {
			return result[i].SessionCount > result[j].SessionCount
		}
		return result[i].SuggestedSlug < result[j].SuggestedSlug
	})

	return result
}

// mostCommonRemote returns the normalized remote URL that appears most
// frequently across the sessions, or "" if none have remotes.
func mostCommonRemote(sessions []SessionInfo) string {
	counts := make(map[string]int)
	for _, s := range sessions {
		for _, url := range s.Remotes {
			counts[NormalizeRemote(url)]++
		}
	}
	var best string
	var bestN int
	for url, n := range counts {
		if n > bestN || (n == bestN && url < best) {
			best = url
			bestN = n
		}
	}
	return best
}
