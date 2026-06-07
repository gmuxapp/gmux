// Package paths provides common file paths used by both gmux and gmuxd.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SocketPath returns the path to the gmuxd Unix socket for local IPC.
func SocketPath() string {
	return filepath.Join(StateDir(), "gmuxd.sock")
}

// SessionSocketDir returns the directory that holds per-session Unix
// sockets. Both the runner (which binds the sockets) and gmuxd (which
// scans them for discovery) resolve it here so they always agree on a
// single location.
//
// The GMUX_SOCKET_DIR env var overrides everything; it is used by tests
// and devcontainers and is passed through to the daemon so both ends
// scan the same place. Otherwise the default is per-user so that two OS
// users on the same host never collide on one world-shared directory
// (whoever creates it first owns it 0700, locking everyone else out):
//
//   - $XDG_RUNTIME_DIR/gmux/sessions when XDG_RUNTIME_DIR is set (the
//     standard per-user, 0700, tmpfs-backed runtime dir on Linux), else
//   - a per-uid subdirectory of the system temp dir, e.g.
//     /tmp/gmux-sessions-1000.
func SessionSocketDir() string {
	if d := os.Getenv("GMUX_SOCKET_DIR"); d != "" {
		return d
	}
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		return filepath.Join(rt, "gmux", "sessions")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("gmux-sessions-%d", os.Getuid()))
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
