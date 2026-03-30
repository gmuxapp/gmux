package workspace

import (
	"bufio"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// DetectRemotes reads git remote URLs from the repository at workspaceRoot.
// Returns a map of remote name to normalized URL (e.g. "github.com/org/repo").
// Returns nil if no VCS root or no remotes are found.
//
// Handles:
//   - git repos (.git/config)
//   - colocated jj repos (.jj/repo/store/git_target -> .git/config)
//   - non-colocated jj repos (.jj/repo/store/git_target -> .jj/repo/store/git/config)
//
// No subprocesses are used; config files are read directly.
func DetectRemotes(workspaceRoot string) map[string]string {
	if workspaceRoot == "" {
		return nil
	}

	gitConfigPath := findGitConfig(workspaceRoot)
	if gitConfigPath == "" {
		return nil
	}

	raw := parseGitConfigRemotes(gitConfigPath)
	if len(raw) == 0 {
		return nil
	}

	remotes := make(map[string]string, len(raw))
	for name, rawURL := range raw {
		if norm := NormalizeRemoteURL(rawURL); norm != "" {
			remotes[name] = norm
		}
	}
	if len(remotes) == 0 {
		return nil
	}
	return remotes
}

// findGitConfig locates the git config file for a workspace root.
func findGitConfig(root string) string {
	// Try jj first: .jj/repo/store/git_target tells us where the git dir is.
	gitTarget := filepath.Join(root, ".jj", "repo", "store", "git_target")
	if data, err := os.ReadFile(gitTarget); err == nil {
		target := strings.TrimSpace(string(data))
		if !filepath.IsAbs(target) {
			target = filepath.Join(root, ".jj", "repo", "store", target)
		}
		target, _ = filepath.Abs(target)
		cfg := filepath.Join(target, "config")
		if _, err := os.Stat(cfg); err == nil {
			return cfg
		}
	}

	// Try .git/config (regular git repo, or colocated jj where git_target
	// pointed to .git which we already checked above).
	gitDir := filepath.Join(root, ".git")
	info, err := os.Lstat(gitDir)
	if err != nil {
		return ""
	}
	if info.IsDir() {
		cfg := filepath.Join(gitDir, "config")
		if _, err := os.Stat(cfg); err == nil {
			return cfg
		}
		return ""
	}

	// .git is a file (worktree). Read the gitdir and find config there.
	data, err := os.ReadFile(gitDir)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(data))
	if !strings.HasPrefix(line, "gitdir: ") {
		return ""
	}
	gitdir := strings.TrimPrefix(line, "gitdir: ")
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(root, gitdir)
	}
	gitdir, _ = filepath.Abs(gitdir)

	// Worktree gitdirs don't have their own remote config.
	// Follow commondir to the main git dir.
	commondir := filepath.Join(gitdir, "commondir")
	if data, err := os.ReadFile(commondir); err == nil {
		target := strings.TrimSpace(string(data))
		if !filepath.IsAbs(target) {
			target = filepath.Join(gitdir, target)
		}
		target, _ = filepath.Abs(target)
		cfg := filepath.Join(target, "config")
		if _, err := os.Stat(cfg); err == nil {
			return cfg
		}
	}

	// Fallback: assume .git/worktrees/<name> layout.
	parent := filepath.Dir(gitdir)
	if filepath.Base(parent) == "worktrees" {
		mainGit := filepath.Dir(parent)
		cfg := filepath.Join(mainGit, "config")
		if _, err := os.Stat(cfg); err == nil {
			return cfg
		}
	}

	return ""
}

// parseGitConfigRemotes extracts remote name/URL pairs from a git config file.
// Parses only [remote "name"] sections and their url = ... lines.
func parseGitConfigRemotes(path string) map[string]string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	remotes := make(map[string]string)
	var currentRemote string

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Section header: [remote "origin"]
		if strings.HasPrefix(line, "[") {
			currentRemote = ""
			if name := parseRemoteSection(line); name != "" {
				currentRemote = name
			}
			continue
		}

		// Key-value inside a remote section
		if currentRemote != "" && strings.HasPrefix(line, "url") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 && strings.TrimSpace(parts[0]) == "url" {
				remotes[currentRemote] = strings.TrimSpace(parts[1])
			}
		}
	}
	return remotes
}

// remoteRe matches [remote "name"] section headers.
var remoteRe = regexp.MustCompile(`^\[remote\s+"([^"]+)"\]`)

func parseRemoteSection(line string) string {
	m := remoteRe.FindStringSubmatch(line)
	if m == nil {
		return ""
	}
	return m[1]
}

// NormalizeRemoteURL converts a git remote URL to a canonical form
// suitable for equality comparison: "host/owner/repo" with no scheme,
// no auth, no trailing .git.
//
// Handles:
//   - SCP style:  git@github.com:org/repo.git
//   - HTTPS:      https://github.com/org/repo.git
//   - SSH:        ssh://git@github.com/org/repo
//   - Git:        git://github.com/org/repo.git
//
// Returns "" for URLs that can't be normalized.
func NormalizeRemoteURL(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return ""
	}

	var host, path string

	// SCP-style: user@host:path (no scheme, has colon but no //)
	if isSCPStyle(rawURL) {
		idx := strings.Index(rawURL, ":")
		hostPart := rawURL[:idx]
		path = rawURL[idx+1:]
		// Strip user@ from host
		if at := strings.LastIndex(hostPart, "@"); at >= 0 {
			host = hostPart[at+1:]
		} else {
			host = hostPart
		}
	} else {
		// Standard URL with scheme
		u, err := url.Parse(rawURL)
		if err != nil || u.Host == "" {
			return ""
		}
		host = u.Hostname()
		path = u.Path
	}

	// Clean up path
	path = strings.TrimPrefix(path, "/")
	path = strings.TrimSuffix(path, ".git")
	path = strings.TrimSuffix(path, "/")

	if host == "" || path == "" {
		return ""
	}

	return host + "/" + path
}

// isSCPStyle returns true for SCP-style git URLs like "git@host:path".
// These have a colon but no :// scheme prefix.
func isSCPStyle(s string) bool {
	// Must contain a colon
	colon := strings.Index(s, ":")
	if colon < 0 {
		return false
	}
	// Must not have :// (that's a URL scheme)
	if colon+2 < len(s) && s[colon:colon+3] == "://" {
		return false
	}
	// Must not start with / (absolute path)
	if strings.HasPrefix(s, "/") {
		return false
	}
	return true
}
