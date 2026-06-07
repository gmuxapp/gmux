// Package paths provides common file paths used by both gmux and gmuxd.
package paths

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// sessionIDRe matches well-formed session IDs. IDs are minted by
// cli/gmux/internal/naming.SessionID as "sess-<hex>"; the allowlist
// here (sess- prefix followed by alphanumerics, hyphens, and
// underscores) is deliberately a touch broader than pure hex so it
// stays robust to ID-shape evolution, while still excluding every
// path-dangerous character. Validating against this shape keeps an
// attacker-influenced ID (the daemon derives it from a runner's
// /meta, and a malicious socket_path in POST /v1/register could try
// to steer it) from carrying path separators or ".." into
// filepath.Join.
var sessionIDRe = regexp.MustCompile(`^sess-[A-Za-z0-9_-]+$`)

// IsValidSessionID reports whether id is a well-formed local session
// ID safe to use as a path segment under SessionsDir.
func IsValidSessionID(id string) bool {
	return sessionIDRe.MatchString(id)
}

// SocketPath returns the path to the gmuxd Unix socket for local IPC.
func SocketPath() string {
	return filepath.Join(StateDir(), "gmuxd.sock")
}

// StateDir returns the gmux state directory (~/.local/state/gmux).
func StateDir() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "gmux")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "gmux")
}

// SessionsDir returns the directory under StateDir that holds
// per-session subdirectories (meta.json, scrollback). Both the
// runner (writing scrollback) and gmuxd (writing meta.json,
// reading scrollback) derive their target paths from this so the
// on-disk contract has a single source of truth.
func SessionsDir() string {
	return filepath.Join(StateDir(), "sessions")
}

// SessionDir returns the per-session subdirectory for id under
// SessionsDir. The directory holds meta.json (written by gmuxd's
// sessionmeta package) and scrollback / scrollback.0 (written by
// the runner's scrollback package).
func SessionDir(id string) string {
	return filepath.Join(SessionsDir(), id)
}

// NormalizePath expands a stored path to its absolute form for use in
// filesystem operations. Expands ~ prefix to $HOME and calls filepath.Clean.
func NormalizePath(p string) string {
	if p == "" {
		return ""
	}
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Clean(home)
		}
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[2:])
		}
	}
	return filepath.Clean(p)
}

// CanonicalizePath converts an absolute filesystem path to its canonical
// stored form: symlinks resolved, $HOME prefix replaced with ~.
// Paths not under $HOME are returned cleaned but absolute.
// Empty input returns empty.
func CanonicalizePath(abs string) string {
	if abs == "" {
		return ""
	}
	// Resolve symlinks. Failure is non-fatal (path may be on a remote
	// machine or the target may not exist yet).
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	abs = filepath.Clean(abs)

	home, err := os.UserHomeDir()
	if err != nil {
		return abs
	}
	home = filepath.Clean(home)

	if abs == home {
		return "~"
	}
	if strings.HasPrefix(abs, home+"/") {
		return "~" + abs[len(home):]
	}
	return abs
}
