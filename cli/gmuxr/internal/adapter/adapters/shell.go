// Package adapters contains all built-in adapter implementations.
// Each adapter gets its own file. Community adapters follow the same pattern.
package adapters

import "github.com/gmuxapp/gmux/cli/gmuxr/internal/adapter"

// Shell is the fallback adapter. It matches all commands and parses
// OSC 0/2 title sequences for live sidebar titles. It does not report
// running/idle status — that's for agent adapters, not plain shells.
type Shell struct{}

// NewShell creates a shell adapter.
func NewShell() *Shell {
	return &Shell{}
}

func (g *Shell) Name() string { return "shell" }

func (g *Shell) Match(_ []string) bool { return true }

func (g *Shell) Prepare(ctx adapter.PrepareContext) ([]string, []string) {
	return ctx.Command, nil
}

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
		// Look for ESC ] followed by 0 or 2, then ;
		if data[i] != 0x1b || data[i+1] != ']' {
			continue
		}
		if data[i+2] != '0' && data[i+2] != '2' {
			continue
		}
		if data[i+3] != ';' {
			continue
		}
		// Find the title string terminated by BEL or ESC backslash
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

// CheckSilence is a no-op for shells. Activity status is for agent adapters.
func (g *Shell) CheckSilence() *adapter.Status {
	return nil
}
