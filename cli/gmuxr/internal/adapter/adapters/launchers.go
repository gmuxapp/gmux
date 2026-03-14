package adapters

import "github.com/gmuxapp/gmux/cli/gmuxr/internal/adapter"

// Launcher describes how to start a new session with a given adapter.
// Each adapter file adds itself to Launchers if it can be launched independently.
type Launcher struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	Command     []string `json:"command"`
	Description string   `json:"description,omitempty"`
}

// Launchers lists all adapters that can be launched from the UI.
// Shell is not here — it's always added by gmuxd as the default.
// Each adapter file appends to this via init().
var Launchers []Launcher

// All contains instances of all non-fallback adapters, registered via init().
// The shell adapter is not here — it's always the fallback.
var All []adapter.Adapter

// Fallback returns the shell adapter (always the fallback).
func Fallback() adapter.Adapter {
	return NewShell()
}
