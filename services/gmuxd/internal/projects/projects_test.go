package projects

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

// --- Load / Save ---

func TestLoadMissingFile(t *testing.T) {
	s, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Items) != 0 {
		t.Fatalf("expected empty items, got %d", len(s.Items))
	}
}

func TestSaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	original := &State{
		Version: currentVersion,
		Items: []Item{
			{Slug: "gmux", Match: []MatchRule{{Remote: "github.com/gmuxapp/gmux"}, {Path: "/home/user/dev/gmux"}}},
			{Slug: "scripts", Match: []MatchRule{{Path: "/home/user/scripts"}}},
		},
	}
	if err := original.Save(dir); err != nil {
		t.Fatal(err)
	}

	// File should exist with 0600 permissions.
	info, err := os.Stat(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("permissions = %o, want 600", perm)
	}

	loaded, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(loaded.Items))
	}
	if loaded.Items[0].Slug != "gmux" || loaded.Items[0].Match[0].Remote != "github.com/gmuxapp/gmux" {
		t.Errorf("item 0 = %+v", loaded.Items[0])
	}
	if loaded.Items[1].Slug != "scripts" {
		t.Errorf("item 1 = %+v", loaded.Items[1])
	}
}

func TestLoadDropsInvalidItems(t *testing.T) {
	// Regression: a hand-edited "match": null entry must be dropped on
	// load instead of poisoning the state (issue #118). Manager.Update
	// validates the whole state before every save, so a bad on-disk
	// entry would otherwise block all future mutations.
	dir := t.TempDir()
	raw := `{"version": 3, "items": [
		{"slug": "good", "match": [{"path": "/home/user/good"}]},
		{"slug": "broken", "match": null},
		{"slug": "good", "match": [{"path": "/home/user/dup-slug"}]},
		{"slug": "also-good", "match": [{"path": "/home/user/also"}]}
	]}`
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Items) != 2 {
		t.Fatalf("expected 2 items after sanitize, got %d: %+v", len(s.Items), s.Items)
	}
	if s.Items[0].Slug != "good" || s.Items[1].Slug != "also-good" {
		t.Errorf("items = %+v", s.Items)
	}
	if err := s.Validate(); err != nil {
		t.Errorf("sanitized state should validate: %v", err)
	}

	// The original bytes must be backed up before repair.
	if _, err := os.Stat(filepath.Join(dir, fileName+".bak")); err != nil {
		t.Errorf("expected backup file: %v", err)
	}

	// Mutations through the Manager must work again (this was the
	// user-visible breakage: invalid state blocked every update).
	mgr := NewManager(dir)
	if _, err := mgr.AddProject("fresh", []MatchRule{{Path: "/home/user/fresh"}}); err != nil {
		t.Fatalf("AddProject after repair: %v", err)
	}
	reloaded, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Items) != 3 {
		t.Fatalf("expected repaired state persisted with 3 items, got %d", len(reloaded.Items))
	}
}



func TestSaveCreatesNestedDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "state", "gmux")
	s := &State{Items: []Item{{Slug: "a", Match: []MatchRule{{Path: "/tmp/a"}}}}}
	if err := s.Save(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, fileName)); err != nil {
		t.Fatalf("file not created: %v", err)
	}
}

func TestLoadCorruptedFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, fileName), []byte("{invalid json"), 0o600)
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected error for corrupted file")
	}
}

// --- Validate ---

