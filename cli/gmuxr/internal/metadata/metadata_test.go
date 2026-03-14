package metadata

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteAndRead(t *testing.T) {
	dir := t.TempDir()
	origDir := MetaDir
	// Override meta dir for test
	MetaDir = dir
	defer func() { MetaDir = origDir }()

	meta := New("sess-1", "pi:test:1", "pi", "/tmp/test", []string{"pi", "--session", "/tmp/s.jsonl"})
	if err := meta.Write(); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	got, err := Read("pi:test:1")
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}

	if got.SessionID != "sess-1" {
		t.Errorf("expected sess-1, got %s", got.SessionID)
	}
	if got.State != "starting" {
		t.Errorf("expected starting, got %s", got.State)
	}
	if got.Version != 1 {
		t.Errorf("expected version 1, got %d", got.Version)
	}
}

func TestSetStateAndExited(t *testing.T) {
	dir := t.TempDir()
	origDir := MetaDir
	MetaDir = dir
	defer func() { MetaDir = origDir }()

	meta := New("sess-2", "pi:test:2", "pi", "/tmp", []string{"pi"})
	meta.Write()

	meta.SetRunning(12345)
	got, _ := Read("pi:test:2")
	if got.State != "running" || got.Pid != 12345 {
		t.Errorf("unexpected state after SetRunning: %+v", got)
	}

	meta.SetExited(0)
	got, _ = Read("pi:test:2")
	if got.State != "exited" || got.ExitCode == nil || *got.ExitCode != 0 {
		t.Errorf("unexpected state after SetExited: %+v", got)
	}
}

func TestCleanup(t *testing.T) {
	dir := t.TempDir()
	origDir := MetaDir
	MetaDir = dir
	defer func() { MetaDir = origDir }()

	meta := New("sess-3", "pi:test:3", "pi", "/tmp", []string{"pi"})
	meta.Write()

	if err := meta.Cleanup(); err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}

	path := filepath.Join(dir, "pi:test:3.json")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("expected file to be removed after cleanup")
	}
}

func TestListAll(t *testing.T) {
	dir := t.TempDir()
	origDir := MetaDir
	MetaDir = dir
	defer func() { MetaDir = origDir }()

	New("s1", "pi:a:1", "pi", "/tmp", []string{"pi"}).Write()
	New("s2", "pi:a:2", "pi", "/tmp", []string{"pi"}).Write()

	all, err := ListAll()
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("expected 2, got %d", len(all))
	}
}
