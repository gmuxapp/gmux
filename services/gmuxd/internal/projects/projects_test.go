package projects

import (
	"os"
	"path/filepath"
	"testing"
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
		Items: []Item{
			{Slug: "gmux", Remote: "github.com/gmuxapp/gmux", Paths: []string{"/home/user/dev/gmux"}},
			{Slug: "scripts", Paths: []string{"/home/user/scripts"}},
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
	if loaded.Items[0].Slug != "gmux" || loaded.Items[0].Remote != "github.com/gmuxapp/gmux" {
		t.Errorf("item 0 = %+v", loaded.Items[0])
	}
	if loaded.Items[1].Slug != "scripts" {
		t.Errorf("item 1 = %+v", loaded.Items[1])
	}
}

func TestSaveCreatesNestedDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "state", "gmux")
	s := &State{Items: []Item{{Slug: "a", Paths: []string{"/tmp/a"}}}}
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
		{Slug: "gmux", Remote: "github.com/gmuxapp/gmux", Paths: []string{"/home/user/dev/gmux"}},
		{Slug: "scripts", Paths: []string{"/home/user/scripts"}},
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
	s := &State{Items: []Item{{Slug: "", Paths: []string{"/tmp"}}}}
	if err := s.Validate(); err == nil {
		t.Fatal("expected error for empty slug")
	}
}

func TestValidateInvalidSlug(t *testing.T) {
	for _, slug := range []string{"Has-Caps", "-leading", "trailing-", "has spaces", "a/b"} {
		s := &State{Items: []Item{{Slug: slug, Paths: []string{"/tmp"}}}}
		if err := s.Validate(); err == nil {
			t.Errorf("expected error for slug %q", slug)
		}
	}
}

func TestValidateValidSlugs(t *testing.T) {
	for _, slug := range []string{"a", "ab", "a-b", "a1", "123", "my-project-2"} {
		s := &State{Items: []Item{{Slug: slug, Paths: []string{"/tmp"}}}}
		if err := s.Validate(); err != nil {
			t.Errorf("slug %q should be valid: %v", slug, err)
		}
	}
}

func TestValidateDuplicateSlug(t *testing.T) {
	s := &State{Items: []Item{
		{Slug: "foo", Paths: []string{"/a"}},
		{Slug: "foo", Paths: []string{"/b"}},
	}}
	if err := s.Validate(); err == nil {
		t.Fatal("expected error for duplicate slug")
	}
}

