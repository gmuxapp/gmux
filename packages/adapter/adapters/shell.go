// Package adapters contains all built-in adapter implementations.
// Each adapter gets its own file. For contributor docs, see the website
// page "Writing an Adapter".
package adapters

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/paths"
)

// All contains instances of all non-fallback adapters, registered via init().
var All []adapter.Adapter

// DefaultFallback returns the shell adapter (always the fallback).
func DefaultFallback() adapter.Adapter { return NewShell() }

// AllAdapters returns all adapters including the default fallback.
// Use this when you need to iterate all capabilities (scanner, resume, etc).
func AllAdapters() []adapter.Adapter {
	result := make([]adapter.Adapter, len(All)+1)
	copy(result, All)
	result[len(All)] = DefaultFallback()
	return result
}

// FindByAdapter returns the adapter with the given name, or nil if not found.
// Includes the shell fallback adapter.
func FindByAdapter(name string) adapter.Adapter {
	for _, a := range AllAdapters() {
		if a.Name() == name {
			return a
		}
	}
	return nil
}

// Compile-time interface checks.
var (
	_ adapter.ConversationDescriber = (*Shell)(nil)
	_ adapter.Resumer               = (*Shell)(nil)
	_ adapter.CommandTitler         = (*Shell)(nil)
	_ adapter.SessionRegistrar      = (*Shell)(nil)
	_ adapter.SessionFinalizer      = (*Shell)(nil)
	_ adapter.PromptSignaler        = (*Shell)(nil)
)

// Shell is the fallback adapter. It matches all commands and parses
// OSC 0/2 title sequences for live sidebar titles. Busy/idle status
// comes from runner-tracked OSC 133 prompt marks (see
// StatusFromPromptMarks), not from Monitor.
type Shell struct{}

func NewShell() *Shell { return &Shell{} }

func (g *Shell) Name() string          { return "shell" }
func (g *Shell) Discover() bool        { return true }
func (g *Shell) Match(_ []string) bool { return true }

func (g *Shell) Env(_ adapter.EnvContext) []string { return nil }

// CommandTitle shows the full command with args (e.g. "pytest -x").
func (g *Shell) CommandTitle(command []string) string {
	if len(command) == 0 {
		return "shell"
	}
	base := adapter.BaseName(command[0])
	if len(command) > 1 {
		return base + " " + strings.Join(command[1:], " ")
	}
	return base
}

func (g *Shell) Launchers() []adapter.Launcher {
	return []adapter.Launcher{{
		ID:          "shell",
		Label:       "Shell",
		Command:     nil,
		Description: "Default shell",
	}}
}

// StatusFromPromptMarks opts shell sessions into runner-side OSC 133
// prompt-mark tracking: Working flips true when a command starts
// executing and false when the command finishes / the next prompt is
// drawn. That busy/idle signal is what lets `gmux wait` and `gmux send
// --wait` work on shell sessions (issue #373). Shells whose
// integration doesn't emit the marks simply never report Status, and
// the daemon rejects idle waits on them.
//
// The tracking lives in the runner (not Monitor) because a fast
// command's busy and idle marks can arrive in one PTY read, and
// Monitor can only return a single collapsed Event per chunk — which
// would swallow the working→idle pulse that send --wait keys on.
func (g *Shell) StatusFromPromptMarks() bool { return true }

// --- Conversation storage (file-backed: refs are shell state-file paths) ---

// shellSessionsDir returns the directory for shell state files.
func shellSessionsDir() string {
	return filepath.Join(paths.StateDir(), "shell-sessions")
}

func (g *Shell) ConversationRootDir() string {
	return shellSessionsDir()
}

func (g *Shell) ConversationDir(cwd string) string {
	root := g.ConversationRootDir()
	if root == "" {
		return ""
	}
	// Expand ~ to absolute before encoding so canonical and absolute
	// paths resolve to the same directory.
	abs := paths.NormalizePath(cwd)
	path := strings.TrimPrefix(abs, "/")
	encoded := "--" + strings.ReplaceAll(path, "/", "-") + "--"
	return filepath.Join(root, encoded)
}

