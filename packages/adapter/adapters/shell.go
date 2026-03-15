// Package adapters contains all built-in adapter implementations.
// Each adapter gets its own file. To add an adapter, see docs/adapters.md.
package adapters

import "github.com/gmuxapp/gmux/packages/adapter"

// Launchers lists all adapters that can be launched from the UI.
// Shell is not here — it's always added by gmuxd as the default.
var Launchers []adapter.Launcher

// All contains instances of all non-fallback adapters, registered via init().
var All []adapter.Adapter

// DefaultFallback returns the shell adapter (always the fallback).
func DefaultFallback() adapter.Adapter { return NewShell() }

// Shell is the fallback adapter. It matches all commands and parses
// OSC 0/2 title sequences for live sidebar titles. It does not report
// running/idle status — that's for agent adapters, not plain shells.
type Shell struct{}

func NewShell() *Shell { return &Shell{} }

func (g *Shell) Name() string         { return "shell" }
func (g *Shell) Match(_ []string) bool { return true }

func (g *Shell) Env(_ adapter.EnvContext) []string { return nil }

func (g *Shell) Monitor(output []byte) *adapter.Status {
	if title := parseOSCTitle(output); title != "" {
		return &adapter.Status{Title: title}
	}
	return nil
}

// parseOSCTitle extracts the title from OSC 0 or OSC 2 escape sequences.
// Handles both BEL (\x07) and ST (ESC \) terminators.
func parseOSCTitle(data []byte) string {
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
			if data[j] == 0x07 { // BEL
				return string(data[start:j])
			}
			if data[j] == 0x1b && j+1 < len(data) && data[j+1] == '\\' { // ST
				return string(data[start:j])
			}
		}
	}
	return ""
}