func TestValidateValid(t *testing.T) {
	s := &State{Items: []Item{
		{Slug: "gmux", Match: []MatchRule{{Remote: "github.com/gmuxapp/gmux"}, {Path: "/home/user/dev/gmux"}}},
		{Slug: "scripts", Match: []MatchRule{{Path: "/home/user/scripts"}}},
	}}
	if err := s.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateEmptyState(t *testing.T) {
	s := &State{}
	if err := s.Validate(); err != nil {
		t.Fatalf("empty state should be valid: %v", err)
	}
}

func TestValidateEmptySlug(t *testing.T) {
	s := &State{Items: []Item{{Slug: "", Match: []MatchRule{{Path: "/tmp"}}}}}
	if err := s.Validate(); err == nil {
		t.Fatal("expected error for empty slug")
	}
}

func TestValidateInvalidSlug(t *testing.T) {
	for _, slug := range []string{"Has-Caps", "-leading", "trailing-", "has spaces", "a/b"} {
		s := &State{Items: []Item{{Slug: slug, Match: []MatchRule{{Path: "/tmp"}}}}}
		if err := s.Validate(); err == nil {
			t.Errorf("expected error for slug %q", slug)
		}
	}
}

func TestValidateValidSlugs(t *testing.T) {
	for _, slug := range []string{"a", "ab", "a-b", "a1", "123", "my-project-2"} {
		s := &State{Items: []Item{{Slug: slug, Match: []MatchRule{{Path: "/tmp"}}}}}
		if err := s.Validate(); err != nil {
			t.Errorf("slug %q should be valid: %v", slug, err)
		}
	}
}

func TestValidateDuplicateSlug(t *testing.T) {
	s := &State{Items: []Item{
		{Slug: "foo", Match: []MatchRule{{Path: "/a"}}},
		{Slug: "foo", Match: []MatchRule{{Path: "/b"}}},
	}}
	if err := s.Validate(); err == nil {
		t.Fatal("expected error for duplicate slug")
	}
}

func TestValidateRemoteWithPaths(t *testing.T) {
	// Remote-based projects should also have paths (for launch directory).
	s := &State{Items: []Item{
		{Slug: "foo", Match: []MatchRule{{Remote: "github.com/org/repo"}, {Path: "/tmp"}}},
	}}
	if err := s.Validate(); err != nil {
		t.Fatalf("remote + paths should be valid: %v", err)
	}
}

func TestValidateNoPaths(t *testing.T) {
	s := &State{Items: []Item{
		{Slug: "foo"},
	}}
	if err := s.Validate(); err == nil {
		t.Fatal("expected error for missing paths")
	}
}

func TestValidateRemoteOnly(t *testing.T) {
	// A project with only a remote rule (no path) is valid in v2.
	s := &State{Items: []Item{
		{Slug: "foo", Match: []MatchRule{{Remote: "github.com/org/repo"}}},
	}}
	if err := s.Validate(); err != nil {
		t.Fatalf("remote-only project should be valid: %v", err)
	}
}

func TestValidateEmptyMatchRules(t *testing.T) {
	s := &State{Items: []Item{
		{Slug: "foo", Match: []MatchRule{}},
	}}
	if err := s.Validate(); err == nil {
		t.Fatal("expected error for empty match rules")
	}
}

func TestValidateRuleMustHavePathOrRemote(t *testing.T) {
	s := &State{Items: []Item{
		{Slug: "foo", Match: []MatchRule{{}}},
	}}
	if err := s.Validate(); err == nil {
		t.Fatal("expected error for rule with neither path nor remote")
	}
}

func TestValidateDuplicatePaths(t *testing.T) {
	s := &State{Items: []Item{
		{Slug: "a", Match: []MatchRule{{Path: "/home/user/dev"}}},
		{Slug: "b", Match: []MatchRule{{Path: "/home/user/dev"}}},
	}}
	if err := s.Validate(); err == nil {
		t.Fatal("expected error for duplicate paths")
	}
}

func TestValidateNestedPathsAllowed(t *testing.T) {
	s := &State{Items: []Item{
		{Slug: "parent", Match: []MatchRule{{Path: "/home/user/dev/gmux"}}},
		{Slug: "child", Match: []MatchRule{{Path: "/home/user/dev/gmux/.grove/teak"}}},
	}}
	if err := s.Validate(); err != nil {
		t.Fatalf("nested paths should be valid: %v", err)
	}
}

func TestValidateExactOnPathOK(t *testing.T) {
	s := &State{Items: []Item{
		{Slug: "home", Match: []MatchRule{{Path: "~", Exact: true}}},
	}}
	if err := s.Validate(); err != nil {
		t.Fatalf("exact on path should be valid: %v", err)
	}
}

func TestValidateExactOnRemoteRejected(t *testing.T) {
	s := &State{Items: []Item{
		{Slug: "bad", Match: []MatchRule{{Remote: "github.com/org/repo", Exact: true}}},
	}}
	if err := s.Validate(); err == nil {
		t.Fatal("exact on remote-only rule should be rejected")
	}
}

// --- Match ---

func TestMatchPathBased(t *testing.T) {
	s := &State{Items: []Item{
		{Slug: "gmux", Match: []MatchRule{{Path: "/home/user/dev/gmux"}}},
	}}

	// Exact match on cwd.
	if m := s.Match(MatchParams{Cwd: "/home/user/dev/gmux", WorkspaceRoot: ""}); m == nil || m.Slug != "gmux" {
		t.Error("expected match on exact cwd")
	}
	// Subdirectory match.
	if m := s.Match(MatchParams{Cwd: "/home/user/dev/gmux/src", WorkspaceRoot: ""}); m == nil || m.Slug != "gmux" {
		t.Error("expected match on subdirectory")
	}
	// Match via workspace_root.
	if m := s.Match(MatchParams{Cwd: "/somewhere/else", WorkspaceRoot: "/home/user/dev/gmux"}); m == nil || m.Slug != "gmux" {
		t.Error("expected match via workspace_root")
	}
	// No match.
	if m := s.Match(MatchParams{Cwd: "/home/user/dev/other", WorkspaceRoot: ""}); m != nil {
		t.Errorf("expected no match, got %q", m.Slug)
	}
	// No false positive on prefix overlap.
	if m := s.Match(MatchParams{Cwd: "/home/user/dev/gmux-other", WorkspaceRoot: ""}); m != nil {
		t.Errorf("expected no match for prefix overlap, got %q", m.Slug)
	}
}

func TestMatchRemoteBased(t *testing.T) {
	s := &State{Items: []Item{
		{Slug: "gmux", Match: []MatchRule{{Remote: "github.com/gmuxapp/gmux"}, {Path: "/home/user/dev/gmux"}}},
	}}

	// Match via HTTPS-style remote.
	remotes := map[string]string{"origin": "https://github.com/gmuxapp/gmux.git"}
	if m := s.Match(MatchParams{Cwd: "/any/dir", WorkspaceRoot: "", Remotes: remotes}); m == nil || m.Slug != "gmux" {
		t.Error("expected match on HTTPS remote")
	}
	// Match via SSH-style remote.
	remotes = map[string]string{"origin": "git@github.com:gmuxapp/gmux.git"}
	if m := s.Match(MatchParams{Cwd: "/any/dir", WorkspaceRoot: "", Remotes: remotes}); m == nil || m.Slug != "gmux" {
		t.Error("expected match on SSH remote")
	}
	// Match on upstream, not just origin.
	remotes = map[string]string{
		"origin":   "git@github.com:fork/gmux.git",
		"upstream": "https://github.com/gmuxapp/gmux",
	}
	if m := s.Match(MatchParams{Cwd: "/any/dir", WorkspaceRoot: "", Remotes: remotes}); m == nil || m.Slug != "gmux" {
		t.Error("expected match on upstream remote")
	}
	// No match.
	remotes = map[string]string{"origin": "https://github.com/other/repo"}
	if m := s.Match(MatchParams{Cwd: "/any/dir", WorkspaceRoot: "", Remotes: remotes}); m != nil {
		t.Errorf("expected no match, got %q", m.Slug)
	}
}

func TestMatchPathSpecificityOverRemote(t *testing.T) {
	s := &State{Items: []Item{
		{Slug: "teak", Match: []MatchRule{{Path: "/home/user/dev/gmux/.grove/teak"}}},
		{Slug: "gmux", Match: []MatchRule{{Remote: "github.com/gmuxapp/gmux"}, {Path: "/home/user/dev/gmux"}}},
	}}

	remotes := map[string]string{"origin": "git@github.com:gmuxapp/gmux.git"}

	// Session in teak directory with the gmux remote: the more specific
	// path match (teak) wins over the remote match (gmux).
	m := s.Match(MatchParams{Cwd: "/home/user/dev/gmux/.grove/teak/src", WorkspaceRoot: "", Remotes: remotes})
	if m == nil || m.Slug != "teak" {
		t.Errorf("expected teak (most specific path), got %v", m)
	}
	// Session in main gmux directory: gmux path matches and remote also matches.
	m = s.Match(MatchParams{Cwd: "/home/user/dev/gmux/src", WorkspaceRoot: "", Remotes: remotes})
	if m == nil || m.Slug != "gmux" {
		t.Errorf("expected gmux, got %v", m)
	}
	// Session with remote but no path match: remote fallback.
	m = s.Match(MatchParams{Cwd: "/other/dir", WorkspaceRoot: "", Remotes: remotes})
	if m == nil || m.Slug != "gmux" {
		t.Errorf("expected gmux (remote fallback), got %v", m)
	}
}

func TestMatchLongestPrefixWins(t *testing.T) {
	s := &State{Items: []Item{
		{Slug: "parent", Match: []MatchRule{{Path: "/home/user/dev/gmux"}}},
		{Slug: "child", Match: []MatchRule{{Path: "/home/user/dev/gmux/.grove/teak"}}},
	}}

	// Session in teak: child (longer prefix) wins.
	m := s.Match(MatchParams{Cwd: "/home/user/dev/gmux/.grove/teak/file.go", WorkspaceRoot: ""})
	if m == nil || m.Slug != "child" {
		t.Errorf("expected child (longest prefix), got %v", m)
	}
	// Session in gmux root: parent wins.
	m = s.Match(MatchParams{Cwd: "/home/user/dev/gmux/src/main.go", WorkspaceRoot: ""})
	if m == nil || m.Slug != "parent" {
		t.Errorf("expected parent, got %v", m)
	}
}

func TestMatchSpecificChildPathBeatsVagueParentEvenForRemoteProject(t *testing.T) {
	s := &State{Items: []Item{
		{Slug: "home", Match: []MatchRule{{Path: "/home/mg"}}},
		{Slug: "gmux", Match: []MatchRule{{Remote: "github.com/gmuxapp/gmux"}, {Path: "/home/mg/dev/gmux"}}},
		{Slug: "dots", Match: []MatchRule{{Remote: "github.com/mgabor3141/dots"}, {Path: "/home/mg/.local/share/chezmoi"}}},
	}}

	m := s.Match(MatchParams{Cwd: "/home/mg/dev/gmux/src", WorkspaceRoot: "", Remotes: map[string]string{"origin": "git@github.com:gmuxapp/gmux.git"}})
	if m == nil || m.Slug != "gmux" {
		t.Errorf("expected gmux, got %v", m)
	}

	m = s.Match(MatchParams{Cwd: "/home/mg/.local/share/chezmoi", WorkspaceRoot: "", Remotes: map[string]string{"origin": "git@github.com:mgabor3141/dots.git"}})
	if m == nil || m.Slug != "dots" {
		t.Errorf("expected dots, got %v", m)
	}
}

func TestMatchRemoteProjectFallsBackToPath(t *testing.T) {
	s := &State{Items: []Item{
		{Slug: "gmux", Match: []MatchRule{{Remote: "github.com/gmuxapp/gmux"}, {Path: "/home/user/dev/gmux"}}},
	}}

	// Session has no remotes (e.g. new git init that hasn't pushed).
	// Should still match via the project's paths as a fallback.
	if m := s.Match(MatchParams{Cwd: "/home/user/dev/gmux/src", WorkspaceRoot: ""}); m == nil || m.Slug != "gmux" {
		t.Error("expected remote project to fall back to path match")
	}
	// Session outside the project directory with no remotes: no match.
	if m := s.Match(MatchParams{Cwd: "/home/user/dev/other", WorkspaceRoot: ""}); m != nil {
		t.Errorf("expected no match, got %q", m.Slug)
	}
}

func TestMatchPathProjectStillTakesPrecedenceOverRemoteFallback(t *testing.T) {
	s := &State{Items: []Item{
		{Slug: "teak", Match: []MatchRule{{Path: "/home/user/dev/gmux/.grove/teak"}}},
		{Slug: "gmux", Match: []MatchRule{{Remote: "github.com/gmuxapp/gmux"}, {Path: "/home/user/dev/gmux"}}},
	}}

	// Session in teak directory with no remotes.
	// Path-matched project (teak) should win over remote project's path fallback (gmux).
	m := s.Match(MatchParams{Cwd: "/home/user/dev/gmux/.grove/teak/src", WorkspaceRoot: ""})
	if m == nil || m.Slug != "teak" {
		t.Errorf("expected teak (path precedence), got %v", m)
	}
}

func TestMatchNoMatch(t *testing.T) {
	s := &State{Items: []Item{
		{Slug: "gmux", Match: []MatchRule{{Remote: "github.com/gmuxapp/gmux"}, {Path: "/home/user/dev/gmux"}}},
		{Slug: "scripts", Match: []MatchRule{{Path: "/home/user/scripts"}}},
	}}
	m := s.Match(MatchParams{Cwd: "/home/user/dev/other", WorkspaceRoot: "", Remotes: map[string]string{"origin": "github.com/other/repo"}})
	if m != nil {
		t.Errorf("expected no match, got %q", m.Slug)
	}
}

func TestMatchEmptyState(t *testing.T) {
	s := &State{}
	if m := s.Match(MatchParams{Cwd: "/any/dir", WorkspaceRoot: "/any/ws", Remotes: map[string]string{"o": "url"}}); m != nil {
		t.Errorf("expected no match on empty state, got %v", m)
	}
}

// --- NormalizeRemote ---

func TestNormalizeRemote(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"https://github.com/org/repo.git", "github.com/org/repo"},
		{"git@github.com:org/repo.git", "github.com/org/repo"},
		{"ssh://git@github.com/org/repo", "github.com/org/repo"},
		{"http://github.com/org/repo/", "github.com/org/repo"},
		{"github.com/org/repo", "github.com/org/repo"},
		{"git://github.com/org/repo.git", "github.com/org/repo"},
	}
	for _, tt := range tests {
		got := NormalizeRemote(tt.input)
		if got != tt.want {
			t.Errorf("NormalizeRemote(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// --- Slugify / SlugFrom ---

func TestSlugify(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"My Project", "my-project"},
		{"hello_world", "hello-world"},
		{"---trim---", "trim"},
		{"UPPER", "upper"},
		{"a--b", "a-b"},
		{"", "project"},
		{"123", "123"},
	}
	for _, tt := range tests {
		got := Slugify(tt.input)
		if got != tt.want {
			t.Errorf("Slugify(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestSlugFromRemote(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"https://github.com/gmuxapp/gmux.git", "gmux"},
		{"git@github.com:org/My-Repo.git", "my-repo"},
		{"github.com/someone/cool_project", "cool-project"},
	}
	for _, tt := range tests {
		got := SlugFromRemote(tt.input)
		if got != tt.want {
			t.Errorf("SlugFromRemote(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestSlugFromPath(t *testing.T) {
	got := SlugFromPath("/home/user/dev/my-project")
	if got != "my-project" {
		t.Errorf("SlugFromPath = %q, want %q", got, "my-project")
	}
}

func TestIsValidSlug(t *testing.T) {
	valid := []string{"a", "ab", "a-b", "a1b", "123", "my-project-2"}
	for _, s := range valid {
		if !IsValidSlug(s) {
			t.Errorf("IsValidSlug(%q) should be true", s)
		}
	}
	invalid := []string{"", "A", "-a", "a-", "a--b", "a b", "a/b"}
	for _, s := range invalid {
		if IsValidSlug(s) {
			t.Errorf("IsValidSlug(%q) should be false", s)
		}
	}
}

// --- UniqueSlug ---

func TestUniqueSlugNoConflict(t *testing.T) {
	items := []Item{{Slug: "foo"}}
	if got := UniqueSlug("bar", items); got != "bar" {
		t.Errorf("got %q, want %q", got, "bar")
	}
}

func TestUniqueSlugWithConflict(t *testing.T) {
	items := []Item{{Slug: "foo"}, {Slug: "foo-2"}}
	got := UniqueSlug("foo", items)
	if got != "foo-3" {
		t.Errorf("got %q, want %q", got, "foo-3")
	}
}

// --- Discovered ---

func TestDiscoveredBasic(t *testing.T) {
	s := &State{Items: []Item{
		{Slug: "gmux", Match: []MatchRule{{Remote: "github.com/gmuxapp/gmux"}, {Path: "/dev/gmux"}}},
	}}

	sessions := []SessionInfo{
		// Matches gmux (should be excluded from discovered).
		{ID: "s1", Cwd: "/dev/gmux", Remotes: map[string]string{"origin": "git@github.com:gmuxapp/gmux.git"}},
		// Two sessions with the same remote but different cwds: separate entries.
		{ID: "s2", Cwd: "/dev/other", Remotes: map[string]string{"origin": "https://github.com/org/other.git"}},
		{ID: "s3", Cwd: "/dev/other-wt", Remotes: map[string]string{"origin": "https://github.com/org/other.git"}},
		// Standalone session (no remote, no workspace_root).
		{ID: "s4", Cwd: "/tmp/scratch"},
	}

	discovered := s.Discovered(sessions)
	if len(discovered) != 3 {
		t.Fatalf("expected 3 discovered entries (one per cwd), got %d: %+v", len(discovered), discovered)
	}

	// Each entry has exactly one path and one session.
	for _, d := range discovered {
		if len(d.Paths) != 1 {
			t.Errorf("%s: expected 1 path, got %v", d.SuggestedSlug, d.Paths)
		}
		if d.SessionCount != 1 {
			t.Errorf("%s: expected 1 session, got %d", d.SuggestedSlug, d.SessionCount)
		}
	}

	// Both remote-bearing entries get the remote for display/slug.
	byPath := map[string]DiscoveredProject{}
	for _, d := range discovered {
		byPath[d.Paths[0]] = d
	}
	if d, ok := byPath["/dev/other"]; !ok || d.Remote != "github.com/org/other" {
		t.Errorf("/dev/other: expected remote 'github.com/org/other', got %+v", byPath["/dev/other"])
	}
	if d, ok := byPath["/dev/other-wt"]; !ok || d.Remote != "github.com/org/other" {
		t.Errorf("/dev/other-wt: expected remote 'github.com/org/other', got %+v", byPath["/dev/other-wt"])
	}
	if d, ok := byPath["/tmp/scratch"]; !ok || d.Remote != "" {
		t.Errorf("/tmp/scratch: expected no remote, got %+v", byPath["/tmp/scratch"])
	}
}

func TestDiscoveredMergesWorkspaceRoot(t *testing.T) {
	s := &State{}
	sessions := []SessionInfo{
		{ID: "s1", Cwd: "/dev/gmux", WorkspaceRoot: "/dev/gmux"},
		{ID: "s2", Cwd: "/dev/gmux/.grove/teak", WorkspaceRoot: "/dev/gmux"},
	}

	discovered := s.Discovered(sessions)
	if len(discovered) != 1 {
		t.Fatalf("expected 1 group (shared workspace_root), got %d", len(discovered))
	}
	if discovered[0].SessionCount != 2 {
		t.Errorf("session_count = %d, want 2", discovered[0].SessionCount)
	}
}

func TestDiscoveredSuperrepoSubdirsSeparate(t *testing.T) {
	// A superrepo ~/ft contains a subdirectory with its own remote.
	// Sessions in both share a workspace_root of ~/ft, but sessions
	// in the subdirectory also report a remote. Discovery should
	// offer ~/ft as one entry (grouped by workspace_root), not merge
	// it with other paths that happen to share the remote.
	s := &State{}
	sessions := []SessionInfo{
		// Sessions in the superrepo root.
		{ID: "s1", Cwd: "/home/user/ft", WorkspaceRoot: "/home/user/ft",
			Remotes: map[string]string{"origin": "github.com/org/functions"}},
		{ID: "s2", Cwd: "/home/user/ft/mission-control", WorkspaceRoot: "/home/user/ft",
			Remotes: map[string]string{"origin": "github.com/org/functions"}},
		// Session in a completely different clone of the same remote.
		{ID: "s3", Cwd: "/home/user/dev/functions",
			Remotes: map[string]string{"origin": "github.com/org/functions"}},
	}

	discovered := s.Discovered(sessions)
	if len(discovered) != 2 {
		t.Fatalf("expected 2 entries (superrepo + separate clone), got %d: %+v", len(discovered), discovered)
	}

	byPath := map[string]DiscoveredProject{}
	for _, d := range discovered {
		byPath[d.Paths[0]] = d
	}

	// Superrepo groups by workspace_root, gets both sessions.
	ft := byPath["/home/user/ft"]
	if ft.SessionCount != 2 {
		t.Errorf("ft: expected 2 sessions, got %d", ft.SessionCount)
	}
	// Slug comes from the remote when available.
	if ft.SuggestedSlug != "functions" {
		t.Errorf("ft: expected slug 'functions' (from remote), got %q", ft.SuggestedSlug)
	}

	// Separate clone is its own entry.
	clone := byPath["/home/user/dev/functions"]
	if clone.SessionCount != 1 {
		t.Errorf("functions clone: expected 1 session, got %d", clone.SessionCount)
	}

	// Both have the same remote (for display), but are separate suggestions.
	if ft.Remote != clone.Remote {
		t.Errorf("expected same remote on both, got %q vs %q", ft.Remote, clone.Remote)
	}
}

func TestDiscoveredEmpty(t *testing.T) {
	s := &State{}
	if d := s.Discovered(nil); d != nil {
		t.Errorf("expected nil, got %+v", d)
	}
}

func TestDiscoveredAllMatched(t *testing.T) {
	s := &State{Items: []Item{
		{Slug: "gmux", Match: []MatchRule{{Path: "/dev/gmux"}}},
	}}
	sessions := []SessionInfo{
		{ID: "s1", Cwd: "/dev/gmux/src"},
	}
	if d := s.Discovered(sessions); d != nil {
		t.Errorf("expected nil (all matched), got %+v", d)
	}
}

// TestDiscoveredExcludesRemoteOwnedButPathMatched is the exact
// apps/apps-2 repro: the host owns project `apps` by a PATH rule
// (/mnt/user/apps), while its sessions also report a github remote.
// A viewer recomputing discovery by remote would still offer `apps`
// (it can't see the path rule); the owning host's own Discovered()
// must not, because Match() claims the session via the path rule.
func TestDiscoveredExcludesRemoteOwnedButPathMatched(t *testing.T) {
	s := &State{Items: []Item{
		{Slug: "apps", Match: []MatchRule{{Path: "/mnt/user/apps"}}},
	}}
	sessions := []SessionInfo{
		{ID: "s1", Cwd: "/mnt/user/apps", WorkspaceRoot: "/mnt/user/apps",
			Remotes: map[string]string{"origin": "https://github.com/mgabor3141/apps.git"}},
	}
	if d := s.Discovered(sessions); d != nil {
		t.Errorf("owned-by-path session must not be discovered, got %+v", d)
	}
}

// TestDiscoveredLastActive verifies the LastActive field is populated
// from the most-recent session in the group and drives recency sort.
func TestDiscoveredLastActive(t *testing.T) {
	s := &State{}
	sessions := []SessionInfo{
		{ID: "old", Cwd: "/work/old", LastActive: "2026-01-01T00:00:00Z"},
		{ID: "newA", Cwd: "/work/new", LastActive: "2026-01-01T00:00:00Z"},
		{ID: "newB", Cwd: "/work/new", LastActive: "2026-03-01T00:00:00Z"},
	}
	d := s.Discovered(sessions)
	if len(d) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(d))
	}
	// Group /work/new has the most-recent LastActive, so it sorts first
	// and carries the max timestamp of its sessions.
	if d[0].Paths[0] != "/work/new" {
		t.Errorf("expected /work/new first (most recent), got %q", d[0].Paths[0])
	}
	if d[0].LastActive != "2026-03-01T00:00:00Z" {
		t.Errorf("LastActive = %q, want 2026-03-01T00:00:00Z", d[0].LastActive)
	}
	if d[1].LastActive != "2026-01-01T00:00:00Z" {
		t.Errorf("second group LastActive = %q, want 2026-01-01T00:00:00Z", d[1].LastActive)
	}
}

// --- Session membership ---

func TestAddSession(t *testing.T) {
	s := State{Items: []Item{
		{Slug: "gmux", Match: []MatchRule{{Path: "/dev/gmux"}}},
	}}
	if !s.AddSession("gmux", "key-1") {
		t.Fatal("expected AddSession to return true")
	}
	if s.Items[0].Sessions[0] != "key-1" {
		t.Errorf("expected key-1, got %v", s.Items[0].Sessions)
	}

	// Duplicate should be a no-op.
	if s.AddSession("gmux", "key-1") {
		t.Error("duplicate AddSession should return false")
	}
	if len(s.Items[0].Sessions) != 1 {
		t.Errorf("expected 1 session, got %d", len(s.Items[0].Sessions))
	}
}

func TestRemoveSession(t *testing.T) {
	s := State{Items: []Item{
		{Slug: "gmux", Match: []MatchRule{{Path: "/dev/gmux"}}, Sessions: []string{"a", "b", "c"}},
	}}
	if !s.RemoveSession("gmux", "b") {
		t.Fatal("expected RemoveSession to return true")
	}
	if len(s.Items[0].Sessions) != 2 || s.Items[0].Sessions[0] != "a" || s.Items[0].Sessions[1] != "c" {
		t.Errorf("expected [a, c], got %v", s.Items[0].Sessions)
	}

	if s.RemoveSession("gmux", "nonexistent") {
		t.Error("removing nonexistent key should return false")
	}
}

func TestReorderSessions(t *testing.T) {
	t.Run("reorder visible keys preserves hidden tail", func(t *testing.T) {
		// `dead-1` and `dead-2` are dead/resumable entries that the
		// viewer doesn't show in the sidebar. The viewer reorders
		// what it sees (req = [c, a, b]) and trusts the daemon to
		// keep the hidden tail.
		s := State{Items: []Item{
			{Slug: "gmux", Sessions: []string{"a", "b", "c", "dead-1", "dead-2"}},
		}}
		if !s.ReorderSessions("gmux", []string{"c", "a", "b"}) {
			t.Fatal("expected ReorderSessions to return true")
		}
		want := []string{"c", "a", "b", "dead-1", "dead-2"}
		if !equalStrings(s.Items[0].Sessions, want) {
			t.Errorf("got %v, want %v", s.Items[0].Sessions, want)
		}
	})

	t.Run("store-validated keys not in existing list are appended", func(t *testing.T) {
		// The HTTP handler filters unknown IDs against the store before
		// calling this store-agnostic method. A newly known session can
		// still be inserted at its requested position.
		s := State{Items: []Item{
			{Slug: "gmux", Sessions: []string{"a", "b"}},
		}}
		s.ReorderSessions("gmux", []string{"z", "a", "b"})
		want := []string{"z", "a", "b"}
		if !equalStrings(s.Items[0].Sessions, want) {
			t.Errorf("got %v, want %v", s.Items[0].Sessions, want)
		}
	})

	t.Run("empty request is a no-op order-wise", func(t *testing.T) {
		s := State{Items: []Item{
			{Slug: "gmux", Sessions: []string{"a", "b", "c"}},
		}}
		s.ReorderSessions("gmux", []string{})
		want := []string{"a", "b", "c"}
		if !equalStrings(s.Items[0].Sessions, want) {
			t.Errorf("got %v, want %v", s.Items[0].Sessions, want)
		}
	})

	t.Run("unknown slug returns false and changes nothing", func(t *testing.T) {
		s := State{Items: []Item{
			{Slug: "gmux", Sessions: []string{"a"}},
		}}
		if s.ReorderSessions("unknown", []string{"a"}) {
			t.Error("expected false for unknown slug")
		}
		if !equalStrings(s.Items[0].Sessions, []string{"a"}) {
			t.Errorf("existing project mutated: %v", s.Items[0].Sessions)
		}
	})
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRemoveSessionFromAll(t *testing.T) {
	s := State{Items: []Item{
		{Slug: "gmux", Match: []MatchRule{{Path: "/dev/gmux"}}, Sessions: []string{"a"}},
		{Slug: "yapp", Match: []MatchRule{{Path: "/dev/yapp"}}, Sessions: []string{"b", "c"}},
	}}
	slug := s.RemoveSessionFromAll("b")
	if slug != "yapp" {
		t.Errorf("expected 'yapp', got %q", slug)
	}
	if len(s.Items[1].Sessions) != 1 || s.Items[1].Sessions[0] != "c" {
		t.Errorf("expected [c], got %v", s.Items[1].Sessions)
	}
}

func TestFindSessionProject(t *testing.T) {
	s := State{Items: []Item{
		{Slug: "gmux", Match: []MatchRule{{Path: "/dev/gmux"}}, Sessions: []string{"a"}},
		{Slug: "yapp", Match: []MatchRule{{Path: "/dev/yapp"}}, Sessions: []string{"b"}},
	}}
	if slug := s.FindSessionProject("b"); slug != "yapp" {
		t.Errorf("expected 'yapp', got %q", slug)
	}
	if slug := s.FindSessionProject("nonexistent"); slug != "" {
		t.Errorf("expected empty, got %q", slug)
	}
}

func TestDiscoveredActiveCount(t *testing.T) {
	s := State{Items: []Item{
		{Slug: "gmux", Match: []MatchRule{{Remote: "github.com/gmuxapp/gmux"}, {Path: "/dev/gmux"}}},
	}}
	sessions := []SessionInfo{
		{ID: "s1", Cwd: "/dev/yapp", Alive: true},
		{ID: "s2", Cwd: "/dev/yapp", Alive: false},
		{ID: "s3", Cwd: "/other", Alive: true},
	}
	discovered := s.Discovered(sessions)
	// s1 and s2 share cwd, s3 is separate
	for _, d := range discovered {
		if d.Paths[0] == "/dev/yapp" {
			if d.ActiveCount != 1 {
				t.Errorf("yapp active_count = %d, want 1", d.ActiveCount)
			}
			if d.SessionCount != 2 {
				t.Errorf("yapp session_count = %d, want 2", d.SessionCount)
			}
		}
	}
}

// --- Manager ---

func TestManagerAutoAssign(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)

	// Create a project.
	err := mgr.Update(func(s *State) bool {
		s.Items = []Item{
			{Slug: "gmux", Match: []MatchRule{{Remote: "github.com/gmuxapp/gmux"}, {Path: "/dev/gmux"}}},
		}
		return true
	})
	if err != nil {
		t.Fatal(err)
	}

	// Auto-assign a matching session.
	slug := mgr.AutoAssignSession(SessionInfo{
		ID: "sess-1", Cwd: "/dev/gmux/src", WorkspaceRoot: "/dev/gmux",
		Remotes: map[string]string{"origin": "github.com/gmuxapp/gmux"},
		Alive:   true,
	})
	if slug != "gmux" {
		t.Errorf("expected 'gmux', got %q", slug)
	}

	// Verify persisted.
	state, _ := mgr.Load()
	if len(state.Items[0].Sessions) != 1 || state.Items[0].Sessions[0] != "sess-1" {
		t.Errorf("expected [sess-1], got %v", state.Items[0].Sessions)
	}

	// Duplicate should be a no-op.
	slug2 := mgr.AutoAssignSession(SessionInfo{
		ID: "sess-1", Cwd: "/dev/gmux/src",
		Alive: true,
	})
	if slug2 != "" {
		t.Errorf("duplicate auto-assign should return empty, got %q", slug2)
	}
}

func TestManagerAutoAssignDistinctIDsDoNotCollide(t *testing.T) {
	mgr := NewManager(t.TempDir())
	if err := mgr.Update(func(s *State) bool {
		s.Items = []Item{{Slug: "gmux", Match: []MatchRule{{Path: "/dev/gmux"}}}}
		return true
	}); err != nil {
		t.Fatal(err)
	}

	mgr.AutoAssignAll([]SessionInfo{
		{ID: "sess-adapter-a", Cwd: "/dev/gmux", Alive: true},
		{ID: "sess-adapter-b", Cwd: "/dev/gmux", Alive: true},
	})
	state, err := mgr.Load()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"sess-adapter-a", "sess-adapter-b"}
	if !equalStrings(state.Items[0].Sessions, want) {
		t.Errorf("distinct IDs must not collide: got %v, want %v", state.Items[0].Sessions, want)
	}
}

func TestManagerAutoAssignKeepsIDOnDisplayNameChange(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)

	mgr.Update(func(s *State) bool {
		s.Items = []Item{
			{Slug: "gmux", Match: []MatchRule{{Path: "/dev/gmux"}}},
		}
		return true
	})

	// Session initially registered by ID (no slug yet).
	mgr.AutoAssignSession(SessionInfo{
		ID: "sess-1", Cwd: "/dev/gmux", Alive: true,
	})

	state, _ := mgr.Load()
	if state.Items[0].Sessions[0] != "sess-1" {
		t.Fatalf("expected sess-1, got %v", state.Items[0].Sessions)
	}

	// A later upsert must leave the membership ID and its position unchanged.
	mgr.AutoAssignSession(SessionInfo{ID: "sess-1", Cwd: "/dev/gmux", Alive: true})

	state, _ = mgr.Load()
	if len(state.Items[0].Sessions) != 1 || state.Items[0].Sessions[0] != "sess-1" {
		t.Errorf("expected [sess-1], got %v", state.Items[0].Sessions)
	}
}

func TestManagerDismissSession(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)

	mgr.Update(func(s *State) bool {
		s.Items = []Item{
			{Slug: "gmux", Match: []MatchRule{{Path: "/dev/gmux"}}, Sessions: []string{"sess-x", "key-2"}},
		}
		return true
	})

	slug := mgr.DismissSession("sess-x")
	if slug != "gmux" {
		t.Errorf("expected 'gmux', got %q", slug)
	}

	state, _ := mgr.Load()
	if len(state.Items[0].Sessions) != 1 || state.Items[0].Sessions[0] != "key-2" {
		t.Errorf("expected [key-2], got %v", state.Items[0].Sessions)
	}
}

func TestManagerUnmatchedNotAutoAssigned(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)

	mgr.Update(func(s *State) bool {
		s.Items = []Item{
			{Slug: "gmux", Match: []MatchRule{{Path: "/dev/gmux"}}},
		}
		return true
	})

	// Session doesn't match any project.
	slug := mgr.AutoAssignSession(SessionInfo{
		ID: "sess-1", Cwd: "/dev/other", Alive: true,
	})
	if slug != "" {
		t.Errorf("unmatched session should not be assigned, got %q", slug)
	}

	state, _ := mgr.Load()
	if len(state.Items[0].Sessions) != 0 {
		t.Errorf("expected empty sessions, got %v", state.Items[0].Sessions)
	}
}

