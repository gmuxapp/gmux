// Package paths provides common file paths used by both gmux and gmuxd.
package paths

import (
	"os"
	"path/filepath"
)

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
