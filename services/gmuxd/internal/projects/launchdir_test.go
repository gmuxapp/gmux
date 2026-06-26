package projects

import "testing"

func TestItemCanonicalDir(t *testing.T) {
	tests := []struct {
		name string
		item *Item
		want string
	}{
		{"nil", nil, ""},
		{"no rules", &Item{Slug: "p"}, ""},
		{"first path rule", &Item{Slug: "p", Match: []MatchRule{{Path: "/dev/proj"}}}, "/dev/proj"},
		{"remote then path", &Item{Slug: "p", Match: []MatchRule{
			{Remote: "github.com/o/r"}, {Path: "/dev/proj"},
		}}, "/dev/proj"},
		{"first of several paths", &Item{Slug: "p", Match: []MatchRule{
			{Path: "/dev/a"}, {Path: "/dev/b"},
		}}, "/dev/a"},
		{"reference ignored", &Item{Slug: "p", Peer: "host", Match: []MatchRule{{Path: "/dev/proj"}}}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.item.CanonicalDir(); got != tc.want {
				t.Errorf("CanonicalDir() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStateProjectBySlug(t *testing.T) {
	s := &State{Items: []Item{
		{Slug: "owned", Match: []MatchRule{{Path: "/dev/owned"}}},
		{Slug: "ref", Peer: "host"},
	}}
	if got := s.ProjectBySlug("owned"); got == nil || got.Slug != "owned" {
		t.Errorf("expected owned project, got %v", got)
	}
	if got := s.ProjectBySlug("ref"); got != nil {
		t.Errorf("reference should not resolve, got %v", got)
	}
	if got := s.ProjectBySlug("missing"); got != nil {
		t.Errorf("missing slug should be nil, got %v", got)
	}
	if got := s.ProjectBySlug(""); got != nil {
		t.Errorf("empty slug should be nil, got %v", got)
	}
}

func TestCanonicalDirForSession(t *testing.T) {
	s := &State{Items: []Item{
		{Slug: "proj", Match: []MatchRule{{Path: "/dev/proj"}}},
	}}

	// By assigned slug.
	if got := s.CanonicalDirForSession("proj", MatchParams{}); got != "/dev/proj" {
		t.Errorf("by slug = %q, want /dev/proj", got)
	}
	// Slug empty: fall back to matching the (deleted) cwd against rules.
	if got := s.CanonicalDirForSession("", MatchParams{Cwd: "/dev/proj/gone"}); got != "/dev/proj" {
		t.Errorf("by cwd match = %q, want /dev/proj", got)
	}
	// No owning project at all.
	if got := s.CanonicalDirForSession("", MatchParams{Cwd: "/elsewhere"}); got != "" {
		t.Errorf("unmatched = %q, want empty", got)
	}
}

// The assigned slug is authoritative: a session must resume in its own
// project's dir, never get relocated into a different project whose
// path rule happens to match the (deleted) cwd.
func TestCanonicalDirForSession_SlugBeatsCwdMatch(t *testing.T) {
	s := &State{Items: []Item{
		{Slug: "home", Match: []MatchRule{{Path: "/dev/proj"}}},
		{Slug: "proj", Match: []MatchRule{{Path: "/dev/proj/sub"}}},
	}}
	// Session is stamped to "home" but its cwd sits under proj's rule.
	got := s.CanonicalDirForSession("home", MatchParams{Cwd: "/dev/proj/sub/gone"})
	if got != "/dev/proj" {
		t.Errorf("got %q, want /dev/proj (assigned slug wins over cwd match)", got)
	}
}

// A slug-assigned project with only a remote rule has no canonical
// local folder: return empty so the caller falls through to $HOME
// rather than silently borrowing some other project's path.
func TestCanonicalDirForSession_RemoteOnlyProjectYieldsEmpty(t *testing.T) {
	s := &State{Items: []Item{
		{Slug: "remote", Match: []MatchRule{{Remote: "github.com/o/r"}}},
		{Slug: "other", Match: []MatchRule{{Path: "/dev/other"}}},
	}}
	got := s.CanonicalDirForSession("remote", MatchParams{Cwd: "/dev/other/gone"})
	if got != "" {
		t.Errorf("got %q, want empty (remote-only project has no canonical local dir)", got)
	}
}

func TestResolveLaunchDir(t *testing.T) {
	exists := map[string]bool{"/cwd": true, "/canon": true, "/home": true}
	isDir := func(p string) bool { return exists[p] }

	tests := []struct {
		name       string
		candidates []string
		wantDir    string
		wantIdx    int
	}{
		{"cwd exists", []string{"/cwd", "/canon", "/home"}, "/cwd", 0},
		{"cwd gone, canonical", []string{"/gone", "/canon", "/home"}, "/canon", 1},
		{"cwd+canon gone, home", []string{"/gone", "/gone2", "/home"}, "/home", 2},
		{"skip empties", []string{"", "", "/home"}, "/home", 2},
		{"none", []string{"/gone", "", "/x"}, "", -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir, idx := ResolveLaunchDir(isDir, tc.candidates...)
			if dir != tc.wantDir || idx != tc.wantIdx {
				t.Errorf("ResolveLaunchDir = (%q, %d), want (%q, %d)", dir, idx, tc.wantDir, tc.wantIdx)
			}
		})
	}
}
