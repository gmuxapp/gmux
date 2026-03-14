package adapters

import (
	"path/filepath"

	"github.com/gmuxapp/gmux/cli/gmuxr/internal/adapter"
)

// Pi is the adapter for the pi coding agent.
// It recognizes `pi` and `pi-coding-agent` commands, injects session
// tracking, and monitors PTY output for agent state transitions.
//
// Future: a Sidecar() will watch ~/.pi/sessions/<id>/ for JSONL events
// and produce richer status (thinking, waiting, tool_running, etc.).
type Pi struct{}

func NewPi() *Pi { return &Pi{} }

func (p *Pi) Name() string { return "pi" }

// Match returns true if any argument in the command is the `pi` or
// `pi-coding-agent` binary. This handles direct invocation, npx wrappers,
// nix run, full paths, etc.
func (p *Pi) Match(cmd []string) bool {
	for _, arg := range cmd {
		base := filepath.Base(arg)
		if base == "pi" || base == "pi-coding-agent" {
			return true
		}
		if arg == "--" {
			break
		}
	}
	return false
}

// Prepare injects pi-specific environment. The common GMUX_* vars are
// set automatically by the runner.
func (p *Pi) Prepare(ctx adapter.PrepareContext) ([]string, []string) {
	// For now, pass command through unchanged.
	// Future: inject --session-id for session file correlation.
	return ctx.Command, nil
}

// Monitor parses PTY output for pi agent state patterns.
// Pi outputs structured status lines that we can detect:
//   - "⏳" or spinner → thinking/active
//   - "?" prompt or waiting → attention
//   - Tool execution markers → active with tool label
//
// For v1, we rely on the generic activity detection. Once pi's session
// file watching (Sidecar) is implemented, this becomes a secondary signal.
func (p *Pi) Monitor(output []byte) *adapter.Status {
	// v1: no output parsing yet. The generic adapter's activity detection
	// covers the basics. Pi-specific parsing will be added when we have
	// stable output format documentation from pi.
	//
	// Future patterns to detect:
	// - "Thinking..." / spinner → Status{Label: "thinking", State: "active"}
	// - "waiting for" / "?" prompt → Status{Label: "waiting", State: "attention"}
	// - "Running tool:" → Status{Label: "running <tool>", State: "active"}
	return nil
}