func TestManagerDeadResumableNotAutoAssigned(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)

	mgr.Update(func(s *State) bool {
		s.Items = []Item{
			{Slug: "gmux", Match: []MatchRule{{Path: "/dev/gmux"}}},
		}
		return true
	})

	assigned := mgr.AutoAssignSession(SessionInfo{
		ID: "sess-1", Cwd: "/dev/gmux", Alive: false, Resumable: true,
	})
	if assigned != "" {
		t.Fatalf("dead/resumable session should not be auto-assigned, got %q", assigned)
	}

	state, _ := mgr.Load()
	if len(state.Items[0].Sessions) != 0 {
		t.Fatalf("dead/resumable session was added back to sidebar membership: %v", state.Items[0].Sessions)
	}
}

// --- UnmatchedActiveCount ---

func TestUnmatchedActiveCount(t *testing.T) {
	s := State{Items: []Item{
		{Slug: "gmux", Match: []MatchRule{{Path: "/dev/gmux"}}, Sessions: []string{"s1"}},
	}}
	sessions := []SessionInfo{
		{ID: "s1", Cwd: "/dev/gmux", Alive: true},        // matched + in array
		{ID: "s2", Cwd: "/dev/gmux", Alive: true},        // matched but not in array
		{ID: "s3", Cwd: "/somewhere/else", Alive: true},  // unmatched + alive
		{ID: "s4", Cwd: "/somewhere/else", Alive: false}, // unmatched + dead
		{ID: "s5", Cwd: "/another/place", Alive: true},   // unmatched + alive
	}
	count := s.UnmatchedActiveCount(sessions)
	// s3 and s5 are unmatched+alive. s2 matches gmux (not counted). s4 is dead.
	if count != 2 {
		t.Errorf("expected 2, got %d", count)
	}
}

