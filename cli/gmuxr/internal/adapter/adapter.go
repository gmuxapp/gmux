// Package adapter defines the interface for teaching gmux-run how to
// launch and monitor specific tools. Adapters are matched by command
// and produce Status events for the sidebar.
package adapter

import "path/filepath"

// Status represents an application-reported status for the sidebar.
type Status struct {
	Label string `json:"label"`           // Short text: "thinking", "3/10 passed"
	State string `json:"state"`           // active|attention|success|error|paused|info
	Icon  string `json:"icon,omitempty"`  // Optional icon hint
}

// Adapter teaches gmux-run how to work with a specific child process.
type Adapter interface {
	// Name returns the adapter identifier (e.g. "pi", "pytest", "generic").
	Name() string

	// Match returns true if this adapter handles the given command.
	Match(command []string) bool

	// Prepare modifies the command and environment before launch.
	// Returns the (possibly modified) command and adapter-specific env vars.
	// Common env vars (GMUX, GMUX_SOCKET, etc.) are set by the runner.
	Prepare(ctx PrepareContext) (command []string, env []string)

	// Monitor receives PTY output and optionally produces a Status.
	// Called on every PTY read with raw bytes. Must be cheap — no
	// allocations or regex compilation per call. Return nil for no change.
	Monitor(output []byte) *Status
}

// PrepareContext provides launch context to Prepare().
type PrepareContext struct {
	Command    []string
	Cwd        string
	SessionID  string
	SocketPath string
}

// BaseName extracts the base name of a command argument, stripping path.
func BaseName(arg string) string {
	return filepath.Base(arg)
}
