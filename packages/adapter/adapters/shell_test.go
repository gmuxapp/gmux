package adapters

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gmuxapp/gmux/packages/adapter"
)

func TestShellImplementsInterfaces(t *testing.T) {
	var s adapter.Adapter = NewShell()
	if _, ok := s.(adapter.SessionFiler); !ok {
		t.Fatal("Shell should implement SessionFiler")
	}
	if _, ok := s.(adapter.Resumer); !ok {
		t.Fatal("Shell should implement Resumer")
	}
	if _, ok := s.(adapter.CommandTitler); !ok {
		t.Fatal("Shell should implement CommandTitler")
	}
	if _, ok := s.(adapter.SessionRegistrar); !ok {
		t.Fatal("Shell should implement SessionRegistrar")
	}
	if _, ok := s.(adapter.SessionFinalizer); !ok {
		t.Fatal("Shell should implement SessionFinalizer")
	}
}

func TestShellWriteAndParseStateFile(t *testing.T) {
	// Override state dir to a temp location.
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	path, err := WriteShellStateFile("sess-abc123", "/home/user/dev/project", []string{"fish"})
	if err != nil {
		t.Fatalf("WriteShellStateFile: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state file not created: %v", err)
	}

	sh := NewShell()
	info, err := sh.ParseSessionFile(path)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}

	if info.ID != "sess-abc123" {
		t.Errorf("ID = %q, want %q", info.ID, "sess-abc123")
	}
	if info.Cwd != "/home/user/dev/project" {
		t.Errorf("Cwd = %q, want %q", info.Cwd, "/home/user/dev/project")
	}
	if info.Title != "fish" {
		t.Errorf("Title = %q, want %q", info.Title, "fish")
	}
	if info.FilePath != path {
		t.Errorf("FilePath = %q, want %q", info.FilePath, path)
	}
	// Slug derived from cwd basename.
	if info.Slug != "project" {
		t.Errorf("Slug = %q, want %q", info.Slug, "project")
	}
}

func TestShellCanResume(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	path, err := WriteShellStateFile("sess-resume1", "/home/user/work", []string{"bash"})
	if err != nil {
		t.Fatalf("WriteShellStateFile: %v", err)
	}

	sh := NewShell()
	if !sh.CanResume(path) {
		t.Error("CanResume should return true for valid state file")
	}

	badPath := filepath.Join(tmp, "nonexistent.json")
	if sh.CanResume(badPath) {
		t.Error("CanResume should return false for missing file")
	}
}

func TestShellResumeCommand(t *testing.T) {
	t.Setenv("SHELL", "/usr/bin/fish")
	sh := NewShell()
	cmd := sh.ResumeCommand(&adapter.SessionFileInfo{
		Cwd: "/home/user/project",
	})
	if len(cmd) != 1 || cmd[0] != "/usr/bin/fish" {
		t.Errorf("ResumeCommand = %v, want [/usr/bin/fish]", cmd)
	}
}

func TestShellSessionDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	sh := NewShell()
	dir := sh.SessionDir("/home/user/dev/project")
	expected := filepath.Join(tmp, "gmux", "shell-sessions", "--home-user-dev-project--")
	if dir != expected {
		t.Errorf("SessionDir = %q, want %q", dir, expected)
	}
}

func TestShellRemoveStateFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	path, err := WriteShellStateFile("sess-remove1", "/home/user/dev", []string{"zsh"})
	if err != nil {
		t.Fatalf("WriteShellStateFile: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state file should exist: %v", err)
	}

	RemoveShellStateFile("sess-remove1", "/home/user/dev")

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("state file should be removed after RemoveShellStateFile")
	}
}

func TestAllAdaptersIncludesShell(t *testing.T) {
	all := AllAdapters()
	found := false
	for _, a := range all {
		if a.Name() == "shell" {
			found = true
			break
		}
	}
	if !found {
		t.Error("AllAdapters() should include the shell adapter")
	}
}

func TestFindByKind(t *testing.T) {
	// Shell is the fallback — not in All — so FindByKind is the only way
	// to look it up by name without a match call.
	shell := FindByKind("shell")
	if shell == nil {
		t.Fatal("FindByKind(\"shell\") returned nil")
	}
	if shell.Name() != "shell" {
		t.Errorf("got adapter name %q, want \"shell\"", shell.Name())
	}

	// Unknown kind should return nil.
	if got := FindByKind("nonexistent"); got != nil {
		t.Errorf("FindByKind(\"nonexistent\") = %v, want nil", got)
	}
}

func TestShellOnRegister(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	sh := NewShell()
	info, err := sh.OnRegister("sess-reg1", "/home/user/dev/myproject", []string{"bash"})
	if err != nil {
		t.Fatalf("OnRegister: %v", err)
	}

	// State file should exist so the session can be rediscovered after restart.
	statePath := filepath.Join(sh.SessionDir("/home/user/dev/myproject"), "sess-reg1.json")
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state file not created at %s: %v", statePath, err)
	}

	// Slug should be derived from the cwd basename.
	if info.Slug != "myproject" {
		t.Errorf("Slug = %q, want \"myproject\"", info.Slug)
	}
}

func TestShellOnDismiss(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	sh := NewShell()
	// Register creates the state file.
	if _, err := sh.OnRegister("sess-dis1", "/home/user/dev/proj", []string{"zsh"}); err != nil {
		t.Fatalf("OnRegister: %v", err)
	}
	statePath := filepath.Join(sh.SessionDir("/home/user/dev/proj"), "sess-dis1.json")
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state file should exist before dismiss: %v", err)
	}

	// Dismiss removes it.
	sh.OnDismiss("sess-dis1", "/home/user/dev/proj")
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Error("state file should be gone after OnDismiss")
	}
}