func TestUnmatchedActiveCountAllMatched(t *testing.T) {
	s := State{Items: []Item{
		{Slug: "gmux", Match: []MatchRule{{Path: "/dev/gmux"}}},
	}}
	sessions := []SessionInfo{
		{ID: "s1", Cwd: "/dev/gmux/src", Alive: true},
	}
	if count := s.UnmatchedActiveCount(sessions); count != 0 {
		t.Errorf("expected 0, got %d", count)
	}
}

func TestManagerWatchRemovals(t *testing.T) {
	mgr := NewManager(t.TempDir())
	if err := mgr.Update(func(s *State) bool {
		s.Items = []Item{{Slug: "gmux", Match: []MatchRule{{Path: "/dev/gmux"}}, Sessions: []string{"sess-1"}}}
		return true
	}); err != nil {
		t.Fatal(err)
	}

	events := make(chan store.Event)
	done := make(chan struct{})
	go func() { mgr.WatchRemovals(events); close(done) }()
	// Activity pulses (#399's turn model) share the bus but are not
	// removals — membership must survive them.
	events <- store.Event{Type: store.EventSessionActivity, ID: "sess-1"}
	events <- store.Event{Type: store.EventSessionRemove, ID: "sess-1"}
	events <- store.Event{Type: store.EventSessionRemove, ID: "sess-1"} // idempotent after dismiss/removal
	close(events)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("WatchRemovals did not return after channel close")
	}

	state, err := mgr.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Items[0].Sessions) != 0 {
		t.Errorf("expected empty sessions, got %v", state.Items[0].Sessions)
	}
}

