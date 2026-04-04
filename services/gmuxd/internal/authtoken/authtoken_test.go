package authtoken

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validToken returns a 64-char hex token for testing.
func validToken() string {
	return strings.Repeat("ab", 32)
}

// otherToken returns a different valid token.
func otherToken() string {
	return strings.Repeat("cd", 32)
}

// shortToken returns a hex string that's too short.
func shortToken() string {
	return strings.Repeat("ab", 15) // 30 chars, below minimum 64
}

func TestGeneratesTokenWhenNothingExists(t *testing.T) {
	dir := t.TempDir()

	token, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != tokenBytes*2 {
		t.Fatalf("token length = %d, want %d", len(token), tokenBytes*2)
	}
	// Must be valid hex.
	if _, err := hex.DecodeString(token); err != nil {
		t.Fatalf("token is not hex: %v", err)
	}

	// File should exist with 0600 permissions.
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

func TestReadsExistingFile(t *testing.T) {
	dir := t.TempDir()

	token1, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}

	token2, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if token2 != token1 {
		t.Errorf("second load = %q, want %q", token2, token1)
	}
}

func TestCorruptedFileIsHardError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, fileName)
	os.WriteFile(path, []byte("not-a-valid-token"), 0o600)

	_, err := LoadOrCreate(dir)
	if err == nil {
		t.Fatal("expected error for corrupted file, got nil")
	}
	if !strings.Contains(err.Error(), "corrupted") {
		t.Errorf("error should mention corruption: %v", err)
	}
}

func TestTruncatedFileIsHardError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, fileName)
	// Write valid hex but too short (32 chars).
	os.WriteFile(path, []byte(strings.Repeat("ab", 16)), 0o600)

	_, err := LoadOrCreate(dir)
	if err == nil {
		t.Fatal("expected error for truncated file, got nil")
	}
	if !strings.Contains(err.Error(), "too short") {
		t.Errorf("error should mention length: %v", err)
	}
}

func TestCreatesStateDir(t *testing.T) {
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

// ── GMUXD_TOKEN env var ──

func TestEnvVarSeedsFileWhenNoFileExists(t *testing.T) {
	dir := t.TempDir()
	tok := validToken()
	t.Setenv(envVar, tok)

	got, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != tok {
		t.Errorf("returned token = %q, want %q", got, tok)
	}

	// Should be written to disk.
	data, _ := os.ReadFile(filepath.Join(dir, fileName))
	if strings.TrimSpace(string(data)) != tok {
		t.Errorf("file content = %q, want %q", strings.TrimSpace(string(data)), tok)
	}
}

func TestEnvVarMatchingFileSucceeds(t *testing.T) {
	dir := t.TempDir()
	tok := validToken()

	// Write matching file first.
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, fileName), []byte(tok+"\n"), 0o600)

	t.Setenv(envVar, tok)

	got, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != tok {
		t.Errorf("returned token = %q, want %q", got, tok)
	}
}

func TestEnvVarMismatchFailsHard(t *testing.T) {
	dir := t.TempDir()

	// Write one token to file.
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, fileName), []byte(validToken()+"\n"), 0o600)

	// Set a different token in env.
	t.Setenv(envVar, otherToken())

	_, err := LoadOrCreate(dir)
	if err == nil {
		t.Fatal("expected error for mismatched tokens, got nil")
	}
	if !strings.Contains(err.Error(), "does not match") {
		t.Errorf("error should mention mismatch: %v", err)
	}
}

func TestEnvVarTooShortFails(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(envVar, shortToken())

	_, err := LoadOrCreate(dir)
	if err == nil {
		t.Fatal("expected error for short token, got nil")
	}
	if !strings.Contains(err.Error(), "too short") {
		t.Errorf("error should mention length: %v", err)
	}
}

func TestEnvVarNotHexFails(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(envVar, strings.Repeat("zz", 32)) // 64 chars, not hex

	_, err := LoadOrCreate(dir)
	if err == nil {
		t.Fatal("expected error for non-hex token, got nil")
	}
	if !strings.Contains(err.Error(), "not valid hex") {
		t.Errorf("error should mention hex: %v", err)
	}
}

func TestEnvVarIsUnsetAfterLoad(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(envVar, validToken())

	_, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}

	if val, ok := os.LookupEnv(envVar); ok {
		t.Errorf("env var should be unset after load, got %q", val)
	}
}

func TestEnvVarIsUnsetEvenOnError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(envVar, shortToken())

	_, _ = LoadOrCreate(dir)

	if val, ok := os.LookupEnv(envVar); ok {
		t.Errorf("env var should be unset after error, got %q", val)
	}
}

func TestEnvVarSeedsFileCreatesStateDir(t *testing.T) {
	// Simulates first container boot: state dir doesn't exist yet.
	dir := filepath.Join(t.TempDir(), "nested", "state", "gmux")
	tok := validToken()
	t.Setenv(envVar, tok)

	got, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != tok {
		t.Errorf("returned token = %q, want %q", got, tok)
	}

	data, err := os.ReadFile(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatalf("file not created: %v", err)
	}
	if strings.TrimSpace(string(data)) != tok {
		t.Errorf("file content = %q, want %q", strings.TrimSpace(string(data)), tok)
	}
}

func TestEnvVarThenRestartWithoutEnvVar(t *testing.T) {
	// Simulates: first start seeds via env var, second start (container
	// restart) has no env var but file persists on the volume.
	dir := t.TempDir()
	tok := validToken()
	t.Setenv(envVar, tok)

	first, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first != tok {
		t.Fatalf("first load = %q, want %q", first, tok)
	}

	// Second call: env var was unset by first call.
	second, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second != tok {
		t.Errorf("second load = %q, want %q", second, tok)
	}
}

func TestLongerThan64HexCharsAccepted(t *testing.T) {
	// openssl rand -hex 64 produces 128 hex chars. Must be valid.
	dir := t.TempDir()
	tok := strings.Repeat("ab", 64) // 128 hex chars
	t.Setenv(envVar, tok)

	got, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != tok {
		t.Errorf("returned token = %q, want %q", got, tok)
	}
}

func TestEnvVarWithCorruptedFileFails(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, fileName), []byte("garbage"), 0o600)

	t.Setenv(envVar, validToken())

	_, err := LoadOrCreate(dir)
	if err == nil {
		t.Fatal("expected error for corrupted file with env var set, got nil")
	}
	if !strings.Contains(err.Error(), "corrupted") {
		t.Errorf("error should mention corruption: %v", err)
	}
}

// ── Equal ──

func TestEqualConstantTime(t *testing.T) {
	a := validToken()
	b := otherToken()

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
	token := validToken()
	prefix := token[:16]

	if Equal(token, prefix) {
		t.Error("Equal should reject prefix match")
	}
}
