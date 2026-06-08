package adapters

import (
	"os"
	"testing"

	"github.com/gmuxapp/gmux/packages/adapter"
)

// Compile-time interface checks.
var (
	_ adapter.Launchable        = (*PiSDK)(nil)
	_ adapter.SessionFiler      = (*PiSDK)(nil)
	_ adapter.FileMonitor       = (*PiSDK)(nil)
	_ adapter.FileAttributor    = (*PiSDK)(nil)
	_ adapter.Resumer           = (*PiSDK)(nil)
	_ adapter.CommandTitler     = (*PiSDK)(nil)
	_ adapter.SubprocessAdapter = (*PiSDK)(nil)
)

// ── Name ────────────────────────────────────────────────────────────────────

func TestPiSDKName(t *testing.T) {
	if NewPiSDK().Name() != "pi-sdk" {
		t.Fatal("expected 'pi-sdk'")
	}
}

// ── Match ────────────────────────────────────────────────────────────────────

func TestPiSDKMatchAlwaysFalse(t *testing.T) {
	p := NewPiSDK()
	// pi-sdk sessions are not PTY sessions; no gmux-run process ever matches them.
	for _, cmd := range [][]string{
		{"pi"},
		{"node", "pi-sdk-lib.js"},
		{"pi-sdk"},
		{},
		nil,
	} {
		if p.Match(cmd) {
			t.Errorf("Match(%v) = true, want false", cmd)
		}
	}
}

// ── Env / Monitor ────────────────────────────────────────────────────────────

func TestPiSDKEnvNil(t *testing.T) {
	if env := NewPiSDK().Env(adapter.EnvContext{}); env != nil {
		t.Fatalf("expected nil env, got %v", env)
	}
}

func TestPiSDKMonitorNil(t *testing.T) {
	if NewPiSDK().Monitor([]byte("some output")) != nil {
		t.Fatal("Monitor should return nil")
	}
}

// ── CommandTitle ─────────────────────────────────────────────────────────────

func TestPiSDKCommandTitle(t *testing.T) {
	p := NewPiSDK()
	cases := []struct {
		cmd  []string
		want string
	}{
		{nil, "pi"},
		{[]string{}, "pi"},
		{[]string{"node", "/path/to/index.js", "--cwd", "/some/dir"}, "pi"},
	}
	for _, tc := range cases {
		got := p.CommandTitle(tc.cmd)
		if got != tc.want {
			t.Errorf("CommandTitle(%v) = %q, want %q", tc.cmd, got, tc.want)
		}
	}
}

// ── Launchers ────────────────────────────────────────────────────────────────

func TestPiSDKLaunchersCount(t *testing.T) {
	p := NewPiSDK()
	if n := len(p.Launchers()); n != 1 {
		t.Fatalf("expected 1 launcher, got %d", n)
	}
}

func TestPiSDKLauncherID(t *testing.T) {
	l := NewPiSDK().Launchers()[0]
	if l.ID != "pi-sdk" {
		t.Errorf("launcher ID: got %q, want %q", l.ID, "pi-sdk")
	}
}

func TestPiSDKLauncherAvailableWhenLibPathSet(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "index.*.js")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	p := &PiSDK{pi: NewPi(), libPath: f.Name()}
	l := p.Launchers()[0]
	if !l.Available {
		t.Error("launcher should be available when libPath is set")
	}
}

func TestPiSDKLauncherUnavailableWhenNoLibPath(t *testing.T) {
	p := &PiSDK{pi: NewPi(), libPath: ""}
	l := p.Launchers()[0]
	if l.Available {
		t.Error("launcher should not be available when libPath is empty")
	}
}

// ── SubprocessCommand ────────────────────────────────────────────────────────

func TestPiSDKSubprocessCommandStructure(t *testing.T) {
	p := &PiSDK{pi: NewPi(), libPath: "/some/path/index.js"}
	cmd := p.SubprocessCommand("/my/project")

	if len(cmd) != 4 {
		t.Fatalf("expected 4 args, got %d: %v", len(cmd), cmd)
	}
	if cmd[0] != "node" {
		t.Errorf("cmd[0]: got %q, want \"node\"", cmd[0])
	}
	if cmd[1] != "/some/path/index.js" {
		t.Errorf("cmd[1]: got %q, want lib path", cmd[1])
	}
	if cmd[2] != "--cwd" {
		t.Errorf("cmd[2]: got %q, want \"--cwd\"", cmd[2])
	}
	if cmd[3] != "/my/project" {
		t.Errorf("cmd[3]: got %q, want cwd", cmd[3])
	}
}

func TestPiSDKSubprocessCommandCwdPropagated(t *testing.T) {
	p := &PiSDK{pi: NewPi(), libPath: "/lib/index.js"}
	for _, cwd := range []string{"/a", "/b/c", "/home/user/project"} {
		cmd := p.SubprocessCommand(cwd)
		if cmd[3] != cwd {
			t.Errorf("cwd %q: got cmd[3]=%q", cwd, cmd[3])
		}
	}
}

// ── Path resolution ──────────────────────────────────────────────────────────

func TestResolvePiSDKLibPath_EnvOverrideFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "index.*.js")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	t.Setenv("GMUX_PI_SDK_LIB", f.Name())
	got := resolvePiSDKLibPath()
	if got != f.Name() {
		t.Errorf("got %q, want %q", got, f.Name())
	}
}

func TestResolvePiSDKLibPath_EnvNonexistent(t *testing.T) {
	t.Setenv("GMUX_PI_SDK_LIB", "/nonexistent/path/index.js")
	got := resolvePiSDKLibPath()
	if got != "" {
		t.Errorf("expected empty for nonexistent path, got %q", got)
	}
}

func TestResolvePiSDKLibPath_DirectoryRejected(t *testing.T) {
	// A directory path must be rejected even if it exists.
	dir := t.TempDir()
	t.Setenv("GMUX_PI_SDK_LIB", dir)
	got := resolvePiSDKLibPath()
	if got != "" {
		t.Errorf("expected empty for directory path, got %q", got)
	}
}

// ── Delegation to Pi ─────────────────────────────────────────────────────────

func TestPiSDKDelegatesSessionRootDir(t *testing.T) {
	p := NewPiSDK()
	pi := NewPi()
	if p.SessionRootDir() != pi.SessionRootDir() {
		t.Errorf("SessionRootDir mismatch: %q vs %q", p.SessionRootDir(), pi.SessionRootDir())
	}
}

func TestPiSDKDelegatesSessionDir(t *testing.T) {
	p := NewPiSDK()
	pi := NewPi()
	cwd := t.TempDir()
	if p.SessionDir(cwd) != pi.SessionDir(cwd) {
		t.Errorf("SessionDir mismatch for %q", cwd)
	}
}

func TestPiSDKDelegatesCanResume(t *testing.T) {
	p := NewPiSDK()
	// Non-existent path: both should return false
	if p.CanResume("/nonexistent/path.jsonl") {
		t.Error("CanResume should return false for nonexistent path")
	}
}