// --- Manager.Broadcast is called on mutations ---

func TestManagerBroadcastCalled(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)
	called := 0
	mgr.Broadcast = func(*State) { called++ }

	mgr.Update(func(s *State) bool {
		s.Items = []Item{{Slug: "test", Match: []MatchRule{{Path: "/test"}}}}
		return true
	})
	if called != 1 {
		t.Errorf("expected 1 broadcast, got %d", called)
	}

	// No-op update should not broadcast.
	mgr.Update(func(s *State) bool { return false })
	if called != 1 {
		t.Errorf("expected still 1 broadcast after no-op, got %d", called)
	}
}

func TestManagerCleanupSessions(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)

	mgr.Update(func(s *State) bool {
		s.Items = []Item{
			{Slug: "gmux", Match: []MatchRule{{Path: "/dev/gmux"}}, Sessions: []string{"s1", "s2", "s3"}},
			{Slug: "yapp", Match: []MatchRule{{Path: "/dev/yapp"}}, Sessions: []string{"s4", "s5"}},
		}
		return true
	})

	// Only s1, s3, and s4 are known to the store.
	known := map[string]bool{"s1": true, "s3": true, "s4": true}
	mgr.CleanupSessions(known)

	state, _ := mgr.Load()
	if got := state.Items[0].Sessions; len(got) != 2 || got[0] != "s1" || got[1] != "s3" {
		t.Errorf("gmux sessions: expected [s1 s3], got %v", got)
	}
	if got := state.Items[1].Sessions; len(got) != 1 || got[0] != "s4" {
		t.Errorf("yapp sessions: expected [s4], got %v", got)
	}
}

