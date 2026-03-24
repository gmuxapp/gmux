package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectRootJJSimple(t *testing.T) {
	// Simple jj repo: .jj/repo is a directory (main workspace)
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".jj", "repo"), 0o755)

	got := DetectRoot(root)
	if got != root {
		t.Errorf("expected %q, got %q", root, got)
	}
}

func TestDetectRootJJFromSubdir(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".jj", "repo"), 0o755)
	subdir := filepath.Join(root, "src", "pkg")
	os.MkdirAll(subdir, 0o755)

	got := DetectRoot(subdir)
	if got != root {
		t.Errorf("expected %q, got %q", root, got)
	}
}

func TestDetectRootJJWorkspace(t *testing.T) {
	// Main workspace at /main/.jj/repo/ (a real directory)
	// Secondary workspace at /secondary/.jj/repo (a file containing relative path)
	dir := t.TempDir()
	mainRoot := filepath.Join(dir, "main")
	secondaryRoot := filepath.Join(dir, "secondary")

	os.MkdirAll(filepath.Join(mainRoot, ".jj", "repo"), 0o755)
	os.MkdirAll(filepath.Join(secondaryRoot, ".jj"), 0o755)

	// jj writes a relative path in the repo file for secondary workspaces
	relPath, _ := filepath.Rel(filepath.Join(secondaryRoot, ".jj"), filepath.Join(mainRoot, ".jj", "repo"))
	os.WriteFile(
		filepath.Join(secondaryRoot, ".jj", "repo"),
		[]byte(relPath),
		0o644,
	)

	got := DetectRoot(secondaryRoot)
	if got != mainRoot {
		t.Errorf("expected main root %q, got %q", mainRoot, got)
	}

	// Main workspace detects itself
	got = DetectRoot(mainRoot)
	if got != mainRoot {
		t.Errorf("expected main root %q, got %q", mainRoot, got)
	}
}

func TestDetectRootGitSimple(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".git"), 0o755)

	got := DetectRoot(root)
	if got != root {
		t.Errorf("expected %q, got %q", root, got)
	}
}

func TestDetectRootGitFromSubdir(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".git"), 0o755)
	subdir := filepath.Join(root, "src", "pkg")
	os.MkdirAll(subdir, 0o755)

	got := DetectRoot(subdir)
	if got != root {
		t.Errorf("expected %q, got %q", root, got)
	}
}

func TestDetectRootGitWorktree(t *testing.T) {
	// Main repo at /main/.git/ (directory)
	// Worktree at /worktree/.git (file pointing to main/.git/worktrees/wt)
	dir := t.TempDir()
	mainRoot := filepath.Join(dir, "main")
	worktreeRoot := filepath.Join(dir, "worktree")

	mainGit := filepath.Join(mainRoot, ".git")
	os.MkdirAll(filepath.Join(mainGit, "worktrees", "wt"), 0o755)
	os.MkdirAll(worktreeRoot, 0o755)

	// Write the commondir file (how git worktrees reference the main repo)
	wtGitdir := filepath.Join(mainGit, "worktrees", "wt")
	os.WriteFile(
		filepath.Join(wtGitdir, "commondir"),
		[]byte("../..\n"),
		0o644,
	)

	// Write .git file in worktree
	os.WriteFile(
		filepath.Join(worktreeRoot, ".git"),
		[]byte("gitdir: "+wtGitdir+"\n"),
		0o644,
	)

	got := DetectRoot(worktreeRoot)
	if got != mainRoot {
		t.Errorf("expected main root %q, got %q", mainRoot, got)
	}
}

func TestDetectRootGitWorktreeFallbackLayout(t *testing.T) {
	// Worktree without commondir file, relying on directory structure
	dir := t.TempDir()
	mainRoot := filepath.Join(dir, "main")
	worktreeRoot := filepath.Join(dir, "worktree")

	mainGit := filepath.Join(mainRoot, ".git")
	wtGitdir := filepath.Join(mainGit, "worktrees", "wt")
	os.MkdirAll(wtGitdir, 0o755)
	os.MkdirAll(worktreeRoot, 0o755)

	os.WriteFile(
		filepath.Join(worktreeRoot, ".git"),
		[]byte("gitdir: "+wtGitdir+"\n"),
		0o644,
	)

	got := DetectRoot(worktreeRoot)
	if got != mainRoot {
		t.Errorf("expected main root %q, got %q", mainRoot, got)
	}
}

func TestDetectRootJJPreferredOverGit(t *testing.T) {
	// Colocated: both .jj and .git in the same directory
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".jj", "repo"), 0o755)
	os.MkdirAll(filepath.Join(root, ".git"), 0o755)

	got := DetectRoot(root)
	if got != root {
		t.Errorf("expected %q, got %q", root, got)
	}
	// Both point to the same root, so the result is the same.
	// The important thing is that jj is checked first (no error).
}

func TestDetectRootJJNestedWorkspace(t *testing.T) {
	// Mimics the gmux repo layout:
	// /repo/.jj/repo/ (directory, main workspace)
	// /repo/.grove/teak/.jj/repo (file: "../../../.jj/repo")
	dir := t.TempDir()
	mainRoot := dir
	teakRoot := filepath.Join(dir, ".grove", "teak")

	os.MkdirAll(filepath.Join(mainRoot, ".jj", "repo"), 0o755)
	os.MkdirAll(filepath.Join(teakRoot, ".jj"), 0o755)
	os.WriteFile(
		filepath.Join(teakRoot, ".jj", "repo"),
		[]byte("../../../.jj/repo"),
		0o644,
	)

	// Secondary workspace resolves to main
	got := DetectRoot(teakRoot)
	if got != mainRoot {
		t.Errorf("expected %q, got %q", mainRoot, got)
	}

	// Subdir of secondary workspace also resolves to main
	subdir := filepath.Join(teakRoot, "packages", "adapter")
	os.MkdirAll(subdir, 0o755)
	got = DetectRoot(subdir)
	if got != mainRoot {
		t.Errorf("from subdir: expected %q, got %q", mainRoot, got)
	}
}

func TestDetectRootNoVCS(t *testing.T) {
	dir := t.TempDir()

	got := DetectRoot(dir)
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestDetectRootEmptyString(t *testing.T) {
	// Empty string should not panic
	got := DetectRoot("")
	// Result depends on os.Getwd, just verify no panic
	_ = got
}
