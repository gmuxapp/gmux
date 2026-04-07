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

// Compile-time interface checks.
var (
	_ adapter.SessionFiler = (*Shell)(nil)
	_ adapter.Resumer      = (*Shell)(nil)
	_ adapter.CommandTitler = (*Shell)(nil)
)

// Shell is the fallback adapter. It matches all commands and parses
// OSC 0/2 title sequences for live sidebar titles. It does not report
// running/idle status — that's for agent adapters, not plain shells.
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

func (g *Shell) Monitor(_ []byte) *adapter.Status {
	// Shell title parsing is handled centrally in gmux so all sessions
	// can use terminal titles as a fallback, not just shell sessions.
	return nil
}

// --- SessionFiler ---

// shellSessionsDir returns the directory for shell state files.
func shellSessionsDir() string {
	return filepath.Join(paths.StateDir(), "shell-sessions")
}

func (g *Shell) SessionRootDir() string {
	return shellSessionsDir()
}

func (g *Shell) SessionDir(cwd string) string {
	root := g.SessionRootDir()
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

func (g *Shell) ParseSessionFile(path string) (*adapter.SessionFileInfo, error) {
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
	return &adapter.SessionFileInfo{
		ID:       sf.ID,
		Title:    g.CommandTitle(sf.Command),
		Slug:     adapter.Slugify(filepath.Base(sf.Cwd)),
		Cwd:      sf.Cwd,
		Created:  sf.Created,
		FilePath: path,
	}, nil
}

// --- Resumer ---

func (g *Shell) ResumeCommand(info *adapter.SessionFileInfo) []string {
	// Resume a shell session by launching the user's default shell in the
	// original cwd. The cwd is passed via the session metadata, not the
	// command itself.
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	return []string{shell}
}

func (g *Shell) CanResume(path string) bool {
	_, err := g.ParseSessionFile(path)
	return err == nil
}

// WriteStateFile creates a shell state file for a session. Called by gmuxd
// when a shell session is first discovered (on register). The file allows
// the session scanner to rediscover the session after a gmuxd restart.
func WriteShellStateFile(sessionID, cwd string, command []string) (string, error) {
	sh := NewShell()
	dir := sh.SessionDir(cwd)
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
	dir := sh.SessionDir(cwd)
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
