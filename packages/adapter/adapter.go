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
// It carries only granular booleans; any display text is the
// frontend's concern, derived from these plus exit_code.
type Status struct {
	Working bool `json:"working"`         // true while adapter is busy (spinner, building)
	Error   bool `json:"error,omitempty"` // true when the adapter hit a retryable error (red dot)
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

// maxSlugLen caps slug length. Slugify truncates to this and
// IsValidSlug enforces it, so the two agree on the canonical shape
// and downstream consumers (/@<peer>/<slug> URLs, ${peer}::${slug}
// folder keys) always see a bounded slug.
const maxSlugLen = 40

// validSlugRe matches a normalized slug: lowercase alphanumeric
// segments joined by single hyphens, no leading/trailing hyphen.
// Mirrors the daemon's project-slug rule so session slugs that flow
// into /@<peer>/<slug> URLs and the ${peer}::${slug} folder key can't
// carry "/", "::", newlines, or other separators.
var validSlugRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// IsValidSlug reports whether s is already a well-formed slug
// (the canonical output shape of Slugify): the right character shape
// and within the length cap, so a valid slug never bypasses Slugify's
// truncation.
func IsValidSlug(s string) bool {
	return len(s) <= maxSlugLen && validSlugRe.MatchString(s)
}

// Slugify converts a string to a URL-safe slug: lowercase, non-alphanum
// runs replaced by hyphens, trimmed, capped at 40 characters.
func Slugify(s string) string {
	s = strings.ToLower(s)
	s = slugRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return ""
	}
	if len(s) > maxSlugLen {
		s = s[:maxSlugLen]
		s = strings.TrimRight(s, "-")
	}
	return s
}
