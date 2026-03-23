// Package workspace detects VCS workspace roots for jj and git repositories.
// Used to group sessions that belong to the same repository.
package workspace

import (
	"os"
	"path/filepath"
	"strings"
)

// DetectRoot walks up from dir looking for jj or git repository markers and
// returns the workspace root. Returns "" if no VCS root is found.
//
// Detection order:
//  1. jj: look for .jj/ directory. If .jj/repo is a file (secondary
//     workspace), read it to find the main workspace root.
//  2. git: look for .git. If it's a file (worktree), read it to find the
//     main worktree root. If it's a directory, its parent is the root.
//  3. No VCS found: return "".
func DetectRoot(dir string) string {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}

	cur := dir
	for {
		// Check jj first (preferred when colocated with git).
		if root := checkJJ(cur); root != "" {
			return root
		}
		if root := checkGit(cur); root != "" {
			return root
		}

		parent := filepath.Dir(cur)
		if parent == cur {
			break // reached filesystem root
		}
		cur = parent
	}
	return ""
}

// checkJJ checks for a .jj directory at dir and resolves the workspace root.
//
// jj workspace layout:
//   - Main workspace: .jj/repo is a directory (the actual store).
//   - Secondary workspace: .jj/repo is a regular file containing a relative
//     path to the main workspace's .jj/repo directory (e.g. "../../../.jj/repo").
func checkJJ(dir string) string {
	jjDir := filepath.Join(dir, ".jj")
	info, err := os.Lstat(jjDir)
	if err != nil || !info.IsDir() {
		return ""
	}

	repoPath := filepath.Join(jjDir, "repo")
	repoInfo, err := os.Lstat(repoPath)
	if err != nil {
		// .jj exists but no repo entry; still a jj directory.
		return dir
	}

	if repoInfo.IsDir() {
		// Main workspace: .jj/repo is the store directory.
		return dir
	}

	// Secondary workspace: .jj/repo is a file containing a path to the
	// main workspace's .jj/repo. Read and resolve it.
	data, err := os.ReadFile(repoPath)
	if err != nil {
		return dir
	}
	target := strings.TrimSpace(string(data))
	if target == "" {
		return dir
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(jjDir, target)
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return dir
	}
	// target is something like /path/to/main-workspace/.jj/repo
	// The main workspace root is two levels up from the target.
	mainJJ := filepath.Dir(target)   // .jj
	mainRoot := filepath.Dir(mainJJ) // workspace root
	return mainRoot
}

// checkGit checks for a .git entry at dir and resolves the workspace root.
// Handles both regular repos (.git is a directory) and worktrees (.git is a
// file containing "gitdir: /path/to/.git/worktrees/<name>").
func checkGit(dir string) string {
	gitPath := filepath.Join(dir, ".git")
	info, err := os.Lstat(gitPath)
	if err != nil {
		return ""
	}

	if info.IsDir() {
		// Regular git repo: .git is a directory, dir is the root.
		return dir
	}

	// .git is a file (worktree marker). Read it to find the main repo.
	data, err := os.ReadFile(gitPath)
	if err != nil {
		return dir // can't read, fall back to this directory
	}

	line := strings.TrimSpace(string(data))
	if !strings.HasPrefix(line, "gitdir: ") {
		return dir
	}

	gitdir := strings.TrimPrefix(line, "gitdir: ")
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(dir, gitdir)
	}
	gitdir, err = filepath.Abs(gitdir)
	if err != nil {
		return dir
	}

	// gitdir is something like /path/to/main-repo/.git/worktrees/<name>
	// Walk up to find the main .git directory.
	// Standard layout: .git/worktrees/<name> → 2 levels up is .git
	mainGitDir := resolveMainGitDir(gitdir)
	if mainGitDir == "" {
		return dir
	}

	// The main repo root is the parent of .git/
	return filepath.Dir(mainGitDir)
}

// resolveMainGitDir walks up from a worktree gitdir path to find the main
// .git directory. Returns "" if the structure doesn't match expectations.
func resolveMainGitDir(gitdir string) string {
	// Typical: /repo/.git/worktrees/name → parent is "worktrees" → parent is ".git"
	// But also handle commondir: read commondir file if it exists.
	commondir := filepath.Join(gitdir, "commondir")
	data, err := os.ReadFile(commondir)
	if err == nil {
		target := strings.TrimSpace(string(data))
		if !filepath.IsAbs(target) {
			target = filepath.Join(gitdir, target)
		}
		target, err = filepath.Abs(target)
		if err == nil {
			return target
		}
	}

	// Fallback: walk up assuming standard .git/worktrees/<name> layout.
	parent := filepath.Dir(gitdir)           // .git/worktrees
	if filepath.Base(parent) == "worktrees" {
		return filepath.Dir(parent)          // .git
	}
	return ""
}
