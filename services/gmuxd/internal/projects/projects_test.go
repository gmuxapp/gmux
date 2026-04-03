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

func TestMatchRemoteProjectFallsBackToPath(t *testing.T) {
	s := &State{Items: []Item{
		{Slug: "gmux", Remote: "github.com/gmuxapp/gmux", Paths: []string{"/home/user/dev/gmux"}},
	}}

	// Session has no remotes (e.g. new git init that hasn't pushed).
	// Should still match via the project's paths as a fallback.
	if m := s.Match("/home/user/dev/gmux/src", "", nil); m == nil || m.Slug != "gmux" {
		t.Error("expected remote project to fall back to path match")
	}
	// Session outside the project directory with no remotes: no match.
	if m := s.Match("/home/user/dev/other", "", nil); m != nil {
		t.Errorf("expected no match, got %q", m.Slug)
	}
}

func TestMatchPathProjectStillTakesPrecedenceOverRemoteFallback(t *testing.T) {
	s := &State{Items: []Item{
		{Slug: "teak", Paths: []string{"/home/user/dev/gmux/.grove/teak"}},
		{Slug: "gmux", Remote: "github.com/gmuxapp/gmux", Paths: []string{"/home/user/dev/gmux"}},
	}}

	// Session in teak directory with no remotes.
	// Path-matched project (teak) should win over remote project's path fallback (gmux).
	m := s.Match("/home/user/dev/gmux/.grove/teak/src", "", nil)
	if m == nil || m.Slug != "teak" {
		t.Errorf("expected teak (path precedence), got %v", m)
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

// --- Session membership ---

func TestAddSession(t *testing.T) {
	s := State{Items: []Item{
		{Slug: "gmux", Paths: []string{"/dev/gmux"}},
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
		{Slug: "gmux", Paths: []string{"/dev/gmux"}, Sessions: []string{"a", "b", "c"}},
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

func TestRemoveSessionFromAll(t *testing.T) {
	s := State{Items: []Item{
		{Slug: "gmux", Paths: []string{"/dev/gmux"}, Sessions: []string{"a"}},
		{Slug: "yapp", Paths: []string{"/dev/yapp"}, Sessions: []string{"b", "c"}},
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
		{Slug: "gmux", Paths: []string{"/dev/gmux"}, Sessions: []string{"a"}},
		{Slug: "yapp", Paths: []string{"/dev/yapp"}, Sessions: []string{"b"}},
	}}
	if slug := s.FindSessionProject("b"); slug != "yapp" {
		t.Errorf("expected 'yapp', got %q", slug)
	}
	if slug := s.FindSessionProject("nonexistent"); slug != "" {
		t.Errorf("expected empty, got %q", slug)
	}
}

func TestSessionKey(t *testing.T) {
	if key := SessionKey("sess-123", "resume-abc"); key != "resume-abc" {
		t.Errorf("expected resume key, got %q", key)
	}
	if key := SessionKey("sess-123", ""); key != "sess-123" {
		t.Errorf("expected session ID, got %q", key)
	}
}

func TestDiscoveredActiveCount(t *testing.T) {
	s := State{Items: []Item{
		{Slug: "gmux", Remote: "github.com/gmuxapp/gmux", Paths: []string{"/dev/gmux"}},
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
			{Slug: "gmux", Remote: "github.com/gmuxapp/gmux", Paths: []string{"/dev/gmux"}},
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
		Alive: true,
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

func TestManagerAutoAssignResumeKeyUpgrade(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)

	mgr.Update(func(s *State) bool {
		s.Items = []Item{
			{Slug: "gmux", Paths: []string{"/dev/gmux"}},
		}
		return true
	})

	// Session initially registered by ID (no resume key yet).
	mgr.AutoAssignSession(SessionInfo{
		ID: "sess-1", Cwd: "/dev/gmux", Alive: true,
	})

	state, _ := mgr.Load()
	if state.Items[0].Sessions[0] != "sess-1" {
		t.Fatalf("expected sess-1, got %v", state.Items[0].Sessions)
	}

	// Session gets a resume key (file attributed).
	mgr.AutoAssignSession(SessionInfo{
		ID: "sess-1", Cwd: "/dev/gmux", Alive: true,
		ResumeKey: "2026-04-03_abc123.jsonl",
	})

	state, _ = mgr.Load()
	if len(state.Items[0].Sessions) != 1 {
		t.Fatalf("expected 1 session after upgrade, got %d", len(state.Items[0].Sessions))
	}
	if state.Items[0].Sessions[0] != "2026-04-03_abc123.jsonl" {
		t.Errorf("expected resume key, got %q", state.Items[0].Sessions[0])
	}
}

func TestManagerDismissSession(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)

	mgr.Update(func(s *State) bool {
		s.Items = []Item{
			{Slug: "gmux", Paths: []string{"/dev/gmux"}, Sessions: []string{"key-1", "key-2"}},
		}
		return true
	})

	slug := mgr.DismissSession("sess-x", "key-1")
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
			{Slug: "gmux", Paths: []string{"/dev/gmux"}},
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

// --- UnmatchedActiveCount ---

func TestUnmatchedActiveCount(t *testing.T) {
	s := State{Items: []Item{
		{Slug: "gmux", Paths: []string{"/dev/gmux"}, Sessions: []string{"s1"}},
	}}
	sessions := []SessionInfo{
		{ID: "s1", Cwd: "/dev/gmux", Alive: true},       // matched + in array
		{ID: "s2", Cwd: "/dev/gmux", Alive: true},        // matched but not in array
		{ID: "s3", Cwd: "/somewhere/else", Alive: true},   // unmatched + alive
		{ID: "s4", Cwd: "/somewhere/else", Alive: false},  // unmatched + dead
		{ID: "s5", Cwd: "/another/place", Alive: true},    // unmatched + alive
	}
	count := s.UnmatchedActiveCount(sessions)
	// s3 and s5 are unmatched+alive. s2 matches gmux (not counted). s4 is dead.
	if count != 2 {
		t.Errorf("expected 2, got %d", count)
	}
}

func TestUnmatchedActiveCountAllMatched(t *testing.T) {
	s := State{Items: []Item{
		{Slug: "gmux", Paths: []string{"/dev/gmux"}},
	}}
	sessions := []SessionInfo{
		{ID: "s1", Cwd: "/dev/gmux/src", Alive: true},
	}
	if count := s.UnmatchedActiveCount(sessions); count != 0 {
		t.Errorf("expected 0, got %d", count)
	}
}

// --- DismissSession with ID fallback ---

func TestManagerDismissSessionByIDFallback(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)

	// Array stores session by ID (no resume key at assignment time).
	mgr.Update(func(s *State) bool {
		s.Items = []Item{
			{Slug: "gmux", Paths: []string{"/dev/gmux"}, Sessions: []string{"sess-1"}},
		}
		return true
	})

	// Dismiss provides resume key, but array has the session by ID.
	// DismissSession should fall back to trying the ID.
	slug := mgr.DismissSession("sess-1", "resume-key-abc")
	if slug != "gmux" {
		t.Errorf("expected 'gmux', got %q", slug)
	}

	state, _ := mgr.Load()
	if len(state.Items[0].Sessions) != 0 {
		t.Errorf("expected empty sessions, got %v", state.Items[0].Sessions)
	}
}

// --- Manager.Broadcast is called on mutations ---

func TestManagerBroadcastCalled(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)
	called := 0
	mgr.Broadcast = func() { called++ }

	mgr.Update(func(s *State) bool {
		s.Items = []Item{{Slug: "test", Paths: []string{"/test"}}}
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

func TestManagerAutoAssignAllAlive(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)

	mgr.Update(func(s *State) bool {
		s.Items = []Item{
			{Slug: "gmux", Paths: []string{"/dev/gmux"}},
			{Slug: "yapp", Paths: []string{"/dev/yapp"}},
		}
		return true
	})

	sessions := []SessionInfo{
		{ID: "s1", Cwd: "/dev/gmux/src", Alive: true},
		{ID: "s2", Cwd: "/dev/yapp", Alive: true},
		{ID: "s3", Cwd: "/dev/gmux", Alive: false}, // dead: should be skipped
		{ID: "s4", Cwd: "/other", Alive: true},      // unmatched: should be skipped
	}

	mgr.AutoAssignAllAlive(sessions)

	state, _ := mgr.Load()
	if len(state.Items[0].Sessions) != 1 || state.Items[0].Sessions[0] != "s1" {
		t.Errorf("gmux sessions: expected [s1], got %v", state.Items[0].Sessions)
	}
	if len(state.Items[1].Sessions) != 1 || state.Items[1].Sessions[0] != "s2" {
		t.Errorf("yapp sessions: expected [s2], got %v", state.Items[1].Sessions)
	}
}