func TestManagerCleanupSessionsNoOrphans(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)
	broadcastCalled := false
	mgr.Broadcast = func(*State) { broadcastCalled = true }

	mgr.Update(func(s *State) bool {
		s.Items = []Item{
			{Slug: "gmux", Match: []MatchRule{{Path: "/dev/gmux"}}, Sessions: []string{"s1", "s2"}},
		}
		return true
	})
	broadcastCalled = false // reset from the Update

	known := map[string]bool{"s1": true, "s2": true}
	mgr.CleanupSessions(known)

	// No orphans, so no save/broadcast should happen.
	if broadcastCalled {
		t.Error("broadcast should not be called when nothing changed")
	}
}

func TestManagerAutoAssignAll(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)

	mgr.Update(func(s *State) bool {
		s.Items = []Item{
			{Slug: "gmux", Match: []MatchRule{{Path: "/dev/gmux"}}},
			{Slug: "yapp", Match: []MatchRule{{Path: "/dev/yapp"}}},
		}
		return true
	})

	sessions := []SessionInfo{
		{ID: "s1", Cwd: "/dev/gmux/src", Alive: true},
		{ID: "s2", Cwd: "/dev/yapp", Alive: true},
		{ID: "s3", Cwd: "/dev/gmux", Alive: false, Resumable: true}, // dead+resumable: skipped
		{ID: "s4", Cwd: "/dev/gmux", Alive: false},                  // dead, no resume: skipped
		{ID: "s5", Cwd: "/other", Alive: true},                      // unmatched: skipped
	}

	mgr.AutoAssignAll(sessions)

	state, _ := mgr.Load()
	if len(state.Items[0].Sessions) != 1 || state.Items[0].Sessions[0] != "s1" {
		t.Errorf("gmux sessions: expected [s1], got %v", state.Items[0].Sessions)
	}
	if len(state.Items[1].Sessions) != 1 || state.Items[1].Sessions[0] != "s2" {
		t.Errorf("yapp sessions: expected [s2], got %v", state.Items[1].Sessions)
	}
}

