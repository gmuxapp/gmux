package adapters

import (
	"testing"

	"github.com/gmuxapp/gmux/packages/adapter"
)

// --- Name ---

func TestPiSbxName(t *testing.T) {
	if NewPiSbx().Name() != "pi-sbx" {
		t.Fatal("expected 'pi-sbx'")
	}
}

// --- Match ---

func TestPiSbxMatchExecCommand(t *testing.T) {
	p := NewPiSbx()
	if !p.Match([]string{"sbx", "exec", "-it", "pi-workspace", "--", "pi"}) {
		t.Fatal("should match sbx exec -it pi-workspace -- pi")
	}
}

func TestPiSbxMatchRunCommand(t *testing.T) {
	p := NewPiSbx()
	if !p.Match([]string{"sbx", "run", "pi-workspace"}) {
		t.Fatal("should match sbx run pi-workspace (covers existing pi-session alias)")
	}
}

func TestPiSbxMatchFullPathSbx(t *testing.T) {
	p := NewPiSbx()
	if !p.Match([]string{"/usr/local/bin/sbx", "exec", "-it", "pi-workspace", "--", "pi"}) {
		t.Fatal("should match full path to sbx")
	}
}

func TestPiSbxNoMatchWithoutSbx(t *testing.T) {
	p := NewPiSbx()
	if p.Match([]string{"bash", "pi-workspace"}) {
		t.Fatal("should not match without sbx in command")
	}
}

func TestPiSbxNoMatchOtherWorkspace(t *testing.T) {
	p := NewPiSbx()
	if p.Match([]string{"sbx", "run", "my-other-workspace"}) {
		t.Fatal("should not match a different workspace name")
	}
}

func TestPiSbxNoMatchAfterDoubleDash(t *testing.T) {
	p := NewPiSbx()
	// pi-workspace appearing after -- should not match
	if p.Match([]string{"echo", "--", "sbx", "pi-workspace"}) {
		t.Fatal("should not match pi-workspace after '--'")
	}
}

// --- Env / Monitor ---

func TestPiSbxEnvNil(t *testing.T) {
	if env := NewPiSbx().Env(adapter.EnvContext{}); env != nil {
		t.Fatalf("expected nil env, got %v", env)
	}
}

func TestPiSbxMonitorNoOp(t *testing.T) {
	p := NewPiSbx()
	if p.Monitor([]byte("some output")) != nil {
		t.Fatal("Monitor should return nil (status driven by file, not PTY)")
	}
}

// --- Launchers ---

func TestPiSbxLaunchers(t *testing.T) {
	launchers := NewPiSbx().Launchers()
	if len(launchers) != 1 {
		t.Fatalf("expected 1 launcher, got %d", len(launchers))
	}
	l := launchers[0]
	if l.ID != "pi-sbx" {
		t.Errorf("expected ID 'pi-sbx', got %q", l.ID)
	}
	if l.Label == "" {
		t.Error("launcher label must not be empty")
	}
	want := []string{"sbx", "exec", "-it", "pi-workspace", "--", "pi"}
	if len(l.Command) != len(want) {
		t.Fatalf("expected command %v, got %v", want, l.Command)
	}
	for i, arg := range want {
		if l.Command[i] != arg {
			t.Errorf("command[%d]: want %q, got %q", i, arg, l.Command[i])
		}
	}
}

// --- Capabilities ---

func TestPiSbxImplementsCapabilities(t *testing.T) {
	var a adapter.Adapter = NewPiSbx()
	if _, ok := a.(adapter.Launchable); !ok {
		t.Fatal("should implement Launchable")
	}
	if _, ok := a.(adapter.SessionFiler); !ok {
		t.Fatal("should implement SessionFiler")
	}
	if _, ok := a.(adapter.FileMonitor); !ok {
		t.Fatal("should implement FileMonitor")
	}
	if _, ok := a.(adapter.FileAttributor); !ok {
		t.Fatal("should implement FileAttributor")
	}
	if _, ok := a.(adapter.Resumer); !ok {
		t.Fatal("should implement Resumer")
	}
}

// --- Delegation smoke tests ---
// These verify that session file handling delegates to Pi correctly.
// Full parsing behaviour is tested in pi_test.go.

func TestPiSbxSessionRootDirDelegates(t *testing.T) {
	t.Setenv("PI_CODING_AGENT_DIR", "")
	pi := NewPi()
	piSbx := NewPiSbx()
	if piSbx.SessionRootDir() != pi.SessionRootDir() {
		t.Error("SessionRootDir should delegate to Pi")
	}
}

func TestPiSbxSessionDirDelegates(t *testing.T) {
	pi := NewPi()
	piSbx := NewPiSbx()
	cwd := "/home/user/dev/project"
	if piSbx.SessionDir(cwd) != pi.SessionDir(cwd) {
		t.Error("SessionDir should delegate to Pi")
	}
}
