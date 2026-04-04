package adapter

import "time"

// SessionFileInfo holds metadata extracted from a tool's session file.
type SessionFileInfo struct {
	ID           string
	Title        string
	Slug         string // Human-readable, URL-safe session identity. Set by the adapter.
	Cwd          string
	Created      time.Time
	MessageCount int
	FilePath     string
}

// FileEvent represents a meaningful change extracted from new file content.
type FileEvent struct {
	Title  string
	Status *Status
	Unread *bool // if non-nil, sets the session's unread flag
}

// BoolPtr returns a pointer to v. Convenience for setting FileEvent.Unread.
func BoolPtr(v bool) *bool { return &v }

// Launchable is implemented by adapters that want to expose one or more
// launch presets in the UI.
type Launchable interface {
	// Launchers returns the launch presets this adapter contributes.
	// Adapters may return zero, one, or many presets.
	Launchers() []Launcher
}

// SessionFiler is implemented by adapters whose tools write session
// files to disk (pi, claude-code, etc). Used by gmuxd for resumable
// session discovery and session file attribution.
type SessionFiler interface {
	// SessionRootDir returns the parent directory containing all per-cwd
	// session subdirectories (e.g. ~/.pi/agent/sessions/).
	// Used by the scanner to enumerate all known working directories.
	SessionRootDir() string

	// SessionDir returns the directory where this tool stores session
	// files for the given cwd. Returns "" if not applicable.
	SessionDir(cwd string) string

	// ParseSessionFile reads a session file and returns display metadata.
	// Called by gmuxd for resumable discovery and live file monitoring.
	ParseSessionFile(path string) (*SessionFileInfo, error)
}

// FileMonitor is implemented by adapters that want to react to changes
// in their attributed session file. gmuxd calls ParseNewLines when
// inotify fires on an attributed file.
type FileMonitor interface {
	// ParseNewLines receives lines appended since the last read.
	// filePath is the attributed session file being monitored; adapters
	// may read it to inspect preceding context (e.g. counting consecutive
	// errors to detect exhausted retries).
	// Returns events that should update the session's state.
	ParseNewLines(lines []string, filePath string) []FileEvent
}

// SessionFileLister is an optional extension of SessionFiler for adapters
// whose session files aren't organized as direct children of per-cwd
// subdirectories. When implemented, the scanner uses ListSessionFiles
// instead of the default one-level directory listing.
type SessionFileLister interface {
	// ListSessionFiles returns all session file paths under the root.
	// Called instead of the default per-subdirectory listing.
	ListSessionFiles() []string
}

// FileCandidate describes a live session that could own a file.
// Passed to FileAttributor.AttributeFile for matching.
type FileCandidate struct {
	SessionID  string
	Cwd        string
	StartedAt  time.Time
	Scrollback string // recent terminal text; empty if unavailable
}

// FileAttributor is optionally implemented by adapters that need custom
// file-to-session matching. Without it, the daemon falls back to the
// first candidate.
//
// AttributeFile is called for every candidate count (including 1).
// Returning "" rejects the file; for single-candidate cases, the daemon
// may still attribute via a freshness-based fallback (mtime < 30s).
type FileAttributor interface {
	// AttributeFile returns the session ID of the candidate that owns
	// the file, or "" if no candidate matches. The daemon provides
	// scrollback text when available.
	AttributeFile(filePath string, candidates []FileCandidate) string
}

// CommandTitler is optionally implemented by adapters that want to
// control how a command array is displayed as a fallback title.
// Without it, the store joins the full command (e.g. "pytest -x").
// Adapters that use resume commands should implement this to avoid
// titles like "codex resume 019cfb54-...".
type CommandTitler interface {
	// CommandTitle returns a display title for the given command.
	CommandTitle(command []string) string
}

// Resumer is implemented by adapters whose sessions can be resumed
// after the process exits.
type Resumer interface {
	// ResumeCommand returns the command to resume the given session.
	ResumeCommand(info *SessionFileInfo) []string

	// CanResume returns whether a session file represents a resumable
	// session (vs a corrupted/empty/incompatible one).
	CanResume(path string) bool
}