// shellStateFile is the JSON structure written to disk for shell sessions.
type shellStateFile struct {
	ID      string    `json:"id"`
	Cwd     string    `json:"cwd"`
	Command []string  `json:"command"`
	Created time.Time `json:"created"`
}

// DescribeConversation reads a shell state file (the ref is the absolute
// file path) and returns display metadata.
func (g *Shell) DescribeConversation(ref string) (*adapter.ConversationInfo, error) {
	path := ref
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sf shellStateFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return nil, err
	}
	if sf.ID == "" || sf.Cwd == "" {
		return nil, os.ErrInvalid
	}
	return &adapter.ConversationInfo{
		ID:           sf.ID,
		Title:        g.CommandTitle(sf.Command),
		Slug:         adapter.Slugify(filepath.Base(sf.Cwd)),
		Cwd:          sf.Cwd,
		Created:      sf.Created,
		LastActivity: fileLastActivity(path),
		Ref:          path,
	}, nil
}

// --- Resumer ---

func (g *Shell) ResumeCommand(info *adapter.ConversationInfo) []string {
	// Resume a shell session by launching the user's default shell in the
	// original cwd. The cwd is passed via the session metadata, not the
	// command itself.
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	return []string{shell}
}

func (g *Shell) CanResume(ref string) bool {
	_, err := g.DescribeConversation(ref)
	return err == nil
}

// --- SessionRegistrar ---

// OnRegister writes a shell state file so gmuxd can rediscover the session
// after a restart, and returns the initial slug derived from the cwd.
func (g *Shell) OnRegister(id, cwd string, command []string) (adapter.RegistrationInfo, error) {
	_, err := WriteShellStateFile(id, cwd, command)
	if err != nil {
		return adapter.RegistrationInfo{}, err
	}
	slug := adapter.Slugify(filepath.Base(paths.NormalizePath(cwd)))
	if slug == "" {
		slug = "shell"
	}
	return adapter.RegistrationInfo{Slug: slug}, nil
}

// --- SessionFinalizer ---

// OnDismiss removes the shell state file when a session is dismissed.
func (g *Shell) OnDismiss(id, cwd string) {
	RemoveShellStateFile(id, cwd)
}

// WriteShellStateFile creates a shell state file for a session. Called by
// OnRegister. The file allows the session scanner to rediscover the session
// after a gmuxd restart.
func WriteShellStateFile(sessionID, cwd string, command []string) (string, error) {
	sh := NewShell()
	dir := sh.ConversationDir(cwd)
	if dir == "" {
		return "", os.ErrInvalid
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	sf := shellStateFile{
		ID:      sessionID,
		Cwd:     cwd,
		Command: command,
		Created: time.Now().UTC(),
	}
	data, err := json.Marshal(sf)
	if err != nil {
		return "", err
	}

	path := filepath.Join(dir, sessionID+".json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// RemoveShellStateFile deletes the state file for a shell session.
// Called when a shell session is dismissed.
func RemoveShellStateFile(sessionID, cwd string) {
	sh := NewShell()
	dir := sh.ConversationDir(cwd)
	if dir == "" {
		return
	}
	os.Remove(filepath.Join(dir, sessionID+".json"))
}

// ParseOSCTitle extracts the title from OSC 0 or OSC 2 escape sequences.
// Handles both BEL (\x07) and ST (ESC \) terminators.
func ParseOSCTitle(data []byte) string {
	for i := 0; i < len(data)-4; i++ {
		if data[i] != 0x1b || data[i+1] != ']' {
			continue
		}
		if data[i+2] != '0' && data[i+2] != '2' {
			continue
		}
		if data[i+3] != ';' {
			continue
		}
		start := i + 4
		for j := start; j < len(data); j++ {
			if data[j] == 0x07 {
				return string(data[start:j])
			}
			if data[j] == 0x1b && j+1 < len(data) && data[j+1] == '\\' {
				return string(data[start:j])
			}
		}
	}
	return ""
}