// Peer-owned sessions must never enter the local projects.json:
// project membership is owned by the session's origin host (ADR 0002).
func TestManagerAutoAssignSkipsPeerOwnedSession(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)

	mgr.Update(func(s *State) bool {
		s.Items = []Item{
			{Slug: "gmux", Match: []MatchRule{{Path: "/dev/gmux"}}},
		}
		return true
	})

	// A session from a peer that *would* match the local rule by cwd:
	// must be ignored. The peer owns the membership; we trust the
	// stamp arriving over the wire instead.
	assigned := mgr.AutoAssignSession(SessionInfo{
		ID: "sess-tower-1", Cwd: "/dev/gmux", Host: "tower", Alive: true,
	})
	if assigned != "" {
		t.Errorf("expected no assignment for peer session, got %q", assigned)
	}

	state, _ := mgr.Load()
	if len(state.Items[0].Sessions) != 0 {
		t.Errorf("local projects.json should remain empty, got %v", state.Items[0].Sessions)
	}
}

func TestManagerAutoAssignAllSkipsPeerOwnedSessions(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)

	mgr.Update(func(s *State) bool {
		s.Items = []Item{
			{Slug: "gmux", Match: []MatchRule{{Path: "/dev/gmux"}}},
		}
		return true
	})

	sessions := []SessionInfo{
		{ID: "local-1", Cwd: "/dev/gmux", Alive: true},
		{ID: "peer-1", Cwd: "/dev/gmux", Host: "tower", Alive: true},
		{ID: "peer-2", Cwd: "/dev/gmux", Host: "laptop", Alive: true},
	}
	mgr.AutoAssignAll(sessions)

	state, _ := mgr.Load()
	if len(state.Items[0].Sessions) != 1 || state.Items[0].Sessions[0] != "local-1" {
		t.Errorf("expected only local-1 assigned, got %v", state.Items[0].Sessions)
	}
}

func TestNormalizePathExpandsTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	// Verify the wrapper delegates correctly to paths.NormalizePath.
	if got := NormalizePath("~/dev/gmux"); got != home+"/dev/gmux" {
		t.Errorf("NormalizePath(~/dev/gmux) = %q, want %q", got, home+"/dev/gmux")
	}
}

func TestMatchHomeProjectExact(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}

	s := &State{Items: []Item{
		{Slug: "home", Match: []MatchRule{{Path: "~", Exact: true}}},
	}}

	// Session at $HOME itself matches.
	m := s.Match(MatchParams{Cwd: home})
	if m == nil || m.Slug != "home" {
		t.Errorf("session at $HOME: expected 'home', got %v", m)
	}

	// Session under $HOME does NOT match (exact).
	m = s.Match(MatchParams{Cwd: home + "/dev/gmux/src"})
	if m != nil {
		t.Errorf("session under $HOME with exact: expected nil, got %q", m.Slug)
	}

	// Session outside $HOME does not match.
	m = s.Match(MatchParams{Cwd: "/tmp/scratch"})
	if m != nil {
		t.Errorf("session outside $HOME: expected nil, got %q", m.Slug)
	}
}

func TestMatchExactPath(t *testing.T) {
	s := &State{Items: []Item{
		{Slug: "scripts", Match: []MatchRule{{Path: "/home/user/scripts", Exact: true}}},
	}}

	// Exact cwd match.
	if m := s.Match(MatchParams{Cwd: "/home/user/scripts"}); m == nil || m.Slug != "scripts" {
		t.Error("expected match on exact cwd")
	}
	// Exact workspace_root match.
	if m := s.Match(MatchParams{Cwd: "/other", WorkspaceRoot: "/home/user/scripts"}); m == nil || m.Slug != "scripts" {
		t.Error("expected match on exact workspace_root")
	}
	// Subdirectory does NOT match.
	if m := s.Match(MatchParams{Cwd: "/home/user/scripts/bin"}); m != nil {
		t.Errorf("subdirectory should not match exact rule, got %q", m.Slug)
	}
}

func TestMatchExactWithRemoteFallback(t *testing.T) {
	// A project with an exact path and a remote. The remote should still
	// match sessions anywhere; exact only constrains the path rule.
	s := &State{Items: []Item{
		{Slug: "scripts", Match: []MatchRule{
			{Remote: "github.com/org/scripts"},
			{Path: "/home/user/scripts", Exact: true},
		}},
	}}

	remotes := map[string]string{"origin": "git@github.com:org/scripts.git"}

	// Exact path match (no remote needed).
	if m := s.Match(MatchParams{Cwd: "/home/user/scripts"}); m == nil || m.Slug != "scripts" {
		t.Error("expected exact path match")
	}
	// Subdirectory: path doesn't match (exact), but remote does.
	if m := s.Match(MatchParams{Cwd: "/home/user/scripts/bin", Remotes: remotes}); m == nil || m.Slug != "scripts" {
		t.Error("expected remote fallback when exact path rejects subdir")
	}
	// Subdirectory without remote: no match at all.
	if m := s.Match(MatchParams{Cwd: "/home/user/scripts/bin"}); m != nil {
		t.Errorf("expected no match without remote, got %q", m.Slug)
	}
}

func TestManagerSeedIfEmpty(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)

	mgr.SeedIfEmpty()

	state, err := mgr.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(state.Items))
	}
	if state.Items[0].Slug != "home" {
		t.Errorf("slug = %q, want 'home'", state.Items[0].Slug)
	}
	if len(state.Items[0].Match) != 1 || state.Items[0].Match[0].Path != "~" || !state.Items[0].Match[0].Exact {
		t.Errorf("match = %+v, want [{path: ~, exact: true}]", state.Items[0].Match)
	}
}

func TestManagerSeedIfEmptySkipsWhenProjectsExist(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)

	// Add a project first.
	mgr.Update(func(s *State) bool {
		s.Items = []Item{{Slug: "existing", Match: []MatchRule{{Path: "/dev/existing"}}}}
		return true
	})

	mgr.SeedIfEmpty()

	state, _ := mgr.Load()
	if len(state.Items) != 1 || state.Items[0].Slug != "existing" {
		t.Errorf("expected existing project unchanged, got %+v", state.Items)
	}
}

// --- AssignmentsByKey (per ADR 0002) ---

