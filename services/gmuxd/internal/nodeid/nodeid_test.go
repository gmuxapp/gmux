package nodeid

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGeneratesWhenNothingExists(t *testing.T) {
	dir := t.TempDir()

	id, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	hexPart, ok := strings.CutPrefix(id, prefix)
	if !ok {
		t.Fatalf("id %q missing %q prefix", id, prefix)
	}
	if len(hexPart) != idBytes*2 {
		t.Fatalf("id hex part = %d chars, want %d", len(hexPart), idBytes*2)
	}
	if _, err := hex.DecodeString(hexPart); err != nil {
		t.Fatalf("id hex part not valid hex: %v", err)
	}

	// File persisted with 0600.
	info, err := os.Stat(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatalf("stat id file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("id file perm = %o, want 600", perm)
	}
}

func TestStableAcrossCalls(t *testing.T) {
	dir := t.TempDir()

	first, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("first LoadOrCreate: %v", err)
	}
	second, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("second LoadOrCreate: %v", err)
	}
	if first != second {
		t.Fatalf("id changed across calls: %q != %q", first, second)
	}
}

func TestDistinctPerStateDir(t *testing.T) {
	a, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("two fresh node ids collided: %q", a)
	}
}

func TestCorruptedFileIsHardError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte("not-a-node-id\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(dir); err == nil {
		t.Fatal("expected error for corrupted id file, got nil")
	}
}

func TestReadsExistingValidFile(t *testing.T) {
	dir := t.TempDir()
	want := prefix + strings.Repeat("ab", idBytes)
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte(want+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
