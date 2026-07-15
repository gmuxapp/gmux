package projectmatch

import (
	"os"
	"testing"
)

func TestMatchCharacterization(t *testing.T) {
	entries := []Entry{
		{Rules: []Rule{{Remote: "git@github.com:acme/repo.git"}}},
		{Reference: true, Rules: []Rule{{Path: "/work/reference"}, {Remote: "github.com/acme/reference"}}},
		{Rules: []Rule{{Path: "/work"}}},
		{Rules: []Rule{{Path: "/work/repo"}}},
		{Rules: []Rule{{Path: "/exact", Exact: true}}},
		{Rules: []Rule{{Remote: "github.com/acme/repo"}}},
		{Rules: []Rule{{Path: "/dual/path", Remote: "github.com/acme/dual"}}},
	}

	tests := []struct {
		name string
		in   Inputs
		want int
		ok   bool
	}{
		{name: "equal path", in: Inputs{CWD: "/work/repo"}, want: 3, ok: true},
		{name: "descendant path", in: Inputs{CWD: "/work/repo/src"}, want: 3, ok: true},
		{name: "workspace path", in: Inputs{CWD: "/tmp", WorkspaceRoot: "/work/repo/pkg"}, want: 3, ok: true},
		{name: "path boundary", in: Inputs{CWD: "/workbench"}, want: -1},
		{name: "exact cwd", in: Inputs{CWD: "/exact"}, want: 4, ok: true},
		{name: "exact workspace", in: Inputs{WorkspaceRoot: "/exact"}, want: 4, ok: true},
		{name: "exact rejects descendant", in: Inputs{CWD: "/exact/child"}, want: -1},
		{name: "path beats earlier remote", in: Inputs{CWD: "/work/repo", Remotes: map[string]string{"origin": "https://github.com/acme/repo.git"}}, want: 3, ok: true},
		{name: "first remote wins", in: Inputs{CWD: "/elsewhere", Remotes: map[string]string{"origin": "ssh://git@github.com/acme/repo/"}}, want: 0, ok: true},
		{name: "reference path ignored", in: Inputs{CWD: "/work/reference"}, want: 2, ok: true},
		{name: "reference remote ignored", in: Inputs{Remotes: map[string]string{"origin": "github.com/acme/reference"}}, want: -1},
		{name: "dual-arm rule remote matches when path does not", in: Inputs{CWD: "/elsewhere", Remotes: map[string]string{"origin": "https://github.com/acme/dual.git"}}, want: 6, ok: true},
		{name: "empty", in: Inputs{}, want: -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Match(entries, tt.in)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("Match() = (%d, %v), want (%d, %v)", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestMatchEqualLengthPathKeepsFirstEntry(t *testing.T) {
	entries := []Entry{
		{Rules: []Rule{{Path: "/one"}}},
		{Rules: []Rule{{Path: "/one"}}},
	}
	got, ok := Match(entries, Inputs{CWD: "/one/child"})
	if !ok || got != 0 {
		t.Fatalf("Match() = (%d, %v), want first entry", got, ok)
	}
}

func TestMatchPathRuleOrderDoesNotOverrideLongerMatch(t *testing.T) {
	entries := []Entry{{Rules: []Rule{{Path: "/repo/deep"}, {Path: "/repo"}}}}
	got, ok := Match(entries, Inputs{CWD: "/repo/deep/file"})
	if !ok || got != 0 {
		t.Fatalf("Match() = (%d, %v), want entry 0", got, ok)
	}
}

func TestNormalizeRemoteCharacterization(t *testing.T) {
	tests := map[string]string{
		"https://github.com/org/repo.git":  "github.com/org/repo",
		"http://github.com/org/repo/":      "github.com/org/repo",
		"ssh://git@github.com/org/repo":    "github.com/org/repo",
		"git://github.com/org/repo.git":    "github.com/org/repo",
		"git@github.com:org/repo.git":      "github.com/org/repo",
		"ssh://git@example.com:8080/repo":  "example.com/8080/repo",
		"example.com:8080/repo.git":        "example.com/8080/repo",
		"user@example.com:org/repo.git///": "example.com/org/repo.git",
	}
	for input, want := range tests {
		if got := NormalizeRemote(input); got != want {
			t.Errorf("NormalizeRemote(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestNormalizePathCharacterization(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got := NormalizePath("~/repo/../project/"); got != home+"/project" {
		t.Fatalf("NormalizePath() = %q, want %q", got, home+"/project")
	}
	if got := NormalizePath(""); got != "" {
		t.Fatalf("NormalizePath(empty) = %q", got)
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatal(err)
	}
}