func TestValidateRemoteWithPaths(t *testing.T) {
	// Remote-based projects should also have paths (for launch directory).
	s := &State{Items: []Item{
		{Slug: "foo", Remote: "github.com/org/repo", Paths: []string{"/tmp"}},
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

func TestValidateRemoteWithoutPaths(t *testing.T) {
	s := &State{Items: []Item{
		{Slug: "foo", Remote: "github.com/org/repo"},
	}}
	if err := s.Validate(); err == nil {
		t.Fatal("expected error for remote without paths")
	}
}

func TestValidateDuplicatePaths(t *testing.T) {
	s := &State{Items: []Item{
		{Slug: "a", Paths: []string{"/home/user/dev"}},
		{Slug: "b", Paths: []string{"/home/user/dev"}},
	}}
	if err := s.Validate(); err == nil {
		t.Fatal("expected error for duplicate paths")
	}
}

func TestValidateNestedPathsAllowed(t *testing.T) {
	s := &State{Items: []Item{
		{Slug: "parent", Paths: []string{"/home/user/dev/gmux"}},
		{Slug: "child", Paths: []string{"/home/user/dev/gmux/.grove/teak"}},
	}}
	if err := s.Validate(); err != nil {
		t.Fatalf("nested paths should be valid: %v", err)
	}
}

// --- Match ---

func TestMatchPathBased(t *testing.T) {
	s := &State{Items: []Item{
		{Slug: "gmux", Paths: []string{"/home/user/dev/gmux"}},
	}}

	// Exact match on cwd.
	if m := s.Match("/home/user/dev/gmux", "", nil); m == nil || m.Slug != "gmux" {
		t.Error("expected match on exact cwd")
	}
	// Subdirectory match.
	if m := s.Match("/home/user/dev/gmux/src", "", nil); m == nil || m.Slug != "gmux" {
		t.Error("expected match on subdirectory")
	}
	// Match via workspace_root.
	if m := s.Match("/somewhere/else", "/home/user/dev/gmux", nil); m == nil || m.Slug != "gmux" {
		t.Error("expected match via workspace_root")
	}
	// No match.
	if m := s.Match("/home/user/dev/other", "", nil); m != nil {
		t.Errorf("expected no match, got %q", m.Slug)
	}
	// No false positive on prefix overlap.
	if m := s.Match("/home/user/dev/gmux-other", "", nil); m != nil {
		t.Errorf("expected no match for prefix overlap, got %q", m.Slug)
	}
}

func TestMatchRemoteBased(t *testing.T) {
	s := &State{Items: []Item{
		{Slug: "gmux", Remote: "github.com/gmuxapp/gmux", Paths: []string{"/home/user/dev/gmux"}},
	}}

	// Match via HTTPS-style remote.
	remotes := map[string]string{"origin": "https://github.com/gmuxapp/gmux.git"}
	if m := s.Match("/any/dir", "", remotes); m == nil || m.Slug != "gmux" {
		t.Error("expected match on HTTPS remote")
	}
	// Match via SSH-style remote.
	remotes = map[string]string{"origin": "git@github.com:gmuxapp/gmux.git"}
	if m := s.Match("/any/dir", "", remotes); m == nil || m.Slug != "gmux" {
		t.Error("expected match on SSH remote")
	}
	// Match on upstream, not just origin.
	remotes = map[string]string{
		"origin":   "git@github.com:fork/gmux.git",
		"upstream": "https://github.com/gmuxapp/gmux",
	}
	if m := s.Match("/any/dir", "", remotes); m == nil || m.Slug != "gmux" {
		t.Error("expected match on upstream remote")
	}
	// No match.
	remotes = map[string]string{"origin": "https://github.com/other/repo"}
	if m := s.Match("/any/dir", "", remotes); m != nil {
		t.Errorf("expected no match, got %q", m.Slug)
	}
}

func TestMatchPathPrecedenceOverRemote(t *testing.T) {
	s := &State{Items: []Item{
		{Slug: "teak", Paths: []string{"/home/user/dev/gmux/.grove/teak"}},
		{Slug: "gmux", Remote: "github.com/gmuxapp/gmux", Paths: []string{"/home/user/dev/gmux"}},
	}}

	remotes := map[string]string{"origin": "git@github.com:gmuxapp/gmux.git"}

	// Session in teak directory with the gmux remote: path wins.
	m := s.Match("/home/user/dev/gmux/.grove/teak/src", "", remotes)
	if m == nil || m.Slug != "teak" {
		t.Errorf("expected teak (path precedence), got %v", m)
	}
	// Session in main gmux directory: only remote matches.
	m = s.Match("/home/user/dev/gmux/src", "", remotes)
	if m == nil || m.Slug != "gmux" {
		t.Errorf("expected gmux (remote), got %v", m)
	}
}

func TestMatchLongestPrefixWins(t *testing.T) {
	s := &State{Items: []Item{
		{Slug: "parent", Paths: []string{"/home/user/dev/gmux"}},
		{Slug: "child", Paths: []string{"/home/user/dev/gmux/.grove/teak"}},
	}}

	// Session in teak: child (longer prefix) wins.
	m := s.Match("/home/user/dev/gmux/.grove/teak/file.go", "", nil)
	if m == nil || m.Slug != "child" {
		t.Errorf("expected child (longest prefix), got %v", m)
	}
	// Session in gmux root: parent wins.
	m = s.Match("/home/user/dev/gmux/src/main.go", "", nil)
	if m == nil || m.Slug != "parent" {
		t.Errorf("expected parent, got %v", m)
	}
}

func TestMatchNoMatch(t *testing.T) {
	s := &State{Items: []Item{
		{Slug: "gmux", Remote: "github.com/gmuxapp/gmux", Paths: []string{"/home/user/dev/gmux"}},
		{Slug: "scripts", Paths: []string{"/home/user/scripts"}},
	}}
	m := s.Match("/home/user/dev/other", "", map[string]string{"origin": "github.com/other/repo"})
	if m != nil {
		t.Errorf("expected no match, got %q", m.Slug)
	}
}

func TestMatchEmptyState(t *testing.T) {
	s := &State{}
	if m := s.Match("/any/dir", "/any/ws", map[string]string{"o": "url"}); m != nil {
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
		{Slug: "gmux", Remote: "github.com/gmuxapp/gmux", Paths: []string{"/dev/gmux"}},
	}}

	sessions := []SessionInfo{
		// Matches gmux (should be excluded from discovered).
		{ID: "s1", Cwd: "/dev/gmux", Remotes: map[string]string{"origin": "git@github.com:gmuxapp/gmux.git"}},
		// Two sessions in the same project (shared remote).
		{ID: "s2", Cwd: "/dev/other", Remotes: map[string]string{"origin": "https://github.com/org/other.git"}},
		{ID: "s3", Cwd: "/dev/other-wt", Remotes: map[string]string{"origin": "https://github.com/org/other.git"}},
		// Standalone session (no remote, no workspace_root).
		{ID: "s4", Cwd: "/tmp/scratch"},
	}

	discovered := s.Discovered(sessions)
	if len(discovered) != 2 {
		t.Fatalf("expected 2 discovered groups, got %d: %+v", len(discovered), discovered)
	}

	// First group should be the one with 2 sessions (sorted by count).
	if discovered[0].SessionCount != 2 {
		t.Errorf("first group session_count = %d, want 2", discovered[0].SessionCount)
	}
	if discovered[0].Remote != "github.com/org/other" {
		t.Errorf("first group remote = %q, want %q", discovered[0].Remote, "github.com/org/other")
	}
	if discovered[0].SuggestedSlug != "other" {
		t.Errorf("first group slug = %q, want %q", discovered[0].SuggestedSlug, "other")
	}

	// Second group: standalone session.
	if discovered[1].SessionCount != 1 {
		t.Errorf("second group session_count = %d, want 1", discovered[1].SessionCount)
	}
	if discovered[1].SuggestedSlug != "scratch" {
		t.Errorf("second group slug = %q, want %q", discovered[1].SuggestedSlug, "scratch")
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

func TestDiscoveredEmpty(t *testing.T) {
	s := &State{}
	if d := s.Discovered(nil); d != nil {
		t.Errorf("expected nil, got %+v", d)
	}
}

func TestDiscoveredAllMatched(t *testing.T) {
	s := &State{Items: []Item{
		{Slug: "gmux", Paths: []string{"/dev/gmux"}},
	}}
	sessions := []SessionInfo{
		{ID: "s1", Cwd: "/dev/gmux/src"},
	}
	if d := s.Discovered(sessions); d != nil {
		t.Errorf("expected nil (all matched), got %+v", d)
	}
}
