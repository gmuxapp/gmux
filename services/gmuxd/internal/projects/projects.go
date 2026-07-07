// Package projects manages the user-curated project list that controls
// which sessions appear in the sidebar.
//
// Each owned project has a list of match rules (remote URLs or filesystem
// paths). A session matches a project if
// any rule matches. First matching project in list order wins; among
// path rules, longest prefix wins. State is persisted to
// projects.json in the state directory.
package projects

import (
	"encoding/json"
	"fmt"
	"log"
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
const currentVersion = 3

// MatchRule is a single criterion for matching sessions to a project.
// Exactly one of Path or Remote should be set.
//
// Pre-v3 schemas allowed a Hosts []string field for scoping a rule to
// specific peers. The field is no longer honoured: in the cross-host
// project ownership model, scoping is implicit in ownership (each
// project is owned by exactly one host). v2 files containing the
// field decode silently because the JSON decoder ignores unknown
// fields; the field is dropped on next Save.
type MatchRule struct {
	Path   string `json:"path,omitempty"`
	Remote string `json:"remote,omitempty"`
	Exact  bool   `json:"exact,omitempty"` // path must match exactly, not as prefix
}

// Item is a single entry in the user's projects.json items[] list. Two
// shapes share this struct:
//
//   - Owned: Slug + Match[] + Sessions[]. This is a project owned by
//     this host. Match drives session attribution; Sessions[] holds
//     the ordered list of session keys for sidebar order.
//   - Reference: Slug + Peer. This is a viewer-side reference to a
//     project owned by a peer. Match and Sessions are unused: the
//     peer's projects.json is the source of truth for both. The viewer
//     just declares "show this peer's project in my sidebar at this
//     position."
//
// Validate enforces exactly one of {Match present, Peer present}.
type Item struct {
	Slug     string      `json:"slug"`
	Peer     string      `json:"peer,omitempty"`
	Match    []MatchRule `json:"match,omitempty"`
	Sessions []string    `json:"sessions,omitempty"`
	// NodeID is the referenced peer's stable opaque identity (ADR 0007).
	// Peer (the display name) is the runtime key; NodeID is only the
	// viewer's liveness anchor (ADR 0017): it keeps a reference matching
	// the right host across re-adds and stops a reused name from matching
	// the wrong one. Set only on references; stamped at creation.
	NodeID string `json:"node_id,omitempty"`
}

// IsReference reports whether this item is a reference to a peer-owned
// project. References have no local match rules; their content is
// driven entirely by the peer's stamps on the wire.
func (i *Item) IsReference() bool {
	return i.Peer != ""
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

	// Before a real schema upgrade rewrites the file, snapshot the
	// pre-migration bytes to projects.json.bak so the change is
	// recoverable (notably across the 2.0 upgrade). Best-effort.
	original := data
	backedUp := false
	if onDiskVersion(data) < currentVersion {
		backupFile(path, original)
		backedUp = true
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

	// Drop invalid items (e.g. a hand-edited "match": null) instead of
	// letting them poison the whole state: Manager.Update validates the
	// full state before every save, so one bad on-disk entry would make
	// every future mutation fail. The repair is in-memory only: the
	// on-disk file is untouched until the next Save persists the
	// sanitized form, so repeated loads of a still-invalid file re-drop
	// the same items (and re-write an identical backup, since the file
	// hasn't changed). Snapshot the original bytes before dropping —
	// unless the migration path above already did, in which case the
	// pre-migration backup must not be clobbered with migrated bytes.
	if dropped := s.sanitize(); len(dropped) > 0 {
		if !backedUp {
			backupFile(path, original)
		}
		for _, err := range dropped {
			log.Printf("projects: dropping invalid item in %s: %v", path, err)
		}
	}
	return &s, nil
}

// sanitize removes items that fail validation, keeping the rest in
// order. Each item is checked against the already-accepted prefix, so
// cross-item rules (duplicate slugs, duplicate paths) resolve
// first-wins. Returns one error per dropped item.
func (s *State) sanitize() []error {
	valid := State{Version: s.Version}
	var dropped []error
	for _, item := range s.Items {
		candidate := State{Version: s.Version, Items: append(append([]Item{}, valid.Items...), item)}
		if err := candidate.Validate(); err != nil {
			dropped = append(dropped, fmt.Errorf("item %q: %w", item.Slug, err))
			continue
		}
		valid.Items = candidate.Items
	}
	s.Items = valid.Items
	return dropped
}

// onDiskVersion peeks at the schema version of a raw projects.json
// document. A missing or invalid version field is the original
// pre-version format (0).
func onDiskVersion(data []byte) int {
	var doc struct {
		Version int `json:"version"`
	}
	_ = json.Unmarshal(data, &doc)
	return doc.Version
}

// backupFile writes the pre-migration bytes to path+".bak" (0600),
// overwriting any earlier backup. Best-effort: a failure is logged but
// never fatal — a missing backup must not stop projects.json loading.
func backupFile(path string, original []byte) {
	bak := path + ".bak"
	if err := os.WriteFile(bak, original, 0o600); err != nil {
		log.Printf("projects: could not write %s: %v", bak, err)
		return
	}
	log.Printf("projects: backed up pre-migration state to %s", bak)
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
		// References use a composite identity (peer+slug); owned
		// projects use just slug. Detect duplicates within each
		// kind separately so a local owned "gmux" and a reference
		// to peer "workstation/gmux" can coexist.
		key := item.Slug
		if item.IsReference() {
			key = item.Peer + "::" + item.Slug
		}
		if slugs[key] {
			if item.IsReference() {
				return fmt.Errorf("duplicate reference %s/%s", item.Peer, item.Slug)
			}
			return fmt.Errorf("duplicate slug %q", item.Slug)
		}
		slugs[key] = true

		// node_id is a reference anchor; it's meaningless on an owned
		// project (the viewer owns it, so there's no peer to anchor to).
		if item.NodeID != "" && !item.IsReference() {
			return fmt.Errorf("item %q: node_id is only valid on references", item.Slug)
		}

		if item.IsReference() {
			// References carry no rules or sessions of their own.
			// A non-empty Match[] would be a user error; reject it
			// to keep the data shape unambiguous.
			if len(item.Match) > 0 {
				return fmt.Errorf("item %q: references cannot carry match rules", item.Slug)
			}
			if len(item.Sessions) > 0 {
				return fmt.Errorf("item %q: references cannot carry sessions", item.Slug)
			}
			continue
		}

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
		// Skip reference items: their content is driven by peer
		// stamps, not local rules. Match() returns owned projects
		// only.
		if item.IsReference() {
			continue
		}
		for _, rule := range item.Match {
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
// already present. Returns true if the session was added. References
// are skipped: their session order is the peer's responsibility.
func (s *State) AddSession(slug, key string) bool {
	for i := range s.Items {
		if s.Items[i].Slug != slug || s.Items[i].IsReference() {
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

// ReorderSessions applies a partial-reorder to a project's sessions
// list: the keys in `req` take their positions (relative to each
// other) at the start; any existing keys not in `req` keep their
// relative order at the tail.
//
// This lets a viewer reorder what it can see without enumerating
// dead / hidden entries it doesn't track. Critical for peer reorders
// via the /v1/peers/{peer}/... proxy: the viewer never sees the full
// projects.json on the peer, so it can't reconstruct the hidden tail.
//
// Returns true if the project was found. References (peer-owned
// projects) are skipped: their session order lives on the peer, not
// in the local file, so the local PATCH endpoint must not touch
// them even when the slug collides with a local owned project.
func (s *State) ReorderSessions(slug string, req []string) bool {
	for i := range s.Items {
		if s.Items[i].Slug != slug || s.Items[i].IsReference() {
			continue
		}
		inReq := make(map[string]bool, len(req))
		for _, k := range req {
			inReq[k] = true
		}
		merged := make([]string, 0, len(req)+len(s.Items[i].Sessions))
		merged = append(merged, req...)
		for _, k := range s.Items[i].Sessions {
			if !inReq[k] {
				merged = append(merged, k)
			}
		}
		s.Items[i].Sessions = merged
		return true
	}
	return false
}

// RemoveSession removes a session key from a project's sessions list.
// Returns true if the session was found and removed. References are
// skipped: they carry no sessions[] of their own.
func (s *State) RemoveSession(slug, key string) bool {
	for i := range s.Items {
		if s.Items[i].Slug != slug || s.Items[i].IsReference() {
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

// Assignment is one project's claim on a session: the slug of the
// owning project and the 0-based index in its Sessions[] array.
// The zero value (Slug == "", Index == 0) means "no project claims
// this session" and is what AssignmentsByKey returns by default for
// keys not found in any project.
type Assignment struct {
	Slug  string
	Index int
}

// AssignmentsByKey returns a flat map from session key to the
// project Assignment claiming it. Pure derivation from State; the
// caller is expected to use the result to stamp sessions (see
// store.Store.Reconcile).
//
// First occurrence wins on duplicate keys: a key appearing in two
// items would point at the first item's Assignment. This shouldn't
// happen because dismiss/auto-assign keep entries unique, but the
// behaviour is at least defined.
func (s *State) AssignmentsByKey() map[string]Assignment {
	out := make(map[string]Assignment)
	for _, item := range s.Items {
		for i, key := range item.Sessions {
			if _, exists := out[key]; exists {
				continue
			}
			out[key] = Assignment{Slug: item.Slug, Index: i}
		}
	}
	return out
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
// Uses Slug if available (stable across restarts), falls back to
// session ID (ephemeral, for sessions without attribution).
func SessionKey(id, slug string) string {
	if slug != "" {
		return slug
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
	// LocalHost is true when Host is a Local peer (devcontainer): the
	// session lives on a peer but the parent owns its project
	// assignment. AutoAssign treats these as local for the purposes
	// of writing into projects.json.
	LocalHost bool
	Alive     bool
	Resumable bool // dead but has a resume command persisted
	Slug      string
	// LastActive is an RFC3339 timestamp of the session's most recent
	// noteworthy activity (last_activity_at, falling back to
	// created_at). Used to sort discovered suggestions by recency so a
	// peer-advertised discovered row sorts consistently against the
	// viewer's own local discovery.
	LastActive string
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
	// LastActive is the most-recent LastActive among the sessions in
	// this group, as an RFC3339 string. Mirrors the TS
	// DiscoveredProject.last_active field so peer-advertised rows sort
	// consistently with the viewer's locally-computed ones.
	LastActive string `json:"last_active,omitempty"`
}

// UnmatchedActiveCount returns the number of alive sessions that don't
// match any configured project and aren't in any project's sessions array.
func (s *State) UnmatchedActiveCount(sessions []SessionInfo) int {
	count := 0
	for _, sess := range sessions {
		if !sess.Alive {
			continue
		}
		key := SessionKey(sess.ID, sess.Slug)
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
		lastActive := ""
		for _, s := range group {
			if s.Alive {
				active++
			}
			if s.LastActive > lastActive {
				lastActive = s.LastActive
			}
		}

		dp := DiscoveredProject{
			Paths:        []string{dir},
			SessionCount: len(group),
			ActiveCount:  active,
			LastActive:   lastActive,
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

	// Recency first, then active sessions, then most sessions, then
	// alphabetically by slug, then by path. Mirrors the TS sort so
	// peer-advertised rows interleave consistently with local ones.
	sort.Slice(result, func(i, j int) bool {
		if result[i].LastActive != result[j].LastActive {
			return result[i].LastActive > result[j].LastActive
		}
		if result[i].ActiveCount != result[j].ActiveCount {
			return result[i].ActiveCount > result[j].ActiveCount
		}
		if result[i].SessionCount != result[j].SessionCount {
			return result[i].SessionCount > result[j].SessionCount
		}
		if result[i].SuggestedSlug != result[j].SuggestedSlug {
			return result[i].SuggestedSlug < result[j].SuggestedSlug
		}
		return result[i].Paths[0] < result[j].Paths[0]
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
