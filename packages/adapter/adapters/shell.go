// Package adapters contains all built-in adapter implementations.
// Each adapter gets its own file. For contributor docs, see the website
// page "Writing an Adapter".
package adapters

import "github.com/gmuxapp/gmux/packages/adapter"

// All contains instances of all non-fallback adapters, registered via init().
var All []adapter.Adapter

// DefaultFallback returns the shell adapter (always the fallback).
func DefaultFallback() adapter.Adapter { return NewShell() }

// AllLaunchers derives the UI launch presets from registered adapters and the
// shell fallback. Adapters are returned in registration order, with shell last.
// gmuxd filters this compiled launcher list based on per-host availability.
func AllLaunchers() []adapter.Launcher {
	var launchers []adapter.Launcher
	seen := map[string]struct{}{}

	appendLaunchers := func(ls []adapter.Launcher) {
		for _, l := range ls {
			if _, ok := seen[l.ID]; ok {
				continue
			}
			seen[l.ID] = struct{}{}
			launchers = append(launchers, l)
		}
	}

	for _, a := range All {
		if launchable, ok := a.(adapter.Launchable); ok {
			appendLaunchers(launchable.Launchers())
		}
	}

	if launchable, ok := DefaultFallback().(adapter.Launchable); ok {
		appendLaunchers(launchable.Launchers())
	}

	return launchers
}

// Shell is the fallback adapter. It matches all commands and parses
// OSC 0/2 title sequences for live sidebar titles. It does not report
// running/idle status — that's for agent adapters, not plain shells.
type Shell struct{}

func NewShell() *Shell { return &Shell{} }

func (g *Shell) Name() string          { return "shell" }
func (g *Shell) Discover() bool        { return true }
func (g *Shell) Match(_ []string) bool { return true }

func (g *Shell) Env(_ adapter.EnvContext) []string { return nil }

func (g *Shell) Launchers() []adapter.Launcher {
	return []adapter.Launcher{{
		ID:          "shell",
		Label:       "Shell",
		Command:     nil,
		Description: "Default shell",
	}}
}

func (g *Shell) Monitor(_ []byte) *adapter.Status {
	// Shell title parsing is handled centrally in gmuxr so all sessions
	// can use terminal titles as a fallback, not just shell sessions.
	return nil
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
