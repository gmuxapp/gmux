package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeRemoteURL(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		// SCP-style
		{"git@github.com:gmuxapp/gmux.git", "github.com/gmuxapp/gmux"},
		{"git@github.com:gmuxapp/gmux", "github.com/gmuxapp/gmux"},
		{"git@gitlab.com:org/sub/repo.git", "gitlab.com/org/sub/repo"},

		// HTTPS
		{"https://github.com/gmuxapp/gmux.git", "github.com/gmuxapp/gmux"},
		{"https://github.com/gmuxapp/gmux", "github.com/gmuxapp/gmux"},
		{"https://gitlab.com/org/sub/repo.git", "gitlab.com/org/sub/repo"},

		// SSH with scheme
		{"ssh://git@github.com/gmuxapp/gmux.git", "github.com/gmuxapp/gmux"},
		{"ssh://git@github.com/gmuxapp/gmux", "github.com/gmuxapp/gmux"},

		// Git protocol
		{"git://github.com/gmuxapp/gmux.git", "github.com/gmuxapp/gmux"},

		// HTTP (some self-hosted)
		{"http://gitea.local/org/repo.git", "gitea.local/org/repo"},

		// Trailing slash
		{"https://github.com/gmuxapp/gmux/", "github.com/gmuxapp/gmux"},

		// Edge cases
		{"", ""},
		{" ", ""},
		{"/local/path", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := NormalizeRemoteURL(tt.input)
			if got != tt.want {
				t.Errorf("NormalizeRemoteURL(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestNormalizeRemoteURLSymmetry(t *testing.T) {
	// The same repo accessed via different protocols must normalize to the same value.
	// This is the core property that makes "any shared remote" grouping work.
	urls := []string{
		"git@github.com:gmuxapp/gmux.git",
		"https://github.com/gmuxapp/gmux.git",
		"ssh://git@github.com/gmuxapp/gmux.git",
		"git://github.com/gmuxapp/gmux.git",
		"https://github.com/gmuxapp/gmux",
	}
	want := "github.com/gmuxapp/gmux"
	for _, u := range urls {
		got := NormalizeRemoteURL(u)
		if got != want {
			t.Errorf("NormalizeRemoteURL(%q) = %q, want %q", u, got, want)
		}
	}
}

func TestParseGitConfigRemotes(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config")
	os.WriteFile(cfg, []byte(`[core]
	repositoryformatversion = 0
	bare = false
[remote "origin"]
	url = https://github.com/gmuxapp/gmux.git
	fetch = +refs/heads/*:refs/remotes/origin/*
[remote "upstream"]
	url = git@github.com:other/gmux.git
	fetch = +refs/heads/*:refs/remotes/upstream/*
[branch "main"]
	remote = origin
	merge = refs/heads/main
`), 0o644)

	got := parseGitConfigRemotes(cfg)
	if len(got) != 2 {
		t.Fatalf("expected 2 remotes, got %d: %v", len(got), got)
	}
	if got["origin"] != "https://github.com/gmuxapp/gmux.git" {
		t.Errorf("origin = %q", got["origin"])
	}
	if got["upstream"] != "git@github.com:other/gmux.git" {
		t.Errorf("upstream = %q", got["upstream"])
	}
}

func TestDetectRemotesGitRepo(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	os.MkdirAll(gitDir, 0o755)
	os.WriteFile(filepath.Join(gitDir, "config"), []byte(`[remote "origin"]
	url = https://github.com/gmuxapp/gmux.git
`), 0o644)

	remotes := DetectRemotes(root)
	if remotes == nil {
		t.Fatal("expected remotes, got nil")
	}
	if remotes["origin"] != "github.com/gmuxapp/gmux" {
		t.Errorf("origin = %q, want %q", remotes["origin"], "github.com/gmuxapp/gmux")
	}
}

func TestDetectRemotesJJColocated(t *testing.T) {
	// Colocated jj repo: .jj/repo/store/git_target points to .git
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".jj", "repo", "store"), 0o755)
	os.MkdirAll(filepath.Join(root, ".git"), 0o755)

	os.WriteFile(filepath.Join(root, ".jj", "repo", "store", "git_target"), []byte("../../../.git"), 0o644)
	os.WriteFile(filepath.Join(root, ".git", "config"), []byte(`[remote "origin"]
	url = git@github.com:gmuxapp/gmux.git
`), 0o644)

	remotes := DetectRemotes(root)
	if remotes == nil {
		t.Fatal("expected remotes, got nil")
	}
	if remotes["origin"] != "github.com/gmuxapp/gmux" {
		t.Errorf("origin = %q", remotes["origin"])
	}
}

func TestDetectRemotesJJNonColocated(t *testing.T) {
	// Non-colocated jj repo: .jj/repo/store/git_target points to internal git dir
	root := t.TempDir()
	gitDir := filepath.Join(root, ".jj", "repo", "store", "git")
	os.MkdirAll(gitDir, 0o755)
	os.MkdirAll(filepath.Join(root, ".jj", "repo", "store"), 0o755)

	os.WriteFile(filepath.Join(root, ".jj", "repo", "store", "git_target"), []byte("git"), 0o644)
	os.WriteFile(filepath.Join(gitDir, "config"), []byte(`[remote "origin"]
	url = https://github.com/gmuxapp/gmux.git
`), 0o644)

	remotes := DetectRemotes(root)
	if remotes == nil {
		t.Fatal("expected remotes, got nil")
	}
	if remotes["origin"] != "github.com/gmuxapp/gmux" {
		t.Errorf("origin = %q", remotes["origin"])
	}
}

func TestDetectRemotesWorktree(t *testing.T) {
	// Worktree: .git is a file pointing to main repo's .git/worktrees/wt
	dir := t.TempDir()
	mainRoot := filepath.Join(dir, "main")
	worktreeRoot := filepath.Join(dir, "worktree")

	mainGit := filepath.Join(mainRoot, ".git")
	wtGitdir := filepath.Join(mainGit, "worktrees", "wt")
	os.MkdirAll(wtGitdir, 0o755)
	os.MkdirAll(worktreeRoot, 0o755)

	os.WriteFile(filepath.Join(mainGit, "config"), []byte(`[remote "origin"]
	url = https://github.com/gmuxapp/gmux.git
`), 0o644)
	os.WriteFile(filepath.Join(wtGitdir, "commondir"), []byte("../..\n"), 0o644)
	os.WriteFile(filepath.Join(worktreeRoot, ".git"), []byte("gitdir: "+wtGitdir+"\n"), 0o644)

	remotes := DetectRemotes(worktreeRoot)
	if remotes == nil {
		t.Fatal("expected remotes, got nil")
	}
	if remotes["origin"] != "github.com/gmuxapp/gmux" {
		t.Errorf("origin = %q", remotes["origin"])
	}
}

func TestDetectRemotesNoVCS(t *testing.T) {
	root := t.TempDir()
	remotes := DetectRemotes(root)
	if remotes != nil {
		t.Errorf("expected nil, got %v", remotes)
	}
}

func TestDetectRemotesNoRemotes(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".git"), 0o755)
	os.WriteFile(filepath.Join(root, ".git", "config"), []byte(`[core]
	bare = false
`), 0o644)

	remotes := DetectRemotes(root)
	if remotes != nil {
		t.Errorf("expected nil for repo with no remotes, got %v", remotes)
	}
}

func TestDetectRemotesEmpty(t *testing.T) {
	remotes := DetectRemotes("")
	if remotes != nil {
		t.Errorf("expected nil for empty path, got %v", remotes)
	}
}

func TestDetectRemotesMultiple(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".git"), 0o755)
	os.WriteFile(filepath.Join(root, ".git", "config"), []byte(`[remote "origin"]
	url = git@github.com:mgabor3141/gmux.git
[remote "upstream"]
	url = git@github.com:gmuxapp/gmux.git
`), 0o644)

	remotes := DetectRemotes(root)
	if len(remotes) != 2 {
		t.Fatalf("expected 2 remotes, got %d: %v", len(remotes), remotes)
	}
	if remotes["origin"] != "github.com/mgabor3141/gmux" {
		t.Errorf("origin = %q", remotes["origin"])
	}
	if remotes["upstream"] != "github.com/gmuxapp/gmux" {
		t.Errorf("upstream = %q", remotes["upstream"])
	}
}
