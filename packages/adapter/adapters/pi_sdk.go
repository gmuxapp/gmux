package adapters

import (
	"os"
	"os/exec"
	"path/filepath"

	"github.com/gmuxapp/gmux/packages/adapter"
)

func init() {
	All = append(All, NewPiSDK())
}

// PiSDK is an adapter for pi sessions driven by the pi-sdk Node subprocess.
// Session files are identical to regular pi JSONL sessions; all file-related
// capabilities delegate to Pi.
type PiSDK struct {
	pi      *Pi
	libPath string // resolved at Discover() time
}

func NewPiSDK() *PiSDK {
	return &PiSDK{pi: NewPi()}
}

// ── Adapter base ────────────────────────────────────────────────────────────

func (a *PiSDK) Name() string { return "pi-sdk" }

func (a *PiSDK) Discover() bool {
	if _, err := exec.LookPath("node"); err != nil {
		return false
	}
	a.libPath = resolvePiSDKLibPath()
	return a.libPath != ""
}

// Match always returns false: pi-sdk sessions are not PTY sessions, so no
// gmux-run process will ever report this adapter kind.
func (a *PiSDK) Match(_ []string) bool { return false }

func (a *PiSDK) Env(_ adapter.EnvContext) []string { return nil }

func (a *PiSDK) Monitor(_ []byte) *adapter.Event { return nil }

// ── Launchable ──────────────────────────────────────────────────────────────

func (a *PiSDK) Launchers() []adapter.Launcher {
	return []adapter.Launcher{
		{
			ID:          "pi-sdk",
			Label:       "pi (sdk)",
			Command:     []string{"pi-sdk"}, // sentinel; gmuxd detects SubprocessAdapter before exec
			Description: "New pi session via SDK subprocess bridge",
			Available:   a.libPath != "",
		},
	}
}

// ── SubprocessAdapter ───────────────────────────────────────────────────────

func (a *PiSDK) SubprocessCommand(cwd string) []string {
	return []string{"node", a.libPath, "--cwd", cwd}
}

// ── File capabilities (delegate to Pi) ─────────────────────────────────────

func (a *PiSDK) SessionRootDir() string { return a.pi.SessionRootDir() }

func (a *PiSDK) SessionDir(cwd string) string { return a.pi.SessionDir(cwd) }

func (a *PiSDK) ParseSessionFile(path string) (*adapter.SessionFileInfo, error) {
	return a.pi.ParseSessionFile(path)
}

func (a *PiSDK) ParseNewLines(lines []string, filePath string) []adapter.Event {
	return a.pi.ParseNewLines(lines, filePath)
}

func (a *PiSDK) AttributeFile(filePath string, candidates []adapter.FileCandidate) string {
	return a.pi.AttributeFile(filePath, candidates)
}

func (a *PiSDK) ResumeCommand(info *adapter.SessionFileInfo) []string {
	return a.pi.ResumeCommand(info)
}

func (a *PiSDK) CanResume(path string) bool { return a.pi.CanResume(path) }

func (a *PiSDK) CommandTitle(_ []string) string { return "pi" }

// ── Path resolution ─────────────────────────────────────────────────────────

// resolvePiSDKLibPath finds pi-sdk-lib/dist/index.js by checking:
//  1. $GMUX_PI_SDK_LIB env var (explicit override)
//  2. <executable-dir>/pi-sdk-lib/dist/index.js (installed alongside gmuxd)
//  3. ~/.gmux/pi-sdk-lib/dist/index.js (user install location)
func resolvePiSDKLibPath() string {
	if p := os.Getenv("GMUX_PI_SDK_LIB"); p != "" {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "pi-sdk-lib", "dist", "index.js")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidate := filepath.Join(home, ".gmux", "pi-sdk-lib", "dist", "index.js")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}
