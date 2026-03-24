package authtoken

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadOrCreateGeneratesToken(t *testing.T) {
	dir := t.TempDir()

	token, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != tokenBytes*2 {
		t.Fatalf("token length = %d, want %d", len(token), tokenBytes*2)
	}

	// File should exist with correct permissions.
	path := filepath.Join(dir, fileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file permissions = %o, want 600", perm)
	}

	// File content should match returned token.
	data, _ := os.ReadFile(path)
	if got := strings.TrimSpace(string(data)); got != token {
		t.Errorf("file content = %q, want %q", got, token)
	}
}

func TestLoadOrCreateReadsExisting(t *testing.T) {
	dir := t.TempDir()

	// Generate first.
	token1, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Second call should return the same token.
	token2, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if token2 != token1 {
		t.Errorf("second load = %q, want %q", token2, token1)
	}
}

func TestLoadOrCreateRegeneratesCorrupted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, fileName)

	// Write garbage.
	os.WriteFile(path, []byte("not-a-valid-token"), 0o600)

	token, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != tokenBytes*2 {
		t.Fatalf("regenerated token length = %d, want %d", len(token), tokenBytes*2)
	}
}

func TestLoadOrCreateCreatesStateDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "state", "gmux")

	token, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != tokenBytes*2 {
		t.Fatalf("token length = %d", len(token))
	}
	if _, err := os.Stat(filepath.Join(dir, fileName)); err != nil {
		t.Fatalf("file not created: %v", err)
	}
}

func TestEqualConstantTime(t *testing.T) {
	a := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	b := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	if !Equal(a, a) {
		t.Error("Equal(a, a) should be true")
	}
	if Equal(a, b) {
		t.Error("Equal(a, b) should be false")
	}
	if Equal(a, "") {
		t.Error("Equal(a, '') should be false")
	}
}

func TestEqualRejectsPartialPrefix(t *testing.T) {
	token := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	prefix := "aaaaaaaaaaaaaaaa"

	if Equal(token, prefix) {
		t.Error("Equal should reject prefix match")
	}
}
