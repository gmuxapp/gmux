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
