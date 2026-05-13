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
		t.Fatal("should match sbx run pi-workspace")
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

func TestPiSbxLaunchersNoEnv(t *testing.T) {
	t.Setenv("PI_CODING_AGENT_DIR", "")
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

func TestPiSbxLaunchersWithWorkdir(t *testing.T) {
	t.Setenv("PI_CODING_AGENT_DIR", "/Users/james/workspace/.pi-user")
	l := NewPiSbx().Launchers()[0]
	want := []string{"sbx", "exec", "-it", "-w", "/Users/james/workspace", "pi-workspace", "--", "pi"}
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

// --- Cwd translation ---

func TestPiSbxParseNewLinesTranslatesCwd(t *testing.T) {
	t.Setenv("PI_CODING_AGENT_DIR", "/Users/james/workspace/.pi-user")
	events := NewPiSbx().ParseNewLines([]string{
		`{"type":"session","id":"abc","cwd":"/home/agent/workspace","timestamp":"2026-03-19T10:00:00Z"}`,
	}, "")
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Cwd != "/Users/james/workspace" {
		t.Errorf("expected translated host cwd, got %q", events[0].Cwd)
	}
}

func TestPiSbxParseNewLinesTranslatesSubpathCwd(t *testing.T) {
	t.Setenv("PI_CODING_AGENT_DIR", "/Users/james/workspace/.pi-user")
	events := NewPiSbx().ParseNewLines([]string{
		`{"type":"session","id":"abc","cwd":"/home/agent/workspace/subdir/project","timestamp":"2026-03-19T10:00:00Z"}`,
	}, "")
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Cwd != "/Users/james/workspace/subdir/project" {
		t.Errorf("expected translated subpath, got %q", events[0].Cwd)
	}
}

func TestPiSbxParseNewLinesPassthroughNonSandboxCwd(t *testing.T) {
	t.Setenv("PI_CODING_AGENT_DIR", "/Users/james/workspace/.pi-user")
	events := NewPiSbx().ParseNewLines([]string{
		`{"type":"session","id":"abc","cwd":"/some/other/path","timestamp":"2026-03-19T10:00:00Z"}`,
	}, "")
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Cwd != "/some/other/path" {
		t.Errorf("non-sandbox cwd should pass through unchanged, got %q", events[0].Cwd)
	}
}

func TestPiSbxParseNewLinesNoTranslationWithoutEnv(t *testing.T) {
	t.Setenv("PI_CODING_AGENT_DIR", "")
	events := NewPiSbx().ParseNewLines([]string{
		`{"type":"session","id":"abc","cwd":"/home/agent/workspace","timestamp":"2026-03-19T10:00:00Z"}`,
	}, "")
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Cwd != "/home/agent/workspace" {
		t.Errorf("without PI_CODING_AGENT_DIR cwd should pass through, got %q", events[0].Cwd)
	}
}

func TestPiSbxParseSessionFileTranslatesCwd(t *testing.T) {
	t.Setenv("PI_CODING_AGENT_DIR", "/Users/james/workspace/.pi-user")
	path := writeTempJSONL(t,
		`{"type":"session","version":3,"id":"abc","timestamp":"2026-03-15T10:00:00Z","cwd":"/home/agent/workspace"}`,
	)
	info, err := NewPiSbx().ParseSessionFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Cwd != "/Users/james/workspace" {
		t.Errorf("expected translated cwd, got %q", info.Cwd)
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
