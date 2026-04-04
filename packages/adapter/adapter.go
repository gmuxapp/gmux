// Package adapter defines the interface for teaching gmux how to work
// with specific tools. Adapters are matched by command and produce
// Status events for the sidebar. Both gmux and gmuxd import this package.
package adapter

import (
	"path/filepath"
	"regexp"
	"strings"
)

// Status represents an application-reported status for the sidebar.
type Status struct {
	Label   string `json:"label"`             // display text ("working", "3/5 passed")
	Working bool   `json:"working"`           // true while adapter is busy (spinner, building)
	Error   bool   `json:"error,omitempty"`   // true when the adapter hit a retryable error (red dot)
	Title   string `json:"title,omitempty"`   // if set, updates the session title (transient)
}

// Adapter teaches gmux how to work with a specific child process.
// This is the base interface — all adapters must implement it.
type Adapter interface {
	// Name returns the adapter identifier (e.g. "pi", "shell").
	Name() string

	// Discover reports whether this adapter's backing tool is available on
	// the current machine. gmuxd calls this during startup to decide which
	// adapter launchers should be exposed on this system.
	Discover() bool

	// Match returns true if this adapter handles the given command.
	Match(command []string) bool

	// Env returns adapter-specific environment variables for the child.
	// Common GMUX_* vars are set automatically by the runner.
	// Return nil if no extra env is needed.
	Env(ctx EnvContext) []string

	// Monitor receives PTY output and optionally produces a Status.
	// Called on every PTY read with raw bytes. Must be cheap — no
	// allocations or regex compilation per call.
	// Return nil for no change.
	Monitor(output []byte) *Status
}

// EnvContext provides launch context to Env().
type EnvContext struct {
	Cwd        string
	SessionID  string
	SocketPath string
}

// Launcher describes how to start a new session with a given adapter.
// Available is populated by gmuxd after adapter discovery runs on the current host.
type Launcher struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	Command     []string `json:"command"`
	Description string   `json:"description,omitempty"`
	Available   bool     `json:"available"`
}

// BaseName extracts the base name of a command argument, stripping path.
func BaseName(arg string) string {
	return filepath.Base(arg)
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify converts a string to a URL-safe slug: lowercase, non-alphanum
// runs replaced by hyphens, trimmed, capped at 40 characters.
func Slugify(s string) string {
	s = strings.ToLower(s)
	s = slugRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return ""
	}
	if len(s) > 40 {
		s = s[:40]
		s = strings.TrimRight(s, "-")
	}
	return s
}
