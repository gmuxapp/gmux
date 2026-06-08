package adapters

import (
	"os/exec"

	"github.com/gmuxapp/gmux/packages/adapter"
)

// Compile-time interface checks.
var (
	_ adapter.Launchable        = (*PiSDKSbx)(nil)
	_ adapter.SubprocessAdapter = (*PiSDKSbx)(nil)
	_ adapter.SessionFiler      = (*PiSDKSbx)(nil)
	_ adapter.FileMonitor       = (*PiSDKSbx)(nil)
	_ adapter.FileAttributor    = (*PiSDKSbx)(nil)
	_ adapter.Resumer           = (*PiSDKSbx)(nil)
)

func init() {
	All = append(All, NewPiSDKSbx())
}

// PiSDKSbx is the adapter for pi sessions driven by the pi-sdk Node subprocess
// running inside a Docker AI Sandbox (sbx). The Node process runs on the host
// but launches pi inside the sandbox via the --sbx flag.
// All file capabilities delegate to PiSDK (and transitively to Pi).
type PiSDKSbx struct {
	sdk *PiSDK
}

func NewPiSDKSbx() *PiSDKSbx {
	return &PiSDKSbx{sdk: NewPiSDK()}
}

// ── Adapter base ────────────────────────────────────────────────────────────

func (a *PiSDKSbx) Name() string { return "pi-sdk-sbx" }

// Discover returns true if both sbx and the pi-sdk-lib prerequisites are present.
func (a *PiSDKSbx) Discover() bool {
	if _, err := exec.LookPath("sbx"); err != nil {
		return false
	}
	return a.sdk.Discover()
}

// Match always returns false: pi-sdk-sbx sessions are not PTY sessions.
func (a *PiSDKSbx) Match(_ []string) bool { return false }

func (a *PiSDKSbx) Env(_ adapter.EnvContext) []string { return nil }

func (a *PiSDKSbx) Monitor(_ []byte) *adapter.Event { return nil }

// ── Launchable ──────────────────────────────────────────────────────────────

func (a *PiSDKSbx) Launchers() []adapter.Launcher {
	return []adapter.Launcher{
		{
			ID:          "pi-sdk-sbx",
			Label:       "pi (sdk, sandbox)",
			Command:     []string{"pi-sdk-sbx"}, // sentinel; not executed directly
			Description: "New pi session via SDK subprocess bridge (sandbox)",
			Available:   a.sdk.libPath != "",
		},
	}
}

// ── SubprocessAdapter ───────────────────────────────────────────────────────

// SubprocessCommand returns the same Node command as PiSDK but with --sbx,
// which tells pi-sdk-lib to create the pi session via sbx exec.
func (a *PiSDKSbx) SubprocessCommand(cwd string) []string {
	base := a.sdk.SubprocessCommand(cwd)
	return append(base, "--sbx")
}

// ── File capabilities (delegate to PiSDK) ───────────────────────────────────

func (a *PiSDKSbx) SessionRootDir() string { return a.sdk.SessionRootDir() }

func (a *PiSDKSbx) SessionDir(cwd string) string { return a.sdk.SessionDir(cwd) }

func (a *PiSDKSbx) ParseSessionFile(path string) (*adapter.SessionFileInfo, error) {
	return a.sdk.ParseSessionFile(path)
}

func (a *PiSDKSbx) ParseNewLines(lines []string, filePath string) []adapter.Event {
	return a.sdk.ParseNewLines(lines, filePath)
}

func (a *PiSDKSbx) AttributeFile(filePath string, candidates []adapter.FileCandidate) string {
	return a.sdk.AttributeFile(filePath, candidates)
}

func (a *PiSDKSbx) ResumeCommand(info *adapter.SessionFileInfo) []string {
	return a.sdk.ResumeCommand(info)
}

func (a *PiSDKSbx) CanResume(path string) bool { return a.sdk.CanResume(path) }