func TestAssignmentsByKey_FlattensProjectArrays(t *testing.T) {
	state := &State{Items: []Item{
		{Slug: "gmux", Sessions: []string{"a", "b", "c"}},
		{Slug: "yapp", Sessions: []string{"d"}},
	}}

	got := state.AssignmentsByKey()
	want := map[string]Assignment{
		"a": {Slug: "gmux", Index: 0},
		"b": {Slug: "gmux", Index: 1},
		"c": {Slug: "gmux", Index: 2},
		"d": {Slug: "yapp", Index: 0},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d (got=%v)", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("key %q: got %+v, want %+v", k, got[k], v)
		}
	}
}

func TestAssignmentsByKey_EmptyForNoProjects(t *testing.T) {
	state := &State{}
	if got := state.AssignmentsByKey(); len(got) != 0 {
		t.Errorf("expected empty map, got %v", got)
	}
}

func TestAssignmentsByKey_FirstOccurrenceWinsOnDuplicate(t *testing.T) {
	// Defensive contract: duplicate keys shouldn't happen but the
	// behaviour is defined.
	state := &State{Items: []Item{
		{Slug: "first", Sessions: []string{"shared"}},
		{Slug: "second", Sessions: []string{"shared"}},
	}}
	got := state.AssignmentsByKey()
	if got["shared"].Slug != "first" {
		t.Errorf("expected first occurrence to win, got %+v", got["shared"])
	}
}

// ── References (v3) ─────────────────────────────────────────────────

// When a local owned project and a peer reference share a slug, the
// session-membership operations must target the owned entry only:
// references carry no Sessions[] of their own and writing to them
// would corrupt the file shape and confuse the peer (whose
// projects.json is the SOT for that slug).
func TestSessionOpsSkipReferenceWithSameSlug(t *testing.T) {
	// Reference appears BEFORE the owned entry in items[], so a
	// naive `Slug == slug` loop would hit it first. The guard in
	// AddSession/RemoveSession/ReorderSessions must skip it.
	s := State{Items: []Item{
		{Slug: "gmux", Peer: "tower"},                    // reference
		{Slug: "gmux", Match: []MatchRule{{Path: "/x"}}}, // owned
	}}

	if !s.AddSession("gmux", "sess-1") {
		t.Fatal("AddSession returned false")
	}
	if len(s.Items[0].Sessions) != 0 {
		t.Errorf("reference contaminated by AddSession: %v", s.Items[0].Sessions)
	}
	if !equalStrings(s.Items[1].Sessions, []string{"sess-1"}) {
		t.Errorf("owned project sessions = %v, want [sess-1]", s.Items[1].Sessions)
	}

	if !s.ReorderSessions("gmux", []string{"sess-1"}) {
		t.Fatal("ReorderSessions returned false")
	}
	if len(s.Items[0].Sessions) != 0 {
		t.Errorf("reference contaminated by ReorderSessions: %v", s.Items[0].Sessions)
	}

	if !s.RemoveSession("gmux", "sess-1") {
		t.Fatal("RemoveSession returned false")
	}
	if len(s.Items[1].Sessions) != 0 {
		t.Errorf("owned sessions not cleared: %v", s.Items[1].Sessions)
	}
}

func TestValidateRejectsReferenceWithMatchRules(t *testing.T) {
	s := State{Items: []Item{
		{Slug: "gmux", Peer: "tower", Match: []MatchRule{{Path: "/x"}}},
	}}
	if err := s.Validate(); err == nil {
		t.Error("expected validation error for reference with match rules")
	}
}

// A reference's node_id anchor must survive a Save/Load round-trip so
// the daemon can follow a renamed peer across restarts (ADR 0017).
func TestReferenceNodeIDRoundTrips(t *testing.T) {
	dir := t.TempDir()
	orig := &State{Items: []Item{
		{Slug: "apps", Peer: "gmux-hs", NodeID: "node_abc"},
	}}
	if err := orig.Save(dir); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Items) != 1 || loaded.Items[0].NodeID != "node_abc" || loaded.Items[0].Peer != "gmux-hs" {
		t.Errorf("reference did not round-trip: %+v", loaded.Items)
	}
}

func TestValidateRejectsNodeIDOnOwnedProject(t *testing.T) {
	s := State{Items: []Item{
		{Slug: "gmux", Match: []MatchRule{{Path: "/x"}}, NodeID: "node_abc"},
	}}
	if err := s.Validate(); err == nil {
		t.Error("expected validation error for node_id on an owned project")
	}
}

// The positive counterpart: a reference *with* a node_id is valid — it's
// the rename-proof anchor — so Validate must accept it. Guards against a
// future change that mistakenly rejects node_id on references too.
func TestValidateAcceptsNodeIDOnReference(t *testing.T) {
	s := State{Items: []Item{
		{Slug: "apps", Peer: "gmux-hs", NodeID: "node_abc"},
	}}
	if err := s.Validate(); err != nil {
		t.Errorf("node_id on a reference should be valid, got: %v", err)
	}
}

func TestValidateAllowsSameSlugAsOwnedAndReference(t *testing.T) {
	s := State{Items: []Item{
		{Slug: "gmux", Match: []MatchRule{{Path: "/x"}}},
		{Slug: "gmux", Peer: "tower"},
	}}
	if err := s.Validate(); err != nil {
		t.Errorf("expected mixed owned+reference with same slug to validate, got: %v", err)
	}
}

func TestValidateRejectsDuplicateReferences(t *testing.T) {
	s := State{Items: []Item{
		{Slug: "gmux", Peer: "tower"},
		{Slug: "gmux", Peer: "tower"},
	}}
	if err := s.Validate(); err == nil {
		t.Error("expected duplicate reference rejection")
	}
}

// PruneNamespacedKeys is the GC hook fired when a Local peer
// (devcontainer) is removed. It must:
//   - drop keys whose suffix is exactly "@<peer>"
//   - leave slug-keyed entries alone (those survive container restarts
//     because the slug is stable)
//   - leave references alone (their session order is the peer's, not
//     ours)
//   - not be fooled by peer names that are prefixes of other peer names
//     ("dev" must not match keys ending in "@develop")
func TestPruneNamespacedKeys(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)
	mgr.Update(func(s *State) bool {
		s.Items = []Item{
			{
				Slug:  "proj",
				Match: []MatchRule{{Path: "/x"}},
				Sessions: []string{
					"sess-a@container",   // should be pruned
					"sess-b@container",   // should be pruned
					"sess-c@develop",     // not @container; kept
					"sess-d",             // local id; kept
					"claude-attribution", // slug; kept (survives container restart)
				},
			},
			{
				Slug:     "remote",
				Peer:     "container",
				Sessions: nil,
			},
		}
		return true
	})

	mgr.PruneNamespacedKeys("container")

	state, _ := mgr.Load()
	want := []string{"sess-c@develop", "sess-d", "claude-attribution"}
	if !equalStrings(state.Items[0].Sessions, want) {
		t.Errorf("after prune: got %v, want %v", state.Items[0].Sessions, want)
	}
	// Reference entry untouched (it had no sessions to begin with;
	// pin the loop's continue-on-IsReference branch).
	if len(state.Items[1].Sessions) != 0 {
		t.Errorf("reference entry mutated: %v", state.Items[1].Sessions)
	}
}

func TestPruneNamespacedKeysEmptyPeerNameNoop(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)
	mgr.Update(func(s *State) bool {
		s.Items = []Item{
			{Slug: "p", Match: []MatchRule{{Path: "/x"}}, Sessions: []string{"a", "b"}},
		}
		return true
	})
	mgr.PruneNamespacedKeys("")
	state, _ := mgr.Load()
	if !equalStrings(state.Items[0].Sessions, []string{"a", "b"}) {
		t.Errorf("empty peer pruned sessions: %v", state.Items[0].Sessions)
	}
}
