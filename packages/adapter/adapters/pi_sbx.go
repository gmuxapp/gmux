package adapters

import (
	"os"
	"os/exec"
	"path/filepath"

	"github.com/gmuxapp/gmux/packages/adapter"
)

// Compile-time interface checks.
var (
	_ adapter.Launchable     = (*PiSbx)(nil)
	_ adapter.SessionFiler   = (*PiSbx)(nil)
	_ adapter.FileMonitor    = (*PiSbx)(nil)
	_ adapter.FileAttributor = (*PiSbx)(nil)
	_ adapter.Resumer        = (*PiSbx)(nil)
)

func init() {
	All = append(All, NewPiSbx())
}

// PiSbx is the adapter for launching pi inside a Docker AI Sandbox (sbx).
// It implements the full pi adapter interface by delegating session file
// handling and status monitoring to the Pi adapter.
type PiSbx struct {
	pi *Pi
}

func NewPiSbx() *PiSbx { return &PiSbx{pi: NewPi()} }

func (p *PiSbx) Name() string { return "pi-sbx" }

// Discover returns true if the sbx binary is available on this machine.
func (p *PiSbx) Discover() bool {
	_, err := exec.LookPath("sbx")
	return err == nil
}

// Match returns true for sbx commands that reference pi-workspace.
// Handles both `sbx run pi-workspace` and `sbx exec -it pi-workspace -- pi`.
func (p *PiSbx) Match(cmd []string) bool {
	hasSbx := false
	for _, arg := range cmd {
		if filepath.Base(arg) == "sbx" {
			hasSbx = true
		}
		if hasSbx && arg == "pi-workspace" {
			return true
		}
		if arg == "--" {
			break
		}
	}
	return false
}

// Env returns no extra environment variables.
func (p *PiSbx) Env(_ adapter.EnvContext) []string { return nil }

// Monitor is a no-op — status is driven by the JSONL session file via FileMonitor.
func (p *PiSbx) Monitor(_ []byte) *adapter.Event { return nil }

// workspaceDir derives the host workspace path from PI_CODING_AGENT_DIR.
// PI_CODING_AGENT_DIR points to the .pi-user directory inside the workspace
// (e.g. /Users/james/workspace/.pi-user), so the workspace is its parent.
func (p *PiSbx) workspaceDir() string {
	dir := os.Getenv("PI_CODING_AGENT_DIR")
	if dir == "" {
		return ""
	}
	return filepath.Dir(dir)
}

func (p *PiSbx) Launchers() []adapter.Launcher {
	// Default: run pi without a specific working directory.
	cmd := []string{"sbx", "exec", "-it", "pi-workspace", "--", "pi"}
	// If we know the workspace path, pass it via -w so pi starts in the right
	// project directory and picks up the correct session context.
	if ws := p.workspaceDir(); ws != "" {
		cmd = []string{"sbx", "exec", "-it", "-w", ws, "pi-workspace", "--", "pi"}
	}
	return []adapter.Launcher{{
		ID:          "pi-sbx",
		Label:       "Pi (sandbox)",
		Command:     cmd,
		Description: "Launch pi in the workspace sandbox",
	}}
}

// --- SessionFiler (delegates to Pi) ---

func (p *PiSbx) SessionRootDir() string { return p.pi.SessionRootDir() }
func (p *PiSbx) SessionDir(cwd string) string { return p.pi.SessionDir(cwd) }
func (p *PiSbx) ParseSessionFile(path string) (*adapter.SessionFileInfo, error) {
	return p.pi.ParseSessionFile(path)
}

// --- FileMonitor (delegates to Pi) ---

func (p *PiSbx) ParseNewLines(lines []string, filePath string) []adapter.Event {
	return p.pi.ParseNewLines(lines, filePath)
}

// --- FileAttributor (delegates to Pi) ---

func (p *PiSbx) AttributeFile(filePath string, candidates []adapter.FileCandidate) string {
	return p.pi.AttributeFile(filePath, candidates)
}

// --- Resumer (delegates to Pi) ---

func (p *PiSbx) ResumeCommand(info *adapter.SessionFileInfo) []string {
	return p.pi.ResumeCommand(info)
}

func (p *PiSbx) CanResume(path string) bool { return p.pi.CanResume(path) }
